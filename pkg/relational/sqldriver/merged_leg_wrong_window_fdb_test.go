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

// TestFDB_MergedLegBinding_WrongWindowsAreUnobservable is the STANDING form of the
// wrong-window mutation, and it exists because that mutation was, until now, gated
// by nothing.
//
// # What was missing
//
// The claim carried by DIVERGENCES.md and by the census gate's own remedy text is
// that the merged-leg bindings are not load-bearing on any covered shape. Half of
// its evidence was a mutation somebody performed BY HAND — edit
// bindMergedOuterLegs to aim every leg at the wrong slots, run the suite, watch it
// stay green. Nothing in CI performed it. So on the day a read starts depending on
// which slots its window covers, nothing went red: the binder kept binding, the
// census kept counting, and a green run would have been read as "the bindings are
// correct" when all it ever meant was "nobody looked".
//
// The sibling pin (TestFDB_MergedLegBinding_ReaderShapeIsRedundant) does not close
// that gap and does not claim to. It proves the two RESOLUTION ROUTES agree — the
// binder's window versus the alias resolving to nothing — which is a statement
// about the binding's PRESENCE. This one is about its AIM, and the two fail on
// different days: a consumer that reads a leg column gets a wrong value from a
// misaimed window long before it gets no value at all.
//
// # What it does
//
// The same merged-row reader shape (newMergedLegReaderShape — the corpus's only
// one), run twice in one process: once with each leg window aimed at its own leg,
// once with every window rotated onto a SIBLING leg's span via
// EvaluationContext.WithMergedLegWrongWindows. The rows must agree, and must be
// the answer Java gives. When they stop agreeing, the binder has become
// load-bearing and the failure message says what that costs.
//
// Rotation rather than "everything at slot 0", which is what the by-hand mutation
// used: leg 0 of a merged row already starts at 0, so a constant offset leaves the
// FIRST leg — `ST`, the one the corpus's reader actually reads — aimed correctly.
// The by-hand mutation was weaker than its description; the standing one is not.
//
// # It cannot pass vacuously, and that is asserted, not hoped
//
// Two floors, on each run:
//
//   - the shape still READS a binder window (reads > 0, on exactly one merged-row
//     shape). An upstream change that stops planning this shape as a merged outer
//     stops producing the read, and then every comparison here is between two
//     identical runs. That is the failure mode that killed the first draft of the
//     sibling pin, which pinned a plausible cousin binding the same windows and
//     reading none of them.
//   - the read RESOLVED TO A MISAIMED WINDOW (misaimed > 0 under the hook, and
//     exactly 0 without it). Windows-bound-wrong is the weaker floor and is not
//     the one used: a merged row whose legs nothing looks up can be misaimed all
//     day and prove nothing. What has to be true is that the perturbed window is
//     the one the read went through.
//
// # The mutation check, which cannot be committed as its own test
//
// The instrument's own discrimination was verified by neutering the hook —
// making misaimMergedLegWindows return immediately, so WithMergedLegWrongWindows
// binds CORRECT windows — and re-running. Measured, verbatim:
//
//	THE WRONG-WINDOW ARM NEVER ENGAGED for "ST" out of ST[0,3):ID,C,ARR|OT[3,5):ID,K.
//	  reads through a MISAIMED window: map[]
//
// with the reads floor above it still satisfied (3 reads, same single shape) and
// the row comparison still green. That is the discrimination the instrument needs:
// the reads are not a side effect of the hook (they survive its removal), and the
// misaim floor is what the hook actually moves. It stays a note rather than a
// committed test because the mutation is an edit to the production-side binder,
// and a test that neuters the binder for its own execution would be asserting on
// the neuter rather than on the code that ships.
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
	// is process-wide and this suite is parallel, so a delta also counts whatever
	// TestFDB_ExistsInnerShadow and the redundancy pin — which read the same alias
	// out of the same merged shape — happened to read in the same window.
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
				out = append(out, fmt.Sprintf("%v", executor.RowValue(r)))
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

	// FLOOR 1. The shape must actually read a binder window, on exactly ONE
	// merged-row shape — the shape this run's result gets registered under.
	proven, provenReads := soleRead(correctSink.Reads())
	if proven.Alias != redundantReaderAlias || provenReads <= 0 {
		t.Fatalf("this execution did not produce exactly one multi-leg reader shape for %q.\n"+
			"  reads this execution made: %v\n"+
			"  With no read, the wrong-window arm below perturbs a binding nobody\n"+
			"  consults and the row comparison is between two identical runs — the\n"+
			"  instrument would be green while measuring nothing, which is the exact\n"+
			"  state it was built to end. More than one shape means the registration\n"+
			"  covers a proper subset of what this run measured. Re-derive the reader\n"+
			"  shape from the census before trusting this test.\n  sql: %s\n  plan: %s",
			redundantReaderAlias, correctSink.Reads(), q, plan.Explain())
	}
	// The correct-window run must read through NO misaimed window, or "misaimed"
	// has stopped meaning what the floor below reads it as.
	if misaimed := correctSink.MisaimedReads(); len(misaimed) != 0 {
		t.Fatalf("the CORRECT-window run read through a misaimed window: %v\n"+
			"  Nothing asked for the perturbation here. Either the hook has escaped "+
			"its EvaluationContext (it is a per-execution copy, exactly so a parallel "+
			"suite cannot leak it) or the misaimed stamp is being set on windows "+
			"misaimMergedLegWindows never moved. Either way the floor below stops "+
			"discriminating.", misaimed)
	}
	if fmt.Sprint(correct) != fmt.Sprint(mergedLegReaderShapeWant) {
		t.Fatalf("with correct windows the rows are wrong: %v, want %v\n  sql: %s\n  plan: %s",
			correct, mergedLegReaderShapeWant, q, plan.Explain())
	}

	// ROUTE B: every window rotated onto a sibling leg's span.
	wrong, wrongSink, wErr := run(true)

	// FLOOR 2. The perturbation reached the READ, not merely the bind. Checked
	// before the rows, because a wrong-window run that read no misaimed window
	// agrees with route A for a reason that has nothing to do with the claim.
	misaimedReads := wrongSink.MisaimedReads()
	if misaimedReads[proven] <= 0 {
		t.Fatalf("THE WRONG-WINDOW ARM NEVER ENGAGED for %q out of %s.\n"+
			"  reads through a MISAIMED window: %v\n"+
			"  reads through ANY binder window:  %v\n"+
			"  exec error (if any): %v\n\n"+
			"  The rows below would agree because nothing was perturbed, and this test\n"+
			"  would report the standing mutation as passing while performing no\n"+
			"  mutation. Two ways to get here: the merged row stopped carrying two legs\n"+
			"  of DIFFERENT shape (a rotation between identically-shaped legs is a\n"+
			"  no-op and is deliberately not counted as a misaim), or the hook stopped\n"+
			"  reaching the binder — it rides EvaluationContext, so a path that builds\n"+
			"  a FRESH context instead of copying this one silently drops it.",
			redundantReaderAlias, proven.Shape, misaimedReads, wrongSink.Reads(), wErr)
	}

	// THE CLAIM. Wrong windows everywhere, same rows.
	//
	// An execution ERROR counts as a difference: a misaimed window can be narrower
	// than the leg it displaced, so a read that depends on it can fail loudly on an
	// out-of-range ordinal rather than return a wrong value. Both mean the same
	// thing about the binding.
	if wErr != nil || fmt.Sprint(correct) != fmt.Sprint(wrong) {
		t.Fatalf("MISAIMING THE BINDINGS CHANGED THE ANSWER for %q out of %s.\n"+
			"  correct windows: %v\n"+
			"  wrong windows:   %v\n"+
			"  wrong-window exec error (if any): %v\n\n"+
			"  ACTIVATION HAS ARRIVED. The merged-leg bindings are now LOAD-BEARING on\n"+
			"  a covered shape, and this test is the instrument that was placed to say\n"+
			"  so. Do not relax it. The procedure is:\n\n"+
			"    1. REMOVE the proven-redundant exclusion for this (alias, shape) from\n"+
			"       the census criterion — see assertMergedLegBindingCensus in\n"+
			"       embedded_fdb_test.go. It excuses these reads on the strength of\n"+
			"       their being unobservable, which is what just stopped being true.\n"+
			"       Both proofs registering it (this test and\n"+
			"       TestFDB_MergedLegBinding_ReaderShapeIsRedundant) stop registering\n"+
			"       on a failing path, so the exclusion is already gone in this run;\n"+
			"       what has to change is the wording that calls it expected.\n"+
			"    2. The binder's CORRECTNESS becomes a real invariant. This test\n"+
			"       inverts: the wrong-window run must now be asserted to DIFFER, and\n"+
			"       the correct-window run to give Java's answer.\n"+
			"    3. The SHADOWING and FIRST-CLAIM-WINS semantics in DIVERGENCES.md stop\n"+
			"       being unobservable and need justifying against this actual\n"+
			"       consumer — they are currently justified only by consistency with\n"+
			"       the leg table's other readers.\n"+
			"    4. DIVERGENCES.md's \"that reader is NOT load-bearing\" paragraph is\n"+
			"       now false. Rewrite it from this measurement.\n"+
			"  sql: %s\n  plan: %s",
			redundantReaderAlias, proven.Shape, correct, wrong, wErr, q, plan.Explain())
	}

	// Registered only on the passing path, and only for the SHAPE this run proved.
	// The census gate excuses a read from the activation criterion when a proof in
	// THIS run showed it not load-bearing; misaiming the window is such a proof, and
	// a strictly different perturbation from the sibling pin's decline, so both
	// register and the gate names both.
	executor.RegisterRedundantMergedLegReader(proven, t.Name())
}
