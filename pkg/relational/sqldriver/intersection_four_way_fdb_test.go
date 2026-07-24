package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_FourWayIntersection proves RFC-190.5a through the production
// SQL→Cascades→executor path. Every decoy satisfies exactly three of the four
// indexed equalities, so omitting any leg (without retaining its residual)
// leaks a distinct wrong row.
func TestFDB_FourWayIntersection(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ix4way")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ix4way")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE ix4way "+
			"CREATE TABLE ix4 (id BIGINT NOT NULL, a BIGINT, b BIGINT, c BIGINT, d BIGINT, payload STRING, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_a ON ix4 (a) "+
			"CREATE INDEX idx_b ON ix4 (b) "+
			"CREATE INDEX idx_c ON ix4 (c) "+
			"CREATE INDEX idx_d ON ix4 (d)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ix4way/s WITH TEMPLATE ix4way")

	dsn := fmt.Sprintf("fdbsql:///testdb_ix4way?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, `INSERT INTO ix4 VALUES
		(1, 1, 2, 3, 4, 'all-four'),
		(2, 1, 2, 3, 9, 'miss-d'),
		(3, 1, 2, 9, 4, 'miss-c'),
		(4, 1, 9, 3, 4, 'miss-b'),
		(5, 9, 2, 3, 4, 'miss-a'),
		(6, 9, 9, 9, 9, 'miss-all')`)

	const query = "SELECT * FROM ix4 WHERE a = 1 AND b = 2 AND c = 3 AND d = 4"
	var plan string
	if err := db.QueryRowContext(ctx, "EXPLAIN "+query).Scan(&plan); err != nil {
		t.Fatalf("explain: %v", err)
	}
	if got := strings.Count(plan, "Intersection("); got != 1 {
		t.Fatalf("intersection node count = %d, want 1; plan: %s", got, plan)
	}
	for _, indexName := range []string{"IDX_A", "IDX_B", "IDX_C", "IDX_D"} {
		if !strings.Contains(plan, "IndexScan("+indexName) {
			t.Fatalf("four-way intersection is missing %s; plan: %s", indexName, plan)
		}
	}

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id, a, b, c, d int64
		var payload string
		if err := rows.Scan(&id, &a, &b, &c, &d, &payload); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("four-way rows = %v, want [1]; a missing leg leaks one of the three-of-four decoys (plan: %s)", ids, plan)
	}
}
