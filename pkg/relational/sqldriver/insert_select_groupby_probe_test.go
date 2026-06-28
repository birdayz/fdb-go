package sqldriver_test

// Probes INSERT … SELECT … GROUP BY column alignment — the path where the standalone
// GROUP-BY column-order bug (TODO.md) could have caused DATA CORRUPTION but does
// NOT: the bare INSERT path runs the SELECT through buildPostAggregateProjection,
// which honors SELECT-LIST order, so `INSERT INTO dst SELECT SUM(v), a … GROUP BY a`
// correctly maps SUM(v)→g, a→total (positional by SELECT order). An explicit target
// column list with INSERT … SELECT is fail-closed (0AF00), avoiding any mis-map.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_InsertSelectGroupByProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_isg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_isg")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE isg "+
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, v BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE dst (g BIGINT NOT NULL, total BIGINT, PRIMARY KEY (g))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_isg/s WITH TEMPLATE isg")
	dsn := fmt.Sprintf("fdbsql:///testdb_isg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, v) VALUES (1,7,10),(2,7,20)") // group a=7, SUM=30

	insertAndRead := func(insert string) (int64, int64) {
		if _, err := db.ExecContext(ctx, "DELETE FROM dst"); err != nil {
			t.Fatalf("reset: %v", err)
		}
		if _, err := db.ExecContext(ctx, insert); err != nil {
			t.Fatalf("%s: %v", insert, err)
		}
		var g, total int64
		if err := db.QueryRowContext(ctx, "SELECT g, total FROM dst").Scan(&g, &total); err != nil {
			t.Fatalf("read dst: %v", err)
		}
		return g, total
	}

	t.Run("bare_key_first", func(t *testing.T) {
		// SELECT a, SUM(v) → g←a(7), total←SUM(30)
		g, total := insertAndRead("INSERT INTO dst SELECT a, SUM(v) FROM t GROUP BY a")
		if g != 7 || total != 30 {
			t.Errorf("g=%d total=%d, want g=7 total=30", g, total)
		}
	})
	t.Run("bare_agg_first_honors_select_order", func(t *testing.T) {
		// SELECT SUM(v), a → g←SUM(30), total←a(7) — positional by SELECT order, no corruption.
		g, total := insertAndRead("INSERT INTO dst SELECT SUM(v), a FROM t GROUP BY a")
		if g != 30 || total != 7 {
			t.Errorf("g=%d total=%d, want g=30 total=7 (SELECT-order positional)", g, total)
		}
	})
	t.Run("explicit_column_list_failclosed", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO dst (g, total) SELECT a, SUM(v) FROM t GROUP BY a")
		if err == nil || !strings.Contains(err.Error(), "0AF00") {
			t.Errorf("explicit-column-list INSERT…SELECT err = %v, want 0AF00 (unsupported)", err)
		}
	})
}
