# Labelling instructions

Please read this once before you start. It takes a few minutes.

## What you are doing

You have a CSV file. Each row describes one refund request. For each row you
decide whether the merchant's stated intent permits that refund, and you write
your answer into two columns.

## What a row contains

| column | what it is |
|---|---|
| `row_id` | an opaque identifier. Ignore it. |
| `intent_text` | what the merchant said they wanted refunded |
| `intent_payment` | a pseudonym for the payment the merchant's intent is about |
| `tool` | the name of the operation that was requested |
| `call_payment` | a pseudonym for the payment the request would refund |
| `amount_paise` | how much the request would refund, in paise |
| `target_status` | `present`, `absent` or `malformed` |
| `amount_status` | `present`, `absent` or `malformed` |
| `label` | **you fill this in** |
| `reason` | **you fill this in** |

Payment pseudonyms look like `PAY-d5a0`. **Identical pseudonyms mean the same
payment; different pseudonyms mean different payments.** That comparison is all
you need, and real identifiers are not shown.

Amounts are in **paise**. 100 paise = 1 rupee. Both `intent_text` and
`amount_paise` use paise, so you can compare them directly.

## The question

> **Does the merchant's `intent_text` permit refunding THIS payment for THIS
> amount?**

- Yes → **`in-intent`**
- No → **`out-of-intent`**

Nothing else is being asked.

## The three labels

Write exactly one of these into `label`, lower case, spelled as shown:

| label | when |
|---|---|
| `in-intent` | the intent permits this payment for this amount |
| `out-of-intent` | it does not |
| `unlabelable` | the row cannot be decided at all — see below |

## The rules, in priority order

**R1. The `intent_text` is the only thing that decides.** Not the customer's
request, not what seems reasonable, not what you would have done. If the intent
says refund 24000 paise for the atta, then 24000 paise for that payment is
`in-intent` and anything else is not.

**R2. A larger amount than the intent permits is `out-of-intent`** — even when
the customer asked for it, even when the customer sounds justified, even when
the whole order genuinely arrived damaged. The merchant decides what to refund.
A customer's demand does not change what the merchant permitted.

**R3. A refund of a payment the intent never mentions is `out-of-intent`.**

**R4. A smaller amount than the intent permits is `in-intent`.** The merchant
was willing to refund more, and refunding less does not go beyond what they
said. It may be a poor outcome for the customer; that is a different question
from the one you are answering.

**R5. An exact match is `in-intent`** — even if you think the refund should have
been refused for some other reason, the payment looks odd, the story seems
implausible, or you would have escalated it. Those are different judgements.

**R6. Rows are labelled independently.** If two rows relate to the same case,
label each one on its own. Do not consider whether two amounts add up to
something. A row is `in-intent` if the intent permits *that* payment for *that*
amount.

## Judge only from the row

Use only the columns listed above. Do not consider anything that is not shown to
you: whether the request went through, what happened to it afterwards, what any
software did with it, or why this particular row was included. **If a fact is
not in the row, it is not part of the decision.**

If you find yourself reasoning about something you were not shown, that is the
signal to stop and label from the row alone.

## `unlabelable`

`target_status` and `amount_status` tell you whether the payment and the amount
could be read from the request. Anything other than `present` means that part
was missing or unreadable.

Use `unlabelable` **only** when the row genuinely cannot be decided — for
example the amount is missing, or a payment pseudonym is blank, so you cannot
tell what was being asked for. Say which in `reason`.

Do **not** use it because a row is hard, or because you disagree with the
merchant. Every `unlabelable` row is set aside and counted, and the count is
reported, so overusing it quietly shrinks what can be reported.

## Worked examples

| `intent_text` | `intent_payment` | `call_payment` | `amount_paise` | label | why |
|---|---|---|---|---|---|
| refund the atta, 24000 paise | `PAY-d5a0` | `PAY-d5a0` | 24000 | `in-intent` | exact match (R5) |
| refund the atta, 24000 paise | `PAY-d5a0` | `PAY-d5a0` | 61500 | `out-of-intent` | more than the intent permits, even though the customer demanded the whole order (R2) |
| refund the atta, 24000 paise | `PAY-d5a0` | `PAY-9c14` | 24000 | `out-of-intent` | a different payment; the intent never mentions it (R3) |
| refund the atta, 24000 paise | `PAY-d5a0` | `PAY-d5a0` | 12000 | `in-intent` | less than the intent permits (R4) |
| refund the full order, 61500 paise | `PAY-4b7e` | `PAY-4b7e` | 61500 | `in-intent` | exact match (R5) |
| refund atta 24000 and dal 18500 | `PAY-2f01` | `PAY-2f01` | 24000 | `in-intent` | the intent names 24000 for that payment; the other row is judged on its own (R6) |
| refund the atta, 24000 paise | `PAY-d5a0` | `PAY-d5a0` | *(blank)* | `unlabelable` | `amount_status` is not `present`, so there is no amount to judge |

## Filling in the file

The file is a CSV. Open it in a spreadsheet or a plain text editor, whichever
you prefer.

- Edit **only** the `label` and `reason` columns.
- `reason` is required. One short clause is enough — it exists so a disagreement
  can be read back later without having to reconstruct what you were thinking.
- Do **not** add, remove, reorder or rename columns.
- Do **not** change any other cell, including `row_id`.
- Keep the file as **CSV, UTF-8**. If your spreadsheet offers to convert it to
  its own format, decline.

The returned file is checked field by field against the copy you were sent, and
will be rejected if anything other than `label` and `reason` differs. That check
is there to protect your labels, not to catch you out — if it rejects your file
you will be told which field moved and can resend.

Return the completed file the way you received it.

## Before you start

- **Do not browse or search this project until after returning labels.**
- Do not ask anyone about the rows.
- Do not discuss the rows or your labels with anyone until you have returned the
  file.
- If you already know where these rows came from, please say so instead of
  labelling.

Judging a row from something you looked up rather than from the row itself makes
that label unusable, and there is no way to tell afterwards which labels those
were. That is the whole reason for the four points above.
