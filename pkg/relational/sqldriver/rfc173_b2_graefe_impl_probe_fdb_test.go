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
		if err == nil {
			t.Fatal("duplicate CTE name in a subquery-nested WITH must fail LOUD, got rows")
		}
	})
	t.Run("Q14_union_branch_on_resolves", func(t *testing.T) {
		check(t, `WITH "C" AS (`+cteBody+`) SELECT "C"."AK", "C2"."CID" FROM "C" JOIN CC AS "C2" ON "C"."BK" = "C2"."CID" UNION ALL SELECT LA."K", 0 FROM LA WHERE LA."K" = 110`,
			"110|0")
	})
}
