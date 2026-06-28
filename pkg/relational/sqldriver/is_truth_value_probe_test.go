package sqldriver_test

// Probes IS [NOT] TRUE / FALSE truth-value predicates (3VL) on a boolean column
// with true/false/NULL. `IS UNKNOWN` is not in the grammar (Java's grammar
// likewise: testValue=(TRUE|FALSE|NULL_LITERAL)) → 42601, conformant; `IS NULL`
// is the equivalent for booleans.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_IsTruthValueProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_istv")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_istv")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE istv "+
			"CREATE TABLE t (id BIGINT NOT NULL, flag BOOLEAN, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_istv/s WITH TEMPLATE istv")
	dsn := fmt.Sprintf("fdbsql:///testdb_istv?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, flag) VALUES (1, true), (2, false)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (3)") // flag NULL

	ids := func(where string) []int64 {
		rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE "+where)
		if err != nil {
			t.Fatalf("query WHERE %s: %v", where, err)
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
	ck := func(name, where string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := ids(where); !eq(got, want) {
				t.Errorf("%s = %v, want %v", where, got, want)
			}
		})
	}

	ck("is_true", "flag IS TRUE", []int64{1})
	ck("is_false", "flag IS FALSE", []int64{2})
	ck("is_not_true", "flag IS NOT TRUE", []int64{2, 3})   // false OR NULL
	ck("is_not_false", "flag IS NOT FALSE", []int64{1, 3}) // true OR NULL
	ck("is_null_equiv_unknown", "flag IS NULL", []int64{3})
	ck("is_not_null", "flag IS NOT NULL", []int64{1, 2})

	t.Run("is_unknown_rejected", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT id FROM t WHERE flag IS UNKNOWN")
		if err == nil || !strings.Contains(err.Error(), "42601") {
			t.Errorf("IS UNKNOWN error = %v, want 42601 (not in grammar; use IS NULL)", err)
		}
	})
}
