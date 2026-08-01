package sqldriver_test

// CURRENT_TIMESTAMP is STATEMENT-scoped (SQL standard): every reference
// within one statement observes the same instant. Java (4.12.11.0) does
// not implement CURRENT_TIMESTAMP at all — the token parses
// (RelationalParser.g4:1009-1012) into an inert visitor stub
// (BaseVisitor.java:1432 visitSimpleFunctionCall → visitChildren) — so
// this is a Go read-side extension; its statement scoping follows Java's
// idiom for per-execution constants (QueryExecutionContext.java:34-43
// builds the EvaluationContext's constant bindings ONCE per execution
// and ConstantObjectValue.eval reads that fixed slot per row).
//
// The SELECT path historically drifted: each row's projection evaluated
// against a bare positional row with no statement clock, so
// CURRENT_TIMESTAMP fell back to per-row time.Now(). The fix stamps the
// session's statement instant on the executor EvaluationContext
// (WithStatementTime, cascades_generator.go) and wraps the frontier row
// in a clock-bearing RowEvalContext whenever the operator's values
// reference the CURRENT_TIMESTAMP family (values.DependsOnStatementClock).

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestFDB_CurrentTimestamp_StatementStable_Select scans a table large
// enough that per-row evaluation is spread over tens of milliseconds,
// repeatedly for ~3 wall-clock seconds. CURRENT_TIMESTAMP formats at
// SECOND precision, so per-row evaluation WOULD drift whenever a scan
// straddles a second boundary — and in a 3s loop of back-to-back scans,
// several boundaries necessarily fall inside some scan's evaluation
// window. Statement-stable evaluation returns ONE distinct timestamp
// per statement in every execution.
func TestFDB_CurrentTimestamp_StatementStable_Select(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_cts_select", "cts_select",
		"CREATE TABLE Item (id BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()

	// 10k rows, batched 1000 per INSERT.
	const total, batch = 10000, 1000
	for lo := 0; lo < total; lo += batch {
		var sb strings.Builder
		sb.WriteString("INSERT INTO Item VALUES ")
		for i := 0; i < batch; i++ {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "(%d)", lo+i)
		}
		if _, err := db.ExecContext(ctx, sb.String()); err != nil {
			t.Fatalf("seed INSERT batch at %d: %v", lo, err)
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	var firstTS, lastTS string
	execs := 0
	for execs == 0 || time.Now().Before(deadline) {
		rows, err := db.QueryContext(ctx, "SELECT id, CURRENT_TIMESTAMP FROM Item")
		if err != nil {
			t.Fatalf("SELECT: %v", err)
		}
		distinct := map[string]bool{}
		n := 0
		for rows.Next() {
			var id int64
			var ts string
			if err := rows.Scan(&id, &ts); err != nil {
				rows.Close()
				t.Fatalf("scan: %v", err)
			}
			distinct[ts] = true
			n++
		}
		rerr := rows.Err()
		rows.Close()
		if rerr != nil {
			t.Fatalf("rows: %v", rerr)
		}
		if n != total {
			t.Fatalf("row count = %d, want %d", n, total)
		}
		if len(distinct) != 1 {
			t.Fatalf("CURRENT_TIMESTAMP drifted within ONE statement: %d distinct values %v (SQL fixes it per statement)", len(distinct), keysOf(distinct))
		}
		for ts := range distinct {
			if firstTS == "" {
				firstTS = ts
			}
			lastTS = ts
		}
		execs++
	}
	if execs < 3 {
		t.Fatalf("only %d executions in the 3s window — the scan is too slow for the boundary-straddle argument to hold; shrink the table or lengthen the window", execs)
	}
	// Cross-statement control: the loop spans ~3 seconds at second
	// precision, so the FIRST and LAST statements must observe different
	// instants — the clock is statement-scoped, not frozen per session.
	if firstTS == lastTS {
		t.Fatalf("cross-statement control: first and last statement (~3s apart) returned the same CURRENT_TIMESTAMP %q — the clock is frozen beyond statement scope", firstTS)
	}
}

// TestFDB_CurrentTimestamp_StatementStable_Where pins the WHERE plan
// shape (the predicates-filter frontier) with the same boundary-straddle
// detector: the query flows through executePredicatesFilter /
// executeFilter — whose clock-need probe walks the predicate's embedded
// values — and the projected CURRENT_TIMESTAMP must stay uniform per
// statement while `WHERE CURRENT_TIMESTAMP = CURRENT_TIMESTAMP` keeps
// every row (a statement-stable clock can never make the two references
// disagree; per-row wall-clock evaluation can, exactly at a second
// boundary between the two operand evaluations).
func TestFDB_CurrentTimestamp_StatementStable_Where(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_cts_where", "cts_where",
		"CREATE TABLE Item (id BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()

	const total, batch = 10000, 1000
	for lo := 0; lo < total; lo += batch {
		var sb strings.Builder
		sb.WriteString("INSERT INTO Item VALUES ")
		for i := 0; i < batch; i++ {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "(%d)", lo+i)
		}
		if _, err := db.ExecContext(ctx, sb.String()); err != nil {
			t.Fatalf("seed INSERT batch at %d: %v", lo, err)
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	execs := 0
	for execs == 0 || time.Now().Before(deadline) {
		rows, err := db.QueryContext(ctx,
			"SELECT id, CURRENT_TIMESTAMP FROM Item WHERE CURRENT_TIMESTAMP = CURRENT_TIMESTAMP")
		if err != nil {
			t.Fatalf("SELECT with CURRENT_TIMESTAMP predicate: %v", err)
		}
		distinct := map[string]bool{}
		n := 0
		for rows.Next() {
			var id int64
			var ts string
			if err := rows.Scan(&id, &ts); err != nil {
				rows.Close()
				t.Fatalf("scan: %v", err)
			}
			distinct[ts] = true
			n++
		}
		rerr := rows.Err()
		rows.Close()
		if rerr != nil {
			t.Fatalf("rows: %v", rerr)
		}
		if n != total {
			t.Fatalf("WHERE CURRENT_TIMESTAMP = CURRENT_TIMESTAMP kept %d rows, want %d — the two references in one statement disagreed", n, total)
		}
		if len(distinct) != 1 {
			t.Fatalf("projected CURRENT_TIMESTAMP drifted within ONE statement under a clocked WHERE: %d distinct values %v", len(distinct), keysOf(distinct))
		}
		execs++
	}
	if execs < 3 {
		t.Fatalf("only %d executions in the 3s window — the scan is too slow for the boundary-straddle argument to hold; shrink the table or lengthen the window", execs)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
