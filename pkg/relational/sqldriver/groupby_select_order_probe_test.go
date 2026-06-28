package sqldriver_test

// KNOWN BUG sentinel — GROUP BY column ORDER (TODO.md "GROUP BY ignores SELECT-list
// column order").
//
// A standalone `SELECT <aggregate>, <key> ... GROUP BY <key>` returns its result
// columns in the aggregate's native KEYS-FIRST order, NOT the SELECT-list order.
// E.g. `SELECT SUM(v), a FROM t GROUP BY a` returns columns [A, SUM(V)] instead of
// [SUM(V), A]. Standard SQL (and any client doing POSITIONAL column access) expects
// SELECT-list order. The data is correct and the column NAMES are correct, so
// NAME-based access is a sound workaround (asserted below).
//
// Root cause: the standalone GROUP BY builder emits a bare LogicalAggregate with no
// post-aggregate Project (logical_predicate.go ~3313); translateAggregate builds
// GroupByExpression as groupKeys-then-aggregates; aggregateOutputColumns mirrors
// that, and join-leg/CTE anchoring relies on it — so reordering needs a reviewed,
// cross-cutting change (reuse buildPostAggregateProjection for the standalone case).
//
// This test PINS THE CURRENT (wrong) positional order so the behavior can't drift
// silently; when the bug is fixed, the positional assertion flips to SELECT order
// and this comment + assertion get updated.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_GroupBySelectOrderProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_gso")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_gso")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE gso CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_gso/s WITH TEMPLATE gso")
	dsn := fmt.Sprintf("fdbsql:///testdb_gso?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// single group a=7, SUM(v)=10+20=30
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, v) VALUES (1, 7, 10), (2, 7, 20)")

	// key-first SELECT: order already matches — correct in both order and names.
	t.Run("key_first_select_correct", func(t *testing.T) {
		var c0, c1 int64
		if err := db.QueryRowContext(ctx, "SELECT a, SUM(v) FROM t GROUP BY a").Scan(&c0, &c1); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if c0 != 7 || c1 != 30 {
			t.Errorf("SELECT a, SUM(v) = (%d, %d), want (7, 30)", c0, c1)
		}
	})

	// aggregate-first SELECT: positional order is currently WRONG (keys-first).
	t.Run("agg_first_positional_is_currently_keysfirst_BUG", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT SUM(v), a FROM t GROUP BY a")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		if !rows.Next() {
			t.Fatal("no rows")
		}
		var c0, c1 int64
		if err := rows.Scan(&c0, &c1); err != nil {
			t.Fatalf("scan: %v", err)
		}
		// CURRENT (buggy) behavior: columns come back keys-first → (a=7, SUM=30).
		// When the SELECT-order bug is fixed, this becomes (30, 7) — flip then.
		if c0 != 7 || c1 != 30 {
			t.Errorf("positional order changed: got (%d, %d) cols=%v. If now (30,7) the "+
				"SELECT-order bug is FIXED — update this sentinel + TODO.md", c0, c1, cols)
		}
	})

	// NAME-based access is the sound workaround — correct regardless of order.
	t.Run("name_based_access_is_correct", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT SUM(v) AS total, a AS grp FROM t GROUP BY a")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns: %v", err)
		}
		if !rows.Next() {
			t.Fatal("no rows")
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		byName := map[string]int64{}
		for i, c := range cols {
			if n, ok := vals[i].(int64); ok {
				byName[c] = n
			}
		}
		// regardless of positional order, the named columns carry the right values.
		if byName["TOTAL"] != 30 {
			t.Errorf("TOTAL (SUM(v)) = %d, want 30 (cols=%v)", byName["TOTAL"], cols)
		}
		if byName["GRP"] != 7 {
			t.Errorf("GRP (a) = %d, want 7 (cols=%v)", byName["GRP"], cols)
		}
	})
}
