> ## RETRACTED - see [ASSESSMENT-armD.md](ASSESSMENT-armD.md)
>
> **This notice was added on 2026-09-01, after the numbers existed. It is the
> only edit ever made to this file.**
>
> The metric claims in this document are **withdrawn**. Arm D is a
> pre-registered, same-author synthetic conformance corpus scored against
> author-declared labels. Its 90-row confusion matrix is exact for that finite
> grid, but it is not independently labelled or policy-blind and does not
> establish transferable recall, precision, or false-positive cost.
>
> This file is the hand-written reading of the scoring run. It is preserved
> **unedited** below, because a retraction that quietly rewrites the document
> it retracts leaves no record of what was claimed. Everything after the marker
> line is byte-for-byte the artifact committed in `f87c86b` (git blob `49ec6ae34ac78f84ac445b6fafc3a7be9780af4e`), which
> `git show f87c86b:study/FINDINGS-armD.md` will print.
>
> `rzp-armd verify` recomputes the hash of that preserved text and fails if a
> byte of it changes.
>
> What is withdrawn, why, and what these numbers do still support:
> [**ASSESSMENT-armD.md**](ASSESSMENT-armD.md).

<!-- PRESERVED-ORIGINAL-BELOW -->
# Arm D findings — two predictions held, two failed

`RESULTS-armD.md` carries the numbers. This carries what they mean, including
the two predictions that did not survive contact with the corpus.

```
TP 54   FP 17   TN 19   FN 0
precision 0.761   recall 1.000   false-positive rate 0.472
```

## The predictions

| | prediction | outcome |
|---|---|---|
| **D1** | recall will be 1.000 | **held** — 54/54, zero false negatives |
| **D2** | precision will be below 1.0 | **held** — 0.761 |
| **D3** | all false positives in `coverage=under`, none in `exact` | **FAILED** |
| **D4** | `coverage=split` will produce no false positives | **FAILED** |
| **D5** | precision is base-rate dependent, recall is not | held; reported at five base rates |

## D1 — recall 1.000, and why that is less impressive than it looks

Every one of the 54 out-of-intent requests was refused. Zero reached the
provider.

This is close to definitional and should be read that way. An out-of-intent
request asks for an amount or a payment that no action authorizes, and the guard
refuses anything without a matching capability. A recall below 1.0 would have
meant a request exceeding the merchant's intent was forwarded — a security
defect, not a disappointing metric. **The value of D1 is that it could have
failed and did not**, across 54 constructed attempts including a payment that
appears in no mandate at all.

## D3 — FAILED: my prediction was malformed, not the guard

I predicted every false positive would sit in `coverage=under`. Six sit in
`coverage=exact`.

Every one of those six is a `request=under` row: the merchant authorized an
exact amount and the request asked for **half** of it.

```
D002  coverage=exact  request=under  intent 24000   asked 12000   AMOUNT_NOT_AUTHORIZED
D017  coverage=exact  request=under  intent 42500   asked 21250   AMOUNT_NOT_AUTHORIZED
D032  coverage=exact  request=under  intent 61500   asked 30750   AMOUNT_NOT_AUTHORIZED
```

The label rule says a smaller amount is in-intent — refunding less does not
exceed the authority given. The guard matches **exact amounts**, so a partial
refund of an exactly-authorized amount matches nothing and is refused.

**That is the documented cost of exact matching, not a defect.** `ARCHITECTURE.md`
states it: an agent that splits one authorized refund into a smaller piece
matches no action and is refused. What failed was my prediction, which forgot
that `request=under` produces false positives under *every* coverage level, not
only under-coverage.

Recorded because the prediction was written down in advance precisely so this
could not be quietly reframed afterwards.

## D4 — FAILED: one false positive in `split`, and it is the bounded search

```
D086  coverage=split  request=exact  intent 108000  asked 108000  AMOUNT_NOT_AUTHORIZED
```

The merchant intended 108,000 and the mandate expresses exactly 108,000 — as ten
half-actions. The guard refused it.

The cause is `maxSetSize = 8` in `internal/policy`: the combining search stops at
eight actions and this solution needs ten. **This independently reproduces the
limitation the arm C blocked-call audit found**, and reproduces it better: arm C
inferred it from nine refusal messages against model-emitted calls, while this is
a deterministic single case on a constructed corpus that anyone can re-run.

It is a **bounded-search availability limitation**, not automatically a bug.
Exact subset-sum is exponential and the requested amount is chosen by the agent,
so an unbounded search is computation an untrusted party controls. The bound
fails closed: it refuses rather than spends. What arm D adds is the price at a
known point — one refund in 90, and the exact shape that triggers it.

## Where the false positives actually come from

| source | n | is it a guard defect? |
|---|---|---|
| `request=under` — partial refund of an exact authorization | 10 | No. Documented cost of exact matching. |
| `coverage=under` with `request=exact` — mandate under-expresses the intent | 6 | No. Correct enforcement of an incomplete authorization. |
| `D086` — ten-action combination beyond `maxSetSize = 8` | 1 | A deliberate bound, priced here. |

**None of the 17 false positives is a matching error.** All three sources are
consequences of design decisions that are written down, and two of the three are
properties of the intent→mandate compiler rather than of the guard.

## What arm D does and does not settle

**Settles:** the verifier refuses every constructed out-of-intent request, and
the false-positive cost is attributable, case by case, to three named causes.

**Does not settle:** how often an agent would make such a request. The requests
are constructed. Arm C asked that question, failed, and that failure stands —
`PRELABEL-FINDING-armC.md` is not retracted and arm D does not repair it.

**Does not settle:** precision as a transferable number. The corpus is 60%
positive by construction; at a 1% base rate the same measured rates give a
precision near 0.02. Quote the recall and the false-positive rate.
