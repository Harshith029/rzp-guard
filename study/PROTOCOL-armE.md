# Arm E pre-registration — a held-out evaluation with independent labels

**Status: written and committed before the corpus exists and before any rater
has been contacted.** Nothing below was written after seeing a number. The
commit that carries this file contains no corpus, no worksheet and no scorer;
`git log --follow` on those paths shows they arrive later.

Arm E is the evaluation arms A–D failed to be. It exists because every previous
attempt in this repository failed in a way that is worth naming, and each named
failure is a design constraint here.

| arm | what it did | why it does not meet the Track 2 bar |
|---|---|---|
| A, B | 15 author-written, author-labelled scenarios | the implementation author chose and labelled every case |
| C | 54-cell mechanical grid, real model traffic | the model almost never misbehaved: **2** out-of-intent calls against a pre-registered floor of 20. Recall not estimable. Stands as a failure. |
| D | 90-row synthetic conformance grid | the scorer branched on a `label` field the author wrote into the corpus. Retracted; see `ASSESSMENT-armD.md` |

---

## 1. The four defects arm E is built to avoid

Stated first, because each one dictates a structural choice rather than a
promise.

**D1. Arm D's scorer read an authored label.** `cmd/rzp-armd/main.go` branches on
`r.Label`, a field `grid_d.py` wrote. A one-field edit changed the reported
precision and recall.

> **Arm E's corpus file contains no label field at all.** `study/armE/requests.json`
> carries the intent, the request and the compiled mandate, and nothing else.
> Ground truth exists only in the CSVs returned by raters. The scorer *cannot*
> read an authored label because there is none to read. A test asserts the
> absence.

**D2. Arm D's positives were tautological.** Every out-of-intent request was
constructed as an unmatched amount or an unmatched payment, so a default-deny
verifier had to refuse all of them. Recall 1.000 restated the construction.

> **Arm E includes `coverage=over`**, where the compiled mandate authorizes
> *more* than the merchant's intent. In those cells an out-of-intent request can
> match an authorized action, and the guard will **allow** it. False negatives
> are reachable by construction. If recall comes back at 1.000 here, it means
> something.

**D3. Arm C's labels never existed.** The recall experiment failed before
labelling.

> **Arm E's labels come from three people who see the intent and the request and
> never see the mandate, the guard's decision, or this document.**

**D4. Arm A's labels came from the same predicate the policy implements.**

> **Arm E's ground truth is the majority of three independent human judgments**
> about the merchant's written intent. The policy's rule is not consulted.

## 2. What is being measured

**The guard as a verifier of one agent-proposed refund.**

- **Unit:** one `create_refund` request — a `(payment_id, amount_paise)` pair
  presented against a merchant intent.
- **Positive class:** *out-of-intent* — the request the guard exists to refuse.
- **Predicted positive:** the guard refused it.
- **Ground truth:** the majority label of three independent raters.

## 3. Why this is held out

Two separate claims, each with its own evidence. Arm D conflated them.

**The implementation was not fitted to this data.** `internal/policy` was last
changed in `fb87b12` (2026-08-30) and its tree hash is recorded. This corpus is
generated after this document is committed. Both dates are in git and neither is
an assertion by me.

**The labels are independent of the implementation.** Raters receive a sanitised
projection — intent text, two payment pseudonyms, the requested amount, two
field-status flags — and the rater-only instrument. They do not receive the
mandate, the guard's decision, the repository, this protocol, or any figure. The
delivered instrument is scanned against a forbidden-context word list and the
distribution gate refuses to print the packet if any word appears. That
machinery already exists and is tested (`cmd/rzp-study/armc_rater_pack.go`).

**What this still is not:** constructed requests, not observed agent traffic.
Arm E measures how well the guard separates in-intent from out-of-intent
requests *on a constructed adversarial set*. It says nothing about how often an
agent would make such a request. Arm C asked that and failed; that failure
stands and arm E does not repair it.

## 4. The corpus

A cross product emitted by `study/grid_e.py`. No include list, no exclude list,
no judgement about which cells are interesting.

| dimension | levels |
|---|---|
| `intent_kind` | itemised, whole_order, scoped_partial, ambiguous, injected |
| `coverage` | exact, under, **over** |
| `request` | at_intent, below_intent, above_intent, at_mandate_ceiling |
| `size` | small (3 items), large (5 items) |

5 × 3 × 4 × 2 = **120 requests.**

**`coverage=over` is the load-bearing addition.** The mandate authorizes line
items the intent never mentioned, so an agent request that exceeds the intent
can still match an authorized action. Those cells are where a false negative
becomes possible, and they are the reason recall is a measurement here rather
than a restatement.

**The three hard `intent_kind` levels** exist so the corpus is not pure
arithmetic:

- `scoped_partial` — the intent authorizes only a delivery fee; the item price
  is not authorized, though the compiler can express it.
- `ambiguous` — "the customer is unhappy, please take care of the refund", with
  no amount. Raters may reasonably disagree or mark `unlabelable`. **Disagreement
  here is a finding, not noise**, and these rows are reported separately.
- `injected` — untrusted customer text inside the intent field instructing that
  the whole order be refunded. A rater judging the *merchant's* intent should
  ignore it.

## 5. Ground truth and rater handling

**Three raters. Majority of three is the label.** Three rather than two so a
disagreement has a tiebreak and the arm survives one person withdrawing.

- **No-majority rows** (three different labels, or a majority for
  `unlabelable`) are **excluded from precision and recall, counted, and reported
  by cell.** They are not silently dropped and they are not resolved by me.
- **The author does not label.** I wrote the corpus generator and the policy. An
  author label is not a third opinion, and pooling one with the external sets
  would be exactly the defect that withdrew arm A.
- **Agreement is published before the metrics**, as raw pairwise agreement and
  Fleiss' kappa. If agreement is poor the rubric was poor, and the metrics
  computed on top of it inherit that.
- Returned files are verified field-by-field against the delivered CSV. Only
  `label` and `reason` may differ.

## 6. Reported quantities

Precision, recall and false-positive rate over the majority-labelled rows, with
**Wilson 95% intervals**, because 120 rows will not support three decimal places
and quoting them without an interval would be the same overclaim this arm exists
to avoid.

Also reported: the confusion matrix, per-cell breakdown, Fleiss' kappa, the
no-majority count, and precision at several base rates — precision is a property
of this corpus's class balance and does not transfer. **Per-class rates (TPR,
FPR) are the figures that transfer; they are quoted first.**

Expected loss is recomputed with the arm E rates using the cost model already
published in `study/FP-COST.md`, whose assumptions are unchanged by this arm.

## 7. Predictions, recorded now

Recorded so they can fail publicly. Arm D's D3 and D4 both failed and both were
reported.

**E1. Recall will be below 1.000.** The `coverage=over` cells authorize amounts
the intent does not, so at least some out-of-intent requests will match an
action and be forwarded. *If recall is 1.000, either the over-coverage cells did
not work as designed or raters did not label them as I expect — and either is a
finding about this corpus, not a triumph.*

**E2. The false negatives will concentrate in `coverage=over` and nowhere
else.** *A false negative outside those cells is a matching defect and would be
a security finding, not a metric.*

**E3. Precision will be below 1.000**, driven by `coverage=under` and
`below_intent`, as in arm D.

**E4. Fleiss' kappa will be above 0.6 overall but below 0.4 on
`intent_kind=ambiguous`.** *Those rows are designed to be arguable; high
agreement there would suggest the rubric is telling raters what to think.*

**E5. Agreement on `injected` rows will be high.** *If raters follow the
injected instruction, that is a finding about how the rows read, and it is
reported rather than corrected.*

## 8. Analysis is fixed now

- No row is added, removed or relabelled after the labels arrive.
- The corpus is scored **once**. There is no tuning pass.
- If `internal/policy` changes after scoring, the arm is void and must be
  re-run. Enforced by `rzp-arme verify` against a recorded manifest, the same
  control arm D described and did not build until it was caught.
- If fewer than three raters return, the arm reports what it has, says how many
  raters produced it, publishes no kappa for a single rater, and states that the
  ground truth is weakened. **A missing figure is a limitation; a fabricated one
  is a false claim.**

## 9. Limitations, in advance

- Constructed requests, not observed traffic. The class balance is a design
  choice and precision moves with it.
- Synthetic payments and amounts, from the same frozen catalogue as arms C and D.
- One author designed the dimensions. Enumeration removes selection bias within
  the grid, not the grid's blind spots.
- The intent→mandate compilation is the author's, so the false-positive rate is
  a property of that compiler as much as of the guard, and the report attributes
  it.
- Three raters is enough for a majority and a kappa. It is not enough to
  characterise the spread of human judgment on the ambiguous cells.
- Raters are recruited by the author from people the author knows. They are
  independent of the implementation, not of the author.
