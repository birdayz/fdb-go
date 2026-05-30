# RFC-045: Vector / HNSW relational SQL parity

**Status:** Draft
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
| Match candidate | `vector_index_match_candidate.go` (232 LOC) | `NewVectorIndexScanMatchCandidate` exists |
| `DistanceRank` comparison | `predicates/comparisons.go` | stub present |
| SQL grammar | `RelationalParser.g4` | `vectorIndexDefinition`, `qualifyClause`, `overClause`, `windowSpec`, `nonAggregateWindowedFunction(ROW_NUMBER …)` all present |

### Missing (the "relational bits")

1. **DDL.** No `vectorIndexDefinition` handler in `pkg/relational`. Java's is
   `DdlVisitor.visitVectorIndexDefinition`; the resulting metadata `Index` must have root
   `KeyWithValueExpression` (cols before split = partition prefix, first col after split =
   vector column), be non-unique, no grouping, no version columns
   (`VectorIndexMaintainerFactory.VectorIndexValidator`).
2. **Query front-end.** `grep QualifyClause` → 0 hits. `extractFunctionNameFromCall`
   (`cascades_generator.go:2237`) only returns the function *name* string. Nothing builds a
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

## Fix

Four staged commits (each green + committed independently; one logical change per commit),
matching the TODO 9.1–9.4 breakdown. Java is the reference at every step.

**9.1 — DDL.** Port `DdlVisitor.visitVectorIndexDefinition`: parse
`USING HNSW`, the indexed vector column, `PARTITION BY (...)` prefix, optional covering
`includeClause`, and `OPTIONS(metric=…, ef_search=…, m=…, …)` → a metadata `Index{Type:
"vector"}` with the `KeyWithValueExpression` root and the HNSW option keys already defined in
`metadata_evolution_validator.go`. Reuse the existing `newVectorIndex` constructor
(`index.go:336`). Validate structure exactly as Java's `VectorIndexValidator` (non-unique,
no grouping, ≥1 col after split, first post-split col = vector).

**9.2 — Query front-end + transform.** Add the `OVER`/`windowSpec`/`partitionClause`/
`windowOptionsClause` visitor and `QUALIFY` handling to the SQL→Value path, mirroring Java
`ExpressionVisitor.visitNonAggregateWindowedFunction` / `visitOverClause`. The distance
function in the `ORDER BY` selects the specialized value
(`EuclideanDistanceRowNumberValue` / `CosineDistanceRowNumberValue` /
`DotProductDistanceRowNumberValue` / `EuclideanSquareDistanceRowNumberValue`) — flesh out the
seed classes. Port `transformComparisonMaybe` so the `QUALIFY … <= K` (and `< K`) comparison
lowers to a single `DistanceRankValueComparison`. Enforce Java's guards: window functions in
`WHERE` error (`42F21`); only ascending ORDER BY; `ROW_NUMBER` is index-only.

**9.3 — Candidate wiring.** Add a vector branch to the candidate enumeration so a
`vector`-type metadata index yields a `NewVectorIndexScanMatchCandidate` (Go analog of
`VectorIndexMaintainerFactory.createMatchCandidates`). The candidate's
`toVectorIndexScanComparisons` splits partition-equality predicates from the one
`DistanceRankValueComparison` → `VectorIndexScanComparisons` → the existing maintainer scan
(`ScanByDistance` / `SearchKNN`). Mirror the constraints: index must be partitioned, query
must bind partition keys, exactly one distance rank, SQL metric must equal index metric.

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
- **9.3:** planner test asserting a `vector`-index metadata yields a vector candidate and the
  planned shape is a vector index scan (EXPLAIN).
- **9.4:** the ported yamsql + an FDB integration test (real testcontainers FDB): insert
  vectors, run the documentation KNN queries, assert rows + distances and the EXPLAIN plan
  shape; determinism 10×.
- Fuzz the `transformComparisonMaybe` comparison-rewrite if it does any non-trivial parsing.

## Non-goals

- General-purpose window functions (`LAG`/`LEAD`/running aggregates over plain tables) — Java
  lacks them; a Go-only extension is deferred, not part of this RFC.
- `RANK`/`DENSE_RANK` over a rank index — separate (rank-index) parity, not vector.
- Any change to the HNSW on-disk format or maintainer (already ported + wire-verified).
