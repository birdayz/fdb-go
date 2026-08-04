package embedded

// RFC-198 Decision 5's OTHER half: ScanLimiterState becomes
// transaction-scoped, but the RFC-130 memory byte budget (ExecuteState,
// paginatingRows.execState) stays STATEMENT-scoped. A memory budget is a
// property of one statement's buffering operators; a transaction-scoped one
// would make a later statement fail for memory an earlier one consumed.
//
// MEASURED at introduction: that failure cannot currently manifest through
// sequential statements, because every RFC-130 charge is released within its
// page (the ChargeMemory/ReleaseMemory pairs in query/executor). That makes a
// wrongly-shared state observationally SILENT at the SQL level — which is
// exactly why the pin here is structural (pointer distinctness) plus the
// negative-result fact (released-to-zero) whose change would make sharing
// observable again. An internal test, because paginatingRows and its
// execState are package-private.

import (
	"context"
	"database/sql/driver"
	"fmt"
	"io"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func TestExecuteStateIsStatementScopedInTx(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newSimConnection(t, 71)
	for i := 0; i < 10; i++ {
		if _, err := c.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", i, i), nil); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	c.SetOptions(api.NewOptionsBuilder().
		Set(api.OptMaxStatementMemoryBytes, int64(1_000_000)).Build())

	if _, err := c.Begin(); err != nil {
		t.Fatalf("begin: %v", err)
	}

	runSorted := func(stmt int) *paginatingRows {
		t.Helper()
		rows, err := c.QueryContext(ctx, "SELECT id FROM t ORDER BY v, id", nil)
		if err != nil {
			t.Fatalf("statement %d: %v", stmt, err)
		}
		pr, ok := rows.(*paginatingRows)
		if !ok {
			t.Fatalf("statement %d returned %T, want *paginatingRows", stmt, rows)
		}
		dest := make([]driver.Value, 1)
		for {
			if err := pr.Next(dest); err != nil {
				if err == io.EOF {
					break
				}
				t.Fatalf("statement %d iteration: %v", stmt, err)
			}
		}
		pr.Close() //nolint:errcheck
		return pr
	}

	pr1 := runSorted(1)

	// The negative-result pin: after a fully drained and closed statement,
	// every RFC-130 charge has been released. THIS fact is why a
	// transaction-scoped ExecuteState would be silent for sequential
	// statements today. If it ever fails, an operator started holding memory
	// past its page — at that moment the statement-scoped requirement below
	// stops being merely structural and becomes load-bearing at the SQL
	// level, and the e2e companion in sqldriver/sim_sql_tx_budget_test.go
	// needs a real cross-statement failure case.
	if used := pr1.execState.MemUsed(); used != 0 {
		t.Fatalf("statement 1 finished with %d bytes still charged: an operator now holds "+
			"RFC-130 memory past its page, so a shared ExecuteState is no longer silent — "+
			"re-arm the cross-statement memory tests (RFC-198 Decision 5)", used)
	}

	pr2 := runSorted(2)

	// The structural split: each statement in the transaction mints its own
	// ExecuteState. Dragging props.State onto the embeddedTx the way
	// ScanState is held makes these the same pointer.
	if pr1.execState == pr2.execState {
		t.Fatalf("two statements in one explicit transaction share one ExecuteState: " +
			"the RFC-130 memory budget must stay STATEMENT-scoped while the scan budget " +
			"is transaction-scoped (RFC-198 Decision 5)")
	}
}
