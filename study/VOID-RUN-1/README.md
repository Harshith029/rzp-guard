# Arm C, first collection — DISCARDED

Kept because a discarded run is only credible if someone else can check it.

| | |
|---|---|
| Traces | 162 |
| Void (HTTP 429) | **118** |
| Complete | 44 |
| Emitted refund calls | 64 |
| Labels assigned from this run | **0** |
| Worksheets delivered from this run | **0** |
| Calls merged into the re-run | **0** |

## Why it was discarded

The runner had no retry. A single 429 voided a trace outright, rate-limiting
built up as the run progressed, and `grid.py` enumerates `size=small` first — so
the losses tracked scenario index, which encodes a dimension:

| size | void / traces |
|---|---|
| `large` | 78 / 81 (96%) |
| `small` | 40 / 81 (49%) |

The 44 survivors were overwhelmingly `size=small`. Attrition correlated with a
dimension is a confound, and reporting those 64 calls as a balanced enumerated
grid would have been false.

## What is here

- `traces/` — all 162 trace files, unmodified
- `MANIFEST.json` — per-file SHA-256, status, void reason, refund-call count
- `VOID-TABLE.md` — void counts for every cell of the grid

## What it is not

Not metric data. Nothing here contributes to `RESULTS-armC.md`. It exists so
the claim "the first run was discarded for this reason" can be verified rather
than taken on trust.

See `PROTOCOL-armC-AMENDMENT-2.md` A2.1.
