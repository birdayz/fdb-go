package sqldriver_test

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// TestFDB_WithinBoxDup is the certificate for gathering a single merge-opaque
// box whose concat carries the SAME column name K TWICE (a `(A FULL B)` box
// with both A.K and B.K) via the raw seed: buriedLegBounds windows the two
// same-named buried leaves at DISTINCT slots, so a QUALIFIED A.K / B.K routes
// to its own window, not first-match, through every outer operator.
//
// Doubly-null discrimination (a dimension a cross-leg dup doesn't exercise):
// A.K and B.K go NULL on OPPOSITE unmatched rows, and their value sets are
// DISJOINT ({10,30} vs {20,40}), so a cross-window mis-bind surfaces the
// wrong value set / crosses the NULL buckets. The composition pin (WHERE A.K
// = X — a within-box dup IN a buried-element predicate) is the load-bearing
// guard for the interaction between the two window resolutions.
//
//	A(AID,K,ARR): (1,10,[7,8]) (2,30,[9])   -- box left; K∈{10,30}; ARR for owner-side
//	B(BID,K):     (1,20) (3,40)             -- box right; K∈{20,40}; join A.AID=B.BID
//	C(CID,ARR):   (1,[10,30,8])             -- separate scan owner; 10/30 match A.K for compose
//	D(DID,DK):    (1,50) (2,60)             -- nested box-in-box outer leg
//
// Box (A FULL B): matched A.K=10/B.K=20; A-only A.K=30/B.K=NULL; B-only A.K=NULL/B.K=40.
func TestFDB_WithinBoxDup(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatal(err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	b := metadata.NewSchemaTemplateBuilder().SetName("wbdup")
	b.AddTable("A", []metadata.ColumnSpec{
		metadata.NewColumnSpec("AID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), true), 3),
	}, []string{"AID"})
	b.AddTable("B", []metadata.ColumnSpec{
		metadata.NewColumnSpec("BID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
	}, []string{"BID"})
	b.AddTable("C", []metadata.ColumnSpec{
		metadata.NewColumnSpec("CID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), true), 2),
	}, []string{"CID"})
	b.AddTable("D", []metadata.ColumnSpec{
		metadata.NewColumnSpec("DID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("DK", api.NewLongType(true), 2),
	}, []string{"DID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	md := tmpl.Underlying()

	mk2 := func(table, f1, f2 string, v1, v2 int64) proto.Message {
		d := md.GetRecordType(table).Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName(protoreflect.Name(f1)), protoreflect.ValueOfInt64(v1))
		m.Set(d.Fields().ByName(protoreflect.Name(f2)), protoreflect.ValueOfInt64(v2))
		return m
	}
	mkArr := func(table, idField, arrField string, id int64, vals ...int32) proto.Message {
		d := md.GetRecordType(table).Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName(protoreflect.Name(idField)), protoreflect.ValueOfInt64(id))
		fd := d.Fields().ByName(protoreflect.Name(arrField))
		l := m.NewField(fd).List()
		for _, v := range vals {
			l.Append(protoreflect.ValueOfInt32(v))
		}
		m.Set(fd, protoreflect.ValueOfList(l))
		return m
	}
	mkA := func(aid, k int64, vals ...int32) proto.Message {
		d := md.GetRecordType("A").Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName("AID"), protoreflect.ValueOfInt64(aid))
		m.Set(d.Fields().ByName("K"), protoreflect.ValueOfInt64(k))
		fd := d.Fields().ByName("ARR")
		l := m.NewField(fd).List()
		for _, v := range vals {
			l.Append(protoreflect.ValueOfInt32(v))
		}
		m.Set(fd, protoreflect.ValueOfList(l))
		return m
	}

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Create()
		if sErr != nil {
			return nil, sErr
		}
		recs := []proto.Message{
			mkA(1, 10, 7, 8), mkA(2, 30, 9),
			mk2("B", "BID", "K", 1, 20), mk2("B", "BID", "K", 3, 40),
			mkArr("C", "CID", "ARR", 1, 10, 30, 8),
			mk2("D", "DID", "DK", 1, 50), mk2("D", "DID", "DK", 2, 60),
		}
		for _, r := range recs {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	wantSet := func(t *testing.T, q string, want []string) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
		if perr != nil {
			t.Fatalf("plan %q: %v", q, perr)
		}
		explain := plan.Explain()
		var got []string
		_, eerr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if sErr != nil {
				return nil, sErr
			}
			cur, cErr := executor.ExecutePlan(ctx, plan, store, executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
			if cErr != nil {
				return nil, cErr
			}
			defer cur.Close()
			rows, rErr := executor.CollectAll(ctx, cur)
			if rErr != nil {
				return nil, rErr
			}
			for _, r := range rows {
				got = append(got, fmt.Sprintf("%v", executor.RowValue(r)))
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("exec %q: %v", q, eerr)
		}
		sort.Strings(got)
		if !slices.Equal(got, want) {
			t.Fatalf("rows = %v, want %v\n  sql: %s\n  plan: %s", got, want, q, explain)
		}
	}

	// DOUBLY-NULL discrimination: GROUP BY each within-box dup column. A.K∈{10,30,NULL} and
	// B.K∈{20,40,NULL} are DISJOINT — a cross-window mis-bind (A.K reading B.K's window)
	// would surface {20,40,NULL} for GROUP BY A.K. The A.K=NULL group is the B-only row
	// (A null-supplied); the B.K=NULL group is the A-only row — the OPPOSITE unmatched row.
	// C.arr has 3 elements → COUNT 3 per group.
	t.Run("doubly_null_group_by_A_K", func(t *testing.T) {
		wantSet(t, `SELECT A."K", COUNT(*) FROM A FULL OUTER JOIN B ON A."AID" = B."BID", C, C."ARR" AS "X" GROUP BY A."K"`,
			[]string{"map[COUNT(*):3 K:10]", "map[COUNT(*):3 K:30]", "map[COUNT(*):3 K:<nil>]"})
	})
	t.Run("doubly_null_group_by_B_K", func(t *testing.T) {
		wantSet(t, `SELECT B."K", COUNT(*) FROM A FULL OUTER JOIN B ON A."AID" = B."BID", C, C."ARR" AS "X" GROUP BY B."K"`,
			[]string{"map[COUNT(*):3 K:20]", "map[COUNT(*):3 K:40]", "map[COUNT(*):3 K:<nil>]"})
	})
	// SELECT DISTINCT both dup columns — each routes to its own window (matched {10,20},
	// A-only {30,NULL}, B-only {NULL,40}). DISTINCT dedups the C×unnest cross product.
	t.Run("select_both_dup_columns", func(t *testing.T) {
		wantSet(t, `SELECT DISTINCT A."K", B."K" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", C, C."ARR" AS "X"`,
			[]string{"map[A.K:10 B.K:20]", "map[A.K:30 B.K:<nil>]", "map[A.K:<nil> B.K:40]"})
	})

	// COMPOSITION (the load-bearing pin): a within-box dup column IN a buried-element
	// predicate (`WHERE A.K = X`) — composes the within-box window resolution with the
	// buried-element predicate bake at the predicate bake site. DISCRIMINATING: A.K=30 is
	// the A-only row (B.K=NULL there), and 30∈C.arr, so {30,30} appears ONLY because A.K
	// resolved to windows[A]; a cross-window mis-bind to B.K (=NULL on that row) → NULL=30 →
	// drop. A.K=10 matched (B.K=20): {10,10} iff A.K read (not B.K=20). The B twin → [].
	t.Run("compose_within_box_buried_element", func(t *testing.T) {
		wantSet(t, `SELECT A."K", "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", C, C."ARR" AS "X" WHERE A."K" = "X"`,
			[]string{"map[K:10 X:10]", "map[K:30 X:30]"})
		wantSet(t, `SELECT B."K", "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", C, C."ARR" AS "X" WHERE B."K" = "X"`, nil)
	})

	// OWNER-SIDE dup: the unnest owner is A, a BURIED box leaf (not the separate scan C).
	// `FROM (A FULL B), A.arr AS X GROUP BY A.K`. The A-null (B-only) row has A.ARR=NULL →
	// 0 unnest rows (Java explode-over-null), so no A.K=NULL group. A.K=10→[7,8] (2),
	// A.K=30→[9] (1).
	t.Run("owner_side_buried_box_leaf", func(t *testing.T) {
		wantSet(t, `SELECT A."K", COUNT(*) FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X" GROUP BY A."K"`,
			[]string{"map[COUNT(*):1 K:30]", "map[COUNT(*):2 K:10]"})
	})

	// NESTED box-in-box within-box dup: (A FULL B) FULL D (chained, left-assoc).
	// buriedLegBounds RECURSES — inner (A,B) + outer (D) each get their own distinct window.
	t.Run("nested_box_in_box_group_A_K", func(t *testing.T) {
		wantSet(t, `SELECT A."K", COUNT(*) FROM A FULL OUTER JOIN B ON A."AID" = B."BID" FULL OUTER JOIN D ON A."AID" = D."DID", C, C."ARR" AS "X" GROUP BY A."K"`,
			[]string{"map[COUNT(*):3 K:10]", "map[COUNT(*):3 K:30]", "map[COUNT(*):3 K:<nil>]"})
	})
	t.Run("nested_box_in_box_select_all_leaves", func(t *testing.T) {
		wantSet(t, `SELECT A."K", B."K", D."DK" FROM A FULL OUTER JOIN B ON A."AID" = B."BID" FULL OUTER JOIN D ON A."AID" = D."DID"`,
			[]string{"map[A.K:10 B.K:20 DK:50]", "map[A.K:30 B.K:<nil> DK:60]", "map[A.K:<nil> B.K:40 DK:<nil>]"})
	})
}
