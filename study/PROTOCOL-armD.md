# Arm D pre-registration — a synthetic conformance corpus

> ## What arm D is
>
> **Arm D is a pre-registered, same-author synthetic conformance corpus scored
> against author-declared labels.**
>
> **Its 90-row confusion matrix is exact for that finite grid, but it is not
> independently labelled or policy-blind and does not establish transferable
> recall, precision, or false-positive cost.**
>
> It is an engineering regression and conformance suite. It is **not** the metric
> rescue, it does not meet the Track 2 metric bar, and it does not repair arm C.
> Arm C's failed agent-trace result stands unchanged in
> `PRELABEL-FINDING-armC.md`.
>
> ### Why the original framing was wrong
>
> **The scorer never reads the intent.** `cmd/rzp-armd/main.go` branches on
> `r.Label`, a field authored into `requests.json`. `intent_text` is loaded and
> never used in a decision, and `intent_payment` is not even in the struct. A
> one-field edit to `label` changes the reported precision and recall. The claim
> "ground truth comes from intent, never the mandate" was false **as an
> implementation claim**, whatever the generator intended.
>
> **Freezing the policy first does not blind the author.** The same person who
> knew the decision rule constructed the corpus afterwards. A date proves the
> code was not fitted to the data; it does not make the data independent of the
> author's knowledge of the code.
>
> **D1 is tautological.** Every positive was constructed as an unmatched amount
> or an unmatched payment. A default-deny capability verifier must refuse all of
> them. Recall 1.000 restates the construction; it does not measure detection.
>
> **The false-positive labels are not settled ground truth.** An intent reading
> "refund the item price, 24,000 paise" does not self-evidently mean a
> 12,000-paise partial refund is in-intent. The six `coverage=exact /
> request=under` rows counted as false positives may be a correct exact-
> authorization refusal rather than a merchant-harming block. Deciding that needs
> independent human labels or a separately justified rule, and has neither.
>
> ### The minimum path to a literal Track-qualifying claim
>
> Independent blinded labels for these 90 rows: each rater shown the intended
> payment, the intent, and the requested refund — and **not** the mandate or the
> guard's decision. Until that exists, nothing here is a metric result.



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

## 3. What the policy freeze does and does not buy

The usual worry is that the implementation was tuned to the test data. That is
answerable in this arm with a date rather than an argument:

```
policy last changed   fb87b12   2026-08-30
arm D corpus created            2026-09-01 or later
```

The detector was written and frozen before this corpus existed, so it cannot
have been fitted to data that did not exist. **That is all the date buys.** It
does not blind the author, who knew the decision rule while constructing the
corpus, and it does not make the labels independent. See the banner above.

Additionally — and only the last of these is enforced by the harness, a
distinction the original wording got wrong:

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

**Will support:** "on a pre-registered, same-author synthetic conformance
corpus of 90 constructed refund requests scored against author-declared
labels, the guard's decisions matched those labels as follows ..." — a
conformance statement about a finite grid, not a metric.

**Will not support:** any claim about how often an agent misbehaves, any fraud
rate, any generalisation to merchant traffic, or that arm C succeeded.

## 8. Limitations, in advance

- Constructed requests, not observed traffic. Base rates are a design choice.
- Synthetic payments and amounts, from the same catalogue as arm C.
- One author designed the dimensions. Enumeration removes selection bias within
  the grid, not the grid's blind spots.
- The intent→mandate compilation is the author's, so the false-positive rate is
  a property of that compiler as much as of the guard. The report attributes it.
