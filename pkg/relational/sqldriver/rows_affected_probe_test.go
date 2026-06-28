package sqldriver_test

// Pins the database/sql RowsAffected() contract for DML: INSERT (multi/single),
// UPDATE (multi-match / zero-match), DELETE (single / zero-match / all). The count
// reflects exactly the rows written/changed/removed.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_RowsAffectedProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_rap")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rap")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE rap CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rap/s WITH TEMPLATE rap")
	dsn := fmt.Sprintf("fdbsql:///testdb_rap?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	affected := func(name, q string, want int64) {
		t.Run(name, func(t *testing.T) {
			res, err := db.ExecContext(ctx, q)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				t.Fatalf("RowsAffected: %v", err)
			}
			if n != want {
				t.Errorf("%s RowsAffected = %d, want %d", name, n, want)
			}
		})
	}
	affected("insert_multi", "INSERT INTO t (id, a) VALUES (1,10),(2,20),(3,30)", 3)
	affected("insert_single", "INSERT INTO t (id, a) VALUES (4,40)", 1)
	affected("update_multi_match", "UPDATE t SET a = a + 1 WHERE a >= 20", 3) // ids 2,3,4
	affected("update_zero_match", "UPDATE t SET a = 0 WHERE id = 999", 0)
	affected("delete_single", "DELETE FROM t WHERE id = 1", 1)
	affected("delete_zero_match", "DELETE FROM t WHERE a > 100", 0)
	affected("delete_all_remaining", "DELETE FROM t", 3) // ids 2,3,4 remain
}
