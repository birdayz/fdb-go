package sqldriver_test

// Probes LIKE pattern semantics: `_` matches one char, `%` matches any run,
// matching is case-sensitive, and regex metacharacters are LITERAL (the pattern
// 'a.c' must match only the literal 'a.c', NOT 'abc' — a regex-leak bug would
// make '.' match any char). Also anchoring (LIKE is whole-string).

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
	setup := openTestDB(t, "/testdb_like")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_like")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE likep CREATE TABLE t (id BIGINT NOT NULL, s STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_like/s WITH TEMPLATE likep")
	dsn := fmt.Sprintf("fdbsql:///testdb_like?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// 1 abc, 2 aXc, 3 a.c, 4 ABC, 5 abcd, 6 (empty), 7 NULL
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, s) VALUES (1,'abc'),(2,'aXc'),(3,'a.c'),(4,'ABC'),(5,'abcd'),(6,'')")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (7)")

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

	// underscore = exactly one char: matches abc, aXc, a.c (3 chars a_c) — NOT ABC (case), NOT abcd (len).
	ck("underscore_one_char", "SELECT id FROM t WHERE s LIKE 'a_c'", []int64{1, 2, 3})
	// percent = any run: a%  matches all starting 'a' lowercase.
	ck("percent_prefix", "SELECT id FROM t WHERE s LIKE 'a%'", []int64{1, 2, 3, 5})
	// suffix.
	ck("percent_suffix", "SELECT id FROM t WHERE s LIKE '%c'", []int64{1, 2, 3}) // 'ABC' ends uppercase 'C', case-sensitive
	// CRITICAL: literal dot, NOT regex any-char → only 'a.c'.
	ck("literal_dot_not_regex", "SELECT id FROM t WHERE s LIKE 'a.c'", []int64{3})
	// exact (no wildcards) is whole-string anchored.
	ck("exact_anchored", "SELECT id FROM t WHERE s LIKE 'abc'", []int64{1})
	// case-sensitive: 'ABC' pattern matches only id=4.
	ck("case_sensitive", "SELECT id FROM t WHERE s LIKE 'ABC'", []int64{4})
	// % matches empty too → 'a%' won't match '' but '%' matches everything non-NULL.
	ck("percent_matches_all_nonnull", "SELECT id FROM t WHERE s LIKE '%'", []int64{1, 2, 3, 4, 5, 6})
	// NULL LIKE anything → UNKNOWN → excluded (id=7 never matches).
	ck("null_never_matches", "SELECT id FROM t WHERE s LIKE '%'", []int64{1, 2, 3, 4, 5, 6})
}
