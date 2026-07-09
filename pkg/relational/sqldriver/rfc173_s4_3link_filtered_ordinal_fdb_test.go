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

// TestFDB_RFC173S4_ThreeLinkFilteredOrdinalizes is the ROWS + PLAN-PLACEMENT cert for the
// deeper-nesting slice: a FILTERED linear 3-link chain ordinalizes (chainedBaseOrdinalizes +
// the spine-scoped box-leg-conjunct arm) and must answer the same rows as the name-model
// parent, with the mixed-inner-ref clause landing at the innermost Explode and a lone
// pure-outer conjunct SARGing the scan. NONE of these observables discriminate ordinal from
// name-model — plans, rows, and the SARG are IDENTICAL between the two models (the lazy
// ⊆-outerLegs path precedes the seed-form fork; a first cut of this cert claimed the SARG as
// an ordinal-only signature and passed verbatim on the name-model parent). The seed-form
// BOUNDARY is pinned white-box in rfc173_2d_chained_spine_seed_test.go (the RC AnchoredJoin
// flag off translateChainedUnnestJoin, feature-off-control-verified); THIS cert pins that the
// rows stay correct on both sides of that boundary: the ordinalizing linear shapes, the
// box-base chain that declines (c5b), the FORK chain that declines (owner two links back —
// the shape the unscoped gate lift malformed-planned at ordinal -1), and the buried 2-chain.
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
	// sub is the top-level SHADOW scalar (same bare name as the element's SUB
	// array): 999 nowhere matches a Z value; T4(2) carries sub=20 so the
	// depth-3 shadow straddle T4.SUB=Z has EXACTLY ONE positive match — a
	// slot-misread to ID would instead match T4(11) (ID=11, Z=11).
	mkT4 := func(id, sub int64, sarr ...protoreflect.Value) proto.Message {
		m := dynamicpb.NewMessage(t4Desc)
		m.Set(t4Desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(t4Desc.Fields().ByName("SUB"), protoreflect.ValueOfInt64(sub))
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
			mkT4(1, 999, mkElem([]int32{1, 7}, mkElem2(11, 12), mkElem2(13))),
			mkT4(2, 20, mkElem([]int32{9}, mkElem2(20))),
			mkT4(11, 999, mkElem([]int32{5}, mkElem2(11))), // ID=11 matches Z=11 (straddle detector)
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
	// assertOrdinal3Link pins the 3-link NESTING signature: exactly THREE Explode
	// legs (one per link) over a bare Scan(T4) base — NOT a NestedLoopJoin box.
	// This does NOT discriminate ordinal from name-model (a name-model 3-link
	// produces the same shape); it excludes the box-base shape and pins that no
	// flattening/restructure regression collapses the per-link Explodes. Robust to
	// interposed PredicatesFilters (a pushed-down conjunct wraps a middle FlatMap).
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

	// LONE outer filter: ID=1 → {11,12,13}. The scan-pushable conjunct stays lazy
	// (⊆-outerLegs) and SARGs the scan — Scan(T4, [=]). The SARG is an OPTIMIZATION
	// pin, NOT a model discriminator (the lazy path precedes the seed-form fork, so
	// the name model SARGs identically); it pins that the filter merge never
	// regresses the pushable conjunct to a residual filter.
	q0 := `SELECT "Z" ` + base + ` WHERE T4."ID" = 1`
	ex0, r0 := run("threelink_outer_filter_SARGs", q0)
	assertOrdinal3Link("threelink_outer_filter_SARGs", ex0)
	if !strings.Contains(ex0, "Scan(T4, [=") {
		t.Fatalf("threelink_outer_filter_SARGs: lone outer conjunct must SARG the scan; plan=%s", ex0)
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

	// FORK chain: W's owner is X, TWO links back — not the immediately preceding Y.
	// The unscoped gate lift admitted this and rooted W's collection at Y's element
	// slot (elementRootIdx = the PRECEDING link), descending SUB on ELEM2 → loud
	// "ordinal -1" malformed plan; with colliding field names it would have been
	// SILENTLY WRONG rows. The owner-linearity check declines the fork to the
	// name-model residual, which resolves each owner BY NAME: cross-product of Y's
	// SUBSTRUCT (2 elems on T4(1)) × W's SUB. T4(1): {1,7}×2; T4(2): {9}; T4(11): {5}.
	q7 := `SELECT "W" FROM T4, T4."SARR" AS "X", "X"."SUBSTRUCT" AS "Y", "X"."SUB" AS "W"`
	_, r7 := run("fork_projection_declines_namemodel", q7)
	wantRows("fork_projection_declines_namemodel", r7,
		[]string{"map[W:1]", "map[W:1]", "map[W:5]", "map[W:7]", "map[W:7]", "map[W:9]"}, q7)

	// The FILTERED fork: on the unscoped cut this only survived through the very
	// box-leg-conjunct over-decline the slice removes — with the arm scoped to
	// boxes, the LINEARITY check alone must keep the fork name-model and correct.
	q8 := `SELECT "W" FROM T4, T4."SARR" AS "X", "X"."SUBSTRUCT" AS "Y", "X"."SUB" AS "W" WHERE T4."ID" = 1`
	_, r8 := run("fork_filtered_declines_namemodel", q8)
	wantRows("fork_filtered_declines_namemodel", r8,
		[]string{"map[W:1]", "map[W:1]", "map[W:7]", "map[W:7]"}, q8)

	// DEPTH-3 SHADOW-SLOT straddle: T4.SUB (the outer shadow scalar, slot 3 of the
	// ordinal row) = Z. Exactly T4(2) matches (sub=20, Z=20) → {20}. THE positive
	// slot-correctness pin for the depth-3 positional bake: a slot-0/ID misread
	// returns {11} (T4(11): ID=11, Z=11), a bare-name mis-resolution to the
	// element's SUB array errors — each failure mode is distinguishable.
	q9 := `SELECT "Z" ` + base + ` WHERE T4."SUB" = "Z"`
	_, r9 := run("threelink_shadow_slot_straddle", q9)
	wantRows("threelink_shadow_slot_straddle", r9, []string{"map[Z:20]"}, q9)

	// The shadow straddle under OR (rides the CNF/pushdown path): (T4.SUB=Z) OR
	// (T4.ID=1) → {20} ∪ {11,12,13}.
	q10 := `SELECT "Z" ` + base + ` WHERE T4."SUB" = "Z" OR T4."ID" = 1`
	_, r10 := run("threelink_shadow_in_or", q10)
	wantRows("threelink_shadow_in_or", r10,
		[]string{"map[Z:11]", "map[Z:12]", "map[Z:13]", "map[Z:20]"}, q10)

	// AT-ordinality rows at depth 3: AT on the FIRST link (every T4 has one SARR
	// element → P=1 throughout) and AT on the MID link (T4(1) has two SUBSTRUCT
	// elements → P=2 for Z=13 — the discriminating ordinal).
	q11 := `SELECT "P", "Z" FROM T4, T4."SARR" AS "X" AT "P", "X"."SUBSTRUCT" AS "Y", "Y"."DEEP" AS "Z"`
	_, r11 := run("threelink_at_first_link", q11)
	wantRows("threelink_at_first_link", r11,
		[]string{"map[P:1 Z:11]", "map[P:1 Z:11]", "map[P:1 Z:12]", "map[P:1 Z:13]", "map[P:1 Z:20]"}, q11)
	q12 := `SELECT "P", "Z" FROM T4, T4."SARR" AS "X", "X"."SUBSTRUCT" AS "Y" AT "P", "Y"."DEEP" AS "Z"`
	_, r12 := run("threelink_at_mid_link", q12)
	wantRows("threelink_at_mid_link", r12,
		[]string{"map[P:1 Z:11]", "map[P:1 Z:11]", "map[P:1 Z:12]", "map[P:1 Z:20]", "map[P:2 Z:13]"}, q12)

	// The FULL-BOX-BOTTOM chained spine with a box-leg WHERE — the silent-wrong
	// composition the pureSpine bit closes: the spine bottoms in a FULL box
	// (clusterArity 1 → ADMITTED), but its bottom aliases are BOX LEGS, so the
	// box-leg-conjunct arm must stay active and decline the WHOLE chain to
	// name-model coherently with the first link's own gate. An incoherent split
	// ordinalizes the chained link over the first link's name-model seed and
	// returns WRONG A-side values. ON matches (A=1,B=11); A(1).SARR SUB={1,7}.
	q13 := `SELECT "A"."ID", "B"."ID", "Y" FROM T4 AS "A" FULL OUTER JOIN T4 AS "B" ON "A"."ID" + 10 = "B"."ID", "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "A"."ID" = 1`
	_, r13 := run("fullbox_bottom_boxleg_filter", q13)
	wantRows("fullbox_bottom_boxleg_filter", r13,
		[]string{"map[A.ID:1 B.ID:11 Y:1]", "map[A.ID:1 B.ID:11 Y:7]"}, q13)
}
