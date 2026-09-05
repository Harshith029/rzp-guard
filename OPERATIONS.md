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

### A second event on the same token

```
RZP_GUARD_ALERT AUDIT_BROKEN file="..." reason="..."
```

Only when running with `-child-tee`. It means the evidence file stopped being
writable — a full disk, or the file removed underneath a running guard.

**No money is stuck and nothing needs resolving.** The child received the bytes
and its reply will settle the action normally. What has broken is the guard's
ability to *prove* what crossed the boundary, so from that point the tee file is
short and must not be read as a complete record. The guard keeps running and
stops writing to it.

Alert on it anyway. A control whose audit trail silently truncates is worth
knowing about before someone cites the file as evidence.

---

## Is anything stuck right now?

Three ways, none of which touches the running guard:

```bash
# The one to alert on, if -admin-addr is set.
curl -s localhost:9090/metrics | grep rzp_guard_in_doubt_actions
```

Or from the status file:

```bash
cat "$STATUS_FILE" | jq '{needs_operator, in_doubt_count, in_doubt_actions, encumbered_paise}'
```

Requires the guard to have been started with `-status-file`. The document is
rewritten atomically, so a read never sees a partial file, and it is published
**without taking any lock** — reading it cannot disturb the guard.

`needs_operator: true` is the single field worth alerting on.

Or, if you have the operator credential, ask the state file directly — this now
works while the guard is running:

```bash
rzp-guard-operator -mandate m.json -state rzp-guard.db list
```

### Actions held RESERVED, which are not IN_DOUBT

```bash
rzp-guard-operator -mandate m.json -state rzp-guard.db list
```

Alongside anything `IN_DOUBT`, this now reports actions sitting in `RESERVED`.
That state is normally momentary — it lasts from the moment a refund is
authorized to the moment the child's reply settles it. One that persists means
either the reply never came, or the durable write recording the commit failed.

**Neither case is resolvable from here**, because `resolve` accepts only
`IN_DOUBT`. Budget stays encumbered until something moves it. Restarting the
guard promotes a stranded `RESERVED` to `IN_DOUBT`, which *is* resolvable —
check the receipt in Razorpay before deciding the outcome.

This used to be invisible: `list` reported only `IN_DOUBT`, so on a
long-running guard a stranded reservation could not be seen at all.

---

## Resolving a stuck refund

**This requires stopping the guard**, and it is the only common task that still
does. The line is not read versus write: `queue`, `approve`, `decline` and
`backup` all write and all run beside a live guard. It is whether the command
moves state the guard is holding **in its own memory**. A running guard restores
a ledger at startup and decides from it, so resolving an action underneath it
would leave it authorizing against a view that is no longer true.

If a guard is live, the CLI says so with the holder's pid rather than failing
with a lock error.

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

## A refund the guard wrongly refused

The guard blocks about **45% of legitimate refunds** by its own published
measurement (`study/RESULTS-armE.md`). That is survivable only if somebody
unblocks them, and until recently nobody could: the guard held a file-wide
exclusive lock, so every operator action needed the payment proxy stopped first.
It does not any more.

**None of this needs the guard stopped.**

```bash
# 1. What was refused, deduplicated, with how many times the agent retried.
rzp-guard-operator -mandate m.json -state rzp-guard.db queue

# 2. Decide. Either the mandate was written narrower than the merchant meant,
#    in which case approve the specific refusal --
rzp-guard-operator -mandate m.json -state rzp-guard.db approve 14 \
  -operator you@merchant.example -reason "customer produced the order confirmation"

#    -- or the guard was right, in which case say so, so the queue can tell
#    "worked and correct" from "nobody has looked".
rzp-guard-operator -mandate m.json -state rzp-guard.db decline 14 \
  -operator you@merchant.example -reason "already refunded on 2026-09-01"

# 3. Tell the agent to retry. The guard picks up the grant within a second.
```

### What an approval can and cannot do

| | |
| --- | --- |
| Amount | **Exactly** what was refused. Taken from the recorded refusal, never from a flag, so you cannot approve a refund nobody asked for. |
| Uses | **One.** It becomes an ordinary row in the same ledger as any mandate action. |
| Life | 15 minutes by default, one hour maximum. |
| Cumulative cap | **Still applies.** An operator can correct a wrong refusal; an operator cannot raise the merchant's own ceiling. That one needs the merchant. |
| Expired mandate | **Cannot be overridden.** Ask the merchant for a new mandate. |
| Attribution | Every grant carries the operator's name and reason into the audit table. |

### If the queue says it is incomplete

The deny path is rate limited, because recording every refusal puts a durable
write on a path that used to cost 779 nanoseconds — and an agent looping on a
refused call would otherwise saturate the state file the money path depends on.
Identical refusals are coalesced; a flood of *distinct* ones is capped.

What the cap drops is counted, never silent:

```bash
curl -s localhost:9090/metrics | grep rzp_guard_denials_unrecorded_total
jq .denials_unrecorded "$STATUS_FILE"
```

**Non-zero means the queue below is incomplete.** A short queue because it
overflowed must not be mistaken for a short queue because nothing was refused.
In practice a non-zero value means an agent is generating refusals faster than
any merchant legitimately can, which is itself the finding.

### Reading the queue

A queue that only ever grows means nobody is working it, and the false-positive
cost model assumes somebody is. Two numbers are worth watching:

- `rzp_guard_operator_approved_total` rising means **mandates are being written
  narrower than merchants intend**. The fix is upstream, in `rzp-mandate`, not
  here.
- `decline` outnumbering `approve` means the guard is mostly right and the
  refusals are correct — which is the good case, and it is only visible because
  declines are recorded.

---

## Backups

**RPO and RTO are set by how often you run this, not by what it does.** The
figures below are what the mechanism supports; the schedule is yours.

| | |
| --- | --- |
| **RPO** | The age of the last backup. A daily backup means up to 24h of ledger history lost. |
| **RTO** | Minutes: stop the guard, copy the file, start it. Nothing to rebuild. |
| **What loss costs** | Not money — Razorpay is the system of record for whether a refund happened. It costs the record of *which authorizations were consumed*, so every action returns to AVAILABLE and a replayed mandate can spend its authority a second time. Every IN_DOUBT refund awaiting a human vanishes along with the question. |

```bash
# Takeable WHILE THE GUARD RUNS. VACUUM INTO holds a read transaction, so a
# concurrent writer serializes behind it rather than producing a torn copy.
rzp-guard-operator -mandate m.json -state rzp-guard.db backup \
  -operator you@merchant.example -out backups/rzp-guard-$(date -u +%Y%m%dT%H%M%SZ).db
```

It refuses to overwrite. The copy carries no lease, so restoring it starts
cleanly rather than blaming a process that no longer exists.

**Never `cp` the state file while the guard runs.** In WAL mode that copies the
main database without the `-wal`, so it is missing every committed transaction
that has not been checkpointed — a file that opens cleanly and is silently out
of date, which is the worst shape a backup can have.

### Verify before you need it

```bash
rzp-guard-operator verify-backup -out backups/rzp-guard-20260905T101500Z.db
```

Needs no state file, no mandate and no running guard — the moment this is needed
is the moment the original is gone. It runs `integrity_check`, refuses a schema
version this build cannot read, and confirms the **operator credential** is
present, without which a restored file is one the guard refuses to start against.

The counts it prints are the point: "1.2 MB written" says nothing, "3 mandates,
412 actions, 2 awaiting an operator" is something you can sanity-check.

### Restoring

```sh
# 1. Stop the guard.
# 2. Verify the backup (above). Do not skip this.
# 3. cp backups/<chosen>.db rzp-guard.db
# 4. Start the guard, then immediately:
rzp-guard-operator -mandate m.json -state rzp-guard.db list
```

Step 4 matters: a restored file may hold IN_DOUBT actions from before the
backup, and those refunds may have landed since. Reconcile them against Razorpay
by receipt before letting an agent near it.

---

## Production mode

```sh
rzp-guard -mode production ...     # or RZP_GUARD_MODE=production
```

It **refuses to start** unless every optional protection is present, and reports
all of them at once:

| Requirement | Why |
| --- | --- |
| `-mandate-pubkey` and a valid signature | Unsigned, anyone who can write the mandate file grants authority. Every other check assumes the mandate is genuine and nothing else establishes it. |
| `-decision-log` | Without it, an incident review has only stderr. |
| `-refund-timeout` | A hung child otherwise holds a reservation and its budget indefinitely. |
| `-admin-addr` or `-status-file` | With neither, nothing reports a refund stuck awaiting an operator. |

Set it in the unit file or container spec rather than on the command line: it
then survives somebody retyping an argument list at 3am.

---

## Metrics and health

```sh
rzp-guard -admin-addr 127.0.0.1:9090 ...
```

**Loopback only, and refused otherwise** — not warned about. A `0.0.0.0` bind is
a metrics port enumerating a merchant's refund activity.

| Endpoint | Means |
| --- | --- |
| `/healthz` | The process is alive. Nothing more, deliberately: a liveness probe that fails on a dependency gets the process restarted, and a restart here promotes every in-flight reservation to IN_DOUBT. |
| `/readyz` | Not ready when operator grants cannot be read — a guard in that state refuses refunds a human already approved, and looks identical to one nobody has issued grants to. IN_DOUBT actions do *not* make it unready: they need a person, not a load balancer. |
| `/metrics` | Prometheus text format. **Aggregates only** — no action ids, payment ids or receipts. A scrape is stored and forwarded; identifiers should not travel that way. The status file carries them instead, at 0600. |

The one to alert on is `rzp_guard_in_doubt_actions > 0`. The one that tells you
something about your *mandates* rather than your guard is
`rzp_guard_operator_approved_total`.

---

## What is not here

No supervisor configuration, no capacity plan, no timed restore drill. Those are
real gaps and they are listed as such in the README's limits rather than papered
over here. In particular: the backup path above has never been exercised under
an actual failure, only under tests, and an untested restore is a plan rather
than a capability.

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
