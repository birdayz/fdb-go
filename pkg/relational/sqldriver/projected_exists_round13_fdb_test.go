package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// TestFDB_ProjectedExistsRound13_NestedSubqueryBoundary pins the RFC-141 R4
// round-13 convergence fix: the structural parse-tree EXISTS detectors must STOP
// at nested-subquery boundaries. An EXISTS that belongs to a nested scalar /
// IN / derived-table subquery's OWN clause is classified in that subquery's own
// translation context — it must NOT be mis-attributed to the OUTER expression and
// over-rejected.
//
// The regression: round-12's structural EXISTS detectors recursed into nested
// subqueries, so
//
//	SELECT id, (SELECT MAX(id) FROM t2 WHERE EXISTS (SELECT 1 FROM t3)) FROM t1
//
// was falsely rejected with "projected EXISTS in this query shape is not yet
// supported" — the outer projection's detector descended into the scalar subquery
// and saw the inner EXISTS. After the boundary stop the query plans and returns
// correct rows. Controls prove the genuine outer-level rejections (round-12) still
// fire — only nested-subquery EXISTS stops being mis-attributed.
func TestFDB_ProjectedExistsRound13_NestedSubqueryBoundary(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_pexr13")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_pexr13")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE pexr13_tmpl "+
		"CREATE TABLE t1 (id BIGINT NOT NULL, col1 BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE t2 (id BIGINT NOT NULL, fk BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE t3 (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_pexr13/s WITH TEMPLATE pexr13_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_pexr13?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// t1: rows 1,2,3. t2: one row with fk=2 (so EXISTS(t2.fk=t1.id) is true only
	// for t1.id=2). t3: two rows (so a non-correlated EXISTS(SELECT 1 FROM t3) is
	// always true, and MAX(id) over t2 = 100).
	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 10), (2, 20), (3, 30)")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 2)")
	mustExec(t, db, ctx, "INSERT INTO t3 VALUES (500, 1), (501, 2)")

	assertRejected := func(t *testing.T, q, wantMsg string) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err == nil {
			n := 0
			for rows.Next() {
				n++
			}
			rows.Close()
			t.Fatalf("query %q returned %d rows instead of a clean error", q, n)
		}
		if !strings.Contains(err.Error(), wantMsg) {
			t.Fatalf("query %q: expected clean rejection containing %q, got: %v", q, wantMsg, err)
		}
	}

	// --- THE round-13 regression: a scalar subquery whose OWN WHERE has an EXISTS,
	// used as a projection item. Must PLAN and return correct rows (the inner
	// scalar subquery is uncorrelated: MAX(id) over t2 = 100 for every outer row,
	// since EXISTS(SELECT 1 FROM t3) is true). ---
	t.Run("scalar_subquery_where_exists_in_projection", func(t *testing.T) {
		q := "SELECT id, (SELECT MAX(id) FROM t2 WHERE EXISTS (SELECT 1 FROM t3)) FROM t1 ORDER BY id"
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("round-13 query rejected (regression): %v", err)
		}
		defer rows.Close()
		type row struct {
			id  int64
			max sql.NullInt64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.max); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, r)
		}
		if len(got) != 3 {
			t.Fatalf("got %d rows, want 3: %+v", len(got), got)
		}
		for _, r := range got {
			if !r.max.Valid || r.max.Int64 != 100 {
				t.Fatalf("id=%d: scalar subquery = %v, want 100 (MAX(id) over t2, EXISTS(t3) true)", r.id, r.max)
			}
		}
		if got[0].id != 1 || got[1].id != 2 || got[2].id != 3 {
			t.Fatalf("ids = %d,%d,%d want 1,2,3", got[0].id, got[1].id, got[2].id)
		}
	})

	// --- Variant: a scalar subquery's WHERE-EXISTS ALONGSIDE a top-level projected
	// EXISTS in the SAME outer SELECT. The outer projected EXISTS is folded
	// normally; the scalar subquery's inner EXISTS is the subquery's concern. ---
	t.Run("scalar_subquery_exists_plus_top_level_projected_exists", func(t *testing.T) {
		q := "SELECT id, " +
			"EXISTS (SELECT 1 FROM t2 WHERE t2.fk = t1.id) AS has_t2, " +
			"(SELECT MAX(id) FROM t2 WHERE EXISTS (SELECT 1 FROM t3)) AS mx " +
			"FROM t1 ORDER BY id"
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("scalar-subquery-EXISTS + top-level projected EXISTS rejected (regression): %v", err)
		}
		defer rows.Close()
		type row struct {
			id    int64
			hasT2 bool
			mx    sql.NullInt64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.hasT2, &r.mx); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, r)
		}
		if len(got) != 3 {
			t.Fatalf("got %d rows, want 3: %+v", len(got), got)
		}
		// has_t2 is true only for id=2; mx=100 for every row.
		for _, r := range got {
			wantHas := r.id == 2
			if r.hasT2 != wantHas {
				t.Fatalf("id=%d: has_t2=%v want %v", r.id, r.hasT2, wantHas)
			}
			if !r.mx.Valid || r.mx.Int64 != 100 {
				t.Fatalf("id=%d: mx=%v want 100", r.id, r.mx)
			}
		}
	})

	// --- Variant: a derived-table / FROM-subquery whose OWN WHERE has an EXISTS.
	// The EXISTS belongs to the derived table's body, planned in its own context;
	// the outer query reads the derived table's output. ---
	t.Run("derived_table_where_exists", func(t *testing.T) {
		// The derived table selects t1 rows for which EXISTS(t2.fk=t1.id) — i.e.
		// only id=2 — then the outer query reads its id.
		q := "SELECT d.id FROM " +
			"(SELECT id FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.fk = t1.id)) AS d " +
			"ORDER BY d.id"
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("derived-table WHERE-EXISTS rejected (regression): %v", err)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, v)
		}
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		if len(got) != 1 || got[0] != 2 {
			t.Fatalf("derived-table WHERE-EXISTS: got %v want [2]", got)
		}
	})

	// --- Variant: a scalar subquery whose WHERE has a BURIED CASE-EXISTS. The
	// scalar subquery itself may reject it in its OWN context — assert that's the
	// scalar subquery's behavior, consistent with running it standalone (the inner
	// WHERE buries an EXISTS in a scalar, which the per-subquery guard rejects). ---
	t.Run("scalar_subquery_buried_case_exists_rejected_in_own_context", func(t *testing.T) {
		const wantScalar = "EXISTS nested in a scalar expression is not yet supported"
		innerWhere := "CASE WHEN EXISTS (SELECT 1 FROM t3) THEN 1 ELSE 0 END = 1"

		// Standalone: the subquery's own WHERE buries an EXISTS → rejected.
		standalone := "SELECT MAX(id) FROM t2 WHERE " + innerWhere
		var standaloneErr string
		if rows, err := db.QueryContext(ctx, standalone); err != nil {
			standaloneErr = err.Error()
		} else {
			rows.Close()
			t.Fatalf("standalone buried-CASE-EXISTS subquery unexpectedly planned — expected %q", wantScalar)
		}
		if !strings.Contains(standaloneErr, wantScalar) {
			t.Fatalf("standalone buried-CASE-EXISTS: expected %q, got: %v", wantScalar, standaloneErr)
		}

		// Nested in an outer projection: SAME rejection, attributed to the inner
		// subquery's own context (the message matches the standalone run). The
		// boundary stop ensures the OUTER detector does not pre-empt it with the
		// "projected EXISTS in this query shape" message — the inner subquery's own
		// guard fires.
		nested := "SELECT id, (SELECT MAX(id) FROM t2 WHERE " + innerWhere + ") FROM t1"
		assertRejected(t, nested, wantScalar)

		// SAME inner WHERE as a DERIVED TABLE — the derived-table build path also
		// routes through the unified select-build guard, so it rejects consistently
		// (root fix: the subquery-build path now carries the WHERE-scalar-EXISTS
		// guard that the outer PlanVisitor path has).
		derived := "SELECT d.m FROM (SELECT MAX(id) AS m FROM t2 WHERE " + innerWhere + ") AS d"
		assertRejected(t, derived, wantScalar)

		// SAME inner WHERE inside an EXISTS subquery's WHERE — the EXISTS-subquery
		// build path routes through the same guard, so it rejects too. (Without the
		// unified guard the inner buried EXISTS silently folds to constant-false.)
		existsSub := "SELECT id FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE " + innerWhere + ")"
		assertRejected(t, existsSub, wantScalar)
	})

	// --- CONTROL: the genuine round-12 OUTER-level rejections must STILL fire. ---

	// A projected CASE-WHEN-EXISTS at the OUTER level — the EXISTS is at the outer
	// query's level, buried in a CASE in the SELECT list → still rejected.
	t.Run("control_outer_case_when_exists_rejected", func(t *testing.T) {
		const want = "projected EXISTS in this query shape is not yet supported"
		assertRejected(t,
			"SELECT id, CASE WHEN EXISTS (SELECT 1 FROM t2 WHERE t2.fk = t1.id) THEN 1 ELSE 0 END FROM t1",
			want)
	})

	// A WHERE NOT (NOT EXISTS(...)) at the OUTER level — a wrapped WHERE-EXISTS the
	// NLJ rule does not route as a semi-join → still rejected (round-12 P1a).
	t.Run("control_where_not_not_exists_rejected", func(t *testing.T) {
		const want = "EXISTS in this query shape is not yet supported"
		assertRejected(t,
			"SELECT id FROM t1 WHERE NOT (NOT EXISTS (SELECT 1 FROM t2 WHERE t2.fk = t1.id))",
			want)
	})

	// A WHERE buried-scalar EXISTS at the OUTER level still rejected (round-12).
	t.Run("control_where_scalar_exists_rejected", func(t *testing.T) {
		const want = "EXISTS nested in a scalar expression is not yet supported"
		assertRejected(t,
			"SELECT id FROM t1 WHERE CASE WHEN EXISTS (SELECT 1 FROM t2 WHERE t2.fk = t1.id) THEN 1 ELSE 0 END = 1",
			want)
	})

	// --- CONTROL: directly-handled shapes still work. ---
	t.Run("control_top_level_projected_exists", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.fk = t1.id) AS has_t2 FROM t1 ORDER BY id"
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("top-level projected EXISTS rejected (regression): %v", err)
		}
		defer rows.Close()
		var ids []int64
		var bs []bool
		for rows.Next() {
			var id int64
			var b bool
			if err := rows.Scan(&id, &b); err != nil {
				t.Fatalf("scan: %v", err)
			}
			ids = append(ids, id)
			bs = append(bs, b)
		}
		if len(ids) != 3 {
			t.Fatalf("got %d rows want 3", len(ids))
		}
		for i, id := range ids {
			if want := id == 2; bs[i] != want {
				t.Fatalf("id=%d has_t2=%v want %v", id, bs[i], want)
			}
		}
	})
}
