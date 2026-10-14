// Package sqlbench holds SQL benchmarks whose source is identical in every
// compared tree: the plan_cache_bench_test.go workloads of
// pkg/relational/sqltest/metamorphic (same schemas, data, statements and timed
// loops), outside that package's test binary, whose TestMain arms query
// censuses that differ between trees. Results are checked outside the timer,
// and each benchmark reports its schema's store prefix length, which the
// directory layer allocates per database.
package sqlbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/keyspace"
	_ "fdb.dev/pkg/relational/sqldriver"
	"fdb.dev/pkg/testutil/benchcluster"
)

func TestMain(m *testing.M) { os.Exit(benchcluster.Main(m)) }

// benchSeq gives each benchmark body a fresh database; Go runs a body more
// than once to size b.N.
var benchSeq atomic.Int64

var (
	prefixDBOnce sync.Once
	prefixDB     *recordlayer.FDBDatabase
	prefixDBErr  error
)

// schemaPrefixBytes is the length of the schema's record store prefix: the
// domain, database and schema values the directory layer allocated.
func schemaPrefixBytes(b *testing.B, ctx context.Context, dbPath, schema string) int {
	b.Helper()
	clusterFile := benchcluster.ClusterFile(b)
	prefixDBOnce.Do(func() {
		fdb.MustAPIVersion(730)
		var conn fdb.Database
		conn, prefixDBErr = fdb.OpenDatabase(clusterFile)
		if prefixDBErr == nil {
			prefixDB = recordlayer.NewFDBDatabase(conn)
		}
	})
	if prefixDBErr != nil {
		b.Fatalf("open FDB for prefix lookup: %v", prefixDBErr)
	}
	ss, err := keyspace.New(subspace.Sub()).LookupSchemaSubspace(ctx, prefixDB, strings.ToUpper(dbPath), schema)
	if err != nil {
		b.Fatalf("resolve %s/%s store prefix: %v", dbPath, schema, err)
	}
	return len(ss.Bytes())
}

// createSchema creates database dbPath with schema STORE of a new template
// holding ddl, and opens a pool on it.
func createSchema(b *testing.B, ctx context.Context, dbPath, tmpl, ddl string) *sql.DB {
	b.Helper()
	clusterFile := benchcluster.ClusterFile(b)
	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s", strings.ToUpper(dbPath), clusterFile))
	if err != nil {
		b.Fatalf("sql.Open: %v", err)
	}
	defer setup.Close()
	execOrFail(b, setup, ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))
	execOrFail(b, setup, ctx, fmt.Sprintf("CREATE SCHEMA TEMPLATE %s %s", tmpl, ddl))
	execOrFail(b, setup, ctx, fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl))
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=STORE", strings.ToUpper(dbPath), clusterFile))
	if err != nil {
		b.Fatalf("sql.Open: %v", err)
	}
	b.Cleanup(func() { db.Close() })
	return db
}

// pinConn returns one connection of db, so a benchmark warms and measures
// the same connection's plan cache instead of whichever the pool hands out.
func pinConn(b *testing.B, ctx context.Context, db *sql.DB) *sql.Conn {
	b.Helper()
	conn, err := db.Conn(ctx)
	if err != nil {
		b.Fatalf("pin connection: %v", err)
	}
	b.Cleanup(func() { conn.Close() })
	return conn
}

// audit logs one machine-readable, untimed receipt: FDB_BENCH_AUDIT and a
// JSON object.
func audit(b *testing.B, fields map[string]any) {
	b.Helper()
	fields["case"] = b.Name()
	line, err := json.Marshal(fields)
	if err != nil {
		b.Fatalf("audit: %v", err)
	}
	b.Logf("FDB_BENCH_AUDIT %s", line)
}

// checksum is the CRC-32 of the items' text, in order.
func checksum(items []item) uint32 {
	h := crc32.NewIEEE()
	for _, it := range items {
		fmt.Fprintf(h, "%d|%s|%d\n", it.id, it.name, it.price)
	}
	return h.Sum32()
}

func execOrFail(b *testing.B, db *sql.DB, ctx context.Context, sql string) {
	b.Helper()
	if _, err := db.ExecContext(ctx, sql); err != nil {
		b.Fatalf("exec %q: %v", sql, err)
	}
}

type item struct {
	id, price int64
	name      string
}

// BenchmarkFDB_PlanCacheHit is a point select by primary key with the plan
// cache warm: parse, cache lookup and one record read per operation.
func BenchmarkFDB_PlanCacheHit(b *testing.B) {
	ctx := context.Background()
	seq := benchSeq.Add(1)
	dbPath := fmt.Sprintf("/FRL/bench_pc_hit_%d", seq)
	db := createSchema(b, ctx, dbPath, fmt.Sprintf("bph_tmpl_%d", seq),
		"CREATE TABLE Item (item_id BIGINT, name STRING, price BIGINT, PRIMARY KEY (item_id))")
	for i := int64(1); i <= 10; i++ {
		execOrFail(b, db, ctx, fmt.Sprintf("INSERT INTO Item VALUES (%d, 'item_%d', %d)", i, i, i*100))
	}
	prefix := schemaPrefixBytes(b, ctx, dbPath, "STORE")
	const query = "SELECT item_id, name, price FROM Item WHERE item_id = 1"
	want := []item{{id: 1, name: "item_1", price: 100}}

	conn := pinConn(b, ctx, db)
	run := func(got []item) ([]item, error) {
		rows, err := conn.QueryContext(ctx, query)
		if err != nil {
			return got, err
		}
		defer rows.Close()
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.id, &it.name, &it.price); err != nil {
				return got, err
			}
			got = append(got, it)
		}
		return got, rows.Err()
	}
	got, err := run(nil) // warm the plan cache
	if err != nil || !equalItems(got, want) {
		b.Fatalf("warm-up: %v, %v; want %v", got, err, want)
	}
	audit(b, map[string]any{"query": query, "rows": len(got), "checksum": checksum(got), "prefix_bytes": prefix})
	got = make([]item, 0, len(want))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err = run(got[:0])
		b.StopTimer()
		if err != nil || !equalItems(got, want) {
			b.Fatalf("iteration %d: %v, %v; want %v", i, got, err, want)
		}
		b.StartTimer()
	}
	b.StopTimer()
	b.ReportMetric(float64(prefix), "prefix-B")
}

// BenchmarkFDB_IndexScanRange selects half of 100 rows through the price index.
func BenchmarkFDB_IndexScanRange(b *testing.B) {
	ctx := context.Background()
	seq := benchSeq.Add(1)
	dbPath := fmt.Sprintf("/FRL/bench_idxrange_%d", seq)
	db := createSchema(b, ctx, dbPath, fmt.Sprintf("bench_idxrange_tmpl_%d", seq),
		"CREATE TABLE Products (id BIGINT, price BIGINT, name STRING, PRIMARY KEY(id)) "+
			"CREATE INDEX idx_products_price ON Products (price)")
	for i := int64(1); i <= 100; i++ {
		execOrFail(b, db, ctx, fmt.Sprintf("INSERT INTO Products VALUES (%d, %d, 'product_%d')", i, i, i))
	}
	prefix := schemaPrefixBytes(b, ctx, dbPath, "STORE")
	const query = "SELECT id, name FROM Products WHERE price > 50"
	var want []item
	for i := int64(51); i <= 100; i++ {
		want = append(want, item{id: i, name: fmt.Sprintf("product_%d", i)})
	}

	conn := pinConn(b, ctx, db)
	run := func(got []item) ([]item, error) {
		rows, err := conn.QueryContext(ctx, query)
		if err != nil {
			return got, err
		}
		defer rows.Close()
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.id, &it.name); err != nil {
				return got, err
			}
			got = append(got, it)
		}
		return got, rows.Err()
	}
	got, err := run(nil) // warm the plan cache
	if err != nil || !equalItems(got, want) {
		b.Fatalf("preflight: %d rows, %v; want ids 51..100", len(got), err)
	}
	audit(b, map[string]any{"query": query, "rows": len(got), "checksum": checksum(got), "prefix_bytes": prefix})
	got = make([]item, 0, len(want))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err = run(got[:0])
		b.StopTimer()
		if err != nil || !equalItems(got, want) {
			b.Fatalf("iteration %d: %d rows, %v; want ids 51..100", i, len(got), err)
		}
		b.StartTimer()
	}
	b.StopTimer()
	b.ReportMetric(float64(prefix), "prefix-B")
}

// BenchmarkFDB_TimestampInsert inserts one row per operation into a table
// with an index on its TIMESTAMP column.
func BenchmarkFDB_TimestampInsert(b *testing.B) {
	ctx := context.Background()
	seq := benchSeq.Add(1)
	dbPath := fmt.Sprintf("/FRL/bench_ts_ins_%d", seq)
	db := createSchema(b, ctx, dbPath, fmt.Sprintf("bench_ts_insert_tmpl_%d", seq),
		"CREATE TABLE Events (id BIGINT, ts TIMESTAMP, label STRING, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_events_ts ON Events (ts)")
	prefix := schemaPrefixBytes(b, ctx, dbPath, "STORE")
	conn := pinConn(b, ctx, db)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ts := fmt.Sprintf("2025-01-01 00:00:%02d", i%60)
		res, err := conn.ExecContext(ctx, fmt.Sprintf("INSERT INTO Events VALUES (%d, '%s', 'evt_%d')", i+1, ts, i+1))
		b.StopTimer()
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			b.Fatalf("iteration %d: %d rows affected, %v", i, n, err)
		}
		b.StartTimer()
	}
	b.StopTimer()
	var count int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM Events").Scan(&count); err != nil || count != int64(b.N) {
		b.Fatalf("after %d inserts: COUNT(*) = %d, %v", b.N, count, err)
	}
	// Every insert reported one row; the count and both end IDs confirm them.
	for _, id := range []int64{1, int64(b.N)} {
		var got int64
		if err := conn.QueryRowContext(ctx, fmt.Sprintf("SELECT id FROM Events WHERE id = %d", id)).Scan(&got); err != nil || got != id {
			b.Fatalf("after %d inserts: id %d: got %d, %v", b.N, id, got, err)
		}
	}
	audit(b, map[string]any{"rows": count, "first_id": 1, "last_id": b.N, "prefix_bytes": prefix})
	b.ReportMetric(float64(prefix), "prefix-B")
}

func equalItems(a, b []item) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
