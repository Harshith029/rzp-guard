# Arm D — assessment and retraction

**Dated 2026-09-01. This document supersedes the metric claims in
`RESULTS-armD.md`, `PROTOCOL-armD.md` and `FINDINGS-armD.md`.**

> **Arm D is a pre-registered, same-author synthetic conformance corpus scored
> against author-declared labels.**
>
> **Its 90-row confusion matrix is exact for that finite grid, but it is not
> independently labelled or policy-blind and does not establish transferable
> recall, precision, or false-positive cost.**

Arm D does **not** change the project's headline. `README.md` still says this
project does not meet the Track 2 precision/recall bar and that the headline
result is a failed experiment. Arm C's failed agent-trace result stands
unchanged in `PRELABEL-FINDING-armC.md`; nothing here repairs it.

---

## 1. What is withdrawn

Four claims were published on 2026-09-01 and are withdrawn:

| withdrawn claim | why it was wrong |
|---|---|
| "A held-out evaluation" | The author both wrote the decision rule and built the corpus. A freeze date is not a held-out set. |
| "Ground truth comes from the intent, never the mandate" | False as an implementation claim. The scorer branches on an authored `label` field. |
| "Recall 1.000" as a detection result | Tautological on this corpus. Every positive was constructed to match no capability. |
| "Precision / false-positive cost" as measurable quantities | The labels that define both are contested and have one author. |

## 2. The five findings, and what each one actually was

### 2.1 The scorer never reads the intent — P0

`cmd/rzp-armd/main.go` branches on `r.Label`, a field written into
`requests.json` by `study/grid_d.py`. `intent_text` was loaded into the struct
and never used in a decision; `intent_payment` was not in the struct at all. **A
one-field edit to `label` changes every reported number.**

The generator *derives* that label from the intent, so the published values are
consistent with what was intended. But the claim was about the implementation,
and the implementation scores against an author-declared field. That is a
weaker thing, and stating it the strong way is the defect.

### 2.2 "Held out by a date" is not held out — P0

Freezing the policy before the corpus existed proves the code was not fitted to
the data. It does not blind an author who knew the decision rule while building
the corpus. Those are different guarantees and only the first was ever bought.

**D1 was tautological.** Every positive was constructed as an unmatched amount
or an unmatched payment. A default-deny capability verifier must refuse all of
them. Recall 1.000 restates the construction; it does not measure detection.

What D1 *does* establish is narrower and still worth having: across 54
constructed attempts, including a payment that appears in no mandate at all,
none was forwarded. It could have failed and did not. That is a conformance
result about this program, not a detection rate.

### 2.3 The false-positive labels are contested — P1

An intent reading "refund the item price, 24,000 paise" does not self-evidently
make a 12,000-paise partial refund in-intent. The six `coverage=exact /
request=under` rows counted as false positives may instead be a correct refusal
to honour half of an exactly-authorized amount.

**That question is open.** Settling it needs independent labels or a separately
justified rule, and arm D has neither. Every figure derived from those 17 rows —
precision, false-positive rate, "value refused" — inherits the doubt.

### 2.4 A control that was described but never built — P1

`PROTOCOL-armD.md` §3 said the policy not changing after scoring was "enforced
by the harness". Nothing enforced it. `rzp-armd` read whatever `internal/policy`
contained and merely refused to overwrite its own output.

`rzp-armd verify` now enforces it, and enforces more than the protocol ever
claimed. See §4.

### 2.5 The pre-registration commit's CI failed and I did not look — P1

`ca1e4c1` failed `TestTheExclusiveLockHoldsUnderConcurrentOpens` in CI: *0 of 16
concurrent opens succeeded on ONE state file, want exactly 1*. I pushed it and
moved on.

It was not a fluke, and it is now fixed. See §5.

## 3. How this retraction is recorded

A retraction that quietly rewrites the document it retracts leaves no record of
what was claimed. So:

- `RESULTS-armD.md`, `PROTOCOL-armD.md` and `FINDINGS-armD.md` are **preserved
  unedited**. Each carries a dated notice at the top and nothing else changed.
  `rzp-armd verify` hashes the text below the marker line and fails if a byte of
  it moves. The manifest records the git blob of each original, so
  `git show f87c86b:study/RESULTS-armD.md` is an independent check.
- An earlier attempt at this correction **edited those files in place**, which
  created a second problem: `PROTOCOL-armD.md` says "nothing below was written
  after seeing a number" while having been edited after the numbers existed, and
  `RESULTS-armD.md` says "computed, not written by hand" while carrying
  hand-written amendments. That attempt also left the two documents
  self-contradictory — a corrected banner over uncorrected sections, including a
  sentence fragment where a claim had been half-removed. Reverting to the
  originals and putting the correction here is what fixes both.
- **The generator no longer emits the withdrawn wording, and no longer writes to
  that path.** `rzp-armd` now produces `armD/CONFORMANCE-armD.md`. It cannot
  overwrite or regenerate `RESULTS-armD.md`, which is now purely a historical
  artifact. The two documents therefore report the same matrix in different
  prose, deliberately: one is what was published, the other is what the program
  says today.

## 4. What `rzp-armd verify` checks

Read-only. It never writes, never re-scores into a file, and exits non-zero on
any mismatch. `go test ./cmd/rzp-armd/` runs it, so CI fails if any of this
drifts — which is the difference between a control and a description of one.

| checked | against |
|---|---|
| every non-test source in the scorer's import closure, plus `main.go` | `armD/manifest.json` |
| `requests.json`, all 90 compiled mandates, all 3 corpus generators | `armD/manifest.json` |
| re-deciding all 90 requests in memory | the recorded matrix |
| the matrix **printed** in `CONFORMANCE-armD.md` and in the preserved `RESULTS-armD.md` | the recorded matrix |
| the preserved bodies of all three retracted documents | recorded SHA-256, cross-referenced to git blobs |

Its first version hashed `internal/policy` alone, while the score also ran
`internal/mandate`, `internal/lifecycle` and `internal/opauth`. A verifier
narrower than the thing it verifies gives a green light it has not earned.
`manifest_test.go` now recomputes the import closure from the source and fails
if the manifest stops covering it, so it cannot narrow again by accident.

`internal/storage` is excluded because it is genuinely not in that closure — the
scorer's persister is `nil`. That is a convenient exclusion, given §5 landed in
`internal/storage` on the same day, so it is checked by a test rather than
asserted.

**Two limitations, stated rather than buried.** The manifest was recorded after
scoring, not before; what makes it more than self-attestation is git, which
shows the policy commit `fb87b12` (2026-08-30) predating the corpus. And
`recordManifest` refuses to overwrite an existing manifest, so re-recording
requires deleting the file first — visible in the diff, but a determined author
could still do it. Verification proves reproducibility and stability. It does
not make the labels independent, and nothing in this file claims otherwise.

## 5. The lock defect, reproduced and fixed

**Cause.** Under SQLite's `locking_mode = EXCLUSIVE`, a connection that has read
the database keeps its SHARED lock for the connection's whole life. Sixteen
processes starting together could each hold SHARED, none could upgrade to
EXCLUSIVE, and every one failed — not one owner and fifteen refusals, but
**zero** owners.

**Reproduction, before the fix:**

| condition | runs | failures |
|---|---|---|
| pinned container, default resources | 30 | 0 |
| pinned container, `--cpus=0.5`, `-race` | 12 | **1** |

**Fix.** A bounded retry in `storage.Open`. Each attempt uses a fresh
connection, because closing the handle is what releases the SHARED lock that
blocks everyone; jittered backoff stops contenders colliding again in lockstep;
a deadline ends it. Not a `busy_timeout` — that sleeps on the *same* connection,
so every waiter would sleep while holding the lock that blocks the others, and
it would apply to every later statement, turning a fast refusal into a stall
mid-refund.

The direction stays fail-closed. Nothing has been forwarded when `Open` runs, so
exhausting the deadline refuses to start: unavailable, never two ledgers.

**After the fix,** `--cpus=0.5` with `-race`: **60 runs, 0 failures, 0 data
races.** The test now asserts all four properties rather than counting
successes — exactly one owner; every loser refused with the named ownership
error inside a bound; the winner still able to write *and* the file still closed
to a later opener; and two further tests prove the wait is bounded and that a
schema-version refusal is not retried as if it were contention.

## 6. What arm D still supports, and what it does not

**Supports:** on a pre-registered, same-author synthetic conformance corpus of
90 constructed refund requests scored against author-declared labels, the
guard's decisions matched those labels as recorded, reproducibly, with the
decision path pinned. Two pre-registered predictions failed and both failures
are recorded rather than reframed — including `D086`, which independently
reproduces the `maxSetSize = 8` bounded-search limitation that the arm C blocked
-call audit had only inferred, and reproduces it as a single deterministic row
anyone can re-run.

**Does not support:** any rate of agent misbehaviour, any fraud rate, any
generalisation to merchant traffic, any transferable precision, recall or
false-positive cost, or any claim that arm C succeeded.

## 7. The minimum remaining path

**Independent blinded labels for these 90 rows.** Each rater is shown the
intended payment, the intent and the requested refund — and **not** the mandate
and **not** the guard's decision. Until those labels exist, nothing in arm D is
a metric result, and this document says so in place of the ones it supersedes.
