# Negative suite — clean-clone result

`clean-clone-d0dcdd0.log` is the authoritative run: a fresh clone of the public
`origin/master` tip, not the development working tree.

| | |
|---|---|
| Clone HEAD | `d0dcdd057db9ed0953a14c414c1298752701993d` |
| Source | `git clone https://github.com/Harshith029/rzp-guard` |
| Result | 8 blocked, 0 bypassed, 1 skipped, **total 9** |
| Cases reporting | N1 N2 N3 N4 N5 N6 N7 N8 N9 — each exactly once |
| Exit | 0 |

**N1 is skipped, so this is not a clean local result.** N1 needs a host that can
create symlinks; it runs in Linux CI. Cite CI for that case, never this log
alone. The suite says so itself rather than leaving it to a reader.

## Why this log exists rather than a quoted summary

Every negative-suite summary quoted between N9's introduction and its repair was
invalid. N9's command substitution tripped `set -e` — a non-zero exit is exactly
what N9 verifies — so the suite aborted after N8 and exited 0 with **no summary
at all**. Silence read as success, and an older run's numbers were repeated as
though current. `FAILURES.md` F29 and the commit adding this directory record it.

Two things now make that failure impossible to repeat quietly:

- the terminal summary is machine-checkable — every case N1–N9 must produce
  exactly one terminal state, and `blocked + bypassed + skipped` must equal 9;
- an EXIT trap outside the function fails the process if the suite leaves
  without emitting a summary, whatever exit code it would have had.

A working-tree run taken just before this one showed the same case results but
ended `EXIT=127`, because `run.sh` was edited while bash was executing it. That
run is not published here: it measured an editor, not the product.
