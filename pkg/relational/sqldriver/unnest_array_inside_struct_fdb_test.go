package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFDB_UnnestArrayInsideStruct measures what an unnest whose array lives
// INSIDE a struct column does, end to end.
//
// The FROM item `n.arr` is two segments like `t.arr`, and the resolution that
// types its elements (unnestElementStructFields) cannot tell them apart from its
// own result: `t.arr` is a DIRECT match returning the array column, while
// `n.arr` is a NESTED match returning the struct ROOT `N` with the descent to
// `ARR` carried in the accessor chain — a chain that lookup discards. The
// IsArray test then runs against the struct, which is not an array.
//
// So the two shapes differ in exactly one place, and the row that shows it is a
// struct-element array: only then does anything depend on the element's fields
// having been carried.
func TestFDB_UnnestArrayInsideStruct(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	const dbPath = "/testdb_unnest_arr_in_struct"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE uais_tmpl "+
			"CREATE TYPE AS STRUCT leaf (sk BIGINT, co BIGINT) "+
			"CREATE TYPE AS STRUCT holder (tag BIGINT, arr leaf ARRAY) "+
			"CREATE TABLE t (id BIGINT, n holder, PRIMARY KEY (id)) "+
			"CREATE TABLE u (id2 BIGINT, arr leaf ARRAY, PRIMARY KEY (id2))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE uais_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.ExecContext(ctx,
		"INSERT INTO t VALUES (1, (99, [(10, 300), (20, 200)]))"); err != nil {
		t.Fatalf("INSERT t: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO u VALUES (1, [(10, 300), (20, 200)])"); err != nil {
		t.Fatalf("INSERT u: %v", err)
	}

	cases := []struct {
		name   string
		sql    string
		want   string
		rearms string
	}{
		{
			// THE CONTROL: the array is a top-level column, so the FROM item is a
			// DIRECT match and the element's struct fields are carried. If this
			// arm moves, the comparison below is not about nesting at all.
			name: "top_level_array_control",
			sql:  "SELECT x.co FROM u, u.arr AS x ORDER BY x.co",
			want: "CO|200;300",
			rearms: "the top-level unnest of a struct-element array changed; the " +
				"nested arm below is only interpretable against this control",
		},
		{
			// The array reached THROUGH a struct column. It never reaches the
			// unnest machinery at all: a comma source is classified as a lateral
			// unnest by resolving segment 0 against the in-scope source ALIASES,
			// and `N` is a struct column, not an alias — so the FROM item is read
			// as a database qualifier and dies here.
			//
			// This is the fact that makes unnestElementStructFields' nested arm
			// unreachable from SQL. That arm is driven directly by
			// TestUnnestElementStructFieldsTakesTheLeaf, so the two records sit
			// together: what keeps it unreachable, and what it does when reached.
			name: "array_inside_struct",
			sql:  "SELECT x.co FROM t, n.arr AS x ORDER BY x.co",
			want: "ERROR: 42F00: Unknown database N",
			rearms: "a two-segment FROM item whose first segment is a STRUCT COLUMN " +
				"is now classified as a lateral unnest instead of a database " +
				"qualifier; unnestElementStructFields' nested arm goes live and its " +
				"rows want asserting here (expect CO|200;300)",
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
