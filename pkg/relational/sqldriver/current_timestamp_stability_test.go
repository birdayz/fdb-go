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
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/sqldriver"
	"fdb.dev/pkg/simfdb"
)

// minStraddleExecs is the sample floor the boundary-straddle detector needs:
// per-row wall-clock evaluation only reveals itself when a statement happens to
// span a second boundary, so a single execution proves very little and the loop
// has to run several.
//
// It is a LOOP GUARD, deliberately, and not an assertion after a purely
// time-boxed loop. As an assertion it stated a THROUGHPUT claim — "this machine
// can scan 10k rows at least three times in three seconds" — which is a
// property of the load on the host, not of the code under test. These tests run
// t.Parallel() alongside the rest of the suite and, under `just test`, beside up
// to four concurrent FDB containers; one scan there can outlast the whole
// window, and the loop would exit having sampled once and then fail with "only
// 1 executions in the 3s window". Observed exactly that way. Bounding the loop
// by BOTH the sample floor and the window keeps the full 3 seconds of
// straddle-chances on an idle machine while still collecting the samples the
// argument rests on when the host is busy.
const minStraddleExecs = 3

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

	// Keep the full 10k-row semantic population; only fixture setup may retry.
	const total = 10000
	seedCurrentTimestampItems(t, db, total, nil)

	deadline := time.Now().Add(3 * time.Second)
	var firstTS, lastTS string
	execs := 0
	for execs < minStraddleExecs || time.Now().Before(deadline) {
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

	const total = 10000
	seedCurrentTimestampItems(t, db, total, nil)

	deadline := time.Now().Add(3 * time.Second)
	execs := 0
	for execs < minStraddleExecs || time.Now().Before(deadline) {
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
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// seedCurrentTimestampItems bounds setup work per MVCC window and uses the
// existing whole-transaction retry only for definite time-limit failures. SQL
// autocommit remains single-shot; unknown completion is never replayed here.
func seedCurrentTimestampItems(t *testing.T, db *sql.DB, total int, onRetry func(int, error)) {
	t.Helper()
	ctx := context.Background()
	const batch = 100
	for lo := 0; lo < total; lo += batch {
		var sb strings.Builder
		sb.WriteString("INSERT INTO Item VALUES ")
		for i := 0; i < min(batch, total-lo); i++ {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "(%d)", lo+i)
		}
		retryTx(t, db, txRetryOpts{OnRetry: onRetry}, func(a txAttempt) error {
			if _, err := a.tx.ExecContext(ctx, sb.String()); err != nil {
				return fmt.Errorf("seed INSERT batch at %d: %w", lo, err)
			}
			return a.tx.Commit()
		})
	}
}

func TestCurrentTimestampSeedRetriesExpiredTransaction(t *testing.T) {
	t.Parallel()
	env := dst.NewSim(256)
	env.Buggify = dst.DisabledBuggifier()
	sim := simfdb.New(env)
	backend := recordlayer.NewFDBDatabaseWithBackend(sim).SetEnv(env)
	backend.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	key := "sim://" + t.Name()
	t.Cleanup(sqldriver.RegisterBackend(key, backend))
	setup := openSpiked(t, key, "/timestamp_seed", "")
	ctx := context.Background()
	for _, ddl := range []string{
		"CREATE DATABASE /timestamp_seed",
		"CREATE SCHEMA TEMPLATE timestamp_seed_tmpl CREATE TABLE Item (id BIGINT, PRIMARY KEY (id))",
		"CREATE SCHEMA /timestamp_seed/s WITH TEMPLATE timestamp_seed_tmpl",
	} {
		if _, err := setup.ExecContext(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	db := openSpiked(t, key, "/timestamp_seed", "s")
	db.SetMaxOpenConns(1)
	// Initialize the query connection before arming the DML fault. Catalog
	// bootstrap has its own idempotent transaction and must not consume it.
	var initial int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM Item").Scan(&initial); err != nil || initial != 0 {
		t.Fatalf("initial population: %d, %v", initial, err)
	}
	sim.InjectOnce(1007)
	retries := 0
	const total = 1000
	seedCurrentTimestampItems(t, db, total, func(attempt int, err error) {
		retries++
		if attempt != 1 || !api.IsTransactionTimeLimit(err) {
			t.Fatalf("unexpected setup retry: attempt=%d err=%v", attempt, err)
		}
	})
	if retries != 1 {
		t.Fatalf("injected expiry caused %d caller-owned retries, want 1", retries)
	}
	rows, err := db.QueryContext(ctx, "SELECT id FROM Item ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil || id != int64(n) {
			t.Fatalf("seed row %d: id=%d err=%v", n, id, err)
		}
		n++
	}
	if err := rows.Err(); err != nil || n != total {
		t.Fatalf("seed population after expiry/retry: rows=%d want=%d err=%v", n, total, err)
	}
}
