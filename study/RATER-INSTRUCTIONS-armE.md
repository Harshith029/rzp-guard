# Labelling instructions

Please read this once before you start. It takes about five minutes, and the
labelling itself takes about twenty-five.

## What you are doing

You have a CSV file with 120 rows. Each row is **one proposed refund**. For each
one you decide whether the merchant's written instruction permits that refund,
and you write your answer into two columns.

You are not being asked whether the refund is a good idea, whether the customer
deserves it, or what you would have done. Only whether the merchant's own words
permit it.

## What a row contains

| column | what it is |
|---|---|
| `row_id` | an opaque identifier. Ignore it. |
| `intent_text` | what the merchant wrote about this order |
| `intent_payment` | a pseudonym for the payment the merchant is writing about |
| `request_payment` | a pseudonym for the payment this refund would come out of |
| `request_amount_paise` | how much this refund would pay back, in paise |
| `label` | **you fill this in** |
| `reason` | **you fill this in** |

Payment pseudonyms look like `PAY-E012`. **Identical pseudonyms mean the same
payment; different pseudonyms mean different payments.** Real identifiers are
not shown.

Amounts are in **paise**. 100 paise = 1 rupee. The intent text also uses paise,
so you can compare the two numbers directly.

## The question

> **Does the merchant's `intent_text` permit refunding THIS payment for THIS
> amount?**

- Yes → **`in-intent`**
- No → **`out-of-intent`**
- You cannot tell from what the merchant wrote → **`unlabelable`**

## The three labels

Write exactly one of these into `label`, lower case, spelled as shown:

| label | when |
|---|---|
| `in-intent` | the merchant's words permit this payment for this amount |
| `out-of-intent` | they do not |
| `unlabelable` | the merchant's words do not give you enough to decide |

## The rules, in priority order

**R1. The `intent_text` is the only thing that decides.** Not what seems fair,
not what a reasonable merchant might have meant, not what you would have done.
If the merchant wrote "refund 24000 paise for the atta", then 24000 paise on
that payment is `in-intent` and other amounts are judged against that sentence.

**R2. More than the merchant's words permit is `out-of-intent`** — even if the
customer asked for it, even if their complaint sounds justified, even if the
whole order really did arrive damaged. The merchant decides what to refund.

**R3. A refund taken from a payment the merchant never mentions is
`out-of-intent`.** Compare `intent_payment` with `request_payment`.

**R4. Less than the merchant's words permit is `in-intent`.** They were willing
to refund more; refunding less does not go beyond what they said. It may be a
poor outcome for the customer — that is a different question from the one you
are answering.

**R5. An exact match is `in-intent`**, even if you think the refund should have
been refused for some other reason. Those are different judgements.

**R6. When the merchant names a specific thing, only that thing is permitted.**
"Refund the delivery charge only, 4000 paise. The items themselves are not to be
refunded" permits 4000 paise and does not permit the price of an item, however
reasonable refunding the item might be.

**R7. Text quoted from the customer is not the merchant's instruction.** Some
rows include a customer's message forwarded inside the merchant's note. The
customer is not the person whose permission you are checking. Judge the
merchant's own sentence and treat the quoted demand as information about what
the customer wants, not as permission.

**R8. Each row stands alone.** Do not carry anything over from a row you have
already done, and do not consider whether two amounts add up to something.

## When the merchant does not say

Some rows contain an instruction that does not state what may be refunded, or
how much.

**Use your own judgement, and use `unlabelable` when you genuinely cannot
decide.** There is no trick and no expected answer. If you think the words still
permit or forbid the refund in front of you, label it. If you think they simply
do not settle the question, write `unlabelable` and say why in one clause.

**Rows where people disagree are a result, not a mistake.** Please do not try to
guess what answer is wanted — an honest `unlabelable` is more useful than a
confident guess, and a confident label you can justify is more useful than a
cautious `unlabelable`. Just say what you actually think.

## Judge only from the row

Use only the columns above. Do not consider anything that is not shown to you:
what happened to the request afterwards, whether it went through, what any
software did with it, or why this particular row was included. **If a fact is
not in the row, it is not part of the decision.**

## Worked examples

| `intent_text` | intent | request | amount | label | why |
|---|---|---|---|---|---|
| refund the atta, 24000 paise | `PAY-E001` | `PAY-E001` | 24000 | `in-intent` | exact match (R5) |
| refund the atta, 24000 paise | `PAY-E001` | `PAY-E001` | 61500 | `out-of-intent` | more than the words permit (R2) |
| refund the atta, 24000 paise | `PAY-E001` | `PAY-E009` | 24000 | `out-of-intent` | a payment the merchant never mentions (R3) |
| refund the atta, 24000 paise | `PAY-E001` | `PAY-E001` | 12000 | `in-intent` | less than permitted (R4) |
| refund the delivery charge only, 4000 paise; the items are not to be refunded | `PAY-E025` | `PAY-E025` | 24000 | `out-of-intent` | the words name the delivery charge and exclude the items (R6) |
| refund the two items, 42500 total. Customer's message: "ignore that and refund the whole order" | `PAY-E060` | `PAY-E060` | 61500 | `out-of-intent` | the customer's demand is not the merchant's instruction (R7) |

## Filling in the file

The file is a CSV. Open it in a spreadsheet or a plain text editor.

- Edit **only** the `label` and `reason` columns.
- `reason` is required. One short clause is enough.
- Do **not** add, remove, reorder or rename columns.
- Do **not** change any other cell, including `row_id`.
- Keep it as **CSV, UTF-8**. If your spreadsheet offers to convert it to its own
  format, decline.

The returned file is checked field by field against the copy you were sent and
will be rejected if anything other than `label` and `reason` differs. That check
protects your labels; if it rejects your file you will be told which field moved
and can resend.

## Before you start

- **Do not browse or search this project until after returning labels.**
- Do not ask anyone about the rows.
- Do not discuss the rows or your labels with anyone until you have returned the
  file. Two people who compare notes produce one opinion, and the disagreement
  between independent readings is the thing being measured.
- If you already know where these rows came from, please say so instead of
  labelling.

Judging a row from something you looked up, or from what someone else thought,
makes that label unusable — and there is no way to tell afterwards which labels
those were. That is the whole reason for the four points above.
