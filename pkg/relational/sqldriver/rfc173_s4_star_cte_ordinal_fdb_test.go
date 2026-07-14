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
	rquery "fdb.dev/pkg/relational/core/query"
)

// The STAR-BODIED CTE/derived-table OPAQUE ORDINAL LEG (RFC-173 S4, item B —
// derivedBodyStarOrdinalLeg): a projection-less single-link unnest body
// (`WITH S AS (SELECT * FROM t, t.arr AS x) …`) referenced as a JOIN LEG now
// gates ordinal — the parent seeds positionally over the body's own seed
// labels — instead of name-modeling the parent (1 P4) and the enclosed body
// (1 P5). This was the GraefeImplProbe2 Q5 class (whose star-body rows remain
// pinned there); this file pins the wider shape family's rows plus the
// producer retirement.
//
// Data (SCARR is the scalar array; T4(11) has an EMPTY SCARR so it vanishes
// from every unnest; the top-level SUB scalar is the shadow column):
//
//	T4(1)  SUB=999 SCARR=[100,200]
//	T4(2)  SUB=20  SCARR=[300]
//	T4(11) SUB=999 SCARR=[]
//
// CC scans the same table (IDs 1, 2, 11).
func TestFDB_RFC173S4_StarCTEOrdinalLeg(t *testing.T) {
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
	if err := saveStarCTERows(ctx, db, ks, md); err != nil {
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

	const starCTE = `WITH "S" AS (SELECT * FROM T4, T4."SCARR" AS "X") `

	// The Q5 class: qualified reads over the enclosed star leg. S rows:
	// (1,100),(1,200),(2,300) — T4(11)'s empty SCARR contributes nothing —
	// × 3 CC rows.
	want("enclosed_qualified", starCTE+`SELECT "S"."ID", "S"."X" FROM "S", T4 AS "CC"`, []string{
		"map[S.ID:1 S.X:100]", "map[S.ID:1 S.X:100]", "map[S.ID:1 S.X:100]",
		"map[S.ID:1 S.X:200]", "map[S.ID:1 S.X:200]", "map[S.ID:1 S.X:200]",
		"map[S.ID:2 S.X:300]", "map[S.ID:2 S.X:300]", "map[S.ID:2 S.X:300]",
	})

	// A BARE unique-name read over the enclosed leg.
	want("enclosed_bare_x", starCTE+`SELECT "X" FROM "S", T4 AS "CC"`, []string{
		"map[X:100]", "map[X:100]", "map[X:100]",
		"map[X:200]", "map[X:200]", "map[X:200]",
		"map[X:300]", "map[X:300]", "map[X:300]",
	})

	// WITH ORDINALITY in the body: (X,O) = (100,1),(200,2),(300,1), × 3 CC rows.
	want("at_body", `WITH "S" AS (SELECT * FROM T4, T4."SCARR" AS "X" AT "O") SELECT "S"."X", "S"."O" FROM "S", T4 AS "CC"`, []string{
		"map[S.O:1 S.X:100]", "map[S.O:1 S.X:100]", "map[S.O:1 S.X:100]",
		"map[S.O:2 S.X:200]", "map[S.O:2 S.X:200]", "map[S.O:2 S.X:200]",
		"map[S.O:1 S.X:300]", "map[S.O:1 S.X:300]", "map[S.O:1 S.X:300]",
	})

	// A body WHERE (plain filter above the unnest join): S = (1,200),(2,300).
	want("where_body", `WITH "S" AS (SELECT * FROM T4, T4."SCARR" AS "X" WHERE "X" > 150) SELECT "S"."ID", "S"."X" FROM "S", T4 AS "CC"`, []string{
		"map[S.ID:1 S.X:200]", "map[S.ID:1 S.X:200]", "map[S.ID:1 S.X:200]",
		"map[S.ID:2 S.X:300]", "map[S.ID:2 S.X:300]", "map[S.ID:2 S.X:300]",
	})

	// The DERIVED-TABLE twin (LogicalCTE directly in leg position). Its runtime
	// row's INTERNAL field names are the bare column names (the derived-boundary
	// projection resolves the qualified reads to the leg's own columns), unlike
	// the WITH form's qualified S.ID/S.X — but the DRIVER-visible labels are
	// bare [ID X] for BOTH forms, unchanged from the name model (pinned below).
	want("derived_twin", `SELECT "S"."ID", "S"."X" FROM (SELECT * FROM T4, T4."SCARR" AS "X") AS "S", T4 AS "CC"`, []string{
		"map[ID:1 X:100]", "map[ID:1 X:100]", "map[ID:1 X:100]",
		"map[ID:1 X:200]", "map[ID:1 X:200]", "map[ID:1 X:200]",
		"map[ID:2 X:300]", "map[ID:2 X:300]", "map[ID:2 X:300]",
	})
	t.Run("driver_labels_bare_both_forms", func(t *testing.T) {
		for _, q := range []string{
			`SELECT "S"."ID", "S"."X" FROM (SELECT * FROM T4, T4."SCARR" AS "X") AS "S", T4 AS "CC"`,
			starCTE + `SELECT "S"."ID", "S"."X" FROM "S", T4 AS "CC"`,
		} {
			plan, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
			if perr != nil {
				t.Fatalf("plan %q: %v", q, perr)
			}
			if got := fmt.Sprintf("%v", embedded.ResultColumnLabelsForPlan(plan, md)); got != "[ID X]" {
				t.Fatalf("driver labels = %s, want [ID X] (the name model's labels)\n  sql: %s", got, q)
			}
		}
	})

	// The COLLIDING-label body (`… AS "SUB"` — the element alias shadows the
	// outer scalar SUB=999/20) DECLINES the star admission and stays
	// name-model: the element's bare key must keep WINNING the collision
	// (last-write-wins), never the outer scalar — a positional first-match
	// would serve 999/20. Since it declines, ONLY the name-model path runs here;
	// the rows pin that name-model shadow semantics, which the ordinal admission
	// deliberately declines (unique-label guard) to preserve.
	want("colliding_label_shadow", `WITH "S2" AS (SELECT * FROM T4, T4."SCARR" AS "SUB") SELECT "S2"."SUB" FROM "S2", T4 AS "CC"`, []string{
		"map[S2.SUB:100]", "map[S2.SUB:100]", "map[S2.SUB:100]",
		"map[S2.SUB:200]", "map[S2.SUB:200]", "map[S2.SUB:200]",
		"map[S2.SUB:300]", "map[S2.SUB:300]", "map[S2.SUB:300]",
	})

	// A PARENT WHERE over a join with a join/unnest-bodied CTE leg is a
	// PRE-EXISTING loud reach gap INDEPENDENT of the star admission: the
	// resolver cannot derive the CTE's column schema for the WHERE scope
	// (the buildCTEColumnSource decline — the bare-projected class
	// `WITH D AS (SELECT "X" AS "XV" …) … WHERE D."XV" = 1` fails the same
	// way, verified pre-change), so the predicate never resolves and the
	// filter loud-declines (0AF00). Pinned so the gap stays LOUD and its
	// eventual fix consciously updates this.
	t.Run("parent_where_preexisting_loud", func(t *testing.T) {
		q := starCTE + `SELECT "S"."X" FROM "S", T4 AS "CC" WHERE "CC"."ID" = 2`
		if _, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil); perr == nil {
			t.Fatalf("parent WHERE over a CTE-leg join planned — the resolver reach gap closed; " +
				"add row assertions for the parent-WHERE shapes (cross-leg straddle included)")
		}
	})
}

// TestFDB_RFC173S4_ChainedStarCTE pins the CHAINED star body's rows (RFC-173 S4
// item B, the second-to-last producer residual): a projection-less multi-link
// unnest body (`WITH S AS (SELECT * FROM t, t.sarr AS x, x.sub AS y) …`)
// referenced as a JOIN LEG now gates ordinal through the SAME factored gate its
// fresh translation runs (chainedUnnestOrdinalGate via derivedBodyStarOrdinalLeg)
// instead of name-modeling the parent (1 P4). DIFFERENTIAL: every row set below
// was captured VERBATIM from the pre-change name-model tree (same fixture) —
// the ordinalization changes plans, never rows.
//
// SARR fixture: T4(1)=[{SUB:[1,2]},{SUB:[3]}], T4(2)=[{SUB:[4]}], T4(11) empty.
// Chained rows (ID,Y): (1,1),(1,2),(1,3),(2,4) — × 3 CC rows for the parent join.
func TestFDB_RFC173S4_ChainedStarCTE(t *testing.T) {
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
	if err := saveStarCTERows(ctx, db, ks, md); err != nil {
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
	times3 := func(rows ...string) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r, r, r)
		}
		return out
	}

	const chainedCTE = `WITH "S" AS (SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y") `

	want("qualified_y", chainedCTE+`SELECT "S"."Y" FROM "S", T4 AS "CC"`,
		times3("map[S.Y:1]", "map[S.Y:2]", "map[S.Y:3]", "map[S.Y:4]"))

	want("qualified_id", chainedCTE+`SELECT "S"."ID" FROM "S", T4 AS "CC"`,
		times3("map[S.ID:1]", "map[S.ID:1]", "map[S.ID:1]", "map[S.ID:2]"))

	want("id_and_y", chainedCTE+`SELECT "S"."ID", "S"."Y" FROM "S", T4 AS "CC"`,
		times3("map[S.ID:1 S.Y:1]", "map[S.ID:1 S.Y:2]", "map[S.ID:1 S.Y:3]", "map[S.ID:2 S.Y:4]"))

	// A BARE unique-name read over the enclosed chained leg.
	want("bare_y", chainedCTE+`SELECT "Y" FROM "S", T4 AS "CC"`,
		times3("map[Y:1]", "map[Y:2]", "map[Y:3]", "map[Y:4]"))

	// WITH ORDINALITY on the chained link: (Y,O) = (1,1),(2,2) within elem{1,2};
	// (3,1) within elem{3}; (4,1) within elem{4}.
	want("at_chained_link", `WITH "S" AS (SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" AT "O") SELECT "S"."Y", "S"."O" FROM "S", T4 AS "CC"`,
		times3("map[S.O:1 S.Y:1]", "map[S.O:2 S.Y:2]", "map[S.O:1 S.Y:3]", "map[S.O:1 S.Y:4]"))

	// A body WHERE (plain filter above the chained spine): Y > 2 keeps {3,4}.
	want("where_body", `WITH "S" AS (SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE "Y" > 2) SELECT "S"."Y" FROM "S", T4 AS "CC"`,
		times3("map[S.Y:3]", "map[S.Y:4]"))

	// The DERIVED-TABLE twin (LogicalCTE directly in leg position) — bare
	// internal labels, same values (the single-link star class's twin rule).
	want("derived_twin", `SELECT "S"."Y" FROM (SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y") AS "S", T4 AS "CC"`,
		times3("map[Y:1]", "map[Y:2]", "map[Y:3]", "map[Y:4]"))
}

// TestRFC173StarCTEOrdinalCensus pins the producer retirement of the star-CTE
// class: every admitted shape plans AND fires 0 P4/P5. RED-ON-REVERT (of
// derivedBodyStarOrdinalLeg + its gate/schema arms): each single-link shape
// fires 2 (1 P4 name-model parent + 1 P5 enclosed body) and each chained shape
// fires 1 (P4 — the chained body itself ordinalizes fresh). The COLLIDING-label
// bodies (single-link and chained) are the documented residuals (they keep the
// name model, so they still fire — asserted non-zero so their eventual
// ordinalization consciously updates this pin). Serial (process-global observer).
func TestRFC173StarCTEOrdinalCensus(t *testing.T) { //nolint:paralleltest // process-global observer, must be serial
	md := buildChainedUnnestMetadata(t)
	count := func(sql string) (int, error) {
		n := 0
		rquery.SetProducerCensusObserver(func(rquery.ProducerCensusRecord) { n++ })
		defer rquery.SetProducerCensusObserver(nil)
		_, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		return n, err
	}

	zeroed := []struct{ name, sql string }{
		{"enclosed_qualified", `WITH "S" AS (SELECT * FROM T4, T4."SCARR" AS "X") SELECT "S"."ID", "S"."X" FROM "S", T4 AS "CC"`},
		{"at_body", `WITH "S" AS (SELECT * FROM T4, T4."SCARR" AS "X" AT "O") SELECT "S"."X", "S"."O" FROM "S", T4 AS "CC"`},
		{"where_body", `WITH "S" AS (SELECT * FROM T4, T4."SCARR" AS "X" WHERE "X" > 150) SELECT "S"."ID" FROM "S", T4 AS "CC"`},
		{"derived_twin", `SELECT "S"."ID" FROM (SELECT * FROM T4, T4."SCARR" AS "X") AS "S", T4 AS "CC"`},
		{"struct_array_body", `WITH "S" AS (SELECT * FROM T4, T4."SARR" AS "X") SELECT "S"."ID" FROM "S", T4 AS "CC"`},
		// The CHAINED star body (multi-link spine) — admitted through the
		// factored chainedUnnestOrdinalGate; RED-on-revert: 1 P4 each (the
		// chained body itself ordinalizes fresh, only the parent name-modeled).
		{"chained_star", `WITH "S" AS (SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y") SELECT "S"."Y" FROM "S", T4 AS "CC"`},
		{"chained_star_at", `WITH "S" AS (SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" AT "O") SELECT "S"."Y", "S"."O" FROM "S", T4 AS "CC"`},
		{"chained_star_where", `WITH "S" AS (SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE "Y" > 2) SELECT "S"."Y" FROM "S", T4 AS "CC"`},
		{"chained_star_derived_twin", `SELECT "S"."Y" FROM (SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y") AS "S", T4 AS "CC"`},
	}
	for _, tc := range zeroed {
		n, err := count(tc.sql)
		if err != nil {
			t.Errorf("%s: plan error (should ordinalize cleanly): %v\n  sql: %s", tc.name, err, tc.sql)
			continue
		}
		if n != 0 {
			t.Errorf("%s: fired %d P4/P5 producer(s), want 0 (the star-CTE opaque ordinal leg regressed "+
				"to the name model)\n  sql: %s", tc.name, n, tc.sql)
		}
	}

	// Documented residuals — still name-model, still firing (RFC-173 item B
	// remainder). When one of these ordinalizes, move it to the zeroed list.
	residual := []struct{ name, sql string }{
		// Colliding boundary labels: positional first-match would invert the
		// element-shadows-outer collision rule, so the admission declines.
		{"colliding_label", `WITH "S2" AS (SELECT * FROM T4, T4."SCARR" AS "SUB") SELECT "S2"."SUB" FROM "S2", T4 AS "CC"`},
		// The chained twin of the colliding-label body (the element alias SUB
		// shadows the top-level scalar SUB) — same unique-label decline.
		{"chained_colliding_label", `WITH "S" AS (SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "SUB") SELECT "S"."SUB" FROM "S", T4 AS "CC"`},
	}
	for _, tc := range residual {
		n, err := count(tc.sql)
		if err != nil {
			t.Errorf("%s: plan error: %v\n  sql: %s", tc.name, err, tc.sql)
			continue
		}
		if n == 0 {
			t.Errorf("%s: fired 0 producers — the residual ordinalized; move it to the zeroed list "+
				"(and verify its rows against the name-model shadow semantics)\n  sql: %s", tc.name, tc.sql)
		}
	}
}

// saveStarCTERows writes the shared T4 fixture rows for the star-CTE tests.
// SCARR carries the scalar-unnest values; SARR carries struct elements
// ({SUB:[…]}) for the CHAINED star shapes (T4(1)=[{1,2},{3}], T4(2)=[{4}],
// T4(11) empty — it vanishes from every unnest).
func saveStarCTERows(ctx context.Context, db *recordlayer.FDBDatabase, ks subspace.Subspace, md *recordlayer.RecordMetaData) error {
	t4Desc := md.GetRecordType("T4").Descriptor
	scarrFD := t4Desc.Fields().ByName("SCARR")
	sarrFD := t4Desc.Fields().ByName("SARR")
	elemDesc := sarrFD.Message()
	mkElem := func(sub ...int32) protoreflect.Value {
		m := dynamicpb.NewMessage(elemDesc)
		sl := m.NewField(elemDesc.Fields().ByName("SUB")).List()
		for _, s := range sub {
			sl.Append(protoreflect.ValueOfInt32(s))
		}
		m.Set(elemDesc.Fields().ByName("SUB"), protoreflect.ValueOfList(sl))
		return protoreflect.ValueOfMessage(m)
	}
	mkT4 := func(id, sub int64, scarr []int32, sarr ...protoreflect.Value) proto.Message {
		m := dynamicpb.NewMessage(t4Desc)
		m.Set(t4Desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(t4Desc.Fields().ByName("SUB"), protoreflect.ValueOfInt64(sub))
		sl := m.NewField(scarrFD).List()
		for _, s := range scarr {
			sl.Append(protoreflect.ValueOfInt32(s))
		}
		m.Set(scarrFD, protoreflect.ValueOfList(sl))
		al := m.NewField(sarrFD).List()
		for _, e := range sarr {
			al.Append(e)
		}
		m.Set(sarrFD, protoreflect.ValueOfList(al))
		return m
	}
	_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		for _, r := range []proto.Message{
			mkT4(1, 999, []int32{100, 200}, mkElem(1, 2), mkElem(3)),
			mkT4(2, 20, []int32{300}, mkElem(4)),
			mkT4(11, 999, nil),
		} {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	})
	return err
}
