package sqldriver_test

// Property test: string-column SARG result must equal a Go-side full-scan oracle
// (Go string comparison) across predicate shapes incl. LIKE prefix. Random
// strings (fixed seed) from a small alphabet → frequent prefixes/dups, plus
// empty strings and NULLs. Catches string tuple-ordering / SARG bugs.

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

func TestFDB_StringOracleConsistency(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_stroracle")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_stroracle")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE stroracle "+
			"CREATE TABLE t (id BIGINT NOT NULL, s STRING, PRIMARY KEY (id)) "+
			"CREATE INDEX t_s ON t (s)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_stroracle/s WITH TEMPLATE stroracle")
	dsn := fmt.Sprintf("fdbsql:///testdb_stroracle?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	rng := rand.New(rand.NewSource(99887766))
	const n = 150
	type rec struct {
		id     int64
		s      string
		isNull bool
	}
	letters := "ab"
	model := make([]rec, 0, n)
	for i := 0; i < n; i++ {
		r := rec{id: int64(i)}
		switch rng.Intn(12) {
		case 0:
			r.isNull = true
			mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t (id) VALUES (%d)", r.id))
		case 1:
			r.s = "" // empty string
			mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t (id, s) VALUES (%d, '')", r.id))
		default:
			ln := 1 + rng.Intn(4)
			var sb strings.Builder
			for j := 0; j < ln; j++ {
				sb.WriteByte(letters[rng.Intn(len(letters))])
			}
			r.s = sb.String()
			mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t (id, s) VALUES (%d, '%s')", r.id, r.s))
		}
		model = append(model, r)
	}

	sqlIDs := func(where string) []int64 {
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
	oracle := func(pred func(s string) bool) []int64 {
		var out []int64
		for _, r := range model {
			if !r.isNull && pred(r.s) {
				out = append(out, r.id)
			}
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

	type tc struct {
		where string
		pred  func(s string) bool
	}
	lits := []string{"a", "ab", "b", "aa", "ba"}
	var cases []tc
	for _, v := range lits {
		v := v
		cases = append(
			cases,
			tc{fmt.Sprintf("s = '%s'", v), func(s string) bool { return s == v }},
			tc{fmt.Sprintf("s < '%s'", v), func(s string) bool { return s < v }},
			tc{fmt.Sprintf("s > '%s'", v), func(s string) bool { return s > v }},
			tc{fmt.Sprintf("s <= '%s'", v), func(s string) bool { return s <= v }},
			tc{fmt.Sprintf("s >= '%s'", v), func(s string) bool { return s >= v }},
			tc{fmt.Sprintf("s LIKE '%s%%'", v), func(s string) bool { return strings.HasPrefix(s, v) }},
		)
	}
	cases = append(
		cases,
		tc{"s = ''", func(s string) bool { return s == "" }},
		tc{"s >= '' AND s < 'b'", func(s string) bool { return s >= "" && s < "b" }},
		tc{"s IN ('a', 'ab', 'ba')", func(s string) bool { return s == "a" || s == "ab" || s == "ba" }},
		tc{"s BETWEEN 'a' AND 'b'", func(s string) bool { return s >= "a" && s <= "b" }},
	)

	mism := 0
	for _, c := range cases {
		if !eq(sqlIDs(c.where), oracle(c.pred)) {
			mism++
			t.Errorf("WHERE %s: SQL != oracle", c.where)
		}
	}
	if mism == 0 {
		t.Logf("all %d string predicate shapes match the oracle", len(cases))
	}
}
