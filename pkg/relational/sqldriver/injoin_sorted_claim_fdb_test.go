package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestFDB_InJoin_PreserveRequestClaimsNoOrder pins Java's IN-source choice for
// an IN with no ordering request: `WHERE a IN (3, 1, 2)` over an index on `a`
// elects an InJoin whose source is NOT sorted, because nothing asked for an
// order (Java ImplementInJoinRule passes a null sort order for a
// non-exhaustive request). A sorted claim arises only from a requested part
// the inner fixes through the explode; that claim carrying sorted values is
// pinned by TestInJoinRule_SortedClaimIsBackedByActuallySortedValues, and the
// InJoin executing in its in-value order by
// TestIntegration_InJoinRowContinuation_OneRowPerTx.
func TestFDB_InJoin_PreserveRequestClaimsNoOrder(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/FRL/testdb_injoin_sorted")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_injoin_sorted")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE injoin_sorted_tmpl "+
			"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_a ON t (a)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_injoin_sorted/s WITH TEMPLATE injoin_sorted_tmpl")
	dsn := fmt.Sprintf("fdbsql:///FRL/TESTDB_INJOIN_SORTED?cluster_file=%s&schema=S", clusterFilePath)
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
	if strings.Contains(up, "BINDING ASC") || strings.Contains(up, "BINDING DESC") {
		t.Fatalf("a preserve request must not claim a sorted IN source, got: %s", plan)
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
	slices.Sort(got)
	if want := []int64{1, 2, 3}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rows = %v, want the set %v", got, want)
	}
}
