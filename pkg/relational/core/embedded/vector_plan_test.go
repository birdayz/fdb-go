package embedded

import (
	"errors"
	"strings"
	"testing"

	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
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
