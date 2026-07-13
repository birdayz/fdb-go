package sqldriver_test

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
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

// TestFDB_RFC173S4_BoxDupClass2 is the certificate for the RFC-173 S4 box-dup CLASS-2
// lift: a CROSS-LEG dup where one leg peels to a merge-opaque box (`FROM (A FULL C), B,
// C.arr AS X` with buried A.K duplicating scan B.K) now GATHERS via the raw seed instead
// of declining. Each leg keeps its own [Offset,Width) window — A.K via the box's
// buried-leaf sub-window (finalizeSeedWindows), B.K via its scan run — so a qualified
// read routes by SLOT through EVERY outer operator. Every pin is DISCRIMINATING: A.K is
// distinct 100/300/NULL and B.K=200 is constant, so a mis-bind (buried A.K → scan B.K)
// shows 200 / never NULL / a collapsed order, and the FULL-NULL padded row (A.K=NULL,
// C.arr=[55]) proves the nil-supplied box leg resolves through the gather dispatch.
//
// A WITHIN-box dup (two same-named columns in ONE box) is a distinct shape that still
// DECLINES (doubly-null-padded — its own increment); sentineled white-box by
// TestRFC173W5_Gathered_DeclineBoundary case (b2-within).
//
//	A(AID,K):   (1,100) (2,300)             -- buried dup column, distinct values
//	C(CID,ARR): (1,[7,8]) (2,[9]) (5,[55])  -- owns array; join A.AID=C.CID
//	B(BID,K):   (1,200)                     -- scan, dup K=200 (never NULL)
//	D(DID,DK):  (1,100)                      -- correlate target (matches A.K=100 only)
//
// Box (A FULL C): A.K=100/ARR[7,8]; A.K=300/ARR[9]; A.K=NULL(padded)/ARR[55].
func TestFDB_RFC173S4_BoxDupClass2(t *testing.T) {
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

	b := metadata.NewSchemaTemplateBuilder().SetName("boxdupc2")
	b.AddTable("A", []metadata.ColumnSpec{
		metadata.NewColumnSpec("AID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
	}, []string{"AID"})
	b.AddTable("C", []metadata.ColumnSpec{
		metadata.NewColumnSpec("CID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), true), 2),
	}, []string{"CID"})
	b.AddTable("B", []metadata.ColumnSpec{
		metadata.NewColumnSpec("BID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
	}, []string{"BID"})
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
	mkC := func(cid int64, vals ...int32) proto.Message {
		d := md.GetRecordType("C").Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName("CID"), protoreflect.ValueOfInt64(cid))
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
			mk2("A", "AID", "K", 1, 100), mk2("A", "AID", "K", 2, 300),
			mkC(1, 7, 8), mkC(2, 9), mkC(5, 55),
			mk2("B", "BID", "K", 1, 200),
			mk2("D", "DID", "DK", 1, 100),
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

	run := func(t *testing.T, q string) (string, []string) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
		if perr != nil {
			t.Fatalf("plan %q: %v", q, perr)
		}
		explain := plan.Explain()
		var out []string
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
				out = append(out, fmt.Sprintf("%v", executor.RowValue(r)))
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("exec %q: %v", q, eerr)
		}
		return explain, out
	}
	// wantSet asserts the row SET (sorted). gatherMarker (e.g. "A.K#") pins that the plan
	// GATHERED the raw seed with a baked ordinal — a name-model fallback would carry a
	// name-keyed read, not "#N", so its absence catches a regression to decline.
	wantSet := func(t *testing.T, q string, want []string, gatherMarker string) {
		t.Helper()
		explain, got := run(t, q)
		sort.Strings(got)
		if !slices.Equal(got, want) {
			t.Fatalf("rows = %v, want %v\n  sql: %s\n  plan: %s", got, want, q, explain)
		}
		if gatherMarker != "" && !strings.Contains(explain, gatherMarker) {
			t.Fatalf("plan did not gather (missing baked ordinal %q — a regression to name-model decline)\n  sql: %s\n  plan: %s", gatherMarker, q, explain)
		}
	}
	// wantOrdered preserves executor order (ORDER BY pins).
	wantOrdered := func(t *testing.T, q string, want []string, gatherMarker string) {
		t.Helper()
		explain, got := run(t, q)
		if !slices.Equal(got, want) {
			t.Fatalf("ordered rows = %v, want %v\n  sql: %s\n  plan: %s", got, want, q, explain)
		}
		if gatherMarker != "" && !strings.Contains(explain, gatherMarker) {
			t.Fatalf("sort key not baked (missing %q)\n  sql: %s\n  plan: %s", gatherMarker, q, explain)
		}
	}

	// FULL-NULL grouped rows: GROUP BY the buried dup A.K over the FULL box. The padded row
	// (A.K=NULL) counts its own group — a mis-bind to B.K=200 would fold it into a 200 group
	// and never show NULL. A.K distinct 100/300 disambiguates the buried read.
	t.Run("full_null_grouped", func(t *testing.T) {
		wantSet(t, `SELECT A."K", COUNT(*) FROM A FULL OUTER JOIN C ON A."AID" = C."CID", B, C."ARR" AS "X" GROUP BY A."K"`,
			[]string{"map[A.K:100 COUNT(*):2]", "map[A.K:300 COUNT(*):1]", "map[A.K:<nil> COUNT(*):1]"}, "A.K#")
	})

	// CROSS-LEG buried predicate: `A.K <> B.K` spans the buried A.K and the scan B.K — it
	// realizes as a JOIN predicate (each side its own quantifier). A.K∈{100,300,NULL},
	// B.K=200: <> keeps 100 & 300 (NULL<>200=NULL drops); = keeps none (100/300≠200, NULL).
	t.Run("cross_leg_buried_predicate", func(t *testing.T) {
		wantSet(t, `SELECT A."K", "X" FROM A FULL OUTER JOIN C ON A."AID" = C."CID", B, C."ARR" AS "X" WHERE A."K" <> B."K"`,
			[]string{"map[A.K:100 X:7]", "map[A.K:100 X:8]", "map[A.K:300 X:9]"}, "")
		wantSet(t, `SELECT A."K", "X" FROM A FULL OUTER JOIN C ON A."AID" = C."CID", B, C."ARR" AS "X" WHERE A."K" = B."K"`,
			nil, "")
	})

	// Box-dup ORDER BY over the buried dup A.K (the axis the ORDER BY-over-gather fix
	// closed, verified here for the box-DUP shape). DESC≠ASC on 100/300; padded A.K=NULL
	// sorts LAST (DESC) / FIRST (ASC) — nulls-smallest, matching single-source.
	t.Run("order_by_buried_dup", func(t *testing.T) {
		wantOrdered(t, `SELECT A."K", "X" FROM A FULL OUTER JOIN C ON A."AID" = C."CID", B, C."ARR" AS "X" ORDER BY A."K" DESC, "X"`,
			[]string{"map[A.K:300 X:9]", "map[A.K:100 X:7]", "map[A.K:100 X:8]", "map[A.K:<nil> X:55]"}, ".K#")
		wantOrdered(t, `SELECT A."K", "X" FROM A FULL OUTER JOIN C ON A."AID" = C."CID", B, C."ARR" AS "X" ORDER BY A."K" ASC, "X"`,
			[]string{"map[A.K:<nil> X:55]", "map[A.K:100 X:7]", "map[A.K:100 X:8]", "map[A.K:300 X:9]"}, ".K#")
	})

	// COMPOUND ORDER BY over buried A.K + scan B.K: both leaves bake (recursion over a
	// box-dup). A.K+B.K = A.K+200 (monotonic in A.K); the whole sum reads NULL if either
	// leaf fails to bake → element order → this fails.
	t.Run("order_by_compound_buried_scan", func(t *testing.T) {
		wantOrdered(t, `SELECT A."K", "X" FROM A FULL OUTER JOIN C ON A."AID" = C."CID", B, C."ARR" AS "X" ORDER BY A."K" + B."K" DESC, "X"`,
			[]string{"map[A.K:300 X:9]", "map[A.K:100 X:7]", "map[A.K:100 X:8]", "map[A.K:<nil> X:55]"}, ".K#")
	})

	// The three axes the ORDER BY fix never touched — pinned as regressions so the whole
	// de-risk sweep is a sentinel: DISTINCT over the buried+scan dup, correlated EXISTS
	// referencing the buried A.K, and the gather nested in a derived table.
	t.Run("distinct_buried_scan_dup", func(t *testing.T) {
		wantSet(t, `SELECT DISTINCT A."K", B."K" FROM A FULL OUTER JOIN C ON A."AID" = C."CID", B, C."ARR" AS "X"`,
			[]string{"map[A.K:100 B.K:200]", "map[A.K:300 B.K:200]", "map[A.K:<nil> B.K:200]"}, "")
	})
	t.Run("correlated_exists_buried", func(t *testing.T) {
		wantSet(t, `SELECT A."K", "X" FROM A FULL OUTER JOIN C ON A."AID" = C."CID", B, C."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM D WHERE D."DK" = A."K")`,
			[]string{"map[A.K:100 X:7]", "map[A.K:100 X:8]"}, "")
	})
	t.Run("subquery_wrapped", func(t *testing.T) {
		wantSet(t, `SELECT "T"."AK", "T"."X" FROM (SELECT A."K" AS "AK", "X" FROM A FULL OUTER JOIN C ON A."AID" = C."CID", B, C."ARR" AS "X") AS "T"`,
			[]string{"map[AK:100 X:7]", "map[AK:100 X:8]", "map[AK:300 X:9]", "map[AK:<nil> X:55]"}, "")
	})
}
