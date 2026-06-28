package sqldriver_test

// Verify the MATERIALIZED nested-loop-join hash path (nljCursor.tryBuildHashIndex,
// triggered at >=100 inner rows when NO index serves the join) rejects NULL=NULL.
// The hash index buckets NULL keys under a nil map key, so a NULL outer key
// probes the nil bucket and finds NULL-key inner rows — they must be rejected by
// the passesJoinPredicates re-check. (Distinct path from the index-probe NULL
// fix: here there is no index on the join column, so the planner materializes.)

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_MaterializedNLJNullKey(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_matnull")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_matnull")
	// NOTE: no index on k — forces a materialized NLJ (not an index probe).
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE matnull "+
			"CREATE TABLE a (id BIGINT NOT NULL, k BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, k BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_matnull/s WITH TEMPLATE matnull")
	dsn := fmt.Sprintf("fdbsql:///testdb_matnull?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

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

	// Confirm the plan materializes (NestedLoopJoin, not a FlatMap index probe).
	t.Run("uses_materialized_nlj", func(t *testing.T) {
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN SELECT a.id, b.id FROM a JOIN b ON a.k = b.k").Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		if !strings.Contains(plan, "NestedLoopJoin") {
			t.Fatalf("expected a materialized NestedLoopJoin plan, got: %s", plan)
		}
	})

	t.Run("inner_count_no_null_match", func(t *testing.T) {
		if got := scalar("SELECT COUNT(*) FROM a JOIN b ON a.k = b.k"); got != 118 {
			t.Errorf("materialized NLJ INNER count = %d, want 118 (NULL≠NULL)", got)
		}
	})
	t.Run("left_nullextended_count", func(t *testing.T) {
		if got := scalar("SELECT COUNT(*) FROM a LEFT JOIN b ON a.k = b.k WHERE b.id IS NULL"); got != 2 {
			t.Errorf("materialized NLJ LEFT null-extended = %d, want 2", got)
		}
	})
}
