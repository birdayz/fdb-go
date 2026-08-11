package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFDB_UnnestElementMemberInGather is a NEGATIVE result, pinned because the
// claim it supports is load-bearing and would otherwise be a guess.
//
// bakeGatheredGroupValue (pkg/relational/core/query/unnest_gather.go) is the
// GROUP-BY/gathered twin of bakeUnnestElementRefOrdinal: same skip gate
// (`fv.Resolved != nil && !fv.SourceRelativeBaked()` — return the node
// untouched), same `elementSlots map[string]int`, same name-keyed slot lookup on
// `fv.Field`. In the sibling that shape was a LIVE defect: `SourceRelativeBaked`
// additionally requires a SINGLE accessor, so a struct-element MEMBER reference
// (two unpinned accessors) was skipped, stayed unbaked, and EXISTS silently
// dropped every row.
//
// Making element members resolvable in the correlated scopes is what puts a
// multi-accessor element reference within reach of paths that never saw one
// before, so the twin has to be measured rather than assumed safe.
//
// MEASURED: it does not reproduce. Every shape below answers correctly, flat and
// two-level, with and without a correlated EXISTS over the member.
//
// WHAT THIS DOES AND DOES NOT ESTABLISH. It establishes that these shapes are
// correct — nothing more. It is NOT proof that bakeGatheredGroupValue's gate is
// safe in general: the gate still carries the narrow predicate, and the reason
// these queries miss it (the references reaching it are not unpinned
// multi-accessor nodes) is a property of how they are lowered, not a guarantee
// anyone stated. If a future change routes a baked multi-accessor element
// reference into that walk, the same silent drop is available there, and the
// failure will look like these arms losing rows rather than raising an error.
// That is why the assertions are VALUES: an absence-of-error check would pass
// with the defect fully present.
func TestFDB_UnnestElementMemberInGather(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	const dbPath = "/testdb_unnest_elem_gather"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE ueg_tmpl "+
			"CREATE TYPE AS STRUCT deep (dk BIGINT) "+
			"CREATE TYPE AS STRUCT elem (ek BIGINT, d deep) "+
			"CREATE TABLE t (id BIGINT, arr elem ARRAY, PRIMARY KEY (id)) "+
			"CREATE TABLE u (uk BIGINT, PRIMARY KEY (uk))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE ueg_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.ExecContext(ctx,
		"INSERT INTO t VALUES (1, [(10, (91)), (20, (92))]), (2, [(30, (93))])"); err != nil {
		t.Fatalf("INSERT t: %v", err)
	}
	// uk matches exactly one ek (10) and exactly one dk (91), so the two
	// correlated arms below are DISCRIMINATING rather than blanket-true.
	if _, err := db.ExecContext(ctx, "INSERT INTO u VALUES (10), (91)"); err != nil {
		t.Fatalf("INSERT u: %v", err)
	}

	cases := []struct {
		name   string
		sql    string
		want   string
		rearms string
	}{
		{
			// Single-accessor control: the shape the narrow predicate admits.
			name: "group_by_flat_member",
			sql:  "SELECT x.ek, COUNT(*) FROM t, t.arr AS x GROUP BY x.ek ORDER BY x.ek",
			want: "EK,COUNT(*)|10 1;20 1;30 1",
			rearms: "the SINGLE-accessor element path through the gathered walk changed; " +
				"the multi-accessor arms below are only interpretable against it",
		},
		{
			name:   "group_by_two_level_member",
			sql:    "SELECT x.d.dk, COUNT(*) FROM t, t.arr AS x GROUP BY x.d.dk ORDER BY x.d.dk",
			want:   "DK,COUNT(*)|91 1;92 1;93 1",
			rearms: "a MULTI-ACCESSOR element member in a grouping key started losing rows",
		},
		{
			name:   "group_by_two_level_member_with_having",
			sql:    "SELECT x.d.dk, COUNT(*) FROM t, t.arr AS x GROUP BY x.d.dk HAVING COUNT(*) = 1 ORDER BY x.d.dk",
			want:   "DK,COUNT(*)|91 1;92 1;93 1",
			rearms: "a HAVING over a multi-accessor element grouping key started losing rows",
		},
		{
			// The closest analogue of the defect: a grouped unnest whose member
			// is ALSO correlated into an EXISTS.
			name:   "group_by_two_level_member_with_correlated_exists",
			sql:    "SELECT x.d.dk, COUNT(*) FROM t, t.arr AS x WHERE EXISTS (SELECT 1 FROM u WHERE u.uk = x.d.dk) GROUP BY x.d.dk ORDER BY x.d.dk",
			want:   "DK,COUNT(*)|91 1",
			rearms: "THE CLOSEST ANALOGUE OF THE SIBLING'S SILENT DROP — an empty result here is that defect arriving in the gathered walk",
		},
		{
			name:   "group_by_flat_member_with_correlated_exists",
			sql:    "SELECT x.ek, COUNT(*) FROM t, t.arr AS x WHERE EXISTS (SELECT 1 FROM u WHERE u.uk = x.ek) GROUP BY x.ek ORDER BY x.ek",
			want:   "EK,COUNT(*)|10 1",
			rearms: "the single-accessor twin of the arm above; if BOTH go empty the cause is the EXISTS, not the arity",
		},
		{
			// The member in the AGGREGATED value rather than the grouping key —
			// a different position in the same walk.
			name:   "aggregate_over_two_level_member",
			sql:    "SELECT x.ek, SUM(x.d.dk) FROM t, t.arr AS x GROUP BY x.ek ORDER BY x.ek",
			want:   "EK,SUM(X.D.DK)|10 91;20 92;30 93",
			rearms: "a multi-accessor element member in an AGGREGATED position started reading wrong",
		},
	}

	for i := range cases {
		tc := cases[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := runShape(t, ctx, db, tc.sql); got != tc.want {
				t.Fatalf("query %q\n   got: %s\n  want: %s\n  RE-ARMED IF THIS CHANGES: %s",
					tc.sql, got, tc.want, tc.rearms)
			}
		})
	}
}
