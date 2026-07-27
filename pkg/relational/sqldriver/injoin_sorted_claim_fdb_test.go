package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_InJoin_SortedClaimMatchesExecutionOrder is the end-to-end proof
// that a RecordQueryInJoinPlan claiming IsSorted() now actually EXECUTES its
// IN-list in that order, against real FDB — not just that the plan's
// GetInValues() field holds a sorted slice in a unit test.
//
// `WHERE a IN (3, 1, 2)` over an index on `a` (mirrors
// TestFDB_INProj_OuterProjectionOverInJoin's already-established InJoin
// election) has no requested ordering, so ImplementInJoinRule's only
// reachable arm today (buildSourcesFromProvided) fires: it matches the
// explode alias against the index scan's fixed equality binding and sets
// sorted=true unconditionally. Before this fix, extractInValues copied the
// literal list straight from the SQL text's (3, 1, 2) order into
// SetInValues, so the plan claimed sorted=true while iterating 3, 1, 2.
//
// There is no ORDER BY here on purpose: this query has no downstream
// InMemorySort to correct row order, so the raw row sequence IS the
// InJoin's own FlatMapPipelined-over-inValues iteration order — SQL makes
// no ordering promise for this query in general, but for THIS specific,
// single-threaded, non-concurrent execution against the fixed InJoin plan
// shape, the sequence is deterministic and is exactly what "sorted" should
// mean. That is the property this test pins.
func TestFDB_InJoin_SortedClaimMatchesExecutionOrder(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_injoin_sorted")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_injoin_sorted")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE injoin_sorted_tmpl "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_a ON t (a)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_injoin_sorted/s WITH TEMPLATE injoin_sorted_tmpl")
	dsn := fmt.Sprintf("fdbsql:///testdb_injoin_sorted?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	for i := 1; i <= 5; i++ {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", i, i))
	}

	explain := mwjoExplainer(t, db, ctx)
	plan := explain("SELECT id FROM t WHERE a IN (3, 1, 2)")
	up := strings.ToUpper(plan)
	if !strings.Contains(up, "INJOIN") {
		t.Fatalf("expected the query to elect an InJoin (optimization must fire), got: %s", plan)
	}
	if strings.Contains(up, "INUNION") {
		t.Fatalf("expected a bare InJoin, not an InUnion, got: %s", plan)
	}
	if !strings.Contains(up, "ASC") {
		t.Fatalf("expected the InJoin to claim an ascending sorted order (SORTED ASC), got: %s", plan)
	}

	rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE a IN (3, 1, 2)")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	want := []int64{1, 2, 3}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("InJoin claims SORTED ASC but the actual row execution order was %v, want %v (literal IN-list order was 3,1,2)", got, want)
	}
}
