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
> This file is the pre-registration, committed before the corpus existed. It is preserved
> **unedited** below, because a retraction that quietly rewrites the document
> it retracts leaves no record of what was claimed. Everything after the marker
> line is byte-for-byte the artifact committed in `ca1e4c1` (git blob `db57fd5902f085d95c9ea1f7681b766b9c885af6`), which
> `git show ca1e4c1:study/PROTOCOL-armD.md` will print.
>
> `rzp-armd verify` recomputes the hash of that preserved text and fails if a
> byte of it changes.
>
> What is withdrawn, why, and what these numbers do still support:
> [**ASSESSMENT-armD.md**](ASSESSMENT-armD.md).

<!-- PRESERVED-ORIGINAL-BELOW -->
# Arm D pre-registration — a held-out verifier evaluation

**Status: written and committed before the corpus exists.** Nothing below was
written after seeing a number.

Arm C failed to estimate recall because the traffic generator almost never
emitted an out-of-intent call: two candidates in 340. That is a fact about the
generator, not about the guard, and no amount of labelling repairs it.

Arm D measures a different thing, and says so in its first line.

---

## 1. What is being measured

**The guard as a verifier**, scored on a corpus of constructed refund requests.

Track 2 asks for "a working detector, **verifier** or auto-responder for one
class of loss, with measured precision and recall on a held-out test set". This
arm evaluates the verifier.

**Unit:** one refund request — a `(payment_id, amount_paise)` pair presented
against a merchant intent and the mandate compiled from it.

**Positive class:** *out-of-intent* — the request the guard exists to refuse.

## 2. What arm D is NOT

Stated before the design, because this is where an evaluation like this goes
wrong:

- **Not agent traffic.** The requests are constructed, not emitted by a model.
  This arm cannot say how often an agent would make such a request, and no
  number from it may be presented as a rate of agent misbehaviour.
- **Not a replacement for arm C.** Arm C asked what an agent does. It failed,
  that failure stands, and `PRELABEL-FINDING-armC.md` is not retracted.
- **Not a fraud-detection result.** The guard is deterministic. This measures
  whether it classifies correctly, not whether it predicts anything.

## 3. Why "held out" is defensible here

The usual worry is that the implementation was tuned to the test data. That is
answerable in this arm with a date rather than an argument:

```
policy last changed   fb87b12   2026-08-30
arm D corpus created            2026-09-01 or later
```

**The detector was written and frozen before this corpus existed.** It cannot
have been fitted to data that did not exist, and any reader can check the two
commits. That is a stronger guarantee than a random train/test split over a
corpus the author already held.

Additionally, and enforced by the harness:

- the corpus is a **mechanical cross product**, not a chosen set of cases;
- **the policy is not modified after scoring.** If `internal/policy` changes
  after `RESULTS-armD.md` is written, the arm is void and must be re-run;
- the corpus is **scored once**. There is no tuning pass.

## 4. Ground truth comes from intent, never from the mandate

This is the defect that withdrew arm A's headline, and the reason this section
exists.

> A request is **out-of-intent** if the merchant's `intent_text` does not
> authorize refunding that payment for that amount. Otherwise it is
> **in-intent**.

The guard decides from the **compiled mandate**. The label comes from the
**intent**. These are different predicates, and the compilation from intent to
mandate is deliberately lossy — `compile_mandate.py` documents what it cannot
express. **The gap between them is exactly what this arm measures.** If labels
were computed from the mandate, a perfect score would mean the guard agrees with
itself, which is worth nothing.

## 5. The corpus

A cross product, emitted by `grid_d.py`:

| dimension | levels |
|---|---|
| `intent_scope` | one_item, two_items, whole_order |
| `coverage` | exact, under, split — how much of the intent the mandate can express |
| `request` | exact, under, over, wrong_payment, far_over — what is asked for |
| `size` | small (3 items), large (5 items) |

3 × 3 × 5 × 2 = **90 requests**, each with a label fixed by construction from
the intent.

**Both classes are populated by design.** `over`, `far_over` and
`wrong_payment` are out-of-intent by the §4 rule; `exact` and `under` are
in-intent. That is not manufacturing a result — it is the minimum requirement
for measuring recall at all, and arm C failed precisely because its positive
class was empty.

## 6. Predictions, recorded now

**D1.** Recall will be **1.000**. Every out-of-intent request asks for an amount
or payment no action authorizes, and the guard refuses anything without a
matching capability. *If recall is below 1.0, an out-of-intent request reached
the provider and that is a security defect, not a metric.*

**D2.** Precision will be **below 1.0**, because `coverage=under` cells make the
mandate under-express the intent, so in-intent requests will be refused. These
are the false positives and they are the substantive number.

**D3.** All false positives will fall in `coverage=under` and none in
`coverage=exact`. *If an `exact` cell produces a false positive, the matching
logic is wrong.*

**D4.** `coverage=split` will produce **no** false positives — combining should
cover an intent reachable only as a sum. *A false positive here means combining
is broken.*

**D5.** Precision is **base-rate dependent** and the reported figure is a
property of this corpus's construction. Recall is not. The report will state
both and give precision at more than one class balance.

## 7. What the result will and will not support

**Will support:** "on a pre-registered, mechanically enumerated, held-out corpus
of 90 constructed refund requests, scored against a policy frozen before the
corpus existed, the verifier achieved precision X and recall Y, with the
false-positive cost attributable to mandate under-coverage."

**Will not support:** any claim about how often an agent misbehaves, any fraud
rate, any generalisation to merchant traffic, or that arm C succeeded.

## 8. Limitations, in advance

- Constructed requests, not observed traffic. Base rates are a design choice.
- Synthetic payments and amounts, from the same catalogue as arm C.
- One author designed the dimensions. Enumeration removes selection bias within
  the grid, not the grid's blind spots.
- The intent→mandate compilation is the author's, so the false-positive rate is
  a property of that compiler as much as of the guard. The report attributes it.
