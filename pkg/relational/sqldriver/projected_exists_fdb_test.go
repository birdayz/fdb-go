package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_ProjectedExists ports Java 4.12's exists-in-select.yamsql scenarios
// (RFC-141 Phase 2): EXISTS / NOT EXISTS as a SELECT-list column value, not
// just a WHERE predicate. Each case asserts BOTH the rows AND that the plan
// fires the existential FlatMap (FirstOrDefault inner — the pure-map shape that
// projects the boolean), matching Java's
//
//	FLATMAP q0 -> { ... | DEFAULT NULL AS q1 RETURN (q0.ID AS ID, exists(q1) ...) }
//
// The boolean column is the existential probe per outer row: ExistsValue.eval
// reads the inner binding (bound non-null ⇒ true, NULL ⇒ false).
func TestFDB_ProjectedExists(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_projexists")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_projexists")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE projexists_tmpl "+
		"CREATE TABLE t1(id BIGINT, col1 BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT, t1_id BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t3(id BIGINT, t2_id BIGINT, label STRING, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_projexists/s WITH TEMPLATE projexists_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_projexists?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 10), (2, 20), (3, 30)")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 1), (200, 1), (300, 3)")
	mustExec(t, db, ctx, "INSERT INTO t3 VALUES (1000, 100, 'a'), (2000, 100, 'b'), (3000, 300, 'c')")

	// requireExistentialFlatMap asserts the existential FlatMap fired — a
	// FlatMap whose inner is a FirstOrDefault (the one-row existential inner).
	// This is the RFC-141 plan shape; a fallback (full scan + post-filter,
	// or a non-existential plan) would not contain FirstOrDefault under a
	// FlatMap and is rejected so the test proves the optimization, not just
	// the answer.
	requireExistentialFlatMap := func(t *testing.T, q string) {
		t.Helper()
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN %q: %v", q, err)
		}
		if !strings.Contains(plan, "FlatMap") {
			t.Errorf("expected FlatMap in plan for %q, got:\n%s", q, plan)
		}
		if !strings.Contains(plan, "FirstOrDefault") {
			t.Errorf("expected FirstOrDefault (existential one-row inner) in plan for %q, got:\n%s", q, plan)
		}
	}

	type idBool struct {
		id int64
		b  bool
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
	queryBool := func(t *testing.T, q string) []bool {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []bool
		for rows.Next() {
			var b bool
			if err := rows.Scan(&b); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, b)
		}
		return out
	}
	sortIDBool := func(rs []idBool) {
		for i := 1; i < len(rs); i++ {
			for j := i; j > 0 && rs[j-1].id > rs[j].id; j-- {
				rs[j-1], rs[j] = rs[j], rs[j-1]
			}
		}
	}
	eqIDBool := func(got, want []idBool) bool {
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

	// Case 1: correlated EXISTS in projection, no join. (Java line 41.)
	t.Run("correlated_exists_in_projection", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1"
		requireExistentialFlatMap(t, q)
		got := queryIDBool(t, q)
		sortIDBool(got)
		want := []idBool{{1, true}, {2, false}, {3, true}}
		if !eqIDBool(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// Case 2: non-correlated EXISTS in projection. (Java-style SELECT EXISTS(...).)
	t.Run("noncorrelated_exists_in_projection", func(t *testing.T) {
		q := "SELECT EXISTS (SELECT 1 FROM t2) AS any_t2 FROM t1"
		requireExistentialFlatMap(t, q)
		got := queryBool(t, q)
		// One row per t1 row (3), all TRUE (t2 is non-empty).
		if len(got) != 3 {
			t.Fatalf("got %d rows, want 3", len(got))
		}
		for i, b := range got {
			if !b {
				t.Errorf("row %d: got %v, want true", i, b)
			}
		}
	})

	// Case 2b: non-correlated EXISTS over an EMPTY subquery ⇒ all FALSE; the
	// FirstOrDefault NULL path must read as "no row" (the dimension that an
	// always-one-row inner would get wrong).
	t.Run("noncorrelated_exists_empty_subquery", func(t *testing.T) {
		q := "SELECT EXISTS (SELECT 1 FROM t2 WHERE t2.id = 99999) AS any_t2 FROM t1"
		requireExistentialFlatMap(t, q)
		got := queryBool(t, q)
		if len(got) != 3 {
			t.Fatalf("got %d rows, want 3", len(got))
		}
		for i, b := range got {
			if b {
				t.Errorf("row %d: got %v, want false (empty subquery)", i, b)
			}
		}
	})

	// Case 3: NOT EXISTS in projection. (Java line 114.)
	t.Run("not_exists_in_projection", func(t *testing.T) {
		q := "SELECT id, NOT EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS no_t2 FROM t1"
		requireExistentialFlatMap(t, q)
		got := queryIDBool(t, q)
		sortIDBool(got)
		want := []idBool{{1, false}, {2, true}, {3, false}}
		if !eqIDBool(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// Case 4: projected EXISTS whose subquery is itself a JOIN (Java line 56 /
	// 104). The existential inner is a NestedLoopJoin(t3, t2) under the
	// FirstOrDefault — the correlated probe per outer row over a joined inner.
	t.Run("projected_exists_over_join_subquery", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t3, t2 WHERE t3.t2_id = t2.id AND t2.t1_id = t1.id) AS has_t3 FROM t1"
		requireExistentialFlatMap(t, q)
		got := queryIDBool(t, q)
		sortIDBool(got)
		want := []idBool{{1, true}, {2, false}, {3, true}}
		if !eqIDBool(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// NOTE: Java's exists-in-select.yamsql also covers MULTIPLE existentials in
	// one query — multiple projected EXISTS, and EXISTS in WHERE *and* SELECT
	// together (Java lines 85, 94). Those need >1 existential quantifier folded
	// into one SelectExpression via nested FlatMaps with intermediate
	// record-bundling (Java's "FLATMAP q3 -> { ... } ... RETURN (..., exists(
	// q5._0), exists(q5._1))" shape). That multi-existential chaining was never
	// supported in the Go port — even multiple WHERE-EXISTS (`WHERE EXISTS(...)
	// AND EXISTS(...)`) falls back to a text plan and "could not plan query" on
	// master; ImplementNestedLoopJoinRule.implementExistentialSelect handles a
	// single existential (2 quantifiers) only. It is a separate, larger
	// extension tracked in TODO.md (RFC-141 follow-up), not a Phase 2 regression.
}

func mustExec(t *testing.T, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, ctx context.Context, q string,
) {
	t.Helper()
	if _, err := db.ExecContext(ctx, q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
