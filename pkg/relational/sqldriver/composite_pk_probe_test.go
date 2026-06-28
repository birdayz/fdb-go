package sqldriver_test

// Probes composite primary keys (PRIMARY KEY (a,b)) — wire-relevant key encoding
// + PK prefix/range scans and ordering: exact two-col lookup, prefix scan on the
// leading PK column, range on the trailing PK column, PK-order ORDER BY, and a
// duplicate composite PK → 23505.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_CompositePKProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_compk")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_compk")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE compk "+
			"CREATE TABLE t (a BIGINT NOT NULL, b BIGINT NOT NULL, v BIGINT, PRIMARY KEY (a, b))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_compk/s WITH TEMPLATE compk")
	dsn := fmt.Sprintf("fdbsql:///testdb_compk?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// (a,b): (1,1) (1,2) (1,3) (2,1) (2,2) (3,1)
	mwjoMustExec(t, db, ctx, "INSERT INTO t (a,b,v) VALUES (1,1,11),(1,2,12),(1,3,13),(2,1,21),(2,2,22),(3,1,31)")

	pairs := func(q string) []string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var a, b int64
			if err := rows.Scan(&a, &b); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, fmt.Sprintf("%d.%d", a, b))
		}
		return out
	}
	sorted := func(q string) []string {
		o := pairs(q)
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

	t.Run("exact_two_col_pk", func(t *testing.T) {
		if got := sorted("SELECT a, b FROM t WHERE a = 1 AND b = 2"); !eq(got, []string{"1.2"}) {
			t.Errorf("PK (1,2) = %v, want [1.2]", got)
		}
	})
	t.Run("prefix_leading_pk_col", func(t *testing.T) {
		if got := sorted("SELECT a, b FROM t WHERE a = 1"); !eq(got, []string{"1.1", "1.2", "1.3"}) {
			t.Errorf("PK prefix a=1 = %v, want [1.1 1.2 1.3]", got)
		}
	})
	t.Run("range_trailing_pk_col", func(t *testing.T) {
		if got := sorted("SELECT a, b FROM t WHERE a = 1 AND b > 1"); !eq(got, []string{"1.2", "1.3"}) {
			t.Errorf("PK a=1,b>1 = %v, want [1.2 1.3]", got)
		}
	})
	t.Run("order_by_pk", func(t *testing.T) {
		// PK order: (1,1),(1,2),(1,3),(2,1),(2,2),(3,1).
		got := pairs("SELECT a, b FROM t ORDER BY a, b")
		want := []string{"1.1", "1.2", "1.3", "2.1", "2.2", "3.1"}
		if !eq(got, want) {
			t.Errorf("ORDER BY a,b = %v, want %v", got, want)
		}
	})
	t.Run("order_by_pk_desc", func(t *testing.T) {
		got := pairs("SELECT a, b FROM t ORDER BY a DESC, b DESC")
		want := []string{"3.1", "2.2", "2.1", "1.3", "1.2", "1.1"}
		if !eq(got, want) {
			t.Errorf("ORDER BY a,b DESC = %v, want %v", got, want)
		}
	})
	t.Run("duplicate_composite_pk_23505", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO t (a,b,v) VALUES (1, 2, 999)")
		if err == nil || !strings.Contains(err.Error(), "23505") {
			t.Errorf("duplicate composite PK (1,2) error = %v, want 23505", err)
		}
	})
	t.Run("partial_pk_different_a_ok", func(t *testing.T) {
		// (1,5) is a new composite key (b differs) — must succeed.
		if _, err := db.ExecContext(ctx, "INSERT INTO t (a,b,v) VALUES (1, 5, 15)"); err != nil {
			t.Errorf("distinct composite PK (1,5) insert failed: %v", err)
		}
	})
}
