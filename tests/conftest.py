"""Shared fixtures for the fast (offline) lane.

Nothing here touches Docker, the network, Razorpay credentials or a model key.
"""

from datetime import datetime, timedelta, timezone

import pytest

from rzp_guard.mandate import load_mandate
from rzp_guard.policy import Guard

NOW = datetime(2026, 8, 24, 12, 0, 0, tzinfo=timezone.utc)


def build_mandate(actions, *, cumulative=500_000, per_minute=10, expires=None,
                  tools=None):
    return load_mandate({
        "mandate_id": "mnd_test",
        "expires_at": (expires or NOW + timedelta(hours=4)).isoformat(),
        "allowed_tools": tools or ["fetch_payment", "fetch_all_payments", "create_refund"],
        "authorized_refund_actions": actions,
        "global": {
            "max_cumulative_paise": cumulative,
            "max_calls_per_minute": per_minute,
        },
    })


@pytest.fixture
def now():
    return NOW


@pytest.fixture
def guard_factory():
    def _make(actions, **kw):
        return Guard(build_mandate(actions, **kw))
    return _make


def payment_entity(pay_id, amount, notes=None, **extra):
    """Shaped like a real Razorpay payment entity (PLAN.md 2.5 / corpus fixtures)."""
    return {
        "id": pay_id,
        "entity": "payment",
        "amount": amount,
        "currency": "INR",
        "status": "captured",
        "order_id": "order_SYN" + pay_id[-4:],
        "created_at": 1756000000,
        "notes": notes or {},
        **extra,
    }
