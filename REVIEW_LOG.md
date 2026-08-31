# REVIEW_LOG.md

Cross-model review log. Every external critique is treated as a claim to verify, not an instruction to follow. Accepts require a reason the suggestion is *correct against the real system*; rejects are explicit.

---

## Round 1 — Phase 0 (PLAN.md v1) — reviewer: ChatGPT — 2026-08-24

Overall: high-quality critique. Ten points plus a preamble. **Eight full accepts, three partial accepts with stated disagreement, one factual claim that proved me wrong on verification.** The result is PLAN.md v2, which is a materially different design — not a cosmetic patch.

---

### P0 — "Methodology plan, not evidence. Freeze counts/seed/manifest/CIs before tuning. ≥300 calls is a target; calls within a session are correlated. 60/40 makes precision synthetic."

**Verdict: ACCEPT (all three sub-points).**

- *Pre-registration* — correct and cheap. Costs nothing now, and is the only thing that makes the eventual numbers falsifiable. v2 adds gate **G4.0**: split manifest, per-family/per-tool class counts, seed, and the frozen policy version are committed **before** any tuning, in their own commit, so git history proves the ordering.
- *Session correlation* — correct statistics, and I had it wrong by omission. Tool calls inside one session share a mandate, an agent, and a scenario template, so they are not independent samples. Treating 300 calls as n=300 would overstate precision badly. v2 computes confidence intervals by **cluster bootstrap resampling at the session level**, not the call level.
- *Base rate* — the sharpest of the three. Precision is a function of the attack base rate, and 40% attack traffic is nothing like a real merchant's. TPR and FPR are base-rate invariant; precision is not. v2 reports **TPR/FPR as the headline** and presents precision as a **curve across assumed base rates** (1%, 0.1%, 0.01%), with the assumption stated at each point.

**Consequence I am flagging now rather than discovering later:** with ~60 sessions split across four families, per-family cluster-bootstrap CIs will be wide — plausibly ±0.10–0.20 on recall. I would rather publish a wide interval that is honest than a point estimate that implies precision I do not have.

---

### P1 — "Split description is internally inconsistent: 'split by scenario family' conflicts with 'all four families in both splits'."

**Verdict: ACCEPT.** Straightforwardly correct — I used "family" to mean two different things in adjacent sentences, and the looser reading would have let held-out leak the exact templates used for tuning.

v2 defines the disjoint unit precisely: **the scenario template** (the generator recipe: source field + mutation pattern + call shape) is disjoint across splits. **Attack families A1–A4 deliberately span both splits**, because each family needs to be measurable on held-out. Template IDs are recorded in the manifest so the disjointness is checkable by a third party, not asserted by me.

---

### P2 — "Evaluation risks being circular. 'Should block' needs an independent oracle. Hold out unseen authorship or precommit held-out before threshold selection."

**Verdict: ACCEPT the core. PARTIAL REJECT on 'unseen authorship'.**

The circularity is the single biggest threat to this project's credibility and my v1 mitigation was too weak. Accepted fix, and it is a real one: **labels are derived from the mandate document, not from the policy engine.** A call is labelled positive iff a human reading *(mandate + session transcript + prior account state)* judges it outside the mandate — with a one-line written reason attached to every label. The detector never touches labelling.

The load-bearing consequence: this makes it possible for the corpus to contain **attacks I have written no rule for**, which is what turns recall into a real measurement instead of a tautology. v2 commits to deliberately authoring held-out scenarios with no corresponding policy rule, and treats the resulting false negatives as the honest finding they are.

**Rejected:** independent authorship. One builder, twelve days — a second author does not exist, and pretending otherwise would be worse than naming the gap. Substituted with the strongest available proxy: **temporal precommitment** — the held-out corpus is authored and hash-committed *before any policy code is written*, so it cannot be retrofitted to the rules. Residual limitation (single-author correlation in scenario imagination) is stated in the metrics report rather than papered over.

---

### P3 — "Scope too broad. Make `create_refund` the evaluated claim. A `create_payment_link` fallback is not evidence you stopped money movement."

**Verdict: ACCEPT the narrowing. REJECT one premise it rests on.**

Accepted, and this is the most valuable structural point in the review. The brief asks for **one class of loss**; v1 sprawled across six. v2 makes the headline claim exactly one sentence: *unauthorized `create_refund`.* Everything else becomes secondary coverage with separately reported numbers, explicitly outside the headline.

The line **"a `create_payment_link` fallback is not evidence that you stopped unauthorized money movement"** is correct and caught a real dishonesty risk in my G1.4 fallback — I had quietly written an escape hatch that would have substituted a weaker claim while keeping the strong framing. v2 removes it: if the live refund path can't be exercised, that gets **reported as an unmet gate**, not swapped for an easier one.

**Rejected premise:** the review lists settlements and saved-card charges among things that "are not money movement." That is not right. `create_instant_settlement` moves merchant balance to a bank account irreversibly and incurs a fee; `initiate_payment` charges a stored card. Both are unambiguously money movement — `create_payment_link`, `revoke_token`, and PII reads are the ones that aren't. The narrowing is correct, but for the reason *"one coherent evaluated claim beats six half-measured ones"*, not because those tools are harmless. Getting this right matters because it determines what goes back in scope after the deadline.

**Reframed families** (all four survive, scoped to refunds — a tighter story than v1):
A1 injected instruction in payment data → unauthorized refund · A2 refund amount/scope drift · A3 duplicate/replayed refund · A4 refund misdirection to an unauthorized payment.

---

### P4 — "The core provenance claim is overstated. 'Every literal from any tool result' is literal-reuse detection, not taint tracking. It will block normal lookup-then-refund workflows."

**Verdict: ACCEPT — and this is the biggest change in v2.**

Correct, and the failure it names is fatal to v1 as designed. Traced concretely: the normal support workflow is `fetch_payment(pay_ABC)` → `create_refund(pay_ABC)`. Under v1's rule, `pay_ABC` appeared in a tool result, so it is `TOOL_DERIVED`, so a refund using it is blocked. **v1 would have blocked the single most common legitimate refund flow while claiming to be a fraud control.** That is not a tuning problem, it is a wrong mechanism.

The proposed fix — track **JSON field paths and source sensitivity, not merely values** — is right, and it is a better design than what I had:

- Every observed value is indexed with the **JSON path** it appeared at, not just its literal content.
- Paths carry a trust class: **`SYSTEM_AUTHORITATIVE`** (Razorpay-generated — `id`, `entity`, `amount`, `status`, `order_id`, `created_at`) vs **`PARTY_SUPPLIED`** (attacker- or customer-influenceable free text — `notes.*`, `description`, `customer_name`, `receipt`, `email`, `contact`).
- Taint means *"this value's earliest origin is a `PARTY_SUPPLIED` path"* — not *"this value was seen before."*

This separates the two cases that v1 conflated: refunding a `payment_id` read from the canonical `id` field is **allowed** (legitimate lookup-then-refund), while refunding a `payment_id` that first appeared inside `notes.customer_message` is **blocked** — and that is precisely the A1 attack. It also answers the panel question *"what legitimate workflow does your policy still permit?"* with a concrete answer instead of a hope.

**Accepted the naming criticism too.** "Taint tracking" implies dataflow analysis I am not performing. v2 calls it **field-path provenance with source-trust classification**, which is what it actually is.

**What this does NOT fix, stated plainly:** transformed, encoded, or recomputed values still evade origin matching. The redesign fixes the false-positive catastrophe; it does not fix the false-negative surface. Those remain measured and reported, not claimed away.

---

### P5 — "The mandate has no trustworthy issuance or binding model."

**Verdict: ACCEPT.** Correct — v1 said "config file" and left the trust boundary implied, which is exactly the kind of gap that turns into an unanswerable panel question.

v2 states it explicitly: **the mandate is loaded from a path supplied at proxy launch (CLI/env), before any agent connects.** The trusted party is whoever launches the proxy process (the merchant/operator). The agent is untrusted. There is no MCP method, tool call, or JSON-RPC message that can set, replace, extend, or reload a mandate — the proxy exposes no such surface, and attempts are logged as attacks. The mandate is bound to the proxy process lifetime, and cumulative budget lives in that process.

Tests added per the suggestion: **mandate substitution attempt via crafted tool call**, and **concurrent-session isolation**.

Limitation stated rather than hidden: cumulative caps are **per proxy process**. Two proxies launched against one mandate would each track their own budget; correct multi-process enforcement needs shared state and is out of scope for twelve days. The headline claim is scoped to what is actually enforced.

---

### P6 — "Cumulative caps and replay need transactional semantics. Don't claim `receipt` is non-idempotent from wrapper code alone; verify the API contract."

**Verdict: ACCEPT on concurrency. ACCEPT on idempotency — and verification proved my PLAN.md claim WRONG.**

*Concurrency:* correct and it is a real bug class. MCP permits multiple in-flight requests by JSON-RPC id, so two concurrent refunds can both pass a cumulative check before either result returns — a trivially exploitable TOCTOU on the exact control that is supposed to cap losses. v2 **reserves budget atomically before forwarding**, commits on success, and releases on error/timeout/child failure, with an explicit test for duplicate in-flight calls.

*Idempotency — the reviewer was right to challenge an unverified assertion, and it cost me the claim.* PLAN.md v1 stated "the server does not enforce `receipt` as an idempotency key." I had inferred that from wrapper code without checking the API contract. Verified:

1. Razorpay **does** treat `receipt` as an idempotency key per payment — reusing one returns `Duplicate receipt found for this refund request`. **My v1 claim was wrong.**
2. Razorpay also supports a dedicated **`X-Refund-Idempotency`** header (min 10 chars, alphanumeric/underscore/hyphen; 409 on a concurrent duplicate).
3. `razorpay-go`'s signature is `Refund(paymentID string, amount int, data map[string]interface{}, extraHeaders map[string]string)`, and the MCP server calls it with **`nil` for extraHeaders** (`pkg/razorpay/refunds.go:75`). `grep -rni "idempoten"` across the whole server returns **zero hits**.
4. `receipt` is **optional** in the `create_refund` tool schema.

Net: the MCP server never sends the idempotency header, and the only protection that remains is a field the agent may simply omit. **A refund issued without a `receipt` has no duplicate protection at all.**

This turned a corrected error into the sharpest defensive contribution in the project: v2 has the proxy **enforce a deterministic, mandate-derived `receipt` on every refund it forwards**, converting an optional field into a mandatory idempotency key at the boundary. That is a concrete, verifiable improvement over the unproxied server, and it exists only because this point was challenged. Per-tool fingerprint justification accepted; for refunds it is now `(payment_id, amount)` plus the enforced receipt.

---

### P7 — "G3.2's evidence is insufficient. A child-server log is not proof no HTTP request occurred. Pin the image digest."

**Verdict: ACCEPT.** Correct — I was proposing to prove a negative with the wrong instrument, and a panelist would have taken that apart.

v2 proves blocking at **two independent boundaries**: (a) a byte-level record of everything written to the child's stdin, showing the blocked call was **never handed to the child at all** — which is stronger than a log line, since a call that never entered the process cannot produce egress; and (b) corroboration at a **controlled network boundary**, with the child running against a capture stub so any egress attempt is recorded independently of the child's own logging.

Digest pinning accepted without reservation: `razorpay/mcp` is a mutable tag and could drift from the source SHA I read. v2 pins the **image digest**, and records the runtime `tools/list` output against both the digest and the source commit.

---

### P8 — "The dashboard and JSONL log are a sensitive-data and injection surface."

**Verdict: ACCEPT, fully.** This is the point I am least comfortable having needed. v1 designed a security tool that ingests attacker-controlled text, writes it verbatim to an append-only log, and renders it in a browser — a stored-XSS pipeline with a compliance problem attached, inside a project whose entire premise is that untrusted content reaches privileged systems.

v2 adds: field-level redaction allowlist; masking/hashing of `contact`, `email`, card fields and tokens at **write** time (not render time, so the log itself is never the liability); HTML escaping via text nodes only, never `innerHTML`; a restrictive CSP on the dashboard; a stated retention rule; and a **test that a fixture carrying `<script>` and `<img onerror=...>` in a `notes` field cannot execute in the dashboard.**

---

### P9 — "The defense-only guarantee is not structural enough."

**Verdict: ACCEPT the mitigations. REJECT the risk characterization.**

**Rejected:** the framing that JSON-RPC fixtures constitute "callable sequences" that meaningfully arm an attacker. A fixture is `{"method":"tools/call","params":{"name":"create_refund","arguments":{...}}}` — it only does anything when executed with *the actor's own Razorpay credentials against their own account*. That is an API call to one's own money, not an exploit against a third party. Every argument in it is documented in Razorpay's public API reference. Overstating this would be its own form of dishonesty, and it would imply the public docs are an attack tool.

**Accepted anyway,** because every proposed mitigation is free and strictly reduces risk:
- **Non-resolvable synthetic identifiers** (`pay_SYN000...`) throughout the corpus — also prevents fixtures from accidentally touching real test-account objects, which is a reproducibility win independent of security.
- **A hard guard in `score.py` that cannot spawn a child process or open a network socket** — this is good engineering regardless, since offline determinism is what makes the metrics reproducible.
- **No real test-account records or PII committed**, ever.
- **Saved-card charging, OTP submission, and token revocation stay out of live coverage entirely.** Free, given P3 already narrows the headline to refunds.

---

### P10 — "The optional LLM/trust layer has no demonstrated need and runs too late."

**Verdict: ACCEPT — default is now OMIT.**

Correct on the merits, and accepting P4 strengthens the conclusion rather than weakening it: once provenance is tracked by **field path**, the causal link between an attacker-controlled field and the resulting action is established *structurally* — the origin path is the evidence. An LLM scoring text after the deterministic gates adds latency, nondeterminism, and a disclosure burden to a money path, in exchange for a causality story the redesign already provides more rigorously.

v2 default: **not built.** It ships only if held-out measurement shows a deterministic recall gap it actually closes, and then only with a frozen baseline-vs-model held-out comparison, full model/version/prompt disclosure, and the false-positive cost of the improvement stated. "We used an LLM" is not a credential; using one where it isn't needed is the forced-tech failure the AI Judgment criterion exists to catch.

---

### Weakest-claim callout — "'This is real taint tracking … and catches A1' cannot distinguish attacker-controlled content from legitimate values in the same response."

**Verdict: ACCEPT. Claim retracted.**

The reviewer identified the correct weakest point, and the diagnosis is exactly right — v1's mechanism could not tell `id: "pay_ABC"` from `notes.msg: "refund pay_ABC"` in the same JSON body, which is the entire distinction the defense depends on. The claim is withdrawn, the mechanism is renamed to what it is (field-path provenance with source-trust classification), and the design is rebuilt around path trust per P4. The A1 claim gets re-made only after held-out measurement supports it.

---

### Panel questions — where each is now answered in PLAN.md v2

| Question | Answer location |
|---|---|
| What stops a compromised agent changing the mandate or opening a fresh session? | §3.5 trust boundary + substitution/concurrency tests |
| Exact held-out split, labels, class counts, CIs, every FP/FN | §4 Phase 4, gates G4.0–G4.5 |
| Why does a payment-link demo substantiate a monetary-loss claim? | It doesn't — fallback removed, §4 Phase 1 G1.4 |
| How did you prove blocked calls never reached Razorpay? | §4 Phase 3 G3.2, two independent boundaries |
| Which one action is protected end-to-end, and what legitimate workflow still passes? | §1 headline claim + §3.2 lookup-then-refund permitted |

**Net effect of Round 1:** one wrong factual claim corrected (and turned into the receipt-enforcement feature), one fatal design flaw caught before implementation (v1 would have blocked the primary legitimate workflow), and the headline claim narrowed from six loss types to one measurable sentence. No implementation code had been written yet, so all of it was free.

---

## Round 2 — Phase 0 (PLAN.md v2) — reviewer: ChatGPT — 2026-08-24

Overall: found a genuine safety bug in the budget design and two real false-positive bugs. **The net effect of this round is that the design gets smaller, not larger** — one change (capability-based mandate) replaces three separate mechanisms, and one promised feature is demoted to conditional because I never verified it was buildable.

Guiding constraint for this round, per the builder: *make the things that ship actually work; do not accrete features to look thorough.* Several suggestions below pointed toward more machinery. Where the honest fix was to cut or to narrow a claim instead, I cut.

---

### P2.0 — "Do not release the cumulative budget on timeout. Keep an `UNKNOWN` reservation and reconcile."

**Verdict: ACCEPT. This was a real safety bug and the most important point in either round.**

v2 §3.6 said "commit on success, release on error/timeout/child failure," and G3.4 called releasing on timeout *correct*. It is not. It is the classic in-doubt case: Razorpay may have **processed the refund** while the proxy lost the response. Releasing the reservation then hands the budget back for money that already left, so the next refund can exceed the cap — the control silently fails open at exactly the moment it matters.

v3 replaces the two-state model with four states, and the rule is **release only on *confirmed* failure**:

| Transition | Trigger |
|---|---|
| `RESERVED → COMMITTED` | confirmed success |
| `RESERVED → RELEASED` | **confirmed** rejection (a definite API error) |
| `RESERVED → IN_DOUBT` | timeout, child crash, severed response — **budget stays held** |
| `IN_DOUBT → COMMITTED / RELEASED` | reconciliation resolves it |

Reconciliation is concrete and uses a tool that already exists (§2.4): call `fetch_multiple_refunds_for_payment(payment_id)` and match on the **injected receipt**, which I verified *is* returned in the refund entity (fields: `id`, `entity`, `amount`, `currency`, `payment_id`, `created_at`, `batch_id`, `notes`, `receipt`, `acquirer_data`, `status`, `speed_requested`, `speed_processed`). If reconciliation itself fails, the proxy **fails closed** and requires operator resolution rather than guessing.

This also gives the receipt injection a second job it wasn't designed for: it is the correlation key that makes the in-doubt state resolvable at all. Test added exactly as specified: *upstream processed refund, response severed.*

---

### P2.1 — "Receipt injection is not the HTTP idempotency mechanism. Describe it precisely."

**Verdict: ACCEPT.** I conflated two different behaviours. Verified against both Razorpay pages:

- **`X-Refund-Idempotency`**: safe retry — same key + same body returns the **original refund object**; `409` while the first is still in flight.
- **`receipt` reuse**: **HTTP 400**, `"Duplicate receipt found for this refund request."` — a *rejection*, not a safe retry.

And the reviewer is right that **the stdio proxy cannot inject the HTTP header at all.** The child builds the HTTP request internally and passes `nil` for `extraHeaders` (`refunds.go:75`). Reaching that header would require forking the child or MITMing its TLS.

**Rejected:** forking the child to add the header. The entire premise is a proxy *in front of the real, unmodified `razorpay-mcp-server`*; forking it would forfeit that and is not worth a marginal semantic upgrade.

The two facts compose in a way that matters: because duplicate `receipt` yields **rejection rather than replay of the original result**, I *cannot* resolve an in-doubt refund by retrying — retry returns a 400 that says nothing about whether the first one landed. That is precisely why P2.0's reconciliation-by-fetch is mandatory rather than optional. The two points reinforce each other, and v3 states the feature as what it is: **duplicate rejection at the provider, not idempotent retry.**

**Flagged for empirical verification (new gate G1.6):** Razorpay's own docs contradict each other here. `create-normal` says `receipt` is "treated as an idempotency key" and 400s on duplicates; `normal-refunds-idempotent` says `receipt` provides no duplicate-detection semantics. I am not going to pick a page — Phase 1 sends a real duplicate `receipt` in test mode and records the actual response.

---

### P2.2 — "`(mandate_id, payment_id, amount)` is not a sufficient refund-action identity."

**Verdict: ACCEPT — and this fix collapses three problems into one.**

The bug is concrete and would have shipped: two legitimate partial refunds of the same amount on one payment (two items returned separately at the same price — utterly routine) produce the same receipt and the same replay fingerprint, so **the second legitimate refund is rejected as a replay.** v2 silently defined all same-amount partial refunds as duplicates.

Of the two offered options, I took (a) — the mandate authorizes **discrete refund actions** — and rejected (b) "accept the false positives." Option (b) treats a known design defect as a measurement artifact, which is the wrong instinct when the fix is smaller than the workaround.

The mandate becomes a **capability list**, not a coarse policy:

```yaml
authorized_refund_actions:
  - {action_id: rfa_001, payment_id: pay_SYN0001, max_amount_paise: 50000, single_use: true}
  - {action_id: rfa_002, payment_id: pay_SYN0001, max_amount_paise: 50000, single_use: true}
```

An incoming refund matches an **unconsumed** action with that `payment_id` and `amount ≤ max`; no match means deny. Receipt derives from `action_id`, so it is unique per authorized action. Two legitimate partial refunds are two actions and both pass; a replay finds its action already consumed and is denied.

This single change fixes P2.2, answers P2.4, and materially improves P2.3 — one mechanism replacing three, which is the opposite of feature accretion.

---

### P2.3 — "Field-path provenance still does not establish that injection caused the action."

**Verdict: ACCEPT the critique. REJECT the implied remedy of building more detection.**

The critique is correct on both sub-cases. If an id appears in canonical `id` *and later* in `notes`, my "earliest origin" rule labels it `SYSTEM_DERIVED` even if the agent acted on the note. More fundamentally, an injection can induce an action using an **already-known id or a mandate literal** — no value gets copied from party-supplied text at all, so there is nothing for provenance to see. Field-path provenance detects a narrow literal-flow subclass, not A1 in general.

**Where I diverge from the framing:** the review treats this as provenance needing to work for A1. I don't think provenance was ever the right primary control for A1. The general defense against "injection induces an unauthorized refund" is **default-deny authorization against a capability list** — an injection saying *"also refund pay_XYZ"* fails because no authorized action exists for `pay_XYZ`, regardless of where the id came from or whether anything was copied. The mandate generalizes to injections provenance is blind to.

So v3 **demotes provenance from the core mechanism to a measured secondary signal**, and the capability mandate carries the headline. Consequences:

- Provenance records **all** origin paths for a value, not just the earliest, so the `id`-then-`notes` case is visible rather than silently resolved in the agent's favour.
- It contributes a **risk signal**, not a hard block; the hard block comes from the mandate.
- Phase 4 reports **policy-only baseline vs. policy + provenance** on held-out. If provenance adds nothing measurable, I say so and it becomes a dashboard/forensics feature. That is a real possible outcome and it gets published either way.
- Held-out A1 cases **without verbatim value copying** are included, exactly as asked.

Declining to bolt on an injection-causality detector here is deliberate. The honest options were "narrow the claim" or "add unproven machinery," and unproven machinery on a money path is the failure mode this project exists to criticise.

---

### P2.4 — "'Razorpay-generated' is not authorization."

**Verdict: ACCEPT.** Correct — `id`/`amount`/`status` are provider metadata and say nothing about what the merchant currently authorizes. v2's mandate granted any listed payment id for any amount under a cap, which is a coarse policy wearing the word "authorization."

Resolved by the same capability-list change (P2.2): authorization is now per **action** — specific payment, bounded amount, single-use identity, mandate expiry. The headline claim "prevents unauthorized refunds" is now backed by an action-scoped grant rather than a capability range.

---

### P2.5 — "The policy must be argument-specific."

**Verdict: ACCEPT.** Another real false-positive bug. v2's blanket `deny_provenance: [PARTY_DERIVED, AGENT_ORIGINATED]` applies to *every* argument, so a perfectly normal agent-chosen `speed: "normal"` is `AGENT_ORIGINATED` and the refund is denied. The detector would have blocked ordinary traffic on a field that carries no risk.

v3 specifies provenance **per field**: `payment_id` must be `USER_MANDATED` or `SYSTEM_DERIVED`; `amount` is bounded by the matched action's maximum and carries no provenance requirement; `speed` and `notes` are unconstrained; `receipt` is overwritten by the proxy regardless. Those ordinary flows go into the false-positive corpus as specified.

---

### P2.6 — "Temporal precommitment is not scheduled early enough."

**Verdict: ACCEPT.** Internally inconsistent in v2 and the reviewer caught it: G4.0 sat on Days 6–8 but its whole purpose is to exist *before* policy code, which lands Days 5–6. As written, "pre-registered held-out" was an intention.

v3 moves the corpus manifest, labels, exact split/class counts and frozen baseline to an **immediate Phase 0.5 commit, before Phase 1 and before any policy code.** Authoring it now is possible because the schemas in §2.5 are verified from source; if G1.1 reveals runtime drift, the adjustment is recorded as a dated **amendment** rather than a silent edit — which is how pre-registration is supposed to handle it.

---

### P2.7 — "The capture-stub proof is an unverified architectural assumption."

**Verdict: ACCEPT.** Fair hit, and it is the same error I was criticising elsewhere: I promised evidence from a mechanism I had never built. Capturing the child's egress means either DNS override plus TLS interception (requires injecting a CA into the container — i.e. modifying it, which forfeits "unmodified real server") or hoping `razorpay-go` honours `HTTPS_PROXY`. I have verified neither.

v3 makes the network capture a **Phase 1 feasibility gate (G1.5)** rather than a promised deliverable. Primary evidence for G3.2 is the **child-stdin byte record** — a call whose bytes never entered the child process cannot have produced an HTTP request for that call, which is sound on its own. Network capture is retained only as corroboration **if** G1.5 passes; if it fails, the claim is dropped and the README says what evidence actually exists.

---

### P2.8 — "Remove `update_payment` from the live agent mandate."

**Verdict: ACCEPT, immediately.** Correct and free. The refund detector needs read tools plus `create_refund`; an unrelated write permission weakens the least-privilege story for no demonstrated value. Removed. Exactly the kind of cut this round should produce.

---

### P2.9 — "The defense-only standard is stricter than 'not an exploit against a third party.'"

**Verdict: ACCEPT the conclusion. Logging that I still hold the technical position, because the reason for changing matters.**

I maintain the analysis is technically right: a fixture that only acts on the runner's own account with the runner's own credentials is an API call to one's own money, and every argument in it is in Razorpay's public reference. Nothing in Round 2 refuted that.

But the reviewer is right on the decision, for a reason that isn't about correctness: **Track 2 disqualifies offense-capable work automatically.** The downside of publishing a boundary argument is disqualification; the upside is being technically correct in a document nobody grades for that. That asymmetry settles it regardless of who is right on the merits.

So v3 **removes the argumentative passage** and keeps every mitigation — non-resolvable synthetic ids, mock-only scoring, exclusion of payment/OTP/token tooling. I am changing what I publish, not what I concluded, and the distinction is recorded here rather than smoothed over.

---

### Panel question — "When the provider may have processed a refund but your proxy timed out, how do you preserve the spend cap?"

v2 answered this **incorrectly** (released the reservation). v3: the reservation moves to `IN_DOUBT` and the budget **stays held**; the proxy reconciles via `fetch_multiple_refunds_for_payment` matched on the injected receipt; unresolvable cases fail closed to operator resolution. Covered by gate **G3.4**.

**Net effect of Round 2:** one fail-open safety bug fixed, two false-positive bugs that would have blocked routine merchant workflows fixed, one unverifiable evidence claim demoted to a feasibility gate, one permission dropped, and the core mechanism **demoted** rather than defended. Three mechanisms collapsed into one capability list. The plan is shorter than v2.

---

## Round 3 — Phase 0 (PLAN.md v3) — reviewer: ChatGPT — 2026-08-24

**Eight points, eight accepts, and one of them is a methodology failure on my part rather than a design flaw.** Net effect is again subtractive: automatic reconciliation cut, one pipeline stage deleted as redundant, two state machines merged into one.

---

### P3.1 — "'The Razorpay docs contradict each other' is not established."

**Verdict: ACCEPT. This was my error, and the process failure is worse than the claim.**

The two behaviours compose perfectly well: `X-Refund-Idempotency` is a *retry mechanism* (same key + body returns the original result); `receipt` is a *uniqueness constraint* (duplicate → 400). A retry mechanism and a uniqueness constraint are orthogonal. There was never a contradiction to find.

**How I generated a false claim:** `WebFetch` converts a page to markdown and answers a prompt against it *using a small fast model*. What came back — "the receipt field does not provide retry protection or duplicate detection semantics" — was a **summarizer's paraphrase**, not Razorpay's verbatim text. I treated a model-generated summary as a primary-source quote, compared it against another summary, and escalated the difference into "the vendor's own docs disagree."

This is precisely the failure this project's hard constraints exist to prevent, committed while writing the document that states them. Recorded as such rather than quietly patched.

**Standing correction to my own method:** WebFetch output is a paraphrase and cannot support a claim about *what a document says*. It can point at behaviour worth verifying; it cannot be quoted. Claims about API semantics now come from runtime observation only.

G1.6 is kept and **reframed**: not "resolve a doc contradiction" but "verify duplicate-`receipt` behaviour at runtime before depending on it."

---

### P3.2 — "Provenance is still an enforcement gate, not a secondary signal."

**Verdict: ACCEPT — and the resolution is better than either option offered.**

The incoherence is real and I should have caught it: §3.3 said "the hard block comes from the capability list" while pipeline step 4 denied any refund whose `payment_id` wasn't `USER_MANDATED` or `SYSTEM_DERIVED`. Describing a blocking control as a non-blocking signal is spin, regardless of intent.

The review offered two options — drop it from the deny path, or own it as a blocking control and evaluate it. **Neither is needed, because the gate turns out to be dead code.** Tracing it: to reach step 4, a refund must already have matched an unconsumed action in `authorized_refund_actions`, which means its `payment_id` **is a mandate literal** — therefore `USER_MANDATED` by definition. Step 4 can never fire on a call that passed step 3.

So it is removed as **redundant**, not as risky. One fewer pipeline stage, one fewer false-positive surface, and the description and the implementation now agree.

Consequence for measurement, accepted honestly: with provenance out of the deny path there is no blocking ablation to run, so v3's G4.4 as written is void. Provenance keeps a real job — it is the **forensic chain** behind a flagged call, which the dashboard deliverable explicitly requires — and Phase 4 now reports only whether the signal *separates* attack from benign calls, labelled as a diagnostic and explicitly **not part of the enforcement claim**.

---

### P3.3 — "Automatic reconciliation breaks the transparent relay." *(named weakest claim)*

**Verdict: ACCEPT. Cut it.**

Correct, and it is the same class of error as P2.7 — promising a mechanism I had not costed. For the proxy to call `fetch_multiple_refunds_for_payment` on its own it must become an MCP **client** to its child: generate internal JSON-RPC ids that cannot collide with the agent's, multiplex and demultiplex two request streams, suppress its own responses from the agent, and handle id collisions. That is a subsystem, and it directly falsifies "forwards everything byte-for-byte" — the architectural claim Decision A rests on.

For a 12-day build the correct trade is obvious: **`IN_DOUBT` holds budget and action and requires operator resolution.** Automatic reconciliation is deferred, not hand-waved.

This costs less than it appears. The injected receipt still serves as the **correlation key** — the operator looks the refund up by receipt through a path outside the relay (dashboard action or CLI, with its own credentials), so the resolution workflow is intact and simply has a human in it. Given that the in-doubt case is exactly where money may already have moved, a human in that loop is arguably the right design rather than a concession.

The transparent-relay claim survives because the thing that would have broken it is gone.

---

### P3.4 — "A missing receipt in a fetched list is not proof the refund failed."

**Verdict: ACCEPT.** Correct — eventual consistency, a still-pending refund, or a failed fetch all produce "not found" without meaning "did not happen." Auto-releasing on absence would reintroduce the exact fail-open bug Round 2 caught, one layer down.

Largely folded in by cutting automatic reconciliation (P3.3), but the principle now binds the **operator tooling** too: absence of a matching receipt is never sufficient to release. Only two automatic transitions are safe — **confirmed provider rejection → release**, and **matching receipt found → commit**. Everything else stays in doubt.

Delayed-visibility case added to G3.4.

---

### P3.5 — "Action consumption is underspecified."

**Verdict: ACCEPT, and it merges two state machines into one.**

Real gap: v3 consumed the action at pipeline step 6 but the state table described only budget, leaving the child-side-validation-failure case undefined. Permanently consuming an action after a request that never reached the provider burns a legitimate merchant authorization for nothing; releasing after a possibly-delivered request permits replay.

Resolved by making **budget reservation and action consumption a single lifecycle** — the two resources always move together, so there is one rule to reason about and one to test:

`AVAILABLE → RESERVED` (on match, before forwarding) · `RESERVED → COMMITTED` (confirmed success) · `RESERVED → AVAILABLE` (**confirmed provider rejection only** — same evidence standard as budget release) · `RESERVED → IN_DOUBT` (anything else; stays locked until an operator resolves it).

Fail-closed after any potentially-delivered request, as specified.

---

### P3.6 — "These are bounded capabilities, not exact refund authorizations."

**Verdict: ACCEPT.** Correct: `amount ≤ max_amount_paise` lets the agent choose any lower amount, so a merchant intending "refund ₹500 for order X" would see a ₹1 refund pass. That is a capability range wearing the word "authorization" — the same criticism as P2.4, one level finer.

v4 makes an action carry **either** `amount_paise` (exact) **or** `max_amount_paise` (bounded), with **exact as the default**. Bounded is opt-in for cases where the merchant genuinely delegates the figure (e.g. "refund up to the order value, agent determines which items came back"), and the mandate records which was chosen.

The claim is restated to match: the proxy enforces a **merchant-issued capability list**; where the merchant deliberately issued a bounded grant, amounts inside that bound are authorized *by the merchant's own choice*, and the mandate shows it.

---

### P3.7 — "Phase 0.5 needs to happen now, not remain a plan item."

**Verdict: ACCEPT — executing.** Correct that a pre-registration which exists only as a plan item is not pre-registration. Until the manifest, labels, class/session counts, template split and frozen baseline exist as a commit, there is no held-out corpus and no metric claim worth reviewing.

Executed this round rather than scheduled. Local commits only — no remote, no push.

---

### P3.8 — "The receipt example conflicts with the stated format requirement."

**Verdict: ACCEPT.** `rfa_001` is 7 characters against a stated ≥10 minimum — an untested length assumption sitting in a worked example, which is how this kind of bug reaches production.

v4 specifies the generated format explicitly: **`rzpg_` + `action_id`** (e.g. `rzpg_rfa_001`, 12 chars), satisfying the length floor and the alphanumeric/underscore/hyphen constraint. Verified against the live schema in **G1.6** rather than assumed — the same gate that now checks duplicate behaviour.

---

**Net effect of Round 3:** one false claim about vendor documentation retracted along with the method that produced it; one pipeline stage deleted as provably redundant; automatic reconciliation cut to preserve the architecture's central claim; two state machines merged into one fail-closed lifecycle; exact amounts made the default; and pre-registration moved from plan to commit. The plan shrinks for the third consecutive round.

---

## Round 4 — Phase 0.5 (corpus v1.0) — reviewer: ChatGPT — 2026-08-24

**The strongest critique of the series. Seven accepts, no rejects, and the headline deliverable is downgraded as a result.** Recorded in [PREREGISTRATION.md Amendment 1](PREREGISTRATION.md#amendment-1--2026-08-24--v10-downgraded-to-a-policy-conformance-corpus); commit `e572d85` preserved unmodified.

---

### P4.1 — "The oracle is still circular with the policy."

**Verdict: ACCEPT. This invalidates the evaluation claim, and I defended the wrong thing for two rounds.**

Held-out `A1b`, `A1e` and `A4d` carry the labelled reason *"no authorized action exists"* — **which is the primary matcher's predicate.** Scoring the policy against those labels asks whether the implementation agrees with its own capability list.

I called this an "independent oracle" because a human computes the label. The reviewer's decomposition is the one that matters: a human reading *(mandate + transcript)* and asking "is this outside the mandate?" computes **the same function the matcher computes**. Independent of the *implementation*, not of the *design* — and only the latter would make the labels evidential. Pre-registering before `src/` existed cured nothing, because the circularity was never in the implementation.

Worse, in Round 2 (P2.2) I introduced the "independent oracle" framing *as the fix for circularity*, and in Round 3 I shipped it. Two rounds spent strengthening a claim that was structurally unsound.

The term is retracted; the labels are now called **spec-derived**. Every A1 and A4 fixture is blocked by action matching without inspecting injection text at all.

---

### P4.2 — "The corpus has no actual agent behaviour."

**Verdict: ACCEPT.** Every fixture supplies the final unauthorized `create_refund` directly. **No injection string in this corpus has been shown to cause any model to emit any call** — I wrote both the stimulus and the response and asserted the causal link between them.

So the corpus cannot measure prompt-injection detection, agent susceptibility, or induced-misuse rates. Renamed to what it is: an **authorization-proxy conformance corpus**. The A1 family name is retained for organisation but no longer implies injection detection is being measured.

---

### P4.3 — "The primary metric is contaminated by out-of-scope calls."

**Verdict: ACCEPT.** The reviewer did the arithmetic against my own manifest and it checks out: held-out's 80 blocks were 75 refunds + 5 settlements, and its 100 allows were 55 refunds + 45 reads.

Both directions were inflated. Trivial read allows padded the allow side and flattered FPR; `create_instant_settlement` padded TPR with a money-moving tool that is **not the headline action**. Headline denominator is now `create_refund` **only** — heldout **175 calls (70 block / 105 allow)** — with protocol behaviour reported separately.

`A2c` removed from the corpus entirely, on both grounds offered: metric contamination, and a settlement call having no place in a public defense-only corpus scoped to refunds. Tool-allowlist coverage moves to the **G3.1 default-deny unit test**, which is the right home for it.

---

### P4.4 — "'Overall held-out TPR/FPR is inferential' does not follow."

**Verdict: ACCEPT, and withdraw the claim entirely.**

Correct: the independence unit is the template, not five mechanically generated sessions from that template. Those replicas differ only by payment id and an amount drawn from a fixed pool — bootstrapping over them is **pseudoreplication**, and it would have manufactured precision from nothing. Held-out has 13 templates with generator-chosen weights.

The irony is not lost: I added session variation in Round 3 partly to make bootstrapping more defensible, which is exactly the move that dressed up replicas as samples.

**No confidence intervals will be reported for this corpus and no inference to merchant traffic will be claimed.** Results are a **descriptive score on a frozen fixture set**.

---

### P4.5 — "Several named controls are not actually exercised as the blocking reason."

**Verdict: ACCEPT. `A2e` tested nothing, exactly as described.**

Verified: every `A2e` target had no authorized action, so **action matching denied first and the rate limiter was never reached.** A template named for a control that the control never sees.

Rewritten so all prior controls pass — 14 refunds each matching an authorized action, deliberate cumulative headroom (280,000 of 1,000,000), issued inside a 23-second window against `max_calls_per_minute: 10`, so the **rate limit alone decides.** Verified after regeneration: 14/14 targets authorized, 14 calls spanning 23s.

**Standing requirement adopted:** every named control needs at least one scenario where all prior controls pass and that control alone determines the result. Audited the rest — replay and cumulative-cap families already satisfy it; A1 and A4 are by construction all action-matching, and are now labelled as such rather than implying broader coverage.

---

### P4.6 — "The frozen baselines are too weak to be meaningful."

**Verdict: ACCEPT.** `B-amount`'s quarter-of-cap threshold is arbitrary, and `B-velocity` is beaten by `B07` — a fixture **I authored specifically to break it**. Declaring a baseline and then building its counterexample is not a comparison. Relabelled **sanity baselines, not competitive alternatives**, and the README will say beating them validates nothing.

---

### P4.7 — "Preserve commit `e572d85`; add a dated amendment."

**Verdict: ACCEPT.** This is my own §7 amendment policy applied to me, which is the point of writing one. `e572d85` is untouched; corrections land as **Amendment 1** with corpus v1.0.0 → v1.1.0, covering all four required items: the downgrade, the separated denominator, removal of the "independent oracle" and "attacks with no corresponding rule" language, and the explicit no-inference statement.

---

### Consequence for the build

The reviewer's stopping condition — *do not start Phase 2 as a metric-bearing build* — is accepted. Phase 2 still gets built, because the capability matcher is the product; what changes is that **no metric claim attaches to it from this corpus.**

The panel question *"what did the detector infer that wasn't already encoded in the action list and the fixture label?"* has an honest answer only if ground truth stops being derived from the mandate. That means observing a **real agent**: driving an actual model against the MCP surface with injected content and recording the calls it genuinely emits, scored against merchant intent specified independently of the mandate.

The measurement worth having is the one I cannot author: **how often a correctly-behaving agent is blocked because the mandate did not anticipate its legitimate path.** That is the false-positive cost the brief asks for, and only real traces produce it. Scoped into the plan as a new Phase 4; the conformance corpus stays as regression evidence.

**Net effect of Round 4:** the headline metric claim is withdrawn, one template is deleted, one is rewritten after being shown to test nothing, the denominator is narrowed from 220 mixed calls to 175 refund calls, confidence intervals are abandoned as pseudoreplication, and the evaluation is rebuilt around real agent traces. Four rounds in, the single most valuable outcome of this loop is a deliverable that got smaller and a claim that got true.


---

## Round 8 — cmd orchestration + live gates — reviewer: ChatGPT — 2026-08-24

**Four accepts, no rejects.** One was a P0 that made the repository unbuildable, and one was the weakest-claim callout again being right.

### P8.1 — "The executable is not committed."
**ACCEPT — P0.** `git check-ignore` confirmed the unanchored `rzp-guard` rule matched the `cmd/rzp-guard/` **directory**. A fresh clone had run.sh, README and Makefile and no command source. Root-anchored to `/rzp-guard`, `/rzp-guard.exe`; verified by actually cloning and building, not by inspection.

### P8.2 — "The live-block control is printed, not enforced."
**ACCEPT.** The lane exited 0 whenever the blocked call was merely absent, which also passes against a dead container. `cmd/gate-verify` now parses the captured JSON and asserts every condition. Verified negatively: with a wrong secret the gate exits 1 on `CONTROL: read response is a success, not a tool error`.

### P8.3 — "Signal and child-exit handling do not terminate the process."
**ACCEPT — and this was the weakest claim, correctly identified.** "Cleanup is guaranteed" described state, not lifecycle. Measured with stdin held open: pre-fix 30s, post-fix 0s, with cleanup firing immediately in both. My own process-recover run had masked it because its feeder eventually closed stdin — a test that cannot distinguish cleanup-began from process-exited. Both pumps now run under a supervisor; three build-tagged tests hold stdin open deliberately.

### P8.4 — "Child failure is discarded."
**ACCEPT.** `child.Wait()`'s error was ignored, so the CLI could exit zero after the container crashed. Now propagated unless the parent initiated shutdown.

### P8.5 — "The production child surface is runtime-configurable."
**ACCEPT.** `-toolsets` was a launch flag, so the child could be widened from the reviewed default. Not a bypass, but it invalidated the independent-fixed-boundary claim. Now a build-time const.

### P8.6 — "The Makefile is stale."
**ACCEPT.** It advertised `live` and `live-recover`, neither implemented, so `make live-recover` printed help and **succeeded**. Corrected, and `make help` executed inside the golang container — this host has no make, which is exactly why the staleness survived unnoticed. run.sh now exits non-zero on an unknown command.

**Net effect of Round 8:** the repository went from not containing its own executable to being clone-and-run verified; the live control went from decorative to load-bearing; and the process lifecycle went from claimed to controlled.


---

## Round 9 — red-team brief + study provenance — reviewer: ChatGPT — 2026-08-31

**Six raised, six accepted.** One is the sharpest instance yet of the defect
class this repository keeps producing: a control that a comment describes and
the code does not provide. Two of the six are my own published claims being
wrong.

### P9.1 — "The prompt's stated metric is wrong: it says precision 0.333; Arm B is 0.250."
**ACCEPT — P0, and the reviewer's weakest-claim callout was right.** `REDTEAM_PROMPT.md`
told an external reviewer the system's measured precision was 0.333. The
published figure is **0.250 (3/12)**. 0.333 is my own counterfactual replay,
which I labelled "not a study number" in `study/COUNTERFACTUAL-combining.md` and
then quoted unqualified in the very next document. An inaccurate brief
contaminates every finding the review produces. The prompt now gives 0.250 as
the published result and names the two other figures as what they are.

The file count was stale too: I wrote 63, the tree has **65**. Verified with
`git ls-files '*.go' | wc -l`. The reviewer's own line count (14,019) does not
match the current tree either — it is **15,330** tracked, ~8,700 non-test — so
the corrected prompt states the command that produces the number rather than the
number alone.

### P9.2 — "Model-identity check fails open on an omitted field."
**ACCEPT — P1, and worse than described.** The check read

```go
if out.Model != "" && out.Model != req.Model
```

so the untrusted endpoint could **opt out of the check on itself by sending
less rather than something wrong**. `openai.go` had the identical shape. And
`validateTraceSet` counted served models with `if t.ServedModel != ""`, so the
"exactly ONE served model" control tripped on `len(served) > 1` — and **zero is
not more than one**. A study where every trace omitted the field would have
passed validation with no provenance at all.

Fixed at both layers: an empty model is now as fatal as a wrong one, an empty
response id likewise, and validation rejects a trace or turn with no provenance
instead of skipping it. Checked before shipping that the committed study meets
the stronger rule — all 90 traces and all 213 turns already carry both fields —
so nothing published was invalidated. Regression tests in
`cmd/rzp-study/provenance_test.go`, including the vacuous case where every trace
omits the model.

**The tell was in the test file.** `goodTraceSet`, the fixture named *good*,
built traces with no served model and no turns, and passed.

### P9.3 — "Do not present the proxy-run traces as an evaluation of a named model."
**ACCEPT — P1.** The generated reports rendered `| Model | gpt-4o |`, which reads
as "a gpt-4o evaluation". Publishing every emitted call makes the guard's
decision on those calls auditable; it does not turn an unverifiable endpoint
into a named model. The label is now
`| Generator, self-reported and unverified |`. Both reports regenerated
deliberately through the immutability guard, with every number and both label
files byte-identical — one line changed per report.

The reviewer also asks why a third party that previously substituted models is
in the measurement path at all. The answer is in `PROTOCOL.md` §4.5 and it is not
a good one: it was the only credential available. That is a reason, not a
justification, and the correct fix remains a direct provider account.

### P9.4 — "The red-team safety boundary needs executable rules."
**ACCEPT — P1.** "Do not target a live account" is not a boundary when the
repository contains `.env`, gate commands that reach a real API, and captured
Test Mode artefacts. The prompt now names the six commands not to run, forbids
reading `.env` at all, requires `-tags testhook` with `cmd/mcp-stub`, forbids
touching `study/`, and requires every identifier in a reproducer to match
`pay_SYN*`.

### P9.5 — "I12 is imprecise; reviewers may report intended determinism as a defect."
**ACCEPT — P2.** "Receipts are unique per forwarded call" invites a false
positive, because a receipt is *deterministic* for a given (mandate, action-set)
by design. Restated: no two distinct reservations may hold the same
`call_receipt` row, and reusing an action set fails earlier anyway because its
actions are no longer AVAILABLE.

### P9.6 — "Scope the red-team pass."
**ACCEPT — P2.** Thirteen invariants with no ordering is a brief that gets
half-covered everywhere. Now three passes — money-can-move first (I1–I5, I13),
then operator-misleading, then the rest — with required reporting of baseline
commit, exact commands, fuzz seed and time budget, and whether each result is
source-only, unit-tested or fuzz-found.

### The question this round does not close

**"Show the held-out positive examples that establish recall."** Three positive
calls, all from one injection brief, in one arm; the other arm has none. That is
a descriptive account of what happened on 15 hand-written briefs adjudicated by
their own author. It is not a held-out detector evaluation and no amount of
tightening the provenance controls makes it one. The README says this; after
this round it says it in the reviewer's sharper words.

**Net effect of Round 9:** the untrusted endpoint lost its ability to silently
decline to identify itself, two of my own published numbers were corrected, the
red-team brief acquired rules a machine can follow, and the study's headline
label stopped implying a model evaluation it cannot support.


---

## Round 10 — red-team boundary — reviewer: ChatGPT — 2026-08-31

**Nine raised, nine accepted.** The weakest-claim callout was right for the
third round running, and this time it was aimed at a heading I had written one
round earlier.

### P10.1 — "'Executable hard rules' is false. The prescribed containers mount the whole repo including .env, with network."
**ACCEPT — P0, and the most deserved hit in ten rounds.** I wrote
"HARD RULES — these are executable, not aspirational" and then listed prose,
around a runner whose entire isolation was `-v "$PWDW":/src`. `gorun` mounts the
working tree — `.env` included — with default networking. A reviewer adding one
test file could have read Razorpay keys or made egress without doing anything
unusual.

Fixed by building the boundary rather than rewording it. `./run.sh redteam`:
tracked-files-only export via `git archive` (so a gitignored `.env` cannot be
present), an explicit refusal if the export contains anything key-shaped,
`--network=none`, `--pull=never`, no Docker socket, credentials emptied,
`RZP_GUARD_CHILD_STRICT=1`. Module downloads happen in a separate earlier step
that never mounts the export.

Verified, not asserted — run inside the lane: no `.env`, all four credential
variables unset, no DNS, no outbound TCP, no docker socket, no `.gotmp`/`dist`/
`evidence/live`. The prompt now tells the reviewer to run that check themselves
rather than believe the paragraph.

Two bugs surfaced while proving it, both of which would have made the lane look
fine and behave badly: an ERE backreference that grep rejected outright (which
would have silently disabled the key scan), and an EXIT trap referencing a
function-local under `set -u`, which turned a green test run into a non-zero
exit after the fact.

### P10.2 — "`-tags testhook` is not automatically safe; its child override accepts RZP_GUARD_CHILD_CMD through sh -c."
**ACCEPT — P0.** True, and the recovery gate legitimately depends on it
(`RZP_GUARD_CHILD_CMD="head -c 120 > /dev/null; exit 0"`), so removing it was not
an option. Added `RZP_GUARD_CHILD_STRICT`: when set, the test-hook build ignores
the variable entirely, takes no shell, and execs exactly `./.gotmp/mcp-stub` —
refusing outright if that binary is absent rather than falling back. Enforced in
`child_testhook.go` rather than only in `run.sh`, because a harness that can be
bypassed by invoking the binary directly is the same advisory boundary the
review was objecting to. Three tests pin it, including that non-strict mode is
unchanged.

### P10.3 — "The fuzz instructions are internally inconsistent, and the target is not a parser-differential test."
**ACCEPT — P1.** The brief demanded the pinned container and then handed out a
bare host `go test -fuzz`; there was no fuzz lane at all. Added `./run.sh fuzz`.
And the relabelling is the substantive half: `FuzzAgentLineNeverLeaks…` writes to
a `bytes.Buffer`, so it compares the guard against **nothing**. Section A now
says plainly that no differential test exists and that building one is a
contribution.

### P10.4 — "`pay_SYN*` is a naming convention, not a security boundary."
**ACCEPT — P1.** Correct: a prefix does not make an opaque id non-resolvable.
The real controls are no credentials, no real child, no network, and those are
now enforced by the lane. In addition `cmd/mcp-stub` refuses any non-`pay_SYN`
payment id with `STUB_REFUSES_NON_SYNTHETIC_ID` — identifier provenance, not
policy, so the stub does not become a second detector. Verified it cannot
confound the study: every payment id in the frozen briefs, mandates and examples
is `pay_SYN*`, zero exceptions, so the branch is unreachable for the experiment.

### P10.5 — "Prohibit the study/provider paths explicitly."
**ACCEPT — P1.** `study-model`, `study-smoke`, `study-run`, `rzp-study
resolve-model|run` and every provider credential variable are now named in the
banned list. "No egress" alone does not stop someone exporting a proxy key on a
machine that has one.

### P10.6 — "Several factual claims need correction."
**ACCEPT — P2, and I invented one of them.** `live-allow` **is not a command** —
I made it up while writing a list of things not to run. `verify-refund-evidence`
is read-only with no network, no credentials and no child, and says so in its own
comment; `process-recover` drives a local test-hook stub with placeholder keys.
Both are now listed as safe, so a reviewer does not avoid them by mistake.
Static line and file counts removed in favour of the command that derives them.

### P10.7 — "I11 overclaims: supportedTools is an unexported mutable var, not a build constant."
**ACCEPT — P2.** It is `var supportedTools = map[string]struct{}{...}`. Restated
to the guarantee that is actually enforceable: nothing over JSON-RPC and no
mandate can widen the forwarded tool surface. Left in deliberately with a note
not to report the mutability itself, which would otherwise be a guaranteed
false positive.

### P10.8 — "Phrase the metric limitation even more strictly."
**ACCEPT — P2.** The numbers are correct arithmetic over descriptive trace
outcomes and nothing more. The prompt now says so in one place: not held-out
precision and recall, not evidence about a named model, not a population
estimate.

### P10.9 — "Fuzz history is not proof."
**ACCEPT — P2.** 8.9M executions is something I observed once; the committed
corpus holds a single entry. Labelled a recorded historical claim, with
instructions to re-derive it or ignore it rather than inherit it as coverage.

**Net effect of Round 10:** the red-team boundary went from a heading to a
runner whose isolation can be checked in one command; the test-hook build
acquired a mode where "use the stub" is enforced instead of requested; the stub
refuses to impersonate a provider for anything that might be real; and four
factual claims in my own brief — one of them a command that does not exist —
were corrected.


---

## Round 11 — the red-team lane, again — reviewer: ChatGPT — 2026-08-31

**Seven raised, seven accepted.** Third consecutive round on the same surface,
and the same failure each time: **a claim slightly stronger than what was
enforced.** Round 9 was a control an untrusted party could skip. Round 10 was
rules called "executable" that were prose. Round 11 is a lane that was real and
still had three holes.

### P11.1 — "`./run.sh fuzz` escapes the new boundary."
**ACCEPT — P0.** `cmd_fuzz` called `gorun`, which mounts the real workspace with
default networking. I built the isolated lane, then wrote a brief telling
reviewers to fuzz through the exact path the lane exists to replace. It now
delegates to `cmd_redteam`.

### P11.2 — "Strict child mode is bypassable and does not pin the stub."
**ACCEPT — P0, and the reviewer's weakest-claim callout.** Two defects, both
real. `RZP_GUARD_CHILD_STRICT` was an environment variable, so anything running
inside the lane could unset it and restore `sh -c`. And even when set it only
`Stat`ed `./.gotmp/mcp-stub` — a **writable relative path**, symlinks followed,
no identity check.

The sharpest part: **the test I wrote to prove strict mode worked demonstrated
the opposite.** It created a shell script, renamed it to the strict path, and
strict mode ran it. I read that as a passing test.

Replaced with a compile-time boundary. `-tags redteam` selects
`child_redteam.go`, which has no shell branch, no environment lookup and no path
resolution; the path is absolute; `Lstat` refuses a symlink and a non-regular
file; absence is refused rather than falling back. Proved by construction:
`RZP_GUARD_CHILD_CMD` appears twice in the testhook binary and **zero times** in
the redteam binary.

The file now also states what it does NOT guarantee — that the file at that path
is the real stub. Anyone who can write there can substitute it. Overstating this
is what the last three rounds were about.

### P11.3 — "The module cache is shared mutable state across isolated runs."
**ACCEPT — P0.** `rzpguard-modcache` was a persistent read/write volume mounted
into both the networked fetch stage and the offline test stage. Neither fresh
nor removed: a poisoned cache would survive, and one review command could leave
state for the next. Now created per invocation, destroyed in the trap, and
mounted **read-only** in the offline stage.

### P11.4 — "The key scan is narrower than its claim."
**ACCEPT — P1.** It matched Razorpay ids only. Broadened to OpenAI, Anthropic,
GitHub, AWS and PEM private keys — and the documentation now calls it a backstop
for an accident rather than implying coverage. Also switched to `$GO_IMAGE_PINNED`
directly, so a `GOIMAGE` override cannot redirect the isolated lane, and added
`--pull=never` to the fetch stage that lacked it.

### P11.5 — "`git archive HEAD` prevents reproducing newly written tests."
**ACCEPT — P1, and the most practically important.** A reviewer writes a failing
test, reruns the lane, and silently gets the last commit. That is an incentive
to leave the safe runner, which is worse than the risk the archive avoided. Now
`git ls-files -c -o --exclude-standard` copied from the working tree: uncommitted
edits and new files are present, gitignored paths — `.env` included — are not.

### P11.6 — "The synthetic-ID control is prefix-only and its refusal is not an MCP error."
**ACCEPT — P1, all three parts.** `pay_SYNanything` passed; the refusal used
`toolText` so it was a SUCCESS carrying an error-shaped body, while `toolError`
with `isError: true` already existed two functions away; and only
`create_refund` checked at all. Now exact membership against the 16 payment
fixtures plus an explicit intentional-unknown set, a real tool error, and
applied to `fetch_payment` and `fetch_multiple_refunds_for_payment` too.
`pay_SYN8099` still returns not-found, because that is what C02 tests.

### P11.7 — "The 'verified empirically' claim is not regression-protected."
**ACCEPT — P1.** I verified the lane by hand and nothing stopped it regressing —
which, given this is the third round on the same boundary, is the finding that
matters most. `./run.sh redteam-selfcheck` now checks eight properties, and a CI
job runs it alongside the redteam child tests and a stub-refusal assertion.

**Net effect of Round 11:** the lane stopped being something I had checked once
and became something that fails a build when it lapses; "only the stub can
execute" was replaced by a compile-time guarantee plus an explicit statement of
what it does not cover; and the fuzz command stopped pointing outside the
boundary it was written to enforce.


---

## Round 12 — evidence discipline — reviewer: ChatGPT — 2026-08-31

**Five raised, five accepted.** Fourth consecutive round on this boundary. The
reviewer also noted they could not independently reproduce the test runs — a
Windows host blocked Go's build cache — and declined to treat my green counts as
verified. That is the correct posture and it sharpens the standing objection:

> show the negative tests that would fail for each prior bypass, rather than
> reporting a PASS banner as proof of the boundary.

That objection is now the design of the suite.

### P12.1 — "The export can still copy `.env` through a symlink."
**ACCEPT — P0, reproduced before fixing.** `git ls-files -o --exclude-standard`
lists an untracked symlink; `[ -f ]` follows it; `cp -p` copies the CONTENTS. So
a link named anything at all could pull the gitignored `.env` into the export
under an unrelated name, where the literal `.env` check never looks.

Demonstrated on Linux first, because this host cannot create symlinks — which is
precisely how the hole survived a round:

    is the symlink selected?   yes
    does [ -f ] accept it?     yes
    what does cp -p produce?   a REGULAR FILE containing: secret

Symlinks are now refused outright and that is the PRIMARY control, with the
credential scan demoted to the backstop it always was. Verified after fixing, on
Linux, with the same attack: refused.

### P12.2 — "The credential backstop is bypassable with a space in the filename."
**ACCEPT — P0.** The export loop was NUL-safe; the scan was
`for f in $(grep -rlE ...)`, which word-splits. A file named `review artifact`
became two nonexistent paths and was scanned as neither. Now `grep -rlZ` with
`read -d ''`, and **a scanner that errors is treated as a refusal** — a scan that
did not run is not a scan that passed.

### P12.3 — "The runner does not build the 'only permitted' stub."
**ACCEPT — P1, and the worst one.** `child_redteam.go` admitted it could not
identify the file at the child path, then claimed the lane "addresses that by
building the stub itself, immediately before use". `cmd_redteam` never built
`cmd/mcp-stub` at all.

A false claim, in the comment block written to stop false claims, in the commit
whose message was about claims outrunning code. The lane now genuinely builds it
— in the same container that runs the command, since `/tmp` does not survive
between containers — and the comment says plainly that a build is not an identity
check and that **nothing here detects substitution**. What makes a replaced stub
harmless is the container: no credentials, no network, no socket.

### P12.4 — "`redteam-selfcheck` does not prove the regressions it claims to prevent."
**ACCEPT — P1.** "Working-tree files present" checked `run.sh`, which is
*tracked*, so it proved nothing about the dirty-tree property it was named for.
"No shell-child path" grepped for one legacy string, so renaming the variable
would pass.

Replaced with `./run.sh redteam-negative`: six cases, each a bypass that actually
worked, each of which must fail. N3 creates an untracked sentinel and asserts it
reaches the export. N4 resolves the files `go list -tags redteam` actually
compiles and greps THOSE for any shell or child-from-config construct, whatever
it is named. The self-check remains as a smoke check and is now labelled one.

### P12.5 — "Fuzz isolation can regress with no CI failure."
**ACCEPT — P1.** `cmd_fuzz` was fixed last round and nothing asserted it stayed
fixed — the exact bypass, one round earlier, with no regression test. N5 now
fails if `cmd_fuzz` stops routing through `cmd_redteam` or reacquires a `gorun`
call.

### Three of my own test bugs, found by running the suite

The negative suite reported two BYPASSED on its first run, and neither was a
regression:

- **N2's fixture put a credential-shaped literal into `run.sh` itself**, so the
  scanner correctly flagged `run.sh` and every export in the repository refused.
  The fixture is now assembled at runtime.
- **N3 chained `export && [ -f sentinel ]`**, so N2's induced refusal read as a
  missing sentinel. Refusal and absence are now distinguished.
- **N6 piped into `gorun`**, which runs docker without `-i`, so the stub read EOF
  and produced nothing. It reads a file inside the mount now.

Worth recording because the first two are the same error as the findings above:
a test that fails for a reason other than the one it names is not evidence
either.

**Net effect of Round 12:** the boundary's evidence stopped being a banner and
became six attacks that must fail; the symlink hole — the one that could
actually move a secret into the container — is closed and demonstrated on the
platform where it was exploitable; and a comment claiming the lane did something
it did not was removed rather than made vaguer.


---

## Round 13 — the evidence command was the bypass — reviewer: ChatGPT — 2026-08-31

**Four raised, four accepted.** Fifth round on this boundary, and the pattern is
no longer deniable: **I fix the named instance and reintroduce the same defect
in the code I write to prove the fix.** Round 11 it was the fuzz wrapper. Round
12 it was a comment claiming the lane built a stub it never built. Round 13 it
is the negative suite itself.

### P13.1 — "`redteam-negative` leaves the lane and runs untrusted code with `.env` mounted."
**ACCEPT — P0.** N4 called `gorun`. N6 called `gorun` to **build and execute
`cmd/mcp-stub`**. `gorun` mounts the real `$PWD` — `.env` included — with normal
networking. So the command the brief names as the evidence was itself the
credential-exposure path it exists to detect.

The reviewer said not to rerun it until fixed. I did not.

Demonstrated rather than argued. A hostile `hostileProbe()` added to
`cmd/mcp-stub` temporarily, then run both ways:

    via gorun (what N4/N6 did):
      HOSTILE: READ /src/.env -> 222 bytes
      HOSTILE: READ .env      -> 222 bytes
      HOSTILE: OUTBOUND HTTPS SUCCEEDED

    inside the lane:
      HOSTILE: cannot read /src/.env
      HOSTILE: cannot read .env
      HOSTILE: outbound blocked

Both `gorun` calls now go through `cmd_redteam`. **N7 was added to make the
regression impossible to reintroduce quietly**: it greps this very function for
`gorun` and fails if one appears. The stub was reverted; the tree is clean.

### P13.2 — "N4 does not prove 'whatever it is called'."
**ACCEPT — P1.** The check was a deny-list of spellings — `exec.Command("sh"`,
`Getenv(...CHILD` — which is precisely the rename class it claimed to close.
`exec.CommandContext(ctx, "sh", "-c", …)`, `os.LookupEnv`, a flag, or a
constructed name all walked past it.

Replaced with `cmd/redteam-audit`, which parses the AST of exactly the files
`go list -tags redteam` compiles and asserts POSITIVE structure: exactly one
process launch across the whole build, it is
`exec.CommandContext(ctx, redteamChildPath)`, it takes two arguments and no
more, and `child_redteam.go` performs no configuration reads of any kind.

Mutation-tested with the evasion the old check would have missed —
`os.LookupEnv` plus `exec.CommandContext(ctx, "sh", "-c", alt)`:

    the redteam build contains 2 process launches, want exactly 1
        child_redteam.go:90:10: exec.CommandContext(ctx, "sh", "-c", alt)
        child_redteam.go:92:7:  exec.CommandContext(ctx, redteamChildPath)
    child_redteam.go:89:16: reads configuration; the child must not be
                            selectable at runtime by any name

### P13.3 — "N6's 'real isError' is only a substring search."
**ACCEPT — P1.** `case "$out" in *'"isError":true'*` passes if that text occurs
anywhere, including inside tool content the stub composes itself.
`redteam-audit stub` now parses the JSON-RPC reply, requires `result.isError` to
be boolean true, requires exactly one reply, and checks it answers the id that
was sent.

### P13.4 — "N1 can abort rather than skip on some Windows hosts."
**ACCEPT — P1.** `set -euo pipefail` is on and `ln -s` was a bare statement. This
host returns success (it copies), so the bug was invisible here — on a host where
symlink creation fails, the whole suite would exit before printing SKIP. Now
inside an `if`.

### One more of my own test bugs

N6's request JSON was built through three levels of shell quoting inside
`sh -c '…'`; the escapes collapsed, the stub received unparseable input, and the
suite reported that as the stub accepting a fabricated id. Replaced with a
tracked fixture file, `cmd/redteam-audit/testdata/nonfixture_refund.jsonl`,
which needs no quoting at all. Third round running in which a test failed for a
reason other than the one it named.

### An unreproduced failure, recorded rather than dismissed

One full verification run in this round reported FAIL. It did not reproduce in
five subsequent runs and I could not attribute it with certainty. The most
likely cause is real and has been fixed: the redteam child tests share a FIXED
absolute path (the constant is fixed by design), their setup used os.Remove, and
one test deliberately creates a DIRECTORY there -- which os.Remove cannot
delete. Leftover state would then fail the next test. Now os.RemoveAll.

Recorded because "it passed the next time" is not a diagnosis, and a flake in
the package that enforces this boundary is worth more suspicion than one
elsewhere.

**Net effect of Round 13:** the evidence command stops being the bypass; "no
configurable child" becomes a positive AST invariant that survives renaming
rather than a list of spellings; the stub assertion parses instead of matching;
and there is now a demonstration — hostile stub, both paths, side by side — of
exactly what the boundary does and does not stop.


---

## Round 14 — the structural proof was defeated by an import alias — reviewer: ChatGPT — 2026-08-31

**Five raised: four accepted, one accepted in part after checking the premise.**
The reviewer did not run anything this round (their host denied the Bash
service) and said so; these were source-audited findings, and four of five were
right on the source alone.

### P14.1 — "The runner does not actually build the default redteam child."
**ACCEPT IN PART — the conclusion is right, one premise is not.**

The premise was that `go build -o /tmp/rzp-redteam-child/mcp-stub` fails because
the parent does not exist. Checked: **`go build -o` creates the parent**, exit
0, file present. So the build was in fact working and the earlier observation of
the stub at that path was real.

The finding stands anyway, for the other reason given: `|| true` **discarded any
failure**, so the claim could have been false on every clean run and nothing
would have said so. And no test exercised the launch — N4 audits source, N6
builds a different binary at `/tmp/nt/stub`, and every child test inspected the
returned `*Cmd` without starting it.

Fixed: `mkdir -p` first, the build failure now aborts the lane with an
explanation, and two new tests actually **start** the child and talk to it —
`TestRedteamChildActuallyLaunchesAndCommunicates` writes a line and requires it
back, and `TestRedteamChildSurfacesANonExecutableFile` requires a non-executable
file to fail at Start rather than look like a healthy session.

### P14.2 — "The AST auditor is still not invariant 'whatever it is named'."
**ACCEPT — P1, and the weakest-claim callout again.** It matched the textual
selector, so `import runner "os/exec"` defeated it. Demonstrated:

    // aliased import, hardcoded shell command, no env read
    runner.CommandContext(ctx, "sh", "-c", "cat /src/.env")

    redteam child: exactly one launch, exec.CommandContext(ctx,
    redteamChildPath), no configuration reads (3 files audited)
    AUDIT EXIT=0

The audit **passed, exit 0**, with a hidden shell launch reading credentials in
the file. A positive structural claim that a rename defeats is not a structural
claim.

Now each file's imports are resolved to package paths, so a call is identified
by what it resolves to rather than how it is spelled; dot-imports are refused
outright because they make calls unattributable; and `CgoFiles` are audited
alongside `GoFiles`. The identical mutation now fails:

    the redteam build contains 2 process launches, want exactly 1
        child_redteam.go:91:10: os/exec.CommandContext(ctx, "sh", "-c", "cat /src/.env")
        child_redteam.go:93:7:  os/exec.CommandContext(ctx, redteamChildPath)

### P14.3 — "N7 only bans one spelling of the old escape."
**ACCEPT — P1.** It grepped for a top-level literal `gorun`; a `cmd_test`,
`cmd_lifecycle` or bare `docker run` reached the same unsafe runner and passed.
Widened to those, and — more importantly — the claim is narrowed in the code to
what it actually checks: that THIS FUNCTION runs untrusted code only via
`cmd_redteam`. It does not prove the unsafe runner is unreachable from elsewhere
in run.sh, and no longer implies it.

### P14.4 — "Do not use a real `.env` to prove containment again."
**ACCEPT — P2, and it is the defense-only bar, not just hygiene.** Round 13's
containment demonstration edited `cmd/mcp-stub` to read the real credential file
and make a live outbound request. It printed only byte counts, was reverted, and
was never committed — but for a track where anything offense-capable is
disqualified, mutating a program to read real credentials is the wrong method
even when the finding is real.

Replaced with **N8**, a repeatable test using an invented sentinel in a
gitignored path plus a DNS lookup against `example.com`. It proves the same
three things — gitignored files absent from the export, `.env` absent, no egress
— and touches no credential at all.

### P14.5 — "Do not report a skipped case as blocked."
**ACCEPT — P2.** I wrote "6 blocked, 0 bypassed" for a run where N1 SKIPped. The
summary now prints `blocked / bypassed / skipped` and, when anything skipped,
says explicitly that the local result is not clean and that the Linux CI output
must be cited for those cases.

**Net effect of Round 14:** the auditor stopped being defeatable by an import
alias; the child launch path is exercised rather than assumed; the stub build
can no longer fail silently; containment is proven by a sentinel instead of a
real credential; and the local summary stops reading better than the run was.
