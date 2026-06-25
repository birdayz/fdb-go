package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// FuzzSQL_QueryContext closes the execution half of the P0.3-F gap: an arbitrary
// SQL *string* driven through the real database/sql driver → planner → executor →
// FDB, asserting that **no panic escapes the db/sql boundary** for any input. The
// boundary recover (connection.go / paginatingRows.Next) converts an internal
// panic into a generic error, so this target validates the never-panic-to-caller
// guarantee end to end (a clean error is fine; a crash is a bug). The
// complementary FuzzSQLPlan (pkg/relational/core/embedded) surfaces the
// underlying planner/semantic panics directly, before the recover.
//
// Container-gated + shallow by nature (each input is a real FDB transaction), so
// the seed corpus runs in CI while active fuzzing is opt-in. The db/sql boundary
// recover remains the production backstop; this narrows, the recover guarantees.
func FuzzSQL_QueryContext(f *testing.F) {
	if clusterFilePath == "" {
		f.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const dbPath = "/fuzz_qctx"

	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s", dbPath, clusterFilePath))
	if err != nil {
		f.Fatalf("open setup conn: %v", err)
	}
	f.Cleanup(func() { setup.Close() })

	// Best-effort, idempotent setup — tolerate "already exists" across re-runs and
	// across parallel fuzz workers sharing the one FDB container.
	_, _ = setup.ExecContext(ctx, "CREATE DATABASE "+dbPath)
	_, _ = setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE fuzz_tmpl "+
		"CREATE TABLE t (id BIGINT NOT NULL, name STRING, amount BIGINT, PRIMARY KEY (id))")
	_, _ = setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE fuzz_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		f.Fatalf("open query conn: %v", err)
	}
	f.Cleanup(func() { db.Close() })

	_, _ = db.ExecContext(ctx, "INSERT INTO t VALUES (1, 'a', 10), (2, 'b', 20), (3, 'c', 30)")

	// Sanity: the schema must be queryable, else the fuzz would be vacuous.
	if _, qerr := db.QueryContext(ctx, "SELECT id FROM t"); qerr != nil {
		f.Fatalf("setup sanity query failed (schema not ready): %v", qerr)
	}

	seeds := []string{
		"SELECT id, name, amount FROM t WHERE id = 1",
		"SELECT * FROM t ORDER BY amount DESC LIMIT 2 OFFSET 1",
		"SELECT name, COUNT(*), SUM(amount) FROM t GROUP BY name HAVING COUNT(*) >= 1",
		"SELECT a.id FROM t a, t b WHERE a.id = b.id",
		"SELECT id FROM t WHERE name IN (SELECT name FROM t)",
		"SELECT CASE WHEN amount > 15 THEN 'hi' ELSE 'lo' END FROM t",
		"SELECT amount / 0 FROM t",
		"SELECT id FROM t WHERE EXISTS (SELECT 1 FROM t b WHERE b.id = t.id)",
		"WITH c AS (SELECT id FROM t) SELECT id FROM c",
		"SELECT UPPER(name) FROM t ORDER BY name",
		"",
		"SELECT",
		"SELECT no_col FROM t",
		"SELECT id FROM no_such_table",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, q string) {
		rows, qerr := db.QueryContext(ctx, q)
		if qerr != nil {
			return // a clean error is the correct outcome; only a panic is a bug
		}
		defer rows.Close()
		cols, cerr := rows.Columns()
		if cerr != nil {
			return
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			_ = rows.Scan(ptrs...)
		}
		_ = rows.Err()
	})
}
