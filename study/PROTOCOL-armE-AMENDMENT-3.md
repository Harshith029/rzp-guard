# Arm E, Amendment 3 — the policy changed again, deliberately this time, and here is the proof it was harmless

**Dated 2026-09-05.** This amends `PROTOCOL-armE.md` §3 and extends
`PROTOCOL-armE-AMENDMENT-2.md`. It does not change any reported number.

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

## What changed in the policy

The operator-grant path — the human half of the false-positive workflow that
`FP-COST.md` §7 said did not exist.

| File | Change |
| --- | --- |
| `internal/policy/override.go` | new. `GrantSource`, the poll cache, `operatorOverride` |
| `internal/policy/policy.go` | three refusal sites now pass through `operatorOverride`; `Decision` gains `PaymentID`, `RequestedPaise`, `OperatorGrantID`; new rule id `OPERATOR_APPROVED` |

A refund the mandate refuses gets one further check against grants a named human
issued through `rzp-guard-operator` against a refusal the guard actually
recorded. Exact payment, exact amount, single use, expiring, and reserved
through the same ledger as any mandate action.

## The equivalence check, run rather than assumed

Same method as amendment 2: every one of the 120 arm E requests decided under
the new tree, `request_id, allowed, rule` recorded for all of them, compared
against the digest published at scoring time.

```
declared frozen tree   f18c634e303ee980d164e43321c149223701205f  (fb87b12)
tree at amendment 2    2289146a3c350dfa961deba7bbd503dd603163eb
tree now               b063b5cabcababd4de682b541b310d36a349c2e3

decisions_sha256  9e6489fc71ace82d…   UNCHANGED
matrix            TP 22 FP 30 TN 36 FN 8   UNCHANGED
kappa             0.604 over 120 rows, 24 no-majority   UNCHANGED
inputs_sha256     970e74bd… -> 4f46bd92…   CHANGED
```

Re-running `rzp-arme score` rewrote **exactly one line** of
`study/armE/manifest.json`: `inputs_sha256`. Every other recorded value — the
matrix, the decisions digest, both label digests, the exclusion counts — is
byte-identical.

## Why it came out that way, and why that is not the evidence

`operatorOverride` returns its argument unchanged when `g.grants == nil`. A
`Guard` only acquires a grant source through `SetGrantSource`, which
`cmd/rzp-arme` does not call and has no reason to: the scorer constructs a guard
from a mandate file and decides, with no state file and no operator. So on this
corpus the new branch is not merely unexercised, it is unreachable.

That is an explanation of the result. The 120-row comparison is the result. The
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
