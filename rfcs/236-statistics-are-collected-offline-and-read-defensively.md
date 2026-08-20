# RFC-236 — Statistics are COLLECTED offline and read DEFENSIVELY

**Status:** proposed
**Supersedes:** the first draft of this RFC (an `IndexMaintainer` capability), NAK'd by both gates
**Implements:** CQ-88 phase 1
**Depends on:** RFC-235 §§17-18

## 1. Decision

Planner statistics are **collected by an offline job** — the same shape as
`RebalanceSPFreshIndex` and `OnlineIndexer`, which this library already ships —
and **read defensively at plan time** behind a per-connection opt-in.

This is not a server. It is the library's existing maintainer pattern: the
library provides the machinery, the operator decides when to run it, and stores
that never run it behave exactly as they do today.

Four properties carry the design:

1. **Collected, not derived.** The collector may SCAN, so it is not bounded by
   what FDB can answer in a single sampled RPC. §2 measured that ceiling and it
   is the reason the previous two designs failed.
2. **Estimates, never bounds.** Statistics feed cost and nothing else. A stale
   statistic costs a suboptimal plan, never a wrong row. This is what makes
   best-effort acceptable, and it has exactly one boundary (§6).
3. **Outside the Java contract.** Statistics live in their own keyspace, not in
   the record store's, so no byte Java reads or writes is affected.
4. **All-or-nothing per query.** A partial answer is worse than none (§5).

## 2. Why the two previous designs failed, measured

Both are committed probes, not recollections.

**Sampled range size has a floor, and the floor is on the wrong side.**
`estimated_range_size_probe_test.go` over a VALUE index:

    range              entries   est_bytes   TRUE_bytes   est/true
    whole index           4000      404494       409743      0.987x
    price=0 (~50%)        2000      201498       203743      0.989x
    price=1 (~25%)        1000      101498       103000      0.985x
    price=2 (~10%)         400           0        41200      0.000x
    price=3 (small)        599      101498        61697      1.645x
    price=4 (1 row)          1           0          103      0.000x

Accurate to ~1.5% above roughly 100KB. Below it: **0 for a 41KB range**, and
quantization — 599 entries reporting the same value as 1000, 64% over. This is
FDB behaviour, not a Go defect: the repo's own C-port test, from
`unit_tests.cpp:2500`, records that a freshly written range may estimate 0
because the storage server has not compacted it.

**And the floor makes per-type sizing useless for the decision it was for.**
`per_type_size_estimate_probe_test.go`:

    type        rows   est_bytes
    Order       3000      322250
    Customer     150           0

The small table is invisible. A join between a large and a small table is
exactly where a size difference decides the plan, and the small side is the one
that disappears. A hybrid cannot rescue it: "estimated 0" and "genuinely tiny"
are indistinguishable from the return value, and substituting the 1e6 default
for a refusal makes the small table look the LARGER — strictly worse than
constants, which at least tie.

**Conclusion.** No plan-time primitive answers this. A collector that scans does,
and scanning is affordable precisely because it is offline.

## 3. What the tie actually needs

RFC-235 §18 measured Go and Java BOTH resolving cross-product nesting by a hash,
disagreeing in 14 of 16 spellings. §18's third conclusion — "cardinality drives
nothing in either engine" — was measured against an unfed model and is not
evidence that cardinality cannot discriminate.

The discriminating term is not the one it looks like. `FlatMapCost`
(`cost_formulas.go:101-112`):

    Cardinality: outerCard * innerCard                                  SYMMETRIC
    CPU:         (outer.CPU + outerCard*(innerCPU+IterationOverhead)) * 0.9

Multiplication is commutative, so the cardinality term is identical for both
nestings at every fidelity and can never break the tie. The CPU term is
asymmetric: `outerCard * innerCPU` charges one inner re-execution per outer row,
so it prefers the smaller outer. With every leaf at the same constant, both
nestings evaluate identically and the comparison falls through to the tie-break.

Real counts do not add a tie-break; they let the CPU term do its job. Equal
cardinalities tie again, correctly — there is then nothing to choose.

## 4. Shape

### 4a. The collector

Library API, mirroring `RebalanceSPFreshIndex`'s signature so it composes with
the maintainers already here:

```go
// CollectStatistics scans the store and writes per-record-type statistics.
// Offline: it is a maintenance job, not a query path.
func CollectStatistics(
    ctx context.Context,
    db *FDBDatabase,
    storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error),
    statsSubspace subspace.Subspace,
    opts CollectOptions,
) (*StatisticsReport, error)
```

Per record type it records an EXACT count, obtained by scanning. Exactness is
affordable here and removes every problem §2 found: no floor, no quantization,
no bytes-to-rows conversion, no unit mixing.

Scanning is **continuation-driven and batched** — the store's own cursor
machinery, the same way `OnlineIndexer` walks a store — so a large table costs
many small transactions rather than one that cannot commit. `CollectOptions`
carries the batch size and an optional per-type row cap; a type that hits the
cap is recorded as **absent**, never as a partial count.

### 4b. Where it is stored

**Not inside the record store's subspace.** That namespace is Java's:
`FDBRecordStoreKeyspace` defines 0-10 and is `@API(UNSTABLE)`. It has already
grown — `INDEX_SLIDING_WINDOW_SPACE(10)` postdates this port's original 0-9
transcription, which is how the gap was found. Taking a prefix there is not a
question of whether Java reads it; it is a collision waiting for an upstream
release to claim the number.

**Not derived from the relational keyspace either.** An earlier draft put
statistics under `RelationalKeyspace`, which covers only SQL-created schemas. A
record store is constructed with an ARBITRARY `subspace.Subspace` — hand-built
stores, Java-authored layouts, tenant-scoped stores — so anything keyed off the
relational path silently excludes them.

**A store IS its subspace prefix**, whatever produced it. So statistics are keyed
by exactly that:

    <statsRoot> / <store subspace prefix bytes> / <recordTypeKey>
        -> (count, collectedAtVersion, collectedAtUnixNanos, rowsScanned)

plus one header per store carrying the collection's own version and time. This is
layout-agnostic by construction and provably zero bytes inside any record store:
Java is unaffected today and stays unaffected when it claims prefix 11.

The cost of being outside is that dropping a store does not drop its statistics.
That is handled by §5's freshness rule rather than by cleanup alone — an orphan
is old, and old entries are refused — plus a `clear` verb on the CLI. An orphan
cannot silently mislead: a recreated store would have to match both the prefix
and the freshness window.

**Tenants.** FDB tenants are separate keyspaces, so a store's subspace prefix is
only meaningful WITHIN its tenant: two tenants may hold byte-identical prefixes
for entirely different stores. The stats root therefore lives INSIDE the tenant,
never above it. Getting this backwards is the one genuinely dangerous mistake
available in this design — tenant A's statistics would describe tenant B's
tables, and the symptom would be a bad plan rather than an error.

The reader needs no tenant logic at all: it opens the store through whatever
tenant its connection already uses, so it lands in the correct stats root by
construction. Only the collector chooses (§4d).

### 4c. The reader

`statistics_reader.go` in `core/embedded`. Two layers, deliberately split:

- `evaluateCollectedStatistics` does the I/O — the snapshot read, the current
  version — and gathers it into a `statisticsGateInput`.
- `decideStatistics` is PURE: facts in, verdict out.

The split exists so every refusal arm can be driven by a table-driven test.
Two of them require a cluster MISBEHAVING — a version read that fails, an entry
stamped ahead of the cluster after a restore from backup — and an arm no test
drives fires for the first time in front of an operator, where it reads as a
finding rather than as an untested branch. `TestDecideStatistics` drives all
eight; `TestDecideStatisticsCoversEveryRefusal` fails the build if a ninth is
added without a case.

The verdict is a `StatisticsStatus`, not a bool, because it has a second
consumer: `frl stats show` prints it. One decision function, two callers. Had
the CLI re-derived "is this usable", it would eventually have reported usable
for statistics the planner had already refused — the most expensive kind of
wrong, because it reads as a confirmation.

The reader opens no record store: the schema subspace IS the store subspace, so
it is independent of `fetchIndexStateSnapshot`, including its early return when
a schema has no indexes. That case matters — a join between two index-less
tables is exactly where cardinality alone decides the order.

### 4d. The CLI

    frl stats collect --database /db --schema NAME [--batch-size N] [--max-records-per-type N]
    frl stats collect --database /db --all-schemas [--concurrency N]
    frl stats show    --database /db --schema NAME [-o text|json]
    frl stats clear   --database /db --schema NAME [--yes]

**No tenant flags.** The earlier draft had `--tenant` / `--all-tenants` over
`fdb.Database.ListTenants()`. That was written without checking what "tenant"
means in this codebase.

`grep -rnE 'ListTenants|OpenTenant|TenantName|fdb\.Tenant' --include='*.go'
pkg/relational/` returns 0; the same command over `pkg/fdbgo/` returns 90, which
is the control that says the sweep is well formed. Meanwhile a case-insensitive
grep for "tenant" under `pkg/relational/` returns 80 non-test hits — and that is
exactly the finding, not a contradiction of it: the multi-tenancy that exists
there is SaaS tenancy expressed as SCHEMAS, and it already has a fan-out mechanism — `pkg/relational/core/fleet`
— with per-target transactions, per-target failure isolation, bounded
concurrency and a resumable pass, driving `index build --all-schemas` and the
migration fan-out today.

So `--all-schemas` is not a new mechanism. `fleet.CollectStatistics` is a
`step`, exactly like an index build, and it shares `fleet.ListTargets` with the
other two modes rather than enumerating schemas its own way: a statistics pass
covering a different set of schemas than the migration pass would leave exactly
the tenants nobody thought about uncollected, and one uncollected type disables
statistics for that whole schema.

**Single-schema commands route through the SQL driver connection.** Not through
`withStore`. The statistics location is derived from the keyspace root and the
schema's store subspace, and the planner derives it one way, inside
`EmbeddedConnection`. A CLI deriving it a second way is two pieces of code
hoping they agree, and the failure is silent: the collector writes where the
planner never looks, every command reports success, and plans just never change.

The fan-out is the one path that CANNOT use a connection — it has no single
schema to bind one to — so it is a genuine second derivation, and it is pinned
by `TestIntegration_Stats_FleetCollectIsReadableByTheConnection`: the fan-out
writes, the connection reads, and a disagreement about the database-path
convention or schema case folding shows up there and nowhere else.

### 4e. A declared type with no rows is an exact ZERO, not an absence

The collector seeds every type the metadata declares at 0 before tallying, so a
table that exists and is empty gets an entry saying so.

This is not a detail. Counting only what the scan OBSERVES leaves an empty table
absent, and §5's completeness gate then refuses the whole schema — permanently,
until somebody inserts a row. A freshly created schema is mostly empty tables,
so the feature would be off exactly where it had just been switched on. The
first implementation did this, and a test codified it with a rationale that was
itself wrong: "zero would tell the cost model the table is empty, the most
selective claim available". It cannot. `NewCollectedStatistics` clamps a count
below 1 up to 1, so a stored 0 reaches the cost model as a one-row table. The
danger was already neutralised at the read side and the write side was paying
schema-wide lockout for it.

An exact 0 from a full scan is as trustworthy as an exact 5. ABSENT stays
reserved for NOT COUNTED — a type over `--max-records-per-type` — which is a
different fact and still refuses the schema, as intended.

### 4f. The collector is retry-safe, and that is not free

`db.Run` RETRIES its closure. A batch that trips `transaction_too_old` after
tallying most of its rows re-runs from the same continuation and re-reads them,
so tallying into durable counters double-counts.

The direction is the worst available: retries are likeliest on the LONGEST
batches, so the inflation lands preferentially on the biggest tables — the ones a
join-order decision is most sensitive to — and it is silent, because an inflated
count is a well-formed number every gate passes through.

Each attempt therefore accumulates into its own map, reset at the top of the
closure, merged only after `Run` returns. Nothing inside the retried closure
mutates a durable accumulator, so the invariant is checkable by reading rather
than by an argument about idempotence.

## 5. Reading defensively

A statistic is used only if ALL of these hold. Any failure means the whole query
plans on constants — absent, expired, incomplete and failed are ONE outcome:

0. **The flag is on.** `PLANNER_STATISTICS`, default false, reachable as
   `?planner_statistics=true` in the DSN, and part of the plan-cache key in
   BOTH halves. The all-defaults early return is the half that is easy to miss:
   with only a render arm, a flag-ON connection whose other options are default
   renders `""`, byte-identical to flag-off, and shares its cache entry.
1. **The read succeeds.** Any error → constants. Planning acquires no new
   failure mode.
2. **An entry exists.** Absent is not zero.
3. **It is fresh, judged on FDB VERSIONS.** `age = readVersion -
   collectedAtVersion`, bound `24 * 60 * 60 * 1e6` (~24h at FDB's ~1e6
   versions/second). Versions rather than wall clock because they are the
   CLUSTER's own clock: monotone within it, and immune to skew between the host
   that ran the collector and the host planning the query. A wall-clock
   comparison across two machines can make an entry effectively immortal, which
   would quietly defeat this gate — and with it the argument that an orphaned
   entry is harmless because it is old. A NEGATIVE age refuses rather than
   reading as infinitely fresh: a restore from backup moves versions backwards.
4. **Complete over the WHOLE SCHEMA.** Not the query.

Completeness is schema-wide on two counts. It is undecidable query-wide where
the read happens — before the planner exists, so which types a query touches is
unknown. And it would be insufficient anyway: `FullUnorderedScanExpression` SUMS
per-type cardinalities, so one absent type inside one scan node yields
`1e6 + realCount`, an inversion BELOW the granularity a per-query gate is even
defined at. The cost, stated rather than discovered: one uncollected type
disables statistics for every query in that schema.

Why refusal must be all-or-nothing: a refusal returns
`LeafScanCardinality = 1e6`, larger than almost any real count, so one missing
type standing beside a real 150-row count makes the missing table the largest in
the schema and drives the join from the wrong side. Half a statistic is worse
than none, which at least ties.

The provider is therefore NOT `MapStatistics`. Several production sites ask for
the EMPTY record type name when a leaf's types are unknown, and `MapStatistics`
answers an unknown name with `LeafScanCardinality` — wrong in the inverting
direction. `CollectedStatistics` answers the empty name with the whole-store
SUM, which is on the same scale as the data.

### 5a. Metadata this port does not fully model is refused outright

When a schema declares JOINED or UNNESTED synthetic record types, `RecordTypes()`
deliberately omits them — the port carries their declarations opaquely rather
than modelling them. So the record-type set is a PARTIAL view, and completeness
is undecidable against a partial view: certifying a schema complete after
checking a subset is the gate producing the very inversion it exists to prevent.

`DeclaresSyntheticRecordTypes`' own doc already fixed the convention this obeys —
a caller computing over "all record types" for a coverage decision or a count
must refuse rather than answer from the partial set. A completeness check is both.

The refusal is enforced at every boundary, not only at the gate, and that
distinction cost two review rounds to get right:

- **Before any I/O.** The verdict is fixed by a property of the metadata, so
  reading statistics cannot change it. Reading anyway spends an FDB transaction
  per opt-in plan-cache miss and discards the answer.
- **Before collection.** Both the single-schema and fleet paths refuse up front.
  Collecting would read every record in the store to produce a set the planner
  has already decided to reject — per tenant, across a fleet.
- **Symmetrically.** The fleet reports REFUSED, not no-work. Collapsing them made
  a million-row tenant print "nothing to build" and exit 0 while `--schema` on
  that same tenant exited non-zero — one outcome meaning two things depending on
  which flag was used.
- **Naming the types.** "Metadata declares unmodeled synthetic types" costs a
  schema all its statistics without saying which declaration did it, in text and
  in JSON.

## 6. The one line that must not be crossed

Collected statistics are **estimates**, and must never become **bounds**.

This is what makes a possibly-hours-old number safe to plan with: a stale count
can cost a plan badly, and can never return a wrong row. The reason is
structural, not careful — the ESTIMATE side (`Cost`) consumes a
`StatisticsProvider`; the PROOF side (`Cardinalities`, `provenCardinalities`,
`CardinalityProver`) does not. A rule that drops a DISTINCT because "max is 1"
is reasoning from plan shape and cannot be misled by a number.

Two things follow, and both are now pinned rather than asserted:

- `TestCardinalityProofTakesNoStatistics` reflects over every proof producer and
  fails if one gains a route to a statistics provider. Adding such a parameter
  compiles and passes every other test, which is exactly why a signature test is
  the right instrument.
- `TestProofInformsEstimateNotTheReverse` guards the one place the two sides
  MEET. `BoundedCostHinter.HintCostWithin` and `CostWithinBounds` take the
  proven bounds AND the statistics provider — legitimately, because they return
  a `Cost`. A proof may inform an estimate; the reverse must not exist. The
  check is not "does it take stats" but "what does it hand back".

`fkChainCardinalityCap` is the one place a statistic becomes something the code
calls a bound, and it is worth naming precisely because it looks like a
counter-example. Its BINDING argument is structural and absolute; only the
MAGNITUDE comes from `RecordTypeCardinality`. A table that has grown makes that
number an under-estimate — the unsound direction — and it is safe only because
the value reaches `properties.Cost` and stops. Its doc comment says so, and
points at the tests above.

## 7. Scope

**In:** the collector and its CLI, single-schema and fleet; the keyspace; the
tuple format; the reader with all its gates; the connection flag and its
cache-key component; the tests in §8.

**Out, and named so they are not assumed:** histograms, NDV, MCV and any
distribution (the collector could compute them — it scans — but selectivity
consumes them and that is a second change); automatic/triggered collection;
incremental recollection; per-index statistics; the `Cardinalities` clamp (§6);
plan-cache invalidation on data drift. Collection does NOT invalidate cached
plans, so a freshly collected statistic reaches only queries planned afterwards.

## 8. Tests

- **Collector correctness.** Counts equal a ground-truth scan, across several
  types, empty types, and after deletes.
- **Batch invariance.** Batch 7 and batch 100000 must agree — the continuation
  path is exercised, not assumed.
- **Cap.** Crossing the row cap ABORTS the run and stores nothing, and the
  scan stops — asserted on RecordsScanned, because the report looks identical
  whether the cap bounds work or only suppresses output.
- **Replace, not merge.** Deleting a whole type and re-collecting removes its
  entry rather than leaving the old count behind.
- **Reader gates, every arm driven** (`TestDecideStatistics`), plus a vacuity
  guard that fails when a refusal constant has no case.
- **The estimate/proof line** (§6), two signature tests.
- **Directional acceptance** (`TestFDB_CollectedStatisticsDriveJoinOrder`): the
  same schema and SQL over MIRRORED arrangements. Flag off, the two plans must
  be IDENTICAL — the data cannot reach the decision. Flag on, they must DIFFER,
  each driving from whichever table is smaller in ITS arrangement. A fixed
  tie-break cannot produce a driver that follows row counts across a mirrored
  pair, which is what makes this unsatisfiable by writing a test to satisfy it.
- **Measured improvement at scale** (`TestFDB_Stress_StatisticsJoinOrder`), §11.
- **Cross-derivation agreement.** The fleet fan-out writes; the connection
  reads. §4d.
- **CLI end-to-end.** Collect / show / clear, the JSON shapes, the confirmation
  gate, and that a refused `clear` clears nothing.
- **Flag off is inert.** The full suite plans identically with the flag absent.
- **No corpus entry sets the flag.** The cross-engine corpus is the parity net
  and must not become row-count sensitive.

## 9. Acceptance

1. The plan follows the DATA across a mirrored pair, and does not without the
   flag.
2. The plan statistics choose is measurably faster at scale, with a control.
3. Every reader gate is driven by a test, including the ones a healthy corpus
   never reaches.
4. A store with no collected statistics plans exactly as today.
5. `frl stats` collects a real schema and the counts match a scan, and what the
   fan-out writes is what the planner reads.

## 10. Alternatives rejected

**An `IndexMaintainer` capability** (this RFC's first draft). Both gates NAK'd
it: the planner asks per-RECORD-TYPE questions while an index answers
per-INDEX, and the mapping is not a function — an index may span types, a FanOut
index emits many entries per record, and a type may have no index. The records
subspace, where the question actually lives, is written by no maintainer at all.

**Sampled range size at plan time.** §2. Blind exactly where it matters.

**COUNT indexes as the only source.** Exact and floorless, but it requires every
costed table to declare an index and pay an atomic mutation per write. Kept as a
possible future refinement — `EvaluateAggregateFunction` already routes it — but
not required, because an offline collector needs no schema change at all.

**Maintaining counts on the write path (the RFC-204 shape).** Rejected on wire
compat: a Go-maintained count a Java writer does not update goes silently stale.
That is the failure this design avoids by being explicitly stale-aware instead of
falsely current.

## 11. Measured

`TestFDB_Stress_StatisticsJoinOrder`, on a single-node FDB testcontainer, at
`b66cdec` of the test file. Two MIRRORED arrangements of the same schema and the
same SQL — `SELECT a.v, b.id FROM a, b WHERE b.a_id = a.id`, with `a.id` a
primary key and an index on `b.a_id`, so either table can drive and only its row
count says which should. Statistics are collected once per arrangement; the two
settings then read the same stored bytes, so the OFF numbers are also the
evidence that the opt-in gate holds.

| arrangement | rows | OFF | ON | ratio | plan |
|---|---|---|---|---|---|
| `a_small` (a=50, b=1,000,000) | 1,000,000 out | 1m38.5s | 1m19.9s | 1.23x | unchanged — **control** |
| `a_big` (a=1,000,000, b=50) | 50 out | **2m34.4s** | **22ms** | **6928x** | changed — **win** |

The comparison is OFF-vs-ON WITHIN an arrangement, never across the pair: the
two return different row counts, so a max taken across them compares different
queries. (The first version of this test did exactly that and reported a 12.7x
"control regression" that was two different result sets being differenced.)

**The win.** With no statistics the planner drove from `A` in both
arrangements — the tie-break is fixed, so it cannot be right twice. In `a_big`
that means scanning a million rows and probing a 50-row table a million times.
With statistics it drives from the 50-row `B` and probes `A`'s primary key
fifty times: 2m34s becomes 22ms.

**The control, and its honest caveat.** In `a_small` the tie-break already chose
well, both settings plan the same join, and the times should match. They differ
by 1.23x. ON ran second, so a warmer cache is the likely cause, and the
measurement is not clean enough to call that zero. It is worth stating plainly
rather than rounding away — and it is also five thousand times smaller than the
win it sits beside, so it does not put the result in question. The test's
control window is 0.5x-2.0x for exactly this reason.

**What this does NOT show.** A workload whose joins the tie-break already
happens to order correctly gains nothing — that is the `a_small` row. The value
is not a uniform speedup; it is the removal of a failure mode whose cost is
unbounded and whose occurrence is arbitrary.

## 12. Prior art in this codebase, and why it does not answer

Java has `SizeStatisticsCollectorCursor` / `SizeStatisticsGroupingCursor` /
`SizeStatisticsResults`, and reaching for them is the obvious first move.

They count KEYS. `SizeStatisticsResults.getKeyCount()` is documented as "the
total number of keys in the requested key range"
(`SizeStatisticsResults.java:182-185`), and a key is not a record: the record
layer splits a record over 100KB into chunks at suffixes 1, 2, 3…, and stores
the record version inline. One logical record can occupy several keys, and the
ratio varies per row with the row's size — so a key count is not even a
consistent over-estimate.

For "how big is this table" in bytes, that is the right instrument. For "how
many rows", it silently answers a different question, and the error is
proportional to how many large records a table holds — which correlates with
exactly the tables an operator cares about. The collector iterates the record
cursor instead, and `counts split records once, not once per chunk`
(`statistics_test.go`) is the pin: it stores three records across more than
three keys and requires the answer to be three, with a guard that fails the test
if splitting ever stops happening and the two counts coincide.
