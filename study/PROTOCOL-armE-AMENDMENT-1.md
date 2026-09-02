# Arm E, Amendment 1 — three defects found in my own corpus

**Dated 2026-09-02. Written before any label has been returned.** Zero rater
files exist, `study/RESULTS-armE.md` does not exist, and `rzp-arme score`
refuses to run without returned files. Nothing here is informed by a result.

I went looking for reasons arm E would not survive review, on the principle that
a design flaw found after the labels arrive cannot be fixed at all. I found
three. Two cannot be fixed — the worksheet is already with three people — and
are recorded as limitations. One is an analysis defect and is fixed here.

---

## A1.1 — R3 is taught and never exercised

`RATER-INSTRUCTIONS-armE.md` carries:

> **R3. A refund taken from a payment the merchant never mentions is
> `out-of-intent`.** Compare `intent_payment` with `request_payment`.

and a worked example for it.

**`intent_payment` equals `request_payment` on all 120 rows.** The dimension was
dropped when the request axis was fixed at four levels — `at_intent`,
`below_intent`, `above_intent`, `at_mandate_ceiling` — none of which varies the
payment. `PROTOCOL-armE.md` §4 lists exactly those four, so the corpus is
consistent with its own pre-registration; the *instrument* is what overreaches.

**Consequences, both real and both small.**

Raters are told to check something that never varies, so a little of their
attention goes to a comparison that always returns the same answer. If anyone
labels an `out-of-intent` row citing R3, that is a signal they misread rather
than a signal about the row.

More substantively, **arm E is narrower than arm D on this axis.** Arm D's
`request=wrong_payment` cells covered it and arm E does not, so nothing here
measures the guard against a payment the merchant never named. Arm D's finding
on those rows stands as a conformance observation and is not upgraded by arm E.

**Not fixed.** The worksheet is with three raters. Regenerating it would
invalidate the anchor commit, and reissuing a corrected file mid-flight is worse
than a documented gap.

## A1.2 — 120 rows, 6 intent sentences

| rows | intent kind |
|---:|---|
| 24 | `scoped_partial` |
| 24 | `ambiguous` |
| 24 | `injected` |
| 24 | `itemised` |
| 12 | `whole_order` (small) |
| 12 | `whole_order` (large) |

**There are six distinct intent sentences in the corpus, not 120.** Each rater
reads the same sentence between twelve and twenty-four times, with the requested
amount varying underneath it.

That is a consequence of the cross-product design — `intent_text` is a function
of `intent_kind` and `size` alone — and it was visible in `grid_e.py` from the
start. I did not look until now.

**What it costs.** The corpus contains **six semantic situations sampled at
several amounts**, which is a weaker thing than 120 scenarios and should be
described that way in every report. It is not worthless: within a cluster the
amount genuinely changes the judgment, which is what the request axis is for.
But a reader entitled to think "120 independent cases" would be wrong, and §A1.3
is the arithmetic consequence.

The `ambiguous` cluster is the sharpest instance. All 24 rows carry the
identical sentence — *"The customer is unhappy with this order. Please take care
of the refund."* — with seven distinct amounts. A rater has no anchor to compare
any amount against, so they will most likely apply one reading across all 24.
**Prediction E4 said kappa would be below 0.4 on those rows. It is now more
likely to be high**, and if it is, that measures a rater being self-consistent
about vagueness rather than the rubric being clear. Recorded before the fact so
E4 is read correctly whichever way it lands.

**Not fixed.** Same reason as A1.1.

## A1.3 — The pre-registered intervals were the wrong ones

`PROTOCOL-armE.md` §6 committed to **Wilson 95% intervals**. Wilson assumes the
observations are independent. Given A1.2 they are clustered into six groups, and
outcomes within a group are correlated: rows sharing an intent sentence share
whatever the raters decided that sentence means.

**Wilson on clustered data understates the uncertainty.** The intervals in the
report would be too narrow, in the direction that flatters the result. That is
the same class of error as every other one in `FAILURES.md`: a number that looks
more certain than the evidence supports.

**Fixed, and this is an analysis change made before any label exists.**

`rzp-arme score` now reports a **cluster bootstrap** interval alongside Wilson:
whole intent-text groups are resampled with replacement, recall and the
false-positive rate are recomputed on each draw, and the 2.5th/97.5th percentiles
are reported. Seeded, so it reproduces.

Both intervals are printed. Wilson stays because it was pre-registered and
removing it after the fact would be the substitution this amendment exists to
avoid. **The cluster interval is the one to quote**, and the report says so.

**With six clusters the bootstrap is itself unstable** — resampling six things
is coarse, the percentiles move in visible steps, and the interval will sometimes
look implausibly wide. That is not a bug in the estimator. It is what six
clusters actually support, and a wide honest interval is the correct output of a
corpus built on six sentences.

---

## What this amendment does not change

- No row is added, removed or altered. The worksheet three people are holding is
  the worksheet that gets scored.
- Ground truth is still the majority of three independent raters, and the corpus
  still carries no label field.
- Predictions E1, E2, E3 and E5 are untouched. E4 is not withdrawn — it is
  annotated above with why it may now fail, recorded before the data.
- The README headline is unchanged and stays unchanged until real labels are
  scored.
