package sqldriver_test

// Permanent regression pins for CTE double-reference over a box (array)
// unnest, and for scope/ON-clause resolution over CTE and derived-table legs
// more broadly. These are exactly the shapes that hid real bugs: a shared
// CTE body referenced twice, box unnest mixed with LEFT/FULL JOIN, and
// predicates that must (or must not) evaluate inside the join's
// positional/name-keyed row.
//  P1  CTE double-reference: a CTE body containing the filtered box unnest is
//      ONE shared logical tree; two references translate the SAME node twice in
//      one translator. The unnestGatherBoxLegTypes record is CONSUME-ONCE (the
//      enclosedGatherCache discipline) precisely so a later translation whose
//      gather declined (enclosure) can never bake over a stale record — a bug
//      here surfaces as a loud "baked FieldValue evaluated against a
//      non-positional row context". Both legs must answer correct rows: the
//      enclosed leg's qualified reads resolve via the schema-complete merge
//      fabrication (executor qualifyAlias/qualifyOuterRow on Complete legs).
//  P2  FULL box + IS NULL on each leg (the doubly-null class).
//  P3  OR mixing the ELEMENT and a box leg.
//  P4  NOT over the null-supplied leg (3VL: NULL drops).
//  P5  IS NOT NULL on the preserved leg's ARRAY column (non-scalar bake).
//  P6  scalar subquery in ONE conjunct AND a plain box-leg conjunct in the
//      other (whole-pred Unbakeable — stays name-model, correct rows).

import (
	"context"
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

func TestFDB_CTEBoxUnnestOnResolutionProbe(t *testing.T) {
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

	b := metadata.NewSchemaTemplateBuilder().SetName("b2gip")
	b.AddTable("LA", []metadata.ColumnSpec{
		metadata.NewColumnSpec("AID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), true), 3),
	}, []string{"AID"})
	b.AddTable("LB", []metadata.ColumnSpec{
		metadata.NewColumnSpec("BID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
	}, []string{"BID"})
	b.AddTable("CC", []metadata.ColumnSpec{
		metadata.NewColumnSpec("CID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("CV", api.NewLongType(true), 2),
	}, []string{"CID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	md := tmpl.Underlying()

	mkLA := func(aid, k int64, vals ...int32) proto.Message {
		d := md.GetRecordType("LA").Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName("AID"), protoreflect.ValueOfInt64(aid))
		m.Set(d.Fields().ByName("K"), protoreflect.ValueOfInt64(k))
		arrVals := make([]protoreflect.Value, 0, len(vals))
		for _, v := range vals {
			arrVals = append(arrVals, protoreflect.ValueOfInt32(v))
		}
		setArrayField(m, d.Fields().ByName("ARR"), arrVals...)
		return m
	}
	mk2 := func(table, f1, f2 string, v1, v2 int64) proto.Message {
		d := md.GetRecordType(table).Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName(protoreflect.Name(f1)), protoreflect.ValueOfInt64(v1))
		m.Set(d.Fields().ByName(protoreflect.Name(f2)), protoreflect.ValueOfInt64(v2))
		return m
	}

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Create()
		if sErr != nil {
			return nil, sErr
		}
		// LA: aid=1 K=100 arr[7,8] (matches LB bid=1); aid=2 K=110 arr[9] (unmatched).
		// LB: bid=1 K=5; bid=3 K=6 (unmatched). CC: cid=1 cv=900.
		for _, r := range []proto.Message{
			mkLA(1, 100, 7, 8), mkLA(2, 110, 9),
			mk2("LB", "BID", "K", 1, 5), mk2("LB", "BID", "K", 3, 6),
			mk2("CC", "CID", "CV", 1, 900),
		} {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, sql string) ([]string, error) {
		t.Helper()
		plan, subs, perr := embedded.PlanRecordQueryWithSubqueries(sql, md, nil)
		if perr != nil {
			return nil, perr
		}
		var out []string
		_, eerr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if sErr != nil {
				return nil, sErr
			}
			evalCtx, bindErr := prebindScalarSubqueries(ctx, store, subs)
			if bindErr != nil {
				return nil, bindErr
			}
			cursor, cErr := executor.ExecutePlan(ctx, plan, store, evalCtx, nil, recordlayer.DefaultExecuteProperties())
			if cErr != nil {
				return nil, cErr
			}
			defer cursor.Close()
			rows, rErr := executor.CollectAll(ctx, cursor)
			if rErr != nil {
				return nil, rErr
			}
			for _, r := range rows {
				// POSITIONAL, in slot order. The name-keyed form this replaced sorted
				// the row map's keys, so permuting (Fields, Slots) together rendered
				// identically -- blind in the one dimension a mis-bound leg window
				// moves.
				out = append(out, positionalPipeSprint(r))
			}
			return nil, nil
		})
		if eerr != nil {
			return nil, eerr
		}
		sort.Strings(out)
		return out, nil
	}
	want := func(t *testing.T, sql string, expect ...string) {
		t.Helper()
		got, err := run(t, sql)
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		sort.Strings(expect)
		if strings.Join(got, ",") != strings.Join(expect, ",") {
			t.Fatalf("rows = %v, want %v\n  %s", got, expect, sql)
		}
	}

	const leftBox = `FROM LA LEFT JOIN LB ON LA."AID" = LB."BID", LA."ARR" AS "X"`
	const fullBox = `FROM LA FULL OUTER JOIN LB ON LA."AID" = LB."BID", LA."ARR" AS "X"`
	const cteBody = `SELECT LA."K" AS "AK", LB."K" AS "BK", "X" AS "XV" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID", LA."ARR" AS "X" WHERE LA."K" = 100`

	// P1a: un-enclosed reference FIRST (the gather records; consume-once eats
	// the record), enclosed second (the gather is skipped; NO stale record
	// fires — the pre-fix behavior was a loud "non-positional row context"
	// error here). BOTH legs answer the correct rows: the enclosed leg's
	// qualified reads resolve through the schema-complete merge fabrication
	// (qualifyAlias on a Complete projection-output leg) — the sentinel that
	// used to pin all-NULL rows here flipped when that executor fix landed.
	t.Run("P1a_cte_double_ref_unenclosed_then_enclosed", func(t *testing.T) {
		want(t, `WITH "C" AS (`+cteBody+`) SELECT "AK", "BK", "XV" FROM "C" UNION ALL SELECT "C"."AK", "C"."BK", "C"."XV" FROM "C", CC`,
			"100|5|7", "100|5|8", "100|5|7", "100|5|8")
	})
	// P1b: the reverse order — same consume-once guarantee, same correct rows
	// on both legs.
	t.Run("P1b_cte_double_ref_enclosed_then_unenclosed", func(t *testing.T) {
		want(t, `WITH "C" AS (`+cteBody+`) SELECT "C"."AK", "C"."BK", "C"."XV" FROM "C", CC UNION ALL SELECT "AK", "BK", "XV" FROM "C"`,
			"100|5|7", "100|5|8", "100|5|7", "100|5|8")
	})
	// P2: FULL box + IS NULL, each leg. LB-only rows have LA NULL → NULL ARR
	// explodes to zero rows, so only the LA-unmatched row survives LB.K IS NULL,
	// and LA.K IS NULL yields nothing.
	t.Run("P2a_full_box_null_supplied_is_null", func(t *testing.T) {
		want(t, `SELECT LA."K", LB."K", "X" `+fullBox+` WHERE LB."K" IS NULL`, "110|<nil>|9")
	})
	t.Run("P2b_full_box_preserved_is_null", func(t *testing.T) {
		want(t, `SELECT LA."K", LB."K", "X" `+fullBox+` WHERE LA."K" IS NULL`)
	})
	// P3: OR mixing the element and a box leg.
	t.Run("P3_or_element_and_leg", func(t *testing.T) {
		want(t, `SELECT LA."K", LB."K", "X" `+leftBox+` WHERE "X" = 9 OR LB."K" = 5`,
			"100|5|7", "100|5|8", "110|<nil>|9")
	})
	// P4: NOT over the null-supplied leg — 3VL: NOT(NULL = 5) is NULL → drops.
	t.Run("P4_not_null_supplied_3vl", func(t *testing.T) {
		want(t, `SELECT LA."K", LB."K", "X" `+leftBox+` WHERE NOT (LB."K" = 5)`)
	})
	// P5: IS NOT NULL on the preserved leg's ARRAY column (non-scalar bake).
	t.Run("P5_array_col_is_not_null", func(t *testing.T) {
		want(t, `SELECT LA."K", LB."K", "X" `+leftBox+` WHERE LA."ARR" IS NOT NULL`,
			"100|5|7", "100|5|8", "110|<nil>|9")
	})
	// P6: scalar subquery in one conjunct AND a plain box-leg conjunct in the
	// other — the WHOLE predicate must go Unbakeable (name-model, correct rows:
	// MAX(CV)=900 matches no LB.K → empty).
	t.Run("P6_mixed_subquery_and_leg_conjunct", func(t *testing.T) {
		want(t, `SELECT LA."K", LB."K", "X" `+leftBox+` WHERE LA."K" = 100 AND LB."K" = (SELECT MAX("CV") FROM CC)`)
	})
}

// Isolation probes for the P1 failures: single ENCLOSED reference (no double
// ref — no record can exist), and the WHERE-free body (is the FILTER the
// trigger or is the enclosed CTE box unnest broken generally?).
func TestFDB_CTEBoxUnnestOnResolutionProbe2(t *testing.T) {
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

	b := metadata.NewSchemaTemplateBuilder().SetName("b2gip2")
	b.AddTable("LA", []metadata.ColumnSpec{
		metadata.NewColumnSpec("AID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), true), 3),
	}, []string{"AID"})
	b.AddTable("LB", []metadata.ColumnSpec{
		metadata.NewColumnSpec("BID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
	}, []string{"BID"})
	b.AddTable("CC", []metadata.ColumnSpec{
		metadata.NewColumnSpec("CID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("CV", api.NewLongType(true), 2),
	}, []string{"CID"})
	// CD keys on an ARRAY-ELEMENT value (7 ∈ LA.ARR) — the Q26 discriminator.
	// A separate table so its row can't disturb the CC cross-join cardinality
	// the Q1-Q5 pins depend on.
	b.AddTable("CD", []metadata.ColumnSpec{
		metadata.NewColumnSpec("XK", api.NewLongType(false), 1),
		metadata.NewColumnSpec("XV", api.NewLongType(true), 2),
	}, []string{"XK"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	md := tmpl.Underlying()

	mkLA := func(aid, k int64, vals ...int32) proto.Message {
		d := md.GetRecordType("LA").Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName("AID"), protoreflect.ValueOfInt64(aid))
		m.Set(d.Fields().ByName("K"), protoreflect.ValueOfInt64(k))
		arrVals := make([]protoreflect.Value, 0, len(vals))
		for _, v := range vals {
			arrVals = append(arrVals, protoreflect.ValueOfInt32(v))
		}
		setArrayField(m, d.Fields().ByName("ARR"), arrVals...)
		return m
	}
	mk2 := func(table, f1, f2 string, v1, v2 int64) proto.Message {
		d := md.GetRecordType(table).Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName(protoreflect.Name(f1)), protoreflect.ValueOfInt64(v1))
		m.Set(d.Fields().ByName(protoreflect.Name(f2)), protoreflect.ValueOfInt64(v2))
		return m
	}

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Create()
		if sErr != nil {
			return nil, sErr
		}
		for _, r := range []proto.Message{
			mkLA(1, 100, 7, 8), mkLA(2, 110, 9),
			mk2("LB", "BID", "K", 1, 5), mk2("LB", "BID", "K", 3, 6),
			mk2("CC", "CID", "CV", 1, 900),
			mk2("CD", "XK", "XV", 7, 700),
		} {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	// runPlanned returns the emitted rows AND the plan that produced them. An
	// order assertion whose failure shows only the rows makes every
	// re-derivation of "which plan emitted this" a fresh investigation, and the
	// answer is the one fact that decides whether the order changed because the
	// SORT changed or because the JOIN did.
	runPlanned := func(t *testing.T, sql string) ([]string, string, error) {
		t.Helper()
		plan, subs, perr := embedded.PlanRecordQueryWithSubqueries(sql, md, nil)
		if perr != nil {
			return nil, "", perr
		}
		planText := plan.Explain()
		var out []string
		_, eerr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if sErr != nil {
				return nil, sErr
			}
			evalCtx, bindErr := prebindScalarSubqueries(ctx, store, subs)
			if bindErr != nil {
				return nil, bindErr
			}
			cursor, cErr := executor.ExecutePlan(ctx, plan, store, evalCtx, nil, recordlayer.DefaultExecuteProperties())
			if cErr != nil {
				return nil, cErr
			}
			defer cursor.Close()
			rows, rErr := executor.CollectAll(ctx, cursor)
			if rErr != nil {
				return nil, rErr
			}
			for _, r := range rows {
				// POSITIONAL, in slot order. The name-keyed form this replaced sorted
				// the row map's keys, so permuting (Fields, Slots) together rendered
				// identically -- blind in the one dimension a mis-bound leg window
				// moves.
				out = append(out, positionalPipeSprint(r))
			}
			return nil, nil
		})
		if eerr != nil {
			return nil, planText, eerr
		}
		return out, planText, nil
	}
	run := func(t *testing.T, sql string) ([]string, error) {
		t.Helper()
		out, _, err := runPlanned(t, sql)
		return out, err
	}
	check := func(t *testing.T, sql string, expect ...string) {
		t.Helper()
		got, err := run(t, sql)
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		sort.Strings(got)
		sort.Strings(expect)
		if strings.Join(got, ",") != strings.Join(expect, ",") {
			t.Fatalf("rows = %v, want %v\n  %s", got, expect, sql)
		}
	}
	// checkOrdered compares the cursor's EMISSION ORDER (no sorting) — the
	// ORDER BY pin's whole point: a missing merged key made ORDER BY a silent
	// no-op sort, so the order itself is the discriminating assertion.
	checkOrdered := func(t *testing.T, sql string, expect ...string) {
		t.Helper()
		got, planText, err := runPlanned(t, sql)
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		if strings.Join(got, ",") != strings.Join(expect, ",") {
			t.Fatalf("ordered rows = %v, want %v\n  %s\n  plan: %s", got, expect, sql, planText)
		}
	}
	// checkOrderedSlot is checkOrdered for a query whose ORDER BY determines
	// only ONE column: it asserts that column's EMISSION SEQUENCE exactly (the
	// ordering assertion) plus the row MULTISET (nothing lost, nothing
	// duplicated), and says nothing about the order of rows the query itself
	// leaves free.
	//
	// The distinction is load-bearing, not a relaxation. `ORDER BY K` over a
	// join fixes the K sequence and nothing else; the order WITHIN a K group is
	// decided by the join orientation the planner picks and by the sort's
	// tie-break. Pinning the whole rendered row therefore pins a planner
	// decision the SQL never asked for, and it fails the day a correct new
	// alternative wins the cost race — which reads as an ordering regression
	// and is not one. Assert the sequence that IS determined.
	checkOrderedSlot := func(t *testing.T, sql string, slot int, wantSeq []string, wantRows ...string) {
		t.Helper()
		got, planText, err := runPlanned(t, sql)
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		gotSeq := make([]string, 0, len(got))
		for _, row := range got {
			parts := strings.Split(row, "|")
			if slot >= len(parts) {
				t.Fatalf("row %q has no slot %d\n  %s\n  plan: %s", row, slot, sql, planText)
			}
			gotSeq = append(gotSeq, parts[slot])
		}
		if strings.Join(gotSeq, ",") != strings.Join(wantSeq, ",") {
			t.Fatalf("slot %d sequence = %v, want %v (rows %v)\n  %s\n  plan: %s",
				slot, gotSeq, wantSeq, got, sql, planText)
		}
		gotSorted := append([]string(nil), got...)
		wantSorted := append([]string(nil), wantRows...)
		sort.Strings(gotSorted)
		sort.Strings(wantSorted)
		if strings.Join(gotSorted, ",") != strings.Join(wantSorted, ",") {
			t.Fatalf("rows = %v, want %v\n  %s\n  plan: %s", got, wantRows, sql, planText)
		}
	}

	const cteBody = `SELECT LA."K" AS "AK", LB."K" AS "BK", "X" AS "XV" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID", LA."ARR" AS "X" WHERE LA."K" = 100`
	const cteBodyNoWhere = `SELECT LA."K" AS "AK", LB."K" AS "BK", "X" AS "XV" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID", LA."ARR" AS "X"`

	// Q1: single ENCLOSED reference of the FILTERED body (no double ref — no
	// record can exist). The enclosed leg's qualified reads (C.AK etc.)
	// resolve via the schema-complete merge fabrication — before the
	// executor's qualifyAlias fabricated C.* keys for a projection-output
	// leg, this shape returned all-NULL rows instead of the real values.
	t.Run("Q1_single_enclosed_ref_filtered", func(t *testing.T) {
		check(t, `WITH "C" AS (`+cteBody+`) SELECT "C"."AK", "C"."BK", "C"."XV" FROM "C", CC`,
			"100|5|7", "100|5|8")
	})
	// Q2: single ENCLOSED reference of the UNFILTERED body — proves the
	// resolution is independent of the body's WHERE (the trigger was the
	// enclosed CTE box unnest itself, never the filter). Includes the
	// null-supplied LB row.
	t.Run("Q2_single_enclosed_ref_unfiltered", func(t *testing.T) {
		check(t, `WITH "C" AS (`+cteBodyNoWhere+`) SELECT "C"."AK", "C"."BK", "C"."XV" FROM "C", CC`,
			"100|5|7", "100|5|8", "110|<nil>|9")
	})
	// Q3: single UN-ENCLOSED reference (control — the gathered path, CORRECT).
	t.Run("Q3_single_unenclosed_ref_filtered", func(t *testing.T) {
		check(t, `WITH "C" AS (`+cteBody+`) SELECT "AK", "BK", "XV" FROM "C"`,
			"100|5|7", "100|5|8")
	})
	// Q4: UNFILTERED body double-ref — both legs correct (the un-enclosed leg
	// via the gathered path; the enclosed leg via the merge fabrication).
	t.Run("Q4_unfiltered_double_ref", func(t *testing.T) {
		check(t, `WITH "C" AS (`+cteBodyNoWhere+`) SELECT "AK", "BK", "XV" FROM "C" UNION ALL SELECT "C"."AK", "C"."BK", "C"."XV" FROM "C", CC`,
			"100|5|7", "100|5|8", "110|<nil>|9", "100|5|7", "100|5|8", "110|<nil>|9")
	})
	// Q5: STAR body — no Project wrapper at all (the CTE leg IS the unnest
	// FlatMap's RC row). Its schema-complete authority is the RC arm
	// (flat_map_cursor computedComplete), not executeProjection — this pin
	// covers the class a projection-only fix would have missed (it was
	// all-NULL too, a distinct unpinned instance found in the design consult).
	t.Run("Q5_star_body_enclosed_qualified_reads", func(t *testing.T) {
		check(t, `WITH "S" AS (SELECT * FROM LA, LA."ARR" AS "X") SELECT "S"."K", "S"."X" FROM "S", CC`,
			"100|7", "100|8", "110|9")
	})
	// Q6: BARE reads over the enclosed reference — the pre-fix WORKING path
	// (bare keys pass through mergeRows Pass A); regression guard that the
	// fabrication arm did not disturb it.
	t.Run("Q6_bare_reads_enclosed_regression_guard", func(t *testing.T) {
		check(t, `WITH "C" AS (`+cteBodyNoWhere+`) SELECT "AK", "BK", "XV" FROM "C", CC`,
			"100|5|7", "100|5|8", "110|<nil>|9")
	})
	// Q7: ORDER BY a qualified CTE column, ORDER asserted (not just the row
	// set): pre-fix the missing merged key made ORDER BY C.XV a silent no-op
	// sort — the row SET can't catch that, only the emission order can.
	t.Run("Q7_order_by_qualified_cte_column", func(t *testing.T) {
		checkOrdered(t, `WITH "C" AS (`+cteBodyNoWhere+`) SELECT "C"."XV" FROM "C", CC ORDER BY "C"."XV" DESC`,
			"9", "8", "7")
	})
	// Q8: the qualifyOuterRow dimension — the Complete CTE leg is the
	// PRESERVED side of a LEFT JOIN whose inner never matches, so every row is
	// a pad row built by qualifyOuterRow (not mergeRows). C.* must resolve on
	// pad rows too; the null-supplied CC2 column stays NULL.
	// Q9: the ON-over-CTE-leg battery. Discovered while pinning Q8: the ON
	// resolver's scope build could not derive a schema for a join/unnest-
	// bodied CTE (buildCTEColumnSource declined it), the name fell into the
	// "unresolvable table" non-drop-risk arm, and the ON was silently DROPPED
	// — the join returned CROSS-PRODUCT rows (every C row matched every CC
	// row; pre-existing, no unnest needed — a plain-join CTE body did it too).
	// Fixed twice over: (a) buildCTEOnOnlySource derives an ON-RESOLUTION-ONLY
	// schema from the explicitly-ALIASED projection list at WITH registration
	// (the cteOnScopes map, consumed only by upgradeJoinOnPredicates — never
	// the global cteScopes, so WHERE/projection resolution over comma-joined
	// multi-leg CTEs keeps its clean decline, the flatten-evasion class);
	// (b) a declared CTE whose derivation still declines registers a MARKER
	// that routes to the loud DROP RISK 0AF00 (the derived-table twin's
	// behavior), never a silent drop.
	t.Run("Q9a_on_over_unnest_cte_left_pads", func(t *testing.T) {
		check(t, `WITH "C" AS (`+cteBodyNoWhere+`) SELECT "C"."AK", "CC2"."CV" FROM "C" LEFT JOIN CC AS "CC2" ON "C"."AK" = "CC2"."CID"`,
			"100|<nil>", "100|<nil>", "110|<nil>")
	})
	t.Run("Q9b_on_over_unnest_cte_inner_no_match", func(t *testing.T) {
		check(t, `WITH "C" AS (`+cteBodyNoWhere+`) SELECT "C"."AK", "CC2"."CV" FROM "C" INNER JOIN CC AS "CC2" ON "C"."AK" = "CC2"."CID"`)
	})
	t.Run("Q9c_on_over_unnest_cte_reversed_preserved", func(t *testing.T) {
		check(t, `WITH "C" AS (`+cteBodyNoWhere+`) SELECT "C"."AK", "CC2"."CV" FROM CC AS "CC2" LEFT JOIN "C" ON "C"."AK" = "CC2"."CID"`,
			"<nil>|900")
	})
	t.Run("Q9d_on_over_plain_join_cte_pads", func(t *testing.T) {
		check(t, `WITH "J" AS (SELECT LA."K" AS "AK", LB."K" AS "BK" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") SELECT "J"."AK", "CC2"."CV" FROM "J" LEFT JOIN CC AS "CC2" ON "J"."AK" = "CC2"."CID"`,
			"100|<nil>", "110|<nil>")
	})
	// The derived-table twin ANSWERS, and answers the same rows as its CTE
	// sibling Q9a. It used to decline 0AF00 because the derived-table schema
	// derivation could not describe a body whose FROM list carries a lateral
	// unnest leg; that body now derives its exact row from the same place the
	// translator does, so the ON resolves and the LEFT JOIN pads exactly as the
	// CTE spelling of the identical query does.
	//
	// Pinned as ROWS against Q9a rather than as "it plans": the hazard this whole
	// battery exists for is a SILENTLY DROPPED ON producing cross-product rows,
	// and only the rows can tell a resolved ON from a dropped one. Three rows
	// with a NULL CV is the padded answer; a cross product would be nine.
	t.Run("Q9e_derived_twin_answers_like_its_CTE_sibling", func(t *testing.T) {
		check(t, `SELECT "C"."AK", "CC2"."CV" FROM (`+cteBodyNoWhere+`) AS "C" LEFT JOIN CC AS "CC2" ON "C"."AK" = "CC2"."CID"`,
			"100|<nil>", "100|<nil>", "110|<nil>")
	})
	// Q10: the SCALAR-SUBQUERY build path — a threading hole. The subquery's
	// inner plan builds through the CTECatalog chain, which
	// initially never received the ON-only scopes: the ON of `C JOIN CC2`
	// inside the subquery was silently dropped and COUNT counted the CROSS
	// product (3) instead of the joined answer (0 — no C.AK ∈ {100,110}
	// equals CID=1). The discriminator never collides with NULL, so a broken
	// subquery cannot masquerade as either answer.
	t.Run("Q10_scalar_subquery_path_on_resolves", func(t *testing.T) {
		// The datum keys the subquery value twice (its rendered name + the
		// positional _N key), so the joined COUNT appears in two slots.
		check(t, `WITH "C" AS (`+cteBodyNoWhere+`) SELECT LA."K", (SELECT COUNT(*) FROM "C" JOIN CC AS "C2" ON "C"."AK" = "C2"."CID") FROM LA WHERE LA."K" = 110`,
			"110|0")
	})
	// Q11: an UNALIASED QUALIFIED projection in a multi-leg CTE body. This used
	// to be loud: the schema was guessed from the body's FROM legs by NAME, and
	// no name it could offer matched the runtime row's "LA.K" key. The CTE's row
	// is now the body's own exact result type, so the slot is addressable by its
	// leaf name and binds the ordinal the body really emits.
	//
	// The INNER join keeps no row (K ∈ {100,110}, CID = 1), so the VALUES are
	// pinned by the comma companion — the discriminating half: a slot bound to
	// the wrong ordinal answers AID {1,2} here, not K {100,110}.
	t.Run("Q11_unaliased_qualified_body_resolves", func(t *testing.T) {
		check(t, `WITH "UQ" AS (SELECT LA."K" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") SELECT "UQ"."K" FROM "UQ" JOIN CC AS "C2" ON "UQ"."K" = "C2"."CID"`)
		check(t, `WITH "UQ" AS (SELECT LA."K" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") SELECT "UQ"."K" FROM "UQ", CC`,
			"100", "110")
	})
	// Q12: WITH c(x) COLUMN ALIASES over a multi-leg body. The renames are a
	// SCOPE-level view and never appear on the runtime row — which is exactly
	// why they used to be undeliverable and the shape stayed loud. A rename is
	// now just a different name for the same ordinal, so it carries. Same
	// two-query shape as Q11: the INNER join keeps nothing, the comma companion
	// pins the values a mis-bound rename would move.
	t.Run("Q12_column_aliased_multileg_body_resolves", func(t *testing.T) {
		check(t, `WITH "CA" ("X") AS (SELECT LA."K" AS "AK" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") SELECT "CA"."X" FROM "CA" JOIN CC AS "C2" ON "CA"."X" = "C2"."CID"`)
		check(t, `WITH "CA" ("X") AS (SELECT LA."K" AS "AK" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") SELECT "CA"."X" FROM "CA", CC`,
			"100", "110")
	})
	t.Run("Q8_pad_row_preserved_cte_leg", func(t *testing.T) {
		check(t, `WITH "C" AS (`+cteBodyNoWhere+`) SELECT "C"."AK", "C"."XV", "CC2"."CV" FROM "C" LEFT JOIN CC AS "CC2" ON "C"."AK" = "CC2"."CID"`,
			"100|7|<nil>", "100|8|<nil>", "110|9|<nil>")
	})
	// Q13: FULL JOIN over the CTE leg — the unmatched-INNER pad path
	// (streaming_cursors' qualifyOuterRow(innerRows) arm) that Q8/Q9c's
	// LEFT-preserved shapes never reach. The CTE rows never match CID=1, so
	// BOTH sides pad: three C-preserved pads (CV nil) + one CC-preserved pad
	// (C.* nil).
	t.Run("Q13_full_join_cte_leg_both_pads", func(t *testing.T) {
		check(t, `WITH "C" AS (`+cteBodyNoWhere+`) SELECT "C"."AK", "C"."XV", "CC2"."CV" FROM "C" FULL OUTER JOIN CC AS "CC2" ON "C"."AK" = "CC2"."CID"`,
			"100|7|<nil>", "100|8|<nil>", "110|9|<nil>", "<nil>|<nil>|900")
	})
	// Q14: the UNION-BRANCH path — the THIRD empty-scope short-circuit
	// (visitUnion checked only cteScopes, so a WITH declaring ONLY
	// join/unnest-bodied CTEs dropped the whole ON-only context and a union
	// branch's join ON silently cross-producted; the plain non-union spelling
	// of the same join answered correctly). First branch: C.BK=5 never
	// matches CID=1 → 0 rows joined (2 rows cross — the discriminator);
	// second branch contributes one real row so total breakage can't
	// masquerade as success.
	// Q15: a BARE unqualified projection in a multi-leg CTE body IS derivable
	// — the runtime key mirrors the SQL spelling (the body plans as
	// Project([AID],…) and the comma-form control answers real values), so
	// the ON resolves and pads correctly. An over-narrow derivation check
	// once declined this to 0AF00 by mistake.
	t.Run("Q15_bare_ref_body_on_resolves", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT "AID" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"1|900", "2|<nil>")
	})
	// Q16: a DERIVED-SOURCE CTE body (`FROM (SELECT …) d` — zero joins, but
	// declined by the global deriver for the derivedQuery reason) derives its
	// ON-only schema from the projection list like any multi-leg body — the
	// sibling of Q15 (an early joins==0 decline wrongly assumed the global
	// deriver owned every zero-join shape).
	t.Run("Q16_derived_source_body_on_resolves", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT "D"."AID" AS "A" FROM (SELECT "AID" FROM LA) AS "D") SELECT "U"."A", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."A" = "C2"."CID"`,
			"1|900", "2|<nil>")
	})
	// Q17: a DUPLICATE join-bodied CTE name inside a SUBQUERY-nested WITH —
	// the third registration loop (the empty-scope short-circuit routes a
	// subquery's own WITH into buildLogicalPlanForQueryWithCatalog when the
	// outer query has no CTEs) used to let it silently last-win while the
	// other two loops errored. Loud DuplicateAlias now, uniformly.
	t.Run("Q17_subquery_nested_duplicate_with_loud", func(t *testing.T) {
		_, err := run(t, `SELECT LA."K", (WITH "X" AS (SELECT LA."K" AS "AK" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID"), "X" AS (SELECT "AID" FROM LA) SELECT COUNT(*) FROM "X") FROM LA`)
		if err == nil || !strings.Contains(err.Error(), "more than once") {
			// The DuplicateAlias message, not just any error — a vacuous
			// err != nil would stay green if the shape started failing for
			// an unrelated reason.
			t.Fatalf("duplicate CTE name in a subquery-nested WITH must fail with DuplicateAlias, got %v", err)
		}
	})
	// loud0AF00 pins the fail-closed decline arm: the shape must error with
	// the ON drop-risk SQLSTATE, never return rows, never fail at runtime.
	loud0AF00 := func(t *testing.T, sql, why string) {
		t.Helper()
		_, err := run(t, sql)
		if err == nil || !strings.Contains(err.Error(), "0AF00") {
			t.Fatalf("%s: must fail LOUD 0AF00, got %v\n  %s", why, err, sql)
		}
	}
	// loudCode is the same fail-closed contract for a shape whose diagnosis is
	// SPECIFIC rather than the reader's generic drop-risk. A mistake INSIDE a
	// CTE body (an ambiguous ref, an absent column) is now reported by the body
	// itself, because the body is built to derive the CTE's exact row instead of
	// being guessed at from its FROM legs' names. Pinning the precise SQLSTATE
	// is what keeps the diagnosis from silently degrading back to 0AF00 — which
	// would still be "loud" and would still pass a code-blind check.
	loudCode := func(t *testing.T, sql, code, why string) {
		t.Helper()
		rows, err := run(t, sql)
		if err == nil {
			t.Fatalf("%s: must fail LOUD %s, got rows=%v\n  %s", why, code, rows, sql)
		}
		if !strings.Contains(err.Error(), code) {
			t.Fatalf("%s: must fail LOUD %s, got %v\n  %s", why, code, err, sql)
		}
	}
	// Q18: an AMBIGUOUS bare ref in a multi-leg body with a DERIVED leg. The bare
	// ref used to resolve SILENTLY against one leg (rows came back where an error
	// was due); then the derivation declined any multi-leg body with a derived
	// leg, which made it loud but anonymous (0AF00 about the READER's join).
	// Building the body to derive the CTE's row runs the body's own resolver, so
	// the ambiguity is now reported where it lives: 42702, naming AID.
	t.Run("Q18_derived_leg_ambiguous_bare_loud", func(t *testing.T) {
		loudCode(t, `WITH "U" AS (SELECT "AID" FROM (SELECT LA."AID" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") "D", LA "L2") SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"42702", "ambiguous bare ref over a derived leg")
	})
	// Q19: derived-source body whose INNER projection is QUALIFIED-spelled. The
	// derived row keys "LA.AID", so a NAME-keyed outer read of D.AID could never
	// resolve at runtime and the shape was declined at plan time to keep it from
	// becoming a runtime malformed-plan error. Reads bind ordinals now, so the
	// runtime KEY is not what the read has to match: the same rows the
	// bare-spelled twin (Q16) answers.
	t.Run("Q19_derived_inner_qualified_resolves", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT "D"."AID" AS "A" FROM (SELECT LA."AID" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") "D") SELECT "U"."A", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."A" = "C2"."CID"`,
			"1|900", "2|<nil>")
	})
	// Q20: the AGGREGATE-arm instance of Q19. MAX(D.AID) over a qualified-keyed
	// derived row once read NOTHING and returned a SILENT NULL — the worst
	// variant of the class — and was then declined. It now answers, and the
	// value is the discriminator the silent-NULL era lacked: MAX over {1,2} is
	// 2, and 2 matches no CID, so the pad is the second half of the proof.
	t.Run("Q20_agg_over_derived_qualified_resolves", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT MAX("D"."AID") AS "M" FROM (SELECT LA."AID" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") "D") SELECT "U"."M", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."M" = "C2"."CID"`,
			"2|<nil>")
	})
	// Q21: a derived JOIN LEG (joinClause.derivedQuery — the non-first-source
	// twin of Q18). Was a runtime leg-adapter breach, then a plan-time decline,
	// and now answers: L2.AID ∈ {1,2} INNER-joined to D.BID ∈ {1,3} keeps AID=1,
	// which is the one value CC matches.
	t.Run("Q21_derived_join_leg_resolves", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT "AID" FROM LA "L2" JOIN (SELECT LB."BID" FROM LB) "D" ON "L2"."AID" = "D"."BID") SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"1|900")
	})
	// Q22: ANTI-OVER-DECLINE — aggregate over a derived-inner-BARE body stays
	// derivable and answers correctly (M = MAX(1,2) = 2, no CC match → pad).
	// The read validation must pass when the inner emits bare keys.
	t.Run("Q22_agg_over_derived_bare_resolves", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT MAX("D"."AID") AS "M" FROM (SELECT "AID" FROM LA) "D") SELECT "U"."M", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."M" = "C2"."CID"`,
			"2|<nil>")
	})
	// Q23: ANTI-OVER-DECLINE — a COMPUTED aliased item whose reads resolve in
	// the inner's bare set stays derivable (harvestColumnRefs validates the
	// expression's refs, it does not blanket-decline computed items).
	t.Run("Q23_computed_over_derived_bare_resolves", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT "D"."AID" + 0 AS "Z" FROM (SELECT "AID" FROM LA) "D") SELECT "U"."Z", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."Z" = "C2"."CID"`,
			"1|900", "2|<nil>")
	})
	// Q24: union-bodied CTE under ON — the union pathway NORMALIZES branch
	// keys (branch-2 spelled qualified still resolves U.AID), for both a
	// plain and a JOIN-SEEDED union. Pins that the union arm needs no decline.
	// (Rows render POSITIONALLY, in SELECT order -- AID then CV -- for BOTH
	// variants. Under the earlier name-keyed rendering the two differed: the
	// PLAIN union body's derivable layout left the key BARE (AID sorting first)
	// while the JOIN-SEEDED body kept the name-model U.AID/C2.CV keys (CV
	// sorting first), so one expectation was column-swapped relative to the
	// other for the same values. Positional rendering removes that artifact.)
	t.Run("Q24_union_branch_keys_normalize", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT "AID" FROM LA UNION ALL SELECT LB."BID" FROM LB) SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"1|900", "2|<nil>", "1|900", "3|<nil>")
		check(t, `WITH "U" AS (SELECT "AID" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID" UNION ALL SELECT LB."BID" FROM LB) SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"1|900", "2|<nil>", "1|900", "3|<nil>")
	})
	// Q25: computed item over a qualified-keyed derived row — the Q19 shape with
	// an expression on top. Same retirement, same answer as the bare-keyed twin
	// (Q23): the computed slot is typed and addressed by the built body, not by
	// whether its read spelling matches a runtime key.
	t.Run("Q25_computed_over_derived_qualified_resolves", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT "D"."AID" + 0 AS "Z" FROM (SELECT LA."AID" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") "D") SELECT "U"."Z", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."Z" = "C2"."CID"`,
			"1|900", "2|<nil>")
	})
	// Q26: the bare UNNEST-ELEMENT ref — the one admitted derivation shape
	// whose write path goes through the RFC-142 QOV value (shadowing rewrite)
	// rather than a plain name-model read; the rewrite qualifies via the QOV
	// CHILD while Field stays the verbatim bare name, so the runtime key is
	// bare "X". CD keys on the element VALUE (XK=7 → XV=700), so a
	// silent-miss would pad ALL rows — the 700 row discriminates.
	t.Run("Q26_bare_unnest_element_on_resolves", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT "X" FROM LA, LA."ARR" AS "X") SELECT "U"."X", "C3"."XV" FROM "U" LEFT JOIN CD AS "C3" ON "U"."X" = "C3"."XK"`,
			"7|700", "8|<nil>", "9|<nil>")
	})
	// Q27-Q29: the ON-ONLY-CTE-LEG hole. An ON-only CTE used as a FROM leg is
	// neither a base table nor a derivedQuery — and
	// buildSelectScope returns a NIL resolver for it, killing BOTH the 42702
	// ambiguity gate and the 42703 unknown-column gate for the whole body.
	// The enumerability walk (cteBodyLegsEnumerable) declines such bodies.
	// V is join-bodied (ON-only); its alias AID collides with LA's column.
	const onOnlyV = `"V" AS (SELECT LA."K" AS "AID", LB."K" AS "Q" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID")`
	//
	// V is no longer opaque — its row is published from its built body — so the
	// two gates the nil resolver used to kill are back on their own terms: the
	// ambiguity 42702s and the unknown column 42703s, each naming the offending
	// identifier. That specificity is the pin: a regression to the anonymous
	// 0AF00 would still be "loud" and would still pass an err != nil check.
	t.Run("Q27_on_only_cte_leg_ambiguous_loud", func(t *testing.T) {
		loudCode(t, `WITH `+onOnlyV+`, "U" AS (SELECT "AID" FROM "V", LA "L2") SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"42702", "ambiguous bare ref over an ON-only CTE leg")
		// both leg orders — the walk must not depend on leg position
		loudCode(t, `WITH `+onOnlyV+`, "U" AS (SELECT "AID" FROM LA "L2", "V") SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"42702", "ambiguous bare ref, ON-only CTE as second leg")
	})
	t.Run("Q28_on_only_cte_leg_nonexistent_col_loud", func(t *testing.T) {
		loudCode(t, `WITH `+onOnlyV+`, "U" AS (SELECT "NOPE" FROM "V", LA "L2") SELECT "U"."NOPE", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."NOPE" = "C2"."CID"`,
			"42703", "nonexistent column over an ON-only CTE leg")
	})
	t.Run("Q29_on_only_cte_leg_in_derived_source_loud", func(t *testing.T) {
		loudCode(t, `WITH `+onOnlyV+`, "U" AS (SELECT "AID" FROM (SELECT "AID" FROM "V", LA "L2") "D") SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"42702", "ON-only CTE leg one level down")
	})
	// Q30: ANTI-OVER-DECLINE — a DERIVABLE CTE leg is enumerable (addSource
	// resolves it via cteScopes; the backstop lives) and the bare ref
	// admits + answers. AID lives only in W (LB carries BID/K); W×LB cross
	// doubles each AID row.
	t.Run("Q30_derivable_cte_leg_resolves", func(t *testing.T) {
		check(t, `WITH "W" AS (SELECT "AID" FROM LA), "U" AS (SELECT "AID" FROM "W", LB "L3") SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"1|900", "1|900", "2|<nil>", "2|<nil>")
	})
	// Q31: the backstop LIVES over enumerable legs — a genuinely ambiguous
	// bare ref over a derivable-CTE leg + base leg still 42702s (proves the
	// enumerability narrowing didn't just widen the 0AF00 blanket).
	t.Run("Q31_derivable_cte_leg_ambiguity_still_fires", func(t *testing.T) {
		_, err := run(t, `WITH "W2" AS (SELECT "K" FROM LA), "U" AS (SELECT "K" FROM "W2", LB "L3") SELECT "U"."K", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."K" = "C2"."CID"`)
		if err == nil || !strings.Contains(err.Error(), "42702") {
			t.Fatalf("ambiguous bare ref over enumerable legs must 42702, got %v", err)
		}
	})
	// Q33: the POSITIONAL-FRONTIER class (an over-decline this pins against): a
	// single-BASE-TABLE derived source keeps its projection row positional,
	// so a QUALIFIED-spelled inner item is readable by ordinal under its
	// last segment — this shape must ADMIT and ANSWER. It used to be the only
	// half of that pair that did; the JOIN-shaped twin (Q19) was declined for
	// having a name-keyed inner row and now answers the same way, so what this
	// pins is the single-table frontier itself, not a contrast with Q19.
	t.Run("Q33_single_table_qualified_inner_resolves", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT "D"."AID" AS "A" FROM (SELECT LA."AID" FROM LA) "D") SELECT "U"."A", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."A" = "C2"."CID"`,
			"1|900", "2|<nil>")
	})
	// Q34: a scalar subquery inside a computed item reads ITS OWN scope, not
	// the derived source — harvestColumnRefsOutsideSubqueries stops at the
	// nested-query boundary (an over-decline this pins against: LB.K was
	// checked against D's emitted set {AID} and spuriously declined).
	t.Run("Q34_scalar_subquery_item_over_derived_resolves", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT (SELECT "K" FROM LB ORDER BY "K" DESC LIMIT 1) AS "Z", "AID" FROM (SELECT "AID" FROM LA) "D") SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"1|900", "2|<nil>")
	})
	// Q35: the hazard arm the boundary-stop must NOT unguard — a subquery
	// CORRELATED into a JOIN-shaped (name-keyed) derived source. Admission
	// no longer inspects the subquery's refs, but the correlated build fails
	// loud at translation (pinned so a future silent path can't creep in).
	t.Run("Q35_correlated_subquery_into_join_keyed_derived_loud", func(t *testing.T) {
		loud0AF00(t, `WITH "U" AS (SELECT (SELECT MAX("K") FROM LB WHERE LB."BID" = "D"."AID") AS "Z" FROM (SELECT LA."AID" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") "D") SELECT "U"."Z", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."Z" = "C2"."CID"`,
			"correlated subquery into a name-keyed derived source")
	})
	// Q32: mixed-star derived source. The star-EXPANDED columns were invisible
	// to a name-list derivation that never touched the catalog, so a read of one
	// declined fail-closed. Building the body expands the star against the
	// catalog like execution does, so D genuinely carries K and the read
	// answers. K ∈ {100,110} matches no CID, so both rows pad — and the K values
	// are what a star expanded to the wrong width would move.
	t.Run("Q32_mixed_star_inner_resolves", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT "D"."K" AS "KK" FROM (SELECT LA.*, "AID" FROM LA) "D") SELECT "U"."KK", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."KK" = "C2"."CID"`,
			"100|<nil>", "110|<nil>")
	})
	// Q36: CTE SHADOWING a catalog table. The leg classifier must mirror
	// EXECUTION's resolution order — a declared CTE
	// shadows a same-named table, so a metadata-first lookup classified the
	// leg by the TABLE's schema while runtime rows came from the CTE (every
	// reachable variant probed LOUD at runtime — malformed-plan, row columns
	// [Z] — but the classification was wrong and the error class regressed
	// from plan-time 0AF00). CTE names now classify FIRST: an ON-only "LA"
	// is opaque → all three variants decline at plan time.
	t.Run("Q36_cte_shadows_table_loud", func(t *testing.T) {
		const shadowLA = `"LA" AS (SELECT LB."K" AS "Z" FROM LB LEFT JOIN CC ON LB."BID" = CC."CID")`
		// The shadowing CTE exposes only Z, so LA."AID" and a bare AID are reads
		// of a column that does not exist ON THE SHADOWING GENERATION — which is
		// the whole point of the pin, and is now said in those words (42703)
		// rather than as the reader's anonymous 0AF00. A regression that
		// resolved these against the TABLE's schema would answer rows.
		loudCode(t, `WITH `+shadowLA+`, "U" AS (SELECT "D"."AID" AS "A" FROM (SELECT LA."AID" FROM LA) "D") SELECT "U"."A", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."A" = "C2"."CID"`,
			"42703", "derived source over a table-shadowing CTE")
		loudCode(t, `WITH `+shadowLA+`, "U" AS (SELECT MAX("D"."AID") AS "M" FROM (SELECT LA."AID" FROM LA) "D") SELECT "U"."M", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."M" = "C2"."CID"`,
			"42703", "aggregate arm over a table-shadowing CTE")
		loudCode(t, `WITH `+shadowLA+`, "U" AS (SELECT "AID" FROM "LA", CC "C9") SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"42703", "shadowed CTE as a multi-leg body leg")
	})
	// Q37: SCHEMA-QUALIFIED legs. Three stacked
	// fixes pin here: (1) the ON-only derivation ran BEFORE
	// normalizeSchemaQualifiedSelectSources, so "s"."LA" classified opaque —
	// spurious 0AF00 (cteLegKind now mirrors the normalizer's strip);
	// writing this pin then EXPOSED two pre-existing bugs independent of
	// CTEs: (2) upgradeJoinOnPredicates' scope build silently declined the
	// dotted source — the "unresolvable table errors precisely downstream"
	// assumption is false for the active-schema form (the demoted scan
	// succeeds), so EVERY explicit join with a schema-qualified leg silently
	// CROSS-PRODUCTED; (3) with (2) fixed, the visitor path's scan kept its
	// defaulted DOTTED alias while the upgraded predicate said QOV(bare) —
	// INNER failed leg attribution loud, LEFT silently padded every row
	// (resolveQualifiedTableNames now strips a defaulted alias in lockstep).
	// BK=5 on the matched row discriminates all three failure modes: a
	// cross-product doubles rows, an ON-drop or alias-desync pads BK.
	t.Run("Q37_schema_qualified_legs_resolve", func(t *testing.T) {
		check(t, `WITH "V" AS (SELECT LA."K" AS "AK", LB."K" AS "BK" FROM "s"."LA" LEFT JOIN "s"."LB" ON LA."AID" = LB."BID") SELECT "V"."AK", "V"."BK", "C2"."CV" FROM "V" LEFT JOIN CC AS "C2" ON "V"."AK" = "C2"."CID"`,
			"100|5|<nil>", "110|<nil>|<nil>")
	})
	// Q38: the standalone (no CTE) schema-qualified explicit-join pins — the
	// pre-existing silent cross-product class in its own right, all four
	// shapes: LEFT with matched+pad rows, INNER keeps ONLY the match,
	// explicit aliases, one-leg-qualified.
	t.Run("Q38_schema_qualified_join_on_live", func(t *testing.T) {
		// (top-level datums key each output twice — rendered + positional —
		// hence the doubled columns, same as Q10)
		check(t, `SELECT LA."K" AS "AK", LB."K" AS "BK" FROM "s"."LA" LEFT JOIN "s"."LB" ON LA."AID" = LB."BID"`,
			"100|5", "110|<nil>")
		check(t, `SELECT LA."K" AS "AK", LB."K" AS "BK" FROM "s"."LA" JOIN "s"."LB" ON LA."AID" = LB."BID"`,
			"100|5")
		check(t, `SELECT "X"."K" AS "AK", "Y"."K" AS "BK" FROM "s"."LA" AS "X" LEFT JOIN "s"."LB" AS "Y" ON "X"."AID" = "Y"."BID"`,
			"100|5", "110|<nil>")
		check(t, `SELECT LA."K" AS "AK", LB."K" AS "BK" FROM "s"."LA" LEFT JOIN LB ON LA."AID" = LB."BID"`,
			"100|5", "110|<nil>")
	})
	// Q39: the 42702 backstop LIVES over schema-qualified legs. The Q37/Q38
	// classifier strip said "s"."LA" was enumerable,
	// but buildSelectScope — the mechanism the bare-ref admission's
	// ambiguity backstop actually runs through — did NOT strip, so its
	// resolver went nil and an ambiguous bare K over ("s"."LA", LB) executed
	// silently. buildSelectScope's addSource now applies the same
	// normalizer-mirror strip, making the classifier's enumerability claim
	// TRUE rather than narrowing it (the alternative — declining the leg —
	// would have regressed Q37).
	t.Run("Q39_ambiguity_fires_over_schema_qualified_leg", func(t *testing.T) {
		_, err := run(t, `WITH "V" AS (SELECT "K" FROM "s"."LA", LB) SELECT "V"."K", "C2"."CV" FROM "V" LEFT JOIN CC AS "C2" ON "V"."K" = "C2"."CID"`)
		if err == nil || !strings.Contains(err.Error(), "42702") {
			t.Fatalf("ambiguous bare ref over a schema-qualified leg must 42702, got %v", err)
		}
	})
	// Q40: WHERE over a schema-qualified explicit join ANSWERS — the same
	// nil-resolver disease as Q39, one consumer over (a loud 0AF00 reach
	// gap before the Q39 backstop fix, which fixed this consumer too).
	t.Run("Q40_where_over_schema_qualified_join_answers", func(t *testing.T) {
		check(t, `SELECT LA."K" AS "AK" FROM "s"."LA" LEFT JOIN "s"."LB" ON LA."AID" = LB."BID" WHERE LA."K" = 100`,
			"100")
	})
	// Q41: the scope builder resolves a CTE-shadowed name through the CTE's
	// OUTPUT schema, not the same-named table's: addSource was catalog-first,
	// so `LA."X"` — X being the CTE's renamed column — 42703'd against the
	// base table. The plain-name variant was broken this way ALL ALONG; the
	// schema-qualified variant broke the same way once the Q39 resolver went
	// live. CTE-first now, mirroring
	// execution's shadowing (and cteLegKind's ordering). X = BID values
	// {1,3} × 2 B-rows.
	t.Run("Q41_cte_shadow_scope_reads_cte_schema", func(t *testing.T) {
		check(t, `WITH "LA" AS (SELECT "BID" AS "X" FROM LB) SELECT "LA"."X" FROM "LA", "s"."LB" AS "B"`,
			"1", "1", "3", "3")
		check(t, `WITH "LA" AS (SELECT "BID" AS "X" FROM LB) SELECT "LA"."X" FROM "LA", LB AS "B"`,
			"1", "1", "3", "3")
	})
	// Q42: a bare ORDER BY key naming a UNIQUE OUTPUT ALIAS takes precedence
	// over FROM-scope ambiguity: both legs carry a
	// column K, but the sort executes over the projected row where alias K
	// is unambiguous — the validation's ambiguity arm now defers to the
	// alias exactly like its ColumnNotFound arm always did. The assertion is
	// K DESC = 2,2,1,1 in the K slot, plus the full row multiset.
	// (Rows render POSITIONALLY, in SELECT order: the K alias then LB.BID. The
	// earlier name-keyed rendering sorted them BID-first under the ordinal model
	// and K-first under the name model, for the same values -- an artifact of the
	// rendering, not of the plan.)
	//
	// checkOrderedSlot, not checkOrdered: `ORDER BY "K"` over this cross join
	// determines the K sequence and NOTHING about the BID order within a K
	// group — that follows the join orientation the planner picks and the
	// sort's PK tie-break. This pin asserted the whole rendered row and so
	// silently pinned `NestedLoopJoin(Scan(LA), Scan(LB))`; it reddened the day
	// the memo correctly began admitting the mirror orientation as a cost
	// alternative, which is not an ordering regression.
	t.Run("Q42_orderby_alias_precedes_scope_ambiguity", func(t *testing.T) {
		checkOrderedSlot(t, `SELECT LA."AID" AS "K", LB."BID" FROM "s"."LA", LB ORDER BY "K" DESC`,
			0, []string{"2", "2", "1", "1"},
			"2|1", "2|3", "1|1", "1|3")
	})
	// Q43: the live resolver's strictness dividend, pinned against a
	// leniency regression: a reference through the ALIASED-AWAY table name
	// is 42703 — before the Q39 resolver fix, a nil resolver let it through
	// leniently.
	t.Run("Q43_aliased_away_name_is_42703", func(t *testing.T) {
		_, err := run(t, `SELECT LA."K" FROM "s"."LA" AS "X" LEFT JOIN "s"."LB" AS "Y" ON "X"."AID" = "Y"."BID"`)
		if err == nil || !strings.Contains(err.Error(), "42703") {
			t.Fatalf("aliased-away table-name reference must 42703, got %v", err)
		}
	})
	// Q44: a NON-recursive CTE body is SELF-INVISIBLE on every
	// register-before-build path: the chain pipelines complete registration before building
	// bodies, so CTE-first scope resolution made `FROM LB` inside the CTE
	// "LB" resolve to ITSELF — a bogus correlated-fallback misroute (0A000)
	// and, through BuildScalar's 42703 arm, a silent base-table value
	// substitution. buildCTEBodySelfHidden now guards the visitor eager
	// build, the visitor rebuild, and BOTH chain loops. COUNT(*)=2 is the
	// TABLE's row count read through the self-named CTE.
	t.Run("Q44_self_named_cte_body_reads_table", func(t *testing.T) {
		// The body filter (BID = 1) makes the count VALUE-DISCRIMINATING:
		// the table-read gives 1, any row-preserving misroute over the
		// unfiltered generation gives 2 (the unfiltered
		// COUNT(*)=2 was value-degenerate with the table cardinality — the
		// exact green-masking pattern Q49 below also demonstrates).
		check(t, `SELECT LA."K", (WITH "LB" AS (SELECT "BID" AS "X" FROM LB WHERE "BID" = 1) SELECT COUNT(*) FROM "LB") FROM LA WHERE LA."K" = 100`,
			"100|1")
		// differently-named control (never broken — isolates causation)
		check(t, `SELECT LA."K", (WITH "W9" AS (SELECT "BID" AS "X" FROM LB WHERE "BID" = 1) SELECT COUNT(*) FROM "W9") FROM LA WHERE LA."K" = 100`,
			"100|1")
		// the WithCTECatalog-route twin (outer WITH forces the other chain)
		check(t, `WITH "W0" AS (SELECT "AID" FROM LA) SELECT (WITH "LB" AS (SELECT "BID" AS "X" FROM LB WHERE "BID" = 1) SELECT COUNT(*) FROM "LB") FROM "W0"`,
			"1", "1")
	})
	// Q45: an enclosing ON through a SHADOWING derivable CTE resolves
	// against the CTE's OUTPUT schema: the ON-upgrade's resolveTable was
	// analyzer-first — the fourth and last catalog-first consumer —
	// over-declining valid ONs (42703 on the CTE's
	// renamed column) and pushing the table-only-column shape to a RUNTIME
	// malformed plan. CTE-first now: the valid ON answers (X∈{1,3}, CID=1
	// matches X=1), and the table-only column fails plan-time 42703.
	t.Run("Q45_on_through_shadowing_cte", func(t *testing.T) {
		check(t, `WITH "LA" AS (SELECT "BID" AS "X" FROM LB) SELECT "LA"."X", CC."CV" FROM "LA" JOIN CC ON "LA"."X" = CC."CID"`,
			"1|900")
		_, err := run(t, `WITH "LA" AS (SELECT "BID" AS "X" FROM LB) SELECT "LA"."X" FROM "LA" JOIN CC ON "LA"."K" = CC."CID"`)
		if err == nil || !strings.Contains(err.Error(), "42703") {
			t.Fatalf("table-only column through a shadowing CTE's ON must fail plan-time 42703, got %v", err)
		}
	})
	// Q46: ORDER BY output-alias precedence works through the SUBQUERY
	// build path too: the postBuild validation has its own ambiguity arm,
	// separate from Q42's top-level one — the same query answered top-level
	// but 42702'd inside a scalar subquery.
	t.Run("Q46_orderby_alias_in_subquery_path", func(t *testing.T) {
		check(t, `SELECT (SELECT LA."AID" AS "KK" FROM "s"."LA", LB ORDER BY "KK" DESC LIMIT 1), LA."K" FROM LA WHERE LA."K" = 100`,
			"2|100")
	})
	// Q47+Q48: the two over-suppressions the Q42 alias
	// bypass would otherwise have allowed — DUPLICATE output aliases must NOT bypass
	// the scope's 42702 (presence-only matching would silently sort by the
	// last one), and an aggregate key whose CANONICAL text matches a quoted
	// alias must NOT suppress genuine ambiguity inside its argument (only a
	// bare identifier key takes the alias route).
	t.Run("Q47_orderby_duplicate_alias_stays_ambiguous", func(t *testing.T) {
		_, err := run(t, `SELECT LA."AID" AS "K", LB."BID" AS "K" FROM "s"."LA", LB ORDER BY "K"`)
		if err == nil || !strings.Contains(err.Error(), "42702") {
			t.Fatalf("duplicate output aliases must keep 42702, got %v", err)
		}
	})
	t.Run("Q48_orderby_agg_canonical_text_stays_ambiguous", func(t *testing.T) {
		_, err := run(t, `SELECT SUM(LA."K") AS "SUM(K)" FROM "s"."LA", LB ORDER BY SUM("K")`)
		if err == nil || !strings.Contains(err.Error(), "42702") {
			t.Fatalf("aggregate canonical-text key must keep 42702, got %v", err)
		}
	})
	// Q49: a NESTED same-named CTE's body sees the OUTER binding: the inner
	// registration used to overwrite the level map's outer entry, so a plain
	// self-DELETE lost BOTH bindings
	// and the inner body's reads fell to the base TABLE (42703 on the outer
	// CTE's renamed column). buildCTEBodySelfHidden now swaps to the
	// PRE-REGISTRATION snapshot — self invisible, outer visible. MAX(X)=3
	// is over the OUTER CTE's X∈{1,3}, per outer row.
	t.Run("Q49_nested_shadow_body_sees_outer", func(t *testing.T) {
		check(t, `WITH "LA" AS (SELECT "BID" AS "X" FROM LB) SELECT (WITH "LA" AS (SELECT LA."X" AS "X" FROM "LA") SELECT MAX("X") FROM "LA"), "O"."X" FROM "LA" AS "O"`,
			"3|1", "3|3")
	})
	// Q50: an inner ON-ONLY CTE shadowing an outer derivable EVICTS the
	// stale outer entry: registration used to write only cteOnScopes, leaving
	// the outer schema installed for this level's MAIN query — the inner
	// CTE's reads resolved against the OUTER generation (a read of the
	// inner's real column Y misrouted 0A000; a read of the outer-only column
	// X silently accepted). Post-evict, Y answers through the real inner CTE
	// (MAX(CV)=900). The X read now lands in the ON-only READ class covered
	// by Q51 below — the STALE-SCHEMA mechanism itself is gone.
	t.Run("Q50_ononly_shadow_evicts_stale_outer", func(t *testing.T) {
		check(t, `WITH "V" AS (SELECT "BID" AS "X" FROM LB) SELECT (WITH "V" AS (SELECT CC."CV" AS "Y" FROM "V" LEFT JOIN CC ON "V"."X" = CC."CID") SELECT MAX("Y") FROM "V") FROM LB LIMIT 1`,
			"900")
	})
	// Q51: MAX-scalar-subquery reads of a column ABSENT from the CTE's row —
	// both the no-shadow case and the shadowed different-schema case (inner
	// exposes Y, read X; the evict removed the stale outer, so the read finds no
	// X on the inner row). Neither may be the name model's silent NULL.
	//
	// The second one carries a second lesson. An undefined column inside a
	// scalar subquery is SPECULATIVELY retried as a correlated reference to the
	// enclosing row, and that retry then failed with its own complaint — a
	// missing WHERE clause, describing a correlated query the user never wrote.
	// A speculation that disproves itself must not replace the diagnosis that
	// prompted it, so both arms land on the same honest 42703.
	t.Run("Q51_ononly_invalid_read_class", func(t *testing.T) {
		// Reading a column ABSENT from the derived source (`NOPE`/`X` are not
		// columns of W=[Y] / inner V=[Y]) must be LOUD, never the name model's
		// silent NULL. It is now the clean PLAN-TIME 42703 the earlier runtime
		// ordinal-resolution miss was only an approximation of: the source
		// publishes its real row, so the resolver can say which column is
		// missing instead of failing when the read misses a slot.
		mustLoudRead := func(t *testing.T, sql string) {
			t.Helper()
			rows, err := run(t, sql)
			if err == nil {
				t.Fatalf("expected a LOUD absent-column read error, got rows=%v\n  %s", rows, sql)
			}
			if !strings.Contains(err.Error(), "42703") {
				t.Fatalf("expected a 42703 column-not-found error, got: %v\n  %s", err, sql)
			}
		}
		mustLoudRead(t, `WITH "W" AS (SELECT CC."CV" AS "Y" FROM LB LEFT JOIN CC ON LB."BID" = CC."CID") SELECT MAX("NOPE") FROM "W"`)
		mustLoudRead(t, `WITH "V" AS (SELECT "BID" AS "X" FROM LB) SELECT (WITH "V" AS (SELECT CC."CV" AS "Y" FROM "V" LEFT JOIN CC ON "V"."X" = CC."CID") SELECT MAX("X") FROM "V") FROM LB LIMIT 1`)
	})
	// Q52: the COINCIDING-schema shadow read ANSWERS CORRECTLY through the
	// leniency + merge-fabrication path (NOT via any install — a previous
	// install approach was unnecessary here AND has been reverted): the evict
	// removes the stale outer, and the read of X resolves against the inner
	// row, which genuinely carries X' = CC.CID over (outer X∈{1,3} LEFT JOIN
	// CC ON X=CID) → {1, NULL} → MAX = 1. This is the discriminator that the
	// evict fixed the stale generation (base returned the OUTER X; a stale
	// read here would give MAX(outer X) = 3, not 1).
	t.Run("Q52_ononly_shadow_coinciding_schema_answers", func(t *testing.T) {
		check(t, `WITH "V" AS (SELECT "BID" AS "X" FROM LB) SELECT (WITH "V" AS (SELECT CC."CID" AS "X" FROM "V" LEFT JOIN CC ON "V"."X" = CC."CID") SELECT MAX("X") FROM "V") FROM LB LIMIT 1`,
			"1")
	})
	// Q53: the shadow-CTE read class, and what separates a SOUND install of the
	// inner generation from the LOSSY one this used to guard against. An
	// ON-resolution-only schema permitted duplicate output names and could not
	// state a quoted alias truthfully, so promoting it to general reads
	// SILENTLY MIS-RESOLVED both — and the answer was to install nothing and
	// fail closed. The published row is now the body's own exact result type,
	// which cannot be lossy about the two things that mattered: a duplicate
	// output name declines the whole source, and a quoted alias is folded by the
	// same rule execution folds it. So the reads that had to fail closed now
	// answer, and each arm below says which of the two it is exercising.
	t.Run("Q53_shadow_reads_resolve_against_the_inner_generation", func(t *testing.T) {
		// quoted lowercase "x" inner alias. The read used to fail closed because
		// a LOSSY install could only have resolved it by accident; the exact
		// install resolves it on purpose, against the INNER generation. The
		// discriminator is which X the WHERE sees: the inner x is CC.CID over
		// (outer X ∈ {1,3} LEFT JOIN CC ON X = CID) → {1, NULL}, while the outer
		// X is BID ∈ {1,3}. `= 1` counts one either way, so the `= 3` companion
		// is the arm that separates them — inner 0, stale-outer 1.
		if rows, err := run(t, `WITH "V" AS (SELECT "BID" AS "X" FROM LB) SELECT (WITH "V" AS (SELECT CC."CID" AS "x" FROM "V" LEFT JOIN CC ON "V"."X" = CC."CID") SELECT COUNT(*) FROM "V" WHERE "X" = 1) FROM LB LIMIT 1`); err != nil ||
			strings.Join(rows, ",") != "1" {
			t.Fatalf("quoted-alias shadow read: rows=%v err=%v, want [1]", rows, err)
		}
		if rows, err := run(t, `WITH "V" AS (SELECT "BID" AS "X" FROM LB) SELECT (WITH "V" AS (SELECT CC."CID" AS "x" FROM "V" LEFT JOIN CC ON "V"."X" = CC."CID") SELECT COUNT(*) FROM "V" WHERE "X" = 3) FROM LB LIMIT 1`); err != nil ||
			strings.Join(rows, ",") != "0" {
			t.Fatalf("quoted-alias shadow read at X=3 must see the INNER x ({1,NULL}), not the outer BID: rows=%v err=%v, want [0]", rows, err)
		}
		// duplicate output name X; WHERE X=1 must not pick one column.
		loud0AF00(t, `WITH "V" AS (SELECT "BID" AS "X" FROM LB) SELECT (WITH "V" AS (SELECT LB."BID" AS "X", LB."K" AS "X" FROM "V" LEFT JOIN LB ON "V"."X" = LB."BID") SELECT COUNT(*) FROM "V" WHERE "X" = 1) FROM LB LIMIT 1`,
			"duplicate-name shadow read")
		// The comma-multi-leg shadow. Declining the join-bodied inner used to be
		// the only thing standing between this shape and the flatten-evasion
		// silent class, so it was pinned loud whatever the reason. The inner IS
		// installed now, which moves the diagnosis onto the thing that is
		// actually wrong with THIS query: a scalar subquery selecting two
		// columns is 42601, and it would be 42601 over a base table too.
		loudCode(t, `WITH "V" AS (SELECT "BID" AS "X" FROM LB) SELECT (WITH "V" AS (SELECT LA."K" AS "X", LB."K" AS "Y" FROM LA JOIN LB ON LA."AID" = LB."BID") SELECT "V"."X", "V"."Y" FROM "V", CC WHERE "V"."X" = CC."CID") FROM LB LIMIT 1`,
			"42601", "two-column scalar subquery over a comma-multi-leg shadow")
		// The one-column spelling of the same shadow ANSWERS, which is what
		// makes the arm above an arity pin rather than a decline in disguise.
		// The inner V is LA JOIN LB on AID=BID → the single row (K=100, K=5), so
		// X is 100; the OUTER V's X is BID ∈ {1,3}, so the value says which
		// generation the read landed on.
		if rows, err := run(t, `WITH "V" AS (SELECT "BID" AS "X" FROM LB) SELECT (WITH "V" AS (SELECT LA."K" AS "X", LB."K" AS "Y" FROM LA JOIN LB ON LA."AID" = LB."BID") SELECT "V"."X" FROM "V", CC) FROM LB LIMIT 1`); err != nil ||
			strings.Join(rows, ",") != "100" {
			t.Fatalf("one-column comma-multi-leg shadow read: rows=%v err=%v, want [100]", rows, err)
		}
	})
	// Q54: the SHADOW-CTE read-reach boundary. Two hand-derived installs were
	// tried and abandoned here (a lossy one silently mis-resolved quoted/dup
	// bodies; a read-sound one reopened flatten-evasion with an EXECUTION PANIC
	// on a comma-multi-leg shape, and its case-preserving Ids mismatched
	// execution's uppercase row keys) — both failures of a schema DERIVED beside
	// the body rather than FROM it. Boundary as it stands:
	//   - BARE read over the coinciding shadow answers (Q52);
	//   - QUALIFIED read (V."X") resolves too, rather than a silent NULL;
	//   - a 2+-extra-leg shadow read whose correlation cannot ordinalize is
	//     still LOUD — the arm below is the pin against the panic returning;
	//   - a DUPLICATE-name body declines the WHOLE source, so even a read of a
	//     unique column in it is loud (complete-schema-or-decline; see Q55 (d)).
	t.Run("Q54_shadow_read_reach_boundary", func(t *testing.T) {
		// The QUALIFIED read `V.X` over the inner V=[X] RESOLVES (a
		// single-namespace derived source, unique leaf `X` — GetByName
		// strips the self-qualifier), so `MAX(V.X)` returns the real value
		// `1|1` — IDENTICAL to the non-qualified sibling Q52 over the same
		// data (X'=CC.CID over the LEFT JOIN → {1, NULL} → MAX = 1). A
		// name-keyed resolution would have silently returned `<nil>` here.
		check(t, `WITH "V" AS (SELECT "BID" AS "X" FROM LB) SELECT (WITH "V" AS (SELECT CC."CID" AS "X" FROM "V" LEFT JOIN CC ON "V"."X" = CC."CID") SELECT MAX("V"."X") FROM "V") FROM LB LIMIT 1`,
			"1")
		_, ePanic := run(t, `WITH "V" AS (SELECT "BID" AS "B" FROM LB) SELECT (WITH "V" AS (SELECT LB."K" AS "B" FROM "V" LEFT JOIN CC ON "V"."B" = CC."CID" LEFT JOIN LA ON "V"."B" = LA."AID") SELECT COUNT(*) FROM "V", CC WHERE "V"."B" = 100) FROM LB LIMIT 1`)
		if ePanic == nil {
			t.Fatal("comma-multi-leg shadow read must be LOUD (an install panicked here), got rows")
		}
		// duplicate X in the body: complete-or-decline declines the WHOLE source
		// (0AF00), even for the unique Y — a partial "keep Y, drop X" install
		// is unsound (a dropped dup name rebinds to another enclosing source).
		// Recovering the unique-Y reach would need a poison-marker refinement
		// that isn't implemented yet.
		_, eDupY := run(t, `WITH "C" AS (SELECT LA."K" AS "X", LB."K" AS "X", LA."AID" AS "Y" FROM LA JOIN LB ON LA."AID" = LB."BID") SELECT "C"."Y", "C2"."CV" FROM "C" JOIN CC AS "C2" ON "C"."Y" = "C2"."CID"`)
		if eDupY == nil || !strings.Contains(eDupY.Error(), "0AF00") {
			t.Fatalf("duplicate-X body must decline the whole source (0AF00), got: %v", eDupY)
		}
	})
	// Q55 — the ON-only CTE schema is COMPLETE-SCHEMA-OR-DECLINE. It installs as
	// ONE source of the enclosing join; the resolver decides bare-ref ambiguity
	// by which SOURCES carry a name, so a PARTIAL install (advertise some runtime
	// columns, drop others) is unsound — a dropped column whose runtime key
	// another enclosing source also carries lets a bare ref SILENTLY bind there.
	// Any obstruction therefore declines the WHOLE source (loud 0AF00), never a
	// partial table. Obstructions, each keyed by the RUNTIME-emitted name
	// (executeProjection uppercases every output key): a quoted CASE-SENSITIVE
	// alias (`AS "x"` → runtime "X", no correct-case ref can name it), and a
	// DUPLICATE runtime name (`AS X, AS X`, or `AS "x", AS "X"` — both emit "X").
	t.Run("Q55_on_only_schema_complete_or_decline", func(t *testing.T) {
		// The regression each case guards against is a future schema change
		// that silently RESOLVES the obstructed reference and returns wrong
		// joined rows instead of declining. All obstructions decline the
		// whole source → a uniform 0AF00 (the caller's ON-drop guard), so
		// pin the CODE, not just err != nil.
		mustLoud := func(t *testing.T, sql string) {
			t.Helper()
			rows, err := run(t, sql)
			if err == nil {
				t.Fatalf("expected a LOUD 0AF00 decline, got rows=%v", rows)
			}
			if !strings.Contains(err.Error(), "0AF00") {
				t.Fatalf("expected 0AF00, got: %v", err)
			}
		}
		// (a) quoted-lowercase alias `AS "x"`. This obstruction is RETIRED, and
		//     the two arms below are what replaced it. The name-derived schema
		//     could not describe the row here — execution keys output columns
		//     UPPERCASE, so `AS "x"` emits "X" and no truthful advertisement of
		//     a column called "x" was possible — and declining both spellings
		//     was the correct-or-loud answer to that. The published row is now
		//     the body's own result type, whose field names are folded by the
		//     SAME rule execution uses, so the two spellings separate cleanly:
		//     "X" is the column and resolves; "x" is not and 42703s by name.
		//     (That the engine folds a QUOTED alias at all is a standing
		//     divergence from Java's case-sensitive quoted identifiers — a
		//     property of the projection's own naming, not of this scope, and
		//     pinned here so closing it shows up as a change in BOTH arms.)
		if rows, err := run(t, `WITH "C" AS (SELECT LA."AID" AS "x" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") SELECT "C2"."CV" FROM "C" JOIN CC AS "C2" ON "C"."X" = "C2"."CID"`); err != nil ||
			strings.Join(rows, ",") != "900" {
			t.Fatalf("folded-name read must join on the body's AID column: rows=%v err=%v", rows, err)
		}
		if _, err := run(t, `WITH "C" AS (SELECT LA."AID" AS "x" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") SELECT "C2"."CV" FROM "C" JOIN CC AS "C2" ON "C"."x" = "C2"."CID"`); err == nil ||
			!strings.Contains(err.Error(), "42703") {
			t.Fatalf("a spelling the published row does not carry must 42703, got %v", err)
		}
		// (b) DUPLICATE output name X — a `C."X"` ref declines, never a silent
		//     pick of the first of the two X columns.
		mustLoud(t, `WITH "C" AS (SELECT LA."AID" AS "X", LB."BID" AS "X", LA."K" AS "Y" FROM LA JOIN LB ON LA."AID" = LB."BID") SELECT "C2"."CV" FROM "C" JOIN CC AS "C2" ON "C"."X" = "C2"."CID"`)
		// (c) the UNSOUND partial-install hole: a dup name is
		//     DROPPED from a partial schema and REBINDS to another enclosing
		//     source. C has dup AID; the enclosing scope also has L.AID; a BARE
		//     `AID` in the ON is AMBIGUOUS (C's dup vs L's) and MUST NOT silently
		//     bind to L.AID and return cross-product rows. Complete-or-decline
		//     closes it: the dup-bodied C declines wholesale → 0AF00.
		mustLoud(t, `WITH "C" AS (SELECT LA."AID" AS "AID", LB."BID" AS "AID", LA."K" AS "Y" FROM LA JOIN LB ON LA."AID" = LB."BID") SELECT "L"."AID" FROM "C" JOIN LA AS "L" ON "AID" = "L"."AID"`)
		// (d) even a UNIQUE reference (Y) into a body that has a dup ELSEWHERE
		//     declines — the whole source is untrustworthy once any column is
		//     obstructed (the reach cost of complete-or-decline; a full-reach
		//     poison-marker fix that keeps unique columns is not implemented yet).
		mustLoud(t, `WITH "C" AS (SELECT LA."AID" AS "X", LB."BID" AS "X", LA."K" AS "Y" FROM LA JOIN LB ON LA."AID" = LB."BID") SELECT "C2"."CV" FROM "C" JOIN CC AS "C2" ON "C"."Y" = "C2"."CID"`)
		// (e) POSITIVE control — a CLEAN body (all-unique, case-safe) still
		//     INSTALLS and resolves (this is not a blanket always-decline).
		if _, err := run(t, `WITH "C" AS (SELECT LA."AID" AS "P", LB."BID" AS "Q" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") SELECT "C2"."CV" FROM "C" JOIN CC AS "C2" ON "C"."P" = "C2"."CID"`); err != nil {
			t.Fatalf("clean all-unique case-safe body must still resolve, got: %v", err)
		}
	})
	// Q56 — the AGGREGATE ON-only body (buildDerivedTableSourceFromAgg) is under
	// the SAME complete-or-decline gate as the projection path. It folds every
	// output via NewUnquoted with no validation, so before the gate a quoted
	// case-sensitive alias (`MIN(x) AS "x"`) resolved a wrong-case `C."X"` ON ref
	// and returned rows, and a duplicate aggregate alias silent-first-matched —
	// both now decline the whole source (0AF00), while a clean aggregate alias
	// still resolves.
	t.Run("Q56_agg_on_only_schema_complete_or_decline", func(t *testing.T) {
		mustLoud := func(t *testing.T, sql string) {
			t.Helper()
			rows, err := run(t, sql)
			if err == nil {
				t.Fatalf("expected a LOUD 0AF00 decline, got rows=%v", rows)
			}
			if !strings.Contains(err.Error(), "0AF00") {
				t.Fatalf("expected 0AF00, got: %v", err)
			}
		}
		// The quoted-lowercase alias obstruction is retired here for the same
		// reason as its projection twin (Q55 (a)): the published row's names are
		// folded by execution's own rule, so the folded spelling IS the column
		// and the unfolded one is honestly absent. MIN over AID ∈ {1,2} is 1,
		// which is the CID that matches — so the value, not merely the absence
		// of an error, is what this pins.
		if rows, err := run(t, `WITH "C" AS (SELECT MIN(LA."AID") AS "x" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") SELECT "C2"."CV" FROM "C" JOIN CC AS "C2" ON "C"."X" = "C2"."CID"`); err != nil ||
			strings.Join(rows, ",") != "900" {
			t.Fatalf("folded aggregate alias must join on MIN(AID)=1: rows=%v err=%v", rows, err)
		}
		if _, err := run(t, `WITH "C" AS (SELECT MIN(LA."AID") AS "x" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") SELECT "C2"."CV" FROM "C" JOIN CC AS "C2" ON "C"."x" = "C2"."CID"`); err == nil {
			t.Fatal("a spelling the published aggregate row does not carry must not resolve")
		}
		// duplicate aggregate output name → decline.
		mustLoud(t, `WITH "C" AS (SELECT MIN(LA."AID") AS "X", MAX(LB."BID") AS "X" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") SELECT "C2"."CV" FROM "C" JOIN CC AS "C2" ON "C"."X" = "C2"."CID"`)
		// POSITIVE control: a clean aggregate alias still installs and resolves.
		if _, err := run(t, `WITH "C" AS (SELECT MIN(LA."AID") AS "M" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") SELECT "C2"."CV" FROM "C" JOIN CC AS "C2" ON "C"."M" = "C2"."CID"`); err != nil {
			t.Fatalf("clean aggregate alias must still resolve, got: %v", err)
		}
		// HIDDEN aggregate must NOT be counted as a duplicate output: the visible
		// COUNT(*) (no alias, renders "COUNT(*)") and the hidden HAVING COUNT(*)
		// (harvested visible=false, same render) must not false-collide. The gate
		// and the schema both consume the VISIBLE-only aggOutputCols authority.
		if _, err := run(t, `WITH "C" AS (SELECT COUNT(*) FROM LA LEFT JOIN LB ON LA."AID" = LB."BID" HAVING COUNT(*) > 0) SELECT "C2"."CV" FROM "C" JOIN CC AS "C2" ON "C2"."CID" = 100`); err != nil {
			t.Fatalf("hidden HAVING aggregate must not false-decline the lone output, got: %v", err)
		}
	})
	t.Run("Q14_union_branch_on_resolves", func(t *testing.T) {
		check(t, `WITH "C" AS (`+cteBody+`) SELECT "C"."AK", "C2"."CID" FROM "C" JOIN CC AS "C2" ON "C"."BK" = "C2"."CID" UNION ALL SELECT LA."K", 0 FROM LA WHERE LA."K" = 110`,
			"110|0")
	})
}
