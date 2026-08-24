You are reviewing a security-critical Go component as an adversarial staff engineer at a company that moves other people's money. Be skeptical by default. Your job is to find what is wrong with this, not to validate it. Prior rounds of your review have found a fail-open budget release, an authorization gap where a fractional amount was authorized as its truncation and forwarded as the original, a circular evaluation corpus, a JSON-RPC correlation bug that let a `tools/list` reply settle a refund, and a hung-process bug. Assume there are more.

## What the project is

`rzp-guard` is an authorization proxy for the Razorpay AI Buildathon (Track 2 — AI Risk Manager). A merchant gives an AI agent real Razorpay API credentials via MCP. The agent can then be induced — by injected content, scope drift, or retry logic — into moving money the merchant never authorized. Every existing control passes, because the credentials are genuinely valid. Authentication is not the gap; authorization of intent is.

The guard speaks MCP over stdio to a calling agent and runs Razorpay's **official, unmodified** MCP server (`razorpay/mcp`, pinned by digest `sha256:435109006d62…506e5c6`) as a child process. It enforces a merchant-issued capability list over exactly one action: `create_refund`. A denied `tools/call` is answered by the guard and its bytes are never written to the child's stdin.

Scope is deliberately one tool. Prior review rounds narrowed it from six.

## Architecture, and why

**Mandate = capability list of discrete refund actions**, not a policy range. An action authorizes one refund of one amount against one payment and is consumed when used. This is why two legitimate partial refunds of equal value both pass (an earlier `(payment_id, amount)` fingerprint rejected the second as a replay — a routine merchant workflow it would have broken), and why a genuine replay does not.

**Three narrowing boundaries, deliberately independent:** the child container is fixed at `--toolsets payments,orders,refunds` (41 → 20 tools); the guard has a build-level allowlist a mandate cannot widen (9 tools); the session mandate narrows further (typically 3).

**No model in the decision path.** The authorization decision is a lookup. An earlier design made provenance tracking the core mechanism; it was removed from the deny path after being shown provably redundant — any refund matching an authorized action necessarily carries a mandate literal, so the gate could never fire. Provenance survives only as a forensic chain and is labelled a narrow literal-flow signal.

**Fail-closed lifecycle.** Reservations are persisted to SQLite before anything is forwarded. Once bytes reach the child, the only automatic outcomes are COMMIT and `IN_DOUBT` — there is no auto-release, because a JSON-RPC error does not prove the request was rejected *before* provider execution (the child can fail after dispatching the HTTP call). COMMIT additionally requires a refund entity matching payment_id AND amount AND the injected receipt AND a non-empty provider-assigned id.

**Ownership is exclusive** (SQLite `locking_mode=EXCLUSIVE`); a second guard process over the same state file is refused, verified by spawning a real second process.

## What was actually built and run this round

Everything below is captured output, not description.

**1. The command package was not in the repository at all.** `.gitignore` contained an unanchored `rzp-guard`, which matched the *directory* `cmd/rzp-guard/`:

```
git check-ignore -v cmd/rzp-guard/main.go
.gitignore:11:rzp-guard    cmd/rzp-guard/main.go
```

Fixed by root-anchoring. Verified in a real clone (`git clone . /tmp/cleanclone`): sources present, `go build ./...` OK, all unit packages pass, and the live gate passes once a `.env` is supplied.

**2. The live gate's control was printed, not enforced.** It exited 0 whenever the blocked call was merely absent from the child tee — which also passes against a dead container or invalid credentials. A new `cmd/gate-verify` parses the captured JSON and asserts every condition. Positive run against the real container:

```
Live block gate
  [PASS] unauthorized create_refund never written to child stdin (found 0)
  [PASS] blocked response is flagged isError
  [PASS] blocked response carries the deciding rule NO_AUTHORIZED_ACTION
  [PASS] CONTROL: real container produced a response for the allowed read id 4
  [PASS] CONTROL: read response is a success, not a tool error
  [PASS] CONTROL: response carries an "entity" field, so it came from the API
  [PASS] CONTROL: at least one tools/call was genuinely forwarded to the child (1)
all assertions passed
```

Negative control, same gate, wrong secret:

```
  [FAIL] CONTROL: read response is a success, not a tool error
  [FAIL] CONTROL: read response body is a JSON entity from the Razorpay API
2 assertion(s) failed        gate-verify exit=1
```

**3. The process did not actually terminate on child death or signal.** Cleanup marked refunds `IN_DOUBT` correctly, but `PumpAgent` ran on the main goroutine, so main stayed blocked on agent stdin. Measured with stdin deliberately held open and the child exiting instantly:

```
PRE-FIX  guard exited after 30s   (cleanup DID fire immediately)
POST-FIX guard exited after 0s
```

The 30s was bounded only by the test's own feeder; with a real client holding the stream it is unbounded. An earlier recovery test masked this because its feeder eventually closed stdin. Both pumps now run under a supervisor returning on whichever boundary fires first. Three build-tagged tests lock it in, all holding stdin open.

**4. Child failure was discarded.** `child.Wait()`'s error was ignored, so the CLI could exit zero after the pinned container crashed or had credentials rejected. Now:

```
child MCP server exited: exit status 3      guard exit=1
```

A child exit that *follows* our own shutdown is still not reported as failure (`parentInitiated`).

**5. Recovery across process death**, verified end to end:

```
forwarded: 1   replies: 0
child stdout closed with 1 unresolved refund(s); marked IN_DOUBT [rfa_demo_001]
RESTART → BLOCKED [ACTION_CONSUMED]: ... (rfa_demo_001=IN_DOUBT); treated as a replay
```

**6. Vendor findings.** Three documented ways to narrow the child's toolset are all broken in the pinned image: the `TOOLSETS` env var cannot express a list (viper does not split env strings, so the server exits with `toolset payments,orders,refunds does not exist`); appended CLI args are silently swallowed because the entrypoint is `sh -c` with a fixed command string (a deliberately bogus toolset produced **no error and all 41 tools** — that control is what exposed it); and the entrypoint offers `--config`, which the binary in the same image rejects with `unknown flag: --config`. The fix is an entrypoint override invoking the same unmodified binary with its own documented flag.

**Test counts:** 77 unit tests plus 3 lifecycle tests, race-clean under `go test -race` in a `golang:1.26` container. Every fix in this project is mutation-verified: the protection is removed, the test is confirmed to fail, and the protection is restored.

## What I am least confident about — attack these

1. **Automatic COMMIT is an unverified compatibility guess.** The expected refund-entity shape comes from Razorpay's *documentation*; no live Test Mode success envelope has ever been captured, because that requires a real captured payment via Checkout. The only passing success fixture is hand-authored by me. The failure mode is fail-closed (unrecognised shape → `IN_DOUBT`), but the success path itself is not demonstrated. **Is fail-closed actually sufficient here, or does an unverified success predicate create a subtler problem I am not seeing?**

2. **The `IN_DOUBT` workflow may be operationally unusable.** Every ambiguous outcome — including a Razorpay error that clearly indicates pre-execution rejection, e.g. the real captured `creating refund failed: The id provided does not exist` — locks the action until a human resolves it. On a busy merchant this could mean constant operator intervention. I chose safety over usability deliberately. **Is that the right trade, and at what volume does it stop being defensible?**

3. **The operator console is a library boundary, not an interface.** Token-gated and durably audited, with no path to it from the relay, but no command or authenticated endpoint invokes it. So the recovery workflow the design depends on has no user-facing half.

4. **No detector metric exists, and I claim none.** The conformance corpus cannot supply one: its labels are computed from the same predicate the policy matches on, so scoring it measures conformance to the spec, not detection. The real measurement needs agent traces with intent specified independently of the mandate, and it has not been run. **Given roughly a week left, is it better to run a small honest agent-trace study, or to ship with no metric and say so plainly?**

5. **Single-instance ownership is enforced by a SQLite exclusive lock.** Cumulative caps are per state file. I have verified a second *process* is refused, but not behaviour under a network filesystem, a container restart with the file still locked, or a stale lock after an unclean kill.

6. **The guard trusts the mandate file completely.** It is loaded from a path at launch and nothing over JSON-RPC can replace it, but there is no signing, no issuance flow, and no revocation.

## What to attack

Find the bug. Specifically:

- Can an agent corrupt refund lifecycle state, bypass the cumulative cap, or get a refund forwarded that the mandate does not authorize? Trace a concrete sequence.
- Is there a state-machine transition, concurrency interleaving, or partial-failure path that ends in a wrong COMMIT or a wrongly released authorization?
- Is any claim above stronger than its evidence supports? Prior rounds caught exactly this repeatedly — "every outstanding request is tracked" was false, "SQLite added" described something not in `go.mod`, "unique across mandates" described a 48-bit truncated hash.
- Is the scope narrowing itself a dodge? One tool, one action, no metric — is this rigour or is it hiding?

Be specific and concrete. Name the file, the sequence, and the consequence. If you think something is fine, say so briefly and move on to what is not.
