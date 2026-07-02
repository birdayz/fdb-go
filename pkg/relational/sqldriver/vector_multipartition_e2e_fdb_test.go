package sqldriver_test

import (
	"context"
	"reflect"
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

// multiPartitionVectorSetup builds a DOCS(ZONE, REGION, ID, EMBEDDING vector(3))
// schema with a 3-d HNSW index partitioned by (ZONE, REGION) — the two-column
// partition that exercises the RFC-046 multi-partition fan-out — and inserts a
// fixed corpus into three partitions:
//
//	(z1, r1): id 11 = (1,0,0)     id 12 = (0.8,0.2,0)
//	(z1, r2): id 21 = (0,1,0)     id 22 = (0.2,0.9,0)
//	(z2, r1): id 31 = (1,0,0)     <- decoy, must be excluded by WHERE zone='z1'
//
// For query vector (1,0,0) the per-partition nearest are: (z1,r1)->11, (z1,r2)->22.
func multiPartitionVectorSetup(t *testing.T, ctx context.Context) (*recordlayer.FDBDatabase, *recordlayer.RecordMetaData, subspace.Subspace) {
	t.Helper()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	b := metadata.NewSchemaTemplateBuilder().SetName("vt")
	b.AddTable("DOCS", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ZONE", api.NewStringType(false), 1),
		metadata.NewColumnSpec("REGION", api.NewStringType(false), 2),
		metadata.NewColumnSpec("ID", api.NewLongType(false), 3),
		metadata.NewColumnSpec("EMBEDDING", api.NewVectorType(64, 3, true), 4),
	}, []string{"ZONE", "REGION", "ID"})
	b.AddVectorIndex("DOCS", "VEC_IDX", "EMBEDDING", []string{"ZONE", "REGION"},
		map[string]string{recordlayer.IndexOptionVectorMetric: "EUCLIDEAN_METRIC"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	desc := md.GetRecordType("DOCS").Descriptor

	makeRec := func(zone, region string, id int64, vec []float64) proto.Message {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName("ZONE"), protoreflect.ValueOfString(zone))
		m.Set(desc.Fields().ByName("REGION"), protoreflect.ValueOfString(region))
		m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(desc.Fields().ByName("EMBEDDING"), protoreflect.ValueOfBytes(recordlayer.SerializeVector(vec)))
		return m
	}
	type rec struct {
		zone, region string
		id           int64
		vec          []float64
	}
	corpus := []rec{
		{"z1", "r1", 11, []float64{1, 0, 0}},
		{"z1", "r1", 12, []float64{0.8, 0.2, 0}},
		{"z1", "r2", 21, []float64{0, 1, 0}},
		{"z1", "r2", 22, []float64{0.2, 0.9, 0}},
		{"z2", "r1", 31, []float64{1, 0, 0}},
	}
	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		for _, r := range corpus {
			if _, e := store.SaveRecord(makeRec(r.zone, r.region, r.id, r.vec)); e != nil {
				return nil, e
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	return db, md, ks
}

// TestFDB_VectorSearch_MultiPartition_Fanout is the RFC-046 (9.5) end-to-end
// proof: a partial partition prefix (only ZONE bound, REGION fanned out) plans
// to a BY_DISTANCE vector scan and executes as a multi-partition K-NN — one
// HNSW search per (z1, *) partition, top-K PER partition. With <= 1 the result
// is the union of each z1 region's nearest: id 11 (from r1) and id 22 (from r2),
// excluding the z2 decoy. A single-partition (or global-top-1) implementation
// would return only id 11 — so two rows across two regions is the load-bearing
// assertion.
func TestFDB_VectorSearch_MultiPartition_Fanout(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db, md, ks := multiPartitionVectorSetup(t, ctx)

	sql := `SELECT id, region FROM docs WHERE zone = 'z1'
		QUALIFY ROW_NUMBER() OVER (PARTITION BY zone, region
			ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])) <= 1`

	exp, got := planExplainAndRun(t, ctx, db, md, ks, sql)
	if !strings.Contains(exp, "VectorIndexScan") {
		t.Fatalf("query did not plan to a vector scan:\n%s", exp)
	}
	if !strings.Contains(exp, "prefix=[=, *]") {
		t.Fatalf("expected a partial prefix [=, *] (region fanned out):\n%s", exp)
	}
	want := []idRegion{{11, "r1"}, {22, "r2"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("multi-partition K-NN = %v, want %v (per-partition top-1 across both z1 regions, excluding z2)", got, want)
	}
}

// TestFDB_VectorSearch_MultiPartition_InequalityResidual pins the
// RFC-046 review condition end-to-end: a partition-column INEQUALITY
// (region > 'r1') is NOT consumed into the scan prefix (the executor encodes
// only an equality prefix tuple); it is enforced as a residual filter above the
// fanned-out scan. The query must therefore exclude r1 and return only r2's
// nearest (id 22). If the inequality were silently dropped, both regions would
// appear (ids 11 and 22) — so a single row {22} is the proof the inequality is
// honored.
func TestFDB_VectorSearch_MultiPartition_InequalityResidual(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db, md, ks := multiPartitionVectorSetup(t, ctx)

	sql := `SELECT id, region FROM docs WHERE zone = 'z1' AND region > 'r1'
		QUALIFY ROW_NUMBER() OVER (PARTITION BY zone, region
			ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])) <= 1`

	exp, got := planExplainAndRun(t, ctx, db, md, ks, sql)
	if !strings.Contains(exp, "VectorIndexScan") {
		t.Fatalf("query did not plan to a vector scan:\n%s", exp)
	}
	want := []idRegion{{22, "r2"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("partition-inequality K-NN = %v, want %v (region>'r1' excludes r1; inequality enforced as residual, not dropped)", got, want)
	}
}

// TestFDB_VectorSearch_MultiPartition_InequalityResidualK2 pins the K>1
// partition-inequality wrong-rows bug (found during RFC-167 Phase 4 review):
// a partition-key inequality (region > 'r1') over a partitioned vector index
// with k=2 per partition. The ONLY residual-bearing plan the old planner could
// produce was Intersection(VectorTopK(rank<=2), PrimaryRange(region>'r1')) keyed
// on the primary key — but the multi-partition vector cursor delivers
// (region, distance) order, NOT pk order, so feeding it into the pk-keyed
// sorted-merge DROPS rows whose distance rank disagrees with their pk order.
//
// In partition (z1,r2) the two rows for query vector (1,0,0) are id 21=(0,1,0)
// (dist √2≈1.414) and id 22=(0.2,0.9,0) (dist √1.45≈1.204): the vector cursor
// emits 22 then 21 (distance order) while the pk-range emits 21 then 22 — so the
// max-key/advance merge advances past 21 and returns only {22}. The correct
// answer is BOTH rows {21, 22} (top-2 of the single surviving region r2).
//
// The fix (RFC-167 Phase 4): the pk-order gate drops the invalid vector
// intersection, and the compensationSafeForYield partition-residual exception
// yields the correct Filter(region>'r1') → VectorScan(self-limiting per-partition
// top-k) plan. The Filter selects the whole r2 partition and never disturbs its
// per-partition top-2, so both rows survive. The plan MUST be the self-limiting
// PredicatesFilter-over-VectorScan shape (rank<=2), NOT an ordered scan and NOT
// an intersection (assert the self-limiting shape).
func TestFDB_VectorSearch_MultiPartition_InequalityResidualK2(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db, md, ks := multiPartitionVectorSetup(t, ctx)

	sql := `SELECT id, region FROM docs WHERE zone = 'z1' AND region > 'r1'
		QUALIFY ROW_NUMBER() OVER (PARTITION BY zone, region
			ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])) <= 2`

	exp, got := planExplainAndRun(t, ctx, db, md, ks, sql)
	if !strings.Contains(exp, "VectorIndexScan") {
		t.Fatalf("query did not plan to a vector scan:\n%s", exp)
	}
	// Self-limiting Filter-over-scan shape, NOT the pk-keyed intersection (the
	// wrong-rows shape) and NOT an ordered scan (which cannot express per-
	// partition top-k).
	if strings.Contains(exp, "Intersection") {
		t.Fatalf("k>1 partition-inequality query planned to a pk-keyed Intersection (drops rows):\n%s", exp)
	}
	if !strings.Contains(exp, "PredicatesFilter") || !strings.Contains(exp, "rank<=") {
		t.Fatalf("expected Filter(region>'r1') → VectorScan(self-limiting rank<=2), got:\n%s", exp)
	}
	want := []idRegion{{21, "r2"}, {22, "r2"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("k>1 partition-inequality K-NN = %v, want %v (both top-2 of r2; the distance-vs-pk order disagreement must NOT drop id 21)", got, want)
	}
}

// TestFDB_VectorSearch_MultiPartition_Pagination pins the cross-partition
// continuation — the whole risk of the fan-out. Driving the multi-partition scan
// page-by-page with a returned-row-limit of 1 must yield, by concatenation, the
// exact same sequence as an unpaged scan — proving the FlatMapContinuation
// resume seeds the saved partition's inner position and then advances to the
// next distinct partition without dropping or repeating rows. Exercised at the
// maintainer level (store.ScanVectorIndexWithPrefix with a partial prefix), the
// direct entry point into vectorMultiPartitionCursor.
func TestFDB_VectorSearch_MultiPartition_Pagination(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db, md, ks := multiPartitionVectorSetup(t, ctx)

	q := []float64{1, 0, 0}
	partial := tuple.Tuple{"z1"} // only ZONE bound; REGION fanned out.
	const k, ef = 1, 64

	_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
		if sErr != nil {
			return nil, sErr
		}
		idx := store.GetMetaData().GetIndex("VEC_IDX")

		// Unpaged: a single scan, no row limit.
		unpaged := collectVectorPKs(t, ctx, store, idx, partial, q, k, ef, nil, 0)

		// Paged: returned-row-limit 1, resume via continuation. collectVectorPage
		// returns (pageRows, nextContinuation); a nil continuation means done.
		var paged []string
		var cont []byte
		for page := 0; page < 100; page++ {
			rows, c := collectVectorPage(t, ctx, store, idx, partial, q, k, ef, cont)
			paged = append(paged, rows...)
			// A page that delivered a row MUST carry a non-nil resume token until
			// the stream truly ends — a nil continuation on a delivered row reads
			// downstream as end-of-scan and would silently truncate the result.
			// Pins wrapContinuation against the silent-nil failure (@claude F3).
			if len(rows) > 0 && c == nil && len(paged) < len(unpaged) {
				t.Fatalf("page %d delivered a row but returned a nil continuation with %d/%d rows seen (silent truncation)", page, len(paged), len(unpaged))
			}
			if c == nil {
				break
			}
			cont = c
		}
		if !reflect.DeepEqual(paged, unpaged) {
			t.Errorf("paginated multi-partition scan = %v, want %v (== unpaged)", paged, unpaged)
		}
		if len(unpaged) != 2 {
			t.Errorf("unpaged multi-partition scan returned %d rows, want 2 (top-1 per z1 region)", len(unpaged))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("pagination test: %v", err)
	}
}

// TestFDB_VectorSearch_MultiPartition_DimensionValidation: the
// partial-prefix fan-out path must validate the query-vector dimension UP FRONT,
// before any partition is matched — so an invalid-length vector errors
// consistently even when the partial prefix matches NO partitions (the
// empty-range case). Without the up-front check the cursor returns
// SourceExhausted (zero rows, no error), unlike the full-prefix and
// unpartitioned paths which validate before touching graph contents.
func TestFDB_VectorSearch_MultiPartition_DimensionValidation(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db, md, ks := multiPartitionVectorSetup(t, ctx)

	// Capture the cursor error in an outer var — db.Run's own error is the
	// transaction result, not the per-row scan error we want to assert on
	// (the closure must not return the cursor error as the `any` result).
	var scanErr error
	_, runErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
		if sErr != nil {
			return nil, sErr
		}
		idx := store.GetMetaData().GetIndex("VEC_IDX")
		// 2-d query against a 3-d index, partial prefix "zX" matching NO partition
		// (the corpus only has z1/z2) — the empty-range fan-out case.
		badVec := []float64{1, 0}
		cur := store.ScanVectorIndexWithPrefix(idx, tuple.Tuple{"zX"}, badVec, 1, 64, nil,
			recordlayer.ScanProperties{ExecuteProperties: recordlayer.DefaultExecuteProperties(), CursorStreamingMode: recordlayer.StreamingModeIterator})
		defer cur.Close()
		_, scanErr = cur.OnNext(ctx)
		return nil, nil
	})
	if runErr != nil {
		t.Fatalf("run: %v", runErr)
	}
	if scanErr == nil {
		t.Fatal("partial-prefix scan with a wrong-dimension query vector over an empty partition range did not error (codex P2 regression)")
	}
	if !strings.Contains(scanErr.Error(), "dimension") {
		t.Fatalf("expected a dimension error, got: %v", scanErr)
	}
}

// TestFDB_VectorSearch_MultiPartition_TrailingEqualityResidual addresses a
// suspected wrong-rows path — which does NOT reproduce. Scenario: a partition
// equality on a NON-leading column with the LEADING column unbound
// (`WHERE region = 'r1'`, zone unbound) over a `PARTITION BY (zone, region)`
// vector index. ComputeBoundParameterPrefixMap consumes the contiguous leading
// EQUALITY run and stops at the first unbound column (zone) — so region='r1' is
// not consumed into the scan prefix, because a positional prefix cannot fix
// column 1 while column 0 ranges free (Java's nextPrefixTuple extracts a leading
// subTuple(key, 0, prefixSize) likewise — Java cannot express this either).
//
// Go does NOT silently drop region and return wrong rows (the suspected failure
// mode). The index-only DistanceRank, AND-combined with the unconsumed
// region='r1', cannot be lowered to a residual filter and no index serves the
// composite, so the final-plan guard rejects it with UnplannableIndexOnly-
// ResidualError. That is a safe outcome (a clean planning error, never wrong
// results), it matches Java (equally unserviceable; Go's typed error beats
// Java's Verify blow-up), and it was unplannable before this change too (no
// regression). Assert UNPLANNABLE; a Go-only trailing-partition
// fan-out (plan + residual) is an allowed but out-of-scope follow-up. Pinning
// the error is the regression sentinel proving the absence of the wrong-rows
// behavior. Plan-only — the query never reaches execution.
func TestFDB_VectorSearch_MultiPartition_TrailingEqualityResidual(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	_, md, _ := multiPartitionVectorSetup(t, ctx)

	sql := `SELECT id, region FROM docs WHERE region = 'r1'
		QUALIFY ROW_NUMBER() OVER (PARTITION BY zone, region
			ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])) <= 1`

	_, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
	if err == nil {
		t.Fatal("trailing-equality vector query (leading partition column unbound) unexpectedly planned; " +
			"expected unplannable (the unconsumed partition equality + index-only DistanceRank cannot be a residual)")
	}
	if !strings.Contains(err.Error(), "not plannable") && !strings.Contains(err.Error(), "index-only") {
		t.Fatalf("expected an unplannable / index-only planning error, got: %v", err)
	}
}

// TestFDB_VectorSearch_MultiPartition_LeadingInequalityResidual pins the
// boundLen-0 admit path of residualIsPartitionContiguous: a
// LEADING partition-column inequality (WHERE zone > 'z1') binds no equality
// prefix, so the scan fans out over ALL partitions and the whole-partition
// Filter(zone>'z1') selects those with zone>'z1', preserving each surviving
// partition's per-partition top-k. residualIsPartitionContiguous admits it
// (residual {zone} at index 0 == boundLen 0), distinct from the rejected
// leading-column-GAP case (region='r1' with zone unbound). Only z2 has
// zone>'z1', so the result is that partition's top-2 = {31}; if the inequality
// were dropped, z1's rows would appear too — so {31} alone proves it is honored,
// and the shape assertion proves it planned to the self-limiting Filter form
// (not an intersection, not unplannable).
func TestFDB_VectorSearch_MultiPartition_LeadingInequalityResidual(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db, md, ks := multiPartitionVectorSetup(t, ctx)

	sql := `SELECT id, region FROM docs WHERE zone > 'z1'
		QUALIFY ROW_NUMBER() OVER (PARTITION BY zone, region
			ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])) <= 2`

	exp, got := planExplainAndRun(t, ctx, db, md, ks, sql)
	if !strings.Contains(exp, "VectorIndexScan") {
		t.Fatalf("query did not plan to a vector scan:\n%s", exp)
	}
	if strings.Contains(exp, "Intersection") {
		t.Fatalf("leading-inequality query planned to an Intersection:\n%s", exp)
	}
	if !strings.Contains(exp, "PredicatesFilter") || !strings.Contains(exp, "rank<=") {
		t.Fatalf("expected Filter(zone>'z1') → VectorScan(self-limiting rank<=2), got:\n%s", exp)
	}
	want := []idRegion{{31, "r1"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("leading-inequality K-NN = %v, want %v (zone>'z1' keeps only z2's (z2,r1) top-2 = {31})", got, want)
	}
}

type idRegion struct {
	id     int64
	region string
}

// planExplainAndRun plans the SQL query against md, executes it over a store
// opened on ks, and returns the plan's EXPLAIN string plus the (ID, REGION)
// rows sorted by ID for a deterministic comparison.
func planExplainAndRun(t *testing.T, ctx context.Context, db *recordlayer.FDBDatabase, md *recordlayer.RecordMetaData, ks subspace.Subspace, sql string) (string, []idRegion) {
	t.Helper()
	plan, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	exp := plan.Explain()

	var out []idRegion
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
		for _, r := range results {
			row := r.Datum.(map[string]any)
			out = append(out, idRegion{row["ID"].(int64), row["REGION"].(string)})
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return exp, out
}

// collectVectorPKs drains a single multi-partition vector scan to completion
// (optional row limit) and returns the primary keys as strings.
func collectVectorPKs(t *testing.T, ctx context.Context, store *recordlayer.FDBRecordStore, idx *recordlayer.Index, prefix tuple.Tuple, q []float64, k, ef int, cont []byte, limit int) []string {
	t.Helper()
	props := recordlayer.DefaultExecuteProperties()
	if limit > 0 {
		props = props.WithReturnedRowLimit(limit)
	}
	cur := store.ScanVectorIndexWithPrefix(idx, prefix, q, k, ef, cont,
		recordlayer.ScanProperties{ExecuteProperties: props, CursorStreamingMode: recordlayer.StreamingModeIterator})
	defer cur.Close()
	var pks []string
	for {
		res, err := cur.OnNext(ctx)
		if err != nil {
			t.Fatalf("scan OnNext: %v", err)
		}
		if !res.HasNext() {
			return pks
		}
		pks = append(pks, res.GetValue().PrimaryKey().String())
	}
}

// collectVectorPage runs one page (returned-row-limit 1) of a multi-partition
// vector scan and returns the page's PKs and the continuation for the next page
// (nil when the scan is exhausted).
func collectVectorPage(t *testing.T, ctx context.Context, store *recordlayer.FDBRecordStore, idx *recordlayer.Index, prefix tuple.Tuple, q []float64, k, ef int, cont []byte) ([]string, []byte) {
	t.Helper()
	props := recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(1)
	cur := store.ScanVectorIndexWithPrefix(idx, prefix, q, k, ef, cont,
		recordlayer.ScanProperties{ExecuteProperties: props, CursorStreamingMode: recordlayer.StreamingModeIterator})
	defer cur.Close()
	var pks []string
	for {
		res, err := cur.OnNext(ctx)
		if err != nil {
			t.Fatalf("page OnNext: %v", err)
		}
		if res.HasNext() {
			pks = append(pks, res.GetValue().PrimaryKey().String())
			continue
		}
		c := res.GetContinuation()
		if c == nil || c.IsEnd() {
			return pks, nil
		}
		b, err := c.ToBytes()
		if err != nil {
			t.Fatalf("continuation ToBytes: %v", err)
		}
		if len(b) == 0 {
			return pks, nil
		}
		return pks, b
	}
}
