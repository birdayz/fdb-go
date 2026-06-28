package sqldriver_test

// Probes the prepared-statement driver contract: a prepared SELECT and a prepared
// INSERT are reused across multiple parameter sets with correct results, and
// LastInsertId() is cleanly unsupported (the record layer uses explicit primary
// keys — there is no auto-increment, so reporting "not supported" is correct,
// unlike returning a bogus 0).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_PreparedStmtProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_psp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_psp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE psp CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_psp/s WITH TEMPLATE psp")
	dsn := fmt.Sprintf("fdbsql:///testdb_psp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,10),(2,20),(3,30)")

	t.Run("prepared_select_reused", func(t *testing.T) {
		stmt, err := db.PrepareContext(ctx, "SELECT a FROM t WHERE id = ?")
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		defer stmt.Close()
		for _, c := range []struct{ id, want int64 }{{1, 10}, {3, 30}, {2, 20}, {1, 10}} {
			var a int64
			if err := stmt.QueryRowContext(ctx, c.id).Scan(&a); err != nil {
				t.Fatalf("query id=%d: %v", c.id, err)
			}
			if a != c.want {
				t.Errorf("stmt(id=%d) = %d, want %d", c.id, a, c.want)
			}
		}
	})

	t.Run("prepared_insert_reused", func(t *testing.T) {
		ins, err := db.PrepareContext(ctx, "INSERT INTO t (id, a) VALUES (?, ?)")
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		defer ins.Close()
		for i := int64(10); i <= 12; i++ {
			res, err := ins.ExecContext(ctx, i, i*100)
			if err != nil {
				t.Fatalf("exec %d: %v", i, err)
			}
			if n, _ := res.RowsAffected(); n != 1 {
				t.Errorf("insert %d RowsAffected = %d, want 1", i, n)
			}
			// LastInsertId is cleanly unsupported.
			if _, err := res.LastInsertId(); err == nil || !strings.Contains(err.Error(), "not supported") {
				t.Errorf("LastInsertId err = %v, want 'not supported'", err)
			}
		}
		// verify the prepared inserts landed.
		var got int64
		if err := db.QueryRowContext(ctx, "SELECT a FROM t WHERE id = 11").Scan(&got); err != nil {
			t.Fatalf("verify: %v", err)
		}
		if got != 1100 {
			t.Errorf("prepared insert id=11 a = %d, want 1100", got)
		}
	})
}
