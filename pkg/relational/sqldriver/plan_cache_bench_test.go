package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"
)

// benchSeq provides unique database paths across benchmark invocations
// within the same process (Go's benchmark harness calls the function body
// multiple times to calibrate b.N).
var benchSeq atomic.Int64

// BenchmarkFDB_PlanCacheHit measures end-to-end query execution with the plan
// cache warm. The first iteration triggers Cascades planning (cache miss); all
// subsequent iterations hit the cache. With b.N typically in the thousands, the
// amortised cost converges to the cached path -- parse + hash + cache lookup +
// FDB scan -- without the Cascades optimiser in the loop.
//
// Compare with BenchmarkFDB_PlanCacheMiss (which defeats the cache each
// iteration) to see the raw planning overhead that the cache eliminates.
func BenchmarkFDB_PlanCacheHit(b *testing.B) {
	if clusterFilePath == "" {
		b.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	seq := benchSeq.Add(1)
	dbPath := fmt.Sprintf("/bench_pc_hit_%d", seq)
	tmpl := fmt.Sprintf("bph_tmpl_%d", seq)

	setup := openBenchDB(b, dbPath)
	execOrFail(b, setup, ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))
	execOrFail(b, setup, ctx,
		fmt.Sprintf("CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, name STRING, price BIGINT, PRIMARY KEY (item_id))", tmpl))
	execOrFail(b, setup, ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl))

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=store", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		b.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// Seed rows so the scan does real work.
	for i := int64(1); i <= 10; i++ {
		execOrFail(b, db, ctx, fmt.Sprintf("INSERT INTO Item VALUES (%d, 'item_%d', %d)", i, i, i*100))
	}

	query := "SELECT item_id, name, price FROM Item WHERE item_id = 1"

	// Warm the cache with one execution.
	warmRows, err := db.QueryContext(ctx, query)
	if err != nil {
		b.Fatalf("warm-up query: %v", err)
	}
	warmRows.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
		for rows.Next() {
			var id, price int64
			var name string
			if err := rows.Scan(&id, &name, &price); err != nil {
				b.Fatalf("scan: %v", err)
			}
		}
		rows.Close()
	}
}

// BenchmarkFDB_PlanCacheMiss measures end-to-end query execution with the plan
// cache defeated. Each iteration uses a unique SQL string (via a literal
// integer in the WHERE clause) so the query hash never matches a cached entry.
// This forces the full Cascades planning pipeline on every iteration, giving a
// baseline to compare against BenchmarkFDB_PlanCacheHit.
func BenchmarkFDB_PlanCacheMiss(b *testing.B) {
	if clusterFilePath == "" {
		b.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	seq := benchSeq.Add(1)
	dbPath := fmt.Sprintf("/bench_pc_miss_%d", seq)
	tmpl := fmt.Sprintf("bpm_tmpl_%d", seq)

	setup := openBenchDB(b, dbPath)
	execOrFail(b, setup, ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))
	execOrFail(b, setup, ctx,
		fmt.Sprintf("CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, name STRING, price BIGINT, PRIMARY KEY (item_id))", tmpl))
	execOrFail(b, setup, ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl))

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=store", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		b.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	for i := int64(1); i <= 10; i++ {
		execOrFail(b, db, ctx, fmt.Sprintf("INSERT INTO Item VALUES (%d, 'item_%d', %d)", i, i, i*100))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Unique SQL text each iteration -- cache never hits.
		q := fmt.Sprintf("SELECT item_id, name, price FROM Item WHERE item_id = %d", (i%10)+1)
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
		for rows.Next() {
			var id, price int64
			var name string
			if err := rows.Scan(&id, &name, &price); err != nil {
				b.Fatalf("scan: %v", err)
			}
		}
		rows.Close()
	}
}

// openBenchDB is the benchmark equivalent of openTestDB.
func openBenchDB(b *testing.B, dbPath string) *sql.DB {
	b.Helper()
	if clusterFilePath == "" {
		b.Skip("FDB not available (no Docker)")
	}
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		b.Fatalf("sql.Open: %v", err)
	}
	b.Cleanup(func() { db.Close() })
	return db
}

func execOrFail(b *testing.B, db *sql.DB, ctx context.Context, sql string) {
	b.Helper()
	if _, err := db.ExecContext(ctx, sql); err != nil {
		b.Fatalf("exec %q: %v", sql, err)
	}
}

// BenchmarkFDB_TimestampInsert measures INSERT throughput into a table with a
// TIMESTAMP column and a secondary index on that column.
func BenchmarkFDB_TimestampInsert(b *testing.B) {
	if clusterFilePath == "" {
		b.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	seq := benchSeq.Add(1)
	dbPath := fmt.Sprintf("/bench_ts_ins_%d", seq)
	tmpl := fmt.Sprintf("bench_ts_insert_tmpl_%d", seq)

	setup := openBenchDB(b, dbPath)
	execOrFail(b, setup, ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))
	execOrFail(b, setup, ctx,
		fmt.Sprintf("CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Events (id BIGINT NOT NULL, ts TIMESTAMP, label STRING, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_events_ts ON Events (ts)", tmpl))
	execOrFail(b, setup, ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl))

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=store", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		b.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ts := fmt.Sprintf("2025-01-01 00:00:%02d", i%60)
		_, err := db.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO Events VALUES (%d, '%s', 'evt_%d')", i+1, ts, i+1))
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
	}
}

// BenchmarkFDB_TimestampRangeScan measures SELECT COUNT(*) with a TIMESTAMP
// range predicate on a pre-populated table with a secondary index on the
// timestamp column.
func BenchmarkFDB_TimestampRangeScan(b *testing.B) {
	if clusterFilePath == "" {
		b.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	seq := benchSeq.Add(1)
	dbPath := fmt.Sprintf("/bench_ts_range_%d", seq)
	tmpl := fmt.Sprintf("bench_ts_range_tmpl_%d", seq)

	setup := openBenchDB(b, dbPath)
	execOrFail(b, setup, ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))
	execOrFail(b, setup, ctx,
		fmt.Sprintf("CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Events (id BIGINT NOT NULL, ts TIMESTAMP, label STRING, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_events_ts ON Events (ts)", tmpl))
	execOrFail(b, setup, ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl))

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=store", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		b.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// Pre-populate with 100 rows.
	for i := int64(1); i <= 100; i++ {
		ts := fmt.Sprintf("2025-06-%02d 12:00:00", (i%28)+1)
		execOrFail(b, db, ctx,
			fmt.Sprintf("INSERT INTO Events VALUES (%d, '%s', 'evt_%d')", i, ts, i))
	}

	query := "SELECT COUNT(*) FROM Events WHERE ts > '2025-06-14 00:00:00'"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
		for rows.Next() {
			var cnt int64
			if err := rows.Scan(&cnt); err != nil {
				b.Fatalf("scan: %v", err)
			}
		}
		rows.Close()
	}
}

// BenchmarkFDB_JoinQuery measures an inner join between two tables (Customers
// and Orders) with a secondary index on the foreign key column. The join is
// filtered to a single customer, returning 5 matching orders per iteration.
func BenchmarkFDB_JoinQuery(b *testing.B) {
	if clusterFilePath == "" {
		b.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	seq := benchSeq.Add(1)
	dbPath := fmt.Sprintf("/bench_join_%d", seq)
	tmpl := fmt.Sprintf("bench_join_tmpl_%d", seq)

	setup := openBenchDB(b, dbPath)
	execOrFail(b, setup, ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))
	execOrFail(b, setup, ctx,
		fmt.Sprintf("CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Customers (id BIGINT NOT NULL, name STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE Orders (id BIGINT NOT NULL, cust_id BIGINT, amount BIGINT, PRIMARY KEY(id)) "+
			"CREATE INDEX idx_orders_cust ON Orders (cust_id)", tmpl))
	execOrFail(b, setup, ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl))

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=store", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		b.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// Seed 10 customers.
	for i := int64(1); i <= 10; i++ {
		execOrFail(b, db, ctx,
			fmt.Sprintf("INSERT INTO Customers VALUES (%d, 'customer_%d')", i, i))
	}
	// Seed 50 orders (5 per customer).
	for i := int64(1); i <= 50; i++ {
		custID := ((i - 1) % 10) + 1
		execOrFail(b, db, ctx,
			fmt.Sprintf("INSERT INTO Orders VALUES (%d, %d, %d)", i, custID, i*10))
	}

	query := "SELECT c.name, o.amount FROM Customers c, Orders o WHERE c.id = o.cust_id AND c.id = 1"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
		for rows.Next() {
			var name string
			var amount int64
			if err := rows.Scan(&name, &amount); err != nil {
				b.Fatalf("scan: %v", err)
			}
		}
		rows.Close()
	}
}

// BenchmarkFDB_AggregateGroupBy measures a GROUP BY query with COUNT(*) and
// SUM(amount) aggregates over a secondary-indexed category column. The table
// contains 100 rows spread across 5 categories.
func BenchmarkFDB_AggregateGroupBy(b *testing.B) {
	if clusterFilePath == "" {
		b.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	seq := benchSeq.Add(1)
	dbPath := fmt.Sprintf("/bench_agg_%d", seq)
	tmpl := fmt.Sprintf("bench_agg_tmpl_%d", seq)

	setup := openBenchDB(b, dbPath)
	execOrFail(b, setup, ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))
	execOrFail(b, setup, ctx,
		fmt.Sprintf("CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Sales (id BIGINT NOT NULL, category STRING, amount BIGINT, PRIMARY KEY(id)) "+
			"CREATE INDEX idx_sales_cat ON Sales (category)", tmpl))
	execOrFail(b, setup, ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl))

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=store", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		b.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// Seed 100 rows across 5 categories.
	categories := []string{"electronics", "clothing", "food", "books", "toys"}
	for i := int64(1); i <= 100; i++ {
		cat := categories[(i-1)%5]
		execOrFail(b, db, ctx,
			fmt.Sprintf("INSERT INTO Sales VALUES (%d, '%s', %d)", i, cat, i*5))
	}

	query := "SELECT category, COUNT(*), SUM(amount) FROM Sales GROUP BY category"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
		for rows.Next() {
			var cat string
			var cnt, total int64
			if err := rows.Scan(&cat, &cnt, &total); err != nil {
				b.Fatalf("scan: %v", err)
			}
		}
		rows.Close()
	}
}

// BenchmarkFDB_IndexScanRange measures a secondary index range scan that
// returns roughly half the rows. The Products table has 100 rows with prices
// 1..100 and a secondary index on price; the query selects all products with
// price > 50.
func BenchmarkFDB_IndexScanRange(b *testing.B) {
	if clusterFilePath == "" {
		b.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	seq := benchSeq.Add(1)
	dbPath := fmt.Sprintf("/bench_idxrange_%d", seq)
	tmpl := fmt.Sprintf("bench_idxrange_tmpl_%d", seq)

	setup := openBenchDB(b, dbPath)
	execOrFail(b, setup, ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))
	execOrFail(b, setup, ctx,
		fmt.Sprintf("CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Products (id BIGINT NOT NULL, price BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE INDEX idx_products_price ON Products (price)", tmpl))
	execOrFail(b, setup, ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl))

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=store", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		b.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// Seed 100 products with prices 1..100.
	for i := int64(1); i <= 100; i++ {
		execOrFail(b, db, ctx,
			fmt.Sprintf("INSERT INTO Products VALUES (%d, %d, 'product_%d')", i, i, i))
	}

	query := "SELECT id, name FROM Products WHERE price > 50"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				b.Fatalf("scan: %v", err)
			}
		}
		rows.Close()
	}
}
