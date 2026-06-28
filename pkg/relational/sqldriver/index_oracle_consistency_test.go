package sqldriver_test

// Property test: for an indexed column, the SQL result (which uses the index
// SARG) must equal a Go-side full-scan oracle (filter every row in Go) for a
// wide range of predicate shapes. Any mismatch is a SARG correctness bug. Uses a
// fixed seed for determinism; data includes NULLs, negatives, zero, duplicates.

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

func TestFDB_IndexOracleConsistency(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_idxoracle")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_idxoracle")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE idxoracle "+
			"CREATE TABLE t (id BIGINT NOT NULL, k BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_k ON t (k)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_idxoracle/s WITH TEMPLATE idxoracle")
	dsn := fmt.Sprintf("fdbsql:///testdb_idxoracle?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	rng := rand.New(rand.NewSource(20260628))
	const n = 200
	// model: id -> (k, kNull)
	type row struct {
		id     int64
		k      int64
		isNull bool
	}
	model := make([]row, 0, n)
	for i := 0; i < n; i++ {
		r := row{id: int64(i)}
		switch rng.Intn(10) {
		case 0:
			r.isNull = true
		default:
			r.k = int64(rng.Intn(40) - 15) // range [-15, 24], dups likely
		}
		model = append(model, r)
		if r.isNull {
			mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t (id) VALUES (%d)", r.id))
		} else {
			mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t (id, k) VALUES (%d, %d)", r.id, r.k))
		}
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
	oracle := func(pred func(r row) bool) []int64 {
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

	type tc struct {
		where string
		pred  func(r row) bool
	}
	// SQL 3VL: a NULL k never satisfies an ordered/equality predicate.
	nn := func(f func(k int64) bool) func(r row) bool {
		return func(r row) bool { return !r.isNull && f(r.k) }
	}
	vals := []int64{-15, -7, -1, 0, 1, 5, 13, 24, 30}
	var cases []tc
	for _, v := range vals {
		v := v
		cases = append(
			cases,
			tc{fmt.Sprintf("k = %d", v), nn(func(k int64) bool { return k == v })},
			tc{fmt.Sprintf("k <> %d", v), nn(func(k int64) bool { return k != v })},
			tc{fmt.Sprintf("k < %d", v), nn(func(k int64) bool { return k < v })},
			tc{fmt.Sprintf("k <= %d", v), nn(func(k int64) bool { return k <= v })},
			tc{fmt.Sprintf("k > %d", v), nn(func(k int64) bool { return k > v })},
			tc{fmt.Sprintf("k >= %d", v), nn(func(k int64) bool { return k >= v })},
		)
	}
	cases = append(
		cases,
		tc{"k IS NULL", func(r row) bool { return r.isNull }},
		tc{"k IS NOT NULL", func(r row) bool { return !r.isNull }},
		tc{"k BETWEEN -5 AND 10", nn(func(k int64) bool { return k >= -5 && k <= 10 })},
		tc{"k IN (-7, 0, 5, 13)", nn(func(k int64) bool { return k == -7 || k == 0 || k == 5 || k == 13 })},
		tc{"k > 0 AND k < 10", nn(func(k int64) bool { return k > 0 && k < 10 })},
		tc{"k < -5 OR k > 15", nn(func(k int64) bool { return k < -5 || k > 15 })},
		tc{"k NOT IN (0, 1)", nn(func(k int64) bool { return k != 0 && k != 1 })},
	)

	mism := 0
	for _, c := range cases {
		got := sqlIDs(c.where)
		want := oracle(c.pred)
		if !eq(got, want) {
			mism++
			t.Errorf("WHERE %s: SQL got %d rows, oracle %d rows (mismatch)", c.where, len(got), len(want))
		}
	}
	if mism == 0 {
		t.Logf("all %d predicate shapes match the full-scan oracle", len(cases))
	}
}
