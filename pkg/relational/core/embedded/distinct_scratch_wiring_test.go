package embedded

// The statement layer's half of the unordered-DISTINCT continuation fix: a
// paged SELECT DISTINCT must carry its seen-set through the statement's
// ExecutionScratch, not through every page's continuation bytes (which is
// O(pages^2) to write and re-parse — see executor.ExecutionScratch).
//
// An internal test because paginatingRows and its scratch are package-private,
// and because the wiring is otherwise UNOBSERVABLE from SQL: the by-value
// encoding returns exactly the same rows, just quadratically slower, so a row
// assertion cannot tell the two apart. Dropping the WithExecutionScratch stamp
// in fetchPage is precisely the regression this pins.

import (
	"context"
	"database/sql/driver"
	"fmt"
	"io"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestPagedDistinctWithLimitReturnsEveryRow pins a shape NOTHING in the suite
// covered: DISTINCT under a SQL LIMIT while the statement pages.
//
// It exists because a scratch sweep keyed on "what the surviving continuation
// names" silently broke exactly this query — `SELECT DISTINCT v FROM t LIMIT 3`
// under a scanned-rows limit returned 2 rows and then failed with "seen-set 1
// ... does not hold", because enclosing continuation objects cache their own
// bytes and never call down into the distinct's, so the live entry went
// unmarked and was collected. Every other DISTINCT test stayed green. The
// LIMIT is load-bearing: it is what puts an operator above the DISTINCT that
// serializes a child continuation per row.
// TestExhaustedDistinctStatementLeavesNoScratchState is the STATEMENT-LEVEL
// wiring pin: after a SELECT DISTINCT runs to exhaustion through the real
// driver, the statement's scratch holds nothing.
//
// It exists because this dimension escaped three times. Every other pin here is
// an API-level test that drives BeginPage/SweepAfterPage itself, so it passes
// whether or not fetchPage calls them at all — dropping the sweep from the
// paging loop, or passing the wrong exhausted flag, left the whole suite green
// while a real statement accumulated a seen-set per page. Only a query run
// end-to-end through paginatingRows can see that.
func TestExhaustedDistinctStatementLeavesNoScratchState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newSimConnection(t, 6613)
	const rows, distinct = 60, 6
	for i := 0; i < rows; i++ {
		if _, err := c.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", i, i%distinct), nil); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	// Small pages, so the statement takes many of them and every page both
	// adopts and parks.
	c.SetOptions(api.NewOptionsBuilder().Set(api.OptExecutionScannedRowsLimit, 2).Build())

	result, err := c.QueryContext(ctx, "SELECT DISTINCT v FROM t", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	pr, ok := result.(*paginatingRows)
	if !ok {
		t.Fatalf("query returned %T, want *paginatingRows", result)
	}
	defer pr.Close() //nolint:errcheck

	seen := map[int64]bool{}
	dest := make([]driver.Value, 1)
	for {
		if err := pr.Next(dest); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("iterate: %v", err)
		}
		seen[dest[0].(int64)] = true
	}
	if len(seen) != distinct {
		t.Fatalf("paged DISTINCT returned %d values, want %d", len(seen), distinct)
	}
	if minted := pr.scratch.MintedDistinctSets(); minted < 2 {
		t.Fatalf("statement parked %d seen-sets; the shape must actually page", minted)
	}
	if live := pr.scratch.LiveDistinctSets(); live != 0 {
		t.Fatalf(
			"%d live scratch states after the statement ran to EXHAUSTION (want 0): the "+
				"paging loop must retire at the page boundary and must tell the scratch the "+
				"page exhausted — an exhausted statement can name nothing, so nothing may "+
				"survive it",
			live,
		)
	}
}

func TestPagedDistinctWithLimitReturnsEveryRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newSimConnection(t, 5512)
	const rows, distinct, limit = 40, 8, 5
	for i := 0; i < rows; i++ {
		if _, err := c.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", i, i%distinct), nil); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	c.SetOptions(api.NewOptionsBuilder().Set(api.OptExecutionScannedRowsLimit, 2).Build())

	result, err := c.QueryContext(ctx,
		fmt.Sprintf("SELECT DISTINCT v FROM t LIMIT %d", limit), nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	pr := result.(*paginatingRows)
	defer pr.Close() //nolint:errcheck

	seen := map[int64]int{}
	dest := make([]driver.Value, 1)
	for {
		if err := pr.Next(dest); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("paged DISTINCT ... LIMIT %d failed mid-drain: %v — a resume could not "+
				"find its seen-set, so a state the statement still needed was collected",
				limit, err)
		}
		seen[dest[0].(int64)]++
	}
	if len(seen) != limit {
		t.Fatalf("paged DISTINCT ... LIMIT %d returned %d rows (%v), want %d",
			limit, len(seen), seen, limit)
	}
	for v, n := range seen {
		if n != 1 {
			t.Fatalf("value %d returned %d times under LIMIT", v, n)
		}
	}
}

func TestPagedDistinctCarriesSeenSetInStatementScratch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newSimConnection(t, 8231)

	// v repeats every 5 ids over 40 rows, so duplicate runs are SCATTERED and
	// every one of them straddles a page boundary at the limit set below. A
	// sorted fixture would let a stateless resume pass.
	const rows, distinct = 40, 5
	for i := 0; i < rows; i++ {
		if _, err := c.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", i, i%distinct), nil); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	// Two scanned rows per page => ~20 pages for one DISTINCT statement.
	c.SetOptions(api.NewOptionsBuilder().Set(api.OptExecutionScannedRowsLimit, 2).Build())

	result, err := c.QueryContext(ctx, "SELECT DISTINCT v FROM t", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	pr, ok := result.(*paginatingRows)
	if !ok {
		t.Fatalf("query returned %T, want *paginatingRows", result)
	}
	defer pr.Close() //nolint:errcheck

	seen := map[int64]int{}
	dest := make([]driver.Value, 1)
	for {
		if err := pr.Next(dest); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("iterate: %v", err)
		}
		v, ok := dest[0].(int64)
		if !ok {
			t.Fatalf("row value %#v is not an int64", dest[0])
		}
		seen[v]++
	}

	// Correctness first: the paged statement is exact.
	for v, n := range seen {
		if n != 1 {
			t.Fatalf("value %d emitted %d times across the paged statement — the resume "+
				"lost its dedup state and re-admitted a duplicate", v, n)
		}
	}
	if len(seen) != distinct {
		t.Fatalf("paged DISTINCT emitted %d values, want %d — the resume dropped one",
			len(seen), distinct)
	}

	// And the wiring: the statement's scratch actually held the seen-set, so
	// the continuations were O(1) rather than carrying every emitted key. The
	// statement paged (many pages), so the operator parked once per page.
	if minted := pr.scratch.MintedDistinctSets(); minted < 2 {
		t.Fatalf("the statement's ExecutionScratch parked %d seen-sets across a "+
			"multi-page DISTINCT: the paging loop is not stamping the scratch onto "+
			"the page EvaluationContext, so every continuation is serializing the "+
			"whole seen-set again (O(pages^2))", minted)
	}
}
