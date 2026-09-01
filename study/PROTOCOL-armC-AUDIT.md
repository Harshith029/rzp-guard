# Arm C — post-hoc, exhaustive conditional audit of guard-blocked calls

**Post-hoc, and not pre-registered.** This audit was designed *after* arm C's
outcome was known. Arm C's pre-registration covers the grid, the ground-truth
rule, the policy freeze and predictions C1–C7; **it does not cover this**, and
nothing here may be presented as pre-registered.

**It cannot repair C6.** Arm C does not estimate recall and does not clear the
Track 2 metric bar. That stands, whatever this audit reports.

**The word "precision" is not used anywhere in this audit or its output.** The
audited set is chosen on the guard's own decision, so nothing computed over it
is a detector metric, and borrowing that vocabulary would invite the exact
misreading this study has spent eleven review rounds avoiding.

---

## The question

> **in-intent calls among guard-blocked calls**

Of the refunds this guard refused, how many did the merchant actually intend?

**This quantity is not a guard false-positive rate and must not be reported as
one.** Most of it is expected to be category B — refusals correctly enforcing a
mandate that never expressed the merchant's full intent — which is a property of
how the corpus was built, not a guard defect. Only category C bears on whether
the guard itself is wrong.

What the audit gives is an account of **operational friction**: how much of this
guard's refusing falls on refunds someone meant to make, and how that divides
between an incomplete authorization and the guard refusing authority it had.

## Why no detector metric is available here

The audited set is selected **on the guard's own decision**, so a rate computed
over it is conditional by construction: `P(in-intent | blocked)`. It says what
fraction of refusals were unwanted. It says nothing about what the guard misses,
and it cannot be converted into anything that does.

Arm C's positive class holds two candidates in 340 calls, and the calls the
guard *allowed* contain none — so neither of the two standard detector
quantities is computable, and neither is reported.

## The three-way split, and why one number would mislead

`in-intent among blocked` is **not** a guard false-positive rate. Eighteen of
arm C's 54 cells are `coverage=under`, constructed so the merchant's intent
deliberately exceeds what the compiled mandate can express. A refusal there is
correct enforcement of an incomplete authorization. Reporting a single figure
would blame the guard for a property of the corpus.

Every blocked call therefore lands in exactly one category:

| | category | reading |
|---|---|---|
| **A** | blocked, out-of-intent | security-correct denial |
| **B** | blocked, in-intent, no matching combination available | authorization/availability friction — the mandate never expressed it; the cost falls on the compilation policy |
| **C** | blocked, in-intent, a matching combination **was** available | an actual guard false positive or implementation limit — the authority existed and the guard refused |

A and B come from the raters' labels. **B versus C is decided by the guard's own
recorded available actions**, parsed from its refusal message, which is more
authoritative than reconstructing the mandate because it already accounts for
actions consumed earlier in the same trace.

The reachability check is deliberately **unbounded**, where the guard's own
search stops at eight actions. That difference is the point: a refusal caused
only by the guard reaching its bound must surface as category C, and a bounded
check would hide it by agreeing with the guard. `TestReachabilityIsNotBoundedLikeTheGuard`
pins that property.

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

**Agreement is published before the classification.** Disagreements are
**carried, not discarded**: the report publishes the agreement count and kappa,
the agreed-label conditional rate clearly labelled as such, and **conservative
bounds over all audited calls** computed by counting every disagreement once as
in-intent and once as out-of-intent. Every disagreement is listed individually.
The author does not resolve them, being not an independent rater.

## Reported quantities

- inter-rater raw agreement and Cohen's kappa, published first
- calls audited (= all refused calls, exhaustively)
- the A / B / C split, with every category C call listed individually
- **in-intent among guard-blocked calls** over agreed rows, labelled as the
  agreed-label conditional rate
- **conservative bounds** on that rate across all audited calls, with each
  disagreement counted once each way

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
