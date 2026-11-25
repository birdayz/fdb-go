package sqltest

// Probes multi-row INSERT statement atomicity: a constraint violation on a row in
// the middle of a multi-VALUES INSERT rolls back the ENTIRE statement — no partial
// insert of the preceding/following rows. Covered for a mid-batch duplicate PK
// (23505, against a pre-existing row). The former mid-batch 23502 arm is gone:
// scalar NOT NULL is unexpressible (Java parity: NOT NULL is only allowed for
// ARRAY column type, rejected at CREATE); the scalar 23502 surface is gone.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDB_InsertAtomicityProbe(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_iatp")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_iatp")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA TEMPLATE iatp "+
		"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE u (id BIGINT, a BIGINT, PRIMARY KEY (id))")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_iatp/s WITH TEMPLATE iatp")
	dsn := fmt.Sprintf("fdbsql:///FRL/TESTDB_IATP?cluster_file=%s&schema=S", testkit.ClusterFile())
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
		testkit.MustExecCtx(t, db, ctx, "INSERT INTO t (id, a) VALUES (2, 20)") // pre-existing pk=2
		_, err := db.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (1,10),(2,99),(3,30)")
		if err == nil || !strings.Contains(err.Error(), "23505") {
			t.Fatalf("err = %v, want 23505", err)
		}
		// rows 1 and 3 must NOT have been inserted — only the pre-existing 2 remains.
		if got := ids("t"); !eq(got, []int64{2}) {
			t.Errorf("after failed multi-insert, t = %v, want [2] (atomic rollback, no partial)", got)
		}
	})
	// INSERT reads ahead in windows of ten rows: a key repeated inside a window
	// and one repeated across windows must each see the earlier row.
	t.Run("repeated_pk_in_statement_rolls_back_all", func(t *testing.T) {
		for _, stmt := range []string{
			"INSERT INTO t (id, a) VALUES (5,50),(6,60),(5,51)",
			"INSERT INTO t (id, a) VALUES (11,1),(12,1),(13,1),(14,1),(15,1),(16,1),(17,1),(18,1),(19,1),(20,1),(11,2)",
		} {
			_, err := db.ExecContext(ctx, stmt)
			if err == nil || !strings.Contains(err.Error(), "23505") {
				t.Fatalf("%s: err = %v, want 23505", stmt, err)
			}
			if got := ids("t"); !eq(got, []int64{2}) {
				t.Errorf("%s: t = %v, want [2] (atomic rollback, no partial)", stmt, got)
			}
		}
	})
	t.Run("valid_multi_insert_all_applied", func(t *testing.T) {
		testkit.MustExecCtx(t, db, ctx, "INSERT INTO u (id, a) VALUES (10,1),(11,2),(12,3)")
		if got := ids("u"); !eq(got, []int64{10, 11, 12}) {
			t.Errorf("valid multi-insert u = %v, want [10 11 12]", got)
		}
	})
}
