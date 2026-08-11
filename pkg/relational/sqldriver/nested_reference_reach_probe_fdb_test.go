package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFDB_NestedReferenceReachProbe records the MEASURED reach of two reference
// shapes that neighbour the qualified-projection mint. Both were raised as
// suspected second wrong-rows paths and NEITHER is one — which is the result
// worth keeping, because "it does not reproduce" is only load-bearing while the
// fact that makes it unreachable still holds.
//
// The three-segment arms decline LOUDLY with 42703, and identically with and
// without a join: the reach limit is the SEGMENT COUNT, not the join, and the
// decline comes from reference validation ahead of the projection ladder, not
// from an ordinal that could not be baked. The correlated grouped-scalar arm
// does not decline at all — it ANSWERS — so the shape gate downstream of it is
// not refusing a reachable key.
//
// THE THREE-SEGMENT ARMS ARE EXPECTED TO GO RED, AND THAT RED IS GOOD NEWS.
// Work is in flight to make `alias.struct.member` resolve — Java ANSWERS that
// spelling at the pinned tag, measured live in
// conformance/nested_groupby_key_java_probe_test.go, so Go's 42703 is a known
// divergence and closing it is a capability ARRIVING, not a regression. When
// these arms fail with rows where a 42703 was expected, the correct response is
// to assert the rows (`CO|300;200` for the join arm, `CO|300;200` for the
// single-source arm) and to re-check the qualified projection mint for the same
// struct-root bind the two-segment shape had. The response is NEVER to loosen
// the assertion or delete the arm: this file is the thing that will notice the
// deeper chain reaching a mint that has only ever been driven one level deep.
//
// Each arm names, in its failure message, what gets RE-ARMED if it changes.
func TestFDB_NestedReferenceReachProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	const dbPath = "/testdb_nested_reach_probe"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE nrp_tmpl "+
			"CREATE TYPE AS STRUCT nst (sk BIGINT, co BIGINT) "+
			"CREATE TABLE t1 (id BIGINT, n nst, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id2 BIGINT, other BIGINT, PRIMARY KEY (id2))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE nrp_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.ExecContext(ctx,
		"INSERT INTO t1 VALUES (1, (10, 300)), (2, (20, 200))"); err != nil {
		t.Fatalf("INSERT t1: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t2 VALUES (100, 1), (101, 1), (102, 2)"); err != nil {
		t.Fatalf("INSERT t2: %v", err)
	}

	cases := []struct {
		name   string
		sql    string
		want   string
		rearms string
	}{
		{
			// THREE segments: `alias.struct.member`. The reference's qualifier is
			// the whole two-segment text `A.N`, which names neither a FROM source
			// nor a struct column, because the segments were never split past the
			// first dot. The 42703 below therefore comes from reference
			// VALIDATION, upstream of the projection ladder — the ladder's
			// unresolvable-ordinal arm is never reached, and the next arm shows
			// why that has nothing to do with the join.
			name: "three_segment_over_join",
			sql:  "SELECT a.n.co FROM t1 AS a, t2 AS b ORDER BY id",
			want: `ERROR: 42703: column reference with qualifier "A.N" cannot be resolved`,
			rearms: "THE CAPABILITY ARRIVED, most likely the three-segment " +
				"qualified path landing. Do NOT loosen this assertion. Replace the " +
				"expectation with the ROWS (CO|300;200) and re-check the qualified " +
				"projection mint for the same struct-root bind the two-segment shape " +
				"had — it has only ever been driven one accessor deep",
		},
		{
			// The same three segments WITHOUT a join, for contrast: the reach
			// limit is the segment count, not the join.
			name: "three_segment_single_source",
			sql:  "SELECT t1.n.co FROM t1 ORDER BY id",
			want: `ERROR: 42703: column reference with qualifier "T1.N" cannot be resolved`,
			rearms: "THE CAPABILITY ARRIVED on a single source. Do NOT loosen this " +
				"assertion. Replace the expectation with the ROWS (CO|300;200) and " +
				"re-check ResolveIdentifier's fuse for a chain deeper than one " +
				"accessor, which no test drives today",
		},
		{
			// A CORRELATED grouping key inside a grouped scalar subquery, ordered
			// by that key. Raised as a suspected over-refusal in
			// groupedScalarSortKeys, whose ordinal gate accepts only a CHILDLESS
			// single-accessor FieldValue. It answers, and the reason it answers is
			// that the gate's input is always its own binder's output:
			// bindPostAggregateValueToNativeOrdinals emits exactly that shape for
			// a matched grouping key and errors on every other FieldValue, so the
			// gate restates a postcondition rather than restricting a capability.
			// The rows are the check on that reading.
			name: "correlated_grouped_scalar_order_by",
			sql: "SELECT id, (SELECT COUNT(*) FROM t2 WHERE t2.other = t1.id " +
				"GROUP BY t2.other ORDER BY t2.other) AS c FROM t1 ORDER BY id",
			want: "ID,C|1 2;2 1",
			rearms: "a correlated grouped-scalar ORDER BY key now plans; " +
				"groupedScalarSortKeys' childless single-accessor shape gate is " +
				"the thing to re-derive, since its input is no longer only " +
				"bindPostAggregateValueToNativeOrdinals' own output",
		},
	}

	for i := range cases {
		tc := cases[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := runShape(t, ctx, db, tc.sql)
			if got != tc.want {
				t.Fatalf("query %q\n   got: %s\n  want: %s\n  RE-ARMED IF THIS CHANGES: %s",
					tc.sql, got, tc.want, tc.rearms)
			}
		})
	}
}
