package sqldriver_test

// Pins the RFC-182 audit's #1 finding: the pk-intersection path must apply
// leg compensation (Java WithPrimaryKeyDataAccessRule's
// createIntersectionAndCompensation). Before the fix, WithPrimaryKeyIntersector
// hardcoded NoCompensation, so a residual predicate no leg covered VANISHED
// from the plan: `SELECT * ... WHERE category=? AND price=? AND name=?` over
// idx_category+idx_price planned to a bare Fetch(Intersection(...)) and
// returned rows failing name=? (row 5 'Monitor' leaked). The SELECT id
// variant masked the bug by winning with a single-index plan — projection is
// a load-bearing test axis here, never collapse the two cases.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_IntersectionResidualCompensation(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ixrescomp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ixrescomp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE ixrescomp CREATE TABLE items (id BIGINT NOT NULL, category STRING, name STRING, price BIGINT, PRIMARY KEY (id)) CREATE INDEX idx_category ON items (category) CREATE INDEX idx_price ON items (price)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ixrescomp/s WITH TEMPLATE ixrescomp")
	dsn := fmt.Sprintf("fdbsql:///testdb_ixrescomp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, `INSERT INTO items VALUES
		(1, 'books', 'Go Programming', 45),
		(2, 'books', 'Algorithms', 60),
		(3, 'electronics', 'Keyboard', 120),
		(4, 'electronics', 'Mouse', 30),
		(5, 'electronics', 'Monitor', 120)`)

	queryRows := func(q string) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns: %v", err)
		}
		var got []int64
		for rows.Next() {
			var id int64
			if len(cols) == 1 {
				if err := rows.Scan(&id); err != nil {
					t.Fatalf("scan: %v", err)
				}
			} else {
				var category, name string
				var price int64
				if err := rows.Scan(&id, &category, &name, &price); err != nil {
					t.Fatalf("scan: %v", err)
				}
			}
			got = append(got, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return got
	}
	explain := func(q string) string {
		t.Helper()
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
			t.Fatalf("explain %q: %v", q, err)
		}
		return plan
	}

	// The shape that leaked: both indexed predicates + an unindexed residual,
	// SELECT * (the projection under which the intersection wins).
	starQ := "SELECT * FROM items WHERE category = 'electronics' AND price = 120 AND name = 'Keyboard'"
	starPlan := explain(starQ)
	if !strings.Contains(starPlan, "Intersection(") {
		t.Errorf("SELECT * shape must still plan the pk-intersection (the 2-leg access), got: %s", starPlan)
	}
	if !strings.Contains(starPlan, "PredicatesFilter(") {
		t.Errorf("compensated intersection must carry the name residual as a filter ABOVE the intersection, got: %s", starPlan)
	}
	if got := queryRows(starQ); len(got) != 1 || got[0] != 3 {
		t.Errorf("SELECT * rows: got %v, want [3] — residual name='Keyboard' dropped by pk-intersection", got)
	}

	// The masking variant: projection changes the winner, rows must agree.
	idQ := "SELECT id FROM items WHERE category = 'electronics' AND price = 120 AND name = 'Keyboard'"
	if got := queryRows(idQ); len(got) != 1 || got[0] != 3 {
		t.Errorf("SELECT id rows: got %v, want [3]", got)
	}

	// Residual-free control: disjoint per-leg residuals set-intersect to
	// nothing (Java Compensation::intersect), so the bare intersection shape
	// is preserved — the fix must not have declined it.
	bareQ := "SELECT * FROM items WHERE category = 'electronics' AND price = 120"
	barePlan := explain(bareQ)
	if !strings.Contains(barePlan, "Intersection(") {
		t.Errorf("residual-free conjunction must keep the bare intersection, got: %s", barePlan)
	}
	if got := queryRows(bareQ); len(got) != 2 || got[0] == got[1] {
		t.Errorf("bare intersection rows: got %v, want [3 5] in some order", got)
	}
}
