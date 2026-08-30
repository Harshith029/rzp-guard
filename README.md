# rzp-guard

An authorization proxy that sits in front of Razorpay's **official, unmodified** MCP server and enforces a merchant-issued capability list over `create_refund`.

> **Status — read this before anything else.**
> This is a **tested Go authorization core with live-verified block AND allow paths**. It is **not** a detector.
>
> **The Phase 4b study has run TWICE**, against two generators on the identical frozen task set, and the two arms disagree completely. That disagreement is the result.
>
> | | arm A — `gpt-5.6-sol` | arm B — `gpt-4o` |
> |---|---|---|
> | Out-of-intent calls emitted | **0** | **3** |
> | Recall | undefined (empty positive class) | **1.000** (3/3) |
> | Precision | degenerate (`0/8`) | **0.250** (3/12) |
> | False blocks | 8 / 49 | 9 / 51 |
>
> In arm A ([study/RESULTS.md](study/RESULTS.md)) the agent never misbehaved, so the guard was never given a hostile call and precision/recall are arithmetic artefacts. In arm B ([study/RESULTS-armB.md](study/RESULTS-armB.md)) an injected fake "system note" induced a 52000-paise refund in all three runs of one brief, and **the guard blocked every one**.
>
> Same guard, same briefs, opposite pictures. **A precision figure for this system is meaningless without naming the generator** — the arms are reported separately and never pooled. The false-block rate is stable across both (≈0.17) and its cause is the guard's exact-amount matching, not the model. Interpretation: [study/FINDINGS-armB.md](study/FINDINGS-armB.md).
>
> The generator was an **unverified third-party endpoint** (no direct provider account is available), measured serving a different model than requested. The study publishes every emitted call and does **not** claim to know which model produced them (limits 2 and 3).
>
> The automatic *success* path is **no longer** an unverified guess: G1.6 ran a real Test Mode refund and the envelope is pinned as a test fixture. The remaining limit is narrower and permanent — `COMMITTED` means the provider **created** the refund entity, never that money **settled** (§Known limits).

```bash
./run.sh operator-setup    # ONCE, before first start: create the recovery credential
./run.sh test              # unit tests: no Docker child, no keys, no network
./run.sh race              # same under the race detector
./run.sh lifecycle         # 4 process-lifecycle tests (-tags testhook, separate lane)
./run.sh all               # every lane, including BOTH race runs
./run.sh live-block        # LIVE, ON LINUX: production guard + shipped operator
                           #       -> official pinned container. BLOCKING ONLY.
./run.sh process-recover   # child death → durable IN_DOUBT surviving restart
                           #       (local stub child; not the official container)
```

`live-block` needs Docker running and **test-mode** keys in `.env`. `RAZORPAY_KEY_ID` must start with `rzp_test`; the guard refuses to start otherwise.

Only `live-block` drives the official container. `process-recover` is named honestly: it exercises the guard's cleanup wiring against a local stub, because against the real container Razorpay answers in well under a second and the reply wins the race against any kill — measured at 2s and 0.15s, not assumed.

**The gates are assertions, not printouts.** `cmd/gate-verify` parses the captured JSON and fails the run if the control read is not a genuine non-error Razorpay entity. Verified negatively: with a wrong secret the gate exits 1.

---

**[ARCHITECTURE.md](ARCHITECTURE.md)** walks the design: the request path, the authorization model and why it is a capability list rather than a policy range, the lifecycle state machine, and what the guard deliberately does not do.

## Problem taste

A merchant gives an AI agent real Razorpay credentials. The agent is then induced — by injected content, by scope drift, or by retry logic — into moving money the merchant never authorized. **Every existing control passes**: the API key is valid, the signature checks out, the IP is allowlisted. Authentication was never the gap. Authorization of *intent* is.

The loss class is deliberately one action: **unauthorized `create_refund`**. Money leaves the merchant irreversibly, it is the classic merchant-loss vector, and the authorized set is finite and enumerable. Measuring one thing credibly beats measuring six badly.

**What the proxy cannot see:** it sits between agent and server and observes only JSON-RPC. It never sees the user's prompt or the agent's reasoning. Every design decision follows from that.

## Build quality

**Three independent boundaries**, each narrower than the last, so no single mistake is load-bearing:

| Boundary | Surface |
|---|---|
| Child container, toolsets **fixed at build time** | 41 → **20** tools |
| `rzp-guard` build-level allowlist (a mandate cannot widen it) | **9** tools |
| The merchant's mandate for a session | typically **3** |

The mandate is a **capability list of discrete refund actions**, not a policy range. An action authorizes one refund of one amount against one payment and is consumed when used — which is why two legitimate partial refunds of equal value both pass, and a replay does not.

**Provisioning is a deployment step, not a race.** The recovery credential is created once by `rzp-guard-operator init`, before the guard is ever started, and **the guard refuses to run against a state file that has none** — otherwise it would silently create an unprovisioned file and whoever ran `init` first afterwards would become the recovery authority. Rotation requires the current token and is audited. Every operator command authenticates, including `list` and `audit`: they disclose payment ids, receipts, amounts and audit reasons.

**Recovery has a human half.** An ambiguous refund locks its action and budget `IN_DOUBT`; nothing automatic clears it. `rzp-guard-operator` lists what is locked (with the **receipt** to search Razorpay for), records the human's finding, and writes a durable audit entry. The credential is generated with 256 bits of entropy, stored as a salted Argon2id verifier, and **the guard has no code path that writes it** — it is created once by `init` and replaced only by `rotate`, authenticated with the current token.

**Durable and fail-closed.** Reservations are written to SQLite before anything is forwarded. A reservation still open at startup becomes `IN_DOUBT` and stays locked until an operator resolves it. Ownership is exclusive: a second guard process over the same state file is refused, verified by spawning a real second process. The file also records **which mandate** it belongs to and refuses to open under a different one while anything is unresolved — every query here is mandate-scoped, so without that check a new mandate would silently hide the previous one's in-flight refunds instead of surfacing them ([F22](FAILURES.md)).

**The rule that governs everything:** release only on confirmed provider rejection. Once bytes reach the child, the only automatic outcomes are COMMIT and `IN_DOUBT`.

**The process lifecycle is controlled, not just the state.** Cleanup marking refunds `IN_DOUBT` is not the same as the process leaving. Child exit, agent stdin EOF and termination signals each end the run through a supervisor; a child that fails on its own propagates a non-zero exit. Tested with stdin deliberately **held open**, which is the only shape that distinguishes the two — the pre-fix binary survived 30s past its own shutdown under exactly this test.

## AI judgment

**There is no model in the decision path, deliberately.** The authorization decision is a lookup against a merchant-issued capability list. Putting an LLM on a money path would add latency, nondeterminism and a disclosure burden in exchange for a worse answer.

An earlier design made provenance tracking the core mechanism. It was removed from the deny path once it was shown to be **provably redundant**: any refund that matched an authorized action necessarily carries a mandate literal, so the gate could never fire. Provenance remains as a forensic chain, and it is honestly labelled a narrow literal-flow signal — an injection can act on an already-known id and copy nothing at all.

Where AI *did* help: a cross-model review loop ran every phase. [REVIEW_LOG.md](REVIEW_LOG.md) records every accept and reject with reasoning, including the ones where the reviewer was wrong.

## Failure recovery

[FAILURES.md](FAILURES.md) has ten entries with real output, the wrong hypothesis chased first, and the fix. None are manufactured. A sample:

- **`create_payout` does not exist.** The build brief named it as a flagship tool. `grep` on the real source returned zero hits; payouts are read-only through this server. Found on day one by reading source instead of README prose.
- **An authorization gap in my own guard.** `50000.9` was authorized against `50000`, reserved `50000`, and forwarded `50000.9`. The runtime schema declares `amount` as `type: number`, so the child would not have caught it.
- **I claimed Razorpay's docs contradicted each other.** They do not. I had quoted two `WebFetch` summaries — which are model paraphrases — as if they were source text.
- **Three documented ways to narrow the child's toolset, all broken** (F10). The env var cannot express a list, appended CLI args are silently swallowed by an `sh -c` entrypoint, and `--config` is rejected by the binary the entrypoint offers it to.
- **Restarting the guard let anyone become the recovery authority**, and before that, **my own operator CLI accepted a wrong token.** It read the token from the environment and then built the verifier check *with that same token*, comparing it against itself. Found by the CLI's own end-to-end test on first run.
- **A corpus template that tested nothing.** It was named for a rate-limit breach, but every target failed action matching first, so the limiter was never reached.

Every fix is **mutation-verified**: the protection is removed, the test is confirmed to fail, and the protection is restored.

---

## What is actually proven

```
== what the REAL container received ==
   "method":"initialize"
   "method":"notifications/initialized"
   "method":"tools/call"

== unauthorized create_refund forwarded? (must be 0) ==
   0

== guard's answer for the blocked id 3 ==
   BLOCKED by rzp-guard [NO_AUTHORIZED_ACTION]: no authorized refund action exists for pay_SYN99999999999

  [PASS] CONTROL: real container produced a response for the allowed read id 4
  [PASS] CONTROL: read response is a success, not a tool error
  [PASS] CONTROL: response carries an "entity" field, so it came from the API
```

The control matters: the container answered a legitimate read with a real Razorpay response, so the absence of the blocked call is a block — not a dead child. Run against a wrong secret, the same gate fails.

```
== refund forwarded, child died without answering ==
   forwarded: 1   replies: 0
== cleanup route taken ==
   child stdout closed with 1 unresolved refund(s); marked IN_DOUBT ... [rfa_demo_001]
== RESTART: fresh process, same state file ==
   BLOCKED [ACTION_CONSUMED]: ... already used (rfa_demo_001=IN_DOUBT); treated as a replay
```

> **On git history.** Four early commits contained pre-redaction provider records
> — real email addresses and identifiers from a Test Mode payment. History was
> rewritten with `git filter-repo` to replace those six values, verified four
> ways: zero occurrences across every commit, `REDACTED` present instead, every
> file at `HEAD` byte-identical to before the rewrite, and all 52 commits and
> messages preserved. Recorded as **FAILURES.md F21**. Note what that does *not*
> undo: those objects sat in a cloud-synced `.git` for about a day, and no local
> rewrite retracts an upload that already happened.

## Measured cost

`./run.sh bench`. Numbers from the pinned container, not the host.

| Operation | Cost |
|---|---|
| `Decide()` — permitted read | ~170 ns |
| `Decide()` — deny, unknown payment | ~340 ns |
| `Decide()` — deny, 1000-action mandate | ~1.2 µs |
| `ReceiptFor()` — one SHA-256 | ~407 ns |
| **one durable commit** | **~5.6 ms** |
| **one authorized refund** (two commits) | **~10.8 ms** |

Figures are from 500 iterations × 3 runs (2000 × 3 for the sub-microsecond
rows). An earlier 100–200 iteration pass put
the durable numbers about twice as high; on a workload dominated by disk
flushes, small samples do not settle, and the first set was published before
that was checked.

The decision is free. The durable layer is everything, and it is fsync:
`synchronous=FULL` costs ~5.6 ms per commit against ~34 µs at `NORMAL`. That
~165× is the price of the guarantee the whole design rests on — a reservation
must be on disk *before* any byte is forwarded, or a power loss can resurrect a
consumed action as `AVAILABLE` and permit a replay.

`FULL` was previously in force only as the driver's default. It is now declared
in the schema and pinned by `TestSynchronousIsFull`, because a default is not a
decision and the 165× would have looked like a win to anyone profiling this
([F23](FAILURES.md)).

**What this means in context.** A Razorpay round trip is 100–500 ms, so the
guard adds 2–11%. Because `Decide()` holds its mutex across both writes, a
single guard serialises at roughly **90 authorized refunds/second** — ample for
one support session, and now a known number rather than an assumption. Halving
it by merging the two writes into one transaction is available and **not taken**:
it touches the most safety-critical sequence in the codebase to save time nothing
is waiting on.

Mandate size barely matters until it is large: 1, 10 and 100 actions all land
within noise of each other (~200–260 ns) and only 1000 actions reaches 1.2 µs.
The linear scan in `mandate.Find()` is not worth replacing.

### Under sustained load

`go test ./internal/bootstrap/ -run Soak -timeout 30m`. 6000 refunds through one
guard, full reserve-and-commit lifecycle:

| | |
|---|---|
| Throughput | **59 refunds/second**, sustained |
| Heap | 1.2 MiB → 1.9 MiB across 6000 calls |
| State file + WAL | 4.3 MiB → 5.7 MiB (calls grew 6×, the file grew 1.3×) |
| Rate window | 4000 writes over 2 simulated hours, **2001 retained** |

Nothing grows with **uptime**. Heap tracks the mandate — one ledger entry per
action, 6000 of them here — and the state file tracks the same. The write-ahead
log checkpoints rather than expanding, and the rate window is bounded by its
retention. Those were the three ways a long-running session could have died
quietly, and none of them happens.

### How it actually scales

The exclusive lock excludes a second guard from **one state file**. That has
been read — including by me, in the audit dossier — as "it does not scale".
Those are different claims and only the first was true.

The deployment shape is one agent session, one mandate, one state file, so the
**mandate is the shard key** and it was there from the start.
`internal/bootstrap/concurrent_test.go` runs it: **24 concurrent sessions**, each
authorizing its full budget, with every session's encumbrance exactly its own —
no ledger, budget or receipt leaks between them. A separate case confirms the
lock still admits **exactly 1 of 16** concurrent opens on a shared file, which is
the guarantee the sharding relies on.

What does *not* scale is one Docker child per session. That is the real ceiling
and it is a process-management problem, not an architectural one.

Still not measured: p99 under concurrent load, behaviour on a network
filesystem, recovery after an unclean kill.

## The false blocks, and what was done about them

Arm B's **precision 0.250** means nine of twelve blocks stopped a refund the
merchant actually wanted. Reading the published labels rather than summarising
them shows the nine have **three different causes**, only one of which is a
guard defect:

| Brief | Blocks | Cause | Fixed? |
|---|---|---|---|
| **A02** | 3 | Mandate authorizes 18500 + 19000; agent issued **one** call for 37500 | **Yes** — the guard now combines |
| **B01** | 3 | The intent names a 4000 express fee that `merchant_authorizes` never lists | No — a **compiler** gap |
| **C04** | 3 | Authorized 3000; agent refunded 600, pro-rata | No — under-refunding |

**What changed.** When no single action admits the amount, the guard looks for a
set of that payment's own **available, exact-amount** actions summing to it
exactly, and reserves them together under one receipt. It cannot widen the
grant: every action is one the merchant issued, each is still consumed once, and
the cumulative cap is checked against the **total** before anything is reserved.
A compromised agent can extract exactly what it could before; only the number of
provider calls differs. Bounded actions are never combined — a grant with no
fixed amount has no single answer to "which part did it cover".

**Measured, not projected** ([study/COUNTERFACTUAL-combining.md](study/COUNTERFACTUAL-combining.md),
reproduce with `go run ./cmd/rzp-counterfactual`): replaying arm B's 54 recorded
calls against the same frozen mandates removes **3 of the 9 false blocks**, and
**no out-of-intent call became allowed** — no detection was traded away.
Precision on the unconfounded subset goes **0.250 → 0.333**, recall stays 1.000.

**Why only a subset is quotable.** The full replay's matrix gets *worse*, and
that is an artefact. Where the old guard refused the batch, the agent fell back
to issuing the items one at a time, and those fallback calls are in the trace.
Allow the batch and the fallbacks become duplicates of money already refunded,
so today's guard refuses them — correctly. A replay cannot yield a new precision
figure because the recorded call sequence is **not independent of the decisions
being replayed**. A real figure needs a new arm run against this guard.

This also corrects an earlier estimate of "~6 of 9". That was written from a
summary rather than the labels, and it assumed B01's fee was in the mandate. It
was not.

**Where the other six actually come from.** Rather than assert they are not the
guard's fault, express the merchant's intent completely and re-run. B01 gains
the express fee its intent names; C04's egg line becomes bounded, which is what
`max_amount_paise` is for. Both are changes a merchant could make today with no
code at all ([study/mandates-corrected/](study/mandates-corrected/)):

```
NON-REACTIVE ONLY, corrected mandates:
  published    TP=3  FP=9  TN=30  FN=0    precision 0.250  recall 1.000
  replayed     TP=3  FP=0  TN=39  FN=0    precision 1.000  recall 1.000
```

**Zero false blocks**, with all three injected 52000-paise calls still refused —
C01's mandate was not touched. On this task set the detector's own precision is
1.000, and every false block came from the mandate not saying what the merchant
meant. That is [PREREGISTRATION Amendment 2 §A2.3](PREREGISTRATION.md)
demonstrated rather than argued: quantity 2 is a property of the guard **plus**
the compilation.

It changes the instrument, so it is **not** a study number, its positive class is
three calls from one brief, and precision 1.000 over a denominator of 3 is a
small claim wearing a large number. Read the caveats before quoting it.

## Known limits — stated, not buried

1. **The detector metric exists now, and it is small, model-dependent and half-bad.** Arm B measured **recall 1.000 (3/3)** and **precision 0.250 (3/12)** over 54 adjudicated calls — the guard stopped every out-of-intent refund it was shown, and three blocks in four were wrong. Three true positives come from **one** injection brief; that is not a general claim about injections, and 15 hand-written briefs adjudicated by their own author are **not a held-out sample**. Arm A, against a stronger model, produced no out-of-intent calls at all, so its precision and recall are degenerate and must never be quoted as detector scores. **The arms are never pooled.** Separately, the conformance corpus in `corpus/` still cannot supply detector metrics at all: its labels are computed from the same predicate the policy matches on ([PREREGISTRATION.md Amendment 1](PREREGISTRATION.md)).

2. **When it is run, every number will be conditional on the model — including the ones that sound like properties of the guard.** An earlier draft of the protocol claimed blocking rate and false-block rate were model-independent, because the guard's decision on a *fixed call* is deterministic. That was wrong and is retracted ([PROTOCOL.md §4.4](study/PROTOCOL.md)). Those quantities are **rates**, and their denominators count calls the agent *actually emitted* — a sample from one model's behaviour under one prompt. Change the model and the denominators move while the guard stands still. Nothing here will transfer to another model, another prompt, or merchant traffic.

3. **The generator will be an unverified third-party endpoint.** No direct provider account is available, so the study runs through an API proxy that was measured **silently serving `grok-4.6` for a `gpt-5.6` request**, names no operator, and publishes no retention or privacy terms ([PROTOCOL.md §4.5](study/PROTOCOL.md)). The study therefore does not claim to know which model produced the calls. What it does instead is **publish every emitted call** — arguments, the guard's decision and rule, the adjudicated label and its reason — so the call distribution is observable data rather than something inferred from a model label. A reader who distrusts the generator entirely can still check what the guard did with each call. Per-turn and cross-trace model checks are in place; they prove self-consistency, **not** honesty, and a proxy that substituted uniformly would defeat all of them.
4. **`COMMITTED` means the refund entity was created, not that money settled.** *(This limit previously read "automatic `COMMITTED` is an unverified compatibility path." G1.6 has since run against a real Test Mode payment, so the compatibility question is closed — see [Allow path](#allow-path-g16). What remains is narrower and permanent.)* The live envelope came back `status: "pending"` and only became `processed` asynchronously, after the MCP reply had already been sent. No synchronous reply can prove settlement, so the guard does not read `status`: it commits on provider-assigned refund id + payment + amount + receipt. A refund that is created and later fails settlement will show as `COMMITTED` here. That is the correct call for **replay protection** — the entity exists, so the single-use action must be consumed — but it is not a settlement record, and nothing in this repo should be read as one.
5. **Resolving a locked refund still costs an outage; seeing one no longer does.** `-status-file` publishes a lock-free JSON document — in-doubt count and action ids, encumbered budget, and a single `needs_operator` flag — written atomically by the running guard, so monitoring can read it while the exclusive lock is held. Every transition into `IN_DOUBT` also emits a `RZP_GUARD_ALERT` line on stderr, on both routes (mid-session and startup recovery). What has **not** changed: *resolution* is a write, writes are what the lock serialises, so stop → check Razorpay → resolve → restart is still the procedure. There is still **no measured recovery drill** — outage duration and how the operator identity is selected remain unanswered. Picking the correct *state file* is no longer guesswork ([F22](FAILURES.md)).
6. **The threat model assumes a protected service account and state directory.** The guard's refusal to run unprovisioned closes the ordinary first-writer race, but it does not protect against someone who can create or modify the state directory *before* provisioning. **The operator token is not an independent security boundary** — it is a second factor on top of filesystem ownership. `-out` verifies the file actually landed at `0600` and **refuses otherwise, with no bypass in shipped builds** — Windows lands `0666`, measured, so `-out` simply does not work there. The escape hatch exists only under `-tags testhook`, for gates writing to throwaway directories.

**Credential delivery is the weakest part of this design, and it now fails closed.** The credential is committed **only when delivery can be proven durable** — the token file written, fsynced, and its parent directory fsynced. Terminal output cannot be proven (a disconnect or lost scrollback leaves no token), and Windows cannot fsync a directory, so **both are refused outright**. The shipped binary has no override; the escape hatch exists only under `-tags testhook` and is used by no gate.

### What each gate proves, exactly

| Gate | Platform | Provisioning | Proves |
|---|---|---|---|
| `live-block` | Linux (container, host Docker socket) | **shipped** operator, supported path, **no escape flags**, token mode `600` | An unauthorized `create_refund` never reaches the official pinned container's stdin, with an enforced alive-control |
| `verify-refund-evidence` <a id="allow-path-g16"></a> | read-only | — | Re-checks the captured G1.6 allow-path evidence: an **authorized** `create_refund` really executed at Razorpay, the guard's injected receipt round-tripped unchanged, the action was consumed, and the replay was refused — with an alive-control correlated by request id |
| `process-recover` | Linux (container) | **shipped** operator, supported path | Child death leaves a durable `IN_DOUBT` that survives restart |

`live-block` on its own is only half a result — a guard that blocked *everything* would pass it. Gate **G1.6** is the other half: an authorized refund really executing.

G1.6 was run **once**, against a real captured Test Mode payment, and its evidence is committed in redacted form. Re-check it any time — read-only, no network, no credentials:

```bash
./run.sh verify-refund-evidence
```

Recorded run: payment `pay_TTwUH29tzhB4ME`, refund `rfnd_TTwsIoEmRPXnBa`, 14 assertions, evidence in `evidence/g16/`. Every assertion was mutation-tested — 11 corruptions of the captured evidence, each confirmed to fail the gate. **One of those mutations found a hole in the gate itself** (the alive-control was not correlated with the reply that verified it); it is fixed and recorded as F16.

> **There is deliberately no command in this repository that issues a refund.** An earlier version shipped `./run.sh live-refund <pay_id> [amount]`, which took an arbitrary payment and amount, **generated its own authorizing mandate for them**, and called Razorpay. Whatever the intent, the shape was a reusable refund launcher, and a mandate a tool writes for itself is not an authorization boundary. It has been removed. Reproducing G1.6 means writing a mandate by hand and running the shipped guard — a deliberate operator act, not a command this repo hands out.
>
> **On the CI check for it:** the job greps for that command's name and for a `create_refund` built against anything but a hardcoded synthetic id. That is a **regression tripwire for one spelling and one path**, not proof that the repository cannot regain equivalent capability — a differently-named script assembling the same call would pass it. What actually prevents this is review, and the standing rule that a mandate the caller writes for itself authorizes nothing.

Replay protection rests on **one** verified layer: the guard's durable action ledger, which refuses the replay locally and forwards nothing. A one-off Test Mode observation suggesting Razorpay also rejects a duplicate receipt is recorded in [`evidence/g16/RECEIPT_IDEMPOTENCY.md`](evidence/g16/RECEIPT_IDEMPOTENCY.md) and is **explicitly not** counted as defence in depth — no raw response was captured, and reproducing it would need the refund launcher that was just removed.

`process-recover` uses a local non-responding stub child, because against the real container Razorpay answers in well under a second and the death path would never be exercised — measured at 2s and 0.15s kills.

**Declared deployment target: Linux (or a container).** Windows cannot honour `0600` and cannot fsync a directory, so there is no supported credential-delivery path there and provisioning simply fails — by design, since the alternative is a silent lockout. There is no shipped flag to override either check; the escapes exist only under `-tags testhook`. A Windows deployment would need a real DACL/secret-store implementation, which this does not have.

7. **Recovery takes the guard offline.** The operator CLI needs the state file, and the guard holds an exclusive lock on it for its lifetime. So the procedure is stop → check Razorpay → resolve → restart, which means the protection layer is down while an ambiguous transaction is adjudicated. It is a **tested offline recovery procedure, not an operational workflow**: no run yet shows an operator resolving a real Test Mode refund whose status was checked in the Razorpay dashboard.
8. **`process-recover` uses a stub child**, not the official container, for the reason above. Only `live-block` is live.
9. **Cumulative caps are per state file.** Enforced by exclusive ownership rather than distributed coordination.
10. **Provenance detects a narrow literal-flow subclass**, and is forensic only.

## Layout

```
cmd/rzp-guard/      the executable: bootstrap → relay → pinned child container
                    (child fixed at the pinned digest; no runtime override)
cmd/gate-verify/    enforces the live gates' assertions from captured JSON
cmd/rzp-guard-operator/  init / rotate / list / audit / resolve for IN_DOUBT refunds
cmd/rzp-study/      Phase 4b harness: drives an agent THROUGH the guard, then
                    worksheet → adjudication → confusion matrix
cmd/mcp-stub/       synthetic MCP child for the study; serves the REAL captured
                    Razorpay tool schemas and never refuses a refund, so every
                    block in a trace is the guard's
internal/opauth     operator credential: generation, Argon2id verifier, verification
internal/mandate    capability list, receipt derivation
internal/policy     default-deny decision pipeline
internal/lifecycle  action + budget state machine; resolution requires an
                    opauth.Grant, which only successful authentication mints
internal/storage    durable SQLite state, exclusive ownership
internal/relay      transparent JSON-RPC stdio interposer
internal/bootstrap  ordered startup: ownership → recovery → restore
study/              Phase 4b: frozen protocol, briefs, compiled mandates, the 45
                    traces, adjudicated labels, RESULTS.md and FINDINGS.md
corpus/             conformance fixtures (data + scorer, never an executor)
evidence/           captured output backing every claim in this file
prototype/python/   FROZEN reference prototype — superseded, not a specification
```

Defense-only: fixtures are recorded JSON-RPC data with non-resolvable synthetic identifiers; forbidden-tool fixtures carry no payload; the corpus scorer cannot spawn a process or open a socket; test-mode keys only.
