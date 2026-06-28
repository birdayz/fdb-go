package sqldriver_test

// Property test for the cross-type SARG fix: a DOUBLE indexed column probed with
// BOTH int-literal and double-literal predicates must equal a Go-side full-scan
// oracle (numeric comparison). Broadly regresses the int-literal-vs-DOUBLE fix
// across the predicate space. Fixed seed; data has integral/fractional doubles,
// negatives, zero, dups, NULLs.

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

func TestFDB_DoubleOracleConsistency(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dbloracle")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dbloracle")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE dbloracle "+
			"CREATE TABLE t (id BIGINT NOT NULL, k DOUBLE, PRIMARY KEY (id)) "+
			"CREATE INDEX t_k ON t (k)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dbloracle/s WITH TEMPLATE dbloracle")
	dsn := fmt.Sprintf("fdbsql:///testdb_dbloracle?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	rng := rand.New(rand.NewSource(628628))
	const n = 200
	type row struct {
		id     int64
		k      float64
		isNull bool
	}
	choices := []float64{-7.5, -3.0, -0.5, 0.0, 2.5, 5.0, 7.0, 7.5, 13.0, 13.5}
	model := make([]row, 0, n)
	for i := 0; i < n; i++ {
		r := row{id: int64(i)}
		if rng.Intn(10) == 0 {
			r.isNull = true
			mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t (id) VALUES (%d)", r.id))
		} else {
			r.k = choices[rng.Intn(len(choices))]
			mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t (id, k) VALUES (%d, %g)", r.id, r.k))
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
	oracle := func(pred func(k float64) bool) []int64 {
		var out []int64
		for _, r := range model {
			if !r.isNull && pred(r.k) {
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
		pred  func(k float64) bool
	}
	cases := []tc{
		// int-literal comparands (the fixed cross-type path)
		{"k = 5", func(k float64) bool { return k == 5 }},
		{"k = 7", func(k float64) bool { return k == 7 }},
		{"k > 3", func(k float64) bool { return k > 3 }},
		{"k >= 7", func(k float64) bool { return k >= 7 }},
		{"k < 0", func(k float64) bool { return k < 0 }},
		{"k <= 5", func(k float64) bool { return k <= 5 }},
		{"k <> 5", func(k float64) bool { return k != 5 }},
		{"k BETWEEN 0 AND 8", func(k float64) bool { return k >= 0 && k <= 8 }},
		{"k IN (5, 7, 13)", func(k float64) bool { return k == 5 || k == 7 || k == 13 }},
		{"k > 0 AND k < 10", func(k float64) bool { return k > 0 && k < 10 }},
		// double-literal comparands (always worked)
		{"k = 5.0", func(k float64) bool { return k == 5.0 }},
		{"k > 3.5", func(k float64) bool { return k > 3.5 }},
		{"k < 7.5", func(k float64) bool { return k < 7.5 }},
		{"k >= 7.5", func(k float64) bool { return k >= 7.5 }},
		{"k BETWEEN 2.5 AND 7.5", func(k float64) bool { return k >= 2.5 && k <= 7.5 }},
		{"k IN (2.5, 5.0, 13.5)", func(k float64) bool { return k == 2.5 || k == 5.0 || k == 13.5 }},
		{"k <= -0.5 OR k > 13", func(k float64) bool { return k <= -0.5 || k > 13 }},
	}

	mism := 0
	for _, c := range cases {
		if !eq(sqlIDs(c.where), oracle(c.pred)) {
			mism++
			t.Errorf("WHERE %s: SQL != oracle", c.where)
		}
	}
	if mism == 0 {
		t.Logf("all %d DOUBLE-column predicate shapes (int + double literals) match the oracle", len(cases))
	}
}
