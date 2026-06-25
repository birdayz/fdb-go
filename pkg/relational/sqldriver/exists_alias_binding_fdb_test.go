package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// TestFDB_ExistsAboveJoin_AliasBinding pins RFC-141 Phase 2 P1a: a
// WHERE EXISTS / NOT EXISTS whose OUTER is a JOIN of two distinct tables,
// where the EXISTS subquery correlates to a column of a joined-outer leg.
//
// Plan shape (ImplementNestedLoopJoinRule.implementJoinWithExistential):
//
//	FLATMAP mergedOuter -> {
//	    NestedLoopJoin(emp ⋈ dept)            // the merged outer (2 legs)
//	  | EXISTS subplan filtered by existPreds // correlated to emp/dept leg
//	  | FirstOrDefault(NULL)
//	  | residual QOV IS [NOT] NULL            // the semi-join residual
//	}
//
// The bug: existPreds reference the ORIGINAL leg aliases (e.g. EMP.ID),
// but they run INSIDE the FlatMap inner where only the fresh
// mergedOuterCorr is bound — so EMP.ID resolves to NULL, the
// correlation never matches, and:
//   - WHERE EXISTS drops ALL joined rows (false negatives), and
//   - WHERE NOT EXISTS admits ALL joined rows (false positives).
//
// To force the 3-quantifier join+EXISTS path (NOT the 2-quantifier
// semi-join collapse), the join key (emp.dept_id = dept.id) and the
// EXISTS correlation (proj.owner_id = emp.id) reference DIFFERENT columns
// of DIFFERENT tables, so the cross-join is not subsumed by the EXISTS.
func TestFDB_ExistsAboveJoin_AliasBinding(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_existsabovejoin")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_existsabovejoin")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE eaj_tmpl "+
		"CREATE TABLE emp (id BIGINT NOT NULL, dept_id BIGINT, fname STRING, PRIMARY KEY (id)) "+
		"CREATE TABLE dept (id BIGINT NOT NULL, dname STRING, PRIMARY KEY (id)) "+
		"CREATE TABLE proj (pid BIGINT NOT NULL, owner_id BIGINT, dept_ref BIGINT, pname STRING, PRIMARY KEY (pid))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_existsabovejoin/s WITH TEMPLATE eaj_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_existsabovejoin?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// emp ids 1..4 are DISJOINT from dept ids 10,20 — this is deliberate so a
	// naive "bare-key" rebase (which would resolve the merged row's last-leg-
	// wins bare `id` instead of the qualified leg key) gives a DETECTABLY wrong
	// answer for the right-leg correlation subtest below.
	// emp: 1/Alice@d10, 2/Bob@d10, 3/Carol@d20, 4/Dave@d20
	mustExec(t, db, ctx, "INSERT INTO emp VALUES (1, 10, 'Alice'), (2, 10, 'Bob'), (3, 20, 'Carol'), (4, 20, 'Dave')")
	// dept: 10/Eng, 20/Sales
	mustExec(t, db, ctx, "INSERT INTO dept VALUES (10, 'Eng'), (20, 'Sales')")
	// proj: owner_id ties to emp.id (1,3 own projects); dept_ref ties to
	// dept.id and references ONLY dept 10 (never dept 20). The disjoint id
	// ranges make a wrong-leg bare-key resolution detectable: dept_ref ∈ {10}
	// can only ever match d.id, never the bare emp.id ∈ {1,3}.
	mustExec(t, db, ctx, "INSERT INTO proj VALUES (100, 1, 10, 'P1'), (200, 1, 10, 'P2'), (300, 3, 10, 'P3')")

	// requireJoinAboveExists asserts the join+EXISTS plan shape fired: an
	// inner-join (NestedLoopJoin) feeding a FlatMap with a FirstOrDefault
	// existential inner. This proves we exercise implementJoinWithExistential,
	// not a degenerate single-table semi-join.
	requireJoinAboveExists := func(t *testing.T, q string) {
		t.Helper()
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN %q: %v", q, err)
		}
		if !strings.Contains(plan, "FlatMap") {
			t.Fatalf("expected FlatMap in plan for %q, got:\n%s", q, plan)
		}
		if !strings.Contains(plan, "FirstOrDefault") {
			t.Fatalf("expected FirstOrDefault (existential inner) in plan for %q, got:\n%s", q, plan)
		}
		// The outer of the FlatMap must be the two-table inner join — this is
		// what makes the EXISTS correlate to a JOINED-outer column (the P1a
		// dimension). A degenerate single-table outer would not exercise the
		// merged-row alias rebase.
		if !strings.Contains(plan, "NestedLoopJoin") {
			t.Fatalf("expected NestedLoopJoin (two-table outer) in plan for %q, got:\n%s", q, plan)
		}
	}

	queryNames := func(t *testing.T, q string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		sort.Strings(out)
		return out
	}

	// WHERE EXISTS: join emp ⋈ dept on dept_id, keep employees who own a
	// project. emp 1 (Alice) and emp 3 (Carol) own projects.
	t.Run("where_exists_over_join", func(t *testing.T) {
		q := `SELECT e.fname
		      FROM emp AS e, dept AS d
		      WHERE e.dept_id = d.id
		        AND EXISTS (SELECT 1 FROM proj AS p WHERE p.owner_id = e.id)`
		requireJoinAboveExists(t, q)
		got := queryNames(t, q)
		want := []string{"Alice", "Carol"}
		if !equalStrings(got, want) {
			t.Errorf("WHERE EXISTS over join: got %v, want %v", got, want)
		}
	})

	// WHERE NOT EXISTS: the complement — employees in a joined dept who own
	// NO project. emp 2 (Bob) and emp 4 (Dave).
	t.Run("where_not_exists_over_join", func(t *testing.T) {
		q := `SELECT e.fname
		      FROM emp AS e, dept AS d
		      WHERE e.dept_id = d.id
		        AND NOT EXISTS (SELECT 1 FROM proj AS p WHERE p.owner_id = e.id)`
		requireJoinAboveExists(t, q)
		got := queryNames(t, q)
		want := []string{"Bob", "Dave"}
		if !equalStrings(got, want) {
			t.Errorf("WHERE NOT EXISTS over join: got %v, want %v", got, want)
		}
	})

	// Correlate the EXISTS to the RIGHT join leg (dept) ONLY. This is the
	// revert-proof discriminator: the merged row's last-leg-wins BARE `id`
	// holds emp.id (∈ {1..4}); the dept leg's id (∈ {10,20}) lives ONLY under
	// the qualified key D.ID. A correct rebase resolves `d.id` via D.ID and
	// finds the proj rows whose dept_ref = 10 ⇒ employees in dept 10 (Alice,
	// Bob). A NAIVE bare-key rebase would resolve `d.id` to emp.id (1,2,3,4),
	// which never equals any proj.dept_ref (10) ⇒ ZERO rows — a detectable
	// wrong answer. (The unfixed code drops all rows too; this also pins the
	// fix against the tempting-but-wrong simpler patch.)
	t.Run("where_exists_correlates_right_leg_only", func(t *testing.T) {
		q := `SELECT e.fname
		      FROM emp AS e, dept AS d
		      WHERE e.dept_id = d.id
		        AND EXISTS (SELECT 1 FROM proj AS p WHERE p.dept_ref = d.id)`
		requireJoinAboveExists(t, q)
		got := queryNames(t, q)
		// Only dept 10 is referenced by a proj ⇒ its employees: Alice, Bob.
		want := []string{"Alice", "Bob"}
		if !equalStrings(got, want) {
			t.Errorf("WHERE EXISTS (right leg, dept): got %v, want %v", got, want)
		}
	})

	// Both legs in one EXISTS: correlate to emp.id (owner) AND dept.id
	// (dept_ref). emp 1 (Alice, dept 10) owns proj 100/200 with dept_ref 10 ⇒
	// matches; emp 3 (Carol, dept 20) owns proj 300 with dept_ref 10 ≠ her
	// dept 20 ⇒ NO match. So only Alice qualifies — proving BOTH leg refs are
	// rebased to the correct qualified keys simultaneously.
	t.Run("where_exists_correlates_both_legs", func(t *testing.T) {
		q := `SELECT e.fname
		      FROM emp AS e, dept AS d
		      WHERE e.dept_id = d.id
		        AND EXISTS (SELECT 1 FROM proj AS p
		                    WHERE p.owner_id = e.id AND p.dept_ref = d.id)`
		requireJoinAboveExists(t, q)
		got := queryNames(t, q)
		want := []string{"Alice"}
		if !equalStrings(got, want) {
			t.Errorf("WHERE EXISTS (both legs): got %v, want %v", got, want)
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestFDB_ProjectedExists_FastPath_AliasBinding pins RFC-141 Phase 2 P1b:
// a PROJECTED `SELECT x, EXISTS(...)` whose EXISTS correlation matches the
// inner table's PRIMARY KEY (or a secondary index), taking the
// correlated-scan fast path (tryExistsFlatMap → buildExistsFlatMap →
// yieldExistsFlatMap).
//
// The bug: the fast path pushes the correlation into a parameterized
// PK/index scan, wraps it in FirstOrDefault, and binds the inner row
// under innerCorrelation — but yields the FlatMap with the ORIGINAL
// sel.GetResultValue() unchanged. That result value still references the
// existential QUANTIFIER alias produced by BuildExists (e.g. q$NN), which
// is NOT bound under innerCorrelation. So the projected ExistsValue.Evaluate
// can't find its binding and returns FALSE for EVERY matched row.
//
// The NON-fast path (implementExistentialSelect) already fixes this via
// remapExistentialResultValue; the fast path must do the same rebase.
//
// Here `t2.id = t1.ref` correlates the EXISTS to t2's PRIMARY KEY (id), so
// the fast path fires (a single-row correlated PK probe under FirstOrDefault).
func TestFDB_ProjectedExists_FastPath_AliasBinding(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_projexistsfast")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_projexistsfast")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE pef_tmpl "+
		"CREATE TABLE t1(id BIGINT NOT NULL, ref BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT NOT NULL, payload STRING, PRIMARY KEY(id)) "+
		"CREATE TABLE t3(id BIGINT NOT NULL, sec BIGINT, payload STRING, PRIMARY KEY(id)) "+
		"CREATE INDEX t3_sec ON t3 (sec)")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_projexistsfast/s WITH TEMPLATE pef_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_projexistsfast?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// t1.ref points at t2.id for rows 1 and 3; row 2 points at a missing t2.
	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 100), (2, 999), (3, 300)")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 'a'), (300, 'c')")
	// t3 secondary index target rows.
	mustExec(t, db, ctx, "INSERT INTO t3 VALUES (1000, 100, 'x'), (3000, 300, 'z')")

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
		sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
		return out
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

	// requireFastPath asserts the correlated PK/index probe fast path fired:
	// a FlatMap whose inner is a FirstOrDefault over a single-row correlated
	// scan/index probe (NOT a full inner scan). It requires FirstOrDefault AND
	// the given bound-probe marker — `Scan(...[=])` for a PK probe or
	// `IndexScan(...[=])` for a secondary-index probe — so the test proves the
	// OPTIMIZATION (a parameterized equality probe), not just the answer. A
	// full-scan fallback would not contain `[=]` and is rejected.
	requireFastPath := func(t *testing.T, q, boundMarker string) string {
		t.Helper()
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN %q: %v", q, err)
		}
		if !strings.Contains(plan, "FlatMap") {
			t.Fatalf("expected FlatMap for %q, got:\n%s", q, plan)
		}
		if !strings.Contains(plan, "FirstOrDefault") {
			t.Fatalf("expected FirstOrDefault (existential inner) for %q, got:\n%s", q, plan)
		}
		if !strings.Contains(plan, boundMarker) {
			t.Fatalf("expected bound probe %q (fast path) for %q, got:\n%s", boundMarker, q, plan)
		}
		return plan
	}

	// Case A: projected EXISTS correlated to inner PK ⇒ fast path PK probe.
	// The inner is a parameterized PK-equality scan `Scan(T2, [=])`.
	t.Run("projected_exists_pk_fast_path", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.id = t1.ref) AS has_t2 FROM t1"
		requireFastPath(t, q, "Scan(T2, [=])")
		got := queryIDBool(t, q)
		want := []idBool{{1, true}, {2, false}, {3, true}}
		if !eqIDBool(got, want) {
			t.Errorf("projected EXISTS (PK fast path): got %v, want %v", got, want)
		}
	})

	// Case B: NOT EXISTS on the PK fast path — the complement booleans.
	t.Run("projected_not_exists_pk_fast_path", func(t *testing.T) {
		q := "SELECT id, NOT EXISTS (SELECT 1 FROM t2 WHERE t2.id = t1.ref) AS no_t2 FROM t1"
		requireFastPath(t, q, "Scan(T2, [=])")
		got := queryIDBool(t, q)
		want := []idBool{{1, false}, {2, true}, {3, false}}
		if !eqIDBool(got, want) {
			t.Errorf("projected NOT EXISTS (PK fast path): got %v, want %v", got, want)
		}
	})

	// Case C: projected EXISTS correlated to a SECONDARY INDEX equality ⇒
	// the index-probe fast path (the second branch of tryExistsFlatMap). The
	// inner is a parameterized index-equality scan `IndexScan(T3_SEC, [=])`.
	t.Run("projected_exists_secondary_index_fast_path", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t3 WHERE t3.sec = t1.ref) AS has_t3 FROM t1"
		requireFastPath(t, q, "IndexScan(T3_SEC, [=])")
		got := queryIDBool(t, q)
		// t3 has sec=100 (row 1000) and sec=300 (row 3000); t1.ref is
		// 100,999,300 ⇒ true,false,true.
		want := []idBool{{1, true}, {2, false}, {3, true}}
		if !eqIDBool(got, want) {
			t.Errorf("projected EXISTS (secondary-index fast path): got %v, want %v", got, want)
		}
	})
}
