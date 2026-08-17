package sqldriver_test

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestFDB_MergedLegBinding_WrongWindowsAreUnobservable is the STANDING form of
// the wrong-window mutation: aim every merged-row leg window at a SIBLING leg's
// span and require the answer not to move.
//
// # Why it survives the read channel's retirement
//
// Its sibling (TestFDB_MergedLegBinding_NothingReadsTheBinder) measures that
// nothing resolves to a binder window at all. That is the stronger fact and it
// makes the comparison below hold for a stronger reason than it used to — but it
// does not make this test redundant, because the two fail on different days.
// The sibling goes red when a READ appears; this one goes red when a read appears
// AND depends on which slots its window covers. The second is the one that costs
// wrong rows, and it is the claim DIVERGENCES.md actually carries.
//
// The floors changed with the retirement and the change is the whole
// reconciliation. This test used to require the shape to READ a binder window,
// and the read to resolve through a MISAIMED one — without those the comparison
// was between two identical runs and the instrument was green while measuring
// nothing. Both are unsatisfiable now: the ordinal model resolves a leg reference
// by baked slot, so there is no read to misaim. What replaces them is the pair
// that still discriminates:
//
//   - the shape RUNS and answers Java's rows, so the comparison is over a live
//     shape rather than a query that stopped being planned;
//   - the binder BINDS on it, and binds MISAIMED windows under the hook — the
//     perturbation reaching the bind is what is left to assert once it cannot
//     reach a read.
//
// # What it does
//
// The same merged-row reader shape (newMergedLegReaderShape — the corpus's only
// one), run twice in one process: once with each leg window aimed at its own leg,
// once with every window rotated onto a SIBLING leg's span via
// EvaluationContext.WithMergedLegWrongWindows. The rows must agree, and must be
// the answer Java gives.
//
// Rotation rather than "everything at slot 0", which is what an earlier by-hand
// mutation used: leg 0 of a merged row already starts at 0, so a constant offset
// leaves the FIRST leg — `ST`, the leg this shape's reference names — aimed
// correctly. The by-hand mutation was weaker than its description.
func TestFDB_MergedLegBinding_WrongWindowsAreUnobservable(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	if !values.LegIdentityCensusEnabled() {
		t.Fatal("the leg-identity census gate is OFF, so WithMergedLegWrongWindows is " +
			"not honoured and both runs below bind the SAME correct windows — the " +
			"comparison would pass vacuously and the standing mutation would be " +
			"standing over nothing. The sqldriver TestMain enables it for the whole " +
			"run (runUnderLegIdentityCensus).")
	}

	ctx := context.Background()
	shape := newMergedLegReaderShape(ctx, t)
	db, md, ks, plan, q := shape.db, shape.md, shape.ks, shape.plan, shape.sql

	// run executes the plan, optionally with every merged-row leg window aimed at a
	// sibling's span, and reports the rows plus a sink holding what THIS execution
	// read. An execution error is RETURNED rather than fatal: a misaimed window can
	// be narrower than the leg it replaced, so a load-bearing read of a high ordinal
	// fails loudly instead of returning a wrong value, and that is an activation
	// signal exactly like a changed row.
	//
	// The sink, not a before/after delta over the process-global census: the census
	// is process-wide and this suite is parallel, so a delta also counts whatever a
	// concurrently running test did in the same window.
	run := func(wrong bool) ([]string, *executor.MergedLegReadSink, error) {
		t.Helper()
		sink := executor.NewMergedLegReadSink()
		var out []string
		_, eerr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).
				SetMetaDataProvider(md).SetSubspace(ks).Open()
			if sErr != nil {
				return nil, sErr
			}
			evalCtx := executor.EmptyEvaluationContext().WithMergedLegReadSink(sink)
			if wrong {
				evalCtx = evalCtx.WithMergedLegWrongWindows()
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
				out = append(out, positionalNamedPipeSprint(r))
			}
			return nil, nil
		})
		sort.Strings(out)
		return out, sink, eerr
	}

	// ROUTE A: every leg window aimed at its own leg.
	correct, correctSink, cErr := run(false)
	if cErr != nil {
		t.Fatalf("exec with CORRECT windows %q: %v", q, cErr)
	}
	if fmt.Sprint(correct) != fmt.Sprint(mergedLegReaderShapeWant) {
		t.Fatalf("with correct windows the rows are wrong: %v, want %v\n  sql: %s\n  plan: %s",
			correct, mergedLegReaderShapeWant, q, plan.Explain())
	}
	// The correct-window run must read through NO misaimed window, or "misaimed"
	// has stopped meaning what the floor below reads it as.
	if misaimed := correctSink.MisaimedReads(); len(misaimed) != 0 {
		t.Fatalf("the CORRECT-window run read through a misaimed window: %v\n"+
			"  Nothing asked for the perturbation here. Either the hook has escaped "+
			"its EvaluationContext (it is a per-execution copy, exactly so a parallel "+
			"suite cannot leak it) or the misaimed stamp is being set on windows "+
			"misaimMergedLegWindows never moved.", misaimed)
	}

	// ROUTE B: every window rotated onto a sibling leg's span.
	wrong, wrongSink, wErr := run(true)

	// FLOOR. The perturbation reached the BIND. It can no longer reach a READ —
	// nothing resolves to a binder window since leg references began resolving by
	// baked slot — so the bind is what is left to establish, and without it the
	// rows below agree because nothing was perturbed at all.
	misaimedBinds := executor.MisaimedMergedLegBinds()
	if misaimedBinds == 0 {
		t.Fatalf("THE WRONG-WINDOW ARM NEVER ENGAGED.\n"+
			"  misaimed BINDS over this process: %d\n"+
			"  reads through a MISAIMED window:  %v\n"+
			"  reads through ANY binder window:   %v\n"+
			"  exec error (if any): %v\n\n"+
			"  The rows below would agree because nothing was perturbed, and this test\n"+
			"  would report the standing mutation as passing while performing none. Two\n"+
			"  ways to get here: the merged row stopped carrying two legs of DIFFERENT\n"+
			"  shape (a rotation between identically-shaped legs is a no-op and is\n"+
			"  deliberately not counted as a misaim), or the hook stopped reaching the\n"+
			"  binder — it rides EvaluationContext, so a path that builds a FRESH\n"+
			"  context instead of copying this one silently drops it.",
			misaimedBinds, wrongSink.MisaimedReads(), wrongSink.Reads(), wErr)
	}

	// THE CLAIM. Wrong windows everywhere, same rows.
	//
	// An execution ERROR counts as a difference: a misaimed window can be narrower
	// than the leg it displaced, so a read that depends on it can fail loudly on an
	// out-of-range ordinal rather than return a wrong value. Both mean the same
	// thing about the binding.
	if wErr != nil || fmt.Sprint(correct) != fmt.Sprint(wrong) {
		t.Fatalf("MISAIMING THE BINDINGS CHANGED THE ANSWER.\n"+
			"  correct windows: %v\n"+
			"  wrong windows:   %v\n"+
			"  wrong-window exec error (if any): %v\n\n"+
			"  ACTIVATION HAS ARRIVED. The merged-leg bindings are now LOAD-BEARING on\n"+
			"  a covered shape, and this test is the instrument that was placed to say\n"+
			"  so. Do not relax it. The procedure is:\n\n"+
			"    1. The binder's CORRECTNESS becomes a real invariant. This test\n"+
			"       inverts: the wrong-window run must now be asserted to DIFFER, and\n"+
			"       the correct-window run to give Java's answer.\n"+
			"    2. The SHADOWING and FIRST-CLAIM-WINS semantics in DIVERGENCES.md stop\n"+
			"       being unobservable and need justifying against this actual\n"+
			"       consumer — they are currently justified only by consistency with\n"+
			"       the leg table's other readers.\n"+
			"    3. DIVERGENCES.md's \"that reader is NOT load-bearing\" paragraph is\n"+
			"       now false. Rewrite it from this measurement.\n"+
			"    4. TestFDB_MergedLegBinding_NothingReadsTheBinder will have failed\n"+
			"       beside this one, naming the reader that appeared.\n"+
			"  sql: %s\n  plan: %s",
			correct, wrong, wErr, q, plan.Explain())
	}
}
