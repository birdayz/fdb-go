package embedded

import (
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query"
)

// PlanQueryForTest runs the full Cascades pipeline on a SQL query against
// a schema defined by DDL, with optional table statistics. Returns the
// physical plan's Explain string. No FDB connection needed.
//
// schemaDDL is a CREATE SCHEMA TEMPLATE statement (or just the table/index
// definitions — "CREATE SCHEMA TEMPLATE auto_template" is prepended if
// missing).
//
// stats may be nil to use default statistics (LeafScanCardinality for all
// record types).
func PlanQueryForTest(sql, schemaDDL string, stats properties.StatisticsProvider) (string, error) {
	tmpl, err := buildSchemaTemplateFromDDL(schemaDDL)
	if err != nil {
		return "", fmt.Errorf("schema DDL: %w", err)
	}
	md := tmpl.Underlying()

	root, err := parser.Parse(sql)
	if err != nil {
		return "", fmt.Errorf("parse SQL: %w", err)
	}
	stmts := root.Statements()
	if stmts == nil || len(stmts.AllStatement()) == 0 {
		return "", fmt.Errorf("no statements in SQL")
	}
	sel := stmts.AllStatement()[0].SelectStatement()
	if sel == nil {
		return "", fmt.Errorf("not a SELECT statement")
	}
	q := sel.Query()
	if q == nil {
		return "", fmt.Errorf("malformed SELECT")
	}

	visitor := NewPlanVisitor(md)
	logicalOp, buildErr := visitor.VisitQuery(q)
	if buildErr != nil {
		return "", buildErr
	}
	if logicalOp == nil {
		return "", api.NewError(api.ErrCodeUnsupportedQuery, "could not build logical plan")
	}
	if fn := query.FindUnsupportedFunction(logicalOp); fn != "" {
		return "", api.NewError(api.ErrCodeUndefinedFunction, "unsupported: "+fn)
	}
	if err := resolveQualifiedTableNames(logicalOp, "s"); err != nil {
		return "", err
	}
	if err := validateTablesAndColumns(logicalOp, md); err != nil {
		return "", err
	}
	if msg := findDistinctAggregate(logicalOp); msg != "" {
		return "", api.NewError(api.ErrCodeUnsupportedOperation, msg)
	}

	ref, _ := query.TranslateToCascadesWithSubqueries(logicalOp)
	if ref == nil {
		return "", api.NewError(api.ErrCodeUnsupportedQuery, "Cascades translation failed")
	}

	rules := cascades.DefaultExpressionRules()
	rules = append(rules, cascades.RewritingRules()...)
	planCtx := buildCascadesPlanContext(md)
	planner := cascades.NewPlanner(rules, planCtx).
		WithImplementationRules(cascades.DefaultImplementationRules()).
		WithPlanningExpressionRules(cascades.BatchAExpressionRules()).
		WithStatistics(stats).
		WithMaxTasks(100_000)

	bestExpr, _, planErr := planner.Plan(ref)
	if planErr != nil {
		return "", fmt.Errorf("planning failed: %w", planErr)
	}
	if bestExpr == nil {
		return "", api.NewError(api.ErrCodeUnsupportedQuery, "no plan found")
	}

	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	ph, ok := bestExpr.(planExtractor)
	if !ok {
		return "", fmt.Errorf("best expression is not a physical plan: %T", bestExpr)
	}
	physPlan := ph.GetRecordQueryPlan()
	if physPlan == nil {
		return "", fmt.Errorf("physical plan is nil")
	}
	return physPlan.Explain(), nil
}

// PlanQueryWithMetadata is like PlanQueryForTest but accepts pre-built
// RecordMetaData instead of DDL. Used for testing features that require
// index types not expressible in DDL (aggregate indexes).
func PlanQueryWithMetadata(sql string, md *recordlayer.RecordMetaData, stats properties.StatisticsProvider) (string, error) {
	root, err := parser.Parse(sql)
	if err != nil {
		return "", fmt.Errorf("parse SQL: %w", err)
	}
	stmts := root.Statements()
	if stmts == nil || len(stmts.AllStatement()) == 0 {
		return "", fmt.Errorf("no statements in SQL")
	}
	sel := stmts.AllStatement()[0].SelectStatement()
	if sel == nil {
		return "", fmt.Errorf("not a SELECT statement")
	}
	q := sel.Query()
	if q == nil {
		return "", fmt.Errorf("malformed SELECT")
	}

	visitor := NewPlanVisitor(md)
	logicalOp, buildErr := visitor.VisitQuery(q)
	if buildErr != nil {
		return "", buildErr
	}
	if logicalOp == nil {
		return "", api.NewError(api.ErrCodeUnsupportedQuery, "could not build logical plan")
	}
	if fn := query.FindUnsupportedFunction(logicalOp); fn != "" {
		return "", api.NewError(api.ErrCodeUndefinedFunction, "unsupported: "+fn)
	}
	if err := resolveQualifiedTableNames(logicalOp, "s"); err != nil {
		return "", err
	}
	if err := validateTablesAndColumns(logicalOp, md); err != nil {
		return "", err
	}
	if msg := findDistinctAggregate(logicalOp); msg != "" {
		return "", api.NewError(api.ErrCodeUnsupportedOperation, msg)
	}

	ref, _ := query.TranslateToCascadesWithSubqueries(logicalOp)
	if ref == nil {
		return "", api.NewError(api.ErrCodeUnsupportedQuery, "Cascades translation failed")
	}

	rules := cascades.DefaultExpressionRules()
	rules = append(rules, cascades.RewritingRules()...)
	planCtx := buildCascadesPlanContext(md)
	planner := cascades.NewPlanner(rules, planCtx).
		WithImplementationRules(cascades.DefaultImplementationRules()).
		WithPlanningExpressionRules(cascades.BatchAExpressionRules()).
		WithStatistics(stats).
		WithMaxTasks(100_000)

	bestExpr, _, planErr := planner.Plan(ref)
	if planErr != nil {
		return "", fmt.Errorf("planning failed: %w", planErr)
	}
	if bestExpr == nil {
		return "", api.NewError(api.ErrCodeUnsupportedQuery, "no plan found")
	}

	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	ph, ok := bestExpr.(planExtractor)
	if !ok {
		return "", fmt.Errorf("best expression is not a physical plan: %T", bestExpr)
	}
	physPlan := ph.GetRecordQueryPlan()
	if physPlan == nil {
		return "", fmt.Errorf("physical plan is nil")
	}
	return physPlan.Explain(), nil
}
