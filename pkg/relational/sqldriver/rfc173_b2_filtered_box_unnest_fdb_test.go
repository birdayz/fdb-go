package sqldriver_test

// RFC-173 Outcome-B B2 sub-slice A: the FILTERED box unnest ordinalizes. A WHERE
// conjunct referencing a box leg (`FROM (LA LEFT JOIN LB ON …), LA.ARR AS X
// WHERE LA.K = 100`) used to decline the gathered ordinal path unconditionally,
// falling to the name-model binary lowering (P4-enclosed + P5-un-enclosed
// producers). Now a BAKEABLE conjunct (pre-verified metadata-only) rides the
// gather: the WHERE-merge arm bakes it over the seed's RECORDED legTypes —
// ofOrdinal(QOV($BOX), leafOffset+idx) against the box's OUTPUT row, which is
// WHERE-above-LEFT semantics for BOTH legs (preserved-leg conjuncts filter real
// values; null-supplied-leg conjuncts see NULL on padded rows). Subquery-carrying
// conjuncts stay UNBAKEABLE (name-model, correct rows — the booked Slice-2b
// posture); the EXISTS variant is sub-slice B (deferred, its own consult).
//
// Every pin is an EXACT-ROW assert over the dup-K discriminator fixture (both
// legs carry K with disjoint ranges; unmatched rows on each side) — a wrong-slot
// or first-match regression shows as crossed values, never green-by-luck. The
// step-0 probe battery established these exact rows are the NAME-MODEL answers
// too (pure coverage change). The ORDINAL-fired proof (0 producers post-change)
// is the white-box census pin in core/query (the census observer is a
// process-global unsafe in this parallel suite).

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

func TestFDB_RFC173B2_FilteredBoxUnnest(t *testing.T) {
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

	b := metadata.NewSchemaTemplateBuilder().SetName("b2fbu")
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
		// LA: aid=1 K=100 arr[7,8] (joins LB bid=1); aid=2 K=110 arr[9] (unmatched → LB null-supplied).
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

	run := func(t *testing.T, sql string) []string {
		t.Helper()
		plan, subs, perr := embedded.PlanRecordQueryWithSubqueries(sql, md, nil)
		if perr != nil {
			t.Fatalf("plan %q: %v", sql, perr)
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
			t.Fatalf("exec %q: %v", sql, eerr)
		}
		sort.Strings(out)
		return out
	}
	want := func(t *testing.T, sql string, expect ...string) {
		t.Helper()
		got := run(t, sql)
		sort.Strings(expect)
		if strings.Join(got, ",") != strings.Join(expect, ",") {
			t.Fatalf("rows = %v, want %v\n  %s", got, expect, sql)
		}
	}

	const leftBox = `FROM LA LEFT JOIN LB ON LA."AID" = LB."BID", LA."ARR" AS "X"`
	const fullBox = `FROM LA FULL OUTER JOIN LB ON LA."AID" = LB."BID", LA."ARR" AS "X"`

	// Preserved-leg conjunct: only the matched LA row (K=100) passes.
	t.Run("preserved_leg_conjunct", func(t *testing.T) {
		want(t, `SELECT LA."K", LB."K", "X" `+leftBox+` WHERE LA."K" = 100`, "100|5|7", "100|5|8")
	})
	// Null-supplied-leg conjunct (value): padded rows (LB.K NULL) drop.
	t.Run("null_supplied_leg_value", func(t *testing.T) {
		want(t, `SELECT LA."K", LB."K", "X" `+leftBox+` WHERE LB."K" = 5`, "100|5|7", "100|5|8")
	})
	// Null-supplied-leg IS NULL: ONLY the padded row survives.
	t.Run("null_supplied_leg_is_null", func(t *testing.T) {
		want(t, `SELECT LA."K", LB."K", "X" `+leftBox+` WHERE LB."K" IS NULL`, "110|<nil>|9")
	})
	// FULL box, both leg positions.
	t.Run("full_box_preserved", func(t *testing.T) {
		want(t, `SELECT LA."K", LB."K", "X" `+fullBox+` WHERE LA."K" = 100`, "100|5|7", "100|5|8")
	})
	t.Run("full_box_other_leg", func(t *testing.T) {
		want(t, `SELECT LA."K", LB."K", "X" `+fullBox+` WHERE LB."K" = 5`, "100|5|7", "100|5|8")
	})
	// RIGHT box (LA null-supplied on unmatched LB rows; its NULL array explodes
	// to zero rows) — the conjunct picks the matched pair.
	t.Run("right_box_conjunct", func(t *testing.T) {
		want(t, `SELECT LA."K", LB."K", "X" FROM LA RIGHT JOIN LB ON LA."AID" = LB."BID", LA."ARR" AS "X" WHERE LB."K" = 5`, "100|5|7", "100|5|8")
	})
	// OR spanning both legs (CNF straddles the box) + NOT (De Morgan row).
	t.Run("or_spanning_legs", func(t *testing.T) {
		want(t, `SELECT LA."K", LB."K", "X" `+leftBox+` WHERE LA."K" = 110 OR LB."K" = 5`, "100|5|7", "100|5|8", "110|<nil>|9")
	})
	t.Run("not_conjunct", func(t *testing.T) {
		want(t, `SELECT LA."K", LB."K", "X" `+leftBox+` WHERE NOT (LA."K" = 100)`, "110|<nil>|9")
	})
	// Mixed element + leg conjunct; AT-ordinal variant.
	t.Run("mixed_element_and_leg", func(t *testing.T) {
		want(t, `SELECT LA."K", "X" `+leftBox+` WHERE "X" > 7 AND LA."K" = 100`, "100|8")
	})
	t.Run("at_ordinal_conjunct", func(t *testing.T) {
		want(t, `SELECT LA."K", "X", "O" FROM LA LEFT JOIN LB ON LA."AID" = LB."BID", LA."ARR" AS "X" AT "O" WHERE LA."K" = 100`, "100|1|7", "100|2|8")
	})
	// Scalar-subquery conjunct: UNBAKEABLE → name-model (the verdict ROUTING is
	// pinned white-box by TestRFC173B2_FilteredBoxUnnestCensus's
	// scalar_subquery_operand classifier arm; rows here pin CORRECTNESS). Two
	// discriminating arms: comparison-FALSE ([] — MAX(CV)=900 ≠ any K) and
	// comparison-TRUE (both LA rows: 100<900, 110<900 → all three elements) —
	// the TRUE arm proves the subquery actually EVALUATES; before the harness
	// pre-bound subquery results, an absent binding silently compared K to NULL
	// and BOTH arms returned [] (indistinguishable from the FALSE arm — the
	// silent-NULL hole values.UnboundScalarSubqueryError now closes).
	t.Run("scalar_subquery_unbakeable_false_arm", func(t *testing.T) {
		want(t, `SELECT "X" `+leftBox+` WHERE LA."K" = (SELECT MAX(CV) FROM CC)`)
	})
	t.Run("scalar_subquery_unbakeable_true_arm", func(t *testing.T) {
		want(t, `SELECT "X" `+leftBox+` WHERE LA."K" < (SELECT MAX(CV) FROM CC)`, "7", "8", "9")
	})
	// GROUP BY over the filtered gather.
	t.Run("group_by_over_filtered", func(t *testing.T) {
		want(t, `SELECT LA."K", COUNT(*) `+leftBox+` WHERE LA."K" = 100 GROUP BY LA."K"`, "2|100")
	})
	// NESTED box (`(LA FULL LB) FULL CC`) + box-leg conjunct: the wedge gate
	// (the classify/gather one-authority) ADMITS the nested box, so the
	// conjunct classifies Bakeable and bakes over the nested buried windows —
	// this pin proves the admitted shape answers CORRECT rows (only the
	// fully-matched LA row passes LA.K=100; the LB- and CC-unmatched
	// null-padded rows drop). White-box routing: the census nested arm.
	t.Run("nested_box_conjunct", func(t *testing.T) {
		want(t, `SELECT LA."K", LB."K", CC."CV", "X" FROM LA FULL OUTER JOIN LB ON LA."AID" = LB."BID" FULL OUTER JOIN CC ON LA."AID" = CC."CID", LA."ARR" AS "X" WHERE LA."K" = 100`,
			"900|100|5|7", "900|100|5|8")
	})
}
