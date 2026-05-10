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
