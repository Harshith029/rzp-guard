# FROZEN REFERENCE PROTOTYPE — NOT THE PRODUCT, AND NOT A SPECIFICATION

This Python package is a **historical record** of the Phase 2 design. It is not
shipped, not the runtime, and not evidence of build quality.

**Nothing here has ever communicated with Razorpay's MCP server.**

The product is Go (`../../cmd`, `../../internal`). Go is the sole runtime:
relay, mandate compiler, policy, lifecycle, durable state and operator command.
There are deliberately not two production implementations.

## The Go implementation deliberately DIVERGES from this

An earlier version of this file said the prototype "pins the decision semantics
the Go implementation must reproduce", and one paragraph later that six of its
defects "must not be reproduced". Both cannot be true, and the second is the
accurate one. Where this prototype and the Go product disagree, **assume the Go
product is right and this is the superseded behaviour.**

The Go source names the divergences at the point of each fix:

| Prototype behaviour | Where Go rejects it |
|---|---|
| `int(50000.9) == 50000` — authorized a fraction as its truncation, then forwarded the original | `internal/policy/policy.go`, `policy_test.go` (FAILURES.md F1) |
| `"rzpg_" + action_id` — produced the 6-character `rzpg_a` against a documented 10-character floor | `internal/mandate/mandate.go` |
| Panicked on `float64` values that `json.Decoder.UseNumber` can produce | `internal/policy/policy_test.go` |

## What it is still good for

Reading. The 28 tests in `tests/test_phase2_gates.py` show what the decision
model looked like before the live probes and the review rounds reshaped it, and
several of them describe cases the Go implementation now handles differently and
better.

**Nothing verifies that Go and this agree, and nothing should.** They are not
supposed to agree. The Go behaviour is pinned by Go tests, the live gates and the
Phase 4b traces — not by this.

## Running it (reference only)

Not run by `run.sh` or by CI, and not required to pass for the product to be
correct.

```
.venv/Scripts/python -m pytest prototype/python
```
