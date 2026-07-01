package sqldriver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_RFC173Slice1_NoSpuriousSort is the Graefe §5 ordering-propagation pin
// for RFC-173 Slice 1: making ordinal column resolution authoritative on the
// non-join frontier must NOT break the provided-ordering rebase. A column's
// identity flips name→ordinal, so if the ordering pull-up
// (pullUpOrderingFromSelectChild) stopped agreeing, index-ordering match would
// fail, RemoveSortRule would stop firing, and a redundant InMemorySort would
// reappear over an already index-ordered single-table scan — a plan-property
// regression the row-content shadow is blind to. This pin asserts the sort stays
// gone (and the rows stay correctly ordered) through a projection over the
// index-ordered scan.
func TestFDB_RFC173Slice1_NoSpuriousSort(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := setupPlanShapeDB(t, "r173sort",
		"CREATE TABLE items (id BIGINT NOT NULL, price BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_price ON items (price)")
	for _, it := range []struct{ id, price int }{
		{1, 500}, {2, 50}, {3, 150}, {4, 200}, {5, 25},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO items VALUES (%d, %d)", it.id, it.price)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	// A projection over an index-ordered scan: the ordering is provided by
	// idx_price and must be pulled up THROUGH the projection (whose output column
	// identities are now ordinal). No InMemorySort may appear.
	q := "SELECT id, price FROM items ORDER BY price"
	plan := planExplainVia(t, ctx, db, q)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "IndexScan") {
		t.Fatalf("expected IndexScan (index provides ORDER BY ordering), got: %s", plan)
	}
	if strings.Contains(plan, "InMemorySort") {
		t.Fatalf("RFC-173 Slice 1 spurious-sort regression: index-ordered single-table ORDER BY gained an InMemorySort after the name→ordinal flip: %s", plan)
	}

	got := collectRows(t, db, q)
	wantPrice := []int64{25, 50, 150, 200, 500}
	if len(got) != len(wantPrice) {
		t.Fatalf("want %d rows, got %d: %v", len(wantPrice), len(got), got)
	}
	for i, wp := range wantPrice {
		if got[i][1].(int64) != wp {
			t.Fatalf("row %d price = %v, want %d (ordering under ordinal resolution): %v", i, got[i][1], wp, got)
		}
	}
}

// TestFDB_RFC173Slice1_GroupByHavingOrderBy pins GROUP BY / HAVING / ORDER BY
// over a single-table frontier under authoritative ordinal resolution (RFC-173
// Slice 1, §5). Grouping keys, the HAVING predicate, and the ORDER BY key all
// ride the generic FieldValue path that now resolves by ordinal against the
// positional row on the non-join frontier — they must return the same correct
// grouped rows.
func TestFDB_RFC173Slice1_GroupByHavingOrderBy(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := setupPlanShapeDB(t, "r173grp",
		"CREATE TABLE items (id BIGINT NOT NULL, category STRING, price BIGINT, PRIMARY KEY (id))")
	for _, it := range []struct {
		id       int
		category string
		price    int
	}{
		{1, "electronics", 500},
		{2, "books", 50},
		{3, "clothing", 150},
		{4, "electronics", 200},
		{5, "books", 25},
		{6, "electronics", 75},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO items VALUES (%d, '%s', %d)", it.id, it.category, it.price)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	// GROUP BY category, HAVING COUNT(*) >= 2, ORDER BY category — all keys on the
	// single-table frontier, resolved by ordinal. books(2), electronics(3) pass
	// HAVING; clothing(1) is filtered out.
	q := "SELECT category, COUNT(*) FROM items GROUP BY category HAVING COUNT(*) >= 2 ORDER BY category"
	got := collectRows(t, db, q)
	want := []struct {
		cat string
		cnt int64
	}{
		{"books", 2},
		{"electronics", 3},
	}
	if len(got) != len(want) {
		t.Fatalf("want %d grouped rows, got %d: %v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i][0].(string) != w.cat || got[i][1].(int64) != w.cnt {
			t.Fatalf("row %d = (%v, %v), want (%s, %d): %v", i, got[i][0], got[i][1], w.cat, w.cnt, got)
		}
	}

	// SUM aggregate with GROUP BY + ORDER BY on the grouped key over the frontier.
	q2 := "SELECT category, SUM(price) FROM items GROUP BY category ORDER BY category"
	got2 := collectRows(t, db, q2)
	want2 := []struct {
		cat string
		sum int64
	}{
		{"books", 75},
		{"clothing", 150},
		{"electronics", 775},
	}
	if len(got2) != len(want2) {
		t.Fatalf("want %d grouped rows, got %d: %v", len(want2), len(got2), got2)
	}
	for i, w := range want2 {
		if got2[i][0].(string) != w.cat || got2[i][1].(int64) != w.sum {
			t.Fatalf("row %d = (%v, %v), want (%s, %d): %v", i, got2[i][0], got2[i][1], w.cat, w.sum, got2)
		}
	}
}
