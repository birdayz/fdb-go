package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFDB_ProjectionResultTypeProbe pins projected EXISTS over a derived source
// — a CTE and a derived table — against the controls that differ ONLY by
// removing the projected EXISTS, and against the base-table twin.
//
// Both projected arms used to FAIL, and they failed for two UNRELATED causes
// that the shared symptom hid: the CTE arm never resolved `c.id` inside the
// subquery (42703, a scope-registration gap that reproduced just as well with
// the EXISTS in the WHERE), and the derived arm resolved fine and then died in
// the executor because the join's step-1 ordinal seed refused a projection leg.
//
// Every arm asserts ROWS, including the EXISTS column by value. A control whose
// own failure is swallowed witnesses nothing for the experiment beside it, and a
// test that cannot fail is the dominant false positive in this tree.
func TestFDB_ProjectionResultTypeProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_projrestype")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_projrestype")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE projrestype_tmpl "+
		"CREATE TABLE t1(id BIGINT, v BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT, t1_id BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t3(id BIGINT, t1_id BIGINT, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_projrestype/s WITH TEMPLATE projrestype_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_projrestype?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 10), (2, 20), (3, 30)")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 1), (200, 3)")
	mustExec(t, db, ctx, "INSERT INTO t3 VALUES (900, 1), (901, 2), (902, 3)")

	// requireRows asserts the query returns exactly want, as "a|b" per row in
	// order. A control that silently started erroring must go RED here — a
	// control whose failure is swallowed cannot witness anything for the
	// experiment beside it.
	requireRows := func(t *testing.T, label, q string, want []string) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: unexpected query error: %T: %v", label, err, err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var a, b int64
			if err := rows.Scan(&a, &b); err != nil {
				t.Fatalf("%s: scan: %v", label, err)
			}
			got = append(got, fmt.Sprintf("%d|%d", a, b))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: rows.Err: %T: %v", label, err, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: got %d row(s) %v, want %d %v", label, len(got), got, len(want), want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: row %d = %s, want %s", label, i, got[i], want[i])
			}
		}
	}

	// requireRows3 is requireRows for the three-column projected-EXISTS arms:
	// the EXISTS column is what the two defects destroyed, so it is asserted by
	// VALUE and not merely by the query not erroring.
	requireRows3 := func(t *testing.T, label, q string, want []string) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: unexpected query error: %T: %v", label, err, err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var a, b int64
			var h bool
			if err := rows.Scan(&a, &b, &h); err != nil {
				t.Fatalf("%s: scan: %v", label, err)
			}
			got = append(got, fmt.Sprintf("%d|%d|%t", a, b, h))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: rows.Err: %T: %v", label, err, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: got %d row(s) %v, want %d %v", label, len(got), got, len(want), want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: row %d = %s, want %s", label, i, got[i], want[i])
			}
		}
	}

	// THE CONTROLS ARE THE POINT. Each failing arm has a twin differing ONLY by
	// removing the projected EXISTS, and both twins pass. So it is the
	// projected-EXISTS fold that loses the derived source, not the derived
	// source and not the join.
	want := []string{"1|1", "2|2", "3|3"}

	requireRows(t, "cte_control", `
		WITH c AS (SELECT id, v FROM t1)
		SELECT c.id, t1_id FROM c, t3 WHERE t3.t1_id = c.id ORDER BY c.id`, want)

	// t2 holds (100,1) and (200,3), so the EXISTS is true for t1 ids 1 and 3 and
	// false for 2 — a column with BOTH values, because an EXISTS that is constant
	// across the result cannot witness a wrong-slot read.
	want3 := []string{"1|1|true", "2|2|false", "3|3|true"}

	// The CTE arm: the outer scope of a correlated subquery now registers the
	// enclosing query's WITH legs, so `c.id` inside the EXISTS resolves. It used
	// to fail 42703 "no FROM source aliased as C" — and so did the same query
	// with the EXISTS moved into the WHERE, which is what showed the defect was
	// the outer-scope registration and not the projection.
	requireRows3(t, "cte_exists", `
		WITH c AS (SELECT id, v FROM t1)
		SELECT c.id, t1_id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = c.id) AS h
		FROM c, t3 WHERE t3.t1_id = c.id ORDER BY c.id`, want3)

	requireRows(t, "derived_control", `
		SELECT d.id, t1_id FROM (SELECT id, v FROM t1) AS d, t3
		WHERE t3.t1_id = d.id ORDER BY d.id`, want)

	// The derived-table arm, whose cause is a DIFFERENT one: the projected-EXISTS
	// fold's step-1 ordinal seed refused a PROJECTION leg, so the materialized
	// join kept the folded projection as its own merged row and the correlated
	// read died at execution with "multi-leg row cannot serve a source-relative
	// ordinal". The base-table twin (two scan legs) always worked.
	requireRows3(t, "derived_exists", `
		SELECT d.id, t1_id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = d.id) AS h
		FROM (SELECT id, v FROM t1) AS d, t3
		WHERE t3.t1_id = d.id ORDER BY d.id`, want3)

	// The base-table twin of the arm above, differing ONLY in that its leg is a
	// scan rather than a projection. It passed throughout, which is what
	// localized the defect to the leg shape.
	requireRows3(t, "table_exists_control", `
		SELECT t1.id, t3.t1_id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h
		FROM t1, t3 WHERE t3.t1_id = t1.id ORDER BY t1.id`, want3)

	// The SAME correlation with the EXISTS in the WHERE. For the CTE it is the
	// arm that showed the 42703 was a scope-registration gap and not a
	// projection one; for the derived table it is the shape a projection stating
	// its row took the plan away from entirely, because the orientation gate read
	// the seed's unstated field types as a leg mismatch. Only t1 ids 1 and 3 have
	// a t2 row, so the EXISTS filter has to remove id 2 — a filter that dropped
	// nothing would pass here with the join wrong.
	whereWant := []string{"1|1", "3|3"}

	requireRows(t, "cte_where_exists", `
		WITH c AS (SELECT id, v FROM t1)
		SELECT c.id, t1_id FROM c, t3
		WHERE t3.t1_id = c.id AND EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = c.id)
		ORDER BY c.id`, whereWant)

	requireRows(t, "derived_where_exists", `
		SELECT d.id, t1_id FROM (SELECT id, v FROM t1) AS d, t3
		WHERE t3.t1_id = d.id AND EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = d.id)
		ORDER BY d.id`, whereWant)
}
