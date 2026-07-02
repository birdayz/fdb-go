package sqldriver_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
)

// TestFDB_VectorSearch_TighterOuterLimitRows is the row-level net for the
// divergent-cap vector shape `QUALIFY ROW_NUMBER() <= K ... LIMIT k'` with
// k' < K: exactly k' rows come back and they are the k' NEAREST, regardless
// of which plan shape wins. The planner resolves this shape to the QUALIFY's
// self-limiting rank<=K scan with the tighter Limit(k') surviving above it
// (cost-determined winner — see TestVectorPlan_TighterOuterLimitDoesNotFold);
// this test pins the semantics that make that plan legal: the self-limiting
// scan emits BY DISTANCE, so Limit(k') over rank<=K returns the k' nearest —
// never K rows (widening) and never an arbitrary k'-subset of the top K.
func TestFDB_VectorSearch_TighterOuterLimitRows(t *testing.T) {
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

	// Schema: DOCS(ID, EMBEDDING vector(3)) PK (ID), UN-partitioned HNSW index
	// (global rank — the exact shape of the planner sentinel's decline query).
	b := metadata.NewSchemaTemplateBuilder().SetName("vtl")
	b.AddTable("DOCS", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("EMBEDDING", api.NewVectorType(64, 3, true), 2),
	}, []string{"ID"})
	b.AddVectorIndex("DOCS", "VEC_IDX", "EMBEDDING", nil,
		map[string]string{recordlayer.IndexOptionVectorMetric: "EUCLIDEAN_METRIC"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	desc := md.GetRecordType("DOCS").Descriptor

	// Six docs at strictly increasing distance from the query point (1,0,0):
	// id1 d=0, id2 d=0.1, id3 d=0.2, id4 d~1.41, id5 d~1.41, id6 d=2.
	vecs := map[int64][]float64{
		1: {1, 0, 0},
		2: {0.9, 0, 0},
		3: {0.8, 0, 0},
		4: {0, 1, 0},
		5: {0, 0, 1},
		6: {-1, 0, 0},
	}
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
		for id, v := range vecs {
			if _, e := store.SaveRecord(makeRec(id, v)); e != nil {
				return nil, e
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Rank cap 4 is LOOSER than the outer LIMIT 2.
	sql := `SELECT id FROM docs
		QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])) <= 4
		LIMIT 2`
	plan, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	exp := plan.Explain()
	if !strings.Contains(exp, "VectorIndexScan") {
		t.Fatalf("query did not plan to a vector scan:\n%s", exp)
	}
	// The tighter cap survives; the QUALIFY's own cap is never widened away.
	if iLimit, iScan := strings.Index(exp, "Limit(2"), strings.Index(exp, "VectorIndexScan("); iLimit < 0 || iLimit > iScan {
		t.Fatalf("expected explicit Limit(2) ABOVE the vector scan:\n%s", exp)
	}
	if strings.Contains(exp, "Limit(4") || strings.Contains(exp, "rank<=2") {
		t.Fatalf("divergent caps mixed up (widened Limit or narrowed rank):\n%s", exp)
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
		// Exactly LIMIT 2 rows (the cap was honoured, not widened to rank 4)…
		if len(results) != 2 {
			t.Fatalf("returned %d rows, want 2 (LIMIT 2 must be honoured, not widened to the rank cap 4)", len(results))
		}
		// …and they are the 2 NEAREST (ids 1, 2), not an arbitrary pair of the
		// top-4 — pins the distance-ordered emission the plan shape relies on.
		ids := make([]int64, 0, len(results))
		for _, r := range results {
			ids = append(ids, r.Datum.(map[string]any)["ID"].(int64))
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		if ids[0] != 1 || ids[1] != 2 {
			t.Errorf("rows = %v, want [1 2] (the 2 nearest to (1,0,0))", ids)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
}
