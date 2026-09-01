# Negative suite — both platforms, from a fresh clone of the public tip

Commit `5063e875766ee26fd068d89dc4c259f3faf7c3de` on both.

## Linux — the one that matters

`../ci/redteam-negative-linux-33492881275.log`
<https://github.com/Harshith029/rzp-guard/actions/runs/33492881275>

```
blocked: 10   bypassed: 0   skipped: 0   total: 10
cases reporting: N1 N2 N3 N4 N5 N6 N7 N8 N9 N10
```

**Zero skipped.** `N1` needs a host that can create symlinks, so Linux is the
only platform where it runs at all — which is why the Linux result is the
authoritative one and is preserved rather than cited.

## Windows, native, fresh clone

`native-windows-5063e87.log`

```
=== preflight ===          EXIT_PREFLIGHT=0
=== redteam-negative ===   EXIT_NEGATIVE=0
=== all (Go test lanes) === EXIT_ALL=0   22 packages ok, no FAIL, no race

SKIP      N1 (this host cannot create symlinks; CI runs it on Linux)
BLOCKED   N2 ... N10
blocked: 9   bypassed: 0   skipped: 1   total: 10
cases reporting: N1 N2 N3 N4 N5 N6 N7 N8 N9 N10
NOT a clean result: 1 case(s) did not run here.
```

A run with any skip prints that it is not a clean result. Both platforms
together are what makes the claim; neither alone does.

## Why the earlier local log was withdrawn

`clean-clone-d0dcdd0.log` was removed. It recorded 9 cases with N1 skipped and
exited 0, and I published it as the verification. It was Windows-only, and on
Linux the same commit reported `N4` and `N6` BYPASSED and died at exit 125 —
docker refusing a `--pull=never` run with no local image. Publishing a
single-platform pass as the result was the error; the file is gone rather than
kept alongside a correction, because the numbers in it were never the whole
picture.

`N10` did not exist then. It was added after `N9` was found to invoke `run.sh`
through `sh`, which is dash on Linux, so `N9` had never tested anything there.
