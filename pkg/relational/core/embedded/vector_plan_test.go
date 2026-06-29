package embedded

import (
	"errors"
	"strings"
	"testing"

	cascades "fdb.dev/pkg/recordlayer/query/plan/cascades"
)

// TestVectorPlan_QualifyPlansToVectorScan is the 9.3a/b proof: a full
// SELECT … WHERE <partition> QUALIFY ROW_NUMBER() OVER (… ORDER BY
// <distance>(vec, q)) <= K query must plan to a BY_DISTANCE vector index scan
// (the match candidate binds the DistanceRank predicate to the distance
// placeholder and ToScanPlan emits a RecordQueryVectorIndexPlan).
func TestVectorPlan_QualifyPlansToVectorScan(t *testing.T) {
	t.Parallel()
	schema := `CREATE TABLE docs (
			zone string, doc_id string, embedding vector(3, half),
			PRIMARY KEY (zone, doc_id))
		CREATE VECTOR INDEX doc_idx USING HNSW ON docs(embedding)
			PARTITION BY (zone) OPTIONS (METRIC = EUCLIDEAN_METRIC)`

	sql := `SELECT doc_id FROM docs WHERE zone = 'z1'
		QUALIFY ROW_NUMBER() OVER (
			PARTITION BY zone
			ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])
			OPTIONS ef_search = 64
		) <= 3`

	explain, err := PlanQueryForTest(sql, schema, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !strings.Contains(explain, "VectorIndexScan") {
		t.Fatalf("plan does not use a vector scan:\n%s", explain)
	}
	if !strings.Contains(explain, "BY_DISTANCE") {
		t.Errorf("vector scan is not BY_DISTANCE:\n%s", explain)
	}
}

// TestVectorPlan_PartitionOnlyDoesNotMatchVector covers the required-for-binding
// gate (Graefe/Torvalds): a plain WHERE on the partition column WITHOUT a QUALIFY
// distance-rank must NOT match the vector candidate (the index-only distance
// alias is unbound), so it must plan to a non-vector scan — never a vector scan
// with a nil query vector.
func TestVectorPlan_PartitionOnlyDoesNotMatchVector(t *testing.T) {
	t.Parallel()
	schema := `CREATE TABLE docs (
			zone string, doc_id string, embedding vector(3, half),
			PRIMARY KEY (zone, doc_id))
		CREATE VECTOR INDEX doc_idx USING HNSW ON docs(embedding)
			PARTITION BY (zone) OPTIONS (METRIC = EUCLIDEAN_METRIC)`

	explain, err := PlanQueryForTest("SELECT doc_id FROM docs WHERE zone = 'z1'", schema, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if strings.Contains(explain, "VectorIndexScan") {
		t.Fatalf("plain WHERE matched the vector candidate (distance unbound):\n%s", explain)
	}
}

// TestVectorPlan_UnsupportedQualifyErrors pins codex Finding 1: an unsupported
// QUALIFY window shape must FAIL the query, never be silently dropped (which
// would return rows as if the QUALIFY were absent). Covers the window orderings
// /functions Java rejects (DESC, RANK()) and the `= K` operator Java rejects at
// the DistanceRank comparison.
func TestVectorPlan_UnsupportedQualifyErrors(t *testing.T) {
	t.Parallel()
	schema := `CREATE TABLE docs (
			zone string, doc_id string, embedding vector(3, half),
			PRIMARY KEY (zone, doc_id))
		CREATE VECTOR INDEX doc_idx USING HNSW ON docs(embedding)
			PARTITION BY (zone) OPTIONS (METRIC = EUCLIDEAN_METRIC)`

	cases := []struct {
		name string
		sql  string
		// wantMsg, when set, pins the specific error text. The "> K" and "= K"
		// cases are the only ones uniquely caught by predicateHasUnloweredRowNumber
		// (the transform leaves an un-lowered RowNumberValue): asserting the
		// message makes that check a real sentinel — without it the query still
		// errors, but with a different (UnplannableIndexOnlyResidual) message.
		wantMsg string
	}{
		{
			"DESC window order",
			`SELECT doc_id FROM docs WHERE zone = 'z1'
				QUALIFY ROW_NUMBER() OVER (PARTITION BY zone
					ORDER BY euclidean_distance(embedding, [1.0,0.0,0.0]) DESC) <= 3`,
			"",
		},
		{
			"RANK not supported",
			`SELECT doc_id FROM docs WHERE zone = 'z1'
				QUALIFY RANK() OVER (PARTITION BY zone
					ORDER BY euclidean_distance(embedding, [1.0,0.0,0.0])) <= 3`,
			"",
		},
		{
			"equals operator rejected",
			`SELECT doc_id FROM docs WHERE zone = 'z1'
				QUALIFY ROW_NUMBER() OVER (PARTITION BY zone
					ORDER BY euclidean_distance(embedding, [1.0,0.0,0.0])) = 3`,
			"unsupported window function in QUALIFY",
		},
		{
			"greater-than operator rejected",
			`SELECT doc_id FROM docs WHERE zone = 'z1'
				QUALIFY ROW_NUMBER() OVER (PARTITION BY zone
					ORDER BY euclidean_distance(embedding, [1.0,0.0,0.0])) > 3`,
			"unsupported window function in QUALIFY",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			explain, err := PlanQueryForTest(tc.sql, schema, nil)
			if err == nil {
				t.Fatalf("unsupported QUALIFY (%s) did not error; plan:\n%s", tc.name, explain)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("unsupported QUALIFY (%s) error = %q, want it to contain %q", tc.name, err, tc.wantMsg)
			}
			if strings.Contains(explain, "VectorIndexScan") {
				t.Fatalf("unsupported QUALIFY (%s) produced a vector scan:\n%s", tc.name, explain)
			}
		})
	}
}

// TestVectorPlan_PartialPrefixPlansMultiPartition pins RFC-046 (multi-partition
// vector scan). A MULTI-COLUMN partition (zone, region) with only the leading
// column bound is now planned to a vector scan that fans out over the unbound
// partition column — matching Java, whose VectorIndexMaintainer.scan skip-scans
// the distinct partitions. The load-bearing assertion is that the DistanceRank
// binding SURVIVES the partial prefix: the explain shows `prefix=[=, *]` (region
// fanned out) AND `rank<=3` (the K — and therefore the whole query-vector
// binding — present). Before RFC-046 the partial prefix dropped the distance
// binding entirely, yielding a nil-query-vector plan (`prefix=[=, *], rank<=`,
// no K) — the exact codex/Torvalds regression this inverts.
func TestVectorPlan_PartialPrefixPlansMultiPartition(t *testing.T) {
	t.Parallel()
	schema := `CREATE TABLE docs (
			zone string, region string, doc_id string, embedding vector(3, half),
			PRIMARY KEY (zone, region, doc_id))
		CREATE VECTOR INDEX doc_idx USING HNSW ON docs(embedding)
			PARTITION BY (zone, region) OPTIONS (METRIC = EUCLIDEAN_METRIC)`

	// Two-column partition (zone, region) but only zone is bound — region fanned out.
	sql := `SELECT doc_id FROM docs WHERE zone = 'z1'
		QUALIFY ROW_NUMBER() OVER (
			PARTITION BY zone, region
			ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])
		) <= 3`

	explain, err := PlanQueryForTest(sql, schema, nil)
	if err != nil {
		t.Fatalf("partial-prefix vector query should now plan (RFC-046): %v", err)
	}
	if !strings.Contains(explain, "VectorIndexScan") {
		t.Fatalf("partial-prefix vector query did not plan to a vector scan:\n%s", explain)
	}
	// region unbound → fanned out: prefix shows a wildcard slot.
	if !strings.Contains(explain, "prefix=[=, *]") {
		t.Errorf("expected a partial prefix [=, *] (region fanned out), got:\n%s", explain)
	}
	// The DistanceRank binding survived: K (=3) is present, proving the query
	// vector was NOT dropped (the pre-RFC-046 nil-query-vector regression).
	if !strings.Contains(explain, "rank<=3") {
		t.Fatalf("DistanceRank binding dropped on partial prefix (nil-query-vector regression):\n%s", explain)
	}
}

// TestVectorPlan_PartitionInequalityNotConsumedIntoPrefix pins the Graefe/
// Torvalds RFC-046 condition: a partition-column INEQUALITY must NOT be consumed
// into the scan prefix (the executor encodes only an equality prefix tuple and
// would silently ignore an inequality → wrong rows). It must stay unconsumed —
// the scan prefix shows a wildcard for that column (fanned out), and the
// inequality is enforced elsewhere as a residual. Here a trailing-partition
// inequality (region > 'm') leaves prefix=[=, *].
func TestVectorPlan_PartitionInequalityNotConsumedIntoPrefix(t *testing.T) {
	t.Parallel()
	schema := `CREATE TABLE docs (
			zone string, region string, doc_id string, embedding vector(3, half),
			PRIMARY KEY (zone, region, doc_id))
		CREATE VECTOR INDEX doc_idx USING HNSW ON docs(embedding)
			PARTITION BY (zone, region) OPTIONS (METRIC = EUCLIDEAN_METRIC)`

	sql := `SELECT doc_id FROM docs WHERE zone = 'z1' AND region > 'm'
		QUALIFY ROW_NUMBER() OVER (
			PARTITION BY zone, region
			ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])
		) <= 3`

	explain, err := PlanQueryForTest(sql, schema, nil)
	if err != nil {
		t.Fatalf("partition-inequality vector query should plan (RFC-046): %v", err)
	}
	if !strings.Contains(explain, "VectorIndexScan") {
		t.Fatalf("partition-inequality vector query did not plan to a vector scan:\n%s", explain)
	}
	// region inequality must NOT be folded into the scan prefix as an equality:
	// the second prefix slot stays a wildcard (fanned out + residual elsewhere).
	if !strings.Contains(explain, "prefix=[=, *]") {
		t.Errorf("partition inequality was consumed into the scan prefix (would be silently ignored at execution); explain:\n%s", explain)
	}
	if !strings.Contains(explain, "rank<=3") {
		t.Fatalf("DistanceRank binding dropped on inequality prefix:\n%s", explain)
	}
}

// TestVectorPlan_MetricMismatchDoesNotMatchVector pins the metric-match
// invariant (@claude review): a QUALIFY ORDER BY cosine_distance(...) over an
// index declared EUCLIDEAN_METRIC must NOT plan to a vector scan. The query
// builds a CosineDistanceRowNumberValue, the candidate's placeholder is the
// metric-specific EuclideanDistanceRowNumberValue, so they don't match — the
// DistanceRank stays unmatched / uncompensatable and never lowers to a vector
// scan. (A vector scan with the wrong metric would silently return wrong
// neighbours, so this is a correctness guard, not just an optimization gap.)
func TestVectorPlan_MetricMismatchDoesNotMatchVector(t *testing.T) {
	t.Parallel()
	schema := `CREATE TABLE docs (
			zone string, doc_id string, embedding vector(3, half),
			PRIMARY KEY (zone, doc_id))
		CREATE VECTOR INDEX doc_idx USING HNSW ON docs(embedding)
			PARTITION BY (zone) OPTIONS (METRIC = EUCLIDEAN_METRIC)`

	sql := `SELECT doc_id FROM docs WHERE zone = 'z1'
		QUALIFY ROW_NUMBER() OVER (
			PARTITION BY zone
			ORDER BY cosine_distance(embedding, [1.0, 0.0, 0.0])
		) <= 3`

	explain, err := PlanQueryForTest(sql, schema, nil)
	// The cosine DistanceRank can't be served by a euclidean index and can't be
	// a residual filter (it's index-only), so the query is unplannable: the
	// planner's final-plan guard rejects it with UnplannableIndexOnlyResidualError
	// instead of building a plan that panics at execution.
	var uerr *cascades.UnplannableIndexOnlyResidualError
	if !errors.As(err, &uerr) {
		t.Fatalf("expected UnplannableIndexOnlyResidualError for metric mismatch, got err=%v\nexplain=%s", err, explain)
	}
}

// TestVectorPlan_GlobalRankResidualCanonicalShape is the RFC-156 Phase B landing
// condition (Graefe): a GLOBAL-rank vector K-NN with a NON-partition residual
// must plan to the property-driven canonical shape
//
//	Limit(k) → Filter(residual) → VectorIndexScan(distance-ordered)
//
// — the Limit and Filter ABOVE the scan, k NOT consumed into the scan (the scan
// is "ordered", never "rank<=k"), and NO index-only DistanceRank surviving as a
// residual (the query plans cleanly; the index-only distance marker lives only
// inside the scan's binding, per the match-candidate comment). On Phase A HEAD
// this query either failed to plan (no residual index) or produced an
// Intersection of the global top-k with the residual (the predicate-subset-of-
// global-top-k wrong answer) — never this shape.
func TestVectorPlan_GlobalRankResidualCanonicalShape(t *testing.T) {
	t.Parallel()
	schema := `CREATE TABLE docs (
			doc_id string, category string, embedding vector(3, half),
			PRIMARY KEY (doc_id))
		CREATE VECTOR INDEX doc_idx USING HNSW ON docs(embedding)
			OPTIONS (METRIC = EUCLIDEAN_METRIC)`

	sql := `SELECT doc_id FROM docs WHERE category = 'x'
		QUALIFY ROW_NUMBER() OVER (
			ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])
		) <= 3`

	explain, err := PlanQueryForTest(sql, schema, nil)
	if err != nil {
		t.Fatalf("global-rank residual vector query should plan (RFC-156 Phase B): %v", err)
	}

	iLimit := strings.Index(explain, "Limit(3")
	iFilter := strings.Index(explain, "PredicatesFilter(")
	iScan := strings.Index(explain, "VectorIndexScan(")
	if iLimit < 0 || iFilter < 0 || iScan < 0 {
		t.Fatalf("expected Limit → PredicatesFilter → VectorIndexScan; explain:\n%s", explain)
	}
	// Limit ABOVE Filter ABOVE the scan (string order = top-down nesting).
	if !(iLimit < iFilter && iFilter < iScan) {
		t.Fatalf("expected Limit ABOVE Filter ABOVE VectorIndexScan; explain:\n%s", explain)
	}
	// The scan is distance-ORDERED — k is NOT sunk into it.
	if !strings.Contains(explain, "VectorIndexScan(DOC_IDX, BY_DISTANCE, prefix=[], ordered") {
		t.Fatalf("vector scan is not in ordered-stream mode (k must not be consumed into the scan):\n%s", explain)
	}
	if strings.Contains(explain, "rank<") {
		t.Fatalf("k was consumed into the scan (rank<...) instead of a Limit above the filter:\n%s", explain)
	}
}

// TestVectorPlan_GlobalRankResidualDeterministic pins planner stability
// (query-engine skill): the canonical plan for a fixed global-rank residual
// query must be byte-identical across repeated planning runs.
func TestVectorPlan_GlobalRankResidualDeterministic(t *testing.T) {
	t.Parallel()
	schema := `CREATE TABLE docs (
			doc_id string, category string, embedding vector(3, half),
			PRIMARY KEY (doc_id))
		CREATE VECTOR INDEX doc_idx USING HNSW ON docs(embedding)
			OPTIONS (METRIC = EUCLIDEAN_METRIC)`
	sql := `SELECT doc_id FROM docs WHERE category = 'x'
		QUALIFY ROW_NUMBER() OVER (
			ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])
		) <= 3`

	first, err := PlanQueryForTest(sql, schema, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for i := 0; i < 10; i++ {
		got, err := PlanQueryForTest(sql, schema, nil)
		if err != nil {
			t.Fatalf("plan run %d: %v", i, err)
		}
		if got != first {
			t.Fatalf("plan run %d diverged:\nfirst=%s\ngot=%s", i, first, got)
		}
	}
}

// TestVectorPlan_NoResidualFoldsToSelfLimiting pins the Phase B fast-path
// regression: a GLOBAL-rank vector query with NO residual (and the partition-
// only case) must NOT keep a Limit over an ordered scan — SinkLimitIntoVector-
// ScanRule folds the Limit(k) back into the scan's self-limiting top-k (k IS in
// the scan as rank<=k), restoring the legacy one-shot path byte-for-byte.
func TestVectorPlan_NoResidualFoldsToSelfLimiting(t *testing.T) {
	t.Parallel()

	// (a) un-partitioned, no residual.
	schemaA := `CREATE TABLE docs (
			doc_id string, embedding vector(3, half),
			PRIMARY KEY (doc_id))
		CREATE VECTOR INDEX doc_idx USING HNSW ON docs(embedding)
			OPTIONS (METRIC = EUCLIDEAN_METRIC)`
	sqlA := `SELECT doc_id FROM docs
		QUALIFY ROW_NUMBER() OVER (
			ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])
		) <= 3`
	eA, err := PlanQueryForTest(sqlA, schemaA, nil)
	if err != nil {
		t.Fatalf("no-residual global-rank plan: %v", err)
	}
	if !strings.Contains(eA, "VectorIndexScan(DOC_IDX, BY_DISTANCE, prefix=[], rank<=3") {
		t.Fatalf("no-residual query did not fold k into the scan (rank<=3):\n%s", eA)
	}
	if strings.Contains(eA, "Limit(") || strings.Contains(eA, "ordered") {
		t.Fatalf("no-residual query left an un-folded Limit over an ordered scan:\n%s", eA)
	}

	// (b) partition-only residual (WHERE on the partition column): the partition
	// equality is consumed into the prefix, leaving NO residual filter — also
	// folds to the self-limiting per-partition scan.
	schemaB := `CREATE TABLE docs2 (
			zone string, doc_id string, embedding vector(3, half),
			PRIMARY KEY (zone, doc_id))
		CREATE VECTOR INDEX doc_idx2 USING HNSW ON docs2(embedding)
			PARTITION BY (zone) OPTIONS (METRIC = EUCLIDEAN_METRIC)`
	sqlB := `SELECT doc_id FROM docs2 WHERE zone = 'z1'
		QUALIFY ROW_NUMBER() OVER (PARTITION BY zone
			ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])) <= 3`
	eB, err := PlanQueryForTest(sqlB, schemaB, nil)
	if err != nil {
		t.Fatalf("partition-only plan: %v", err)
	}
	if !strings.Contains(eB, "VectorIndexScan(DOC_IDX2, BY_DISTANCE, prefix=[=], rank<=3") {
		t.Fatalf("partition-only query is not the self-limiting per-partition scan:\n%s", eB)
	}
	if strings.Contains(eB, "Limit(") {
		t.Fatalf("partition-only query left an un-folded Limit:\n%s", eB)
	}
}

// TestVectorPlan_MetricMismatchInJoinDoesNotLeak pins the JOIN dimension of the
// metric-mismatch case — the shape Graefe + Torvalds both reproduced as a
// regression when validateNoIndexOnlyResidual was prematurely retired. Here the
// index-only cosine DistanceRank is a predicate of a SelectExpression (the join
// body), not a standalone LogicalFilter, so it reaches a PHYSICAL residual filter
// via ImplementSimpleSelectRule / the NLJ residual builder — NOT the gated
// ImplementFilterRule. The catch-all validateNoIndexOnlyResidual backstop (which
// the ImplementFilterRule !isIndexOnly() gate does NOT replace) must still reject
// it with the clean UnplannableIndexOnlyResidualError rather than building a plan
// that panics in Comparison.EvalAgainst at execution.
func TestVectorPlan_MetricMismatchInJoinDoesNotLeak(t *testing.T) {
	t.Parallel()
	schema := `CREATE TABLE docs (
			zone string, doc_id string, embedding vector(3, half),
			PRIMARY KEY (zone, doc_id))
		CREATE TABLE tags (zone string, doc_id string, tag string,
			PRIMARY KEY (zone, doc_id))
		CREATE VECTOR INDEX doc_idx USING HNSW ON docs(embedding)
			PARTITION BY (zone) OPTIONS (METRIC = EUCLIDEAN_METRIC)`

	sql := `SELECT d.doc_id FROM docs d, tags t
		WHERE d.zone = 'z1' AND d.zone = t.zone AND d.doc_id = t.doc_id
		QUALIFY ROW_NUMBER() OVER (
			PARTITION BY d.zone
			ORDER BY cosine_distance(d.embedding, [1.0, 0.0, 0.0])
		) <= 3`

	explain, err := PlanQueryForTest(sql, schema, nil)
	var uerr *cascades.UnplannableIndexOnlyResidualError
	if !errors.As(err, &uerr) {
		t.Fatalf("expected UnplannableIndexOnlyResidualError for metric mismatch in a join, got err=%v\nexplain=%s", err, explain)
	}
}
