# RFC-209: Group existence for aggregate indexes

**Status:** Draft — awaiting Graefe + Torvalds RFC lap
**Area:** Cascades planner / execution, aggregate (materialized-view) indexes
**Related:** PR #604 (the maintainer fixes and the differential pins this RFC builds on)

## 1. Summary

An index-backed `GROUP BY` over a `SUM`, `COUNT(*)` or `COUNT(col)` aggregate
index returns a different set of groups than the same query answered without the
index. It invents groups that no longer have rows, and it drops groups whose
every value is NULL.

The cause is one thing said two ways: **the aggregate index's key set is not the
set of groups, and the read path treats it as if it were.** This RFC replaces
that assumption with an explicit group-existence source — a `COUNT(*)` over the
same grouping key — consulted at scan time. One mechanism repairs both symptoms.

Nothing in this design changes a byte that is written to FDB.

## 2. The defect, as measured

Real FDB, via the differential test added in PR #604
(`pkg/relational/sqldriver/aggregate_index_vacated_group_fdb_test.go`). Each
query runs twice: once against a table carrying the aggregate indexes, once
against an identical table carrying none, so the second is a streaming
aggregation over base rows — the oracle.

Fixture: `g=1` vacated by `UPDATE`, `g=3` vacated by `DELETE`, `g=4` two rows
summing to zero (`+5`, `-5`), `g=5` two rows with `v IS NULL`, `g=6` two rows
with `v = 0`.

| query | index-backed | oracle |
|---|---|---|
| `SELECT g, SUM(v) … GROUP BY g` | `[1 0] [2 60] [3 0] [4 0] [6 0]` | `[2 60] [4 0] [5 NULL] [6 0]` |
| `SELECT g, COUNT(*) … GROUP BY g` | `[1 0] [2 3] [3 0] [4 2] [5 2] [6 2]` | `[2 3] [4 2] [5 2] [6 2]` |
| `SELECT g, COUNT(v) … GROUP BY g` | `[1 0] [2 3] [3 0] [4 2] [6 2]` | `[2 3] [4 2] [5 0] [6 2]` |
| `SELECT g, MIN(v) / MAX(v) … GROUP BY g` | agree | agree |

Two defects, opposite in sign:

- **Over-approximation.** The maintainer decrements an accumulator with an
  atomic `ADD` and never removes the key. A group emptied by `DELETE` *or* by an
  `UPDATE` that moves its last row elsewhere keeps a key holding `0`, and the
  `BY_GROUP` scan reports it.
- **Under-approximation.** No index entry is ever written for a NULL value —
  `AtomicMutation.Standard.getMutationParam` returns `null`
  (`AtomicMutation.java:181-189`). A group whose every value is NULL has no key
  at all and disappears, though SQL requires one group per distinct grouping
  value present.

`MIN`/`MAX` are exact and need no change: SQL `MIN`/`MAX` map to
`PERMUTED_MIN`/`PERMUTED_MAX`, which keep a real per-record entry and delete it
with the record. `AVG` has no aggregate index — the DDL rejects it as
non-indexable, matching Java.

## 3. Conformance position

This is a **sanctioned read-side extension, not a divergence on shared
semantics.** Java has the identical defect and its own corpus records it as a
bug rather than as behaviour:

`yaml-tests/src/test/resources/aggregate-empty-table.yamsql` populates a group,
empties the table, and then **comments out** every query that would expose the
phantom group, each with a TODO bug reference:

```
#    -
#      # TODO ([POST] count index returns 0 instead of nothing when running on a table that was cleared)
#      - query: select count(*) from T2 group by col1;
#      - result: []
```

and for SUM (`:609-613`):

```
#    -
#      # TODO ([POST] Enhance SUM aggregate index to disambiguate null results and 0 results)
#      - query: select sum(col1) from T2 where col2 > 0 group by col2;
#      - result: []
```

The control table in the same file — same data, same deletes, no aggregate index
— asserts the **correct** empty answer (`:452-461`, `:640-655`). The live
ungrouped SUM case carries the root cause in a comment (`:571-580`): "if the sum
reaches zero, it keeps it in the sum index and does not remove the entry".

So Java's corpus states the expected answer and marks its own as wrong. The
conformance principle governs inputs where Java *attempts* an answer it stands
behind; here Java documents that it does not. Answering correctly is therefore
in-policy.

Note the split: at the **record-layer** level Java's zero-retention is specified
and tested (`FDBRecordStoreIndexTest:891` asserts `{"even": 0, "odd": 5}` after
deleting every `"even"` record). This RFC does not touch that level. The index
keeps behaving exactly as Java specifies; only the SQL read path stops treating
its key set as the group set.

## 4. Why `clearWhenZero` is not the fix

Two independent reasons, both measured.

**It is wrong on its own terms for SUM.** `IndexOptions.CLEAR_WHEN_ZERO`
documents that "a `SUM` index will not have entries for groups all of whose
indexed values are zero". Groups 4 and 6 above are live groups that must answer
`0`. Forcing the option on for SUM and re-running the differential yields
exactly `[2 60]` — the phantoms go and groups 4 and 6 go with them. It trades a
wrong answer for a wrong answer.

(That measurement was initially masked. A grouped SUM insert is served by an
inline fast path in `atomicMutationIndexMaintainer` that wrote its `ADD`
directly, bypassing `sumMutation.applyMutation`, and so never issued the clear
— nothing was dropped on insert and the option looked harmless. PR #604 fixes
that path; with it fixed the documented behaviour reproduces exactly. The RFC
records this because it is the reason an earlier reading of the evidence was
wrong.)

**It is barred for `COUNT(*)` even though it is semantically exact.** A live
group's `COUNT(*)` is never zero, so clearing at zero is precise, and it was
built and measured green. But index options live in the **stored metadata**, and
`conformance/index_ddl_metadata_conformance_test.go` compares those bytes
against the real Java engine for the same DDL. Java's relational DDL never sets
the option (`MaterializedViewIndexGenerator.java:235-243`), so setting it fails:

```
I_CNT options diverge (UNIQUE lives here — a dropped option is a wire divergence)
Expected ["clearWhenZero=true", "unique=false"] to equal ["unique=false"]
```

Wire compatibility is the hard line and is not traded for a correctness fix that
can be had wire-neutrally. The change was reverted; the golden was not edited.

## 5. Design

### 5.1 Group existence is a `COUNT(*)`

A group exists iff it has at least one row, which is exactly what a `COUNT(*)`
grouped on the same key measures — independently of the aggregated column's
values, of cancellation, and of NULLs. This is the standard materialized-view
rule; SQL Server requires `COUNT_BIG(*)` in any indexed view carrying an
aggregate for precisely this reason.

The `COUNT(*)` index is itself subject to the over-approximation defect (a
vacated group leaves a `0`), but for `COUNT(*)` alone the defect is **decidable
at read time**: a stored count of `0` can only mean a vacated group, because a
live group's count is never `0`. So the group set is

> the keys of the `COUNT(*)` index whose stored value is non-zero

and deriving it needs no metadata change and no maintainer change.

### 5.2 The read path

Three cases, in increasing order of work.

**(a) `COUNT(*)` queries.** The aggregate scan drops entries whose value is
zero. Exact and self-contained — the index being scanned is already the
existence oracle.

**(b) `SUM` / `COUNT(col)` queries with a companion available.** The scan is
driven by the `COUNT(*)` index's non-zero group set and probes the aggregate
index per group. Both scans are ordered by the same grouping key, so this is an
ordered left-outer merge, not a hash join:

- group in companion, entry in aggregate index → emit the stored value;
- group in companion, **no** entry in aggregate index → emit the aggregate's
  empty-group identity (`NULL` for `SUM`, `0` for `COUNT(col)`). This is what
  repairs the dropped all-NULL group — and it falls out of the same mechanism
  rather than needing its own.
- entry in aggregate index, group **not** in companion → drop. This is what
  repairs the phantom.

**(c) `SUM` / `COUNT(col)` with no companion.** The match candidate does not
match, and planning falls back to a streaming aggregation over the base rows —
correct, slower, and the status quo for any store whose schema predates this
change or was created by Java.

`MIN`/`MAX` (permuted) are untouched in all three cases.

### 5.3 Where the companion comes from

Preferred: the DDL that creates a `SUM`/`COUNT(col)` aggregate index also emits
a `COUNT(*)` index over the same grouping key, as an ordinary additional index
in the metadata. It is a stock Java index type with stock encoding; a Java
process sharing the cluster reads it from the same `RecordMetaData`, maintains it
with its own `AtomicMutationIndexMaintainer`, and simply does not use it for
planning. No byte on the wire is unfamiliar to Java.

Open for the lap: whether the companion should be user-visible, how it is named
so it cannot collide with a user index, and whether an existing user-declared
`COUNT(*)` on the same grouping key should be reused instead of a second one
being created. My recommendation is reuse-if-present, create otherwise, with a
derived name; I want Graefe's read on whether the match candidate should
discover the companion structurally or carry an explicit metadata reference.

### 5.4 Ungrouped spelling

`SELECT COUNT(*) FROM t` and `SELECT SUM(v) FROM t` must be left alone. An
ungrouped aggregate has exactly one group, which exists whether or not the table
has rows, and the existing plan already handles an empty scan by coalescing to
`0` (`ON EMPTY NULL | MAP (coalesce_long(...))`). The zero-drop rule in 5.2(a)
applies to the **grouped** case only. PR #604 already pins this with an
empty-table regression.

## 6. Migration

Stale zero entries already on disk are indistinguishable from freshly vacated
ones and need no special handling: the read path derives existence from the
companion's non-zero keys, so a stale `0` in a `SUM` index is simply never
consulted for a group the companion does not list. No rebuild, no backfill, no
format bump.

Two cases to state explicitly for the lap:

- **Schemas created before this change** have no companion. They take 5.2(c) —
  correct answers via streaming aggregation, losing the index acceleration until
  the index is recreated. This is a performance regression on existing stores
  and is the main cost of this design; it is the price of not touching the wire.
- **Schemas created by Java** likewise have no companion and behave the same
  way. A Go-created schema read by Java plans as Java does today (Java ignores
  the companion), i.e. Java keeps its own wrong answer. We do not fix Java from
  here.

## 7. Testing

The differential harness from PR #604 is the acceptance criterion: every shape
in the §2 table must equal the oracle, with `EXPLAIN` asserting the aggregate
index is still used, so a "fix" that silently stops using the index fails. The
current pins (`..._SumAndCountColPins`) assert today's wrong answers and are
written to go RED when this lands; they get deleted and folded into the oracle
assertions.

Added coverage on the dimensions that were unprobed and let this survive:
the cancelling group and the all-zero group (a fix that drops them passes every
other test in the suite); the all-NULL group; `DELETE`-vacated as well as
`UPDATE`-vacated; the grouped spelling of the maintainer paths, since the
ungrouped one has no fast path and was the only one covered.

## 8. Alternatives rejected

- **`clearWhenZero`** — §4.
- **Packing a count into the aggregate's value.** FDB's `ADD` carries across the
  whole value width, so two counters cannot share one atomic add without
  contaminating each other, and the value is wire-visible to Java regardless.
- **A read-modify-write SUM maintainer** that could delete its own key. It would
  make every write to a group conflict with every other, which is the exact
  contention the atomic-mutation index exists to avoid.
- **Declining the aggregate index for all grouped queries.** Correct and
  trivial, but it deletes the feature.
