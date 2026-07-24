package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// TestFDB_IntersectionOrderingGate pins RFC-181 P0.1: the PK-intersection
// executor is a strict PK-sorted merge, but nothing gated leg ORDER — an
// INEQUALITY-bound index leg emits (a, pk) order, whose pk sequence
// interleaves, and the merge silently DROPPED intersection rows. Java gates
// every intersection on comparison-key compatibility per leg
// (WithPrimaryKeyDataAccessRule intersectOrderings +
// isCompatibleComparisonKey); Go must decline the non-PK-monotone
// composition (falling back to a filtered single-index plan — correct rows)
// and still allow the all-equality-bound composition.
func TestFDB_IntersectionOrderingGate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ixgate")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ixgate")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE ixgate "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_a ON t (a) "+
			"CREATE INDEX idx_b ON t (b) "+
			"CREATE TABLE t_desc (id BIGINT NOT NULL, a BIGINT, b BIGINT, sort_key BIGINT NOT NULL, payload STRING, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_desc_a_sort ON t_desc (a, sort_key) "+
			"CREATE INDEX idx_desc_b_sort ON t_desc (b, sort_key)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ixgate/s WITH TEMPLATE ixgate")
	dsn := fmt.Sprintf("fdbsql:///testdb_ixgate?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// PK order and a-order INTERLEAVE (a = 1000 - id*7 % 997): the a-index
	// leg's pk sequence is non-monotone under the pk-keyed merge. Each
	// predicate alone matches many rows; the conjunction matches few — the
	// shape where an index intersection should out-cost a single leg.
	var sb strings.Builder
	sb.WriteString("INSERT INTO t (id, a, b) VALUES ")
	for i := 1; i <= 200; i++ {
		if i > 1 {
			sb.WriteString(", ")
		}
		b := 7
		if i%13 == 0 {
			b = 3 // 15 rows with b=3, scattered
		}
		fmt.Fprintf(&sb, "(%d, %d, %d)", i, 1000-(i*7)%997, b)
	}
	mwjoMustExec(t, db, ctx, sb.String())

	collect := func(q string) map[int64]bool {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		got := map[int64]bool{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[id] = true
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return got
	}

	t.Run("range_plus_equality_returns_all_rows", func(t *testing.T) {
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN SELECT id FROM t WHERE a > 5 AND b = 3").Scan(&plan); err != nil {
			t.Fatalf("explain: %v", err)
		}
		t.Logf("plan: %s", plan)
		got := collect("SELECT id FROM t WHERE a > 5 AND b = 3")
		want := 0
		for i := 1; i <= 200; i++ {
			if i%13 == 0 && 1000-(i*7)%997 > 5 {
				want++
				if !got[int64(i)] {
					t.Fatalf("row id=%d MISSING (%d/%d found) — a non-PK-monotone leg reached the pk-sorted intersection merge and dropped rows (plan: %s)", i, len(got), want, plan)
				}
			}
		}
		if len(got) != want {
			t.Fatalf("got %d rows, want %d (plan: %s)", len(got), want, plan)
		}
	})

	t.Run("descending_common_secondary_order_pages_exactly", func(t *testing.T) {
		// Both equality-bound legs expose the same free physical suffix
		// (sort_key, id). Duplicate sort_key values make ID DESC load-bearing:
		// comparing on sort_key alone can admit/drop the wrong record, while a
		// forward merge or forward leg produces the wrong row order.
		mwjoMustExec(t, db, ctx, "INSERT INTO t_desc (id, a, b, sort_key) VALUES "+
			"(1, 7, 9, 10), "+
			"(3, 7, 9, 90), "+
			"(7, 7, 9, 100), "+
			"(11, 7, 9, 100), "+
			"(15, 7, 9, 90), "+
			"(20, 7, 9, 50), "+
			"(25, 7, 9, 50), "+
			"(2, 7, 8, 120), "+
			"(4, 7, 8, 90), "+
			"(5, 7, 6, 70), "+
			"(6, 5, 9, 130), "+
			"(8, 4, 9, 100), "+
			"(9, 3, 9, 90), "+
			"(10, 7, 2, 60), "+
			"(12, 1, 9, 80)")

		const query = "SELECT * FROM t_desc " +
			"WHERE a = 7 AND b = 9 ORDER BY sort_key DESC, id DESC"
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+query).Scan(&plan); err != nil {
			t.Fatalf("explain: %v", err)
		}
		t.Logf("plan: %s", plan)
		if !strings.Contains(plan, "Intersection(") {
			t.Fatalf("common secondary ordering must reach an intersection: %s", plan)
		}
		for _, indexName := range []string{"IDX_DESC_A_SORT", "IDX_DESC_B_SORT"} {
			if !strings.Contains(plan, "IndexScan("+indexName) {
				t.Fatalf("descending intersection is missing %s: %s", indexName, plan)
			}
		}
		if strings.Contains(plan, "InMemorySort") {
			t.Fatalf("reverse intersection should satisfy ORDER BY without InMemorySort: %s", plan)
		}
		if got := strings.Count(plan, "REVERSE"); got < 3 {
			t.Fatalf("want reverse merge plus two reverse legs (at least 3 REVERSE markers), got %d: %s", got, plan)
		}

		// A two-scanned-row budget forces the SQL driver to resume the
		// intersection repeatedly. This exercises the reverse flag through
		// executor construction and continuation restore, not merely the first
		// in-memory page.
		conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
			ec.SetOptions(api.NewOptionsBuilder().
				Set(api.OptExecutionScannedRowsLimit, 2).
				Build())
		})
		rows, err := conn.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()

		type result struct {
			id      int64
			sortKey int64
		}
		got := make([]result, 0, 7)
		for rows.Next() {
			var row result
			var a, b sql.NullInt64
			var payload sql.NullString
			if err := rows.Scan(&row.id, &a, &b, &row.sortKey, &payload); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		want := []result{
			{id: 11, sortKey: 100},
			{id: 7, sortKey: 100},
			{id: 15, sortKey: 90},
			{id: 3, sortKey: 90},
			{id: 25, sortKey: 50},
			{id: 20, sortKey: 50},
			{id: 1, sortKey: 10},
		}
		if len(got) != len(want) {
			t.Fatalf("paged descending intersection returned %d rows, want %d: got=%v plan=%s",
				len(got), len(want), got, plan)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("paged descending intersection row %d = %+v, want %+v: got=%v plan=%s",
					i, got[i], want[i], got, plan)
			}
		}
	})
}
