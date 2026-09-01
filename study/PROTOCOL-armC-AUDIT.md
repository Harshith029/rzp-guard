# Arm C — exhaustive false-block audit

A separate, narrower study on arm C's collected traces. Documented on its own
because it answers a different question from arm C and must not be read as
rescuing it.

**It cannot repair C6.** Arm C does not estimate recall and does not clear the
Track 2 metric bar. That stands, whatever this audit reports.

---

## The question

> **in-intent calls among guard-blocked calls**

Of the refunds this guard refused, how many did the merchant actually intend?
That is the false-positive cost Track 2 asks to be reported honestly, and it is
the substantive quantity arm C's traces *can* support.

## Why it is not precision, and cannot be converted into one

The audited set is selected **on the guard's own decision**. A rate computed
over such a set is conditional by construction: it is `P(in-intent | blocked)`.

Precision would be `P(out-of-intent | blocked)` over a set whose positive class
was adequately populated — and arm C's is not, with two candidate positives in
340 calls. Recall needs the calls the guard *allowed* to contain positives, and
they do not. Neither is available here, and neither is reported.

## Design

**Exhaustive, not sampled.** Every one of the 72 calls the guard refused is in
the audit. No sampling, no curation, no exclusion. The count is a property of
the corpus, not a choice.

**Blinded, and the raters are not told what the rows share.** The delivered rows
are the same sanitised authorization-relevant projection used throughout arm C:
opaque row id (`A-nnn`, a distinct id space from the main worksheet), the
merchant's intent, tool name, pseudonymised intent/call payment identities,
amount in paise, and target/amount statuses. **No outcome field of any kind.**

The selection reads the guard's decision; the delivered file never carries it,
and `auditExportedWorksheet` refuses any file that mentions one. Raters are not
told these are refused calls — knowing would change how they label.

**Two external raters, independently.** Both label the same 72 rows. Neither has
worked on the implementation, read `grid.py`, or seen the repository. The
author does **not** label this set: with 72 rows and a near-arithmetic rubric,
an author-rater adds nothing and risks being read as a third opinion.

**Agreement is published before the result**, and disagreements are **excluded
from the headline rate rather than resolved by the author**, who is not an
independent rater. Every disagreement is listed.

## Reported quantities

- inter-rater raw agreement and Cohen's kappa
- calls audited (= all refused calls)
- both raters in-intent / both out-of-intent / disagreed / unlabelable
- **in-intent among guard-blocked calls**, over the rows both raters agreed on

## Stated limitations

- **A conditional rate, not a detector metric.** It says what fraction of
  refusals were unwanted. It says nothing about what the guard misses.
- **Selected on the outcome.** The rows are not a random sample of traffic and
  their distribution is not representative of anything.
- **Near-arithmetic rubric.** On this corpus 99% of rows are an exact amount
  match or an amount below what the intent authorizes. High agreement here
  largely shows that two people can compare two numbers; it is weak evidence
  that the ground truth is well defined.
- **Synthetic, model-generated traffic.** Not merchant traffic. The generator's
  served model is self-reported by an endpoint measured substituting models.
- **The cost falls on the compilation policy, not on enforcement.** A mandate
  can only authorize what someone wrote down; `coverage=under` cells were
  constructed so the merchant's intent exceeds what the mandate expresses, and
  those blocks are correct enforcement of an incomplete authorization.

## Files

| | |
|---|---|
| Delivered to rater 1 | `adjudication/audit-armC-e1.csv` + `LABELLING-armC.md` |
| Delivered to rater 2 | `adjudication/audit-armC-e2.csv` + the same rubric |
| Returned as | `adjudication/audit-labels-armC-e1.csv`, `-e2.csv` |
| Never delivered | `audit-rowmap-armC.json`, `audit-projection-armC.json` |
| Hashes | `adjudication/SHA256SUMS-audit-armC.txt` |
| Result | `AUDIT-armC.md`, written by `rzp-study audit-armC` |

The 340-row worksheets remain in the repository as part of arm C's record. They
were **not delivered to anyone**: sending two volunteers 340 rows to establish a
recall figure that cannot exist was not a reasonable use of their time.
