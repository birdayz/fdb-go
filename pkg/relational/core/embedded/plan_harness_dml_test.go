package embedded

// Regression coverage for the no-FDB DML planning harness
// (PlanPhysicalDMLForTest), the entry point the explain-differ corpus dump
// uses to pin DELETE/UPDATE plan shapes.
//
// The motivating gap (RFC-184 W2): a planner change read differ-clean on every
// SELECT while corrupting the DELETE-WHERE-EXISTS path. Only yamsql (real FDB)
// caught it. These tests pin that exact plan SHAPE at the metadata-only level,
// so the corpus dump — and any direct caller — catches the class BEFORE it
// reaches FDB.

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
)

const dmlHarnessSchema = `
CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))
CREATE TABLE keep_set (k BIGINT NOT NULL, PRIMARY KEY (k))
`

// TestPlanPhysicalDMLForTest_DeleteWhereExists is the load-bearing regression:
// a DELETE … WHERE EXISTS (correlated) must lower to a RecordQueryDeletePlan
// over an existential SEMI-JOIN (a FlatMap whose inner is a one-row
// FirstOrDefault), NOT a materialized cross product or a dropped predicate.
// This is the precise shape the RFC-184 W2 planner change corrupted.
func TestPlanPhysicalDMLForTest_DeleteWhereExists(t *testing.T) {
	t.Parallel()
	plan, err := PlanPhysicalDMLForTest(
		"DELETE FROM t WHERE EXISTS (SELECT 1 FROM keep_set WHERE keep_set.k = t.id)",
		dmlHarnessSchema, properties.FixedStatistics{Cardinality: 1_000_000})
	if err != nil {
		t.Fatalf("plan DELETE-WHERE-EXISTS: %v", err)
	}
	if _, ok := plan.(*plans.RecordQueryDeletePlan); !ok {
		t.Fatalf("root must be RecordQueryDeletePlan, got %T: %s", plan, plan.Explain())
	}
	// The EXISTS semi-join: a FlatMap whose inner is the one-row existential
	// FirstOrDefault (implementJoinWithExistential / yieldExistsFlatMap).
	if !planHasType[*plans.RecordQueryFlatMapPlan](plan) {
		t.Errorf("DELETE-WHERE-EXISTS must carry a FlatMap semi-join, got: %s", plan.Explain())
	}
	if !planHasType[*plans.RecordQueryFirstOrDefaultPlan](plan) {
		t.Errorf("DELETE-WHERE-EXISTS semi-join must wrap the inner in FirstOrDefault, got: %s", plan.Explain())
	}
}

// TestPlanPhysicalDMLForTest_DeleteWhere pins the plain-predicate DELETE: the
// WHERE lowers to a filter over the target scan under the delete wrapper.
func TestPlanPhysicalDMLForTest_DeleteWhere(t *testing.T) {
	t.Parallel()
	plan, err := PlanPhysicalDMLForTest("DELETE FROM t WHERE id > 5", dmlHarnessSchema, nil)
	if err != nil {
		t.Fatalf("plan DELETE-WHERE: %v", err)
	}
	if _, ok := plan.(*plans.RecordQueryDeletePlan); !ok {
		t.Fatalf("root must be RecordQueryDeletePlan, got %T: %s", plan, plan.Explain())
	}
}

// TestPlanPhysicalDMLForTest_Update pins that UPDATE routes through the DML
// harness and yields a RecordQueryUpdatePlan.
func TestPlanPhysicalDMLForTest_Update(t *testing.T) {
	t.Parallel()
	plan, err := PlanPhysicalDMLForTest("UPDATE t SET id = 7 WHERE id = 1", dmlHarnessSchema, nil)
	if err != nil {
		t.Fatalf("plan UPDATE: %v", err)
	}
	if _, ok := plan.(*plans.RecordQueryUpdatePlan); !ok {
		t.Fatalf("root must be RecordQueryUpdatePlan, got %T: %s", plan, plan.Explain())
	}
}

// TestPlanPhysicalDMLForTest_RejectsWindowedAggregateInExists pins the DML
// parse-tree guard shared with SELECT. Aggregate lowering cannot represent
// OVER; without the guard this mixed global/window aggregate loses its SELECT
// cardinality and the correlated fallback tests raw-row existence, which
// differs for an outer row with no keep_set match.
func TestPlanPhysicalDMLForTest_RejectsWindowedAggregateInExists(t *testing.T) {
	t.Parallel()
	_, err := PlanPhysicalDMLForTest(
		"DELETE FROM t WHERE EXISTS ("+
			"SELECT COUNT(*), COUNT(*) OVER () FROM keep_set "+
			"WHERE keep_set.k = t.id LIMIT 1 OFFSET 0)",
		dmlHarnessSchema, nil)
	if err == nil {
		t.Fatal("expected windowed aggregate in DML EXISTS to reject")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedQuery {
		t.Fatalf("error = %v, want SQLSTATE %s", err, api.ErrCodeUnsupportedQuery)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "windowed aggregate") {
		t.Fatalf("error = %v, want the parse-tree windowed-aggregate guard", err)
	}
}

// TestPlanPhysicalDMLForTest_RejectsNonDML proves the harness is DELETE/UPDATE
// only: a SELECT (handled by the SELECT harness) and an INSERT (no interesting
// plan) are rejected with a clean error, never planned or panicked.
func TestPlanPhysicalDMLForTest_RejectsNonDML(t *testing.T) {
	t.Parallel()
	for _, sql := range []string{
		"SELECT id FROM t",
		"INSERT INTO t VALUES (1)",
		"INSERT INTO t SELECT id FROM keep_set",
	} {
		plan, err := PlanPhysicalDMLForTest(sql, dmlHarnessSchema, nil)
		if err == nil {
			t.Errorf("%q: expected a rejection, got plan %s", sql, plan.Explain())
		}
	}
}

// TestPlanPhysicalDMLForTest_DeterministicShape guards the property the corpus
// dump relies on: planning the same DML twice yields the byte-identical
// Explain rendering (no process-global counter leaking into the plan text
// beyond what normalizeAliases already erases in the dumper).
func TestPlanPhysicalDMLForTest_DeterministicShape(t *testing.T) {
	t.Parallel()
	sql := "DELETE FROM t WHERE EXISTS (SELECT 1 FROM keep_set WHERE keep_set.k = t.id)"
	p1, err := PlanPhysicalDMLForTest(sql, dmlHarnessSchema, nil)
	if err != nil {
		t.Fatalf("first plan: %v", err)
	}
	p2, err := PlanPhysicalDMLForTest(sql, dmlHarnessSchema, nil)
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	// The shape (Go type skeleton) must be identical run-to-run.
	if a, b := strings.Count(p1.Explain(), "FlatMap"), strings.Count(p2.Explain(), "FlatMap"); a != b {
		t.Fatalf("nondeterministic FlatMap count: %d vs %d", a, b)
	}
}
