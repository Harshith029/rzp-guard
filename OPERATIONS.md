# Running rzp-guard

What to do when something happens. Written for whoever is on call, not for
whoever wrote it.

> **This has never run in production.** No deployment exists, no drill has been
> timed, and the guard refuses non-test Razorpay credentials by design. What
> follows is the procedure the code actually implements — every command here is
> real and every failure mode is one the tests exercise — but none of it has
> been executed under load or under pressure. Treat it as a starting point to
> rehearse, not a document that has been proven in anger.

---

## The one alert that matters

```
RZP_GUARD_ALERT IN_DOUBT action=rfa_001 reason="..."
```

**What it means.** A refund was authorized and forwarded, and the guard cannot
tell whether Razorpay executed it. The money may have moved. The action is
frozen, its budget stays held, and **nothing will resolve it without a human.**

**Alert on this token.** There is no metrics endpoint; a log rule on
`RZP_GUARD_ALERT` is the intended integration. It appears on both routes into
`IN_DOUBT` — mid-session, and at startup when a previous process died with work
in flight.

**Urgency.** Not a page in itself: the design is *deliberately* biased toward
producing these rather than guessing. But every one represents real money in an
unknown state, and they do not clear on their own.

---

## Is anything stuck right now?

Without touching the running guard:

```bash
cat "$STATUS_FILE" | jq '{needs_operator, in_doubt_count, in_doubt_actions, encumbered_paise}'
```

Requires the guard to have been started with `-status-file`. The document is
rewritten atomically, so a read never sees a partial file, and it is published
**without taking the state file's lock** — reading it cannot disturb the guard.

`needs_operator: true` is the single field worth alerting on.

---

## Resolving a stuck refund

**This requires stopping the guard.** The operator CLI needs the state file and
the guard holds an exclusive lock on it for its entire lifetime. Reading status
is lock-free; *resolving* is a write, and writes are what the lock serialises.

```bash
# 1. Stop the guard. In-flight calls are locked, not lost.
#    Refunds still outstanding become IN_DOUBT and are named on stderr.

# 2. List what needs a decision. Note the RECEIPT for each.
rzp-guard-operator -mandate M -state S list

# 3. For each action, search Razorpay for that receipt.
#    The receipt is the idempotency key the guard injected, so it is the
#    reliable way to find the refund — not the amount, not the timestamp.

# 4. Record what you found.
rzp-guard-operator -mandate M -state S -operator "your.name" \
    resolve rfa_001 landed     -reason "found rfnd_XXX in the dashboard, processed"
# or
rzp-guard-operator -mandate M -state S -operator "your.name" \
    resolve rfa_001 not-landed -reason "no refund exists for this receipt"

# 5. Restart the guard.
```

### The judgement call in step 3

- **`landed`** consumes the action permanently. Correct when a refund entity
  exists for that receipt.
- **`not-landed`** returns the action to `AVAILABLE`, so it can be attempted
  again.

**Absence of a matching record is not, by itself, sufficient to answer
`not-landed`.** Razorpay creates a refund asynchronously — a live capture came
back `pending` and only became `processed` after the reply had been sent. If you
looked too early you will see nothing and conclude wrongly, and the cost of that
mistake is a **duplicate refund**. When in doubt, wait and look again.

Every resolution is written to the audit trail with your name and reason, in the
same transaction as the state change:

```bash
rzp-guard-operator -mandate M -state S -operator "your.name" audit
```

---

## Startup refusals, and what each means

The guard refuses rather than warns. Each message names its own remedy.

| Message contains | Cause | Do this |
|---|---|---|
| `is not a test-mode key` | A live Razorpay key | Use `rzp_test_*`. **Do not remove the check.** |
| `has no operator recovery credential` | State file never provisioned | `rzp-guard-operator ... init -out TOKENFILE`, once, as a deployment step |
| `provisioned by a TEST FIXTURE` | File created by a gate; its token was discarded | Use a fresh state file. No human can resolve refunds in this one. |
| `belongs to another mandate` | Reusing a state file across mandates with work outstanding | Reopen with the mandate it names, resolve what is stranded, or use a new file |
| `owned by another guard process` | A guard is already running | Stop it. Two guards would each enforce the cap against their own ledger. |
| `schema version` | File written by a different build | Match the binary to the file. **Do not delete the file** — it may hold IN_DOUBT actions. |
| `start child (is Docker running?)` | Child container could not start | Start Docker; check the pinned image is pullable |

---

## Things that will make it worse

- **Deleting the state file.** It holds the ledger and the operator credential.
  Deleting it discards every `IN_DOUBT` action — money whose fate is unknown —
  and leaves no record that they existed.
- **Running a second guard on one state file.** It is refused, and the refusal
  is protecting you: two guards each check the cumulative cap against their own
  in-memory ledger, so between them they can spend past it.
- **Weakening `synchronous`.** It costs ~5.6 ms per commit against ~34 µs at
  `NORMAL`, so it looks like a ~165× win. It is the guarantee that a reservation
  is on disk before its refund is forwarded ([F23](FAILURES.md)).
- **Resolving `not-landed` because the dashboard looked empty.** See above.

---

## What is not here

No backup procedure, no supervisor configuration, no capacity plan, no timed
drill. Those are real gaps and they are listed as such in the README's limits
rather than papered over here.

**Backups:** the state file is a single SQLite database. Copying it while the
guard runs is unsafe — the guard holds an exclusive lock and a copy taken
mid-transaction is not guaranteed consistent. There is no supported online
backup path today.

---

## Before the repository is published, or any push

Run the gate:

```sh
./run.sh preflight
```

It scans **all reachable history** and refuses if any commit contains the
prohibited launcher signature — a refund action granted to a caller-supplied
target.

**It is a text-pattern scan, and its claim is exactly that narrow.** A pass
means no reachable source matched the signature. It is not, and cannot be,
proof that no reachable commit can launch a caller-selected refund: it cannot
see the same capability built by concatenation, read from data, written in a
language it does not read, or shaped in a way nobody added to the list.
`FAILURES.md` F26 is why this exists: such a command survived in history for
four commits after it was deleted from the tip, and every check in place at the
time looked only at `HEAD`. A clone carries its history.

It keys on the shape, not the name. Four commits still contain the identifier
`cmd_live_refund` on purpose — they are exit-2 tombstones explaining the
removal — so counting the name would flag safe carriers and miss a rename.
`redteam-negative` N9 builds a synthetic fixture with the dangerous shape and
requires the scan to refuse it, so the tripwire is known to be able to fail.

**The scan is not sufficient on its own.** It proves a property of *this*
repository's history. It cannot tell you whether the pre-purge objects exist
somewhere else. Before first publication, also confirm by hand:

- no GitHub/GitLab repository, fork, or gist ever received a push
- no CI artifact, release asset, or build cache holds the old objects
- no clone exists on another machine, or in a backup or sync service

That last one is not hypothetical here. This working copy lives under a
**OneDrive-synced path**, with `.git` inside it, and the OneDrive client is
running. The pre-rewrite pack files were therefore uploaded, and OneDrive keeps
version history and a recycle bin. The purge is complete in this repository's
object store; it is *not* complete in that cloud copy, and restoring an earlier
version of the folder would bring the old objects back. Treat the sync copy as
out of scope for the purge and do not share that folder.

A local backup bundle of the pre-rewrite history was also taken deliberately, so
the rewrite stayed reversible. Delete it once you are satisfied with the result.
