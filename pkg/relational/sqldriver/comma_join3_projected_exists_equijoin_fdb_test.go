package sqldriver_test

// The 3-way COMMA join with a PROJECTED EXISTS and equijoin predicates — the
// shape whose plan carries a join result value that is a bare baked FieldValue
// rather than a RecordConstructorValue.
//
// TestJoinResultValueWithBakedOrdinalsIsAlwaysAnRC found that structurally, at
// planning time. This runs it, because a structural finding about a plan is only
// a bug if executing the plan is a bug: executor.newOrdinalJoinBuild refuses to
// build an ordinal join whose result value carries baked ordinals and is not an
// RC, so if the cursor is reached, the query fails with the internal
// "planner bug" error rather than returning rows.
//
// That internal error was sighted once during a full-suite run and never
// reproduced in isolation. These two queries are the isolation: both fail,
// deterministically, and they fail identically at this branch's base commit — the
// defect is PRE-EXISTING, not introduced by the leg-identity retyping.
//
// The fix is not here. Which result value that rule arm hands its FlatMap decides
// which row the parent reads, so it is a Cascades planner change and belongs with
// the planner gate, not inside a leg-identity retyping.
//
// So what these arms pin is the CURRENT DISPOSITION, exactly: the query must fail
// with THAT error. That is a two-way tripwire, and deliberately so.
//
//   - If the error text or class changes, this goes red — a silent demotion to a
//     non-build cursor would be far worse than the error, because the cursor would
//     read leg slots off a row with no leg windows and return WRONG ROWS instead of
//     failing.
//   - If the query starts WORKING, this goes red too, with the row expectations
//     it must then satisfy stated right here. A fix cannot land unnoticed, and
//     whoever lands it gets the assertion for free.
//
// The row expectations are not decoration: the data makes a mis-bound leg window
// visible. EE holds CK 100 and 300, so the projected EXISTS must be TRUE, FALSE,
// TRUE across the three surviving triples — a window one slot off answers
// UNIFORMLY, which is the failure a row-count check would miss.

import (
	"context"
	"sort"
	"strings"
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
				out = append(out, unnestSprint(executor.RowValue(r)))
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
		assertKnownNonRCJoinFailureOrRows(t, rows, plan, err,
			[]string{"[100 true]", "[200 false]", "[300 true]"})
	})

	// The SINGLE-column form is the one for which a bare (non-RC) result value is
	// even representable, so it is the arm that can reach newOrdinalJoinBuild's
	// non-RC error. Its EXISTS still varies per row.
	t.Run("single_column_projection", func(t *testing.T) {
		rows, plan, err := runQ(t, `SELECT EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") `+
			`FROM A, B, EEV WHERE A."AID" = B."BID" AND B."BID" = EEV."VK"`)
		assertKnownNonRCJoinFailureOrRows(t, rows, plan, err,
			[]string{"[false]", "[true]", "[true]"})
	})
}

// assertKnownNonRCJoinFailureOrRows is the two-way tripwire for a shape on
// knownNonRCJoinResultValueDebt: it must fail with the ordinal-join-build error,
// and if it ever stops failing it must return `want`.
func assertKnownNonRCJoinFailureOrRows(t *testing.T, rows []string, plan string, err error, want []string) {
	t.Helper()
	const knownErr = "ordinal join build: result value contains baked ordinal references"
	if err != nil {
		if !strings.Contains(err.Error(), knownErr) {
			t.Fatalf("failed with an UNEXPECTED error: %v\n"+
				"The known disposition for this shape is a loud refusal from "+
				"executor.newOrdinalJoinBuild (%q). A different failure means something else "+
				"changed; a SILENT success with wrong rows would be worse still, because the "+
				"cursor would read leg slots off a row that has no leg windows.\n  %s",
				err, knownErr, plan)
		}
		return
	}
	// It works now. That is the outcome we want — assert the rows and delete the
	// debt entry.
	if len(rows) != len(want) {
		t.Fatalf("the shape now EXECUTES (good) but rows = %v, want %v.\n"+
			"Delete this shape from knownNonRCJoinResultValueDebt and turn this arm into a "+
			"plain row assertion.\n  %s", rows, want, plan)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("the shape now EXECUTES (good) but rows = %v, want %v — a leg window one "+
				"slot off answers the EXISTS uniformly instead of per row.\n  %s",
				rows, want, plan)
		}
	}
	t.Errorf("the shape now EXECUTES and returns the right rows — the defect is FIXED.\n" +
		"Delete its entry from knownNonRCJoinResultValueDebt and replace this call with a " +
		"plain row assertion, so the fix is recorded as a fix rather than as debt.")
}
