package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_QualifiedNestedAccessorShapeMatrix maps which SQL shapes route a
// two-segment nested reference (`n.co`, `N` a struct column and `CO` a member)
// through the resolver mints that build the reference THEMSELVES, as opposed to
// the ones that delegate to Resolver.ResolveIdentifier.
//
// The distinction is invisible from the SQL: `SELECT n.co` looks the same in
// every row below, and the shapes differ only in how many FROM items there are,
// whether their aliases collide, and whether the reference appears in the
// SELECT list, the ORDER BY or the GROUP BY. Each of those routes it somewhere
// else. So the matrix is the coverage claim: a probe of one shape is a
// statement about that shape only.
//
// Every arm reads the OUTPUT COLUMN NAME as well as the value. The name is the
// dimension on which this defect is SILENT — a reference that binds the struct
// root emits a column called `N` carrying the whole struct, and a client that
// scans into `any` gets that struct with no error at all. Only a client
// demanding the member's own type sees a failure, and then it sees a type
// error, which reads like a client bug rather than a wrong-column read.
//
// Fixture (same as the value test): id=1 n=(sk=10, co=300), id=2 n=(sk=20,
// co=200). Member CO is 300/200, member SK is 10/20, the root is neither.
func TestFDB_QualifiedNestedAccessorShapeMatrix(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	const dbPath = "/testdb_qual_nested_matrix"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE qnm_tmpl "+
			"CREATE TYPE AS STRUCT nst (sk BIGINT, co BIGINT) "+
			"CREATE TABLE t1 (id BIGINT, n nst, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id2 BIGINT, other BIGINT, PRIMARY KEY (id2)) "+
			"CREATE TABLE t3 (id3 BIGINT, arr nst ARRAY, PRIMARY KEY (id3))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE qnm_tmpl"); err != nil {
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
	if _, err := db.ExecContext(ctx, "INSERT INTO t2 VALUES (100, 7)"); err != nil {
		t.Fatalf("INSERT t2: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t3 VALUES (1, [(10, 300), (20, 200)])"); err != nil {
		t.Fatalf("INSERT t3: %v", err)
	}

	cases := []struct {
		name string
		sql  string
		// want is "COLNAMES|row;row". An arm whose reference binds the struct
		// ROOT shows up here as the column name `N` and a struct-shaped cell,
		// neither of which any correct answer contains.
		want string
	}{
		// --- delegating mints (ResolveIdentifier / resolveQualifiedBaked) ---
		{"single_source", "SELECT n.co FROM t1 ORDER BY id", "CO|300;200"},
		{"join_distinct_aliases", "SELECT n.co FROM t1 AS a, t2 AS b ORDER BY id", "CO|300;200"},
		{"unnest_struct_array", "SELECT e.co FROM t3, t3.arr AS e ORDER BY e.co", "CO|200;300"},
		{"derived_table", "SELECT n.co FROM (SELECT * FROM t1) AS d ORDER BY n.co", "CO|200;300"},
		{"cte", "WITH c AS (SELECT * FROM t1) SELECT n.co FROM c ORDER BY n.co", "CO|200;300"},

		// --- the self-building mints: duplicate alias forces the per-binding bake ---
		{"dup_alias_select", "SELECT n.co FROM t1 AS a, t2 AS a ORDER BY id", "CO|300;200"},
		{"dup_alias_order_by", "SELECT id, n.co FROM t1 AS a, t2 AS a ORDER BY n.co", "ID,CO|2 200;1 300"},
		// --- GROUP BY, which used to be refused outright and now mints ---
		//
		// These two arms were pinned as the LOUD decline an upstream gate spent
		// on any nested grouping key, WITH the instruction that lifting the gate
		// would fail them and the admitted reference must then be re-pointed at
		// `CO|200;300`. That is exactly what happened, and exactly what they now
		// assert — measured, not predicted. The GROUP BY qualified path
		// (upgradeAggregateOperands) is one of the callers of the mint this file
		// exists to guard, so these are the arms that put it under test.
		//
		// THE HARD FROM SHAPES ARE THE POINT. Both go through the ladder's
		// struct-descent arm over a source the SELECT arms do not exercise: a
		// DUPLICATE alias, where a childless bake would misread the other leg's
		// slot over the merged row, and a lateral unnest binding whose element is
		// a struct, where the reference's root is the unnest element rather than
		// a base table. A key that bound the struct ROOT would show here as the
		// column name `N`/`E` and a struct-shaped cell.
		{
			"dup_alias_group_by", "SELECT n.co FROM t1 AS a, t2 AS a GROUP BY n.co ORDER BY n.co",
			"CO|200;300",
		},
		{
			"unnest_group_by_element_field", "SELECT e.co FROM t3, t3.arr AS e GROUP BY e.co ORDER BY e.co",
			"CO|200;300",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := runShape(t, ctx, db, tc.sql)
			if got != tc.want {
				t.Fatalf("query %q\n   got: %s\n  want: %s\n"+
					"  (a column named N, or a cell that is a struct rather than "+
					"300/200, means the reference bound the struct ROOT instead of "+
					"the member CO)", tc.sql, got, tc.want)
			}
		})
	}
}

// runShape renders a query's column names and rows as one comparable string,
// scanning into `any` so a struct-valued cell is REPORTED rather than converted
// into a scan error. That is deliberate: scanning into int64 turns the defect
// into a type complaint, and the point of this matrix is to show what a client
// that does not demand a type silently receives.
func runShape(t *testing.T, ctx context.Context, db *sql.DB, q string) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "ERROR(columns): " + err.Error()
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "ERROR(scan): " + err.Error()
		}
		cells := make([]string, len(vals))
		for i, v := range vals {
			cells[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(cells, " "))
	}
	if err := rows.Err(); err != nil {
		return "ERROR(iterate): " + err.Error()
	}
	return strings.Join(cols, ",") + "|" + strings.Join(out, ";")
}
