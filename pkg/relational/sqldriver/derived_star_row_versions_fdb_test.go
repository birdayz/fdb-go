package sqldriver_test

// A projection-less star over ROW-VERSIONED tables, read through a derived
// table or a CTE, beside the ORDER BY that answers. The star-join WHERE arm
// below is re-pinned when its exact binding lands. The UNNEST arm retains both
// visible X labels (the table column and scalar element), so D.X is a semantic
// 42702 rather than a planner failure or a shadow-preferred answer.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_DerivedStarRowVersionsWhere(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dsrv")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dsrv")
	mwjoMustExec(t, setup, ctx, `CREATE SCHEMA TEMPLATE dsrv_tpl
		CREATE TABLE aa (id BIGINT, y BIGINT, PRIMARY KEY (id))
		CREATE TABLE bb (id BIGINT, z BIGINT, PRIMARY KEY (id))
		WITH OPTIONS(store_row_versions=true)`)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dsrv/s1 WITH TEMPLATE dsrv_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///TESTDB_DSRV?cluster_file=%s&schema=S1", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO aa VALUES (1, 20), (2, 10)")
	mwjoMustExec(t, db, ctx, "INSERT INTO bb VALUES (1, 3)")

	readY := func(t *testing.T, query string) ([]int64, error) {
		t.Helper()
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var ys []int64
		for rows.Next() {
			var y int64
			if err := rows.Scan(&y); err != nil {
				return nil, err
			}
			ys = append(ys, y)
		}
		return ys, rows.Err()
	}

	// The control: the same derived star join answers under ORDER BY.
	ys, err := readY(t, "SELECT d.y FROM (SELECT * FROM aa, bb) d ORDER BY d.y")
	if err != nil || len(ys) != 2 || ys[0] != 10 || ys[1] != 20 {
		t.Fatalf("ORDER BY over the derived star join: ys=%v err=%v, want [10 20]", ys, err)
	}

	// The filtered twin binds the same exact derived row and selects only y=10.
	const where = "SELECT d.y FROM (SELECT * FROM aa, bb) d WHERE d.y = 10"
	ys, err = readY(t, where)
	if err != nil || len(ys) != 1 || ys[0] != 10 {
		t.Fatalf("%q: ys=%v err=%v, want [10]", where, ys, err)
	}
}

// The CTE spelling of a star over a lateral unnest in a row-versioned table:
// the derived spelling is pinned as 0AF00 in derived_star_row_versions.yaml;
// this one is semantically ambiguous: THINGS.X and the scalar element X are
// both visible outputs of the star body. Pin 42702 rather than permitting a
// shadow-preferred answer.
func TestFDB_DerivedStarRowVersionsUnnestCTE(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dsrvu")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dsrvu")
	mwjoMustExec(t, setup, ctx, `CREATE SCHEMA TEMPLATE dsrvu_tpl
		CREATE TABLE things (id BIGINT, x BIGINT, arr BIGINT ARRAY, PRIMARY KEY (id))
		WITH OPTIONS(store_row_versions=true)`)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dsrvu/s1 WITH TEMPLATE dsrvu_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///TESTDB_DSRVU?cluster_file=%s&schema=S1", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO things VALUES (1, 5, [7, 8])")

	const cte = "WITH d AS (SELECT * FROM things, things.arr AS x) SELECT d.x FROM d"
	rows, err := db.QueryContext(ctx, cte)
	if err == nil {
		rows.Close()
		t.Fatalf("%q planned; the booked row-versioned unnest star now agrees on its row — re-pin it to [7],[8]", cte)
	}
	if !strings.Contains(err.Error(), "42702") {
		t.Fatalf("%q error = %v, want semantic ambiguity 42702", cte, err)
	}
}
