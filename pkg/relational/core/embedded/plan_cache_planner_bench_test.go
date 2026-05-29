package embedded

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query"
)

// BenchmarkPlannerPlanVsCache isolates exactly what the plan cache buys: the
// cost of the Cascades planning pipeline (logical build → translate → memo
// search → physical extraction) versus a warm cache hit, with NO FDB and NO
// query execution. The SQL is parsed once, up front, because parsing happens
// on both the hit and the miss path in production (the cache is keyed on the
// already-parsed query's text) — so the honest "what the cache saves" delta is
// planning-minus-lookup, with parsing excluded from both.
//
//	plan_uncached: full Cascades pipeline every iteration (cache miss path)
//	plan_cached:   normalize + map lookup + MoveToBack (cache hit path)
//
// Run:
//
//	go test ./pkg/relational/core/embedded/ -run '^$' \
//	    -bench 'BenchmarkPlannerPlanVsCache' -benchmem
func BenchmarkPlannerPlanVsCache(b *testing.B) {
	const ordersSchema = `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  customer_id BIGINT,
  status STRING,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX idx_customer ON ORDERS(customer_id)
CREATE INDEX idx_status ON ORDERS(status)
CREATE INDEX idx_amount ON ORDERS(amount)
`
	const joinSchema = `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  customer_id BIGINT,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX idx_customer ON ORDERS(customer_id)
CREATE TABLE CUSTOMERS (
  id BIGINT NOT NULL,
  name STRING,
  PRIMARY KEY (id)
)
`
	scenarios := []struct {
		name   string
		schema string
		sql    string
	}{
		{"point_lookup", ordersSchema, "SELECT id, amount FROM orders WHERE id = 1"},
		{"index_equality", ordersSchema, "SELECT id, amount FROM orders WHERE customer_id = 42"},
		{"index_range", ordersSchema, "SELECT id FROM orders WHERE amount > 9000"},
		{"group_by_agg", ordersSchema, "SELECT status, COUNT(*) FROM orders GROUP BY status"},
		{"two_table_join", joinSchema, "SELECT o.id, o.amount FROM orders o, customers c WHERE o.customer_id = c.id AND c.id = 1"},
	}

	for _, sc := range scenarios {
		sc := sc

		md, q := setupPlannerBench(b, sc.schema, sc.sql)
		// Sanity: the scenario must actually plan, and gives us a cacheable plan.
		warm := planParsedForBench(b, q, md)

		b.Run(sc.name+"/plan_uncached", func(b *testing.B) {
			for b.Loop() {
				_ = planParsedForBench(b, q, md)
			}
		})

		b.Run(sc.name+"/plan_cached", func(b *testing.B) {
			cache := NewPlanCache(256)
			cache.Put(sc.sql, warm, nil)
			for b.Loop() {
				if _, _, ok := cache.Get(sc.sql); !ok {
					b.Fatal("expected cache hit")
				}
			}
		})
	}
}

// setupPlannerBench builds in-memory metadata from DDL and parses the query
// once, returning both for reuse across benchmark iterations.
func setupPlannerBench(b *testing.B, schemaDDL, sql string) (*recordlayer.RecordMetaData, antlrgen.IQueryContext) {
	b.Helper()
	tmpl, err := buildSchemaTemplateFromDDL(schemaDDL)
	if err != nil {
		b.Fatalf("schema DDL: %v", err)
	}
	md := tmpl.Underlying()

	root, err := parser.Parse(sql)
	if err != nil {
		b.Fatalf("parse %q: %v", sql, err)
	}
	stmts := root.Statements()
	if stmts == nil || len(stmts.AllStatement()) == 0 {
		b.Fatalf("no statements in %q", sql)
	}
	sel := stmts.AllStatement()[0].SelectStatement()
	if sel == nil {
		b.Fatalf("not a SELECT: %q", sql)
	}
	q := sel.Query()
	if q == nil {
		b.Fatalf("malformed SELECT: %q", sql)
	}
	return md, q
}

// planParsedForBench runs the Cascades pipeline on an already-parsed query and
// returns the physical plan. Mirrors planSelectCascades minus the cache, the
// FDB statistics fetch (defaults are used), and execution.
func planParsedForBench(b *testing.B, q antlrgen.IQueryContext, md *recordlayer.RecordMetaData) plans.RecordQueryPlan {
	b.Helper()
	visitor := NewPlanVisitor(md)
	logicalOp, err := visitor.VisitQuery(q)
	if err != nil {
		b.Fatalf("visit: %v", err)
	}
	if logicalOp == nil {
		b.Fatal("nil logical plan")
	}
	if err := resolveQualifiedTableNames(logicalOp, "s"); err != nil {
		b.Fatalf("resolve table names: %v", err)
	}
	if err := validateTablesAndColumns(logicalOp, md); err != nil {
		b.Fatalf("validate: %v", err)
	}
	ref, _ := query.TranslateToCascadesWithSubqueries(logicalOp)
	if ref == nil {
		b.Fatal("Cascades translation failed")
	}

	rules := cascades.DefaultExpressionRules()
	rules = append(rules, cascades.RewritingRules()...)
	planCtx := buildCascadesPlanContext(md)
	planner := cascades.NewPlanner(rules, planCtx).
		WithImplementationRules(cascades.DefaultImplementationRules()).
		WithPlanningExpressionRules(cascades.BatchAExpressionRules()).
		WithStatistics(nil).
		WithMaxTasks(100_000)

	bestExpr, _, err := planner.Plan(ref)
	if err != nil {
		b.Fatalf("plan: %v", err)
	}
	if bestExpr == nil {
		b.Fatal("no plan found")
	}
	ph, ok := bestExpr.(interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	})
	if !ok {
		b.Fatalf("best expression is not a physical plan: %T", bestExpr)
	}
	plan := ph.GetRecordQueryPlan()
	if plan == nil {
		b.Fatal("physical plan is nil")
	}
	return plan
}
