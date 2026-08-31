"""Compile the arm C grid into briefs and mandates.

Deliberately a SEPARATE script from compile_mandate.py, which it imports rather
than edits. compile_mandate.py is a member of arm A/B's freeze: changing a byte
of it would invalidate `freeze_sha256` for two completed arms and retroactively
break results that are already published. Arm C therefore reuses the
compilation policy without touching the file that defines it.

The compilation policy is unchanged (P1-P6 in compile_mandate.py). What differs
is only the source of briefs -- a mechanical cross product from grid.py instead
of a hand-authored list -- and the output directories.

Run:  python study/compile_armC.py
"""

import hashlib
import json
import pathlib
import sys

HERE = pathlib.Path(__file__).resolve().parent


def main() -> int:
    sys.path.insert(0, str(HERE))
    from compile_mandate import canonical, compile_mandate
    from grid import BRIEFS

    briefs_dir = HERE / "briefs-armC"
    mandates_dir = HERE / "mandates-armC"
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

        ids = [a["action_id"] for a in mandate["authorized_refund_actions"]]
        if len(ids) != len(set(ids)):
            raise SystemExit("%s: duplicate action_id in compiled mandate" % bid)

        bpath = briefs_dir / ("%s.json" % bid)
        mpath = mandates_dir / ("%s.json" % bid)
        bpath.write_bytes(canonical(brief))
        mpath.write_bytes(canonical(mandate))

        rows.append({
            "brief_id": bid,
            "cell": brief["cell"],
            "family": brief["family"],
            "authorized_actions": len(ids),
            "authorized_total_paise": mandate["global"]["max_cumulative_paise"],
            # Arm C states its predicted false blocks in the compile note with
            # this exact prefix, emitted by grid.py's `under` coverage level.
            "predicted_false_block":
                brief["compile_note"].startswith("PREDICTED FALSE BLOCK"),
            "brief_sha256": hashlib.sha256(bpath.read_bytes()).hexdigest(),
            "mandate_sha256": hashlib.sha256(mpath.read_bytes()).hexdigest(),
        })

    (HERE / "compiled_index-armC.json").write_bytes(canonical(rows))

    gaps = [r for r in rows if r["predicted_false_block"]]
    pressure = [r for r in rows if r["cell"]["pressure"] != "none"]
    print("compiled %d briefs" % len(rows))
    print("  cells applying pressure: %d" % len(pressure))
    print("  PREDICTED false blocks, recorded pre-trace: %d" % len(gaps))
    print("  total authorized actions: %d"
          % sum(r["authorized_actions"] for r in rows))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
