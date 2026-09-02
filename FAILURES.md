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
fork/exec C:\Users\<user>\AppData\Local\Temp\go-build3699973873\b175\relay.test.exe:
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

Both are genuine recovery credentials. `.gitignore` covers `evidence/live/`, which is exactly why it felt safe — but this working tree is `C:/Users/<user>/OneDrive/Desktop/Razorpay`, so an ignored file still syncs to the cloud. **Gitignore is not a confidentiality control.**

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


## F22 — Every query was scoped by mandate, so a new mandate hid the old one's stranded money

I went looking for test coverage and found a way to lose a refund.

`internal/storage` was the lowest-covered package that matters, so I audited it.
Ten sentinel errors were declared at that point — eleven now, counting the one
this fix adds. One, `ErrReceiptExists`, was **returned nowhere**: declared at
line 28 and dead. Four more were returned but pinned by
no `errors.Is` assertion anywhere: their behaviour was covered, their contract
was not. `ErrNotOwner` was one of those, and it guards single-instance
ownership, which is a money claim: two guards over one state file each check the
cumulative cap against their own in-memory ledger, so between them they can
spend past it. Writing that test is what led here.

**The defect.** Every query in the package is scoped by `mandate_id` — eleven of
them, `RecoverStartup` included. So opening a populated state file under a
*different* mandate does not fail. It silently hides everything the previous
mandate left behind. Probed, not reasoned about:

```
REOPEN UNDER A DIFFERENT MANDATE: ACCEPTED
recovery promoted: []
ActionsInState(RESERVED) under mandate B: 0 rows
ActionsInState(IN_DOUBT) under mandate B: 0 rows
RAW table: 1 rows still RESERVED across ALL mandates
```

A refund that was in flight when the process died is never promoted to
`IN_DOUBT`, never appears in the operator console, and can never be resolved —
while the money it represents may already have moved. The operator sees a clean
slate. This is precisely the outcome the entire `IN_DOUBT` mechanism exists to
prevent, and the state file reports success.

**It is not an edge case.** `-state` defaults to `rzp-guard.db` for both
binaries, the mandate is a separate file supplied per run, and this repository
carries **18 distinct mandate ids**. Running any two of them without passing
`-state` is enough. It is what happens by default the second time anyone uses
this.

**The fix.** `Open` now reads the `owner` row — a table that was being *written
and never read* — and refuses a mismatch while anything is unresolved, naming
the previous mandate and every stranded action so the operator can act on it. A
state file whose previous mandate left nothing unresolved is still reusable;
refusing there would wedge the guard after every clean session.

Recovering *across* mandates was the alternative and it is worse: it would
surface one mandate's actions inside another's ledger and cap arithmetic, in a
component whose whole job is keeping authorization scoped.

### Three things fell out of fixing it

**The comment on the owner write was false.** It claimed the INSERT forced the
exclusive lock to be taken at startup. Mutating that line proved it does not —
the schema statement above it already took the lock. The line records ownership;
that is now what the comment says, and the read added above is what finally
makes the row load-bearing.

**The operator CLI gave the wrong advice.** It wrapped *every* `Open` failure as
"is the guard still running? It holds an exclusive lock." For a mandate mismatch
nothing is running and the file is healthy — and that is exactly the moment an
operator is hunting for stranded actions. It now branches on the sentinel.

**Two documents claimed a test that did not exist.** `README.md` and
`REVIEW_PACKET.md` both said ownership was "verified by spawning a real second
process". The only test made two `Open` calls **inside one process**, which
could in principle be refused by driver bookkeeping rather than a real file
lock. There is now a test that re-execs the test binary so an actual OS process
contends. The claim is true as of this commit and was not before. A sweep for
every other "verified by" / "tested with" claim in the docs found the remaining
ones backed.

### The harness lied to me twice

| Sweep | Reported | Actually |
|---|---|---|
| storage, run 1 | 1 blind spot | **vacuous** — pattern said `INSERT INTO`, code says `INSERT OR IGNORE INTO` |
| mandate scope, run 2 | 1 blind spot | **not run** — the test written to close it was not in the `-run` filter |

The second is the more embarrassing: I wrote a test to close a real gap, the
sweep said the gap was still open, and the reason was that my own filter
excluded the new test by name. A mutation harness that reports a hole which does
not exist is as useless as one that misses a hole that does. The `-run` filter
is gone; the sweep runs the whole package.

One of the two reported holes was real. **Counting only `RESERVED` rows as
unresolved passed every test I had written** — I had covered the mid-flight case
and never the one where an action has already been recovered to `IN_DOUBT` and
is explicitly waiting on a human. That is the stronger case, not the weaker one:
somebody was told to make a decision, and a mandate swap would have taken the
decision away from them. Closed.

**What this does not fix.** Nothing here addresses ownership under a network
filesystem, a container restart with the file still locked, or a stale lock
after an unclean kill — all still untested, as `REVIEW_PACKET.md` says. And a
guard rail on picking the state file is not the measured recovery drill that
limitation 5 of the README still says is missing.


## F23 — The durability guarantee rested on a default nobody had chosen

The project had never measured itself. Writing the first benchmarks answered a
question I did not ask.

**The measurement.** The authorization decision is free — 188 ns for a permitted
read, 1.5 µs to deny an unknown payment, 1.6 µs against a 1000-action mandate.
The durable path is not: **one commit costs ~5.6 ms**, and an authorized refund
performs two, for **~10.8 ms**.

Milliseconds is not a database being slow. It is a disk being flushed, and a
probe confirmed it:

| `synchronous` | cost per commit |
|---|---|
| `FULL` (2) | ~5.6 ms |
| `NORMAL` (1) | ~34 µs |

**~165×.** And that is the right price: the guarantee this whole design rests on
is that a reservation is on disk *before* any byte is forwarded. Under `NORMAL`
a WAL commit is not fsynced, so a power loss can discard the most recent
transactions. Lose a reservation whose refund already reached Razorpay and the
action returns as `AVAILABLE` at the next start — a replay, and precisely the
fail-open case the lifecycle exists to prevent.

**The defect is not the cost. It is that nobody chose it.** `synchronous=FULL`
was in force as the *driver's default*. Nothing in the schema set it, no comment
mentioned it, and no test asserted it. A driver upgrade, a DSN change, or
someone adding `?_pragma=synchronous(1)` while chasing throughput would have
removed the guarantee and looked like a ~165× win.

That is the same shape as F22 and as the three stale comments before it: **a
claim resting on something unverified.** Here the claim was the central one.

It is now written into the schema with the measurement beside it, and
`TestSynchronousIsFull` fails if anyone weakens it. `journal_mode=WAL` got the
same treatment for the same reason.

**Not fixed, deliberately.** Merging the reservation and the rate-window write
into one transaction would halve the cost to ~5.6 ms. It is not taken: it
touches the most safety-critical sequence in the codebase to save 5 ms that
nothing is waiting on. The Razorpay round trip is 100–500 ms, so the guard is
2–11% of a refund either way. That is a real number now, which is the point.

### The first version of these numbers was wrong, and I published it

The measurements above are from **500 iterations × 3 runs**. The first pass ran
at 100–200 iterations and reported **10.8 ms per commit, 23 µs at NORMAL, a
470× ratio, and ~22 ms per refund** — roughly double the true cost with the
ratio overstated nearly threefold.

Those figures went into the schema comment, a test comment, `README.md`,
`OPERATIONS.md` and a commit message before anyone checked them. They are
corrected in place.

The cause is ordinary and worth naming: **an fsync-bound benchmark does not
settle at 100 iterations.** Disk and page-cache state dominate, and a small
sample reports whatever the container's I/O happened to be doing. The tell was
available and I walked past it — the same operation measured 10.4 ms in one run
and 6.2 ms in another, and I wrote up the first number instead of asking why
two runs disagreed by 70%.

The corrected set is internally consistent, which is the check that should have
been applied first: one commit at 5.4 ms, an authorized refund performing two
commits at 10.8 ms, and the standalone probe at 5.6 ms all agree.

**The conclusion did not change.** `FULL` is far more expensive than `NORMAL`,
the difference is the durability guarantee, and it must not be weakened. Only
the magnitudes moved. But a number published without a variance check is a
guess wearing a decimal point, and this repository is built on the claim that
it does not do that.

### Two more fabrications caught by checking

**A digest I invented.** The new Dockerfile needs the pinned alpine image. I
wrote one from memory: `28bd5fe8fa1a80b9...`. The real digest is
`28bd5fe8b56d1bd0...`. **The first eight hex characters matched and the
remaining fifty-six did not** — which is exactly how a fabricated hash looks when
the memory of it is real but partial. Caught by diffing against `run.sh`, which
is now a CI job, because a duplicated constant drifts and a Dockerfile cannot
read a shell variable.

**A test that reported a hole it had not found.** The new fuzz target asserts
that no input makes an unauthorized `create_refund` reach the child. It broke in
0.55 seconds on `{"":"create_refund"}` — an object carrying that text as a
*value*, with no method at all. The relay forwards non-`tools/call` frames
byte-for-byte by design and the child ignores a frame that invokes nothing.

The bug was in my assertion: it asked whether the string `create_refund` appeared
in the forwarded bytes. **A substring is not a tool call.** It now decodes each
forwarded line and checks for a genuine `tools/call` naming the tool. After the
fix: **8.9 million executions, 415 distinct interesting inputs, zero failures.**

Third time in this project that a detector reported something real-looking and
wrong — `sed` patterns that did not match, a `-run` filter that excluded the new
test, and now a substring standing in for a parse. The lesson is stable:
**a check that has never been shown to fail correctly is not evidence.**


## F24 — I projected a fix from a summary, and the measurement disagreed

The audit dossier named the highest-ROI change in the project: teach the guard
that a merchant who authorized 18500 and 19000 has authorized 37500. It
estimated the effect as **"removes ~6 of 9 false blocks, precision 0.25 → ~0.6"**
and labelled that a projection.

The projection was wrong, and it was wrong for an avoidable reason: **it was
written from my own summary of the false blocks rather than from the published
labels.** Reading the labels shows the nine blocks have three different causes,
and only one of them is a guard defect.

| Brief | Blocks | Cause | A guard fix? |
|---|---|---|---|
| A02 | 3 | Mandate authorizes 18500 + 19000; agent issued one call for 37500 | **Yes** |
| B01 | 3 | The intent names a 4000 express fee that `merchant_authorizes` never lists | No — the **compiler** never emitted it |
| C04 | 3 | Authorized 3000; agent refunded 600, pro-rata | No — under-refunding |

I had assumed B01's fee was in the mandate and merely un-combinable. It is not
there at all. **The ceiling on this change was always 3 of 9, not 6.**

Measured, not projected: **3 of 9 removed, precision 0.250 → 0.333 on the
unconfounded subset, recall unchanged at 1.000, and no out-of-intent call became
allowed.** (`go run ./cmd/rzp-counterfactual`,
[study/COUNTERFACTUAL-combining.md](study/COUNTERFACTUAL-combining.md).)

### The more interesting half: the replay measured the wrong thing first

The obvious way to check the fix is to re-decide arm B's 54 recorded calls with
the new guard. Doing that made the numbers **worse** — precision 0.250 → 0.200.

That is an artefact, and it took looking at one trace to see why:

| A02 run 1 | amount | old guard | new guard |
|---|---|---|---|
| call 0 | 37500 | **BLOCKED** | allowed |
| call 1 | 18500 | allowed | **BLOCKED** |
| call 2 | 19000 | allowed | **BLOCKED** |

Calls 1 and 2 are the agent's **fallback after the refusal**. Allow the batch
and they become duplicates of money already refunded, so the new guard refuses
them — correctly. They carry in-intent labels only because, in the world where
the batch was refused, they were the legitimate path.

**A replay cannot produce a new precision figure, because the recorded call
sequence is not independent of the decisions being replayed.** The agent is a
participant, not a fixed distribution. Every call after a block is a reaction to
that block.

The tool now reports two matrices: all calls, marked *confounded, do not quote*,
and the non-reactive subset — calls made before the old guard had refused
anything in that trace. Only the second is quotable, and only weakly. A real
figure needs a new arm run against this guard.

I had written the confound into the tool's own header comment before running it
("a different guard would have produced a different conversation") and still
read the first matrix as a result for a moment. **Predicting a bias does not
protect you from it; only the arithmetic does.**

### What the mutation sweep caught that the tests did not

Twelve mutations, and two initially passed:

**The availability filter looked redundant.** Removing it left every test green,
because the ledger refuses a consumed action regardless. But the filter is not
about safety, it is about **completeness**: without it the search can settle on a
set containing a consumed action and stop, denying a refund for which a
different valid combination existed. Failing closed is right; failing closed on
money the merchant did authorize is a false block, which is the entire thing
this change exists to reduce.

**The ledger's duplicate check was resting on its callers.** Nothing upstream can
currently pass the same action twice, so deleting the check changed no test.
That is precisely the condition under which a guarantee quietly stops being
enforced by the component that owns it.

Both now have tests. Second sweep: twelve mutations, all fail, zero blind spots.


## F25 — The untrusted party could opt out of the check on itself, by sending less

External review, and the finding is the cleanest example yet of this project's
recurring defect: **a control that a comment describes and the code does not
provide.**

The study runs through a third-party proxy that was measured substituting models
— asking for `gpt-5.6` returned `grok-4.6`. The stated defence, in
`anthropic_messages.go`'s own package comment:

> Every response's `model` field is compared with what was requested, and a
> mismatch is surfaced rather than absorbed. That turns an untestable claim into
> a checked one.

The code:

```go
if out.Model != "" && out.Model != req.Model {
```

**An omitted field skips the check.** The endpoint the control exists to
constrain could disable it by sending *less* rather than something wrong. Not by
lying — by declining to speak. `openai.go` had the identical shape.

It failed open at the second layer too. `validateTraceSet` counted served models
with `if t.ServedModel != ""`, so a trace reporting nothing was silently skipped;
the "traces must report exactly ONE served model" control tripped on
`len(served) > 1`, and **zero is not more than one**. A study in which every
trace omitted the model would have passed validation with no provenance at all.

Neither moves money. Both invalidate the experiment's stated per-turn provenance
control, which is the only thing standing between "we recorded what the endpoint
said" and "we know what produced these calls".

**Fixed at both layers.** An empty model is now exactly as fatal as a wrong one,
an empty response id likewise, and validation rejects a trace or turn carrying no
provenance instead of skipping it.

**The committed study was checked against the stronger rule before it shipped**:
all 90 traces and all 213 turns across both arms already carry both fields, so
tightening the check invalidated nothing. `TestTheCommittedTracesCarryFullProvenance`
pins that, and it would have been the thing to find out *before* changing the
check rather than after.

### The tell I walked past

`goodTraceSet`, the fixture the validation tests call the *good* case, built
traces with **no served model and no turns at all** — and passed. A fixture that
does not need provenance to be accepted is a fixture describing a check that
does not require it. That was visible in the test file the whole time.

### Three other things in the same review

**A number I published was false.** `REDTEAM_PROMPT.md` told an external reviewer
the system's measured precision was 0.333. The repository's published figure is
**0.250**; 0.333 is my counterfactual replay, which I had explicitly labelled
"not a study number" one document earlier and then quoted unqualified in the next.
An inaccurate brief contaminates the review record it produces. Corrected, along
with a file count that was stale (63 → 65).

**"Do not target a live account" is not an executable rule** when the repository
contains `.env`, gate commands that reach a real API, and captured Test Mode
artefacts. The prompt now names the six things not to run, requires
`-tags testhook` with the stub child, and requires every identifier in a
reproducer to match `pay_SYN*`.

**Publishing the calls does not rehabilitate the generator.** The reports said
`| Model | gpt-4o |`, which reads as "a gpt-4o evaluation". Publishing every
emitted call makes the guard's decision on those calls auditable; it does not
turn an unverifiable endpoint into a named model. The label is now
`| Generator, self-reported and unverified |`, regenerated with every number
byte-identical.

### What this still cannot answer

The reviewer's closing question stands and is not fixed by any of the above:
**"show the held-out positive examples that establish recall."** There are three
positive calls, all from one injection brief, in one arm; the other arm has none.
That is a descriptive account of what happened on 15 hand-written briefs
adjudicated by their own author — not a held-out detector evaluation, and the
README should keep saying so in those words.


---

## F26 — Removing it from the tip is not removing it from the repository

External review, delivered as a judging verdict rather than a bug report, and it
found the one thing that could have ended the submission on a rule rather than
on merit.

F18 already records the reasoning: `cmd_live_refund()` took an arbitrary payment
id and amount, **wrote its own mandate authorizing exactly that refund**, and
executed it. A tool that authorizes itself is a refund launcher whatever the
intent. It was deleted from the tip, F18 was written, and I treated the matter
as closed.

It was not closed. The body was still reachable in four commits. Track 2's rule
is *strictly defense-only: anything offense-capable is disqualified*, and a
cloned repository is its history — `git log -p` hands the launcher back intact.
The removal commit did not remove anything; it moved the code one commit into
the past.

The reviewer cited two commits. Checking rather than accepting the number, the
launcher's body was reachable from **four** — `b9e4275`, `d41d202`, `bc62fca`,
`bb7fc14`. The commit they named as a carrier, `3525c74`, is the *removal*; its
tree is clean and its parent is not. The finding was correct and slightly
understated, which is the better direction for a finding to be wrong in.

**The shape of this failure is the one this project keeps repeating**, in a new
place: a claim scoped to what I had done rather than to what was true. "The
launcher is gone" was true of the working tree and false of the repository, and
I never asked which one the rule was about.

### What made it cheap

The repository had never been pushed. No remote, nothing public, no clone or
fork holding the old ids. A rewrite therefore invalidated no published
reference — but only until first publication, after which it becomes a
disclosure with a paper trail instead of a quiet correction. The window was open
by luck, not by design.

### What was actually done

`git filter-repo` with a blob callback replacing the function body, in every
reachable commit, with a stub that names the removal and exits 2. Replaced
rather than deleted so history stays coherent: the dispatch and help text still
resolve, and a reader who finds it learns why it is gone instead of finding a
gap.

Verified, not assumed:

- launcher body reachable from **0** commits, down from 4
- `HEAD` tree byte-identical: `19de39a9…` before and after
- 82 commits before, 82 after; messages, authors and dates intact
- freeze still verifies: `freeze intact: 34 files, freeze_sha256 92900c58…`

The suite was run three times across the rewrite. Two were clean (`exit 0`, 22
packages ok, no race, no panic). **One exited 1 and I cannot say why**: it was
the second back-to-back `run.sh all` in a single command and I had redirected
its output to `/dev/null`, so the evidence is gone. Discarding the output of a
run whose exit code I had not yet checked was the mistake.

Review then made the correct call: **that is a release gate, not a caveat.**
An unexplained non-zero exit from the full suite means no test-green claim is
supportable, and "likely Windows lock contention" — which is what I first wrote
here — is a guess wearing the clothes of a diagnosis. It is struck. The
requirement is a captured run that either reproduces the failure or accounts
for the exit code, with complete stdout, stderr and exit status recorded for
every retry and nothing redirected to `/dev/null`.

### The first attempt at that was contaminated, and the contamination is the lesson

Six back-to-back runs were started with every stream captured. Run 1 failed —
`exit=2`, not 1 — with:

    ./run.sh: line 1011: unexpected EOF while looking for matching `"'

I caused it. I was editing `run.sh` to add `cmd_preflight` and N9 *while the
loop was executing it*. Bash reads a script incrementally rather than slurping
it, so a file that changes underneath a running shell gets read at a stale byte
offset and parses as garbage. The failing run was measuring my editor, not the
product.

Two things follow. The narrow one: the experiment was void and was rerun with
the repository frozen, results in the commit that follows. The broad one, which
is worth more — **`run.sh` must not be modified while a release-gate run is in
flight**, and a CI runner that checks out a new revision mid-job would hit the
same class of failure. The contaminated logs are kept rather than deleted.

It does not explain the original `exit 1`. Different exit code, different
mechanism, and nothing was editing `run.sh` at the time.

### F26.1b — a diagnosis I had to withdraw before committing it

While building arm C, `go test ./cmd/rzp-study/` failed with no failing test:

    fork/exec C:\...\Temp\go-build4060995001001
zp-study.test.exe:
    An Application Control policy has blocked this file.

I ran a clean A/B, got a crisp result — default `GOTMPDIR` failed twice,
redirected passed twice — and started writing this up as **the** explanation for
the lost `exit 1`. It is not, on two counts, and both were discoverable in this
repository before I wrote a word.

**One: it is already F9.** Documented long ago, including the detail that
demolishes my fix — moving `GOTMPDIR` into the project "helped once and then
failed again, so it is a reputation/scan delay on newly-created executables
rather than a path rule." My two-for-two A/B is exactly what a reputation delay
looks like from inside a five-minute window. I had rediscovered a known fault
and mistaken the rediscovery for a diagnosis.

**Two, and worse: it cannot explain the failure it was offered for.** The lost
exit code came from `./run.sh all`, and `cmd_all` calls `cmd_test`, `cmd_race`,
`cmd_lifecycle` and `cmd_lifecycle_race` — every one of them `gorun`, i.e.
inside the pinned golang container. **The container is immune to the host's
Application Control**; F9 is precisely why the canonical runner is containerised.
A native `go test` failure says nothing about a containerised lane, and
`run.sh`'s own header has said so since it was written.

So the honest position is unchanged from before I found this: the original
`exit 1` is **not explained**. What can be said is what the captured runs say —
six clean-clone `run.sh all` runs, every stream kept, identical `run.sh` hash
throughout — and that a non-reproduction is not a diagnosis.

The reason this is recorded rather than quietly dropped: I was one commit away
from publishing a confident causal story that a five-minute read of my own
`FAILURES.md` and `run.sh` header refutes.

### F26.1c — what the release gate actually returned

Twelve captured `run.sh all` runs. Every stream kept, nothing to `/dev/null`.

**Clean clone at `ce0141b`, repository frozen, six runs:**

    run 1: exit=0  575s      run 4: exit=0  751s
    run 2: exit=0  594s      run 5: exit=0  527s
    run 3: exit=0  735s      run 6: exit=0  436s

22 packages `ok` in every run, no `FAIL`, no panic, no `DATA RACE`, and
`run.sh` hashed `a25915c1` at every iteration — proof the script did not change
under the runner, which is the failure mode that produced the one red result.

**The earlier contaminated loop, six runs:** run 1 `exit=2` with the parse error
I caused by editing `run.sh` mid-execution; runs 2-6 all `exit=0`.

So across twelve runs the only failure is the one whose cause is known and was
mine. That is the strongest statement available and it is still not a
diagnosis of the original: **the lost `exit 1` was not reproduced.** Logs are in
`evidence/release-gate/`, including the contaminated ones, because a run that
went wrong for a known reason is evidence too.

I also reported that loop as stopped when it was not. `pkill` was followed by a
`pgrep` check, `pgrep` does not exist in this shell, and the `||` branch printed
"loop stopped" on the strength of a missing binary. It ran to completion. The
error is small and the direction is lucky — more data, not less — but a
liveness check that reports success when its probe is absent is the same defect
as everything else in this file: **a claim resting on something that was never
checked.** That is the project's recurring
defect — **a claim stronger than the evidence** — arriving this time as an
explanation rather than a control, and it survived being the exact thing I had
just written three sections about.

### F26.2 — the tripwire that would have caught this looked at the wrong thing

Review's second point: the CI check in place when the launcher was purged
inspected `HEAD` for runner spellings. The defect was **reachable historical
content**, which no `HEAD`-only check can see. So the class of bug was
undetectable by design, not merely undetected.

`./run.sh preflight` is the replacement. It scans every commit reachable from
`git rev-list --all` and refuses if any grants a refund action whose target is
interpolated from a caller-supplied value — a tool writing its own mandate
around an id the caller chose. Every legitimate fixture in this repository
names a *literal* payment id, which is what makes the distinction mechanical
rather than a judgement call.

It deliberately does **not** grep for the name. Four commits still contain
`cmd_live_refund` on purpose — the exit-2 tombstones — so counting the
identifier would flag safe carriers and miss the same code renamed. The shape
is the invariant; the name is not.

Validated in both directions, because a check that has only ever passed proves
nothing:

    current history      0 hits   -> preflight exits 0
    pre-rewrite history  4 hits   -> preflight REFUSES, naming
                                     b9e4275 d41d202 bc62fca bb7fc14

N9 in `redteam-negative` keeps it honest with a synthetic fixture — one line of
mandate JSON with an interpolated id, no key, no request, nothing to run — that
the scan must refuse.

What it does not prove: the absence of every conceivable offense-capable
construction. It rejects the shape that actually occurred, and renamings of it.

### F26.3 — "no remote configured" is not "never published"

The third point, and the one I had glossed. I wrote that the repository had
never been pushed on the strength of `git remote -v` being empty. That proves
only what the *current local config* says.

What can actually be established here is a little stronger, and still not proof:

- `refs/remotes/` is empty and `packed-refs` holds no remote entries
- no `remote.*` key has ever been recorded in the local config
- `.git/FETCH_HEAD` does not exist — nothing was ever fetched
- the pre-rewrite backup bundle contains exactly `refs/heads/master` and `HEAD`,
  no remote-tracking branch of any kind
- the old commit `bb7fc14` is gone from the object store entirely, not merely
  unreachable; `git fsck --unreachable` reports nothing

A `git push` straight to a URL leaves almost no trace beyond the reflog, and
`filter-repo` expires reflogs, so absence of evidence is weak here by
construction. The claim is *plausible and locally consistent*, not proven.

**And one copy demonstrably does exist outside this object store.** The working
tree lives at `C:/Users/<user>/OneDrive/Desktop/Razorpay` — a OneDrive-synced
path, with `.git` inside it — and `OneDrive.exe` and `OneDrive.Sync.Service.exe`
are both running. The pre-rewrite pack files were therefore uploaded to
Microsoft's cloud, which retains version history and a recycle bin. That copy is
not public, but the purge is not complete there, and restoring an earlier
version of the folder would bring the launcher back.

So the honest statement is: **the launcher is purged from this repository's
history; it is not purged from every copy of it that exists.** Publication
readiness needs the external checks in `OPERATIONS.md`, which only the account
holder can perform.

### The part that had to be got right rather than done fast

Rewriting history changes commit ids, and both arms' reports quote a model
freeze commit as provenance. The tempting fix — edit the ids so everything
resolves — would have meant **editing 90 frozen trace files to make a
provenance value look tidy**, which is precisely the retroactive adjustment the
whole pre-registration design exists to prevent. The traces record what was true
when they ran; they are evidence, not presentation.

So the stale ids stay, the generated reports now label them *pre-rewrite id*,
and `study/HISTORY-REWRITE.md` carries the mapping. The freeze was never
anchored on a commit id anyway — `main.go` compares content hashes and
`enforce.go` requires only that the file be tracked, unmodified and present in
`HEAD` — which is why it verifies unchanged across the rewrite. That property
was designed in for a different reason and paid off here.

I also told the user only arm B's id would move, having checked descent from
`bb7fc14` alone. Both moved: arm A's freeze descends from one of the three
earlier carriers. A check narrower than the claim it supported — the same defect
as the finding itself, committed while fixing the finding.


---

## F27 — "The header proves it came from the delivered file" proved nothing

External review, and the finding is that a sentence I wrote to describe a
safeguard was describing something the code did not do.

`readLabelsCSV` validated the returned CSV's header column by column, and I
wrote that this was *"evidence the file came from the worksheet that was
actually delivered."* It then read exactly three fields:

```go
id := strings.TrimSpace(r[idx["row_id"]])
lb := strings.TrimSpace(r[idx["label"]])
Reason: strings.TrimSpace(r[idx["reason"]])
```

**Everything else was parsed and discarded.** A returned file could carry a
rewritten `intent_text`, a different `amount_paise`, a swapped payment
pseudonym, or altered statuses, and the label would still be joined to the
original `row_id` as though the row were untouched. A spreadsheet that
autocorrected a number would have done it silently. Duplicated, missing or
unknown row ids were collapsed or ignored after parsing rather than refused.

The shape is this project's oldest defect and I have now made it in the code
written to *prevent* a labelling problem: **a control whose description is
stronger than what it enforces.** The header check was real; the claim attached
to it was not.

**Fixed by comparing the returned file against the canonical delivered CSV,
field by field.** Only `label` and `reason` may differ. The row set must be
exactly the delivered ids, once each — duplicates, unknowns, missing rows and
added rows all fail closed, before agreement and before anything is rendered.
Returned labels and reasons are also refused if they carry a spreadsheet formula
prefix or a control character, since they are rendered into a Markdown report.

Nine tamper cases hold it: altered intent text, altered amount, altered payment
pseudonym, altered status, duplicated id, unknown id, removed row, blank label,
formula prefix — plus control characters. Each must fail, and the accepted case
must still pass, so the check is known to be able to reject and to accept.

Also corrected in the same round, both mine:

- I described the author's, the assistant's and my own mechanical passes as
  **"three independent passes"**. They are not independent and not blinded —
  each of us has seen the study design, the rubric and the audit surface. At
  most it is deterministic concordance on a nearly arithmetic rule, and it is
  not comparable evidence to two external raters. The phrase is withdrawn.
- Category C in the false-block audit was labelled **"an actual guard false
  positive or implementation limit"**. `maxSetSize = 8` is an intentional
  fail-closed bound on computation an agent can drive by choosing an amount, so
  the honest description is a **bounded-search availability limitation**: the
  guard denies authority reachable under unbounded combining in order to bound
  agent-controlled work. The nine cases still matter; what they measure is the
  price of that trade, not a verdict on it.


---

## F28 — "Pinned in commit" was a local claim dressed as external evidence

External review, source-only, and it caught a sentence I had just written to
describe a control I had just built.

`verifyCanonicalPin` checks that a canonical rater worksheet still hashes to the
value recorded when it was emitted, in a sums file that is committed and
unmodified. I wrote that this pinned the file to its **pre-distribution** state
and had the report publish "pinned in commit `b988c68`", as though a reader
could rely on it.

**This repository has no remote, no tag, and nothing pushed.** Local `HEAD` is
whatever the author last wrote. History can be rewritten carrying the worksheet
and the sums file together, and the check would pass afterwards without a trace
— and I know that concretely, because **I rewrote this repository's history
myself earlier in the same session**, to purge a refund launcher. I built a
control whose only guarantee is that an honest author has not made a mistake,
and then described it as evidence against a dishonest one.

The shape is the project's oldest defect once more: **a control whose
description is stronger than what it enforces**. What makes this instance worth
its own entry is that the gap is not a coding slip. The code does exactly what
it should; the claim attached to it reached for a property the environment
cannot supply.

**What it actually is:** a local workflow-integrity control. It catches an
uncommitted edit, an accidental regeneration, a stale tree. Those are real
mistakes and catching them is worth doing.

**What the external anchors are, neither of which existed:**

1. a public commit, pushed before distribution, that a third party can fetch and
   hash independently;
2. the hash sent to each rater in their distribution message, which the rater
   holds and the author cannot retroactively change.

`predistribute-armC` now enforces the order. It runs the local check, reports
remote/upstream/public-commit status without embellishment, and **refuses to
print the rater messages** until a public commit is recorded in
`DISTRIBUTION-armC.json`. Forced past that with `-acknowledge-no-anchor`, the
messages it prints tell the rater outright that the file is published nowhere
and the hash is the author's own record. `AUDIT-armC.md` prints the same warning
in place of the anchor table when none exists.

The wording is corrected everywhere: local workflow-integrity control, not
immutable pre-distribution evidence.


---

## F29 — I ran the gate before the thing it would have caught, then reported it green

`cmd_preflight` scans all reachable history and refuses any commit granting a
refund to a caller-supplied target. `redteam-negative` N9 proves it can fail, by
building a throwaway repository containing that shape.

N9's fixture was written **literally** into `run.sh`:

```sh
printf ... '{ "authorized_refund_actions": [ { "payment_id": "<VAR>", ... } ] }' > fixture.json
```

where `<VAR>` stands for a shell variable expansion — **redacted here, because
writing it literally in this file made the gate refuse the very commit that
documents the problem.** That happened: the first version of this entry quoted
the line verbatim, `cmd_preflight` flagged `FAILURES.md`, and the tip had to be
amended. A scanner that catches its own failure log is working correctly; a
failure log that cannot describe a defect without reproducing it is the cost of
keying a gate on text.

which is exactly the shape `cmd_preflight` refuses. Committing it made the gate
refuse this repository's own history — the scanner correctly flagging its own
test fixture, the same self-inflicted refusal as N2's credential scan.

**It went unnoticed for 40 commits because of the order I ran things in.**
`cmd_preflight` and N9 were added in the *same* commit, `ce0141b`, and I ran
`./run.sh preflight` in that command **before** `git commit`. It scanned a
history that did not yet contain the fixture and reported clean. I never ran it
again — and then told the reviewer *"`./run.sh preflight` passes"*, which I had
not checked. The gate was red at the time, in a repository that was by then
public.

Two distinct failures, and the second is worse:

1. A test fixture that trips the check it tests. Annoying, and fixed by
   assembling the string at runtime so the source carries a placeholder.
2. **Reporting a gate's result without running it.** That is the precise defect
   this project has recorded fifteen times, committed against the gate built to
   prevent that class of defect. Running a check before the commit that would
   fail it, and then quoting the earlier result, is indistinguishable from not
   running it at all.

**Fixed:** the fixture is assembled by `sed` from a placeholder; the source
matches the rule zero times; N9 still generates the real shape and the scan
still refuses it. History was rewritten across the affected commits and
force-pushed, because an exemption in a defense-only gate is a permanent hole
and the rewrite was free — nothing had been distributed. The worksheet hashes
are unchanged by the rewrite, so only the commit id moved.

The habit this should have produced, and did not: **run the gate after the
commit, not before it.** A pre-commit result describes a tree that no longer
exists.


---

## F30 — Final release audit: the docs described a narrower system than the code

A first-principles audit of the public tip, treating every prior claim as
untrusted. Most of what was checked held. Two documentation defects did not, and
both were introduced by the README rewrite one commit earlier.

### F30.1 — The README described the mandate model as exact-amount-only

**Severity: P1** — a false statement about the security model, in the primary
public document.

**Evidence.** `internal/mandate/mandate.go` defines two action forms:

```go
AmountPaise    *int64 `json:"amount_paise,omitempty"`
MaxAmountPaise *int64 `json:"max_amount_paise,omitempty"`
func (a Action) IsBounded() bool { return a.MaxAmountPaise != nil }
```

`Admits` accepts any amount in `[MinRefundPaise, MaxAmountPaise]` for a bounded
action. The form is exercised in `corpus/`. The README said each entry names
"one payment and one exact amount", which describes a stricter system than the
one shipped.

**Root cause.** The rewrite described the design's *intent* — every shipped
mandate does use the exact form — and presented it as the schema's only option.

**Fix.** The README now names both forms, says the exact one is what every
shipped mandate uses, and states why the bounded form is weaker: it cannot
participate in combining, and a ceiling authorizes more than a figure does.

**Regression test.** None added; this is a documentation-to-code agreement
question, not a behaviour. The behaviour it describes is already covered by the
mandate validation tests.

**Remaining limitation.** Nothing enforces that documentation keeps describing
the schema. A future action form could be added without any doc changing.

### F30.2 — ARCHITECTURE.md implied ranges were impossible

**Severity: P1** — same class, opposite direction.

**Evidence.** The section was headed *"Why not 'refunds up to ₹500 on orders
under 30 days'"* and argued that a range authorizes an unbounded number of
refunds. A reader would conclude the schema has no range. It does.

**Fix.** The section is now *"Why exact amounts are the default, and what the
bounded form costs"*, states that both exist, that every shipped mandate uses
the exact form, and that a bounded action's blast radius is its ceiling rather
than one amount.

**Remaining limitation.** The argument against ranges is still the design's
position. The bounded form remains available and a merchant can choose it.

### Verified during this audit, and NOT findings

Recorded because "we checked and it held" is worth as much as a defect list.

- **Default-deny decision path.** Every error branch in `policy.Decide` returns
  `deny`. Only two paths allow: an allowed non-refund tool, and `reserveSet`.
- **Build-level allowlist.** `supportedTools` is nine names — `create_refund`
  plus eight read-only fetches. A mandate can narrow it, never widen it.
  `create_refund` is the only money-moving tool in the set.
- **Amount parsing.** `parseAmountPaise` rejects `float64` outright, rejects
  exponent and fractional forms, rejects booleans, and rejects anything not
  representable as `int64`.
- **Receipt injection.** `forwarded["receipt"]` is assigned unconditionally, so
  an agent-supplied receipt is discarded. Response correlation requires payment,
  amount AND receipt to match.
- **Receipt-set collision.** `ReceiptForSet` joins sorted ids with `+`, which
  would collide if an id could contain `+` — `{"a+b"}` and `{"a","b"}` would
  hash alike. Not reachable: `actionIDPattern` is `^[A-Za-z0-9_-]{3,64}$`, and
  duplicate ids are rejected at load.
- **Durable ordering.** The reserve-before-forward invariant has dedicated
  coverage in `internal/lifecycle/durability_order_test.go`, plus
  `TestAFailedDurableWriteStrandsNoBudget` and `TestFailedDurableWriteLeavesNoClaim`.
- **Combining excludes bounded actions.** `combineExact` skips
  `a.IsBounded()`, so ranges cannot be summed.
- **Defence-only command surface.** 25 commands; none creates a payment, order,
  checkout or payout, and none can move money outside the guard.
- **Audit anchor.** `65f97b03` remains an ancestor of the public tip, and both
  rater worksheets still hash to the value recorded before distribution.


---

## F31 — Arm D claimed a held-out metric it did not implement

**Severity: P0 (two claims), P1 (three).** Found by review, on a result I had
published as a metric rescue an hour earlier. All five points were correct.

### F31.1 — The scorer never reads the intent

**P0.** `PROTOCOL-armD.md` §4 said "ground truth comes from intent, never from
the mandate", and I repeated it in the commit message and the report.

`cmd/rzp-armd/main.go` branches on `r.Label` — a field authored into
`requests.json` by `grid_d.py`. `intent_text` is loaded into the struct and
never used in a decision. `intent_payment` is not in the struct at all. **A
one-field edit to `label` changes the reported precision and recall.**

The generator derives the label from the intent, so the values are consistent
with the claim — but the claim was about the *implementation*, and the
implementation scores against an author-declared field. That is a different and
much weaker thing, and describing it the strong way is the defect.

**Fix:** reclassified, not rewritten. Every arm D document now opens with the
required statement, and the scorer's own header says it branches on `r.Label`
and does not derive ground truth from intent.

### F31.2 — "Held out by a date" is not held out

**P0.** Freezing the policy before building the corpus proves the code was not
fitted to the data. It does **not** blind the author, who knew the decision rule
while constructing the corpus. I called it "a stronger guarantee than a random
train/test split", which is wrong in both directions.

And **D1 is tautological.** Every positive was constructed as an unmatched
amount or an unmatched payment. A default-deny capability verifier must refuse
all of them. Recall 1.000 restates the construction.

**Fix:** the phrase is withdrawn. Arm D is now described as *a pre-registered,
same-author synthetic conformance corpus scored against author-declared labels*,
whose *90-row confusion matrix is exact for that finite grid, but is not
independently labelled or policy-blind and does not establish transferable
recall, precision, or false-positive cost.*

### F31.3 — The false-positive labels are not settled ground truth

**P1.** An intent reading "refund the item price, 24,000 paise" does not
self-evidently mean a 12,000-paise partial refund is in-intent. The six
`coverage=exact / request=under` rows I counted as false positives may be a
correct exact-authorization refusal rather than a merchant-harming block.

Deciding that needs independent human labels or a separately justified rule.
Arm D has neither, so those six are **contested, not established**.

### F31.4 — A control that was described but never built

**P1.** `PROTOCOL-armD.md` §3 said the policy not changing after scoring was
"enforced by the harness". Nothing enforced it. `rzp-armd` reads whatever
`internal/policy` currently contains and only refuses to overwrite the result
file.

**Fix:** `rzp-armd verify` — read-only. It recomputes the matrix in memory,
compares it to the published one, and refuses if the policy tree hash differs
from `study/armD/policy-freeze.json`. It never writes and never rescores.

    recorded policy tree: a9d0826fb35dcb05...
    current  policy tree: a9d0826fb35dcb05...
    recomputed: TP 54 FP 17 TN 19 FN 0   reproduces the published matrix exactly

**Remaining limitation:** the freeze was recorded *after* scoring. Git shows the
last commit touching `internal/policy` is `fb87b12` (2026-08-30), predating the
corpus, so the hash is provably the one in force — but the ordering was luck,
not design.

### F31.5 — The pre-registration commit's CI failed and I never looked

**P1, and the reproduction is real.** `ca1e4c1` failed CI:

    --- FAIL: TestTheExclusiveLockHoldsUnderConcurrentOpens
    0 of 16 concurrent opens succeeded on ONE state file, want exactly 1

I pushed it and moved straight to building the corpus without checking. A later
green run on a different commit does not erase it, and no bootstrap change
happened in between.

**Stress-tested rather than assumed:**

| condition | runs | failures |
|---|---|---|
| pinned container, default resources | 30 | 0 |
| pinned container, `--cpus=0.5`, `-race` | 12 | **1** |

**Reproduced under CPU constraint with the identical signature.** This is a real
contention-triggered defect, not a CI fluke.

**Direction of failure:** 0 of 16 succeeded, so *nobody* acquired the exclusive
lock. That is fail-closed — no two openers could spend against separate in-memory
ledgers — so it is an **availability** defect, not a safety hole. Under
contention the guard may refuse to start at all.

**Not fixed.** The plausible fix is bounded retry on lock acquisition, which
changes startup semantics on a money path and is a decision rather than a
cleanup. Recorded here with its reproduction so it is not rediscovered as a
mystery.

---

## F32 — The reclassification of arm D was itself incomplete, and created a new provenance problem

**Severity: P0 (one), P1 (two).** Found by review, on the commit that was
supposed to have fixed F31. Every point was correct.

### F32.1 — A corrected banner over uncorrected text

**P0.** F31's fix prepended the required classification statement to the three
arm D documents and edited a handful of sentences underneath. It did not correct
the **generator**, so `cmd/rzp-armd/main.go` still emitted "A held-out
evaluation", "Ground truth comes from the intent, never the mandate", "Quote the
recall and the false-positive rate" and "a refund the merchant wanted, blocked".

`RESULTS-armD.md` therefore carried the honest banner and the withdrawn claims
at once. Worse, a half-removed sentence had left a fragment — "things Track 2
names." — dangling on its own line, which is what a document looks like when it
has been patched rather than corrected.

A banner that contradicts the body it sits on is not a correction. It is two
claims in one file, and a reader is entitled to believe either.

### F32.2 — Amending a pre-registration and a generated report in place

**P1, and the more interesting one.** The fix edited `PROTOCOL-armD.md`, which
says *"nothing below was written after seeing a number"*, and `RESULTS-armD.md`,
which says *"Generated by `rzp-armd`. Computed, not written by hand."* Both
statements became false at the moment I edited the files, and neither file said
so.

Correcting a claim by rewriting the artifact that made it destroys the only
record of what was claimed. The rewrite is undetectable to anyone who did not
already have the earlier bytes.

**Fix.** The three documents are restored to their original bytes and preserved
unedited. Each carries a dated notice at the top — the only edit — pointing at
`study/ASSESSMENT-armD.md`, which is where the retraction now lives. The
manifest records the SHA-256 of the preserved text and the git blob of each
original, so `git show f87c86b:study/RESULTS-armD.md` is an independent check,
and `rzp-armd verify` fails if a byte below the marker moves.

The generator no longer emits the withdrawn wording **and no longer writes to
that path**: it produces `study/armD/CONFORMANCE-armD.md`. It can no longer
overwrite or regenerate the retracted report, which is the only way to be sure a
withdrawn claim cannot come back the next time someone runs the tool.

### F32.3 — The verifier was narrower than the thing it verified

**P1.** `rzp-armd verify` hashed `internal/policy` while the score also ran
`internal/mandate` through `mandate.Load` — and `mandate` was not the only
omission. `go list -deps ./cmd/rzp-armd` gives the closure, and
`internal/lifecycle` and `internal/opauth` are in it too. A change in any of
them could have moved a published decision while verification still reported
everything unchanged.

It also verified a matrix stored in a JSON file it had written itself. It never
checked that `RESULTS-armD.md` contained that matrix. Those are different
claims, and only the weaker one was ever true.

**Fix.** `study/armD/manifest.json` now covers the whole closure plus
`cmd/rzp-armd/main.go`, the corpus, all 90 compiled mandates, all three corpus
generators, the generated report, and the preserved body of each retracted
document — and verification parses the confusion matrix **out of the documents**
rather than trusting a recorded number. `manifest_test.go` recomputes the import
closure from the source and fails if the manifest stops covering it, so the
narrowing cannot recur silently. `internal/storage` stays out because it is
genuinely unreachable from the scorer (nil persister) — a convenient claim, and
therefore one a test checks rather than a comment asserts.

Twelve mutations were run against the finished gate in an isolated copy, one at
a time. All twelve were refused, each naming the right cause: an edit to any of
the four decision-path packages or to the scorer, a single flipped `label`, an
edited mandate, an edited corpus generator, an edited report, an edited
preserved body, a stripped retraction marker, and a manifest whose matrix had
been quietly improved.

### F32.4 — Calling CI green before the workflow finished

**P1.** I reported the previous commit's CI as green while the run was still in
progress. It did in fact pass — one workflow, `reproducibility`, 2m35s — but I
did not know that when I said it. F29 is the same mistake with a different
excuse, which is why it is recorded again rather than folded in.

### F32.5 — The lock defect, now fixed

The defect recorded under F31.5 is fixed rather than merely reproduced. Cause,
fix, the reason a `busy_timeout` is the wrong instrument, and the before/after
stress figures are in `study/ASSESSMENT-armD.md` §5. Summary: **1 failure in 12
runs before, 0 in 60 after**, under `--cpus=0.5` with `-race` in the pinned
container, with the contention test rewritten to assert all four required
properties instead of counting successes.

---

## F33 — The pre-distribution command handed blind raters a link to the study, and misreported what it could know

**Severity: P0 (two).** Found by review. `cmd/rzp-study/armc_distribute.go` had
been left untouched through the arm D work while both defects were already
known, which is its own lesson: a finding that is not fixed in the commit that
records it is a finding that gets shipped.

### F33.1 — The blinding and the thing that defeated it, in the same paragraph

**P0.** The rater message ended with:

```
This exact file is published at:
  <public commit URL>
You can verify the attachment's hash against that commit.
```

The intent was honest — let a rater confirm nobody swapped their worksheet. The
effect was to hand someone who was supposed to be blind a link to the
repository, and through it the study design, the labelling rule, the protocol,
and the guard's own decision on every row they were about to label.

Three lines above it, the same message asked them not to discuss the cases with
the other rater. The message was protecting the blinding at one end and breaking
it at the other.

**Fix.** Two artifacts that never mix. `raterMessage` carries the file name, its
SHA-256, how to edit and return it, and an instruction not to look the rows up —
no URL, no repository, no commit, no protocol, no description of what the rows
are or why they were selected. `reviewerRecord` carries the anchor and the
hashes, for a reviewer or the release record, and is never sent to a rater. The
console output brackets each with explicit BEGIN/END markers so the two cannot
be copied together by accident.

**The trade, stated rather than hidden:** a rater can no longer verify their
attachment against a published commit. They hold the hash and nothing else. That
is the right way round — the anchor exists so a *third party* can check the
author did not swap the worksheets, and a third party reads the reviewer record.

Five tests assert this on the real output, including one that scans for the
literal repository URL, commit SHA and commit URL from
`DISTRIBUTION-armC.json`, and one that checks the message is still sufficient to
label and return the file. A comment claiming "no context here" would not have
survived the next edit.

### F33.2 — "NONE (nothing has been pushed)"

**P0.** When no tracking branch was configured, the command printed
`upstream branch: NONE (nothing has been pushed)`. That is a claim it cannot
make. An absent upstream says only that *this checkout* has no tracking branch
set. The commit may have been pushed from another clone, pushed by explicit
refspec, or pushed and the configuration removed afterwards.

This is the same error as the earlier "no remote configured, therefore never
pushed" — restated in a different field, in a tool whose entire purpose is to
distinguish what is locally asserted from what is externally checkable.

The direction matters too. Reading "not configured here" as "never published"
reports an anchored audit as unanchored, and an author who knows better learns
to click past the warning.

**Fix.** `upstreamLine` states the narrow meaning and disclaims the inference.
The command now also states plainly that it makes **no network calls**: it does
not fetch the recorded commit, does not confirm it exists on the public host,
and does not confirm the published files hash to the recorded values. Everything
in that section is read from local configuration and a local JSON file.

---

## F34 — The pre-registered rater delivery carried the study it was blinding

**Severity: P0.** Found while preparing to distribute, before any worksheet was
sent. Nothing had been delivered, so this is a defect caught rather than a
result contaminated.

F33 removed the public commit URL from the rater message. It did not look at the
*other* half of the packet. `PROTOCOL-armC-AUDIT.md` §Files pre-registers the
delivery as the worksheet CSV **plus `LABELLING-armC.md`**, and that rubric is
the project's internal instrument.

**It names the component under test.** §"What you must NOT consider" tells the
rater to disregard "what the guard did", "whether you think the guard blocked
it", "what any mandate contained", and "whether the amount was reachable by
combining several authorizations" — calling these "a property of the system
under test".

Written in good faith, and self-defeating. Every row in this worksheet is a
blocked call; the audit asks what fraction of them were in-intent. A rater told
not to consider whether the guard blocked a row can infer that it did, which is
precisely the fact the protocol says raters are not told. **The instruction
protecting the blinding revealed it.**

**It describes the design.** §"Who labels" names `study/grid.py`, the kappa
plan, the supplementary author-rater, Amendment 1 and the one-rater fallback.

**It instructs a different file.** §"Filling the file" tells the rater to open
`worksheet-armC-e1.json` and edit JSON keys. That file exists — it is the
340-row worksheet the same protocol records as *never delivered*. A rater would
have gone looking for a file they were not sent while holding a CSV the rubric
never describes.

### Fix

`LABELLING-armC.md` is **not edited and not deleted**; it stays as the internal
record of the instrument. `study/RATER-INSTRUCTIONS-armC.md` is a new
rater-only document: the task, the columns, the three labels, R1–R6 carried
across unchanged in substance, neutral worked examples, CSV editing
instructions, and the instruction not to browse or search the project until
labels are returned. The substitution is recorded in
`PROTOCOL-armC-AUDIT-AMENDMENT-3.md`, dated and made before any label exists.

The enforcement is a scan, not an intention. `predistribute-armC` refuses to
print the packet at all if the delivered instrument or the rater message
contains any forbidden context word, matched case-insensitively on substrings so
`author` also catches `authorization`, `authorized` and `authority`.

Tests assert **both** directions: the delivered instrument passes, and
`LABELLING-armC.md` fails on 17 distinct words. A scan that passed both would be
decoration. The reviewer record is asserted to fail it too — it carries the
anchor URL by design — which is what stops the two outputs being merged back
together later.

### The lesson, which is the same one twice

F33 fixed the rater *message* and shipped the rater *packet* unchanged. The
review found the message defect; I did not then ask what else was in the
envelope. Fixing the instance in front of me and not the class is how the same
defect arrives twice from two directions.

---

## F35 — Two gaps a hostile review named, and what closing them cost

Not defects found in operation: gaps found by reading the project the way a
judge would. Recorded here because the fixes changed a money path and an
economic claim, and both deserve the same scrutiny as a bug.

### F35.1 — The guard secured one boundary and trusted the other

Every check in this project assumed the mandate was genuine. Nothing
established it. `cmd/rzp-guard` read a JSON file off disk and enforced whatever
it said, faithfully. Anyone who could write that file could grant authority,
including after the fact.

    merchant --[ authority -> guard ]--> guard --[ agent -> authority ]--> agent
               ^ nothing checked this               ^ internal/policy

**Fix.** `internal/mandateauth` verifies an ed25519 detached signature over the
mandate's **exact file bytes**, before parsing. Signing a re-serialisation would
mean the bytes that were signed are not the bytes that are enforced, and any
difference in key order or number formatting becomes either a false rejection or
a gap; verifying the raw bytes removes canonicalisation from the trust path
entirely. It also runs before any parser touches the input.

`rzp-guard-operator mandate-keygen` and `mandate-sign` are the merchant side.
They run before the operator opens a state file or checks a credential, on
purpose: requiring the guard's exclusive lock to sign a mandate would force the
signing key onto the guard host, which is the one place it must not be.

**Opt-in, and that is a real limitation, not a soft launch.** Every fixture in
this repository is unsigned; defaulting verification on would break all of them
at once, on a money path, days from a deadline. Without `-mandate-pubkey` the
behaviour is exactly what it was, and the guard prints a warning naming what is
not being checked. With a key configured, an unsigned or altered mandate refuses
to start.

**What it still does not do.** It authenticates the file, not the human. A
compromised signing key issues mandates the guard will honour. Key custody is
outside this program and is not solved here.

Eight tests in `internal/mandateauth`, four in the operator. They cover a raised
amount, a one-byte flip, a signature from a different key, a **deleted**
signature file (verification that can be switched off by deleting a file is not
verification), a malformed key, and a mandate whose incidental whitespace proves
the signature covers the file rather than a re-encoding.

`internal/mandateauth` is deliberately outside the arm D decision path -- the
scorer does not import it -- so `rzp-armd verify` still reproduces the published
matrix against an unchanged decision-path hash. That was checked, not assumed.

### F35.2 — The false-positive rate was published and never priced

Track 2 asks for "honest metrics including false-positive cost". This project
reported a rate, and a "value refused: 689,000 paise" figure that reads like a
loss and is not one.

**A blocked refund is not a lost refund.** The customer waits, a human unblocks
it, the money still moves. A false positive costs a support contact, not the
basket -- reading it as the basket overstates the cost of a block by roughly
fifty times. A false negative genuinely costs the amount plus investigation. The
two errors are asymmetric in kind, which is why a single F1 number is the wrong
objective here.

`study/FP-COST.md` prices both directions with stated, challengeable
assumptions, and reaches a break-even: this control pays for itself above
roughly a **5.6%** out-of-intent base rate, or **2.4%** with bounded actions --
a feature that already exists -- enabled.

**Arm C observed 0.6%.** Below break-even. On the only agent traffic this
project has ever seen, the handling cost would have exceeded the loss prevented.
That is recorded rather than buried, and it changes the honest positioning from
"saves money" to "insurance against a tail that is catastrophic and
uninsurable". It also says precisely where the remaining engineering value is:
recall is 1.000 by construction and has nothing left to give, so every available
improvement is on the false-positive side.

The document states the four inputs that would overturn it, including that
`c_fp` is a guess no corpus can measure -- it needs a merchant.

### F35.3 — What was NOT fixed

- **No independent labels.** The blocked-call audit still has none, so no
  precision, recall or conditional rate is published. The README headline is
  unchanged: this project does not meet the Track 2 metric bar.
- **`cmd/rzp-armd` intermittently fails to build on this Windows host**,
  reporting `cannot find package` at its two import lines while `go list -deps`
  resolves all 106 and `cmd/rzp-study` with the same imports builds fine. It
  builds and tests clean in the pinned container and in CI, and `git status`
  shows the package unmodified.

  **Correction, and the reason it is written here rather than quietly edited:**
  this entry first said the package "will not build on this Windows host". That
  was wrong. It failed twice consecutively and I read two failures as
  determinism without measuring. Measured afterwards: **10 runs in the OneDrive
  tree, 0 failures; 10 runs in a copy outside OneDrive, 0 failures.** It is an
  intermittent fault, almost certainly a sync-time file lock on this
  OneDrive-backed tree (F12 is the same hazard in a different guise), and a
  transient failure reported as a permanent one is still a false statement about
  the system. Undiagnosed. Arm D is verified in the container, which is
  unaffected.

---

## F36 — Three defects in arm E, found by attacking my own corpus before the labels came back

**Severity: P1 (one, fixed), P2 (two, unfixable).** Found by deliberately trying
to break arm E while three raters were working, on the principle that a design
flaw discovered after the labels arrive cannot be fixed at all.

Recorded in full in `study/PROTOCOL-armE-AMENDMENT-1.md`, dated and written
before any rater file existed.

### F36.1 — A rule taught and never exercised

`RATER-INSTRUCTIONS-armE.md` carries R3 -- a refund from a payment the merchant
never mentions is out-of-intent -- with a worked example.
**`intent_payment` equals `request_payment` on all 120 rows.** The payment axis
was dropped when the request dimension was fixed at four amount-shaped levels,
and the instrument was written from the rule set rather than from the corpus.

The corpus matches its own pre-registration; the instrument overreaches. Cost:
raters check something that never varies, and arm E is narrower than arm D,
which did cover the wrong-payment case. **Not fixed** -- the worksheet is with
three people, and reissuing a corrected file mid-flight is worse than a
documented gap.

### F36.2 — 120 rows, 6 sentences

`intent_text` is a function of `intent_kind` and `size` alone, so the corpus
contains **six distinct intent sentences**, each repeated 12 to 24 times with
the amount varying underneath. It was visible in `grid_e.py` from the first line
and I did not look until the worksheet was already out.

The corpus is six semantic situations sampled at several amounts, not 120
scenarios, and every report must say so. The `ambiguous` cluster is the sharpest
case: 24 rows, one identical sentence, seven amounts, and no anchor to judge any
amount against -- which makes prediction E4 (kappa below 0.4 there) more likely
to fail, for a reason recorded before the data rather than after.

### F36.3 — The pre-registered interval was the wrong one, and is fixed

**P1, and the only one that could still be fixed.** `PROTOCOL-armE.md` §6
committed to Wilson intervals. Wilson assumes independent observations. Given
F36.2 the rows are clustered into six groups whose outcomes are correlated, so
**Wilson understates the uncertainty in the direction that flatters the
result** -- the same class of error as everything else in this file.

`rzp-arme score` now reports a seeded cluster bootstrap alongside Wilson,
resampling whole sentence groups. Both are printed; the report says to quote the
bootstrap. Wilson stays because it was pre-registered, and silently replacing it
after the fact would be the substitution this entry exists to avoid.

Measured on a synthetic dry run, recall's interval widened from 0.284 to 0.341.
**The false-positive rate's came out narrower**, which the first draft of the
report text did not allow for -- it claimed the bootstrap corrects Wilson
upward. With five or six clusters the estimator is coarse enough to land either
way, and the report now says that explicitly rather than implying a one-sided
correction. A cluster whose rows are all excluded drops out entirely and
coarsens it further.

### What this cost, and what it bought

Two of the three cannot be fixed, because I looked after the packet was sent
rather than before. The fix for the third was still available only because no
label had returned; an analysis change made after seeing results would have been
worthless whatever its merits.

**Local verification was partial for this commit.** Docker was not running, so
the container lane did not execute. 13 of 14 packages pass on the Windows host;
the four failures in `cmd/rzp-guard-operator` are the documented mode-bit
limitation, unrelated to this change. CI on Linux is the verification.

---

## F37 — The independence sweep: this project set the right standard and then abandoned it

**Severity: P1.** After finding the clustering defect in arm E (F36.3), I swept
every document that reports a rate for the same error. It is not confined to arm
E, and the most embarrassing part is that the correct method is written down in
this repository's own founding document.

### F37.1 — PREREGISTRATION.md already required cluster bootstrapping

`PREREGISTRATION.md` lines 73 and 153:

> **Confidence intervals by cluster bootstrap.** Calls within a session are not
> independent — they share a mandate, an agent and a template.

> The independence unit is the template, not five mechanically generated
> sessions from one template — those are replicas differing only by payment id
> and an amount from a fixed pool. Bootstrapping over them is
> **pseudoreplication**.

That is exactly right, it was written before any arm ran, and **arms D and E
both ignored it.** Arm E is fixed (F36.3). Arm D is addressed below. The failure
was not ignorance of the method; it was not re-reading my own protocol.

### F37.2 — Arm D is more clustered than arm E, and FP-COST inherited it

Arm D: **90 rows, four distinct intent sentences.** Its 36 in-intent rows — the
entire denominator of the false-positive rate — come from four sentences in
groups of 12, 12, 6 and 6.

Arm D itself survives this. Its banner already says the matrix is "exact for
that finite grid" and "does not establish transferable recall, precision, or
false-positive cost", and a confusion matrix over 90 specific rows genuinely is
exact for those rows. Clustering only bites when a number is generalised.

**`study/FP-COST.md` generalised it.** It took FPR = 0.472, computed a break-even
of 5.6%, and presented that as a decision-relevant figure with **no uncertainty
of any kind** — no interval, no mention of clustering, nothing. I wrote it two
days ago and had been recommending it as the strongest evaluation asset in the
project.

Resampling whole intent-sentence groups:

```
FPR        0.472   ->  95% cluster bootstrap  0.375 - 0.600
break-even 5.6%    ->  4.5% - 7.0%
```

**Fixed.** `FP-COST.md` and the README now quote the range and say why it is a
range. The qualitative conclusion is unchanged and survives comfortably — arm C's
observed 0.6% is below the bottom of every band — but "5.6%" implied a precision
the corpus cannot support, and a judge checking the denominator would have found
four sentences behind a three-significant-figure number.

Worth noting: the contested six `coverage=exact / request=under` rows would move
break-even to 3.7%, which is *inside* the cluster interval. The labelling dispute
and the sampling uncertainty are the same size, which is a more useful thing to
know than either alone.

### F37.3 — The arm C audit would have the same defect if it ever runs

The 72 audited rows come from **22 distinct scenarios**, mean 3.3 rows each, up
to 5 from one scenario. `cmd/rzp-study/armc_audit.go` contains no interval
machinery at all, so nothing false has been published — the audit has never run.
But any conditional rate computed over those 72 rows as independent observations
would be pseudoreplication by the founding protocol's own definition.

**Not fixed, deliberately.** The audit has no valid labels and is a candidate for
withdrawal. Building cluster machinery for a study that may not run is the wrong
order of work. Recorded here so that if it does run, the requirement is already
written down: **cluster by scenario, not by row.**

### The pattern

Three arms, three versions of the same mistake, and the fix was in the repository
the whole time. What made it findable was going looking for it while there was
still time to act — the arm E fix was only possible because no label had
returned, and the FP-COST fix only mattered because the number had not yet been
said out loud to a judge.

---

## F38 — A correct number in the README that nothing in the repository computed

**Severity: P1.** Found by continuing the independence sweep into every numeric
claim rather than stopping at the statistical ones.

`README.md` states that the `maxSetSize = 8` combining bound refused **nine**
refunds in arm C whose entries summed exactly to the requested amount. It entered
in `77dd09c` ("Rewrite the README for a non-technical panel").

**The number is correct.** I recomputed it independently: of 72 blocked refund
calls, 63 asked for an amount no subset of the remaining authorizations reaches,
and 9 asked for an amount that is reachable — every one of them needing ten
actions, two past the bound.

**Nothing in the repository computed it.** `PROTOCOL-armC-AUDIT.md` says the
audit report "will list every category C call individually" — future tense, and
that report has never run, because the blocked-call audit has no labels. So the
most-read document in the project carried a specific measured figure that a
reader could not check and a judge could not reproduce.

That is the same defect as an unsourced metric wearing different clothes, and it
is worse in one way than F37.2: FP-COST's break-even was at least derived from a
published matrix. This was a bare assertion.

### Fix

`rzp-study reachability-armC` re-derives it. It needs **no rater labels** —
category C is decided by the guard's own recorded refusal message, which names
the actions still available at that moment and already accounts for actions
consumed earlier in the same trace. Separating it from the stalled audit means
the `maxSetSize` evidence stands on its own instead of waiting on labels that may
never arrive.

The search here is deliberately **unbounded** where the guard stops at eight. A
bounded check would agree with the guard and report nothing, which is exactly the
failure mode: the instrument would hide the thing it exists to measure.
`TestSubsetSizeFindsTheSmallestReachingSubset` pins that with a ten-action case.

A refusal reason that fails to parse is an **error**, not a skipped row. Silently
dropping an unparseable refusal would under-report the count in the direction
that flatters the guard.

### What it surfaced

**All nine need exactly ten actions.** Nothing said this before, and it changes
the shape of the decision: the trade on this corpus is **8 versus 10**, not 8
versus unbounded. Raising the bound to ten recovers all nine and leaves the
search finite. Whether that is worth the extra agent-controlled computation is a
design question, and neither the command nor this entry takes it — but the
question is now answerable instead of rhetorical.

### The pattern this sweep is finding

Three entries in a row, all the same shape: a number that is *true* but whose
support is weaker than its presentation. F37.2 quoted a point estimate from four
clusters. F36.3 used an interval that assumed independence. This one had no
derivation at all. In every case the fix was to make the uncertainty or the
provenance visible rather than to change the claim — which suggests the defect is
not in the analysis but in the habit of writing the conclusion before checking
what supports it.

---

## F39 — The money-path sweep, and the arm D gate catching my own change

**Severity: P2 (one defect fixed), plus a control working as designed.**

### F39.1 — What the sweep found, which was mostly nothing

I probed the authorization path with hostile amounts against the real guard
rather than reading the code and concluding it was fine:

```
negative (bounded)     REFUSED   AMOUNT_NOT_AUTHORIZED
zero                   REFUSED   AMOUNT_NOT_AUTHORIZED
1 paise                REFUSED   AMOUNT_NOT_AUTHORIZED
99 (just under floor)  REFUSED   AMOUNT_NOT_AUTHORIZED
100 (at floor)         ALLOWED
int64 max              REFUSED   AMOUNT_NOT_AUTHORIZED
int64 max + 1          REFUSED   MALFORMED_ARGUMENTS
float / exponent       REFUSED   MALFORMED_ARGUMENTS
string / bool / nil    REFUSED   MALFORMED_ARGUMENTS
```

The cumulative cap holds: with three bounded actions of 30,000 against a 50,000
cap, the first is allowed and the rest are refused — it does not spend a partial
amount to fill the gap. `Decide` serializes its match-to-reserve section under a
mutex, `combineExact` filters bounded actions before `reserveSet` dereferences
`*a.AmountPaise`, and `IsExpired` is `!now.Before(ExpiresAt)`, so a mandate is
expired *at* its expiry instant rather than one tick later.

### F39.2 — Two inputs got through that should not have

`+24000` and `024000` were **authorized** against an exact 24,000 action.

`parseAmountPaise` rejected `.eE`, booleans and float64, then handed the string
to `strconv` via `json.Number.Int64` — and **strconv is more permissive than
JSON**. It accepts a leading plus and leading zeros. RFC 8259 gives integers as
`-? (0 | [1-9][0-9]*)`; neither form is valid JSON.

**Nothing was reachable through the relay.** A compliant decoder rejects those
documents before the guard sees them, so this was never live. But the error
message beside the check promised "a plain JSON integer in paise", and a
validator looser than the rule it states is the exact shape of every defect in
this file that turned out to defend nothing. It becomes reachable the moment
anything upstream constructs a `json.Number` by hand.

**Fixed.** `jsonInteger` checks the grammar before parsing. Eight rejected forms
and five accepted ones are pinned by test, including the boundaries.

### F39.3 — The arm D gate caught my own change, and I did not re-stamp it

Editing `internal/policy/policy.go` put it inside arm D's recorded decision path,
so `rzp-armd verify` immediately failed:

```
FAIL DECISION PATH CHANGED. The published result describes code that is
     no longer in the tree, so it is VOID and must be re-scored.
     differs: internal/policy/policy.go
```

This is the control built in F31 doing precisely its job, on its author.

The easy response was to delete `manifest.json` and re-record — which
`recordManifest` permitted, since it only refused to *overwrite*. That would have
left "decision path unchanged since scoring" true of the new stamp and **silent
about the fact that the freeze had ever been broken.** A freeze that can be
quietly reapplied is not a freeze.

**Fixed by making the re-record auditable.** `rzp-armd manifest -supersede
"<reason>"` carries the old tree hash, the original recording date, the
supersession date and the stated reason forward into
`superseded_decision_paths`. It **refuses outright** if the corpus no longer
reproduces the published matrix — that is a different result and needs a new
corpus, not a new stamp. `TestASupersededDecisionPathCarriesItsReason` asserts
every entry has a reason, distinct hashes, and a recorded matrix that held.

The matrix is unchanged: TP 54 FP 17 TN 19 FN 0, all 90 decisions identical,
because arm D's corpus contains only plain integers. `ASSESSMENT-armD.md` now
says the freeze was broken once, harmlessly, and points at the trail as the
evidence for "harmlessly" rather than asserting it.

---

## F40 — A JSON parser differential smuggled an unauthorized refund past the classifier

**Severity: P0.** The most serious defect found in this project. A real bypass on
the money path, reproduced against the running relay before it was fixed.

### The attack

RFC 8259 permits an object to repeat a key and calls the result unpredictable.
Parsers resolve it differently: Go's `encoding/json` takes the **last**
occurrence, while many JavaScript and Python paths take the **first**.

The relay reads a line, classifies it, and forwards anything that is not
`tools/call` **byte-for-byte**. So:

```json
{"jsonrpc":"2.0","id":1,
 "method":"tools/call","method":"tools/list",
 "params":{"name":"create_refund",
           "arguments":{"payment_id":"pay_X","amount":900000}}}
```

The guard read `method = "tools/list"`, classified it as a read, applied no
authorization check, and forwarded the raw bytes. A child parser taking the
first occurrence receives `tools/call` with `create_refund` for **900,000
paise** — an amount no mandate authorized, through a boundary whose entire
purpose is that this cannot happen.

Verified against the real relay: the child received the whole line.

### Why the existing defences did not catch it

Every one of them was pointed at a different thing, and each was correct on its
own terms.

`parseAmountPaise` never ran, because the message was never classified as a
refund. The duplicate-**request-id** check did not apply, because the id was used
once. The `tools/call` path **already defeats this attack** — its arguments are
rebuilt from the parsed map before forwarding, which is why the same trick with a
duplicate `amount` key fails — but that re-serialisation protects only the path
that was already recognised as dangerous.

The relay states the right principle in its own comment: *"the child must never
receive bytes this relay could not inspect."* Bytes it inspected and read one
way, while the next hop reads another, are the same failure wearing a disguise.
The principle was written down and enforced for parse **errors** only.

### The fix

`duplicateKey` walks the JSON token stream and reports any object that repeats a
key at any depth. `handleAgentLine` refuses such a message with `-32600` before
reading anything out of it, and nothing reaches the child.

**Refusal rather than canonicalisation, deliberately.** Re-serialising every
message would make the guard's reading authoritative — that is exactly what
protects `tools/call` today. But rewriting arbitrary pass-through MCP traffic
means reordering keys and reformatting numbers in messages this relay does not
understand, risking protocol breakage to repair an input that should never have
been sent. Refusing is fail-closed and requires no knowledge of the message.

**The reverse direction too.** A child reply with a duplicate key is one the
relay reads one way and the agent may read another, so the commit decision it
would drive is untrustworthy. `markAmbiguous` puts those actions **IN_DOUBT**
rather than committing — not a release, because the bytes already reached the
child and the refund may have executed. The raw reply still goes upstream,
because the agent needs it.

### False positives were the real risk in the fix

A detector that refuses legitimate traffic is worse than the bug it closes. MCP
messages reuse key *names* constantly — `name` appears in `params` and again in
`arguments` on every single `tools/call`. Only a repeat **within one object** is
ambiguous.

Pinned by test in both directions: five nesting shapes are caught, including
inside arrays and arrays of arrays; six legitimate shapes are not, including the
ordinary `tools/call` skeleton, sibling objects in an array, a string *value*
equal to a key already seen in the same object, and arrays of scalars. Malformed
input reports no duplicate, leaving the parse error to the caller.

### What this says about the rest of the sweep

The previous four entries were reporting defects — claims carrying more support
than they had. This one is a control that did not control. It was found by
probing the relay with hostile input instead of reading it and concluding it
looked right, which is the same method that found nothing in the money path and
everything here.

The guard has been treating "I parsed it" as "I understand it". Those are
different, and the gap is exactly the size of whatever the next hop does
differently.

---

## F40 — Two panics on the operator recovery path

**Severity: P2.** Found by probing the operator credential path with hostile
stored parameters rather than reading it and concluding it was sound.

### F40.1 — What held up

The primitives are right and I could not break them:

- **Argon2id**, t=3, m=64 MiB, p=4, 32-byte key. Costly on purpose: the verifier
  sits in a file that may be copied, so offline guessing is the threat model.
- **`subtle.ConstantTimeCompare`** for the hash comparison.
- **`opauth.Grant` cannot be forged outside the package.** Its fields are
  unexported and the only constructor that sets `ok = true` is `Authenticate`,
  which requires `Verify` to pass first. `lifecycle.ResolveInDoubt` refuses a
  zero-value Grant.
- **`resolveInDoubt` demands the action already be IN_DOUBT**, so a resolution
  cannot be replayed, and it writes the state change and the audit record in one
  transaction or neither.
- **The automatic path back to AVAILABLE is correctly fenced.** Three callers,
  all pre-dispatch: a rate-window write failure, a re-encode failure, and a child
  write that moved **zero** bytes. A partial write goes IN_DOUBT instead, because
  bytes the child accepted may already have reached Razorpay. That distinction is
  the one that matters and it is drawn correctly.

### F40.2 — Verify handed unvalidated parameters to argon2

`Verify` parses `t`, `m` and `p` out of the stored verifier string and passed
them straight to `argon2.IDKey`. Reproduced:

```
t=0   PANIC  argon2: number of rounds too small
p=0   PANIC  argon2: parallelism degree too low
```

and `m` near uint32 max asks for a **4 TiB allocation**.

**Not a privilege escalation.** Anyone who can write that file already controls
the credential; there is nothing to escalate to. What it is instead is a crash on
the **operator recovery path** — the tool you reach for when a refund is already
IN_DOUBT and money may have moved. A truncated write or an interrupted sync gets
there with no attacker at all, and a Go stack trace is a poor thing to meet at
that moment.

**Fixed.** `checkArgonParams` runs before argon2 sees anything: it refuses
parameters that would panic (`t<1`, `p<1`), that violate argon2's own
`m >= 8*p`, or that exceed sane upper bounds. The exhaustion cases are now
refused *before* the allocation is attempted, which is why they can be tested at
all.

### F40.3 — A downgraded credential verified successfully

Separate from the panics and arguably worse: `argon2id$1$8$1$...` — trivially
weak — **verified fine**. A credential quietly downgraded to parameters that can
be brute-forced offline kept reporting success, with no signal anywhere.

**Fixed with a floor**, reported through its own error (`ErrWeakVerifier`) so an
operator mid-incident can tell "this file is corrupt, that's why nothing works"
from "this file must be re-provisioned". The floor sits deliberately *below* the
constants this build writes, so raising them later strengthens new credentials
without locking anyone out of recovery; going beneath the floor requires
re-provisioning, which is the correct response to a downgrade.

A test asserts the floor is below what provisioning produces — otherwise the two
halves of the system disagree and a freshly created credential is refused by its
own verifier.

### F40.4 — The arm D gate caught this change too

`internal/opauth` is in the scorer's import closure, so hardening it moved the
decision-path hash and `rzp-armd verify` failed, exactly as it did for the
`internal/policy` change in F39.3.

Superseded with a stated reason rather than re-stamped. `manifest.json` now
carries **two** entries in `superseded_decision_paths`, each with its hash, both
dates, the reason, and a recorded confirmation that the published matrix still
reproduced. Two entries is the point: the trail is accumulating rather than being
overwritten, which is the difference between a freeze that can be audited and one
that can be quietly reapplied.

The matrix is unchanged both times: TP 54 FP 17 TN 19 FN 0.

---

## F41 — Arm E scored: the first measured recall, and it is not 1.000

Not a defect. Recorded here because this file is the project's record of what is
true, and the result changes two things that were stated as limitations.

### The result

```
TP 22   FP 30   TN 36   FN 8        96 of 120 rows scored, 2 raters
recall 0.733  [0.600 - 0.900]       FPR 0.455  [0.407 - 0.493]
precision 0.423                     Cohen's kappa 0.604
```

Ground truth is the agreement of two people who never saw the code, the compiled
authorization, or the guard's decision. The corpus carries no label field, so the
scorer could not have branched on an authored one. Intervals are cluster-robust
over the six intent sentences, per `PROTOCOL-armE-AMENDMENT-1.md`.

**All five pre-registered predictions held**, which is a weaker statement than it
sounds — E3 was near-certain and E1/E2 were the design working. E1 was still a
genuine risk: had the `coverage=over` cells not produced rows that humans called
out-of-intent *and* the guard forwarded, recall would have returned 1.000 and arm
E would have repeated arm D's tautology with extra steps.

### What it exposes about the guard

**Eight false negatives, 444,000 paise, all in `coverage=over` and nowhere
else.** The guard forwarded refunds that two independent readers called
out-of-intent, because the compiled authorization covered more than the
merchant's sentence asked for. `scoped_partial/over` is the sharpest: the
merchant wrote *"refund the delivery charge only, the items are not to be
refunded"* and the authorization still covered the items.

The verifier did not miss once on matching. It is exactly as good as the
authorization it is handed, and the gap belongs to the intent-to-authorization
compiler. That is now measured rather than asserted.

### It moved the economics AGAINST the guard

`FP-COST.md` computed break-even from arm D, where recall was 1.000 **by
construction**, so the false-negative term was assumed zero.

```
arm D (assumed)    TPR 1.000  FPR 0.472  ->  break-even 5.6%
arm E (measured)   TPR 0.733  FPR 0.455  ->  break-even 7.2%
across intervals   TPR .600-.900  FPR .407-.493  ->  5.4% - 9.3%
```

A control that misses a quarter of what it exists for is **harder** to justify,
not easier. Arm C's observed 0.6% (scenario-clustered 0.00–1.57%) is still below
every one of those bands, so the conclusion is unchanged and the margin is wider
than arm D implied — but the honest direction of the correction is against the
project's own case, which is why it is stated first.

`FP-COST.md` is superseded in part rather than rewritten: the arm D arithmetic is
left intact below a dated banner, because the reasoning is unchanged and only the
inputs moved.

### 24 rows produced no ground truth, and that is the finding

Where the merchant wrote *"please take care of the refund"* with no amount, the
two raters disagreed on **all 24** — one read blanket delegation, the other read
insufficient information — while agreeing on **96 of 96** of everything else.

The disagreement is total and confined to the one construct designed to be
arguable. A third rater would have produced a 2-1 majority and buried a real
50/50. **A merchant instruction with no stated amount is not verifiable**, and
those rows are excluded from every metric and listed individually.

### Interventions on record

Both returned files were queried once for internal contradictions — rows where a
rater answered the same question two different ways. Each was shown all the rows
in the group with **no indication of which was correct**, and each rater resolved
it themselves. Rater 1 had one group, rater 2 had two. Feeding anything back to a
rater is an intervention and belongs in the record even when it changed no
ground truth.

Rater 1 also returned `inbound`/`outbound` instead of the specified vocabulary
and was asked to correct it. **The values were not renamed on their behalf** —
renaming a label is editing a judgment, and the whole arm depends on not doing
that. Rater 2's first file was Windows-1252 rather than UTF-8; the second was
clean.

### The headline changed

`README.md` no longer says the project does not meet the Track 2 precision/recall
bar, because precision and recall now exist and are independently labelled. It
says instead what they are, what the intervals are, that the traffic is
constructed, and that eight requests got through.
