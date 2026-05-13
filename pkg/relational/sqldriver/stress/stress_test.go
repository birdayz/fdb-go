package stress_test

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/birdayz/fdb-record-layer-go/pkg/relational/sqldriver"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

var clusterFilePath string

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "stress: no Docker, skipping\n")
		os.Exit(0)
	}
	defer container.Terminate(context.Background()) //nolint:errcheck

	clusterContent, err := container.ClusterFile(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ClusterFile: %v\n", err)
		os.Exit(1)
	}

	tmp, err := os.CreateTemp("", "fdb-stress-*.cluster")
	if err != nil {
		fmt.Fprintf(os.Stderr, "CreateTemp: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(clusterContent); err != nil {
		fmt.Fprintf(os.Stderr, "WriteString: %v\n", err)
		os.Exit(1)
	}
	tmp.Close()
	clusterFilePath = tmp.Name()

	os.Exit(m.Run())
}

type stressHarness struct {
	t         *testing.T
	db        *sql.DB
	dbPath    string
	schema    string
	batchSize int
}

func newStressHarness(t *testing.T, suffix string) *stressHarness {
	t.Helper()
	dbPath := "/stress_" + suffix
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sysDSN := fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", clusterFilePath)
	sysDB, err := sql.Open("fdbsql", sysDSN)
	if err != nil {
		t.Fatalf("sql.Open __SYS: %v", err)
	}
	defer sysDB.Close()

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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sysDSN := fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", clusterFilePath)
	sysDB, err := sql.Open("fdbsql", sysDSN)
	if err != nil {
		h.t.Fatalf("sql.Open __SYS: %v", err)
	}
	defer sysDB.Close()

	tmplName := "stress_tmpl_" + strings.ReplaceAll(h.dbPath, "/", "")
	if _, err := sysDB.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmplName+" "+template); err != nil {
		h.t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := sysDB.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s", h.dbPath, h.schema, tmplName)); err != nil {
		h.t.Fatalf("CREATE SCHEMA: %v", err)
	}
}

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

	return queryResult{Query: query, Duration: time.Since(start), RowCount: count}
}

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

func TestFDB_Stress_10K(t *testing.T) {
	t.Parallel()
	runStressSuite(t, "10k", 10_000)
}

func TestFDB_Stress_100K(t *testing.T) {
	t.Parallel()
	runStressSuite(t, "100k", 100_000)
}

func runStressSuite(t *testing.T, suffix string, n int) {
	h := newStressHarness(t, suffix)

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

	h.bulkInsert("customers", numCustomers, func(i int) string {
		return fmt.Sprintf("(%d, 'customer_%d', '%s')", i, i, tiers[i%len(tiers)])
	})
	h.bulkInsert("orders", n, func(i int) string {
		custID := rng.Intn(numCustomers)
		amount := rng.Intn(10000) + 1
		status := statuses[i%len(statuses)]
		return fmt.Sprintf("(%d, %d, %d, '%s')", i, custID, amount, status)
	})

	t.Run("pk_lookup_first", func(t *testing.T) {
		r := h.timeQuery("SELECT * FROM orders WHERE id = 0")
		r.expectRows(t, "PK lookup id=0", 1)
		if r.Duration > 2*time.Second {
			t.Errorf("PK lookup took %v — expected O(1)", r.Duration)
		}
	})
	t.Run("pk_lookup_middle", func(t *testing.T) {
		r := h.timeQuery("SELECT * FROM orders WHERE id = ?", n/2)
		r.expectRows(t, "PK lookup id=N/2", 1)
	})
	t.Run("pk_lookup_last", func(t *testing.T) {
		r := h.timeQuery("SELECT * FROM orders WHERE id = ?", n-1)
		r.expectRows(t, "PK lookup id=N-1", 1)
	})

	t.Run("index_customer_eq", func(t *testing.T) {
		r := h.timeQuery("SELECT id, amount FROM orders WHERE customer_id = 0")
		r.mustSucceed(t, "idx_customer eq")
	})
	t.Run("index_amount_range", func(t *testing.T) {
		r := h.timeQuery("SELECT id FROM orders WHERE amount > 9000")
		r.mustSucceed(t, "idx_amount range >9000")
	})
	t.Run("index_status_count", func(t *testing.T) {
		r := h.timeQuery("SELECT COUNT(*) FROM orders WHERE status = 'pending'")
		r.expectRows(t, "idx_status count pending", 1)
	})

	t.Run("full_scan_count", func(t *testing.T) {
		ctx := context.Background()
		var count int64
		err := h.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM orders").Scan(&count)
		if err != nil {
			t.Fatalf("COUNT(*): %v", err)
		}
		t.Logf("COUNT(*) = %d (expected %d)", count, n)
		if count != int64(n) {
			t.Errorf("COUNT(*) = %d, want %d — inserts may have failed", count, n)
		}
	})
	t.Run("full_scan_filter", func(t *testing.T) {
		r := h.timeQuery("SELECT COUNT(*) FROM orders WHERE amount > 5000")
		r.mustSucceed(t, "full scan filter amount>5000")
	})

	t.Run("group_by_status", func(t *testing.T) {
		r := h.timeQuery("SELECT status, COUNT(*), SUM(amount) FROM orders GROUP BY status ORDER BY status")
		r.mustSucceed(t, "GROUP BY status")
		if r.RowCount != len(statuses) {
			t.Errorf("got %d groups, want %d (possible FDB timeout truncation)", r.RowCount, len(statuses))
		}
	})
	t.Run("group_by_customer_having", func(t *testing.T) {
		r := h.timeQuery("SELECT customer_id, SUM(amount) FROM orders GROUP BY customer_id HAVING SUM(amount) > 50000 ORDER BY customer_id")
		r.mustSucceed(t, "GROUP BY customer HAVING")
	})

	t.Run("join_10_outer", func(t *testing.T) {
		r := h.timeQuery("SELECT o.id, c.name FROM orders o, customers c WHERE o.customer_id = c.id AND o.id < 10 ORDER BY o.id")
		r.mustSucceed(t, "JOIN 10 orders x customers")
		if r.RowCount < 1 {
			t.Error("JOIN returned 0 rows")
		}
	})
	if n <= 10_000 {
		t.Run("join_100_outer", func(t *testing.T) {
			r := h.timeQuery("SELECT o.id, c.name FROM orders o, customers c WHERE o.customer_id = c.id AND o.id < 100 ORDER BY o.id")
			r.mustSucceed(t, "JOIN 100 orders x customers")
		})
		t.Run("join_filtered_both", func(t *testing.T) {
			r := h.timeQuery("SELECT COUNT(*) FROM orders o, customers c WHERE o.customer_id = c.id AND c.tier = 'gold' AND o.status = 'pending'")
			r.mustSucceed(t, "JOIN filtered both sides")
		})
	}

	t.Run("order_by_pk_full", func(t *testing.T) {
		r := h.timeQuery("SELECT id FROM orders ORDER BY id")
		r.mustSucceed(t, "ORDER BY PK (full)")
		if r.RowCount != n {
			t.Errorf("got %d rows, want %d (silent truncation?)", r.RowCount, n)
		}
	})
	t.Run("order_by_pk_index_filter", func(t *testing.T) {
		r := h.timeQuery("SELECT id, amount FROM orders WHERE customer_id = 0 ORDER BY id")
		r.mustSucceed(t, "ORDER BY PK + index filter")
	})

	t.Run("scan_all_narrow", func(t *testing.T) {
		r := h.timeQuery("SELECT id FROM orders ORDER BY id")
		r.mustSucceed(t, "scan all rows ordered")
		if r.RowCount != n {
			t.Errorf("got %d rows, want %d (silent truncation)", r.RowCount, n)
		}
	})
	t.Run("scan_all_wide", func(t *testing.T) {
		r := h.timeQuery("SELECT id, customer_id, amount, status FROM orders ORDER BY id")
		r.mustSucceed(t, "scan all rows wide")
		if r.RowCount != n {
			t.Errorf("got %d rows, want %d", r.RowCount, n)
		}
	})

	t.Run("exists_subquery", func(t *testing.T) {
		if n <= 10_000 {
			r := h.timeQuery("SELECT id FROM customers c WHERE EXISTS (SELECT 1 FROM orders o WHERE o.customer_id = c.id AND o.status = 'pending') ORDER BY id")
			r.mustSucceed(t, "EXISTS subquery")
		} else {
			r := h.timeQuery("SELECT id FROM customers c WHERE EXISTS (SELECT 1 FROM orders o WHERE o.customer_id = c.id AND o.status = 'pending') AND c.id < 10 ORDER BY id")
			r.mustSucceed(t, "EXISTS subquery (limited)")
		}
	})

	t.Run("in_list", func(t *testing.T) {
		r := h.timeQuery("SELECT id, amount FROM orders WHERE customer_id IN (0, 1, 2, 3, 4) ORDER BY id")
		r.mustSucceed(t, "IN-list 5 values")
	})

	t.Run("update_by_index", func(t *testing.T) {
		r := h.timeExec("UPDATE orders SET amount = amount + 1 WHERE customer_id = 0")
		r.mustSucceed(t, "UPDATE by index")
	})
	t.Run("delete_single_row", func(t *testing.T) {
		r := h.timeExec("DELETE FROM orders WHERE id = 0")
		r.expectRows(t, "DELETE single row", 1)
	})
}
