package sqldriver_test

// End-to-end, real-FDB benchmark of a projection over a full primary-key scan,
// the shape of the 1M stress test's "scan all rows" queries: the per-row cost
// of the projection operator, with FDB reads in the loop.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
)

func benchProjectionScan(b *testing.B, query string) {
	b.Helper()
	if clusterFilePath == "" {
		b.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	seq := benchSeq.Add(1)
	dbPath := fmt.Sprintf("/FRL/BENCH_PROJSCAN_%d_%d", os.Getpid(), seq)
	tmpl := fmt.Sprintf("bench_projscan_tmpl_%d_%d", os.Getpid(), seq)

	setup := openBenchDB(b, dbPath)
	execOrFail(b, setup, ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))
	execOrFail(b, setup, ctx, fmt.Sprintf("CREATE SCHEMA TEMPLATE %s "+
		"CREATE TABLE orders (id BIGINT, customer_id BIGINT, amount BIGINT, status STRING, PRIMARY KEY (id))", tmpl))
	execOrFail(b, setup, ctx, fmt.Sprintf("CREATE SCHEMA %s/STORE WITH TEMPLATE %s", dbPath, tmpl))

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=STORE", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		b.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	const rows = 20000
	statuses := []string{"pending", "shipped", "delivered", "cancelled"}
	for start := 0; start < rows; start += 1000 {
		var values strings.Builder
		for id := start; id < start+1000; id++ {
			if id > start {
				values.WriteByte(',')
			}
			fmt.Fprintf(&values, "(%d,%d,%d,'%s')", id, id%1000, id%10000, statuses[id%4])
		}
		execOrFail(b, db, ctx, "INSERT INTO orders VALUES "+values.String())
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, err := db.QueryContext(ctx, query)
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
		cols, err := r.Columns()
		if err != nil {
			b.Fatalf("columns: %v", err)
		}
		dest := make([]any, len(cols))
		for j := range dest {
			dest[j] = new(any)
		}
		n := 0
		for r.Next() {
			if err := r.Scan(dest...); err != nil {
				b.Fatalf("scan: %v", err)
			}
			n++
		}
		r.Close()
		if n != rows {
			b.Fatalf("iteration %d: got %d rows, want %d", i, n, rows)
		}
	}
	b.StopTimer()
	if secs := b.Elapsed().Seconds(); secs > 0 {
		b.ReportMetric(float64(b.N)*rows/secs, "rows/sec")
	}
}

func BenchmarkFDB_ProjectionScan_Narrow(b *testing.B) {
	benchProjectionScan(b, "SELECT id FROM orders ORDER BY id")
}

func BenchmarkFDB_ProjectionScan_Wide(b *testing.B) {
	benchProjectionScan(b, "SELECT id, customer_id, amount, status FROM orders ORDER BY id")
}

func BenchmarkFDB_ProjectionScan_Star(b *testing.B) {
	benchProjectionScan(b, "SELECT * FROM orders ORDER BY id")
}
