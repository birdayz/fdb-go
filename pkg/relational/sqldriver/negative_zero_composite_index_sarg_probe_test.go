package sqldriver_test

// A zero-valued float equality must find BOTH signed zeros even when it is NOT
// the terminal column of a composite index's equality prefix (RFC-208,
// superseding TODO CQ-28's terminal-only stopgap).
//
// IEEE says -0.0 == +0.0 and this engine's `=` follows IEEE, but FDB tuple
// encoding preserves the sign bit, so the two zeros are distinct adjacent index
// keys and a logical probe must cover both. Before RFC-208, `v = 0 AND w = 5`
// consumed both columns, so the zero was not terminal and the single physical
// probe returned NOTHING for a stored (-0.0, 5).
//
// Widening the whole range is not available: the union of (-0.0,5) and (+0.0,5)
// is not a contiguous interval — the span between them also admits (-0.0, w>5)
// and (+0.0, w<5) — so a single TupleRange spanning both would return WRONG
// rows rather than missing ones.
//
// RFC-208 keeps the full `[=,=]` comparison prefix and binds one exact range for
// each physical zero sign at runtime. No broad interval or residualized suffix
// is needed, and parameters/correlations use the same binder. The correlated
// case is pinned separately by TestFDB_CorrelatedZeroCompositeSentinel.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_NegativeZeroCompositeIndexSargProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nzcsarg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nzcsarg")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE nzcsarg "+
		"CREATE TABLE t (id BIGINT, v DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX t_vw ON t (v, w)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nzcsarg/s WITH TEMPLATE nzcsarg")
	dsn := fmt.Sprintf("fdbsql:///testdb_nzcsarg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// id 1: NEGATIVE zero at w=5 — the row `v = 0 AND w = 5` must find.
	// id 2: positive control so the query is not vacuous.
	// id 3: POSITIVE zero at a DIFFERENT w — a naive single-interval widening
	//       would wrongly pull it in, so it guards the opposite error.
	// id 4: negative zero at a different w, guarding the same from below.
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v, w) VALUES "+
		"(1, -0.0, 5), (2, 5.0, 5), (3, 0.0, 9), (4, -0.0, 1)")

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
		var o []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			o = append(o, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
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

	for _, tc := range []struct {
		query string
		want  []int64
		why   string
	}{
		{
			"SELECT id FROM t WHERE v = 0 AND w = 5",
			[]int64{1},
			"a stored -0.0 must satisfy `= 0` in a NON-terminal composite equality prefix",
		},
		{
			"SELECT id FROM t WHERE v = 0 AND w = 9",
			[]int64{3},
			"the trailing equality must still bind — a widened interval spanning both " +
				"zeros would also admit the w values in between",
		},
		{
			"SELECT id FROM t WHERE v = 0 AND w = 1",
			[]int64{4},
			"same, guarding the low side of the interval",
		},
		{
			"SELECT id FROM t WHERE v = 0",
			[]int64{1, 3, 4},
			"terminal position still returns every zero row",
		},
		{
			"SELECT id FROM t WHERE v = 5.0 AND w = 5",
			[]int64{2},
			"an ordinary nonzero equality keeps the full composite prefix",
		},
	} {
		t.Run(tc.query, func(t *testing.T) {
			// Pin that the COMPOSITE INDEX path actually ran. Without this the
			// row assertions pass on a primary scan too, because the residual
			// IEEE comparison handles signed zero correctly on its own — so the
			// test would prove nothing about the SARG path it exists to cover,
			// and reverting the fix would not reliably redden it.
			if plan := explainOnConn(t, ctx, conn, tc.query); !strings.Contains(plan, "T_VW") {
				t.Fatalf("%s\nplan = %s\nwant the composite index T_VW: on a primary scan the "+
					"residual filter answers correctly by itself, so this case would pass "+
					"without exercising the changed matching path at all", tc.query, plan)
			}
			if got := ids(t, tc.query); !eq(got, tc.want) {
				t.Fatalf("%s\n%s = %v, want %v", tc.why, tc.query, got, tc.want)
			}
		})
	}
}
