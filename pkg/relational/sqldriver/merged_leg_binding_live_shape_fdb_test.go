package sqldriver_test

import (
	"context"
	"fmt"
	"sort"
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

// TestFDB_MergedLegBinding_LiveBoxGatherShape pins what bindMergedOuterLegs
// actually does on the shape it actually runs on, and what that is worth.
//
// # Why this test and not the unit fixtures
//
// The unit pins (merged_outer_leg_binding_test.go) cover a DEGENERATE case the
// production corpus does not produce: they make the join's own alias equal one
// of the leg aliases, so the "skip the join's own alias" branch engages. On every
// live shape it does not. A clustered box gathers under a MINTED box alias
// (`B$BOX`, `C$BOX`) while its legs keep the user aliases (`A`, `B`), and a
// minted box alias is never a leg alias — so the skip is dead here and every leg
// binds. That is the branch profile production runs, and nothing exercised it
// end to end.
//
// # The verdict this test records: NOTHING READS THEM *HERE*
//
// The binder is LIVE — it executes on this shape, and across the whole sqldriver
// suite it binds windows over ten thousand times. It is not dormant, and the
// docs that called it consumer-free were wrong about that.
//
// Its bindings ARE read — but not on this shape. Measured on the full sqldriver
// corpus:
//
//   - Instrumenting the binding lookup: over a full run, exactly THREE lookups
//     resolve to a binder-produced window. All three are on the
//     `foldable_colliding_answers` shape in TestFDB_ExistsInnerShadow, a
//     two-leg `FROM ST, OT` merge — NOT, as an earlier version of this comment
//     claimed, on a single-leg unnest. None is on a box-gather shape.
//   - Replacing the whole body with `return ec` (bind nothing at all): the
//     entire suite stays green, including that reader's own test.
//   - Pointing every window at slot 0 of the merged row (bind everything WRONG):
//     the entire suite stays green.
//
// So the suite's greenness on this shape is NOT evidence the bindings are
// correct. It is evidence nothing consults them here, and that where something
// does consult them, it has a second route to the same answer
// (TestFDB_MergedLegBinding_ReaderShapeIsRedundant proves that directly). That
// distinction is the whole finding: a correct-looking mechanism whose
// correctness no test can currently observe, sitting on a per-outer-row path.
//
// This test therefore asserts BOTH halves, because either alone misleads:
//
//  1. the bindings this shape produces are the RIGHT ones (each leg alias bound
//     to its own window, resolving to its own columns) — so that if a consumer
//     ever arrives, it arrives onto correct bindings rather than onto bindings
//     nobody ever checked; and
//  2. the read count for this shape is ZERO — a negative result, pinned so that
//     the day it stops being zero, someone is told that the shadowing semantics
//     documented in DIVERGENCES.md have acquired a real consumer and need
//     re-justifying against it.
//
// A: (AID=1, K=100, ARR=[7,8]); B: (BID=1, K=200). A and B share the column K,
// which is what makes the per-leg windows discriminating: a read that resolves
// to the wrong leg's window returns 200 where it should return 100.
func TestFDB_MergedLegBinding_LiveBoxGatherShape(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	if !values.LegIdentityCensusEnabled() {
		t.Fatal("the leg-identity census gate is OFF, so the merged-leg binding " +
			"census records nothing and every assertion below is vacuous. The " +
			"sqldriver TestMain enables it for the whole run (runUnderLegIdentityCensus).")
	}

	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatal(err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	// Distinctive table names: the census is process-global and the suite runs in
	// parallel, so this test must be able to pick its OWN bindings out of it.
	b := metadata.NewSchemaTemplateBuilder().SetName("mlblive")
	b.AddTable("MLBA", []metadata.ColumnSpec{
		metadata.NewColumnSpec("AID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), true), 3),
	}, []string{"AID"})
	b.AddTable("MLBB", []metadata.ColumnSpec{
		metadata.NewColumnSpec("BID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
	}, []string{"BID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	md := tmpl.Underlying()

	aDesc := md.GetRecordType("MLBA").Descriptor
	a := dynamicpb.NewMessage(aDesc)
	a.Set(aDesc.Fields().ByName("AID"), protoreflect.ValueOfInt64(1))
	a.Set(aDesc.Fields().ByName("K"), protoreflect.ValueOfInt64(100))
	arrFd := aDesc.Fields().ByName("ARR")
	arr := a.NewField(arrFd).List()
	arr.Append(protoreflect.ValueOfInt32(7))
	arr.Append(protoreflect.ValueOfInt32(8))
	a.Set(arrFd, protoreflect.ValueOfList(arr))

	bDesc := md.GetRecordType("MLBB").Descriptor
	bm := dynamicpb.NewMessage(bDesc)
	bm.Set(bDesc.Fields().ByName("BID"), protoreflect.ValueOfInt64(1))
	bm.Set(bDesc.Fields().ByName("K"), protoreflect.ValueOfInt64(200))

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).
			SetMetaDataProvider(md).SetSubspace(ks).Create()
		if sErr != nil {
			return nil, sErr
		}
		for _, r := range []proto.Message{a, bm} {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	// The LIVE shape: a clustered FULL OUTER box gathered with a lateral unnest.
	// This is `within_box_buried_dup_gathers` in TestFDB_BareTwinGather, which is
	// where the binder was first observed firing in production.
	const q = `SELECT MLBA."K", MLBB."K", "X" ` +
		`FROM MLBA FULL OUTER JOIN MLBB ON MLBA."AID" = MLBB."BID", MLBA."ARR" AS "X"`

	plan, subs, perr := embedded.PlanRecordQueryWithSubqueries(q, md, nil)
	if perr != nil {
		t.Fatalf("plan %q: %v", q, perr)
	}

	var out []string
	if _, eerr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).
			SetMetaDataProvider(md).SetSubspace(ks).Open()
		if sErr != nil {
			return nil, sErr
		}
		evalCtx, bindErr := prebindScalarSubqueries(ctx, store, subs)
		if bindErr != nil {
			return nil, bindErr
		}
		cur, cErr := executor.ExecutePlan(ctx, plan, store, evalCtx, nil,
			recordlayer.DefaultExecuteProperties())
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
	}); eerr != nil {
		t.Fatalf("exec %q: %v", q, eerr)
	}
	sort.Strings(out)

	// (0) ROWS CORRECT. A.K and B.K are the SAME column name in two legs, so a
	// window that resolved to the wrong leg shows up here as 200-where-100.
	want := []string{"map[MLBA.K:100 MLBB.K:200 X:7]", "map[MLBA.K:100 MLBB.K:200 X:8]"}
	if fmt.Sprint(out) != fmt.Sprint(want) {
		t.Fatalf("rows = %v, want %v\n  sql: %s\n  plan: %s", out, want, q, plan.Explain())
	}

	binds, reads := executor.MergedLegBindingCensus()

	// (1) THE BINDER FIRED ON THIS SHAPE, and bound OUR legs. This is the claim
	// the docs had backwards — the binder was described as having no non-test
	// consumer, which read as "does not run".
	ourBinds := map[executor.MergedLegBinding]int{}
	for bind, n := range binds {
		if bind.LegAlias == "MLBA" || bind.LegAlias == "MLBB" {
			ourBinds[bind] = n
		}
	}
	if len(ourBinds) == 0 {
		t.Fatalf("bindMergedOuterLegs bound NO window for MLBA/MLBB on the box-gather "+
			"shape.\n  Either the shape stopped gathering as a merged concat, or the "+
			"binder stopped running on it. Either way the rest of this test — and the "+
			"DIVERGENCES.md entry describing this path — is describing something that "+
			"no longer happens.\n  census: %s", executor.FormatMergedLegBindingCensus())
	}

	// (2) EACH LEG BOUND ITS OWN WINDOW, and the windows do not overlap. MLBA
	// contributes 3 columns (AID, K, ARR) and MLBB 2 (BID, K), so a correct
	// decomposition gives disjoint, adjacent windows — and MLBB's K sits at a
	// slot inside MLBB's window, never inside MLBA's.
	byLeg := map[string]executor.MergedLegBinding{}
	for bind := range ourBinds {
		if prev, seen := byLeg[bind.LegAlias]; seen && prev != bind {
			t.Fatalf("leg %s bound TWO different windows in one corpus: %+v and %+v.\n"+
				"  A leg alias resolving to different slots depending on which call "+
				"answered is the wrong-row failure per-leg binding exists to remove.",
				bind.LegAlias, prev, bind)
		}
		byLeg[bind.LegAlias] = bind
	}
	for leg, wantWidth := range map[string]int{"MLBA": 3, "MLBB": 2} {
		bind, ok := byLeg[leg]
		if !ok {
			t.Fatalf("leg %s never bound a window; bound legs are %v", leg, byLeg)
		}
		if bind.Width != wantWidth {
			t.Fatalf("leg %s bound a window of width %d, want %d (%+v).\n"+
				"  The window must be the leg's OWN column count: too wide reads the "+
				"NEXT leg's columns, too narrow makes the leg's own tail unreadable.",
				leg, bind.Width, wantWidth, bind)
		}
	}
	if wa, wb := byLeg["MLBA"], byLeg["MLBB"]; wa.Offset+wa.Width > wb.Offset && wb.Offset >= wa.Offset {
		t.Fatalf("the two leg windows OVERLAP: MLBA [%d,%d) and MLBB [%d,%d).\n"+
			"  Overlapping windows mean one leg's ordinal reads the other's column, "+
			"and both legs here declare a column named K.",
			wa.Offset, wa.Offset+wa.Width, wb.Offset, wb.Offset+wb.Width)
	}

	// (3) THE JOIN'S-OWN-ALIAS SKIP NEVER ENGAGED. The box gathers under a
	// MINTED alias, which is not a leg alias, so both legs bound. The unit
	// fixtures only cover the opposite case.
	for bind := range ourBinds {
		if bind.OuterAlias == bind.LegAlias {
			t.Fatalf("leg %s was bound while EQUAL to the join's own alias (%+v) — "+
				"the skip should have kept the join's alias on the whole merged row",
				bind.LegAlias, bind)
		}
	}

	// (4) THE VERDICT. Nothing reads these bindings on a BOX-GATHER shape — and
	// the reason is CAUSAL: the only planner-side producer of such a read, the
	// leg-local bake, is deleted on this branch and returns with CQ-63. So the
	// criterion is COUPLED to that producer rather than a bare zero: a read with
	// no producer is a change of finding and fires; a read once the bake is back
	// is CQ-53 phase 2 working and must not. The corpus-wide form of the same
	// implication runs in the sqldriver TestMain, where both censuses are
	// complete, and carries the one exclusion this shape does not need.
	for _, leg := range []string{"MLBA", "MLBB"} {
		if n := reads[leg]; mergedLegReadIsAlarm(n) {
			t.Fatalf("leg %s's binder-produced window was READ %d time(s).\n\n"+
				"  This is a CHANGE OF FINDING, not necessarily a bug. The merged-leg\n"+
				"  binder was measured to have NO reader on any box-gather shape: over a\n"+
				"  full sqldriver run its bindings were looked up three times, all on a\n"+
				"  two-leg FROM ST, OT merge and none here, and both neutering it and\n"+
				"  pointing every window at the wrong slot left the whole suite green.\n\n"+
				"  A reader existing changes three things at once:\n"+
				"    - the SHADOWING semantics in DIVERGENCES.md (a leg shadows an\n"+
				"      enclosing binding for the duration of the inner) stop being\n"+
				"      unobservable and need justifying against this actual consumer;\n"+
				"    - the FIRST-CLAIM-WINS precedence likewise; and\n"+
				"    - the binder's correctness becomes load-bearing, so the wrong-window\n"+
				"      mutation that is green today must be made to fail.\n\n"+
				"  Do those three, then update this assertion to the new expected count.\n"+
				"  census: %s", leg, n, executor.FormatMergedLegBindingCensus())
		}
	}
}
