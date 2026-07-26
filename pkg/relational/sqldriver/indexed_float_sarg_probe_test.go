package sqldriver_test

// Regression for the indexed-FLOAT(32-bit) cross-type SARG bug: an indexed
// FLOAT column returned ZERO rows for EVERY equality/range comparison,
// because the constant comparand packed into the index range as a tuple
// DOUBLE (0x21) against the column's tuple SINGLE (0x20) index entries —
// a mismatched FDB tuple type code that never matches, even when the
// literal (1.5, 1.0) is exactly representable in float32 so precision
// wasn't the issue. Non-indexed FLOAT and indexed DOUBLE were both fine;
// only the indexed FLOAT(32) SARG path was cross-type-broken. Fixed by
// expr.narrowConstAgainstFloatColumn (the FLOAT-column analogue of
// widenIntConstAgainstDouble/narrowFloatConstAgainstInt) plus the
// executor's coerceTupleElement, which downcasts the wire-packed value to
// a genuine Go float32 at the tuple-packing boundary.
//
// This file also pins the width-narrowing BOUNDARY case that the naive
// "just cast to float32 and keep the operator" approach gets wrong: 0.1
// is not exactly representable in float32 (float32(0.1) rounds to
// 0.10000000149011612 as a real number, which is > the double literal
// 0.1). A row that stores 0.1 therefore has a REAL stored value strictly
// greater than the literal 0.1 — `f > 0.1` must include it, `f < 0.1` /
// `f <= 0.1` must exclude it. A naive unadjusted-operator rewrite
// (`f > float32(0.1)` with the SAME `>`) would wrongly EXCLUDE that exact
// row, since colVal == float32(0.1) is not strictly greater than itself —
// this is the mutation-sensitive case: reverting the ceil/floor op
// rewrite to a bare round-and-keep-op turns this test red.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_IndexedFloatSargProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ifs")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ifs")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE ifs "+
		"CREATE TABLE noidx (id BIGINT NOT NULL, f FLOAT, PRIMARY KEY (id)) "+
		"CREATE TABLE withidx (id BIGINT NOT NULL, f FLOAT, PRIMARY KEY (id)) "+
		"CREATE TABLE dblidx (id BIGINT NOT NULL, f DOUBLE, PRIMARY KEY (id)) "+
		"CREATE TABLE bnd (id BIGINT NOT NULL, f FLOAT, PRIMARY KEY (id)) "+
		"CREATE INDEX wi_f ON withidx (f) "+
		"CREATE INDEX di_f ON dblidx (f) "+
		"CREATE INDEX bnd_f ON bnd (f)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ifs/s WITH TEMPLATE ifs")
	dsn := fmt.Sprintf("fdbsql:///testdb_ifs?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO noidx (id, f) VALUES (1,1.5),(2,2.5),(3,0.5)")
	mwjoMustExec(t, db, ctx, "INSERT INTO withidx (id, f) VALUES (1,1.5),(2,2.5),(3,0.5),(4,1.0)")
	mwjoMustExec(t, db, ctx, "INSERT INTO dblidx (id, f) VALUES (1,1.5),(2,2.5),(3,0.5)")
	// id 10: stores 0.1 (rounds to float32(0.1) at insert, a REAL value
	// strictly > the double literal 0.1). id 11: clearly below. id 12:
	// clearly above.
	mwjoMustExec(t, db, ctx, "INSERT INTO bnd (id, f) VALUES (10, 0.1), (11, 0.05), (12, 0.2)")

	ids := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			o = append(o, v)
		}
		sort.Slice(o, func(i, j int) bool { return o[i] < o[j] })
		return o
	}
	eq := func(g, w []int64) bool {
		if len(g) != len(w) {
			return false
		}
		for i := range g {
			if g[i] != w[i] {
				return false
			}
		}
		return true
	}
	ck := func(name, q string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := ids(q); !eq(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	// Baseline: non-indexed FLOAT (residual path, double-space comparison) —
	// unaffected by this fix, kept as a control.
	ck("noindex_float_eq_correct", "SELECT id FROM noidx WHERE f = 1.5", []int64{1})
	ck("noindex_float_range_correct", "SELECT id FROM noidx WHERE f > 1.0", []int64{1, 2})
	// Baseline: DOUBLE-indexed (the common type) — unaffected, kept as a control.
	ck("double_indexed_eq_correct", "SELECT id FROM dblidx WHERE f = 1.5", []int64{1})

	// FIXED: indexed FLOAT now returns the correct rows via the index SARG,
	// not merely via a residual-filter fallback — assert the plan actually
	// uses the index range scan (not a full scan + filter), the same
	// EXPLAIN discipline crosstype_const_sarg_fdb_test.go uses for the
	// DOUBLE-column sibling fix.
	t.Run("indexed_float_uses_index_range_scan", func(t *testing.T) {
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN SELECT id FROM withidx WHERE f = 1.5").Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		if !strings.Contains(plan, "IndexScan(WI_F") {
			t.Fatalf("expected an IndexScan(WI_F ...) range scan for `f = 1.5`, got: %s", plan)
		}
	})
	ck("indexed_float_eq_double_lit", "SELECT id FROM withidx WHERE f = 1.5", []int64{1})
	ck("indexed_float_range_double_lit", "SELECT id FROM withidx WHERE f > 1.0", []int64{1, 2})
	ck("indexed_float_reversed_eq", "SELECT id FROM withidx WHERE 1.5 = f", []int64{1})
	// int-literal widen (exact — 1 fits float32 trivially): mirrors
	// widenIntConstAgainstDouble's int-vs-DOUBLE fix, one width narrower.
	ck("indexed_float_eq_int_lit", "SELECT id FROM withidx WHERE f = 1", []int64{4})
	ck("indexed_float_in_list", "SELECT id FROM withidx WHERE f IN (1.5, 2.5)", []int64{1, 2})
	ck("indexed_float_le", "SELECT id FROM withidx WHERE f <= 1.0", []int64{3, 4})
	ck("indexed_float_ne", "SELECT id FROM withidx WHERE f <> 1.5", []int64{2, 3, 4})

	// BOUNDARY (mutation-sensitive): 0.1 is not exactly representable in
	// float32; the stored value for id 10 is the REAL number
	// float64(float32(0.1)) ≈ 0.10000000149011612, strictly greater than
	// the double literal 0.1. A correct SARG must reflect that:
	//   f > 0.1  → includes id 10 (its true value IS > 0.1)
	//   f >= 0.1 → includes id 10 (same reason, and exact-boundary safe)
	//   f < 0.1  → excludes id 10 (its true value is NOT < 0.1)
	//   f <= 0.1 → excludes id 10 (its true value is NOT <= 0.1)
	// A naive "round the literal to float32 and keep the operator"
	// implementation would instead evaluate `storedF32 > float32(0.1)`,
	// which is false for id 10 (equal, not greater) — WRONGLY excluding
	// it from `f > 0.1`. That is exactly the bug this rewrite (ceil/floor
	// + op adjustment) fixes.
	ck("boundary_gt_excludes_naive_would_miss", "SELECT id FROM bnd WHERE f > 0.1", []int64{10, 12})
	ck("boundary_ge_includes", "SELECT id FROM bnd WHERE f >= 0.1", []int64{10, 12})
	ck("boundary_lt_excludes_row10", "SELECT id FROM bnd WHERE f < 0.1", []int64{11})
	ck("boundary_le_excludes_row10", "SELECT id FROM bnd WHERE f <= 0.1", []int64{11})
	// Equality against a value that can never equal any float32 in the
	// table (0.15 is not exactly representable and no row was inserted
	// with a value rounding to it): must return empty, not error.
	ck("boundary_eq_unrepresentable_returns_empty", "SELECT id FROM bnd WHERE f = 0.15", nil)
}
