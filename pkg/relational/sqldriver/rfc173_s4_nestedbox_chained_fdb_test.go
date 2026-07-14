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

// TestFDB_RFC173S4_NestedLeftBoxChained pins the NESTED-LEFT-box lateral-unnest
// rows — `(A LEFT B) LEFT C` under both a single-link and a chained unnest.
// Every one of these shapes STRANDED before the SelectMergeRule dissolved-box
// barrier ("best expression is not a physical plan: LogicalProjectionExpression"):
// the REWRITING phase pruned the box's group to the flattened
// `[A, B(null-on-empty), C(null-on-empty)]` select — a canonical seed Go's
// PLANNING cannot re-partition (no select-level predicates survive the
// dissolve, and the positional-merge arm declines to collapse a null-on-empty
// quantifier) — so RED-ON-REVERT is the plan error itself. The row
// expectations are hand-computed from the data below (the name-model ground
// truth; the nested box's LEFT semantics are independently pinned by the
// null-supply-barrier and W4-left suites).
//
// Data (SARR elements as {SUB…; SUBSTRUCT DEEP…}; SCARR the scalar array):
//
//	T4(10)  SARR=[{SUB[1,2,10]; DEEP[11,12]}, {SUB[3]; DEEP[13]}]  SCARR=[100,200]
//	T4(20)  SARR=[{SUB[4,5,6];  DEEP[20]}]                        SCARR=[300]
//	T4(3)   SARR=[{SUB[3,9];    DEEP[5,7]}]                       SCARR=[]
//	T4(110) SARR=[{SUB[7];      DEEP[8]}]                         SCARR=[400]
//
// Box: A LEFT JOIN B ON A.ID+10=B.ID LEFT JOIN C ON C.ID=A.ID+90 — matches:
//
//	A=10  → B=20,  C null (100 absent)
//	A=20  → B null, C=110
//	A=3   → B null, C null
//	A=110 → B null, C null
func TestFDB_RFC173S4_NestedLeftBoxChained(t *testing.T) {
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
	scarrFD := t4Desc.Fields().ByName("SCARR")

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
	mkT4 := func(id int64, scarr []int32, sarr ...protoreflect.Value) proto.Message {
		m := dynamicpb.NewMessage(t4Desc)
		m.Set(t4Desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(t4Desc.Fields().ByName("SUB"), protoreflect.ValueOfInt64(5))
		sl := m.NewField(sarrFD).List()
		for _, e := range sarr {
			sl.Append(e)
		}
		m.Set(sarrFD, protoreflect.ValueOfList(sl))
		scl := m.NewField(scarrFD).List()
		for _, s := range scarr {
			scl.Append(protoreflect.ValueOfInt32(s))
		}
		m.Set(scarrFD, protoreflect.ValueOfList(scl))
		return m
	}

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		for _, r := range []proto.Message{
			mkT4(10, []int32{100, 200},
				mkElem([]int32{1, 2, 10}, mkElem2(11, 12)),
				mkElem([]int32{3}, mkElem2(13))),
			mkT4(20, []int32{300}, mkElem([]int32{4, 5, 6}, mkElem2(20))),
			mkT4(3, nil, mkElem([]int32{3, 9}, mkElem2(5, 7))),
			mkT4(110, []int32{400}, mkElem([]int32{7}, mkElem2(8))),
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
			t.Fatalf("%s: plan error (the nested-LEFT-box strand — red-on-revert): %v\n  sql: %s", name, perr, q)
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
				out = append(out, fmt.Sprintf("%v", executor.RowValue(r)))
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("%s: exec error: %v\n  sql: %s", name, eerr, q)
		}
		sort.Strings(out)
		return out
	}
	want := func(name, q string, exp []string) {
		t.Helper()
		got := run(name, q)
		sort.Strings(exp)
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", exp) {
			t.Errorf("%s: rows = %v, want %v\n  sql: %s", name, got, exp, q)
		}
	}
	// RFC-173 S4 cap: a WHERE on a join-leg column of the OUTER box under a
	// chained lateral unnest is the un-ordinalizable straddle — LOUD-REJECT
	// (correct-or-loud), the same cap the FULL-box chained spine takes. No
	// correct representation exists today (the chained merged-corr rebase's
	// baked ordinal collides with the first link's inner Explode; a box-quantifier
	// bake sinks below the nested outer null-extension into the null-supplied scan;
	// the name-model residual strands at physicalization). Row-answering shapes
	// (element rows, leg projections, element/AT WHERE, deeper links) never carry a
	// box-leg conjunct and ordinalize.
	wantReject := func(name, q string) {
		t.Helper()
		_, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
		if perr == nil {
			t.Errorf("%s: expected a loud reject (box-leg WHERE over a chained OUTER box), got a plan\n  sql: %s", name, q)
			return
		}
		if !strings.Contains(perr.Error(), "OUTER JOIN under a chained lateral unnest") {
			t.Errorf("%s: expected the chained-box-leg-WHERE reject, got: %v\n  sql: %s", name, perr, q)
		}
	}

	nested := `FROM T4 AS "A" LEFT JOIN T4 AS "B" ON "A"."ID" + 10 = "B"."ID" LEFT JOIN T4 AS "C" ON "C"."ID" = "A"."ID" + 90`

	// Chained element rows, unfiltered: every A row unnests its SARR elements'
	// SUB values; B/C match at most one row each, so multiplicity is 1 per A.
	want("chained_elements",
		`SELECT "Y" `+nested+`, "A"."SARR" AS "X", "X"."SUB" AS "Y"`,
		[]string{
			"map[Y:1]", "map[Y:2]", "map[Y:10]", "map[Y:3]",
			"map[Y:4]", "map[Y:5]", "map[Y:6]",
			"map[Y:3]", "map[Y:9]",
			"map[Y:7]",
		})

	// Leg projection with the null-supplied legs: A=10 carries B=20/C=NULL,
	// A=20 carries B=NULL/C=110, A=3 and A=110 carry both NULL.
	want("chained_leg_projection",
		`SELECT "A"."ID", "B"."ID", "C"."ID", "Y" `+nested+`, "A"."SARR" AS "X", "X"."SUB" AS "Y"`,
		[]string{
			"map[A.ID:10 B.ID:20 C.ID:<nil> Y:1]",
			"map[A.ID:10 B.ID:20 C.ID:<nil> Y:2]",
			"map[A.ID:10 B.ID:20 C.ID:<nil> Y:10]",
			"map[A.ID:10 B.ID:20 C.ID:<nil> Y:3]",
			"map[A.ID:20 B.ID:<nil> C.ID:110 Y:4]",
			"map[A.ID:20 B.ID:<nil> C.ID:110 Y:5]",
			"map[A.ID:20 B.ID:<nil> C.ID:110 Y:6]",
			"map[A.ID:3 B.ID:<nil> C.ID:<nil> Y:3]",
			"map[A.ID:3 B.ID:<nil> C.ID:<nil> Y:9]",
			"map[A.ID:110 B.ID:<nil> C.ID:<nil> Y:7]",
		})

	// Element WHERE: Y>3 → A=10 keeps {10}, A=20 keeps {4,5,6}, A=3 keeps {9},
	// A=110 keeps {7}.
	want("chained_element_filter",
		`SELECT "Y" `+nested+`, "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "Y" > 3`,
		[]string{"map[Y:10]", "map[Y:4]", "map[Y:5]", "map[Y:6]", "map[Y:9]", "map[Y:7]"})

	// Box-leg WHERE — the un-ordinalizable straddle, LOUD-REJECTED at the cap
	// (preserved leg, first null-supplying leg, second null-supplying leg).
	wantReject("chained_boxlegA_filter",
		`SELECT "Y" `+nested+`, "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "A"."ID" = 10`)
	wantReject("chained_boxlegB_filter",
		`SELECT "Y" `+nested+`, "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "B"."ID" = 20`)
	wantReject("chained_boxlegC_filter",
		`SELECT "Y" `+nested+`, "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "C"."ID" = 110`)

	// THREE-link chain over the nested box: X.SUBSTRUCT AS Y2, Y2.DEEP AS Z.
	want("threelink",
		`SELECT "Z" `+nested+`, "A"."SARR" AS "X", "X"."SUBSTRUCT" AS "Y2", "Y2"."DEEP" AS "Z"`,
		[]string{
			"map[Z:11]", "map[Z:12]", "map[Z:13]",
			"map[Z:20]", "map[Z:5]", "map[Z:7]", "map[Z:8]",
		})

	// SINGLE-link scalar-array unnest over the nested box, with leg projection
	// (the ORDINAL-path strand twin: 0 name-model producers fire for this).
	// A=3's SCARR is empty → no rows for it.
	want("singlelink_scarr_legs",
		`SELECT "A"."ID", "B"."ID", "C"."ID", "X" `+nested+`, "A"."SCARR" AS "X"`,
		[]string{
			"map[A.ID:10 B.ID:20 C.ID:<nil> X:100]",
			"map[A.ID:10 B.ID:20 C.ID:<nil> X:200]",
			"map[A.ID:20 B.ID:<nil> C.ID:110 X:300]",
			"map[A.ID:110 B.ID:<nil> C.ID:<nil> X:400]",
		})

	// SINGLE-link struct-array unnest over the nested box.
	want("singlelink_sarr",
		`SELECT "A"."ID", "B"."ID" `+nested+`, "A"."SARR" AS "X"`,
		[]string{
			"map[A.ID:10 B.ID:20]", "map[A.ID:10 B.ID:20]",
			"map[A.ID:20 B.ID:<nil>]",
			"map[A.ID:3 B.ID:<nil>]",
			"map[A.ID:110 B.ID:<nil>]",
		})

	// AT ordinal on the FIRST link over the nested box: A=10 has TWO SARR
	// elements (P=2 discriminates a constant-1 ordinal).
	want("at_first_link",
		`SELECT "P", "Y" `+nested+`, "A"."SARR" AS "X" AT "P", "X"."SUB" AS "Y"`,
		[]string{
			"map[P:1 Y:1]", "map[P:1 Y:2]", "map[P:1 Y:10]", "map[P:2 Y:3]",
			"map[P:1 Y:4]", "map[P:1 Y:5]", "map[P:1 Y:6]",
			"map[P:1 Y:3]", "map[P:1 Y:9]",
			"map[P:1 Y:7]",
		})
}
