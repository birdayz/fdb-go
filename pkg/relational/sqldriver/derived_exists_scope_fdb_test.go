package sqldriver_test

// Correlated EXISTS referencing a DERIVED-TABLE alias. The EXISTS subquery's
// outer scope (buildOuterScopeSources) registered only REAL tables and lateral
// unnest legs — a derived-table source (`FROM (SELECT ...) e`) resolved
// through analyzer.ResolveTable, failed silently, and the correlated
// reference died with 42703 ("no FROM source aliased as E" single-source,
// `column reference with qualifier "E" cannot be resolved` join form) even
// though the SELECT scope registers the same source via
// buildDerivedTableSource. Table-alias correlation, uncorrelated EXISTS, and
// plain derived joins always worked — the gap was EXISTS-correlation-specific.

import (
	"context"
	"database/sql"
	"sort"
	"testing"
)

func TestFDB_DerivedAliasExistsCorrelation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/drv_ex"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	const tmpl = "drv_ex_tmpl"
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
	if _, err := db.ExecContext(ctx, "INSERT INTO emp VALUES (1, 1, 'alice'), (2, 2, 'bob')"); err != nil {
		t.Fatalf("seed emp: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO dept VALUES (1, 'eng'), (2, 'ops'), (3, 'empty')"); err != nil {
		t.Fatalf("seed dept: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO badge VALUES (10, 1)"); err != nil {
		t.Fatalf("seed badge: %v", err)
	}

	want := func(t *testing.T, label, q string, want []string) {
		t.Helper()
		r, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Errorf("%s: query error: %v\n  sql: %s", label, err, q)
			return
		}
		defer r.Close()
		var got []string
		for r.Next() {
			var n sql.NullString
			if err := r.Scan(&n); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, n.String)
		}
		if err := r.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		sort.Strings(got)
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

	// (a) Single derived source, correlated EXISTS on the derived alias.
	want(t, "single/EXISTS",
		"SELECT e.fname FROM (SELECT * FROM emp) e "+
			"WHERE EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)",
		[]string{"alice"})
	want(t, "single/NOT EXISTS",
		"SELECT e.fname FROM (SELECT * FROM emp) e "+
			"WHERE NOT EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)",
		[]string{"bob"})

	// (b) Derived leg in a join, EXISTS correlated to the DERIVED alias.
	want(t, "join/EXISTS-derived",
		"SELECT e.fname FROM (SELECT * FROM emp) e JOIN dept d ON e.dept_id = d.id "+
			"WHERE EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)",
		[]string{"alice"})
	want(t, "join/NOT EXISTS-derived",
		"SELECT e.fname FROM (SELECT * FROM emp) e JOIN dept d ON e.dept_id = d.id "+
			"WHERE NOT EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)",
		[]string{"bob"})

	// (c) Derived leg as the JOIN's second source (`d JOIN (SELECT...) e`).
	want(t, "join2/EXISTS-derived",
		"SELECT d.dname FROM dept d JOIN (SELECT * FROM emp) e ON e.dept_id = d.id "+
			"WHERE EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)",
		[]string{"eng"})

	// (d) Projection-shaped derived body (column subset + rename): the
	// virtual schema follows the BODY's projections, not the base table.
	want(t, "renamed/EXISTS",
		"SELECT e.nm FROM (SELECT id AS eid, fname AS nm FROM emp) e "+
			"WHERE EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.eid)",
		[]string{"alice"})

	// (e) Regression guards: table-alias correlation and uncorrelated EXISTS
	// through a derived leg keep working.
	want(t, "guard/table-corr",
		"SELECT e.fname FROM (SELECT * FROM emp) e JOIN dept d ON e.dept_id = d.id "+
			"WHERE EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = d.id)",
		[]string{"alice"})
	want(t, "guard/uncorr",
		"SELECT e.fname FROM (SELECT * FROM emp) e "+
			"WHERE EXISTS (SELECT 1 FROM badge b)",
		[]string{"alice", "bob"})

	// (g) The INTERSECTION of the two fixed classes: a LEFT JOIN against a
	// DERIVED leg with the EXISTS correlated to the derived alias — the
	// derived-source scope registration and the correlated step-1
	// orientation (null-on-empty leg + dissolved box) meet here.
	want(t, "left-derived/EXISTS",
		"SELECT d.dname FROM dept d LEFT JOIN (SELECT * FROM emp) e ON e.dept_id = d.id "+
			"WHERE EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)",
		[]string{"eng"})
	want(t, "left-derived/NOT EXISTS",
		"SELECT d.dname FROM dept d LEFT JOIN (SELECT * FROM emp) e ON e.dept_id = d.id "+
			"WHERE NOT EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)",
		[]string{"empty", "ops"})

	// (h) A derived-table alias that SHADOWS an enclosing WITH-CTE of the
	// same name, the derived body referencing that CTE: the alias-carrier
	// wrapper reuses the CTE scope, so without the shadow-stack pop the
	// body's `FROM c` rebound to the wrapper itself (dropping the WITH
	// binding) and the query returned ZERO rows. Plain, qualified-star
	// (the rebuild path), and EXISTS-correlated forms.
	want(t, "cte-shadow/plain",
		"WITH c AS (SELECT id, fname FROM emp WHERE id = 1) "+
			"SELECT c.fname FROM (SELECT * FROM c) c",
		[]string{"alice"})
	// The EXISTS-correlated shadow form is a CURRENT LIMITATION: the EXISTS
	// outer-scope builder (buildDerivedTableSource) resolves the derived
	// body against the CATALOG only — a body reading a WITH-CTE is not
	// derivable there, and the query REJECTS LOUDLY (0AF00; it has never
	// returned rows). Wrong rows are the one forbidden outcome; flip to the
	// rows assert when the scope builder learns CTE bodies (booked in
	// TODO.md with the derived-alias follow-ons).
	{
		q := "WITH c AS (SELECT id, fname FROM emp) " +
			"SELECT c.fname FROM (SELECT * FROM c) c " +
			"WHERE EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = c.id)"
		r, err := db.QueryContext(ctx, q)
		if err == nil {
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
				t.Errorf("cte-shadow/exists returned WRONG rows: %v, want the loud rejection or exactly [alice]\n  sql: %s", got, q)
			}
		}
	}
	{
		q := "WITH c AS (SELECT id, fname FROM emp WHERE id = 1) " +
			"SELECT c.* FROM (SELECT * FROM c) c"
		r, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Errorf("cte-shadow/qstar: query error: %v\n  sql: %s", err, q)
		} else {
			n := 0
			var id sql.NullInt64
			var name sql.NullString
			for r.Next() {
				if err := r.Scan(&id, &name); err != nil {
					t.Fatalf("scan: %v", err)
				}
				n++
			}
			r.Close()
			if n != 1 || id.Int64 != 1 || name.String != "alice" {
				t.Errorf("cte-shadow/qstar: got %d rows (last id=%v name=%v), want exactly (1, alice)\n  sql: %s",
					n, id.Int64, name.String, q)
			}
		}
	}

	// (f) QUALIFIED STAR over the derived source + correlated EXISTS:
	// `SELECT e.*` routes the no-joins derived build through the
	// qualified-star REBUILD (needRebuild → buildLogicalPlanForSelect) — a
	// third derived arm that also dropped the alias wrapper (bare innerOp),
	// silently undoing the visitor path's alias fidelity for exactly this
	// shape (it rejected 42703 pre-scope-fix, 0AF00 mid-fix; wrong rows were
	// never possible only because the failures were loud). All three
	// derived arms now carry the LogicalCTE(alias) wrapper.
	{
		q := "SELECT e.* FROM (SELECT id, fname FROM emp) e " +
			"WHERE EXISTS (SELECT 1 FROM badge b WHERE b.emp_id = e.id)"
		r, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Errorf("qualified-star/EXISTS: query error: %v\n  sql: %s", err, q)
		} else {
			n := 0
			var id sql.NullInt64
			var name sql.NullString
			for r.Next() {
				if err := r.Scan(&id, &name); err != nil {
					t.Fatalf("scan: %v", err)
				}
				n++
			}
			r.Close()
			if n != 1 || id.Int64 != 1 || name.String != "alice" {
				t.Errorf("qualified-star/EXISTS: got %d rows (last id=%v name=%v), want exactly (1, alice)\n  sql: %s",
					n, id.Int64, name.String, q)
			}
		}
	}
}
