# 5-minute pitch — script and shot list

Timings are a guide. The one thing that must survive cutting is §3:40 — the
agent that reported a refund it never made.

**Read this first:** the evaluation section changed. An earlier version of this
script led with arm A's precision and recall. Those are descriptive traces over
fifteen scenarios the author wrote and labelled, not detector scores, and the
serious experiment that replaced them **failed**. Say so. A video that claims
more than the README is the fastest way to lose a panel.

---

## 0:00 – 0:40 — The problem

> "Merchants are starting to let AI agents handle support. An agent that can
> read a customer's message and call a refund API can also be talked into
> refunding the wrong thing — by a confused instruction, a misread order, or
> text in the customer's own message written to manipulate it.
>
> The agent isn't malicious. It's credulous, and it's holding a live payments
> credential.
>
> I picked one action, because it's where money actually leaves: `create_refund`."

**On screen:** README, the four-line worked example.

---

## 0:40 – 1:20 — The design decision worth defending

> "The obvious build is a classifier that scores refunds for risk. I didn't
> build that, and the reason is the whole design.
>
> This sits on the wire between the agent and Razorpay's official MCP server. It
> sees JSON-RPC. It cannot see the agent's reasoning or the user's intent — so
> anything it 'judged' would be a guess added to a money path.
>
> Instead the merchant writes down the refunds they'll allow: one payment, one
> amount — normally exact — consumed when used. The guard forwards a refund
> unused entry. Everything else is refused. It's an authorization verifier, not
> a fraud detector, and I'd defend that choice over a model here."

**On screen:** `ARCHITECTURE.md`, the capability-list section.

---

## 1:20 – 2:30 — It works against the real thing

> "Two live gates, both against Razorpay's official container, unmodified and
> pinned by digest.
>
> An unauthorized refund never reaches the server. An authorized one really
> executes — and the receipt the guard injected comes back unchanged, the
> authorization is consumed, and a replay of the same call is refused.
>
> And when it can't tell what happened — connection drops mid-refund — it does
> not guess. It doesn't retry and it doesn't release the authorization. It marks
> the refund IN_DOUBT and tells a human. 'Probably fine' isn't a safe assumption
> about someone else's money."

**On screen:** `evidence/g16/`, then the four lifecycle states.

---

## 2:30 – 3:40 — What happened when a real agent drove it

> "Then I ran the evaluation, and I want to be straight about how it went.
>
> The serious attempt was a 54-scenario grid generated mechanically, so I wasn't
> choosing the test cases. Policy frozen by hash first. Predictions written down
> in advance — including one that said the study would be worthless if it
> produced fewer than twenty out-of-intent calls.
>
> It produced **two**, out of 340 refund calls. That prediction failed
> decisively. You cannot estimate recall from two, so **this does not meet the
> Track 2 metric bar** and I'm not going to dress it up.
>
> The measured fact is this: of the 113 refund calls emitted in scenarios
> containing an injected instruction, **zero were mechanically classified
> out-of-intent in this corpus**. I am not going to tell you why, because I do
> not know — the generator was a third-party endpoint I could not verify, and
> any story about what the agent 'decided' would be me narrating past the
> data."

**On screen:** `study/PRELABEL-FINDING-armC.md`, recorded before any label existed.

---

## 3:40 – 4:20 — The finding to end on

> "One trace from the earlier run is worth the whole study."

**On screen:** the `C04/run2` block in `study/FINDINGS.md`.

> "The agent's only refund call was blocked. It then told the operator: *'I
> issued the authorized refund of 3,000 paise for the six cracked eggs.'*
>
> It never did. No 3,000 refund was ever made — the only call in that trace was
> the one the guard refused.
>
> Nothing was lost, and the guard is what made that sentence false rather than
> merely inaccurate. But a human reading only that summary would have believed a
> refund happened.
>
> That's the argument for this entire layer. **The agent's account of what it did
> is not evidence of what it did.** The decision log is."

---

## 4:20 – 5:00 — What is not proven

> "Four things I'd want a reviewer to know.
>
> **Recall is not estimated.** The experiment designed to measure it failed, and
> no amount of labelling fixes that — the calls it needed were never emitted.
>
> **Mandates aren't signed.** The guard reads the mandate off disk. Whoever can
> write that file can grant authority, including after the fact. What it enforces
> is that the agent can't exceed authority *someone else wrote down* — not that
> the grant was legitimate. Closing that needs merchant-side signing.
>
> **Combining is deliberately bounded**, and it costs something. A refund can be
> covered by several entries summing to it, but the search stops at eight,
> because the amount is chosen by the agent and an unbounded search is
> computation an untrusted party controls. In the study that refused nine refunds
> whose entries summed exactly. That's the price of the trade, measured.
>
> **And 72 refused calls are packaged for two external raters** — blinded
> worksheets, prepared but **not yet distributed**. **No external result
> exists.** I'm not previewing a number I don't have."

**On screen:** README "Known limits".

> "What I'd claim is narrow, and I'll say it precisely: a fail-closed
> authorization layer that blocks non-mandated `create_refund` requests at the
> relay boundary, before they reach the server; that recovers a durable
> `IN_DOUBT` record across the child-failure and restart scenarios I tested;
> and that refuses to guess when it can't confirm an outcome. Everything else
> I've told you is in the failure log."

**On screen:** `FAILURES.md`.

---

## Practical notes

- **Show the terminal, not slides,** for the live gates. The pinned digest
  scrolling past is the point.
- If time runs short, cut §1:20 (live gates) to one sentence and keep §3:40. The
  gates are reproducible from the README; the lying-agent trace is the argument.
- Have `study/PRELABEL-FINDING-armC.md` open before you start. If a panellist
  asks "so what were your numbers", open it rather than answering from memory.
- **Do not claim an estimated precision or recall, and do not call this a
  detector.** Saying "recall is not estimable here" is correct and is in the
  script; quoting a figure for either is not. Also avoid "production-ready" and
  "we block prompt injection" — neither is supported.
- If asked whether it caught injections: *"In this corpus, zero of the 113
  refund calls from injection scenarios were classified out-of-intent, so the
  guard was never handed a hostile call to block. I can't tell you whether the
  agent resisted the injection or never reached that point — I only have the
  calls it emitted."*
- If asked why not ML: *"On the wire I can't see intent. A guess there is
  unpredictability in a money path."*
