"""Merchant-issued capability list.

The mandate is the authorization boundary. It is loaded from a path supplied at
proxy launch, before any agent connects; nothing arriving over JSON-RPC can set,
replace, extend or reload it (PLAN.md 3.6).

Authorization is per DISCRETE refund action, not a policy range. An action grants
one refund of a specific amount (or up to a bound the merchant deliberately chose)
against one payment, and is consumed when used. Two legitimate partial refunds of
equal value are two actions -- which is why they both pass.
"""

from __future__ import annotations

from datetime import datetime, timezone

from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator

# Razorpay's create_refund schema enforces a 100 paise floor (PLAN.md 2.5).
MIN_REFUND_PAISE = 100


class RefundAction(BaseModel):
    """One discrete, single-use authorization to refund a specific payment."""

    model_config = ConfigDict(extra="forbid", frozen=True)

    action_id: str = Field(min_length=1)
    payment_id: str = Field(min_length=1)

    # Exactly one of these. Exact is the default the merchant should use; a bound
    # is opt-in and means the merchant deliberately delegated the figure, so an
    # amount inside it is authorized BY THE MERCHANT'S OWN CHOICE (PLAN.md 3.2).
    amount_paise: int | None = None
    max_amount_paise: int | None = None

    @model_validator(mode="after")
    def _exactly_one_amount_rule(self) -> RefundAction:
        exact, bounded = self.amount_paise, self.max_amount_paise
        if (exact is None) == (bounded is None):
            raise ValueError(
                f"action {self.action_id}: set exactly one of amount_paise "
                f"(exact, preferred) or max_amount_paise (bounded, opt-in)"
            )
        value = exact if exact is not None else bounded
        if value < MIN_REFUND_PAISE:
            raise ValueError(
                f"action {self.action_id}: amount {value} is below Razorpay's "
                f"{MIN_REFUND_PAISE} paise floor, so it could never be forwarded"
            )
        return self

    @property
    def is_bounded(self) -> bool:
        return self.max_amount_paise is not None

    def admits(self, amount_paise: int) -> bool:
        """Does this action authorize a refund of exactly this amount?"""
        if self.amount_paise is not None:
            return amount_paise == self.amount_paise
        return MIN_REFUND_PAISE <= amount_paise <= self.max_amount_paise

    def ceiling(self) -> int:
        """Worst-case value this action can consume, for budget reservation."""
        return self.amount_paise if self.amount_paise is not None else self.max_amount_paise

    def describe(self) -> str:
        if self.amount_paise is not None:
            return f"{self.action_id} (exactly {self.amount_paise} paise on {self.payment_id})"
        return f"{self.action_id} (up to {self.max_amount_paise} paise on {self.payment_id})"


class GlobalLimits(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)

    max_cumulative_paise: int = Field(ge=0)
    max_calls_per_minute: int = Field(gt=0)


class Mandate(BaseModel):
    """The complete grant for one proxy session."""

    model_config = ConfigDict(extra="forbid", frozen=True, populate_by_name=True)

    mandate_id: str = Field(min_length=1)
    expires_at: datetime
    allowed_tools: list[str]
    authorized_refund_actions: list[RefundAction] = Field(default_factory=list)
    limits: GlobalLimits = Field(alias="global")

    @field_validator("expires_at")
    @classmethod
    def _require_timezone(cls, v: datetime) -> datetime:
        # A naive datetime would silently compare against local time and could
        # extend a mandate past its intended expiry.
        if v.tzinfo is None:
            raise ValueError("expires_at must carry an explicit timezone offset")
        return v.astimezone(timezone.utc)

    @model_validator(mode="after")
    def _unique_action_ids(self) -> Mandate:
        seen: set[str] = set()
        for a in self.authorized_refund_actions:
            if a.action_id in seen:
                raise ValueError(
                    f"duplicate action_id {a.action_id!r}: action ids are the "
                    f"consumption key and the injected receipt, so they must be unique"
                )
            seen.add(a.action_id)
        return self

    def is_expired(self, now: datetime) -> bool:
        return now >= self.expires_at

    def permits_tool(self, tool: str) -> bool:
        """Default-deny: anything not listed is denied, including unknown tools."""
        return tool in self.allowed_tools

    def literals(self) -> set[str]:
        """Values the merchant named. Used to mark arguments USER_MANDATED."""
        out: set[str] = {self.mandate_id}
        for a in self.authorized_refund_actions:
            out.add(a.payment_id)
            out.add(a.action_id)
            if a.amount_paise is not None:
                out.add(str(a.amount_paise))
            if a.max_amount_paise is not None:
                out.add(str(a.max_amount_paise))
        return out


def load_mandate(raw: dict) -> Mandate:
    """Parse and validate. Raises pydantic ValidationError on anything malformed."""
    return Mandate.model_validate(raw)
