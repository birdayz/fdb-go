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

// TestFDB_MergedLegBinding_NothingReadsTheBinder is the standing measurement
// that the merged-leg binder has NO consumer.
//
// # What it replaced, and why the replacement is stronger
//
// This test used to be a LICENSE. The merged-leg binding census alarmed when a
// read resolved to a bindMergedOuterLegs window on a MULTI-LEG merged row while
// no leg-local bake produced anything, the corpus contained three such reads a
// run — all on redundantReaderAlias, out of the shape newMergedLegReaderShape
// builds — and the exclusion that kept the gate from being permanently red was
// granted here, by running the same query down BOTH resolution routes and
// getting the same rows.
//
// The ordinal model removed the reads themselves. A leg reference resolves by
// BAKED SLOT now, so the binder's windows are built (fifteen thousand a run) and
// consulted by nothing. "The binding is redundant on this shape" has become "the
// binding is not looked at", which is the stronger statement and needs no
// exclusion at all: the census asserts zero reads outright.
//
// # What this test now measures, and why it is not a tautology
//
// Two things the census alone cannot say:
//
//   - the shape is still REACHED. The census's zero is over the whole suite and
//     is satisfied by a suite that stopped planning this shape entirely. Here the
//     query is built, planned and executed, and its ROWS are checked against the
//     answer TestFDB_ExistsInnerShadow pins as live Java 4.12.11.0 behaviour — so
//     the zero below is a zero over a shape that ran.
//   - the binder still BOUND on it. Windows bound with none read is the retired
//     steady state; windows neither bound nor read would mean the binder stopped
//     running on this shape and the read zero says nothing about the retirement.
//
// # Why a context-scoped sink and not the process census
//
// The census is process-wide and this suite is parallel, so a before/after delta
// also counts whatever any concurrently running test read. The sink rides one
// EvaluationContext and holds exactly what THIS execution did.
func TestFDB_MergedLegBinding_NothingReadsTheBinder(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	if !values.LegIdentityCensusEnabled() {
		t.Fatal("the leg-identity census gate is OFF, so no read is recorded whatever " +
			"happens and the zero below holds vacuously. The sqldriver TestMain enables " +
			"it for the whole run (runUnderLegIdentityCensus).")
	}

	ctx := context.Background()
	shape := newMergedLegReaderShape(ctx, t)
	db, md, ks, plan, q := shape.db, shape.md, shape.ks, shape.plan, shape.sql

	sink := executor.NewMergedLegReadSink()
	var got []string
	if _, eerr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).
			SetMetaDataProvider(md).SetSubspace(ks).Open()
		if sErr != nil {
			return nil, sErr
		}
		evalCtx := executor.EmptyEvaluationContext().WithMergedLegReadSink(sink)
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
			got = append(got, positionalNamedPipeSprint(r))
		}
		return nil, nil
	}); eerr != nil {
		t.Fatalf("exec %q: %v", q, eerr)
	}
	sort.Strings(got)

	// The shape RAN and answered correctly, so the zero below is a zero over a
	// live shape rather than over a query that stopped being planned.
	if fmt.Sprint(got) != fmt.Sprint(mergedLegReaderShapeWant) {
		t.Fatalf("rows = %v, want %v\n  sql: %s\n  plan: %s",
			got, mergedLegReaderShapeWant, q, plan.Explain())
	}

	// THE MEASUREMENT. Nothing resolved to a binder-produced window.
	if reads := sink.Reads(); len(reads) != 0 {
		t.Fatalf("this execution READ %d binder-produced window shape(s): %v\n"+
			"  The merged-leg binder is supposed to have no consumer left: a leg\n"+
			"  reference resolves by baked SLOT, and the whole-suite census asserts zero\n"+
			"  reads. A read here means the binder is load-bearing again, which changes\n"+
			"  three things at once — the SHADOWING semantics in DIVERGENCES.md (a leg\n"+
			"  shadows an enclosing binding for the duration of the inner) stop being\n"+
			"  unobservable and need justifying against this consumer, FIRST-CLAIM-WINS\n"+
			"  likewise, and the binder's correctness becomes a real invariant rather\n"+
			"  than a convention.\n"+
			"  Do NOT relax this. Establish which reader appeared, then decide whether\n"+
			"  the binding or the reader is the thing to change.\n"+
			"  sql: %s\n  plan: %s", len(reads), reads, q, plan.Explain())
	}

	// The binder still RAN on this shape. Without this, a binder that stopped
	// being reached would report the identical zero above, and the retirement
	// this test records would be indistinguishable from the instrument dying.
	binds, _ := executor.MergedLegBindingCensus()
	if len(binds) == 0 {
		t.Fatal("the merged-leg binder bound NOTHING over this process.\n" +
			"  The read zero above then says nothing about the retirement: a binder\n" +
			"  nobody runs reads exactly like a binder nobody consults. Either the\n" +
			"  binder stopped being reached or its bind counter is dead.")
	}
}
