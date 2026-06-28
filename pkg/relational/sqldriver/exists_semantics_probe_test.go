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
}
