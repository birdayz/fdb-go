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
func budgetAfterDrain(t *testing.T, n, perPage, attempts int) int64 {
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
		cont = b
	}
	return state.MemUsed()
}

// TestDistinctBudgetChargesDataNotPages pins that the statement budget reflects
// the DATA the scratch holds, not the number of pages or retries that produced
// it.
//
// Two defects made this false at once, and they cancelled so precisely that the
// whole suite passed. (a) Entries left the scratch through THREE doors —
// SweepAfterPage, discardDistinct, and adoptDistinct's predecessor/sibling
// eviction — and only the first two returned their bytes; the third used a bare
// delete, so every page permanently leaked its predecessor's charge. (b) The
// fold charged nothing, so the committed base carried no weight at all.
//
// Measured before the fix, same 40 distinct values: 77 bytes at 1 row/page, 71
// at 3, 59 at 10, and **0** in a single page — the figure went DOWN as pages
// grew, because it was leak rather than data, and a statement that committed 40
// keys reported holding nothing. After: ~79 at every page size, which is the
// committed set's actual weight.
func TestDistinctBudgetChargesDataNotPages(t *testing.T) {
	t.Parallel()
	const n = 40

	one := budgetAfterDrain(t, n, 1, 1)
	few := budgetAfterDrain(t, n, 10, 1)
	single := budgetAfterDrain(t, n, n, 1)

	// The base's weight is real: a statement holding 40 committed keys must not
	// report zero. This is the half the fold charge pays for.
	if single == 0 {
		t.Fatalf("a statement that committed %d distinct keys reports 0 bytes charged: the "+
			"committed base carries no weight, so a high-cardinality DISTINCT can never "+
			"trip the memory budget it is supposed to fail loudly on", n)
	}
	// And it is DATA, not pages: 40 pages must not charge more than 1 page.
	// A per-page leak shows up here as growth with page count.
	if one > single {
		t.Fatalf("draining in %d pages charges %d bytes but a single page charges %d: "+
			"entries are leaving the scratch without returning their bytes (every removal "+
			"must go through retire), so the budget grows with the number of pages rather "+
			"than with the data", n, one, single)
	}
	if few > single {
		t.Fatalf("draining in pages of 10 charges %d bytes vs %d for a single page — same leak",
			few, single)
	}
}

// TestDistinctBudgetIsRetryIndependent pins that re-executing a page — which
// the FDB retry loop does from an unchanged continuation — does not inflate the
// statement's charge. Each attempt parks its own entry, and the ones that lose
// must give their bytes back.
func TestDistinctBudgetIsRetryIndependent(t *testing.T) {
	t.Parallel()
	const n, perPage = 40, 3
	once := budgetAfterDrain(t, n, perPage, 1)
	twice := budgetAfterDrain(t, n, perPage, 2)
	thrice := budgetAfterDrain(t, n, perPage, 3)
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
