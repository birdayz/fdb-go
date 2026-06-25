package sqldriver_test

import (
	"context"
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

// TestFDB_VectorSearch_SPFreshE2E is the RFC-094 094.6 end-to-end proof: a
// K-NN SQL query against a USING SPFRESH index plans to a BY_DISTANCE vector
// index scan (the OPTIMIZATION fires — pinned via Explain) and returns the k
// nearest records, with the records inserted through plain SaveRecord — the
// §6b cold-start path, no bulk build anywhere.
func TestFDB_VectorSearch_SPFreshE2E(t *testing.T) {
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

	// Schema: DOCS(ID, EMBEDDING vector(3)) with an UNPARTITIONED 3-d SPFresh
	// index (SPFresh rejects PARTITION BY).
	b := metadata.NewSchemaTemplateBuilder().SetName("vt")
	b.AddTable("DOCS", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("EMBEDDING", api.NewVectorType(64, 3, true), 2),
	}, []string{"ID"})
	b.AddVectorIndexUsing("SPFRESH", "DOCS", "VEC_IDX", "EMBEDDING", nil,
		map[string]string{recordlayer.IndexOptionSPFreshMetric: "EUCLIDEAN_METRIC"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	desc := md.GetRecordType("DOCS").Descriptor

	makeRec := func(id int64, vec []float64) proto.Message {
		m := dynamicpb.NewMessage(desc)
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
		t.Fatalf("setup (cold-start inserts): %v", err)
	}

	sql := `SELECT id FROM docs
		QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(embedding, [0.9, 0.1, 0.0])) <= 2`
	plan, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if exp := plan.Explain(); !strings.Contains(exp, "VectorIndexScan") {
		t.Fatalf("USING SPFRESH query did not plan to a vector scan:\n%s", exp)
	}

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
			t.Fatalf("SPFresh K-NN returned %d rows, want 2", len(results))
		}
		ids := make([]int64, 0, len(results))
		for _, r := range results {
			ids = append(ids, r.Datum.(map[string]any)["ID"].(int64))
		}
		// UNSORTED on purpose: BY_DISTANCE output order is part of the
		// contract — d²(1)=0.02 < d²(2)=1.62 at this query, so the rows must
		// arrive [1 2] exactly (Graefe merge-HEAD F3).
		if ids[0] != 1 || ids[1] != 2 {
			t.Errorf("K-NN ids = %v, want [1 2] IN DISTANCE ORDER (nearest to (0.9,0.1,0.0))", ids)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
}
