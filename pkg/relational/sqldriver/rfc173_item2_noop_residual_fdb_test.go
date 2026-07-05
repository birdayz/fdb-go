package sqldriver_test

// RFC-173 QP-REF-BIND item 2, commit 1 — the no-op existential residual on
// LEFT JOIN + correlated EXISTS. Pre-fix, EXISTS and NOT EXISTS returned
// IDENTICAL rows (all depts): the dissolved LEFT box (INNER + null-on-empty
// quantifier) merged into the existential select, and the materialized step-1
// NLJ of the 3-quantifier existential implementation dropped the
// null-on-empty flag (LEFT degraded to INNER) and embedded the correlated
// ON-predicate leg standalone — its sibling reference then resolved against
// the WRONG leg via the correlation-unchecked frontier fallback (E.DEPT_ID =
// D.ID degenerated to emp.dept_id = emp.id → cross product; the residual then
// filtered per-row correctly, hence identical row sets for both polarities).
//
// The W4-left matrix missed the class because its LEFT variant carried a
// masking `d.id = 3` conjunct — every pin here is deliberately UNMASKED
// except the explicit masked regression pin. The ID column-name collision
// across all three tables is load-bearing (it is what made the orphaned
// reference resolve silently instead of loudly): keep the table shapes.

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"testing"
)

func TestFDB_RFC173Item2_LeftJoinExistsResidual(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/i2_noop"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	const tmpl = "i2_noop_tmpl"
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE emp (id BIGINT, dept_id BIGINT, fname STRING, PRIMARY KEY (id))"+
		" CREATE TABLE dept (id BIGINT, dname STRING, PRIMARY KEY (id))"+
		" CREATE TABLE badge (id BIGINT, emp_id BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE "+tmpl); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// alice(dept 1) HAS a badge (badge 10 → emp 1), bob(dept 2) has none;
	// dept 3 has NO emp — its LEFT-join row null-extends e.
	if _, err := db.ExecContext(ctx, "INSERT INTO emp VALUES (1, 1, 'alice'), (2, 2, 'bob')"); err != nil {
		t.Fatalf("seed emp: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO dept VALUES (1, 'eng'), (2, 'ops'), (3, 'empty')"); err != nil {
		t.Fatalf("seed dept: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO badge VALUES (10, 1)"); err != nil {
		t.Fatalf("seed badge: %v", err)
	}

	rows := func(t *testing.T, q string) []string {
		t.Helper()
		r, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query: %v\n  sql: %s", err, q)
		}
		defer r.Close()
		var got []string
		for r.Next() {
			var n sql.NullString
			if err := r.Scan(&n); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if n.Valid {
				got = append(got, n.String)
			} else {
				got = append(got, "<NULL>")
			}
		}
		if err := r.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		sort.Strings(got)
		return got
	}
	want := func(t *testing.T, label, q string, want []string) {
		t.Helper()
		got := rows(t, q)
		if len(got) != len(want) {
			t.Errorf("%s: rows = %v, want %v\n  sql: %s", label, got, want, q)
			return
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: rows = %v, want %v\n  sql: %s", label, got, want, q)
				return
			}
		}
	}

	// (A) The bug class, UNMASKED: LEFT + correlated EXISTS into the
	// NULL-SUPPLYING leg. Pre-fix both polarities returned all 3 depts.
	qA := "SELECT d.dname FROM dept d LEFT JOIN emp e ON e.dept_id = d.id " +
		"WHERE EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)"
	qAn := "SELECT d.dname FROM dept d LEFT JOIN emp e ON e.dept_id = d.id " +
		"WHERE NOT EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)"
	want(t, "A/EXISTS", qA, []string{"eng"})
	want(t, "A/NOT EXISTS", qAn, []string{"empty", "ops"})

	// (A-masked) The historical masked pin (W4-left pin g shape) keeps
	// passing: the masking conjunct must not regress.
	want(t, "A/masked",
		"SELECT d.dname FROM dept d LEFT JOIN emp e ON e.dept_id = d.id "+
			"WHERE d.id = 3 AND NOT EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)",
		[]string{"empty"})

	// (B) Correlation into the PRESERVED leg (the box quantifier is named by
	// the RIGHTMOST leg, so this variant binds under the OTHER alias).
	want(t, "B/EXISTS",
		"SELECT d.dname FROM dept d LEFT JOIN emp e ON e.dept_id = d.id "+
			"WHERE EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = d.id)",
		[]string{"eng"})
	want(t, "B/NOT EXISTS",
		"SELECT d.dname FROM dept d LEFT JOIN emp e ON e.dept_id = d.id "+
			"WHERE NOT EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = d.id)",
		[]string{"empty", "ops"})

	// (C) Mirrored declaration order: emp LEFT JOIN dept, correlation into
	// the preserved (emp) leg. bob's dept exists, so only the badge decides.
	want(t, "C/EXISTS",
		"SELECT e.fname FROM emp e LEFT JOIN dept d ON e.dept_id = d.id "+
			"WHERE EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)",
		[]string{"alice"})
	want(t, "C/NOT EXISTS",
		"SELECT e.fname FROM emp e LEFT JOIN dept d ON e.dept_id = d.id "+
			"WHERE NOT EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)",
		[]string{"bob"})

	// (D) RIGHT-join twins (normalized to LEFT-with-swapped-children; the
	// null-supplying side is the LEFT operand).
	want(t, "D/EXISTS",
		"SELECT d.dname FROM emp e RIGHT JOIN dept d ON e.dept_id = d.id "+
			"WHERE EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)",
		[]string{"eng"})
	want(t, "D/NOT EXISTS",
		"SELECT d.dname FROM emp e RIGHT JOIN dept d ON e.dept_id = d.id "+
			"WHERE NOT EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)",
		[]string{"empty", "ops"})

	// (E) INNER regression guard: the 2+1 flatten path must stay intact, and
	// the decline must not over-fire on the uncorrelated flat INNER shape —
	// pin the plan shape (NLJ step-1 under the existential FlatMap).
	qE := "SELECT d.dname FROM dept d JOIN emp e ON e.dept_id = d.id " +
		"WHERE EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)"
	want(t, "E/EXISTS", qE, []string{"eng"})
	want(t, "E/NOT EXISTS",
		"SELECT d.dname FROM dept d JOIN emp e ON e.dept_id = d.id "+
			"WHERE NOT EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)",
		[]string{"ops"})
	var planE string
	if err := db.QueryRowContext(ctx, "EXPLAIN "+qE).Scan(&planE); err != nil {
		t.Fatalf("explain INNER: %v", err)
	}
	if !containsAll(planE, "FlatMap", "NestedLoopJoin", "FirstOrDefault") {
		t.Errorf("INNER+EXISTS plan lost the flattened NLJ→FlatMap shape (decline over-fired?): %s", planE)
	}

	// (F) Derived-table leg through the flatten with the EXISTS correlated to
	// the TABLE alias: leg-shaped subtrees must keep planning (the
	// leg-correlation decline keys on CROSS-leg refs, not on a leg being a
	// subquery). The EXISTS-correlated-to-the-DERIVED-alias twin is a
	// separate pre-existing semantic-scope gap (42703), pinned and fixed in
	// its own commit.
	want(t, "F/derived",
		"SELECT e.fname FROM (SELECT * FROM emp) e JOIN dept d ON e.dept_id = d.id "+
			"WHERE EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = d.id)",
		[]string{"alice"})

	// (G) Plain LEFT JOIN (no EXISTS): the null-extension path is untouched.
	want(t, "G/plain LEFT",
		"SELECT e.fname FROM dept d LEFT JOIN emp e ON e.dept_id = d.id",
		[]string{"<NULL>", "alice", "bob"})

	// (I) WHERE-conjunct classes over LEFT + EXISTS: the conjunct is
	// WHERE-class and must see the null-extended row (filter ABOVE the
	// DefaultOnEmpty — Java's placement). I2 and I3 were silently wrong on
	// master (cross-product rows); the merge rebases the conjunct through
	// the box's anchored record constructor, whose hidden leg correlations
	// the split must re-expose (merge-seed-aware classification).
	want(t, "I1/conj+NOT EXISTS",
		"SELECT d.dname FROM dept d LEFT JOIN emp e ON e.dept_id = d.id "+
			"WHERE e.fname = 'alice' AND NOT EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)",
		nil)
	want(t, "I2/conj+EXISTS",
		"SELECT d.dname FROM dept d LEFT JOIN emp e ON e.dept_id = d.id "+
			"WHERE e.fname = 'alice' AND EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)",
		[]string{"eng"})
	want(t, "I3/antijoin+NOT EXISTS",
		"SELECT d.dname FROM dept d LEFT JOIN emp e ON e.dept_id = d.id "+
			"WHERE e.id IS NULL AND NOT EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)",
		[]string{"empty"})
	// The plain anti-join guard (no EXISTS): WHERE above the box, unchanged.
	want(t, "I4/plain antijoin",
		"SELECT d.dname FROM dept d LEFT JOIN emp e ON e.dept_id = d.id WHERE e.id IS NULL",
		[]string{"empty"})

	// (J) Projected EXISTS over an outer join: the fold is INNER-only and
	// the shape must REJECT cleanly (never wrong rows through a dead
	// existential binding).
	if _, err := db.QueryContext(ctx,
		"SELECT d.dname, EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id) "+
			"FROM dept d LEFT JOIN emp e ON e.dept_id = d.id"); err == nil {
		t.Error("projected EXISTS over LEFT JOIN must reject cleanly (INNER-only fold), got rows")
	}

	// (K) Scalar subquery INSIDE a correlated EXISTS body over a bare-scan
	// outer: the below-FOD predicate carries the pre-evaluated scalar's
	// binding alias — neither the outer binding nor an existential leg. The
	// fail-closed rebase authority used to DECLINE the yield (loud 0AF00);
	// before THAT guard the shape silently returned zero rows (the binding
	// never resolved below the FOD). The scalar alias is a ROOT-context
	// external binding — visible below the FOD in every filter arm — so the
	// authority now passes it through and the pin asserts the ROWS: badge
	// ids {1,2} > MIN(dept.id)=1 admits badge 2 (emp 1) → [alice].
	{
		q := "SELECT e.fname FROM emp e WHERE EXISTS (SELECT 1 FROM badge b " +
			"WHERE b.emp_id = e.id AND b.id > (SELECT MIN(id) FROM dept d2))"
		r, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Errorf("scalar-in-EXISTS must plan and execute (the scalar alias is a root-context binding): %v", err)
		} else {
			var got []string
			for r.Next() {
				var n sql.NullString
				if err := r.Scan(&n); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got = append(got, n.String)
			}
			r.Close()
			if len(got) != 1 || got[0] != "alice" {
				t.Errorf("scalar-in-EXISTS returned WRONG rows: %v, want exactly [alice]\n  sql: %s", got, q)
			}
		}
	}

	// (H) EXPLAIN shape + determinism for the bug class, both polarities:
	// the winner must carry the correlated step-1 (DefaultOnEmpty preserves
	// the LEFT null-extension under the existential FlatMap — the fix's
	// plan shape, not merely its rows), and one deterministic winner per
	// shape.
	for _, q := range []string{qA, qAn} {
		var first string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&first); err != nil {
			t.Fatalf("explain: %v", err)
		}
		if !containsAll(first, "FlatMap", "DefaultOnEmpty", "FirstOrDefault") {
			t.Errorf("bug-class plan lost the correlated step-1 shape: %s", first)
		}
		for i := 0; i < 9; i++ {
			var again string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&again); err != nil {
				t.Fatalf("explain %d: %v", i, err)
			}
			if again != first {
				t.Errorf("EXPLAIN is nondeterministic:\n  first: %s\n  again: %s", first, again)
				break
			}
		}
	}
}

// containsAll reports whether s contains every substring.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
