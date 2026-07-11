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
//      non-positional row context". The enclosed leg's rows carry the
//      PRE-EXISTING enclosed-CTE-box-unnest silent-NULL residual (booked,
//      parent-identical) — flip-sentinels, see P1a/Q1.
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
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
			// Pre-evaluate scalar subqueries and bind results (as the sql
			// driver's fetchPage does) — execution without the bindings fails
			// loudly with values.UnboundScalarSubqueryError.
			evalCtx := executor.EmptyEvaluationContext()
			if len(subs) > 0 {
				scalarResults := make(map[values.CorrelationIdentifier]any, len(subs))
				for _, ssq := range subs {
					result, ssqErr := executor.EvaluateScalarSubquery(ctx, ssq.Plan, store, evalCtx, recordlayer.DefaultExecuteProperties())
					if ssqErr != nil {
						return nil, ssqErr
					}
					scalarResults[ssq.Alias] = result
				}
				evalCtx = evalCtx.WithScalarSubqueries(scalarResults)
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
	// error here). FLIP-SENTINEL: the enclosed leg's NULL rows are the
	// PRE-EXISTING enclosed-CTE-box-unnest silent-NULL bug (booked; fails
	// identically on the pre-B2 parent — strictly correct is
	// (100,5,7),(100,5,8) for BOTH legs); flips when that bug is fixed.
	t.Run("P1a_cte_double_ref_unenclosed_then_enclosed", func(t *testing.T) {
		want(t, `WITH "C" AS (`+cteBody+`) SELECT "AK", "BK", "XV" FROM "C" UNION ALL SELECT "C"."AK", "C"."BK", "C"."XV" FROM "C", CC`,
			"100|5|7", "100|5|8", "<nil>|<nil>|<nil>", "<nil>|<nil>|<nil>")
	})
	// P1b: the reverse order — same consume-once guarantee, same pre-existing
	// enclosed-leg residual (flip-sentinel as P1a).
	t.Run("P1b_cte_double_ref_enclosed_then_unenclosed", func(t *testing.T) {
		want(t, `WITH "C" AS (`+cteBody+`) SELECT "C"."AK", "C"."BK", "C"."XV" FROM "C", CC UNION ALL SELECT "AK", "BK", "XV" FROM "C"`,
			"100|5|7", "100|5|8", "<nil>|<nil>|<nil>", "<nil>|<nil>|<nil>")
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
			// Pre-evaluate scalar subqueries and bind results (as the sql
			// driver's fetchPage does) — execution without the bindings fails
			// loudly with values.UnboundScalarSubqueryError.
			evalCtx := executor.EmptyEvaluationContext()
			if len(subs) > 0 {
				scalarResults := make(map[values.CorrelationIdentifier]any, len(subs))
				for _, ssq := range subs {
					result, ssqErr := executor.EvaluateScalarSubquery(ctx, ssq.Plan, store, evalCtx, recordlayer.DefaultExecuteProperties())
					if ssqErr != nil {
						return nil, ssqErr
					}
					scalarResults[ssq.Alias] = result
				}
				evalCtx = evalCtx.WithScalarSubqueries(scalarResults)
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
	check := func(t *testing.T, sql string, expect ...string) {
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

	const cteBody = `SELECT LA."K" AS "AK", LB."K" AS "BK", "X" AS "XV" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID", LA."ARR" AS "X" WHERE LA."K" = 100`
	const cteBodyNoWhere = `SELECT LA."K" AS "AK", LB."K" AS "BK", "X" AS "XV" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID", LA."ARR" AS "X"`

	// Q1: single ENCLOSED reference of the FILTERED body (no double ref — no
	// record can exist). FLIP-SENTINEL: the all-NULL rows are the PRE-EXISTING
	// enclosed-CTE-box-unnest silent-NULL bug (booked; parent-identical, occurs
	// with or without the WHERE — see Q2 — so NOT a B2 class). Strictly correct:
	// (100,5,7),(100,5,8). Flips when the enclosed-CTE bug is fixed.
	t.Run("Q1_single_enclosed_ref_filtered", func(t *testing.T) {
		check(t, `WITH "C" AS (`+cteBody+`) SELECT "C"."AK", "C"."BK", "C"."XV" FROM "C", CC`,
			"<nil>|<nil>|<nil>", "<nil>|<nil>|<nil>")
	})
	// Q2: single ENCLOSED reference of the UNFILTERED body — proves the trigger
	// is the ENCLOSED CTE box unnest itself, not the filter. Same flip-sentinel
	// (strictly correct: the three real rows).
	t.Run("Q2_single_enclosed_ref_unfiltered", func(t *testing.T) {
		check(t, `WITH "C" AS (`+cteBodyNoWhere+`) SELECT "C"."AK", "C"."BK", "C"."XV" FROM "C", CC`,
			"<nil>|<nil>|<nil>", "<nil>|<nil>|<nil>", "<nil>|<nil>|<nil>")
	})
	// Q3: single UN-ENCLOSED reference (control — the gathered path, CORRECT).
	t.Run("Q3_single_unenclosed_ref_filtered", func(t *testing.T) {
		check(t, `WITH "C" AS (`+cteBody+`) SELECT "AK", "BK", "XV" FROM "C"`,
			"100|5|7", "100|5|8")
	})
	// Q4: UNFILTERED body double-ref — the un-enclosed leg is correct; the
	// enclosed leg carries the same pre-existing residual (flip-sentinel as Q1).
	t.Run("Q4_unfiltered_double_ref", func(t *testing.T) {
		check(t, `WITH "C" AS (`+cteBodyNoWhere+`) SELECT "AK", "BK", "XV" FROM "C" UNION ALL SELECT "C"."AK", "C"."BK", "C"."XV" FROM "C", CC`,
			"100|5|7", "100|5|8", "110|<nil>|9", "<nil>|<nil>|<nil>", "<nil>|<nil>|<nil>", "<nil>|<nil>|<nil>")
	})
}
