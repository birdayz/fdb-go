package sqldriver_test

// TestFDB_ExistsAggregateScopeLeak pins the fix for a PRE-EXISTING parser
// scope leak: harvestAggregates (select_parser) descended into a projected
// EXISTS subquery and promoted its aggregate into the OUTER query's aggregate
// set, wrongly classifying the outer query as an aggregate → a spurious 42803
// "column must appear in GROUP BY" on the outer's non-grouped columns. It
// already guarded scalar `(SELECT ...)` subqueries; the fix extends the same
// boundary to EXISTS subqueries.
//
// This is the KEYSTONE for the correlated EXISTS-over-aggregate cardinality
// fix (which cannot trust the aggregate set until this leak is closed).

import (
	"context"
	"database/sql"
	"testing"
)

func TestFDB_ExistsAggregateScopeLeak(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/testdb_exists_agg_scope_leak"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE eagl_tmpl"+
		" CREATE TABLE p (id BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE e (eid BIGINT, eref BIGINT, PRIMARY KEY (eid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE eagl_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		"INSERT INTO p VALUES (1), (2), (3)",
		"INSERT INTO e VALUES (301, 1)", // e non-empty
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}

	// The keystone: a projected EXISTS-of-aggregate must NOT make the outer
	// query an aggregate query. An UNCORRELATED aggregate EXISTS is fully
	// correct after the leak fix — the non-correlated path keeps the aggregate
	// (always one row) → EXISTS true for every p → [[1 t][2 t][3 t]].
	t.Run("uncorrelated_projected_no_42803", func(t *testing.T) {
		rows, qErr := db.QueryContext(ctx, "SELECT p.id, EXISTS (SELECT COUNT(*) FROM e) FROM p ORDER BY p.id")
		if qErr != nil {
			t.Fatalf("projected EXISTS-of-aggregate still errors (scope leak not closed): %v", qErr)
		}
		defer rows.Close()
		var got []struct {
			id int64
			ex bool
		}
		for rows.Next() {
			var id int64
			var ex sql.NullBool
			if sErr := rows.Scan(&id, &ex); sErr != nil {
				t.Fatalf("scan: %v", sErr)
			}
			got = append(got, struct {
				id int64
				ex bool
			}{id, ex.Valid && ex.Bool})
		}
		want := []struct {
			id int64
			ex bool
		}{{1, true}, {2, true}, {3, true}}
		if len(got) != len(want) {
			t.Fatalf("uncorrelated_projected: got %v want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("uncorrelated_projected: got %v want %v", got, want)
			}
		}
	})

	// A projected scalar subquery over an aggregate must STILL be scoped
	// correctly (the pre-existing scalar guard) — the outer stays non-aggregate.
	t.Run("scalar_subquery_still_scoped", func(t *testing.T) {
		rows, qErr := db.QueryContext(ctx, "SELECT p.id, (SELECT COUNT(*) FROM e) FROM p ORDER BY p.id")
		if qErr != nil {
			t.Fatalf("projected scalar-of-aggregate regressed: %v", qErr)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id, c int64
			if sErr := rows.Scan(&id, &c); sErr != nil {
				t.Fatalf("scan: %v", sErr)
			}
			ids = append(ids, id)
		}
		if len(ids) != 3 {
			t.Fatalf("scalar_subquery: got %v want 3 rows", ids)
		}
	})

	// A REAL outer aggregate must still be detected (the leak fix must not
	// weaken structural aggregate detection).
	t.Run("real_outer_aggregate_still_detected", func(t *testing.T) {
		var c int64
		if sErr := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM p").Scan(&c); sErr != nil {
			t.Fatalf("COUNT(*) regressed: %v", sErr)
		}
		if c != 3 {
			t.Fatalf("COUNT(*) = %d want 3", c)
		}
		// A non-grouped column alongside a real aggregate MUST still 42803.
		_, e2 := db.QueryContext(ctx, "SELECT p.id, COUNT(*) FROM p")
		if e2 == nil {
			t.Fatalf("SELECT p.id, COUNT(*) FROM p should still 42803 (grouping error)")
		}
		if !contains42803GL(e2.Error()) {
			t.Fatalf("want 42803 for a real ungrouped aggregate, got %v", e2)
		}
	})
}

func contains42803GL(s string) bool {
	for i := 0; i+5 <= len(s); i++ {
		if s[i:i+5] == "42803" {
			return true
		}
	}
	return false
}
