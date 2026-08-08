# RFC-209 — Group existence for aggregate indexes

- **Status:** **ACCEPTED AND IMPLEMENTED.** This document landed as PR #605
  (`329db80fd`); the design it describes landed as PR #612 (`a56472861`),
  companion discovery and the `GroupExistenceMerge` operator included. It read
  `PROPOSED` for some time after that — the rot this port names explicitly, a
  status field claiming a shipped design is unstarted, and it was found by
  someone sent to build a feature that already existed.
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

This design **does** write to FDB, and an earlier draft's claim that it changes
no byte was false. The companion is a stored `Index` in the persisted
`RecordMetaData`, and it maintains an index entry on every insert, update and
delete. What it does not do is introduce a new index type, a new index option, or
a new on-disk format: the companion is a plain grouped `COUNT` index of exactly
the kind Java already writes, already maintains, and already reads back out of
stored metadata without being told about it. §4.1 states the interop facts and
their caveats.

## 2. The defect, as measured

Real FDB, via `TestFDB_AggregateIndexVacatedGroup_SumAndCountColOracle` and
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
can be had with an index kind Java already understands. The change was reverted;
the golden was not edited.

### 4.1 The wire footprint this design does have

§1 states it and §6 depends on it, so it is stated once here in full. The
companion adds a stored `Index` to the persisted `RecordMetaData` and an index
entry maintained on every write. The claim that matters is not that nothing is
written — it is that **a Java app opening the same store maintains the companion
correctly without having created it, and without knowing what it is for.** That
is a property of how Java loads metadata, and it is data-driven end to end
(citations against `fdb-record-layer/`, tag 4.12.11.0):

- Stored metadata is loaded wholesale, with no whitelist:
  `FDBMetaDataStore.loadAndSetCurrent` calls `buildMetaData(proto, validate=false)`
  (`FDBMetaDataStore.java:279`), and `RecordMetaDataBuilder.loadProtoExceptRecords`
  constructs `new Index(indexProto)` for every index in the proto
  (`RecordMetaDataBuilder.java:183,211`).
- Maintainer selection is a pure map lookup on the type string
  (`IndexMaintainerFactoryRegistryImpl.java:79`). `"count"` resolves to
  `AtomicMutationIndexMaintainer`, registered via `ServiceLoader` and always
  present in core.
- An index name Java has never seen defaults to `READABLE`
  (`RecordStoreState.java:219-221`).
- Grouped `COUNT` is handled generically off the `GroupingKeyExpression`
  (`AtomicMutationIndexMaintainer.java:129-157`).

So the companion is maintained by Java as a plain count index. Four caveats,
each of which Go satisfies:

1. The Java app must open against the **stored** metadata (`FDBMetaDataStore`),
   not a locally compiled `RecordMetaData`. A locally compiled one simply does
   not contain the companion, and the store is then written with an index it does
   not maintain.
2. Version fields must satisfy `0 < addedVersion <= lastModifiedVersion <=
   metaData.version`, or any validating path throws
   (`IndexValidator.java:53-56`, `MetaDataValidator.java:124-133`). Go's
   `addIndexCommon` (`pkg/recordlayer/metadata.go:404-412`) supplies them;
   measured on the fixture: added 3, lastModified 3, metadata version 3.
3. A grouped `COUNT` must carry `groupedCount == 0` or
   `AtomicMutationIndexMaintainerFactory.java:99-104` throws. Go's `GroupAll`
   sets it (`pkg/recordlayer/key_expression.go:1194-1199`).
4. `clearWhenZero` must match on both sides. Neither side sets any option, so it
   is absent/false on both — and the read path does not depend on the maintainer
   clearing zeroes in any case: it drops zero-valued entries at **read** time via
   `liveGroupsOnly`
   (`pkg/recordlayer/query/executor/executor_new_plans.go:353-372`).

The companion's type is `IndexTypeCount = "count"`
(`pkg/recordlayer/aggregate_group_existence.go`, `pkg/recordlayer/index.go:16`).
All four caveats are **executed against a live JVM**, not asserted in this
prose, by the spec `RFC-209 group-existence companion cross-language` in
`conformance/metadata_store_conformance_test.go`: Go persists the metadata and
writes records; **Java** loads that stored metadata, scans the companion
`BY_GROUP` (caveats 1-3), then inserts into an existing group, inserts into a
new group, and deletes a group's last row through the same metadata; Go re-reads
and must agree exactly. The write/delete step is the load-bearing half — a Java
engine that loaded the companion but did not maintain it passes a scan and fails
here.

An earlier draft credited `conformance/index_ddl_metadata_conformance_test.go`
with this. That was wrong: there Java builds its *own* template, so no Java code
ever touches the auto-emitted companion. That test's real job is narrower and
still worth having — it bounds Go's stored index set to Java's plus exactly the
allowlisted companions.

Caveat 4 is a measurement rather than a hope. Neither side sets `clearWhenZero`,
and Java's maintainer decrements a vacated group to `0` and **leaves the key**.
Dropping that zero is the read side's job, which Go does via `liveGroupsOnly`, so
Go's correctness never depends on Java clearing anything. If Java ever begins
clearing zeroed groups, that expectation is where it surfaces.

Caveat 1 says "open through `FDBMetaDataStore`", and that leaves one question the
read path cannot answer: **evolution**. Every read entry point builds with
`validate=false` (`FDBMetaDataStore.java:252,279,366`), but every *mutating* entry
point — `addIndex`, `dropIndex`, `updateRecords`, `mutateMetaData` — funnels into
`saveAndSetCurrent`, which builds the new proto with `validate=true`
(`FDBMetaDataStore.java:376`) **and re-builds the old, Go-written proto with
`validate=true`** before running the evolution validator
(`FDBMetaDataStore.java:394-396`). That old-side rebuild drags the companion
through `RecordMetaDataBuilder.build(true)` → `MetaDataValidator.validate()`
(`RecordMetaDataBuilder.java:1493-1496`) and hence through
`AtomicMutationIndexMaintainerFactory`'s `IndexValidator`. Had the companion
failed there, a Go-written schema would be a poison pill: readable by a Java app
forever, evolvable never.

MEASURED — it does not fail, so there is no caveat 5. The same conformance file
runs `FDBMetaDataStore.addIndex("T", …)` (`FDBMetaDataStore.java:633,670-674`) of
an unrelated `VALUE` index against Go's stored template; Java returns
`{Ok:true Version:4 IndexNames:[I_SUM I_SUM__GROUP_COUNT J_V_IDX]}`, and Go then
re-loads the evolved metadata, still finds `I_SUM__GROUP_COUNT`, and re-scans it
to `{a:2 b:1}`. (The read-side spec was already stronger than caveat 2 needed:
its `RecordMetaData.build(proto)` resolves to `build(true)`
(`RecordMetaData.java:578-580`, `RecordMetaDataBuilder.java:1431-1445`), so
`MetaDataValidator` has always run over the companion there. What the evolution
step adds is the old→new `MetaDataEvolutionValidator` leg and the real mutating
API.)

That measurement also settles an `InvalidExpressionException: index type does not
support non-group fields` observed during an earlier mutation experiment on this
branch. It is not about the companion. The throw is at
`AtomicMutationIndexMaintainerFactory.java:99-104` and is reachable only when an
index's *type* disagrees with its *expression* — a value-less type (`count`) over
an expression that has grouped columns. Both directions are reproduced
deliberately in the spec, by rewriting one index's type in the stored bytes
(neither engine validates on that path) and re-running the same evolution:

- companion (`GroupAll`, zero grouped columns) retyped `sum` → `index type only
  supports integer field`, thrown at
  `AtomicMutationIndexMaintainerFactory.java:131-150`. Not the observed message:
  the per-record-type hook runs before any grouping check
  (`IndexValidator.java:51-52`), and the companion's only column is the `STRING`
  grouping column, so the integer check fires first and `validateGrouping(1)` is
  never reached.
- owner `I_SUM` (one grouped column) retyped `count` → `index type does not
  support non-group fields; use COUNT_NOT_NULL`. This is the observed message,
  and it comes from the corrupted type, never from the companion as Go emits it.

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

The distinction against §4.1 is exact and worth stating, since this design *does*
write metadata: a companion is a **separate index of a type Java already has a
maintainer for**, which Java loads and maintains generically. A pointer property
is an **unknown field on an index Java does share**, which nothing in Java reads
and which changes what that index's stored options must equal.

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
and planning falls back to the base-table plan (5.3(c)). An explicit reference
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
plan or yields nothing; when it yields nothing, what remains is the base-table
plan of 5.3(c). One query path, visible to the cost model.

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
falls back to the base-table plan. That plan must be named for what it is rather
than as "streaming aggregation", which makes it sound cheap. MEASURED, the
planner builds `StreamingAgg(keys=[CUSTOMER_ID], InMemorySort([CUSTOMER_ID ASC],
Scan(ORDERS)))`: a full base-table scan **plus a full materializing in-memory
sort of every row**, because nothing supplies the grouping order. On a large
table — the exact case a mid-backfill companion leaves behind — that sort is
plausibly not plannable in memory at all. This is a correct answer bought at a
price that may not be payable, not a graceful degradation.

### 5.3.1 Cost: the companion scan is priced as a real child

The operator is visible to the cost model (§5.3), which is only worth anything if
it is **priced**. The companion-joined plan is not a cheaper spelling of the
aggregate-index plan — it is a second index scan, and the RFC must say so.

**What it costs.** The companion scan is a `BY_GROUP` scan over the `COUNT(*)`
index, which has one entry per group that has ever existed. So the plan's index
I/O is roughly **twice** the single-aggregate-index plan's, plus the merge. The
cost model must charge the companion scan as a real scan child, not fold it in as
a constant.

**An earlier draft required the planner to flip to the base table as the group
count approaches base-table cardinality. That proposal is struck.** It was
grounded in the reasoning that a near-unique grouping key makes the companion
scan approach one entry per row, so the merge reads about twice the base-table
cardinality to produce about that many rows while the base-table plan reads the
table once. The reasoning is arithmetically fine and the conclusion is wrong.
Four grounds, in order of force.

**(a) It prescribes a plan Java refuses to build.** Java's Cascades has no sort
operator and no hash aggregation: `ImplementStreamingAggregationRule` yields
nothing unless a satisfying order already exists, and `RecordQuerySortPlan` is
constructed only by the legacy `RecordQueryPlanner`, never by Cascades. So
"streaming aggregation over the base table" in the shape the flip wanted is not a
Java plan at all, and the flip would have made the cost model prefer it.

**(b) The rival the planner actually builds is a sort, not a stream.** MEASURED
on the fixture query, the base-table candidate is
`StreamingAgg(keys=[CUSTOMER_ID], InMemorySort([CUSTOMER_ID ASC], Scan(ORDERS)))`
— it sorts the whole table. `RecordQueryInMemorySortPlan` is Go's sanctioned
read-side fallback, not a plan Java would recognise. The flip's premise, "the
base-table plan reads the table once", omits the sort.

**(c) The economics never invert.** MEASURED, rows held constant at 100000, four
group-count regimes, best-of-2, companion-joined merge versus the base-table
plan:

| groups | merge | base table | ratio |
|---|---|---|---|
| = rows (unique) | 465.54 ms | 861.03 ms | 1.85x |
| rows / 1.25 | 367.72 ms | 858.61 ms | 2.33x |
| rows / 10 | 50.91 ms | 776.77 ms | 15.26x |
| 10 | 5.39 ms | 542.98 ms | 100.76x |

A confirmation run agreed: 1.87x / 2.23x / 15.08x / 99.05x. The merge's advantage
decays monotonically toward the unique limit and **never inverts**. The flip
would therefore have chosen the slower plan at every measured point, including
the one it was designed for.

**(d) The narrower version — a scan-order gate — is rejected as a bolted-on
special case.** It would exist to make the base-table candidate available in the
near-unique regime. It already is: MEASURED, inverting the cost comparator below
the physical gate flips the winner to the base-table plan, so that candidate is
in the memo and competing in exactly that regime. Nothing needs to be bolted on
to offer it. It simply loses. Design principle 10 applies — match the
architectural property, do not add an `if` that reproduces a downstream
observable.

**One honest gap in the measurement.** The base-table rival above leaves a sort
on the table: an ordered index scan on `customer_id` exists and could have
supplied the grouping order, which would remove the `InMemorySort`. So the 1.85x
margin at the unique limit is measured against a sort-then-aggregate competitor,
and it could narrow if that sort were eliminated. That is an **open measurement
question**, recorded so nobody re-derives it — it is not a reason to keep the
flip, because a flip is unwarranted until a measurement shows an inversion and
none does.

**The 1e6 default cardinality is not a dependency of this design.**
`TODO.md:13800-13820` records, MEASURED, that RFC-204 P1 left `fetchTableStatistics`
(`cascades_generator.go`) dead on every SQL-created schema, so the cost model
plans on its 1e6 default. With the flip struck, nothing here needs the per-table
row count: §5.3.2 shows the merge-vs-base choice is structural and does not move
when the statistic sweeps nine orders of magnitude. The item is still worth
closing; this RFC does not wait on it.

There is a useful symmetry worth recording. The remedy that TODO item names
first is "(a) read per-type row counts from COUNT-type aggregate indexes when the
schema declares them (`IndexTypes.COUNT`, grouped by record type)" — and this
RFC's create-if-absent companion puts a `COUNT` index on precisely those schemas.
RFC-209 therefore supplies the input that item needs, rather than competing with
it.

**Consequence to state plainly:** the plan this design gives up is not the
base-table plan, it is the pre-RFC aggregate-index-only plan — one scan instead
of two merged scans. That plan is faster and *wrong*: it reports phantom groups.
So the performance question this RFC must answer is not "does the merge beat the
base table" (measured: yes, everywhere) but "how much does correctness cost
against the incorrect plan it replaces". §7 sets that as a bounded multiple.

**Fail-closed by construction.** 5.3(c) is not a check that can be forgotten:
the resolved, readable companion is a **constructor precondition** of the
companion-joined plan, so that plan is unconstructible without one. There is no
code path that builds the fast plan and then validates it.

### 5.3.2 What actually decides the merge, and it is not the magnitude cost

§5.3.1 reads as though the scalar cost estimate governs the choice. It does not,
and the RFC would mislead an implementer by implying it. MEASURED, by
rung-by-rung instrumentation of `planningCostModelCompareWith` on the fixture
pair:

- The two candidates **tie** through the data-access rung. The whole-plan
  max-cardinality gate abstains — neither side has a proven bound. Residuals are
  0 / 0. Data access is 1 / 1.
- Then **three independent rungs each pick the merge unaided**:
  `comparePrimaryScanVsIndexScan` (covering index over primary scan);
  `inMemorySortCount` (0 versus 1); and last the scalar `EstimateCostWith`
  fallback, routing through `RecordQueryMultiIntersectionOnValuesPlan.HintCost`'s
  driving-leg branch.
- Neutralizing any **one** of the three changes nothing. The winner flips only
  when all three go at once.

Three consequences, each worth stating because each is a trap:

1. **The choice is structural, not economic.** It does not move when table
   statistics sweep nine orders of magnitude (1 to 1e9). This is why §5.3.1 can
   say the dead `fetchTableStatistics` is not a dependency.
2. **Counting the merge's legs honestly changes the outcome.** The merge reads
   two streams; the data-access rung counts it as 1. Counting 2 makes that rung
   fire *ahead* of all three deciding rungs and bars the merge at every
   cardinality. Whoever corrects that count is changing this design's plan
   choice, not tidying an estimate.
3. **The driving-leg `HintCost` branch is live, and inert only as a decider.** It
   executes during ordinary planning; it is merely never the rung that settles
   this pair. Deleting it as dead code is a mistake — and note that deleting it
   is *not* how one neutralizes rung 3 to reproduce the finding above. Without
   that branch `HintCost` falls back to `IntersectionCost`, whose min-of-legs
   cardinality understates the merge and whose CPU stops charging the companion
   leg, so rung 3 then prefers the merge *more* strongly. Reproducing this
   section means bypassing the comparison, not removing the formula; a literal
   delete-the-branch reproduction gets the wrong answer.

Both facts are pinned:
`TestGroupExistenceMerge_DecisionIsStructuralNotEconomic` and
`TestMultiIntersectionHintCost_DrivingBranchIsLive`.

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
take 5.3(c): correct answers at the price 5.3(c) names — a base-table scan plus a
full materializing sort, which on a large table may not be plannable at all. This
is the main cost of the design, and it is the price of not forcing an on-disk
migration, *not* the price of not touching the wire: §4.1 records that the
companion is written. A Go-created schema read by Java plans as Java does today
(Java ignores the companion while still maintaining it, §4.1), i.e. Java keeps
its own wrong answer; we do not fix Java from here.

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
(`TestFDB_AggregateIndexVacatedGroup_SumAndCountColOracle`) assert today's wrong
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

**Cost (§5.3.1) — a bounded multiple of the incorrect plan.** An earlier draft
required streaming aggregation to win on a near-unique grouping key and failed
the RFC if the planner picked the companion-joined merge there. That criterion is
refuted by §5.3.1(c): the merge is faster than the base-table plan at every
measured regime, so the old criterion demanded the slower plan.

The replacement measures against the right baseline. The merge replaces the
pre-RFC aggregate-index-only plan — one scan, faster, and **wrong**, because it
reports phantom groups.

**That plan is unconstructible by design, so the criterion is timed against a
stand-in.** Once a SUM owner exists the companion is emitted create-if-absent
(§5.2) and the planner always builds the merge; there is no switch that produces
the one-scan-and-wrong plan and therefore nothing to start a stopwatch on. A
criterion phrased against it would be a number nobody could ever obtain.

The stand-in is a **grouped `COUNT(*)` at matched group cardinality**. By §5.3(a)
a grouped `COUNT(*)` is its own group-existence oracle, so it takes no companion
and plans as a bare single `BY_GROUP` aggregate-index scan — the exact physical
shape the pre-RFC plan had: one scan of one entry per group, one aggregate value
per group, an ordering the scan already satisfies, a `HAVING` over the aggregate.
It is constructible, so it is timed. The stress test asserts that shape rather
than assuming it: the reference `EXPLAIN` must contain exactly one
`AggregateIndex` scan and must not contain `GroupExistenceMerge`. If companion
discovery ever attaches a companion to a grouped `COUNT(*)`, the reference stops
being the pre-RFC shape and the test fails rather than quietly measuring
something else.

MEASURED, rows held at 100000, same store, back-to-back, best-of-2, the same four
regimes as §5.3.1(c):

| regime | groups | merge | reference (single scan) | merge / reference |
|---|---|---|---|---|
| unique | 100000 | 457.41 ms | 194.13 ms | 2.36x |
| near-unique | 80000 | 378.10 ms | 131.21 ms | 2.88x |
| moderate | 10000 | 53.84 ms | 21.77 ms | 2.47x |
| low | 10 | 5.33 ms | 5.28 ms | 1.01x |

**The criterion is that the merge stays within 3.5x of the single-scan reference
at every regime whose reference actually measures scan work.**

Single-pair ratios proved too unstable to set a bound from: successive runs put
the worst regime (near-unique) at 2.86x, 3.08x, then 3.24x with no code change
between them. Each ratio is therefore the MEDIAN of 5 back-to-back
merge/reference pairs — pairing puts any slow moment on both sides so it divides
out of that pair's ratio, and the median discards the pair that got unlucky
anyway. Median-of-5 over three consecutive runs:

| regime | median-of-5 ratios | headroom to 3.5x |
| --- | --- | --- |
| unique | 2.46 / 2.52 / 2.49 | ~28-30% |
| near-unique | 3.08 / 2.86 / 2.98 | ~12-18% |
| moderate | 2.08 / 2.08 / 1.97 | ~41-44% |
| low | 1.05 / 1.08 / 1.07 | excluded, see below |

The median is load-bearing, not cosmetic: in the first of those runs an
individual near-unique pair measured 3.63x — above the bound. A single-pair
criterion would have failed a clean run.

Worst observed median headroom is about 12%, against a median that still drifts
~7% run to run. That is tight on purpose, and the consequence is stated rather
than hidden: an occasional flake is possible on a loaded or nearly-full disk. The
remedy is more pairs (9 or 11, costing seconds), never a looser bound —
loosening would buy back precisely the regression headroom the number exists to
deny. The earlier 3x is superseded twice over: it was not chosen from data, and
it sits below medians already observed at 3.08x.

The bound is applied only where the reference measures scan work. Below 1000
groups the reference reads too few index entries for its runtime to be anything
but fixed per-query cost, so the ratio would measure harness overhead. That
exclusion is load-bearing rather than convenient, and it is probed: driving the
bound down to 0.5x reds every regime above the threshold and still leaves `low`
passing, which shows the skip is doing the work rather than `low` happening to
sit under whatever number is written here.

Two readings the table demands, both about output cardinality rather than
scanning. The reference's `HAVING` cannot be tuned to the merge's selectivity:
the fixture's grouping key is an exact partition, so group sizes take at most two
values and `COUNT(*) > T` is all-or-nothing in three of the four regimes. The
test picks the closest-to-50% non-degenerate threshold where one exists and falls
back to selecting every group where none does, logging which (`refUniform`). So
the reference returned *twice* the merge's rows at unique, moderate, and low —
deflating those ratios — and *half* at near-unique, inflating it. The worst point
is therefore the inflated one, which is the conservative direction for a bound.
And at `low` the ratio collapses to 1.01x: at ten groups both plans are fixed
cost, and the second scan is free.

**What re-opens this criterion:** any regime exceeding 3.5x, or the reference
ceasing to plan as a single bare aggregate-index scan — the second is a change to
§5.3(a), not to cost, and the test fails on it directly.

**What re-opens the struck flip:** the merge becoming *slower* than the
base-table alternative at any regime. That is the inversion §5.3.1(c) measured
and did not find; if it ever appears, the flip becomes worth building.

The stress test no longer asserts "merge wins". It records all four regimes,
logging `GEMERGE_REGIME` for merge-versus-base-table and `GEMERGE_REFERENCE` for
merge-versus-single-scan and `GEMERGE_RATIO` for the median statistic and its
spread, and checks all three plans' row counts against ground truth computed in
Go. The bound above **is asserted automatically**: the median ratio is compared
against 3.5x in every regime at or above 1000 groups, and exceeding it fails the
run. Which plan is *faster* remains unasserted — that is the question the axis
exists to answer — but the cost criterion itself is enforced, not read off by a
reviewer. Runs must live on the same
filesystem with headroom, per the repo's stress-comparison rule; a run taken
above ~95% disk utilisation measures the disk, not the plan. The run recorded
above was taken at 97%, which is why the bound is read from the *ratio* of two
plans measured back-to-back against one store — a uniformly slow disk largely
divides out — and why the absolute milliseconds above should not be compared
against §5.3.1(c)'s earlier run.

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
- **`FlatMap(companion, DefaultOnEmpty(NULL, aggregate probe))`** — the
  Java-precedented shape, and the one alternative here that Java actually builds:
  `RewriteOuterJoinRule.java:45-112` produces it and
  `RecordQueryDefaultOnEmptyPlan.java:112-120` executes it. It gives the same
  outer semantics §5.3(b) needs — drive from the companion, substitute the
  identity when the aggregate side is empty. It loses on shape: it is **N
  correlated probes**, one aggregate-index probe per companion group, against
  **one ordered merge** of two streams already co-grouped on the same key. Java
  tolerates the correlated form only because it has no merge operator with which
  to express the alternative — Java's outer join *is* the correlated `FlatMap`
  re-scan. Go has a materialized merge, so it is not forced into the re-scan. This
  is the same asymmetry recorded elsewhere in the port, where
  `RecordQueryNestedLoopJoinPlan` is a read-side extension Java lacks.
- **Packing a count into the aggregate's value.** FDB's `ADD` carries across the
  whole value width, so two counters cannot share one atomic add without
  contaminating each other, and the value is wire-visible to Java regardless.
- **A read-modify-write SUM maintainer** that could delete its own key. Every
  write to a group would conflict with every other — the exact contention the
  atomic-mutation index exists to avoid.
- **Declining the aggregate index for all grouped queries.** Correct and
  trivial, but it deletes the feature.
