# rzp-guard

An authorization proxy that sits in front of Razorpay's **official, unmodified** MCP server and enforces a merchant-issued capability list over `create_refund`.

> **Status — read this before anything else.**
> This is a **tested Go authorization core with a live-verified block path**. It is **not** a detector, and **no precision, recall or false-positive numbers exist**. Test counts measure authorization and transport conformance, nothing more. The automatic *success* path is an explicitly unverified compatibility guess (§Known limits).

```bash
./run.sh operator-setup    # ONCE, before first start: create the recovery credential
./run.sh test              # unit tests: no Docker child, no keys, no network
./run.sh race              # same under the race detector
./run.sh lifecycle         # 4 process-lifecycle tests (-tags testhook, separate lane)
./run.sh all               # every lane, including BOTH race runs
./run.sh live-block        # LIVE: unauthorized refund never reaches the real
                           #       pinned container, with an ENFORCED alive-control
./run.sh process-recover   # child death → durable IN_DOUBT surviving restart
                           #       (local stub child; not the official container)
```

`live-block` proves one thing: **the production guard blocks before the child's stdin**. It does not demonstrate safe credential provisioning — its fixture uses a test-hook `init-ephemeral` that discards the token, and provisioning on Windows is separately unproven.

`live-block` needs Docker running and **test-mode** keys in `.env`. `RAZORPAY_KEY_ID` must start with `rzp_test`; the guard refuses to start otherwise.

Only `live-block` drives the official container. `process-recover` is named honestly: it exercises the guard's cleanup wiring against a local stub, because against the real container Razorpay answers in well under a second and the reply wins the race against any kill — measured at 2s and 0.15s, not assumed.

**The gates are assertions, not printouts.** `cmd/gate-verify` parses the captured JSON and fails the run if the control read is not a genuine non-error Razorpay entity. Verified negatively: with a wrong secret the gate exits 1.

---

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

**Durable and fail-closed.** Reservations are written to SQLite before anything is forwarded. A reservation still open at startup becomes `IN_DOUBT` and stays locked until an operator resolves it. Ownership is exclusive: a second guard process over the same state file is refused, verified by spawning a real second process.

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

## Known limits — stated, not buried

1. **No detector metric exists.** No precision, recall or false-positive cost. The conformance corpus in `corpus/` cannot supply them: its labels are computed from the same predicate the policy matches on, so scoring against it measures conformance to the spec, not detection ([PREREGISTRATION.md Amendment 1](PREREGISTRATION.md)). The real measurement needs agent traces with intent specified independently of the mandate, and it has not been run.
2. **Automatic `COMMITTED` is an unverified compatibility path.** The expected refund-entity shape comes from Razorpay's documentation; no live success envelope has been captured, because that needs a real captured payment via Checkout. The failure mode is fail-closed — an unrecognised success shape yields `IN_DOUBT`, never a wrong COMMIT — but the success path itself is not demonstrated.
3. **Recovery is an availability gap, not just an inconvenience.** Stop → check Razorpay → resolve → restart means a window with no guard and no forwarding service. There is **no measured recovery drill**: outage duration, what blocks new requests meanwhile, and how the correct state file and operator identity are selected are all unanswered. The CLI does not address them.
4. **The threat model assumes a protected service account and state directory.** The guard's refusal to run unprovisioned closes the ordinary first-writer race, but it does not protect against someone who can create or modify the state directory *before* provisioning. **The operator token is not an independent security boundary** — it is a second factor on top of filesystem ownership. `-out` verifies the file actually landed at `0600` and **refuses otherwise, with no bypass in shipped builds** — Windows lands `0666`, measured, so `-out` simply does not work there. The escape hatch exists only under `-tags testhook`, for gates writing to throwaway directories.

**Credential delivery is the weakest part of this design, and it now fails closed.** The credential is committed **only when delivery can be proven durable** — the token file written, fsynced, and its parent directory fsynced. Terminal output cannot be proven (a disconnect or lost scrollback leaves no token), and Windows cannot fsync a directory, so **both are refused unless `-accept-delivery-risk` is passed**, which prints an `UNSUPPORTED FOR DEPLOYMENT` banner.

The practical consequence, stated rather than hidden: **provisioning is not supported on Windows.** `-out` fails the `0600` check and terminal output fails the durability check. The robust answer is an OS secret store, and this does not have one.
5. **Recovery takes the guard offline.** The operator CLI needs the state file, and the guard holds an exclusive lock on it for its lifetime. So the procedure is stop → check Razorpay → resolve → restart, which means the protection layer is down while an ambiguous transaction is adjudicated. It is a **tested offline recovery procedure, not an operational workflow**: no run yet shows an operator resolving a real Test Mode refund whose status was checked in the Razorpay dashboard.
6. **`process-recover` uses a stub child**, not the official container, for the reason above. Only `live-block` is live.
7. **Cumulative caps are per state file.** Enforced by exclusive ownership rather than distributed coordination.
8. **Provenance detects a narrow literal-flow subclass**, and is forensic only.

## Layout

```
cmd/rzp-guard/      the executable: bootstrap → relay → pinned child container
                    (child fixed at the pinned digest; no runtime override)
cmd/gate-verify/    enforces the live gates' assertions from captured JSON
cmd/rzp-guard-operator/  init / rotate / list / audit / resolve for IN_DOUBT refunds
internal/opauth     operator credential: generation, Argon2id verifier, verification
internal/mandate    capability list, receipt derivation
internal/policy     default-deny decision pipeline
internal/lifecycle  action + budget state machine, operator console
internal/storage    durable SQLite state, exclusive ownership
internal/relay      transparent JSON-RPC stdio interposer
internal/bootstrap  ordered startup: ownership → recovery → restore
corpus/             conformance fixtures (data + scorer, never an executor)
evidence/           captured output backing every claim in this file
prototype/python/   FROZEN reference prototype — not the product
```

Defense-only: fixtures are recorded JSON-RPC data with non-resolvable synthetic identifiers; forbidden-tool fixtures carry no payload; the corpus scorer cannot spawn a process or open a socket; test-mode keys only.
