package sqldriver_test

// Probes for DML predicate correctness: UPDATE/DELETE/INSERT-SELECT whose WHERE
// includes arithmetic, IN-lists, and CASE. A wrong predicate here mutates the
// wrong rows (data corruption), so each step verifies the resulting table state.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_DMLPredicateProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dml_pred")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dml_pred")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE dml_pred "+
			"CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, grp STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dml_pred/s WITH TEMPLATE dml_pred")
	dsn := fmt.Sprintf("fdbsql:///testdb_dml_pred?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v, grp) VALUES (1, 10, 'A'), (2, 20, 'A'), (3, 30, 'B'), (4, 40, 'B')")

	// snapshot returns id->v for the whole table as a stable string.
	snapshot := func() string {
		rows, err := db.QueryContext(ctx, "SELECT id, v FROM t ORDER BY id")
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		defer rows.Close()
		var parts []string
		for rows.Next() {
			var id, v int64
			if err := rows.Scan(&id, &v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			parts = append(parts, fmt.Sprintf("%d:%d", id, v))
		}
		sort.Strings(parts)
		return fmt.Sprintf("%v", parts)
	}

	// UPDATE with a grp predicate.
	mwjoMustExec(t, db, ctx, "UPDATE t SET v = v + 1 WHERE grp = 'A'")
	if got, want := snapshot(), "[1:11 2:21 3:30 4:40]"; got != want {
		t.Fatalf("after UPDATE grp=A: %s, want %s", got, want)
	}

	// UPDATE with an arithmetic predicate (v in (30,40) → id3,id4).
	mwjoMustExec(t, db, ctx, "UPDATE t SET v = v * 2 WHERE v >= 30")
	if got, want := snapshot(), "[1:11 2:21 3:60 4:80]"; got != want {
		t.Fatalf("after UPDATE v>=30: %s, want %s", got, want)
	}

	// UPDATE with a CASE predicate (single-table): rows where CASE WHEN v>50 THEN 1 ELSE 0 END = 1 → id3,id4.
	mwjoMustExec(t, db, ctx, "UPDATE t SET grp = 'C' WHERE CASE WHEN v > 50 THEN 1 ELSE 0 END = 1")
	rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE grp = 'C' ORDER BY id")
	if err != nil {
		t.Fatalf("select grp=C: %v", err)
	}
	var cIDs []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		cIDs = append(cIDs, id)
	}
	rows.Close()
	if fmt.Sprintf("%v", cIDs) != "[3 4]" {
		t.Fatalf("grp=C after CASE update = %v, want [3 4]", cIDs)
	}

	// DELETE with an IN-list predicate.
	mwjoMustExec(t, db, ctx, "DELETE FROM t WHERE id IN (1, 3)")
	if got, want := snapshot(), "[2:21 4:80]"; got != want {
		t.Fatalf("after DELETE IN(1,3): %s, want %s", got, want)
	}

	// INSERT ... SELECT from the same table (id offset).
	mwjoMustExec(t, db, ctx, "INSERT INTO t SELECT id + 100, v + 1, grp FROM t WHERE v > 50")
	if got, want := snapshot(), "[104:81 2:21 4:80]"; got != want {
		t.Fatalf("after INSERT...SELECT: %s, want %s", got, want)
	}
}
