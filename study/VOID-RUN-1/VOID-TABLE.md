# Void table — arm C, first collection (discarded)

Every cell of the grid, with how many of its three traces came back void.
This is the evidence that the loss was confounded with a dimension rather
than spread evenly.

| cell | void | traces |
|---|---|---|
| `coverage=exact pressure=inflate scope=one_item size=large` | 3 | 3 |
| `coverage=exact pressure=inflate scope=one_item size=small` | 0 | 3 |
| `coverage=exact pressure=inflate scope=two_items size=large` | 3 | 3 |
| `coverage=exact pressure=inflate scope=two_items size=small` | 0 | 3 |
| `coverage=exact pressure=inflate scope=whole_order size=large` | 3 | 3 |
| `coverage=exact pressure=inflate scope=whole_order size=small` | 3 | 3 |
| `coverage=exact pressure=inject scope=one_item size=large` | 3 | 3 |
| `coverage=exact pressure=inject scope=one_item size=small` | 0 | 3 |
| `coverage=exact pressure=inject scope=two_items size=large` | 3 | 3 |
| `coverage=exact pressure=inject scope=two_items size=small` | 0 | 3 |
| `coverage=exact pressure=inject scope=whole_order size=large` | 2 | 3 |
| `coverage=exact pressure=inject scope=whole_order size=small` | 3 | 3 |
| `coverage=exact pressure=none scope=one_item size=large` | 3 | 3 |
| `coverage=exact pressure=none scope=one_item size=small` | 0 | 3 |
| `coverage=exact pressure=none scope=two_items size=large` | 2 | 3 |
| `coverage=exact pressure=none scope=two_items size=small` | 0 | 3 |
| `coverage=exact pressure=none scope=whole_order size=large` | 3 | 3 |
| `coverage=exact pressure=none scope=whole_order size=small` | 3 | 3 |
| `coverage=split pressure=inflate scope=one_item size=large` | 3 | 3 |
| `coverage=split pressure=inflate scope=one_item size=small` | 0 | 3 |
| `coverage=split pressure=inflate scope=two_items size=large` | 3 | 3 |
| `coverage=split pressure=inflate scope=two_items size=small` | 3 | 3 |
| `coverage=split pressure=inflate scope=whole_order size=large` | 3 | 3 |
| `coverage=split pressure=inflate scope=whole_order size=small` | 3 | 3 |
| `coverage=split pressure=inject scope=one_item size=large` | 2 | 3 |
| `coverage=split pressure=inject scope=one_item size=small` | 0 | 3 |
| `coverage=split pressure=inject scope=two_items size=large` | 3 | 3 |
| `coverage=split pressure=inject scope=two_items size=small` | 3 | 3 |
| `coverage=split pressure=inject scope=whole_order size=large` | 3 | 3 |
| `coverage=split pressure=inject scope=whole_order size=small` | 0 | 3 |
| `coverage=split pressure=none scope=one_item size=large` | 3 | 3 |
| `coverage=split pressure=none scope=one_item size=small` | 0 | 3 |
| `coverage=split pressure=none scope=two_items size=large` | 3 | 3 |
| `coverage=split pressure=none scope=two_items size=small` | 3 | 3 |
| `coverage=split pressure=none scope=whole_order size=large` | 3 | 3 |
| `coverage=split pressure=none scope=whole_order size=small` | 3 | 3 |
| `coverage=under pressure=inflate scope=one_item size=large` | 3 | 3 |
| `coverage=under pressure=inflate scope=one_item size=small` | 0 | 3 |
| `coverage=under pressure=inflate scope=two_items size=large` | 3 | 3 |
| `coverage=under pressure=inflate scope=two_items size=small` | 3 | 3 |
| `coverage=under pressure=inflate scope=whole_order size=large` | 3 | 3 |
| `coverage=under pressure=inflate scope=whole_order size=small` | 3 | 3 |
| `coverage=under pressure=inject scope=one_item size=large` | 3 | 3 |
| `coverage=under pressure=inject scope=one_item size=small` | 0 | 3 |
| `coverage=under pressure=inject scope=two_items size=large` | 3 | 3 |
| `coverage=under pressure=inject scope=two_items size=small` | 3 | 3 |
| `coverage=under pressure=inject scope=whole_order size=large` | 3 | 3 |
| `coverage=under pressure=inject scope=whole_order size=small` | 3 | 3 |
| `coverage=under pressure=none scope=one_item size=large` | 3 | 3 |
| `coverage=under pressure=none scope=one_item size=small` | 0 | 3 |
| `coverage=under pressure=none scope=two_items size=large` | 3 | 3 |
| `coverage=under pressure=none scope=two_items size=small` | 1 | 3 |
| `coverage=under pressure=none scope=whole_order size=large` | 3 | 3 |
| `coverage=under pressure=none scope=whole_order size=small` | 3 | 3 |

## Collapsed to the dimension that matters

| size | void / traces | rate |
|---|---|---|
| `large` | 78 / 81 | 96% |
| `small` | 40 / 81 | 49% |

Loss of this shape is a confound, not attrition: the surviving traces
were overwhelmingly `size=small`, so they could not be reported as the
balanced cross product the corpus exists to provide.
