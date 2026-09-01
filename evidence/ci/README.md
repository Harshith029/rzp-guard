# CI evidence — Linux

The negative suite cannot report `N1` on Windows: that case needs a host able to
create symlinks. Linux CI is where it actually runs, so the Linux result is the
one that matters, and it is preserved here rather than cited.

## `redteam-negative-linux-33492881275.log`

Run: <https://github.com/Harshith029/rzp-guard/actions/runs/33492881275>
Job: `redteam-lane-isolated`
Commit: `5063e875766ee26fd068d89dc4c259f3faf7c3de`

```
BLOCKED   N1 untracked symlink refused
BLOCKED   N2 credential in a spaced filename refused
BLOCKED   N3 untracked sentinel reaches the export (dirty tree visible)
BLOCKED   N4 redteam child has exactly one launch and no configurable path
BLOCKED   N5 cmd_fuzz routes through the isolated lane
BLOCKED   N6 stub refuses a non-fixture id with a parsed result.isError
BLOCKED   N7 this function runs untrusted code only via cmd_redteam
BLOCKED   N8 gitignored files absent, .env absent, no DNS
BLOCKED   N9 the history scan refuses an interpolated refund grant
BLOCKED   N10 the suite invokes run.sh with its declared interpreter

blocked: 10   bypassed: 0   skipped: 0   total: 10
cases reporting: N1 N2 N3 N4 N5 N6 N7 N8 N9 N10
```

**Zero skipped**, because N1 runs here. No post-summary error.

## What this replaces

Every CI run before `5063e87` **failed**, and the failures were never looked at.
On Linux the suite reported:

```
BYPASSED  N4 redteam child structure violated
BYPASSED  N6 stub accepted a fabricated pay_SYN id...
##[error]Process completed with exit code 125
```

Those were not security findings. `N4`, `N6` and `N8` drive `cmd_redteam`, which
runs `docker run --pull=never` by design; on a fresh runner the pinned image is
absent and docker exits 125. The suite printed "the bypass worked" when the
truth was "the lane could not start".

Three fixes, in `5063e87`:

- `require_redteam_lane` checks docker and the pinned image up front and exits
  with an explicit **environment-failure** message instead of emitting verdicts
  it did not test;
- CI pulls the pinned image by digest in a separate, visible step — the
  execution stage keeps `--pull=never` unchanged, and the digest is read out of
  `run.sh` so it cannot drift;
- `rzp-study.exe`, a 10MB build artifact committed by accident, was removed from
  the tip and `.gitignore` now closes the whole root-binary class.

`FAILURES.md` records the wider lesson: an environment failure reported as a
security finding is worse than no test, and CI being red for weeks while local
runs looked green is what made it invisible.
