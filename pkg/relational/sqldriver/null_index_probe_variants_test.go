package sqldriver_test

// Follow-up to the NULL-equality-SARG fix: exercise NULL keys through other
// index-probe shapes — multi-column index probe, correlated EXISTS / NOT EXISTS
// — all of which build equality SARGs via scanComparisonsToTupleRange. NULL keys
// must never match (NULL = x is UNKNOWN). >=100 rows to force the index-probe
// plan.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_NullIndexProbeVariants(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nullidxv")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nullidxv")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE nullidxv "+
			"CREATE TABLE t1 (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t2_ab ON t2 (a, b) CREATE INDEX t1_a ON t1 (a)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nullidxv/s WITH TEMPLATE nullidxv")
	dsn := fmt.Sprintf("fdbsql:///testdb_nullidxv?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Both tables 120 rows: a=id, b=id. t1 id=5 has b=NULL; t2 id=5 has b=NULL.
	// So for the (a,b) equi-join, the (t1.id=5, t2.id=5) pair has a=5=5 but
	// b=NULL=NULL → must NOT match. Every other v matches one t2.
	const n = 120
	var v1, v2 []string
	for i := 1; i <= n; i++ {
		if i == 5 {
			v1 = append(v1, fmt.Sprintf("(%d, %d, NULL)", i, i))
			v2 = append(v2, fmt.Sprintf("(%d, %d, NULL)", i, i))
		} else {
			v1 = append(v1, fmt.Sprintf("(%d, %d, %d)", i, i, i))
			v2 = append(v2, fmt.Sprintf("(%d, %d, %d)", i, i, i))
		}
	}
	mwjoMustExec(t, db, ctx, "INSERT INTO t1 (id, a, b) VALUES "+strings.Join(v1, ", "))
	mwjoMustExec(t, db, ctx, "INSERT INTO t2 (id, a, b) VALUES "+strings.Join(v2, ", "))

	scalar := func(q string) int64 {
		var v int64
		if err := db.QueryRowContext(ctx, q).Scan(&v); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		return v
	}

	// Multi-column index probe with NULL on the 2nd component: the v=5 pair has
	// b=NULL on both sides → must NOT match. 119 pairs (1..120 minus v=5).
	t.Run("multicol_null_2nd", func(t *testing.T) {
		got := scalar("SELECT COUNT(*) FROM t1 JOIN t2 ON t1.a = t2.a AND t1.b = t2.b")
		if got != 119 {
			t.Errorf("multi-col NULL-2nd join count = %d, want 119 (v=5 b=NULL must not match)", got)
		}
	})

	// Correlated EXISTS on a (a is non-null for all): every t1 has a t2 with
	// same a → 120.
	t.Run("exists_a_all_match", func(t *testing.T) {
		got := scalar("SELECT COUNT(*) FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.a = t1.a)")
		if got != 120 {
			t.Errorf("EXISTS on a count = %d, want 120", got)
		}
	})

	// Correlated EXISTS on (a,b): the v=5 t1 (b=NULL) must NOT find a t2 match
	// (t2.b=NULL too, NULL≠NULL) → 119 t1 rows have a match.
	t.Run("exists_ab_null_excluded", func(t *testing.T) {
		got := scalar("SELECT COUNT(*) FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.a = t1.a AND t2.b = t1.b)")
		if got != 119 {
			t.Errorf("EXISTS on (a,b) count = %d, want 119 (t1 id=5 b=NULL no match)", got)
		}
	})

	// Correlated NOT EXISTS on (a,b): only the v=5 t1 has NO match → 1.
	t.Run("not_exists_ab_only_null", func(t *testing.T) {
		got := scalar("SELECT COUNT(*) FROM t1 WHERE NOT EXISTS (SELECT 1 FROM t2 WHERE t2.a = t1.a AND t2.b = t1.b)")
		if got != 1 {
			t.Errorf("NOT EXISTS on (a,b) count = %d, want 1 (only t1 id=5)", got)
		}
	})
}
