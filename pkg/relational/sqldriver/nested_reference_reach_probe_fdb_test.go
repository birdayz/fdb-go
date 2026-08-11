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
// THIS FILE'S PURPOSE CHANGED WHEN THE THREE-SEGMENT PATH LANDED, and the
// history is kept because a pin whose reason has evaporated is worse than no
// pin. It was written as a NEGATIVE-result file: both three-segment arms
// refused with 42703, identically with and without a join, which isolated the
// reach limit as the SEGMENT COUNT rather than the join. That isolation is
// gone — the segment count is supported now, both arms ANSWER, and the
// distinction they existed to draw no longer exists to be drawn.
//
// What they watch INSTEAD is the thing the capability made reachable: a
// three-segment reference descends TWO accessors deep, and the mint it goes
// through had only ever been driven ONE deep. So the arms now assert the
// leaf's ROWS on both shapes. A struct-root bind at depth two would show up
// here as a struct-shaped cell exactly as it did at depth one, and nothing
// else in this file would notice.
//
// The join arm returns SIX rows and that is correct, not a bug: its fixture
// gives t2 three rows, so the cross join is 2x3, and the CO values repeat per
// outer row. The earlier version of this comment predicted two rows for that
// arm — written from the other fixture's single t2 row, and wrong. Measure the
// shape; do not infer it from a sibling.
//
// THE DUPLICATE-ALIAS SQUARE AT DEPTH TWO IS NOT HERE, deliberately.
// TestFDB_DuplicateFromAliasPerAttributeBindsTheRightLeg owns it and was
// MEASURED to catch the projection mint's fuse being removed, so an arm for it
// here would be padding. What these two arms hold that it does not is the
// distinct-alias join and the single-source route.
//
// The correlated grouped-scalar arm is unchanged and still a negative result:
// it ANSWERS, so the shape gate downstream of it is not refusing a reachable
// key.
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
			// THREE segments over a join: `alias.struct.member`, routed through
			// the qualified projection mint. The rows are what this arm is for —
			// the mint binds the leg AND carries a TWO-accessor descent, and
			// dropping the descent there reads the struct where CO was named.
			//
			// SIX rows: t2 has three rows, so the cross join repeats each outer
			// row's CO three times. A two-row expectation here would be a
			// different query's answer.
			name: "three_segment_over_join",
			sql:  "SELECT a.n.co FROM t1 AS a, t2 AS b ORDER BY id",
			want: "CO|300;300;300;200;200;200",
			rearms: "the three-segment descent over a join stopped reading the " +
				"leaf. A struct-shaped cell means the mint bound the ROOT at depth " +
				"two — the same defect the two-segment shape had, one accessor " +
				"deeper. A row-COUNT change instead means the join shape moved, " +
				"which is a different bug: check the fixture's t2 row count first",
		},
		{
			// The same three segments WITHOUT a join. It used to be here to prove
			// the 42703 was about segment count rather than the join; both answer
			// now, so what it holds is the SINGLE-SOURCE route through
			// ResolveIdentifier's fuse at depth two, which the join arm does not
			// exercise.
			name: "three_segment_single_source",
			sql:  "SELECT t1.n.co FROM t1 ORDER BY id",
			want: "CO|300;200",
			rearms: "the single-source three-segment descent stopped reading the " +
				"leaf. This arm and the join arm go through DIFFERENT mints, so one " +
				"reddening alone localises the regression: this one is " +
				"ResolveIdentifier's fuse, the other is the qualified projection mint",
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
