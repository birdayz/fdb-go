package sqldriver_test

// Probes LIKE pattern matching: prefix (index SARG), suffix/infix (residual),
// `_` single-char and `%` multi-char wildcards, exact match, match-all, and
// case sensitivity. Prefix LIKE lowers to a STARTS_WITH index range; the rest
// are residual filters.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_LikePatternProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_likeprobe")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_likeprobe")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE likeprobe "+
			"CREATE TABLE t (id BIGINT NOT NULL, s STRING, PRIMARY KEY (id)) "+
			"CREATE INDEX t_s ON t (s)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_likeprobe/s WITH TEMPLATE likeprobe")
	dsn := fmt.Sprintf("fdbsql:///testdb_likeprobe?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// id1='' id2='abc' id3='abcd' id4='xabc' id5='ABC'
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, s) VALUES (1,''),(2,'abc'),(3,'abcd'),(4,'xabc'),(5,'ABC')")

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

	ck("prefix", "SELECT id FROM t WHERE s LIKE 'abc%'", []int64{2, 3})
	ck("suffix_case_sensitive", "SELECT id FROM t WHERE s LIKE '%abc'", []int64{2, 4})
	ck("infix", "SELECT id FROM t WHERE s LIKE '%bc%'", []int64{2, 3, 4})
	ck("exact", "SELECT id FROM t WHERE s LIKE 'abc'", []int64{2})
	ck("underscore_single", "SELECT id FROM t WHERE s LIKE 'ab_'", []int64{2})
	ck("prefix_then_underscore", "SELECT id FROM t WHERE s LIKE 'abc_'", []int64{3})
	ck("match_all", "SELECT id FROM t WHERE s LIKE '%'", []int64{1, 2, 3, 4, 5})
	ck("no_match", "SELECT id FROM t WHERE s LIKE 'zzz%'", nil)
	// Case sensitivity: 'ABC' must NOT match lowercase prefix.
	ck("case_sensitive_prefix", "SELECT id FROM t WHERE s LIKE 'AB%'", []int64{5})
}
