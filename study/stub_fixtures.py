"""Generate cmd/mcp-stub/fixtures.json from FROZEN inputs.

Two sources, both read-only:

  evidence/tools_list.json   the real tool schemas captured from the official
                             pinned container, so the agent under test sees
                             byte-identical schemas to production;

  study/briefs/*.json        the frozen briefs, parsed for their payment
                             records so fetch_payment cannot contradict the
                             order details the agent was shown.

Nothing frozen is modified. Payment records are DERIVED by regex from the brief
text rather than hand-written, so they cannot drift away from it.

pay_SYN8099 is referenced only by C02's injected text and deliberately gets no
record: a support agent looking it up should find nothing, which is the honest
behaviour for an id the merchant has never seen.
"""

import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
OUT = ROOT / "cmd" / "mcp-stub" / "fixtures.json"

WANT = ["fetch_payment", "fetch_multiple_refunds_for_payment", "create_refund"]

# "(payment pay_SYN8001, 61500 paise)" and the same with a trailing colon.
PAYMENT_RE = re.compile(r"payment (pay_SYN\d+), (\d+) paise")


def main() -> int:
    tl = json.loads((ROOT / "evidence" / "tools_list.json").read_text(encoding="utf-8"))
    by_name = {t["name"]: t for t in tl["tools"]}
    missing = [n for n in WANT if n not in by_name]
    if missing:
        print("tool schemas absent from the capture: %s" % missing, file=sys.stderr)
        return 1
    tools = [by_name[n] for n in WANT]

    payments = {}
    for path in sorted((ROOT / "study" / "briefs").glob("*.json")):
        brief = json.loads(path.read_text(encoding="utf-8"))
        for pid, amount in PAYMENT_RE.findall(brief["agent_task"]):
            amount = int(amount)
            if pid in payments and payments[pid]["amount"] != amount:
                print("conflicting amounts for %s across briefs" % pid, file=sys.stderr)
                return 1
            payments[pid] = {
                "id": pid,
                "entity": "payment",
                "amount": amount,
                "currency": "INR",
                "status": "captured",
                "captured": True,
                "method": "card",
                "amount_refunded": 0,
                "refund_status": None,
            }

    if not payments:
        print("no payments parsed from the briefs; the regex has drifted", file=sys.stderr)
        return 1

    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text(
        json.dumps({"tools": tools, "payments": payments},
                   indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8", newline="\n")

    print("wrote %s" % OUT.relative_to(ROOT))
    print("  tool schemas (real, from the pinned container): %d" % len(tools))
    print("  synthetic payments derived from frozen briefs:  %d" % len(payments))
    print("  ids: %s" % ", ".join(sorted(payments)))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
