package sqldriver_test

import (
	"context"
	"fmt"
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

// combosRec is the rich row shape shared by every residual-combo subtest: a
// long pk, the residual columns exercised by the various WHERE shapes
// (CATEGORY/PRICE/USER_ID/DESCRIPTION — all UN-indexed, so each stays a residual
// Filter above the ordered vector scan), and the SPFresh-indexed EMBEDDING.
type combosRec struct {
	id    int64
	cat   string
	price int64
	user  int64
	desc  string
	vec   []float64
}

// TestFDB_VectorSearch_ResidualFilterCombos is the RFC-156 Phase C breadth proof
// (the user's "compose with other filters and shit"): an un-partitioned SPFresh
// vector index composes with a wide spread of NON-equality residual shapes —
// numeric range, BETWEEN, LIKE, NOT LIKE, AND-of-two, OR-of-two. For each shape
// the corpus is built so the GLOBAL nearest rows are DECOYS that FAIL the
// predicate while the true k-nearest MATCHING rows sit farther out. The test
// pins BOTH halves of the contract:
//
//   - PLAN: Limit(k) → Filter(residual) → VectorIndexScan(ordered). The residual
//     must stay a Filter ABOVE the ordered scan (never sunk into the scan as a
//     rank<= limit, and never demoted to a non-vector full scan + sort). The
//     "ordered" token + the absence of "rank<" is the EXPLAIN-pin.
//   - ANSWER: the true k nearest MATCHING rows in distance order, decoys excluded.
//
// If ANY shape fails to plan to that exact shape or returns the wrong set, that
// is a REAL composition bug (a planner gap, not a test artifact) and the
// assertion fails loudly — never weakened.
func TestFDB_VectorSearch_ResidualFilterCombos(t *testing.T) {
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

	// One rich schema shared across every shape; each subtest writes into its own
	// keyspace (t.Name() includes the subtest name) so the corpora never collide.
	b := metadata.NewSchemaTemplateBuilder().SetName("vt")
	b.AddTable("DOCS", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("CATEGORY", api.NewStringType(false), 2),
		metadata.NewColumnSpec("PRICE", api.NewLongType(false), 3),
		metadata.NewColumnSpec("USER_ID", api.NewLongType(false), 4),
		metadata.NewColumnSpec("DESCRIPTION", api.NewStringType(false), 5),
		metadata.NewColumnSpec("EMBEDDING", api.NewVectorType(64, 3, true), 6),
	}, []string{"ID"})
	b.AddVectorIndexUsing("SPFRESH", "DOCS", "VEC_IDX", "EMBEDDING", nil,
		map[string]string{recordlayer.IndexOptionSPFreshMetric: "EUCLIDEAN_METRIC"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	desc := md.GetRecordType("DOCS").Descriptor

	makeRec := func(r combosRec) proto.Message {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(r.id))
		m.Set(desc.Fields().ByName("CATEGORY"), protoreflect.ValueOfString(r.cat))
		m.Set(desc.Fields().ByName("PRICE"), protoreflect.ValueOfInt64(r.price))
		m.Set(desc.Fields().ByName("USER_ID"), protoreflect.ValueOfInt64(r.user))
		m.Set(desc.Fields().ByName("DESCRIPTION"), protoreflect.ValueOfString(r.desc))
		m.Set(desc.Fields().ByName("EMBEDDING"), protoreflect.ValueOfBytes(recordlayer.SerializeVector(r.vec)))
		return m
	}

	// d packs a 1-D distance into a 3-D embedding on the x-axis: euclidean
	// distance from the origin query [0,0,0] is exactly x. Lets each corpus be
	// described purely by "how far" each row sits.
	d := func(x float64) []float64 { return []float64{x, 0, 0} }

	type shape struct {
		name   string
		where  string
		corpus []combosRec
		want   []int64
	}
	// Every shape: q = origin, k = 2, matches at distance 1/2/3 (ids 10/11/12),
	// decoys NEARER (distance 0.1/0.2/0.3) but failing the predicate → want
	// [10, 11] (the 2 nearest matching, decoy-free, id 12 dropped by k=2).
	shapes := []shape{
		{
			name:  "numeric_range_gt",
			where: "price > 100",
			corpus: []combosRec{
				{1, "x", 50, 0, "none", d(0.1)},  // nearer, price<=100 — DECOY
				{2, "x", 100, 0, "none", d(0.2)}, // nearer, price==100 (not >) — DECOY
				{10, "x", 200, 0, "none", d(1)},  // MATCH (nearest passing)
				{11, "x", 300, 0, "none", d(2)},  // MATCH
				{12, "x", 150, 0, "none", d(3)},  // MATCH (excluded by k=2)
			},
			want: []int64{10, 11},
		},
		{
			name:  "numeric_between",
			where: "price BETWEEN 100 AND 200",
			corpus: []combosRec{
				{1, "x", 50, 0, "none", d(0.1)},  // nearer, below range — DECOY
				{2, "x", 500, 0, "none", d(0.2)}, // nearer, above range — DECOY
				{10, "x", 150, 0, "none", d(1)},  // MATCH (in [100,200])
				{11, "x", 200, 0, "none", d(2)},  // MATCH (inclusive upper)
				{12, "x", 100, 0, "none", d(3)},  // MATCH (inclusive lower, excluded by k=2)
			},
			want: []int64{10, 11},
		},
		{
			name:  "string_like",
			where: "category LIKE '%foo%'",
			corpus: []combosRec{
				{1, "bar", 0, 0, "none", d(0.1)},  // nearer, no 'foo' — DECOY
				{2, "baz", 0, 0, "none", d(0.2)},  // nearer, no 'foo' — DECOY
				{10, "afoob", 0, 0, "none", d(1)}, // MATCH (contains 'foo')
				{11, "foox", 0, 0, "none", d(2)},  // MATCH (prefix 'foo')
				{12, "myfoo", 0, 0, "none", d(3)}, // MATCH (suffix 'foo', excluded by k=2)
			},
			want: []int64{10, 11},
		},
		{
			name:  "string_not_like",
			where: "description NOT LIKE '%bar%'",
			corpus: []combosRec{
				{1, "x", 0, 0, "has bar inside", d(0.1)}, // nearer, contains 'bar' — DECOY
				{2, "x", 0, 0, "bartender", d(0.2)},      // nearer, contains 'bar' — DECOY
				{10, "x", 0, 0, "clean text", d(1)},      // MATCH (no 'bar')
				{11, "x", 0, 0, "nice copy", d(2)},       // MATCH (no 'bar')
				{12, "x", 0, 0, "great desc", d(3)},      // MATCH (excluded by k=2)
			},
			want: []int64{10, 11},
		},
		{
			name:  "and_of_two",
			where: "user_id = 7 AND price > 100",
			corpus: []combosRec{
				{1, "x", 50, 7, "none", d(0.1)},  // nearer, price fails — DECOY
				{2, "x", 500, 9, "none", d(0.2)}, // nearer, user fails — DECOY
				{3, "x", 50, 9, "none", d(0.3)},  // nearer, both fail — DECOY
				{10, "x", 200, 7, "none", d(1)},  // MATCH (both hold)
				{11, "x", 300, 7, "none", d(2)},  // MATCH (both hold)
				{12, "x", 150, 7, "none", d(3)},  // MATCH (excluded by k=2)
			},
			want: []int64{10, 11},
		},
		{
			name:  "or_of_two",
			where: "category = 'a' OR category = 'b'",
			corpus: []combosRec{
				{1, "c", 0, 0, "none", d(0.1)}, // nearer, neither — DECOY
				{2, "d", 0, 0, "none", d(0.2)}, // nearer, neither — DECOY
				{10, "a", 0, 0, "none", d(1)},  // MATCH (= 'a')
				{11, "b", 0, 0, "none", d(2)},  // MATCH (= 'b')
				{12, "a", 0, 0, "none", d(3)},  // MATCH (excluded by k=2)
			},
			want: []int64{10, 11},
		},
	}

	for _, sh := range shapes {
		sh := sh
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

			// Cold-start inserts through SaveRecord (no bulk build) — the §6b path.
			_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, sErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				if sErr != nil {
					return nil, sErr
				}
				for _, r := range sh.corpus {
					if _, e := store.SaveRecord(makeRec(r)); e != nil {
						return nil, e
					}
				}
				return nil, nil
			})
			if err != nil {
				t.Fatalf("setup: %v", err)
			}

			sql := fmt.Sprintf(`SELECT id FROM docs WHERE %s
				QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(embedding, [0.0, 0.0, 0.0])) <= 2`, sh.where)
			plan, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
			if err != nil {
				t.Fatalf("FAILED TO PLAN residual %q over SPFresh vector scan — REAL composition bug: %v", sh.where, err)
			}
			exp := plan.Explain()
			t.Logf("[%s] WHERE %s\nplan: %s", sh.name, sh.where, exp)

			// PLAN-PIN: Limit → Filter → VectorIndexScan(ordered), in that nesting
			// order, residual NOT sunk into the scan.
			assertOrderedResidualPlan(t, exp, sh.where)

			// ANSWER-PIN: the true 2 nearest MATCHING rows in distance order.
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
				if !reflect.DeepEqual(ids, sh.want) {
					t.Errorf("residual %q: K-NN ids = %v, want %v (true 2 nearest passing rows in "+
						"distance order, decoys excluded) — REAL composition bug", sh.where, ids, sh.want)
				}
				return nil, nil
			})
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
		})
	}
}

// assertOrderedResidualPlan pins the canonical Phase C residual shape:
// Limit(k) → Filter(residual) → VectorIndexScan(..., ordered). It checks the
// tokens are present AND nested in that order (positional, since Explain renders
// the tree as nested calls), the scan is ordered (not self-limiting), and k was
// NOT sunk into the scan as a rank<= limit.
func assertOrderedResidualPlan(t *testing.T, exp, where string) {
	t.Helper()
	idxLimit := strings.Index(exp, "Limit(")
	idxFilter := strings.Index(exp, "Filter(")
	idxScan := strings.Index(exp, "VectorIndexScan")
	if idxScan < 0 || !strings.Contains(exp, "ordered") {
		t.Fatalf("residual %q did NOT plan to an ordered VectorIndexScan — REAL bug (planner fell back "+
			"to a non-vector scan?):\n%s", where, exp)
	}
	if idxLimit < 0 {
		t.Fatalf("residual %q: missing the Limit(k) above the scan:\n%s", where, exp)
	}
	if idxFilter < 0 {
		t.Fatalf("residual %q: the residual did NOT survive as a Filter above the ordered scan "+
			"(it was sunk or dropped) — REAL bug:\n%s", where, exp)
	}
	if !(idxLimit < idxFilter && idxFilter < idxScan) {
		t.Fatalf("residual %q: plan nesting is not Limit → Filter → VectorIndexScan "+
			"(Limit@%d Filter@%d Scan@%d):\n%s", where, idxLimit, idxFilter, idxScan, exp)
	}
	if strings.Contains(exp, "rank<") {
		t.Fatalf("residual %q: k was SUNK into the scan (rank<) instead of a Limit above the "+
			"filter — REAL bug (the residual would cull the self-limited top-k → under-return):\n%s", where, exp)
	}
}

// TestFDB_VectorSearch_ColdStartCappedHonestTruncation is the per-PR REDUCED pin
// for the honest-truncation bug RFC-156 stress coverage surfaced — a SILENT WRONG
// ANSWER, the exact "never a silent < k" violation Phase C forbids — and proves
// the fix end-to-end.
//
// The capped-posting bug is config-INDEPENDENT: the read-path reads only the
// 4·Lmax+1 ENVELOPE of an oversized cold-start posting, so any corpus with more
// than 4·Lmax+1 rows in one posting hides its larger-PK tail. This variant tunes
// Lmax DOWN to 16 (envelope = 65) so a few hundred cold-start rows reproduce the
// IDENTICAL code path at a fraction of the inserts, keeping it inside the per-PR
// sqldriver_test timeout (standard + race). The production-scale (20k, default
// Lmax) version lives in the stress target:
// //pkg/relational/sqldriver/stress, TestFDB_VectorSearch_ColdStartCappedHonestTruncation.
//
// ROOT CAUSE (now fixed). Cold-start SaveRecord inserts pile every row into ONE
// coarse cell with a single oversized posting (>4*Lmax entries) and a
// queued-but-unrun split task. The ordered-stream search reads only the 4*Lmax+1
// ENVELOPE of that posting (spfresh_query.go scoreCells records the overflow in
// frontier.capped and re-files the split) — posting entries are keyed by PK, so
// the envelope is the SMALLEST 4*Lmax+1 PKs and every larger-PK row is INVISIBLE
// to the query. BEFORE the fix, with only one coarse cell the cursor set
// streamExhaust purely on widenRouteComplete (ignoring frontier.capped) and
// returned SourceExhausted — claiming the index was COMPLETE while it examined a
// small fraction of the entries. The repro below (matching row at the LARGEST PK,
// in the invisible tail) returned [] with SourceExhausted: a silent, signal-free
// wrong answer. The fix gates streamExhaust on `widenRouteComplete &&
// len(frontier.capped)==0`; a capped posting now sets budgetHit → ScanLimitReached.
//
// HONEST CONTRACT (what this asserts, two phases):
//
//  1. UNMAINTAINED (cold-start, capped posting): the query CANNOT see the tail in
//     one pass, so it must signal incompleteness with ScanLimitReached (resumable),
//     NEVER an incomplete/empty set under SourceExhausted.
//  2. AFTER MAINTENANCE: the first query's terminal re-filed the split
//     (refileCapped); the rebalancer processes it and the oversized posting splits
//     into <=Lmax pieces (no longer capped). Re-running the SAME query now returns
//     the true nearest match [nW] with SourceExhausted (genuinely complete).
//
// Together this proves the fix: no silent under-return, and the read-path re-file
// heals it.
func TestFDB_VectorSearch_ColdStartCappedHonestTruncation(t *testing.T) {
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
	// Lmax=16 → the read-path envelope is 4·Lmax+1 = 65 entries, so a cold-start
	// corpus of nW=400 rows in one oversized posting hides PKs 66..400 in the
	// invisible tail — the exact capped-posting condition, reached with ~400 rows
	// instead of the 20k the default Lmax (256, envelope 1025) would require.
	b.AddVectorIndexUsing("SPFRESH", "DOCS", "VEC_IDX", "EMBEDDING", nil,
		map[string]string{
			recordlayer.IndexOptionSPFreshMetric:     "EUCLIDEAN_METRIC",
			recordlayer.IndexOptionSPFreshLmax:       "16",
			recordlayer.IndexOptionSPFreshCellTarget: "4",
			recordlayer.IndexOptionSPFreshCellMax:    "8",
		})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	desc := md.GetRecordType("DOCS").Descriptor
	storeBuilder := func(rtx *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
		return recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
	}

	makeRec := func(id int64, cat string, vec []float64) proto.Message {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(desc.Fields().ByName("CATEGORY"), protoreflect.ValueOfString(cat))
		m.Set(desc.Fields().ByName("EMBEDDING"), protoreflect.ValueOfBytes(recordlayer.SerializeVector(vec)))
		return m
	}

	// nW rows, cold-start (NO rebalance) → one oversized capped posting (nW well
	// over the 4·Lmax+1 = 65 read envelope). The matching row has the LARGEST PK
	// (so it lands in the invisible posting tail) AND is the nearest (at the origin
	// query). Every other row is a far decoy.
	const nW = 400
	for start := 0; start < nW; start += 500 {
		end := start + 500
		if end > nW {
			end = nW
		}
		s, e := start, end
		if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, sErr := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			if sErr != nil {
				return nil, sErr
			}
			for i := s; i < e; i++ {
				id := int64(i + 1)
				cat, vec := "other", []float64{float64(i%128)*0.5 + 10, float64(i/128)*0.5 + 10, 1}
				if id == nW {
					cat, vec = "target", []float64{0, 0, 0} // nearest, but largest PK → invisible tail
				}
				if _, e := store.SaveRecord(makeRec(id, cat, vec)); e != nil {
					return nil, e
				}
			}
			return nil, nil
		}); err != nil {
			t.Fatalf("insert @%d: %v", start, err)
		}
	}

	const sql = `SELECT id FROM docs WHERE category = 'target'
		QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(embedding, [0.0, 0.0, 0.0])) <= 1`
	plan, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	// runQuery drains the executor cursor manually so it observes the TERMINAL
	// reason directly (CollectAll would re-map an out-of-band stop to an error).
	runQuery := func() ([]int64, recordlayer.NoNextReason) {
		var ids []int64
		var reason recordlayer.NoNextReason
		_, qerr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, sErr := storeBuilder(rtx)
			if sErr != nil {
				return nil, sErr
			}
			cursor, cErr := executor.ExecutePlan(ctx, plan, store,
				executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
			if cErr != nil {
				return nil, cErr
			}
			defer cursor.Close()
			for {
				res, e := cursor.OnNext(ctx)
				if e != nil {
					return nil, e
				}
				if !res.HasNext() {
					reason = res.GetNoNextReason()
					break
				}
				ids = append(ids, res.GetValue().Datum.(map[string]any)["ID"].(int64))
			}
			return nil, nil
		})
		if qerr != nil {
			t.Fatalf("execute: %v", qerr)
		}
		return ids, reason
	}

	// Phase 1 — UNMAINTAINED: the capped posting hides the true nearest match in
	// its tail. The terminal MUST be ScanLimitReached (honest "I could not see the
	// whole posting; more may exist"), NOT SourceExhausted with a silent < k.
	// (Pre-fix this returned [] + SourceExhausted — the silent wrong answer.)
	ids, reason := runQuery()
	t.Logf("cold-start (capped) residual K-NN: ids=%v reason=%v (isOutOfBand=%v)", ids, reason, reason.IsOutOfBand())
	if reason != recordlayer.ScanLimitReached {
		t.Errorf("honest-truncation contract: a read-capped posting (true nearest match in the "+
			"invisible tail) must surface ScanLimitReached, got reason=%v ids=%v — a silent under-return "+
			"under SourceExhausted is the §1 'never a silent < k' violation", reason, ids)
	}
	if !reason.IsOutOfBand() {
		t.Errorf("ScanLimitReached must be out-of-band (resumable); reason=%v is in-band", reason)
	}

	// Maintenance — the first query's terminal re-filed the oversized posting's
	// split (refileCapped); draining the queue splits it into <=Lmax pieces.
	if _, rerr := recordlayer.RebalanceSPFreshIndex(ctx, db, storeBuilder, "VEC_IDX"); rerr != nil {
		t.Fatalf("rebalance: %v", rerr)
	}

	// Phase 2 — AFTER MAINTENANCE: the posting is no longer capped, so the true
	// nearest match is now visible and the index is genuinely complete: the SAME
	// query returns [nW] with SourceExhausted. The re-file path healed the
	// under-return — no permanent data loss, just an honest signal in the interim.
	ids2, reason2 := runQuery()
	t.Logf("post-rebalance residual K-NN: ids=%v reason=%v (isOutOfBand=%v); true answer=[%d]",
		ids2, reason2, reason2.IsOutOfBand(), int64(nW))
	if !reflect.DeepEqual(ids2, []int64{nW}) {
		t.Errorf("post-maintenance: query for the nearest CATEGORY='target' returned ids=%v, want [%d] "+
			"(the true nearest, now visible after the capped posting split)", ids2, int64(nW))
	}
	if reason2 != recordlayer.SourceExhausted {
		t.Errorf("post-maintenance: a fully-maintained index (no capped postings) must return "+
			"SourceExhausted (genuinely complete), got reason=%v", reason2)
	}
}
