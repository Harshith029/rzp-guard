"""Hash-commit the Phase 4b freeze, and verify it later.

    python study/freeze.py freeze    # write study/manifest.json
    python study/freeze.py verify    # exit 1 if anything drifted

`verify` is the point. A freeze nobody can check is a promise, not a control.
The trace runner calls it before the first trace and refuses to run on drift, so
a brief edited after the fact cannot quietly become the ground truth it is
supposed to have preceded.

Deliberately excluded from the manifest:
  - study/model.frozen.json, which is written by the resolution step in
    PROTOCOL.md §4 and is hashed into the manifest by that step, not this one;
  - anything under study/traces/, which does not exist until traces run.
"""

import hashlib
import json
import pathlib
import sys

HERE = pathlib.Path(__file__).resolve().parent
MANIFEST = HERE / "manifest.json"

# Every input that must be fixed before the first trace.
MEMBERS = [
    "PROTOCOL.md",
    "briefs.py",
    "compile_mandate.py",
    "compiled_index.json",
]


def members() -> list[str]:
    names = list(MEMBERS)
    names += sorted("briefs/%s" % p.name for p in (HERE / "briefs").glob("*.json"))
    names += sorted("mandates/%s" % p.name for p in (HERE / "mandates").glob("*.json"))
    return names


def digest(rel: str) -> str:
    return hashlib.sha256((HERE / rel).read_bytes()).hexdigest()


def build() -> dict:
    files = {rel: digest(rel) for rel in members()}
    # A digest over the per-file digests, so a single value identifies the whole
    # freeze -- including whether a file was ADDED or REMOVED, which a per-file
    # comparison alone would miss.
    joined = "".join("%s:%s\n" % (k, files[k]) for k in sorted(files))
    return {
        "phase": "4b",
        "frozen_before_first_trace": True,
        "protocol": "study/PROTOCOL.md",
        "declared_trace_count": 45,
        "files": files,
        "freeze_sha256": hashlib.sha256(joined.encode("utf-8")).hexdigest(),
    }


def cmd_freeze() -> int:
    m = build()
    MANIFEST.write_text(
        json.dumps(m, indent=2, sort_keys=True) + "\n", encoding="utf-8", newline="\n")
    print("froze %d files" % len(m["files"]))
    print("freeze_sha256 %s" % m["freeze_sha256"])
    return 0


def cmd_verify() -> int:
    if not MANIFEST.exists():
        print("no manifest; run: python study/freeze.py freeze", file=sys.stderr)
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
        print("FREEZE VIOLATED", file=sys.stderr)
        for b in bad:
            print("  " + b, file=sys.stderr)
        return 1

    print("freeze intact: %d files, freeze_sha256 %s"
          % (len(recorded["files"]), recorded["freeze_sha256"]))
    return 0


if __name__ == "__main__":
    mode = sys.argv[1] if len(sys.argv) > 1 else "verify"
    if mode == "freeze":
        raise SystemExit(cmd_freeze())
    if mode == "verify":
        raise SystemExit(cmd_verify())
    print("usage: freeze.py (freeze|verify)", file=sys.stderr)
    raise SystemExit(2)
