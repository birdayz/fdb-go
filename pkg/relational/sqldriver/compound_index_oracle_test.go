package sqldriver_test

// Property test: compound predicates over a 2-column index (a,b) must equal a
// Go-side full-scan oracle. Exercises prefix-equality + trailing-range, IN on the
// leading column, ranges on both, and gaps — the compound-SARG space — across
// random data (NULLs, dups). Completes the single-column oracle trio.

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

func TestFDB_CompoundIndexOracle(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_cmpidxora")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_cmpidxora")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE cmpidxora "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_ab ON t (a, b)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_cmpidxora/s WITH TEMPLATE cmpidxora")
	dsn := fmt.Sprintf("fdbsql:///testdb_cmpidxora?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	rng := rand.New(rand.NewSource(13371337))
	const n = 180
	type rec struct {
		id   int64
		a, b int64
		aN   bool
		bN   bool
	}
	model := make([]rec, 0, n)
	for i := 0; i < n; i++ {
		r := rec{id: int64(i)}
		if rng.Intn(9) == 0 {
			r.aN = true
		} else {
			r.a = int64(rng.Intn(5)) // [0,4] heavy dup on leading col
		}
		if rng.Intn(9) == 0 {
			r.bN = true
		} else {
			r.b = int64(rng.Intn(10)) // [0,9]
		}
		model = append(model, r)
		cols, vals := "id", fmt.Sprintf("%d", r.id)
		if !r.aN {
			cols += ", a"
			vals += fmt.Sprintf(", %d", r.a)
		}
		if !r.bN {
			cols += ", b"
			vals += fmt.Sprintf(", %d", r.b)
		}
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t (%s) VALUES (%s)", cols, vals))
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
	oracle := func(pred func(r rec) bool) []int64 {
		var out []int64
		for _, r := range model {
			if pred(r) {
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
	// a-predicate and b-predicate combine; NULL never satisfies an ordered/eq pred.
	aEq := func(v int64) func(rec) bool { return func(r rec) bool { return !r.aN && r.a == v } }
	type tc struct {
		where string
		pred  func(r rec) bool
	}
	cases := []tc{
		{"a = 2 AND b > 5", func(r rec) bool { return !r.aN && r.a == 2 && !r.bN && r.b > 5 }},
		{"a = 2 AND b = 3", func(r rec) bool { return !r.aN && r.a == 2 && !r.bN && r.b == 3 }},
		{"a = 1 AND b BETWEEN 2 AND 7", func(r rec) bool { return !r.aN && r.a == 1 && !r.bN && r.b >= 2 && r.b <= 7 }},
		{"a > 1 AND b < 5", func(r rec) bool { return !r.aN && r.a > 1 && !r.bN && r.b < 5 }},
		{"a IN (0, 2, 4) AND b >= 3", func(r rec) bool {
			return !r.aN && (r.a == 0 || r.a == 2 || r.a == 4) && !r.bN && r.b >= 3
		}},
		{"a = 3", aEq(3)},
		{"a = 1 AND b IS NULL", func(r rec) bool { return !r.aN && r.a == 1 && r.bN }},
		{"a IS NULL AND b = 4", func(r rec) bool { return r.aN && !r.bN && r.b == 4 }},
		{"b = 4", func(r rec) bool { return !r.bN && r.b == 4 }}, // leading col unbound
		{"a = 2 OR b = 9", func(r rec) bool { return (!r.aN && r.a == 2) || (!r.bN && r.b == 9) }},
	}

	mism := 0
	for _, c := range cases {
		if !eq(sqlIDs(c.where), oracle(c.pred)) {
			mism++
			t.Errorf("WHERE %s: SQL != oracle", c.where)
		}
	}
	if mism == 0 {
		t.Logf("all %d compound (a,b) predicate shapes match the oracle", len(cases))
	}
}
