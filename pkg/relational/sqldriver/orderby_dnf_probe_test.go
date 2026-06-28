package sqldriver_test

// Probes for multi-key ORDER BY (mixed ASC/DESC) and disjunctive (DNF) WHERE
// predicates — ordering-key direction handling and OR-of-AND normalization.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_OrderByDNFProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ob_dnf")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ob_dnf")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE ob_dnf "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_ab ON t (a, b)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ob_dnf/s WITH TEMPLATE ob_dnf")
	dsn := fmt.Sprintf("fdbsql:///testdb_ob_dnf?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b) VALUES (1, 1, 2), (2, 1, 5), (3, 3, 4), (4, 3, 1), (5, 2, 2)")

	// orderedIDs preserves result order.
	orderedIDs := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, v)
		}
		return out
	}
	sortedIDs := func(q string) []int64 {
		out := orderedIDs(q)
		for i := 1; i < len(out); i++ {
			for j := i; j > 0 && out[j-1] > out[j]; j-- {
				out[j-1], out[j] = out[j], out[j-1]
			}
		}
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

	// ORDER BY a ASC, b DESC: a=1{b5 id2, b2 id1}, a=2{id5}, a=3{b4 id3, b1 id4}.
	t.Run("multikey_asc_desc", func(t *testing.T) {
		if got := orderedIDs("SELECT id FROM t ORDER BY a ASC, b DESC"); !eqi(got, []int64{2, 1, 5, 3, 4}) {
			t.Errorf("ORDER BY a ASC, b DESC = %v, want [2 1 5 3 4]", got)
		}
	})
	t.Run("multikey_desc_asc", func(t *testing.T) {
		// a DESC, b ASC: a=3{b1 id4, b4 id3}, a=2{id5}, a=1{b2 id1, b5 id2}.
		if got := orderedIDs("SELECT id FROM t ORDER BY a DESC, b ASC"); !eqi(got, []int64{4, 3, 5, 1, 2}) {
			t.Errorf("ORDER BY a DESC, b ASC = %v, want [4 3 5 1 2]", got)
		}
	})
	t.Run("dnf_two_conjuncts", func(t *testing.T) {
		if got := sortedIDs("SELECT id FROM t WHERE (a = 1 AND b = 2) OR (a = 3 AND b = 4)"); !eqi(got, []int64{1, 3}) {
			t.Errorf("DNF = %v, want [1 3]", got)
		}
	})
	t.Run("or_chain", func(t *testing.T) {
		if got := sortedIDs("SELECT id FROM t WHERE a = 1 OR a = 3"); !eqi(got, []int64{1, 2, 3, 4}) {
			t.Errorf("OR-chain = %v, want [1 2 3 4]", got)
		}
	})
	t.Run("and_or_mix", func(t *testing.T) {
		// (a = 1 OR a = 2) AND b >= 2 → a∈{1,2}: id1(b2),id2(b5),id5(b2); b>=2 all → [1,2,5].
		if got := sortedIDs("SELECT id FROM t WHERE (a = 1 OR a = 2) AND b >= 2"); !eqi(got, []int64{1, 2, 5}) {
			t.Errorf("AND-OR mix = %v, want [1 2 5]", got)
		}
	})
}
