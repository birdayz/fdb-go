package sqldriver_test

// Regression for a real wrong-rows bug: an equi-join on indexed columns plans
// as a correlated index-nested-loop (FlatMap(outer=Scan, inner=IndexScan([=])));
// when the outer's join key is NULL, the inner index probe was built as
// `k = <NULL>` and seeked the [null] index entries — wrongly MATCHING the inner's
// NULL-keyed rows (SQL NULL = NULL is UNKNOWN, must not match). Root cause:
// scanComparisonsToTupleRange appended a nil equality comparand to the scan
// prefix instead of returning an empty range. Fixed there. A COUNT of 119
// instead of 118 means NULL wrongly matched. (Both tables have 120 rows with
// indexes on k so the planner picks the index-probe join.)

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_CorrelatedNullKeyJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_hashnull")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_hashnull")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE hashnull "+
			"CREATE TABLE a (id BIGINT NOT NULL, k BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, k BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX a_k ON a (k) CREATE INDEX b_k ON b (k)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_hashnull/s WITH TEMPLATE hashnull")
	dsn := fmt.Sprintf("fdbsql:///testdb_hashnull?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Both tables 120 rows (k=id) to force the >=100-row hash-join path on the
	// materialized inner. a.k is NULL for id=5; b.k is NULL for id=10.
	const n = 120
	var aVals, bVals []string
	for i := 1; i <= n; i++ {
		if i == 5 {
			aVals = append(aVals, fmt.Sprintf("(%d, NULL)", i))
		} else {
			aVals = append(aVals, fmt.Sprintf("(%d, %d)", i, i))
		}
		if i == 10 {
			bVals = append(bVals, fmt.Sprintf("(%d, NULL)", i))
		} else {
			bVals = append(bVals, fmt.Sprintf("(%d, %d)", i, i))
		}
	}
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, k) VALUES "+strings.Join(aVals, ", "))
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id, k) VALUES "+strings.Join(bVals, ", "))

	scalar := func(q string) int64 {
		var v int64
		if err := db.QueryRowContext(ctx, q).Scan(&v); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		return v
	}

	// INNER equi-join on k. Matches for v in 1..120 except v=5 (a NULL) and
	// v=10 (b NULL) → 118. NULL must NOT match NULL.
	t.Run("inner_count_no_null_match", func(t *testing.T) {
		if got := scalar("SELECT COUNT(*) FROM a JOIN b ON a.k = b.k"); got != 118 {
			t.Errorf("INNER hash-join count = %d, want 118 (NULL≠NULL); 119 ⇒ NULL wrongly matched", got)
		}
	})

	// LEFT join: every a row is preserved (120); a5 (NULL key) null-extends.
	t.Run("left_count_preserves_all", func(t *testing.T) {
		// Each a (v≠5) matches exactly one b (except v=10 where b is NULL → a10
		// null-extends). So rows = 120 (one per a; matched or null-extended).
		if got := scalar("SELECT COUNT(*) FROM a LEFT JOIN b ON a.k = b.k"); got != 120 {
			t.Errorf("LEFT hash-join count = %d, want 120", got)
		}
	})

	// LEFT join: count a rows that null-extend (no b match): a5 (NULL key) and
	// a10 (k=10, but b10.k is NULL so no match) → 2.
	t.Run("left_nullextended_count", func(t *testing.T) {
		if got := scalar("SELECT COUNT(*) FROM a LEFT JOIN b ON a.k = b.k WHERE b.id IS NULL"); got != 2 {
			t.Errorf("LEFT null-extended count = %d, want 2 (a5 NULL-key, a10 vs b10-NULL)", got)
		}
	})
}
