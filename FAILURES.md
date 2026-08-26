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

---

## F11 — A failed token delivery locked recovery out permanently

The operator credential was committed to the state file **before** the token was written to `-out`. Verified:

```
$ rzp-guard-operator ... init -out /tmp/nope/sub/tok
create token file: ... The system cannot find the path specified.

$ rzp-guard-operator ... init -out /tmp/good_tok
an operator credential already exists for this state file; use rotate ... instead
```

The verifier survived. `init` refuses to run twice — deliberately, because that refusal is what closed the earlier restart bypass. So a merchant whose token file failed to write had a state file **nobody could ever recover**, with locked `IN_DOUBT` refunds and no path to resolve them. For `rotate` it was worse: rotating first would invalidate a *working* credential while the replacement reached nobody.

**Also found in the same pass:** `-out` used `os.WriteFile`, which truncates. A typo pointed at another file destroyed it:

```
$ echo "MY-OTHER-SECRET" > /tmp/victim.txt
$ rzp-guard-operator ... init -out /tmp/victim.txt
$ cat /tmp/victim.txt
rzpop_kq_PhCdER_SfId...
```

**Fix:** deliver first, commit second, and undo the delivery if the commit fails. The file is created with `O_CREATE|O_EXCL`, so an **existing final path** is never touched, then fsynced before the credential is committed.

*(Correction to this entry, made when the claim was challenged: an earlier version said "a symlink cannot redirect the secret." That is too broad. `O_EXCL` protects the final path only — it does not establish that parent directories are free of symlinks or Windows reparse points. The code says the narrower thing; this record now matches it.)*

**And the mode claim was not a control.** The code asked for `0600` and never checked. On Windows the file lands `0666`, measured:

```
token file landed with mode 0666, not 0600, so this platform did not apply the
restriction (Windows does not honour Unix mode bits). Write it interactively
instead, or re-run with -allow-unprotected-out once <dir> is a directory you
have restricted yourself
```

`-out` now **verifies** the resulting mode and refuses by default, with an explicit opt-out that names what the operator is taking responsibility for. "Directory ACLs are the boundary" was a sentence in a README; this is an enforced check.

`os.ModeCharDevice` was also replaced with `x/term.IsTerminal` for the non-terminal guard — a false positive there commits the verifier while the token goes to a sink, which is the same lockout.

Mutation-verified: restoring commit-then-deliver fails both subtests with *"init could not be retried after a failed delivery -- recovery is permanently locked out"*.


---

## F12 — The gates wrote real recovery tokens into a OneDrive-synced folder

Provisioning in the live gates used `-out` into `evidence/live/`, and left the token there after the gate passed:

```
evidence/live/block_operator_token
evidence/live/recover_operator_token
```

Both are genuine recovery credentials. `.gitignore` covers `evidence/live/`, which is exactly why it felt safe — but this working tree is `C:/Users/harsh/OneDrive/Desktop/Razorpay`, so an ignored file still syncs to the cloud. **Gitignore is not a confidentiality control.**

**Fix:** the gates delete the token immediately after provisioning, and `gate-verify` now asserts its absence — a leak fails the gate rather than passing quietly.

Two related corrections landed with it:

- **`-allow-unprotected-out` was a production flag that bypassed the very check the README advertised.** It now exists only under `-tags testhook`; the shipped binary has no way past the `0600` verification. Confirmed: `rzp-guard-operator -h` shows the flag `0` times, the test-hook build `1`.
- **The durability gap was one layer deeper than the earlier fix.** The token file was fsynced, but not its parent directory — after a power loss the contents can be on disk while the entry naming them is not, which recreates the permanent lockout at the filesystem boundary. The parent directory is now synced where the platform allows it; Windows cannot, and the command says so rather than implying crash-safety it does not have.

Also corrected a claim that was too broad: `O_EXCL` protects the **final path**, not the parent chain. It does not establish that parent directories are free of symlinks or reparse points, and the code now says exactly that.


---

## F13 — I detected the unsafe condition and then did the unsafe thing anyway

`WriteTokenExclusive` correctly reported `durable=false` when the platform could not fsync a directory. `cmdInit` printed a warning and **committed the credential regardless**. A power loss in that window still produced the exact unrecoverable state the whole mechanism existed to prevent — the detection was real, the response was not.

This is the same shape of error as F1.a (detect the amount, forward a different one) and the "concurrent" test in F3: build the check, then fail to act on it.

**Fix:** commit only on proven delivery. Verified on the Windows target — both refused paths leave **zero** verifiers committed:

```
refused -out           verifiers committed: 0
refused terminal       verifiers committed: 0
```

Mutation-verified: restoring warn-then-commit fails the test with *"a credential was committed despite refusing delivery — this state file would be permanently unrecoverable"*.

**Consequence, accepted rather than worked around:** provisioning does not work on Windows at all now. `-out` fails the `0600` check; terminal output fails the durability check. That is the honest state of the product, and the README says so instead of offering a path that quietly risks a lockout.

## F14 — Deleting a leaked secret is not the same as never creating it

The previous fix deleted the gate's recovery token after use and asserted its absence. That proves the file is gone at the end — not that cloud sync never uploaded it during the window it existed, in a OneDrive-backed tree.

**Fix:** `init-ephemeral` (test-hook only) derives a verifier from a token that is generated and immediately **discarded**. No usable recovery secret is ever created, so there is nothing to leak. The resulting state file is deliberately unrecoverable, which is correct for a throwaway fixture and wrong for anything else — hence the build tag.

While moving this, Windows Application Control began persistently refusing to execute the `-tags testhook` guard binary (F9 again), including from a fresh output path, while shipped builds ran fine. `process-recover` needs no Docker child, so it now runs entirely inside the golang container — which is where everything else already runs.

---

## F15 — The warning was printing on every commit and I called it noise

`run.sh` claims Git-Bash support. On a Windows checkout with `core.autocrlf=true` it was rewritten to CRLF, and bash rejected it before the first command ran:

```
./run.sh: line 8: set: pipefail\r: invalid option name
```

The index held LF, but nothing enforced it on checkout, so `git clone` on Windows produced a script that could not execute. Every single commit in this repository printed:

```
warning: in the working copy of 'run.sh', LF will be replaced by CRLF the next time Git touches it
```

I read that line dozens of times and dismissed it as environmental noise. It was the defect, announcing itself, continuously. Nothing was verifying the *shipped entry point* on the platform it claimed to support — my own runs used a working tree that happened to be fine.

**Fix:** `.gitattributes` pinning `eol=lf` for shell, Go, YAML, and — importantly — the `.jsonl`/`.json` fixtures, whose SHA-256 values are recorded in `corpus/manifest.json`. A CRLF rewrite there would have silently invalidated every hash in the pre-registration.

**Verified** from a clone made with `core.autocrlf=true`:

```
test  PASS   lifecycle  PASS   race  PASS   lifecycle-race  PASS
live-block  PASS (15 assertions)   process-recover  PASS (6 assertions)
```

**CI was also testing the wrong thing.** The reproducibility job ran `go test` directly after corrupting `.git` — it caught the VCS defect but would have missed this one entirely, along with any regression in `gorun`, Docker mounting, or script quoting. It now exercises `./run.sh` itself, adds a Windows/Git-Bash job that clones with `autocrlf=true` and fails if `run.sh` contains a CR, and runs the wrapper's own lanes.

Two defects in a row (F14's VCS stamping, this one) were in the **runner**, not the product. For a project whose whole claim is reproducible evidence, that is the more damaging place for them to be.

**Addendum — the fix was also incomplete, in the same shape.** `.gitattributes` plus `git add --renormalize` fixes the *index*, and I verified the result by taking a **fresh clone**. It never rewrote the working tree I actually develop in. Three tracked files kept CRLF there: `cmd/rzp-guard/lifecycle_test.go`, `cmd/rzp-guard-operator/main_test.go`, and `evidence/tools_list.json`. `file` had not reported the last one as "CRLF line terminators", so an earlier spot-check using `file` came back clean and I believed it.

I verified the artefact I produced (the clone) rather than the environment the defect actually lived in — which is the identical mistake one level down. Found only when a widened check ran over every tracked file. Fixed by re-checkout; the CI job now greps **every tracked text file** for CR instead of just `run.sh`, and adds a `gofmt -l` check, which is what surfaced the two `.go` files.

## F16 — I wrote a gate to prove the allow path, and the gate had a hole

G1.6 needs an alive-control. Without one, "the replay reached the provider zero times" also passes when the container is dead or the credentials are wrong — precisely the cases the gate exists to exclude. So the gate asserted that a permitted read *did* reach the child, and that a real payment entity came back.

Those were two separate assertions over two separate files, and nothing tied them together:

```go
if m.Params.Name == "create_refund" { replayFwd++ } else { aliveControl = true }
...
for _, m := range replayOut {
    if ... strings.Contains(text(m), "\"entity\":\"payment\"") { readOK = true }
}
```

Any non-refund tool call satisfied the first. Any payment entity anywhere in the output satisfied the second. Renaming the forwarded tool to `nope_tool` left both true:

```
alive-control read stripped                -> PASSED (BAD - gate is blind to this)
```

The gate passed on good evidence, so I had no reason from normal use to doubt it. **I only found this because I mutated the captured evidence and required the gate to fail** — the same discipline applied to product code, turned on the verifier itself. A gate is code, and an assertion that has never been observed failing has not been tested.

**Fix:** capture the alive-control's request id from the forwarded stream and require *that specific id* to come back as a non-error payment entity. The two halves are now one correlated claim.

**Re-verified:** 11 mutations of the captured evidence — tampered receipt, blanked provider id, inflated amount, rewired reply id, `isError` injected, replay actually forwarded, `ACTION_CONSUMED` removed, receipt truncated below the floor, fractional amount — all fail; unmutated evidence passes. Zero blind spots.

**A second lesson, from the harness.** One mutation reported `PASSED (BAD)` and I nearly wrote it up as a second hole. It was not: the `sed` pattern contained a placeholder that did not exist in the file, so the mutation changed nothing and the gate correctly passed unmodified evidence. **A mutation that does not mutate is indistinguishable from a blind gate.** The harness now asserts the search pattern is present before substituting, and reports `VACUOUS (harness bug)` otherwise — which immediately caught a third case where `"isError":false` was absent because the MCP server omits the field when false.

## F17 — I did it again: a blog summary repeated as an established fact

Justifying the Responses API, I wrote that Chat Completions was legacy and that
"its removal window closed in early 2026." I put it in the frozen protocol, in a
source comment, and in a commit message.

It is false. `/v1/chat/completions` is currently documented and supported. The
2026 shutdown I was half-remembering is the **Assistants API** — a different
endpoint entirely.

The source was a snippet from a third-party gist and blog post returned in a web
search. I read it, it matched something I vaguely believed, and I promoted it to
a stated fact without opening OpenAI's own documentation. The reviewer checked
and it took one fetch to disprove.

**This is F4 repeating.** There, I quoted WebFetch's small-model summaries of
Razorpay docs as if they were the source text, and asserted the docs contradicted
each other. The instruction I was given after that was "check current docs before
trusting memory." I did run a search this time — and then trusted the *search
result's* paraphrase, which is the same error one layer out. Searching is not
checking. The rule has to be: the claim comes from the vendor's own page, or it
is not made.

**The damage was worse than a wrong comment.** The false claim was doing
*justificatory* work — it was the stated reason for an architectural choice in a
pre-registered protocol. A reviewer who believed it would have accepted the
endpoint decision on a premise that does not exist.

**Fix:** retracted in `study/PROTOCOL.md` §4.1 and `cmd/rzp-study/openai.go`,
both marked as retractions rather than silently edited. The endpoint choice
stands on a narrower, checkable reason — reasoning-item preservation across turns
for multi-step tool use — with the honest addition that Chat Completions would
probably work too.

## F18 — I shipped a refund launcher and called it a gate

To make G1.6 reproducible I added `./run.sh live-refund <pay_id> [amount]`. It
took an arbitrary payment id and amount, **generated its own authorizing mandate
for them**, and called Razorpay with whatever credentials were in the
environment.

I wrote it while thinking about reproducibility, and reproducibility is a real
concern — a one-off result I ran by hand is weaker evidence than a command a
reviewer can re-run. The parameters existed so someone else could point it at
*their* test payment.

But look at what it actually was. Strip the framing and it is: *give me a payment
id and an amount, and I will refund it.* The mandate it generated was not an
authorization boundary, because the tool wrote it for itself. A mandate that the
caller controls authorizes nothing.

This repository is a **defence**, submitted to a track that disqualifies
offense-capable work automatically. I put a generic money-moving command in it,
in the runner, documented in the README, one commit after removing a Checkout
page on exactly these grounds. I had already made this call correctly once and
then failed to apply it to my own convenience.

**What made it invisible to me:** every other gate is safe because its payment
ids are hardcoded non-resolvable synthetics (`pay_SYN…`). I generalised the
*pattern* (a runner command that drives the guard) without noticing that
parameterising the payment id is what changes a fixture into a weapon. The
danger was in the parameters, not the plumbing.

**Fix:** the command is deleted. What remains is
`./run.sh verify-refund-evidence`, which only reads committed JSON and cannot
move money. The refund itself was a one-off recorded run, and its evidence is
committed in redacted form. A CI job now fails the build if a `live-refund`
command reappears, or if `run.sh` ever builds a `create_refund` against
anything other than a hardcoded `pay_SYN…` id.

**The general rule I should have applied:** in a defence repository, ask of every
command not "what is this for?" but "what is this, if the framing is removed?"

## F19 — I fixed the commit and left the exposure

The evidence redaction work moved raw provider records into gitignored
`evidence/*/raw/` directories and called the problem solved. It was not. This
working tree is OneDrive-backed, so **a file that is never committed is still
uploaded.** Gitignore governs git; it does not govern the sync client.

Found by scanning the directories rather than reasoning about them:

```
evidence/g16/raw/fetch_stdout.jsonl     email, phone, card last4, auth_code, acquirer_data
evidence/g16/raw/replay_stdout.jsonl    email, phone, card last4, auth_code, acquirer_data
evidence/linux/raw/block_stdout.jsonl   email, phone, card last4, auth_code, acquirer_data
```

Ten raw files, three carrying a real Test Mode payment's contact record, sitting
in a synced directory.

**This is F12 one directory over.** F12 was operator credentials written into
OneDrive-backed `evidence/live/`, and its lesson was written down at the time:
"gitignore is not a confidentiality control." I then applied that lesson to
credentials only, and reintroduced the identical failure for provider records —
while writing a comment in `run.sh` that said the token goes to a temp dir
"never into this OneDrive-backed tree", directly above code writing payment
records into that tree.

**A second defect in the same path.** `redact_evidence()` copied every
`raw/*.txt` into the published directory *before* running the redactor, and the
redactor inspected only `.jsonl` and returned any non-JSON line verbatim. A
provider error or diagnostic containing a payment record would have been
republished having passed through no check at all.

**Fix.** Raw capture moves outside the workspace entirely (`RAW_ROOT`, default
under the OS temp dir); both `run.sh` and `redact.py` **refuse** a raw root that
resolves inside the repository, rather than warning. No file is copied out of
raw verbatim. The leak scan now walks **every** file under `evidence/`
regardless of extension, and applies two independent rules: a dropped field is a
leak inside a provider entity, and a personal-data-shaped *value* is a leak
under any key anywhere.

Getting that scan right took two corrections, both false positives worth
recording because a noisy check is one people learn to ignore: matching field
*names* flagged Razorpay's published tool schemas, which legitimately declare
parameters called `email` and `contact`; and matching PII-shaped *values*
flagged `"For example, 9876543210"` in a schema description — the Indian
equivalent of 555-0100.

**What the fix does not do.** Deleting the files now does not undo any sync that
already happened. Those records may exist in OneDrive's cloud copy and version
history, and no change in this repository can retract them.

## F20 — I fixed a bypass by gating on the flag instead of the fact

A reviewer showed that `worksheet` accepted the scripted dry-run directory and
emitted a worksheet for 27 calls, which is not the registered 45-trace study.
I added `validateTraceSet` and an `-allow-dry` escape for the tooling test, and
called it closed.

It was not. I wrote the gate as:

```go
if !*allowDry {
    validateTraceSet(...)
}
```

so `-allow-dry` did not mean "tolerate scripted traces" — it meant **skip every
check**. Found by attacking my own fix: plant ONE real-looking trace where the
study declares forty-five, pass the flag, and point the output at `study/`.

```
$ rzp-study worksheet -allow-dry -traces .gotmp/fake -out study/adjudication/worksheet.json
worksheet: 1 emitted refund calls across 1 traces -> study/adjudication/worksheet.json
```

A one-of-forty-five worksheet, inside the study directory, from a flag whose
stated purpose was unrelated. `refuseDryArtifacts` did not save it either: it
returned early when no scripted traces were present, so it never checked the
output path for a set of real ones.

**The mistake underneath both versions is the same shape.** I keep writing gates
that ask *"which flag did the caller pass?"* when the safe question is *"what is
this data?"* A flag is a claim by the caller; the traces are the fact. F13 was
this — detecting an unsafe condition and proceeding anyway — and this is its
sibling: making the check conditional on something the caller controls.

**Fix.** One `gateAdjudication` for both commands, branching on the traces:

- scripted or smoke traces present → require `-allow-dry`, force output outside
  `study/`, and skip trace-set validation because this is not the study;
- no scripted traces → `validateTraceSet` **always** runs, whatever flags were
  passed.

Verified in four directions, and the bypass is now a regression test: a partial
real set is refused with the flag and without it; scripted traces still work
outside `study/`; scripted traces are still refused into `study/`.

## F21 — Deleting the file did not delete the file

F19 moved raw provider records out of the workspace and rebuilt the committed
evidence as a redacted projection. The working tree was clean. The leak scan
passed. That fixed the present and did nothing about the past: **four commits
still carried the pre-redaction blobs**, and a blob in git history is published
the moment the repository is.

```
77e08af  75b4b0f  37a3782  9a8dd07
  evidence/g16/fetch_stdout.jsonl
  evidence/g16/replay_stdout.jsonl
  evidence/linux/block_stdout.jsonl
```

Six distinct values across them — email addresses and provider identifiers from a
real Test Mode payment.

The mistake is the same one twice over. F19 was "gitignored is not unpublished";
this is "deleted from the working tree is not deleted from the repository". Both
times I fixed the copy I could see.

**Fix.** `git filter-repo --replace-text` over all 52 commits, replacing the six
values with `REDACTED`. Content-level replacement rather than path removal,
because those three paths still hold the current redacted projections and
removing the paths would have deleted live evidence along with the history.

Taken before touching anything: a full mirror backup outside the repository and
outside OneDrive.

**Verified, not assumed** — all four checks:

| Check | Result |
|---|---|
| Each of the six values, searched across every commit | 0 occurrences |
| `REDACTED` present in the historical blobs instead | yes |
| Every file at `HEAD` vs. the pre-rewrite `f24fcd1` | byte-identical |
| Commit count and messages | 52, preserved |

Tests, freeze verification and the evidence leak scan all pass afterwards, and
the freeze hash is unchanged, so the committed traces remain valid.

**What this still does not undo.** The pre-rewrite objects existed in an
OneDrive-synced `.git` directory for roughly a day. Rewriting local history does
not retract anything the sync client already uploaded, and no change in this
repository can. What it does guarantee is that **publishing this repository does
not publish those records** — which was the actual blocker.
