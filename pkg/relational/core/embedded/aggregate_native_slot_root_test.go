package embedded

import (
	"reflect"
	"testing"

	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

const aggregateNativeContractDDL = `
	CREATE TABLE t1 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id))
	CREATE TABLE t2 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id))
`

func TestAggregateNativeContract_GroupedUnionRetainsExactOuterLayout(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(aggregateNativeContractDDL)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	query := parseQuery(t, `SELECT SUM(y) AS s FROM (
		SELECT COUNT(*) AS y FROM t1 GROUP BY col1
		UNION ALL
		SELECT COUNT(*) AS y FROM t2 GROUP BY col1
	) AS x`)
	body, ok := query.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		t.Fatalf("outer body = %T, want QueryTermDefault", query.QueryExpressionBody())
	}
	simple, ok := body.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		t.Fatalf("outer term = %T, want SimpleTable", body.QueryTerm())
	}
	sq, err := extractFromSimpleTable(simple)
	if err != nil {
		t.Fatalf("extract outer SELECT: %v", err)
	}
	unionBody, ok := sq.derivedQuery.QueryExpressionBody().(*antlrgen.SetQueryContext)
	if !ok {
		t.Fatalf("derived body = %T, want SetQuery", sq.derivedQuery.QueryExpressionBody())
	}
	leftBody, ok := unionBody.GetLeft().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		t.Fatalf("left UNION body = %T, want QueryTermDefault", unionBody.GetLeft())
	}
	leftSimple, ok := leftBody.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		t.Fatalf("left UNION term = %T, want SimpleTable", leftBody.QueryTerm())
	}
	leftSQ, err := extractFromSimpleTable(leftSimple)
	if err != nil {
		t.Fatalf("extract left UNION leg: %v", err)
	}
	leftOp := buildLogicalPlanForSelect(leftSQ)
	leftProject := findProjection(leftOp)
	leftAggregate := findAggregate(leftOp)
	if leftProject == nil || leftAggregate == nil {
		t.Fatalf("left tree lacks project/aggregate: project=%T aggregate=%T tree=%s", leftProject, leftAggregate, leftOp.Explain(""))
	}
	if len(leftProject.AggregateOutputOrdinals) != len(leftProject.Projections) {
		t.Fatalf("left project layout: projections=%v ordinals=%v slots=%v post=%d tree=%s",
			leftProject.Projections, leftProject.AggregateOutputOrdinals, leftAggregate.OutputSlots,
			len(leftSQ.postAggExprs), leftOp.Explain(""))
	}

	inner, err := buildLogicalPlanForQueryBodyWithCTECatalog(
		sq.derivedQuery.QueryExpressionBody(), tmpl.Underlying(), defaultEmbeddedSchema, nil, nil)
	if err != nil {
		t.Fatalf("build grouped UNION: %v", err)
	}
	op := buildOuterPlanOnDerived(sq, inner)
	project := findProjection(op)
	aggregate := findAggregate(op)
	if project == nil || aggregate == nil {
		t.Fatalf("outer tree lacks project/aggregate: project=%T aggregate=%T tree=%s", project, aggregate, op.Explain(""))
	}
	if len(project.AggregateOutputOrdinals) != len(project.Projections) {
		t.Fatalf("outer project layout: projections=%v ordinals=%v slots=%v tree=%s",
			project.Projections, project.AggregateOutputOrdinals, aggregate.OutputSlots, op.Explain(""))
	}
	if len(project.AggregateOutputOrdinals) != 1 || project.AggregateOutputOrdinals[0] != len(aggregate.GroupKeys) {
		t.Fatalf("outer SUM slot = %v, want native call ordinal %d", project.AggregateOutputOrdinals, len(aggregate.GroupKeys))
	}
	if _, _, err := planWithOptions(t, `SELECT SUM(y) AS s FROM (
		SELECT COUNT(*) AS y FROM t1 GROUP BY col1
		UNION ALL
		SELECT COUNT(*) AS y FROM t2 GROUP BY col1
	) AS x`, aggregateNativeContractDDL, nil); err != nil {
		t.Fatalf("physical planning: %v", err)
	}
}

func TestAggregateNativeContract_GroupAliasRewriteMovesStructuralSegments(t *testing.T) {
	t.Parallel()
	query := parseQuery(t, `SELECT MAX(z) FROM (SELECT col1 FROM t1) AS x GROUP BY x.col1 AS z ORDER BY z`)
	body, ok := query.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		t.Fatalf("body = %T, want QueryTermDefault", query.QueryExpressionBody())
	}
	simple, ok := body.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		t.Fatalf("term = %T, want SimpleTable", body.QueryTerm())
	}
	sq, err := extractFromSimpleTable(simple)
	if err != nil {
		t.Fatalf("extract SELECT: %v", err)
	}
	if len(sq.aggCols) != 1 || len(sq.groupBy) != 1 || len(sq.orderBy) != 1 {
		t.Fatalf("classification: agg=%d group=%d order=%d", len(sq.aggCols), len(sq.groupBy), len(sq.orderBy))
	}
	wantSegments := []string{"X", "COL1"}
	if !reflect.DeepEqual(sq.aggCols[0].aggArgSegs, wantSegments) {
		t.Fatalf("aggregate alias segments = %v, want %v", sq.aggCols[0].aggArgSegs, wantSegments)
	}
	if !reflect.DeepEqual(sq.orderBy[0].segs, wantSegments) {
		t.Fatalf("ORDER BY alias segments = %v, want %v", sq.orderBy[0].segs, wantSegments)
	}

	tmpl, err := buildSchemaTemplateFromDDL(aggregateNativeContractDDL)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	op, err := NewPlanVisitor(tmpl.Underlying()).VisitQuery(query)
	if err != nil {
		t.Fatalf("logical build: %v", err)
	}
	aggregate := findAggregate(op)
	if aggregate == nil || len(aggregate.AggregateOperands) != 1 || aggregate.AggregateOperands[0] == nil {
		t.Fatalf("aggregate operand was not exact after alias rewrite: %#v", aggregate)
	}
	if _, _, err := planWithOptions(t,
		`SELECT MAX(z) FROM (SELECT col1 FROM t1) AS x GROUP BY x.col1 AS z ORDER BY z`,
		aggregateNativeContractDDL, nil); err != nil {
		t.Fatalf("physical planning: %v", err)
	}
}
