"""Default-deny decision pipeline for create_refund.

Deterministic. No model, no scoring, no learned component -- the authorization
decision is a lookup against a merchant-issued capability list, and that is the
whole point. Provenance is attached to every record for forensics but never
influences the outcome.

Pipeline order (PLAN.md 3.7):
  1 mandate validity -> 2 tool allowlist -> 3 action match -> 4 rate limit
  -> 5 atomic reserve -> 6 receipt injection
"""

from __future__ import annotations

from collections import deque
from dataclasses import dataclass, field
from datetime import datetime, timedelta

from .lifecycle import ActionState, Ledger, LedgerError
from .mandate import Mandate, RefundAction
from .provenance import ProvenanceIndex

REFUND_TOOL = "create_refund"
RECEIPT_PREFIX = "rzpg_"

# Fields that carry authorization weight. Everything else on create_refund
# (speed, notes) is agent-chosen and must never, on its own, cause a denial --
# a blanket rule over all arguments would reject ordinary traffic.
AUTHORIZING_FIELDS = ("payment_id", "amount")


class Rule(str):
    """Decision rule identifiers, recorded verbatim in the decision log."""


MANDATE_EXPIRED = Rule("MANDATE_EXPIRED")
TOOL_NOT_ALLOWED = Rule("TOOL_NOT_ALLOWED")
MALFORMED_ARGUMENTS = Rule("MALFORMED_ARGUMENTS")
NO_AUTHORIZED_ACTION = Rule("NO_AUTHORIZED_ACTION")
AMOUNT_NOT_AUTHORIZED = Rule("AMOUNT_NOT_AUTHORIZED")
ACTION_CONSUMED = Rule("ACTION_CONSUMED")
RATE_LIMIT_EXCEEDED = Rule("RATE_LIMIT_EXCEEDED")
CUMULATIVE_CAP_EXCEEDED = Rule("CUMULATIVE_CAP_EXCEEDED")
ALLOWED = Rule("ALLOWED")


@dataclass
class Decision:
    allowed: bool
    rule: str
    reason: str
    tool: str
    matched_action_id: str | None = None
    receipt: str | None = None
    forwarded_arguments: dict | None = None
    provenance: dict = field(default_factory=dict)

    def to_record(self) -> dict:
        return {
            "allowed": self.allowed,
            "rule": self.rule,
            "reason": self.reason,
            "tool": self.tool,
            "matched_action_id": self.matched_action_id,
            "receipt": self.receipt,
            "provenance": self.provenance,
        }


def receipt_for(action_id: str) -> str:
    """Deterministic idempotency receipt.

    Razorpay requires >=10 chars, alphanumeric/underscore/hyphen. The prefix
    guarantees the floor even for short action ids -- v3's worked example used a
    bare 7-character id and would have failed the live schema (round 3, P3.8).
    Actual acceptance is verified at runtime by gate G1.6, not assumed here.
    """
    return f"{RECEIPT_PREFIX}{action_id}"


class RateLimiter:
    """Sliding one-minute window over forwarded calls."""

    def __init__(self, max_per_minute: int):
        self.max_per_minute = max_per_minute
        self._times: deque[datetime] = deque()

    def _evict(self, now: datetime) -> None:
        cutoff = now - timedelta(seconds=60)
        while self._times and self._times[0] <= cutoff:
            self._times.popleft()

    def would_exceed(self, now: datetime) -> bool:
        self._evict(now)
        return len(self._times) >= self.max_per_minute

    def record(self, now: datetime) -> None:
        self._times.append(now)

    def count(self, now: datetime) -> int:
        self._evict(now)
        return len(self._times)


class Guard:
    """Session-scoped authorization state.

    Bound to the proxy process lifetime. Nothing arriving over JSON-RPC can
    replace the mandate; there is no method here that accepts one.
    """

    def __init__(self, mandate: Mandate):
        self._mandate = mandate
        self.ledger = Ledger(max_cumulative_paise=mandate.limits.max_cumulative_paise)
        self.rate = RateLimiter(mandate.limits.max_calls_per_minute)
        self.provenance = ProvenanceIndex()
        self._literals = mandate.literals()

    @property
    def mandate(self) -> Mandate:
        """Read-only. There is deliberately no setter."""
        return self._mandate

    # -- observation ----------------------------------------------------

    def observe_result(self, seq: int, tool: str, payload) -> None:
        """Index a tool result the agent read back. Forensic only."""
        self.provenance.observe(seq, tool, payload)

    # -- decision -------------------------------------------------------

    def decide(self, tool: str, arguments: dict, now: datetime) -> Decision:
        chains = {
            f: self.provenance.chain(arguments.get(f), self._literals)
            for f in AUTHORIZING_FIELDS
            if f in arguments
        }

        def deny(rule: str, reason: str, action_id: str | None = None) -> Decision:
            return Decision(False, rule, reason, tool,
                            matched_action_id=action_id, provenance=chains)

        # 1. mandate validity
        if self._mandate.is_expired(now):
            return deny(MANDATE_EXPIRED,
                        f"mandate {self._mandate.mandate_id} expired at "
                        f"{self._mandate.expires_at.isoformat()}; call at {now.isoformat()}")

        # 2. tool allowlist -- default-deny, unknown tools included
        if not self._mandate.permits_tool(tool):
            return deny(TOOL_NOT_ALLOWED,
                        f"{tool} is not in allowed_tools {sorted(self._mandate.allowed_tools)}")

        # Non-refund permitted tools (reads) pass through untouched.
        if tool != REFUND_TOOL:
            return Decision(True, ALLOWED, f"{tool} is an allowed non-refund tool",
                            tool, provenance=chains)

        # 3. action match
        payment_id = arguments.get("payment_id")
        amount = arguments.get("amount")
        if not isinstance(payment_id, str) or not isinstance(amount, (int, float)):
            return deny(MALFORMED_ARGUMENTS,
                        f"create_refund requires string payment_id and numeric amount; "
                        f"got payment_id={payment_id!r}, amount={amount!r}")
        amount = int(amount)

        for_payment = [a for a in self._mandate.authorized_refund_actions
                       if a.payment_id == payment_id]
        if not for_payment:
            return deny(NO_AUTHORIZED_ACTION,
                        f"no authorized refund action exists for {payment_id}")

        admitting = [a for a in for_payment if a.admits(amount)]
        if not admitting:
            return deny(AMOUNT_NOT_AUTHORIZED,
                        f"{amount} paise is not authorized for {payment_id}; "
                        f"actions: {', '.join(a.describe() for a in for_payment)}")

        available = [a for a in admitting if self.ledger.is_available(a.action_id)]
        if not available:
            states = ", ".join(
                f"{a.action_id}={self.ledger.state(a.action_id).value}" for a in admitting
            )
            return deny(ACTION_CONSUMED,
                        f"every action authorizing {amount} paise on {payment_id} is "
                        f"already used ({states}); treated as a replay",
                        action_id=admitting[0].action_id)

        action = _pick(available)

        # 4. rate limit
        if self.rate.would_exceed(now):
            return deny(RATE_LIMIT_EXCEEDED,
                        f"{self.rate.count(now)} calls already in the last 60s, "
                        f"limit is {self.rate.max_per_minute}",
                        action_id=action.action_id)

        # 5. atomic reserve -- action and budget together, before forwarding
        try:
            self.ledger.reserve(action.action_id, amount)
        except LedgerError as exc:
            return deny(CUMULATIVE_CAP_EXCEEDED, str(exc), action_id=action.action_id)

        self.rate.record(now)

        # 6. receipt injection
        receipt = receipt_for(action.action_id)
        forwarded = dict(arguments)
        forwarded["receipt"] = receipt

        return Decision(
            True, ALLOWED,
            f"matches {action.describe()}; reserved {amount} paise "
            f"({self.ledger.remaining_paise()} remaining of "
            f"{self.ledger.max_cumulative_paise})",
            tool,
            matched_action_id=action.action_id,
            receipt=receipt,
            forwarded_arguments=forwarded,
            provenance=chains,
        )

    # -- outcome resolution (called by the relay once the child replies) --

    def commit(self, action_id: str) -> None:
        self.ledger.commit(action_id)

    def release_confirmed_rejection(self, action_id: str) -> None:
        self.ledger.release_confirmed_rejection(action_id)

    def mark_in_doubt(self, action_id: str) -> None:
        self.ledger.mark_in_doubt(action_id)


def _pick(candidates: list[RefundAction]) -> RefundAction:
    """Prefer an exact action over a bounded one, so a bounded grant is not
    spent on a refund an exact action already covers. Ties broken by action_id
    for determinism -- the decision log must replay identically."""
    return sorted(candidates, key=lambda a: (a.is_bounded, a.action_id))[0]


__all__ = ["Guard", "Decision", "receipt_for", "ActionState", "REFUND_TOOL"]
