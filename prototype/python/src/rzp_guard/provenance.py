"""Field-path provenance -- FORENSIC ONLY. This module gates nothing.

It answers "where did this argument value come from?" for the decision record and
the dashboard. It is deliberately NOT in the deny path: given the capability list,
a provenance gate on payment_id was provably dead code, because any refund that
matched an authorized action necessarily carries a mandate literal (REVIEW_LOG
round 3, P3.2).

Two honest limits, both measured rather than argued away:
  * an injection can act on an already-known id or a mandate literal, copying
    nothing -- there is no origin trail to find;
  * transformed values (rupees restated as paise, amount+1) lose the link.
So this detects a narrow literal-flow subclass. It is a forensic aid, not a
prompt-injection detector, and nothing in the codebase claims otherwise.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum


class TrustClass(str, Enum):
    """How much the merchant's own system of record vouches for a field."""

    SYSTEM_AUTHORITATIVE = "SYSTEM_AUTHORITATIVE"  # Razorpay-generated
    PARTY_SUPPLIED = "PARTY_SUPPLIED"              # customer/attacker influenceable
    UNCLASSIFIED = "UNCLASSIFIED"


class Provenance(str, Enum):
    USER_MANDATED = "USER_MANDATED"        # the merchant named it
    SYSTEM_DERIVED = "SYSTEM_DERIVED"      # read from a canonical field
    PARTY_DERIVED = "PARTY_DERIVED"        # first seen in party-supplied text
    AGENT_ORIGINATED = "AGENT_ORIGINATED"  # appears nowhere upstream


# Razorpay-generated fields: the merchant's system of record.
_SYSTEM_KEYS = frozenset(
    {"id", "entity", "amount", "status", "order_id", "created_at", "currency",
     "payment_id", "refund_id", "speed_processed", "speed_requested", "batch_id"}
)
# Fields any paying customer can write at checkout, so an attacker can too.
_PARTY_KEYS = frozenset(
    {"notes", "description", "customer_name", "receipt", "email", "contact",
     "customer_email", "customer_contact", "name"}
)


def classify_path(path: str) -> TrustClass:
    """Trust class for a dotted JSON path such as 'items[0].notes.msg'."""
    segments = [seg.split("[")[0] for seg in path.split(".") if seg]
    # Party-supplied wins anywhere in the path: everything nested under `notes`
    # is attacker-influenceable regardless of the leaf's name.
    if any(seg in _PARTY_KEYS for seg in segments):
        return TrustClass.PARTY_SUPPLIED
    if segments and segments[-1] in _SYSTEM_KEYS:
        return TrustClass.SYSTEM_AUTHORITATIVE
    return TrustClass.UNCLASSIFIED


@dataclass(frozen=True)
class Origin:
    seq: int
    tool: str
    path: str
    trust: TrustClass

    def describe(self) -> str:
        return f"seq {self.seq} · {self.tool} · {self.path} [{self.trust.value}]"


@dataclass
class ProvenanceIndex:
    """Every literal the agent has read back, with ALL the paths it appeared at.

    Recording every path (not just the earliest) keeps the awkward case visible:
    an id that appears in a canonical `id` field AND later inside `notes` is
    reported as both, rather than silently resolving in the agent's favour.
    """

    _origins: dict[str, list[Origin]] = field(default_factory=dict)

    def observe(self, seq: int, tool: str, payload) -> None:
        """Walk a tool result and index every scalar leaf with its path."""
        for path, value in _walk(payload):
            key = _norm(value)
            if key is None:
                continue
            origin = Origin(seq=seq, tool=tool, path=path, trust=classify_path(path))
            bucket = self._origins.setdefault(key, [])
            if origin not in bucket:
                bucket.append(origin)

    def origins(self, value) -> list[Origin]:
        key = _norm(value)
        return list(self._origins.get(key, [])) if key is not None else []

    def classify(self, value, mandate_literals: set[str]) -> Provenance:
        key = _norm(value)
        if key is not None and key in mandate_literals:
            return Provenance.USER_MANDATED
        seen = self.origins(value)
        if not seen:
            return Provenance.AGENT_ORIGINATED
        # Earliest origin decides the label, but every path is retained above and
        # rendered in the chain, so a later party-supplied sighting stays visible.
        first = min(seen, key=lambda o: o.seq)
        if first.trust is TrustClass.SYSTEM_AUTHORITATIVE:
            return Provenance.SYSTEM_DERIVED
        if first.trust is TrustClass.PARTY_SUPPLIED:
            return Provenance.PARTY_DERIVED
        return Provenance.AGENT_ORIGINATED

    def chain(self, value, mandate_literals: set[str]) -> dict:
        """Human-readable forensic record for one argument."""
        seen = self.origins(value)
        return {
            "value": _norm(value),
            "provenance": self.classify(value, mandate_literals).value,
            "origins": [o.describe() for o in sorted(seen, key=lambda o: o.seq)],
            "also_seen_in_party_supplied": any(
                o.trust is TrustClass.PARTY_SUPPLIED for o in seen
            ),
        }


def _norm(value) -> str | None:
    """Normalize to a comparable key. Booleans are excluded -- `True`/`False`
    collide with too much to be meaningful evidence of anything."""
    if isinstance(value, bool) or value is None:
        return None
    if isinstance(value, (int, float)):
        return str(int(value)) if float(value).is_integer() else str(value)
    if isinstance(value, str):
        s = value.strip()
        return s or None
    return None


def _walk(node, prefix: str = ""):
    if isinstance(node, dict):
        for k, v in node.items():
            yield from _walk(v, f"{prefix}.{k}" if prefix else str(k))
    elif isinstance(node, list):
        for i, v in enumerate(node):
            yield from _walk(v, f"{prefix}[{i}]")
    else:
        if prefix:
            yield prefix, node
