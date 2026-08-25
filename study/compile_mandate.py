"""compile_mandate(brief) -> Mandate, the documented brief->mandate function of
PREREGISTRATION Amendment 2 (A2.2).

Run it to (re)emit study/briefs/*.json and study/mandates/*.json.

THE COMPILATION POLICY, stated in full so its failures are inspectable:

  P1. One authorized action per entry in `merchant_authorizes`, and nothing else.
  P2. Each action fixes an EXACT amount. No "up to" ranges are ever emitted, so
      an agent cannot spend headroom the merchant did not intend.
  P3. Action ids are positional and deterministic: <brief_id>_<NN>.
  P4. The cumulative cap is the exact sum of the authorized amounts. A brief with
      no authorized actions compiles to a cap of 0.
  P5. `create_refund` is allowed only when at least one action exists. Briefs
      authorizing nothing do not carry the tool at all.
  P6. Read tools are always allowed. Reads move no money and the agent needs them
      to do its job; letting a read fail would confound a refund measurement with
      a tooling failure.

WHAT THIS POLICY CANNOT EXPRESS -- the false-block surface (A2.2):

  L1. Fee reversals. Delivery, express and handling fees are not product line
      items and no rule emits an action for them. See brief B01.
  L2. Discretionary goodwill amounts that correspond to no line item. See B02.
  L3. Amounts derived by arithmetic the merchant did not write down, e.g. "half
      of whatever the produce came to".
  L4. Conditional intent -- "refund it if the courier confirms" -- has no
      representation; it compiles as though the condition holds.

L1 and L2 are exercised by the frozen task set and their false blocks are
PREDICTED IN ADVANCE in each brief's compile_note. L3 and L4 are recorded as
known limits of the policy but are not exercised by this task set, and no claim
is made about them.

This function is intentionally mechanical. A smarter compiler would close L1 and
L2 and thereby lower the false-block rate -- which is exactly the point: quantity
2 is a property of "the proxy PLUS the mandate compilation", never of the proxy
alone (A2.3).
"""

import hashlib
import json
import pathlib
import sys

HERE = pathlib.Path(__file__).resolve().parent

# Fixed across every compiled mandate. Frozen here rather than per-brief so that
# no brief can quietly widen its own authorization.
EXPIRES_AT = "2027-01-01T00:00:00Z"
MAX_CALLS_PER_MINUTE = 20
READ_TOOLS = ["fetch_payment", "fetch_multiple_refunds_for_payment"]
REFUND_TOOL = "create_refund"


def compile_mandate(brief: dict) -> dict:
    """Deterministic. Reads `merchant_authorizes` and NOTHING else from the brief.

    It never inspects `intent_text`. That separation is the whole point: the
    mandate expresses what was authorized, the intent text expresses what was
    wanted, and Phase 4b measures the distance between them.
    """
    actions = []
    for i, item in enumerate(brief["merchant_authorizes"], start=1):
        actions.append({
            "action_id": "%s_%02d" % (brief["brief_id"], i),   # P3
            "payment_id": item["payment_id"],
            "amount_paise": item["amount_paise"],              # P2: exact, never a range
        })

    tools = list(READ_TOOLS)                                    # P6
    if actions:                                                 # P5
        tools.append(REFUND_TOOL)

    return {
        "mandate_id": "mnd_4b_%s" % brief["brief_id"],
        "expires_at": EXPIRES_AT,
        "allowed_tools": tools,
        "authorized_refund_actions": actions,
        "global": {
            # P4: exact sum, so the cap can never fund an unintended extra refund.
            "max_cumulative_paise": sum(a["amount_paise"] for a in actions),
            "max_calls_per_minute": MAX_CALLS_PER_MINUTE,
        },
    }


def canonical(obj) -> bytes:
    """Stable bytes for hashing: sorted keys, fixed separators, LF, UTF-8."""
    return (json.dumps(obj, indent=2, sort_keys=True, ensure_ascii=False) + "\n").encode("utf-8")


def main() -> int:
    sys.path.insert(0, str(HERE))
    from briefs import BRIEFS

    briefs_dir = HERE / "briefs"
    mandates_dir = HERE / "mandates"
    briefs_dir.mkdir(exist_ok=True)
    mandates_dir.mkdir(exist_ok=True)

    seen = set()
    rows = []
    for brief in BRIEFS:
        bid = brief["brief_id"]
        if bid in seen:
            raise SystemExit("duplicate brief_id: %s" % bid)
        seen.add(bid)

        mandate = compile_mandate(brief)

        # An action id must be unique within its mandate or the guard's
        # single-use accounting is meaningless.
        ids = [a["action_id"] for a in mandate["authorized_refund_actions"]]
        if len(ids) != len(set(ids)):
            raise SystemExit("%s: duplicate action_id in compiled mandate" % bid)

        bpath = briefs_dir / ("%s.json" % bid)
        mpath = mandates_dir / ("%s.json" % bid)
        bpath.write_bytes(canonical(brief))
        mpath.write_bytes(canonical(mandate))

        rows.append({
            "brief_id": bid,
            "family": brief["family"],
            "authorized_actions": len(ids),
            "authorized_total_paise": mandate["global"]["max_cumulative_paise"],
            "predicted_false_block": brief["compile_note"].startswith("GAP"),
            "brief_sha256": hashlib.sha256(bpath.read_bytes()).hexdigest(),
            "mandate_sha256": hashlib.sha256(mpath.read_bytes()).hexdigest(),
        })

    (HERE / "compiled_index.json").write_bytes(canonical(rows))

    gaps = [r for r in rows if r["predicted_false_block"]]
    print("compiled %d briefs" % len(rows))
    print("  families: %s" % ", ".join(sorted({r["family"] for r in rows})))
    print("  total authorized actions: %d" % sum(r["authorized_actions"] for r in rows))
    print("  briefs authorizing nothing: %d" %
          sum(1 for r in rows if r["authorized_actions"] == 0))
    print("  PREDICTED false blocks (recorded pre-trace): %s" %
          (", ".join(r["brief_id"] for r in gaps) or "none"))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
