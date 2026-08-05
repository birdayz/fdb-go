package executor

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

// Executing a page must be IDEMPOTENT with respect to the ExecutionScratch.
//
// The continuation bytes are not merely held for the statement, they are
// EXECUTED more than once: paginatingRows.fetchPage runs its whole body inside
// the FDB retry loop (runInCapturedTx -> DB.Run -> TransactCtx,
// fdb/database.go:323), which re-invokes the closure from the UNCHANGED
// r.continuation after any retryable error and resets r.buf, discarding the
// failed attempt's rows. A retryable error is routine here, not exotic: a
// resumed page runs on a ~4s budget against a 5s transaction wall.
//
// These are the two failures that shipped in the first version of the
// by-reference carry, both reproduced before the fix. The by-value encoding was
// immune to both by construction, because bytes are immutable — so a fix that
// only pins by-value behaviour proves nothing here.

// TestDistinctScratchSurvivesRetryOfAnAbortedPage pins the SILENT ROW LOSS
// case: an attempt that dies mid-drain must leave the committed seen-set
// exactly as its retry expects to find it.
//
// Before the fix the aborted attempt's Adds landed in the LIVE shared set while
// its rows were discarded, so the retry treated those values as already emitted
// and they vanished with no error at all — 10 of 12 distinct values returned.
func TestDistinctScratchSurvivesRetryOfAnAbortedPage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n, distinct, perPage = 60, 12, 3
	evalCtx, plan := distinctFixture(t, n, distinct)
	scratch := NewExecutionScratch()
	evalCtx = evalCtx.WithExecutionScratch(scratch)
	state := recordlayer.NewExecuteState(1 << 30)

	// page drains from cont, aborting after abortAfter rows when positive.
	page := func(cont []byte, abortAfter int) ([]any, []byte) {
		t.Helper()
		props := recordlayer.DefaultExecuteProperties()
		props.State = state
		props = props.WithReturnedRowLimit(perPage)
		cursor, err := ExecutePlan(ctx, plan, nil, evalCtx, cont, props)
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		var vals []any
		var last recordlayer.RecordCursorResult[QueryResult]
		for {
			if abortAfter > 0 && len(vals) >= abortAfter {
				// The transaction dies here: no terminal continuation is ever
				// serialized, and the rows are discarded by the paging loop.
				_ = cursor.Close()
				return nil, nil
			}
			res, nerr := cursor.OnNext(ctx)
			if nerr != nil {
				t.Fatalf("OnNext: %v", nerr)
			}
			last = res
			if !res.HasNext() {
				break
			}
			vals = append(vals, res.GetValue().Positional.Slots[0])
		}
		var b []byte
		if c := last.GetContinuation(); c != nil && !c.IsEnd() {
			var berr error
			if b, berr = c.ToBytes(); berr != nil {
				t.Fatalf("ToBytes: %v", berr)
			}
		}
		_ = cursor.Close()
		return vals, b
	}

	emitted := map[any]int{}
	record := func(vals []any) {
		for _, v := range vals {
			emitted[v]++
		}
	}

	v1, c1 := page(nil, 0)
	record(v1)
	if c1 == nil {
		t.Fatal("page 1 produced no continuation; the fixture must span several pages")
	}

	// Attempt A at page 2 dies mid-drain. Its rows are thrown away.
	if vA, _ := page(c1, 2); vA != nil {
		t.Fatalf("aborted attempt returned %v; it must yield nothing", vA)
	}
	// Attempt B is the retry loop re-running the closure from the SAME bytes.
	v2, cont := page(c1, 0)
	record(v2)

	for cont != nil {
		var vs []any
		vs, cont = page(cont, 0)
		record(vs)
	}

	for v, count := range emitted {
		if count != 1 {
			t.Fatalf("value %v emitted %d times: retrying a page re-admitted a duplicate", v, count)
		}
	}
	if len(emitted) != distinct {
		t.Fatalf(
			"drain emitted %d distinct values, want %d — an ABORTED attempt mutated the "+
				"committed seen-set, so its retry treated the discarded rows as already "+
				"emitted and they were lost SILENTLY (no error). A page's execution must "+
				"be idempotent w.r.t. the scratch: the adopted set is read-only and the "+
				"page accumulates into a private delta",
			len(emitted), distinct,
		)
	}
}

// TestDistinctScratchSurvivesRetryAfterSuccessorMinted pins the HARD ERROR
// case: a page that completed and minted its successor token can still fail
// retryably at COMMIT, and the retry re-runs from the predecessor's bytes.
//
// Before the fix, parking evicted the predecessor immediately, so that routine
// retry became "seen-set 1 ... which this execution's scratch does not hold".
// Minting a successor proves nothing; only ADOPTING one proves the page that
// minted it was consumed.
func TestDistinctScratchSurvivesRetryAfterSuccessorMinted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n, distinct, perPage = 60, 12, 3
	evalCtx, plan := distinctFixture(t, n, distinct)
	evalCtx = evalCtx.WithExecutionScratch(NewExecutionScratch())
	state := recordlayer.NewExecuteState(1 << 30)

	page := func(cont []byte) []byte {
		t.Helper()
		props := recordlayer.DefaultExecuteProperties()
		props.State = state
		props = props.WithReturnedRowLimit(perPage)
		cursor, err := ExecutePlan(ctx, plan, nil, evalCtx, cont, props)
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		var last recordlayer.RecordCursorResult[QueryResult]
		for {
			res, nerr := cursor.OnNext(ctx)
			if nerr != nil {
				t.Fatalf("OnNext: %v", nerr)
			}
			last = res
			if !res.HasNext() {
				break
			}
		}
		var b []byte
		if c := last.GetContinuation(); c != nil && !c.IsEnd() {
			var berr error
			if b, berr = c.ToBytes(); berr != nil {
				t.Fatalf("ToBytes: %v", berr)
			}
		}
		_ = cursor.Close()
		return b
	}

	c1 := page(nil)
	if c1 == nil {
		t.Fatal("page 1 produced no continuation")
	}
	// Page 2 runs to completion and mints its successor — then its commit
	// fails retryably, so nothing downstream ever adopts that successor.
	_ = page(c1)

	// The retry: same bytes, same scratch.
	props := recordlayer.DefaultExecuteProperties()
	props.State = state
	if _, err := ExecutePlan(ctx, plan, nil, evalCtx, c1, props); err != nil {
		t.Fatalf(
			"retrying a completed-then-failed page from its own continuation errored: %v — "+
				"eviction must key on ADOPT, not on PARK: minting a successor does not "+
				"prove the attempt that minted it committed",
			err,
		)
	}
}

// TestDistinctScratchEvictionStaysBounded pins that keying eviction on adoption
// (rather than on parking) did NOT trade the leak away for an unbounded
// scratch: a long drain must not accumulate one live entry per page.
func TestDistinctScratchEvictionStaysBounded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n, perPage = 300, 1
	evalCtx, plan := distinctFixture(t, n, n)
	scratch := NewExecutionScratch()
	evalCtx = evalCtx.WithExecutionScratch(scratch)

	rows, sizes := drainPaged(t, ctx, plan, evalCtx, perPage)
	if len(rows) != n {
		t.Fatalf("drain emitted %d rows, want %d", len(rows), n)
	}
	if minted := scratch.MintedDistinctSets(); minted < int64(len(sizes)) {
		t.Fatalf("scratch minted %d sets over %d pages; expected one per page", minted, len(sizes))
	}
	// Two: the entry the last page adopted, plus the one it published.
	if live := scratch.LiveDistinctSets(); live > 2 {
		t.Fatalf(
			"scratch holds %d live states after a %d-page drain (want <= 2): adoption must "+
				"evict the predecessor, or the scratch grows one entry per page for the "+
				"life of the statement",
			live, len(sizes),
		)
	}
}
