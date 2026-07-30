package sqldriver_test

// The 3-way COMMA join with a PROJECTED EXISTS and equijoin predicates — the
// shape that returned NO ROWS AT ALL, in two independent ways at once.
//
// Both were pre-existing, verified by running these arms at this branch's base
// commit, and both had to be fixed for a single row to come out:
//
//  1. The lowest join — the positional-merge lower of `FROM A, B` — flowed a row
//     whose MERGE SLOTS WERE UNTYPED, because PartitionSelectRule's
//     single-live-lower arm hands its select an untyped flowed row and the merge
//     round then scavenged the leg types off that value and found none. Untyped,
//     the equijoin operand pushed into B's scan could not bake to a pinned
//     ordinal; a source-relative operand evaluates to NULL against the
//     build-bound row, so the scan matched nothing. Zero rows, NO error. Fixed
//     by taking the leg type from the QUANTIFIER, which is what Java does
//     (Quantifier.java:801-803 — getFlowedObjectValue() is always typed).
//  2. The MIDDLE join's result value is a bare baked `ofOrdinal(QOV(merge), 0)`
//     — legitimate, and exactly what PartitionSelectRule.java:281+319 mints —
//     but executor.newOrdinalJoinBuild refused to build an ordinal join whose
//     result value was not a RecordConstructorValue, so the query failed
//     outright with an internal "planner bug" error. Fixed by the build's Bare
//     arm; declining the build instead was measured at zero rows.
//
// The row expectations are not decoration: the data makes a mis-bound leg window
// visible. EE holds CK 100 and 300, so the projected EXISTS must be TRUE, FALSE,
// TRUE across the three surviving triples — a window one slot off answers
// UNIFORMLY, which is the failure a row-count check would miss. Rows are rendered
// POSITIONALLY, so the assertion pins the slot ORDER too: a name-keyed rendering
// would pass with two columns swapped.

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
)

func TestFDB_CommaJoin3ProjectedExistsWithEquijoins(t *testing.T) {
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
	md := existsGatherSchemaMetadata(t)

	mkA := func(aid, k int64) proto.Message {
		d := md.GetRecordType("A").Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName("AID"), protoreflect.ValueOfInt64(aid))
		m.Set(d.Fields().ByName("K"), protoreflect.ValueOfInt64(k))
		return m
	}
	mk1 := func(table, f string, v int64) proto.Message {
		d := md.GetRecordType(table).Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName(protoreflect.Name(f)), protoreflect.ValueOfInt64(v))
		return m
	}
	mkB := func(bid, k int64) proto.Message {
		d := md.GetRecordType("B").Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName("BID"), protoreflect.ValueOfInt64(bid))
		m.Set(d.Fields().ByName("K"), protoreflect.ValueOfInt64(k))
		return m
	}

	// A.AID 1..3 join B.BID join EEV.VK on equality. EE holds CK 100 and 300, so
	// the projected EXISTS(EE.CK = A.K) is TRUE for A.K 100 and 300 and FALSE for
	// 200 — a per-row answer, so a mis-bound leg window shows up as a uniform
	// column rather than merely a different row count.
	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Create()
		if sErr != nil {
			return nil, sErr
		}
		recs := []proto.Message{
			mkA(1, 100), mkA(2, 200), mkA(3, 300),
			mkB(1, 11), mkB(2, 22), mkB(3, 33),
			mk1("EEV", "VK", 1), mk1("EEV", "VK", 2), mk1("EEV", "VK", 3),
			mk1("EE", "CK", 100), mk1("EE", "CK", 300),
		}
		for _, r := range recs {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	runQ := func(t *testing.T, sql string) ([]string, string, error) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		if perr != nil {
			return nil, "", perr
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
				out = append(out, positionalSprint(r))
			}
			return nil, nil
		})
		sort.Strings(out)
		return out, explain, eerr
	}

	// A.AID = B.BID AND B.BID = EEV.VK pins the 3-way join to the diagonal, so
	// there are exactly three surviving triples and the EXISTS column varies
	// across them.
	t.Run("two_column_projection", func(t *testing.T) {
		rows, plan, err := runQ(t, `SELECT A."K", EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") `+
			`FROM A, B, EEV WHERE A."AID" = B."BID" AND B."BID" = EEV."VK"`)
		assertJoin3Rows(t, rows, plan, err,
			[]string{"[100 true]", "[200 false]", "[300 true]"})
	})

	// The SINGLE-column form is the one for which a bare (non-RC) result value is
	// representable at the TOP as well, so it exercises the build's Bare arm at two
	// levels. Its EXISTS still varies per row.
	t.Run("single_column_projection", func(t *testing.T) {
		rows, plan, err := runQ(t, `SELECT EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") `+
			`FROM A, B, EEV WHERE A."AID" = B."BID" AND B."BID" = EEV."VK"`)
		assertJoin3Rows(t, rows, plan, err,
			[]string{"[false]", "[true]", "[true]"})
	})
}

// positionalSprint renders a result row from its POSITIONAL slots, so the
// assertion pins the slot ORDER as well as the values. A name-keyed rendering
// (unnestSprint over the row map) passes with two columns swapped and hides
// exactly the mis-bound-window failure these arms exist to catch.
func positionalSprint(r executor.QueryResult) string {
	if r.Positional == nil {
		return unnestSprint(executor.RowValue(r))
	}
	return fmt.Sprint(r.Positional.Slots)
}

// assertJoin3Rows requires the shape to EXECUTE and to return exactly want. Any
// error is a failure now: the two defects that made this shape unrunnable are
// fixed, and the specific error each produced is quoted so a regression is
// recognizable at a glance rather than merely red.
func assertJoin3Rows(t *testing.T, rows []string, plan string, err error, want []string) {
	t.Helper()
	if err != nil {
		t.Fatalf("must EXECUTE, failed with: %v\n"+
			"Two regressions produce a failure here. %q means the ordinal join build "+
			"stopped accepting a bare (non-RC) baked result value — the shape "+
			"PartitionSelectRule legitimately mints. Anything else, start from the plan.\n  %s",
			err, "ordinal join build: result value contains baked ordinal references", plan)
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %v, want %v.\nZERO rows with no error is the untyped-merge-slot "+
			"regression: an equijoin operand that cannot bake to a pinned ordinal evaluates to "+
			"NULL against the build-bound row, so the pushed-down scan matches nothing.\n  %s",
			rows, want, plan)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("rows = %v, want %v — a leg window one slot off answers the EXISTS "+
				"uniformly instead of per row.\n  %s", rows, want, plan)
		}
	}
}
