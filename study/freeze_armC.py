"""Arm C's freeze, kept separate from arm A/B's.

A single shared manifest cannot work. Adding arm C's briefs to
study/manifest.json would change `freeze_sha256`, and two completed arms record
that value in their published results -- so a later arm would retroactively
invalidate earlier ones. arms.json already states that principle; this file is
what makes it true for arm C.

Members are the inputs that must be fixed before arm C's first trace:

  PROTOCOL-armC.md        the pre-registration, including the predictions
  grid.py                 the enumeration -- the corpus is a pure function of it
  compile_armC.py         brief -> mandate, for this arm
  compiled_index-armC.json
  briefs-armC/*.json
  mandates-armC/*.json

compile_mandate.py is NOT a member even though arm C uses its policy: it
belongs to arm A/B's freeze, and a file cannot be governed by two manifests
without one of them being able to break the other. compile_armC.py imports it,
and any change to it would break arm A/B's verification first.

Run:  python study/freeze_armC.py freeze
      python study/freeze_armC.py verify
"""

import hashlib
import json
import pathlib
import sys

HERE = pathlib.Path(__file__).resolve().parent
MANIFEST = HERE / "manifest-armC.json"

MEMBERS = [
    "PROTOCOL-armC.md",
    "grid.py",
    "compile_armC.py",
    "compiled_index-armC.json",
]

# 54 briefs x 3 runs each. Declared here, before any trace exists, so a short
# run cannot later be presented as the whole corpus.
DECLARED_TRACE_COUNT = 162


def members() -> list[str]:
    names = list(MEMBERS)
    names += sorted("briefs-armC/%s" % p.name
                    for p in (HERE / "briefs-armC").glob("*.json"))
    names += sorted("mandates-armC/%s" % p.name
                    for p in (HERE / "mandates-armC").glob("*.json"))
    return names


def digest(rel: str) -> str:
    return hashlib.sha256((HERE / rel).read_bytes()).hexdigest()


def build() -> dict:
    files = {rel: digest(rel) for rel in members()}
    joined = "".join("%s:%s\n" % (k, files[k]) for k in sorted(files))
    return {
        "arm": "C",
        "frozen_before_first_trace": True,
        "protocol": "study/PROTOCOL-armC.md",
        "declared_trace_count": DECLARED_TRACE_COUNT,
        "files": files,
        "freeze_sha256": hashlib.sha256(joined.encode("utf-8")).hexdigest(),
    }


def cmd_freeze() -> int:
    m = build()
    MANIFEST.write_text(
        json.dumps(m, indent=2, sort_keys=True) + "\n",
        encoding="utf-8", newline="\n")
    print("froze %d files" % len(m["files"]))
    print("freeze_sha256 %s" % m["freeze_sha256"])
    return 0


def cmd_verify() -> int:
    if not MANIFEST.exists():
        print("no arm C manifest; run: python study/freeze_armC.py freeze",
              file=sys.stderr)
        return 1
    recorded = json.loads(MANIFEST.read_text(encoding="utf-8"))
    current = build()

    bad = []
    for rel, want in sorted(recorded["files"].items()):
        path = HERE / rel
        if not path.exists():
            bad.append("MISSING  %s" % rel)
        elif digest(rel) != want:
            bad.append("CHANGED  %s" % rel)
    for rel in sorted(current["files"]):
        if rel not in recorded["files"]:
            bad.append("ADDED    %s (not in the freeze)" % rel)

    if bad:
        print("ARM C FREEZE VIOLATED", file=sys.stderr)
        for b in bad:
            print("  %s" % b, file=sys.stderr)
        return 1

    print("arm C freeze intact: %d files, freeze_sha256 %s"
          % (len(recorded["files"]), recorded["freeze_sha256"]))
    print("declared trace count: %d" % recorded["declared_trace_count"])
    return 0


def main() -> int:
    cmd = sys.argv[1] if len(sys.argv) > 1 else ""
    if cmd == "freeze":
        return cmd_freeze()
    if cmd == "verify":
        return cmd_verify()
    print("usage: freeze_armC.py freeze|verify", file=sys.stderr)
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
