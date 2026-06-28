package sqldriver_test

// Regression probe for the RFC-154 Phase 1 fail-closed change: an ON predicate
// the resolver cannot build now surfaces a clean error instead of being silently
// dropped → cross product. This must NOT break legitimate (resolver-buildable) ON
// shapes — function calls, arithmetic, IS [NOT] NULL, CASE, BETWEEN, LIKE,
// IN-value-list. Each asserts a hand-computed row set; an unexpected error here
// would mean the fail-closed gate over-rejects a valid ON clause.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_OnClauseShapes_StillWork(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_on_shapes")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_on_shapes")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE on_shapes "+
			"CREATE TABLE a (id BIGINT NOT NULL, x BIGINT, name STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, y BIGINT, name STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_on_shapes/s WITH TEMPLATE on_shapes")
	dsn := fmt.Sprintf("fdbsql:///testdb_on_shapes?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// a: (1, x=5, 'X'), (2, x=10, 'y')
	// c: (50, y=5, 'x'), (51, y=99, 'Y')
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, x, name) VALUES (1, 5, 'X'), (2, 10, 'y')")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, y, name) VALUES (50, 5, 'x'), (51, 99, 'Y')")

	check := func(name, on string, want []string) {
		t.Run(name, func(t *testing.T) {
			q := "SELECT a.id, c.id FROM a JOIN c ON " + on
			rows, err := db.QueryContext(ctx, q)
			if err != nil {
				t.Fatalf("query %q: %v (fail-closed gate over-rejected a valid ON?)", q, err)
			}
			got := siScanRows(t, rows)
			if !eqStrSlices(got, want) {
				t.Errorf("ON %q rows = %v, want %v", on, got, want)
			}
		})
	}

	// equi on values: a1.x=5=c50.y; a2.x=10 matches neither.
	check("equi_col_col", "a.x = c.y", []string{"1|50"})
	// arithmetic in ON: a.x+1 = c.y → a1:6 no, a2:11 no → empty. Use a.x = c.y-0.
	check("arithmetic", "a.x = c.y + 0", []string{"1|50"})
	// function calls both sides: UPPER('X')=UPPER('x') and UPPER('y')=UPPER('Y').
	check("function_both_sides", "UPPER(a.name) = UPPER(c.name)", []string{"1|50", "2|51"})
	// IS NOT NULL (sole, always true) → cross product.
	check("is_not_null_sole", "a.x IS NOT NULL", []string{"1|50", "1|51", "2|50", "2|51"})
	// BETWEEN in ON: c.y BETWEEN 1 AND 50 → c50(5) yes, c51(99) no → a × c50.
	check("between", "c.y BETWEEN 1 AND 50", []string{"1|50", "2|50"})
	// IN value list in ON (must NOT be rejected as a subquery).
	check("in_value_list", "c.y IN (5, 99)", []string{"1|50", "1|51", "2|50", "2|51"})
	// LIKE in ON.
	check("like", "c.name LIKE 'Y'", []string{"1|51", "2|51"})
	// CASE in ON: match when CASE(a.x>5 → c.y else 5) = c.y. a1.x=5→5=c.y: c50(5)✓
	// c51(99)✗; a2.x=10→c.y=c.y always true: c50✓ c51✓.
	check("case_when", "CASE WHEN a.x > 5 THEN c.y ELSE 5 END = c.y", []string{"1|50", "2|50", "2|51"})
	// compound AND with a function conjunct.
	check("compound_func_and_eq", "a.x = c.y AND UPPER(a.name) = 'X'", []string{"1|50"})
}
