package sqldriver_test

// Metamorphic bug hunter: runs the SAME query text against two schemas that
// hold IDENTICAL data and differ ONLY in which secondary indexes exist. Any
// row-set difference is a planner/index-matching defect, because the answer to
// a query may not depend on the presence of an index.
//
// Second oracle (TLP): for any predicate P, the id sets of `WHERE P`,
// `WHERE NOT P` and `WHERE P IS NULL` must partition the table exactly.

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

type mhDB struct {
	name string
	db   *sql.DB
}

func mhScanIDs(ctx context.Context, db *sql.DB, q string) ([]int64, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v sql.NullInt64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		if !v.Valid {
			out = append(out, -999999)
			continue
		}
		out = append(out, v.Int64)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func TestFDB_MetamorphicIndexDifferential(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_mh")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_mh")
	table := "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c DOUBLE, s STRING, f BOOLEAN, PRIMARY KEY (id)) "
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA TEMPLATE mh_idx "+table+
		"CREATE INDEX t_a ON t (a) "+
		"CREATE INDEX t_ab ON t (a, b) "+
		"CREATE INDEX t_c ON t (c) "+
		"CREATE INDEX t_s ON t (s) "+
		"CREATE INDEX t_ba ON t (b, a) "+
		"CREATE INDEX t_f ON t (f)")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA TEMPLATE mh_noidx "+table)
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_mh/si WITH TEMPLATE mh_idx")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_mh/sn WITH TEMPLATE mh_noidx")

	open := func(schema string) *sql.DB {
		dsn := fmt.Sprintf("fdbsql:///FRL/TESTDB_MH?cluster_file=%s&schema=%s", testkit.ClusterFile(), strings.ToUpper(schema))
		db, err := sql.Open("fdbsql", dsn)
		if err != nil {
			t.Fatalf("open %s: %v", schema, err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	idx := mhDB{"idx", open("si")}
	noidx := mhDB{"noidx", open("sn")}

	dataRand := rand.New(rand.NewSource(20260819))
	const nRows = 140
	var vals []string
	for i := 1; i <= nRows; i++ {
		vals = append(vals, testkit.MhRowLiteral(dataRand, i))
	}
	for start := 0; start < len(vals); start += 20 {
		end := start + 20
		if end > len(vals) {
			end = len(vals)
		}
		stmt := "INSERT INTO t " + testkit.MhCols + " VALUES " + strings.Join(vals[start:end], ", ")
		testkit.MustExecCtx(t, idx.db, ctx, stmt)
		testkit.MustExecCtx(t, noidx.db, ctx, stmt)
	}

	seed := int64(1)
	if s := os.Getenv("MH_SEED"); s != "" {
		fmt.Sscan(s, &seed)
	}
	iters := 150
	if s := os.Getenv("MH_ITERS"); s != "" {
		fmt.Sscan(s, &iters)
	}
	g := &testkit.MhGen{R: rand.New(rand.NewSource(seed))}

	allIDs, err := mhScanIDs(ctx, idx.db, "SELECT id FROM t ORDER BY id")
	if err != nil {
		t.Fatalf("baseline scan: %v", err)
	}
	if len(allIDs) != nRows {
		t.Fatalf("fixture population: got %d rows, want %d", len(allIDs), nRows)
	}

	bothErr, checked := 0, 0
	for i := 0; i < iters; i++ {
		p := g.Pred(2)
		q := "SELECT id FROM t WHERE " + p + " ORDER BY id"

		gi, ei := mhScanIDs(ctx, idx.db, q)
		gn, en := mhScanIDs(ctx, noidx.db, q)
		switch {
		case ei != nil && en != nil:
			bothErr++
			continue
		case ei != nil || en != nil:
			t.Errorf("ERROR-ASYMMETRY seed=%d i=%d\n  q: %s\n  idx err:   %v\n  noidx err: %v", seed, i, q, ei, en)
			continue
		}
		checked++
		if !mhEqIDs(gi, gn) {
			t.Errorf("ROW-DIFF seed=%d i=%d\n  q: %s\n  idx  (%d): %v\n  noidx(%d): %v\n  only-in-idx: %v\n  only-in-noidx: %v",
				seed, i, q, len(gi), gi, len(gn), gn, mhDiff(gi, gn), mhDiff(gn, gi))
			continue
		}

		// TLP: P / NOT P / P IS NULL must partition the table.
		// The P leg is q itself, already read on idx above as gi; only the
		// NOT P and P IS NULL legs need a query.
		union := append([]int64(nil), gi...)
		ok := true
		for _, variant := range []string{"NOT (" + p + ")", "(" + p + ") IS NULL"} {
			ids, err := mhScanIDs(ctx, idx.db, "SELECT id FROM t WHERE "+variant+" ORDER BY id")
			if err != nil {
				ok = false
				break
			}
			union = append(union, ids...)
		}
		if !ok {
			continue
		}
		sort.Slice(union, func(x, y int) bool { return union[x] < union[y] })
		if !mhEqIDs(union, allIDs) {
			t.Errorf("TLP-PARTITION seed=%d i=%d\n  p: %s\n  union(%d) != all(%d)\n  missing: %v\n  extra: %v",
				seed, i, p, len(union), len(allIDs), mhDiff(allIDs, union), mhDiff(union, allIDs))
		}
	}
	t.Logf("seed=%d iters=%d checked=%d both-error=%d", seed, iters, checked, bothErr)
	if checked < iters/2 {
		t.Fatalf("instrument dead: only %d/%d predicates were actually compared", checked, iters)
	}
}

func mhEqIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// mhDiff returns the multiset a-minus-b.
func mhDiff(a, b []int64) []int64 {
	cnt := map[int64]int{}
	for _, v := range b {
		cnt[v]++
	}
	var out []int64
	for _, v := range a {
		if cnt[v] > 0 {
			cnt[v]--
			continue
		}
		out = append(out, v)
	}
	return out
}

var _ = testkit.MhScanStrings
