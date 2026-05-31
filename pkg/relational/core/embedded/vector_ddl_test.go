package embedded

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
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
