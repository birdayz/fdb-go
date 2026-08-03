package sqldriver_test

// A stored index predicate that is a PROVED tautology describes a COMPLETE
// index: `WHERE TRUE` rejects no record, so the index holds an entry for every
// row and a scan over it is not lossy. The planner already reasons this way —
// index_expansion.go declines to attach a tautological candidate predicate,
// mirroring ValueIndexExpansionVisitor.java:141, so such an index stays as
// matchable as any full value index — and the executor's filtered-index
// backstop must agree, or a plan the planner is entitled to build dies at
// execution with a 0AF00 no plan can avoid.
//
// The completeness proof is what admits the index, not the mere presence of a
// predicate field: an unprovable predicate (an opaque programmatic Go
// predicate, any non-constant comparison) still fails closed at the backstop.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_TautologyIndexPredicateExecutes(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_taut_idx")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_taut_idx")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE taut_idx "+
			"CREATE TABLE t1 (id BIGINT, col1 BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX i_true AS SELECT col1 FROM t1 WHERE TRUE")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_taut_idx/s WITH TEMPLATE taut_idx")
	dsn := fmt.Sprintf("fdbsql:///testdb_taut_idx?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx,
		"INSERT INTO t1 (id, col1) VALUES (1, 10), (2, 20), (3, 30), (4, 200)")

	const q = "SELECT col1 FROM t1 WHERE col1 < 100"

	// The plan must actually reach the tautological index — otherwise a base
	// scan would satisfy the row assertion while the backstop stayed broken.
	var plan string
	if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN %q: %v", q, err)
	}
	if !strings.Contains(strings.ToUpper(plan), "I_TRUE") {
		t.Fatalf("plan does not use the tautological index I_TRUE:\n%s", plan)
	}

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q over a WHERE TRUE index: %v\n"+
			"— a proved-tautology index predicate rejects no record, so the index is "+
			"complete and the executor's filtered-index backstop must admit it", q, err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, fmt.Sprintf("%d", v))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	sort.Strings(got)
	if want := "10 20 30"; strings.Join(got, " ") != want {
		t.Fatalf("rows = %q, want %q", strings.Join(got, " "), want)
	}
}
