package sqldriver_test

// RFC-173 B2 sub-slice A — the impl-review's shape battery, kept as PERMANENT
// regression pins (the review's own requirement: the CTE double-reference class
// gets a pinned regression).
//  P1  CTE double-reference: a CTE body containing the filtered box unnest is
//      ONE shared logical tree; two references translate the SAME node twice in
//      one translator. The unnestGatherBoxLegTypes record is CONSUME-ONCE (the
//      enclosedGatherCache discipline) precisely so a later translation whose
//      gather declined (enclosure) can never bake over a stale record — the
//      pre-fix failure was a loud "baked FieldValue evaluated against a
//      non-positional row context". Both legs answer correct rows: the
//      enclosed leg's qualified reads resolve via the schema-complete merge
//      fabrication (executor qualifyAlias/qualifyOuterRow on Complete legs) —
//      these pins were the all-NULL flip-sentinels for that bug until the
//      executor fix landed.
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

func TestFDB_RFC173B2_GraefeImplProbe(t *testing.T) {
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
		fd := d.Fields().ByName("ARR")
		l := m.NewField(fd).List()
		for _, v := range vals {
			l.Append(protoreflect.ValueOfInt32(v))
		}
		m.Set(fd, protoreflect.ValueOfList(l))
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
				if m, isMap := r.Datum.(map[string]any); isMap {
					keys := make([]string, 0, len(m))
					for k := range m {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					parts := make([]string, 0, len(keys))
					for _, k := range keys {
						parts = append(parts, unnestSprint(m[k]))
					}
					out = append(out, strings.Join(parts, "|"))
				} else {
					out = append(out, unnestSprint(r.Datum))
				}
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
func TestFDB_RFC173B2_GraefeImplProbe2(t *testing.T) {
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
		fd := d.Fields().ByName("ARR")
		l := m.NewField(fd).List()
		for _, v := range vals {
			l.Append(protoreflect.ValueOfInt32(v))
		}
		m.Set(fd, protoreflect.ValueOfList(l))
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
				if m, isMap := r.Datum.(map[string]any); isMap {
					keys := make([]string, 0, len(m))
					for k := range m {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					parts := make([]string, 0, len(keys))
					for _, k := range keys {
						parts = append(parts, unnestSprint(m[k]))
					}
					out = append(out, strings.Join(parts, "|"))
				} else {
					out = append(out, unnestSprint(r.Datum))
				}
			}
			return nil, nil
		})
		if eerr != nil {
			return nil, eerr
		}
		return out, nil
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
		got, err := run(t, sql)
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		if strings.Join(got, ",") != strings.Join(expect, ",") {
			t.Fatalf("ordered rows = %v, want %v\n  %s", got, expect, sql)
		}
	}

	const cteBody = `SELECT LA."K" AS "AK", LB."K" AS "BK", "X" AS "XV" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID", LA."ARR" AS "X" WHERE LA."K" = 100`
	const cteBodyNoWhere = `SELECT LA."K" AS "AK", LB."K" AS "BK", "X" AS "XV" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID", LA."ARR" AS "X"`

	// Q1: single ENCLOSED reference of the FILTERED body (no double ref — no
	// record can exist). The enclosed leg's qualified reads (C.AK etc.)
	// resolve via the schema-complete merge fabrication — this pin used to be
	// the all-NULL flip-sentinel for the enclosed-CTE silent-NULL bug (the
	// executor's qualifyAlias refused to fabricate C.* keys for the
	// projection-output leg); it flipped when that fix landed.
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
			"<nil>|100", "<nil>|110")
	})
	// The derived-table twin keeps its pre-existing LOUD 0AF00 (its schema
	// derivation is a separate booked widening); the message names the exact
	// hazard the CTE class silently hit before the fix.
	t.Run("Q9e_derived_twin_stays_loud", func(t *testing.T) {
		_, err := run(t, `SELECT "C"."AK", "CC2"."CV" FROM (`+cteBodyNoWhere+`) AS "C" LEFT JOIN CC AS "CC2" ON "C"."AK" = "CC2"."CID"`)
		if err == nil || !strings.Contains(err.Error(), "0AF00") {
			t.Fatalf("derived-table twin must stay LOUD 0AF00 (schema derivation booked separately), got %v", err)
		}
	})
	// Q10: the SCALAR-SUBQUERY build path — the review-proven threading hole.
	// The subquery's inner plan builds through the CTECatalog chain, which
	// initially never received the ON-only scopes: the ON of `C JOIN CC2`
	// inside the subquery was silently dropped and COUNT counted the CROSS
	// product (3) instead of the joined answer (0 — no C.AK ∈ {100,110}
	// equals CID=1). The discriminator never collides with NULL, so a broken
	// subquery cannot masquerade as either answer.
	t.Run("Q10_scalar_subquery_path_on_resolves", func(t *testing.T) {
		// The datum keys the subquery value twice (its rendered name + the
		// positional _N key), so the joined COUNT appears in two slots.
		check(t, `WITH "C" AS (`+cteBodyNoWhere+`) SELECT LA."K", (SELECT COUNT(*) FROM "C" JOIN CC AS "C2" ON "C"."AK" = "C2"."CID") FROM LA WHERE LA."K" = 110`,
			"0|110|0")
	})
	// Q11: an UNALIASED QUALIFIED projection in a multi-leg CTE body — the
	// runtime row keys that slot by its qualified source name ("LA.K", no bare
	// key), so no ON-only schema is derivable that matches execution; the
	// declared-CTE drop-risk arm keeps the enclosing ON LOUD (0AF00) instead
	// of resolving a name the merged row never carries (a silent runtime miss).
	// Widening (a real post-CTE output schema) is booked with the derived twin.
	t.Run("Q11_unaliased_qualified_body_stays_loud", func(t *testing.T) {
		_, err := run(t, `WITH "UQ" AS (SELECT LA."K" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") SELECT "UQ"."K" FROM "UQ" JOIN CC AS "C2" ON "UQ"."K" = "C2"."CID"`)
		if err == nil || !strings.Contains(err.Error(), "0AF00") {
			t.Fatalf("unaliased-qualified multi-leg CTE ON must stay LOUD 0AF00, got %v", err)
		}
	})
	// Q12: WITH c(x) COLUMN ALIASES over a multi-leg body — the renames exist
	// in the scope view only, never on the runtime row, so deriving them here
	// would turn today's loud failure into a silent runtime miss. Loud.
	t.Run("Q12_column_aliased_multileg_body_stays_loud", func(t *testing.T) {
		_, err := run(t, `WITH "CA" ("X") AS (SELECT LA."K" AS "AK" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") SELECT "CA"."X" FROM "CA" JOIN CC AS "C2" ON "CA"."X" = "C2"."CID"`)
		if err == nil {
			t.Fatal("column-aliased multi-leg CTE ON must fail LOUD, got rows")
		}
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
	// the ON resolves and pads correctly. A first over-narrowing declined
	// this to 0AF00, regressing the class vs its pre-narrowing behavior
	// (review-caught).
	t.Run("Q15_bare_ref_body_on_resolves", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT "AID" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"900|1", "<nil>|2")
	})
	// Q16: a DERIVED-SOURCE CTE body (`FROM (SELECT …) d` — zero joins, but
	// declined by the global deriver for the derivedQuery reason) derives its
	// ON-only schema from the projection list like any multi-leg body —
	// review-caught sibling of Q15 (the joins==0 early-decline wrongly
	// assumed the global deriver owned every zero-join shape).
	t.Run("Q16_derived_source_body_on_resolves", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT "D"."AID" AS "A" FROM (SELECT "AID" FROM LA) AS "D") SELECT "U"."A", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."A" = "C2"."CID"`,
			"900|1", "<nil>|2")
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
			// an unrelated reason (review-caught assertion-strength gap).
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
	// Q18: an AMBIGUOUS bare ref in a multi-leg body with a DERIVED leg. The
	// bare-ref admission rests on the body build's 42702 ambiguity backstop,
	// which only sees ENUMERABLE (base-table) legs — a derived leg hides its
	// columns, the check never fires, and the bare ref silently resolved
	// against the OTHER leg (review-caught: rows came back where an error was
	// due). The derivation now declines any multi-leg body with a derived
	// leg. (The standalone body's missing 42702 is a separate pre-existing
	// resolver gap, booked with the derived-table scope item.)
	t.Run("Q18_derived_leg_ambiguous_bare_loud", func(t *testing.T) {
		loud0AF00(t, `WITH "U" AS (SELECT "AID" FROM (SELECT LA."AID" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") "D", LA "L2") SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"ambiguous bare ref over a derived leg")
	})
	// Q19: derived-source body whose INNER projection is QUALIFIED-spelled —
	// the derived row keys "LA.AID", so the outer D.AID read can never
	// resolve at runtime (review-caught: admission turned the base's clean
	// plan-time 0AF00 into a runtime malformed-plan error). The read-authority
	// recursion (derivedEmittedBareNames) declines it back to plan time.
	t.Run("Q19_derived_inner_qualified_loud", func(t *testing.T) {
		loud0AF00(t, `WITH "U" AS (SELECT "D"."AID" AS "A" FROM (SELECT LA."AID" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") "D") SELECT "U"."A", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."A" = "C2"."CID"`,
			"aliased read over a qualified-keyed derived row")
	})
	// Q20: the AGGREGATE-arm instance of Q19 — MAX(D.AID) over a
	// qualified-keyed derived row read NOTHING and returned a SILENT NULL
	// (found by widening the review's probe to the agg arm; the worst variant
	// of the class — no error at all). cteBodyReadsResolvable validates agg
	// args/group cols against the emitted set.
	t.Run("Q20_agg_over_derived_qualified_loud", func(t *testing.T) {
		loud0AF00(t, `WITH "U" AS (SELECT MAX("D"."AID") AS "M" FROM (SELECT LA."AID" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") "D") SELECT "U"."M", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."M" = "C2"."CID"`,
			"aggregate arg over a qualified-keyed derived row")
	})
	// Q21: a derived JOIN LEG (joinClause.derivedQuery — the non-first-source
	// twin of Q18). Was a runtime leg-adapter breach; now a plan-time decline.
	t.Run("Q21_derived_join_leg_loud", func(t *testing.T) {
		loud0AF00(t, `WITH "U" AS (SELECT "AID" FROM LA "L2" JOIN (SELECT LB."BID" FROM LB) "D" ON "L2"."AID" = "D"."BID") SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"derived join leg")
	})
	// Q22: ANTI-OVER-DECLINE — aggregate over a derived-inner-BARE body stays
	// derivable and answers correctly (M = MAX(1,2) = 2, no CC match → pad).
	// The read validation must pass when the inner emits bare keys.
	t.Run("Q22_agg_over_derived_bare_resolves", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT MAX("D"."AID") AS "M" FROM (SELECT "AID" FROM LA) "D") SELECT "U"."M", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."M" = "C2"."CID"`,
			"<nil>|2")
	})
	// Q23: ANTI-OVER-DECLINE — a COMPUTED aliased item whose reads resolve in
	// the inner's bare set stays derivable (harvestColumnRefs validates the
	// expression's refs, it does not blanket-decline computed items).
	t.Run("Q23_computed_over_derived_bare_resolves", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT "D"."AID" + 0 AS "Z" FROM (SELECT "AID" FROM LA) "D") SELECT "U"."Z", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."Z" = "C2"."CID"`,
			"900|1", "<nil>|2")
	})
	// Q24: union-bodied CTE under ON — the union pathway NORMALIZES branch
	// keys (branch-2 spelled qualified still resolves U.AID), for both a
	// plain and a JOIN-SEEDED union. Pins the seed-arm advertising as sound;
	// probed while hunting Q18-siblings — the union arm needs no decline.
	t.Run("Q24_union_branch_keys_normalize", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT "AID" FROM LA UNION ALL SELECT LB."BID" FROM LB) SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"900|1", "<nil>|2", "900|1", "<nil>|3")
		check(t, `WITH "U" AS (SELECT "AID" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID" UNION ALL SELECT LB."BID" FROM LB) SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"900|1", "<nil>|2", "900|1", "<nil>|3")
	})
	// Q25: computed item over a qualified-keyed derived row — the reads fail
	// validation, decline (was already loud at translation; the decline moves
	// it to the uniform 0AF00).
	t.Run("Q25_computed_over_derived_qualified_loud", func(t *testing.T) {
		loud0AF00(t, `WITH "U" AS (SELECT "D"."AID" + 0 AS "Z" FROM (SELECT LA."AID" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") "D") SELECT "U"."Z", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."Z" = "C2"."CID"`,
			"computed reads over a qualified-keyed derived row")
	})
	// Q26: the bare UNNEST-ELEMENT ref — the one admitted derivation shape
	// whose write path goes through the RFC-142 QOV value (shadowing rewrite)
	// rather than a plain name-model read; the rewrite qualifies via the QOV
	// CHILD while Field stays the verbatim bare name, so the runtime key is
	// bare "X" (review-caught as the unpinned admitted class). CD keys on the
	// element VALUE (XK=7 → XV=700), so a silent-miss would pad ALL rows —
	// the 700 row discriminates.
	t.Run("Q26_bare_unnest_element_on_resolves", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT "X" FROM LA, LA."ARR" AS "X") SELECT "U"."X", "C3"."XV" FROM "U" LEFT JOIN CD AS "C3" ON "U"."X" = "C3"."XK"`,
			"700|7", "<nil>|8", "<nil>|9")
	})
	// Q27-Q29: the ON-ONLY-CTE-LEG hole (review-caught, round 7). An ON-only
	// CTE used as a FROM leg is neither a base table nor a derivedQuery — and
	// buildSelectScope returns a NIL resolver for it, killing BOTH the 42702
	// ambiguity gate and the 42703 unknown-column gate for the whole body.
	// The enumerability walk (cteBodyLegsEnumerable) declines such bodies.
	// V is join-bodied (ON-only); its alias AID collides with LA's column.
	const onOnlyV = `"V" AS (SELECT LA."K" AS "AID", LB."K" AS "Q" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID")`
	t.Run("Q27_on_only_cte_leg_ambiguous_loud", func(t *testing.T) {
		loud0AF00(t, `WITH `+onOnlyV+`, "U" AS (SELECT "AID" FROM "V", LA "L2") SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"ambiguous bare ref over an ON-only CTE leg")
		// both leg orders — the walk must not depend on leg position
		loud0AF00(t, `WITH `+onOnlyV+`, "U" AS (SELECT "AID" FROM LA "L2", "V") SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"ambiguous bare ref, ON-only CTE as second leg")
	})
	t.Run("Q28_on_only_cte_leg_nonexistent_col_loud", func(t *testing.T) {
		loud0AF00(t, `WITH `+onOnlyV+`, "U" AS (SELECT "NOPE" FROM "V", LA "L2") SELECT "U"."NOPE", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."NOPE" = "C2"."CID"`,
			"nonexistent column over an ON-only CTE leg (nil resolver kills 42703)")
	})
	t.Run("Q29_on_only_cte_leg_in_derived_source_loud", func(t *testing.T) {
		loud0AF00(t, `WITH `+onOnlyV+`, "U" AS (SELECT "AID" FROM (SELECT "AID" FROM "V", LA "L2") "D") SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"ON-only CTE leg one level down (recursion inherits the enumerability walk)")
	})
	// Q30: ANTI-OVER-DECLINE — a DERIVABLE CTE leg is enumerable (addSource
	// resolves it via cteScopes; the backstop lives) and the bare ref
	// admits + answers. AID lives only in W (LB carries BID/K); W×LB cross
	// doubles each AID row.
	t.Run("Q30_derivable_cte_leg_resolves", func(t *testing.T) {
		check(t, `WITH "W" AS (SELECT "AID" FROM LA), "U" AS (SELECT "AID" FROM "W", LB "L3") SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"900|1", "900|1", "<nil>|2", "<nil>|2")
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
	// Q33: the POSITIONAL-FRONTIER class (review-caught over-decline): a
	// single-BASE-TABLE derived source keeps its projection row positional,
	// so a QUALIFIED-spelled inner item is readable by ordinal under its
	// last segment — this shape must ADMIT and ANSWER (contrast Q19, whose
	// JOIN-shaped inner row is name-keyed and stays declined).
	t.Run("Q33_single_table_qualified_inner_resolves", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT "D"."AID" AS "A" FROM (SELECT LA."AID" FROM LA) "D") SELECT "U"."A", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."A" = "C2"."CID"`,
			"900|1", "<nil>|2")
	})
	// Q34: a scalar subquery inside a computed item reads ITS OWN scope, not
	// the derived source — harvestColumnRefsOutsideSubqueries stops at the
	// nested-query boundary (review-caught over-decline: LB.K was checked
	// against D's emitted set {AID} and spuriously declined).
	t.Run("Q34_scalar_subquery_item_over_derived_resolves", func(t *testing.T) {
		check(t, `WITH "U" AS (SELECT (SELECT "K" FROM LB ORDER BY "K" DESC LIMIT 1) AS "Z", "AID" FROM (SELECT "AID" FROM LA) "D") SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"900|1", "<nil>|2")
	})
	// Q35: the hazard arm the boundary-stop must NOT unguard — a subquery
	// CORRELATED into a JOIN-shaped (name-keyed) derived source. Admission
	// no longer inspects the subquery's refs, but the correlated build fails
	// loud at translation (pinned so a future silent path can't creep in).
	t.Run("Q35_correlated_subquery_into_join_keyed_derived_loud", func(t *testing.T) {
		loud0AF00(t, `WITH "U" AS (SELECT (SELECT MAX("K") FROM LB WHERE LB."BID" = "D"."AID") AS "Z" FROM (SELECT LA."AID" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") "D") SELECT "U"."Z", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."Z" = "C2"."CID"`,
			"correlated subquery into a name-keyed derived source")
	})
	// Q32: mixed-star derived source — the star-EXPANDED columns are not in
	// the emitted set (no catalog access at derivation), so an outer read of
	// one declines fail-closed (also exercises the empty-sentinel guard: the
	// star slot must not deposit a "" claim).
	t.Run("Q32_mixed_star_inner_fail_closed", func(t *testing.T) {
		loud0AF00(t, `WITH "U" AS (SELECT "D"."K" AS "KK" FROM (SELECT LA.*, "AID" FROM LA) "D") SELECT "U"."KK", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."KK" = "C2"."CID"`,
			"star-expanded column read over a mixed-star derived source")
	})
	// Q36: CTE SHADOWING a catalog table (review-caught, round 8). The leg
	// classifier must mirror EXECUTION's resolution order — a declared CTE
	// shadows a same-named table, so a metadata-first lookup classified the
	// leg by the TABLE's schema while runtime rows came from the CTE (every
	// reachable variant probed LOUD at runtime — malformed-plan, row columns
	// [Z] — but the classification was wrong and the error class regressed
	// from plan-time 0AF00). CTE names now classify FIRST: an ON-only "LA"
	// is opaque → all three variants decline at plan time.
	t.Run("Q36_cte_shadows_table_loud", func(t *testing.T) {
		const shadowLA = `"LA" AS (SELECT LB."K" AS "Z" FROM LB LEFT JOIN CC ON LB."BID" = CC."CID")`
		loud0AF00(t, `WITH `+shadowLA+`, "U" AS (SELECT "D"."AID" AS "A" FROM (SELECT LA."AID" FROM LA) "D") SELECT "U"."A", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."A" = "C2"."CID"`,
			"derived source over a table-shadowing ON-only CTE")
		loud0AF00(t, `WITH `+shadowLA+`, "U" AS (SELECT MAX("D"."AID") AS "M" FROM (SELECT LA."AID" FROM LA) "D") SELECT "U"."M", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."M" = "C2"."CID"`,
			"aggregate arm over a table-shadowing ON-only CTE")
		loud0AF00(t, `WITH `+shadowLA+`, "U" AS (SELECT "AID" FROM "LA", CC "C9") SELECT "U"."AID", "C2"."CV" FROM "U" LEFT JOIN CC AS "C2" ON "U"."AID" = "C2"."CID"`,
			"shadowed CTE as a multi-leg body leg")
	})
	// Q37: SCHEMA-QUALIFIED legs (review-caught, round 8). Three stacked
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
			"<nil>|100|5", "<nil>|110|<nil>")
	})
	// Q38: the standalone (no CTE) schema-qualified explicit-join pins — the
	// pre-existing silent cross-product class in its own right, all four
	// shapes: LEFT with matched+pad rows, INNER keeps ONLY the match,
	// explicit aliases, one-leg-qualified.
	t.Run("Q38_schema_qualified_join_on_live", func(t *testing.T) {
		// (top-level datums key each output twice — rendered + positional —
		// hence the doubled columns, same as Q10)
		check(t, `SELECT LA."K" AS "AK", LB."K" AS "BK" FROM "s"."LA" LEFT JOIN "s"."LB" ON LA."AID" = LB."BID"`,
			"100|5|100|5", "110|<nil>|110|<nil>")
		check(t, `SELECT LA."K" AS "AK", LB."K" AS "BK" FROM "s"."LA" JOIN "s"."LB" ON LA."AID" = LB."BID"`,
			"100|5|100|5")
		check(t, `SELECT "X"."K" AS "AK", "Y"."K" AS "BK" FROM "s"."LA" AS "X" LEFT JOIN "s"."LB" AS "Y" ON "X"."AID" = "Y"."BID"`,
			"100|5|100|5", "110|<nil>|110|<nil>")
		check(t, `SELECT LA."K" AS "AK", LB."K" AS "BK" FROM "s"."LA" LEFT JOIN LB ON LA."AID" = LB."BID"`,
			"100|5|100|5", "110|<nil>|110|<nil>")
	})
	// Q39: the 42702 backstop LIVES over schema-qualified legs (review-caught,
	// round 9). The round-8 classifier strip said "s"."LA" was enumerable,
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
	// nil-resolver disease as Q39, one consumer over (found as a loud 0AF00
	// reach gap by the round-8 review's third-consumer hunt; the backstop
	// fix flipped it to answering, exactly as that review predicted the
	// unification would).
	t.Run("Q40_where_over_schema_qualified_join_answers", func(t *testing.T) {
		check(t, `SELECT LA."K" AS "AK" FROM "s"."LA" LEFT JOIN "s"."LB" ON LA."AID" = LB."BID" WHERE LA."K" = 100`,
			"100|100")
	})
	// Q41: the scope builder resolves a CTE-shadowed name through the CTE's
	// OUTPUT schema, not the same-named table's (review-caught, round 10):
	// addSource was catalog-first, so `LA."X"` — X being the CTE's renamed
	// column — 42703'd against the base table. The plain-name variant was
	// broken this way ALL ALONG; the schema-qualified variant regressed in
	// round 9 when the resolver went live. CTE-first now, mirroring
	// execution's shadowing (and cteLegKind's ordering). X = BID values
	// {1,3} × 2 B-rows.
	t.Run("Q41_cte_shadow_scope_reads_cte_schema", func(t *testing.T) {
		check(t, `WITH "LA" AS (SELECT "BID" AS "X" FROM LB) SELECT "LA"."X" FROM "LA", "s"."LB" AS "B"`,
			"1", "1", "3", "3")
		check(t, `WITH "LA" AS (SELECT "BID" AS "X" FROM LB) SELECT "LA"."X" FROM "LA", LB AS "B"`,
			"1", "1", "3", "3")
	})
	// Q42: a bare ORDER BY key naming a UNIQUE OUTPUT ALIAS takes precedence
	// over FROM-scope ambiguity (review-caught, round 10): both legs carry a
	// column K, but the sort executes over the projected row where alias K
	// is unambiguous — the validation's ambiguity arm now defers to the
	// alias exactly like its ColumnNotFound arm always did. checkOrdered:
	// K DESC = 2,2,1,1 (the ordering itself is the assertion).
	t.Run("Q42_orderby_alias_precedes_scope_ambiguity", func(t *testing.T) {
		checkOrdered(t, `SELECT LA."AID" AS "K", LB."BID" FROM "s"."LA", LB ORDER BY "K" DESC`,
			"2|2|1", "2|2|3", "1|1|1", "1|1|3")
	})
	// Q43: the live resolver's strictness dividend, pinned against a
	// leniency regression (review-requested): a reference through the
	// ALIASED-AWAY table name is 42703 — pre-round-9 the nil resolver let
	// it through leniently.
	t.Run("Q43_aliased_away_name_is_42703", func(t *testing.T) {
		_, err := run(t, `SELECT LA."K" FROM "s"."LA" AS "X" LEFT JOIN "s"."LB" AS "Y" ON "X"."AID" = "Y"."BID"`)
		if err == nil || !strings.Contains(err.Error(), "42703") {
			t.Fatalf("aliased-away table-name reference must 42703, got %v", err)
		}
	})
	// Q44: a NON-recursive CTE body is SELF-INVISIBLE on every
	// register-before-build path (review-caught on all three gates, round
	// 11): the chain pipelines complete registration before building
	// bodies, so CTE-first scope resolution made `FROM LB` inside the CTE
	// "LB" resolve to ITSELF — a bogus correlated-fallback misroute (0A000)
	// and, through BuildScalar's 42703 arm, a silent base-table value
	// substitution. buildCTEBodySelfHidden now guards the visitor eager
	// build, the visitor rebuild, and BOTH chain loops. COUNT(*)=2 is the
	// TABLE's row count read through the self-named CTE.
	t.Run("Q44_self_named_cte_body_reads_table", func(t *testing.T) {
		// The body filter (BID = 1) makes the count VALUE-DISCRIMINATING:
		// the table-read gives 1, any row-preserving misroute over the
		// unfiltered generation gives 2 (review-caught: the unfiltered
		// COUNT(*)=2 was value-degenerate with the table cardinality — the
		// exact green-masking pattern the Q49 forensic demonstrated).
		check(t, `SELECT LA."K", (WITH "LB" AS (SELECT "BID" AS "X" FROM LB WHERE "BID" = 1) SELECT COUNT(*) FROM "LB") FROM LA WHERE LA."K" = 100`,
			"1|100|1")
		// differently-named control (never broken — isolates causation)
		check(t, `SELECT LA."K", (WITH "W9" AS (SELECT "BID" AS "X" FROM LB WHERE "BID" = 1) SELECT COUNT(*) FROM "W9") FROM LA WHERE LA."K" = 100`,
			"1|100|1")
		// the WithCTECatalog-route twin (outer WITH forces the other chain)
		check(t, `WITH "W0" AS (SELECT "AID" FROM LA) SELECT (WITH "LB" AS (SELECT "BID" AS "X" FROM LB WHERE "BID" = 1) SELECT COUNT(*) FROM "LB") FROM "W0"`,
			"1|1", "1|1")
	})
	// Q45: an enclosing ON through a SHADOWING derivable CTE resolves
	// against the CTE's OUTPUT schema (review-caught, round 11): the
	// ON-upgrade's resolveTable was analyzer-first — the fourth and last
	// catalog-first consumer — over-declining valid ONs (42703 on the CTE's
	// renamed column) and pushing the table-only-column shape to a RUNTIME
	// malformed plan. CTE-first now: the valid ON answers (X∈{1,3}, CID=1
	// matches X=1), and the table-only column fails plan-time 42703.
	t.Run("Q45_on_through_shadowing_cte", func(t *testing.T) {
		check(t, `WITH "LA" AS (SELECT "BID" AS "X" FROM LB) SELECT "LA"."X", CC."CV" FROM "LA" JOIN CC ON "LA"."X" = CC."CID"`,
			"900|1")
		_, err := run(t, `WITH "LA" AS (SELECT "BID" AS "X" FROM LB) SELECT "LA"."X" FROM "LA" JOIN CC ON "LA"."K" = CC."CID"`)
		if err == nil || !strings.Contains(err.Error(), "42703") {
			t.Fatalf("table-only column through a shadowing CTE's ON must fail plan-time 42703, got %v", err)
		}
	})
	// Q46: ORDER BY output-alias precedence works through the SUBQUERY
	// build path too (review-caught: the postBuild validation has its own
	// ambiguity arm that was missed in round 10 — the same query answered
	// top-level but 42702'd inside a scalar subquery).
	t.Run("Q46_orderby_alias_in_subquery_path", func(t *testing.T) {
		check(t, `SELECT (SELECT LA."AID" AS "KK" FROM "s"."LA", LB ORDER BY "KK" DESC LIMIT 1), LA."K" FROM LA WHERE LA."K" = 100`,
			"2|100|2")
	})
	// Q47+Q48: the two review-caught over-suppressions the round-10 alias
	// bypass would have allowed — DUPLICATE output aliases must NOT bypass
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
	// Q49: a NESTED same-named CTE's body sees the OUTER binding
	// (review-caught, round 12): the inner registration overwrites the
	// level map's outer entry, so a plain self-DELETE lost BOTH bindings
	// and the inner body's reads fell to the base TABLE (42703 on the outer
	// CTE's renamed column). buildCTEBodySelfHidden now swaps to the
	// PRE-REGISTRATION snapshot — self invisible, outer visible. MAX(X)=3
	// is over the OUTER CTE's X∈{1,3}, per outer row.
	t.Run("Q49_nested_shadow_body_sees_outer", func(t *testing.T) {
		check(t, `WITH "LA" AS (SELECT "BID" AS "X" FROM LB) SELECT (WITH "LA" AS (SELECT LA."X" AS "X" FROM "LA") SELECT MAX("X") FROM "LA"), "O"."X" FROM "LA" AS "O"`,
			"3|1|3", "3|3|3")
	})
	// Q50: an inner ON-ONLY CTE shadowing an outer derivable EVICTS the
	// stale outer entry (review-caught, round 13): registration wrote only
	// cteOnScopes, leaving the outer schema installed for this level's MAIN
	// query — the inner CTE's reads resolved against the OUTER generation
	// (a read of the inner's real column Y misrouted 0A000; a read of the
	// outer-only column X silently accepted). Post-evict, Y answers through
	// the real inner CTE (MAX(CV)=900). The X read now lands in the booked
	// ON-only READ class's lenient silence (see the flip-sentinel below) —
	// the STALE-SCHEMA mechanism itself is gone.
	t.Run("Q50_ononly_shadow_evicts_stale_outer", func(t *testing.T) {
		check(t, `WITH "V" AS (SELECT "BID" AS "X" FROM LB) SELECT (WITH "V" AS (SELECT CC."CV" AS "Y" FROM "V" LEFT JOIN CC ON "V"."X" = CC."CID") SELECT MAX("Y") FROM "V") FROM LB LIMIT 1`,
			"900|900")
	})
	// Q51: the booked ON-only READ class — the MAX-scalar-subquery variants
	// that resolve through buildSelectScope's nil-resolver LENIENCY + the
	// executor merge fabrication (the path that is load-bearing for the
	// enclosed comma-FROM reads Q1-Q5), INDEPENDENT of any install. NO-SHADOW
	// invalid read → SILENT NULL. SHADOW different-schema read (inner exposes
	// Y, read X) → SILENT NULL (the evict removed the stale outer; the read
	// finds no X on the inner row). Both are FLIP-SENTINELS for the booked
	// cteOnScopes-aware read resolution (carefully — the flatten-evasion gate
	// pin must hold); each flips to a loud 42703-family error with a truth
	// pass here. Contrast Q53: the WHERE-based reads at the main-query level
	// are already LOUD (0AF00), so only these leniency-path scalar reads
	// remain silent.
	t.Run("Q51_ononly_invalid_read_class", func(t *testing.T) {
		// RFC-173 cap: the BOOKED FLIP has landed. Reading a column ABSENT from the
		// derived source (`NOPE`/`X` are not columns of W=[Y] / inner V=[Y]) is now a
		// LOUD ordinal-resolution error, not the name model's silent NULL — exactly
		// the "flips to a loud 42703-family error" the header booked. (The error is a
		// runtime ordinal-resolution miss today; a clean plan-time 42703
		// column-not-found is a booked message-quality refinement — the direction,
		// loud-not-silent, is the cap contract.)
		mustLoudRead := func(t *testing.T, sql string) {
			t.Helper()
			rows, err := run(t, sql)
			if err == nil {
				t.Fatalf("expected a LOUD absent-column read error (booked flip-sentinel), got rows=%v\n  %s", rows, sql)
			}
			if !strings.Contains(err.Error(), "not resolvable") {
				t.Fatalf("expected an ordinal-resolution (absent column) error, got: %v\n  %s", err, sql)
			}
		}
		mustLoudRead(t, `WITH "W" AS (SELECT CC."CV" AS "Y" FROM LB LEFT JOIN CC ON LB."BID" = CC."CID") SELECT MAX("NOPE") FROM "W"`)
		mustLoudRead(t, `WITH "V" AS (SELECT "BID" AS "X" FROM LB) SELECT (WITH "V" AS (SELECT CC."CV" AS "Y" FROM "V" LEFT JOIN CC ON "V"."X" = CC."CID") SELECT MAX("X") FROM "V") FROM LB LIMIT 1`)
	})
	// Q52: the COINCIDING-schema shadow read ANSWERS CORRECTLY through the
	// leniency + merge-fabrication path (NOT via any install — an earlier
	// round's install was unnecessary here AND is reverted): the evict
	// removes the stale outer, and the read of X resolves against the inner
	// row, which genuinely carries X' = CC.CID over (outer X∈{1,3} LEFT JOIN
	// CC ON X=CID) → {1, NULL} → MAX = 1. This is the discriminator that the
	// evict fixed the stale generation (base returned the OUTER X; a stale
	// read here would give MAX(outer X) = 3, not 1).
	t.Run("Q52_ononly_shadow_coinciding_schema_answers", func(t *testing.T) {
		check(t, `WITH "V" AS (SELECT "BID" AS "X" FROM LB) SELECT (WITH "V" AS (SELECT CC."CID" AS "X" FROM "V" LEFT JOIN CC ON "V"."X" = CC."CID") SELECT MAX("X") FROM "V") FROM LB LIMIT 1`,
			"1|1")
	})
	// Q53: the LOSSY-INSTALL regression pins (review-caught, round 15). An
	// earlier round installed the inner shadow CTE's ON-only DERIVED schema
	// into the global cteScopes to make an exotic coinciding read answer —
	// but buildCTEOnOnlySource's schema is ON-resolution-only and LOSSY
	// (NewUnquoted; permits duplicate output names), so promoting it to
	// general reads SILENTLY MIS-RESOLVED quoted-alias and duplicate-name
	// bodies. The install is reverted (plain evict, matching the derivable
	// arm's shadow delete); these WHERE-based reads now FAIL CLOSED (0AF00),
	// the correct-or-loud state — an install would silently accept them.
	t.Run("Q53_lossy_shadow_reads_fail_closed", func(t *testing.T) {
		// quoted lowercase "x" inner alias; WHERE "X"=1 uppercase must not
		// resolve through a case-folded install.
		loud0AF00(t, `WITH "V" AS (SELECT "BID" AS "X" FROM LB) SELECT (WITH "V" AS (SELECT CC."CID" AS "x" FROM "V" LEFT JOIN CC ON "V"."X" = CC."CID") SELECT COUNT(*) FROM "V" WHERE "X" = 1) FROM LB LIMIT 1`,
			"quoted-alias shadow read")
		// duplicate output name X; WHERE X=1 must not pick one column.
		loud0AF00(t, `WITH "V" AS (SELECT "BID" AS "X" FROM LB) SELECT (WITH "V" AS (SELECT LB."BID" AS "X", LB."K" AS "X" FROM "V" LEFT JOIN LB ON "V"."X" = LB."BID") SELECT COUNT(*) FROM "V" WHERE "X" = 1) FROM LB LIMIT 1`,
			"duplicate-name shadow read")
		// the comma-multi-leg shadow (my banked flatten-evasion probe): a
		// join-bodied inner installed into cteScopes would reopen the
		// flatten-evasion silent class; the evict keeps it loud.
		loud0AF00(t, `WITH "V" AS (SELECT "BID" AS "X" FROM LB) SELECT (WITH "V" AS (SELECT LA."K" AS "X", LB."K" AS "Y" FROM LA JOIN LB ON LA."AID" = LB."BID") SELECT "V"."X", "V"."Y" FROM "V", CC WHERE "V"."X" = CC."CID") FROM LB LIMIT 1`,
			"comma-multi-leg shadow read")
	})
	// Q54: the SHADOW-CTE read-reach boundary — the terminal state after the
	// install approach was tried TWICE and abandoned (a lossy install
	// silently mis-resolved quoted/dup bodies; a read-sound install reopened
	// flatten-evasion with an EXECUTION PANIC on a comma-multi-leg shape, and
	// its case-preserving Ids mismatched execution's uppercase row keys). The
	// stable state is the plain evict: an inner shadow ON-only CTE joins the
	// booked ON-only READ class. Boundary:
	//   - BARE read over the coinciding shadow answers via leniency +
	//     fabrication (Q52, install-independent);
	//   - QUALIFIED read (V."X") is SILENT NULL — the fabrication provides
	//     bare column keys on the merged row, not the "V.X" qualified key;
	//     this is the one silent residual, a FLIP-SENTINEL for the booked
	//     cteOnScopes-aware read resolution (the sound fix needs BOTH that
	//     and a fix to execution's quoted-alias uppercasing);
	//   - a comma-multi-leg / 2+-extra-leg shadow read is LOUD (the evict
	//     keeps it out of cteScopes; an install PANICKED here — regression pin);
	//   - a DUPLICATE-name body with a UNIQUE column read still PLANS (the
	//     unique column resolves; a blanket dup-decline wrongly rejected it).
	t.Run("Q54_shadow_read_reach_boundary", func(t *testing.T) {
		// RFC-173 cap: the BOOKED FLIP has landed. The QUALIFIED read `V.X` over the
		// inner V=[X] now RESOLVES (a single-namespace derived source, unique leaf
		// `X` — GetByName strips the self-qualifier), so `MAX(V.X)` returns the real
		// value `1|1` — IDENTICAL to the non-qualified sibling Q52 over the same data
		// (X'=CC.CID over the LEFT JOIN → {1, NULL} → MAX = 1). The name model's
		// `<nil>` here was the "silent flip-sentinel" the header booked as wrong.
		check(t, `WITH "V" AS (SELECT "BID" AS "X" FROM LB) SELECT (WITH "V" AS (SELECT CC."CID" AS "X" FROM "V" LEFT JOIN CC ON "V"."X" = CC."CID") SELECT MAX("V"."X") FROM "V") FROM LB LIMIT 1`,
			"1|1")
		_, ePanic := run(t, `WITH "V" AS (SELECT "BID" AS "B" FROM LB) SELECT (WITH "V" AS (SELECT LB."K" AS "B" FROM "V" LEFT JOIN CC ON "V"."B" = CC."CID" LEFT JOIN LA ON "V"."B" = LA."AID") SELECT COUNT(*) FROM "V", CC WHERE "V"."B" = 100) FROM LB LIMIT 1`)
		if ePanic == nil {
			t.Fatal("comma-multi-leg shadow read must be LOUD (an install panicked here), got rows")
		}
		// duplicate X in the body: complete-or-decline declines the WHOLE source
		// (0AF00), even for the unique Y — the partial "keep Y, drop X" install
		// this once pinned as answering `1|900` is unsound (review-caught: a
		// dropped dup name rebinds to another enclosing source).
		// The unique-Y reach is the booked cost; the poison-marker slice restores it.
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
		// A silent-wrong flip-sentinel: the regression each guards is a future
		// schema change that RESOLVES the obstructed reference and returns joined
		// rows. All obstructions decline the whole source → a uniform 0AF00 (the
		// caller's ON-drop guard), so pin the CODE, not just err != nil.
		mustLoud := func(t *testing.T, sql string) {
			t.Helper()
			rows, err := run(t, sql)
			if err == nil {
				t.Fatalf("expected a LOUD 0AF00 decline (silent-wrong sentinel), got rows=%v", rows)
			}
			if !strings.Contains(err.Error(), "0AF00") {
				t.Fatalf("expected 0AF00, got: %v", err)
			}
		}
		// (a) quoted-lowercase alias `AS "x"` — every reference (wrong-case "X"
		//     or correct-case "x") declines the source, never a silent join.
		mustLoud(t, `WITH "C" AS (SELECT LA."AID" AS "x" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") SELECT "C2"."CV" FROM "C" JOIN CC AS "C2" ON "C"."X" = "C2"."CID"`)
		mustLoud(t, `WITH "C" AS (SELECT LA."AID" AS "x" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") SELECT "C2"."CV" FROM "C" JOIN CC AS "C2" ON "C"."x" = "C2"."CID"`)
		// (b) DUPLICATE output name X — a `C."X"` ref declines, never a silent
		//     pick of the first of the two X columns.
		mustLoud(t, `WITH "C" AS (SELECT LA."AID" AS "X", LB."BID" AS "X", LA."K" AS "Y" FROM LA JOIN LB ON LA."AID" = LB."BID") SELECT "C2"."CV" FROM "C" JOIN CC AS "C2" ON "C"."X" = "C2"."CID"`)
		// (c) the UNSOUND partial-install hole (review-caught): a dup name is
		//     DROPPED from a partial schema and REBINDS to another enclosing
		//     source. C has dup AID; the enclosing scope also has L.AID; a BARE
		//     `AID` in the ON is AMBIGUOUS (C's dup vs L's) and MUST NOT silently
		//     bind to L.AID and return cross-product rows. Complete-or-decline
		//     closes it: the dup-bodied C declines wholesale → 0AF00.
		mustLoud(t, `WITH "C" AS (SELECT LA."AID" AS "AID", LB."BID" AS "AID", LA."K" AS "Y" FROM LA JOIN LB ON LA."AID" = LB."BID") SELECT "L"."AID" FROM "C" JOIN LA AS "L" ON "AID" = "L"."AID"`)
		// (d) even a UNIQUE reference (Y) into a body that has a dup ELSEWHERE
		//     declines — the whole source is untrustworthy once any column is
		//     obstructed (the reach cost of complete-or-decline; the full-reach
		//     poison-marker fix that keeps unique columns is a booked slice).
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
		// wrong-case ref over a quoted-lowercase aggregate alias → decline.
		mustLoud(t, `WITH "C" AS (SELECT MIN(LA."AID") AS "x" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID") SELECT "C2"."CV" FROM "C" JOIN CC AS "C2" ON "C"."X" = "C2"."CID"`)
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
