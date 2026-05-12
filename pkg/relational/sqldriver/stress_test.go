package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// stressHarness manages setup/teardown for large-table stress tests.
// It handles batched INSERT to stay within FDB's 5s transaction limit
// and provides helpers for timing queries.
type stressHarness struct {
	t         *testing.T
	db        *sql.DB
	dbPath    string
	schema    string
	batchSize int
}

func newStressHarness(t *testing.T, suffix string) *stressHarness {
	t.Helper()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	dbPath := "/stress_" + suffix
	ctx := context.Background()

	sysDB := openTestDB(t, "/__SYS")
	if _, err := sysDB.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=main", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return &stressHarness{
		t:         t,
		db:        db,
		dbPath:    dbPath,
		schema:    "main",
		batchSize: 500,
	}
}

func (h *stressHarness) createSchema(template string) {
	h.t.Helper()
	ctx := context.Background()
	sysDB := openTestDB(h.t, "/__SYS")

	tmplName := "stress_tmpl_" + strings.ReplaceAll(h.dbPath, "/", "")
	if _, err := sysDB.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmplName+" "+template); err != nil {
		h.t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := sysDB.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s", h.dbPath, h.schema, tmplName)); err != nil {
		h.t.Fatalf("CREATE SCHEMA: %v", err)
	}
}

// bulkInsert inserts n rows using batched multi-row INSERT VALUES.
// Each batch is a separate transaction to stay within FDB's 5s limit.
// genRow(i) returns the VALUES clause for row i (e.g., "(1, 'foo', 42)").
func (h *stressHarness) bulkInsert(table string, n int, genRow func(i int) string) time.Duration {
	h.t.Helper()
	ctx := context.Background()
	start := time.Now()

	for offset := 0; offset < n; offset += h.batchSize {
		end := offset + h.batchSize
		if end > n {
			end = n
		}
		var rows []string
		for i := offset; i < end; i++ {
			rows = append(rows, genRow(i))
		}
		stmt := fmt.Sprintf("INSERT INTO %s VALUES %s", table, strings.Join(rows, ", "))
		if _, err := h.db.ExecContext(ctx, stmt); err != nil {
			h.t.Fatalf("INSERT batch [%d..%d): %v", offset, end, err)
		}
	}
	elapsed := time.Since(start)
	h.t.Logf("bulkInsert %s: %d rows in %v (%.0f rows/s)", table, n, elapsed, float64(n)/elapsed.Seconds())
	return elapsed
}

type queryResult struct {
	Query    string
	Duration time.Duration
	RowCount int
	Err      error
}

// timeQuery runs a SELECT query and returns timing + row count.
func (h *stressHarness) timeQuery(query string, args ...any) queryResult {
	ctx := context.Background()
	start := time.Now()

	rows, err := h.db.QueryContext(ctx, query, args...)
	if err != nil {
		return queryResult{Query: query, Duration: time.Since(start), Err: err}
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		return queryResult{Query: query, Duration: time.Since(start), RowCount: count, Err: err}
	}

	elapsed := time.Since(start)
	return queryResult{Query: query, Duration: elapsed, RowCount: count}
}

// timeExec runs a DML statement and returns timing + rows affected.
func (h *stressHarness) timeExec(stmt string, args ...any) queryResult {
	ctx := context.Background()
	start := time.Now()

	result, err := h.db.ExecContext(ctx, stmt, args...)
	elapsed := time.Since(start)
	if err != nil {
		return queryResult{Query: stmt, Duration: elapsed, Err: err}
	}
	affected, _ := result.RowsAffected()
	return queryResult{Query: stmt, Duration: elapsed, RowCount: int(affected)}
}

func (r queryResult) log(t *testing.T, label string) {
	t.Helper()
	if r.Err != nil {
		t.Logf("  %-40s  ERROR: %v (after %v)", label, r.Err, r.Duration)
	} else {
		t.Logf("  %-40s  %5d rows  %v", label, r.RowCount, r.Duration)
	}
}

func (r queryResult) mustSucceed(t *testing.T, label string) {
	t.Helper()
	r.log(t, label)
	if r.Err != nil {
		t.Fatalf("%s: %v", label, r.Err)
	}
}

func (r queryResult) expectRows(t *testing.T, label string, expected int) {
	t.Helper()
	r.mustSucceed(t, label)
	if r.RowCount != expected {
		t.Fatalf("%s: expected %d rows, got %d", label, expected, r.RowCount)
	}
}

// ---------------------------------------------------------------------------
// Stress tests
// ---------------------------------------------------------------------------

// TestFDB_Stress_10K exercises a 10,000-row table with various query
// patterns. This is the baseline — should complete in under 3 minutes.
// Run with: bazelisk test //pkg/relational/sqldriver:sqldriver_test --test_arg="--test.run=TestFDB_Stress" --test_timeout=300
func TestFDB_Stress_10K(t *testing.T) {
	if testing.Short() {
		t.Skip("stress tests skipped in short mode")
	}
	t.Parallel()
	runStressSuite(t, "10k", 10_000)
}

// TestFDB_Stress_100K exercises a 100,000-row table. This tests FDB
// transaction timeout handling (5s limit) and continuation semantics
// for large result sets. JOINs are limited to small outer sets because
// NLJ is O(N×M) with per-row FDB transactions.
func TestFDB_Stress_100K(t *testing.T) {
	if testing.Short() {
		t.Skip("stress tests skipped in short mode")
	}
	t.Parallel()
	runStressSuite(t, "100k", 100_000)
}

func runStressSuite(t *testing.T, suffix string, n int) {
	if n > 10_000 {
		t.Logf("WARNING: >10K rows exposes known scalability issues:")
		t.Logf("  - PK lookups scale O(N) instead of O(1)")
		t.Logf("  - Aggregations may silently truncate on FDB 5s timeout")
		t.Logf("  - JOINs are O(N×M) NLJ without correlated index probes")
	}
	h := newStressHarness(t, suffix)

	// Schema: two tables for JOIN tests, with and without indexes.
	h.createSchema(`
		CREATE TABLE orders (
			id BIGINT NOT NULL,
			customer_id BIGINT NOT NULL,
			amount BIGINT NOT NULL,
			status STRING NOT NULL,
			PRIMARY KEY (id)
		)
		CREATE INDEX idx_customer ON orders (customer_id)
		CREATE INDEX idx_status ON orders (status)
		CREATE INDEX idx_amount ON orders (amount)
		CREATE TABLE customers (
			id BIGINT NOT NULL,
			name STRING NOT NULL,
			tier STRING NOT NULL,
			PRIMARY KEY (id)
		)
		CREATE INDEX idx_tier ON customers (tier)
	`)

	numCustomers := n / 10
	if numCustomers < 100 {
		numCustomers = 100
	}
	tiers := []string{"gold", "silver", "bronze"}
	statuses := []string{"pending", "shipped", "delivered", "cancelled"}
	rng := rand.New(rand.NewSource(42))

	// Insert customers
	t.Log("--- SETUP ---")
	h.bulkInsert("customers", numCustomers, func(i int) string {
		return fmt.Sprintf("(%d, 'customer_%d', '%s')", i, i, tiers[i%len(tiers)])
	})

	// Insert orders
	h.bulkInsert("orders", n, func(i int) string {
		custID := rng.Intn(numCustomers)
		amount := rng.Intn(10000) + 1
		status := statuses[i%len(statuses)]
		return fmt.Sprintf("(%d, %d, %d, '%s')", i, custID, amount, status)
	})

	t.Log("--- POINT LOOKUPS (PK) ---")
	r := h.timeQuery("SELECT * FROM orders WHERE id = 0")
	r.expectRows(t, "PK lookup id=0", 1)
	if r.Duration > 2*time.Second {
		t.Errorf("PK lookup took %v — scans may be O(N) instead of O(1)", r.Duration)
	}
	r = h.timeQuery("SELECT * FROM orders WHERE id = ?", n/2)
	r.mustSucceed(t, "PK lookup id=N/2")
	r = h.timeQuery("SELECT * FROM orders WHERE id = ?", n-1)
	r.mustSucceed(t, "PK lookup id=N-1")

	t.Log("--- INDEX SCANS ---")
	// Index scan on customer_id — should be fast, ~10 rows per customer
	r = h.timeQuery("SELECT id, amount FROM orders WHERE customer_id = 0")
	r.mustSucceed(t, "idx_customer eq")

	// Index range scan on amount
	r = h.timeQuery("SELECT id FROM orders WHERE amount > 9000")
	r.mustSucceed(t, "idx_amount range >9000")

	// Index scan on status — each status has ~N/4 rows
	r = h.timeQuery("SELECT COUNT(*) FROM orders WHERE status = 'pending'")
	r.expectRows(t, "idx_status count pending", 1)

	t.Log("--- FULL TABLE SCANS ---")
	r = h.timeQuery("SELECT COUNT(*) FROM orders")
	r.expectRows(t, "full scan COUNT(*)", 1)

	// Full scan with filter on non-indexed column
	r = h.timeQuery("SELECT COUNT(*) FROM orders WHERE amount > 5000")
	r.mustSucceed(t, "full scan filter amount>5000")

	t.Log("--- AGGREGATIONS ---")
	r = h.timeQuery("SELECT status, COUNT(*), SUM(amount) FROM orders GROUP BY status ORDER BY status")
	r.mustSucceed(t, "GROUP BY status")
	if r.RowCount != len(statuses) {
		t.Errorf("GROUP BY status: got %d rows, want %d (possible FDB timeout truncation)", r.RowCount, len(statuses))
	}

	r = h.timeQuery("SELECT customer_id, SUM(amount) FROM orders GROUP BY customer_id HAVING SUM(amount) > 50000 ORDER BY customer_id")
	r.mustSucceed(t, "GROUP BY customer HAVING")

	t.Log("--- JOINS ---")
	// NLJ join: orders × customers on customer_id.
	// Known issue: NLJ does full inner scan per outer row (no correlated
	// index probes). At 10K+ this is catastrophic.
	r = h.timeQuery("SELECT o.id, c.name FROM orders o, customers c WHERE o.customer_id = c.id AND o.id < 10 ORDER BY o.id")
	r.mustSucceed(t, "JOIN 10 orders × customers")
	if r.RowCount < 1 {
		t.Error("JOIN returned 0 rows")
	}

	if n <= 10_000 {
		r = h.timeQuery("SELECT o.id, c.name FROM orders o, customers c WHERE o.customer_id = c.id AND o.id < 100 ORDER BY o.id")
		r.mustSucceed(t, "JOIN 100 orders × customers")

		r = h.timeQuery("SELECT COUNT(*) FROM orders o, customers c WHERE o.customer_id = c.id AND c.tier = 'gold' AND o.status = 'pending'")
		r.mustSucceed(t, "JOIN filtered both sides")
	} else {
		t.Log("  (skipping large JOINs at >10K — NLJ is O(N×M))")
	}

	t.Log("--- ORDER BY (index-backed) ---")
	r = h.timeQuery("SELECT id FROM orders ORDER BY id")
	r.mustSucceed(t, "ORDER BY PK (full)")
	if r.RowCount != n {
		t.Errorf("ORDER BY PK: got %d rows, want %d (silent truncation from FDB timeout?)", r.RowCount, n)
	}

	r = h.timeQuery("SELECT id, amount FROM orders WHERE customer_id = 0 ORDER BY id")
	r.mustSucceed(t, "ORDER BY PK + index filter")

	t.Log("--- LARGE RESULT SET SCANS ---")
	r = h.timeQuery("SELECT id FROM orders ORDER BY id")
	r.mustSucceed(t, "scan all rows ordered")
	if r.RowCount != n {
		t.Errorf("scan all rows: got %d rows, want %d (silent truncation)", r.RowCount, n)
	}

	r = h.timeQuery("SELECT id, customer_id, amount, status FROM orders ORDER BY id")
	r.mustSucceed(t, "scan all rows wide")
	if r.RowCount != n {
		t.Errorf("scan all rows wide: got %d rows, want %d", r.RowCount, n)
	}

	t.Log("--- EXISTS SUBQUERY ---")
	if n <= 10_000 {
		r = h.timeQuery("SELECT id FROM customers c WHERE EXISTS (SELECT 1 FROM orders o WHERE o.customer_id = c.id AND o.status = 'pending') ORDER BY id")
		r.mustSucceed(t, "EXISTS subquery")
	} else {
		r = h.timeQuery("SELECT id FROM customers c WHERE EXISTS (SELECT 1 FROM orders o WHERE o.customer_id = c.id AND o.status = 'pending') AND c.id < 10 ORDER BY id")
		r.mustSucceed(t, "EXISTS subquery (limited)")
	}

	t.Log("--- IN-LIST ---")
	r = h.timeQuery("SELECT id, amount FROM orders WHERE customer_id IN (0, 1, 2, 3, 4) ORDER BY id")
	r.mustSucceed(t, "IN-list 5 values")

	t.Log("--- DML AT SCALE ---")
	// UPDATE with index-backed WHERE
	r = h.timeExec("UPDATE orders SET amount = amount + 1 WHERE customer_id = 0")
	r.mustSucceed(t, "UPDATE by index")

	// DELETE with index-backed WHERE
	r = h.timeExec("DELETE FROM orders WHERE id = 0")
	r.expectRows(t, "DELETE single row", 1)

	t.Log("--- DONE ---")
}
