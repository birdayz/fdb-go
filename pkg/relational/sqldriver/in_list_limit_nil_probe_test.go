package sqldriver_test

// Bug hunt probe (row-level): an IN-list query with a top-level LIMIT (no
// ORDER BY) must return the matching rows. Before the fix the limit wrapper did
// not relink its extracted inner, so the plan kept a nil inner
// (InJoin(<nil>)/Fetch(<nil>)) → 0 rows (non-covering) or an execution error
// (covering).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_InListLimitReturnsRows(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_inlimit")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_inlimit")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE inlimit "+
			"CREATE TABLE orders (id BIGINT, customer_id BIGINT, amount BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_customer ON orders(customer_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_inlimit/s WITH TEMPLATE inlimit")
	dsn := fmt.Sprintf("fdbsql:///testdb_inlimit?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO orders (id,customer_id,amount) VALUES (1,2,100),(2,3,200),(3,1,300),(4,4,400),(5,0,500)")

	count := func(q string) int {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		return n
	}

	// All 5 customer_ids are present; LIMIT 5 must return all 5 matching rows.
	if got := count("SELECT id, amount FROM orders WHERE customer_id IN (0,1,2,3,4) LIMIT 5"); got != 5 {
		t.Errorf("non-covering IN+LIMIT: got %d rows, want 5", got)
	}
	if got := count("SELECT id FROM orders WHERE customer_id IN (0,1,2,3,4) LIMIT 5"); got != 5 {
		t.Errorf("covering IN+LIMIT: got %d rows, want 5", got)
	}
	// A smaller LIMIT must cap correctly.
	if got := count("SELECT id, amount FROM orders WHERE customer_id IN (0,1,2,3,4) LIMIT 2"); got != 2 {
		t.Errorf("IN+LIMIT 2: got %d rows, want 2", got)
	}
}
