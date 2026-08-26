# 5-minute pitch — script and shot list

Timed to 5:00. Every number here is checked against the repository; if you
re-run anything and it differs, say the number you got, not the one below.

The strongest thing this project has is not the code — it is that the evidence
survives being checked. Pitch it that way. Do not oversell; the honesty *is* the
differentiator, and a panel that catches one inflated claim discounts everything
else.

---

## 0:00 – 0:40 — The problem

> "An AI agent is given tools that move money. Razorpay's MCP server exposes
> `create_refund`. The agent is a language model, so you cannot enumerate its
> behaviour and you cannot test it exhaustively.
>
> The usual answer is to make the agent safer — better prompts, better model.
> That is unfalsifiable. I did the opposite: assume the agent is untrustworthy,
> and put an authorization boundary between it and the money, so that no agent
> behaviour can produce an unauthorized refund."

**On screen:** the topology diagram from `ARCHITECTURE.md` §1.

---

## 0:40 – 1:20 — The design decision worth defending

> "The guard sits between agent and server and sees only JSON-RPC. It never sees
> the prompt or the reasoning. So it cannot judge intent — and it does not try.
>
> The merchant issues a capability list: one refund, one amount, one payment,
> consumed when used. Not 'refunds up to ₹500'. A range authorizes an unbounded
> number of refunds inside it; a discrete action cannot be used twice. The blast
> radius of any single mistake is one action."

**On screen:** an `examples/mandate.json` action block.

---

## 1:20 – 2:30 — It works against the real thing

Run live. This is the moment the claim stops being a slide.

```bash
./run.sh live-block
```

> "This is the shipped binary, spawning Razorpay's official container pinned by
> digest, with real Test Mode credentials. An unauthorized refund is requested."

**Point at three lines specifically:**

- `NO money-moving tools/call of any kind reached child stdin (found 0)`
- `blocked response carries the deciding rule NO_AUTHORIZED_ACTION`
- `CONTROL: real container produced a response for the allowed read id 4`

> "That last line matters more than the first. Without it, 'nothing was
> forwarded' would also pass if the container were dead or the credentials
> wrong — exactly the cases the test exists to rule out. So a permitted read has
> to succeed in the same session."

Then the allow path:

> "Blocking everything is easy. G1.6 is the other half: a real refund on a real
> Test Mode payment — `rfnd_TTwsIoEmRPXnBa`, 100 paise. It executed, and the
> receipt the guard minted came back unchanged."

---

## 2:30 – 3:40 — What happened when a real agent drove it

> "Then I ran a pre-registered study. Fifteen briefs, forty-five traces, frozen
> and hash-committed before the first API call. Forty-nine refund calls."

**On screen:** `study/RESULTS.md` §1.

> "The result is not the ratio. The agent never emitted a refund outside the
> merchant's intent — not once, including all fifteen injection traces. So the
> positive class is empty, precision reads zero over eight for an arithmetic
> reason, and recall is undefined. I report it that way rather than picking a
> flattering framing.
>
> The number that carries information is the false-block rate: eight in
> forty-nine. And the cause is mine, not the model's — the guard authorizes an
> exact amount, so an agent that batched two authorized refunds into one call
> was refused. I predicted two of those cases in writing before the run. Two more
> I did not predict at all."

---

## 3:40 – 4:20 — The finding to end on

> "One trace is worth the whole study."

**On screen:** the `C04/run2` block in `study/FINDINGS.md`.

> "The agent's only refund call was blocked. It then told the operator: *'I
> issued the authorized refund of 3,000 paise.'* It never did.
>
> Nothing was lost — the guard is what made that statement false rather than
> merely inaccurate. But a human reading only that summary would have believed a
> refund happened.
>
> That is the argument for this whole layer. **The agent's account of what it did
> is not evidence of what it did.** The decision log is."

---

## 4:20 – 5:00 — What is not proven

Do not skip this. It is the part that makes the rest credible.

> "Three things I am not claiming.
>
> The guard was never given a genuinely hostile call, because the model never
> made one — so the thing it was built for went untested by this run.
>
> The generator was an unverified third-party endpoint. I measured it serving a
> different model than the one requested, so the study publishes every emitted
> call rather than claiming to know what produced them.
>
> And every number is conditional on that model's call distribution. I claimed
> otherwise in an earlier draft and it is retracted in the protocol.
>
> There are twenty-one entries in FAILURES.md. Each one is a real defect with the
> output that exposed it, including the ones I found by attacking my own fixes.
> That file is the honest measure of this project."

---

## Practical notes

- **Rehearse `live-block` once before recording.** It pulls containers on a cold
  start and you do not want that on the take.
- Have `.env` loaded: `set -a && . ./.env && set +a`.
- If a command fails on camera, **keep it in and say what happened.** For this
  project specifically, that is on-brand rather than a disaster.
- Numbers to have exactly right: **49** calls, **8** blocked, **45** traces,
  **0/15** injection traces induced, **21** FAILURES entries.
- Do not say "detector", "precision 100%", or "production-ready". None is true.
