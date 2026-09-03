# Arm E, Amendment 2 — the policy freeze was broken, and here is the proof it was harmless

**Dated 2026-09-03. Found by external review, not by me.** This amends
`PROTOCOL-armE.md` §3. It does not change any reported number.

## What §3 claimed

> **The implementation was not fitted to this data.** `internal/policy` was last
> changed in `fb87b12` (2026-08-30) and its tree hash is recorded.

**Both halves were wrong by the time anyone read them.**

1. `internal/policy` was changed again in **`8a25767`** — the money-path sweep,
   committed *after* the arm E corpus existed (`7d0405b`). So `fb87b12` was no
   longer the last change.
2. **No tree hash was ever recorded anywhere.** `study/armE/manifest.json`
   carried `raters`, `labels_sha256`, `matrix`, `excluded` and `note` — and no
   policy hash. The sentence described a control that did not exist.

Arm D had a mechanism for exactly this — `rzp-armd manifest -supersede` carries
the old hash, both dates and a reason, and refuses if the matrix moved. **Arm E
never got one.** That is the actual defect: not the edit, but that the edit could
happen silently to the arm carrying the headline result.

## What changed in the policy

`8a25767`, +40 lines in `policy.go`, +44 in its tests. It added a `jsonInteger`
validator so `parseAmountPaise` follows the RFC 8259 integer grammar — rejecting
`+500` and leading zeros like `00500`, which `strconv` had been accepting.

## The equivalence check, run rather than assumed

Every one of the 120 arm E requests was decided twice: once under the policy tree
at `fb87b12`, once under the current tree. Same corpus, same mandates, same fixed
clock. Both runs recorded `request_id, allowed, rule` for all 120 rows.

```
fb87b12   120 decisions   allowed 54  refused 66
HEAD      120 decisions   allowed 54  refused 66

diff: none. Both outputs sha256 9e6489fc71ace82d…
```

**All 120 decisions are identical, and so is every rule string.** Not just the
same totals — the same row-by-row outcome for the same stated reason.

Why it came out that way: arm E's amounts are emitted as `%d`, so every one is a
plain integer with no sign and no leading zero. The tightened grammar rejects
inputs this corpus does not contain. That is an explanation of the result, not a
substitute for it — the check was run because "it probably doesn't matter" is
what a freeze exists to stop people saying.

## What this does and does not mean

- **It does not mean the freeze held.** It did not. The policy changed after the
  corpus existed and nothing in the repository noticed.
- **It does mean the reported numbers are unaffected.** Recall 0.733, FPR 0.455,
  precision 0.423, TP 22 / FN 8 / FP 30 / TN 36 all stand, and `rzp-arme verify`
  reproduces the matrix from the raters' returned files under the current tree.
- **It does mean §3 overstated the control.** "Hash-pinned" described something
  that was never written down. The corrected claim is narrower and true: *the
  policy was not changed in response to this data, and the one change that
  followed it has been shown not to move any decision in it.*

## Corrected

- `PROTOCOL-armE.md` §3 now carries a pointer here instead of the false claim.
- `study/armE/manifest.json` now records `policy_tree` for `fb87b12` and for the
  current tree, with this amendment named as the supersession reason — so the
  next break is visible rather than silent.
- `README.md` no longer says "policy frozen and hash-pinned before the corpus
  existed."

## The lesson

A freeze that nothing enforces is a sentence, not a control. Arm D's break was
caught because `rzp-armd` refused to re-stamp a manifest; arm E's went unnoticed
for a day because no equivalent gate existed for the arm that mattered most. The
mechanism belongs on the result you intend people to rely on, not only on the one
you happened to build tooling for first.
