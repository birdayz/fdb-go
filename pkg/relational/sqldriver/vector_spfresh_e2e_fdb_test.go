package sqldriver_test

import (
	"context"
	"reflect"
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
		// arrive [1 2] exactly.
		if ids[0] != 1 || ids[1] != 2 {
			t.Errorf("K-NN ids = %v, want [1 2] IN DISTANCE ORDER (nearest to (0.9,0.1,0.0))", ids)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
}

// TestFDB_VectorSearch_ResidualBugPin is the RFC-156 Phase B wrong-answer
// regression (§1). An un-partitioned SPFresh vector index + a NON-partition
// residual (CATEGORY='target') with QUALIFY ROW_NUMBER() OVER (ORDER BY
// distance) <= K. The corpus is built so the GLOBAL top-K nearest rows are
// DECOYS that FAIL the predicate, while the true K-nearest MATCHING rows sit
// just behind them within the scan's re-ranked horizon:
//
//	query q = (1,0,0), K = 2
//	decoys  (CATEGORY='other') : id 1 (1,0,0) d²=0 · id 2 (0.99,0.01,0) d²≈0
//	matches (CATEGORY='target'): id 10 (0.9,0.1,0) d²=0.02 · id 11 (0.8,0.2,0)
//	                              d²=0.08 · id 12 (0.7,0.3,0) d²=0.18
//
// Correct answer = the 2 nearest MATCHING rows in distance order: [10, 11].
//
// On Phase A HEAD this is the latent bug: k is sunk BELOW the residual, so the
// scan returns the global top-2 {1,2} (decoys) and the residual filters them
// out → ∅ (or the query fails to plan, since a residual over a self-limiting
// vector scan is rejected). Phase B plans Limit(2) → Filter(CATEGORY) →
// VectorIndexScan(ordered): the ordered scan streams its re-ranked horizon, the
// Filter culls the decoys, and the Limit takes the true 2 nearest matching.
func TestFDB_VectorSearch_ResidualBugPin(t *testing.T) {
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

	// DOCS(ID, CATEGORY, EMBEDDING vector(3)) — un-partitioned SPFresh index on
	// EMBEDDING; CATEGORY is a plain (un-indexed) residual column.
	b := metadata.NewSchemaTemplateBuilder().SetName("vt")
	b.AddTable("DOCS", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("CATEGORY", api.NewStringType(false), 2),
		metadata.NewColumnSpec("EMBEDDING", api.NewVectorType(64, 3, true), 3),
	}, []string{"ID"})
	b.AddVectorIndexUsing("SPFRESH", "DOCS", "VEC_IDX", "EMBEDDING", nil,
		map[string]string{recordlayer.IndexOptionSPFreshMetric: "EUCLIDEAN_METRIC"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	desc := md.GetRecordType("DOCS").Descriptor

	makeRec := func(id int64, category string, vec []float64) proto.Message {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(desc.Fields().ByName("CATEGORY"), protoreflect.ValueOfString(category))
		m.Set(desc.Fields().ByName("EMBEDDING"), protoreflect.ValueOfBytes(recordlayer.SerializeVector(vec)))
		return m
	}
	type rec struct {
		id       int64
		category string
		vec      []float64
	}
	corpus := []rec{
		{1, "other", []float64{1, 0, 0}},       // global nearest — DECOY (fails predicate)
		{2, "other", []float64{0.99, 0.01, 0}}, // 2nd nearest    — DECOY
		{10, "target", []float64{0.9, 0.1, 0}}, // nearest MATCH
		{11, "target", []float64{0.8, 0.2, 0}}, // 2nd nearest MATCH
		{12, "target", []float64{0.7, 0.3, 0}}, // 3rd nearest MATCH (excluded by K=2)
	}
	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		for _, r := range corpus {
			if _, e := store.SaveRecord(makeRec(r.id, r.category, r.vec)); e != nil {
				return nil, e
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	sql := `SELECT id FROM docs WHERE category = 'target'
		QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])) <= 2`
	plan, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
	if err != nil {
		t.Fatalf("plan (RFC-156 Phase B should make this plannable): %v", err)
	}
	exp := plan.Explain()
	// Canonical Phase B shape: Limit ABOVE Filter ABOVE an ORDERED vector scan;
	// k is NOT sunk into the scan (no rank<).
	if !strings.Contains(exp, "VectorIndexScan") || !strings.Contains(exp, "ordered") {
		t.Fatalf("expected an ordered VectorIndexScan; explain:\n%s", exp)
	}
	if strings.Contains(exp, "rank<") {
		t.Fatalf("k was sunk into the scan instead of a Limit above the filter:\n%s", exp)
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
		ids := make([]int64, 0, len(results))
		for _, r := range results {
			ids = append(ids, r.Datum.(map[string]any)["ID"].(int64))
		}
		// The true 2 nearest MATCHING rows in distance order — decoys 1 & 2
		// (nearer, CATEGORY='other') excluded; id 12 (farther) excluded by K=2.
		want := []int64{10, 11}
		if !reflect.DeepEqual(ids, want) {
			t.Errorf("residual K-NN ids = %v, want %v (true 2 nearest CATEGORY='target', "+
				"decoys excluded, in distance order — the §1 wrong-answer bug)", ids, want)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
}

// TestFDB_VectorSearch_RarePredicateWidening is the RFC-156 Phase C end-to-end
// proof that the demand-driven ordered-stream scan fixes the residual
// under-return when the matching rows lie BEYOND the fixed Phase-B horizon. The
// corpus is 250 decoy rows (CATEGORY='other') tightly clustered at the origin
// plus 12 matching rows (CATEGORY='target') in a FAR cluster at strictly
// increasing distance:
//
//   - ALL 250 globally-nearest rows are decoys, so the 3 nearest matches sit at
//     global distance ranks ≥ 251 — far beyond Phase B's fixed re-rank horizon
//     (c≈200). Phase B materialized only the top-~200 by distance, so the
//     residual culled every survivor → the §1 under-return to ∅.
//   - Phase C's streaming scan re-ranks its whole scanned horizon and widens on
//     demand as the Filter→Limit above pulls, so it returns the true 3 nearest
//     CATEGORY='target' rows. (The batched-widening mechanic itself is pinned
//     deterministically by the bulk-build white-box spec
//     "SPFresh ordered-stream cursor (RFC-156 Phase C)"; the insert-path topology
//     here may reach the matches by re-ranking the full probe horizon instead —
//     either way the fixed-200 under-return is fixed.)
func TestFDB_VectorSearch_RarePredicateWidening(t *testing.T) {
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

	b := metadata.NewSchemaTemplateBuilder().SetName("vt")
	b.AddTable("DOCS", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("CATEGORY", api.NewStringType(false), 2),
		metadata.NewColumnSpec("EMBEDDING", api.NewVectorType(64, 3, true), 3),
	}, []string{"ID"})
	b.AddVectorIndexUsing("SPFRESH", "DOCS", "VEC_IDX", "EMBEDDING", nil,
		map[string]string{recordlayer.IndexOptionSPFreshMetric: "EUCLIDEAN_METRIC"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	desc := md.GetRecordType("DOCS").Descriptor

	makeRec := func(id int64, category string, vec []float64) proto.Message {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(desc.Fields().ByName("CATEGORY"), protoreflect.ValueOfString(category))
		m.Set(desc.Fields().ByName("EMBEDDING"), protoreflect.ValueOfBytes(recordlayer.SerializeVector(vec)))
		return m
	}
	// 250 'other' decoys near the origin (ids 1..250) + 12 'target' matches in a
	// far cluster (ids 1001..1012) at increasing distance — so id 1001/1002/1003
	// are the 3 nearest matches.
	type rec struct {
		id  int64
		cat string
		vec []float64
	}
	var corpus []rec
	for i := 0; i < 250; i++ {
		corpus = append(corpus, rec{
			int64(i + 1), "other",
			[]float64{float64(i%16) * 0.01, float64(i/16) * 0.01, 0},
		})
	}
	for i := 1; i <= 12; i++ {
		corpus = append(corpus, rec{int64(1000 + i), "target", []float64{50 + float64(i)*2, 0, 0}})
	}
	for start := 0; start < len(corpus); start += 100 {
		batch := corpus[start:min(start+100, len(corpus))]
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, sErr := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			if sErr != nil {
				return nil, sErr
			}
			for _, r := range batch {
				if _, e := store.SaveRecord(makeRec(r.id, r.cat, r.vec)); e != nil {
					return nil, e
				}
			}
			return nil, nil
		})
		if err != nil {
			t.Fatalf("setup batch @%d: %v", start, err)
		}
	}

	sql := `SELECT id FROM docs WHERE category = 'target'
		QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(embedding, [0.0, 0.0, 0.0])) <= 3`
	plan, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if exp := plan.Explain(); !strings.Contains(exp, "VectorIndexScan") || !strings.Contains(exp, "ordered") {
		t.Fatalf("expected an ordered VectorIndexScan; explain:\n%s", exp)
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
		ids := make([]int64, 0, len(results))
		for _, r := range results {
			ids = append(ids, r.Datum.(map[string]any)["ID"].(int64))
		}
		// The 3 nearest CATEGORY='target' rows in distance order. All 250 decoys
		// are nearer, so a fixed-200 horizon (Phase B) culls every survivor → ∅;
		// Phase C's streaming scan returns the true matches.
		want := []int64{1001, 1002, 1003}
		if !reflect.DeepEqual(ids, want) {
			t.Errorf("rare-predicate K-NN ids = %v, want %v (true 3 nearest CATEGORY='target' "+
				"beyond the fixed-200 horizon — the §1 under-return, fixed by Phase C)", ids, want)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
}
