package sqltest

// A generated ARITHMETIC index (`CREATE INDEX i AS SELECT a + 1 FROM t`) is
// matched as Java matches it: LongArithmethicFunctionKeyExpression.toValue gives
// the column the Value `a + 1`, so an equality on the expression is a scan range
// and an ORDER BY on it is the index order, through the SQL driver.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDB_ArithmeticIndex_IsMatched(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_arithmatch")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_arithmatch")
	testkit.MustExecCtx(t, setup, ctx, `CREATE SCHEMA TEMPLATE arithmatch_tpl
		CREATE TABLE t(id BIGINT, a BIGINT, PRIMARY KEY(id))
		CREATE INDEX i_a1 AS SELECT a + 1 FROM t`)
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_arithmatch/s1 WITH TEMPLATE arithmatch_tpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_ARITHMATCH?cluster_file=%s&schema=S1", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO t VALUES (1, 4), (2, 9), (3, -2)")

	for _, tc := range []struct {
		name, q, plan string
		want          []int64
	}{
		{"equality_on_the_expression", `SELECT id FROM t WHERE a + 1 = 5`, "IndexScan(I_A1, [=]", []int64{1}},
		{"order_by_the_expression", `SELECT id FROM t ORDER BY a + 1`, "IndexScan(I_A1, [*]", []int64{3, 1, 2}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var plan string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+tc.q).Scan(&plan); err != nil {
				t.Fatalf("EXPLAIN %s: %v", tc.q, err)
			}
			if !strings.Contains(plan, tc.plan) || strings.Contains(plan, "Sort") {
				t.Fatalf("%s:\n  %s\nwant the arithmetic index %q and no sort", tc.q, plan, tc.plan)
			}
			rows, err := db.QueryContext(ctx, tc.q)
			if err != nil {
				t.Fatalf("%s: %v", tc.q, err)
			}
			defer rows.Close()
			var got []int64
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				got = append(got, id)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("%s = %v, want %v", tc.q, got, tc.want)
			}
		})
	}
}
