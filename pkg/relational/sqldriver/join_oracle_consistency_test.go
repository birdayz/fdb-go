package sqldriver_test

// Property test: equi- and theta-joins over two indexed tables must equal a
// Go-side nested-loop oracle implementing SQL join semantics (NULL keys never
// match; LEFT preserves unmatched left rows). Random data (fixed seed) with
// NULLs, dups, and overlapping key ranges catches join shapes not manually
// enumerated (the class where the NULL-join SARG bug lived).

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

func TestFDB_JoinOracleConsistency(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_joinoracle")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_joinoracle")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE joinoracle "+
			"CREATE TABLE a (id BIGINT NOT NULL, k BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, k BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX a_k ON a (k) CREATE INDEX b_k ON b (k)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_joinoracle/s WITH TEMPLATE joinoracle")
	dsn := fmt.Sprintf("fdbsql:///testdb_joinoracle?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	type rec struct {
		id     int64
		k      int64
		isNull bool
	}
	rng := rand.New(rand.NewSource(424242))
	gen := func(tbl string, n int) []rec {
		out := make([]rec, 0, n)
		for i := 0; i < n; i++ {
			r := rec{id: int64(i)}
			if rng.Intn(8) == 0 {
				r.isNull = true
				mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO %s (id) VALUES (%d)", tbl, r.id))
			} else {
				r.k = int64(rng.Intn(12)) // [0,11], heavy duplication → many matches
				mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO %s (id, k) VALUES (%d, %d)", tbl, r.id, r.k))
			}
			out = append(out, r)
		}
		return out
	}
	as := gen("a", 40)
	bs := gen("b", 40)

	sqlPairs := func(q string) []string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var x int64
			var y sql.NullInt64
			if err := rows.Scan(&x, &y); err != nil {
				t.Fatalf("scan: %v", err)
			}
			yy := "NULL"
			if y.Valid {
				yy = fmt.Sprintf("%d", y.Int64)
			}
			out = append(out, fmt.Sprintf("%d|%s", x, yy))
		}
		sort.Strings(out)
		return out
	}
	eqS := func(g, w []string) bool {
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

	innerOracle := func(match func(ak, bk int64) bool) []string {
		var out []string
		for _, ra := range as {
			if ra.isNull {
				continue
			}
			for _, rb := range bs {
				if rb.isNull {
					continue
				}
				if match(ra.k, rb.k) {
					out = append(out, fmt.Sprintf("%d|%d", ra.id, rb.id))
				}
			}
		}
		sort.Strings(out)
		return out
	}
	leftOracle := func(match func(ak, bk int64) bool) []string {
		var out []string
		for _, ra := range as {
			matched := false
			if !ra.isNull {
				for _, rb := range bs {
					if !rb.isNull && match(ra.k, rb.k) {
						out = append(out, fmt.Sprintf("%d|%d", ra.id, rb.id))
						matched = true
					}
				}
			}
			if !matched {
				out = append(out, fmt.Sprintf("%d|NULL", ra.id))
			}
		}
		sort.Strings(out)
		return out
	}

	t.Run("inner_equi", func(t *testing.T) {
		got := sqlPairs("SELECT a.id, b.id FROM a JOIN b ON a.k = b.k")
		want := innerOracle(func(ak, bk int64) bool { return ak == bk })
		if !eqS(got, want) {
			t.Errorf("INNER equi: SQL %d pairs, oracle %d pairs", len(got), len(want))
		}
	})
	t.Run("left_equi", func(t *testing.T) {
		got := sqlPairs("SELECT a.id, b.id FROM a LEFT JOIN b ON a.k = b.k")
		want := leftOracle(func(ak, bk int64) bool { return ak == bk })
		if !eqS(got, want) {
			t.Errorf("LEFT equi: SQL %d rows, oracle %d rows", len(got), len(want))
		}
	})
	t.Run("inner_theta_gt", func(t *testing.T) {
		got := sqlPairs("SELECT a.id, b.id FROM a JOIN b ON a.k > b.k")
		want := innerOracle(func(ak, bk int64) bool { return ak > bk })
		if !eqS(got, want) {
			t.Errorf("INNER theta >: SQL %d pairs, oracle %d pairs", len(got), len(want))
		}
	})
}
