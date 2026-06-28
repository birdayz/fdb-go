package sqldriver_test

// Sibling coverage to the NULL equality-SARG fix: a correlated INEQUALITY /
// range index probe whose outer comparand is NULL. `b.k > NULL`, `b.k BETWEEN
// NULL AND x` etc. are UNKNOWN for every row → the probe must match nothing (the
// outer row null-extends under LEFT). Exercises the inequality NULL-comparand
// branch of scanComparisonsToTupleRange on the correlated-probe path.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_CorrelatedNullInequality(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_corrineq")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_corrineq")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE corrineq "+
			"CREATE TABLE a (id BIGINT NOT NULL, k BIGINT, lo BIGINT, hi BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, k BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX b_k ON b (k)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_corrineq/s WITH TEMPLATE corrineq")
	dsn := fmt.Sprintf("fdbsql:///testdb_corrineq?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// a: id1 k=5 lo=3 hi=7; id2 k=NULL lo=NULL hi=NULL. b: 120 rows k=1..120.
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, k, lo, hi) VALUES (1, 5, 3, 7)")
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id) VALUES (2)")
	var bVals []string
	for i := 1; i <= 120; i++ {
		bVals = append(bVals, fmt.Sprintf("(%d, %d)", i, i))
	}
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id, k) VALUES "+strings.Join(bVals, ", "))

	scalar := func(q string) int64 {
		var v int64
		if err := db.QueryRowContext(ctx, q).Scan(&v); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		return v
	}

	// b.k > a.k: a1(5) → b6..b120 = 115. a2(NULL) → empty (b.k > NULL UNKNOWN).
	t.Run("inner_gt", func(t *testing.T) {
		if got := scalar("SELECT COUNT(*) FROM a JOIN b ON b.k > a.k"); got != 115 {
			t.Errorf("INNER b.k>a.k count = %d, want 115 (a2 NULL → no match)", got)
		}
	})
	// LEFT: 115 matches (a1) + a2 null-extends = 116.
	t.Run("left_total", func(t *testing.T) {
		if got := scalar("SELECT COUNT(*) FROM a LEFT JOIN b ON b.k > a.k"); got != 116 {
			t.Errorf("LEFT b.k>a.k count = %d, want 116", got)
		}
	})
	// LEFT null-extended: only a2 (NULL k) → 1.
	t.Run("left_nullextended", func(t *testing.T) {
		if got := scalar("SELECT COUNT(*) FROM a LEFT JOIN b ON b.k > a.k WHERE b.id IS NULL"); got != 1 {
			t.Errorf("LEFT null-extended = %d, want 1 (only a2 NULL key)", got)
		}
	})
	// Correlated BETWEEN with NULL bounds: a1 → b.k in [3,7] = b3..b7 = 5; a2
	// (lo,hi NULL) → empty.
	t.Run("between_null_bounds", func(t *testing.T) {
		if got := scalar("SELECT COUNT(*) FROM a JOIN b ON b.k BETWEEN a.lo AND a.hi"); got != 5 {
			t.Errorf("INNER BETWEEN count = %d, want 5 (a1 [3,7]; a2 NULL bounds → no match)", got)
		}
	})
}
