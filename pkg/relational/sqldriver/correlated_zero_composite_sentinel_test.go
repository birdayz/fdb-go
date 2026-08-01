package sqldriver_test

// The correlated half of CQ-28 (RFC-196's "known gap, deliberately
// uncovered"), now FIXED and asserted. This file was the flip-sentinel that
// pinned the WRONG behavior; the fix landed, it fired, and it now pins the
// correct rows.
//
// The shape: `t.v = o.k AND t.w = 5` against composite index (v, w), where
// the comparand o.k is CORRELATED — supplied per-probe by an outer row and
// therefore opaque at plan time. When the runtime value is a signed zero,
// IEEE `=` (this engine's ruled semantics — see DIVERGENCES.md and RFC-205)
// requires matching BOTH stored zeros, but -0.0 and +0.0
// are distinct adjacent index keys and the two wanted keys
// (-0.0, 5) / (+0.0, 5) are NOT a contiguous interval: the span between
// them also admits (-0.0, w>5) and (+0.0, w<5). A single probe missed a
// row; a naively widened interval would return wrong rows.
//
// The fix is an execution-time physical range set. The binder uses the
// physical key-component type and, when a zero-valued float equality does NOT
// terminate the scan prefix, records one exact alternative per signed zero.
// The range-set cursor enumerates the Cartesian product lazily in key order.
// Disjoint ranges make the union duplicate-free with no dedup layer, while the
// full composite prefix stays sarg'd — no de-sarg and no residual filter.
//
// Rows 3 and 4 sit BETWEEN the two zero groups' w=5 keys, so this test
// fails loudly in BOTH error directions: a single-sign probe misses id 1 or
// 5; a naive contiguous widening pulls in 3 and/or 4. Table t2 carries the
// same rows with NO index, pinning index-path/residual-path agreement (the
// sargability differential principle: the access path must never change the
// answer).

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

func TestFDB_CorrelatedZeroCompositeSentinel(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_czs")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_czs")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE czs "+
		"CREATE TABLE t (id BIGINT, v DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX t_vw ON t (v, w) "+
		"CREATE TABLE t2 (id BIGINT, v DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE o (id BIGINT, k DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_czs/s WITH TEMPLATE czs")
	dsn := fmt.Sprintf("fdbsql:///testdb_czs?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// id 1: NEGATIVE zero at w=5 — must match a +0.0 (and -0.0) outer key.
	// id 2: nonzero control at w=5.
	// id 3: negative zero at w=9 — between the zero groups' w=5 keys; a naive
	//       contiguous widening wrongly pulls it in.
	// id 4: positive zero at w=1 — same guard from the other side.
	// id 5: POSITIVE zero at w=5 — must match a -0.0 (and +0.0) outer key.
	const rows = " (1, -0.0, 5), (2, 5.0, 5), (3, -0.0, 9), (4, 0.0, 1), (5, 0.0, 5)"
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v, w) VALUES"+rows)
	mwjoMustExec(t, db, ctx, "INSERT INTO t2 (id, v, w) VALUES"+rows)
	// Outer keys: id 10 = +0.0, id 30 = -0.0 (both correlation directions),
	// id 20 = nonzero control, and id 40 = NULL (SQL 3VL control).
	mwjoMustExec(t, db, ctx, "INSERT INTO o (id, k) VALUES (10, 0.0), (20, 5.0), (30, -0.0), (40, NULL)")

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	ids := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, err := conn.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
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

	for _, tc := range []struct {
		query string
		want  []int64
		why   string
	}{
		{
			"SELECT t.id FROM t, o WHERE t.v = o.k AND t.w = 5 AND o.id = 10",
			[]int64{1, 5},
			"correlated +0.0 on a NON-terminal composite column must return BOTH zeros' rows, exactly once each, and no in-between rows",
		},
		{
			"SELECT t.id FROM t, o WHERE t.v = o.k AND t.w = 5 AND o.id = 30",
			[]int64{1, 5},
			"correlated -0.0 (the other correlation direction) must return the same rows",
		},
		{
			"SELECT t2.id FROM t2, o WHERE t2.v = o.k AND t2.w = 5 AND o.id = 10",
			[]int64{1, 5},
			"residual-filter path (no index on t2) must agree with the index path, +0.0 outer",
		},
		{
			"SELECT t2.id FROM t2, o WHERE t2.v = o.k AND t2.w = 5 AND o.id = 30",
			[]int64{1, 5},
			"residual-filter path must agree with the index path, -0.0 outer",
		},
		{
			"SELECT t.id FROM t, o WHERE t.v = o.k AND t.w = 5 AND o.id = 40",
			nil,
			"a correlated NULL equality comparand is UNKNOWN and must bind to an empty range set",
		},
		{
			"SELECT t2.id FROM t2, o WHERE t2.v = o.k AND t2.w = 5 AND o.id = 40",
			nil,
			"the residual path must agree that a correlated NULL equality matches no rows",
		},
		{
			"SELECT t.id FROM t, o WHERE t.v = o.k AND o.id = 10",
			[]int64{1, 3, 4, 5},
			"correlated zero in TERMINAL position still widens correctly (all zero-v rows)",
		},
		{
			"SELECT id FROM t WHERE v = 0 AND w = 5",
			[]int64{1, 5},
			"the constant-zero composite case CQ-28 fixed must stay fixed",
		},
		{
			"SELECT t.id FROM t, o WHERE t.v = o.k AND t.w = 5 AND o.id = 20",
			[]int64{2},
			"a correlated NONZERO comparand keeps the full composite prefix and matches",
		},
	} {
		if got := ids(t, tc.query); !eq(got, tc.want) {
			t.Errorf("%s\n%q returned %v, want %v", tc.why, tc.query, got, tc.want)
		}
	}

	// The fix must not de-sarg the correlated composite probe: the join's
	// inner side still scans the composite index. A "fix" that dropped the
	// index probe would pass the row assertions above through a slower
	// residual scan — pin the access path itself.
	var plan string
	if err := conn.QueryRowContext(ctx,
		"EXPLAIN SELECT t.id FROM t, o WHERE t.v = o.k AND t.w = 5 AND o.id = 10").Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	if !strings.Contains(plan, "T_VW") {
		t.Errorf("correlated composite probe no longer uses index T_VW — de-sargged?\nplan: %s", plan)
	}
	if !strings.Contains(plan, "[=, =]") {
		t.Errorf("correlated composite probe dropped part of the (v, w) equality prefix\nplan: %s", plan)
	}
}

// Shape pins for physical range sets beyond the one-zero sentinel above: the
// 2^k Cartesian product, aggregate consumers over a plural physical prefix,
// and the negative result that a REVERSE range-set scan is not SQL-reachable
// today.
// Each subtest is a proof that justified a design decision; the failure
// messages say what gets re-armed if the pinned fact changes.
func TestFDB_CorrelatedZeroRangeSetShapes(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_czf")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_czf")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE czf "+
		"CREATE TABLE t (id BIGINT, v DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX t_vw ON t (v, w) "+
		"CREATE TABLE m (id BIGINT, a DOUBLE, b DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX m_abw ON m (a, b, w) "+
		"CREATE TABLE o (id BIGINT, k DOUBLE, k2 DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_czf/s WITH TEMPLATE czf")
	dsn := fmt.Sprintf("fdbsql:///testdb_czf?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx,
		"INSERT INTO t (id, v, w) VALUES (1, -0.0, 5), (2, 5.0, 5), (3, -0.0, 9), (4, 0.0, 1), (5, 0.0, 5)")
	// m: ALL FOUR sign combinations at w=5 (ids 1-4) — the 2^2 Cartesian range
	// set must cover — plus guards: ids 5-6 sit between the wanted
	// (a,b,5) keys in index order (wrong w), ids 7-8 have a nonzero in one
	// fork column (must not match a zero comparand).
	mwjoMustExec(t, db, ctx,
		"INSERT INTO m (id, a, b, w) VALUES "+
			"(1, -0.0, -0.0, 5), (2, -0.0, 0.0, 5), (3, 0.0, -0.0, 5), (4, 0.0, 0.0, 5), "+
			"(5, -0.0, -0.0, 9), (6, 0.0, 0.0, 1), (7, -0.0, 5.0, 5), (8, 5.0, 0.0, 5)")
	mwjoMustExec(t, db, ctx, "INSERT INTO o (id, k, k2) VALUES (10, 0.0, -0.0)")

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	ids := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, err := conn.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
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

	// TWO correlated zero comparands on one index (a, b, w): the binder records
	// two alternatives per non-terminal zero equality and the cursor enumerates
	// their 2^2 Cartesian product lazily — four disjoint ranges, exactly one row
	// per sign combination, no duplicates, and the full prefix stays sarg'd.
	t.Run("two-zero-cartesian", func(t *testing.T) {
		q := "SELECT m.id FROM m, o WHERE m.a = o.k AND m.b = o.k2 AND m.w = 5 AND o.id = 10"
		plan := explainOnConn(t, ctx, conn, q)
		if !strings.Contains(plan, "IndexScan(M_ABW, [=, =, =])") {
			t.Errorf("two-zero probe no longer sargs the full (a, b, w) prefix — de-sargged or residualized?\nplan: %s", plan)
		}
		got := ids(t, q)
		want := []int64{1, 2, 3, 4}
		if !eq(got, want) {
			t.Errorf("two correlated zero equalities must return exactly one row per sign combination: got %v, want %v\nplan: %s", got, want, plan)
		}
	})

	// Aggregate consumers sit above the same range-set scan; a dropped or
	// duplicated physical range would corrupt COUNT/MAX silently (no row list
	// to eyeball), so pin the aggregate values over the correlated join.
	t.Run("aggregate-over-range-set", func(t *testing.T) {
		for _, tc := range []struct {
			q    string
			want int64
		}{
			{"SELECT COUNT(*) FROM t, o WHERE t.v = o.k AND t.w = 5 AND o.id = 10", 2},
			{"SELECT MAX(t.id) FROM t, o WHERE t.v = o.k AND t.w = 5 AND o.id = 10", 5},
		} {
			plan := explainOnConn(t, ctx, conn, tc.q)
			if !strings.Contains(plan, "IndexScan(T_VW, [=, =])") {
				t.Errorf("aggregate over the correlated range set lost the composite index probe\nquery: %s\nplan: %s", tc.q, plan)
			}
			var got int64
			if err := conn.QueryRowContext(ctx, tc.q).Scan(&got); err != nil {
				t.Fatalf("%q: %v", tc.q, err)
			}
			if got != tc.want {
				t.Errorf("%q = %d, want %d\nplan: %s", tc.q, got, tc.want, plan)
			}
		}
	})

	// RFC-205 makes the REVERSE range-set path SQL-reachable: ORDER BY t.v DESC
	// is satisfied directly by a reverse composite-index probe. Pin both its
	// physical order (+0 branch before -0) and transparent statement pagination
	// under budgets that stop inside and between the two signed-zero branches.
	t.Run("reverse-order-and-resume", func(t *testing.T) {
		q := "SELECT t.id FROM t, o WHERE t.v = o.k AND t.w = 5 AND o.id = 10 ORDER BY t.v DESC"
		plan := explainOnConn(t, ctx, conn, q)
		if !strings.Contains(plan, "IndexScan(T_VW, [=, =]) REVERSE") || strings.Contains(plan, "InMemorySort") {
			t.Fatalf("DESC over the correlated range set must use the reverse composite probe directly\nplan: %s", plan)
		}

		unpaged := scanInt64Rows(t, ctx, conn, q)
		if got, want := fmt.Sprint(unpaged), "[5 1]"; got != want {
			t.Fatalf("reverse physical range order = %s, want %s\nplan: %s", got, want, plan)
		}
		for budget := 2; budget <= 4; budget++ {
			pagedConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
				ec.SetOptions(api.NewOptionsBuilder().
					Set(api.OptExecutionScannedRowsLimit, budget).
					Build())
			})
			paged := scanInt64Rows(t, ctx, pagedConn, q)
			if fmt.Sprint(paged) != fmt.Sprint(unpaged) {
				t.Fatalf("reverse range-set continuation dropped, duplicated, or reordered rows (budget %d): paged=%v unpaged=%v",
					budget, paged, unpaged)
			}
		}
	})
}
