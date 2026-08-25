# Probe: is the injected receipt a provider-side idempotency key?

**Date:** 2026-08-25 · **Mode:** Razorpay Test Mode · **Result: yes, enforced by Razorpay.**

## Why this was probed

`ReceiptFor(mandate_id, action_id)` is deterministic, and the guard injects it into
every forwarded `create_refund`. The design notes claimed this made a duplicate
"detectable at Razorpay." That was an assumption. If it were false, the guard's
replay protection would rest entirely on its own local SQLite ledger, and the
docs would have been overclaiming defence in depth.

## What was sent

A second `create_refund` for `pay_TTwUH29tzhB4ME`, 100 paise, carrying the
receipt `rzpg_6b8602afde6b` — already used by refund `rfnd_TTwf8Hhbx0sjZQ`.

Sent **directly to the official pinned MCP container**, deliberately bypassing
the guard. The guard blocks this replay itself (`ACTION_CONSUMED`), so routing
through it would have tested the guard, not the provider. The point was to learn
what Razorpay does when the guard is *not* in the path.

## Response

```
isError: true
creating refund failed: Duplicate receipt found for this refund request.
```

## What this establishes

Two independent layers refuse the same replay:

| Layer | Mechanism | Verified by |
|---|---|---|
| Primary | durable action ledger, `ACTION_CONSUMED` | `./run.sh live-refund` (gate G1.6) |
| Backstop | provider rejects the duplicate receipt | this probe |

The backstop is what survives the guard's worst case: if the ledger were lost or
rolled back, a retry of the same authorized action still produces the same
receipt, and Razorpay refuses it.

## Scope limit

Tested: same receipt, same payment, same amount. **Not** tested, and therefore
not claimed: whether receipt uniqueness is scoped merchant-wide, e.g. the same
receipt against a different payment. The guard cannot emit that call — an action
is bound to one payment id — so the untested case is not on the defended path.

## Why no script for this is committed

The command was run inline and is described above rather than shipped as a repo
script. A committed executable whose function is "issue a refund straight to
Razorpay, bypassing the guard" is misuse-shaped regardless of intent. The
reproduction is documented in prose; the defended paths are what get runners.
