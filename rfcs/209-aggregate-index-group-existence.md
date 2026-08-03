# RFC-209 — Group existence for aggregate indexes

- **Status:** PROPOSED
- **Branch:** `rfc/209-aggregate-index-group-existence`
- **Area:** Cascades planner / execution, aggregate (materialized-view) indexes
- **Depends on:** PR #604 (`fix/agg-index-vacated-group`) — the maintainer parity
  fixes and the differential pins every measurement below is taken from. This RFC
  is read-side only; #604 is code-only and carries no part of this design.

## 1. Summary

An index-backed `GROUP BY` over a `SUM`, `COUNT(*)` or `COUNT(col)` aggregate
index returns a different set of groups than the same query answered without the
index. It invents groups that no longer have rows, and it drops groups whose
every value is NULL.

The cause is one thing said two ways: **the aggregate index's key set is not the
set of groups, and the read path treats it as if it were.** This RFC replaces
that assumption with an explicit group-existence source — a `COUNT(*)` over the
same grouping key, discovered structurally — consulted by a plan-visible
operator. One mechanism repairs both symptoms.

Nothing in this design changes a byte that is written to FDB.

## 2. The defect, as measured

Real FDB, via `TestFDB_AggregateIndexVacatedGroup_SumAndCountColPins` and
`TestFDB_AggregateIndexVacatedGroup` in
`pkg/relational/sqldriver/aggregate_index_vacated_group_fdb_test.go` (PR #604).
Each query runs twice: once against a table carrying the aggregate indexes, once
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
- **Under-approximation.** No index entry is written for a NULL value. For
  `SUM_LONG` that is `AtomicMutation.java:181-189` (a null `Number` yields a
  null mutation param); for `COUNT_NOT_NULL` it is the separate arm at
  `AtomicMutation.java:165-171`, via `entry.keyContainsNonUniqueNull()`. Either
  way a group whose every value is NULL has no key at all and disappears, though
  SQL requires one group per distinct grouping value present.

**The table validates the design arithmetically, which is worth stating
outright.** `g=5` is absent from the SUM and `COUNT(v)` rows but present as
`[5 2]` in the `COUNT(*)` row. That asymmetry is not incidental — it *is* §5.1's
decidability claim, because `COUNT(*)` never drops a live group precisely
because it never looks at the value. The non-zero `COUNT(*)` keys are
`{2, 4, 5, 6}`, which is exactly the oracle group set in all three rows.

`MIN`/`MAX` are exact and need no change: SQL `MIN`/`MAX` map to
`PERMUTED_MIN`/`PERMUTED_MAX`, which keep a real per-record entry and delete it
with the record. `AVG` has no aggregate index — the DDL rejects it as
non-indexable, matching Java.

## 3. Conformance position

This is a **sanctioned read-side extension, not a divergence on shared
semantics.** Java has the identical defect and its own corpus records it as a
bug rather than as behaviour.

`yaml-tests/src/test/resources/aggregate-empty-table.yamsql` populates a group,
empties the table, and then **comments out** every query that would expose the
phantom group, each with a TODO bug reference — `COUNT(*)` at `426-429` and
`434-437`, `COUNT(col)` at `504-507` and `512-515`:

```
#    -
#      # TODO ([POST] count index returns 0 instead of nothing when running on a table that was cleared)
#      - query: select count(*) from T2 group by col1;
#      - result: []
```

and the grouped SUM at `611-614`:

```
#    -
#      # TODO ([POST] Enhance SUM aggregate index to disambiguate null results and 0 results)
#      - query: select sum(col1) from T2 where col2 > 0 group by col2;
#      - result: []
```

The control table in the same file — same data, same deletes, no aggregate index
— asserts the **correct** empty answer (`450-461`, `639-654`). The live ungrouped
SUM case carries the root cause in a comment at `572-580`; note that the live
comment reads `# TODO (enhance SUM aggregate index…)` (lowercase, no `[POST]`),
distinct from the `[POST] Enhance…` wording in the commented block at `612`.

So Java's corpus states the expected answer and marks its own as wrong. The
conformance principle governs inputs where Java *attempts* an answer it stands
behind; here Java documents that it does not. Answering correctly is in-policy.

**The record-layer level is out of scope and stays as Java specifies it.** In
`fdb-record-layer-core/src/test/java/com/apple/foundationdb/record/provider/foundationdb/FDBRecordStoreIndexTest.java:891`
the assertion is parameterized on the option, and both arms matter:

```java
assertEquals(clearWhenZero ? ImmutableMap.of("odd", 5L) : ImmutableMap.of("even", 0L, "odd", 5L), …)
```

The false arm pins zero-retention as *specified* behaviour at the record-layer
level — which this RFC does not touch. The true arm is direct corroboration of
§4: Java itself pins that `clearWhenZero` makes the vacated group **disappear
entirely** rather than answer 0. Only the SQL read path changes here; the index
keeps behaving exactly as Java specifies. (Note there are two files named
`FDBRecordStoreIndexTest.java` in the Java tree; only the
`provider/foundationdb/` one is meant.)

## 4. Why `clearWhenZero` is not the fix

Two independent reasons, both measured.

**It is wrong on its own terms for SUM.** `IndexOptions.java:176` (doc block
`170-180`) states: "A `SUM` index will not have entries for groups all of whose
indexed values are zero." Groups 4 and 6 above are live groups that must answer
`0`. Forcing the option on for SUM and re-running the differential on branch
`fix/agg-index-vacated-group` yields exactly `[2 60]` — the phantoms go and
groups 4 and 6 go with them. It trades a wrong answer for a wrong answer.

(That measurement was masked at first by a second defect: two inline `AddBytes`
fast paths for grouped SUM bypassed `sumMutation.applyMutation` and so never
issued the `COMPARE_AND_CLEAR`, making the option look harmless on insert. Fixed
in PR #604 via `atomicMutationIndexMaintainer.clearIfZero`, called at
`pkg/recordlayer/atomic_mutation_index_maintainer.go:271` and `:319`. The RFC
records this because it is why an earlier reading of the evidence was wrong.)

**It is barred for `COUNT(*)` even though it is semantically exact.** A live
group's `COUNT(*)` is never zero, so clearing at zero is precise, and it was
built and measured green. But index options live in the **stored metadata**, and
`conformance/index_ddl_metadata_conformance_test.go` compares them against the
real Java engine. On branch `fix/agg-index-vacated-group` the run failed at
`:260` with that file's own message:

```
I_CNT options diverge (UNIQUE lives here — a dropped option is a wire divergence)
Expected ["clearWhenZero=true", "unique=false"] to equal ["unique=false"]
```

Java's relational layer never sets the option anywhere — `grep -rn
"CLEAR_WHEN_ZERO\|clearWhenZero" fdb-relational-core/src/main/java/` returns
**zero hits tree-wide**, which is the claim a line-range citation cannot make.
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

and deriving it needs no metadata change and no maintainer change. §2's table is
the arithmetic proof.

### 5.2 Companion discovery is structural

**The companion is found by structure, never by a stored reference.** §4 forces
this: an explicit companion pointer on the SUM index is a stored index property
that Java's `MaterializedViewIndexGenerator` never emits, so it is the same
class of object as `clearWhenZero` and fails the identical assertion at
`conformance/index_ddl_metadata_conformance_test.go:260`. An RFC cannot argue §4
and then leave a metadata reference on the table.

Matching and creation are two different questions and get two different answers:

- **Matching accepts any structurally-qualifying `COUNT(*)`** — user-declared or
  DDL-emitted alike. A `COUNT` index over the same record type whose grouping
  key expression matches, compared by *normalized root expression*: the same
  `normalizedProto(GetRootExpression())` normalization the wire-conformance test
  itself uses (`index_ddl_metadata_conformance_test.go:252-255`). The SUM index
  has no way to know which companion it "owns", and a user who already declared
  one must not pay for a duplicate.
- **Creation is create-if-absent.** The DDL emits a companion only when no
  qualifying index already exists.

This also settles "companion dropped, SUM remains" without machinery: discovery
re-runs against current metadata at plan time, finds nothing, the match declines,
and planning falls back to streaming aggregation (5.3(c)). An explicit reference
would instead leave a dangling pointer every plan must handle, and would force
`DROP INDEX` to either block or cascade — both metadata-visible, both strictly
worse.

**Naming and collision.** The emitted companion is named by a deterministic
derivation from the owning index's name with a reserved suffix that the DDL
rejects in user-supplied index names, so collision is prevented at creation
rather than resolved afterwards. The name is metadata-visible, which is exactly
why it must be deterministic: a randomized or counter-based name would make two
stores built from the same DDL differ in stored bytes, reintroducing §4 by the
back door. The name carries no semantic weight — matching is structural, so a
companion created by an older binary under a different name still matches.

**The reserved suffix is a user-visible DDL restriction, and Java has none.**
This must be stated plainly rather than buried in the naming rule: `CREATE INDEX`
begins rejecting a name Java's DDL accepts, which narrows the shared DDL surface
rather than extending the read side. It is justified because the alternative is
worse — without reservation, create-if-absent can collide with a pre-existing
user index of the same derived name but different structure, and the failure
surfaces at schema-creation time as a confusing duplicate-name error rather than
at the point the user chose the name. The restriction applies at **`CREATE` only,
never at load.** A schema that already contains an index whose name ends with the
suffix — created by Java, or by an older Go binary — must open and read normally.
That is forced by the rule two paragraphs up: matching is structural and the name
carries no semantic weight, so a colliding name is inert at read time and there
is nothing to reject. Rejecting at load would make this design able to render an
existing Java-written schema unreadable, which no read-side extension may do.

### 5.3 The read path is a plan-visible operator

The existence check **must not** live inside the aggregate-index cursor. A
cursor that secretly opens a second index scan makes the plan a lie: `EXPLAIN`
shows one `AISCAN` while two run, the cost model prices a two-scan plan as one,
and ordering and compensation cannot reason about the hidden stream. That is a
parallel pipeline, which this repo does not permit.

It is a **binary (or n-ary) operator yielded by `AggregateDataAccessRule`, with
the companion scan as a real child.** The rule either yields the companion-joined
plan or yields nothing, and streaming aggregation wins on cost — one query path,
visible to the cost model.

Three cases:

**(a) `COUNT(*)` queries.** The aggregate scan drops entries whose value is
zero. Exact and self-contained — the index being scanned is already the
existence oracle, so no second stream and no new operator.

**(b) `SUM` / `COUNT(col)` with a readable companion.** An **outer** merge
driven by the companion's non-zero group set, over streams already ordered by
the same grouping key:

- group in companion, entry in aggregate index → emit the stored value;
- group in companion, **no** entry in aggregate index → emit the aggregate's
  empty-group identity (`NULL` for `SUM`, `0` for `COUNT(col)`). This is what
  repairs the dropped all-NULL group, and it falls out of the same mechanism;
- entry in aggregate index, group **not** in companion → drop. This repairs the
  phantom.

**Relationship to existing machinery.** Go already merges aggregate scans
sharing a grouping key: `tryMultiAggregateIntersection`
(`pkg/recordlayer/query/plan/cascades/rule_aggregate_data_access.go:304`) builds
a `RecordQueryMultiIntersectionOnValuesPlan`, mirroring Java's
`AggregateDataAccessRule.createIntersectionAndCompensation`
(`fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/query/plan/cascades/rules/AggregateDataAccessRule.java:114`).
This operator is that operator's **outer sibling**, and the difference is
load-bearing: intersection semantics drop case 2, which *is* the all-NULL defect
this RFC exists to fix. The design therefore **extends** the existing operator
with a designated driving stream and outer semantics rather than adding an
unrelated one, so multi-aggregate queries keep a single merge rather than
stacking two.

**Multi-aggregate.** `SELECT g, SUM(v), COUNT(w) FROM t GROUP BY g` already goes
through the multi-aggregate merge. Under this design the companion becomes an
additional **driving** stream with outer semantics against *both* aggregate
streams: the group set comes from the companion, and each aggregate stream
independently contributes either its stored value or its identity. Group
existence is decided once, not per aggregate.

**Compensation under outer semantics.** Java's method is
`createIntersectionAndCompensation`, and changing the merge to outer changes what
compensation sits above. A compensating filter now sees **identity rows** that
did not previously exist: a group present in the companion but absent from the
aggregate index arrives as `SUM(v) = NULL` or `COUNT(col) = 0` rather than not
arriving at all. Two predicates change answers as a direct result:

- `HAVING SUM(v) IS NULL` begins returning all-NULL groups, which previously had
  no row to test.
- `HAVING COUNT(col) = 0` likewise begins returning them.

**Both are the intended fix, not collateral.** They are the `g=5` row of §2's
table seen through a filter — the oracle returns those groups, so a `HAVING` over
the oracle already returns them, and index-backed and unindexed answers converge.
This is called out because "the compensating filter now sees rows it never saw"
is exactly the kind of change that looks like a regression in review; it must be
recognised as the under-approximation repair reaching `HAVING`, and pinned by
tests on both predicates (§7).

Compensation is otherwise unaffected: it operates per surviving group on the
merged row, and the merged row's shape is unchanged — only the set of groups
reaching it grows.

**`aggInnerFilterFullyConsumable` is UNCHANGED** (`rule_aggregate_data_access.go:87`).
It gates whether a residual predicate filters the aggregation **input**, in which
case the candidate must be declined because no post-hoc compensation can
reconstruct the aggregate. The companion merge changes the group **set**, not the
input to aggregation. The two are orthogonal and the guard keeps its existing
semantics and its existing decline. Recorded explicitly so the implementer does
not stall deciding whether outer semantics relax it: they do not.

**(c) No readable companion.** The match candidate does not match, and planning
falls back to a streaming aggregation over base rows — correct, slower, and the
status quo for any store whose schema predates this change or was created by
Java.

### 5.3.1 Cost: this is a cost-model change

The operator is visible to the cost model (§5.3), which is only worth anything if
it is **priced**. The companion-joined plan is not a cheaper spelling of the
aggregate-index plan — it is a second index scan, and the RFC must say so.

**What it costs.** The companion scan is a `BY_GROUP` scan over the `COUNT(*)`
index, which has one entry per group that has ever existed. So the plan's index
I/O is roughly **twice** the single-aggregate-index plan's, plus the merge. The
cost model must charge the companion scan as a real scan child, not fold it in as
a constant.

**The near-unique grouping key is the case that decides correctness of the
choice.** As the grouping key approaches uniqueness, the number of groups
approaches base-table cardinality, so the companion scan approaches a full scan
of one entry per row — and the aggregate index scan does too. The companion-joined
plan then reads about twice the base-table cardinality in index entries to
produce about that many rows, while streaming aggregation reads the base table
once. **The companion-joined plan can therefore be slower than the plan it is
supposed to beat, and a cost model that prices the companion scan at zero would
pick it anyway.** That is the concrete regression this subsection exists to
prevent.

**When streaming aggregation should win.** Once the estimated group count
approaches base-table cardinality, the aggregate-index path loses its entire
premise — that there are far fewer groups than rows — and streaming aggregation
should win on cost. This is not a new special case: it is the existing
aggregate-index-vs-streaming trade-off with the companion's scan added to the
correct side of the comparison. The change is that the companion's cardinality
must enter the estimate, and the group-count estimate that drives it comes from
the same cardinality machinery the aggregate-index path already relies on.

**Consequence to state plainly:** on a near-unique grouping key this design can
*lose* the index acceleration that today's (wrong) plan enjoys. That is the
correct outcome — today's plan is fast and wrong — but it is a performance change
on real queries and must be reviewed as a cost-model change, not smuggled in as a
correctness fix. Acceptance requires a stress comparison on both a low-cardinality
and a near-unique grouping key (§7).

**Fail-closed by construction.** 5.3(c) is not a check that can be forgotten:
the resolved, readable companion is a **constructor precondition** of the
companion-joined plan, so that plan is unconstructible without one. There is no
code path that builds the fast plan and then validates it.

### 5.4 Ungrouped aggregates are out of scope, and the reason is not the one it looks like

`SELECT COUNT(*) FROM t` is safe on its own terms: an ungrouped aggregate has
exactly one group, which exists whether or not the table has rows, and the
existing plan coalesces an empty scan to `0`
(`ON EMPTY NULL | MAP (coalesce_long(...))`). The zero-drop rule in 5.3(a)
applies to the **grouped** case only; PR #604 pins the ungrouped `COUNT(*)`
behaviour with an empty-table regression.

`SELECT SUM(v) FROM t` is **not** safe on its own terms, and an earlier draft of
this RFC was wrong to claim it was — it borrowed the `COUNT(*)` coalesce shape
as evidence for SUM, which has no coalesce. Java's ungrouped SUM has the same
defect family, visible in one file:

- `aggregate-empty-table.yamsql:547-550` — `select sum(col1) from T1`, no
  aggregate index, emptied table, plan `SCAN([IS T1]) | … | ON EMPTY NULL |
  MAP (_._0._0 AS _0)` → `result: [{!null _}]`, i.e. **NULL**.
- `aggregate-empty-table.yamsql:577-580` — `select sum(col1) from T2`, identical
  data and deletes, SUM index `t2_i5`, plan `AISCAN(T2_I5 <,> BY_GROUP …)` →
  `result: [{0}]`, i.e. **0**.

Oracle and index-backed answer, same block, disagreeing.

**Go does not currently reproduce it, and the reason is accidental.** MEASURED
(`TestFDB_AggregateIndexVacatedGroup_UngroupedSumEmptyTable`, PR #604): Go
answers NULL on an emptied table and 0 on live cancelling rows, agreeing with
the oracle on both — but only because Go's planner never routes an ungrouped
`SELECT SUM(v)` through the aggregate index at all. The plan on the *indexed*
table is `Project([SUM(V)#0], StreamingAgg(keys=[], Scan(SI)))`, a full scan.
There is no index-backed path, so there is nothing to diverge.

So: ungrouped SUM is **out of scope because it is not index-backed**, not
because it is correct by design. The test pins exactly that, and fails if the
plan ever becomes index-backed. The moment it does, the companion rule must
extend to the ungrouped case and an emptied table must yield NULL, not the
stored 0, with `:547-550` vs `:577-580` as the oracle — the same two ranges the
test's own failure message cites, so the two documents agree.

(NOT root-caused: *why* the planner declines. `AggregateDataAccessRule` matches
`*expressions.GroupByExpression` and the translator builds one even with zero
grouping keys, so the rule looks reachable. Tracing it is a prerequisite for any
future work that makes the ungrouped path index-backed, not for this RFC.)

## 6. Migration and index state

**Stale zero entries need no handling.** The read path derives existence from the
companion's non-zero keys, so a stale `0` in a SUM index is never consulted for a
group the companion does not list. No rebuild, no backfill, no format bump.

**A companion that is not readable must not be used.** An index in `WRITE_ONLY`,
disabled, or mid-backfill has a *partial* key set. Driving the merge from it
would drop **live** groups — a brand-new wrong answer, strictly worse than
today's phantom. The match candidate therefore requires the companion to be
readable; anything else takes 5.3(c). This is part of the constructor
precondition in §5.3, not a separate check.

**Existing stores lose acceleration until the index is recreated.** Schemas
created before this change, and all Java-created schemas, have no companion and
take 5.3(c): correct answers via streaming aggregation, no index acceleration.
This is the main cost of the design and the price of not touching the wire. A
Go-created schema read by Java plans as Java does today (Java ignores the
companion), i.e. Java keeps its own wrong answer; we do not fix Java from here.

**Precedent and expiry.** Documenting rather than forcing an on-disk migration
follows CQ-90 (`TODO.md:13822`), the on-disk migration decision for CQ-89's
cardinality key change, which landed with #603. That ruling rests on every
affected store being dev/test, and so does this one. **Expiry condition:** the
moment a production deployment exists, "document, do not force" stops being
available and a rebuild path becomes a prerequisite. This section is void at
first production deployment and must be re-decided then.

**Goldens that flip when this lands.** Two expectations encode the
all-NULL-vanishes defect and must gain the missing group:

- `pkg/relational/conformance/yamsql/testdata/aggregate_index_count_not_null.yaml`
- `pkg/relational/sqldriver/plan_shape_conformance_test.go` (the `measurements`
  `COUNT(reading)` case)

Both expect `[["humidity", 1], ["temp", 2]]` for a fixture whose `pressure`
sensor has only NULL readings. `pressure` is absent because `COUNT_NOT_NULL`
writes no entry for a NULL value (`AtomicMutation.java:165-171`) — not, as both
comments used to claim, because of `clearWhenZero`, which the SQL layer never
sets. That attribution is corrected in PR #604. Under this design both become
`[["humidity", 1], ["pressure", 0], ["temp", 2]]`. **A patch that does not flip
them has not fixed the under-approximation.**

## 7. Acceptance criteria

Split by companion availability, because the two cases assert opposite things
about the plan.

**Companion present.** Every shape in §2's table equals the oracle, *and*
`EXPLAIN` shows the companion-joined plan — so a "fix" that silently stops using
the index fails. The existing pins
(`TestFDB_AggregateIndexVacatedGroup_SumAndCountColPins`) assert today's wrong
answers and are written to go RED when this lands; they get deleted and folded
into oracle assertions.

**Companion absent — the most important test, and currently missing.** A SUM
index with no qualifying `COUNT(*)`: answers equal the oracle **and** `EXPLAIN`
does **not** contain the aggregate index scan. This is what pins fail-closed
behaviour; without it, §5.3(c) is an assertion rather than a property.

**Companion present but not readable.** Same assertions as companion-absent, for
an index in `WRITE_ONLY` / disabled / mid-backfill.

**`HAVING` over identity rows (§5.3, compensation).** `HAVING SUM(v) IS NULL` and
`HAVING COUNT(col) = 0` must each return the all-NULL group and must agree with
the oracle. These are the predicates whose answers change because compensation
now sees identity rows, so they are the tests that distinguish "the
under-approximation repair reached `HAVING`" from "a filter regressed".

**Cost (§5.3.1).** A stress comparison on **two** grouping keys, before and
after: one low-cardinality (few groups, many rows) where the companion-joined
plan must win, and one **near-unique** (groups ≈ rows) where streaming
aggregation must win. A run where the planner picks the companion-joined plan on
the near-unique key is a failure of this RFC even if every row is correct —
that is the specific regression §5.3.1 exists to prevent. Both must live on the
same filesystem with headroom, per the repo's stress-comparison rule; a run taken
above ~95% disk utilisation measures the disk, not the plan.

Dimension coverage, chosen from what actually let this survive: the cancelling
group and the all-zero group (a fix that drops them passes every other test in
the suite); the all-NULL group; `DELETE`-vacated as well as `UPDATE`-vacated;
the multi-aggregate shape from §5.3; and the grouped spelling of the maintainer
paths, since the ungrouped one has no fast path and was the only one covered
before PR #604.

## 8. Alternatives rejected

- **`clearWhenZero`** — §4.
- **An explicit companion reference in index metadata** — §5.2; barred by §4 and
  strictly worse on drop-handling.
- **Cursor-internal existence filtering** — §5.3; a hidden parallel pipeline,
  invisible to the cost model.
- **Reusing the intersection operator unchanged** — §5.3; inner semantics drop
  the outer row that *is* the all-NULL fix.
- **Packing a count into the aggregate's value.** FDB's `ADD` carries across the
  whole value width, so two counters cannot share one atomic add without
  contaminating each other, and the value is wire-visible to Java regardless.
- **A read-modify-write SUM maintainer** that could delete its own key. Every
  write to a group would conflict with every other — the exact contention the
  atomic-mutation index exists to avoid.
- **Declining the aggregate index for all grouped queries.** Correct and
  trivial, but it deletes the feature.
