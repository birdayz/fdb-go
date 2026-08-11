package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFDB_QualifiedNestedAccessorReadsTheLeafNotTheStructRoot pins the ROWS a
// two-segment nested reference (`n.co`, where `N` is a struct column and `CO`
// one of its members) produces on every FROM shape that routes the reference
// through a DIFFERENT resolver mint.
//
// The shapes are not interchangeable, and that is the whole point of driving
// all of them: `SELECT n.co` reaches three different mints depending on nothing
// more than how many FROM items there are and whether their aliases collide.
// A single-source probe — which passes — says nothing about the other two,
// because the single-source arm is the only one that goes through
// Resolver.ResolveIdentifier, the one mint that fuses the semantic layer's
// accessor chain onto the root reference.
//
// The fixture makes the struct ROOT and the requested LEAF carry visibly
// different data, so a mint that binds the root instead of the leaf shows up as
// wrong VALUES rather than as a wrong type or a wrong label:
//
//	id=1  n=(sk=10, co=300)
//	id=2  n=(sk=20, co=200)
//
// `n.co` is 300/200. `n.sk` is 10/20. The struct root `n` is neither. No
// permutation of this data makes "read the root" and "read CO" agree, and
// reading the FIRST member instead of the named one disagrees too — which is
// what makes the assertion an assertion and not a coincidence of ordering.
func TestFDB_QualifiedNestedAccessorReadsTheLeafNotTheStructRoot(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	const dbPath = "/testdb_qual_nested_accessor"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE qna_tmpl "+
			"CREATE TYPE AS STRUCT nst (sk BIGINT, co BIGINT) "+
			"CREATE TABLE t1 (id BIGINT, n nst, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id2 BIGINT, other BIGINT, PRIMARY KEY (id2))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE qna_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// t.Cleanup, not defer: the subtests below are parallel, so they run AFTER
	// this function returns and a deferred Close hands every one of them
	// "sql: database is closed" instead of a row.
	t.Cleanup(func() { db.Close() })

	if _, err := db.ExecContext(ctx,
		"INSERT INTO t1 VALUES (1, (10, 300)), (2, (20, 200))"); err != nil {
		t.Fatalf("INSERT t1: %v", err)
	}
	// ONE t2 row, so a cross join neither multiplies nor drops rows: the row
	// COUNT is identical across all three shapes and only the VALUES can differ.
	if _, err := db.ExecContext(ctx, "INSERT INTO t2 VALUES (100, 7)"); err != nil {
		t.Fatalf("INSERT t2: %v", err)
	}

	cases := []struct {
		name string
		sql  string
		want []int64
	}{
		{
			// The CONTROL. Routed through ResolveIdentifier, which fuses the
			// accessor chain. If this arm reds, the fixture itself is wrong and
			// the other arms say nothing.
			name: "single_source",
			sql:  "SELECT n.co FROM t1 ORDER BY id",
			want: []int64{300, 200},
		},
		{
			// Distinct aliases over a join: ResolveQualifiedProjection declines
			// (unique alias bound by its own name) and the reference falls
			// through to resolveQualifiedBaked, which resolves through
			// ResolveIdentifier and therefore fuses.
			name: "join_distinct_aliases",
			sql:  "SELECT n.co FROM t1 AS a, t2 AS b ORDER BY id",
			want: []int64{300, 200},
		},
		{
			// DUPLICATE alias. ResolveQualifiedProjection does NOT decline here
			// (QualifierIsDuplicated forces the per-binding bake), so this is
			// the arm that mints the reference itself — and the mint is the one
			// place the accessor chain can be dropped on the floor.
			name: "duplicate_alias",
			sql:  "SELECT n.co FROM t1 AS a, t2 AS a ORDER BY id",
			want: []int64{300, 200},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows, err := db.QueryContext(ctx, tc.sql)
			if err != nil {
				t.Fatalf("query %q: %v", tc.sql, err)
			}
			defer rows.Close()
			var got []int64
			for rows.Next() {
				var v int64
				if err := rows.Scan(&v); err != nil {
					t.Fatalf("scan %q: %v (a scan failure here is itself the "+
						"symptom: the reference bound the STRUCT ROOT `N` "+
						"instead of the member `N.CO`)", tc.sql, err)
				}
				got = append(got, v)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows %q: %v", tc.sql, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("query %q returned %d rows %v, want %d rows %v",
					tc.sql, len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("query %q row %d: got %d, want %d (full: got %v want %v)\n"+
						"  %d is the member CO; %d/%d are the member SK — a value "+
						"that is neither means the reference read the wrong slot",
						tc.sql, i, got[i], tc.want[i], got, tc.want, tc.want[i], 10, 20)
				}
			}
		})
	}
}
