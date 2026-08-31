# History rewrite, 2026-08-31

Git history was rewritten once more before first publication. This file records
what changed and why, because a rewrite that is not disclosed is indistinguishable
from one that is hiding something.

## What was removed

`cmd_live_refund()` in `run.sh`. It accepted an arbitrary payment id and amount,
wrote its own mandate authorizing exactly that refund, and executed it against
the live API.

`FAILURES.md` F18 records why that is offense-capable regardless of intent: a
tool that authorizes itself is a refund launcher, not a test. It was removed
from the tip well before this rewrite. What remained was its body, still
reachable in four historical commits.

Track 2's rule is *strictly defense-only: anything offense-capable is
disqualified.* A cloned repository includes its history, so "removed from the
tip" was not the same as "not in the repository". The body is now replaced, in
every reachable commit, by a stub that explains the removal and exits non-zero.

## Why it cost nothing to do

The repository had never been pushed. No remote, no clone, no fork held the old
hashes, so no published reference was invalidated. This was the last moment the
rewrite was free.

## What the rewrite did NOT change

The tip. The tree at `HEAD` is byte-identical before and after:

    tree before  19de39a9af74b404607cec0a2bf4a25d8c74fdcb
    tree after   19de39a9af74b404607cec0a2bf4a25d8c74fdcb

All 82 commits, their messages, authors and dates survive. Only commit ids
changed, and only from the earliest rewritten commit forward.

## The commit-id remap

Commit ids are not the study's integrity anchor, but two are quoted as
provenance in the generated reports. Both were rewritten:

| Recorded in | Pre-rewrite id | Post-rewrite id |
|---|---|---|
| `study/RESULTS.md` (arm A) | `ee44b5ca4993c6996fce98e590239815e8a31563` | `cd1d987ca0b8...` |
| `study/RESULTS-armB.md` (arm B) | `f330d960787ba628b394c54791460ffe173088b0` | `0b33681079e7...` |

**The trace files were not edited.** Each trace records the freeze commit that
was current when it ran, and that is frozen evidence. Rewriting 90 trace files
so a provenance value resolves cleanly is exactly the retroactive tidying the
pre-registration design exists to prevent. The stale id is left in place and
explained here instead.

## Why the freeze still verifies

Freeze integrity was never anchored on a commit id. `cmd/rzp-study/main.go`
compares a manifest hash against the hash of the file contents, and
`enforce.go` requires only that the model freeze be tracked, unmodified, and
present in `HEAD`. None of those depend on which commit id carries the file.

Verified after the rewrite:

    freeze intact: 34 files, freeze_sha256 92900c58a7665a7adcd321c0e87815c2b17771ed28c6f98bbd3d0ca8b95720b2

which is the protocol freeze `study/RESULTS-armB.md` records, unchanged.

## The earlier rewrite

This is the second. The first removed a real payment id and key material from
history; `FAILURES.md` F21 records it. `git filter-repo` prompts on a repository
it has already filtered, and this run answered that prompt as a continuation.
