package sqldriver_test

// A projection-less star over ROW-VERSIONED tables, read through a derived
// table or a CTE: NEGATIVE pins of the shapes booked in TODO.md ("Exact
// quantifier binding over a CTE or derived body"), beside the ORDER BY that
// answers. They live here rather than in derived_star_row_versions.yaml
// because their failures are not SQLSTATEs the corpus may credit as correct
// rejections: the star-join WHERE is refused at execution with a raw
// resolution error (`edge lookup D` — the row-version rewrite has produced
// Java's explicit projection, which declares its slots by the leg-qualified
// datum key AA.ID while the derived scope's catalog walk publishes the bare
// names), and the CTE spelling of a star over a lateral unnest fails as an
// XX000 planner failure (the rewrite carries the outer X beside the element X
// while the unnest scope shadows it). When either answers, its pin turns red
// and is re-pinned to the rows.

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
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_dsrv?cluster_file=%s&schema=s1", clusterFilePath))
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

	// The booked refusal. The message names the two layouts of D; a WHERE that
	// answers here means the derived star join's quantifier is bound by the
	// row the body flows — re-pin to the rows [10] and close the TODO entry.
	const where = "SELECT d.y FROM (SELECT * FROM aa, bb) d WHERE d.y = 10"
	ys, err = readY(t, where)
	if err == nil {
		t.Fatalf("%q answered %v; the booked row-versioned star-join WHERE now binds — re-pin it to the rows", where, ys)
	}
	if !strings.Contains(err.Error(), "edge lookup D") {
		t.Fatalf("%q failed for a different reason than the booked layout mismatch: %v", where, err)
	}
}

// The CTE spelling of a star over a lateral unnest in a row-versioned table:
// the derived spelling is pinned as 0AF00 in derived_star_row_versions.yaml;
// this one fails inside the planner, which the driver reports as XX000 with
// the detail withheld, and a code the corpus would credit as a correct
// rejection is pinned here instead. A CTE that answers [7],[8] here means the
// rewrite and the unnest scope agree on the row — re-pin to the rows and
// close that half of the TODO entry.
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
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_dsrvu?cluster_file=%s&schema=s1", clusterFilePath))
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
	if !strings.Contains(err.Error(), "XX000") {
		t.Fatalf("%q failed for a different reason than the booked planner failure: %v", cte, err)
	}
}
