package sqldriver_test

// Extends the subquery-in-ON regression (subquery_in_on_crossproduct_fdb_test.go)
// to the negation and non-LEFT join variants — the most dangerous blind spots, any
// of which slipping past the detector would re-introduce the silent cross-product
// bug. NOT IN-subquery, and IN/scalar subqueries under RIGHT and FULL OUTER joins,
// must ALL reject cleanly with 0AF00 (never a cross product). EXISTS-in-ON stays
// supported (exists_in_on_fdb_test.go).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_SubqueryInOn_JoinTypesAndNegation(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_subq_on_jt")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_subq_on_jt")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE subq_on_jt "+
			"CREATE TABLE a (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, a_id BIGINT, w BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE d (id BIGINT NOT NULL, b_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX b_a_id ON b (a_id) "+
			"CREATE INDEX c_a_id ON c (a_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_subq_on_jt/s WITH TEMPLATE subq_on_jt")
	dsn := fmt.Sprintf("fdbsql:///testdb_subq_on_jt?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id) VALUES (1), (2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id, a_id) VALUES (10, 1), (20, 2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, a_id, w) VALUES (50, 1, 999), (51, 2, 888)")
	mwjoMustExec(t, db, ctx, "INSERT INTO d (id, b_id) VALUES (1, 999), (2, 888)")

	// NOT IN-subquery in ON — the negation must not slip past the IN detector.
	t.Run("not_in_subquery_left_on", func(t *testing.T) {
		assertUnsupported(t, db, ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id "+
			"LEFT JOIN c ON c.a_id = a.id AND c.w NOT IN (SELECT d.b_id FROM d WHERE d.id = a.id + 999)")
	})
	// non-LEFT join types must reject too (detector must not be LEFT-only).
	t.Run("in_subquery_right_on", func(t *testing.T) {
		assertUnsupported(t, db, ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id "+
			"RIGHT JOIN c ON c.a_id = a.id AND c.w IN (SELECT d.b_id FROM d WHERE d.id = a.id + 999)")
	})
	t.Run("in_subquery_full_outer_on", func(t *testing.T) {
		assertUnsupported(t, db, ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id "+
			"FULL OUTER JOIN c ON c.a_id = a.id AND c.w IN (SELECT d.b_id FROM d WHERE d.id = a.id + 999)")
	})
	t.Run("scalar_subquery_right_on", func(t *testing.T) {
		assertUnsupported(t, db, ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id "+
			"RIGHT JOIN c ON c.a_id = a.id AND c.w > (SELECT MAX(d.b_id) FROM d WHERE d.id = a.id + 999)")
	})
	t.Run("not_in_subquery_inner_on", func(t *testing.T) {
		assertUnsupported(t, db, ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id "+
			"JOIN c ON c.a_id = a.id AND c.w NOT IN (SELECT d.b_id FROM d WHERE d.id = a.id + 999)")
	})

	// Control: a NOT IN value LIST (not a subquery) in ON must still work — the
	// negation detector must not over-reject literal lists.
	t.Run("ctrl_not_in_value_list", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id "+
			"LEFT JOIN c ON c.a_id = a.id AND c.w NOT IN (111, 222)")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		got := siScanRows(t, rows)
		want := []string{"1|50", "2|51"} // both c rows have w not in {111,222}
		if !eqStrSlices(got, want) {
			t.Errorf("NOT IN-value-list LEFT JOIN rows = %v, want %v", got, want)
		}
	})
}
