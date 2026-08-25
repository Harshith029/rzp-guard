"""Produce the committed evidence projection from raw provider responses.

    python evidence/redact.py            # rebuild projections from raw/
    python evidence/redact.py --check    # fail if a projection is stale or leaky

Raw MCP responses carry a payment's full record: contact details, card id,
acquirer auth code, order and invoice ids, fees and tax. Test Mode placeholders
are still not a reason to publish provider records by default.

RAW CAPTURE LIVES OUTSIDE THE WORKSPACE ENTIRELY.

It used to live in gitignored `evidence/*/raw/` directories. That was wrong for
this repository in a way gitignore cannot fix: the working tree is OneDrive-
backed, so a file that is never committed is still uploaded. Verified before the
move -- three raw files under evidence/ held a live email address, phone number,
card last-four and acquirer auth code while sitting in a synced directory. The
identical mistake was already recorded for operator credentials (FAILURES.md
F12); this is the same failure one directory over.

Raw now goes to RAW_ROOT, which defaults outside the repo and is REFUSED if it
resolves inside it. Only a MINIMIZED PROJECTION is written under evidence/.

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
import os
import re
import pathlib
import sys
import tempfile

ROOT = pathlib.Path(__file__).resolve().parent
REPO = ROOT.parent


def raw_root() -> pathlib.Path:
    """Where captured provider traffic lives. Never inside the repository.

    A path inside the workspace is refused rather than warned about: on this
    machine the workspace is cloud-synced, so "gitignored" and "not published"
    are different things, and only the first is under git's control.
    """
    env = os.environ.get("RZP_RAW_EVIDENCE_DIR", "").strip()
    root = pathlib.Path(env) if env else pathlib.Path(tempfile.gettempdir()) / "rzp-guard-raw"
    root = root.resolve()
    try:
        root.relative_to(REPO)
    except ValueError:
        return root
    raise SystemExit(
        "RZP_RAW_EVIDENCE_DIR resolves inside the repository (%s).\n"
        "Raw provider records must not live in the workspace: this tree is\n"
        "cloud-synced, so gitignore does not stop them being uploaded." % root)


RAW_ROOT = raw_root()

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
        raw = RAW_ROOT / sub
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
    stray = scan_stray_raw()
    if check:
        bad = False
        if stray:
            print("RAW CAPTURE INSIDE THE WORKSPACE (this tree is cloud-synced):",
                  file=sys.stderr)
            for d in stray:
                print("  " + d, file=sys.stderr)
            bad = True
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

    print("rebuilt projections from %s" % RAW_ROOT)
    print("  leaks after rebuild: %d" % len(leaks))
    if stray:
        print("  STRAY RAW DIRECTORIES IN THE WORKSPACE: %s" % ", ".join(stray),
              file=sys.stderr)
    return 1 if (leaks or stray) else 0


DROPPED = ["email", "contact", "card_id", "auth_code", "acquirer_data",
           "order_id", "invoice_id", "customer_id", "description", "card",
           "notes", "fee", "tax", "vpa", "wallet", "bank", "last4", "token_id"]


# Values that are personal data whatever key they hide behind.
PII_VALUE = re.compile(
    r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}"      # email address
    r"|(?<!\d)(?:\+?91)?[6-9]\d{9}(?!\d)"                  # IN mobile number
)


def _walk_for_leaks(obj, where: str, out: list[str], in_entity: bool = False) -> None:
    """Two independent rules, because one alone gives the wrong answer.

    1. A DROPPED field is a leak when it sits inside a provider ENTITY -- the
       same objects prune() strips. Matching on field NAMES alone flagged
       Razorpay's published tool schemas, which legitimately declare parameters
       called `email`, `contact` and `description`. That is noise, and noise is
       how a real leak gets waved through.

    2. A value that LOOKS like personal data is a leak wherever it appears, under
       any key. Rule 1 depends on the provider's object shape; rule 2 does not,
       so a record arriving in an unexpected wrapper is still caught.
    """
    if isinstance(obj, dict):
        here = in_entity or "entity" in obj
        for k, v in obj.items():
            if here and k in DROPPED:
                out.append("%s: %s (in a provider entity)" % (where, k))
            if isinstance(v, str):
                # A `description` OUTSIDE a provider entity is schema
                # documentation, and Razorpay's published schemas contain
                # example values -- "For example, 9876543210", the Indian
                # equivalent of 555-0100. Flagging those is noise, and noise is
                # how a real leak gets waved through. Inside an entity, a
                # description is payment data and rule 1 already catches it.
                documentation = k == "description" and not here
                if not documentation and PII_VALUE.search(v):
                    out.append("%s: personal-data-shaped value under %r" % (where, k))
                # A tool result carries its entity as JSON *inside* a text field.
                if v.lstrip()[:1] in ("{", "["):
                    try:
                        _walk_for_leaks(json.loads(v), where, out, here)
                        continue
                    except (json.JSONDecodeError, ValueError):
                        pass
            _walk_for_leaks(v, where, out, here)
    elif isinstance(obj, list):
        for x in obj:
            _walk_for_leaks(x, where, out, in_entity)


def scan_leaks() -> list[str]:
    """Independent check on what is actually published, not on what we intended.

    Scans EVERY readable file under evidence/, not just the .jsonl projections.
    The earlier version looked only at committed .jsonl while the runner copied
    every raw *.txt into the published directory untouched -- so a provider error
    or diagnostic carrying a payment record would have been republished without
    ever passing through the redactor.
    """
    out: list[str] = []
    for path in sorted(ROOT.rglob("*")):
        if not path.is_file() or path.suffix in (".py", ".md"):
            continue
        try:
            txt = path.read_text(encoding="utf-8", errors="strict")
        except (UnicodeDecodeError, OSError):
            continue
        rel = str(path.relative_to(ROOT))
        parsed = False
        for line in txt.splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                _walk_for_leaks(json.loads(line), rel, out)
                parsed = True
            except (json.JSONDecodeError, ValueError):
                continue
        if not parsed:
            try:
                _walk_for_leaks(json.loads(txt), rel, out)
            except (json.JSONDecodeError, ValueError):
                # Not JSON. Nothing under evidence/ should be free text, so fall
                # back to the blunt check rather than skipping the file.
                if PII_VALUE.search(txt):
                    out.append("%s: personal-data-shaped value (unparseable file)" % rel)
                for field in DROPPED:
                    if '"%s"' % field in txt:
                        out.append("%s: %s (unparseable file)" % (rel, field))
    return sorted(set(out))


def scan_stray_raw() -> list[str]:
    """A raw capture directory must not exist inside the workspace at all."""
    return [str(d.relative_to(REPO)) for d in ROOT.rglob("raw") if d.is_dir()]


if __name__ == "__main__":
    raise SystemExit(process("--check" in sys.argv))
