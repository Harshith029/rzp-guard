# What the errors cost

> ## SUPERSEDED IN PART, 2026-09-02
>
> Everything below was computed from **arm D**, whose recall was 1.000 *by
> construction* — every positive was built to match no authorization, so the
> false-negative term was assumed to be zero rather than measured.
>
> **Arm E measured it.** Recall is **0.733**, not 1.000: eight out-of-intent
> requests were forwarded. A control that misses a quarter of what it is for is
> harder to justify, not easier, and the break-even moves against it:
>
> | | recall | FPR | break-even |
> |---|---:|---:|---:|
> | arm D (assumed) | 1.000 | 0.472 | 5.6% |
> | **arm E (measured)** | **0.733** | **0.455** | **7.2%** |
> | across arm E's cluster intervals | 0.600–0.900 | 0.407–0.493 | **5.4% – 9.3%** |
>
> **Arm C observed 0.6%** (scenario-clustered 0.00–1.57%), still below every one
> of those bands — so the qualitative conclusion is unchanged and the margin is
> wider than arm D suggested.
>
> The cost model, the assumptions and the reasoning below are unaltered and still
> apply. **Substitute recall 0.733 and FPR 0.455 wherever arm D's figures
> appear**, and read the break-even as 5.4–9.3% rather than 4.5–7.0%.
>
> Sections 6 and 7 are the parts that survive intact: the false positives are
> still attributable to named causes, and the inputs that would overturn the
> conclusion are still the same ones. See `FINDINGS-armE.md`.

Track 2 asks for "honest metrics including false-positive cost". The rates were
published and never priced. This prices them.

**Everything here is arithmetic over numbers already published elsewhere in this
repository.** No new experiment, no new corpus, no new measurement. Every input
that is an assumption is marked as one, with the value that would change the
conclusion.

---

## 1. A false positive does not cost you the refund

`study/armD/CONFORMANCE-armD.md` reports "value refused: 689,000 paise" across
17 rows. **That is not a loss**, and reading it as one inflates the cost of a
block by about fifty times.

A blocked refund is not a refund that never happens. It is a refund that waits
for a human. The money still moves; a person has to make it move.

| | what actually happens |
|---|---|
| **False positive** — a refund the merchant wanted, refused | the customer waits, contacts support, a human unblocks it. Cost = **handling + delay**, not the amount. |
| **False negative** — a refund the merchant did not want, forwarded | the money is gone and has to be clawed back. Cost = **the amount + investigation**. |

The two errors are asymmetric in kind, not just in size, which is why a single
F1 number is the wrong objective for this system.

## 2. The cost inputs

None of these is measured by this project. They are stated so a reader can
substitute their own and recompute.

| symbol | meaning | assumed | basis |
|---|---|---:|---|
| `c_fp` | cost of one wrongly-blocked refund | **₹100** | one support contact plus the delay. ASSUMPTION. |
| `c_fn` | cost of one wrongly-allowed refund | **₹800** | the refunded amount plus investigation. See below. |
| `p` | share of agent refund calls that are out-of-intent | varies | the quantity arm C tried and failed to pin down |

`c_fn` is anchored to this corpus rather than invented: authorized amounts range
24,000–108,000 paise (₹240–₹1,080), so ₹600 is a representative refund, plus
₹200 to investigate one. **A merchant with ₹5,000 baskets should use a much
larger `c_fn`, and every conclusion below moves in the guard's favour.**

## 3. The measured rates

From `study/armD/CONFORMANCE-armD.md`, on a 90-row synthetic conformance grid
with author-declared labels — **not a held-out measurement**, see
`ASSESSMENT-armD.md`:

```
TPR (out-of-intent refused)     1.000     54/54
FPR (in-intent refused)         0.472     17/36
```

**Those 36 in-intent rows come from FOUR distinct merchant intent sentences**,
not 36 independent situations — arm D's `intent_text` is a function of the cell,
so rows sharing a sentence share whatever that sentence means. Treating 0.472 as
a point estimate over 36 independent observations overstates its precision.
Resampling whole sentence groups instead:

```
FPR  0.472   95% cluster bootstrap  0.375 - 0.600   (4 clusters, seeded)
```

The first version of this file quoted 0.472 with no uncertainty at all. That was
wrong, and it is the same defect `PROTOCOL-armE-AMENDMENT-1.md` records in arm E
— found there first, then found here.

Per 1,000 agent refund calls at base rate `p`:

```
false negatives = 1000 · p · (1 − TPR) = 0
false positives = 1000 · (1 − p) · FPR = 472 · (1 − p)

expected cost = FN·c_fn + FP·c_fp
```

**At this operating point the false-negative term is zero, so the entire
expected cost of running the guard is operational.** That is the correct posture
for a money path — it fails toward "a human looks" — but it also means the
false-positive rate is the whole economic story, which makes the contested FP
labels in §5 economically load-bearing rather than a footnote.

## 4. Break-even

The guard is worth running when the loss it prevents exceeds the handling it
creates:

```
p · c_fn  >  (1 − p) · FPR · c_fp

p / (1 − p)  >  FPR · c_fp / c_fn
             =  0.472 × 100 / 800
             =  0.059

p  >  5.6%
```

Propagating the cluster interval on FPR through the same arithmetic:

```
break-even   5.6%   range across the FPR interval   4.5% - 7.0%
```

**Somewhere between about 1 call in 22 and 1 in 14 must be out-of-intent before
this control pays for itself at its current false-positive rate.** Quote the
range. The point estimate is the centre of a genuinely wide band, and the band
is wide because the corpus behind it has four intent sentences.

| base rate `p` | FN / 1000 | FP / 1000 | cost of errors | cost with no guard | net |
|---:|---:|---:|---:|---:|---:|
| 0.6% *(arm C observed)* | 0 | 469 | ₹46,900 | ₹4,800 | **−₹42,100** |
| 5.6% *(break-even)* | 0 | 446 | ₹44,600 | ₹44,800 | ≈ 0 |
| 10% | 0 | 425 | ₹42,500 | ₹80,000 | +₹37,500 |
| 25% | 0 | 354 | ₹35,400 | ₹200,000 | +₹164,600 |
| 50% | 0 | 236 | ₹23,600 | ₹400,000 | +₹376,400 |

## 5. The uncomfortable result

**Arm C observed 2 out-of-intent calls in 340 — a base rate of 0.6%, which is
below break-even.** On the only traffic this project has ever observed, the
guard's handling cost would have exceeded the loss it prevented.

Three things that does and does not mean:

- **It does not mean the control is wrong.** Arm C's traffic came from a model
  that mostly complied, through a proxy measured substituting models
  (`PROTOCOL-armC-AMENDMENT-1.md`). It is one sample of one model's behaviour on
  one day, not a prior over agent traffic — and certainly not over *adversarial*
  agent traffic, which is the case the control exists for. A single successful
  prompt injection against a ₹50,000 basket pays for a lot of support contacts.
- **It does mean the false-positive rate is the biggest single lever**, though
  no longer the only one. *(Corrected 2026-09-03. This bullet used to read
  "recall is already 1.000 by construction; there is nothing to gain there."
  **Arm E measured recall at 0.733**, so that was wrong the moment the labels
  came back: eight out-of-intent requests were forwarded and there is real
  ground to gain on the FN side too. The banner above says sections 6 and 7
  survive intact; this bullet is in section 5 and did not.)* The FP side is
  still where the larger and cheaper improvement sits — see section 6.
- **It does mean the honest pitch is "insurance", not "savings".** At a 0.6%
  base rate this is a control you run because the tail is catastrophic and
  uninsurable, the way you run input validation. Claiming it saves money at
  observed rates would be false.

## 6. Where the false positives come from, and what each would cost to fix

> ### THIS SECTION IS AN ARM D COUNTERFACTUAL, NOT ARM E EVIDENCE
>
> Everything below — the attribution of 17 false positives, `FPR = 7/36 = 0.194`
> and the **≈2.4% break-even** — is computed from **arm D's retracted,
> author-labelled conformance grid**. Arm D was withdrawn by its own author
> because its scorer branched on a label the author wrote.
>
> **It is not a measurement of the submitted configuration.** Nobody has run arm
> E with bounded actions enabled. The claim it supports is *"here is where arm D's
> false positives came from, and what the same change would have done there"* —
> not *"the guard achieves FPR 0.194"*, and not that recall would still be 0.733
> after the change. A wider authorization admits more requests, so recall could
> fall, and no experiment here has measured by how much.
>
> **Current measured performance is arm E's alone:** recall **0.733**, FPR
> **0.455**, precision **0.423**, on constructed traffic
> (`RESULTS-armE.md`). Quote those. Quote 0.194 only as *"a retracted arm's
> counterfactual for a feature that ships off by default."*

`study/FINDINGS-armD.md` attributes all 17, and **none is a matching error**:

| cause | n | fixable? | effect on FPR |
|---|---:|---|---|
| `request=under` — a partial refund of an exactly-authorized amount | 10 | **Yes, already built.** `max_amount_paise` actions are implemented and opt-in; a bounded action admits a smaller refund. | 17 → 7 |
| `coverage=under` — the mandate under-expresses the intent | 6 | No. Correct enforcement of an incomplete authorization. The fix is upstream, in how the merchant writes the mandate. | — |
| `D086` — a sum reachable only by combining ten actions, over `maxSetSize = 8` | 1 | Bounded by design: the requested amount is agent-controlled, so an unbounded subset-sum search is computation an untrusted party chooses. Raising the bound trades a DoS surface for one row in 90. | — |

Turning on bounded actions for the first class:

```
FPR = 7/36 = 0.194
break-even p > 0.194 × 100/800 / (1 + …) ≈ 2.4%
```

**Break-even falls from 5.6% to about 2.4% by enabling a feature that already
exists.** It does not reach arm C's observed 0.6%, and saying otherwise would be
the same overclaim this file exists to avoid.

## 7. What would change these numbers

Stated so they can be attacked:

- **`c_fp` = ₹100 is a guess.** At ₹40 break-even falls to 2.3%; at ₹300 it
  rises to 15.0%. This single unmeasured input moves the conclusion more than
  anything else here, and measuring it needs a merchant, not a corpus.
- **`c_fn` = ₹800 is anchored to a synthetic basket.** Real baskets are larger,
  which favours the guard.
- **FPR = 0.472 is not transferable, and is less precise than one decimal
  suggests.** It comes from a 90-row grid that is 60% positive by construction,
  with labels the author declared, and its 36 negatives come from four intent
  sentences. If the six contested `coverage=exact / request=under` rows are
  ruled correct refusals rather than false positives, FPR falls to 11/36 = 0.306
  and break-even to 3.7% — which is inside the cluster interval above, so that
  dispute and the sampling uncertainty are the same size.
- **TPR = 1.000 is tautological on that corpus.** Every positive was constructed
  to match no capability. **Arm E measured it properly and got 0.733** — a real
  out-of-intent call that happens to match an unused authorization *is*
  forwarded, and it happened eight times. Substitute 0.733.
- **`c_fp` assumes a human is standing by, and nothing here tests that.** The
  whole model prices a false positive as *handling + delay* rather than as the
  refund amount, and that holds only where someone unblocks it within hours. At
  a merchant with no support rota there is no unblocking step: the customer
  waits, escalates, and files a dispute. **The false positives would then cause
  the chargeback this control exists to avoid**, and the sign of the whole
  calculation flips. This is the assumption most likely to be false in the field
  and the cheapest to check — ask one merchant what an unblocked refund costs
  them and how fast it happens.

## 8. The honest one-line summary

**Current, from arm E — the measured configuration:**

> Against labels from two people who saw neither the authorization nor the
> decision, the guard catches **73.3%** of out-of-intent requests and refuses
> **45.5%** of in-intent ones. Priced with the stated assumptions that puts
> break-even between roughly **5.4% and 9.3%** out-of-intent base rate — a range,
> not a point, because only five intent sentences sit behind it. The only agent
> traffic this project has observed ran at **0.6%**, below the bottom of that
> range. **This is a tail-risk control, not a cost saving**, and the number that
> decides its value — the real out-of-intent base rate — is the one arm C failed
> to measure.

**Legacy, from arm D — a retracted arm's counterfactual, not a prediction:**

> On the synthetic conformance grid the guard refused every constructed
> out-of-intent request (recall 1.000 *by construction*) and refused 17 of 36
> in-intent ones, breaking even between 4.5% and 7.0%. Enabling bounded actions
> would have moved that grid's centre to about 2.4%. **Nobody has run arm E with
> bounded actions**, so this is not evidence about the submitted configuration,
> and it says nothing about what recall would be afterwards.
