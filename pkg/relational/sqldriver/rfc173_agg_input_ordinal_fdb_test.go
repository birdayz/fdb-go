package sqldriver_test

// RFC-173 regression pin for the STREAMING-AGGREGATE INPUT edge (group-by key /
// aggregate operand read). Before this slice, a plain single-table aggregate read
// its group keys and operands by NAME off the inner row's Datum map; now the
// translator bakes each key/operand column to the inner frontier's output ORDINAL
// (NewFieldValueOfOrdinal), so the aggregate cursor reads the inner PositionalRow by
// Get(ordinal).
//
// The proof is the values.NameReadForbidden measurement gate: with it ARMED, EVERY
// name-keyed Datum read (present or absent) is a hard error. A query that still read
// its aggregate input by name would fail; a fully ordinalized one succeeds. This test
// arms the gate around the representative single-table aggregate shapes and asserts
// they still return the correct rows — the dimension the dual-window differential
// cannot see (both models agree when BOTH silently read by name).
//
// The gate is a process-global runtime (not plan-time) flag; this test does NOT call
// t.Parallel(), so Go runs it in the serial phase (before any parallel test resumes),
// and it resets the flag in defer — no other test ever observes it armed. The schema
// is unique to this test, so no cached plan is shared.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestFDB_RFC173_AggregateInputOrdinal(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_aggin_ord")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_aggin_ord")
	// idx_g is a covering index whose row layout ([g, id]) differs from the table's
	// declaration order ([id, g, v, price, active]); a grouped scan over it exercises
	// the covering-index dimension — the group key must resolve by NAME against the
	// actual runtime row, NOT a plan-time table ordinal (which would read the wrong
	// slot).
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE aggin_ord "+
			"CREATE TABLE t (id BIGINT NOT NULL, g BIGINT, v BIGINT, price BIGINT, active BOOLEAN, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_g ON t (g)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_aggin_ord/s WITH TEMPLATE aggin_ord")
	dsn := fmt.Sprintf("fdbsql:///testdb_aggin_ord?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// g: 1,1,2 ; v: 10,20,30 ; price: 2,3,4 ; active: T,F,T
	mwjoMustExec(t, db, ctx,
		"INSERT INTO t (id, g, v, price, active) VALUES "+
			"(1, 1, 10, 2, true), (2, 1, 20, 3, false), (3, 2, 30, 4, true)")

	// Arm the measurement gate ONLY for the duration of this serial test: any
	// name-keyed read on the aggregate input path now hard-errors.
	values.NameReadForbidden = true
	defer func() { values.NameReadForbidden = false }()

	pairs := func(t *testing.T, q string) string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q under NameReadForbidden: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var a, b int64
			if err := rows.Scan(&a, &b); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, fmt.Sprintf("%d=%d", a, b))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}
	scalar := func(t *testing.T, q string) int64 {
		var v sql.NullInt64
		if err := db.QueryRowContext(ctx, q).Scan(&v); err != nil {
			t.Fatalf("query %q under NameReadForbidden: %v", q, err)
		}
		return v.Int64
	}

	// Scalar aggregate: operand `v` read by ordinal (the minimal repro).
	t.Run("scalar_count_operand", func(t *testing.T) {
		if got := scalar(t, "SELECT COUNT(v) FROM t"); got != 3 {
			t.Errorf("COUNT(v) = %d, want 3", got)
		}
	})
	t.Run("scalar_sum_operand", func(t *testing.T) {
		if got := scalar(t, "SELECT SUM(v) FROM t"); got != 60 {
			t.Errorf("SUM(v) = %d, want 60", got)
		}
	})
	// Grouped: key `g` read by name-in-row (COUNT(*) reads nothing). This planning
	// prefers the covering idx_g (ordered by g, no sort) — so the group key resolves
	// against the covering row [g, id] whose layout differs from the table's, pinning
	// the layout-robustness of the name-in-positional-row resolution.
	t.Run("group_key_count_star_covering_index", func(t *testing.T) {
		if got := pairs(t, "SELECT g, COUNT(*) FROM t GROUP BY g"); got != "1=2 2=1" {
			t.Errorf("g,COUNT(*) = %q, want %q", got, "1=2 2=1")
		}
	})
	// Grouped: BOTH key `g` and operand `v` read by ordinal, plus a WHERE filter
	// frontier (filter-over-scan is layout-preserving).
	t.Run("group_key_and_operand_filtered", func(t *testing.T) {
		if got := pairs(t, "SELECT g, SUM(v) FROM t WHERE price >= 2 GROUP BY g"); got != "1=30 2=30" {
			t.Errorf("g,SUM(v) WHERE = %q, want %q", got, "1=30 2=30")
		}
	})
	// Compound operand: SUM(v * price) bakes each leaf column to its ordinal.
	t.Run("compound_operand", func(t *testing.T) {
		if got := scalar(t, "SELECT SUM(v * price) FROM t"); got != 10*2+20*3+30*4 {
			t.Errorf("SUM(v*price) = %d, want %d", got, 10*2+20*3+30*4)
		}
	})
	// BOOLEAN group key by ordinal.
	t.Run("group_key_boolean", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT active, COUNT(*) FROM t GROUP BY active")
		if err != nil {
			t.Fatalf("bool group under NameReadForbidden: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var a bool
			var b int64
			if err := rows.Scan(&a, &b); err != nil {
				t.Fatalf("scan bool group: %v", err)
			}
			out = append(out, fmt.Sprintf("%t=%d", a, b))
		}
		sort.Strings(out)
		if got := strings.Join(out, " "); got != "false=1 true=2" {
			t.Errorf("active,COUNT(*) = %q, want %q", got, "false=1 true=2")
		}
	})
}
