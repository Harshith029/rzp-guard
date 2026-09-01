# CI evidence — Linux

`N1` needs a host that can create symlinks, so Linux CI is the only place it
runs. That makes the Linux result the authoritative one for the negative suite.

**Raw job logs are deliberately not committed.** They carry runner paths,
environment detail and timing noise, and they go stale the moment a job re-runs.
What is recorded here is the stable run URL and the figures, which anyone can
re-open and re-check.

## Negative suite, Linux

| | |
|---|---|
| Run | <https://github.com/Harshith029/rzp-guard/actions/runs/33496985165> |
| Job | `redteam-lane-isolated` |
| Commit | `77dd09c05a96fa16073bbfb2f036785e5ccccd66` |

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

Zero skipped, and no error after the summary.

## What this replaced

Every CI run before `5063e87` failed, and nobody had looked. On Linux the suite
reported `N4` and `N6` as BYPASSED and died at exit 125.

Those were never security findings. `N4`, `N6` and `N8` drive `cmd_redteam`,
which runs `docker run --pull=never` by design; on a fresh runner the pinned
image is absent and docker exits 125. The suite printed "the bypass worked" when
the truth was "the lane could not start".

Three fixes, in `5063e87`:

- `require_redteam_lane` checks docker and the pinned image up front and exits
  with an explicit environment-failure message instead of emitting verdicts it
  did not test;
- CI pulls the pinned image by digest in a separate, visible step. The execution
  stage keeps `--pull=never` unchanged and the digest is read out of `run.sh`,
  so it cannot drift;
- a 10MB `rzp-study.exe`, committed by accident, was removed and `.gitignore`
  now excludes the whole root-binary class.
