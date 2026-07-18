package sqldriver_test

// A sort key's binding universe splits on the key's SHAPE (SortKey.BareRef —
// the RFC-180 round-14 rule): a BARE user identifier binds alias-preferred
// output names; a RENDERED item (`ORDER BY SUM(score)`) binds the PROJECTION
// text only. Over the IMMEDIATE reshaping strip, translateSort's flat-key
// arm matched alias-preferred names for BOTH shapes, so an output column
// literally labeled "SUM(SCORE)" (a colliding alias) captured the
// aggregate's sort key and the query silently sorted by the WRONG column
// (player DESC instead of SUM DESC). The deferred strip had the split
// already (aggregate_order_by_java collision pin); this is the immediate
// strip's twin.

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"
)

func TestFDB_SortKeyLabelCollision_ImmediateStrip(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_sklc")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_sklc")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE sklc CREATE TABLE scores (id BIGINT, player STRING, score BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_sklc/s WITH TEMPLATE sklc")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_sklc?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO scores VALUES (1,'zed',10),(2,'amy',90),(3,'moe',50)")

	sums := func(t *testing.T, q string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("must ANSWER: %v\n  sql: %s", err, q)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var a string
			var b int64
			if err := rows.Scan(&a, &b); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, fmt.Sprintf("%s:%d", a, b))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return out
	}

	// The collision: an alias spelled like the rendered aggregate must NOT
	// capture the rendered-item sort key.
	t.Run("colliding_alias", func(t *testing.T) {
		got := sums(t, `SELECT player AS "SUM(SCORE)", SUM(score) AS s2 FROM scores GROUP BY player ORDER BY SUM(score) DESC`)
		want := []string{"amy:90", "moe:50", "zed:10"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("= %v, want %v (a colliding alias captured the aggregate's sort key)", got, want)
		}
	})
	// Control: no collision, same query shape.
	t.Run("no_collision_control", func(t *testing.T) {
		got := sums(t, `SELECT player AS p2, SUM(score) AS s2 FROM scores GROUP BY player ORDER BY SUM(score) DESC`)
		want := []string{"amy:90", "moe:50", "zed:10"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("= %v, want %v", got, want)
		}
	})
	// A BARE alias key still binds the alias (the BareRef half of the split).
	t.Run("bare_alias_key_binds_alias", func(t *testing.T) {
		got := sums(t, `SELECT player AS p2, SUM(score) AS s2 FROM scores GROUP BY player ORDER BY s2 DESC`)
		want := []string{"amy:90", "moe:50", "zed:10"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("= %v, want %v (ORDER BY <alias> must bind the aliased output)", got, want)
		}
	})
	// The DESC->ASC twin of the collision.
	t.Run("colliding_alias_asc", func(t *testing.T) {
		got := sums(t, `SELECT player AS "SUM(SCORE)", SUM(score) AS s2 FROM scores GROUP BY player ORDER BY SUM(score)`)
		want := []string{"zed:10", "moe:50", "amy:90"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("= %v, want %v", got, want)
		}
	})
}
