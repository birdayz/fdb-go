package sqldriver_test

// A generated ARITHMETIC index (`CREATE INDEX i AS SELECT a + 1 FROM t`) is
// created correctly and maintained correctly, but the planner cannot MATCH it.
// The flat candidate bridge in the Cascades data-access layer accepts only
// CARDINALITY and the order-function wrappers under a Function key expression;
// every other function — including the arithmetic lowerings the index generator
// emits — declines, so metadataSufficientForPlanning excludes the candidate
// entirely and no query can ever reach the index.
//
// Java does not have this limitation: LongArithmethicFunctionKeyExpression
// implements QueryableKeyExpression and supplies toValue(argumentValues)
// (LongArithmethicFunctionKeyExpression.java:122-127), which is exactly what
// ValueIndexExpansionVisitor needs to expand the column into a matchable Value.
// The Go bridge has no equivalent arm. This is a REAL parity gap, not a design
// choice.
//
// It is pinned rather than fixed because closing it means teaching the Cascades
// matching infrastructure a new column shape, which is a query-engine change and
// carries its own design and review requirements. This test exists so the gap
// cannot rot into invisibility: when the bridge learns
// arithmetic columns, this test FAILS, and that failure is the signal to replace
// it with the EXPLAIN pins that prove the optimization fires.
//
// Nothing anywhere claims the optimization works today — the AS-SELECT
// arithmetic tests assert METADATA goldens (the emitted key expression) only,
// which remain correct and are unaffected.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_ArithmeticIndex_IsNotYetMatchable(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_arithmatch")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_arithmatch")
	mwjoMustExec(t, setup, ctx, `CREATE SCHEMA TEMPLATE arithmatch_tpl
		CREATE TABLE t(id BIGINT, a BIGINT, PRIMARY KEY(id))
		CREATE INDEX i_a1 AS SELECT a + 1 FROM t`)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_arithmatch/s1 WITH TEMPLATE arithmatch_tpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_arithmatch?cluster_file=%s&schema=s1", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (1, 4), (2, 9)")

	for _, tc := range []struct{ name, q string }{
		{"equality_on_the_expression", `SELECT id FROM t WHERE a + 1 = 5`},
		{"order_by_the_expression", `SELECT id FROM t ORDER BY a + 1`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var plan string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+tc.q).Scan(&plan); err != nil {
				t.Fatalf("EXPLAIN %s: %v", tc.q, err)
			}
			if strings.Contains(plan, "I_A1") {
				t.Fatalf("%s now uses the arithmetic index:\n  %s\n"+
					"The bridge has learned arithmetic columns — REPLACE this "+
					"limitation pin with EXPLAIN assertions that prove the "+
					"optimization fires, and delete the booked gap.", tc.q, plan)
			}
			if !strings.Contains(plan, "Scan(T)") {
				t.Fatalf("%s no longer falls back to a table scan:\n  %s\n"+
					"the plan changed shape; re-derive what this pin should say", tc.q, plan)
			}
		})
	}

	// The index is still MAINTAINED correctly — the gap is matching, not
	// storage. A query that reads the rows must still answer correctly.
	var id int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM t WHERE a + 1 = 5`).Scan(&id); err != nil {
		t.Fatalf("the fallback plan must still answer correctly: %v", err)
	}
	if id != 1 {
		t.Fatalf("got id=%d, want 1", id)
	}
}
