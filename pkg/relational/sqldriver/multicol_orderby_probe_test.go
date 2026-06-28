package sqldriver_test

// Probes multi-column ORDER BY with mixed ASC/DESC directions and NULL placement.
// Mixed directions can't be served by a single forward/reverse index scan, so the
// planner must sort (or compose) correctly. NULL placement must follow the FDB
// tuple convention consistently (NULL sorts smallest → first under ASC, last
// under DESC).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_MultiColOrderByProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_mcorder")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_mcorder")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE mcorder "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_ab ON t (a, b)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_mcorder/s WITH TEMPLATE mcorder")
	dsn := fmt.Sprintf("fdbsql:///testdb_mcorder?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// (a,b): (1,10) (1,20) (2,5) (2,40) (3,10)
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b) VALUES (1,1,10),(2,1,20),(3,2,5),(4,2,40),(5,3,10)")

	orderedIDs := func(q string) []int64 {
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
		return out
	}
	eqOrdered := func(g, w []int64) bool {
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

	// a ASC, b DESC: groups by a asc; within group b desc.
	// a=1: (2,b20)(1,b10); a=2: (4,b40)(3,b5); a=3: (5,b10).
	t.Run("a_asc_b_desc", func(t *testing.T) {
		got := orderedIDs("SELECT id FROM t ORDER BY a ASC, b DESC")
		if !eqOrdered(got, []int64{2, 1, 4, 3, 5}) {
			t.Errorf("ORDER BY a ASC, b DESC = %v, want [2 1 4 3 5]", got)
		}
	})

	// a DESC, b ASC: a=3:(5); a=2:(3,b5)(4,b40); a=1:(1,b10)(2,b20).
	t.Run("a_desc_b_asc", func(t *testing.T) {
		got := orderedIDs("SELECT id FROM t ORDER BY a DESC, b ASC")
		if !eqOrdered(got, []int64{5, 3, 4, 1, 2}) {
			t.Errorf("ORDER BY a DESC, b ASC = %v, want [5 3 4 1 2]", got)
		}
	})

	// Fully forward — index-served. a ASC, b ASC.
	t.Run("a_asc_b_asc", func(t *testing.T) {
		got := orderedIDs("SELECT id FROM t ORDER BY a ASC, b ASC")
		if !eqOrdered(got, []int64{1, 2, 3, 4, 5}) {
			t.Errorf("ORDER BY a ASC, b ASC = %v, want [1 2 3 4 5]", got)
		}
	})

	// Fully reverse — index-served (reverse scan). a DESC, b DESC.
	t.Run("a_desc_b_desc", func(t *testing.T) {
		got := orderedIDs("SELECT id FROM t ORDER BY a DESC, b DESC")
		if !eqOrdered(got, []int64{5, 4, 3, 2, 1}) {
			t.Errorf("ORDER BY a DESC, b DESC = %v, want [5 4 3 2 1]", got)
		}
	})
}

// NULL placement: NULL sorts smallest in FDB tuples → first under ASC, last
// under DESC. Pin that this is consistent.
func TestFDB_OrderByNullPlacement(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nullorder")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nullorder")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE nullorder "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_a ON t (a)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nullorder/s WITH TEMPLATE nullorder")
	dsn := fmt.Sprintf("fdbsql:///testdb_nullorder?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// a = 10, NULL, 20
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1, 10), (3, 20)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (2)")

	order := func(q string) []int64 {
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
		return out
	}
	// ASC: NULL (id2) first, then 10 (id1), 20 (id3).
	if got := order("SELECT id FROM t ORDER BY a ASC"); !(len(got) == 3 && got[0] == 2 && got[1] == 1 && got[2] == 3) {
		t.Errorf("ORDER BY a ASC = %v, want [2 1 3] (NULL first)", got)
	}
	// DESC: 20 (id3), 10 (id1), then NULL (id2) last.
	if got := order("SELECT id FROM t ORDER BY a DESC"); !(len(got) == 3 && got[0] == 3 && got[1] == 1 && got[2] == 2) {
		t.Errorf("ORDER BY a DESC = %v, want [3 1 2] (NULL last)", got)
	}
}
