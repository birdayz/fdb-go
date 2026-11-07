package embedded

// Regression tests for the connection's parse cache (parse_cache.go): a
// repeated statement reuses its parse tree, and parameters and literals bound
// to that tree for one statement never leak into the next.

import (
	"context"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"testing"
)

// parseCacheQuery runs sql with args on c and returns its single BIGINT column.
func parseCacheQuery(t *testing.T, c *EmbeddedConnection, sql string, args ...any) []int64 {
	t.Helper()
	named := make([]driver.NamedValue, len(args))
	for i, a := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	rows, err := c.QueryContext(context.Background(), sql, named)
	if err != nil {
		t.Fatalf("%s %v: %v", sql, args, err)
	}
	defer rows.Close() //nolint:errcheck
	var got []int64
	dest := make([]driver.Value, 1)
	for {
		if err := rows.Next(dest); err == io.EOF {
			return got
		} else if err != nil {
			t.Fatalf("%s %v: next: %v", sql, args, err)
		}
		got = append(got, dest[0].(int64))
	}
}

func TestParseCacheRepeatedStatementBindsEachCallsValues(t *testing.T) {
	t.Parallel()
	c := newSimConnection(t, 71)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if _, err := c.ExecContext(ctx, fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", i, 10*i), nil); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	const byID = "SELECT v FROM t WHERE id = ?"
	for _, id := range []int64{1, 2, 3, 2, 1, 4} {
		got := parseCacheQuery(t, c, byID, id)
		want := []int64{10 * id}
		if id == 4 {
			want = nil
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("id %d: got %v, want %v", id, got, want)
		}
	}
	first, ok := c.parses.trees[byID]
	if !ok {
		t.Fatalf("a repeated statement was not cached")
	}
	again, err := c.parse(byID)
	if err != nil || again != first {
		t.Fatalf("a cached statement was parsed again (%v)", err)
	}

	// Literals bind to the tree's constant tokens for one statement; the
	// release must restore them so the next run of the same text, and a
	// run of a text with another literal, read their own values.
	for _, id := range []int64{2, 3, 2} {
		sql := fmt.Sprintf("SELECT v FROM t WHERE id = %d", id)
		for range 2 {
			if got := parseCacheQuery(t, c, sql); fmt.Sprint(got) != fmt.Sprint([]int64{10 * id}) {
				t.Fatalf("%s: got %v", sql, got)
			}
		}
	}
	// A statement mixing a parameter and a literal.
	const mixed = "SELECT v FROM t WHERE id = ? OR id = 3 ORDER BY id"
	for _, id := range []int64{1, 2} {
		if got := parseCacheQuery(t, c, mixed, id); fmt.Sprint(got) != fmt.Sprint([]int64{10 * id, 30}) {
			t.Fatalf("mixed id %d: got %v", id, got)
		}
	}
}

func TestParseCacheKeepsNoFailedParse(t *testing.T) {
	t.Parallel()
	c := newSimConnection(t, 72)
	const bad = "SELEC v FROM t"
	for range 2 {
		if _, err := c.QueryContext(context.Background(), bad, nil); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
	if _, ok := c.parses.trees[bad]; ok {
		t.Fatalf("a failed parse was cached")
	}
}

func TestParseCacheIsBounded(t *testing.T) {
	t.Parallel()
	var c EmbeddedConnection
	text := func(i int) string { return fmt.Sprintf("SELECT %d FROM t", i) }
	for i := range parseCacheEntries + 1 {
		if _, err := c.parse(text(i)); err != nil {
			t.Fatalf("parse %d: %v", i, err)
		}
	}
	if len(c.parses.trees) != parseCacheEntries || len(c.parses.order) != parseCacheEntries {
		t.Fatalf("cache holds %d trees, %d in order, want %d", len(c.parses.trees), len(c.parses.order), parseCacheEntries)
	}
	if _, ok := c.parses.trees[text(0)]; ok {
		t.Fatalf("the oldest statement was not evicted")
	}
	if _, ok := c.parses.trees[text(parseCacheEntries)]; !ok {
		t.Fatalf("the newest statement was not cached")
	}

	long := "SELECT 1 FROM t WHERE " + strings.Repeat("1 = 1 AND ", parseCacheMaxText/10) + "1 = 1"
	if _, err := c.parse(long); err != nil {
		t.Fatalf("parse long: %v", err)
	}
	if _, ok := c.parses.trees[long]; ok {
		t.Fatalf("a statement over %d bytes was cached", parseCacheMaxText)
	}
}
