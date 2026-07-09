package sqldriver_test

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/core/embedded"
)

// TestFDB_RFC173S4_FullBoxChainedSpine pins the FULL-box-bottomed chained spine
// rows across the pureSpine boundary — the coherence dimension of the Slice-2d
// round-2 silent-wrong: a spine bottoming in a FULL OUTER box is ADMITTED
// (clusterArity==1, pre-slice parity: it ordinalizes unfiltered) but its bottom
// aliases are BOX LEGS, so a box-leg WHERE must decline the WHOLE chain to
// name-model coherently with the first link's own gate. The incoherent split
// (chained link ordinal OVER the first link's name-model seed) served the OTHER
// leg's value through a box-leg column. Rows here are name-model ground truth
// (verified identical on the pre-slice parent); the seed-form boundary itself
// is pinned white-box in rfc173_2d_chained_spine_seed_test.go.
//
// Data (T4(10) carries TWO SARR elements so the first-link AT ordinal has a
// discriminating P=2 row — a constant-1 ordinal emission fails the pin):
//
//	T4(10) sub=5: X1{SUB[1,2,10], SUBSTRUCT[DEEP 11,12]}, X2{SUB[3], SUBSTRUCT[DEEP 13]}
//	T4(20) sub=5: X {SUB[4,5,6],  SUBSTRUCT[DEEP 20]}
//	T4(3)  sub=5: X {SUB[3,9],    SUBSTRUCT[DEEP 5,7]}
//
// FULL JOIN ON A.ID+10=B.ID matches exactly (A=10, B=20); A(20)→30 and A(3)→13
// are B-null; B(10) and B(3) are A-null (whose NULL SARR unnests to no rows).
func TestFDB_RFC173S4_FullBoxChainedSpine(t *testing.T) {
	t.Parallel()
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

	md := buildChainedUnnestMetadata(t)
	t4Desc := md.GetRecordType("T4").Descriptor
	sarrFD := t4Desc.Fields().ByName("SARR")
	elemDesc := sarrFD.Message()
	substructFD := elemDesc.Fields().ByName("SUBSTRUCT")
	elem2Desc := substructFD.Message()

	mkElem2 := func(deep ...int32) protoreflect.Value {
		m := dynamicpb.NewMessage(elem2Desc)
		m.Set(elem2Desc.Fields().ByName("LEAF"), protoreflect.ValueOfInt64(0))
		dl := m.NewField(elem2Desc.Fields().ByName("DEEP")).List()
		for _, d := range deep {
			dl.Append(protoreflect.ValueOfInt32(d))
		}
		m.Set(elem2Desc.Fields().ByName("DEEP"), protoreflect.ValueOfList(dl))
		return protoreflect.ValueOfMessage(m)
	}
	mkElem := func(sub []int32, substruct ...protoreflect.Value) protoreflect.Value {
		m := dynamicpb.NewMessage(elemDesc)
		m.Set(elemDesc.Fields().ByName("K"), protoreflect.ValueOfInt64(0))
		sl := m.NewField(elemDesc.Fields().ByName("SUB")).List()
		for _, s := range sub {
			sl.Append(protoreflect.ValueOfInt32(s))
		}
		m.Set(elemDesc.Fields().ByName("SUB"), protoreflect.ValueOfList(sl))
		ssl := m.NewField(substructFD).List()
		for _, ss := range substruct {
			ssl.Append(ss)
		}
		m.Set(substructFD, protoreflect.ValueOfList(ssl))
		return protoreflect.ValueOfMessage(m)
	}
	mkT4 := func(id int64, sarr ...protoreflect.Value) proto.Message {
		m := dynamicpb.NewMessage(t4Desc)
		m.Set(t4Desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(t4Desc.Fields().ByName("SUB"), protoreflect.ValueOfInt64(5))
		sl := m.NewField(sarrFD).List()
		for _, e := range sarr {
			sl.Append(e)
		}
		m.Set(sarrFD, protoreflect.ValueOfList(sl))
		return m
	}

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		for _, r := range []proto.Message{
			mkT4(10,
				mkElem([]int32{1, 2, 10}, mkElem2(11, 12)),
				mkElem([]int32{3}, mkElem2(13))),
			mkT4(20, mkElem([]int32{4, 5, 6}, mkElem2(20))),
			mkT4(3, mkElem([]int32{3, 9}, mkElem2(5, 7))),
		} {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	run := func(name, q string) []string {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
		if perr != nil {
			t.Fatalf("%s: plan error: %v\n  sql: %s", name, perr, q)
		}
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
				out = append(out, fmt.Sprintf("%v", r.Datum))
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("%s: exec error (an incoherent ordinal/name-model split strands or mis-reads here): %v\n  sql: %s", name, eerr, q)
		}
		sort.Strings(out)
		return out
	}
	want := func(name, q string, exp []string) {
		t.Helper()
		got := run(name, q)
		sort.Strings(exp)
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", exp) {
			t.Fatalf("%s: rows = %v, want %v\n  sql: %s", name, got, exp, q)
		}
	}

	fullBox := `FROM T4 AS "A" FULL OUTER JOIN T4 AS "B" ON "A"."ID" + 10 = "B"."ID"`

	// Box-leg WHERE on the A leg: the round-2 silent-wrong shape (the split
	// served B's value through A.ID). A(10).SARR SUB = {1,2,10} ∪ {3}.
	want("boxlegA_filter",
		`SELECT "A"."ID", "B"."ID", "Y" `+fullBox+`, "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "A"."ID" = 10`,
		[]string{
			"map[A.ID:10 B.ID:20 Y:1]", "map[A.ID:10 B.ID:20 Y:2]",
			"map[A.ID:10 B.ID:20 Y:10]", "map[A.ID:10 B.ID:20 Y:3]",
		})

	// Box-leg WHERE on the OTHER (B) leg — the same coherence obligation from
	// the non-unnested side: B.ID=20 selects the matched pair (A=10).
	want("boxlegB_filter",
		`SELECT "A"."ID", "B"."ID", "Y" `+fullBox+`, "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "B"."ID" = 20`,
		[]string{
			"map[A.ID:10 B.ID:20 Y:1]", "map[A.ID:10 B.ID:20 Y:2]",
			"map[A.ID:10 B.ID:20 Y:10]", "map[A.ID:10 B.ID:20 Y:3]",
		})

	// THREE-link chain over the FULL box + box-leg WHERE — the coherence
	// obligation one level deeper. Z of A(10) = {11,12} ∪ {13}.
	want("threelink_boxleg_filter",
		`SELECT "Z" `+fullBox+`, "A"."SARR" AS "X", "X"."SUBSTRUCT" AS "Y2", "Y2"."DEEP" AS "Z" WHERE "A"."ID" = 10`,
		[]string{"map[Z:11]", "map[Z:12]", "map[Z:13]"})

	// UNFILTERED FULL-box chain (admission parity: ordinalizes, and must answer
	// the name-model ground truth): all A-side SUB values; A-null rows (B=10,
	// B=3 unmatched) contribute nothing (NULL SARR unnests to no rows).
	want("unfiltered",
		`SELECT "Y" `+fullBox+`, "A"."SARR" AS "X", "X"."SUB" AS "Y"`,
		[]string{
			"map[Y:1]", "map[Y:2]", "map[Y:10]", "map[Y:3]",
			"map[Y:4]", "map[Y:5]", "map[Y:6]",
			"map[Y:3]", "map[Y:9]",
		})

	// INNER-only WHERE (no box-leg ref → the box-leg-conjunct flag never sets →
	// stays ordinal over the admitted FULL box): Y>8 → {10, 9}.
	want("inner_only_filter",
		`SELECT "Y" `+fullBox+`, "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "Y" > 8`,
		[]string{"map[Y:10]", "map[Y:9]"})

	// AT on the FIRST link with a MULTI-element owner: T4(10) has two SARR
	// elements, so the first-link ordinal carries a discriminating P=2 row — a
	// first-link ordinal that constantly emitted 1 fails here (plain chain, no
	// box: the P=2 axis the shared-cert data cannot express).
	want("at_first_link_p2",
		`SELECT "P", "Y" FROM T4, T4."SARR" AS "X" AT "P", "X"."SUB" AS "Y"`,
		[]string{
			"map[P:1 Y:1]", "map[P:1 Y:2]", "map[P:1 Y:10]", "map[P:2 Y:3]",
			"map[P:1 Y:4]", "map[P:1 Y:5]", "map[P:1 Y:6]",
			"map[P:1 Y:3]", "map[P:1 Y:9]",
		})
}
