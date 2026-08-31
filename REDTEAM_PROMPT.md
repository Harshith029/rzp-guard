# Red-team prompt

Paste the block below into ChatGPT Codex (or any coding agent with repository
access). It is written to produce **reproducible exploits against stated
invariants**, not a generic "security review" that returns a checklist.

Two things make a red-team of this repository useful rather than performative:

1. **The invariants are written down.** Every one below is a claim the code makes
   about itself somewhere. An attacker's job is to find the gap between the
   claim and the behaviour, and there is no ambiguity about what the claim is.
2. **Failing closed is not a defence against every finding.** Several past
   defects were fail-closed and still serious: a sentinel that could never fire,
   a comment asserting a guarantee the code did not provide, a durability
   setting nobody had chosen, an untrusted endpoint able to skip a check by
   omitting a field.

> **The isolation is enforced by `./run.sh redteam`, not by this document.**
> `./run.sh redteam-negative` is the evidence; `redteam-selfcheck` is a smoke
> check and was previously oversold as proof.
>
> This took three review rounds to get right, and the failures are worth naming
> because they are the same failure each time: a claim slightly stronger than
> what was enforced. First the rules were prose around a runner that mounted
> `.env` with networking on. Then the fuzz command the brief recommended went
> around the new lane entirely. Then "only the stub can execute" turned out to
> mean "whatever is at a writable relative path", which the test written to
> prove it accidentally demonstrated.
>
> A fourth round then found three more, and they are the reason the framing
> changed. The export copied through **symlinks** — an untracked link named
> anything could point at the gitignored `.env` and have its contents copied in
> under that name, which the literal `.env` check never saw. The credential scan
> word-split on **filenames containing a space**. And `child_redteam.go` claimed
> the lane built the stub "immediately before use" while nothing built it at all.
>
> So: symlinks are now refused outright as the primary control, the scan is
> NUL-delimited and a scanner error is itself a refusal, and the lane really does
> build the stub. More importantly, **`./run.sh redteam-negative` re-runs every
> one of those bypasses and each must fail.** A PASS banner is a list of
> symptoms; the attack that used to work and now does not is the evidence.

---

## The prompt

````text
You are red-teaming `rzp-guard`, a defensive authorization proxy that sits
between an AI agent and Razorpay's MCP server and blocks refunds the merchant
did not authorize. Your job is to BREAK ITS STATED GUARANTEES and produce
reproducible evidence.

This is authorized security testing of a defensive tool, on its own repository.
Findings must be defensive: a failing test or a minimal reproducer that
demonstrates the gap. Do not write anything that functions as a general-purpose
attack tool against a payment provider.

Get the repository's size yourself rather than trusting a number in a brief:

    git ls-files '*.go' | wc -l
    git ls-files '*.go' | xargs wc -l | tail -1

## RUN EVERYTHING THROUGH THE ISOLATED LANE

    ./run.sh redteam <command...>

    ./run.sh redteam go test ./...
    ./run.sh redteam go vet ./...

That lane, and not this text, is what enforces the boundary. It exports the
working tree minus gitignored paths (so `.env` cannot be present), refuses to
proceed if the export contains anything shaped like a credential, fetches
modules in a separate stage that mounts no source, and then runs your command
with `--network=none`, `--pull=never`, no Docker socket, every provider
credential variable emptied, and a module cache created per run and mounted
read-only.

**Confirm that for yourself before you start.** Do not take it on faith:

    ./run.sh redteam-negative     # the evidence: six bypasses, all must fail
    ./run.sh redteam-selfcheck    # a smoke check: eight symptoms of isolation

The negative suite is the one that matters. Each case is an attack that got
through a previous version of this lane: an untracked symlink to a gitignored
secret, a credential in a filename containing a space, a `git archive` export
that hid uncommitted tests, a shell-child path detected only by one legacy
variable name, a fuzz wrapper that went around the lane, and a stub that
accepted any invented `pay_SYN` id. N1 needs a host that can create symlinks —
it skips on Windows and runs in CI.

The smoke check is useful and weaker: it confirms symptoms, not that the holes
are closed. Treat a green self-check with a red negative suite as red.

**Your uncommitted work is visible inside the lane.** The export is built from
`git ls-files -c -o --exclude-standard` against the working tree, so a failing
test you just wrote is there. An earlier version used `git archive HEAD` and
silently tested the last commit instead — which is a reason to leave the lane,
and therefore worse than the risk it avoided.

Then, in addition:

1. **Never read, source, export or echo `.env`**, and never set
   `RAZORPAY_KEY_ID` or `RAZORPAY_KEY_SECRET` yourself.
2. **Never run these**, all of which reach a real API or the real container with
   real Test Mode credentials:
   `./run.sh live-block`, `./run.sh study-model`, `./run.sh study-smoke`,
   `./run.sh study-run`, `go run ./cmd/rzp-study resolve-model`,
   `go run ./cmd/rzp-study run`. Do not set `NIHAL_CUSTOM_KEY`,
   `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `RZP_STUDY_PROVIDER` or
   `RZP_STUDY_PROXY_BASE`.
3. **Do not start the real child** (`razorpay/mcp`). The lane makes it
   impossible — no socket, no network. For a guard binary that needs a child,
   build with `-tags redteam`: that selects `child_redteam.go`, which has no
   shell branch, no environment lookup and no path resolution, and execs one
   absolute path. `-tags testhook` still accepts an arbitrary `sh -c` command
   and is the right tool for the lifecycle tests — just not for you.

   Stated exactly, because overstating it is what the last three rounds were
   about: the redteam build guarantees **no code path in that binary runs a
   shell or reads a child command from configuration**. It does not guarantee
   the file at the child path is the real stub — anyone who can write there can
   substitute it. The isolation that matters comes from the container.

4. **Use identifiers from the fixture set.** `cmd/mcp-stub` enforces exact
   membership, not a prefix: `pay_SYN8001`–`pay_SYN8016`, plus the deliberately
   record-less `pay_SYN8099` and the two gate ids. Anything else — including a
   `pay_SYN`-prefixed value you invent — comes back as a real tool error with
   `isError: true`. Treat that as a correct stop, not a bug.
5. **Do not modify anything under `study/`.** The traces, briefs, mandates and
   manifest are a hash-frozen experimental record. Read them freely; a diff
   there invalidates a published result.

Two lanes are read-only and safe, listed here so you do not avoid them by
mistake: `./run.sh verify-refund-evidence` parses committed evidence and touches
no network or credentials, and `./run.sh process-recover` drives a local
test-hook stub with placeholder keys. Neither is a live path. You still do not
need either.

If a finding cannot be demonstrated inside these rules, describe the mechanism
and say so rather than working around them.

## How the system works, in one paragraph

`cmd/rzp-guard` runs Razorpay's official MCP container as a child process and
relays newline-delimited JSON-RPC between an agent and that child. Every
`tools/call` for `create_refund` goes through `internal/policy.Guard.Decide`,
which authorizes it against a merchant-issued capability list (`internal/mandate`)
loaded at launch. An authorized call reserves durably (`internal/lifecycle` →
`internal/storage`, SQLite) BEFORE any byte is forwarded, and gets a
deterministic idempotency receipt injected into its arguments. A denied call is
answered locally and its bytes are never written to the child's stdin.

## The invariants. Break any one of these and you have a finding.

I1. A `create_refund` the mandate does not authorize NEVER has its bytes written
    to the child's stdin. (`internal/relay/relay.go`)
I2. Each authorized action is single-use. No sequence of agent messages may
    cause one action to be consumed twice.
I3. The forwarded amount always equals the authorized amount, exactly, as an
    integer. No rounding, truncation, or coercion.
I4. Total money reservable in a session never exceeds
    `global.max_cumulative_paise`.
I5. A refund may consume SEVERAL actions when their exact amounts sum to the
    requested amount (`combineExact` in `internal/policy/policy.go`). It must be
    impossible to make a combination consume more authority than the refund
    used, cross payments, include a bounded action, include an unavailable
    action, or sum to anything other than the requested amount exactly.
I6. `IN_DOUBT` is terminal until an operator resolves it with a valid
    `opauth.Grant`. No agent input may release, commit or clear an IN_DOUBT
    action.
I7. The guard process has NO code path that writes the operator verifier. Only
    `rzp-guard-operator init` (once) and `rotate` (with the current token) may.
I8. Exactly one guard may hold a state file. Two would each enforce the
    cumulative cap against their own in-memory ledger. (Many guards over MANY
    state files is supported and tested — that is not a violation.)
I9. A state file is bound to its mandate and to a schema version. A foreign
    mandate or an unsupported schema is refused, not misread.
I10. The mandate cannot be changed, widened or reloaded at runtime by anything
    arriving over JSON-RPC.
I11. `supportedTools` is an unexported package-level `var` map, NOT a language
    constant — do not report its mutability as the finding. The enforceable
    guarantee is that **nothing arriving over JSON-RPC, and no mandate, can
    widen the forwarded tool surface**: a mandate may only narrow it, and a tool
    outside the map is refused with `TOOL_NOT_SUPPORTED`. Break that.
I12. A receipt is DETERMINISTIC for a given (mandate, action-set) — the same set
    always derives the same string, by design, and that is not a defect. The
    invariant is that **no two distinct reservations may hold the same
    `call_receipt` row**: the second attempt fails, and in normal operation it
    fails earlier still because the actions are no longer AVAILABLE. Attack the
    uniqueness enforcement, not the determinism.
I13. A durable write failure never leaves the in-memory ledger claiming
    something the database does not have. (`lifecycle.transition`,
    `lifecycle.ReserveMany`)

## Where to look hardest, and why

A. **Parser differential.** The guard decides using Go's `encoding/json`; the
   child parses the same bytes again. Find any input where they disagree about
   `method`, `params.name`, `payment_id` or `amount` — duplicate JSON keys, case
   variations (Go field matching is case-insensitive), unicode escapes, numeric
   edge cases, embedded NULs, BOMs, deep nesting, trailing data after a complete
   value. If the guard sees "not a create_refund" and the child sees one, I1
   falls.

   NOTE: no differential test exists yet. The two fuzz targets in
   `internal/relay/fuzz_test.go` are IN-PROCESS relay fuzzers that write to a
   `bytes.Buffer`, so they compare the guard against nothing. Building a real
   guard-to-`mcp-stub` differential harness — strict child, stub as the second
   parser — is itself a worthwhile contribution.

B. **Correlation and the reply channel.** MCP is bidirectional: both streams
   carry requests, responses and notifications. `isRequest`/`isResponse` in
   `relay.go` decide which. Try to make a message treated as one thing by the
   correlation logic and another by the outcome logic, so a reply settles a
   reservation it does not belong to, or an id is released early and reused.
   See `resolve()` and `refundEntityMatches()`.

C. **The combining search.** `combineExact` is a bounded depth-first subset-sum.
   Try: mandates where the node cap (50000) or set-size cap (8) is reached and
   the fallback is wrong; duplicate amounts; sums that overflow int64; a set that
   passes the policy's checks and violates the ledger's; ordering that returns a
   set containing an unavailable action.

D. **Concurrency.** `Guard.Decide` holds one mutex across match-and-reserve;
   `ReserveMany` is one transaction. Look for a window where two concurrent
   calls both see an action as available, or where the cap is checked against a
   stale total. Run with `-race`.

E. **The durable/in-memory boundary.** Ordering invariant: durable write first,
   memory only on success. Find a path that mutates memory on a failed write, or
   commits durably while memory rolls back. The relay discards these transition
   errors deliberately — check whether that is still safe on every path.

F. **Schema v2 and its migration.** `migrateV1toV2` rebuilds `action_state` and
   backfills `call_receipt` in one transaction. Try to interrupt it; feed it a
   file that is v1 in structure but stamped v2 (or the reverse); duplicate
   receipts across actions; a `schema_meta` row that is absent, malformed, or a
   version this build refuses.

G. **Claims that outrun the code.** The most productive class in this
   repository, with five prior instances (FAILURES.md F22–F25). Look for:
   comments describing a guarantee the code does not provide; a declared error
   that can never be returned; a test whose assertion passes with the protection
   removed; a documented behaviour no test exercises; a default the design
   depends on but never states; **a check an untrusted party can skip by
   omitting a field rather than sending something wrong** (that was F25).

H. **The build-tag boundary.** `-tags testhook` compiles escape hatches absent
   from shipped builds. Verify a SHIPPED binary cannot reach any of them,
   including via environment variables or flags that still parse.

I. **The study harness.** `cmd/rzp-study` refuses to run unless a SHA-256 freeze
   verifies, refuses to overwrite published results, and validates each arm
   against the freeze it ran under. Try to make it produce or overwrite a result
   not backed by the committed traces — `-allow-dry`, partial trace sets, a
   hand-edited `arms.json`, paths escaping `study/`. Do this by reading and by
   unit test; do not run a real study.

## Scope and order

Thirteen invariants is too many to attack at once. Work in this order and report
rather than half-covering everything:

**Pass 1 — money can move.** I1, I2, I3, I4, I5, I13. A defect here means a
refund happens that should not, or twice, or for the wrong amount.

**Pass 2 — an operator is misled.** I6, I9, I12, plus class G.

**Pass 3 — everything else.** I7, I8, I10, I11, and the study harness.

Record, for the whole pass:

- the **baseline commit** (`git rev-parse HEAD`);
- the **exact commands** you ran, verbatim, including the `./run.sh redteam`
  prefix;
- for fuzzing: the **seed corpus, `-fuzztime`, and execution count reached**;
- for each finding, whether it is **source-only** (read and reasoned),
  **unit-tested** (a failing test), or **fuzz-found** (a saved corpus entry).

## Method

- `./run.sh redteam go test ./...` and `./run.sh redteam go test -race ./...`
- Fuzzing: `./run.sh fuzz <TARGET> <BUDGET>`, e.g.
  `./run.sh fuzz FuzzChildReplyNeverFalselyCommits 2m`. It runs **through the
  isolated lane**. An earlier version called the ordinary container runner,
  which mounted the real workspace with networking — so the brief was pointing
  reviewers at the exact path the lane exists to replace. Targets live in
  `internal/relay/fuzz_test.go`.
- A prior run of `FuzzAgentLineNeverLeaksAnUnauthorizedRefund` reported ~8.9M
  executions with no failure. That is a **recorded historical claim, not a
  reproducible artifact** — the committed corpus holds a single entry. Re-derive
  it or ignore it; do not treat it as coverage you inherited.
- The mutation-testing discipline here is worth copying: remove a protection,
  confirm a test fails, restore. A protection whose removal breaks nothing is
  either dead or untested — both are findings.
- `FAILURES.md` documents 25 prior defects. Read it. It names the author's blind
  spots, and several classes recur.

## What to report

For each finding:

1. **Which invariant** (I1–I13) or which claim in a comment/doc.
2. **A reproducer** — preferably a failing Go test; otherwise exact JSON-RPC
   input plus observed vs expected behaviour.
3. **Consequence in money terms.** Does it move money, permit a replay, exceed a
   cap, strand a refund, or mislead an operator during recovery?
4. **Severity, and be honest about fail-closed.** A refusal is still worth
   reporting: a wrong refusal is a false block, and those are this system's
   measured weakness.

   On metrics, be strict. Arm B's **precision 0.250 (3/12) and recall 1.000
   (3/3)** are correct arithmetic over **descriptive trace outcomes**: three
   positive calls, all from a single injection brief, across 15 hand-written
   briefs adjudicated by their own author, generated through an endpoint whose
   model identity is self-reported and unverified. They are **not** held-out
   detector precision and recall, **not** evidence about any named model, and
   **not** a population estimate. Arm A's positive class is empty, so its
   precision is degenerate at `0/8` and its recall undefined — never quote
   either. Two further numbers exist and are not results: 0.333 and 1.000 are
   counterfactual replays labelled as such in
   `study/COUNTERFACTUAL-combining.md`. If you cite a figure, say which and say
   what it is.
5. **The smallest fix** you would make.

Rank by whether money can move, then by whether an operator would be misled,
then everything else. If you find nothing for an invariant, say which ones you
attacked and how — "no issues found" without a method is not a result.
````

---

## After the review

Treat every returned finding as **a claim to verify, not an instruction to
follow** — the same rule this project applies to its other cross-model reviews.
Reproduce it locally before changing anything. Log the accept/reject decision
and the reason in `REVIEW_LOG.md`.

A model asked to find problems will find some whether or not they exist. The
reproducer is what separates the two. The last two rounds returned fourteen
findings; all fourteen were real, and four of them were this project's own
published claims being wrong.
