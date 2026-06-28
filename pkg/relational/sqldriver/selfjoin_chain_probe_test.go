package sqldriver_test

// Probes self-joins (same table, two aliases) and multi-way join chains —
// stressing alias handling (the #1 silent-bug source per CLAUDE.md: quantifier
// vs table alias namespaces). Includes NULL self-join keys and mixed INNER/LEFT.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_SelfJoinChainProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_selfchain")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_selfchain")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE selfchain "+
			"CREATE TABLE emp (id BIGINT NOT NULL, mgr BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX emp_mgr ON emp (mgr)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_selfchain/s WITH TEMPLATE selfchain")
	dsn := fmt.Sprintf("fdbsql:///testdb_selfchain?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// id1 mgr=NULL (CEO); id2 mgr=1; id3 mgr=1; id4 mgr=2
	mwjoMustExec(t, db, ctx, "INSERT INTO emp (id, mgr) VALUES (2,1),(3,1),(4,2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO emp (id) VALUES (1)")

	pairs := func(q string) []string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		return siScanRows(t, rows)
	}

	// Self-join INNER: employee → its manager. id1 (mgr NULL) excluded.
	t.Run("selfjoin_inner", func(t *testing.T) {
		got := pairs("SELECT e.id, m.id FROM emp e JOIN emp m ON e.mgr = m.id")
		want := []string{"2|1", "3|1", "4|2"}
		if !eqStrSlices(got, want) {
			t.Errorf("self-join inner = %v, want %v", got, want)
		}
	})

	// Self-join LEFT: CEO (id1, mgr NULL) null-extends.
	t.Run("selfjoin_left_count", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT COUNT(*) FROM emp e LEFT JOIN emp m ON e.mgr = m.id")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var c int64
		rows.Next()
		_ = rows.Scan(&c)
		if c != 4 { // 3 matched + id1 null-extended
			t.Errorf("self-join LEFT count = %d, want 4", c)
		}
	})

	// 3-way self chain: employee → manager → grand-manager. Only id4 has a full chain.
	t.Run("selfjoin_3way_chain", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT e.id, m.id, gm.id FROM emp e JOIN emp m ON e.mgr = m.id JOIN emp gm ON m.mgr = gm.id")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var a, b, c int64
			_ = rows.Scan(&a, &b, &c)
			got = append(got, fmt.Sprintf("%d|%d|%d", a, b, c))
		}
		sort.Strings(got)
		if len(got) != 1 || got[0] != "4|2|1" {
			t.Errorf("3-way self chain = %v, want [4|2|1]", got)
		}
	})

	// Self-join with a WHERE on one alias: employees managed by id1.
	t.Run("selfjoin_where_on_alias", func(t *testing.T) {
		got := pairs("SELECT e.id, m.id FROM emp e JOIN emp m ON e.mgr = m.id WHERE m.id = 1")
		want := []string{"2|1", "3|1"}
		if !eqStrSlices(got, want) {
			t.Errorf("self-join + WHERE = %v, want %v", got, want)
		}
	})
}
