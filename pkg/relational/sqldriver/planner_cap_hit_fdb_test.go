package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/relational/api"
)

// capHitDB creates an isolated db+schema whose ORDERS table carries the same
// four secondary indexes the planner-level cap regression uses. The index count
// matters: join enumeration explodes with the number of available access paths
// per leg, and that explosion is what exhausts the task budget.
func capHitDB(t *testing.T, tag string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	dbPath := "/capacity_" + tag
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := "capacity_tmpl_" + tag
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE ORDERS (id BIGINT NOT NULL, customer_id BIGINT, status STRING,"+
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
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// assertPlannerCapHit pins the properties that distinguish an exhausted
// planning budget from every other planner failure: the cap sentinel survives
// as a cause, the SQLSTATE is the class-54 program-limit code (Go-only — Java's
// SQL layer never enables these caps, so there is no Java code to port), the
// user-visible message is an actionable "too complex" verdict rather than
// either "could not plan query" or the internal sentinel's wording, and the
// budget numbers reach the caller.
func assertPlannerCapHit(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("planning converged within the task cap; this statement no longer exercises " +
			"the cap — widen the join rather than deleting this test")
	}
	if !errors.Is(err, cascades.ErrPlannerCapHit) {
		t.Fatalf("planning failed for a different reason than the task cap: %v", err)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *api.Error, got %T (%v)", err, err)
	}
	if apiErr.Code != api.ErrCodePlanComplexityLimitReached {
		t.Fatalf("code = %q, want %q", apiErr.Code, api.ErrCodePlanComplexityLimitReached)
	}
	// The user gets an actionable verdict, not the internal sentinel's wording
	// (which names a planner config field). The sentinel stays in the cause,
	// which is what the errors.Is check above rides on.
	if !strings.Contains(apiErr.Message, "too complex to plan") {
		t.Fatalf("message = %q, want the user-facing budget verdict", apiErr.Message)
	}
	if strings.Contains(apiErr.Message, "MaxTasks") {
		t.Fatalf("message leaks the planner config field name: %q", apiErr.Message)
	}
	if _, ok := apiErr.Context["max_task_count"]; !ok {
		t.Fatalf("budget context did not survive to the driver: %v", apiErr.Context)
	}
	if _, ok := apiErr.Context["task_count"]; !ok {
		t.Fatalf("budget context did not survive to the driver: %v", apiErr.Context)
	}
}

// sixWayJoinExists is a WHERE-existential over a six-way self-join. Join
// enumeration over six legs, each with five access paths, exceeds the
// 100,000-task budget long before the memo converges; four legs still plan
// fine, so this is the cap tripping rather than an unplannable shape.
//
// It is deliberately EXISTS rather than `id IN (SELECT ...)`: the IN form fails
// DML translation before planning ever starts, which would test nothing here.
// An EXISTS predicate is translated INTO the DML's own Reference — that is what
// the DML path's CheckBuriedExistentialPredicate guard inspects — so the join is
// enumerated by the DML planner itself, not by the separate scalar-subquery
// pipeline, which is only reached after DML planning has already succeeded.
const sixWayJoinExists = "EXISTS (SELECT 1 FROM " +
	"ORDERS a, ORDERS b, ORDERS c, ORDERS d, ORDERS e, ORDERS f " +
	"WHERE a.id = b.id AND b.id = c.id AND c.id = d.id AND d.id = e.id AND e.id = f.id)"

// TestFDB_PlannerCapHit_DMLPathSQLSTATE drives the DML planner callsite, which
// needs a live connection to reach at all: planDML falls back to explain-only
// whenever sess.DB is nil, so no DB-less test can execute it. That callsite used
// to interpolate the planner error into its own "DML Cascades planning failed"
// 0AF00 message while the SELECT callsite discarded the error entirely, so one
// planner failure produced two different diagnostics depending on statement
// kind. Both now route through the shared classifier, and this pins the DML half
// end to end against a real store, with no injected cap and no seam. DELETE and
// UPDATE are both covered because they reach the planner through different
// logical builders.
func TestFDB_PlannerCapHit_DMLPathSQLSTATE(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"delete", "DELETE FROM ORDERS WHERE " + sixWayJoinExists},
		{"update", "UPDATE ORDERS SET amount = 1 WHERE " + sixWayJoinExists},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := capHitDB(t, "dml_"+tc.name)
			_, err := db.ExecContext(context.Background(), tc.sql)
			assertPlannerCapHit(t, err)
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
	db := capHitDB(t, "select")

	rows, err := db.QueryContext(ctx,
		"SELECT a.id FROM ORDERS a, ORDERS b, ORDERS c, ORDERS d, ORDERS e, ORDERS f "+
			"WHERE a.id = b.id AND b.id = c.id AND c.id = d.id AND d.id = e.id AND e.id = f.id")
	if rows != nil {
		rows.Close()
	}
	assertPlannerCapHit(t, err)
}
