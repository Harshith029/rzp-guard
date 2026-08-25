# One-off Test Mode observation: duplicate receipt behaviour

**Status: a single manual observation, not reproducible evidence, and NOT part of
the defence-in-depth claim.** Read the limits before the finding.

## What was observed

On 2026-08-25, a second `create_refund` was sent for `pay_TTwUH29tzhB4ME`, 100
paise, carrying a receipt already used by refund `rfnd_TTwf8Hhbx0sjZQ`. Razorpay
answered:

```
isError: true
creating refund failed: Duplicate receipt found for this refund request.
```

## Why this is not evidence

Three reasons, and they matter more than the finding:

1. **No raw response was captured.** What is above is the flattened error text
   transcribed by hand. There is no committed envelope a reader can re-check.

2. **It cannot be reproduced from this repository, by design.** Observing it
   requires sending a refund to Razorpay *around* the guard — the guard blocks
   the replay itself, which is the point. A command that does that is a generic
   refund launcher, and this repository deliberately does not ship one. The
   earlier version of this file described that probe as a reusable procedure;
   that description has been removed.

3. **The scope tested was narrow** — same receipt, same payment, same amount.
   Nothing was established about uniqueness scoping beyond that.

## What is actually claimed, after the correction

**Replay protection rests on one verified layer: the guard's durable action
ledger.** A replay is refused locally with `ACTION_CONSUMED`, forwarding nothing.
That is proven by committed evidence and re-checkable at any time with
`./run.sh verify-refund-evidence`.

The provider-side receipt behaviour is recorded here as an **observation that
suggests** a second layer exists. It is not counted as defence in depth, and no
README or PLAN claim rests on it.

## A further limit, on the "survives ledger loss" idea

Even taking the observation at face value, the backstop only holds when a retry
reproduces the **identical receipt** — which requires the identical `mandate_id`
and `action_id`. That is the normal case for a real deployment, because a mandate
is a file the merchant wrote and reloading it yields the same ids. It is *not*
automatic: any process that mints fresh action ids per run produces fresh
receipts and gets no provider-side protection at all.

The now-removed `live-refund` command generated its action id from the clock, so
it would have had exactly this hole. That is a good illustration of why the claim
needed narrowing rather than restating.
