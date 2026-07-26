package sqldriver_test

// Regression pin for TODO.md CQ-27 — an indexed DOUBLE/FLOAT column's
// equality and ordered-range SARG must agree with the full-scan
// residual-filter path on a value stored as -0.0 (negative zero).
//
// Root cause (fixed): scanComparisonsToTupleRange (executor.go) packed a
// zero comparand with whatever sign it happened to carry. FDB tuple encoding
// preserves the IEEE sign bit (pkg/fdbgo/fdb/tuple's adjustFloatBytes), so
// +0.0 and -0.0 are two DISTINCT, adjacent index keys — but the
// residual-filter comparator (cmpAny, RFC-082) follows IEEE/SQL numeric
// equality, where -0.0 == +0.0. A sign-pinned probe or range endpoint
// therefore disagreed with the residual path at exactly zero:
//   - `col = 0` / `col IN (0, ...)` missed a stored -0.0 row.
//   - `col < 0` wrongly included a stored -0.0 row.
//   - `col >= 0` wrongly excluded a stored -0.0 row.
//
// The fix widens the zero endpoint of a scan range to span BOTH adjacent
// keys (equality widens to the [-0.0, +0.0] subtree; `<`/`>=` canonicalize
// their bound to whichever of the two adjacent keys makes the endpoint
// IEEE-correct) rather than normalizing the stored/wire encoding — the raw
// FDB tuple layer's sign-preserving float encoding matches Java's
// (TupleOrderingTest) and the record layer's SORT comparator on purpose
// (executor's compareValues / values.CompareFloat64), so it must not change;
// only the SARG/probe construction, which is Go-only executor logic with no
// Java analog (Java's own predicate comparator is sign-preserving too, per
// Comparisons.compare -> Double.compareTo/equals — RFC-082 is a deliberate,
// already-reviewed Go extension beyond Java), needed to change.
//
// This test asserts the two physical paths for the SAME query now AGREE,
// on both DOUBLE and FLOAT, indexed, across every operator a SARG can be
// built from plus NULL interaction.

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

func TestFDB_NegativeZeroIndexSargProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nzsarg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nzsarg")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE nzsarg "+
		"CREATE TABLE d (id BIGINT NOT NULL, v DOUBLE, PRIMARY KEY (id)) "+
		"CREATE INDEX d_v ON d (v) "+
		"CREATE TABLE f (id BIGINT NOT NULL, v FLOAT, PRIMARY KEY (id)) "+
		"CREATE INDEX f_v ON f (v)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nzsarg/s WITH TEMPLATE nzsarg")
	dsn := fmt.Sprintf("fdbsql:///testdb_nzsarg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// id 1 stores NEGATIVE zero; id 2 is a clear positive control so a
	// non-zero-boundary query still returns something; id 3 is NULL so the
	// SARG's null-boundary handling around a zero comparand is also exercised.
	mwjoMustExec(t, db, ctx, "INSERT INTO d (id, v) VALUES (1, -0.0), (2, 5.0), (3, NULL)")
	mwjoMustExec(t, db, ctx, "INSERT INTO f (id, v) VALUES (1, -0.0), (2, 5.0), (3, NULL)")

	idxConn := pinEmbeddedConn(t, db, func(*embedded.EmbeddedConnection) {})
	fullConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptDisabledPlannerRules, []string{"MatchLeafRule"}).Build())
	})

	ids := func(conn *sql.Conn, q string) []int64 {
		t.Helper()
		rows, err := conn.QueryContext(ctx, q)
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
	requireIndexPlan := func(q string) {
		t.Helper()
		plan := explainOnConn(t, ctx, idxConn, q)
		if !strings.Contains(plan, "IndexScan") {
			t.Fatalf("%q: expected an IndexScan plan on the index-eligible connection (else this "+
				"sentinel proves nothing about the SARG path), got: %s", q, plan)
		}
	}

	// probe runs one query on both the index-eligible connection and the
	// full-scan (MatchLeafRule disabled) connection, asserting BOTH equal
	// `want` — proving the SARG range and the residual filter now AGREE at
	// the signed-zero boundary, not merely that one of them is right.
	probe := func(t *testing.T, tbl, q string, want []int64) {
		t.Helper()
		requireIndexPlan(q)
		full := ids(fullConn, q)
		if !eq(full, want) {
			t.Fatalf("%s: %q via full-scan (residual filter) = %v, want %v", tbl, q, full, want)
		}
		idx := ids(idxConn, q)
		if !eq(idx, want) {
			t.Fatalf("%s: %q via the index = %v, want %v (full-scan agrees on %v — this is a "+
				"SARG-vs-residual disagreement, exactly what CQ-27 was)", tbl, q, idx, want, full)
		}
	}
	// notEqProbe checks `<>` WITHOUT requiring an IndexScan: NOT_EQUALS is
	// ComparisonType.NONE in Java (residual, never sargable — see
	// scanComparisonsToTupleRange's default-arm comment) and, with no other
	// predicate to justify visiting the secondary index at all, the planner
	// takes a full PRIMARY scan here rather than IndexScan(D_V)/IndexScan(F_V)
	// — there is no SARG-vs-residual disagreement to prove for this operator,
	// only that the residual comparator itself is signed-zero-correct.
	notEqProbe := func(t *testing.T, tbl, q string, want []int64) {
		t.Helper()
		got := ids(idxConn, q)
		if !eq(got, want) {
			t.Fatalf("%s: %q = %v, want %v", tbl, q, got, want)
		}
	}

	for _, tbl := range []string{"d", "f"} {
		tbl := tbl
		t.Run(tbl, func(t *testing.T) {
			// Equality: -0.0 == +0.0 under IEEE, so `= 0` must match the stored
			// -0.0 row.
			probe(t, tbl, fmt.Sprintf("SELECT id FROM %s WHERE v = 0", tbl), []int64{1})
			// Inverse of equality: `<> 0` must NOT match the -0.0 row (it's
			// numerically equal to 0), and must not match the NULL row (3VL
			// UNKNOWN).
			notEqProbe(t, tbl, fmt.Sprintf("SELECT id FROM %s WHERE v <> 0", tbl), []int64{2})
			// IN-list: same as equality, via the per-element sub-probe.
			probe(t, tbl, fmt.Sprintf("SELECT id FROM %s WHERE v IN (0, 5)", tbl), []int64{1, 2})
			// `<`: neither -0.0 nor +0.0 is strictly less than 0.
			probe(t, tbl, fmt.Sprintf("SELECT id FROM %s WHERE v < 0", tbl), nil)
			// `<=`: both -0.0 and +0.0 satisfy <= 0, so the -0.0 row matches.
			probe(t, tbl, fmt.Sprintf("SELECT id FROM %s WHERE v <= 0", tbl), []int64{1})
			// `>`: neither -0.0 nor +0.0 is strictly greater than 0.
			probe(t, tbl, fmt.Sprintf("SELECT id FROM %s WHERE v > 0", tbl), []int64{2})
			// `>=`: both -0.0 and +0.0 satisfy >= 0 (RFC-082), so the -0.0 row
			// must NOT be dropped.
			probe(t, tbl, fmt.Sprintf("SELECT id FROM %s WHERE v >= 0", tbl), []int64{1, 2})
			// NULL interaction: IS [NOT] NULL around a zero-valued indexed
			// column must be unaffected by the signed-zero widening.
			probe(t, tbl, fmt.Sprintf("SELECT id FROM %s WHERE v IS NULL", tbl), []int64{3})
			probe(t, tbl, fmt.Sprintf("SELECT id FROM %s WHERE v IS NOT NULL", tbl), []int64{1, 2})
		})
	}
}
