package sqldriver_test

import (
	"context"
	"math"
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

// paramRankCorpus is the shared (un-partitioned HNSW) DOCS table + corpus for
// the RFC-156 P1 e2e tests: query (1,0,0); rows fan out along the x→y arc at
// strictly increasing distance, so the global K-NN order is id 1,2,3,4,5.
// CATEGORY splits them so a residual ('target') interleaves with non-matching
// decoys ('other').
//
//	id 1 (1.0,0.0,0)  d²=0     CATEGORY=other   (global nearest — a DECOY)
//	id 2 (0.9,0.1,0)  d²=0.02  CATEGORY=target  (nearest MATCH)
//	id 3 (0.8,0.2,0)  d²=0.08  CATEGORY=other   (DECOY)
//	id 4 (0.7,0.3,0)  d²=0.18  CATEGORY=target  (2nd MATCH)
//	id 5 (0.6,0.4,0)  d²=0.32  CATEGORY=target  (3rd MATCH)
type paramRankRow struct {
	id       int64
	category string
	vec      []float64
}

func paramRankCorpus() []paramRankRow {
	return []paramRankRow{
		{1, "other", []float64{1.0, 0.0, 0}},
		{2, "target", []float64{0.9, 0.1, 0}},
		{3, "other", []float64{0.8, 0.2, 0}},
		{4, "target", []float64{0.7, 0.3, 0}},
		{5, "target", []float64{0.6, 0.4, 0}},
	}
}

// setupParamRankStore builds an un-partitioned HNSW DOCS index and inserts the
// given corpus, returning the metadata and keyspace for execution.
func setupParamRankStore(t *testing.T, ctx context.Context, db *recordlayer.FDBDatabase, ks subspace.Subspace, corpus []paramRankRow) *recordlayer.RecordMetaData {
	t.Helper()
	b := metadata.NewSchemaTemplateBuilder().SetName("vt")
	b.AddTable("DOCS", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("CATEGORY", api.NewStringType(false), 2),
		metadata.NewColumnSpec("EMBEDDING", api.NewVectorType(64, 3, true), 3),
	}, []string{"ID"})
	// Un-partitioned HNSW (no PARTITION BY) — the global-rank path P1 fixes.
	b.AddVectorIndex("DOCS", "VEC_IDX", "EMBEDDING", nil,
		map[string]string{recordlayer.IndexOptionVectorMetric: "EUCLIDEAN_METRIC"})
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
	return md
}

// runParamRankQuery plans + executes sql against the store with the given bound
// parameters and returns the result ids in emission order.
func runParamRankQuery(t *testing.T, ctx context.Context, db *recordlayer.FDBDatabase, ks subspace.Subspace, md *recordlayer.RecordMetaData, sql string, params []any) []int64 {
	t.Helper()
	plan, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
	if err != nil {
		t.Fatalf("plan: %v\nsql=%s", err, sql)
	}
	exp := plan.Explain()
	if !strings.Contains(exp, "VectorIndexScan") {
		t.Fatalf("query did not plan to a vector scan:\n%s", exp)
	}
	// P1 regression pin (planner shape): an un-partitioned global-rank query is
	// always Limit-bounded — never an unbounded ordered scan, never a sunk rank<.
	if strings.Contains(exp, "rank<") {
		t.Fatalf("k was sunk into the scan (rank<…) instead of a Limit above it:\n%s", exp)
	}
	if i := strings.Index(exp, "ordered"); i >= 0 {
		if first := strings.Index(exp, "Limit("); first < 0 || first > i {
			t.Fatalf("ordered scan is not bounded by a Limit above it (unbounded stream):\n%s", exp)
		}
	}

	evalCtx := executor.EmptyEvaluationContext()
	if len(params) > 0 {
		evalCtx = evalCtx.WithParams(params)
	}
	var ids []int64
	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
		if sErr != nil {
			return nil, sErr
		}
		cursor, cErr := executor.ExecutePlan(ctx, plan, store, evalCtx, nil, recordlayer.DefaultExecuteProperties())
		if cErr != nil {
			return nil, cErr
		}
		defer cursor.Close()
		results, rErr := executor.CollectAll(ctx, cursor)
		if rErr != nil {
			return nil, rErr
		}
		for _, r := range results {
			ids = append(ids, r.Datum.(map[string]any)["ID"].(int64))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("execute: %v\nsql=%s", err, sql)
	}
	return ids
}

// TestFDB_VectorSearch_ZeroCapEmpty is the RFC-156 P1 codex-blocker e2e proof
// for the ZERO-cap branch: a GLOBAL-rank vector query whose adjusted cap is 0
// (ROW_NUMBER() … < 1) MUST return EMPTY. On HEAD globalRankVectorLimit DECLINED
// the zero cap (no Limit added) while the un-partitioned scan still emitted the
// ORDERED form, so the ordered executor — which ignores the scan's own k —
// streamed rows and the query wrongly returned a non-empty result. The fix
// lowers a static Limit(0) above the ordered scan → EMPTY, both with and without
// a residual WHERE.
func TestFDB_VectorSearch_ZeroCapEmpty(t *testing.T) {
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
	md := setupParamRankStore(t, ctx, db, ks, paramRankCorpus())

	cases := []struct {
		name string
		sql  string
	}{
		{
			"no residual",
			`SELECT id FROM docs
				QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])) < 1`,
		},
		{
			"with residual",
			`SELECT id FROM docs WHERE category = 'target'
				QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])) < 1`,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ids := runParamRankQuery(t, ctx, db, ks, md, tc.sql, nil)
			if len(ids) != 0 {
				t.Fatalf("ROW_NUMBER() < 1 returned %v, want EMPTY (the zero-cap codex blocker)", ids)
			}
		})
	}
}

// TestFDB_VectorSearch_NonPositiveCapEmpty pins the RFC-156 correctness-hunt fix:
// a GLOBAL-rank vector K-NN whose ADJUSTED rank cap is ≤ 0 MUST return EMPTY (0
// rows, no error). SQL semantics select no rows for all of:
//
//   - literal `ROW_NUMBER() … <= 0`                  (adjusted cap k   = 0)
//   - parameterized `… <= ?` bound to 0              (adjusted cap k   = 0)
//   - parameterized `… <= ?` bound to a NEGATIVE     (adjusted cap k   < 0)
//   - parameterized `… <  ?` bound to 1              (adjusted cap k-1 = 0)
//
// On HEAD the executor's vector scan evaluated its rank cap with the eager
// evalPositiveInt, which REJECTS k ≤ 0 ("top-K must be positive"). Because the
// Limit operator opens its inner cursor unconditionally — even for Limit(0) /
// Limit(?)=0 — the scan was built (and errored) BEFORE the Limit could cull to
// empty. Only the single case `< 1` (literal comparand K=1, which survives
// evalPositiveInt → self-limiting limit k-1=0 short-circuit) was handled; every
// non-positive comparand erred. The fix evaluates the cap with the tolerant
// evalRankCap and short-circuits a ≤ 0 adjusted cap to EMPTY for BOTH the
// ordered-stream (residual) and self-limiting (no-residual) branches. Both
// shapes are covered: WITH a residual WHERE (forces the ordered-stream branch)
// and WITHOUT (self-limiting / static-Limit branch).
func TestFDB_VectorSearch_NonPositiveCapEmpty(t *testing.T) {
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
	md := setupParamRankStore(t, ctx, db, ks, paramRankCorpus())

	const knn = `ROW_NUMBER() OVER (ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0]))`
	// Each adjusted cap is ≤ 0, so every case selects NO rows. Both spine shapes
	// are exercised: "no residual" (self-limiting / static Limit) and "residual"
	// (ordered-stream, the Filter between Limit and scan blocks the sink).
	cases := []struct {
		name   string
		sql    string
		params []any
	}{
		// Literal <= 0 → adjusted cap k = 0.
		{
			"literal <= 0, no residual",
			`SELECT id FROM docs QUALIFY ` + knn + ` <= 0`, nil,
		},
		{
			"literal <= 0, residual",
			`SELECT id FROM docs WHERE category = 'target' QUALIFY ` + knn + ` <= 0`, nil,
		},
		// Parameterized <= ? bound to 0 → adjusted cap k = 0.
		{
			"param <= 0, no residual",
			`SELECT id FROM docs QUALIFY ` + knn + ` <= ?`,
			[]any{int64(0)},
		},
		{
			"param <= 0, residual",
			`SELECT id FROM docs WHERE category = 'target' QUALIFY ` + knn + ` <= ?`,
			[]any{int64(0)},
		},
		// Parameterized <= ? bound to a NEGATIVE → adjusted cap k < 0.
		{
			"param <= -1, no residual",
			`SELECT id FROM docs QUALIFY ` + knn + ` <= ?`,
			[]any{int64(-1)},
		},
		{
			"param <= -1, residual",
			`SELECT id FROM docs WHERE category = 'target' QUALIFY ` + knn + ` <= ?`,
			[]any{int64(-1)},
		},
		// Parameterized < ? bound to 1 → adjusted cap k-1 = 0.
		{
			"param < 1, no residual",
			`SELECT id FROM docs QUALIFY ` + knn + ` < ?`,
			[]any{int64(1)},
		},
		{
			"param < 1, residual",
			`SELECT id FROM docs WHERE category = 'target' QUALIFY ` + knn + ` < ?`,
			[]any{int64(1)},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// runParamRankQuery t.Fatalf's on ANY execute error, so a green run
			// proves both invariants: no error AND the rows below.
			ids := runParamRankQuery(t, ctx, db, ks, md, tc.sql, tc.params)
			if len(ids) != 0 {
				t.Fatalf("non-positive adjusted rank cap returned %v, want EMPTY (0 rows, no error)", ids)
			}
		})
	}
}

// TestFDB_VectorSearch_ParamRankExact is the RFC-156 P1 codex-blocker e2e proof
// for the PARAMETERIZED-K branch: a GLOBAL-rank vector query bounded by a BOUND
// PARAMETER (ROW_NUMBER() … <= ? / < ?) MUST return exactly the runtime-K
// nearest. On HEAD globalRankVectorLimit declined the non-literal K (no Limit),
// so the ordered scan streamed its whole horizon and the query returned far more
// than K rows. The fix carries the cap as a RUNTIME Value — the K Value itself
// for <=, a K-1 arithmetic Value for < — that the executor evaluates against the
// bound parameters, so the ordered scan + Filter + Limit(?) compose the true K
// nearest MATCHING rows.
func TestFDB_VectorSearch_ParamRankExact(t *testing.T) {
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
	md := setupParamRankStore(t, ctx, db, ks, paramRankCorpus())

	cases := []struct {
		name   string
		sql    string
		params []any
		want   []int64
	}{
		{
			// Global K-NN, no residual: <= 2 → the 2 nearest overall (1, 2).
			"param <= 2, no residual",
			`SELECT id FROM docs
				QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])) <= ?`,
			[]any{int64(2)},
			[]int64{1, 2},
		},
		{
			// Bind a DIFFERENT K (3) against the same plan shape → 3 nearest.
			"param <= 3, no residual",
			`SELECT id FROM docs
				QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])) <= ?`,
			[]any{int64(3)},
			[]int64{1, 2, 3},
		},
		{
			// Residual: <= 2 over CATEGORY='target' → the 2 nearest MATCHING rows
			// (2, 4) — the decoys 1 & 3 (nearer, CATEGORY='other') excluded.
			"param <= 2, residual",
			`SELECT id FROM docs WHERE category = 'target'
				QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])) <= ?`,
			[]any{int64(2)},
			[]int64{2, 4},
		},
		{
			// STRICT <: runtime K-1. < 3 → the top 2 nearest (1, 2).
			"param < 3, no residual (K-1 runtime)",
			`SELECT id FROM docs
				QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])) < ?`,
			[]any{int64(3)},
			[]int64{1, 2},
		},
		{
			// STRICT < with residual: < 3 over CATEGORY='target' → top 2 matching
			// (2, 4).
			"param < 3, residual (K-1 runtime)",
			`SELECT id FROM docs WHERE category = 'target'
				QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])) < ?`,
			[]any{int64(3)},
			[]int64{2, 4},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ids := runParamRankQuery(t, ctx, db, ks, md, tc.sql, tc.params)
			// Distance order is total here, so emission order is deterministic.
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			if !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("param-rank K-NN ids = %v, want %v (runtime-K nearest matching)", ids, tc.want)
			}
		})
	}
}

// runVecQueryIDs plans + executes sql with the given bound params and returns
// the result ids in emission order. Unlike runParamRankQuery it does NOT assert
// the P1 global-rank planner shape (always Limit-bounded, never a sunk `rank<`):
// a LITERAL positive-K no-residual query LEGITIMATELY folds into a self-limiting
// `rank<K` scan via SinkLimitIntoVectorScanRule (RFC-156 Phase B), so the P1
// "rank< is forbidden" pin does not apply to the literal cases this exercises.
// It still confirms the query planned to a vector scan.
func runVecQueryIDs(t *testing.T, ctx context.Context, db *recordlayer.FDBDatabase, ks subspace.Subspace, md *recordlayer.RecordMetaData, sql string, params []any) []int64 {
	t.Helper()
	plan, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
	if err != nil {
		t.Fatalf("plan: %v\nsql=%s", err, sql)
	}
	if exp := plan.Explain(); !strings.Contains(exp, "VectorIndexScan") {
		t.Fatalf("query did not plan to a vector scan:\n%s", exp)
	}
	evalCtx := executor.EmptyEvaluationContext()
	if len(params) > 0 {
		evalCtx = evalCtx.WithParams(params)
	}
	var ids []int64
	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
		if sErr != nil {
			return nil, sErr
		}
		cursor, cErr := executor.ExecutePlan(ctx, plan, store, evalCtx, nil, recordlayer.DefaultExecuteProperties())
		if cErr != nil {
			return nil, cErr
		}
		defer cursor.Close()
		results, rErr := executor.CollectAll(ctx, cursor)
		if rErr != nil {
			return nil, rErr
		}
		for _, r := range results {
			ids = append(ids, r.Datum.(map[string]any)["ID"].(int64))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("execute: %v\nsql=%s", err, sql)
	}
	return ids
}

// TestFDB_VectorSearch_MinInt64CapEmpty pins the RFC-156 codex-delta P2-A
// overflow fix: a GLOBAL-rank vector K-NN whose STRICT rank cap K is
// math.MinInt64 (literal `< -9223372036854775808`, or a parameter bound to it)
// MUST return EMPTY — it must NOT wrap K-1 to a huge POSITIVE and escape into an
// enormous HNSW horizon.
//
// The bug: the executor adjusted a strict `< K` rank to `K-1` UNCONDITIONALLY.
// For K = math.MinInt64, `K-1` wraps to math.MaxInt64 (a huge POSITIVE), slips
// past the `adjusted ≤ 0 ⇒ EMPTY` guard, and becomes the scan horizon → the HNSW
// search allocates a math.MaxInt64-capacity candidate heap (`make(distHeap, 0,
// ef)`), which panics "makeslice: cap out of range". The fix returns EMPTY when
// K ≤ 1 BEFORE subtracting, so the wrap is impossible.
//
// Both spine shapes are exercised: "no residual" (the literal K folds into a
// self-limiting `rank<K` scan; the runtime/param K stays an ordered stream under
// a runtime Limit) and "residual" (the Filter blocks the sink, leaving an
// ordered stream). All four reach the SAME executor rank-cap eval — the single
// guard site under test — which short-circuits BEFORE the ordered/self-limiting
// split. Sanity cases pin the boundary: `< 1` is still EMPTY (adjusted cap 0)
// and `< 2` returns exactly the 1 nearest (adjusted cap 1) — proving the guard
// culls ONLY the wrap, never a legitimate small strict cap.
func TestFDB_VectorSearch_MinInt64CapEmpty(t *testing.T) {
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
	md := setupParamRankStore(t, ctx, db, ks, paramRankCorpus())

	const minI64 = `-9223372036854775808` // math.MinInt64, the overflow comparand
	const knn = `ROW_NUMBER() OVER (ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0]))`
	cases := []struct {
		name   string
		sql    string
		params []any
		want   []int64 // nil ⇒ EMPTY
	}{
		// K = math.MinInt64 ⇒ EMPTY (the overflow case — literal and parameter).
		{
			"literal < MinInt64, no residual",
			`SELECT id FROM docs QUALIFY ` + knn + ` < ` + minI64, nil, nil,
		},
		{
			"literal < MinInt64, residual",
			`SELECT id FROM docs WHERE category = 'target' QUALIFY ` + knn + ` < ` + minI64, nil, nil,
		},
		{
			"param < MinInt64, no residual",
			`SELECT id FROM docs QUALIFY ` + knn + ` < ?`,
			[]any{int64(math.MinInt64)},
			nil,
		},
		{
			"param < MinInt64, residual",
			`SELECT id FROM docs WHERE category = 'target' QUALIFY ` + knn + ` < ?`,
			[]any{int64(math.MinInt64)},
			nil,
		},
		// Sanity: < 1 ⇒ adjusted cap 0 ⇒ EMPTY (still, the small-strict-cap floor).
		{
			"literal < 1, no residual",
			`SELECT id FROM docs QUALIFY ` + knn + ` < 1`, nil, nil,
		},
		{
			"literal < 1, residual",
			`SELECT id FROM docs WHERE category = 'target' QUALIFY ` + knn + ` < 1`, nil, nil,
		},
		// Sanity: < 2 ⇒ adjusted cap 1 ⇒ exactly the 1 nearest. No residual ⇒ the
		// GLOBAL nearest (id 1, a decoy); residual ⇒ the nearest MATCHING row (id 2).
		{
			"literal < 2, no residual",
			`SELECT id FROM docs QUALIFY ` + knn + ` < 2`, nil,
			[]int64{1},
		},
		{
			"literal < 2, residual",
			`SELECT id FROM docs WHERE category = 'target' QUALIFY ` + knn + ` < 2`, nil,
			[]int64{2},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ids := runVecQueryIDs(t, ctx, db, ks, md, tc.sql, tc.params)
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			if len(tc.want) == 0 {
				if len(ids) != 0 {
					t.Fatalf("%s: got %v, want EMPTY (math.MinInt64 strict cap must not wrap into a huge horizon)", tc.name, ids)
				}
				return
			}
			if !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("%s: got %v, want %v", tc.name, ids, tc.want)
			}
		})
	}
}

// TestFDB_VectorSearch_HorizonExceedsDefaultEf is the RFC-156 P2 codex-blocker
// e2e proof: a GLOBAL-rank query whose cap EXCEEDS the default ef_search (200)
// must still return all K rows. The ordered-stream path is taken whenever a
// residual Filter sits between the Limit and the scan (the literal-K no-residual
// case sinks into a self-limiting top-k scan instead, which raises ef_search to
// k on its own — so the P2 horizon bug only bites the ORDERED branch). An
// un-partitioned HNSW ordered stream is fixed-horizon (no posting cells to
// widen), and the executor's ordered branch derives the scan budget (horizon)
// from defaultVectorEfSearch. On HEAD that was a fixed 200, so the ordered scan
// surfaced only the 200 nearest, the Filter passed them, and the Limit(300)
// above silently under-returned 200. The fix raises the horizon to
// max(defaultVectorEfSearch, adjustedK) = 300, so the scan covers all 300.
func TestFDB_VectorSearch_HorizonExceedsDefaultEf(t *testing.T) {
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

	// 350 rows fanned along the x-axis at strictly increasing distance from the
	// origin query, so the 300 nearest are unambiguous (ids 1..300). > 300 so the
	// rank cap (300) exceeds the default ef_search (200). ALL share CATEGORY='x'
	// so the residual matches every row (it exists only to force the ORDERED path
	// — it is the Filter between Limit and scan that blocks the self-limiting
	// sink), and the 300 ordered survivors all pass it.
	const total = 350
	const capN = 300
	corpus := make([]paramRankRow, 0, total)
	for i := 1; i <= total; i++ {
		corpus = append(corpus, paramRankRow{int64(i), "x", []float64{float64(i) * 0.001, 0, 0}})
	}
	md := setupParamRankStore(t, ctx, db, ks, corpus)

	sql := `SELECT id FROM docs WHERE category = 'x'
		QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(embedding, [0.0, 0.0, 0.0])) <= 300`
	// Confirm the ordered path is actually taken (else the horizon fix is moot).
	plan, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if exp := plan.Explain(); !strings.Contains(exp, "ordered") {
		t.Fatalf("expected an ORDERED vector scan (the P2 horizon branch); explain:\n%s", exp)
	}

	ids := runParamRankQuery(t, ctx, db, ks, md, sql, nil)
	if len(ids) != capN {
		t.Fatalf("ROW_NUMBER() <= 300 returned %d rows, want %d — the ordered horizon must be ≥ the rank cap, "+
			"not the fixed default ef_search (200) (the P2 under-return)", len(ids), capN)
	}
	// The returned set must be exactly the 300 nearest (ids 1..300) — HNSW recall
	// at ef = max(200, 300) = 300 over 350 nodes is exact for this monotone fan.
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for i, id := range ids {
		if id != int64(i+1) {
			t.Fatalf("expected the 300 nearest to be ids 1..300; mismatch at position %d: got id %d\nfull=%v", i, id, ids)
		}
	}
}
