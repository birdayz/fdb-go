package sqldriver_test

// Every qualifier/alias spelling of a group-key reference under a COMPUTED
// SELECT item must bind to the right group-key slot.
//
// WHAT THIS DOES NOT COVER, stated up front so nobody mistakes it for coverage
// it lacks: these cases do NOT exercise
// embedded.fieldValueMatchesAggregateGroupKey. Hard-wiring that function to
// `return false` leaves every case below green (and leaves the whole
// //pkg/relational/sqldriver suite green). Its childless-vs-qualified
// relaxation is simply not reached by these shapes —
// values.SemanticEqualsUnderAliasMap, which the caller ORs in first, already
// matches them, because the resolver mints both sides with the same child
// shape. The accessor-name comparison inside that function is therefore not
// probed here at all; the unit-level analysis of it lives in
// pkg/relational/core/embedded/aggregate_group_key_accessor_name_test.go.
//
// What this DOES pin is the resolver's qualifier/alias binding on the
// post-aggregate path: a computed SELECT item (`col + 1`, which a bare
// `SELECT col` would bypass) over a group key spelled bare, qualified,
// table-aliased, or renamed through a derived table or CTE. The swapped-alias
// cases are the sharp ones — `(SELECT b AS a, a AS b FROM t) AS z` makes `z.a`
// really `t.b`, so a name-driven bind picks the wrong column and the group
// count changes rather than erroring.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_AggregateGroupKeyQualifierAndAliasBinding(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_aggkname")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_aggkname")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE aggkname "+
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_aggkname/s WITH TEMPLATE aggkname")
	dsn := fmt.Sprintf("fdbsql:///testdb_aggkname?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// a has 2 distinct values (10, 11); b has 3 (20, 30, 40); id has 3.
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b) VALUES (1, 10, 20), (2, 10, 30), (3, 11, 40)")

	cases := []struct {
		query string
		// wantGroups is the distinct count of the grouped column. A wrong
		// binding would either error or group by the wrong column and produce
		// a different count.
		wantGroups int
	}{
		// Qualifier asymmetry between the SELECT reference and the GROUP BY
		// key: exactly the childless-vs-QOV shape the matcher exists for.
		{"SELECT a + 1, COUNT(*) FROM t GROUP BY a", 2},
		{"SELECT a + 1, COUNT(*) FROM t GROUP BY t.a", 2},
		{"SELECT t.a + 1, COUNT(*) FROM t GROUP BY a", 2},
		{"SELECT t.a + 1, COUNT(*) FROM t GROUP BY t.a", 2},
		{"SELECT b + 1, COUNT(*) FROM t GROUP BY t.b", 3},
		{"SELECT t.b + 1, COUNT(*) FROM t GROUP BY b", 3},
		// Table alias on one side only.
		{"SELECT x.a + 1, COUNT(*) FROM t AS x GROUP BY a", 2},
		{"SELECT a + 1, COUNT(*) FROM t AS x GROUP BY x.a", 2},

		// Derived tables: the projection renames the column, so the accessor
		// name downstream is the ALIAS, not the base column name.
		{"SELECT z.q + 1, COUNT(*) FROM (SELECT a AS q, b FROM t) AS z GROUP BY z.q", 2},
		{"SELECT q + 1, COUNT(*) FROM (SELECT a AS q, b FROM t) AS z GROUP BY z.q", 2},
		{"SELECT z.q + 1, COUNT(*) FROM (SELECT a AS q, b FROM t) AS z GROUP BY q", 2},
		// Reordered projection: z.b sits at a different ordinal than t.b.
		{"SELECT z.b + 1, COUNT(*) FROM (SELECT b, a FROM t) AS z GROUP BY z.b", 3},
		{"SELECT b + 1, COUNT(*) FROM (SELECT b, a FROM t) AS z GROUP BY z.b", 3},
		// SWAPPED aliases: z.a is really t.b. A name-driven bind would pick
		// t.a and return 2 groups instead of 3.
		{"SELECT z.a + 1, COUNT(*) FROM (SELECT b AS a, a AS b FROM t) AS z GROUP BY a", 3},
		{"SELECT a + 1, COUNT(*) FROM (SELECT b AS a, a AS b FROM t) AS z GROUP BY z.a", 3},
		// Same, aliased to the primary key: 3 groups, not 2.
		{"SELECT q + 1, COUNT(*) FROM (SELECT id AS q, a FROM t) AS z GROUP BY z.q", 3},

		// CTEs, same renames through a different binding path.
		{"WITH z AS (SELECT a AS q, b FROM t) SELECT z.q + 1, COUNT(*) FROM z GROUP BY q", 2},
		{"WITH z AS (SELECT a AS q, b FROM t) SELECT q + 1, COUNT(*) FROM z GROUP BY z.q", 2},
		{"WITH z AS (SELECT b AS a, a AS b FROM t) SELECT z.a + 1, COUNT(*) FROM z GROUP BY a", 3},
	}

	for _, tc := range cases {
		rows, err := db.QueryContext(ctx, tc.query)
		if err != nil {
			t.Errorf("%s\n  bind FAILED: %v\n  a group-key reference under a computed SELECT item "+
				"no longer binds to its native slot", tc.query, err)
			continue
		}
		n := 0
		for rows.Next() {
			n++
		}
		rerr := rows.Err()
		rows.Close()
		if rerr != nil {
			t.Errorf("%s: rows.Err: %v", tc.query, rerr)
			continue
		}
		if n != tc.wantGroups {
			t.Errorf("%s: got %d groups, want %d — the post-aggregate reference bound to the "+
				"WRONG group-key slot", tc.query, n, tc.wantGroups)
		}
	}
}
