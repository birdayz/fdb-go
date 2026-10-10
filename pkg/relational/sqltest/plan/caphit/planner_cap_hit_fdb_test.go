package sqltest

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

// capHitDB creates an isolated db+schema whose ORDERS table carries the same
// four secondary indexes the planner-level cap regression uses. The index count
// matters: join enumeration explodes with the number of available access paths
// per leg, and that explosion is what exhausts the task budget.
func capHitDB(t *testing.T, tag string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	dbPath := "/FRL/capacity_" + tag
	setup := testkit.OpenDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := "capacity_tmpl_" + tag
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE ORDERS (id BIGINT, customer_id BIGINT, status STRING,"+
		" amount BIGINT, tier STRING, PRIMARY KEY (id))"+
		" CREATE INDEX idx_customer ON ORDERS(customer_id)"+
		" CREATE INDEX idx_status ON ORDERS(status)"+
		" CREATE INDEX idx_amount ON ORDERS(amount)"+
		" CREATE INDEX idx_tier ON ORDERS(tier)"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE "+tmpl); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+strings.ToUpper(dbPath)+"?cluster_file="+testkit.ClusterFile()+"&schema=MAIN")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// budgetConn pins a connection with an explicit task budget; Java's SQL layer
// sets none, so planning is unbounded unless a connection configures one.
func budgetConn(t *testing.T, db *sql.DB) *sql.Conn {
	t.Helper()
	return testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().Set(api.OptMaxTotalTaskCount, 2_000).Build())
	})
}

// sevenWayJoinExists is a WHERE-existential over a seven-way self-join. Join
// enumeration over seven legs, each with five access paths, exceeds the
// configured budget before the memo converges.
//
// It is deliberately EXISTS rather than `id IN (SELECT ...)`: the IN form fails
// DML translation before planning ever starts, which would test nothing here.
// An EXISTS predicate is translated INTO the DML's own Reference — that is what
// the DML path's CheckBuriedExistentialPredicate guard inspects — so the join is
// enumerated by the DML planner itself, not by the separate scalar-subquery
// pipeline, which is only reached after DML planning has already succeeded.
const sevenWayJoinExists = "EXISTS (SELECT 1 FROM " +
	"ORDERS a, ORDERS b, ORDERS c, ORDERS d, ORDERS e, ORDERS f, ORDERS g " +
	"WHERE a.id = b.id AND b.id = c.id AND c.id = d.id AND d.id = e.id AND e.id = f.id AND f.id = g.id)"

// TestFDB_PlannerCapHit_DMLPathSQLSTATE drives the DML planner callsite, which
// needs a live connection to reach at all: planDML falls back to explain-only
// whenever sess.DB is nil, so no DB-less test can execute it. That callsite used
// to interpolate the planner error into its own "DML Cascades planning failed"
// 0AF00 message while the SELECT callsite discarded the error entirely, so one
// planner failure produced two different diagnostics depending on statement
// kind. Both now route through the shared classifier, and this pins the DML half
// end to end against a real store, with the budget set on the connection. DELETE and
// UPDATE are both covered because they reach the planner through different
// logical builders.
func TestFDB_PlannerCapHit_DMLPathSQLSTATE(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"delete", "DELETE FROM ORDERS WHERE " + sevenWayJoinExists},
		{"update", "UPDATE ORDERS SET amount = 1 WHERE " + sevenWayJoinExists},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn := budgetConn(t, capHitDB(t, "dml_"+tc.name))
			_, err := conn.ExecContext(context.Background(), tc.sql)
			testkit.AssertPlannerCapHit(t, err)
		})
	}
}

// TestFDB_PlannerCapHit_SelectPathSQLSTATE is the same assertion for SELECT,
// through the full driver stack rather than the planner-internal entry point the
// embedded package's regression uses. It proves the classification survives
// every layer between the planner and database/sql — nothing above re-wraps the
// cap hit back into the generic unsupported-query verdict.
func TestFDB_PlannerCapHit_SelectPathSQLSTATE(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := budgetConn(t, capHitDB(t, "select"))

	rows, err := conn.QueryContext(ctx,
		"SELECT a.id FROM ORDERS a, ORDERS b, ORDERS c, ORDERS d, ORDERS e, ORDERS f, ORDERS g "+
			"WHERE a.id = b.id AND b.id = c.id AND c.id = d.id AND d.id = e.id AND e.id = f.id AND f.id = g.id")
	if rows != nil {
		rows.Close()
	}
	testkit.AssertPlannerCapHit(t, err)
}
