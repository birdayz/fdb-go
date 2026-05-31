package embedded

import (
	"strings"
	"testing"
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
