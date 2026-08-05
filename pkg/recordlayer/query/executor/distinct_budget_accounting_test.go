package executor

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

// budgetAfterDrain pages a DISTINCT to exhaustion and returns the bytes still
// charged to the statement. attempts>1 re-runs each page from the same
// continuation, as an FDB retry does; only the last attempt commits (sweeps).
func budgetAfterDrain(t *testing.T, n, perPage, attempts int) (live, final int64) {
	t.Helper()
	ctx := context.Background()
	evalCtx, plan := distinctFixture(t, n, n)
	scratch := NewExecutionScratch()
	evalCtx = evalCtx.WithExecutionScratch(scratch)
	state := recordlayer.NewExecuteState(1 << 30)

	var cont []byte
	for page := 0; page < 5000; page++ {
		var b []byte
		for a := 0; a < attempts; a++ {
			scratch.BeginPage()
			props := recordlayer.DefaultExecuteProperties()
			props.State = state
			props = props.WithReturnedRowLimit(perPage)
			cursor, err := ExecutePlan(ctx, plan, nil, evalCtx, cont, props)
			if err != nil {
				t.Fatalf("page %d attempt %d: %v", page, a, err)
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
			b = nil
			if c := last.GetContinuation(); c != nil && !c.IsEnd() {
				var berr error
				if b, berr = c.ToBytes(); berr != nil {
					t.Fatalf("ToBytes: %v", berr)
				}
			}
			_ = cursor.Close()
			if a == attempts-1 {
				scratch.SweepAfterPage(b == nil)
			}
		}
		if b == nil {
			break
		}
		// The charge while the committed layer is still LIVE. Sampled on the
		// last page that still has a continuation, which is where the layer
		// holds the most and is unambiguously still referenced.
		live = state.MemUsed()
		cont = b
	}
	return live, state.MemUsed()
}

// TestDistinctBudgetChargesDataNotPages pins that the statement budget reflects
// the DATA the scratch holds, not the number of pages that produced it — and
// that the charge is released once the data is gone.
//
// Three defects lived here, and the first two cancelled so precisely that the
// whole suite passed. (a) Entries left the scratch through THREE doors and only
// two returned their bytes; adoptDistinct's predecessor/sibling eviction used a
// bare delete, so every page permanently leaked its predecessor's charge.
// (b) The fold charged nothing, so the committed base carried no weight at all.
// (c) Once (b) was fixed, the fold charge was PERMANENT while the layer it paid
// for was collectible, so a correlated statement accumulated the weight of every
// inner it had already finished.
//
// Measured on 40 distinct values, charge remaining after a completed drain:
// 77 bytes at 1 row/page, 71 at 3, 59 at 10, and 0 in a single page — the
// figure went DOWN as pages grew, because it was leak rather than data.
func TestDistinctBudgetChargesDataNotPages(t *testing.T) {
	t.Parallel()
	const n = 40

	liveOne, finalOne := budgetAfterDrain(t, n, 1, 1)
	liveFew, finalFew := budgetAfterDrain(t, n, 10, 1)

	// A live committed layer weighs something. This is the half the fold charge
	// pays for, and the reason a high-cardinality DISTINCT can be caught at all.
	if liveOne == 0 || liveFew == 0 {
		t.Fatalf("a DISTINCT holding a live committed set reports %d / %d bytes charged: "+
			"the committed layer carries no weight, so a high-cardinality seen-set can "+
			"never trip the memory budget it is supposed to fail loudly on", liveOne, liveFew)
	}
	// And it is DATA, not pages: draining the same values in 40 pages must not
	// charge more than draining them in 4. A per-page leak shows up here.
	if liveOne > liveFew*2 {
		t.Fatalf("draining in pages of 1 charges %d bytes at its peak but pages of 10 charge "+
			"%d: entries are leaving the scratch without returning their bytes (every "+
			"removal must go through retire), so the budget grows with the number of pages "+
			"rather than with the data", liveOne, liveFew)
	}
	// And when the data is gone, so is the charge.
	if finalOne != 0 || finalFew != 0 {
		t.Fatalf("a fully drained statement still holds %d / %d bytes with no live scratch "+
			"state: the fold charge is owned by the committed LAYER and must be released "+
			"when its last referencing state retires, or every finished DISTINCT leaves its "+
			"weight behind", finalOne, finalFew)
	}
}

// TestDistinctBudgetIsRetryIndependent pins that re-executing a page — which
// the FDB retry loop does from an unchanged continuation — does not inflate the
// statement's charge. Each attempt parks its own entry, and the ones that lose
// must give their bytes back.
func TestDistinctBudgetIsRetryIndependent(t *testing.T) {
	t.Parallel()
	const n, perPage = 40, 3
	once, _ := budgetAfterDrain(t, n, perPage, 1)
	twice, _ := budgetAfterDrain(t, n, perPage, 2)
	thrice, _ := budgetAfterDrain(t, n, perPage, 3)
	if once != twice || once != thrice {
		t.Fatalf(
			"identical data charges %d / %d / %d bytes at 1 / 2 / 3 attempts per page: a "+
				"retried page's losing attempts are not returning their bytes, so a "+
				"retry-heavy statement fails the memory budget for memory it does not hold",
			once, twice, thrice,
		)
	}
}

// TestDistinctHighCardinalityTripsTheBudget pins the promise the whole
// by-reference design rests on: a DISTINCT whose seen-set outgrows the
// statement budget fails LOUDLY, never silently.
//
// It is the pin for the fold charge specifically. Fixing only the leak makes
// every byte come back at page boundaries, so nothing ever accumulates and a
// 4000-key drain completes holding almost nothing — the loud failure disappears
// entirely, which is worse than the leak it replaced.
func TestDistinctHighCardinalityTripsTheBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n, perPage, budget = 4000, 250, 2000
	evalCtx, plan := distinctFixture(t, n, n)
	scratch := NewExecutionScratch()
	evalCtx = evalCtx.WithExecutionScratch(scratch)
	state := recordlayer.NewExecuteState(budget)

	var cont []byte
	rows := 0
	var failure error
	for page := 0; page < 5000 && failure == nil; page++ {
		scratch.BeginPage()
		props := recordlayer.DefaultExecuteProperties()
		props.State = state
		props = props.WithReturnedRowLimit(perPage)
		cursor, err := ExecutePlan(ctx, plan, nil, evalCtx, cont, props)
		if err != nil {
			failure = err
			break
		}
		var last recordlayer.RecordCursorResult[QueryResult]
		for {
			res, nerr := cursor.OnNext(ctx)
			if nerr != nil {
				failure = nerr
				break
			}
			last = res
			if !res.HasNext() {
				break
			}
			rows++
		}
		var b []byte
		if failure == nil {
			if c := last.GetContinuation(); c != nil && !c.IsEnd() {
				b, _ = c.ToBytes()
			}
		}
		_ = cursor.Close()
		scratch.SweepAfterPage(b == nil)
		if b == nil {
			break
		}
		cont = b
	}

	if failure == nil {
		t.Fatalf(
			"a %d-key DISTINCT drained %d rows under a %d-byte budget without failing "+
				"(charged %d): the committed base is not charged, so the budget can never "+
				"catch a high-cardinality seen-set and the design's loud-failure promise is "+
				"empty",
			n, rows, budget, state.MemUsed(),
		)
	}
	var memErr *recordlayer.MemoryLimitExceededError
	if !errors.As(failure, &memErr) {
		t.Fatalf("high-cardinality DISTINCT failed with %v, want the memory budget error", failure)
	}
}

// TestDistinctCompletedInnersReleaseTheirCharge pins that a correlated DISTINCT
// which paged and then FINISHED gives its accounting back.
//
// Each inner of FlatMap(outer, Distinct(inner)) that spans a page gets adopted,
// so its keys are folded into a committed layer and charged. The inner then
// finishes and its states retire — at which point the layer is unreachable and
// its bytes must go back. While the fold charge was permanent they did not, so
// live state stayed bounded at zero while the accounting climbed with the
// number of outer rows: measured 20 bytes at 4 outer rows, 38 at 8, 80 at 16,
// 158 at 32. A long enough statement then failed MemoryLimitExceededError for
// seen-sets it had already released.
//
// The charge lifecycle has to mirror the entry lifecycle: the layer owns the
// fold charge, and the last state to retire pays it back.
func TestDistinctCompletedInnersReleaseTheirCharge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const innerRows, perPage = 4, 3

	drain := func(outerRows int) (rows int, live int, mem int64) {
		t.Helper()
		evalCtx, plan := flatMapOverDistinct(t, outerRows, innerRows)
		scratch := NewExecutionScratch()
		evalCtx = evalCtx.WithExecutionScratch(scratch)
		state := recordlayer.NewExecuteState(1 << 30)
		var cont []byte
		for page := 0; page < 5000; page++ {
			scratch.BeginPage()
			props := recordlayer.DefaultExecuteProperties()
			props.State = state
			props = props.WithReturnedRowLimit(perPage)
			cursor, err := ExecutePlan(ctx, plan, nil, evalCtx, cont, props)
			if err != nil {
				t.Fatalf("page %d: %v", page, err)
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
				rows++
				// Per-row serialization: the LIMIT envelope's behaviour, and
				// what makes each inner cursor park in the first place.
				if _, berr := res.GetContinuation().ToBytes(); berr != nil {
					t.Fatalf("row ToBytes: %v", berr)
				}
			}
			var b []byte
			if c := last.GetContinuation(); c != nil && !c.IsEnd() {
				var berr error
				if b, berr = c.ToBytes(); berr != nil {
					t.Fatalf("page ToBytes: %v", berr)
				}
			}
			_ = cursor.Close()
			scratch.SweepAfterPage(b == nil)
			if b == nil {
				break
			}
			cont = b
		}
		return rows, scratch.LiveDistinctSets(), state.MemUsed()
	}

	var small, large int64
	for _, outerRows := range []int{4, 32} {
		rows, live, mem := drain(outerRows)
		if rows != outerRows*innerRows {
			t.Fatalf("outerRows=%d emitted %d rows, want %d", outerRows, rows, outerRows*innerRows)
		}
		if live != 0 {
			t.Fatalf("outerRows=%d left %d live scratch states, want 0", outerRows, live)
		}
		if mem != 0 {
			t.Fatalf(
				"outerRows=%d finished holding %d bytes with ZERO live scratch state: every "+
					"completed inner's committed layer is unreachable, so its fold charge must "+
					"have been released — a permanent charge accumulates until a valid query "+
					"fails the memory budget for sets it no longer holds",
				outerRows, mem,
			)
		}
		if outerRows == 4 {
			small = mem
		} else {
			large = mem
		}
	}
	// Stated as a growth check too: the accounting must not scale with how many
	// inners the statement happened to run.
	if large != small {
		t.Fatalf("accounting grew from %d to %d bytes as outer rows went 4 -> 32", small, large)
	}
}
