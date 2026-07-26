package sqldriver_test

// KNOWN BUG sentinel — an indexed DOUBLE/FLOAT column's equality and ordered
// range SARG DISAGREES with the full-scan residual-filter path on a value
// stored as -0.0 (negative zero). TODO.md CQ-27 "signed-zero SARG vs residual
// disagreement on an indexed DOUBLE/FLOAT column".
//
// Root cause: the SARG range/probe construction
// (scanComparisonsToTupleRange, executor.go) packs a comparand into the raw
// FDB tuple encoding and compares byte ranges — and the tuple encoding
// preserves the IEEE sign bit, so -0.0 sorts strictly BELOW +0.0. The
// residual-filter path instead evaluates via predicates.Comparison's cmpAny,
// which follows SQL/IEEE numeric equality: -0.0 == +0.0 is TRUE, and (per
// RFC-082) a `>=` predicate correctly keeps a -0.0 row against a 0.0 bound.
// The two physical plans for the SAME query can therefore disagree at
// exactly zero:
//   - `col = 0`:    the index MISSES a stored -0.0 row (full scan matches it).
//   - `col IN (0, ...)`: same miss, via the IN sub-probe.
//   - `col < 0`:    the index INCLUDES a stored -0.0 row (full scan excludes
//     it — -0.0 is not strictly less than 0 under IEEE equality).
//   - `col >= 0`:   the index MISSES a stored -0.0 row (full scan includes
//     it — this is RFC-082's rule, reached here through a DIFFERENT physical
//     plan than the one RFC-082's own regression exercises).
//
// Found scaling the sargability differential oracle's DOUBLE/FLOAT coverage
// (sargability_differential_oracle_fdb_test.go) to include boundary rows —
// its generator deliberately does NOT seed a signed-zero row (see the
// dbl_col/flt_col setup comments) specifically so this ALREADY-KNOWN gap
// does not make that ALWAYS-RUN, deterministic test permanently red. This
// file is the dedicated, controlled pin instead: it PASSES today by
// asserting the CURRENT (buggy) boundary, and FAILS the day the gap closes,
// forcing this file to be updated (flip the assertions + the SARG/matching
// fix needs its own RFC and the milestone-level architectural review ACK the
// query-engine skill requires for any matching/data-access-infra change,
// since scanComparisonsToTupleRange is core matching/data-access infra).
//
// A row soundness harness like rowdiff (pkg/relational/conformance/rowdiff)
// deliberately does NOT seed a random -0.0 value into its DOUBLE/FLOAT
// column domain for the identical reason: a RANDOM generator hitting this
// exact combination (indexed column + a query landing on zero + a stored
// -0.0) unpredictably, at nightly scale, would make that nightly red for an
// already-known, not-yet-fixed reason rather than a new finding.

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
	// non-zero-boundary query still returns something.
	mwjoMustExec(t, db, ctx, "INSERT INTO d (id, v) VALUES (1, -0.0), (2, 5.0)")
	mwjoMustExec(t, db, ctx, "INSERT INTO f (id, v) VALUES (1, -0.0), (2, 5.0)")

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
	// full-scan (MatchLeafRule disabled) connection, asserting: the
	// full-scan side is IEEE-correct (`wantCorrect`), and the index side
	// reproduces the CURRENT documented divergence (`wantBuggy`) — proving
	// the disagreement is specific to the SARG/index path, not a general
	// signed-zero comparison bug the residual filter shares.
	probe := func(t *testing.T, tbl, q string, wantBuggy, wantCorrect []int64) {
		t.Helper()
		requireIndexPlan(q)
		full := ids(fullConn, q)
		if !eq(full, wantCorrect) {
			t.Fatalf("%s: %q via full-scan (residual filter) = %v, want %v — the residual path's own "+
				"signed-zero handling regressed; that is a DIFFERENT, more severe bug than CQ-27",
				tbl, q, full, wantCorrect)
		}
		idx := ids(idxConn, q)
		if eq(idx, wantCorrect) {
			t.Fatalf("%s: %q via the index now returns %v, matching the full-scan side — "+
				"the signed-zero SARG gap (TODO.md CQ-27) may be FIXED; flip this sentinel's "+
				"wantBuggy to wantCorrect and close the TODO entry", tbl, q, idx)
		}
		if !eq(idx, wantBuggy) {
			t.Fatalf("%s: %q via the index = %v, want %v (current buggy boundary) or %v (fixed) — "+
				"got something else entirely, investigate before touching this sentinel",
				tbl, q, idx, wantBuggy, wantCorrect)
		}
	}

	for _, tbl := range []string{"d", "f"} {
		tbl := tbl
		t.Run(tbl, func(t *testing.T) {
			// Equality: index MISSES the -0.0 row; full scan matches it.
			probe(t, tbl, fmt.Sprintf("SELECT id FROM %s WHERE v = 0", tbl), nil, []int64{1})
			// IN-list: same miss via the sub-probe.
			probe(t, tbl, fmt.Sprintf("SELECT id FROM %s WHERE v IN (0, 5)", tbl), []int64{2}, []int64{1, 2})
			// `<`: index WRONGLY INCLUDES -0.0 (not strictly < 0 under IEEE).
			probe(t, tbl, fmt.Sprintf("SELECT id FROM %s WHERE v < 0", tbl), []int64{1}, nil)
			// `>=`: index WRONGLY EXCLUDES -0.0 (RFC-082: -0.0 >= 0.0 is TRUE).
			probe(t, tbl, fmt.Sprintf("SELECT id FROM %s WHERE v >= 0", tbl), []int64{2}, []int64{1, 2})
		})
	}
}
