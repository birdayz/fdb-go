package sqldriver_test

// Probes multi-row INSERT statement atomicity: a constraint violation on a row in
// the middle of a multi-VALUES INSERT rolls back the ENTIRE statement — no partial
// insert of the preceding/following rows. Covered for a mid-batch duplicate PK
// (23505, against a pre-existing row) and a mid-batch NOT NULL violation (23502).

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_InsertAtomicityProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_iatp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_iatp")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE iatp "+
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE u (id BIGINT NOT NULL, a BIGINT NOT NULL, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_iatp/s WITH TEMPLATE iatp")
	dsn := fmt.Sprintf("fdbsql:///testdb_iatp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ids := func(table string) []int64 {
		rows, err := db.QueryContext(ctx, "SELECT id FROM "+table)
		if err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			o = append(o, v)
		}
		sort.Slice(o, func(i, j int) bool { return o[i] < o[j] })
		return o
	}
	eq := func(g, w []int64) bool {
		if len(g) != len(w) {
			return false
		}
		for i := range g {
			if g[i] != w[i] {
				return false
			}
		}
		return true
	}

	t.Run("mid_batch_dup_pk_rolls_back_all", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (2, 20)") // pre-existing pk=2
		_, err := db.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (1,10),(2,99),(3,30)")
		if err == nil || !strings.Contains(err.Error(), "23505") {
			t.Fatalf("err = %v, want 23505", err)
		}
		// rows 1 and 3 must NOT have been inserted — only the pre-existing 2 remains.
		if got := ids("t"); !eq(got, []int64{2}) {
			t.Errorf("after failed multi-insert, t = %v, want [2] (atomic rollback, no partial)", got)
		}
	})
	t.Run("mid_batch_not_null_rolls_back_all", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO u (id, a) VALUES (1,10),(2,NULL),(3,30)")
		if err == nil || !strings.Contains(err.Error(), "23502") {
			t.Fatalf("err = %v, want 23502", err)
		}
		if got := ids("u"); len(got) != 0 {
			t.Errorf("after failed multi-insert, u = %v, want [] (atomic rollback)", got)
		}
	})
	t.Run("valid_multi_insert_all_applied", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "INSERT INTO u (id, a) VALUES (10,1),(11,2),(12,3)")
		if got := ids("u"); !eq(got, []int64{10, 11, 12}) {
			t.Errorf("valid multi-insert u = %v, want [10 11 12]", got)
		}
	})
}
