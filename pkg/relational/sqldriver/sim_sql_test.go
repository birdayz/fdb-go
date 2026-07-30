package sqldriver

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/simfdb"
)

// injectSimFDB registers a SimFDB-backed FDBDatabase in the driver's cluster-file cache under
// a unique key, so `sql.Open("fdbsql", "...cluster_file=<key>...")` drives the full SQL engine
// (parser → Cascades planner → executor → record layer) over SimFDB instead of a real cluster —
// no Docker. Returns the cluster_file key to use in DSNs. RFC-199: SimFDB validates the
// *relational* layer, not just the record layer.
func injectSimFDB(t *testing.T, seed uint64) string {
	t.Helper()
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	simDB := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
	// Match the driver's own setup so the SQL metadata-version path behaves identically.
	simDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	key := "sim://" + t.Name()
	fdbDBCache.Store(key, simDB)
	t.Cleanup(func() { fdbDBCache.Delete(key) })
	return key
}

func mustExecSQL(t *testing.T, db *sql.DB, ctx context.Context, query string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, query); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// TestSQL_OverSimFDB drives the full SQL stack over SimFDB: create a database + schema, insert
// rows, and run ordered/aggregate/filter queries — all deterministic, no Docker. This proves
// SimFDB is a drop-in backend for the RELATIONAL layer end to end (RFC-199 keystone extended
// past the record layer).
func TestSQL_OverSimFDB(t *testing.T) {
	t.Parallel()
	key := injectSimFDB(t, 1)
	ctx := context.Background()

	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///simdb?cluster_file=%s", key))
	if err != nil {
		t.Fatalf("open setup: %v", err)
	}
	defer setup.Close()
	mustExecSQL(t, setup, ctx, "CREATE DATABASE /simdb")
	mustExecSQL(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE tmpl CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mustExecSQL(t, setup, ctx, "CREATE SCHEMA /simdb/s WITH TEMPLATE tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///simdb?cluster_file=%s&schema=s", key))
	if err != nil {
		t.Fatalf("open query conn: %v", err)
	}
	defer db.Close()

	const N = 50
	var b []byte
	b = append(b, "INSERT INTO t (id, a) VALUES "...)
	for i := 1; i <= N; i++ {
		if i > 1 {
			b = append(b, ',')
		}
		b = append(b, fmt.Sprintf("(%d,%d)", i, i*10)...)
	}
	mustExecSQL(t, db, ctx, string(b))

	// COUNT(*) — aggregate over SimFDB.
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != N {
		t.Fatalf("COUNT(*) = %d, want %d", n, N)
	}

	// Ordered scan — exercises cursor/continuation chaining through the executor over SimFDB.
	rows, err := db.QueryContext(ctx, "SELECT id, a FROM t ORDER BY id")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer rows.Close()
	want := int64(1)
	for rows.Next() {
		var id, a int64
		if err := rows.Scan(&id, &a); err != nil {
			t.Fatalf("row scan: %v", err)
		}
		if id != want || a != id*10 {
			t.Fatalf("row %d: got id=%d a=%d, want id=%d a=%d", want, id, a, want, want*10)
		}
		want++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if want-1 != N {
		t.Fatalf("scanned %d rows, want %d", want-1, N)
	}

	// Filtered aggregate.
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t WHERE id > 20 AND id <= 40").Scan(&n); err != nil {
		t.Fatalf("filtered count: %v", err)
	}
	if n != 20 {
		t.Fatalf("range (20,40] count = %d, want 20", n)
	}
}
