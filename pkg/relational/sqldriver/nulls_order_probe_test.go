package sqldriver_test

// RFC-164 §5 row-level proof: ORDER BY ... NULLS LAST must put NULLs last even
// when a forward index scan (which yields NULLS FIRST) covers the keys. Before
// the fix the sort was elided against the index ordering → NULLs came first.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_OrderByNullsLast(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nullsorder")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nullsorder")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE nullsorder "+
			"CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_ab ON t(a, b)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nullsorder/s WITH TEMPLATE nullsorder")
	dsn := fmt.Sprintf("fdbsql:///testdb_nullsorder?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// a=5 rows: b = NULL, 10, 20. (id=4 has a=9, excluded.)
	// a=5 group: b ∈ {NULL,10,20}. a=9 group: b ∈ {1,NULL} — a SECOND group with a
	// NULL so the multi-key case below genuinely varies the leading key AND tests
	// NULLS-last within each group (not collapsed to single-key by an a= filter).
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a,b) VALUES (1,5,NULL),(2,5,10),(3,5,20),(4,9,1),(5,9,NULL)")

	order := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			ids = append(ids, id)
		}
		return ids
	}

	// ASC NULLS LAST → 10, 20, NULL → ids 2, 3, 1.
	if got := order("SELECT id FROM t WHERE a = 5 ORDER BY b ASC NULLS LAST"); !eqIDs(got, []int64{2, 3, 1}) {
		t.Errorf("ORDER BY b ASC NULLS LAST: got %v, want [2 3 1] (NULL row id=1 must be last)", got)
	}
	// ASC default (NULLS FIRST) → NULL, 10, 20 → ids 1, 2, 3.
	if got := order("SELECT id FROM t WHERE a = 5 ORDER BY b"); !eqIDs(got, []int64{1, 2, 3}) {
		t.Errorf("ORDER BY b (NULLS FIRST): got %v, want [1 2 3]", got)
	}
	// DESC NULLS FIRST → NULL, 20, 10 → ids 1, 3, 2 (the other non-natural direction).
	if got := order("SELECT id FROM t WHERE a = 5 ORDER BY b DESC NULLS FIRST"); !eqIDs(got, []int64{1, 3, 2}) {
		t.Errorf("ORDER BY b DESC NULLS FIRST: got %v, want [1 3 2] (NULL row id=1 must be first)", got)
	}
	// DESC default (NULLS LAST) → 20, 10, NULL → ids 3, 2, 1.
	if got := order("SELECT id FROM t WHERE a = 5 ORDER BY b DESC"); !eqIDs(got, []int64{3, 2, 1}) {
		t.Errorf("ORDER BY b DESC (NULLS LAST): got %v, want [3 2 1]", got)
	}

	// Genuine multi-key (no a= filter, so the leading key a VARIES): ORDER BY a
	// ASC, then b ASC NULLS LAST within each a group. a=5 → 10,20,NULL = ids 2,3,1;
	// a=9 → 1,NULL = ids 4,5. So [2 3 1 4 5] — proving NULLS-last on the trailing
	// key within BOTH groups while the leading key orders the groups.
	multi := order("SELECT id FROM t ORDER BY a, b ASC NULLS LAST")
	if !eqIDs(multi, []int64{2, 3, 1, 4, 5}) {
		t.Errorf("ORDER BY a, b ASC NULLS LAST: got %v, want [2 3 1 4 5] (NULL b last per a-group)", multi)
	}
}

func eqIDs(a, b []int64) bool {
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
