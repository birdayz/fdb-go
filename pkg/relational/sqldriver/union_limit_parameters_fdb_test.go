package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestFDB_UnionLimitParameters pins the driver boundary that binds LIMIT before
// planning. An unresolved LIMIT ? OFFSET 2 would become the logical API's -1
// no-cap sentinel; bare OFFSET syntax being rejected does not rule that out.
func TestFDB_UnionLimitParameters(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const path = "/testdb_union_limit_parameters"
	setup := openTestDB(t, path)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+path)
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE union_limit_parameters CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+path+"/s WITH TEMPLATE union_limit_parameters")
	// -count repeats the test in the same cluster. Release its catalog entries
	// after closing the query DB so each iteration exercises fresh planning.
	t.Cleanup(func() {
		for _, ddl := range []string{
			"DROP SCHEMA " + path + "/s",
			"DROP SCHEMA TEMPLATE union_limit_parameters",
			"DROP DATABASE " + path,
		} {
			if _, err := setup.ExecContext(ctx, ddl); err != nil {
				t.Errorf("cleanup %s: %v", ddl, err)
			}
		}
	})
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", path, clusterFilePath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (1), (2), (3)")

	const query = "SELECT id FROM t UNION ALL SELECT id FROM t LIMIT ? OFFSET 2"
	stmt, err := db.PrepareContext(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	explain, err := db.PrepareContext(ctx, "EXPLAIN "+query)
	if err != nil {
		t.Fatal(err)
	}
	defer explain.Close()
	checkRows := func(rows *sql.Rows, want int64) {
		t.Helper()
		defer rows.Close()
		var count int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			if id < 1 || id > 3 {
				t.Fatalf("unexpected id %d", id)
			}
			count++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Fatalf("bound LIMIT %d returned %d rows", want, count)
		}
	}
	// Revisit the same SQL and binds after other caps: a reused statement or
	// cached plan must never retain an unresolved sentinel or a previous cap.
	plansByCap := make(map[int64]string)
	for _, cap := range []int64{4, 1, 3, 4} {
		rows, err := db.QueryContext(ctx, query, cap)
		if err != nil {
			t.Fatal(err)
		}
		checkRows(rows, cap)
		rows, err = stmt.QueryContext(ctx, cap)
		if err != nil {
			t.Fatal(err)
		}
		checkRows(rows, cap)
		for _, prepared := range []bool{false, true} {
			var row *sql.Row
			if prepared {
				row = explain.QueryRowContext(ctx, cap)
			} else {
				row = db.QueryRowContext(ctx, "EXPLAIN "+query, cap)
			}
			var plan string
			if err := row.Scan(&plan); err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("Limit(%d, offset=2, UnorderedUnion(", cap)
			if !strings.HasPrefix(plan, want) {
				t.Fatalf("bound LIMIT must reach planning as %q, got %s", want, plan)
			}
			if previous, seen := plansByCap[cap]; seen && plan != previous {
				t.Fatalf("same bound cap changed plan:\nfirst: %s\nnow:   %s", previous, plan)
			}
			plansByCap[cap] = plan
			t.Logf("prepared=%v cap=%d: %s", prepared, cap, plan)
		}
	}
	rows, err := stmt.QueryContext(ctx, int64(-1))
	if rows != nil {
		rows.Close()
	}
	var sqlErr *api.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != api.ErrCodeSyntaxError {
		t.Fatalf("negative bound SQL LIMIT must not expose the no-cap sentinel: got %v", err)
	}
}
