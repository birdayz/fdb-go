package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFDB_UnnestMemberPredicateServesRows pins the ROWS of a WHERE predicate that
// descends into a lateral unnest's struct element.
//
// `FROM orders, orders.items AS i WHERE i.sku = 'x'` filters on a MEMBER of the
// bound element. The reference reaches the unnest rewrite as a two-accessor path
// rooted at the binding (`/I/SKU`) over the element's quantifier object, and the
// element-substitution arm correctly declines it — no member is the element. But
// declining is not the same as rewriting: the surviving reference still carries
// the binding segment, while the inner Explode flows the ELEMENT, whose fields are
// the members alone. The leading segment has to be dropped for the path to name
// anything in the row it is evaluated against.
//
// The projection form of this shape was already covered; every consumer of the
// rewrite is a PREDICATE, and a predicate that resolves to nothing filters every
// row out rather than failing. That is why this is a row assertion and not a plan
// assertion: the query PLANS either way, and a plan-shape check cannot tell right
// rows from no rows.
//
// This is the ROW half of TestStructDDL_UnnestStructArrayFieldAccess
// (pkg/relational/core/embedded/struct_ddl_test.go), which runs these same
// shapes but can only assert that they plan — its package is a metadata-only
// harness with no store. That test was the sole coverage of this shape and it
// was green throughout the defect. If one half moves, move the other.
func TestFDB_UnnestMemberPredicateServesRows(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_umpr")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_umpr")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE umpr "+
			"CREATE TYPE AS STRUCT item (sku STRING, qty BIGINT) "+
			"CREATE TABLE orders (order_id BIGINT, items item ARRAY, PRIMARY KEY (order_id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_umpr/s WITH TEMPLATE umpr")
	dsn := fmt.Sprintf("fdbsql:///testdb_umpr?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, e := db.ExecContext(ctx,
		"INSERT INTO orders (order_id, items) VALUES (1, [('x', 10), ('y', 20)])"); e != nil {
		t.Fatalf("insert order 1: %v", e)
	}
	if _, e := db.ExecContext(ctx,
		"INSERT INTO orders (order_id, items) VALUES (2, [('y', 30)])"); e != nil {
		t.Fatalf("insert order 2: %v", e)
	}

	// queryInts runs a single-BIGINT-column SELECT and returns its rows.
	queryInts := func(t *testing.T, q string) []int64 {
		t.Helper()
		r, e := db.QueryContext(ctx, q)
		if e != nil {
			t.Fatalf("%s: %v", q, e)
		}
		defer r.Close()
		var out []int64
		for r.Next() {
			var v int64
			if se := r.Scan(&v); se != nil {
				t.Fatalf("%s: scan: %v", q, se)
			}
			out = append(out, v)
		}
		if re := r.Err(); re != nil {
			t.Fatalf("%s: rows: %v", q, re)
		}
		return out
	}

	// The SUBJECT: a STRING member predicate. Only order 1 has an item with
	// sku='x', so exactly one row must come back. Zero rows is the failure this
	// pins — the predicate reading a path that names nothing filters everything.
	t.Run("string member predicate selects the matching order", func(t *testing.T) {
		t.Parallel()
		got := queryInts(t, "SELECT order_id FROM orders, orders.items AS i WHERE i.sku = 'x'")
		if len(got) != 1 || got[0] != 1 {
			t.Errorf("SELECT order_id ... WHERE i.sku = 'x' = %v, want [1]. Zero rows means the "+
				"member reference resolved to nothing in the element row rather than to SKU.", got)
		}
	})

	// A NON-LEADING member: `qty` is the element's second field, so a fix that
	// merely drops the binding segment and reads ordinal 0 unconditionally would
	// serve SKU here and match nothing. This arm and the one above are separate
	// sites: one proves the segment is dropped, the other proves the surviving
	// path still points at the right member.
	t.Run("non-leading member predicate selects by the right field", func(t *testing.T) {
		t.Parallel()
		got := queryInts(t, "SELECT order_id FROM orders, orders.items AS i WHERE i.qty = 30")
		if len(got) != 1 || got[0] != 2 {
			t.Errorf("SELECT order_id ... WHERE i.qty = 30 = %v, want [2]. A wrong surviving "+
				"ordinal reads SKU here and matches nothing.", got)
		}
	})

	// The member predicate must also COMBINE with a projection of a sibling
	// member, so the rewrite is exercised where the row carries more than the
	// filtered column.
	t.Run("member predicate alongside a sibling member projection", func(t *testing.T) {
		t.Parallel()
		r, e := db.QueryContext(ctx,
			"SELECT i.qty FROM orders, orders.items AS i WHERE i.sku = 'y'")
		if e != nil {
			t.Fatalf("query: %v", e)
		}
		defer r.Close()
		var got []int64
		for r.Next() {
			var v int64
			if se := r.Scan(&v); se != nil {
				t.Fatalf("scan i.qty: %v", se)
			}
			got = append(got, v)
		}
		if len(got) != 2 || got[0] != 20 || got[1] != 30 {
			t.Errorf("SELECT i.qty ... WHERE i.sku = 'y' = %v, want [20 30]", got)
		}
	})
}
