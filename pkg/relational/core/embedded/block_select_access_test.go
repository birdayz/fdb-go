package embedded

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/relational/core/parser"
	"fdb.dev/pkg/relational/core/query"
)

// planAsSelectBlock plans sql with its block rebuilt as Java builds it
// (LogicalOperator.generateSimpleSelect): one SelectExpression over the FROM
// source, with the WHERE as its predicates and the projection as its result.
func planAsSelectBlock(t *testing.T, sql, ddl string) string {
	t.Helper()
	tmpl, err := buildSchemaTemplateFromDDL(ddl)
	if err != nil {
		t.Fatal(err)
	}
	md := tmpl.Underlying()
	tree, err := parser.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	op, err := NewPlanVisitor(md).VisitQuery(tree.Statements().AllStatement()[0].SelectStatement().Query())
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := query.TranslateToCascadesWithError(op, md)
	if err != nil {
		t.Fatal(err)
	}
	projection, ok := root.Get().(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("root is %T, want the projection this test rebuilds", root.Get())
	}
	// The WHERE is the block's own predicate list, over the FROM source.
	inner := projection.GetInner()
	filter, ok := inner.GetRangesOver().Get().(*expressions.LogicalFilterExpression)
	if !ok || filter.GetInner().GetAlias() != inner.GetAlias() {
		t.Fatalf("projection input is %T, want a WHERE over the same source", inner.GetRangesOver().Get())
	}
	block, err := expressions.NewSelectExpression(projection.GetResultValue(), filter.GetQuantifiers(), filter.GetPredicates())
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := planReferenceToPhysical(expressions.InitialOf(block), md, nil,
		cascades.BatchAExpressionRules(), false, nil, plannerOptionsFrom(nil))
	if err != nil {
		t.Fatal(err)
	}
	return plan.Explain()
}

// A query block that is one Select matches the index as a whole, so its
// data-access compensation is a Select carrying the result value. That has to
// be explored and implemented like any other compensation (Java
// yieldUnknownExpression), not parked as a final.
func TestSelectBlockCompensationReachesTheIndex(t *testing.T) {
	t.Parallel()
	got := planAsSelectBlock(t, "SELECT id FROM orders WHERE amount BETWEEN 100 AND 200", ordersSchema)
	assertPlanContains(t, got, "IndexScan(IDX_AMOUNT")
}

// A projected EXISTS reads its existential quantifier from the block's result
// value. A match over the FROM source alone leaves that quantifier unmatched,
// and the result compensation, rebuilt over the realized scan, cannot read it:
// such a match is impossible, so the existential leg stays in the plan.
func TestResultReadingAnUnmatchedQuantifierIsNotCompensated(t *testing.T) {
	t.Parallel()
	got, err := PlanQueryForTest(
		`SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h FROM t1 ORDER BY id`,
		derivedExistsSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertPlanContains(t, got, "FirstOrDefault")
}
