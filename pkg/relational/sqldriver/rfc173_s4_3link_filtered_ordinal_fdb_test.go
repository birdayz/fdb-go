package sqldriver_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/core/embedded"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// TestFDB_RFC173S4_ThreeLinkFilteredOrdinalizes pins the deeper-nesting slice: a FILTERED
// 3-link chain now ORDINALIZES (the clusterArity gate lifted to accept a chained first-link
// base via chainedBaseOrdinalizes), with the mixed-inner-ref clause landing at the INNERMOST
// Explode (the 2c positional bake, now at depth-3) and a pure-outer conjunct SARGing the scan
// — both ORDINAL-only discriminators (the name model carries the outer ref by name, out of the
// scan's reach). It ALSO relocates the 2c review-round regression guard to the shape that
// STAYS name-model: a BOX-BASE chain (first link's base is a 2-source box → declined, c5b
// territory) keeps the NAME-KEY rebase and answers a straddle correctly with NO ordinal -1
// strand. The 2c bug was a positional bake firing on a name-model fallback; this cert proves
// the box-base name-model shape still routes to the name-key rebase.
func TestFDB_RFC173S4_ThreeLinkFilteredOrdinalizes(t *testing.T) {
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
		m.Set(t4Desc.Fields().ByName("SUB"), protoreflect.ValueOfInt64(999))
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
			mkT4(1, mkElem([]int32{1, 7}, mkElem2(11, 12), mkElem2(13))),
			mkT4(2, mkElem([]int32{9}, mkElem2(20))),
			mkT4(11, mkElem([]int32{5}, mkElem2(11))), // ID=11 matches Z=11 (straddle detector)
		} {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	run := func(name, q string) (string, []string) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
		if perr != nil {
			t.Fatalf("%s: plan error: %v\n  sql: %s", name, perr, q)
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
				out = append(out, fmt.Sprintf("%v", r.Datum))
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("%s: exec error (a mis-placed positional bake strands at ordinal -1 here): %v\n  sql: %s", name, eerr, q)
		}
		sort.Strings(out)
		return explain, out
	}
	wantRows := func(name string, got, exp []string, q string) {
		t.Helper()
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", exp) {
			t.Fatalf("%s: rows = %v, want %v\n  sql: %s", name, got, exp, q)
		}
	}
	// assertOrdinal3Link pins the ORDINAL 3-link signature: exactly THREE Explode legs
	// (one per link) over a bare Scan(T4) base — NOT a NestedLoopJoin box (which the
	// name-model box-base decline carries). Robust to interposed PredicatesFilters
	// (a pushed-down conjunct wraps a middle FlatMap), so it asserts the ordinal
	// nesting without over-pinning where each predicate happens to land.
	assertOrdinal3Link := func(name, explain string) {
		t.Helper()
		if n := strings.Count(explain, "Explode("); n != 3 {
			t.Fatalf("%s: must have exactly 3 Explode legs (ordinal 3-link); got %d; plan=%s", name, n, explain)
		}
		if !strings.Contains(explain, "Scan(T4") {
			t.Fatalf("%s: ordinal 3-link must root at Scan(T4); plan=%s", name, explain)
		}
		if strings.Contains(explain, "NestedLoopJoin") {
			t.Fatalf("%s: single-source 3-link must NOT have a box base; plan=%s", name, explain)
		}
	}

	base := `FROM T4, T4."SARR" AS "X", "X"."SUBSTRUCT" AS "Y", "Y"."DEEP" AS "Z"`

	// LONE outer filter: ID=1 → {11,12,13}. The outer conjunct is baked POSITIONALLY
	// (ofOrdinal over the outer QOV type), becomes scan-pushable, and SARGs the scan
	// (Scan(T4, [=]) — an ORDINAL-only discriminator: the name model carries the outer
	// ref by NAME in the AnchoredJoin record, out of the scan's reach).
	q0 := `SELECT "Z" ` + base + ` WHERE T4."ID" = 1`
	ex0, r0 := run("threelink_outer_filter_SARGs", q0)
	assertOrdinal3Link("threelink_outer_filter_SARGs", ex0)
	if !strings.Contains(ex0, "Scan(T4, [=") {
		t.Fatalf("threelink_outer_filter_SARGs: lone outer conjunct must SARG the scan (ordinal); plan=%s", ex0)
	}
	wantRows("threelink_outer_filter_SARGs", r0, []string{"map[Z:11]", "map[Z:12]", "map[Z:13]"}, q0)

	// MIXED-inner-ref OR: ID=1 → {11,12,13}; Z=20 from ID=2 → {20}. → {11,12,13,20}.
	// The mixed clause lands as a PredicatesFilter at the INNERMOST Explode (the
	// placement the 2c positional bake produces, now at depth-3).
	q1 := `SELECT "Z" ` + base + ` WHERE T4."ID" = 1 OR "Z" = 20`
	ex1, r1 := run("threelink_mixed_OR", q1)
	assertOrdinal3Link("threelink_mixed_OR", ex1)
	if !strings.Contains(ex1, "inner=PredicatesFilter(Explode") {
		t.Fatalf("threelink_mixed_OR: mixed clause must land at the innermost Explode; plan=%s", ex1)
	}
	wantRows("threelink_mixed_OR", r1, []string{"map[Z:11]", "map[Z:12]", "map[Z:13]", "map[Z:20]"}, q1)

	// STRADDLING over a 3-link — a single AND-free comparison mixing outer + the DEEPEST
	// element. T4(11) Z=11 matches ID=11. Innermost-Explode placement — the mixed clause resolves at the deepest level.
	q2 := `SELECT T4."ID", "Z" ` + base + ` WHERE T4."ID" = "Z"`
	ex2, r2 := run("threelink_straddling_match", q2)
	assertOrdinal3Link("threelink_straddling_match", ex2)
	if !strings.Contains(ex2, "inner=PredicatesFilter(Explode") {
		t.Fatalf("threelink_straddling_match: straddle must land at the innermost Explode; plan=%s", ex2)
	}
	wantRows("threelink_straddling_match", r2, []string{"map[T4.ID:11 Z:11]"}, q2)

	// AND with inner ref: ID=1 ∩ Z>11 → {12,13}. AND-splits: the inner conjunct Z>11
	// lands at the innermost Explode; the outer conjunct ID=1 lands as a mid-chain
	// PredicatesFilter (the pushdown does not SARG it once combined with an inner
	// conjunct — a placement detail; both give correct rows).
	q3 := `SELECT "Z" ` + base + ` WHERE T4."ID" = 1 AND "Z" > 11`
	ex3, r3 := run("threelink_and_inner", q3)
	assertOrdinal3Link("threelink_and_inner", ex3)
	if !strings.Contains(ex3, "inner=PredicatesFilter(Explode") {
		t.Fatalf("threelink_and_inner: inner conjunct must land at the innermost Explode; plan=%s", ex3)
	}
	wantRows("threelink_and_inner", r3, []string{"map[Z:12]", "map[Z:13]"}, q3)

	// PURE-outer OR (scan-pushable): ID=1 OR ID=2 → {11,12,13,20}.
	q4 := `SELECT "Z" ` + base + ` WHERE T4."ID" = 1 OR T4."ID" = 2`
	ex4, r4 := run("threelink_pure_outer_OR", q4)
	assertOrdinal3Link("threelink_pure_outer_OR", ex4)
	wantRows("threelink_pure_outer_OR", r4, []string{"map[Z:11]", "map[Z:12]", "map[Z:13]", "map[Z:20]"}, q4)

	// BOX-BASE chain — the relocated name-model regression guard. The first
	// link's base is a 2-source box (T4, T4C), which chainedBaseOrdinalizes DECLINES
	// (c5b territory). It stays NAME-MODEL (NestedLoopJoin base, the outer conjunct a
	// PredicatesFilter — NO scan SARG) and a straddle mixing outer + the deepest element
	// keeps the NAME-KEY rebase, answering correctly with NO ordinal -1 strand. This is
	// the 2c review-round regression dimension, now living on the still-name-model shape.
	boxBase := `FROM T4, T4 AS "T4C", T4."SARR" AS "X", "X"."SUBSTRUCT" AS "Y", "Y"."DEEP" AS "Z"`
	q5 := `SELECT T4."ID", "Z" ` + boxBase + ` WHERE T4."ID" = "Z"`
	ex5, r5 := run("boxbase_straddle_declines_namemodel", q5)
	if !strings.Contains(ex5, "NestedLoopJoin") {
		t.Fatalf("boxbase_straddle: box-base chain must keep its NestedLoopJoin base (name-model decline); plan=%s", ex5)
	}
	// T4(11) Z=11 matches ID=11, × 3 T4C rows.
	wantRows("boxbase_straddle_declines_namemodel", r5,
		[]string{"map[T4.ID:11 Z:11]", "map[T4.ID:11 Z:11]", "map[T4.ID:11 Z:11]"}, q5)

	// A 2-CHAIN buried behind a trailing table (join.Right is a SCAN, not an unnest →
	// never reaches the chained ordinal dispatch): stays on the buried-unnest path.
	// T4(1) Y∈{1,7}, ID=1=Y=1 → {Y:1} × 3 T4C rows = {1,1,1}.
	q6 := `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y", T4 AS "T4C" WHERE T4."ID" = "Y"`
	_, r6 := run("buried_2chain_straddle", q6)
	wantRows("buried_2chain_straddle", r6, []string{"map[Y:1]", "map[Y:1]", "map[Y:1]"}, q6)
}
