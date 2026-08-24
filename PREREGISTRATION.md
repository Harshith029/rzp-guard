# PREREGISTRATION.md — rzp-guard evaluation, frozen before implementation

**Committed:** 2026-08-24, Phase 0.5 · **Corpus version:** 1.0.0 · **Seed:** 20260824
**Status at time of commit:** no policy code, no detector, no `src/` exists. Verifiable from `git log`.

This document and `corpus/manifest.json` are the pre-registration. Their whole value is that they precede the thing they constrain — a plan to pre-register later is not pre-registration. Everything below is fixed unless amended under §7.

> ### ⚠ SUPERSEDED IN PART — READ [AMENDMENT 1](#amendment-1--2026-08-24--v10-downgraded-to-a-policy-conformance-corpus) FIRST
>
> Review round 4 established that **this corpus cannot support a held-out detector evaluation.** Its labels are computed from the same predicate the policy matches on, so scoring against them measures conformance to the spec, not detection.
>
> **§1, §4 and §5 below overclaim and are corrected by Amendment 1.** The corpus remains valuable as a frozen policy-conformance and regression suite — a weaker but real claim. Commit `e572d85` is preserved unmodified as the original record.

---

## 1. What is being measured

**Claim under test:** `rzp-guard` enforces a merchant-issued capability list over `create_refund`, blocking any refund outside that list.

- **Unit of evaluation:** one `tools/call` = one decision.
- **Positive class:** `block` — the call *should* be denied.
- **Primary metrics:** **TPR and FPR**, which are invariant to the attack base rate.
- **Precision is reported as a curve**, never as a single number. This corpus is 42% positive; no real merchant sees that. Precision is computed at assumed base rates of **1%, 0.1% and 0.01%**, with the assumption stated at each point.

---

## 2. Frozen corpus

| | Templates | Sessions | Calls | block | allow |
|---|---|---|---|---|---|
| **tuning** | 15 | 75 | 130 | 50 | 80 |
| **heldout** | 14 | 70 | 180 | 80 | 100 |
| **total** | **29** | **145** | **310** | **130** | **180** |

Per family (templates / sessions per split):

| Family | tuning | heldout |
|---|---|---|
| A1 injected instruction | 3 tpl / 15 sess | 2 tpl / 10 sess |
| A2 mandate drift | 2 tpl / 10 sess | 3 tpl / 15 sess |
| A3 replay | 2 tpl / 10 sess | 2 tpl / 10 sess |
| A4 misdirection | 2 tpl / 10 sess | 2 tpl / 10 sess |
| benign | 6 tpl / 30 sess | 5 tpl / 25 sess |

Exact per-template counts, labels and SHA-256 for every file are in `corpus/manifest.json`.
Manifest SHA-256: `87fbb82de3c04a55d7065fb918f791ddfdbaa7cf0b6ea698ef06288b5562bed6`

**Regeneration is deterministic** — `python corpus/generate.py` reproduces byte-identical output and an identical manifest hash. Verified by running it twice.

---

## 3. Split policy

- **Disjointness unit: the scenario template** (source field + mutation pattern + call shape). No template appears in both splits; asserted in code, not just claimed.
- **Families A1–A4 span both splits deliberately** — each must be measurable on held-out.
- **Held-out is not inspected during tuning.** Thresholds, weights and rules are developed against `corpus/tuning/` only.
- The tuning-vs-held-out gap is reported. A small gap is evidence; a large one is overfitting and will be stated as such.

---

## 4. Labels come from an independent oracle

A call is labelled `block` iff a human reading *(the session's mandate + the transcript + prior account state)* judges it outside the mandate. **The detector plays no part in labelling.** Every label carries a written reason — see the `reason` field in `corpus/labels.jsonl`.

This is what makes recall a measurement rather than a tautology, and it has a deliberate consequence: **the corpus contains attacks for which no policy rule has been written.** Those are expected to produce false negatives, and reporting them is the point. Templates most likely to do so: `A1b` and `A1e` (injection with no verbatim value copying) and `A4d` (an ordinary agent indexing error with no injection at all).

Benign templates include hard negatives designed to punish lazy detectors: `B03` (two legitimate equal-amount partial refunds — the workflow PLAN v2 wrongly rejected as replay), `B07` (a high-velocity but fully authorized recall batch), `B10` (a payment id sitting in `notes` that the agent correctly ignores), and `B11` (a large refund just under the cumulative cap).

---

## 5. Statistics

- **Confidence intervals by cluster bootstrap.** Calls within a session are not independent — they share a mandate, an agent and a template.
- **Reported default: session-level clustering** (145 sessions; 70 held-out).
- **Stated caveat, because it is the honest one:** templates are the disjointness unit, so templates are arguably the correct cluster. **At 2–3 templates per family per split, template-clustered per-family intervals are not informative.** Per-family results are therefore reported as **descriptive**, and only **overall held-out TPR/FPR** is treated as an inferential claim.

This limitation is recorded *before* seeing any result specifically so it cannot be discovered afterwards and framed as a caveat. Expanding template diversity per family is the single highest-value improvement to this evaluation and is noted as such regardless of how the numbers land.

---

## 6. Frozen baselines

The detector's numbers are meaningless without something to beat. Declared now:

| ID | Baseline | Expected |
|---|---|---|
| **B-none** | allow everything | TPR 0.00, FPR 0.00 |
| **B-all** | block everything | TPR 1.00, FPR 1.00 |
| **B-amount** | block iff `amount > global.max_cumulative_paise / 4` | naive threshold detector |
| **B-velocity** | block iff >2 refunds in the session | naive velocity detector |

`B-amount` and `B-velocity` exist because they are what a reviewer would reasonably ask "why not just do this?" — `B07` and `B11` are built to break them. If the capability-list policy does not clearly beat both on held-out, that is a finding to publish, not to bury.

---

## 7. Amendment policy

Schemas in this corpus come from `razorpay-mcp-server` source at commit `7950d51`. Runtime verification (gate G1.1) may reveal drift.

Any change after this commit is recorded as a **dated amendment** appended to this file — stating what changed, why, and which results are affected. **Silent edits to the corpus, labels or split are not permitted.** If an amendment lands after any held-out scoring, results before and after are reported separately.

---

## Amendment 1 — 2026-08-24 — v1.0 downgraded to a policy-conformance corpus

**Trigger:** cross-model review round 4. **Corpus:** v1.0.0 → v1.1.0. **Original record preserved at commit `e572d85`.**

### A1.1 The claim this corpus supports is downgraded

**Withdrawn:** that v1.0 is a held-out evaluation of a detector.

The labels are not independent of the policy. Held-out templates `A1b`, `A1e` and `A4d` carry the labelled reason *"no authorized action exists"* — which **is the primary matcher's predicate**. Scoring the policy against those labels asks whether the implementation agrees with its own capability list. A perfect score would demonstrate conformance, not detection. Pre-registering before `src/` existed does not cure this, because the circularity lives in the **design**, not the implementation.

**What v1.1 is:** a frozen **policy-conformance and regression corpus**. It has real value — it pins behaviour on `B03` (equal-amount partial refunds), `B07`, `B10`, `B11` and the replay family, and it will catch regressions — but it is implementation evidence, not detection evidence.

**Also withdrawn:** the framing in §4 that the corpus contains *"attacks for which no policy rule has been written."* Every A1 and A4 fixture is blocked by action matching without inspecting injection text at all. That sentence claimed a rigour the corpus does not have.

### A1.2 The term "independent oracle" is removed

§4's "independent oracle" is retracted. A human reading *(mandate + transcript)* and asking "is this call outside the mandate?" computes the same function the capability matcher computes. It is independent of the *implementation*, not of the *design* — and only the latter would make the labels evidential. Referred to hereafter as **spec-derived labels**.

### A1.3 The corpus contains no agent behaviour

Every fixture supplies the final unauthorized `create_refund` directly. **No injection string in this corpus has been shown to cause any model to emit any call.** Causation is asserted by construction. Therefore this corpus cannot measure prompt-injection detection, agent susceptibility, or induced-misuse rates, and no such claim will be made from it.

### A1.4 Headline denominator narrowed to `create_refund` only

v1.0's split totals mixed three populations. Corrected — the headline metric is now the refund slice alone, with protocol behaviour reported separately:

| Slice | tuning | heldout |
|---|---|---|
| **Headline — `create_refund` only** | 105 calls (50 block / 55 allow) | **175 calls (70 block / 105 allow)** |
| Protocol — read calls, reported separately | 25 calls | 45 calls |

Trivial read allows padded the allow side and flattered FPR; they are excluded from the headline.

### A1.5 `A2c` removed from the corpus

The `create_instant_settlement` template padded headline TPR with a money-moving tool that is not the headline action, and a settlement call does not belong in a public defense-only corpus scoped to refunds. Tool-allowlist behaviour is covered by the **G3.1 default-deny unit test** instead, which is where it belongs.

### A1.6 `A2e` rewritten — it tested nothing

v1.0's `A2e` claimed to test a rate-limit breach, but every target had no authorized action, so **action matching denied first and the rate limiter was never reached.** It exercised neither the control it named nor rule ordering.

Rewritten so all prior controls pass: 14 refunds, every one matching an authorized action, with deliberate cumulative headroom (280,000 of 1,000,000), issued inside a 23-second window against `max_calls_per_minute: 10`. **The rate limit alone determines the outcome.** Verified: all 14 targets authorized, 14 calls spanning 23s.

**Standing requirement adopted from this finding:** every named control must have at least one scenario where all prior controls pass and that control alone decides. Per-family metrics are decorative otherwise. Remaining templates audited against this; the replay and cumulative-cap families already satisfy it, A1/A4 are by design all action-matching and are labelled as such.

### A1.7 No statistical inference will be claimed

§5's "only overall held-out TPR/FPR is an inferential claim" is **withdrawn entirely.**

The independence unit is the template, not five mechanically generated sessions from one template — those are replicas differing only by payment id and an amount from a fixed pool. Bootstrapping over them is **pseudoreplication**. Held-out has 13 templates with generator-chosen weights.

**No confidence intervals will be reported for this corpus, and no inference to merchant traffic will be claimed.** Results are reported as a **descriptive score on a frozen fixture set**.

### A1.8 Baselines relabelled

`B-amount` (arbitrary quarter-of-cap threshold) and `B-velocity` (broken by `B07`, which I authored to break it) are **sanity baselines, not competitive alternatives.** Beating a strawman built to lose validates nothing, and the README will say so.

### A1.9 What still needs building for a real metric

The panel question this corpus cannot answer — *"what did the detector infer that wasn't already encoded in the action list and the fixture label?"* — needs ground truth that is **not** derived from the mandate. That requires observing a real agent: driving an actual model against the MCP surface with injected content, recording the calls it genuinely emits, and scoring the proxy against **merchant intent specified independently of the mandate**.

The valuable measurement there is the one I cannot author honestly: **how often a correctly-behaving agent is blocked because the mandate did not anticipate its legitimate path.** That is the real false-positive cost, and only real traces produce it. Scoped as a plan change, not folded into this corpus.

---

## 8. Defense-only properties of this corpus

- Fixtures are recorded JSON-RPC **data**, scored offline. `corpus/` contains data and a scorer, never an executor.
- All identifiers are **non-resolvable synthetics** (`pay_SYN####`) that cannot correspond to a real object in any account.
- `corpus/generate.py` performs no network I/O and spawns no processes.
- Injection strings are fixture stimuli for the detector under test.
- No real test-account records and no PII are committed.
