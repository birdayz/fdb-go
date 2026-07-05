package sqldriver_test

// Probes EXISTS / NOT EXISTS across the dimensions that historically hid bugs:
// correlated vs NON-correlated, subquery-empty vs non-empty, and NULL/orphan
// child keys. (CLAUDE.md: non-correlated EXISTS was once wrong on master with
// green CI because every NOT EXISTS test was correlated.)

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_ExistsSemanticsProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_exists")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_exists")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE existstpl "+
			"CREATE TABLE parent (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE child (id BIGINT NOT NULL, pid BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX child_pid ON child (pid)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_exists/s WITH TEMPLATE existstpl")
	dsn := fmt.Sprintf("fdbsql:///testdb_exists?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO parent (id) VALUES (1), (2), (3)")
	// children for p1 (x2), p2 (x1); none for p3. Plus orphan pid=99 and NULL pid.
	mwjoMustExec(t, db, ctx, "INSERT INTO child (id, pid) VALUES (10,1),(11,1),(12,2),(13,99)")
	mwjoMustExec(t, db, ctx, "INSERT INTO child (id) VALUES (14)") // pid NULL

	ids := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			out = append(out, v)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	eq := func(g, w []int64) bool {
		if len(g) != len(w) {
			return false
		}
		for i := range g {
			if g[i] != w[i] {
				return false
			}
		}
		return true
	}
	ck := func(name, q string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := ids(q); !eq(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	// Correlated EXISTS: parents that have a child. p1,p2 yes; p3 no.
	ck("correlated_exists", "SELECT id FROM parent p WHERE EXISTS (SELECT 1 FROM child c WHERE c.pid = p.id)", []int64{1, 2})
	// Correlated NOT EXISTS: parents with no child. p3 only.
	ck("correlated_not_exists", "SELECT id FROM parent p WHERE NOT EXISTS (SELECT 1 FROM child c WHERE c.pid = p.id)", []int64{3})

	// NON-correlated EXISTS over non-empty child → all parents.
	ck("noncorrelated_exists_nonempty", "SELECT id FROM parent WHERE EXISTS (SELECT 1 FROM child)", []int64{1, 2, 3})
	// NON-correlated NOT EXISTS over non-empty child → none.
	ck("noncorrelated_not_exists_nonempty", "SELECT id FROM parent WHERE NOT EXISTS (SELECT 1 FROM child)", nil)

	// NON-correlated EXISTS with a filter that matches NOTHING → EXISTS false → none.
	ck("noncorrelated_exists_emptyfilter", "SELECT id FROM parent WHERE EXISTS (SELECT 1 FROM child WHERE pid = 12345)", nil)
	// NON-correlated NOT EXISTS with empty filter → NOT EXISTS true → all parents.
	ck("noncorrelated_not_exists_emptyfilter", "SELECT id FROM parent WHERE NOT EXISTS (SELECT 1 FROM child WHERE pid = 12345)", []int64{1, 2, 3})

	// NON-correlated EXISTS with a filter that DOES match → all parents.
	ck("noncorrelated_exists_matchfilter", "SELECT id FROM parent WHERE EXISTS (SELECT 1 FROM child WHERE pid = 1)", []int64{1, 2, 3})

	// Correlated NOT EXISTS unaffected by orphan/NULL children (pid=99, pid=NULL
	// match no parent, but p1/p2 still have real children).
	ck("correlated_not_exists_with_orphans", "SELECT id FROM parent p WHERE NOT EXISTS (SELECT 1 FROM child c WHERE c.pid = p.id)", []int64{3})

	// OUTER-ONLY conjunct INSIDE the subquery's WHERE. It must evaluate UNDER the
	// ∃ — ¬∃(P∧Q) ≡ ¬P ∨ ¬∃(Q) — never as an outer PRE-filter, which computes
	// P ∧ ¬∃(Q) and wrongly drops every ¬P outer row. (The positive polarity is
	// insensitive: P ∧ ∃(Q) ≡ ∃(P∧Q) for an outer-only P — which is exactly how
	// the pre-filter bug shipped green: every existing test was either positive
	// or had the conjunct OUTSIDE the subquery.)
	ck("exists_outer_only_conjunct",
		"SELECT id FROM parent p WHERE EXISTS (SELECT 1 FROM child c WHERE p.id = 1)", []int64{1})
	ck("not_exists_outer_only_conjunct",
		"SELECT id FROM parent p WHERE NOT EXISTS (SELECT 1 FROM child c WHERE p.id = 1)", []int64{2, 3})
	ck("not_exists_outer_only_plus_correlated",
		"SELECT id FROM parent p WHERE NOT EXISTS (SELECT 1 FROM child c WHERE c.pid = p.id AND p.id = 1)", []int64{2, 3})
	// Control: a conjunct OUTSIDE the subquery (a sibling of the NOT EXISTS
	// marker) legitimately pre-filters the outer in BOTH polarities.
	ck("not_exists_sibling_conjunct_prefilters",
		"SELECT id FROM parent p WHERE p.id <> 1 AND NOT EXISTS (SELECT 1 FROM child c WHERE c.pid = p.id)", []int64{3})

	// Control: the same scalar at the TOP level (no EXISTS) — the long-supported
	// shape. Discriminates a broken scalar pipeline from a broken EXISTS threading.
	ck("scalar_toplevel_control",
		"SELECT id FROM parent p WHERE p.id > (SELECT MIN(c2.pid) FROM child c2) - 0", []int64{2, 3})

	// A SCALAR SUBQUERY inside the EXISTS subquery's WHERE. The scalar must be
	// evaluated (pre-bound) before the comparison runs; an unevaluated binding
	// compares as NULL and silently drops every row. MIN(c.id)=10, so
	// p.id > 10-9 ⇔ p.id > 1 → {2,3}; with children required (c.pid = p.id) → {2}.
	ck("exists_with_uncorrelated_scalar_inside",
		"SELECT id FROM parent p WHERE EXISTS (SELECT 1 FROM child c WHERE c.pid = p.id AND p.id > (SELECT MIN(c2.id) FROM child c2) - 9)", []int64{2})
	// Outer-only comparison against the scalar (no inner correlation at all).
	ck("exists_with_uncorrelated_scalar_outer_only",
		"SELECT id FROM parent p WHERE EXISTS (SELECT 1 FROM child c WHERE p.id > (SELECT MIN(c2.id) FROM child c2) - 9)", []int64{2, 3})
	// CORRELATED scalar inside the EXISTS WHERE (`MAX(c2.id) WHERE c2.pid =
	// c.pid` — per-inner-row): NO evaluation path exists (the one-shot pre-eval
	// cannot re-run per row; the WHERE channel has no CorrelatedScalarSubquery
	// consumer). It used to SILENTLY return zero rows — the forbidden outcome.
	// Pinned LOUD until the per-row evaluation lands (want {1} then): the query
	// must ERROR, never silently drop rows.
	t.Run("exists_with_correlated_scalar_inside_declines_loud", func(t *testing.T) {
		_, qErr := db.QueryContext(ctx,
			"SELECT id FROM parent p WHERE EXISTS (SELECT 1 FROM child c WHERE c.pid = p.id AND c.id < (SELECT MAX(c2.id) FROM child c2 WHERE c2.pid = c.pid))")
		if qErr == nil {
			t.Fatal("correlated scalar inside EXISTS silently returned rows — must decline loudly (or, once per-row eval lands, flip this pin to want [1])")
		}
	})
}
