# FAILURES.md

Real things that broke, with the actual output, the wrong hypothesis chased first, and the fix. Nothing here is manufactured for the writeup — every entry cost time or a claim.

---

## F1 — Six authorization defects in the Python prototype, found by review, confirmed by running them

**Found:** 2026-08-24, cross-model review round 5. All six were claimed as defects; I reproduced each before accepting it. All six were real.

These are now **required test cases for the Go implementation.** The port fails if it reproduces any of them.

### F1.a — Fractional amount authorized as its truncation, forwarded as the original *(the serious one)*

`policy.py` accepted `int | float`, matched and reserved on `int(amount)`, then forwarded `arguments` unchanged.

```
allowed=True  authorized_against=50000  forwarded_amount=50000.9
reserved=50000  -> AUTHORIZATION GAP: True
```

The proxy authorized one amount and forwarded a different one. On a money path that is the whole failure mode this project exists to prevent, sitting inside the component meant to prevent it. **The authorized amount and the forwarded amount were never asserted equal** — the tests checked `allowed`, `matched_action_id` and `receipt`, but never that the bytes going to Razorpay matched what was approved.

**Go requirement:** accept only JSON integers; reject fractions outright; assert forwarded amount is byte-identical to the authorized amount.

### F1.b — `isinstance(True, int)` is `True` in Python, so booleans passed the type gate

```
amount=True -> rule=AMOUNT_NOT_AUTHORIZED  (int(True)==1, treated as a number, not malformed input)
```

It denied, so it looked safe — but for the wrong reason, via a path that treats `True` as the integer 1. A mandate authorizing 1 paise would have admitted `amount: true`.

**Go requirement:** JSON booleans are not numbers; reject as malformed.

### F1.c — Non-finite amounts crash the guard

```
amount=nan -> ValueError: cannot convert float NaN to integer  <-- UNHANDLED
amount=inf -> OverflowError: cannot convert float infinity to integer  <-- UNHANDLED
```

An unhandled exception inside the decision path. In a relay this is a denial-of-service against the merchant's own agent, and depending on placement could drop the guard while leaving the child running.

**Go requirement:** reject non-finite values before any conversion; no panic path in `decide`.

### F1.d + F1.e — The receipt floor was documented, tested against one example, and not implemented

`receipt_for` was asserted only against `rfa_001`. Against real inputs:

```
action_id='a'        receipt='rzpg_a'         len=6   >=10:False
action_id='x1'       receipt='rzpg_x1'        len=7   >=10:False
action_id='rfa 001'  receipt='rzpg_rfa 001'   len=12  charset_ok:False
action_id='rfa/001'  receipt='rzpg_rfa/001'   len=12  charset_ok:False
```

And the mandate model happily accepted the input that produces them:

```
accepted action_id='rfa 001/x' -> receipt='rzpg_rfa 001/x'
```

`action_id` carried only `min_length=1` — no charset, no length floor. So `rzpg_` + one character is six characters against a documented ten-character minimum, and spaces and slashes pass through into a provider-side correlation key.

**The wrong hypothesis:** I had "verified" the receipt format by testing the constant I happened to use in the example. Testing a worked example is not testing a guarantee.

**Go requirement:** validate the **final generated receipt** — length and charset — not the input; constrain `action_id` at parse time; and make ids unique enough for provider-side correlation, not merely unique within one in-memory mandate.

### F1.f — "Operator-only" resolution was a comment, not a boundary

```
any local code called resolve_in_doubt -> state=AVAILABLE, encumbered=0
IN_DOUBT was cleared with no authentication and no audit trail.
```

`resolve_in_doubt` was a public method. The test asserting the boundary checked only that no method named `reconcile` existed — it proved the absence of a name, not the presence of a boundary.

**Go requirement:** state transitions unexported from the relay package; operator resolution only via a separate authenticated local command path, and every resolution writes an audit record.

---

## F2 — In-memory fail-closed is not fail-closed

Reserved budget, consumed actions and `IN_DOUBT` existed only in process memory. A crash or restart resets the authorization boundary: the same mandate replays, consumed actions come back, and the cumulative cap is bypassed.

The irony is specific — the design's central safety property is *"IN_DOUBT stays locked until an operator resolves it,"* and that property did not survive `Ctrl-C`.

**Fix:** durable local SQLite for mandates, action lifecycle, receipts and decision records. This is the one added dependency the design admits, and it is justified by a live gate rather than by taste: `IN_DOUBT` must still be locked after a restart.

---

## F3 — I called two sequential function calls a concurrency test

`test_concurrent_duplicates_only_one_reserves` makes two sequential `decide()` calls in one thread. The ledger has no synchronization at all. The test proved the *action-consumption* logic, then took credit for atomicity it never exercised.

**Fix:** in Go, a mutex around the match/reserve/rate-limit critical section, and a genuine parallel test under `go test -race ./...`.

---

## F4 — I claimed a contradiction in Razorpay's documentation that did not exist

I asserted that Razorpay's `create-normal` and `normal-refunds-idempotent` pages contradicted each other on `receipt` semantics.

They do not. `X-Refund-Idempotency` is a *retry mechanism*; `receipt` is a *uniqueness constraint*. Orthogonal, and compatible.

**Root cause:** `WebFetch` answers prompts against a page **using a small fast model**, so its output is a paraphrase. I treated two paraphrases as source text, diffed them, and escalated the difference into a claim about the vendor's docs — while writing the document whose hard constraints forbid exactly that.

**Fix:** WebFetch output can point at behaviour worth verifying; it cannot support a claim about what a document says. API-semantics claims now come from runtime observation only (gate G1.6).

---

## F5 — The evaluation corpus measured conformance and I called it detection

The corpus labelled calls with the reason *"no authorized action exists"* — **which is the policy matcher's own predicate.** Scoring against it asks whether the implementation agrees with its own capability list.

I had introduced the "independent oracle" framing two rounds earlier *as the fix for circularity*, then shipped it. A human reading *(mandate + transcript)* computes the same function the matcher computes: independent of the implementation, not of the design.

**Fix:** corpus downgraded to a policy-conformance suite ([PREREGISTRATION.md Amendment 1](PREREGISTRATION.md)); headline denominator narrowed to `create_refund` only; confidence intervals abandoned as pseudoreplication over template replicas; the real metric moved to an agent-trace study with ground truth from an independently authored task brief.

---

## F6 — A corpus template that tested nothing

`A2e` was named for a rate-limit breach. Every target had no authorized action, so **action matching denied first and the rate limiter was never reached.**

**Fix:** rewritten so all prior controls pass — 14 authorized refunds, deliberate cumulative headroom, 14 calls in a 23-second window against a limit of 10 — so the rate limit alone decides. Verified after regeneration. Standing requirement adopted: every named control needs a scenario where all prior controls pass and it alone determines the result.

---

## F7 — `create_payout` does not exist

The build brief specified a proxy over money-moving tools "like `capture_payment`, `create_payout`, `create_refund`, `create_instant_settlement`."

```
grep -rn "create_payout\|CreatePayout" --include=*.go .   ->   zero hits
```

`pkg/razorpay/tools.go:72-76` registers the payouts toolset with `AddReadTools` only. RazorpayX payouts are unreachable as a write path through this server. Found on day one by reading the source instead of the README prose.

**Fix:** scope rebuilt on refunds, which are reachable, irreversible, and exercisable in Test Mode.

---

## F8 — Docker was installed but not running

```
docker --version -> Docker version 29.7.2
docker pull razorpay/mcp -> failed to connect to the docker API at
npipe:////./pipe/dockerDesktopLinuxEngine: The system cannot find the file specified.
```

A present CLI is not a running daemon. Cost: every Phase 1 live gate is still unmet, and "sits in front of the official server" remains a design claim rather than a demonstrated one.

---

## F9 — Windows Application Control intermittently blocks freshly built test binaries

```
fork/exec C:\Users\harsh\AppData\Local\Temp\go-build3699973873\b175\relay.test.exe:
An Application Control policy has blocked this file.
```

Non-deterministic: the same package passed, then failed, then passed. Moving `GOTMPDIR` into the project directory helped once and then failed again, so it is a reputation/scan delay on newly-created executables rather than a path rule.

**The wrong hypothesis:** I first read it as a build failure and ran `go clean -testcache`, which of course changed nothing — the binary compiles fine, it just cannot be executed.

**Fix:** the canonical test runner is now the `golang:1.26` container. It bypasses Windows Application Control entirely, and it is already where `-race` has to run because the host has no C toolchain (`CGO_ENABLED=0`). One command, one environment, reproducible for a reviewer:

```bash
docker run --rm -v "$(pwd -W)":/src -w /src golang:1.26 go test -race ./...
```

Worth noting for the writeup: this is the second environment constraint that pushed work into the container, after the missing C toolchain. Neither was visible from the plan.

---

## F10 — Three documented ways to narrow the child's toolset, all three broken

The design claims defence in depth: the child container is constrained to `payments,orders,refunds`, and `rzp-guard` narrows further to reads plus `create_refund`. The first half turned out to be much harder than reading the README suggested.

### Attempt 1 — the documented `TOOLSETS` environment variable

```
2026/08/24 16:41:06 failed to run stdio server: failed to create server:
failed to create toolsets: toolset payments,orders,refunds does not exist
```

`cmd/razorpay-mcp-server/stdio.go:51` reads `viper.GetStringSlice("toolsets")`. Viper does **not** split an environment string into a slice, so `"payments,orders,refunds"` arrives as one element and is looked up as a single toolset name.

Verified by measurement, not inference — single values work and compose the counts you would expect:

```
TOOLSETS=payments  -> 9 tools
TOOLSETS=refunds   -> 6 tools
TOOLSETS=payments,orders,refunds -> "does not exist"
```

So the documented env var can express exactly one toolset.

### Attempt 2 — the CLI flag, appended to `docker run`

This appeared to work and did not:

```
docker run ... razorpay/mcp stdio --toolsets payments,orders,refunds   -> 41 tools
docker run ... razorpay/mcp stdio --toolsets definitely_not_a_toolset  -> 41 tools, no error
```

A bogus toolset producing no error was the tell. 41 is the *entire* surface, which `EnableToolsets` returns when the name list is empty. The arguments never reached the binary:

```
Entrypoint=[sh -c ./razorpay-mcp-server stdio --key ${RAZORPAY_KEY_ID} ...]
```

The entrypoint is `sh -c` with a **fixed command string**. Anything appended becomes a positional parameter to that shell (`$0`, `$1`, …) and is silently discarded.

**The wrong hypothesis:** I first assumed the flag had worked and that 20-ish tools was simply wrong about which toolsets existed. Testing a deliberately invalid value is what exposed it — a control I would not have run if the first result had looked plausible.

### Attempt 3 — the `CONFIG` file the entrypoint offers

The entrypoint contains `${CONFIG:+--config ${CONFIG}}`, so a mounted YAML file with a real list should work:

```
Error: unknown flag: --config
```

The entrypoint offers a flag **the binary in the same image does not support**. Its own help output lists only `--key`, `--secret`, `--log-file`, `--read-only`, `--toolsets`. Entrypoint and binary are out of sync in the published image.

### The fix

Override the entrypoint to invoke the *same unmodified binary* in the *same pinned image* with its own documented flag:

```
docker run --rm -i --entrypoint ./razorpay-mcp-server \
  -e RAZORPAY_KEY_ID -e RAZORPAY_KEY_SECRET <digest> stdio --toolsets payments,orders,refunds
```

```
41 -> 20 tools
create_instant_settlement: 0
create_payment_link:       0
create_refund:             1
fetch_payment:             1
```

This is not a fork and not a modification: same image, same binary, its own public CLI. It is also **strictly safer than the stock entrypoint**, which places the API key and secret in the container's argv where any process listing can read them; the override passes them in the environment instead.

**What it cost:** the first live run of the executable failed outright, and the honest first reaction was to assume my own flag plumbing was wrong. It was not — three separate vendor-side breakages stacked on top of each other, and only measuring tool counts against known-good single values untangled them.

**What it changes about the claim:** `initiate_payment` is still exposed by the child even at 20 tools, because it lives in the `payments` toolset alongside the reads the mandate needs. That is precisely why `rzp-guard` keeps its own build-level allowlist rather than trusting the child's configuration — the two boundaries are independent, and this is the concrete reason.
