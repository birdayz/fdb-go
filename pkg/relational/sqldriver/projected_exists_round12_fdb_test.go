package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// TestFDB_ProjectedExistsRound12 pins RFC-141 R4 round-12 — the convergence
// backstop for EXISTS in WRAPPED / NESTED positions.
//
// codex round 12 found that an EXISTS that is NOT in a directly-handled position
// silently produced WRONG results, because the planner only point-handles a few
// EXISTS shapes:
//
//   - WHERE: a top-level (or single-NOT-wrapped) existential — IsExistentialPredicate
//     / IsNotExistentialPredicate — lowers to a FirstOrDefault + residual filter.
//     An existential buried under ANY OTHER wrapper (`NOT (NOT EXISTS(...))`,
//     `EXISTS(...) OR p`, deeper AND/OR/NOT nesting) fell into the regular-predicate
//     bucket: the empty FirstOrDefault inner emitted its NULL default, no residual
//     removed it, and EVERY outer row silently passed.
//   - SELECT: a top-level projected `EXISTS(...)` / `NOT EXISTS(...)` (or its single
//     paren/NOT wrapper) folds into the FlatMap result value. A NESTED projected
//     EXISTS (`CASE WHEN EXISTS(...) THEN ...`, `EXISTS(...) AND x`) took the
//     predicate path → the ExistsValue was evaluated ABOVE the FlatMap with the
//     binding dead → constant false / NULL.
//
// EXISTS can be nested arbitrarily deep, so point-handling each shape never
// converges. The convergence fix is a comprehensive STRUCTURAL backstop: any
// EXISTS not in a directly-handled position is detected (typed predicate/parse
// tree, never text) and REJECTED cleanly with ErrCodeUnsupportedQuery (0AF00) —
// never silently mis-evaluated.
//
// Dataset (revert-proof): t1 ids {1,2,3}; t2.fk references only t1.id=2, so
// EXISTS(t2.fk = t1.id) is TRUE only for id 2. A reverted (silent-wrong) backstop
// returns a VISIBLY DIFFERENT result than the clean error each sentinel asserts.
func TestFDB_ProjectedExistsRound12(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_pexr12")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_pexr12")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE pexr12_tmpl "+
		"CREATE TABLE t1 (id BIGINT NOT NULL, col1 BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE t2 (id BIGINT NOT NULL, fk BIGINT, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_pexr12/s WITH TEMPLATE pexr12_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_pexr12?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 10), (2, 20), (3, 30)")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 2)")

	const unsupportedWHERE = "EXISTS in this query shape is not yet supported"
	const unsupportedSELECT = "projected EXISTS in this query shape is not yet supported"

	// assertRejected runs q, fails if it returns rows (the silent-wrong revert),
	// and requires the error message to contain wantMsg (the clean rejection).
	assertRejected := func(t *testing.T, q, wantMsg string) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err == nil {
			n := 0
			cols, _ := rows.Columns()
			for rows.Next() {
				n++
			}
			rows.Close()
			t.Fatalf("query %q returned %d rows (cols=%v) instead of a clean error — "+
				"the wrapped/nested EXISTS was silently mis-evaluated (round-12 revert)", q, n, cols)
		}
		if !strings.Contains(err.Error(), wantMsg) {
			t.Fatalf("query %q: expected clean rejection %q, got: %v", q, wantMsg, err)
		}
	}

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

	exists := "EXISTS (SELECT 1 FROM t2 WHERE t2.fk = t1.id)"

	// ───── P1a: wrapped WHERE EXISTS → clean reject (was silent-wrong: all rows) ─────

	// `NOT (NOT EXISTS(...))` is logically plain EXISTS (→ {2}); before the fix it
	// fell into the regular bucket and returned ALL rows {1,2,3}. A revert returns
	// 3 rows; the backstop rejects.
	t.Run("p1a_where_double_not_exists", func(t *testing.T) {
		assertRejected(t, "SELECT id FROM t1 WHERE NOT (NOT "+exists+")", unsupportedWHERE)
	})

	// `EXISTS(...) OR id = 1` — an existential under a disjunction. Rejected cleanly
	// (the upstream OR-EXISTS guard or this backstop). A revert silently returns
	// the wrong row set.
	t.Run("p1a_where_exists_or_pred", func(t *testing.T) {
		assertRejected(t, "SELECT id FROM t1 WHERE "+exists+" OR id = 1", "")
	})

	// An existential buried inside an AND conjunct under a wrapper:
	// `id > 1 AND NOT (NOT EXISTS(...))`. The plain conjunct routes directly but the
	// buried existential conjunct does not → reject.
	t.Run("p1a_where_buried_in_and", func(t *testing.T) {
		assertRejected(t, "SELECT id FROM t1 WHERE id > 1 AND NOT (NOT "+exists+")", unsupportedWHERE)
	})

	// `NOT (EXISTS(...) AND id > 1)` — the existential is under NOT(AND(...)), not a
	// direct single-NOT of a bare existential → reject.
	t.Run("p1a_where_not_of_exists_and", func(t *testing.T) {
		assertRejected(t, "SELECT id FROM t1 WHERE NOT ("+exists+" AND id > 1)", unsupportedWHERE)
	})

	// ───── P1b: nested projected EXISTS → clean reject (was silent-wrong: ELSE/NULL) ─────

	// `CASE WHEN EXISTS(...) THEN 1 ELSE 0 END` — before the fix the EXISTS evaluated
	// constant-false above the FlatMap, so the column was 0 for EVERY row (id 2's
	// should be 1). A revert returns 3 rows with all-zero; the backstop rejects.
	t.Run("p1b_select_case_when_exists", func(t *testing.T) {
		assertRejected(t,
			"SELECT id, CASE WHEN "+exists+" THEN 1 ELSE 0 END FROM t1", unsupportedSELECT)
	})

	// `EXISTS(...) AND id > 0` in the SELECT list — before the fix the column read
	// NULL for every row. A revert returns 3 rows with a NULL column.
	t.Run("p1b_select_exists_and_pred", func(t *testing.T) {
		assertRejected(t,
			"SELECT id, "+exists+" AND id > 0 FROM t1", unsupportedSELECT)
	})

	// `NOT (EXISTS(...) AND id > 0)` projected — the EXISTS is under NOT(AND), not a
	// direct NOT-of-bare-EXISTS → reject.
	t.Run("p1b_select_not_of_exists_and", func(t *testing.T) {
		assertRejected(t,
			"SELECT id, NOT ("+exists+" AND id > 0) FROM t1", unsupportedSELECT)
	})

	// `(EXISTS(...) OR id > 0)` projected — EXISTS under a disjunction in a SELECT
	// item → reject.
	t.Run("p1b_select_exists_or_pred", func(t *testing.T) {
		assertRejected(t,
			"SELECT id, ("+exists+" OR id > 0) FROM t1", unsupportedSELECT)
	})

	// ───── Controls: the directly-handled shapes STILL WORK ─────

	t.Run("control_where_exists", func(t *testing.T) {
		if got := queryInts(t, "SELECT id FROM t1 WHERE "+exists); !eqInts(got, []int64{2}) {
			t.Fatalf("WHERE EXISTS: got %v want [2]", got)
		}
	})
	t.Run("control_where_not_exists", func(t *testing.T) {
		if got := queryInts(t, "SELECT id FROM t1 WHERE NOT "+exists); !eqInts(got, []int64{1, 3}) {
			t.Fatalf("WHERE NOT EXISTS: got %v want [1 3]", got)
		}
	})
	t.Run("control_where_paren_not_exists", func(t *testing.T) {
		if got := queryInts(t, "SELECT id FROM t1 WHERE NOT ("+exists+")"); !eqInts(got, []int64{1, 3}) {
			t.Fatalf("WHERE NOT (EXISTS): got %v want [1 3]", got)
		}
	})
	t.Run("control_where_exists_and_pred", func(t *testing.T) {
		if got := queryInts(t, "SELECT id FROM t1 WHERE "+exists+" AND id > 1"); !eqInts(got, []int64{2}) {
			t.Fatalf("WHERE EXISTS AND id>1: got %v want [2]", got)
		}
	})

	t.Run("control_select_exists", func(t *testing.T) {
		got := queryIDBool(t, "SELECT id, "+exists+" FROM t1")
		want := []idBool{{1, false}, {2, true}, {3, false}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("SELECT EXISTS: got %v want %v", got, want)
		}
	})
	t.Run("control_select_not_exists", func(t *testing.T) {
		got := queryIDBool(t, "SELECT id, NOT "+exists+" FROM t1")
		want := []idBool{{1, true}, {2, false}, {3, true}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("SELECT NOT EXISTS: got %v want %v", got, want)
		}
	})
	t.Run("control_select_paren_not_exists", func(t *testing.T) {
		got := queryIDBool(t, "SELECT id, NOT ("+exists+") FROM t1")
		want := []idBool{{1, true}, {2, false}, {3, true}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("SELECT NOT (EXISTS): got %v want %v", got, want)
		}
	})
	t.Run("control_select_nested_paren_not_exists", func(t *testing.T) {
		got := queryIDBool(t, "SELECT id, NOT (("+exists+")) FROM t1")
		want := []idBool{{1, true}, {2, false}, {3, true}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("SELECT NOT ((EXISTS)): got %v want %v", got, want)
		}
	})

	// Control: a DIRECT nested EXISTS inside a subquery WHERE still works (the
	// backstop must not false-reject a legitimate nested existential — it is a
	// top-level existential within its OWN SelectExpression).
	t.Run("control_where_exists_subquery_with_nested_exists", func(t *testing.T) {
		// t2.fk=2 references t1.id=2; the inner subquery's EXISTS is non-empty
		// (t2 has a row), so the whole thing is EXISTS(t2.fk=t1.id) → {2}.
		got := queryInts(t, "SELECT id FROM t1 WHERE EXISTS "+
			"(SELECT 1 FROM t2 WHERE t2.fk = t1.id AND EXISTS (SELECT 1 FROM t2 t2b WHERE t2b.id = t2.id))")
		if !eqInts(got, []int64{2}) {
			t.Fatalf("WHERE EXISTS(subq with nested EXISTS): got %v want [2]", got)
		}
	})
}

// TestFDB_ProjectedExistsRound12_DML pins that the round-12 WHERE-EXISTS backstop
// also runs on the DML planning path (DELETE / UPDATE). The DML planner reuses
// the existential NLJ rule, so a buried WHERE existential (`DELETE FROM t1 WHERE
// NOT (NOT EXISTS(...))`) is just as silently-wrong as on SELECT — without the
// guard it matched (deleted/updated) EVERY targeted row instead of just the
// EXISTS-true one. Each DML table gets its own subtest dataset (DML mutates rows).
func TestFDB_ProjectedExistsRound12_DML(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_pexr12dml")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_pexr12dml")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE pexr12dml_tmpl "+
		"CREATE TABLE t1 (id BIGINT NOT NULL, col1 BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE t2 (id BIGINT NOT NULL, fk BIGINT, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_pexr12dml/s WITH TEMPLATE pexr12dml_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_pexr12dml?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 2)")

	const unsupportedWHERE = "EXISTS in this query shape is not yet supported"
	exists := "EXISTS (SELECT 1 FROM t2 WHERE t2.fk = t1.id)"

	remaining := func(t *testing.T) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, "SELECT id FROM t1")
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, v)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	eq := func(got, want []int64) bool {
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

	// Guard sentinel: DELETE with a buried existential rejects cleanly. A revert
	// silently DELETES all 3 rows (the silent-wrong behavior).
	t.Run("delete_buried_exists_rejected", func(t *testing.T) {
		mustExec(t, db, ctx, "DELETE FROM t1")
		mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 10), (2, 20), (3, 30)")
		_, err := db.ExecContext(ctx, "DELETE FROM t1 WHERE NOT (NOT "+exists+")")
		if err == nil {
			t.Fatalf("DELETE with buried EXISTS succeeded (remaining=%v) — should reject cleanly", remaining(t))
		}
		if !strings.Contains(err.Error(), unsupportedWHERE) {
			t.Fatalf("expected clean rejection %q, got: %v", unsupportedWHERE, err)
		}
		// Nothing was deleted.
		if got := remaining(t); !eq(got, []int64{1, 2, 3}) {
			t.Fatalf("rows changed despite clean rejection: %v", got)
		}
	})

	// Control: a DIRECT DELETE WHERE EXISTS still works (deletes only id 2).
	t.Run("control_delete_where_exists", func(t *testing.T) {
		mustExec(t, db, ctx, "DELETE FROM t1")
		mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 10), (2, 20), (3, 30)")
		mustExec(t, db, ctx, "DELETE FROM t1 WHERE "+exists)
		if got := remaining(t); !eq(got, []int64{1, 3}) {
			t.Fatalf("DELETE WHERE EXISTS: remaining %v want [1 3]", got)
		}
	})

	// Control: a DIRECT DELETE WHERE NOT EXISTS still works (deletes ids 1,3).
	t.Run("control_delete_where_not_exists", func(t *testing.T) {
		mustExec(t, db, ctx, "DELETE FROM t1")
		mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 10), (2, 20), (3, 30)")
		mustExec(t, db, ctx, "DELETE FROM t1 WHERE NOT "+exists)
		if got := remaining(t); !eq(got, []int64{2}) {
			t.Fatalf("DELETE WHERE NOT EXISTS: remaining %v want [2]", got)
		}
	})
}
