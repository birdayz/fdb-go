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

`fetchTableStatistics` (`cascades_generator.go:2482`) already does the right
thing structurally — snapshot read, best-effort, nil on any error, returns
`MapStatistics`, called after the plan-cache lookup and before
`newCascadesPlanner`. It gains a second source and a gate; it does not move.

### 4d. The CLI

    frl stats collect [--tenant NAME | --all-tenants] [--store SUBSPACE] ...
    frl stats show    [--tenant NAME | --all-tenants]
    frl stats clear   [--tenant NAME | --all-tenants]

- `--tenant NAME` — one tenant, stats root inside that tenant's keyspace.
- `--all-tenants` — enumerate via `fdb.Database.ListTenants()` and iterate; each
  tenant is self-contained, so a failure in one does not abort the rest.
- neither — the default keyspace, for non-tenant deployments.

Collection is per store and independently restartable. There is no global lock
and no cross-store transaction: a store is the unit of work, and a partially
collected run leaves earlier stores valid and later ones absent — which §5 reads
as "absent", not as "stale".


## 5. Reading defensively

A statistic is used only if ALL of these hold. Any failure means the whole query
plans on constants:

1. **The flag is on.** `PLANNER_STATISTICS`, default false, and part of the
   plan-cache key — `planner_options.go`'s `cacheKeyPart` documents that omitting
   an option from the key is "a wrong-plan bug, not merely a stale-cost one".
2. **Every participating record type has an entry.** Partial statistics are the
   inversion bug: a refusal returns `LeafScanCardinality` = 1e6, the largest
   value in the space, so an estimated table beside a refused one is ranked
   backwards. All-or-nothing is what makes "degrades to today's plan" true.
3. **The entry is fresh enough.** `collectedAtUnixNanos` within a configured
   `maxAge` (default: 24h). Freshness is BOUNDED, not proven — no cheap
   primitive answers "has this table changed since version V" — and §6 is why
   bounding is sufficient.
4. **The read succeeds.** Any error → nil → constants. Planning acquires no new
   failure mode.

Absent, expired, partial, and failed are one outcome: today's behaviour.

## 6. The one line that must not be crossed

Collected statistics are **estimates**, and must never become **bounds**.

Go has a `Cardinalities` / `CostWithinBounds` clamp
(`planning_cost_model.go:1594-1614`) where a PROVEN cardinality constrains
everything above it. A count read at version V is proven at V and nowhere else,
so routing collected counts into the clamp converts staleness from "suboptimal
plan" into "wrong plan". They go to `StatisticsProvider` and stop there.

This is the property that makes a best-effort, operator-scheduled, possibly-hours-old
statistic a safe thing to plan with.

## 7. Scope

**In:** the collector and its CLI; the keyspace; the tuple format; the reader
with all four gates; the connection flag and its cache-key component; the tests
in §8.

**Out, and named so they are not assumed:** histograms, NDV, MCV and any
distribution (the collector could compute them — it scans — but selectivity
consumes them and that is a second change); automatic/triggered collection;
incremental recollection; per-index statistics; the `Cardinalities` clamp (§6);
plan-cache invalidation on data drift.

## 8. Tests

- **Collector correctness.** Counts equal a ground-truth scan, across several
  types, empty types, and after deletes.
- **Batching.** A table larger than one batch collects the same count as one
  smaller than a batch — the continuation path is exercised, not assumed.
- **Cap.** A type over its row cap is recorded ABSENT, never partial.
- **Reader gates, each driven independently:** flag off; a type missing; an
  entry expired; a read error; and the all-pass case. Four refusals and one
  acceptance, so no gate is vacuous.
- **All-or-nothing.** A two-table query with statistics for ONE plans exactly as
  with statistics for NEITHER — asserted on the plan, not on rows.
- **Plan change (the CQ-88 criterion).** Restore
  `multiway_join_order_probe_test.go`'s exact-driver assertion, which sits
  relaxed at `:101` with a re-arm comment naming this work. The relaxation is
  the regression; un-relaxing it is the acceptance.
- **Flag off is inert.** The full suite plans identically with the flag absent,
  including `conformance/cross_join_order_mechanism_probe_test.go` and
  `dup_alias_exists_order_probe_test.go`, which are cross-engine order pins that
  a plan change would redden.
- **No corpus entry sets the flag.** The cross-engine corpus is the parity net
  and must not become row-count sensitive.
- **Cache key.** Two connections differing only in the flag do not share a plan.

## 9. Acceptance

1. The exact-driver assertion in `multiway_join_order_probe_test.go` is restored
   and passes with the flag on.
2. With the flag off, the full suite is unchanged.
3. Every §8 reader gate is driven by a test, including the ones a healthy corpus
   never reaches.
4. A store with no collected statistics plans exactly as today.
5. `frl stats` collects a real schema and the counts match a scan.

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
