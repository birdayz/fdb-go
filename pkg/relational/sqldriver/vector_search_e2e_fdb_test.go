package sqldriver_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/executor"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/embedded"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/metadata"
)

// TestFDB_VectorSearch_QualifyE2E is the 9.4 end-to-end proof: a full vector
// K-NN SQL query — SELECT … QUALIFY ROW_NUMBER() OVER (ORDER BY
// euclidean_distance(vec, q)) <= K — is planned to a BY_DISTANCE vector index
// scan and executed against real FDB, returning the k nearest records by
// distance. Bridges 9.3a/b (SQL → vector plan) and 9.3c (plan → KNN execution).
func TestFDB_VectorSearch_QualifyE2E(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	// Schema: DOCS(ZONE, ID, EMBEDDING vector(3)) PK (ZONE, ID) with a 3-d HNSW
	// index on EMBEDDING partitioned by ZONE (the Java vector-search shape).
	// Uppercase names match SQL identifier normalization.
	b := metadata.NewSchemaTemplateBuilder().SetName("vt")
	b.AddTable("DOCS", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ZONE", api.NewStringType(false), 1),
		metadata.NewColumnSpec("ID", api.NewLongType(false), 2),
		metadata.NewColumnSpec("EMBEDDING", api.NewVectorType(64, 3, true), 3),
	}, []string{"ZONE", "ID"})
	b.AddVectorIndex("DOCS", "VEC_IDX", "EMBEDDING", []string{"ZONE"},
		map[string]string{recordlayer.IndexOptionVectorMetric: "EUCLIDEAN_METRIC"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	desc := md.GetRecordType("DOCS").Descriptor

	// Insert 3 records (all in zone 'z1') with 3-d basis vectors.
	makeRec := func(id int64, vec []float64) proto.Message {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName("ZONE"), protoreflect.ValueOfString("z1"))
		m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(desc.Fields().ByName("EMBEDDING"), protoreflect.ValueOfBytes(recordlayer.SerializeVector(vec)))
		return m
	}
	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		for id, v := range map[int64][]float64{1: {1, 0, 0}, 2: {0, 1, 0}, 3: {0, 0, 1}} {
			if _, e := store.SaveRecord(makeRec(id, v)); e != nil {
				return nil, e
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Plan the K-NN SQL query — it must lower to a BY_DISTANCE vector scan.
	sql := `SELECT id FROM docs WHERE zone = 'z1'
		QUALIFY ROW_NUMBER() OVER (PARTITION BY zone ORDER BY euclidean_distance(embedding, [0.9, 0.1, 0.0])) <= 2`
	plan, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if exp := plan.Explain(); !strings.Contains(exp, "VectorIndexScan") {
		t.Fatalf("query did not plan to a vector scan:\n%s", exp)
	}

	// Execute the plan against the store; query (0.9,0.1,0) → nearest id1, id2.
	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
		if sErr != nil {
			return nil, sErr
		}
		cursor, cErr := executor.ExecutePlan(ctx, plan, store,
			executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if cErr != nil {
			return nil, cErr
		}
		defer cursor.Close()
		results, rErr := executor.CollectAll(ctx, cursor)
		if rErr != nil {
			return nil, rErr
		}
		if len(results) != 2 {
			t.Fatalf("vector K-NN returned %d rows, want 2", len(results))
		}
		ids := make([]int64, 0, len(results))
		for _, r := range results {
			ids = append(ids, r.Datum.(map[string]any)["ID"].(int64))
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		if ids[0] != 1 || ids[1] != 2 {
			t.Errorf("K-NN ids = %v, want [1 2] (nearest to (0.9,0.1,0.0))", ids)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
}
