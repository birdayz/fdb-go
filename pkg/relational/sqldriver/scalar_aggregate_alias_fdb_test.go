package sqldriver_test

// Column-alias pin for SCALAR (ungrouped) aggregates — found by the RFC-182
// rowdiff harness once its generator learned GROUP BY / aggregates.
//
// A GROUPED aggregate keeps a projection above it that carries the output
// aliases, so `SELECT b AS g, MAX(a) AS agg … GROUP BY b` reported [G AGG]
// correctly. A SCALAR aggregate plans to the bare `StreamingAgg(keys=[], …)`
// with no projection, and buildAggColumns named the column from the
// expression while ignoring AggregateSpec.Alias — so `SELECT MAX(a) AS agg
// FROM t` reported the column as `MAX(A)`. Rows were always correct; only
// the metadata was wrong, the same class as the LIMIT-through-projection
// alias drop.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_ScalarAggregate_KeepsAlias(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const dbPath = "/testdb_aggalias"
	setup := openTestDB(t, dbPath)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE aggalias CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id)) CREATE INDEX idx_b ON t (b)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE aggalias")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (1,5,1),(2,7,1),(3,9,2)")

	for _, tc := range []struct {
		name     string
		query    string
		wantCols []string
		wantLast int64
	}{
		{"scalar_max_aliased", "SELECT MAX(a) AS agg FROM t", []string{"AGG"}, 9},
		{"scalar_count_star_aliased", "SELECT COUNT(*) AS agg FROM t", []string{"AGG"}, 3},
		{"scalar_sum_aliased", "SELECT SUM(a) AS total FROM t", []string{"TOTAL"}, 21},
		// Control: a GROUPED aggregate already worked (projection carries it).
		{"grouped_aliased", "SELECT b AS g, MAX(a) AS agg FROM t GROUP BY b WHERE b = 2", nil, 0},
		// Control: with NO alias the generated expression name is correct.
		{"scalar_unaliased_keeps_expression_name", "SELECT MAX(a) FROM t", []string{"MAX(A)"}, 9},
	} {
		tc := tc
		if tc.wantCols == nil {
			continue // the grouped control is asserted separately below
		}
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows, err := db.QueryContext(ctx, tc.query)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()
			cols, err := rows.Columns()
			if err != nil {
				t.Fatalf("columns: %v", err)
			}
			if len(cols) != len(tc.wantCols) || cols[0] != tc.wantCols[0] {
				t.Fatalf("columns = %v, want %v", cols, tc.wantCols)
			}
			if !rows.Next() {
				t.Fatal("no row")
			}
			var got int64
			if err := rows.Scan(&got); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if got != tc.wantLast {
				t.Errorf("value = %d, want %d", got, tc.wantLast)
			}
		})
	}

	t.Run("grouped_aliased_control", func(t *testing.T) {
		t.Parallel()
		rows, err := db.QueryContext(ctx, "SELECT b AS g, MAX(a) AS agg FROM t GROUP BY b")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns: %v", err)
		}
		if len(cols) != 2 || cols[0] != "G" || cols[1] != "AGG" {
			t.Fatalf("grouped columns = %v, want [G AGG]", cols)
		}
	})
}
