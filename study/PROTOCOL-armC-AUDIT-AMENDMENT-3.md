# Amendment 3 — the delivered rater instrument

**Dated 2026-09-01.** Amends `PROTOCOL-armC-AUDIT.md` §Files. Continues the arm
C amendment sequence (`PROTOCOL-armC-AMENDMENT-1.md`, `-2.md`), and is the first
to amend the blocked-call audit protocol.

**Made before any label exists.** No worksheet has been sent to anyone, no
external label has been returned, and nothing in this amendment is informed by a
result. `AUDIT-armC.md` has not been written.

---

## What changed

`PROTOCOL-armC-AUDIT.md` §Files pre-registered the delivery as:

| | |
|---|---|
| Delivered to rater 1 | `adjudication/audit-armC-e1.csv` + `LABELLING-armC.md` |
| Delivered to rater 2 | `adjudication/audit-armC-e2.csv` + the same rubric |

It is now:

| | |
|---|---|
| Delivered to rater 1 | `adjudication/audit-armC-e1.csv` + `RATER-INSTRUCTIONS-armC.md` |
| Delivered to rater 2 | `adjudication/audit-armC-e2.csv` + the same instructions |
| Never delivered | `LABELLING-armC.md` — the internal rubric |

**The worksheets are unchanged.** No CSV, no `SHA256SUMS-audit-armC.txt` entry,
no hash and no anchor commit was touched by this amendment. The rows the raters
label are byte-for-byte the rows that were pinned and published.

**`LABELLING-armC.md` is unchanged.** It is not edited, not deleted and not
superseded as a record. It remains the internal rubric exactly as written.

## Why

`LABELLING-armC.md` was written as the project's own labelling instrument, and
it reads like one. Three parts of it are unsafe to put in front of a rater who
is supposed to be blind:

**It names the component under test.** §"What you must NOT consider" says *"what
the guard did"*, *"whether you think the guard blocked it"*, *"what any mandate
contained, or whether an authorization existed"*, *"whether the amount was
reachable by combining several authorizations"*, and calls these *"a property of
the system under test"*.

Those are protective instructions and they were written in good faith. But the
audit's question is **what fraction of the calls the guard blocked were actually
in-intent** — every row in the worksheet is a blocked call. A rater told not to
consider whether the guard blocked a row can infer that it did, which is exactly
the fact `PROTOCOL-armC-AUDIT.md` §Design says raters are not told. The
instruction that protects the blinding also reveals it.

**It describes the design.** §"Who labels" names `study/grid.py`, the kappa
plan, the supplementary author-rater, Amendment 1, and the one-rater fallback.
None of that belongs in a blind rater's hands.

**It instructs a different file.** §"Filling the file" tells the rater to open
`worksheet-armC-e1.json` and edit JSON keys. That file exists — it is the
340-row worksheet from the earlier adjudication, which §Files records as never
delivered. A rater following the rubric would look for a file they were not
sent, holding a CSV the rubric does not describe.

The pre-registered delivery therefore paired the right rows with the wrong
instructions, and those instructions carried the study.

## What replaces it

`RATER-INSTRUCTIONS-armC.md`: the task, what each column means, the three
permitted labels, the rules in priority order, the definitions needed to judge a
row, neutral worked examples, CSV editing instructions, and the instruction not
to browse or search the project until labels are returned.

**The rules are carried across unchanged in substance.** R1–R6 decide the same
cases, in the same priority order, with the same worked examples. The wording
avoids the vocabulary of the thing under test, and the §"Judge only from the
row" instruction does the work that §"What you must NOT consider" did, without
naming what the row passed through.

`RATER-INSTRUCTIONS-armC.md` also describes the CSV the rater actually receives,
which the previous instrument did not.

## The control

Neutrality that is only intended drifts. `rzp-study predistribute-armC` scans
the delivered instrument and the rater message against a fixed list of forbidden
context words and **refuses to print the packet at all** if any appears. The
list is in `cmd/rzp-study/armc_rater_pack.go`; matching is case-insensitive and
on substrings, so `author` also catches `authorization`, `authorized` and
`authority`.

Tests assert both directions: the delivered instrument passes the scan, and
`LABELLING-armC.md` **fails** it — on 17 distinct words. A scan that passed both
documents would prove nothing about either. The reviewer record is asserted to
fail it too, since it carries the anchor URL on purpose; that is what stops the
two outputs being merged back together later.

## What this does not fix

This is a change to an instrument, made by the same person who wrote the corpus
and the component being audited. It reduces what a rater can infer from the
paperwork. It does not make the audit independent, does not change its post-hoc
status, and does not affect anything in `PROTOCOL-armC-AUDIT.md` §"Why no
detector metric is available here". Arm C's failed recall experiment stands, and
this audit still reports one conditional quantity, not precision.

A rater can no longer verify their attachment against the published commit,
because the anchor is no longer in their message. That trade is recorded in
`FAILURES.md` F33: the anchor exists so a third party can confirm the worksheets
were fixed before labelling, and a third party reads the reviewer record.
