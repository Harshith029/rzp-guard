# Architecture walkthrough

How `rzp-guard` is put together, and why each piece is shaped the way it is.
[README.md](README.md) says what is proven; this says how it works.

---

## 1. The shape of the problem

An AI agent is given tools that move money. The tools are Razorpay's official MCP
server — unmodified, pinned by digest. The agent is a language model, so its
behaviour is not something you can specify or test exhaustively.

The usual instinct is to make the agent safer. This does the opposite: it assumes
the agent is untrustworthy and puts an authorization boundary between it and the
money, so that *no* agent behaviour can produce an unauthorized refund.

```
   ┌─────────┐   JSON-RPC    ┌───────────┐   JSON-RPC   ┌──────────────────┐
   │  agent  │ ────stdio───▶ │ rzp-guard │ ───stdio───▶ │ razorpay/mcp     │
   │ (model) │ ◀──────────── │           │ ◀─────────── │ (official, pinned)│
   └─────────┘               └─────┬─────┘              └──────────────────┘
                                   │
                             ┌─────▼──────┐
                             │ SQLite     │  durable action ledger
                             │ state file │  + audit + operator verifier
                             └────────────┘
```

The guard is a **transparent stdio proxy**. It speaks MCP in both directions and
the child never learns it exists.

### What the guard can see, and what follows from it

It observes JSON-RPC frames. It never sees the user's prompt, the agent's
reasoning, or the conversation. Every design decision below follows from that
single constraint: **it cannot judge intent, so it does not try.** It checks a
call against a merchant-issued list and nothing else.

---

## 2. The request path

Follow one `create_refund` from the agent to Razorpay.

**`cmd/rzp-guard/main.go`** starts the child, wires two pumps, and supervises the
process lifetime. Both directions run concurrently; a shutdown on either side
tears down the other.

**`internal/relay/relay.go` → `handleAgentLine`** — every frame from the agent.

1. **Is it a request?** MCP is bidirectional; the server may send requests to the
   client. Correlation keys off *requests only*. An earlier version keyed off
   "has an id", which conflated a reply travelling agent→child with a new
   outstanding request, and could settle an unrelated refund.
2. **Is it a money-moving tool without an id?** Refused. A refund sent as a
   notification has no reply, so its reservation could never be resolved or
   recovered — an un-answerable refund is one nobody can be accountable for.
3. **`guard.Decide(...)`** — the authorization decision (§3).
4. **Denied?** Answer the agent with the reason. **Write nothing to the child.**
5. **Allowed?** Record the reservation, rewrite the arguments (§4), forward.

**`PumpChild`** reads replies and calls `resolve`, which moves the reservation to
its outcome (§5).

---

## 3. The authorization model

`internal/mandate/mandate.go`, `internal/policy/policy.go`.

The merchant issues a **capability list**, loaded from a path given at process
launch, before any agent connects. Nothing arriving over JSON-RPC can set,
replace, extend or reload it.

Authorization is per **discrete action**, not a policy range:

```json
{ "action_id": "rfa_001", "payment_id": "pay_...", "amount_paise": 24000 }
```

One refund, one amount, one payment, **consumed when used**.

### Why not "refunds up to ₹500 on orders under 30 days"

A range authorizes an unbounded number of refunds inside it. A compromised or
confused agent can drain a range without ever violating it. A discrete action
cannot be used twice, so the blast radius of any single mistake is one action.

The cost is real and this project measured it: an agent that batches two
authorized refunds into one call, or splits one into a smaller piece, matches no
action and is refused. That produced **8 false blocks in 49 calls** in the Phase
4b run — see [study/FINDINGS.md](study/FINDINGS.md). It is the main operational
weakness of the design, and it is a deliberate trade, not an oversight.

### Three layers of tool restriction

| Layer | Where | Why |
|---|---|---|
| `supportedTools` — 9 tools | compiled in, `policy.go` | the build cannot reach a tool nobody reviewed, whatever a mandate says |
| `allowed_tools` | the mandate | the merchant narrows further per deployment |
| `authorized_refund_actions` | the mandate | money-moving calls need a specific matching action |

A tool must clear all three. The first is a build-level ceiling precisely so a
mandate cannot raise it.

### Amount handling

Razorpay's `create_refund` declares `amount` as `{"type":"number"}` — verified
from the live schema, not the docs. So `24000.5` is expressible. The guard
**rejects fractions outright** and forwards a canonical `int64`. An early version
authorized `50000.9` against an action for `50000` and forwarded the fraction
(FAILURES.md F1).

---

## 4. Argument rewriting and the receipt

An approved call is not forwarded verbatim. The guard **injects a receipt**:

```
rzpg_ + first 12 hex of sha256(mandate_id, action_id)
```

Deterministic, so the same action always yields the same receipt. The agent's own
`receipt`, if it supplied one, is overwritten.

That last point stopped being hypothetical during the study: a real agent
spontaneously sent `"receipt": "KD-4471-atta-refund"` — the exact field the guard
uses as an idempotency key — and the guard replaced it with `rzpg_7c8da474e0f0`.
**The agent cannot choose the idempotency key.**

If re-encoding the approved call fails for any reason, the original is **not**
forwarded and the reservation is rolled back. Nothing was written, so releasing
is provably safe there.

---

## 5. The lifecycle, and why it fails closed

`internal/lifecycle/lifecycle.go`. Four states:

```
AVAILABLE ──reserve──▶ RESERVED ──┬── provider confirmed the refund ──▶ COMMITTED
                                  │
                                  └── anything else ──────────────────▶ IN_DOUBT
```

**Once bytes have reached the child there is no automatic release.** Only two
automatic outcomes exist: `COMMITTED` and `IN_DOUBT`. `IN_DOUBT` is terminal
until a human resolves it, and it survives restart.

Two assumptions were removed from earlier versions, both of which treated JSON
syntax as execution evidence:

- *a JSON-RPC error proves the request was rejected before execution* — it does
  not; the child can fail after dispatching the HTTP request;
- *any non-error result proves success* — it does not; `result: null`, an
  unparseable body, or a reply that merely shares the id would all have
  committed.

`COMMIT` now requires a refund entity matching **the payment, the amount, and the
injected receipt**. Anything else is `IN_DOUBT`.

### What COMMITTED does and does not mean

It means *the provider created the refund entity*. It does **not** mean the money
settled. The live G1.6 envelope came back `status: "pending"` and only became
`processed` asynchronously, after the MCP reply was already sent — so no
synchronous reply can prove settlement. `COMMITTED` is enough to consume a
single-use action and prevent a replay, and it is not a settlement record.

### Partial writes

If writing to the child fails midway, bytes it accepted may already have reached
Razorpay. Only a write that moved **zero** bytes is provably pre-dispatch and
safe to release; anything else is `IN_DOUBT`.

---

## 6. Durability and the operator boundary

`internal/storage/storage.go`, `internal/opauth/`, `internal/bootstrap/`.

State is SQLite (pure Go, `CGO_ENABLED=0`) with `PRAGMA locking_mode=EXCLUSIVE` —
single-instance ownership, enforced by the database rather than by convention.
Reservations are durable before forwarding, so a crash mid-flight leaves a record
that recovery promotes to `IN_DOUBT` rather than losing.

Resolving an `IN_DOUBT` action is the one privileged operation, and it requires an
`opauth.Grant` — an unforgeable proof of authentication, not a boolean. The
credential is an Argon2id salted verifier; `ResolveInDoubt` is the only exported
path to clearing a reservation.

**The guard refuses to run against an unprovisioned state file.** Otherwise the
first writer wins, and an attacker who creates the state directory first owns the
operator identity.

Credential delivery **fails closed**: the credential is committed only when
delivery is provably durable — token written, fsynced, parent directory fsynced.
Terminal output cannot be proven durable, and Windows cannot fsync a directory,
so **both are refused**. That is why the declared deployment target is Linux.

---

## 7. What it deliberately does not do

| Not done | Why |
|---|---|
| No model in the decision path | The decision is a lookup. An LLM would add latency, nondeterminism and a disclosure burden for a worse answer. |
| No fork of Razorpay's server | The official image is pinned by digest and run unmodified. A fork would need auditing forever. |
| No refund-issuing command in this repo | A tool that takes an arbitrary payment and amount and writes its own authorizing mandate is a refund launcher whatever the intent. One existed; it was removed (FAILURES.md F18). |
| No intent inference | The guard cannot see intent. Guessing at it would be a detector that fails silently. |
| Provenance is forensic only | It detects a narrow literal-flow subclass and never gates a decision. |

---

## 8. Where the evidence lives

| Claim | Where |
|---|---|
| An unauthorized refund never reaches Razorpay | `./run.sh live-block` — 15 assertions against the pinned container |
| An authorized refund does, and the receipt round-trips | G1.6, `evidence/g16/`, `./run.sh verify-refund-evidence` |
| Child death leaves a durable `IN_DOUBT` surviving restart | `./run.sh process-recover` — 6 assertions |
| What the guard did against real agent traffic | [study/RESULTS.md](study/RESULTS.md), [study/FINDINGS.md](study/FINDINGS.md) |
| Everything that went wrong on the way | [FAILURES.md](FAILURES.md) — 21 entries, each with the real output |

Every gate re-runs against the **committed redacted projection**, not a private
artifact, so the published evidence is exactly what the assertions checked.
