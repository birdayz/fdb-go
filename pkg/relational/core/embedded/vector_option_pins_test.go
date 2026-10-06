package embedded

import (
	"maps"
	"testing"
)

// TestVectorIndexSQLOptionsStoreJavasOptions pins every vector index option
// Java 4.14's DdlVisitor.SUPPORTED_VECTOR_OPTIONS exposes to SQL: the index
// option it writes is the VectorOptionKey's canonical name (IndexOptions), and
// the value is that key's serializer applied to the parsed value (Integer and
// Double String.valueOf, Boolean.parseBoolean, Metric::name). A GuardiANN index
// also records vectorEngine=GUARDIANN; an HNSW index leaves the engine
// implicit. The options a bare index of the same engine carries are the
// baseline, so the comparison is exactly what the OPTIONS clause adds.
func TestVectorIndexSQLOptionsStoreJavasOptions(t *testing.T) {
	t.Parallel()
	shared := "metric = COSINE_METRIC, use_rabitq = TRUE, rabitq_num_ex_bits = 4, " +
		"maintain_stats_probability = 0.50, sample_vector_stats_probability = 1, stats_threshold = 1000"
	sharedWant := map[string]string{
		"hnswMetric":                       "COSINE_METRIC",
		"hnswUseRaBitQ":                    "true",
		"hnswRaBitQNumExBits":              "4",
		"hnswMaintainStatsProbability":     "0.5",
		"hnswSampleVectorStatsProbability": "1.0",
		"hnswStatsThreshold":               "1000",
	}
	for _, c := range []struct {
		engine, options string
		want            map[string]string
	}{
		{
			"HNSW",
			shared + ", connectivity = 8, ef_construction = 64, m_max = 12, m_max_0 = 24",
			map[string]string{"hnswM": "8", "hnswEfConstruction": "64", "hnswMMax": "12", "hnswMMax0": "24"},
		},
		{
			"GUARDIANN",
			shared + ", primary_cluster_min = 2, primary_cluster_hard_max = 300, primary_cluster_max = 200, " +
				"underreplicated_primary_cluster_max = 150, replicated_cluster_max_writes = 7, " +
				"replicated_cluster_target = 3, replication_priority_min = 0.25, " +
				"insert_max_candidate_clusters = 5, delete_max_candidate_clusters = 6, " +
				"split_num_nearest_clusters = 9, merge_num_nearest_clusters = 10, " +
				"reassign_num_neighboring_clusters = 11, collapse_min_duplicates = 50",
			map[string]string{
				"guardiannPrimaryClusterMin":                "2",
				"guardiannPrimaryClusterHardMax":            "300",
				"guardiannPrimaryClusterMax":                "200",
				"guardiannUnderreplicatedPrimaryClusterMax": "150",
				"guardiannReplicatedClusterMaxWrites":       "7",
				"guardiannReplicatedClusterTarget":          "3",
				"guardiannReplicationPriorityMin":           "0.25",
				"guardiannInsertMaxCandidateClusters":       "5",
				"guardiannDeleteMaxCandidateClusters":       "6",
				"guardiannSplitNumNearestClusters":          "9",
				"guardiannMergeNumNearestClusters":          "10",
				"guardiannReassignNumNeighboringClusters":   "11",
				"guardiannCollapseMinDuplicates":            "50",
			},
		},
	} {
		options := func(clause string) map[string]string {
			tmpl, err := buildSchemaTemplateFromDDL("CREATE TABLE T (id BIGINT, v VECTOR(3, FLOAT), PRIMARY KEY (id)) " +
				"CREATE VECTOR INDEX vi USING " + c.engine + " ON T (v) " + clause)
			if err != nil {
				t.Fatalf("%s %s: %v", c.engine, clause, err)
			}
			idx := tmpl.Underlying().GetIndex("VI")
			if idx == nil {
				t.Fatalf("%s: no index VI", c.engine)
			}
			return maps.Clone(idx.Options)
		}
		base := options("")
		if got, want := base["vectorEngine"], map[string]string{"HNSW": "", "GUARDIANN": "GUARDIANN"}[c.engine]; got != want {
			t.Errorf("%s: vectorEngine = %q, want %q", c.engine, got, want)
		}
		got := options("OPTIONS (" + c.options + ")")
		added := map[string]string{}
		for k, v := range got {
			if bv, ok := base[k]; !ok || bv != v {
				added[k] = v
			}
		}
		want := maps.Clone(sharedWant)
		maps.Copy(want, c.want)
		if !maps.Equal(added, want) {
			t.Errorf("%s: OPTIONS added %v, want %v", c.engine, added, want)
		}
	}
}
