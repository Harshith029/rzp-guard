# Arm E, Amendment 3 — the policy changed again, deliberately this time, and here is the proof it was harmless

**Dated 2026-09-05.** This amends `PROTOCOL-armE.md` §3 and extends
`PROTOCOL-armE-AMENDMENT-2.md`. It does not change any reported number.

**Two changes, both checked.** This file was written for the first and extended
for the second rather than spawning an amendment 4, because they are one body of
work committed on one branch on one day, and splitting them would make the trail
harder to follow rather than easier.

## Why this exists

Amendment 2 recorded a policy change that happened *silently* after the corpus
existed, and the lesson it drew was that a freeze nothing enforces is a sentence
rather than a control. The gate that closes it — `inputs_sha256`, recomputed by
`rzp-arme verify` over every non-test file in `internal/policy` — has now fired
for the first time, on purpose.

```
$ go run ./cmd/rzp-arme verify
  recomputed: TP 22 FP 30 TN 36 FN 8
  published:  TP 22 FP 30 TN 36 FN 8
  every decision and rule string matches
rzp-arme: arm E still reproduces its numbers, but the INPUTS changed:
  recomputed 4f46bd92923a5db7, published 970e74bd7d077354
```

That is the control working. The change was known, the numbers were unmoved, and
CI refused to let it pass unexplained anyway. This file is the explanation.

## Change 1 — the operator-grant path

The human half of the false-positive workflow that `FP-COST.md` §7 said did not
exist.

| File | Change |
| --- | --- |
| `internal/policy/override.go` | new. `GrantSource`, the poll cache, `operatorOverride` |
| `internal/policy/policy.go` | three refusal sites now pass through `operatorOverride`; `Decision` gains `PaymentID`, `RequestedPaise`, `OperatorGrantID`; new rule id `OPERATOR_APPROVED` |

A refund the mandate refuses gets one further check against grants a named human
issued through `rzp-guard-operator` against a refusal the guard actually
recorded. Exact payment, exact amount, single use, expiring, and reserved
through the same ledger as any mandate action.

## Change 2 — the durability change

The rate-window slot moved into the reservation's own transaction.

| File | Change |
| --- | --- |
| `internal/policy/policy.go` | `reserveSet` calls `ReserveManyAt` with the call timestamp; the separate `rate.record` and its best-effort rollback are gone |
| `internal/lifecycle/lifecycle.go` | `ReserveManyAt`, and the optional `RateReserver` interface |
| `internal/storage/storage.go` | `ReserveManyWithCall` writes the `call_log` row inside the reservation transaction |

An authorized refund performed **two** commits and now performs **one**. At
`synchronous=FULL` each commit is an fsync, so this is the whole cost of the
allow path.

```
BEFORE  BenchmarkDecideAllowDurable   4.88 ms/op   (4.77, 4.87, 5.01)
AFTER   BenchmarkDecideAllowDurable   2.49 ms/op   (2.43, 2.49, 2.54)
        BenchmarkReserve             2.45 ms/op   -- one commit alone
```

Same machine, same session, 500 iterations x 3 runs each, measured by stashing
the change rather than by comparing against a number in a document. 1.96x, and
the allow path is now ~1.02 commits instead of ~2.0.

The correctness half matters more than the speed. Two transactions could
half-succeed, and the only repair available was a rollback the code itself
described as best-effort: *"the rate write failing usually means the store is
broken, so the release will fail too."* That left actions RESERVED holding
budget against a refund that never left the building, surfaced at the next
restart as an IN_DOUBT an operator has to ask about. One transaction removes
the state rather than compensating for it. Durability is unchanged: both rows
are still on disk before a byte reaches the child.

`TestAnAuthorizedRefundPerformsOneDurableWrite` pins it, because the regression
is silent -- splitting the writes again would pass every functional test, still
be durable, and simply cost twice the fsync.

## The equivalence check, run rather than assumed

Same method as amendment 2: every one of the 120 arm E requests decided under
the new tree, `request_id, allowed, rule` recorded for all of them, compared
against the digest published at scoring time.

```
declared frozen tree   f18c634e303ee980d164e43321c149223701205f  (fb87b12)
tree at amendment 2    2289146a3c350dfa961deba7bbd503dd603163eb
tree after change 1    b063b5cabcababd4de682b541b310d36a349c2e3
tree after change 2    49f7493bf66b8396dab94b0044149c599e5c96b2

decisions_sha256  9e6489fc71ace82d…   UNCHANGED across both
matrix            TP 22 FP 30 TN 36 FN 8   UNCHANGED across both
kappa             0.604 over 120 rows, 24 no-majority   UNCHANGED across both
inputs_sha256     970e74bd… -> 4f46bd92… -> 83146d79…   CHANGED, twice
```

Re-running `rzp-arme score` rewrote **exactly one line** of
`study/armE/manifest.json` each time: `inputs_sha256`. Every other recorded
value — the matrix, the decisions digest, both label digests, the exclusion
counts — is byte-identical.

Arm D gated it too, and its own tool refuses to re-stamp unless the corpus still
reproduces the published matrix. It did, both times: TP 54 FP 17 TN 19 FN 0.
`internal/opgrant` was added to `decisionPathDirs` because the scorer now imports
it transitively and `manifest_test.go` refused to let it stay unhashed — a gap
the gate found rather than a person.

## Why it came out that way, and why that is not the evidence

**Change 1.** `operatorOverride` returns its argument unchanged when
`g.grants == nil`. A `Guard` only acquires a grant source through `SetGrantSource`, which
`cmd/rzp-arme` does not call and has no reason to: the scorer constructs a guard
from a mandate file and decides, with no state file and no operator. So on this
corpus the new branch is not merely unexercised, it is unreachable.

**Change 2** does not touch the decision at all. It changes when a durable write
happens, not what is decided, and the scorer runs the policy with a nil store —
so `l.store != nil` is false and neither the old nor the new write path executes.

Both are explanations of the result. The 120-row comparison is the result. The
distinction matters for the same reason it did in amendment 2: "it obviously
cannot matter" is the sentence a freeze exists to stop people saying, and the
first time the argument is wrong is the time it would have been worth checking.

## What this does and does not mean

- **It does not mean arm E now measures the guard as deployed.** It measures the
  guard's *mandate* decisions. A deployment with an operator working the queue
  would have a different, higher effective allow rate on legitimate refunds, and
  arm E says nothing about that. Measuring it needs an arm with humans in the
  loop, which is arm F's problem.
- **It does mean the reported numbers stand.** Recall 0.733, FPR 0.455,
  precision 0.423, TP 22 / FN 8 / FP 30 / TN 36.
- **It does mean the published FPR is now a ceiling on customer impact rather
  than a description of it** — but only where somebody works the queue. Where
  nobody does, it is exactly what it always was. The mechanism exists; the
  staffing is not something this repository can assert.

## Corrected

- `study/armE/manifest.json` records the new `current_tree`, keeps the previous
  supersession under `prior_supersessions` rather than overwriting it, and names
  this file as the reason.
- `inputs_sha256` re-stamped by `rzp-arme score`, with the diff above as the
  record of what moved.

## The lesson

The gate cost one commit's worth of ceremony and produced a document that says
precisely what changed, what was checked, and what is still unmeasured. The
version of this project that did not have the gate had the same policy change go
by unnoticed for a day. That is the whole argument for gates on results you
intend people to rely on.
