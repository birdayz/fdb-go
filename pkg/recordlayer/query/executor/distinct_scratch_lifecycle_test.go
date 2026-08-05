package executor

import (
	"bytes"
	"context"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// stallInner produces NO rows and reports the SAME non-end position every time.
// That is a real inner shape, not a hypothetical: positionReplayCursor re-emits
// its incoming token verbatim when a resource limit cuts a replay short
// (position_replay_cursor.go:93-102) and emits StartContinuation before its
// first row (:128-130).
type stallInner struct {
	cont   recordlayer.RecordCursorContinuation
	closed bool
}

func (s *stallInner) OnNext(context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	return recordlayer.NewResultNoNext[QueryResult](recordlayer.ScanLimitReached, s.cont), nil
}
func (s *stallInner) Close() error   { s.closed = true; return nil }
func (s *stallInner) IsClosed() bool { return s.closed }

// unwrapDistinctHash reaches the distinctHashCursor inside what ExecutePlan
// returns (a closeHookCursor around it, with no skip/limit applied), so a test
// can drive the REAL resume path — real adoption, real resume-bytes wiring —
// and then control what the inner reports.
func unwrapDistinctHash(t *testing.T, cur recordlayer.RecordCursor[QueryResult]) *distinctHashCursor {
	t.Helper()
	hook, ok := cur.(*closeHookCursor)
	if !ok {
		t.Fatalf("ExecutePlan returned %T, want *closeHookCursor", cur)
	}
	dh, ok := hook.RecordCursor.(*distinctHashCursor)
	if !ok {
		t.Fatalf("close hook wraps %T, want *distinctHashCursor", hook.RecordCursor)
	}
	return dh
}

// TestDistinctNoProgressPageIsByteStable pins that a page which consumes
// NOTHING and leaves the inner at the position it resumed from serializes to
// the BYTE-IDENTICAL continuation it was given.
//
// THE DEFECT IT DETECTS: the by-reference carry minted a fresh token on every
// execution, so two identical logical states produced different bytes. The
// statement's stall detector is a byte comparison against the previous
// continuation (bytes.Equal(contBytes, r.continuation),
// cascades_generator.go:2025), so DISTINCT masked it and the statement would
// fetch empty pages forever instead of reporting the resource limit. Measured
// before the fix on this shape: same inner position, tokens 1 and 2.
func TestDistinctNoProgressPageIsByteStable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n, distinct = 40, 8
	evalCtx, plan := distinctFixture(t, n, distinct)
	evalCtx = evalCtx.WithExecutionScratch(NewExecutionScratch())

	// A real first page, producing a real resumable continuation.
	props := recordlayer.DefaultExecuteProperties()
	props.State = recordlayer.NewExecuteState(1 << 30)
	first, err := ExecutePlan(ctx, plan, nil, evalCtx, nil, props.WithReturnedRowLimit(2))
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	var last recordlayer.RecordCursorResult[QueryResult]
	for {
		res, nerr := first.OnNext(ctx)
		if nerr != nil {
			t.Fatalf("OnNext: %v", nerr)
		}
		last = res
		if !res.HasNext() {
			break
		}
	}
	b1, err := last.GetContinuation().ToBytes()
	if err != nil {
		t.Fatalf("first continuation: %v", err)
	}
	_ = first.Close()
	if b1 == nil {
		t.Fatal("first page produced no resumable continuation")
	}
	var decoded gen.DistinctHashContinuation
	if uerr := decoded.UnmarshalVT(b1); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	innerPos := decoded.GetInnerContinuation()

	// Now resume that continuation twice, each time with an inner that reports
	// the SAME position and yields nothing — a page that cannot make progress.
	stalled := func() []byte {
		t.Helper()
		p := recordlayer.DefaultExecuteProperties()
		p.State = recordlayer.NewExecuteState(1 << 30)
		cur, cerr := ExecutePlan(ctx, plan, nil, evalCtx, b1, p)
		if cerr != nil {
			t.Fatalf("resume: %v", cerr)
		}
		dh := unwrapDistinctHash(t, cur)
		if dh.resumeBytes == nil {
			t.Fatal("resumed cursor did not record the continuation it resumed from; " +
				"the no-progress re-emit cannot work without it")
		}
		dh.inner = &stallInner{cont: recordlayer.NewBytesContinuation(innerPos)}
		res, nerr := dh.OnNext(ctx)
		if nerr != nil {
			t.Fatalf("stalled OnNext: %v", nerr)
		}
		if res.HasNext() {
			t.Fatal("stalled page produced a row")
		}
		out, berr := res.GetContinuation().ToBytes()
		if berr != nil {
			t.Fatalf("stalled ToBytes: %v", berr)
		}
		_ = cur.Close()
		return out
	}

	a, b := stalled(), stalled()
	if !bytes.Equal(a, b) {
		t.Fatalf("two executions of the SAME no-progress page produced different bytes:\n"+
			"  %x\n  %x\nidentical logical state must serialize identically, or the "+
			"paging loop's stall detector never fires and the statement loops forever", a, b)
	}
	if !bytes.Equal(a, b1) {
		t.Fatalf("a no-progress page returned %x, want the continuation it was given (%x): "+
			"the stall detector compares the page's continuation against the PREVIOUS one, "+
			"so a page that advanced nothing must hand back exactly what it received", a, b1)
	}
}

// TestDistinctScratchDoesNotLeakOnSingleLimitedPage pins that a ONE-PAGE
// Limit(Distinct(...)) leaves no scratch garbage behind.
//
// THE DEFECT IT DETECTS: limitEnvelopeCursor serializes its child's
// continuation on every emitted row (executor.go:2005), and parking used to
// happen per serialized continuation, so an N-row page parked N states. Nothing
// ever adopts them and eviction only ran on adoption, so they lived for the
// whole statement — invisible to the memory budget, which releases a page's
// charges at teardown. Measured before the fix: 25 rows, 25 live states.
func TestDistinctScratchDoesNotLeakOnSingleLimitedPage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n = 25
	alias := values.NamedCorrelationIdentifier("limit_distinct_leak")
	evalCtx := EmptyEvaluationContext()
	table := evalCtx.GetOrCreateTempTable(alias, nil)
	for i := 0; i < n; i++ {
		if err := table.Add(dmap(map[string]any{"V": int64(i)})); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	scratch := NewExecutionScratch()
	evalCtx = evalCtx.WithExecutionScratch(scratch)
	plan := plans.NewRecordQueryLimitPlan(
		plans.NewRecordQueryDistinctPlan(plans.NewRecordQueryTempTableScanPlan(alias)),
		int64(n), 0,
	)
	props := recordlayer.DefaultExecuteProperties()
	props.State = recordlayer.NewExecuteState(1 << 30)
	cursor, err := ExecutePlan(ctx, plan, nil, evalCtx, nil, props)
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	rows := 0
	for {
		res, nerr := cursor.OnNext(ctx)
		if nerr != nil {
			t.Fatalf("OnNext: %v", nerr)
		}
		if !res.HasNext() {
			break
		}
		rows++
	}
	_ = cursor.Close()
	if rows != n {
		t.Fatalf("emitted %d rows, want %d", rows, n)
	}
	if live := scratch.LiveDistinctSets(); live > 2 {
		t.Fatalf(
			"%d live scratch states after ONE page of %d rows (want <= 2): an enclosing "+
				"operator serializes a child continuation per emitted row, so parking must "+
				"happen once per CURSOR (the prefix length rides the continuation) rather "+
				"than once per serialized continuation",
			live, rows,
		)
	}
}

// TestDistinctEarlyContinuationResumesFromItsOwnPrefix pins that a
// continuation resumes from the prefix IT named, not from everything its
// cursor went on to see.
//
// A cursor publishes ONE scratch entry and keeps appending to it, so the entry
// outruns the continuations minted from it. stateDeltaN is what tells them
// apart, and adoption must fold exactly order[:n]. Folding the whole slice
// instead passes every other test in the suite while silently dropping rows:
// the resumed page treats keys the cursor sighted AFTER the continuation was
// minted as already emitted, and they are never returned by anyone.
//
// THE SHAPE IS LOAD-BEARING AND HAS TO BE THIS EXACT ONE. ToBytes must run on
// EVERY row, because parkSeen re-publishes the cursor's live slice only when it
// re-runs — that is precisely what limitEnvelopeCursor does (executor.go:2005),
// and it is what makes the entry outrun the early continuation. A test that
// serializes once cannot express the defect at all: the entry and the
// continuation agree, and folding "everything" and folding "the prefix" are the
// same fold.
func TestDistinctEarlyContinuationResumesFromItsOwnPrefix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n = 19
	evalCtx, plan := distinctFixture(t, n, n) // every value distinct
	evalCtx = evalCtx.WithExecutionScratch(NewExecutionScratch())

	props := recordlayer.DefaultExecuteProperties()
	props.State = recordlayer.NewExecuteState(1 << 30)
	cursor, err := ExecutePlan(ctx, plan, nil, evalCtx, nil, props)
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}

	// Drain the WHOLE cursor, serializing on every row. Keep the continuation
	// minted after the FIRST row — by the time the drain ends, the cursor's
	// entry holds all n keys while that continuation still names just one.
	var afterFirst []byte
	var first any
	rows := 0
	for {
		res, nerr := cursor.OnNext(ctx)
		if nerr != nil {
			t.Fatalf("OnNext: %v", nerr)
		}
		if !res.HasNext() {
			break
		}
		rows++
		b, berr := res.GetContinuation().ToBytes()
		if berr != nil {
			t.Fatalf("row %d ToBytes: %v", rows, berr)
		}
		if rows == 1 {
			afterFirst = b
			first = res.GetValue().Positional.Slots[0]
		}
	}
	_ = cursor.Close()
	if rows != n {
		t.Fatalf("drain emitted %d rows, want %d", rows, n)
	}
	if afterFirst == nil {
		t.Fatal("no continuation after the first row")
	}

	// Resuming that early continuation must return rows 2..n: only the single
	// key it named has been emitted as far as it is concerned.
	resumeProps := recordlayer.DefaultExecuteProperties()
	resumeProps.State = recordlayer.NewExecuteState(1 << 30)
	resumed, err := ExecutePlan(ctx, plan, nil, evalCtx, afterFirst, resumeProps)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	got := map[any]bool{}
	for {
		res, nerr := resumed.OnNext(ctx)
		if nerr != nil {
			t.Fatalf("resumed OnNext: %v", nerr)
		}
		if !res.HasNext() {
			break
		}
		got[res.GetValue().Positional.Slots[0]] = true
	}
	_ = resumed.Close()

	if len(got) != n-1 {
		t.Fatalf(
			"resuming the continuation minted after row 1 returned %d rows, want %d "+
				"(every value distinct, one already emitted) — adoption folded MORE than "+
				"the prefix that continuation named (stateDeltaN), so keys the cursor "+
				"sighted after it was minted were treated as already emitted and are lost: %v",
			len(got), n-1, got,
		)
	}
	if got[first] {
		t.Fatalf("resume re-emitted %v, the one value the continuation had already emitted", first)
	}
}
