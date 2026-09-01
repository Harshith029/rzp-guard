# Negative suite — both platforms

Every case is a bypass that once worked. All of them must now fail.

Raw run logs are not committed: they carry host paths and environment detail,
and `.gitignore` excludes `*.log`. The figures and the stable CI link are the
record.

## Linux — authoritative

<https://github.com/Harshith029/rzp-guard/actions/runs/33496985165> · commit
`77dd09c` · see `../ci/README.md`

```
blocked: 10   bypassed: 0   skipped: 0   total: 10
```

`N1` runs only here — it needs symlink creation — so **zero skipped** is a
property of Linux, not of the suite.

## Windows, native, fresh clone of the same commit

```
preflight          EXIT=0
redteam-negative   EXIT=0
all (Go lanes)     EXIT=0     22 packages ok, no FAIL, no race

SKIP      N1 (this host cannot create symlinks; CI runs it on Linux)
BLOCKED   N2 ... N10
blocked: 9   bypassed: 0   skipped: 1   total: 10
cases reporting: N1 N2 N3 N4 N5 N6 N7 N8 N9 N10
```

A run with any skip prints that it is **not a clean result**. Both platforms
together support the claim; neither alone does.

## The suite refuses to be quoted from a partial run

Learned the hard way. `N9` once aborted the whole suite at its own assignment —
under `set -e`, `x="$(cmd)"` terminates the shell when `cmd` exits non-zero, and
a non-zero exit is exactly what `N9` verifies. The suite exited **0 with no
summary at all**, and an older run's numbers were quoted as current.

Now every case must produce exactly one terminal state, `blocked + bypassed +
skipped` must equal the case count, and an `EXIT` trap fails the process if the
suite leaves without emitting a summary — whatever exit code it would have had.
