package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

// TestFDB_ProjectedExistsRound10 pins RFC-141 R4 round-10, two silent-wrong bugs
// codex found:
//
//	P2a — a MULTI-TABLE EXISTS inner (`EXISTS (SELECT 1 FROM t2, t3 WHERE
//	t2.t1_id = t1.id)`) correlating to a NON-rightmost leg (t2). The NLJ rule
//	classified the correlation predicate by a single inner correlation =
//	the RIGHTMOST inner leg (t3); a predicate referencing t2 matched neither
//	that nor "outer-only", so it was evaluated with NO inner binding and
//	dropped EVERY outer row. Fix: route below the FirstOrDefault any predicate
//	that references a correlation OTHER than the FlatMap's outer leg(s) — it
//	touches the inner. Applies to BOTH existential-join rule methods
//	(implementExistentialSelect and the JOIN-in-FROM implementJoinWithExistential).
//
//	P2b — `SELECT col1 AS id, EXISTS(...) FROM t1 ORDER BY t1.id`. The qualified
//	source sort key `t1.id` was stripped to bare `ID`, which collided with the
//	SELECT-list alias `id` (= col1), so the sort ordered by col1 (the output
//	alias) instead of t1.id. Fix: output membership for a sort key is VALUE-based
//	(an output field must genuinely PROJECT the source column), never a bare-name
//	match against an output alias; a non-projected qualified source key is
//	appended as a hidden field named by its QUALIFIED provenance (collision-free).
//
// Data:
//
//	t1: id 1,2,3; col1 = 99,98,97 (so col1 DESCENDS as id ASCENDS — a sort by the
//	    wrong column yields a visibly different order).
//	t2: t1_id in {1,3}  -> EXISTS over t2 correlated to t1.id is true for {1,3}.
//	t3: non-empty       -> the `t2, t3` cross-join is non-empty whenever t2 matches.
func TestFDB_ProjectedExistsRound10(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_pexr10")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_pexr10")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE pexr10_tmpl "+
		"CREATE TABLE t1 (id BIGINT NOT NULL, col1 BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE t2 (id BIGINT NOT NULL, t1_id BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE t3 (id BIGINT NOT NULL, t2_id BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE t4 (id BIGINT NOT NULL, t3_id BIGINT, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_pexr10/s WITH TEMPLATE pexr10_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_pexr10?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 99), (2, 98), (3, 97)")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (10, 1), (20, 3)")
	mustExec(t, db, ctx, "INSERT INTO t3 VALUES (100, 10), (200, 20)")
	mustExec(t, db, ctx, "INSERT INTO t4 VALUES (1000, 100)")

	// queryInts runs a 1-column query and returns the sorted int64 values.
	queryInts := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	eqInts := func(got, want []int64) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	type idBool struct {
		id int64
		e  bool
	}
	queryIDBool := func(t *testing.T, q string) []idBool {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []idBool
		for rows.Next() {
			var r idBool
			if err := rows.Scan(&r.id, &r.e); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
		return out
	}

	// ---- P2a: multi-table inner correlating to a NON-rightmost leg ----

	// WHERE-EXISTS, 2-leg inner, correlation to the LEFT (non-rightmost) leg t2.
	// EXISTS true for t1.id with a matching t2 row → {1,3}. Before the fix the
	// rule pushed `t2.t1_id = t1.id` outside the FOD (t2 unbound) → 0 rows.
	t.Run("p2a_where_multitable_nonrightmost", func(t *testing.T) {
		got := queryInts(t, "SELECT id FROM t1 WHERE EXISTS (SELECT 1 FROM t2, t3 WHERE t2.t1_id = t1.id)")
		if want := []int64{1, 3}; !eqInts(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// Same correlation, but the inner FROM lists the legs in the OTHER order
	// (t3, t2) so t2 is now the rightmost leg — must also be correct, proving the
	// fix is leg-order-independent.
	t.Run("p2a_where_multitable_rightmost", func(t *testing.T) {
		got := queryInts(t, "SELECT id FROM t1 WHERE EXISTS (SELECT 1 FROM t3, t2 WHERE t2.t1_id = t1.id)")
		if want := []int64{1, 3}; !eqInts(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// 3-leg inner, correlation to the leftmost leg.
	t.Run("p2a_where_3leg_leftmost", func(t *testing.T) {
		got := queryInts(t, "SELECT id FROM t1 WHERE EXISTS (SELECT 1 FROM t2, t3, t4 WHERE t2.t1_id = t1.id)")
		if want := []int64{1, 3}; !eqInts(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// Multi-table inner with an INNER-ONLY predicate joining the two legs (no
	// outer correlation on that conjunct) PLUS the outer correlation. Both
	// conjuncts reference the inner and must sit below the FOD. t2.id↔t3.t2_id
	// match for both t2 rows, so the answer is still {1,3}.
	t.Run("p2a_where_multitable_innerjoin_pred", func(t *testing.T) {
		got := queryInts(t, "SELECT id FROM t1 WHERE EXISTS (SELECT 1 FROM t2, t3 WHERE t2.t1_id = t1.id AND t3.t2_id = t2.id)")
		if want := []int64{1, 3}; !eqInts(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// Explicit JOIN ... ON inside the EXISTS inner, correlation on the left leg.
	t.Run("p2a_where_explicit_join_on", func(t *testing.T) {
		got := queryInts(t, "SELECT id FROM t1 WHERE EXISTS (SELECT 1 FROM t2 JOIN t3 ON t3.t2_id = t2.id WHERE t2.t1_id = t1.id)")
		if want := []int64{1, 3}; !eqInts(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// WHERE NOT-EXISTS multi-table inner: TRUE only for the outer row with no
	// matching t2 (id=2). Before the fix the predicate misroute made NOT EXISTS
	// admit ALL rows (the inner was empty for every row).
	t.Run("p2a_where_not_exists_multitable", func(t *testing.T) {
		got := queryInts(t, "SELECT id FROM t1 WHERE NOT EXISTS (SELECT 1 FROM t2, t3 WHERE t2.t1_id = t1.id)")
		if want := []int64{2}; !eqInts(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// Outer-only predicate alongside the multi-table inner: only `col1 > 97`
	// (ids 1,2) AND EXISTS (ids 1,3) → {1}. Proves the outer-only conjunct still
	// filters the OUTER, not the inner.
	t.Run("p2a_where_outer_pred_plus_multitable", func(t *testing.T) {
		got := queryInts(t, "SELECT id FROM t1 WHERE col1 > 97 AND EXISTS (SELECT 1 FROM t2, t3 WHERE t2.t1_id = t1.id)")
		if want := []int64{1}; !eqInts(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// PROJECTED multi-table inner non-rightmost: the boolean per outer row.
	t.Run("p2a_projected_multitable_nonrightmost", func(t *testing.T) {
		got := queryIDBool(t, "SELECT id, EXISTS (SELECT 1 FROM t2, t3 WHERE t2.t1_id = t1.id) AS e FROM t1")
		want := []idBool{{1, true}, {2, false}, {3, true}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// PROJECTED NOT-EXISTS multi-table inner.
	t.Run("p2a_projected_not_exists_multitable", func(t *testing.T) {
		got := queryIDBool(t, "SELECT id, NOT EXISTS (SELECT 1 FROM t2, t3 WHERE t2.t1_id = t1.id) AS e FROM t1")
		want := []idBool{{1, false}, {2, true}, {3, false}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// PROJECTED multi-table inner with a JOIN in the OUTER FROM (the
	// implementJoinWithExistential variant). `t1 JOIN t1 AS x ON x.id = t1.id`
	// keeps one row per t1; the projected EXISTS reads the multi-table inner.
	t.Run("p2a_projected_join_from_multitable", func(t *testing.T) {
		got := queryIDBool(t, "SELECT t1.id, EXISTS (SELECT 1 FROM t2, t3 WHERE t2.t1_id = t1.id) AS e FROM t1 JOIN t1 AS x ON x.id = t1.id")
		want := []idBool{{1, true}, {2, false}, {3, true}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// WHERE-EXISTS multi-table inner over a JOIN in the OUTER FROM.
	t.Run("p2a_where_join_from_multitable", func(t *testing.T) {
		got := queryInts(t, "SELECT t1.id FROM t1 JOIN t1 AS x ON x.id = t1.id WHERE EXISTS (SELECT 1 FROM t2, t3 WHERE t2.t1_id = t1.id)")
		if want := []int64{1, 3}; !eqInts(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// Single-table inner control — must keep working unchanged.
	t.Run("control_single_table_inner", func(t *testing.T) {
		got := queryInts(t, "SELECT id FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id)")
		if want := []int64{1, 3}; !eqInts(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// ---- P2b: qualified ORDER BY whose bare name collides with a SELECT alias ----

	// queryCol1Seq runs `SELECT col1 AS id, EXISTS(...) ... ORDER BY <key>` and
	// returns the col1 column in ROW ORDER (NOT sorted) so the order is asserted.
	queryCol1Seq := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var col1 int64
			var e bool
			if err := rows.Scan(&col1, &e); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, col1)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		return out
	}
	eqSeq := func(got, want []int64) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	// THE BUG: `SELECT col1 AS id ... ORDER BY t1.id`. The output alias `id` IS
	// col1. ORDER BY t1.id must order by the REAL t1.id (1,2,3 ascending), so the
	// col1 column appears 99,98,97. The buggy plan stripped `t1.id`→bare `ID`,
	// collided with output alias `id`(=col1), and sorted by col1 → 97,98,99.
	t.Run("p2b_qualified_orderby_alias_collision_asc", func(t *testing.T) {
		got := queryCol1Seq(t, "SELECT col1 AS id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS e FROM t1 ORDER BY t1.id")
		if want := []int64{99, 98, 97}; !eqSeq(got, want) {
			t.Errorf("ORDER BY t1.id ASC: col1 seq got %v, want %v (wrong-column sort would give [97 98 99])", got, want)
		}
	})

	// DESC variant: ORDER BY t1.id DESC → id 3,2,1 → col1 97,98,99. A no-op /
	// wrong-column sort would NOT produce this descending-by-id sequence.
	t.Run("p2b_qualified_orderby_alias_collision_desc", func(t *testing.T) {
		got := queryCol1Seq(t, "SELECT col1 AS id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS e FROM t1 ORDER BY t1.id DESC")
		if want := []int64{97, 98, 99}; !eqSeq(got, want) {
			t.Errorf("ORDER BY t1.id DESC: col1 seq got %v, want %v", got, want)
		}
	})

	// Bare-column variant of the same collision: `ORDER BY id` (the SELECT-list
	// alias) must order by the OUTPUT alias `id` (= col1) per SQL alias-precedence
	// — col1 ascending = 97,98,99. This proves the alias path (k.Value set by
	// upgradeSortKeyValues) is UNCHANGED: a bare alias key still resolves to the
	// output column, only a QUALIFIED source key (`t1.id`) is distinguished.
	t.Run("p2b_bare_alias_orderby_is_output_column", func(t *testing.T) {
		got := queryCol1Seq(t, "SELECT col1 AS id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS e FROM t1 ORDER BY id")
		if want := []int64{97, 98, 99}; !eqSeq(got, want) {
			t.Errorf("ORDER BY id (output alias): col1 seq got %v, want %v", got, want)
		}
	})

	// Control: `SELECT t1.id ... ORDER BY t1.id DESC` — here t1.id IS projected,
	// so the qualified key pulls up to that output field. id sequence 3,2,1.
	t.Run("p2b_control_selected_qualified_orderby", func(t *testing.T) {
		q := "SELECT t1.id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS e FROM t1 ORDER BY t1.id DESC"
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var idSeq []int64
		for rows.Next() {
			var id int64
			var e bool
			if err := rows.Scan(&id, &e); err != nil {
				t.Fatalf("scan: %v", err)
			}
			idSeq = append(idSeq, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		if want := []int64{3, 2, 1}; !eqSeq(idSeq, want) {
			t.Errorf("control selected-qualified ORDER BY t1.id DESC: id seq got %v, want %v", idSeq, want)
		}
	})
}
