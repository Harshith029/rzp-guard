"""Produce the committed evidence projection from raw provider responses.

    python evidence/redact.py            # rebuild projections from raw/
    python evidence/redact.py --check    # fail if a projection is stale or leaky

Raw MCP responses carry a payment's full record: contact details, card id,
acquirer auth code, order and invoice ids, fees and tax. Test Mode placeholders
are still not a reason to publish provider records by default, so raw responses
live in gitignored `raw/` subdirectories and only a MINIMIZED PROJECTION is
committed.

The projection is a strict allowlist, not a denylist. A denylist silently ships
any field the provider adds later; an allowlist fails closed by dropping it.

WHAT SURVIVES, and why each field is needed:

  entity, id            proof the provider created a real object rather than
                        echoing our request back
  payment_id, amount    proof the refund matched what was authorized
  receipt               the guard's injected idempotency token, round-tripped
  status, currency      COMMITTED-means-created evidence (status was "pending")
  count, items          collection shape for the refunds-list read

Everything else is dropped. The gates are re-run against the projection, so the
committed evidence is exactly what the assertions were checked on -- there is no
private artifact doing the real work.
"""

import json
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parent

KEEP = {
    "entity", "id", "payment_id", "amount", "receipt", "status", "currency",
    "count", "items",
}

# Directories holding captured provider traffic: (dir, filenames)
TARGETS = {
    "g16": ["fetch_stdout.jsonl", "refund_stdout.jsonl", "replay_stdout.jsonl",
            "refund_child_stdin.jsonl", "replay_child_stdin.jsonl",
            "refund_decisions.jsonl"],
    "linux": ["block_stdout.jsonl", "block_child_stdin.jsonl",
              "block_decisions.jsonl"],
}


def prune(obj):
    """Recursively keep only allowlisted keys inside provider entities."""
    if isinstance(obj, list):
        return [prune(o) for o in obj]
    if not isinstance(obj, dict):
        return obj
    # Only prune objects that look like a provider entity. Everything else --
    # JSON-RPC envelopes, tool arguments -- is structural and kept as-is.
    if "entity" in obj:
        return {k: prune(v) for k, v in obj.items() if k in KEEP}
    return {k: prune(v) for k, v in obj.items()}


def redact_line(line: str) -> str:
    try:
        d = json.loads(line)
    except json.JSONDecodeError:
        return line

    def walk(o):
        if isinstance(o, dict):
            # A tool result carries its entity as JSON *inside* a text field.
            if "content" in o and isinstance(o["content"], list):
                for c in o["content"]:
                    if isinstance(c, dict) and c.get("type") == "text":
                        try:
                            inner = json.loads(c["text"])
                        except (json.JSONDecodeError, TypeError):
                            continue
                        # Compact separators: the provider emits compact JSON and
                        # the gates match on it. Re-serializing with spaces broke
                        # the refund gate, which is why the gates are re-run
                        # against the projection rather than assumed equivalent.
                        c["text"] = json.dumps(prune(inner), separators=(",", ":"))
            return {k: walk(v) for k, v in o.items()}
        if isinstance(o, list):
            return [walk(x) for x in o]
        return o

    return json.dumps(walk(d), separators=(',', ':'))


def process(check: bool) -> int:
    stale, missing_raw = [], []
    for sub, names in TARGETS.items():
        d = ROOT / sub
        raw = d / "raw"
        for name in names:
            src, dst = raw / name, d / name
            if not src.exists():
                if dst.exists():
                    missing_raw.append(str(dst.relative_to(ROOT)))
                continue
            want = "".join(redact_line(l) + "\n"
                           for l in src.read_text(encoding="utf-8").splitlines() if l.strip())
            if check:
                have = dst.read_text(encoding="utf-8") if dst.exists() else ""
                if have != want:
                    stale.append(str(dst.relative_to(ROOT)))
            else:
                dst.write_text(want, encoding="utf-8", newline="\n")

    leaks = scan_leaks()
    if check:
        bad = False
        if stale:
            print("STALE projections (raw changed, committed copy did not):", file=sys.stderr)
            for s in stale:
                print("  " + s, file=sys.stderr)
            bad = True
        if leaks:
            print("LEAK: dropped fields present in committed evidence:", file=sys.stderr)
            for l in leaks:
                print("  " + l, file=sys.stderr)
            bad = True
        if bad:
            return 1
        note = " (raw not present locally; projection checked for leaks only)" if missing_raw else ""
        print("evidence projection clean%s" % note)
        return 0

    print("rebuilt projections; leaks after rebuild: %d" % len(leaks))
    return 1 if leaks else 0


DROPPED = ["email", "contact", "card_id", "auth_code", "acquirer_data",
           "order_id", "invoice_id", "customer_id", "description", "card",
           "notes", "fee", "tax", "vpa", "wallet", "bank", "last4", "token_id"]


def scan_leaks() -> list[str]:
    """Independent check on what is actually committed, not on what we intended."""
    out = []
    for sub in TARGETS:
        for path in sorted((ROOT / sub).glob("*.jsonl")):
            txt = path.read_text(encoding="utf-8")
            for field in DROPPED:
                # Match both plain and escaped-in-text occurrences.
                if '"%s"' % field in txt or '\\"%s\\"' % field in txt:
                    out.append("%s: %s" % (path.relative_to(ROOT), field))
    return out


if __name__ == "__main__":
    raise SystemExit(process("--check" in sys.argv))
