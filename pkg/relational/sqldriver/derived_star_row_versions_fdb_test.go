package sqldriver_test

// A projection-less star over a join of ROW-VERSIONED tables, read through a
// derived table: the ORDER BY answers and the WHERE is refused at execution.
// The refusal is a raw resolution error the driver does not code, so it is
// pinned here rather than in derived_star_row_versions.yaml (whose error pins
// need an SQLSTATE). It is a NEGATIVE pin of a shape booked in TODO.md ("Exact
// quantifier binding over a CTE or derived body"): the row-version rewrite has
// already produced Java's explicit projection, and that projection declares
// its slots by the leg-qualified datum key (AA.ID) while the derived scope's
// catalog walk publishes the bare names, so the edge D is read under one
// layout and declared under another. When the WHERE answers, this pin turns
// red and is re-pinned to the rows.

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
