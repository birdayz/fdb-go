package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_ProjectedExists_Round3 pins the three round-3 projected-EXISTS fixes
// (join-from projection, ORDER BY the EXISTS alias, parenthesized NOT (EXISTS))
// AND the §8 safety guard (an unrecognized projected-EXISTS shape rejects
// cleanly with ErrCodeUnsupportedQuery rather than shipping silently-wrong rows).
// Each fix is revert-proof: before the fix the assertion below fails with a
// constant-false boolean, a leaked inner column, a NULL column, or wrong order —
// never a "happens to pass" coincidence.
func TestFDB_ProjectedExists_Round3(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_projexists_r3")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_projexists_r3")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE projexists_r3_tmpl "+
		"CREATE TABLE t1(id BIGINT, col1 BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT, t1_id BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t3(id BIGINT, t1_id BIGINT, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_projexists_r3/s WITH TEMPLATE projexists_r3_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_projexists_r3?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// t1: ids 1..5. t2 references t1 ids {1,3,5} (those rows "have a t2").
	// t3 references t1 ids {2,3} (used for the multi-existential guard case).
	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 10), (2, 20), (3, 30), (4, 40), (5, 50)")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 1), (200, 3), (300, 5)")
	mustExec(t, db, ctx, "INSERT INTO t3 VALUES (1000, 2), (2000, 3)")

	requireExistentialFlatMap := func(t *testing.T, q string) {
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
	}

	// ── Fix 1: projected EXISTS + JOIN in FROM, no WHERE ────────────────────
	//
	// `SELECT t1.id, t2.id, EXISTS(...) FROM t1 JOIN t2 ...`. Before the fix the
	// synthesized existential filter wrapped the WHOLE Project(Join), so the
	// projection — including the ExistsValue — ran ABOVE the FlatMap: the
	// boolean was constant-false AND inner columns leaked. The fix flattens the
	// join's two ForEach quantifiers + the existential into ONE SelectExpression
	// with the projection as its result value (buildExistentialJoinSelect), so
	// the boolean is computed with the inner binding live and no extra column
	// leaks. We JOIN t1 with t2 on t1_id, then probe t3 for the SAME t1 row.
	t.Run("join_from_projected_exists", func(t *testing.T) {
		// Rows: t1 JOIN t2 ON t2.t1_id = t1.id yields the t1 rows that have a t2
		// = {1,3,5}, paired with the matching t2.id. The projected EXISTS probes
		// t3 for t1.id ∈ {2,3}; over the join's surviving t1 ids {1,3,5} only
		// id=3 has a t3 → has_t3 is [false, true, false] for ids [1,3,5].
		q := "SELECT t1.id, t2.id, EXISTS (SELECT 1 FROM t3 WHERE t3.t1_id = t1.id) AS has_t3 " +
			"FROM t1 JOIN t2 ON t2.t1_id = t1.id"
		requireExistentialFlatMap(t, q)

		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()

		// Assert exactly three columns — a leaked inner column would make Scan
		// into three targets fail or mis-bind.
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns: %v", err)
		}
		if len(cols) != 3 {
			t.Fatalf("expected 3 result columns (t1.id, t2.id, has_t3), got %d: %v", len(cols), cols)
		}

		type row struct {
			t1id, t2id int64
			has        bool
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.t1id, &r.t2id, &r.has); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		// sort by t1id then t2id for a stable compare
		for i := 1; i < len(got); i++ {
			for j := i; j > 0 && (got[j-1].t1id > got[j].t1id ||
				(got[j-1].t1id == got[j].t1id && got[j-1].t2id > got[j].t2id)); j-- {
				got[j-1], got[j] = got[j], got[j-1]
			}
		}
		want := []row{
			{1, 100, false},
			{3, 200, true},
			{5, 300, false},
		}
		if len(got) != len(want) {
			t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("row %d: got %+v, want %+v (full: %v)", i, got[i], want[i], got)
			}
		}
		// Explicit no-leak / not-constant-false guards:
		anyTrue := false
		for _, r := range got {
			if r.has {
				anyTrue = true
			}
		}
		if !anyTrue {
			t.Errorf("all has_t3 booleans were false — projection ran above the FlatMap (dead binding): %v", got)
		}
	})

	// ── Fix 2: ORDER BY referencing the EXISTS SELECT-list alias ────────────
	//
	// `SELECT id, EXISTS(...) AS has_t2 FROM t1 ORDER BY has_t2 DESC`. Before the
	// fix the sort key's Value was the raw ExistsValue (upgradeSortKeyValues
	// copies the projected expression), re-applied ABOVE the FlatMap → false for
	// every row → no real ordering. The fix pulls the sort key up to a FieldValue
	// over the folded output column has_t2 (Java's OrderByExpression.pullUp), so
	// the sort orders by the materialized boolean. DESC ⇒ all trues first, then
	// all falses (within each group id ascending, the secondary natural order).
	queryIDBoolOrdered := func(t *testing.T, q string) [][2]int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out [][2]int64
		for rows.Next() {
			var id int64
			var b bool
			if err := rows.Scan(&id, &b); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			bi := int64(0)
			if b {
				bi = 1
			}
			out = append(out, [2]int64{id, bi})
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		return out
	}
	// boolsInOrder extracts just the boolean column in row order.
	boolsInOrder := func(rs [][2]int64) []int64 {
		out := make([]int64, len(rs))
		for i, r := range rs {
			out[i] = r[1]
		}
		return out
	}

	t.Run("orderby_exists_alias_desc", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1 ORDER BY has_t2 DESC"
		got := queryIDBoolOrdered(t, q)
		if len(got) != 5 {
			t.Fatalf("got %d rows, want 5: %v", len(got), got)
		}
		// DESC by boolean: the three trues (ids 1,3,5) must all precede the two
		// falses (ids 2,4). A constant-false bug would leave the natural id order
		// 1..5 with bool column all 0 → this monotonicity check fails.
		bools := boolsInOrder(got)
		seenFalse := false
		trueCount, falseCount := 0, 0
		for i, b := range bools {
			if b == 1 {
				trueCount++
				if seenFalse {
					t.Errorf("DESC order violated at row %d: a true follows a false: %v", i, got)
				}
			} else {
				seenFalse = true
				falseCount++
			}
		}
		if trueCount != 3 || falseCount != 2 {
			t.Errorf("expected 3 trues + 2 falses, got %d/%d: %v", trueCount, falseCount, got)
		}
	})

	t.Run("orderby_exists_alias_asc", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1 ORDER BY has_t2 ASC"
		got := queryIDBoolOrdered(t, q)
		if len(got) != 5 {
			t.Fatalf("got %d rows, want 5: %v", len(got), got)
		}
		// ASC by boolean: the two falses must all precede the three trues.
		bools := boolsInOrder(got)
		seenTrue := false
		for i, b := range bools {
			if b == 1 {
				seenTrue = true
			} else if seenTrue {
				t.Errorf("ASC order violated at row %d: a false follows a true: %v", i, got)
			}
		}
	})

	// ── Fix 3: parenthesized NOT (EXISTS(...)) in a SELECT list ─────────────
	//
	// `SELECT id, NOT (EXISTS(...)) AS no_t2 FROM t1`. Before the fix the NOT
	// child was a PredicatedExpression over a paren-wrap RecordConstructor (not a
	// direct ExistsExpressionAtom), so existsAtomOf returned nil → the EXISTS
	// fell to the predicate path → the column projected NULL. The fix unwraps the
	// parenthesized form to find the ExistsExpressionAtom under NOT, yielding
	// NotValue(ExistsValue). The booleans must equal the negation of EXISTS.
	t.Run("paren_not_exists_in_projection", func(t *testing.T) {
		q := "SELECT id, NOT (EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id)) AS no_t2 FROM t1"
		requireExistentialFlatMap(t, q)
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		got := map[int64]any{}
		for rows.Next() {
			var id int64
			// Scan into *bool so a NULL column (the bug) fails Scan loudly rather
			// than silently coercing.
			var b sql.NullBool
			if err := rows.Scan(&id, &b); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if !b.Valid {
				t.Fatalf("no_t2 was NULL for id=%d — NOT (EXISTS(...)) did not fold to a boolean", id)
			}
			got[id] = b.Bool
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		// no_t2 = NOT EXISTS: ids {2,4} have NO t2 → true; ids {1,3,5} have a t2 → false.
		want := map[int64]any{1: false, 2: true, 3: false, 4: true, 5: false}
		if len(got) != len(want) {
			t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
		}
		for id, w := range want {
			if got[id] != w {
				t.Errorf("id=%d: got no_t2=%v, want %v", id, got[id], w)
			}
		}
	})

	// Bonus: a doubly-parenthesized NOT ((EXISTS(...))) must also fold (the
	// existsAtomOf recursion through nested paren-wraps).
	t.Run("double_paren_not_exists_in_projection", func(t *testing.T) {
		q := "SELECT id, NOT ((EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id))) AS no_t2 FROM t1"
		requireExistentialFlatMap(t, q)
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var id int64
			var b sql.NullBool
			if err := rows.Scan(&id, &b); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if !b.Valid {
				t.Fatalf("no_t2 NULL for id=%d under double parens", id)
			}
			n++
		}
		if n != 5 {
			t.Fatalf("got %d rows, want 5", n)
		}
	})

	// ── Safety guard: GROUP BY on a projected EXISTS rejects cleanly ────────
	//
	// `... GROUP BY id, x` where x is the EXISTS column is the canonical
	// long-tail shape the fold cannot thread through: the aggregate path has no
	// SubqueryPlanner, so the existential never resolves and the group key would
	// silently evaluate to a constant-false column. The §8 guard
	// (validateGroupByProjection's structural EXISTS check) rejects it with the
	// exact "projected EXISTS in this query shape is not yet supported" message —
	// a clean error, NEVER the silently-wrong all-false rows. This is the
	// revert-proof guard sentinel: without the guard the query returns 3 rows
	// with x=false for every row.
	t.Run("guard_rejects_groupby_exists_cleanly", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS x FROM t1 GROUP BY id, x"
		rows, err := db.QueryContext(ctx, q)
		if err == nil {
			var n int
			anyTrue := false
			for rows.Next() {
				var id int64
				var b sql.NullBool
				if scanErr := rows.Scan(&id, &b); scanErr == nil && b.Valid && b.Bool {
					anyTrue = true
				}
				n++
			}
			rows.Close()
			t.Fatalf("GROUP BY on a projected EXISTS returned %d rows (anyTrue=%v) instead of a clean error — "+
				"the guard let a dropped/constant-false EXISTS through", n, anyTrue)
		}
		// Must be EXACTLY the guard's clean unsupported-query message.
		if !strings.Contains(err.Error(), "projected EXISTS in this query shape is not yet supported") {
			t.Fatalf("expected the §8 guard's clean unsupported message, got: %v", err)
		}
	})

	// ── Safety guard: multiple projected EXISTS rejects cleanly ─────────────
	//
	// Multiple projected EXISTS in one SELECT is the documented multi-existential
	// boundary (needs nested FlatMaps with intermediate record-bundling — never
	// supported in the Go port). It must reject cleanly, never return wrong rows.
	t.Run("guard_rejects_multi_existential_cleanly", func(t *testing.T) {
		q := "SELECT id, " +
			"EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2, " +
			"EXISTS (SELECT 1 FROM t3 WHERE t3.t1_id = t1.id) AS has_t3 " +
			"FROM t1"
		rows, err := db.QueryContext(ctx, q)
		if err == nil {
			// The query must NOT silently return rows; if it did, drain + fail.
			cols, _ := rows.Columns()
			var n int
			for rows.Next() {
				n++
			}
			rows.Close()
			t.Fatalf("multi-existential projected EXISTS returned %d rows (cols=%v) instead of a clean error — "+
				"the guard let an unfolded ExistsValue through", n, cols)
		}
		// Must be a clean "not supported" error, not a panic or a wrong-rows pass.
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "not yet supported") && !strings.Contains(msg, "unsupported") &&
			!strings.Contains(msg, "could not plan") {
			t.Fatalf("expected a clean unsupported-query error, got: %v", err)
		}
	})
}
