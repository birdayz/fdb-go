// Package fetchmethod benchmarks one non-covering SQL index query under each
// explicit INDEX_FETCH_METHOD (RFC-258 #923 trees only).
//
// Each method runs on its own pinned connection to a database without a
// StoreTimer, so the measured operations carry no instrumentation. A second
// pinned connection per method reaches the same schema through a registered
// backend with a StoreTimer; before and after the measured loop it checks the
// plan (EXPLAIN: IDX_A, not covering) and, after resetting the timer, which
// path a run took. All of that, and the per-operation row check, is untimed.
package fetchmethod

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
	"fdb.dev/pkg/internal/fdbclient"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/sqldriver"
	"fdb.dev/pkg/testutil/benchcluster"
)

func TestMain(m *testing.M) { os.Exit(benchcluster.Main(m)) }

const (
	rowCount = 1000
	schema   = "S"
	// auditKey is the registered, timed backend's cluster_file key.
	auditKey = "bench-fetch-method-audit"
)

type fixture struct {
	measured, audited *sql.DB
	timer             *recordlayer.StoreTimer
	prefix            int
}

// fixtureSeq names each benchmark body's database: Go runs a body more than
// once (b.N sizing, -test.count), and every run gets fresh plan caches.
var fixtureSeq atomic.Int64

var (
	auditOnce sync.Once
	auditDB   *recordlayer.FDBDatabase
	auditErr  error
)

// loadFixture creates T (id, a = id, c = 'v<id>') with index IDX_A on a and
// rowCount rows in a database of its own, dropping one an earlier process left
// under the same name first and dropping it again when the benchmark ends.
func loadFixture(b *testing.B) *fixture {
	b.Helper()
	ctx := context.Background()
	clusterFile := benchcluster.ClusterFile(b)
	seq := fixtureSeq.Add(1)
	dbPath := fmt.Sprintf("/FRL/BENCH_FETCH_METHOD_%d", seq)
	tmpl := fmt.Sprintf("bench_fetch_method_tmpl_%d", seq)
	drop := func() error {
		catalog, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s&schema=CATALOG", clusterFile))
		if err != nil {
			return err
		}
		defer catalog.Close()
		for _, stmt := range []string{"DROP DATABASE IF EXISTS " + dbPath, "DROP SCHEMA TEMPLATE IF EXISTS " + tmpl} {
			if _, err := catalog.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}
		return nil
	}
	if err := drop(); err != nil {
		b.Fatal(err)
	}
	// Registered first, so it runs after the pools close.
	b.Cleanup(func() {
		if err := drop(); err != nil {
			b.Errorf("drop benchmark schema: %v", err)
		}
	})
	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s", dbPath, clusterFile))
	if err != nil {
		b.Fatalf("sql.Open: %v", err)
	}
	defer setup.Close()
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE " + tmpl +
			" CREATE TABLE T (id BIGINT, a BIGINT, c STRING, PRIMARY KEY (id)) CREATE INDEX idx_a ON T(a)",
		"CREATE SCHEMA " + dbPath + "/" + schema + " WITH TEMPLATE " + tmpl,
	} {
		if _, err := setup.ExecContext(ctx, stmt); err != nil {
			b.Fatalf("%s: %v", stmt, err)
		}
	}
	f := &fixture{}
	f.measured, err = sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, clusterFile, schema))
	if err != nil {
		b.Fatalf("sql.Open: %v", err)
	}
	b.Cleanup(func() { f.measured.Close() })
	for first := 0; first < rowCount; first += 100 {
		var values []string
		for id := first; id < first+100; id++ {
			values = append(values, fmt.Sprintf("(%d, %d, 'v%d')", id, id, id))
		}
		if _, err := f.measured.ExecContext(ctx, "INSERT INTO T VALUES "+strings.Join(values, ", ")); err != nil {
			b.Fatalf("seed rows from %d: %v", first, err)
		}
	}
	auditOnce.Do(func() {
		var backend fdb.BackendDatabase
		backend, auditErr = fdbclient.Open(clusterFile)
		if auditErr == nil {
			auditDB = recordlayer.NewFDBDatabaseWithBackend(backend)
			sqldriver.RegisterBackend(auditKey, auditDB)
		}
	})
	if auditErr != nil {
		b.Fatalf("open audit backend: %v", auditErr)
	}
	f.timer = sqldriver.EnableStoreTimer(auditKey)
	f.audited, err = sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, auditKey, schema))
	if err != nil {
		b.Fatalf("sql.Open audit: %v", err)
	}
	b.Cleanup(func() { f.audited.Close() })
	ss, err := keyspace.New(subspace.Sub()).LookupSchemaSubspace(ctx, auditDB, dbPath, schema)
	if err != nil {
		b.Fatalf("resolve store prefix: %v", err)
	}
	f.prefix = len(ss.Bytes())
	auditLog(b, map[string]any{"phase": "fixture", "rows": rowCount, "prefix_bytes": f.prefix})
	return f
}

// auditLog logs one machine-readable, untimed receipt: FDB_BENCH_AUDIT and a
// JSON object carrying the benchmark name.
func auditLog(b *testing.B, fields map[string]any) {
	b.Helper()
	fields["case"] = b.Name()
	line, err := json.Marshal(fields)
	if err != nil {
		b.Fatalf("audit: %v", err)
	}
	b.Logf("FDB_BENCH_AUDIT %s", line)
}

// pin returns a connection of db carrying method.
func pin(b *testing.B, ctx context.Context, db *sql.DB, method api.IndexFetchMethod) *sql.Conn {
	b.Helper()
	conn, err := db.Conn(ctx)
	if err != nil {
		b.Fatalf("pin connection: %v", err)
	}
	b.Cleanup(func() { conn.Close() })
	if err := conn.Raw(func(driverConn any) error {
		ec, ok := driverConn.(*embedded.EmbeddedConnection)
		if !ok {
			return fmt.Errorf("driver connection is %T", driverConn)
		}
		return ec.SetOption(api.OptIndexFetchMethod, method)
	}); err != nil {
		b.Fatalf("set INDEX_FETCH_METHOD: %v", err)
	}
	return conn
}

type row struct {
	id int64
	c  string
}

// rowsChecksum is the CRC-32 of the rows' text, in order.
func rowsChecksum(rows []row) uint32 {
	h := crc32.NewIEEE()
	for _, r := range rows {
		fmt.Fprintf(h, "%d|%s\n", r.id, r.c)
	}
	return h.Sum32()
}

// query is one benchmark statement; first..first+count-1 are its IDs.
type query struct {
	name         string
	sql          string
	first, count int
}

var queries = []query{
	{name: "rows10", sql: "SELECT id, c FROM T WHERE a >= 100 AND a < 110 AND c IS NOT NULL", first: 100, count: 10},
	{name: "rows1000", sql: "SELECT id, c FROM T WHERE a >= 0 AND c IS NOT NULL", first: 0, count: rowCount},
}

// BenchmarkSQLIndexFetch measures executing and draining one non-covering
// IDX_A query per operation, planning excluded (the plan cache is warm).
//
// Sub-benchmarks: query={rows10,rows1000}/method={SCAN_AND_FETCH,USE_REMOTE_FETCH}.
func BenchmarkSQLIndexFetch(b *testing.B) {
	f := loadFixture(b)
	for _, q := range queries {
		for _, method := range []api.IndexFetchMethod{api.IndexFetchScanAndFetch, api.IndexFetchUseRemoteFetch} {
			b.Run(fmt.Sprintf("query=%s/method=%s", q.name, method), func(b *testing.B) {
				benchQuery(b, f, q, method)
			})
		}
	}
}

func benchQuery(b *testing.B, f *fixture, q query, method api.IndexFetchMethod) {
	ctx := context.Background()
	measured := pin(b, ctx, f.measured, method)
	audited := pin(b, ctx, f.audited, method)

	run := func(conn *sql.Conn, got []row) ([]row, error) {
		rows, err := conn.QueryContext(ctx, q.sql)
		if err != nil {
			return got, err
		}
		defer rows.Close()
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.c); err != nil {
				return got, err
			}
			got = append(got, r)
		}
		return got, rows.Err()
	}
	verify := func(got []row) error {
		if len(got) != q.count {
			return fmt.Errorf("%d rows, want %d", len(got), q.count)
		}
		for i, r := range got {
			id := int64(q.first + i)
			if r.id != id || r.c != fmt.Sprintf("v%d", id) {
				return fmt.Errorf("row %d: %+v, want id %d", i, r, id)
			}
		}
		return nil
	}
	explain := func(conn *sql.Conn) {
		var plan string
		if err := conn.QueryRowContext(ctx, "EXPLAIN "+q.sql).Scan(&plan); err != nil {
			b.Fatalf("EXPLAIN: %v", err)
		}
		if !strings.Contains(plan, "IndexScan(IDX_A") || strings.Contains(plan, "COVERING") {
			b.Fatalf("plan %s is not a non-covering IDX_A scan", plan)
		}
	}
	audit := func(phase string) {
		explain(audited)
		// Resolve the schema and plan on this connection before the reset:
		// catalog reads are record loads too.
		got, err := run(audited, nil)
		if err == nil {
			err = verify(got)
		}
		if err != nil {
			b.Fatalf("%s warm-up: %v", phase, err)
		}
		f.timer.Reset()
		got, err = run(audited, nil)
		if err == nil {
			err = verify(got)
		}
		if err != nil {
			b.Fatalf("%s audit: %v", phase, err)
		}
		remote := f.timer.GetCount(recordlayer.CountRemoteFetch)
		entries := f.timer.GetCount(recordlayer.EventScanRemoteFetchEntry)
		indexScans := f.timer.GetCount(recordlayer.EventScanIndex)
		loads := f.timer.GetCount(recordlayer.EventLoadRecord)
		auditLog(b, map[string]any{
			"phase": phase, "method": method.String(), "query": q.sql, "rows": len(got),
			"remote_openings": remote, "remote_entries": entries, "index_scans": indexScans,
			"record_loads": loads, "checksum": rowsChecksum(got), "prefix_bytes": f.prefix,
		})
		if method == api.IndexFetchUseRemoteFetch {
			if remote < 1 || entries < int64(len(got)) || indexScans != 0 || loads != 0 {
				b.Fatalf("%s audit: USE_REMOTE_FETCH did not return mapped results without record loads", phase)
			}
		} else if remote != 0 || entries != 0 || indexScans < 1 || loads < int64(len(got)) {
			b.Fatalf("%s audit: SCAN_AND_FETCH did not scan the index and load each record", phase)
		}
	}

	audit("before")
	explain(measured)
	got, err := run(measured, nil) // warm the measured connection's plan cache
	if err == nil {
		err = verify(got)
	}
	if err != nil {
		b.Fatalf("warm-up: %v", err)
	}
	got = make([]row, 0, q.count)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err = run(measured, got[:0])
		b.StopTimer()
		if err == nil {
			err = verify(got)
		}
		if err != nil {
			b.Fatalf("operation %d: %v", i, err)
		}
		b.StartTimer()
	}
	b.StopTimer()
	audit("after")
	b.ReportMetric(float64(q.count), "rows/op")
	b.ReportMetric(float64(f.prefix), "prefix-B")
}
