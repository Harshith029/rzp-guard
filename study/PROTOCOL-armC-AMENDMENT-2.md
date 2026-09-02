# Arm C, Amendment 2 — collection, 2026-09-01

> **The `study/VOID-RUN-1/` traces this amendment voided have been REMOVED from
> the tree (2026-09-03).** 165 files from a run this document declares void, kept
> for a while as history and referenced by nothing else. They are in git history
> if anyone wants them. The amendment itself is unchanged — the reason the run
> was voided is the part that mattered, and it is below.

Amendment 1 covered the labelling and blinding surface. This one covers
**collection**: a run was discarded, a re-run replaced it, and a void-recovery
policy is defined here **before** it is executed.

Written and committed before the recovery pass runs. That ordering is the point:
a retry policy invented after seeing which trace failed is a selection rule
wearing a policy's clothes.

---

## A2.1 — The first arm C run is discarded, and preserved as evidence

The first run completed with **118 of 162 traces void** on HTTP 429 — 44
complete. The runner had no retry of any kind, so a single rate-limit voided a
trace outright.

*(Correction: I first reported this as 108 void / 54 usable. That came from a
heuristic I wrote — "no tool calls and one turn" — when each trace already
carries an explicit `status` field. The authoritative count from `status` is 118.
The per-cell figures below were computed from `status` and were always right;
only the total was wrong. Inferring a value that is recorded is the same defect
as the rest of this project's failure log, in miniature.)*

The loss was not uniform. The runner walks the corpus in order, rate-limiting
accumulates, and `grid.py` enumerates `size=small` as G001–G027 and `size=large`
as G028–G054 — so voids tracked scenario index, which encodes a dimension:

| cell | void / traces |
|---|---|
| `size=large` | **78 / 81 (96%)** |
| `size=small` | 40 / 81 (49%) |

Attrition correlated with a dimension is a confound, not attrition. The
surviving 44 traces were overwhelmingly `size=small`, so they could not be
reported as the balanced cross product the corpus exists to provide.

**Stated plainly, because a discarded run invites exactly this suspicion:**

- **No label was assigned** from the void run. No worksheet was delivered to any
  rater from it, and none was ever emitted from it to `study/adjudication`.
- **No call from it is merged** into the re-run. The two runs are not pooled.
  Stitching two collection windows together against an endpoint measured
  substituting models would layer a second confound over the one being fixed.
- **It is preserved and reviewable**, not deleted and not left outside the
  repository: `study/VOID-RUN-1/` holds every trace, a manifest with a SHA-256
  per file, status counts, and the cell-level void table.

The transport fix is retry with exponential backoff, jitter, and `Retry-After`
honoured, for 429 and 5xx only. Other 4xx are never retried.

## A2.2 — Void recovery policy, defined before execution

**The rule, applied uniformly:**

1. After a run completes, **every** trace with `status: void` is re-run
   **exactly once**, under the same frozen corpus, model freeze and retry
   policy.
2. Recovery happens **before any worksheet is emitted**, so no rater ever sees a
   corpus that a later recovery would change.
3. The set of traces to recover is defined by `status == "void"` and by nothing
   else. **No trace is selected for retry by which cell it belongs to, by
   whether recovering it would improve balance, or by any property of the
   result** — a void has no result to select on.
4. Recovery outcomes are recorded per trace in `study/RECOVERY-armC.md`,
   including traces that were still void afterwards.
5. A trace still void after its single recovery attempt is **retained as an
   exclusion**. Its scenario, run index, exact cell and void reason are
   published in `RESULTS-armC.md`, and it is counted in the exclusions table.
   It is not retried again.

**Why exactly once.** Unbounded retries until a corpus looks complete is a
stopping rule driven by the outcome. One attempt per void, decided in advance,
is not.

**What this does not license.** It does not permit re-running a trace that
completed. A completed trace is data, whatever it contains.

## A2.3 — Scope wording, fixed

The final report must describe arm C in these terms and no stronger:

> An evaluation on **pre-registered, model-generated controlled traces**. It is
> stronger than arm B — the corpus is mechanically enumerated rather than
> authored by the implementer, the policy is frozen by hash beforehand, and
> ground truth comes from external raters who saw only a sanitised projection.
> It is **not an independent real-merchant held-out dataset**, and no number
> from it should be read as one.

Specifically it is not: real merchant traffic, a fraud rate, a model-performance
claim, or evidence about mandate authenticity. The generator's identity is
proxy-reported and unverified.
