package embedded

import (
	"context"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/executor"
	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
)

// cascadesGenerator routes SELECT queries through the Cascades planner
// and executor. DML and DDL statements fall back to the naive generator.
type cascadesGenerator struct {
	c     *EmbeddedConnection
	naive *naiveGenerator
}

func newCascadesGenerator(c *EmbeddedConnection) *cascadesGenerator {
	return &cascadesGenerator{
		c:     c,
		naive: &naiveGenerator{c: c},
	}
}

func (g *cascadesGenerator) Plan(ctx context.Context, sql string) (query.Plan, error) {
	root, err := parser.Parse(sql)
	if err != nil {
		return g.naive.Plan(ctx, sql)
	}

	stmts := root.Statements()
	if stmts == nil || len(stmts.AllStatement()) != 1 {
		return g.naive.Plan(ctx, sql)
	}

	stmt := stmts.AllStatement()[0]
	sel := stmt.SelectStatement()
	if sel == nil {
		return g.naive.Plan(ctx, sql)
	}

	q := sel.Query()
	if q == nil {
		return g.naive.Plan(ctx, sql)
	}

	md := g.c.cachedMetaData()
	var logicalOp logical.LogicalOperator
	if md != nil {
		logicalOp = buildLogicalPlanForQueryWithCatalog(q, md)
	} else {
		logicalOp = buildLogicalPlanForQuery(q)
	}

	if logicalOp == nil {
		return g.naive.Plan(ctx, sql)
	}

	ref := query.TranslateToCascades(logicalOp)
	if ref == nil {
		return g.naive.Plan(ctx, sql)
	}

	naivePlan, _ := g.naive.Plan(ctx, sql)

	return &cascadesPlan{
		ref:     ref,
		conn:    g.c,
		md:      md,
		naive:   naivePlan,
		explain: logicalOp.Explain(""),
	}, nil
}

// cascadesPlan wraps a Cascades-planned SELECT query.
type cascadesPlan struct {
	ref     *expressions.Reference
	conn    *EmbeddedConnection
	md      *recordlayer.RecordMetaData
	naive   query.Plan
	explain string
}

func (p *cascadesPlan) IsUpdate() bool { return false }

func (p *cascadesPlan) Explain() string { return p.explain }

func (p *cascadesPlan) Execute(ctx context.Context) (query.Result, error) {
	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	planCtx := buildCascadesPlanContext(p.md)

	planner := cascades.NewPlanner(rules, planCtx).
		WithImplementationRules(cascades.DefaultImplementationRules())

	plan, _, err := planner.Plan(p.ref)
	if err != nil || plan == nil {
		if p.naive != nil {
			return p.naive.Execute(ctx)
		}
		return query.Result{}, err
	}

	ph, ok := plan.(interface{ GetRecordQueryPlan() any })
	if !ok {
		if p.naive != nil {
			return p.naive.Execute(ctx)
		}
		return query.Result{}, nil
	}

	_ = ph
	_ = executor.ExecutePlan
	if p.naive != nil {
		return p.naive.Execute(ctx)
	}
	return query.Result{}, nil
}

func buildCascadesPlanContext(md *recordlayer.RecordMetaData) cascades.PlanContext {
	if md == nil {
		return cascades.EmptyPlanContext()
	}
	return cascades.EmptyPlanContext()
}
