package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_ProjectedExists_OrderByLimit pins RFC-141 Phase 2 P1: a projected
// EXISTS in the SELECT list combined with an intervening ORDER BY / LIMIT.
//
// Root cause of the pre-fix bug: the builder emits Project(Sort(Filter)) —
// the existential filter is NOT the project's direct input; an ORDER BY (and,
// hoisted above the project, a LIMIT) sits between them. The old fold guard
// only matched a project whose DIRECT input was the existential filter, so the
// ORDER BY case fell through to the ordinary projection path. The projected
// ExistsValue was then evaluated by a Map ABOVE the FlatMap — after the
// existential binding (q1) was already gone — so every row read false.
//
// Fix: the fold sees THROUGH the intervening sort/limit, folds the projection
// into the existential SelectExpression (boolean computed with the binding
// live), and re-applies the sort/limit on top — matching Java's
// generateSort(generateSimpleSelect(output...), orderBys). The plan must put
// the sort ABOVE the existential FlatMap, and the boolean must be correct per
// row in the requested order. Before the fix: false for matching rows.
func TestFDB_ProjectedExists_OrderByLimit(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_projexists_ob")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_projexists_ob")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE projexists_ob_tmpl "+
		"CREATE TABLE t1(id BIGINT, col1 BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT, t1_id BIGINT, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_projexists_ob/s WITH TEMPLATE projexists_ob_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_projexists_ob?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// t1: ids 1..5. t2 references t1 ids {1,3,5} → those rows match, 2 and 4 do
	// not. The match set is interleaved so a constant-false bug (or a
	// constant-true bug) is impossible to pass by coincidence.
	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 10), (2, 20), (3, 30), (4, 40), (5, 50)")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 1), (200, 3), (300, 5)")

	type idBool struct {
		id int64
		b  bool
	}
	queryIDBoolOrdered := func(t *testing.T, q string) []idBool {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []idBool
		for rows.Next() {
			var r idBool
			if err := rows.Scan(&r.id, &r.b); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		return out
	}
	// requireSortOverFlatMap asserts the plan keeps the sort ABOVE the
	// existential FlatMap (the fixed shape: fold the projection, sort on top).
	// A FlatMap must be present (the existential probe fired) and a sort node
	// must wrap it.
	requireSortOverFlatMap := func(t *testing.T, q string) {
		t.Helper()
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN %q: %v", q, err)
		}
		if !strings.Contains(plan, "FlatMap") {
			t.Errorf("expected FlatMap (existential probe) in plan for %q, got:\n%s", q, plan)
		}
		if !strings.Contains(plan, "FirstOrDefault") {
			t.Errorf("expected FirstOrDefault (existential one-row inner) in plan for %q, got:\n%s", q, plan)
		}
		if !strings.Contains(plan, "Sort") {
			t.Errorf("expected a Sort node above the existential FlatMap for %q, got:\n%s", q, plan)
		}
	}

	// Case 1: projected EXISTS + ORDER BY id ASC. Rows must arrive in ascending
	// id order with the correct boolean per row.
	t.Run("orderby_asc", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1 ORDER BY id"
		requireSortOverFlatMap(t, q)
		got := queryIDBoolOrdered(t, q)
		want := []idBool{{1, true}, {2, false}, {3, true}, {4, false}, {5, true}}
		if len(got) != len(want) {
			t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("row %d: got %v, want %v (full: %v)", i, got[i], want[i], got)
			}
		}
	})

	// Case 2: projected EXISTS + ORDER BY id DESC. Same booleans, reverse order.
	t.Run("orderby_desc", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1 ORDER BY id DESC"
		requireSortOverFlatMap(t, q)
		got := queryIDBoolOrdered(t, q)
		want := []idBool{{5, true}, {4, false}, {3, true}, {2, false}, {1, true}}
		if len(got) != len(want) {
			t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("row %d: got %v, want %v (full: %v)", i, got[i], want[i], got)
			}
		}
	})

	// Case 3: projected EXISTS + ORDER BY id ASC + LIMIT 3. The first three rows
	// in id order, with correct booleans. A constant-false bug would have read
	// {1,false},{2,false},{3,false} here; the fix yields {1,true},{2,false},{3,true}.
	t.Run("orderby_asc_limit", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1 ORDER BY id LIMIT 3"
		// LIMIT is hoisted above the project; the existential FlatMap + sort
		// still fire below it.
		requireSortOverFlatMap(t, q)
		got := queryIDBoolOrdered(t, q)
		want := []idBool{{1, true}, {2, false}, {3, true}}
		if len(got) != len(want) {
			t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("row %d: got %v, want %v (full: %v)", i, got[i], want[i], got)
			}
		}
	})

	// Case 4: NOT EXISTS + ORDER BY — the complement, to pin that the boolean
	// computed under the live binding negates correctly with the sort on top.
	t.Run("not_exists_orderby_asc", func(t *testing.T) {
		q := "SELECT id, NOT EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS no_t2 FROM t1 ORDER BY id"
		requireSortOverFlatMap(t, q)
		got := queryIDBoolOrdered(t, q)
		want := []idBool{{1, false}, {2, true}, {3, false}, {4, true}, {5, false}}
		if len(got) != len(want) {
			t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("row %d: got %v, want %v (full: %v)", i, got[i], want[i], got)
			}
		}
	})

	// Case 5: ORDER BY a column NOT in the SELECT list (col1). This is Java's
	// remainingOrderByExpressions branch (LogicalOperator.generateSelect): the
	// fold must APPEND col1 to the folded projection, sort on it, then RE-PROJECT
	// to drop it. Without that branch the sort key (a FieldValue over a column
	// the folded result record doesn't carry) silently fails to resolve and the
	// sort no-ops — rows come back in scan order, the ORDER BY ignored.
	//
	// Data: col1 = 10,20,30,40,50 for ids 1..5 (ascending with id), so ORDER BY
	// col1 DESC yields ids 5,4,3,2,1 — distinct from any id-order default, so a
	// no-op sort would visibly fail. t2 matches ids {1,3,5}. The result must
	// expose exactly the two SELECT columns (col1 must not leak).
	t.Run("orderby_col_not_in_select_desc", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1 ORDER BY col1 DESC"
		requireSortOverFlatMap(t, q)
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		if len(cols) != 2 {
			t.Fatalf("expected 2 result columns (col1 must not leak), got %v", cols)
		}
		var got []idBool
		for rows.Next() {
			var r idBool
			if err := rows.Scan(&r.id, &r.b); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		want := []idBool{{5, true}, {4, false}, {3, true}, {2, false}, {1, true}}
		if len(got) != len(want) {
			t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("row %d: got %v, want %v (full: %v) — sort by non-selected col1 ignored?", i, got[i], want[i], got)
			}
		}
	})
}

// TestFDB_ProjectedExists_ScalarSubquery pins RFC-141 Phase 2 P2: a projected
// EXISTS combined with an uncorrelated scalar subquery in the SAME SELECT list.
//
// Root cause of the pre-fix bug: the projected-EXISTS fold took an early return
// BEFORE the loop that collects the projection's uncorrelated scalar subqueries
// into the translator's scalarSubqueries (pre-evaluated and bound by alias at
// execution). With the collection skipped, the executor never got the scalar
// binding, so the scalar column came back NULL.
//
// Fix: scalar-subquery collection runs BEFORE the fold (for every projection),
// so the folded path still registers the scalar plan. Both the EXISTS boolean
// AND the scalar column must be correct. Before the fix: the scalar was NULL.
//
// Note: a scalar subquery in the SELECT list is a Go read-side extension —
// Java's fdb-relational grammar cannot parse it there — so there is no Java
// plan shape to match; the contract is correctness (boolean + scalar), with no
// wire impact (the scalar is pre-evaluated, not stored).
func TestFDB_ProjectedExists_ScalarSubquery(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_projexists_sc")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_projexists_sc")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE projexists_sc_tmpl "+
		"CREATE TABLE t1(id BIGINT, col1 BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT, t1_id BIGINT, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_projexists_sc/s WITH TEMPLATE projexists_sc_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_projexists_sc?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 10), (2, 20), (3, 30)")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 1), (200, 1), (300, 3)")

	type row struct {
		id    int64
		b     bool
		maxid sql.NullInt64
	}
	queryRows := func(t *testing.T, q string) []row {
		t.Helper()
		rs, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rs.Close()
		var out []row
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.id, &r.b, &r.maxid); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, r)
		}
		if err := rs.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		return out
	}
	sortByID := func(rs []row) {
		for i := 1; i < len(rs); i++ {
			for j := i; j > 0 && rs[j-1].id > rs[j].id; j-- {
				rs[j-1], rs[j] = rs[j], rs[j-1]
			}
		}
	}

	// Case 1: projected EXISTS + uncorrelated scalar subquery. The scalar
	// (SELECT MAX(id) FROM t2) is 300 for every row; the EXISTS boolean is
	// per-row (ids 1 and 3 have matches in t2, id 2 does not). Both columns
	// must be correct. Before the fix: maxid came back NULL.
	t.Run("exists_plus_max_scalar", func(t *testing.T) {
		q := "SELECT id, " +
			"EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2, " +
			"(SELECT MAX(id) FROM t2) AS maxid FROM t1"
		got := queryRows(t, q)
		sortByID(got)
		want := []struct {
			id    int64
			b     bool
			maxid int64
		}{
			{1, true, 300},
			{2, false, 300},
			{3, true, 300},
		}
		if len(got) != len(want) {
			t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
		}
		for i := range want {
			if got[i].id != want[i].id || got[i].b != want[i].b {
				t.Errorf("row %d id/bool: got id=%d b=%v, want id=%d b=%v", i, got[i].id, got[i].b, want[i].id, want[i].b)
			}
			if !got[i].maxid.Valid {
				t.Errorf("row %d: scalar maxid came back NULL, want %d (P2 regression)", i, want[i].maxid)
			} else if got[i].maxid.Int64 != want[i].maxid {
				t.Errorf("row %d: scalar maxid=%d, want %d", i, got[i].maxid.Int64, want[i].maxid)
			}
		}
	})

	// Case 2: scalar subquery FIRST, then EXISTS — order independence: the
	// scalar collection must run regardless of column position.
	t.Run("max_scalar_plus_exists", func(t *testing.T) {
		q := "SELECT id, " +
			"(SELECT MAX(t1_id) FROM t2) AS maxfk, " +
			"EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1"
		rs, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rs.Close()
		type r2 struct {
			id    int64
			maxfk sql.NullInt64
			b     bool
		}
		var got []r2
		for rs.Next() {
			var r r2
			if err := rs.Scan(&r.id, &r.maxfk, &r.b); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, r)
		}
		if err := rs.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		for i := 1; i < len(got); i++ {
			for j := i; j > 0 && got[j-1].id > got[j].id; j-- {
				got[j-1], got[j] = got[j], got[j-1]
			}
		}
		// MAX(t1_id) over t2 = 3. Booleans per id: 1=true, 2=false, 3=true.
		want := []struct {
			id    int64
			maxfk int64
			b     bool
		}{
			{1, 3, true},
			{2, 3, false},
			{3, 3, true},
		}
		if len(got) != len(want) {
			t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
		}
		for i := range want {
			if got[i].id != want[i].id || got[i].b != want[i].b {
				t.Errorf("row %d id/bool: got id=%d b=%v, want id=%d b=%v", i, got[i].id, got[i].b, want[i].id, want[i].b)
			}
			if !got[i].maxfk.Valid {
				t.Errorf("row %d: scalar maxfk came back NULL, want %d (P2 regression)", i, want[i].maxfk)
			} else if got[i].maxfk.Int64 != want[i].maxfk {
				t.Errorf("row %d: scalar maxfk=%d, want %d", i, got[i].maxfk.Int64, want[i].maxfk)
			}
		}
	})
}
