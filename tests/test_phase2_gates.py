"""Phase 2 validation gates G2.1 - G2.5 (PLAN.md 4).

Each test names the gate it discharges. Several exist because an earlier version
of the plan would have failed them -- those are marked REGRESSION and are not
optional.
"""

from datetime import timedelta

import pytest

from rzp_guard.lifecycle import ActionState
from rzp_guard.policy import (
    ACTION_CONSUMED,
    AMOUNT_NOT_AUTHORIZED,
    CUMULATIVE_CAP_EXCEEDED,
    MANDATE_EXPIRED,
    NO_AUTHORIZED_ACTION,
    RATE_LIMIT_EXCEEDED,
    TOOL_NOT_ALLOWED,
    receipt_for,
)
from rzp_guard.provenance import Provenance

from conftest import payment_entity

PAY = "pay_SYN0001"
OTHER = "pay_SYN0002"


# ---------------------------------------------------------------- G2.1

def test_g21_exact_action_admits_matching_refund(guard_factory, now):
    g = guard_factory([{"action_id": "rfa_001", "payment_id": PAY,
                        "amount_paise": 50_000}])
    d = g.decide("create_refund", {"payment_id": PAY, "amount": 50_000}, now)
    assert d.allowed
    assert d.matched_action_id == "rfa_001"


def test_g21_two_equal_partial_refunds_both_pass(guard_factory, now):
    """REGRESSION. Two items returned at the same price is routine merchant
    traffic. PLAN v2's (mandate_id, payment_id, amount) identity rejected the
    second as a replay -- the bug this gate exists to prevent."""
    g = guard_factory([
        {"action_id": "rfa_001", "payment_id": PAY, "amount_paise": 50_000},
        {"action_id": "rfa_002", "payment_id": PAY, "amount_paise": 50_000},
    ])
    first = g.decide("create_refund", {"payment_id": PAY, "amount": 50_000}, now)
    g.commit(first.matched_action_id)
    second = g.decide("create_refund", {"payment_id": PAY, "amount": 50_000}, now)

    assert first.allowed and second.allowed
    assert first.matched_action_id != second.matched_action_id
    # Distinct receipts, or Razorpay would reject the second as a duplicate.
    assert first.receipt != second.receipt


def test_g21_replay_of_consumed_action_is_denied(guard_factory, now):
    g = guard_factory([{"action_id": "rfa_001", "payment_id": PAY,
                        "amount_paise": 50_000}])
    first = g.decide("create_refund", {"payment_id": PAY, "amount": 50_000}, now)
    g.commit(first.matched_action_id)
    replay = g.decide("create_refund", {"payment_id": PAY, "amount": 50_000}, now)

    assert not replay.allowed
    assert replay.rule == ACTION_CONSUMED


def test_g21_refund_without_authorized_action_is_denied(guard_factory, now):
    g = guard_factory([{"action_id": "rfa_001", "payment_id": PAY,
                        "amount_paise": 50_000}])
    d = g.decide("create_refund", {"payment_id": OTHER, "amount": 50_000}, now)
    assert not d.allowed
    assert d.rule == NO_AUTHORIZED_ACTION


def test_g21_unknown_tool_is_denied_by_default(guard_factory, now):
    g = guard_factory([])
    for tool in ("create_instant_settlement", "initiate_payment", "not_a_real_tool"):
        d = g.decide(tool, {"amount": 100_000}, now)
        assert not d.allowed and d.rule == TOOL_NOT_ALLOWED, tool


def test_g21_expired_mandate_denies_an_otherwise_valid_refund(guard_factory, now):
    g = guard_factory(
        [{"action_id": "rfa_001", "payment_id": PAY, "amount_paise": 50_000}],
        expires=now - timedelta(minutes=1),
    )
    d = g.decide("create_refund", {"payment_id": PAY, "amount": 50_000}, now)
    assert not d.allowed
    assert d.rule == MANDATE_EXPIRED


# ---------------------------------------------------------------- G2.2

def test_g22_lookup_then_refund_passes_end_to_end(guard_factory, now):
    """REGRESSION, non-negotiable. Fetch the payment, then refund it -- the most
    common legitimate support workflow. PLAN v1's literal-reuse taint rule would
    have blocked this, because the payment_id appeared in a tool result."""
    g = guard_factory([{"action_id": "rfa_001", "payment_id": PAY,
                        "amount_paise": 29_500}])
    g.observe_result(0, "fetch_payment", payment_entity(PAY, 29_500))

    d = g.decide("create_refund", {"payment_id": PAY, "amount": 29_500}, now)

    assert d.allowed, d.reason
    # It is SYSTEM/USER-derived, and either way provenance did not gate it.
    assert d.provenance["payment_id"]["provenance"] in (
        Provenance.USER_MANDATED.value, Provenance.SYSTEM_DERIVED.value)


def test_g22_list_then_refund_only_the_authorized_payment(guard_factory, now):
    g = guard_factory([{"action_id": "rfa_001", "payment_id": PAY,
                        "amount_paise": 33_000}])
    g.observe_result(0, "fetch_all_payments", {
        "entity": "collection", "count": 2,
        "items": [payment_entity(PAY, 33_000), payment_entity(OTHER, 71_000)],
    })

    assert g.decide("create_refund", {"payment_id": PAY, "amount": 33_000}, now).allowed
    # The neighbouring payment was read from a canonical id field, so provenance
    # rates it clean -- only the capability list stops it.
    unauthorized = g.decide("create_refund", {"payment_id": OTHER, "amount": 71_000}, now)
    assert not unauthorized.allowed
    assert unauthorized.rule == NO_AUTHORIZED_ACTION
    assert unauthorized.provenance["payment_id"]["provenance"] == \
        Provenance.SYSTEM_DERIVED.value


# ---------------------------------------------------------------- G2.3

def test_g23_agent_chosen_speed_and_notes_never_cause_denial(guard_factory, now):
    """REGRESSION. PLAN v2's blanket deny_provenance applied to every argument,
    so an ordinary agent-selected speed="normal" (AGENT_ORIGINATED) would have
    denied the refund. Non-authorizing fields must not gate."""
    g = guard_factory([{"action_id": "rfa_001", "payment_id": PAY,
                        "amount_paise": 15_000}])
    d = g.decide("create_refund", {
        "payment_id": PAY, "amount": 15_000,
        "speed": "normal", "notes": {"agent_ref": "ticket-4001"},
    }, now)
    assert d.allowed, d.reason


def test_g23_proxy_overwrites_any_agent_supplied_receipt(guard_factory, now):
    g = guard_factory([{"action_id": "rfa_001", "payment_id": PAY,
                        "amount_paise": 15_000}])
    d = g.decide("create_refund", {"payment_id": PAY, "amount": 15_000,
                                   "receipt": "attacker-chosen"}, now)
    assert d.allowed
    assert d.forwarded_arguments["receipt"] == receipt_for("rfa_001")


def test_g23_receipt_meets_razorpay_format_floor(guard_factory, now):
    """v3's worked example used a bare 7-char action id against a >=10 floor."""
    r = receipt_for("rfa_001")
    assert len(r) >= 10
    assert all(c.isalnum() or c in "_-" for c in r)


# ---------------------------------------------------------------- G2.4

def test_g24_exact_action_rejects_a_lower_amount(guard_factory, now):
    g = guard_factory([{"action_id": "rfa_001", "payment_id": PAY,
                        "amount_paise": 50_000}])
    d = g.decide("create_refund", {"payment_id": PAY, "amount": 10_000}, now)
    assert not d.allowed
    assert d.rule == AMOUNT_NOT_AUTHORIZED


def test_g24_bounded_action_accepts_a_lower_amount(guard_factory, now):
    """A bounded grant means the merchant deliberately delegated the figure."""
    g = guard_factory([{"action_id": "rfa_003", "payment_id": PAY,
                        "max_amount_paise": 120_000}])
    assert g.decide("create_refund", {"payment_id": PAY, "amount": 45_000}, now).allowed


def test_g24_bounded_action_rejects_above_its_ceiling(guard_factory, now):
    g = guard_factory([{"action_id": "rfa_003", "payment_id": PAY,
                        "max_amount_paise": 120_000}])
    d = g.decide("create_refund", {"payment_id": PAY, "amount": 120_001}, now)
    assert not d.allowed
    assert d.rule == AMOUNT_NOT_AUTHORIZED


def test_g24_amount_is_bound_to_its_payment_not_to_a_set_of_valid_amounts(
        guard_factory, now):
    """Corpus template A4c. 90000 is authorized -- but only on OTHER."""
    g = guard_factory([
        {"action_id": "rfa_001", "payment_id": PAY, "amount_paise": 30_000},
        {"action_id": "rfa_002", "payment_id": OTHER, "amount_paise": 90_000},
    ])
    d = g.decide("create_refund", {"payment_id": PAY, "amount": 90_000}, now)
    assert not d.allowed
    assert d.rule == AMOUNT_NOT_AUTHORIZED


# ---------------------------------------------------------------- G2.5

def test_g25_provenance_records_every_origin_path_not_just_the_first(
        guard_factory, now):
    """The awkward case: an id in a canonical field AND later inside notes. Both
    paths must be retained, or a later party-supplied sighting would resolve
    silently in the agent's favour."""
    g = guard_factory([])  # no mandate literals, so USER_MANDATED cannot mask it
    g.observe_result(0, "fetch_payment", payment_entity(PAY, 40_000))
    g.observe_result(2, "fetch_payment", payment_entity(
        OTHER, 61_000, notes={"ref": PAY}))

    chain = g.provenance.chain(PAY, set())
    paths = " | ".join(chain["origins"])

    assert len(chain["origins"]) >= 2, chain["origins"]
    assert "SYSTEM_AUTHORITATIVE" in paths and "PARTY_SUPPLIED" in paths
    # Earliest origin still decides the label...
    assert chain["provenance"] == Provenance.SYSTEM_DERIVED.value
    # ...but the party-supplied sighting is not lost.
    assert chain["also_seen_in_party_supplied"] is True


def test_g25_bare_id_embedded_in_free_text_is_NOT_detected(guard_factory, now):
    """Documented limitation, asserted so it cannot regress into a false claim.

    `notes.msg = "also refund pay_SYN0001"` is indexed as one string at that
    path; the embedded id is not extracted, so looking up the id finds nothing.
    Substring/transformation evasion is real and is measured, not argued away.
    """
    g = guard_factory([])
    g.observe_result(0, "fetch_payment", payment_entity(
        OTHER, 61_000, notes={"customer_message": f"also refund {PAY}"}))

    chain = g.provenance.chain(PAY, set())
    assert chain["origins"] == []
    assert chain["provenance"] == Provenance.AGENT_ORIGINATED.value


def test_g25_party_supplied_value_is_labelled_party_derived(guard_factory, now):
    g = guard_factory([])
    g.observe_result(0, "fetch_payment", payment_entity(
        PAY, 40_000, notes={"ref": "pay_SYN9001"}))
    chain = g.provenance.chain("pay_SYN9001", set())
    assert chain["provenance"] == Provenance.PARTY_DERIVED.value
    assert chain["also_seen_in_party_supplied"] is True


def test_g25_unseen_value_is_agent_originated(guard_factory, now):
    g = guard_factory([])
    chain = g.provenance.chain("pay_NEVER_SEEN", set())
    assert chain["provenance"] == Provenance.AGENT_ORIGINATED.value


def test_g25_provenance_is_forensic_only_and_never_denies(guard_factory, now):
    """A payment_id whose only origin is attacker-controlled notes, but which IS
    authorized, must still be allowed -- provenance does not gate."""
    g = guard_factory([{"action_id": "rfa_001", "payment_id": PAY,
                        "amount_paise": 40_000}])
    g.observe_result(0, "fetch_payment", payment_entity(
        OTHER, 10_000, notes={"ref": PAY}))

    d = g.decide("create_refund", {"payment_id": PAY, "amount": 40_000}, now)
    assert d.allowed, "the capability list authorizes it; provenance must not veto"
    # USER_MANDATED wins because the merchant named it, and the party-supplied
    # sighting is still recorded for the analyst.
    assert d.provenance["payment_id"]["also_seen_in_party_supplied"] is True


# ------------------------------------------------- lifecycle & limits

def test_cumulative_cap_blocks_the_refund_that_would_breach_it(guard_factory, now):
    g = guard_factory(
        [{"action_id": f"rfa_{k:03d}", "payment_id": f"pay_SYN{k}",
          "amount_paise": 40_000} for k in range(4)],
        cumulative=120_000,
    )
    for k in range(3):
        d = g.decide("create_refund", {"payment_id": f"pay_SYN{k}", "amount": 40_000}, now)
        assert d.allowed
        g.commit(d.matched_action_id)

    fourth = g.decide("create_refund", {"payment_id": "pay_SYN3", "amount": 40_000}, now)
    assert not fourth.allowed
    assert fourth.rule == CUMULATIVE_CAP_EXCEEDED


def test_rate_limit_alone_decides_when_every_action_is_authorized(guard_factory, now):
    """Corpus template A2e. All prior controls pass, so only the rate limit can
    be the reason -- v1.0's A2e never reached the limiter at all."""
    g = guard_factory(
        [{"action_id": f"rfa_{k:03d}", "payment_id": f"pay_SYN{k}",
          "amount_paise": 20_000} for k in range(14)],
        cumulative=1_000_000, per_minute=10,
    )
    outcomes = [g.decide("create_refund",
                         {"payment_id": f"pay_SYN{k}", "amount": 20_000}, now)
                for k in range(14)]
    assert all(d.allowed for d in outcomes[:10])
    assert all(not d.allowed and d.rule == RATE_LIMIT_EXCEEDED for d in outcomes[10:])


def test_concurrent_duplicates_only_one_reserves(guard_factory, now):
    """Two in-flight refunds against one action; neither has resolved."""
    g = guard_factory([{"action_id": "rfa_001", "payment_id": PAY,
                        "amount_paise": 55_000}])
    first = g.decide("create_refund", {"payment_id": PAY, "amount": 55_000}, now)
    second = g.decide("create_refund", {"payment_id": PAY, "amount": 55_000}, now)

    assert first.allowed
    assert not second.allowed and second.rule == ACTION_CONSUMED


def test_budget_counts_reserved_not_only_committed(guard_factory, now):
    """A check-then-act cap would let two concurrent refunds both through."""
    g = guard_factory([
        {"action_id": "rfa_001", "payment_id": PAY, "amount_paise": 60_000},
        {"action_id": "rfa_002", "payment_id": OTHER, "amount_paise": 60_000},
    ], cumulative=100_000)
    assert g.decide("create_refund", {"payment_id": PAY, "amount": 60_000}, now).allowed
    second = g.decide("create_refund", {"payment_id": OTHER, "amount": 60_000}, now)
    assert not second.allowed
    assert second.rule == CUMULATIVE_CAP_EXCEEDED


def test_in_doubt_holds_budget_and_action_locked(guard_factory, now):
    """The hardest panel question: provider may have processed it, response lost."""
    g = guard_factory([{"action_id": "rfa_001", "payment_id": PAY,
                        "amount_paise": 60_000}], cumulative=100_000)
    d = g.decide("create_refund", {"payment_id": PAY, "amount": 60_000}, now)
    g.mark_in_doubt(d.matched_action_id)

    assert g.ledger.state("rfa_001") is ActionState.IN_DOUBT
    assert g.ledger.encumbered_paise == 60_000, "budget must NOT be handed back"
    assert g.ledger.remaining_paise() == 40_000
    assert g.ledger.in_doubt_actions() == ["rfa_001"]

    retry = g.decide("create_refund", {"payment_id": PAY, "amount": 60_000}, now)
    assert not retry.allowed and retry.rule == ACTION_CONSUMED


def test_confirmed_rejection_returns_the_action_and_does_not_burn_it(
        guard_factory, now):
    """A request the provider definitively refused must not consume a legitimate
    merchant authorization."""
    g = guard_factory([{"action_id": "rfa_001", "payment_id": PAY,
                        "amount_paise": 20_000}])
    d = g.decide("create_refund", {"payment_id": PAY, "amount": 20_000}, now)
    g.release_confirmed_rejection(d.matched_action_id)

    assert g.ledger.state("rfa_001") is ActionState.AVAILABLE
    assert g.ledger.encumbered_paise == 0
    assert g.decide("create_refund", {"payment_id": PAY, "amount": 20_000}, now).allowed


def test_operator_resolution_is_the_only_exit_from_in_doubt(guard_factory, now):
    g = guard_factory([{"action_id": "rfa_001", "payment_id": PAY,
                        "amount_paise": 20_000}])
    d = g.decide("create_refund", {"payment_id": PAY, "amount": 20_000}, now)
    g.mark_in_doubt(d.matched_action_id)

    # No JSON-RPC-reachable method exits IN_DOUBT.
    assert not hasattr(g, "reconcile")
    g.ledger.resolve_in_doubt("rfa_001", refund_landed=True)
    assert g.ledger.state("rfa_001") is ActionState.COMMITTED
    assert g.ledger.committed_paise == 20_000


def test_mandate_cannot_be_replaced_through_the_guard(guard_factory, now):
    g = guard_factory([])
    with pytest.raises(AttributeError):
        g.mandate = build_other()  # type: ignore[assignment]


def build_other():
    from conftest import build_mandate
    return build_mandate([])
