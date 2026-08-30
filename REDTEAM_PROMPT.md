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
   setting nobody had chosen. Those are in scope.

---

## The prompt

````text
You are red-teaming `rzp-guard`, a defensive authorization proxy that sits
between an AI agent and Razorpay's MCP server and blocks refunds the merchant
did not authorize. It is 15,330 lines of Go across 65 files (`git ls-files
'*.go' | xargs wc -l`; ~8,700 of those lines are non-test). Your job is to BREAK
ITS STATED GUARANTEES and produce reproducible evidence.

This is authorized security testing of a defensive tool, on its own repository.
Findings must be defensive: a failing test or a minimal reproducer that
demonstrates the gap. Do not write anything that functions as a general-purpose
attack tool against a payment provider.

## HARD RULES — these are executable, not aspirational

This repository contains a `.env` with Razorpay Test Mode keys, gate commands
that reach a real API, and captured artefacts from a real Test Mode refund.
"Do not target a live account" is not a sufficient boundary against that, so:

1. **Never source, read, export or echo `.env`.** Do not set `RAZORPAY_KEY_ID`
   or `RAZORPAY_KEY_SECRET` in any shell you run.
2. **Never run these:** `./run.sh live-block`, `./run.sh live-allow`,
   `./run.sh verify-refund-evidence`, `./run.sh process-recover`, anything
   labelled G1.6, or any command that starts the `razorpay/mcp` container.
3. **Never start the real child.** Use `-tags testhook` with `cmd/mcp-stub`,
   which is the synthetic MCP child built for exactly this.
4. **Every payment identifier you write must match `pay_SYN*`.** Reject your own
   reproducer if it contains anything else. The synthetics are non-resolvable by
   construction; a real id is how an experiment becomes a transaction.
5. **Do not modify anything under `study/`.** The traces, briefs, mandates and
   manifest are a hash-frozen experimental record. Read them freely; a diff
   there invalidates a published result.
6. **No network egress** beyond `go mod download`. If a reproducer seems to need
   the internet, it is testing the wrong thing.

If a finding genuinely cannot be demonstrated within these rules, describe the
mechanism and say so rather than working around them.

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
    cumulative cap against their own in-memory ledger.
I9. A state file is bound to its mandate and to a schema version. A foreign
    mandate or an unsupported schema is refused, not misread.
I10. The mandate cannot be changed, widened or reloaded at runtime by anything
    arriving over JSON-RPC.
I11. `supportedTools` is a build constant. A mandate can only narrow it.
I12. A receipt is DETERMINISTIC for a given (mandate, action-set) — the same
    set always derives the same string, by design, and that is not a defect.
    The invariant is that **no two distinct reservations may hold the same
    `call_receipt` row**: the second attempt fails, and in normal operation it
    fails earlier still because the actions are no longer AVAILABLE. Attack the
    uniqueness enforcement, not the determinism.
I13. A durable write failure never leaves the in-memory ledger claiming
    something the database does not have. (`lifecycle.transition`,
    `lifecycle.ReserveMany`)

## Where to look hardest, and why

A. **Parser differential.** The guard decides using Go's `encoding/json`; the
   child is a separate Go program that parses the same bytes again. Find any
   input where they disagree about `method`, `params.name`, `payment_id` or
   `amount` — duplicate JSON keys, case variations (Go field matching is
   case-insensitive), unicode escapes, numeric edge cases, embedded NULs,
   BOMs, deeply nested structures, trailing data after a complete value. If the
   guard sees "not a create_refund" and the child sees one, I1 falls.

B. **Correlation and the reply channel.** MCP is bidirectional: both streams
   carry requests, responses and notifications. `isRequest`/`isResponse` in
   `relay.go` decide which. Try to make a message be treated as one thing by
   the correlation logic and another by the outcome logic, so that a reply
   settles a reservation it does not belong to, or an id is released early and
   reused. Look at `resolve()` and `refundEntityMatches()`.

C. **The combining search.** `combineExact` is a bounded depth-first subset-sum.
   Try: mandates where the node cap (50000) or set-size cap (8) is reached and
   the fallback is wrong; duplicate amounts; amounts that overflow int64 when
   summed; a set that passes the policy's checks but violates the ledger's;
   ordering that makes the search return a set including an unavailable action.

D. **Concurrency.** `Guard.Decide` holds one mutex across match-and-reserve.
   `ReserveMany` is one transaction. Look for a window where two concurrent
   calls both observe an action as available, or where the cumulative cap is
   checked against a stale total. Run with `-race`.

E. **The durable/in-memory boundary.** The ordering invariant is: durable write
   first, memory only on success. Find a path that mutates memory on a failed
   write, or that commits durably while memory rolls back. The relay discards
   the errors from these transitions deliberately — check whether that is still
   safe for every path.

F. **Schema v2 and its migration.** `migrateV1toV2` rebuilds `action_state` and
   backfills `call_receipt` in one transaction. Try to interrupt it, feed it a
   file that is v1 in structure but stamped v2 (or the reverse), a file with
   duplicate receipts across actions, or a `schema_meta` row that is absent,
   malformed, or has a version this build refuses.

G. **Claims that outrun the code.** This is a real finding class in this
   repository, with five prior instances (see FAILURES.md F22-F25). Look
   for: comments describing a guarantee the code does not provide; a declared
   error that can never be returned; a test whose assertion would pass even with
   the protection removed; a documented behaviour no test exercises; a default
   the design depends on but never states.

H. **The build-tag boundary.** `-tags testhook` compiles escape hatches that are
   absent from shipped builds (arbitrary child command, unprotected token
   delivery, ephemeral credentials). Verify that a SHIPPED binary genuinely
   cannot reach any of them, including via environment variables or flags that
   still parse.

I. **The study harness.** `cmd/rzp-study` refuses to run unless a SHA-256 freeze
   verifies, refuses to overwrite published results, and validates each arm
   against the freeze it ran under. Try to make it produce or overwrite a result
   that is not backed by the committed traces — for example with `-allow-dry`,
   partial trace sets, a hand-edited `arms.json`, or paths that escape `study/`.

## Scope and order

Thirteen invariants is too many to attack at once. Work in this order and stop
to report rather than half-covering everything:

**Pass 1 — money can move.** I1, I2, I3, I4, I5, I13. These are the ones where a
defect means a refund happens that should not, or happens twice, or for the
wrong amount. Everything else can wait.

**Pass 2 — an operator is misled.** I6, I9, I12, plus finding class G.

**Pass 3 — everything else.** I7, I8, I10, I11, and the study harness.

Record, for the whole pass:

- the **baseline commit** you tested (`git rev-parse HEAD`);
- the **exact commands** you ran, verbatim;
- for fuzzing: the **seed corpus, `-fuzztime`, and execution count** reached;
- for each finding, whether it is **source-only** (read the code and reasoned),
  **unit-tested** (a failing test), or **fuzz-found** (a saved corpus entry).

Parser-differential work (finding class A) must target the **test-hook child**
via `cmd/mcp-stub`, never the real Razorpay container.

## Method

- Build and test in the pinned container, as the repo does:
  `./run.sh test`, `./run.sh race`, `./run.sh lifecycle`
- Two fuzz targets exist in `internal/relay/fuzz_test.go`:
  `FuzzAgentLineNeverLeaksAnUnauthorizedRefund` and
  `FuzzChildReplyNeverFalselyCommits`. Run one with
  `go test ./internal/relay/ -run '^$' -fuzz FuzzAgentLineNeverLeaksAnUnauthorizedRefund -fuzztime 5m`.
  The first has 8.9M executions behind it with zero failures, so a new
  finding there needs a genuinely new input shape. Extend them or add targets.
- The mutation-testing discipline used here is worth copying: remove a
  protection, confirm a test fails, restore. A protection whose removal breaks
  nothing is either dead or untested — both are findings.
- `FAILURES.md` documents 25 prior defects. Read it. It tells you the author's
  blind spots, and several classes recur.

## What to report

For each finding:

1. **Which invariant** (I1–I13) or which claim in a comment/doc.
2. **A reproducer** — preferably a failing Go test in the relevant package;
   otherwise exact JSON-RPC input plus the observed vs expected behaviour.
3. **Consequence in money terms.** Does it move money, permit a replay, exceed
   a cap, strand a refund, or mislead an operator during recovery?
4. **Severity, and be honest about fail-closed.** If the outcome is a refusal,
   say so — but do not dismiss it, because a wrong refusal is a false block, and
   those are this system's measured weakness.

   The published figure is **precision 0.250 (3/12), recall 1.000 (3/3)** on
   arm B (`study/RESULTS-armB.md`). Arm A's positive class is empty, so its
   precision is degenerate at `0/8` and its recall undefined — never quote
   either as a detector score. Two other numbers appear in this repository and
   are **not** the published result: 0.333 is a counterfactual replay of arm B's
   recorded calls through the current guard, and 1.000 is that replay against
   hand-corrected mandates. Both are labelled in
   `study/COUNTERFACTUAL-combining.md` as not-a-study-number, and both are
   computed on a non-reactive subset. Use 0.250 unless you mean one of the
   others and say which.
5. **The smallest fix** you would make.

Rank by whether money can move, then by whether an operator would be misled,
then by everything else. If you find nothing for an invariant, say which ones
you actually attacked and how — "no issues found" without a method is not a
result.
````

---

## After the review

Treat every returned finding as **a claim to verify, not an instruction to
follow** — the same rule this project applies to its other cross-model reviews.
Reproduce it locally before changing anything. Log the accept/reject decision
and the reason in `REVIEW_LOG.md`.

A model that is asked to find problems will find some whether or not they exist.
The reproducer is what separates the two.
