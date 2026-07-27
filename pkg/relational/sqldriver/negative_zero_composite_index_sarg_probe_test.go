package sqldriver_test

// KNOWN BUG sentinel — TODO.md CQ-28, a narrower sibling of CQ-27 (fixed by
// negative_zero_index_sarg_probe_test.go's pin). CQ-27's fix widens a
// zero-valued FLOAT/DOUBLE scan bound to span both adjacent index keys
// (-0.0 and +0.0 — FDB tuple encoding preserves the IEEE sign bit, so they
// pack to distinct, physically adjacent keys) ONLY at the position that
// TERMINATES the scan range: the sole trailing inequality, or an equality
// that is the last comparison in the whole list. A zero-valued equality that
// is NOT terminal — more columns follow it in a COMPOSITE index's equality
// prefix — is deliberately left unwidened, because widening it correctly
// would require either a genuine two-way range union (not expressible as a
// single TupleRange low/high pair once a later column is also pinned to an
// exact value — the union of "(-0.0, W)" and "(+0.0, W)" is NOT a contiguous
// interval once W narrows the trailing component past the full subtree) or
// teaching the SARG matcher (abstract_data_access_rule.go /
// match_candidate_index.go) to keep a residual filter for a composite
// equality prefix it currently treats as fully covered — matching/data-access
// infra changes, which carry their own design gate before implementation.
//
// This is the reproducer CQ-28 needs: a composite index (v DOUBLE, w BIGINT)
// with a -0.0 leading value, index MISSES the row on `v = 0 AND w = ...`
// exactly like the pre-fix CQ-27 equality case did for a single column.
// PASSES today by asserting the CURRENT (buggy) boundary; FAILS the day
// CQ-28 closes, forcing this file to be updated (flip the assertion + the
// fix needs its own RFC and review ACK).
//
// Safety invariant this file exists to protect: pkg/relational/conformance/rowdiff's
// generator and sargability_differential_oracle_fdb_test.go both now seed
// -0.0 into DOUBLE/FLOAT column data (CQ-27 closed that gap) — safe ONLY
// because neither harness's index pool puts a FLOAT/DOUBLE column in a
// NON-TERMINAL position of a composite index. If that ever changes, CQ-28
// stops being a narrow, deliberately-scoped gap and starts being a silent
// nightly-red generator (see the "Deliberately NOT ..." comments at both
// generator sites, which now point back here instead of at CQ-27).

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
		"CREATE TABLE t (id BIGINT NOT NULL, v DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX t_vw ON t (v, w)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nzcsarg/s WITH TEMPLATE nzcsarg")
	dsn := fmt.Sprintf("fdbsql:///testdb_nzcsarg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// id 1: v is NEGATIVE zero, w = 5 — the row the index SHOULD find for
	// `v = 0 AND w = 5` (IEEE: -0.0 == 0) but currently does not, because `v`
	// is not the terminal comparison in the composite equality prefix.
	// id 2: a clear positive control (v = 5.0) so the query isn't vacuous.
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v, w) VALUES (1, -0.0, 5), (2, 5.0, 5)")

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	q := "SELECT id FROM t WHERE v = 0 AND w = 5"

	plan := explainOnConn(t, ctx, conn, q)
	if !strings.Contains(plan, "IndexScan") {
		t.Fatalf("%q: expected an IndexScan plan (else this sentinel proves nothing about the "+
			"composite SARG path), got: %s", q, plan)
	}

	ids := func() []int64 {
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

	got := ids()
	wantCorrect := []int64{1}
	if eq(got, wantCorrect) {
		t.Fatalf("%q now returns %v, matching IEEE-correct behavior — CQ-28 may be FIXED; "+
			"flip this sentinel to require %v and close the TODO entry", q, got, wantCorrect)
	}
	wantBuggy := []int64(nil)
	if !eq(got, wantBuggy) {
		t.Fatalf("%q = %v, want %v (current buggy boundary, missing id 1's -0.0 row) or %v "+
			"(fixed) — got something else entirely, investigate before touching this sentinel",
			q, got, wantBuggy, wantCorrect)
	}
}
