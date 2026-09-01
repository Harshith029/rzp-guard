# rzp-guard

**Stops an AI agent issuing a refund nobody approved.**

It sits between the agent and Razorpay's official MCP server, and lets a refund
through only if a merchant wrote that exact refund down in advance.

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
list of specific refunds they will allow, each naming one payment and one exact
amount. The guard forwards a refund only if it matches an unused entry.

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
                    ├─ does the mandate authorize it?      exact payment + amount
                    ├─ reserve the entry BEFORE forwarding durable, survives a crash
                    └─ commit only on a matching receipt   or mark IN_DOUBT
```

Every authorized refund moves through four states, written to disk before
anything is forwarded:

| state | meaning |
|---|---|
| `AVAILABLE` | the merchant approved it; unused |
| `RESERVED` | in flight — written to disk *before* the request leaves |
| `COMMITTED` | the provider created a refund matching our receipt |
| `IN_DOUBT` | we could not confirm the outcome — a human must resolve it |

**`IN_DOUBT` is the important one.** If the connection drops mid-refund, the
guard does not guess. It does not retry, and it does not release the
authorization. The entry stays locked and an operator is told, because
"probably fine" is not a safe assumption about someone else's money.

## Evidence

| what | where |
|---|---|
| Continuous integration | [Actions](https://github.com/Harshith029/rzp-guard/actions) — every push runs the full gate set on Linux |
| A real refund really executed | `evidence/g16/` — Test Mode, receipt round-tripped, replay refused |
| A real refund really blocked | `evidence/linux/` — against the official pinned container |
| Attacks that once worked | `./run.sh redteam-negative` — 10 cases, each a bypass that succeeded once and must now fail |

The red-team suite is exhaustive about its own gaps: it prints
`blocked / bypassed / skipped`, refuses to summarise a partial run, and states
that a run with any skipped case is **not** a clean result.

## Running it safely

Nothing below touches real money or needs any credential.

```bash
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

This repository contains no tool that can move money outside the guard. There is
no payment or order creation, no checkout flow, no direct Razorpay client, no
refund launcher, and no realistic card or token fixtures. Adversarial test cases
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

**What is underway:** the guard refused **72 of those 340 calls**. An exhaustive
audit of every one of them is with two external raters, who see a sanitised view
of each call and are not told what the rows have in common. It reports one
conditional quantity — *in-intent calls among refused calls* — split so that a
refusal correctly enforcing an incomplete mandate is never counted as a guard
defect. It is **post-hoc, not pre-registered**, it cannot repair the failed
recall experiment, and **no result is published until those labels return.**

Details: `study/PRELABEL-FINDING-armC.md`, `study/PROTOCOL-armC-AUDIT.md`.

## Known limits

**Mandates are not signed.** The guard reads the mandate from disk and does not
verify who wrote it. Anyone who can write that file can grant authority —
including after the fact. So the honest claim is narrow: the guard enforces that
an agent cannot exceed authority *someone else wrote down*, and makes every
grant single-use, durable and auditable. It does not establish that the grant
was legitimate. Closing this needs merchant-side signing with a key the guard
host does not hold.

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
| `evidence/` | live-gate projections and CI references |
| `FAILURES.md` | what broke, why, and what was changed — including defects found in the fixes for earlier defects |
| `ARCHITECTURE.md` | design and the reasoning behind the capability model |
| `OPERATIONS.md` | running it, and resolving a stuck refund |
