# rzp-guard

**Stops an AI agent issuing a refund nobody approved.**

It sits between the agent and Razorpay's official MCP server, and lets a refund
through only if a merchant authorized it in advance — normally as an exact
amount, which is the default and the form every shipped mandate uses.

---

## Two minutes

If you only have a few minutes, this is the whole project:

| | |
|---|---|
| **1. Watch it work** | `./run.sh demo` — 15 seconds, no credentials, no network |
| **2. That it works** | `./run.sh test` — unit, lifecycle and race lanes in a digest-pinned container |
| **3. That it cannot be bypassed** | `./run.sh redteam-negative` — ten bypasses that once worked, each must still fail |
| **4. What it costs** | [`study/FP-COST.md`](study/FP-COST.md) — both error directions priced, with the break-even |
| **5. What it has not shown** | [Honest evaluation status](#honest-evaluation-status) — the recall experiment failed, and it is published as a failure |

The one number worth arguing about: **this control breaks even at roughly a 5.6%
out-of-intent base rate, and the only agent traffic we observed ran at 0.6%.**
That is in `study/FP-COST.md`, with the assumptions that produce it and the ones
that would overturn it.

---

## The problem

Merchants are starting to let AI agents handle support. An agent that can read a
customer's message and call a refund API can also be talked into refunding the
wrong thing — by a confused instruction, a mistaken reading of an order, or text
in the customer's own message written to manipulate it.

The agent is not malicious. It is credulous, and it holds a live payments
credential.

**One action, chosen because it is where money actually leaves:** `create_refund`.

## What this is

A **fail-closed authorization verifier**. The merchant issues a *mandate* — a
list of specific refunds they will allow, each naming one payment and one
amount. The guard forwards a refund only if it matches an unused entry.

The amount is normally **exact** — that is the form every shipped mandate uses,
and the form the design argues for. The schema also has an **opt-in bounded**
form (`max_amount_paise`) where the merchant delegates the figure up to a
ceiling. It is deliberately weaker: a bounded entry cannot participate in
combining, and a range authorizes more than a single figure does.

```
merchant writes:  "refund pay_A9F2, exactly 24000 paise, once"

agent asks for:   24000 on pay_A9F2   ->  allowed, entry consumed
agent asks again: 24000 on pay_A9F2   ->  refused, already used
agent asks for:   61500 on pay_A9F2   ->  refused, not authorized
agent asks for:   24000 on pay_B7K1   ->  refused, different payment
```

**It is not a fraud classifier.** It does not score, predict, or judge whether a
refund looks suspicious. It checks a refund against a list. That is deliberate:
a proxy sitting on the wire cannot see the agent's reasoning or the user's
intent, so guessing would add unpredictability to a money path. Everything not
explicitly allowed is refused.

## How it works

```
  AI agent  ──►  rzp-guard  ──►  Razorpay MCP server (official, unmodified)
                    │
                    ├─ is this tool allowed at all?        default-deny
                    ├─ does the mandate authorize it?      payment + amount, entry unused
                    ├─ reserve the entry BEFORE forwarding durable, survives a crash
                    └─ commit only on a matching receipt   or mark IN_DOUBT
```

Every authorized refund moves through four states. **`RESERVED` is written to
disk before any byte is forwarded**; the later transitions are persisted once
the outcome is known, which is the earliest point at which they are decidable:

| state | meaning |
|---|---|
| `AVAILABLE` | the merchant approved it; unused |
| `RESERVED` | in flight — **persisted before the request leaves** |
| `COMMITTED` | the provider created a refund matching our receipt — persisted on reply |
| `IN_DOUBT` | the outcome could not be confirmed — persisted when that is established; a human must resolve it |

**`IN_DOUBT` is the important one.** If the connection drops mid-refund, the
guard does not guess. It does not retry, and it does not release the
authorization. The entry stays locked and an operator is told, because
"probably fine" is not a safe assumption about someone else's money.

## Evidence

| what | where |
|---|---|
| Continuous integration | [Actions](https://github.com/Harshith029/rzp-guard/actions) — every push runs the full gate set on Linux |
| A real refund really executed | `evidence/g16/` — Test Mode, receipt round-tripped, replay refused |
| A prohibited request really blocked | `evidence/linux/` — a non-mandated `create_refund` stopped before the official pinned container's stdin boundary |
| Attacks that once worked | `./run.sh redteam-negative` — 10 cases, each a bypass that succeeded once and must now fail |

The red-team suite covers **ten bypasses that were previously observed to work**.
It cannot be exhaustive over attacks nobody has thought of. What it does
guarantee is that it reports its own gaps: it prints `blocked / bypassed /
skipped`, refuses to summarise a partial run, and states that a run with any
skipped case is **not** a clean result.

## Running it safely

Nothing below touches real money or needs any credential.

```bash
./run.sh demo         # 15s: one refund allowed, four refused, one altered mandate rejected
./run.sh test         # unit tests
./run.sh all          # the Go test lanes, including both race detectors
./run.sh preflight    # scans history for a prohibited launcher signature
./run.sh redteam-negative   # every previously-working bypass must still fail
```

`./run.sh all` runs the **Go test lanes only** — not the red-team suite, the
history scan, the fuzzers or the study. Those are separate commands, reported
separately, and the usage text says so.

**Deployment expectation: Linux, in a container.** The guard is built
`CGO_ENABLED=0` and pinned to a Go toolchain by digest. The red-team lane runs
with `--network=none` and `--pull=never`. Windows works for development, with
one caveat the suite prints itself: one red-team case needs a host that can
create symlinks, so it can only run on Linux.

## Defence-only

**Across the released command surface** — the 25 commands `run.sh` exposes —
there is no payment or order creation, no checkout flow, no direct Razorpay
client, no refund launcher, and no realistic card or token fixtures. That is a
statement about what this repository ships, verified by reading that surface;
it is not a proof that no combination of code here could ever be repurposed. Adversarial test cases
are clearly labelled evaluation fixtures using non-resolvable synthetic ids; none
is a reusable exploit.

`./run.sh preflight` scans all reachable history for the signature of a
self-authorizing refund launcher. **It is a text-pattern scan.** A pass means no
reachable source matched that signature — it is a regression tripwire, and it
cannot prove the absence of every offense-capable construction.

## Honest evaluation status

**This project does not meet the Track 2 precision/recall bar, and the headline
result is a failed experiment.**

**Arm C** was the serious attempt: a 54-scenario grid generated mechanically
rather than chosen by the author, the policy frozen by hash beforehand, and
predictions written down in advance. It produced **162 traces and 340 refund
calls**, of which a mechanical application of the labelling rule finds **two**
candidate out-of-intent calls. The pre-registered prediction required at least
twenty. **It failed decisively, and recall cannot be estimated from two.**

In the scenarios containing an injected instruction, **113 refund calls were
emitted and none was mechanically classified out-of-intent**. That is a fact
about this corpus, this prompt and one endpoint on one day. It is *not* evidence
that any model generally resists injection.

Earlier arms A and B are descriptive traces over fifteen scenarios the author
wrote and labelled. **Their precision and recall figures are not detector scores
and must not be quoted as such.**

**Arm D** is a pre-registered, same-author synthetic conformance corpus of 90
constructed refund requests, scored against author-declared labels. Its
confusion matrix is exact for that finite grid, but it is **not independently
labelled or policy-blind, and it does not establish transferable recall,
precision, or false-positive cost.** It was published on 2026-09-01 as a metric
result and that claim was withdrawn the same day; the original documents are
preserved unedited and the retraction is
[`study/ASSESSMENT-armD.md`](study/ASSESSMENT-armD.md). **It does not change the
line above, and it does not repair arm C.** What it is worth is engineering: a
reproducible conformance suite, pinned by a manifest that `go test` enforces,
and one bounded-search limitation reduced to a single deterministic case.

**Arm E is running now.** A 120-row corpus whose ground truth comes from three
independent raters who see the merchant's intent and the requested refund, and
never the compiled authorization or the guard's decision. The pre-registration
([`study/PROTOCOL-armE.md`](study/PROTOCOL-armE.md)) was committed before the
corpus existed; the corpus file carries **no label field**, so the scorer cannot
score against an author-declared label — the defect that withdrew arm D. Unlike
arm D, false negatives are reachable by construction: 10 rows are forwarded above
their stated intent, so recall is a measurement rather than a restatement.
**Labels are out and no result exists yet.** If they return, this section reports
precision, recall and false-positive rate with Wilson intervals and inter-rater
agreement; if they do not, it says so.

**What is prepared, not yet run:** the guard refused **72 of those 340 calls**.
Two blinded external-rater worksheets covering every one of them are prepared
but **have not been distributed, and no external result exists.** Raters would
see a sanitised view of each call and would not be told what the rows have in
common. It is designed to report one
conditional quantity — *in-intent calls among refused calls* — split so that a
refusal correctly enforcing an incomplete mandate is never counted as a guard
defect. It is **post-hoc, not pre-registered**, it cannot repair the failed
recall experiment, and **no result is published until those labels return.**

Details: `study/PRELABEL-FINDING-armC.md`, `study/PROTOCOL-armC-AUDIT.md`.

## Known limits

**Mandate signing is available but off by default.** Without
`-mandate-pubkey`, the guard reads the mandate from disk and does not verify who
wrote it: anyone who can write that file can grant authority, including after
the fact. With a key configured, the mandate must carry a valid ed25519
signature over its exact bytes at `<mandate>.sig`, and an unsigned or altered
mandate refuses to start — verified before parsing, so no re-serialisation sits
between what was signed and what is enforced.

It is opt-in because every fixture in this repository is unsigned, and defaulting
it on would break them all. **Nothing here is enforced by default, and the
unconfigured path prints a warning saying exactly what is not being checked.**
Signing also authenticates the file, not the human: a compromised key issues
mandates the guard will honour, and key custody is outside this program.

So the claim stays narrow. The guard enforces that an agent cannot exceed the
authority *presented to it*, and makes every grant single-use, durable and
auditable.

**Combining is deliberately bounded.** A refund can be covered by several
authorized entries summing to it, but the search stops at eight. Exact
subset-sum is exponential and the amount is chosen by the agent, so an unbounded
search is computation an untrusted party controls. The bound fails closed: it
refuses rather than spends. In arm C this refused **nine** refunds whose entries
summed exactly to the requested amount — the measured price of that trade, not a
verdict on whether the bound is set correctly.

**`COMMITTED` means created, not settled.** The provider confirmed a refund
entity exists. No synchronous reply can prove money moved.

**The evaluation is synthetic.** Model-generated traffic through a third-party
endpoint that was measured serving a different model than requested. Not
merchant traffic, and no claim is made about which model produced the calls.

## Layout

| | |
|---|---|
| `cmd/`, `internal/` | the guard, the operator tool, the study runner |
| `study/` | the evaluation: protocol, corpus, traces, results |
| `study/FP-COST.md` | what each error direction costs, and the break-even base rate |
| `evidence/` | live-gate projections and CI references |
| `FAILURES.md` | what broke, why, and what was changed — including defects found in the fixes for earlier defects |
| `ARCHITECTURE.md` | design and the reasoning behind the capability model |
| `OPERATIONS.md` | running it, and resolving a stuck refund |
