package sqldriver_test

// Probes for correlated EXISTS / NOT EXISTS variations in WHERE (the semi-join
// machinery the EXISTS-in-ON work builds on): correlated, with extra inner
// filter, multi-table inner, NOT EXISTS, and EXISTS combined with a regular
// conjunct.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_CorrelatedExistsProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_corr_exists")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_corr_exists")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE corr_exists "+
			"CREATE TABLE a (id BIGINT NOT NULL, x BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, a_id BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, b_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX b_a_id ON b (a_id) CREATE INDEX c_b_id ON c (b_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_corr_exists/s WITH TEMPLATE corr_exists")
	dsn := fmt.Sprintf("fdbsql:///testdb_corr_exists?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// a: 1,2,3,4. b: a1→{v8}, a2→{v3}, a3→none, a4→{v20}. c: b for a1's b only.
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, x) VALUES (1, 5), (2, 10), (3, 7), (4, 2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id, a_id, v) VALUES (100, 1, 8), (101, 2, 3), (102, 4, 20)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, b_id) VALUES (900, 100)")

	ints := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v sql.NullInt64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, v.Int64)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	eqi := func(g, w []int64) bool {
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
	check := func(name, q string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := ints(q); !eqi(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	// correlated EXISTS: a has a b → a1,a2,a4.
	check("correlated_exists", "SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.a_id = a.id)",
		[]int64{1, 2, 4})
	// correlated NOT EXISTS: a has no b → a3.
	check("correlated_not_exists", "SELECT id FROM a WHERE NOT EXISTS (SELECT 1 FROM b WHERE b.a_id = a.id)",
		[]int64{3})
	// EXISTS + extra inner filter: a has a b with v>5 → a1(v8), a4(v20). a2's b v3 fails.
	check("exists_inner_filter", "SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.a_id = a.id AND b.v > 5)",
		[]int64{1, 4})
	// EXISTS + outer conjunct: (a has b) AND a.x>5 → a1(x5)✗, a2(x10)✓, a4(x2)✗ → a2.
	check("exists_and_outer", "SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.a_id = a.id) AND a.x > 5",
		[]int64{2})
	// nested correlated EXISTS (two levels): a has a b that has a c.
	check("nested_exists", "SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.a_id = a.id AND EXISTS (SELECT 1 FROM c WHERE c.b_id = b.id))",
		[]int64{1})
	// multi-table inner EXISTS: a has a (b join c) chain.
	check("multitable_inner_exists", "SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b, c WHERE b.a_id = a.id AND c.b_id = b.id)",
		[]int64{1})
}
