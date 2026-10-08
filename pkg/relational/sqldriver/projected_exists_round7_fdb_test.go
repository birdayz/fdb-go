package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// TestFDB_ProjectedExists_Round7 pins a COMPUTED, non-selected ORDER BY
// expression over a projected EXISTS — `... ORDER BY col1 + 1` where `col1 + 1`
// is not in the SELECT list. It was rejected while the sort ran above a folded
// record lacking `col1`; the block now carries the key as a hidden column below
// the sort and drops it above, as Java's generateSelect does, so it orders for
// real.
func TestFDB_ProjectedExists_Round7(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := testkit.OpenDB(t, "/FRL/testdb_projexists_r7")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_projexists_r7")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE projexists_r7_tmpl "+
		"CREATE TABLE t1(id BIGINT, col1 BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT, t1_id BIGINT, PRIMARY KEY(id))")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_projexists_r7/s WITH TEMPLATE projexists_r7_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_PROJEXISTS_R7?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// col1 DESCENDS as id ascends, so a real `ORDER BY col1 + 1` differs from id order — a
	// no-op (silently-dropped) sort would visibly fail.
	testkit.MustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 30), (2, 20), (3, 10)")
	testkit.MustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 1), (101, 3)")

	t.Run("computed_nonselected_orderby_sorts", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 " +
			"FROM t1 ORDER BY col1 + 1 DESC"
		rows, qerr := db.QueryContext(ctx, q)
		if qerr != nil {
			t.Fatalf("computed non-selected ORDER BY over projected EXISTS: %v", qerr)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var id int64
			var has bool
			if err := rows.Scan(&id, &has); err != nil {
				t.Fatal(err)
			}
			got = append(got, fmt.Sprintf("%d:%v", id, has))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		// col1+1: id1=31, id2=21, id3=11 → DESC → ids [1 2 3]; t2 references 1 and 3.
		if want := "[1:true 2:false 3:true]"; fmt.Sprint(got) != want {
			t.Fatalf("rows = %v, want %s", got, want)
		}
	})

	// A SELECTED computed expression ordered by its alias sorts by the output column.
	t.Run("selected_alias_orderby_still_folds", func(t *testing.T) {
		q := "SELECT id, col1 + 1 AS c, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 " +
			"FROM t1 ORDER BY c DESC"
		rows, qerr := db.QueryContext(ctx, q)
		if qerr != nil {
			t.Fatalf("selected-alias computed ORDER BY must fold, got: %v", qerr)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id, c int64
			var has bool
			if err := rows.Scan(&id, &c, &has); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		// col1+1: id1=31, id2=21, id3=11 → DESC → ids [1 2 3].
		if fmt.Sprint(ids) != fmt.Sprint([]int64{1, 2, 3}) {
			t.Fatalf("ORDER BY c DESC ids = %v, want [1 2 3]", ids)
		}
	})
}
