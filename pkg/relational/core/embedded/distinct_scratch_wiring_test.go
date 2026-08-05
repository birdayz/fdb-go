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

// TestDistinctScratchIsSweptAtPageBoundaries pins the statement-level half of
// the scratch lifecycle: after a statement runs, the scratch holds only what
// the surviving continuation still needs.
//
// Without the page-boundary sweep, a single page leaves one parked state per
// EMITTED ROW (an enclosing operator serializes its child's continuation on
// every row) and a correlated inner leaves one per completed invocation. Both
// are statement-lifetime growth, and both are invisible to the memory budget,
// which releases a page's charges at teardown — so nothing else would catch it.
func TestDistinctScratchIsSweptAtPageBoundaries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newSimConnection(t, 4471)
	const rows, distinct = 60, 6
	for i := 0; i < rows; i++ {
		if _, err := c.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", i, i%distinct), nil); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	drain := func(label, q string) *paginatingRows {
		t.Helper()
		result, err := c.QueryContext(ctx, q, nil)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		pr, ok := result.(*paginatingRows)
		if !ok {
			t.Fatalf("%s returned %T, want *paginatingRows", label, result)
		}
		dest := make([]driver.Value, 1)
		for {
			if err := pr.Next(dest); err != nil {
				if err == io.EOF {
					break
				}
				t.Fatalf("%s iterate: %v", label, err)
			}
		}
		pr.Close() //nolint:errcheck
		return pr
	}

	// Single unpaged page: every emitted row's continuation gets serialized by
	// the operators above the DISTINCT.
	c.SetOptions(api.NewOptionsBuilder().Build())
	single := drain("single-page DISTINCT", "SELECT DISTINCT v FROM t")
	if live := single.scratch.LiveDistinctSets(); live > 2 {
		t.Fatalf(
			"%d live scratch states after a single-page DISTINCT over %d rows (want <= 2): "+
				"states parked during the page and never named by its surviving continuation "+
				"are garbage and must be swept at the page boundary",
			live, rows,
		)
	}

	// And across many pages, where each page both adopts and publishes.
	c.SetOptions(api.NewOptionsBuilder().Set(api.OptExecutionScannedRowsLimit, 2).Build())
	paged := drain("paged DISTINCT", "SELECT DISTINCT v FROM t")
	if live := paged.scratch.LiveDistinctSets(); live > 2 {
		t.Fatalf(
			"%d live scratch states after a multi-page DISTINCT (want <= 2): the sweep must "+
				"keep only what the surviving continuation names plus what the page adopted",
			live,
		)
	}
	if minted := paged.scratch.MintedDistinctSets(); minted < 2 {
		t.Fatalf("paged DISTINCT minted %d states; the shape must actually page", minted)
	}
}

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
