package sqldriver_test

// RFC-173 S4 regression pin for the ENCLOSED-MIDDLE lateral-unnest under a
// correlated EXISTS with a TRAILING join leg:
//
//	SELECT "X" FROM LA, LA."ARR" AS "X", LB WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = LA."K")
//
// The unnest sits in the MIDDLE of the FROM list, so it lowers as a BURIED leg
// (`join.Right` is the trailing plain leg LB, not the unnest). The root-form
// EXISTS dispatch missed it and it fell to translateJoinWithExists, which
// translated the buried unnest ENCLOSED → the name-model binary seed: the outer
// row (LA) was bound as its name-keyed Datum map and the correlated array source
// `LA.ARR` (and the downstream projection) resolved by NAME. Only the
// unnest-LAST ordering (`LA, LB, LA."ARR" AS "X"`) reached translateUnnestExistsFilter
// and gathered ordinally. Now the dispatch ROTATES the enclosed-middle cluster to
// the root form (rotateEnclosedUnnest — the SAME rotation the non-EXISTS path
// gathers through) and routes it to translateUnnestExistsFilter, so the (LA,LB)
// INNER outer gathers via E-1a and the seed ordinalizes.
//
// The proof is the values.NameReadForbidden measurement gate: ARMED, every
// name-keyed Datum read is a hard error. This test arms the gate around the whole
// family (outer-correlated EXISTS + NOT EXISTS, element-correlated EXISTS,
// multi-column projection incl. the outer array column, element-WHERE + EXISTS,
// AT-ordinality) and asserts the exact rows — the dimension the dual-window
// differential cannot see (both models agree when BOTH silently read by name).
// The fixture discriminates: TWO LB rows (the trailing join multiplies every
// surviving pair), THREE LA rows two of which match the EXISTS correlation, so a
// wrong-slot / first-match / dropped-leg regression shows as crossed or missing
// counts, never green-by-luck. Row results were verified identical to the
// pre-fix name-model answers (representation change only).
//
// The gate is a process-global runtime flag; this test does NOT call t.Parallel()
// (Go runs it in the serial phase) and resets the flag in defer. The schema name
// is unique to this test so no cached plan is shared.

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

func TestFDB_RFC173_EnclosedUnnestExistsOrdinal(t *testing.T) {
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

	b := metadata.NewSchemaTemplateBuilder().SetName("encunnestexists")
	b.AddTable("LA", []metadata.ColumnSpec{
		metadata.NewColumnSpec("AID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), true), 3),
	}, []string{"AID"})
	b.AddTable("LB", []metadata.ColumnSpec{
		metadata.NewColumnSpec("BID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
	}, []string{"BID"})
	b.AddTable("EE", []metadata.ColumnSpec{
		metadata.NewColumnSpec("EID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("CK", api.NewLongType(true), 2),
	}, []string{"EID"})
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
		// LA: aid1 K=100 arr[7,8]; aid2 K=110 arr[9]; aid3 K=100 arr[5].
		// LB: two rows (bid1 K=5, bid2 K=6) — the trailing join multiplies x2.
		// EE: CK=100 (matches LA.K=100) and CK=7 (matches element 7).
		for _, r := range []proto.Message{
			mkLA(1, 100, 7, 8), mkLA(2, 110, 9), mkLA(3, 100, 5),
			mk2("LB", "BID", "K", 1, 5), mk2("LB", "BID", "K", 2, 6),
			mk2("EE", "EID", "CK", 1, 100), mk2("EE", "EID", "CK", 2, 7),
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

	// Arm the measurement gate: any name-keyed Datum read on this path hard-errors.
	values.NameReadForbidden = true
	defer func() { values.NameReadForbidden = false }()

	// Outer-correlated EXISTS: EE.CK = LA.K matches K=100 (aid1 arr[7,8], aid3 arr[5]);
	// aid2 (K=110) drops. Two LB rows double every surviving element.
	t.Run("outer_corr_exists", func(t *testing.T) {
		want(t, `SELECT "X" FROM LA, LA."ARR" AS "X", LB WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = LA."K")`,
			"5", "5", "7", "7", "8", "8")
	})
	// NOT EXISTS: the complement — only aid2 (K=110, arr[9]).
	t.Run("outer_corr_not_exists", func(t *testing.T) {
		want(t, `SELECT "X" FROM LA, LA."ARR" AS "X", LB WHERE NOT EXISTS (SELECT 1 FROM EE WHERE EE."CK" = LA."K")`,
			"9", "9")
	})
	// Element-correlated EXISTS: EE.CK = "X" matches element 7 only (from aid1).
	t.Run("elem_corr_exists", func(t *testing.T) {
		want(t, `SELECT "X" FROM LA, LA."ARR" AS "X", LB WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = "X")`,
			"7", "7")
	})
	// Multi-column projection incl. the OUTER array column LA."ARR": keys sort
	// ARR|K|X. aid1 (arr[7,8]) contributes 4 rows, aid3 (arr[5]) 2 rows.
	t.Run("proj_outer_array_and_key", func(t *testing.T) {
		want(t, `SELECT LA."K", LA."ARR", "X" FROM LA, LA."ARR" AS "X", LB WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = LA."K")`,
			"[5]|100|5", "[5]|100|5", "[7 8]|100|7", "[7 8]|100|7", "[7 8]|100|8", "[7 8]|100|8")
	})
	// Element WHERE conjunct folded with the EXISTS: X>6 keeps 7,8 (drops 5).
	t.Run("element_where_plus_exists", func(t *testing.T) {
		want(t, `SELECT "X" FROM LA, LA."ARR" AS "X", LB WHERE "X" > 6 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = LA."K")`,
			"7", "7", "8", "8")
	})
	// AT ordinality on the buried unnest: element + 1-based position, keys sort O|X.
	t.Run("at_ordinality", func(t *testing.T) {
		want(t, `SELECT "X", "O" FROM LA, LA."ARR" AS "X" AT "O", LB WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = LA."K")`,
			"1|5", "1|5", "1|7", "1|7", "2|8", "2|8")
	})
	// Trailing-leg (LB) conjunct folded with the EXISTS: LB.K=5 keeps only bid1,
	// so every surviving element appears ONCE (not doubled). Exercises the
	// box-leg conjunct bake over the rotated (LA,LB) gather.
	t.Run("trailing_leg_conjunct_plus_exists", func(t *testing.T) {
		want(t, `SELECT "X" FROM LA, LA."ARR" AS "X", LB WHERE LB."K" = 5 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = LA."K")`,
			"5", "7", "8")
	})
}
