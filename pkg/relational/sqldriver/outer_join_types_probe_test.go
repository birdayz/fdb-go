package sqldriver_test

// Probes all four join types and their null-extension semantics: INNER (matches
// only), LEFT (null-extends unmatched left), RIGHT (null-extends unmatched right),
// FULL OUTER (null-extends both). a=1,2 ; b=(10→a1),(20→a99 no match); a2 has no b.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_OuterJoinTypesProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ojt")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ojt")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE ojt "+
		"CREATE TABLE a (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
		"CREATE TABLE b (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX b_aid ON b (a_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ojt/s WITH TEMPLATE ojt")
	dsn := fmt.Sprintf("fdbsql:///testdb_ojt?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id) VALUES (1), (2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id, a_id) VALUES (10, 1), (20, 99)")

	pairs := func(q string) []string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		f := func(n sql.NullInt64) string {
			if n.Valid {
				return fmt.Sprintf("%d", n.Int64)
			}
			return "N"
		}
		var o []string
		for rows.Next() {
			var ai, bi sql.NullInt64
			if err := rows.Scan(&ai, &bi); err != nil {
				t.Fatalf("scan: %v", err)
			}
			o = append(o, f(ai)+"-"+f(bi))
		}
		sort.Strings(o)
		return o
	}
	eq := func(g, w []string) bool {
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
	ck := func(name, q string, want []string) {
		t.Run(name, func(t *testing.T) {
			if got := pairs(q); !eq(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	ck("inner_join", "SELECT a.id, b.id FROM a JOIN b ON b.a_id = a.id", []string{"1-10"})
	ck("left_join", "SELECT a.id, b.id FROM a LEFT JOIN b ON b.a_id = a.id", []string{"1-10", "2-N"})
	ck("right_join", "SELECT a.id, b.id FROM a RIGHT JOIN b ON b.a_id = a.id", []string{"1-10", "N-20"})
	ck("full_outer_join", "SELECT a.id, b.id FROM a FULL OUTER JOIN b ON b.a_id = a.id", []string{"1-10", "2-N", "N-20"})
}
