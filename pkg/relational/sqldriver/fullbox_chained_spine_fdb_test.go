package sqldriver_test

import (
	"context"
	"fmt"
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
	"fdb.dev/pkg/relational/core/embedded"
)

// TestFDB_FullBoxChainedSpine pins the FULL-box-bottomed chained spine rows
// across the pureSpine boundary: a spine bottoming in a FULL OUTER box is
// ADMITTED (clusterArity==1, so it ordinalizes when unfiltered), but its
// bottom aliases are BOX LEGS, so a box-leg WHERE must decline the WHOLE
// chain to name-based resolution, coherently with the first link's own gate.
// An earlier, incoherent split — chained-link ordinal resolution OVER the
// first link's name-based seed — served the OTHER leg's value through a
// box-leg column, silently wrong. Rows here are the name-based ground truth;
// the seed-form boundary itself is pinned white-box in
// 2d_chained_spine_seed_test.go.
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
func TestFDB_FullBoxChainedSpine(t *testing.T) {
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
	elemDesc := arrayElementMessageDescriptor(sarrFD)
	substructFD := elemDesc.Fields().ByName("SUBSTRUCT")
	elem2Desc := arrayElementMessageDescriptor(substructFD)

	mkElem2 := func(deep ...int32) protoreflect.Value {
		m := dynamicpb.NewMessage(elem2Desc)
		m.Set(elem2Desc.Fields().ByName("LEAF"), protoreflect.ValueOfInt64(0))
		deepVals := make([]protoreflect.Value, 0, len(deep))
		for _, d := range deep {
			deepVals = append(deepVals, protoreflect.ValueOfInt32(d))
		}
		setArrayField(m, elem2Desc.Fields().ByName("DEEP"), deepVals...)
		return protoreflect.ValueOfMessage(m)
	}
	mkElem := func(sub []int32, substruct ...protoreflect.Value) protoreflect.Value {
		m := dynamicpb.NewMessage(elemDesc)
		m.Set(elemDesc.Fields().ByName("K"), protoreflect.ValueOfInt64(0))
		subVals := make([]protoreflect.Value, 0, len(sub))
		for _, s := range sub {
			subVals = append(subVals, protoreflect.ValueOfInt32(s))
		}
		setArrayField(m, elemDesc.Fields().ByName("SUB"), subVals...)
		setArrayField(m, substructFD, substruct...)
		return protoreflect.ValueOfMessage(m)
	}
	mkT4 := func(id int64, sarr ...protoreflect.Value) proto.Message {
		m := dynamicpb.NewMessage(t4Desc)
		m.Set(t4Desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(t4Desc.Fields().ByName("SUB"), protoreflect.ValueOfInt64(5))
		setArrayField(m, sarrFD, sarr...)
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
				out = append(out, positionalNamedPipeSprint(r))
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("%s: exec error (an incoherent ordinal/name-based split strands or mis-reads here): %v\n  sql: %s", name, eerr, q)
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
	// A chained lateral unnest whose spine bottoms in a FULL OUTER box that
	// cannot ordinalize (a box-leg predicate, or a nested outer box)
	// LOUD-REJECTS — Java rejects FULL OUTER JOIN at the grammar level, so this
	// Go-only extension caps its reach here rather than sink into a per-leg
	// window composition for a shape Java lacks entirely (TODO.md: ordinalize
	// it as a future reach extension). The row-answering cases (unfiltered /
	// inner-only / no-box) keep their `want` assertions above.
	wantReject := func(name, q string) {
		t.Helper()
		_, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
		if perr == nil {
			t.Fatalf("%s: expected a FULL-OUTER-JOIN loud-reject, got a plan\n  sql: %s", name, q)
		}
		if !strings.Contains(perr.Error(), "FULL OUTER JOIN") {
			t.Fatalf("%s: expected FULL-OUTER-JOIN reject, got: %v\n  sql: %s", name, perr, q)
		}
	}

	fullBox := `FROM T4 AS "A" FULL OUTER JOIN T4 AS "B" ON "A"."ID" + 10 = "B"."ID"`

	// Box-leg WHERE on the A leg: the un-ordinalizable straddle (a box-leg predicate
	// the box-leg window composition cannot yet fold into the chained seed).
	// LOUD-REJECT — Java rejects FULL OUTER JOIN entirely, so this Go-only
	// extension caps its reach here (TODO.md: ordinalize as a future extension).
	wantReject("boxlegA_filter",
		`SELECT "A"."ID", "B"."ID", "Y" `+fullBox+`, "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "A"."ID" = 10`)

	// Box-leg WHERE on the OTHER (B) leg — same straddle from the non-unnested side.
	wantReject("boxlegB_filter",
		`SELECT "A"."ID", "B"."ID", "Y" `+fullBox+`, "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "B"."ID" = 20`)

	// THREE-link chain over the FULL box + box-leg WHERE — the straddle one level deeper.
	wantReject("threelink_boxleg_filter",
		`SELECT "Z" `+fullBox+`, "A"."SARR" AS "X", "X"."SUBSTRUCT" AS "Y2", "Y2"."DEEP" AS "Z" WHERE "A"."ID" = 10`)

	// UNFILTERED FULL-box chain (it ordinalizes when unfiltered, and must
	// answer the name-based ground truth): all A-side SUB values; A-null rows
	// (B=10, B=3 unmatched) contribute nothing (NULL SARR unnests to no rows).
	want("unfiltered",
		`SELECT "Y" `+fullBox+`, "A"."SARR" AS "X", "X"."SUB" AS "Y"`,
		[]string{
			"Y=1", "Y=2", "Y=10", "Y=3",
			"Y=4", "Y=5", "Y=6",
			"Y=3", "Y=9",
		})

	// INNER-only WHERE (no box-leg ref → the box-leg-conjunct flag never sets →
	// stays ordinal over the admitted FULL box): Y>8 → {10, 9}.
	want("inner_only_filter",
		`SELECT "Y" `+fullBox+`, "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "Y" > 8`,
		[]string{"Y=10", "Y=9"})

	// AT on the FIRST link with a MULTI-element owner: T4(10) has two SARR
	// elements, so the first-link ordinal carries a discriminating P=2 row — a
	// first-link ordinal that constantly emitted 1 fails here (plain chain, no
	// box: the P=2 axis the shared-cert data cannot express).
	want("at_first_link_p2",
		`SELECT "P", "Y" FROM T4, T4."SARR" AS "X" AT "P", "X"."SUB" AS "Y"`,
		[]string{
			"P=1|Y=1", "P=1|Y=2", "P=1|Y=10", "P=2|Y=3",
			"P=1|Y=4", "P=1|Y=5", "P=1|Y=6",
			"P=1|Y=3", "P=1|Y=9",
		})

	// The FORK admission composing with the impure bottom: a FORK spine over
	// the FULL box + a box-leg WHERE must decline the WHOLE chain to
	// name-based resolution via the box-leg-conjunct arm — the fork admission
	// must NOT bypass it. W = X.SUB of A(10)'s elements, × Y2's SUBSTRUCT
	// elements per element: X1{SUB[1,2,10], SS×1} → {1,2,10}; X2{SUB[3], SS×1}
	// → {3}.
	wantReject("fork_over_box_boxleg_filter",
		`SELECT "W" `+fullBox+`, "A"."SARR" AS "X", "X"."SUBSTRUCT" AS "Y2", "X"."SUB" AS "W" WHERE "A"."ID" = 10`)

	// The unfiltered fork over the FULL box ordinalizes and must answer the
	// same name-based ground truth.
	want("fork_over_box_unfiltered",
		`SELECT "W" `+fullBox+`, "A"."SARR" AS "X", "X"."SUBSTRUCT" AS "Y2", "X"."SUB" AS "W"`,
		[]string{
			"W=1", "W=2", "W=10", "W=3",
			"W=4", "W=5", "W=6",
			"W=3", "W=9",
		})
	// NESTED-outer-box bottom — `(A LEFT B) FULL C`: the outermost box is a FULL
	// join, so the chained spine bottoms in a FULL box and LOUD-REJECTS (Java
	// rejects FULL OUTER JOIN entirely). Every one of these — filtered,
	// unfiltered, and leg/multi-leg projections — is the same unsupported
	// straddle (TODO.md: ordinalize the outer-box chained spine as a future
	// extension).
	nested := `FROM T4 AS "A" LEFT JOIN T4 AS "B" ON "A"."ID" + 10 = "B"."ID" FULL OUTER JOIN T4 AS "C" ON "A"."ID" = "C"."ID" + 100, "A"."SARR" AS "X", "X"."SUB" AS "Y"`
	wantReject("nestedbox_unfiltered", `SELECT "Y" `+nested)
	wantReject("nestedbox_boxleg_filter", `SELECT "Y" `+nested+` WHERE "A"."ID" = 10`)
	wantReject("nestedbox_inner_only", `SELECT "Y" `+nested+` WHERE "Y" > 8`)
	wantReject("nestedbox_leg_projection", `SELECT "A"."ID", "B"."ID", "Y" `+nested)
	wantReject("nestedbox_cleg_projection", `SELECT "C"."ID", "Y" `+nested)
	wantReject("nestedbox_multileg_star",
		`SELECT "A"."ID" AS "AID", "B"."ID" AS "BID", "C"."ID" AS "CID", "Y" `+nested+` WHERE "Y" = 1`)
}
