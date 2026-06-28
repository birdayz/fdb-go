package sqldriver_test

// Probes null-safe join (`a.k IS NOT DISTINCT FROM b.k`) — the INVERSE of the
// regular equi-join: here NULL keys MUST match NULL (NULL IS NOT DISTINCT FROM
// NULL is TRUE), whereas `a.k = b.k` excludes them. Confirms both behaviors on
// the same data (the regular case is the NULL-join SARG bug fixed this session).

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_NullSafeJoinProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nsjoin")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nsjoin")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE nsjoin "+
			"CREATE TABLE a (id BIGINT NOT NULL, k BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, k BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX a_k ON a (k) CREATE INDEX b_k ON b (k)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nsjoin/s WITH TEMPLATE nsjoin")
	dsn := fmt.Sprintf("fdbsql:///testdb_nsjoin?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// a: id1 k=1, id2 k=NULL, id3 k=2 ; b: id1 k=1, id2 k=NULL, id3 k=9
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, k) VALUES (1, 1), (3, 2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id) VALUES (2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id, k) VALUES (1, 1), (3, 9)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id) VALUES (2)")

	pairs := func(q string) []string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var x, y int64
			if err := rows.Scan(&x, &y); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, fmt.Sprintf("%d.%d", x, y))
		}
		sort.Strings(out)
		return out
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

	t.Run("regular_equi_excludes_null", func(t *testing.T) {
		// a.k=b.k: only (1,1) [k=1]. NULL≠NULL → (2,2) excluded.
		got := pairs("SELECT a.id, b.id FROM a JOIN b ON a.k = b.k")
		if !eq(got, []string{"1.1"}) {
			t.Errorf("equi join = %v, want [1.1] (NULL excluded)", got)
		}
	})
	t.Run("null_safe_includes_null_match", func(t *testing.T) {
		// a.k IS NOT DISTINCT FROM b.k: (1,1) [1=1] AND (2,2) [NULL~NULL].
		got := pairs("SELECT a.id, b.id FROM a JOIN b ON a.k IS NOT DISTINCT FROM b.k")
		if !eq(got, []string{"1.1", "2.2"}) {
			t.Errorf("null-safe join = %v, want [1.1 2.2] (NULL matches NULL)", got)
		}
	})
}
