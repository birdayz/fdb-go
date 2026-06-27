package embedded

import (
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

// TestVectorDDL_PartitionedIndexShape drives the full DDL parse path
// (CREATE TABLE with a vector column + CREATE VECTOR INDEX … PARTITION BY …
// OPTIONS(…)) and verifies the resulting metadata index is a vector index
// whose root is a KeyWithValueExpression splitting the partition prefix
// from the indexed vector column — matching Java's vector index layout.
func TestVectorDDL_PartitionedIndexShape(t *testing.T) {
	t.Parallel()
	ddl := `CREATE TABLE documents (
			zone string, doc_id string, bookshelf string,
			embedding vector(3, half),
			PRIMARY KEY (zone, doc_id))
		CREATE VECTOR INDEX doc_euclid USING HNSW ON documents(embedding)
			PARTITION BY (zone, bookshelf)
			OPTIONS (METRIC = EUCLIDEAN_METRIC, EF_CONSTRUCTION = 100, CONNECTIVITY = 24, USE_RABITQ = true)`

	tmpl, err := buildSchemaTemplateFromDDL(ddl)
	if err != nil {
		t.Fatalf("buildSchemaTemplateFromDDL: %v", err)
	}
	idx := tmpl.Underlying().GetIndex("DOC_EUCLID")
	if idx == nil {
		t.Fatal("vector index DOC_EUCLID not found in metadata")
	}
	if idx.Type != recordlayer.IndexTypeVector {
		t.Errorf("index type = %q, want %q", idx.Type, recordlayer.IndexTypeVector)
	}

	kwv, ok := idx.RootExpression.(*recordlayer.KeyWithValueExpression)
	if !ok {
		t.Fatalf("root expression is %T, want *KeyWithValueExpression", idx.RootExpression)
	}
	// key part = partition prefix (zone, bookshelf); value part = embedding.
	// SQL identifiers normalize to upper-case (StripIdentifierQuotes).
	if got := kwv.SplitPoint(); got != 2 {
		t.Errorf("split point = %d, want 2 (partition columns)", got)
	}
	if got := kwv.InnerKey().FieldNames(); len(got) != 3 ||
		got[0] != "ZONE" || got[1] != "BOOKSHELF" || got[2] != "EMBEDDING" {
		t.Errorf("inner key fields = %v, want [ZONE BOOKSHELF EMBEDDING]", got)
	}

	// Options: dimensions derived from the vector column type; OPTIONS mapped
	// to the recordlayer HNSW keys.
	assertOpt(t, idx, recordlayer.IndexOptionVectorNumDimensions, "3")
	assertOpt(t, idx, recordlayer.IndexOptionVectorMetric, "EUCLIDEAN_METRIC")
	assertOpt(t, idx, recordlayer.IndexOptionHNSWEfConstruction, "100")
	assertOpt(t, idx, recordlayer.IndexOptionHNSWM, "24")
	assertOpt(t, idx, recordlayer.IndexOptionHNSWUseRaBitQ, "true")
}

// TestVectorDDL_UnpartitionedIndexShape verifies an unpartitioned vector
// index has split point 0 (the whole inner key is the vector value).
func TestVectorDDL_UnpartitionedIndexShape(t *testing.T) {
	t.Parallel()
	ddl := `CREATE TABLE docs (id bigint, embedding vector(128, float), PRIMARY KEY (id))
		CREATE VECTOR INDEX doc_cos USING HNSW ON docs(embedding) OPTIONS (METRIC = COSINE_METRIC)`

	tmpl, err := buildSchemaTemplateFromDDL(ddl)
	if err != nil {
		t.Fatalf("buildSchemaTemplateFromDDL: %v", err)
	}
	idx := tmpl.Underlying().GetIndex("DOC_COS")
	if idx == nil {
		t.Fatal("vector index DOC_COS not found")
	}
	kwv, ok := idx.RootExpression.(*recordlayer.KeyWithValueExpression)
	if !ok {
		t.Fatalf("root expression is %T, want *KeyWithValueExpression", idx.RootExpression)
	}
	if got := kwv.SplitPoint(); got != 0 {
		t.Errorf("split point = %d, want 0 (unpartitioned)", got)
	}
	assertOpt(t, idx, recordlayer.IndexOptionVectorNumDimensions, "128")
	assertOpt(t, idx, recordlayer.IndexOptionVectorMetric, "COSINE_METRIC")
}

// TestVectorDDL_Errors covers the rejected shapes (mirroring Java's guards).
func TestVectorDDL_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ddl  string
	}{
		{
			name: "indexed column not vector type",
			ddl: `CREATE TABLE t (id bigint, name string, PRIMARY KEY (id))
				CREATE VECTOR INDEX bad USING HNSW ON t(name)`,
		},
		{
			name: "indexed column missing",
			ddl: `CREATE TABLE t (id bigint, embedding vector(3, half), PRIMARY KEY (id))
				CREATE VECTOR INDEX bad USING HNSW ON t(nope)`,
		},
		{
			name: "partition column missing",
			ddl: `CREATE TABLE t (id bigint, embedding vector(3, half), PRIMARY KEY (id))
				CREATE VECTOR INDEX bad USING HNSW ON t(embedding) PARTITION BY (nope)`,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := buildSchemaTemplateFromDDL(tc.ddl); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func assertOpt(t *testing.T, idx *recordlayer.Index, key, want string) {
	t.Helper()
	if got := idx.Options[key]; got != want {
		t.Errorf("option %q = %q, want %q", key, got, want)
	}
}

// TestVectorDDL_SPFresh drives CREATE VECTOR INDEX … USING SPFRESH through
// the full DDL parse path (RFC-094 094.6): the resulting metadata index is an
// SPFresh index with the metric routed to the SPFresh option namespace and
// the dimension count derived from the column's VECTOR type.
func TestVectorDDL_SPFresh(t *testing.T) {
	t.Parallel()
	ddl := `CREATE TABLE documents (
			doc_id string,
			embedding vector(3, half),
			PRIMARY KEY (doc_id))
		CREATE VECTOR INDEX doc_spf USING SPFRESH ON documents(embedding)
			OPTIONS (METRIC = COSINE_METRIC)`

	tmpl, err := buildSchemaTemplateFromDDL(ddl)
	if err != nil {
		t.Fatalf("buildSchemaTemplateFromDDL: %v", err)
	}
	idx := tmpl.Underlying().GetIndex("DOC_SPF")
	if idx == nil {
		t.Fatal("vector index DOC_SPF not found in metadata")
	}
	if idx.Type != recordlayer.IndexTypeVectorSPFresh {
		t.Errorf("index type = %q, want %q", idx.Type, recordlayer.IndexTypeVectorSPFresh)
	}
	if got := idx.Options[recordlayer.IndexOptionSPFreshNumDimensions]; got != "3" {
		t.Errorf("spfreshNumDimensions = %q, want 3 (derived from vector(3, half))", got)
	}
	if got := idx.Options[recordlayer.IndexOptionSPFreshMetric]; got != "COSINE_METRIC" {
		t.Errorf("spfreshMetric = %q, want COSINE_METRIC (METRIC must route to the SPFresh namespace)", got)
	}
	if _, has := idx.Options[recordlayer.IndexOptionVectorMetric]; has {
		t.Error("hnswMetric set on an SPFresh index — option namespace leak")
	}
}

// TestVectorDDL_SPFreshErrors pins the rejection shapes: PARTITION BY and
// HNSW-only options error loudly at DDL time instead of being silently
// dropped or misapplied.
func TestVectorDDL_SPFreshErrors(t *testing.T) {
	t.Parallel()
	for name, ddl := range map[string]string{
		"partition by": `CREATE TABLE documents (
				zone string, doc_id string, embedding vector(3, half),
				PRIMARY KEY (zone, doc_id))
			CREATE VECTOR INDEX doc_spf USING SPFRESH ON documents(embedding)
				PARTITION BY (zone)`,
		"hnsw-only option": `CREATE TABLE documents (
				doc_id string, embedding vector(3, half),
				PRIMARY KEY (doc_id))
			CREATE VECTOR INDEX doc_spf USING SPFRESH ON documents(embedding)
				OPTIONS (EF_CONSTRUCTION = 100)`,
	} {
		if _, err := buildSchemaTemplateFromDDL(ddl); err == nil {
			t.Errorf("%s: DDL accepted, want loud rejection", name)
		}
	}
}

// TestSPFreshCandidateGate_PartitionedMetadata: the planner's candidate gate
// must refuse a partitioned SPFresh index even when the metadata was
// constructed DIRECTLY (bypassing the DDL and schema-builder rejections) —
// the maintainer cannot execute grouped scans, and an unexecutable candidate
// is worse than no candidate (Graefe merge-HEAD re-review).
func TestSPFreshCandidateGate_PartitionedMetadata(t *testing.T) {
	t.Parallel()
	opts := map[string]string{
		recordlayer.IndexOptionSPFreshNumDimensions: "3",
		recordlayer.IndexOptionSPFreshMetric:        "EUCLIDEAN_METRIC",
	}

	partitioned := recordlayer.NewIndex("V_PART", recordlayer.KeyWithValue(
		recordlayer.Concat(recordlayer.Field("tenant"), recordlayer.Field("embedding")), 1))
	partitioned.Type = recordlayer.IndexTypeVectorSPFresh
	partitioned.Options = opts
	if c := tryVectorIndexCandidate(partitioned, nil); c != nil {
		t.Fatalf("partitioned SPFresh metadata must yield NO planner candidate, got %v", c)
	}

	// Control: the unpartitioned twin (through the schema builder, which
	// supplies real record-type metadata) yields a candidate.
	b := metadata.NewSchemaTemplateBuilder().SetName("vt")
	b.AddTable("DOCS", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("EMBEDDING", api.NewVectorType(64, 3, true), 2),
	}, []string{"ID"})
	b.AddVectorIndexUsing("SPFRESH", "DOCS", "V_OK", "EMBEDDING", nil,
		map[string]string{recordlayer.IndexOptionSPFreshMetric: "EUCLIDEAN_METRIC"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	idx := md.GetIndex("V_OK")
	if idx == nil {
		t.Fatal("index V_OK not in metadata")
	}
	if c := tryVectorIndexCandidate(idx, md); c == nil {
		t.Fatal("unpartitioned SPFresh index must yield a planner candidate")
	}
}
