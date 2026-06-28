package sqldriver_test

// Probes IN (subquery): both engines reject `x IN (SELECT ...)` — Java's
// visitInPredicate asserts ctx.inList().queryExpressionBody()==null with
// UNSUPPORTED_QUERY ("IN predicate does not support nested SELECT"); Go returns
// 0AF00. The supported alternative is a correlated EXISTS, which works (and
// sidesteps the NOT-IN-NULL trap since EXISTS has clean 2VL semantics).

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_InSubqueryProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_insubqp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_insubqp")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE insubqp "+
		"CREATE TABLE outr (id BIGINT NOT NULL, x BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE inr (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_insubqp/s WITH TEMPLATE insubqp")
	dsn := fmt.Sprintf("fdbsql:///testdb_insubqp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO outr (id, x) VALUES (1,1),(2,2),(3,3)")
	mwjoMustExec(t, db, ctx, "INSERT INTO inr (id, v) VALUES (1,1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO inr (id) VALUES (2)") // v NULL

	rejected := func(name, q string) {
		t.Run(name, func(t *testing.T) {
			_, err := db.QueryContext(ctx, q)
			if err == nil || !strings.Contains(err.Error(), "0AF00") {
				t.Errorf("%s error = %v, want 0AF00 (IN-subquery unsupported, use EXISTS)", name, err)
			}
		})
	}
	rejected("in_subquery_rejected", "SELECT id FROM outr WHERE x IN (SELECT v FROM inr)")
	rejected("not_in_subquery_rejected", "SELECT id FROM outr WHERE x NOT IN (SELECT v FROM inr)")

	t.Run("exists_alternative_works", func(t *testing.T) {
		// the supported way to express the IN-subquery semijoin: correlated EXISTS.
		rows, err := db.QueryContext(ctx,
			"SELECT id FROM outr WHERE EXISTS (SELECT 1 FROM inr WHERE inr.v = outr.x)")
		if err != nil {
			t.Fatalf("EXISTS alternative failed: %v", err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			out = append(out, v)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		// x=1 matches inr.v=1; x=2,3 don't match (v NULL doesn't equal anything).
		if len(out) != 1 || out[0] != 1 {
			t.Errorf("EXISTS semijoin = %v, want [1]", out)
		}
	})
	t.Run("not_exists_alternative_works", func(t *testing.T) {
		// NOT EXISTS — the clean-2VL complement (no NOT-IN-NULL trap).
		rows, err := db.QueryContext(ctx,
			"SELECT id FROM outr WHERE NOT EXISTS (SELECT 1 FROM inr WHERE inr.v = outr.x)")
		if err != nil {
			t.Fatalf("NOT EXISTS alternative failed: %v", err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			out = append(out, v)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		// x=2,3 have no matching inr.v (NULL never matches) → returned cleanly (not the trap).
		if len(out) != 2 || out[0] != 2 || out[1] != 3 {
			t.Errorf("NOT EXISTS = %v, want [2 3] (clean 2VL, unlike the NOT-IN-NULL trap)", out)
		}
	})
}
