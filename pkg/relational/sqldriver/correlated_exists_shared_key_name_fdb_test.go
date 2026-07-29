package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// TestFDB_CorrelatedExistsSharedKeyName exercises the correlated-EXISTS fast
// path end to end on the shape RFC-197 item 2 converted: two tables whose
// primary keys are BOTH spelled `ID` and both sit at ordinal 0 of their own
// row, joined by a correlated EXISTS.
//
// That is the shape where the leaf name and the bare ordinal are each useless
// as identity, and where the engine used to rely on the name twice:
//
//   - ImplementNestedLoopJoinRule matched the inner PK column by comparing the
//     metadata name `ID` against the comparand's leaf name, so a comparand that
//     spelled `ID` but read the OUTER row would have been accepted as the inner
//     key. It now resolves the metadata name once against the inner leg's own
//     row layout and compares the resolved ordinal in that layout.
//   - the cost model's point-probe bound found the OUTER reference's `ID` in
//     its equality set and declared a full inner scan a one-record probe.
//
// Neither defect changes the ROWS this query returns on its own — the first is
// guarded upstream, the second only mis-ranks plans — which is precisely why
// they survived: a unit test of either helper is easy to satisfy and a
// row-level test of the query passes with both bugs present. So this test pins
// the two things that ARE observable and that the conversion must not break:
// the answers stay correct, and the fast path still fires (the inner access is
// a correlated probe, not a full scan per outer row).
//
// The unit-level dimension probes live beside the converted helpers
// (rule_implement_nested_loop_join_test.go, point_probe_identity_test.go);
// this is the end-to-end half, over real FoundationDB.
func TestFDB_CorrelatedExistsSharedKeyName(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_cexsharedkey")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_cexsharedkey")
	// products.ID and orders.ID are both the primary key, both named ID, both
	// the first declared column. orders.product_id is the foreign key.
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE cex_tmpl "+
		"CREATE TABLE products (id BIGINT NOT NULL, category STRING, PRIMARY KEY (id)) "+
		"CREATE TABLE orders (id BIGINT NOT NULL, product_id BIGINT, qty BIGINT, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_cexsharedkey/s WITH TEMPLATE cex_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_cexsharedkey?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO products VALUES (1, 'a'), (2, 'b'), (3, 'c'), (4, 'd')")
	// Orders reference products 1 and 3 only. Order IDs deliberately OVERLAP
	// the product IDs (10,11,12 would not exercise the confusion; 1,2,3 do):
	// order id 2 exists while product 2 has NO order, so a proof that confuses
	// orders.ID with products.ID answers the EXISTS for product 2 by finding
	// ORDER 2 and reports it as ordered.
	mustExec(t, db, ctx, "INSERT INTO orders VALUES (1, 1, 5), (2, 3, 7), (3, 1, 9)")

	queryIDs := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows %q: %v", q, err)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}

	eq := func(t *testing.T, got, want []int64, what string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s = %v, want %v", what, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s = %v, want %v", what, got, want)
			}
		}
	}

	t.Run("EXISTS correlated on the FK", func(t *testing.T) {
		// Products 1 and 3 have orders. If the inner key match confused
		// orders.ID with products.ID, product 2 would come back too (order 2
		// exists) and product 4 would still be absent — a wrong-rows answer
		// that a same-IDs corpus would hide.
		got := queryIDs(t, "SELECT id FROM products p WHERE EXISTS "+
			"(SELECT 1 FROM orders o WHERE o.product_id = p.id)")
		eq(t, got, []int64{1, 3}, "EXISTS on FK")
	})

	t.Run("EXISTS correlated on the inner PRIMARY KEY", func(t *testing.T) {
		// This is the arm that builds a correlated PK probe: the inner
		// comparand IS orders.ID, the inner table's primary key. Orders 1,2,3
		// exist, so products 1,2,3 qualify and product 4 does not.
		got := queryIDs(t, "SELECT id FROM products p WHERE EXISTS "+
			"(SELECT 1 FROM orders o WHERE o.id = p.id)")
		eq(t, got, []int64{1, 2, 3}, "EXISTS on inner PK")
	})

	t.Run("NOT EXISTS is the complement", func(t *testing.T) {
		got := queryIDs(t, "SELECT id FROM products p WHERE NOT EXISTS "+
			"(SELECT 1 FROM orders o WHERE o.product_id = p.id)")
		eq(t, got, []int64{2, 4}, "NOT EXISTS on FK")
	})

	t.Run("the inner access is a correlated probe, not a per-row full scan", func(t *testing.T) {
		// The fast path's whole purpose. Without it the EXISTS still answers
		// correctly through the general correlated path, so only the PLAN
		// distinguishes them — which is why the conversion's corpus check
		// counted fast-path firings (30 before, 30 after) rather than rows.
		rows, err := db.QueryContext(ctx, "EXPLAIN SELECT id FROM products p WHERE EXISTS "+
			"(SELECT 1 FROM orders o WHERE o.id = p.id)")
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		defer rows.Close()
		var plan string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan explain: %v", err)
			}
			plan += line + "\n"
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("explain rows: %v", err)
		}
		if plan == "" {
			t.Fatal("EXPLAIN produced no plan")
		}
		// `Scan(ORDERS, [=])` — the equality was pushed INTO the inner scan as
		// a correlated comparison. A bare `Scan(ORDERS)` with the equality left
		// in a filter above it would be the same rows through a full scan per
		// outer row, which is what declining the fast path costs.
		if !strings.Contains(plan, "Scan(ORDERS, [=])") {
			t.Fatalf("inner ORDERS access is not a correlated probe; plan was:\n%s\n"+
				"the identity conversion must not cost the fast path its index probe — "+
				"a declined rewrite here is a full inner scan per outer row", plan)
		}
	})
}
