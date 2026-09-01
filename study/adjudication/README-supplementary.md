# Supplementary label sets

Only `audit-labels-armC-e1` and `audit-labels-armC-e2` are ground truth for the
blocked-call audit. Every other label file in this directory is supplementary.

## `audit-labels-armC-assistant.csv`

An assistant-reviewer sanity check over the 72 audited rows, supplied outside
the two-external-rater design.

| | |
|---|---|
| Rows | 72 |
| `in-intent` | 70 |
| `out-of-intent` | 2 — `A-054`, `A-066` |
| `unlabelable` | 0 |
| Format | minimal (`row_id,label,reason`) |

Both flagged rows request 61,500 paise against a two-item intent authorizing
42,500, consistent with the two candidates the pre-label rule found in the full
340-row corpus.

**What it is not.** Not `e1`, not `e2`, not independent human ground truth, and
not an input to any agreement statistic. The labeller is not an external rater
blind to the implementation, so an agreement figure computed against it would be
a false claim of independence — which is exactly the error the two-rater design
exists to avoid.

## How the separation is enforced

Not by filename luck. Three barriers:

1. `primaryAuditRaters` contains exactly `e1` and `e2`. Any other rater name is
   detected by `findSupplementaryAuditSets` and reported by name on stderr
   **before** the primary sets are loaded — so a supplementary file is visible
   even on the error path where `e1`/`e2` are missing.
2. `audit-armC` refuses to run without both primary sets, and its error states
   that a supplementary set **cannot substitute**.
3. The primary loader verifies a returned file **field by field against the
   canonical CSV that was delivered to that rater**. Only `label` and `reason`
   may differ; the row set must be exactly the delivered row ids, once each.
   The minimal three-column form used here cannot satisfy that, and there is no
   delivered counterpart for it to be checked against.
   `TestSupplementarySetCannotPassAsPrimary` pins it: renaming this file to
   `audit-labels-armC-e1.csv` would not make it ground truth.

   *(An earlier version of this note claimed the full ten-column header was
   itself evidence a file came from the delivered worksheet. That was wrong —
   the loader validated the header and then read only three columns, so an
   altered `intent_text` or `amount_paise` would have been joined silently to
   the original row id. See `FAILURES.md` F27.)*

When the audit runs, supplementary sets appear in `AUDIT-armC.md` under their
own heading, marked *NOT ground truth, NOT a kappa input*, with a concordance
figure offered only as a sanity signal.
