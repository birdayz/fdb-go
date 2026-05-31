# RFC-046: Multi-partition vector (HNSW) index scan

**Status:** Draft
**Phase:** 9.5 (TODO.md)
**Scope:** executor + record-layer maintainer + one planner-binding fix; **zero wire-format
change** (the on-disk HNSW layout and the `VectorIndexScanContinuation` proto are unchanged;
this only adds an *outer* skip-scan continuation wrapper that already exists — `FlatMapContinuation`).

## Problem

Java's `VectorIndexMaintainer.scan` fans out over partitions. A vector index partitioned by
`(zone, region)` queried with only `WHERE zone = 'z1'` is a multi-partition K-NN: Java
skip-scans the distinct full partition prefixes within the (possibly partial) prefix range,
runs one HNSW search per partition, and concatenates each partition's top-K
(`VectorIndexMaintainer.java` ~130-161). The `PARTITION BY zone, region` window means
`ROW_NUMBER() ... <= K` selects the top-K **per partition** — so the result is the union of
per-partition top-K, not a single global top-K.

Go is single-partition: `scanByDistanceWithParams` does one `getStorageForPrefix(prefix)` →
one HNSW graph → one `graph.Search`. Until now `VectorIndexScanMatchCandidate` required the
**full** partition prefix for binding, so a partial-prefix query was cleanly *unplannable*
(pinned by `TestVectorPlan_PartitionedRequiresFullPrefix`) rather than wrong. This RFC ports
the fan-out so a partial prefix plans and executes exactly as in Java.

## Investigation

### Java reference (`VectorIndexMaintainer.scan`, lines 112-161)
- `prefixSize = keyWithValueExpression.getSplitPoint()`.
- `prefixSize > 0` →
  `flatMapPipelined(prefixSkipScan(prefixSize, …), (prefixTuple, innerCont) -> scanSinglePartition(prefixTuple, innerCont, indexSubspace.subspace(prefixTuple), …), continuation, pipelineSize).skipThenLimit(skip, returnedRowLimit)`.
- `prefixSkipScan`/`nextPrefixTuple` (262-314): a `ChainedCursor` that reads **one** KV per
  distinct prefix (limit 1, ITERATOR mode) within `vectorIndexScanBounds.getPrefixRange()`,
  extracts `subTuple(unpack(key), 0, prefixSize)`, and resumes after the last prefix with
  `RANGE_EXCLUSIVE` low + `prefixRange.getHigh()` high.
- `scanSinglePartition` (176-239): per partition, an HNSW search returning
  `getAdjustedLimit()` entries (`< K` → K-1, `<= K` → K), wrapped in a `ListCursor` whose
  continuation serializes the remaining entries (`VectorIndexScanContinuation`).
- Cross-partition "merge" is just concatenation: each partition is independently distance-sorted;
  the final `skipThenLimit` applies the **outer** SQL limit (not the per-partition K).

### Go today
- `multidimensional_index_maintainer.go` already implements this exact shape for R-trees:
  `prefixSkipScanCursor` (`findNextPrefix` reads one KV after `nextPrefixStart`, extracts the
  first `PrefixSize` tuple elements, advances past the prefix subspace via `strinc`) and the
  per-prefix cursor emits a `FlatMapContinuation{OuterContinuation, InnerContinuation}`. This
  is the proven template — but its cross-prefix resume is incomplete (the outer position isn't
  threaded). We do it **fully** for vector (the user-requested Java-aligned continuation).
- `getSubspaceForPrefix(fullPrefix)` → `hnswSubspace.Sub(prefix...)`; every key under a
  partition lives below its full `splitPoint`-element prefix, so a range scan under a partial
  prefix yields keys whose first `splitPoint` elements identify each partition.
- `KeyWithValueExpression.SplitPoint()` (`key_expression.go:1200`) gives the partition size;
  `SearchKNN` already reads it (`vector_index_maintainer.go:747`).

### The planner trap (understated by the TODO)
Dropping the binding guard alone is **insufficient and would produce a nil-query-vector plan.**
`VectorIndexScanMatchCandidate.ComputeBoundParameterPrefixMap` (vector_index_match_candidate.go:214)
walks `parameters = [partitionAlias₀ … partitionAlias_{n-1}, distanceAlias]` and `return`s at
the **first unbound** alias. The `distanceAlias` is **last**, so on a partial prefix
(partitionAlias₁ unbound) the loop returns early and the distance binding is dropped →
`ToScanPlan` sees no DistanceRank → `queryVector == nil`. Java avoids this because
`toVectorIndexScanComparisons` separates the distance rank by **type**
(`instanceof DistanceRankValueComparison`), independent of the prefix length. So the Go fix must
collect the *contiguous equality partition prefix* **and always retain the distance binding**.

## Fix

Four commits, each green independently.

**1. Planner — retain the distance binding on a partial prefix.**
Rework `VectorIndexScanMatchCandidate.ComputeBoundParameterPrefixMap` to (a) collect the
contiguous bound **equality** partition prefix `parameters[0:partitionCount]` (stop at the
first unbound *or non-equality* column), then (b) **always** add `bindings[distanceAlias]`
when present. This mirrors Java's type-based separation: the index-only distance rank is never
gated behind partition-prefix contiguity. `ToScanPlan` is unchanged — it already keys
partition vs distance by type. Any partition column left unbound stays unbound (fanned out);
any partition *predicate* the scan doesn't consume remains a residual via the existing
compensation path (it is **not** index-only, so it can legally be a residual filter — only
the DistanceRank can't).

**Partition INEQUALITY (Graefe + Torvalds condition — traced, not asserted).** Go's executor
encodes only an *equality* prefix tuple into the vector scan range
(`VectorDistanceScanRangeWithPrefix`), and `executeVectorIndexScan` (`executor.go:330-335`)
breaks its prefix loop at the first non-equality. So a partition inequality **must not** be
consumed into the scan prefix — if it were, the executor would silently ignore it (Torvalds (b):
wrong rows). The fix is precise: step 1a's loop consumes only the contiguous **equality** run
(`ComparisonRangeEquality`) and **stops at the first non-equality**, so an inequality binding is
*never* placed into `prefixMap`/`prefixComps`. The unconsumed `zone > 'z1'` predicate is then
enforced exactly like a filter on any non-indexed column: it remains a residual above the scan
(the WHERE `ComparisonPredicate` the vector candidate did not consume). The fan-out ranges over
*all* partitions and the residual drops the non-matching ones — *correct, marginally slower*
than Java (whose `getPrefixRange()` is a full `TupleRange` that narrows the skip-scan by the
inequality endpoint). Threading the endpoint into the skip-scan range is a documented follow-up;
it is an executor-strategy divergence, **not** a wire/correctness one.

This residual behavior is the load-bearing risk Torvalds flagged, so it is **proven by an FDB
E2E test, not a sentence**: a `WHERE zone > 'z1'` (trailing-partition inequality) + distance-rank
QUALIFY query must return exactly the per-partition top-K over the partitions satisfying the
inequality. If the candidate were to *consume* the inequality and compensation failed to re-apply
it (the `ImplementIndexScanRule` path bypasses `Compensation` — see DIVERGENCES.md 7.7), this
test fails loud — at which point the inequality is threaded into the scan range as the contingency
fix. Equality-only consumption is the design that makes the residual the *same* mechanism as a
filter on any non-indexed column, which is the robust choice.

**2. Planner — drop the full-prefix binding guard.**
In `NewVectorIndexScanMatchCandidate`, `parametersRequiredForBinding` becomes
`{distanceAlias}` only (delete the loop that adds every partition alias) — matching Java's
`VectorIndexExpansionVisitor` (`parametersRequiredForBinding` = placeholders where
`value.isIndexOnly()`, i.e. just the distance placeholder). A partial prefix now binds.

**3. Maintainer — fan out over partitions.**
`ScanByDistance` computes `partitionSize` from the index's `KeyWithValueExpression.SplitPoint()`.
- `len(prefix) == partitionSize` (full) or `partitionSize == 0` (unpartitioned) → existing
  single-partition `scanByDistanceWithParams` (unchanged).
- `len(prefix) < partitionSize` → a new `vectorMultiPartitionCursor` that ports Java's
  `flatMapPipelined(prefixSkipScan, scanSinglePartition)`:
  - **outer** `nextPartitionPrefix(lastPrefix)` — 1:1 with Java's `nextPrefixTuple`: read one
    KV from `hnswSubspace` after `lastPrefix` (exclusive) within the partial-prefix range,
    extract the first `partitionSize` tuple elements; nil ⇒ exhausted.
  - **inner** — the existing per-partition search (refactor `scanByDistanceWithParams`'s body
    into a `searchOnePartition(fullPrefix, …) ([]*IndexEntry)` helper) producing that
    partition's top-`k` entries, fed through the existing `vectorSearchCursor`.
  - **calls the graph directly** via the existing `scanByDistanceWithParams` body (refactored
    into `searchOnePartition`), **not** `SearchKNN` — `SearchKNN`'s empty-prefix guard
    (`vector_index_maintainer.go:746-751`) rejects partitioned indexes and would block the
    fan-out; `ScanByDistance` is already off that path (Torvalds note).

**Per-partition K vs global limit — two distinct quantities (Torvalds (c)/(e), framing fixed).**
These were conflated in the draft; the correct model:
- **Per-partition K** is the HNSW search depth = the rank's `getAdjustedLimit()` (`< K` → K-1,
  `<= K` → K). It rides in the scan range (`VectorDistanceScanRangeWithPrefix`'s `k`), exactly as
  today, and is passed **to each** `searchOnePartition`. For `QUALIFY ROW_NUMBER() OVER
  (PARTITION BY …) <= K` this K is **not** an `ExecuteProperties.ReturnedRowLimit` — it is the
  per-window limit, and the SQL semantics are top-K **per partition**, so the union across
  partitions is *intentionally unbounded* (no global cap). The cursor must NOT clamp the union
  to K.
- **Global cap** = `ExecuteProperties.ReturnedRowLimit`, set only when the SQL has an outer
  `LIMIT`/skip (Java's final `skipThenLimit`). The multi-partition cursor honors it across
  partitions by counting delivered rows (the same `ReturnedRowLimit` discipline
  `prefixSkipScanCursor` uses at `multidim:625`), and stops mid-fan-out when hit. With no SQL
  `LIMIT` it is 0/unlimited and the full per-partition-top-K union flows. The earlier draft's
  "outer returned-row-limit caps the total (Java's skipThenLimit)" was wrong for the bare-QUALIFY
  case and is corrected here: K ≠ the row limit.

**4. Maintainer — full cross-partition continuation (Java-aligned). Resume seeding spelled out
(Torvalds).**
The multi-partition cursor emits `FlatMapContinuation{OuterContinuation: pack(currentPrefix),
InnerContinuation: <per-partition VectorIndexScanContinuation>}` on every delivered row, where
`currentPrefix` is the **full** `partitionSize`-element prefix of the partition currently being
drained. Resume is the entire correctness risk, so the seeding is explicit:
  1. Unpack `OuterContinuation` → `resumePrefix` (the partition that was mid-flight).
  2. Rebuild that partition's per-partition cursor by calling `searchOnePartition(resumePrefix, …)`
     and seeding its `vectorSearchCursor` from `InnerContinuation` (the existing replay-from-saved
     -entries path — `parseVectorScanContinuation`), so it resumes *within* the partition at the
     saved position.
  3. Seed the outer skip-scan state so the **next** `nextPartitionPrefix` call starts strictly
     **after** `resumePrefix` (set `nextPartitionStart = strinc(hnswSubspace.Sub(resumePrefix...))`),
     so when the current partition exhausts the cursor advances to the next distinct partition and
     never re-emits `resumePrefix`. This is the seeding `prefixSkipScanCursor` omits (`multidim:597-600`
     admits cross-prefix resume is unsupported); we do it fully here.
On a fresh scan (`continuation == nil`) the outer state starts at `hnswSubspace` and the inner is
empty. The single-partition path (full prefix / unpartitioned) keeps emitting a raw
`VectorIndexScanContinuation` (unchanged); the path is chosen deterministically by
`(len(prefix), partitionSize)`, so resume never guesses the format. Both `FlatMapContinuation` and
`VectorIndexScanContinuation` already exist in `gen/` (used by the multidim maintainer and the
current vector cursor) — **no new proto, no wire change.** Pinned by the page-by-page determinism
test (returned-row-limit 1 across multiple partitions, concatenation == unpaged result).

## Performance
Non-partial-prefix queries are byte-for-byte unchanged (same single-partition path; the
dispatch adds one `len(prefix) < partitionSize` check). Partial-prefix queries do one HNSW
search per matching partition — the same total work Java does, and far cheaper than the only
prior alternative (unplannable → full-scan-then-sort). The skip-scan is one limit-1 KV read per
partition (Java's cost too). FDB 5s tx limit: each partition is a snapshot read; the cursor is
lazy (one partition drained before the next is sought), so memory stays O(k) per partition, not
O(k·partitions). Stress-1M has no vector index → unaffected; run before/after as a guard.

## Test plan
- **Planner:** convert `TestVectorPlan_PartitionedRequiresFullPrefix` from "must be unplannable"
  to "partial prefix now plans to a `VectorIndexScan` with a **non-nil** query vector and
  `prefix=[=, *]`" (assert the explain shows the bound query vector + K, the exact nil-vector
  regression codex found). Keep a unit test that a fully-unbound partition prefix (no equality
  at all) still plans (fan out over **all** partitions).
- **FDB E2E** (`vector_search_e2e_fdb_test.go`, real testcontainers, `t.Parallel()`, unique
  subspace): `PARTITION BY (zone, region)`, insert vectors into several `(z1, r*)` partitions +
  a decoy `z2` partition; `WHERE zone='z1' QUALIFY ROW_NUMBER() OVER (PARTITION BY zone,region
  ORDER BY euclidean_distance(...)) <= K`; EXPLAIN-pin the `VectorIndexScan`, assert the result
  is the **union of per-partition top-K** across every `z1` region and **excludes** `z2`.
- **Continuation:** drive the multi-partition scan page-by-page (returned-row-limit 1) and
  assert the concatenation of pages equals the unpaged result (cross-partition resume correct),
  10× for determinism.
- **Partition inequality (Graefe condition):** an FDB test with `WHERE zone > 'z1'` (or a
  trailing-partition-column inequality) + distance-rank QUALIFY — assert the returned rows are
  exactly the per-partition top-K over the partitions that satisfy the inequality (proving the
  inequality is enforced as a residual, never silently dropped over the fanned-out partitions).
- **Unpartitioned regression:** existing `TestFDB_VectorSearch_QualifyE2E` (single partition)
  stays green unchanged.

## Divergence note
Go keeps its dedicated `RecordQueryVectorIndexPlan` (RFC-045) rather than Java's
`RecordQueryIndexPlan + VectorIndexScanComparisons`; this RFC changes only the maintainer's scan
strategy and one planner binding rule, so that read-side-only divergence is unaffected. After
this lands, `DIVERGENCES.md` "Vector scan is single-partition" is **closed**, and the
vector-candidate comment about requiring the full prefix is deleted.
