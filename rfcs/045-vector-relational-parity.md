# RFC-045: Vector / HNSW relational SQL parity

**Status:** Implemented (Graefe ACK, Torvalds ACK) — full SQL vector K-NN read path lands; see Phase 9 in TODO.md
**Phase:** 9 (TODO.md)
**Scope:** read-side query surface + DDL; **zero new wire format** (the on-disk HNSW
format already matches Java and is unchanged by this RFC).

## Problem

Java's relational layer can create and query an HNSW vector index entirely from SQL:

```sql
CREATE VECTOR INDEX docEuclid USING HNSW ON docView(embedding)
       PARTITION BY (zone, bookshelf) OPTIONS (metric = euclidean_metric);

SELECT docId, euclidean_distance(embedding, ?) AS distance
FROM documents
WHERE zone = 'zone1' AND bookshelf = 'fiction'
QUALIFY ROW_NUMBER() OVER (
    PARTITION BY zone, bookshelf
    ORDER BY euclidean_distance(embedding, ?) ASC
    OPTIONS ef_search = 100
) <= 3;
```

The Go port can do **neither** from SQL. `CREATE VECTOR INDEX` has no DDL handler, and
`QUALIFY ROW_NUMBER() OVER (... ORDER BY <distance>)` is parsed but never planned. This is
the last missing slice of Java parity for vector search — and, per RFC-045's investigation,
it is the *only* window-function surface Java actually supports (general window functions /
`LAG`/`LEAD` / running aggregates do not exist in Java; `RANK` is index-only via a rank
index). So "window functions" reduces to "finish vector relational parity."

This is **not net-new capability** — Java already does it. It is a parity gap, and a
read-side one: wire compat is untouched because the HNSW on-disk format is already ported
and verified; this RFC only lets Go *express* the queries Java can express.

## Investigation

### Already ported and FDB-tested (the heavy lifting is done)

| Layer | Go file | Notes |
|---|---|---|
| HNSW graph (insert/delete/kNN, multi-layer, node serde) | `pkg/recordlayer/hnsw.go` | Wire-compatible with Java `HNSW.java` |
| Index maintainer (per-partition graph, write-lock, scan) | `pkg/recordlayer/vector_index_maintainer.go` | Mirrors `VectorIndexMaintainer.java` |
| RaBitQ quantization + rotation | `pkg/rabitq`, `fht_kac_rotator.go`, `vec_math.go` | |
| HNSW stats | `pkg/recordlayer/hnsw_stats.go` | |
| Chaos verify | `pkg/recordlayer/chaos/verify_vector.go` | |
| Integration tests | `vector_index_test.go`, `rabitq_test.go`, `hnsw_stats_test.go`, `bench/sift_benchmark_test.go` | Prove the core works against real FDB |
| Cascades values (seeds) | `value_row_number.go`, `value_*_distance_row_number.go`, `value_row_number_high_order.go` | Skeletons; `transformComparisonMaybe` is a comment only |
| Match candidate | `vector_index_match_candidate.go` (232 LOC) | `NewVectorIndexScanMatchCandidate` exists, but `ToScanPlan` (:200) emits a **generic** `NewRecordQueryIndexPlan` — see Missing #4/#6 |
| `DistanceRank` comparison | `predicates/comparisons.go:87-89` | `ComparisonDistanceRank{Equals,LessThan,LessThanOrEq}` enums present |
| Executor BY_DISTANCE scan | `index_scan.go:46,338-345`, `vector_index_maintainer.go` | `IndexScanByDistance` → `ScanByDistance`/`ScanVectorIndex`; reachable only via `ScanIndexByType`, **not** from a plan |
| SQL grammar | `RelationalParser.g4` | `vectorIndexDefinition`, `qualifyClause`, `overClause`, `windowSpec`, `nonAggregateWindowedFunction(ROW_NUMBER …)` all present |

### Missing (the "relational bits")

1. **DDL.** No `vectorIndexDefinition` handler in `pkg/relational`. Java's is
   `DdlVisitor.visitVectorIndexDefinition`; the resulting metadata `Index` must have root
   `KeyWithValueExpression` (cols before split = partition prefix, first col after split =
   vector column), be non-unique, no grouping, no version columns
   (`VectorIndexMaintainerFactory.VectorIndexValidator`).
2. **Query front-end.** `grep QualifyClause` → 0 hits. `extractFunctionNameFromCall`
   (`cascades_generator.go:2226`) only returns the function *name* string. Nothing builds a
   `RowNumberValue` from the parse tree, no `OVER`/`windowSpec` visitor, no `QUALIFY` →
   predicate lowering.
3. **`transformComparisonMaybe`.** Java's `RowNumberValue.transformComparisonMaybe` rewrites
   `ROW_NUMBER() OVER(... ORDER BY distance(vec,q)) <= K` into
   `DistanceRankValueComparison(queryVector, k, efSearch, isReturningVectors)`. Go has only a
   doc comment (`value_row_number.go:29-35`, now stale re: "specialisations aren't ported").
4. **Candidate reachability.** `NewVectorIndexScanMatchCandidate` is constructed **nowhere**.
   Java creates it via `VectorIndexMaintainerFactory.createMatchCandidates` →
   `VectorIndexExpansionVisitor`. Go's candidate enumeration
   (`plan_context_builder.go:46` and the metadata-driven builder in the embedded layer) has no
   vector branch, so the planner can never pick a vector scan.
5. **No e2e.** No yamsql / sqldriver test issues `CREATE VECTOR INDEX` or `QUALIFY ROW_NUMBER()`.
6. **No vector *physical plan* — the candidate is reachable-but-inert (Torvalds catch).**
   Even once 9.3 wires the candidate, `VectorIndexScanMatchCandidate.ToScanPlan`
   (`vector_index_match_candidate.go:200-219`) dumps every `ComparisonRange` into a plain
   `NewRecordQueryIndexPlan` — no partition/distance-rank split, no `efSearch`/`isReturningVectors`
   threading. That plan executes a default `BY_VALUE` scan, which **errors** for a vector index
   (`index_scan.go:271`, guard at :269: "must be scanned with BY_DISTANCE"). The executor *has* the BY_DISTANCE
   path (`IndexScanByDistance` → `ScanByDistance`, `index_scan.go:338-345`), but **no physical
   plan type drives it** (`grep VectorIndexPlan pkg/.../plans` → 0). So the split logic and a
   vector-aware scan plan must be **built**, not "branched in."

## Fix

Four staged commits (each green + committed independently; one logical change per commit),
matching the TODO 9.1–9.4 breakdown. Java is the reference at every step.

**9.1 — DDL.** Port `DdlVisitor.visitVectorIndexDefinition` (Java :262) + `parseVectorOptions`
(:328). Java's algorithm: the single `indexColumnList` column is the *value* column (resolve
its type → must be `VECTOR`, derive dimensions → `HNSW_NUM_DIMENSIONS`); `PARTITION BY (...)`
columns become the *key* prefix; `INCLUDE` is rejected; build a `KeyWithValueExpression`
(empty-key form) with key=partition cols, value=[vector col]; map options
`CONNECTIVITY→HNSW_M`, `EF_CONSTRUCTION`, `M_MAX`, `METRIC∈{euclidean,euclidean_square,cosine,
dot_product}`, `USE_RABITQ`, `RABITQ_NUM_EX_BITS`, stats knobs; set type `vector`.
**Symbol correction (Torvalds):** the Go constructor is the exported `NewVectorIndex` at
`index.go:333`, signature `(name string, rootExpression KeyExpression, numDimensions int)` — it
sets only `HNSW_NUM_DIMENSIONS`. So the DDL handler must itself build the
`KeyWithValueExpression` root and merge the remaining HNSW options into `idx.Options` after
construction; it is *not* a one-call reuse. Validate structure as Java's `VectorIndexValidator`
(non-unique, no grouping, ≥1 col after split, first post-split col = vector).

**9.2 — Query front-end + transform.** Add the `OVER`/`windowSpec`/`partitionClause`/
`windowOptionsClause` visitor and `QUALIFY` handling to the SQL→Value path, mirroring Java
`ExpressionVisitor.visitNonAggregateWindowedFunction` / `visitOverClause`. The distance
function in the `ORDER BY` selects the specialized value
(`EuclideanDistanceRowNumberValue` / `CosineDistanceRowNumberValue` /
`DotProductDistanceRowNumberValue` / `EuclideanSquareDistanceRowNumberValue`) — flesh out the
seed classes. Port `transformComparisonMaybe` so the `QUALIFY … <= K` (and `< K`) comparison
lowers to a single `DistanceRankValueComparison`. Enforce Java's guards: window functions in
`WHERE` error (`42F21`); only ascending ORDER BY; `ROW_NUMBER` is index-only.

**9.3 — Candidate wiring + the missing vector physical plan (expanded per Torvalds).** Three
sub-pieces, not one branch:

- *(9.3a) Enumeration.* Add a vector branch to the candidate enumeration so a `vector`-type
  metadata index yields a `NewVectorIndexScanMatchCandidate` (Go analog of
  `VectorIndexMaintainerFactory.createMatchCandidates` → `VectorIndexExpansionVisitor`).
- *(9.3b) Scan-comparison split.* Rework `VectorIndexScanMatchCandidate.ToScanPlan`
  (`vector_index_match_candidate.go:200`, today a generic `NewRecordQueryIndexPlan`) to port
  Java's `toVectorIndexScanComparisons` (`VectorIndexScanMatchCandidate.java:408`): separate the
  partition-equality `ComparisonRange`s (the prefix) from the single distance-rank comparison.
  **Graefe watch-item (a):** the distance rank rides as an *equality-shaped* `ComparisonRange`
  inside the equality bucket (Java keys on `equalityComparison instanceof
  DistanceRankValueComparison`) — replicate that, do **not** invent a parallel "distance slot."
- *(9.3c) Vector physical plan + dispatch.* Introduce a vector-aware physical plan (a
  `RecordQueryVectorIndexPlan`, or a scan-bounds-carrying variant) that threads the query
  vector, `k`, `efSearch`, `isReturningVectors` + partition prefix and, at execution, builds
  `VectorDistanceScanRange(queryVector, k, efSearch)` and dispatches **BY_DISTANCE** via
  `ScanIndexByType`/`ScanVectorIndex` → `ScanByDistance` (`index_scan.go:338-345`). Without this
  the candidate plans a `BY_VALUE` scan that errors at `index_scan.go:271`. The plan must carry
  proper `GetCorrelatedToWithoutChildren()` (the query-vector comparand may be a parameter).

Mirror the constraints at the planner: index must be partitioned, query must bind partition
keys, exactly one distance rank, SQL metric must equal index metric, and ORDER BY ascending —
**Graefe watch-item (b):** put the ASC-only guard at the same site Java does (the OVER-clause
visitor, `UNSUPPORTED_SORT`), not a new bolted-on check.

**9.4 — E2E proof.** Port Java's `window-function-documentation-queries.yamsql` (KNN top-K,
`ef_search`, `<`/`<=`, OR-of-two-KNN, the `WHERE`-clause error case) as a Go yamsql scenario,
plus an FDB integration test that **`EXPLAIN`-pins the vector index scan** (proves the index
fires, not a full-scan fallback) and asserts row + distance correctness, with `t.Parallel()`
and a unique subspace.

## Performance

No hot-path change to existing queries: the vector candidate is only constructed for
`vector`-type indexes and only matches the `QUALIFY ROW_NUMBER()` shape, so non-vector plans
enumerate exactly as today (one extra `index.Type == "vector"` branch in candidate
construction, O(#indexes)). The vector scan itself is the existing, benchmarked HNSW path
(`sift_benchmark_test.go`) — logarithmic kNN vs. an O(N) full-scan-then-sort fallback, so for
KNN queries this is a large improvement over the only currently-expressible alternative
(can't express it at all today). Stress-1M is unaffected (no vector index in that schema);
run before/after as a regression guard regardless.

## Test plan

- **9.1:** unit test that `CREATE VECTOR INDEX … OPTIONS(…)` produces the expected metadata
  `Index` (type, `KeyWithValueExpression` split, option map) byte-identical to Java's proto;
  negative tests for the validator (unique → reject, grouping → reject, no vector col → reject).
- **9.2:** unit tests for `transformComparisonMaybe` (each distance metric → correct
  specialized value + `DistanceRankValueComparison`); `WHERE`-clause window fn → `42F21`;
  descending ORDER BY → unsupported-sort error.
- **9.3:** planner test asserting a `vector`-index metadata yields a vector candidate; a unit
  test that `ToScanPlan` produces the vector physical plan (not `RecordQueryIndexPlan`) with the
  partition prefix and distance-rank correctly split; an execution test that the plan dispatches
  **BY_DISTANCE** (reaches `ScanByDistance`, not the `index_scan.go:271` BY_VALUE error).
- **9.4:** the ported yamsql + an FDB integration test (real testcontainers FDB): insert
  vectors, run the documentation KNN queries, assert rows + distances and the EXPLAIN plan
  shape; determinism 10×.
- Fuzz the `transformComparisonMaybe` comparison-rewrite if it does any non-trivial parsing.

## Divergence note: dedicated vector plan type (Graefe review)

Java does not introduce a `RecordQueryVectorIndexPlan`; `toEquivalentPlan` emits
a plain `RecordQueryIndexPlan` whose scan-bounds are a `VectorIndexScanComparisons`
(the BY_DISTANCE-ness rides in the scan comparisons, not the plan class). Go uses
a dedicated `RecordQueryVectorIndexPlan` instead: Go's `RecordQueryIndexPlan`
carries a flat `[]*ComparisonRange` (no `IndexScanParameters`/`ScanComparisons`
abstraction to host a vector-specific bounds subtype), and a BY_DISTANCE scan
needs typed query-vector / k / ef_search fields the `ComparisonRange` list can't
represent without a parallel encoding. A dedicated leaf plan is the cleaner Go
expression and keeps the BY_DISTANCE dispatch explicit at the executor. This is a
read-side-only extension — no wire-format impact (the on-disk HNSW format and the
`VectorIndexScanContinuation` are unchanged) — and Java still reads/writes the
exact same records. Endorsed as acceptable by Graefe.

## Non-goals

- General-purpose window functions (`LAG`/`LEAD`/running aggregates over plain tables) — Java
  lacks them; a Go-only extension is deferred, not part of this RFC.
- `RANK`/`DENSE_RANK` over a rank index — separate (rank-index) parity, not vector.
- Any change to the HNSW on-disk format or maintainer (already ported + wire-verified).
