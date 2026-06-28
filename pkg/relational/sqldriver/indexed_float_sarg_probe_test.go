package sqldriver_test

// KNOWN BUG sentinel — SEVERE: an indexed FLOAT (32-bit) column returns ZERO rows for
// equality/range comparisons (silent missing rows). TODO.md "indexed FLOAT column
// SARG returns no rows".
//
// `f = 1.5` / `f > 1.0` on an INDEXED FLOAT column → []; the SAME query on a
// NON-indexed FLOAT column is correct (residual path compares in double space). So
// the index SARG is the culprit: the float64 literal is packed into the float32 index
// range with a mismatched FDB tuple type code (float vs double), matching nothing.
// (1.5/1.0 are exactly representable in float32, so this is NOT a precision issue —
// the SARG is fundamentally cross-type-broken for FLOAT columns.) DOUBLE-indexed
// columns are fine; only FLOAT(32) is affected.
//
// This is the sibling of the int-const-vs-DOUBLE-col SARG bug this PR fixes
// (widenIntConstAgainstDouble): here it's double/int-const vs FLOAT-col. Root cause:
// promoteConstant (value_constant_object.go:150) has no float64→TypeCodeFloat case,
// and widenIntConstAgainstDouble doesn't handle FLOAT columns. The fix is a
// cross-WIDTH SARG decision (compare in float32-space, or widen the float32 index
// scan + residual-filter in double-space) — Graefe's design call (concern C: uniform
// PromoteValue/MaximumType). This test pins the current (buggy) boundary; flip the
// indexed cases when fixed.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
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
		"CREATE INDEX wi_f ON withidx (f) "+
		"CREATE INDEX di_f ON dblidx (f)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ifs/s WITH TEMPLATE ifs")
	dsn := fmt.Sprintf("fdbsql:///testdb_ifs?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO noidx (id, f) VALUES (1,1.5),(2,2.5),(3,0.5)")
	mwjoMustExec(t, db, ctx, "INSERT INTO withidx (id, f) VALUES (1,1.5),(2,2.5),(3,0.5)")
	mwjoMustExec(t, db, ctx, "INSERT INTO dblidx (id, f) VALUES (1,1.5),(2,2.5),(3,0.5)")

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

	// CORRECT: non-indexed FLOAT (residual path, double-space comparison).
	t.Run("noindex_float_eq_correct", func(t *testing.T) {
		if got := ids("SELECT id FROM noidx WHERE f = 1.5"); !eq(got, []int64{1}) {
			t.Errorf("noidx f=1.5 = %v, want [1]", got)
		}
	})
	t.Run("noindex_float_range_correct", func(t *testing.T) {
		if got := ids("SELECT id FROM noidx WHERE f > 1.0"); !eq(got, []int64{1, 2}) {
			t.Errorf("noidx f>1.0 = %v, want [1 2]", got)
		}
	})
	// CORRECT: DOUBLE-indexed (the common type) works — scopes the bug to FLOAT(32).
	t.Run("double_indexed_eq_correct", func(t *testing.T) {
		if got := ids("SELECT id FROM dblidx WHERE f = 1.5"); !eq(got, []int64{1}) {
			t.Errorf("dblidx f=1.5 = %v, want [1] (DOUBLE index should work)", got)
		}
	})
	// BUG: indexed FLOAT returns empty (flip these when fixed).
	t.Run("indexed_float_eq_currently_empty_BUG", func(t *testing.T) {
		if got := ids("SELECT id FROM withidx WHERE f = 1.5"); len(got) != 0 {
			t.Errorf("indexed f=1.5 now returns %v — the FLOAT SARG bug may be FIXED; "+
				"flip this sentinel (want [1]) + update TODO.md", got)
		}
	})
	t.Run("indexed_float_range_currently_empty_BUG", func(t *testing.T) {
		if got := ids("SELECT id FROM withidx WHERE f > 1.0"); len(got) != 0 {
			t.Errorf("indexed f>1.0 now returns %v — the FLOAT SARG bug may be FIXED; "+
				"flip this sentinel (want [1 2]) + update TODO.md", got)
		}
	})
}
