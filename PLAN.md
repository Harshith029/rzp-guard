# PLAN.md — Track 2 (AI Risk Manager)

**Project:** `rzp-guard` — an authorization proxy for the Razorpay MCP server
**Phase 2 in progress.** Phase 0.5 conformance corpus committed; Phase 2 mandate/matching/provenance built and green.
Date: 2026-08-24 · Deadline: 2026-09-05 (12 days)

**Version 6**, after five rounds of adversarial review — see [REVIEW_LOG.md](REVIEW_LOG.md). **The plan has shrunk every round.** v5's change is the largest: the headline metric claim is withdrawn from the fixture corpus entirely and rebuilt on real agent traces (Phase 4b).

> **Two things in this plan were not built, and the plan is left standing rather
> than quietly edited to match what shipped.**
>
> **The dashboard is cut.** Every mention below of a dashboard, its templates, or
> its CSP describes a component that does not exist. Resolving an `IN_DOUBT`
> action is CLI-only. It was cut for scope: a web surface on a money path needs
> its own authentication, session handling and XSS review to be worth shipping,
> and none of that would have strengthened the authorization claim this project
> is actually making.
>
> **The false-positive cost curve is not priced in rupees.** §Phase 4 planned one.
> What exists instead is the measured false-block rate (8 of 49) and a
> parameterised cost in [study/FINDINGS.md](study/FINDINGS.md), because putting a
> ₹ figure on a support ticket would have meant inventing the input that
> dominates the answer.

Claims withdrawn across all rounds:

| Withdrawn | Why | Now |
|---|---|---|
| "`receipt` is not enforced as an idempotency key" | Factually wrong | §2.6 |
| "This is real taint tracking" | Was literal-reuse detection; would have blocked the primary legitimate workflow | §3.3 |
| G1.4 fallback to a payment-link demo | Not evidence of stopping money movement | §4 Phase 1 |
| "Release the budget reservation on timeout" | **Fails open** — the provider may have processed the refund | §3.4 |
| "Receipt injection gives idempotency" | Gives duplicate *rejection*, not safe retry; header unreachable from stdio | §3.5 |
| Network capture stub as promised evidence | Never verified as buildable | §4 G1.5 |
| **"Razorpay's docs contradict each other"** | **My error.** I quoted a WebFetch summary as if it were source text — see below | §2.6 |
| **Provenance as an enforcement gate** | Provably redundant given the capability list | §3.3 |
| **Automatic reconciliation** | Would have falsified the transparent-relay claim | §3.4 |
| **Fixture corpus as a detector evaluation** | Labels are computed from the matcher's own predicate — conformance, not detection | Phase 0.5 / 4b |
| **"Independent oracle"** | Independent of the implementation, not of the design | Phase 0.5 G0.3 |
| **Confidence intervals on the corpus** | 5 sessions per template are replicas; bootstrapping them is pseudoreplication | Phase 4a G4.2 |

**Standing correction to my own method:** `WebFetch` answers prompts against a page *using a small fast model*, so its output is a paraphrase, not a quote. It can point at behaviour worth verifying; it cannot support a claim about what a document says. I violated this while writing the document that states it. API-semantics claims now come from runtime observation only.

---

## 1. The claim (one sentence, one action)

> **`rzp-guard` enforces a merchant-issued capability list over `create_refund` calls made by an AI agent holding valid Razorpay MCP credentials, blocking any refund outside that list.**

**Measurement claim, stated separately because round 4 showed they are not the same thing:** conformance to that capability list is demonstrated on a frozen fixture corpus (descriptive, no inference). Detector performance against *real agent behaviour* is measured in Phase 4b on observed traces, with intent specified independently of the mandate. The fixture corpus cannot support the second claim and is no longer offered as if it could.

Stated as a capability list rather than "prevents unauthorized refunds" deliberately. Where the merchant issues a **bounded** grant (§3.2), amounts inside that bound are authorized *by the merchant's own choice* — the mandate records which. Overclaiming that as "every unauthorized refund" would not survive the first panel question.

**Why refunds:** money leaves the merchant irreversibly, it is the classic merchant-loss vector, it is exercisable end-to-end in test mode, and the authorized set is finite and enumerable.

`create_instant_settlement` and `initiate_payment` **are** money movement, deferred only for measurement focus. `create_payment_link`, `revoke_token` and PII reads are **not** — a different loss class.

### 1.1 Attack families, scoped to refunds

| | Family | Mechanism |
|---|---|---|
| **A1** | Injected instruction | Attacker-controlled text in `notes`/`description`/`customer_name`, read back via `fetch_payment`, induces a refund |
| **A2** | Mandate / scope drift | Amount exceeds the authorized action; refund after expiry; tool outside the allowlist |
| **A3** | Replay / duplicate | Consumed action executed again; retry storm |
| **A4** | Misdirection | Refund to a `payment_id` with no authorized action |

### 1.2 What the proxy cannot see

The proxy sees **only JSON-RPC traffic** — never the user's prompt or the agent's reasoning. Any claim requiring visibility into agent reasoning is out of scope by construction.

---

## 2. Ground truth I verified before planning

Shallow-cloned `github.com/razorpay/razorpay-mcp-server` at commit `7950d51d118ca164c32b7cf0cfaa14f34f24849f` (2026-03-26) and read the Go source.

### 2.1 Three corrections to the brief's hypothesis

| # | Assumed | Verified | Evidence |
|---|---|---|---|
| 1 | Server exposes `create_payout` | **Does not exist.** `payouts` is `AddReadTools(FetchPayout, FetchAllPayouts)` only; `grep -rn "create_payout\|CreatePayout"` → zero hits. | `pkg/razorpay/tools.go:72-76` |
| 2 | Hosted server is equivalent | Hosted **omits** `create_refund`, `create_instant_settlement`, `close_qr_code`, `create_registration_link`. | README remote column |
| 3 | Generic "allowed counterparties" | `create_refund` has **no counterparty parameter** — only `payment_id`/`amount`/`speed`/`notes`/`receipt`. | `refunds.go` |

### 2.2 Transport is stdio-only

`cmd/razorpay-mcp-server/main.go:59` registers exactly one subcommand: `rootCmd.AddCommand(stdioCmd)`. **The proxy must be a stdio↔stdio interposer spawning the server as a child.** This also makes the upstream HTTP idempotency header unreachable (§2.6).

### 2.3 Stack facts

Go 1.24.2 · `mark3labs/mcp-go v0.43.2` · `razorpay/razorpay-go v1.4.0` · MIT.
Env: `RAZORPAY_KEY_ID`, `RAZORPAY_KEY_SECRET`, optional `LOG_FILE`, `TOOLSETS`, `READ_ONLY`.
Docker: `razorpay/mcp` — **pinned by digest** (G1.0).

### 2.4 Write-tool surface (17 tools)

`payments`: `capture_payment`, `update_payment`, `initiate_payment`, `resend_otp`, `submit_otp`, `fetch_tokens`, `revoke_token` · `payment_links`: `create_payment_link`, `create_payment_link_upi`, `send_payment_link`, `update_payment_link` · `orders`: `create_order`, `update_order` · `refunds`: `create_refund`, `update_refund` · `qr_codes`: `create_qr_code`, `close_qr_code` · `settlements`: `create_instant_settlement` · `registration_links`: `create_registration_link`

`fetch_tokens` is registered as a **write** tool despite being a read (`tools.go:112`), so `READ_ONLY` does not cleanly partition by side effect.

### 2.5 Verified `create_refund` schema

```
create_refund   payment_id* (string, "pay_" prefix)
                amount*     (number, paise, min 100)
                speed       (string, "normal" | "optimum")
                notes       (object, max 15 pairs)
                receipt     (string, OPTIONAL)
```

### 2.6 Idempotency — what is verified, and what is not

v1 asserted without checking that the server "does not enforce `receipt` as an idempotency key." That was wrong. Two **distinct and compatible** mechanisms exist — a retry mechanism and a uniqueness constraint, which are orthogonal:

| Mechanism | Behaviour | Reachable from a stdio proxy? |
|---|---|---|
| `X-Refund-Idempotency` header | Safe retry — same key + body returns the original refund object | **No** |
| `receipt` field reuse | Duplicate rejected with an error | **Yes** |

The header is unreachable because `razorpay-go`'s signature is `Refund(paymentID string, amount int, data map[string]interface{}, extraHeaders map[string]string)` and the MCP server passes **`nil` for extraHeaders** (`pkg/razorpay/refunds.go:75`); `grep -rni "idempoten"` across the server returns **zero hits**. Reaching it would require forking the child, forfeiting "a proxy in front of the real, unmodified server."

`receipt` is **optional** in the tool schema, so **a refund with no `receipt` has no duplicate protection.** §3.5 closes that.

**What is verified vs. assumed:** the source facts above are verified — I read them. Duplicate-`receipt` behaviour was **not**, and my earlier attempt to establish it from documentation produced a false claim (see the standing correction above). **G1.6 has now observed it directly (2026-08-25, Test Mode):** replaying a used receipt is rejected by Razorpay with `Duplicate receipt found for this refund request.` The receipt is a real provider-side idempotency key. Scope actually tested — same receipt, same payment, same amount; uniqueness scoping beyond that is **not** claimed. Full record: `evidence/g16/RECEIPT_IDEMPOTENCY.md`.

**Consequence for §3.4:** a duplicate `receipt` is expected to *reject* rather than *replay the original result*, so a timed-out refund cannot be resolved by retrying — a retry teaches nothing about whether the first attempt landed.

### 2.8 RUNTIME TRUTH — supersedes every source claim above (gate G1.1 ✅)

Probed the real container over stdio with `initialize` + `tools/list`. Raw capture in `evidence/tools_list_raw.jsonl`, parsed in `evidence/tools_list.json`.

```
image   razorpay/mcp@sha256:435109006d6247103899938cf7b1747ba8be1c1a8a28d452cf9fa8eff506e5c6
built   2025-09-26   arch amd64   size 17.0 MB
server  razorpay-mcp-server 1.0.0   protocolVersion 2024-11-05
tools   41
```

**The pinned image lags `main` by ~6 months, and the tool surface genuinely differs.** This is exactly why the digest is pinned and why runtime supersedes the README:

| In README (main, 2026-03) but **not** in the image | In the image but **not** in the README |
|---|---|
| `create_payment_link_upi`, `send_payment_link`, `fetch_payout_by_id`, `create_registration_link`, `revoke_token`, `detect_stack`, `integrate_razorpay_checkout` | `payment_link_upi_create`, `payment_link_notify`, `fetch_payout_with_id` |

Three are renames (`create_payment_link_upi` → `payment_link_upi_create`, `send_payment_link` → `payment_link_notify`, `fetch_payout_by_id` → `fetch_payout_with_id`); four simply do not exist yet in the pinned build. **Had the guard hard-coded the README's names, its allowlist would have silently referenced tools the child does not expose.**

`create_payout` is **absent at runtime too**, confirming §2.1 correction #1 against the running server rather than the source.

**`create_refund` runtime schema — and a finding that matters for F1.a:**

```json
{"properties":{
  "payment_id":{"type":"string","description":"... ID should have a pay_ prefix."},
  "amount":{"type":"number","minimum":100,"description":"... smallest currency unit"},
  "speed":{"type":"string"}, "notes":{"type":"object"}, "receipt":{"type":"string"}},
 "required":["payment_id","amount"]}
```

`amount` is **`type: number`, not `integer`.** A fractional amount is schema-valid at the MCP layer, so **the child will not reject it** — which makes the prototype's truncate-then-forward defect ([FAILURES.md F1.a](FAILURES.md)) reachable in practice rather than theoretical. The guard must reject fractions itself; nothing downstream will.

### 2.7 Environment blockers (§8)

- **Docker daemon is not running.** CLI `29.7.2`; `docker pull razorpay/mcp` → `failed to connect to the docker API at npipe:////./pipe/dockerDesktopLinuxEngine`.
- **Go is not installed** — source build is not a fallback.
- Available: Python 3.11.9, Node 24.14.0, git 2.53.0.

---

## 3. Architecture

### 3.1 A transparent JSON-RPC relay

```
MCP client ──stdio──► rzp-guard ──stdio (ALLOW only)──► razorpay/mcp@sha256:<pinned> ──HTTPS──► api.razorpay.com
                          │
                          ├─ match refund to an unconsumed authorized action   (the control)
                          ├─ reserve action + budget atomically
                          ├─ inject deterministic receipt
                          ├─ record provenance chain   (forensic only)
                          └─ decision log (JSONL, masked) ──► [dashboard: CUT, see banner]
```

**Decision A — relay at the JSON-RPC line level.** Parse newline-delimited JSON, intercept only `tools/call`, observe results, forward everything else byte-for-byte. Blocking synthesizes a response with the same `id`.

**This claim is now literally true.** v3 had the proxy issuing its own reconciliation reads through the child, which would have required it to become an MCP client — internal id generation, two-way multiplexing, response suppression, collision handling — and would have falsified byte-for-byte relay. That is cut (§3.4). The proxy never originates a JSON-RPC request.

**Decision B — Go 1.24+ is the sole runtime.** Relay, mandate compiler, policy, lifecycle, durable state and operator command are all Go. *(The dashboard named in earlier versions was cut; there is no HTTP server in the product.)* Razorpay's MCP server is Go, so the product matches the ecosystem it plugs into, ships as one static binary, and gets `go test -race` for the concurrency claims the Python prototype asserted without exercising ([FAILURES.md F3](FAILURES.md)).

The Python package is **frozen as a behavioural reference** in `prototype/python/` — its 28 tests pin the decision semantics the port must reproduce, and the six defects found in it are required test cases. There are deliberately **not** two production implementations.

**Decision B2 — one justified dependency: SQLite.** In-memory state is not fail-closed. A crash loses reserved budget, consumed actions and `IN_DOUBT`, so the same mandate replays and the cap is bypassed ([FAILURES.md F2](FAILURES.md)). Durable local state for mandates, action lifecycle, receipts and decision records; `IN_DOUBT` stays locked across restart. Justified by a live gate, not by taste.

**Decision C — no LLM in the decision path.** Ships only if held-out measurement shows a deterministic recall gap it actually closes, with a frozen baseline comparison and full model/prompt disclosure.

### 3.2 The mandate is a capability list — the control

Authorization is per **discrete refund action**. This one structure replaced three separate v2 mechanisms (coarse caps, replay fingerprint, counterparty allowlist).

```yaml
mandate_id: mnd_2026_08_24_001
expires_at: 2026-08-24T17:00:00Z
allowed_tools: [fetch_payment, fetch_all_payments, create_refund]

authorized_refund_actions:
  - {action_id: rfa_001, payment_id: pay_SYN0001, amount_paise: 50000}      # exact (default)
  - {action_id: rfa_002, payment_id: pay_SYN0001, amount_paise: 50000}      # 2nd partial, same amount
  - {action_id: rfa_003, payment_id: pay_SYN0002, max_amount_paise: 120000} # bounded (opt-in)

global:
  max_cumulative_paise: 300000
  max_calls_per_minute: 10
```

An incoming refund must match an **unconsumed** action with that `payment_id` and an amount satisfying it. No match → deny. Without extra machinery this handles:

- **Two legitimate partial refunds of equal amount on one payment** → two actions → both pass. *(v2 rejected the second as a replay — a routine workflow it would have broken.)*
- **Replay** → the action is consumed → deny.
- **A1 injection "also refund pay_XYZ"** → no authorized action → deny, regardless of where the id came from.
- **Amount escalation** → fails the action's amount rule → deny.

**Exact by default, bounded by explicit choice.** `amount_paise` requires an exact match. `max_amount_paise` permits anything at or below it and exists only for genuine delegation ("refund up to the order value, agent determines which items came back"). A bounded grant means amounts inside it are authorized *by the merchant*, and §1 states the claim that way.

The third case above is the important one: **default-deny authorization, not provenance, is what generalizes against injection.** An injection can induce an action using an already-known id, copying nothing — provenance is blind to that; the capability list is not.

`allowed_tools` is minimal: read tools plus `create_refund`. `update_payment` was removed as an unrelated write permission.

### 3.3 Provenance — forensic only, not a gate

v2 called this the core mechanism and v3 still denied on it at pipeline step 4 while describing it as a secondary signal. Both were wrong, and the second was incoherent — a blocking control described as a non-blocking one.

**It is now out of the deny path entirely, because it is provably redundant.** To reach the old step 4, a refund must already have matched an action in `authorized_refund_actions` — which means its `payment_id` *is* a mandate literal, i.e. `USER_MANDATED` by definition. The gate could never fire on a call that passed the capability match. Removed as dead code, not as a risk trade.

What it still does, because it is nearly free *(the dashboard that originally motivated it was cut)*:

- Values in tool results are indexed with **all** their JSON paths, so an id appearing in canonical `id` *and later* in `notes` stays visible rather than resolving silently in the agent's favour.
- Paths carry a source-trust class: `SYSTEM_AUTHORITATIVE` (`id`, `entity`, `amount`, `status`, `order_id`, `created_at`) vs `PARTY_SUPPLIED` (`notes.*`, `description`, `customer_name`, `receipt`, `email`, `contact`).
- It renders the **forensic chain behind a flagged call** — *why* the agent arrived at this argument.

**Honest limits.** It detects a narrow literal-flow subclass, not A1 in general: an injection can act on an already-known id or a mandate literal with no copying at all, and transformed values (₹2,950 → `295000`) lose the link. Phase 4 reports only whether the signal *separates* attack from benign calls, labelled a **diagnostic and explicitly not part of the enforcement claim.**

### 3.4 One lifecycle for action and budget — fail closed on ambiguity

Action consumption and budget reservation move **together**, so there is one rule to reason about and one to test. MCP permits concurrent in-flight requests, so reservation is atomic.

The governing rule: **release only on confirmed provider rejection; anything ambiguous stays locked.**

| Transition | Trigger |
|---|---|
| `AVAILABLE → RESERVED` | matched an unconsumed action, before forwarding |
| `RESERVED → COMMITTED` | confirmed success |
| `RESERVED → AVAILABLE` | **confirmed provider rejection only** |
| `RESERVED → IN_DOUBT` | timeout, child crash, severed response — **action and budget stay locked** |
| `IN_DOUBT → COMMITTED / AVAILABLE` | **operator resolution only** |

Releasing on timeout would fail open: Razorpay may have processed the refund while the proxy lost the response, handing back budget for money that already left. Equally, a request that never reached the provider must not permanently burn a legitimate merchant authorization — hence release on *confirmed* rejection, and only that.

**No automatic reconciliation.** v3 had the proxy calling `fetch_multiple_refunds_for_payment` itself, which would have made it an MCP client to its own child and falsified §3.1. Cut. `IN_DOUBT` requires a human, via the CLI running **outside the relay path** with its own credentials. The injected receipt is the correlation key the operator looks the refund up by, so the workflow is intact with a person in it — arguably correct where money may already have moved.

**Absence is not evidence.** Eventual consistency, a pending refund, or a failed fetch all produce "not found" without meaning "did not happen." Only two automatic transitions are ever safe: confirmed rejection → release, matching receipt found → commit. `IN_DOUBT → AVAILABLE` on a missing record is forbidden in the proxy and in the operator tooling.

### 3.5 Receipt injection — duplicate rejection, stated precisely

The proxy injects a deterministic receipt into every forwarded refund, rejecting calls that supply a conflicting one.

**Format:** `rzpg_` + 12 hex digits of `sha256(mandate_id, action_id)` — e.g. `rzpg_de130f321d86`, satisfying the ≥10-character floor and the alphanumeric/underscore/hyphen constraint. *(v3's worked example used the bare 7-character `rfa_001`, an untested length assumption sitting in a code sample.)* **Verified accepted by the live API in G1.6** — `rzpg_6b8602afde6b` and `rzpg_de130f321d86` were both accepted and returned unchanged — not assumed.

**What it is:** a second, provider-side barrier so a duplicate is rejected even if the proxy's action-consumption check is bypassed.
**What it is not:** the `X-Refund-Idempotency` mechanism — no safe-retry semantics, and unreachable from a stdio proxy (§2.6).
**Its second job:** the correlation key that makes an `IN_DOUBT` case resolvable by an operator at all.

### 3.6 Trust boundary

- The mandate is loaded **from a path supplied at proxy launch**, before any agent connects.
- **Trusted:** whoever launches the proxy. **Untrusted:** the agent.
- **No MCP method, tool call, or JSON-RPC message can set, replace, extend or reload a mandate.** Attempts are logged as attacks.
- Bound to the proxy process lifetime; budget and action state live in that process.

**Limitation:** cumulative caps are **per proxy process**. Multi-process enforcement needs shared state — out of scope for twelve days.

### 3.7 Decision pipeline

1. **Mandate validity** — missing/expired/malformed → DENY.
2. **Tool allowlist** — not listed → DENY (default-deny; unknown tools deny).
3. **Action match** — unconsumed authorized action, matching `payment_id`, amount satisfying `amount_paise` or `max_amount_paise` → else DENY.
4. **Rate limit** → DENY if exceeded.
5. **Reserve** action + budget atomically (§3.4).
6. **Inject receipt**; forward to child stdin.
7. **Resolve** → COMMITTED / AVAILABLE / IN_DOUBT.

*(v3's provenance gate was step 4; deleted as redundant per §3.3.)* Every outcome writes one decision record: rule fired, matched action id, provenance chain with origin paths, human-readable reason.

### 3.7a Integration surface and deliberate non-goals

The impressive dependency is Razorpay's official server; the guard should be boring enough to read in one sitting.

**Child:** the unmodified official Go MCP server in Docker, **pinned by digest**, constrained to `TOOLSETS=payments,orders,refunds`. **Guard:** permits reads plus `create_refund` only — a narrower grant than the child's own surface, so the two boundaries are independent rather than one restating the other.

**Stack:** Python 3.11 `asyncio` subprocess relay · Pydantic mandate model · pytest · JSONL decision log · one FastAPI page.

**Explicit non-goals** — each of these would blur the central proof or reduce testability:

| Not building | Why |
|---|---|
| Webhooks | Inbound surface the proof does not need |
| Direct Razorpay SDK calls from the guard | The guard must reach Razorpay *only* through the child, or "sits in front of the official server" stops being true |
| Automatic reconciliation client | Would make the proxy an MCP client to its own child and falsify the transparent relay (§3.4) |
| LLM in the core path | The authorization decision is a deterministic lookup; a model there adds nondeterminism to a money path (§3.1 Decision C) |
| Database, queue, agent framework, React app | No requirement reaches for them |
| A fork of Razorpay's server | Forfeits "real, unmodified server" for a marginal gain (§2.6) |

### 3.8 Log security *(the dashboard half is CUT — see banner)*

The proxy ingests attacker-controlled text, logs it, and renders it in a browser.

- **Masking at write time**, not render time: `contact`, `email`, card fields and tokens are hashed/masked before touching disk.
- Field-level redaction allowlist; **text-node rendering only, never `innerHTML`**; restrictive CSP; stated retention rule.
- ~~**Test:** a fixture carrying `<script>` and `<img onerror=...>` in a `notes` field cannot execute in the dashboard.~~ **Withdrawn with the dashboard.** Nothing renders these fields as HTML, so there is no surface to test. The masking in the decision log stands and is exercised.

---

## 4. Phased plan with validation gates

### Phase 0.5 — Conformance corpus (**executed; claim downgraded in round 4**)

Committed at `e572d85` before any policy code existed — provable from `git log`, since `src/` is absent from that tree. Corpus v1.1 after [Amendment 1](PREREGISTRATION.md).

- **G0.1** Manifest, labels, split, seed and baselines committed **in their own commit**, so ordering is proven rather than asserted. ✅
- **G0.2** Fixtures authored and hash-committed **before any policy code exists**; deterministic (seed 20260824), regeneration reproduces an identical manifest hash. ✅
- **G0.3** ~~Labels come from an independent oracle~~ — **withdrawn.** Labels are **spec-derived**: the labelled reason *"no authorized action exists"* is the matcher's own predicate, so a human computing it computes the same function the policy does. Independent of the implementation, not of the design — and only the latter would make them evidential.
- **G0.4** Consequently this is a **policy-conformance and regression corpus**, not a detector evaluation. It pins behaviour on the workflows earlier plan versions would have broken (`B03` equal-amount partial refunds, `B07`, `B10`, `B11`, the replay family) and will catch regressions. Real value, weaker claim. The metric claim moves to **Phase 4b**. ✅

### Phase 1 — Ground truth & live harness (Days 1–2)

- **G1.0** Pin the `razorpay/mcp` **image digest**; record digest + source commit together.
- **G1.1** `tools/list` dumped to `evidence/tools_list.json`. *Runtime truth supersedes every schema claim in §2.*
- **G1.2** `fetch_all_payments` round-trips against test mode.
- **G1.3** Relay passes `initialize` + `tools/list` + a read call **byte-identically** vs. talking to the child directly. Diff must be empty.
- **G1.4** *Highest-risk gate:* produce a **captured test-mode payment** (`pay_*`) so `create_refund` is exercisable live. **No fallback** — if it cannot be done it is reported as an unmet gate.
- **G1.5** *Feasibility gate:* can the **unmodified** container be routed to a controlled upstream capture boundary? If not, the network-capture evidence claim is **dropped**, not kept as a promise.
- **G1.6 ✅ DONE (2026-08-25).** **Runtime verification, replacing a documentation claim I got wrong.** Re-check the captured evidence with `./run.sh verify-refund-evidence` (read-only). The runner that performed the refund has been REMOVED as offense-capable; see F18. Against real captured payment `pay_TTwUH29tzhB4ME`:
  - An **authorized** refund executes end to end through the shipped guard and the official pinned container → `rfnd_TTwsIoEmRPXnBa`. **14 assertions.**
  - The injected `rzpg_` receipt is **accepted by the live schema** and returned unchanged.
  - A **duplicate** receipt is **rejected by Razorpay**: `Duplicate receipt found for this refund request.` Scope tested: same receipt/payment/amount; wider uniqueness scoping not claimed.
  - Replay through the guard is refused locally (`ACTION_CONSUMED`), forwarding zero calls, with an alive-control correlated by request id.
  - The real success envelope is pinned as a fixture (`internal/relay/testdata/live_refund_result.json`) and asserted by unit tests, so the commit predicate cannot silently regress.
  - **Newly recorded limit:** the envelope was `status: "pending"` at decision time and only settled to `processed` afterwards. `COMMITTED` therefore means *the provider created the refund entity*, never *the money settled*. See FAILURES.md **F16** for the hole mutation-testing found in this gate itself.

### Phase 2 — Mandate + provenance (Days 3–4)

- **G2.1** Action matching: two legitimate partial refunds of equal amount on one payment **both pass**; one with no authorized action is denied; a replay of a consumed action is denied.
- **G2.2** **The legitimate lookup-then-refund workflow passes end to end.** Regression test for v1's fatal flaw; non-negotiable.
- **G2.3** Agent-chosen `speed` and `notes` never cause a denial. *(v2's blanket provenance rule would have blocked ordinary traffic.)*
- **G2.4** Exact vs. bounded amounts: an exact action rejects a lower amount; a bounded action accepts it.
- **G2.5** Provenance records all origin paths; the `id`-then-`notes` case is visible in the chain. Forensic only — no denial path.

### Phase 3 — Policy, lifecycle, receipt (Days 5–6)

- **G3.1** Default-deny: empty mandate blocks all 17 write tools; unknown tool name blocks.
- **G3.2** A live blocked refund is proven blocked by a **byte-level record of child stdin** showing the call never entered the child — a call whose bytes never reached the process cannot have produced an HTTP request for it. Network capture is corroboration **only if G1.5 passed**.
- **G3.3** A live allowed refund succeeds against test mode and returns a real refund id.
- **G3.4** **In-doubt handling — the hardest panel question.** Simulate *upstream processed the refund, response severed*: action and budget must **stay locked**, and resolution must require an operator. Includes the **delayed-visibility case** — a refund not yet visible in a fetch must **not** release the reservation.
- **G3.5** A confirmed provider rejection returns the action to `AVAILABLE` and does not burn the authorization.
- **G3.6** Two duplicate in-flight refunds: exactly one is forwarded.
- **G3.7** Mandate substitution via crafted tool call fails and is logged as an attack.
- **G3.8** Decision log replays deterministically, bit-for-bit.

### Phase 4 — Measured metrics (Days 6–8) — the graded phase

**Rebuilt after review round 4.** The v1.1 conformance corpus **cannot carry a metric claim**: its labels are computed from the same predicate the policy matches on, so scoring against it measures conformance to the spec, not detection. It remains as regression evidence. See [PREREGISTRATION.md Amendment 1](PREREGISTRATION.md).

The metric claim needs ground truth **not derived from the mandate**, which means observing a real agent instead of authoring its output.

**4a — Conformance (from the frozen corpus, no inference)**

- **G4.1** Policy scored against corpus v1.1 through the **actual proxy**. `score.py` carries a hard guard that cannot spawn a child process or open a socket.
- **G4.2** Reported as a **descriptive score on a frozen fixture set** over the `create_refund` slice only (heldout: 175 calls, 70 block / 105 allow). Protocol behaviour reported separately. **No confidence intervals, no inference to merchant traffic** — five sessions per template are replicas, and bootstrapping them is pseudoreplication.
- **G4.3** Every disagreement between policy and spec is a **conformance bug**, reviewed in writing. A perfect score here means the implementation matches its capability list — nothing more, and the README will say so.
- **G4.4** Baselines `B-amount` and `B-velocity` reported as **sanity checks, not competitive alternatives** (one uses an arbitrary threshold; the other is beaten by a fixture authored to beat it).

**4b — Agent-trace evaluation (the actual metric claim)**

**Protocol frozen in [PREREGISTRATION.md Amendment 2](PREREGISTRATION.md) before any trace was run.**

**✅ RUN TWICE, 2026-08-26.** Two arms over the identical frozen task set, one
variable changed — the generator. Reported separately and never pooled.

| | arm A `gpt-5.6-sol` | arm B `gpt-4o` |
|---|---|---|
| Emitted refund calls | 49 | 54 |
| Out-of-intent | **0** | **3** |
| Recall | undefined (empty positive class) | **1.000** (3/3) |
| Precision | degenerate (`0/8`) | **0.250** (3/12) |
| False blocks | 8/49 | 9/51 |
| Induced misuse | 0/15 | 3/15 |

**Arm A** ([RESULTS.md](study/RESULTS.md), [FINDINGS.md](study/FINDINGS.md)) did
not produce the outcome this section anticipated: the agent emitted no
out-of-intent refund at all, so the guard was never given a hostile call and
`TP=0 FP=8 TN=41 FN=0` makes precision and recall arithmetic artefacts.

**Arm B** ([RESULTS-armB.md](study/RESULTS-armB.md),
[FINDINGS-armB.md](study/FINDINGS-armB.md)) was pre-registered (Amendment F)
precisely because of that gap. An injected fake system note induced a
52000-paise refund in all three runs of brief C01 and **the guard blocked every
one** — the first evidence in this project that it does the job it was built
for. Three blocks in four were still wrong.

What the pair shows is the real result: **same guard, same briefs, opposite
pictures.** A detector rate measured against one model is not a property of the
detector, which is what §4.4's retraction argued in the abstract. G4.7's three
quantities are reported separately per arm; arm A's quantity 1 is `undefined` on
a zero denominator rather than reported as 100%.

- **G4.5** Ground truth is the **task brief**, not the observed calls. The agent's output is what is being measured, not the standard it is measured against.
  - ✅ Ground truth was the brief; the worksheet never showed the mandate or the guard's decision.
- **G4.6** Documented **compile/review boundary**: briefs authored and hash-committed first in natural language; the mandate produced from a brief by a reviewable `compile_mandate(brief)`; coverage gaps recorded at compile time. Separate files alone would not make them independent.
  - ✅ `compile_mandate` reads `merchant_authorizes` and never `intent_text`; two gaps were predicted in writing, and B01's materialised while B02's did not.
- **G4.7** Three quantities, **reported separately and never combined**: blocking rate given an out-of-intent refund was emitted; operational false-block rate given an in-intent refund was emitted; induced-misuse rate, which is a property of the **model** and says nothing about the detector.
  - ✅ Reported separately. Quantity 1 undefined (zero denominator), quantity 2 = 0.163, quantity 3 = 0/15.
- **G4.8** Frozen before the first trace: task set, intent statements, compiled mandates, model/version/temperature/prompts, **declared trace count**, adjudication rule with worked examples.
  - ✅ Frozen and hash-committed; the runner refuses a modified freeze or an uncommitted model choice.
- **G4.9** Reported as an **exploratory agent study** — no intervals, no significance, no inference. Single-adjudicator limitation stated; every adjudicated call published with its reason so a reader can disagree with any of them.
  - ✅ No intervals, no significance. Every one of the 49 calls published with its verdict and reason.

**False-positive cost model:** a blocked legitimate refund costs a support ticket plus a delayed customer refund, priced in ₹ and ops-minutes with assumptions inline, presented as a cost curve.

**Honest scoping note:** 4b is the part that answers *"what did the detector infer that wasn't already encoded in the action list?"* If time runs out, 4b ships with fewer traces rather than 4a's numbers being promoted to carry a claim they cannot support.

### Phase 5 — Dashboard + live demo (Days 9–10)

**Demo sequence** (tangible without broadening the attack surface): Test Checkout creates a captured payment → launch with a merchant-issued mandate → harness attempts an allowed refund → attempts an out-of-list refund → show the child-stdin proof, the decision record, and the real Test Mode refund object.


- **G5.1** A live blocked call appears within 1s with its matched-action decision and provenance chain rendered.
- **G5.2** Dashboard reads **only** from the decision log — no second source of truth.
- **G5.3** The XSS fixture test (§3.8) passes.
- **G5.4** An `IN_DOUBT` reservation is surfaced for operator resolution with its receipt as the lookup key.

### Phase 6 — Hardening, failure write-up, README, pitch (Days 11–12)

- **G6.1** Clean-clone → run → demo works from the README alone.
- **G6.2** At least one **real** failure documented with actual error output, the wrong hypothesis chased first, and the fix. *Five banked from Phase 0: the `create_payout` schema mismatch, the Docker daemon blocker, the incorrect idempotency claim, the fail-open budget release, and quoting a WebFetch summary as source text.*
- **G6.3** `REVIEW_LOG.md` complete for all phases.

---

## 5. Defense-only

- Fixtures are `.jsonl` MCP messages scored offline — data, not scripts. `corpus/` holds data and a scorer, **never an executor**.
- Attack fixtures are by construction the ones the proxy **blocks before forwarding**.
- **Non-resolvable synthetic identifiers** (`pay_SYN000...`) throughout.
- **Saved-card charging, OTP submission and token revocation stay out of live coverage entirely.**
- `score.py` cannot spawn a child process or open a socket.
- Test-mode keys only; no real test-account records or PII committed.

---

## 6. Repo layout

```
rzp-guard/
├── README.md              # structured by the 4 eval criteria
├── PLAN.md  PREREGISTRATION.md  METRICS.md  FAILURES.md  REVIEW_LOG.md
├── src/rzp_guard/
│   ├── relay.py           # stdio JSON-RPC interposer — never originates a request
│   ├── mandate.py         # capability list + validation
│   ├── policy.py          # default-deny pipeline, action matching
│   ├── lifecycle.py       # one state machine: action + budget, fail-closed
│   ├── provenance.py      # field-path index — forensic only
│   ├── decision_log.py    # append-only JSONL, masked at write time
│   └── (dashboard/)       # CUT — never built
├── corpus/                # DATA + scorer only — never an executor
│   ├── templates/  tuning/  heldout/  manifest.json  labels.jsonl
│   └── score.py           # hard guard: no subprocess, no sockets
├── evidence/
└── tests/
```

---

## 7. Risks I am least confident about

1. **Provenance may earn no measurable place.** It is now forensic-only, so this costs the enforcement claim nothing — but G4.4 may show the signal adds little even diagnostically, and that gets published.
2. **Transformation evasion** — restated values lose their origin link. Irrelevant to enforcement now; relevant to the forensic chain's completeness.
3. **Single-author corpus correlation.** Temporal precommitment is the best available substitute for independent authorship; it does not change that I imagined both the attacks and the defenses.
4. **G1.4 may not be reachable** without S2S enablement. Reported as unmet if so.
5. **Per-process budget** (§3.6) is a real enforcement limit.
6. **`IN_DOUBT` needs an operator.** A deliberate trade for a 12-day build; at scale it needs the reconciliation subsystem that was cut, and the README will say so.

---

## 8. Blocked on you

1. **Start Docker Desktop** — the daemon is down. (Alternative: install Go 1.24.2+; Docker is shorter.)
2. **Test-mode API keys** — Dashboard → Settings → API Keys, in **Test Mode**, into a gitignored `.env`.

Both gate Phase 1. Phase 0.5 was not blocked and is done (corpus v1.1).
