# FROZEN REFERENCE PROTOTYPE — NOT THE PRODUCT

This Python package is a **behavioural reference**, kept to pin the decision
semantics the Go implementation must reproduce. It is **not** shipped, not the
runtime, and not evidence of Build Quality beyond unit-level policy behaviour.

**Nothing here has ever communicated with Razorpay's MCP server.**

The product is Go (`../../cmd`, `../../internal`). Go is the sole runtime:
relay, mandate compiler, policy, lifecycle, durable state, dashboard, operator
command. There are deliberately not two production implementations.

## What it is good for

- The 28 policy tests encode decision semantics the Go port must match.
- Six confirmed defects found in it are recorded in [FAILURES.md](../../FAILURES.md)
  and are **required test cases for the Go implementation** — the port must not
  reproduce them.

## Running it (reference only)

```
.venv/Scripts/python -m pytest prototype/python
```
