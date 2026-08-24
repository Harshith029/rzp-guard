"""One state machine for action consumption AND budget -- fail closed on ambiguity.

The two resources always move together, so there is one rule to reason about and
one to test (REVIEW_LOG round 3, P3.5).

The governing rule: RELEASE ONLY ON CONFIRMED PROVIDER REJECTION.

Releasing on timeout fails open. Razorpay may have processed the refund while the
proxy lost the response; handing the budget back would return headroom for money
that already left, and the cap breaks exactly when it matters. Equally, a request
that provably never reached the provider must not permanently burn a legitimate
merchant authorization -- hence release on *confirmed* rejection, and only that.

IN_DOUBT is terminal until a human resolves it. There is no automatic
reconciliation: making the proxy query Razorpay itself would require it to become
an MCP client to its own child (internal ids, multiplexing, response suppression)
and would falsify the transparent-relay claim (REVIEW_LOG round 3, P3.3).
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum


class ActionState(str, Enum):
    AVAILABLE = "AVAILABLE"
    RESERVED = "RESERVED"
    COMMITTED = "COMMITTED"
    IN_DOUBT = "IN_DOUBT"


class LedgerError(Exception):
    pass


@dataclass
class _Entry:
    action_id: str
    state: ActionState = ActionState.AVAILABLE
    reserved_paise: int = 0
    committed_paise: int = 0


@dataclass
class Ledger:
    """Tracks per-action state and the session's cumulative spend.

    Budget is counted as reserved + committed, never committed alone: two
    concurrent refunds must not both pass a cumulative check before either
    result returns. MCP allows multiple in-flight requests by JSON-RPC id, so
    this TOCTOU is reachable, not theoretical.
    """

    max_cumulative_paise: int
    _entries: dict[str, _Entry] = field(default_factory=dict)

    def _entry(self, action_id: str) -> _Entry:
        return self._entries.setdefault(action_id, _Entry(action_id=action_id))

    @property
    def committed_paise(self) -> int:
        return sum(e.committed_paise for e in self._entries.values())

    @property
    def reserved_paise(self) -> int:
        return sum(e.reserved_paise for e in self._entries.values())

    @property
    def encumbered_paise(self) -> int:
        """Everything spent or possibly spent. The number the cap applies to."""
        return self.committed_paise + self.reserved_paise

    def remaining_paise(self) -> int:
        return self.max_cumulative_paise - self.encumbered_paise

    def state(self, action_id: str) -> ActionState:
        return self._entry(action_id).state

    def is_available(self, action_id: str) -> bool:
        return self._entry(action_id).state is ActionState.AVAILABLE

    def reserve(self, action_id: str, amount_paise: int) -> None:
        """Atomically claim the action and its budget, before forwarding."""
        entry = self._entry(action_id)
        if entry.state is not ActionState.AVAILABLE:
            raise LedgerError(
                f"action {action_id} is {entry.state.value}, not AVAILABLE"
            )
        if amount_paise > self.remaining_paise():
            raise LedgerError(
                f"cumulative cap: {amount_paise} paise exceeds {self.remaining_paise()} "
                f"remaining of {self.max_cumulative_paise}"
            )
        entry.state = ActionState.RESERVED
        entry.reserved_paise = amount_paise

    def commit(self, action_id: str) -> None:
        """Confirmed success: the refund happened."""
        entry = self._entry(action_id)
        if entry.state is not ActionState.RESERVED:
            raise LedgerError(f"cannot commit {action_id} from {entry.state.value}")
        entry.committed_paise = entry.reserved_paise
        entry.reserved_paise = 0
        entry.state = ActionState.COMMITTED

    def release_confirmed_rejection(self, action_id: str) -> None:
        """The ONLY path back to AVAILABLE.

        Callers must have positive evidence the provider rejected the request.
        A timeout, a dropped connection, or a child crash is NOT evidence -- use
        mark_in_doubt for those.
        """
        entry = self._entry(action_id)
        if entry.state is not ActionState.RESERVED:
            raise LedgerError(f"cannot release {action_id} from {entry.state.value}")
        entry.reserved_paise = 0
        entry.state = ActionState.AVAILABLE

    def mark_in_doubt(self, action_id: str) -> None:
        """Ambiguous outcome. Budget and action stay locked, pending an operator."""
        entry = self._entry(action_id)
        if entry.state is not ActionState.RESERVED:
            raise LedgerError(f"cannot mark {action_id} in doubt from {entry.state.value}")
        entry.state = ActionState.IN_DOUBT
        # reserved_paise deliberately retained: the money may well have moved.

    def resolve_in_doubt(self, action_id: str, *, refund_landed: bool) -> None:
        """Operator resolution. Not reachable from the JSON-RPC path."""
        entry = self._entry(action_id)
        if entry.state is not ActionState.IN_DOUBT:
            raise LedgerError(f"{action_id} is not IN_DOUBT")
        if refund_landed:
            entry.committed_paise = entry.reserved_paise
            entry.reserved_paise = 0
            entry.state = ActionState.COMMITTED
        else:
            entry.reserved_paise = 0
            entry.state = ActionState.AVAILABLE

    def in_doubt_actions(self) -> list[str]:
        return [e.action_id for e in self._entries.values()
                if e.state is ActionState.IN_DOUBT]
