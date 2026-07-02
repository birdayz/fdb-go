package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_ProjectedExists_Round7 pins the review round-7 case: a COMPUTED, non-selected
// ORDER BY expression over a projected EXISTS — e.g. `... ORDER BY col1 + 1` where `col1 + 1`
// is not in the SELECT list. The folded output record carries only the SELECT fields, so the
// sort re-applied above the FlatMap evaluated `col1 + 1` against a record lacking `col1` →
// NULL every row → the ordering silently became a no-op (wrong order). It is now REJECTED
// cleanly by the projected-EXISTS guard (ErrCodeUnsupportedQuery), never silently
// mis-ordered. A SELECTED column or alias ORDER BY still folds and orders for real — the
// rejection is narrow (only computed expressions absent from the projection bail).
func TestFDB_ProjectedExists_Round7(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_projexists_r7")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_projexists_r7")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE projexists_r7_tmpl "+
		"CREATE TABLE t1(id BIGINT, col1 BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT, t1_id BIGINT, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_projexists_r7/s WITH TEMPLATE projexists_r7_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_projexists_r7?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// col1 DESCENDS as id ascends, so a real `ORDER BY col1 + 1` differs from id order — a
	// no-op (silently-dropped) sort would visibly fail.
	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 30), (2, 20), (3, 10)")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 1), (101, 3)")

	// Revert-proof: removing the fold's bail (cascades_translator.go) makes this query plan
	// and return rows in scan order — silently wrong — instead of erroring.
	t.Run("computed_nonselected_orderby_rejected_cleanly", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 " +
			"FROM t1 ORDER BY col1 + 1 DESC"
		_, qerr := db.QueryContext(ctx, q)
		if qerr == nil {
			t.Fatal("computed non-selected ORDER BY over projected EXISTS must be rejected " +
				"cleanly, not silently mis-ordered")
		}
		if !strings.Contains(qerr.Error(), "projected EXISTS in this query shape is not yet supported") {
			t.Fatalf("expected the §8 guard's clean unsupported message, got: %v", qerr)
		}
	})

	// The rejection is narrow: a SELECTED computed expression, ordered by its alias, still
	// folds and orders for real.
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
