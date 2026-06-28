package sqldriver_test

// Probes SQL name-resolution edges: ORDER BY ordinal / SELECT-alias / expr-alias
// (allowed), SELECT * and qualified/unqualified refs, and the SQL-standard
// scoping rule that a SELECT alias is NOT visible in WHERE / GROUP BY (rejected).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_ResolutionEdgeProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_resedgep")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_resedgep")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE resedgep "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_resedgep/s WITH TEMPLATE resedgep")
	dsn := fmt.Sprintf("fdbsql:///testdb_resedgep?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// a: id1=30, id2=10, id3=20
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a,b) VALUES (1,30,1),(2,10,2),(3,20,3)")

	orderedIDs := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id, second int64
			if err := rows.Scan(&id, &second); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, id)
		}
		return out
	}
	eqOrd := func(g, w []int64) bool {
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

	t.Run("order_by_ordinal", func(t *testing.T) {
		// ORDER BY 2 = ORDER BY a → a sorted 10,20,30 → ids 2,3,1.
		if got := orderedIDs("SELECT id, a FROM t ORDER BY 2"); !eqOrd(got, []int64{2, 3, 1}) {
			t.Errorf("ORDER BY 2 = %v, want [2 3 1]", got)
		}
	})
	t.Run("order_by_select_alias", func(t *testing.T) {
		if got := orderedIDs("SELECT id, a AS x FROM t ORDER BY x"); !eqOrd(got, []int64{2, 3, 1}) {
			t.Errorf("ORDER BY alias = %v, want [2 3 1]", got)
		}
	})
	t.Run("order_by_expr_alias", func(t *testing.T) {
		// a+b: id1=31, id2=12, id3=23 → sorted 12,23,31 → ids 2,3,1.
		if got := orderedIDs("SELECT id, a + b AS s FROM t ORDER BY s"); !eqOrd(got, []int64{2, 3, 1}) {
			t.Errorf("ORDER BY expr-alias = %v, want [2 3 1]", got)
		}
	})
	t.Run("select_star_columns", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT * FROM t")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		cols, _ := rows.Columns()
		rows.Close()
		if len(cols) != 3 {
			t.Errorf("SELECT * columns = %v, want 3 (id,a,b)", cols)
		}
	})
	// SQL-standard scoping: a SELECT alias is not visible in WHERE / GROUP BY.
	rejected := func(name, q string) {
		t.Run(name, func(t *testing.T) {
			_, err := db.QueryContext(ctx, q)
			if err == nil {
				t.Fatalf("%s unexpectedly succeeded; SELECT alias not visible in this clause (SQL scoping)", name)
			}
			if !strings.Contains(err.Error(), "42703") {
				t.Errorf("%s error = %v, want 42703 undefined-column", name, err)
			}
		})
	}
	// WHERE can never reference a SELECT alias in any SQL dialect (the alias does
	// not exist at WHERE-evaluation time). GROUP-BY-alias is intentionally NOT
	// asserted: it's dialect-dependent (Postgres allows it) and Java's
	// QueryVisitor has aliasedGroupByColumns handling, so pinning Go's current
	// rejection could pin a divergence — left for a Java-checked follow-up.
	rejected("alias_not_visible_in_where", "SELECT a AS x FROM t WHERE x > 15")
}
