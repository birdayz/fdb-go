package embedded

import (
	"context"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/antlr4-go/antlr/v4"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/executor"
	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/metadata"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/session"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// cascadesGenerator is the single query generator for all SQL
// statements. SELECT and DML (INSERT/UPDATE/DELETE, VALUES and SELECT)
// route through the Cascades planner. EXPLAIN, SHOW, DDL, and
// transaction statements are handled directly via PlanFunc wrappers
// around the connection's exec* methods.
type cascadesGenerator struct {
	c     *EmbeddedConnection
	cache *PlanCache
}

func newCascadesGenerator(c *EmbeddedConnection) *cascadesGenerator {
	if c.planCache == nil {
		c.planCache = NewPlanCache(256)
	}
	return &cascadesGenerator{
		c:     c,
		cache: c.planCache,
	}
}

func (g *cascadesGenerator) Plan(ctx context.Context, sql string) (query.Plan, error) {
	root, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}

	stmts := root.Statements()
	if stmts == nil || len(stmts.AllStatement()) == 0 {
		return &query.PlanFunc{
			ExecFn: func(_ context.Context) (query.Result, error) {
				return query.Result{RowsAffected: 0}, nil
			},
			UpdateFn:  func() bool { return true },
			ExplainFn: func() string { return "empty" },
		}, nil
	}

	all := stmts.AllStatement()
	if len(all) == 1 {
		return g.planOne(ctx, all[0])
	}

	// Multi-statement batch: every child must be an update plan
	// (DDL/DML only). Refuse a mixed batch containing SELECT/SHOW.
	children := make([]query.Plan, 0, len(all))
	for _, s := range all {
		p, pErr := g.planOne(ctx, s)
		if pErr != nil {
			return nil, pErr
		}
		if !p.IsUpdate() {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation,
				"multi-statement batches must be DDL/DML only")
		}
		children = append(children, p)
	}
	return &query.MultiPlan{Plans: children}, nil
}

// planOne dispatches a single parsed statement to the appropriate
// planning path: EXPLAIN, SELECT (via Cascades), DML (via Cascades),
// SHOW, DDL, or transaction.
func (g *cascadesGenerator) planOne(ctx context.Context, stmt antlrgen.IStatementContext) (query.Plan, error) {
	c := g.c

	// EXPLAIN <inner> → driver.Rows plan with a single PLAN column.
	if util := stmt.UtilityStatement(); util != nil {
		if full := util.FullDescribeStatement(); full != nil {
			return g.planExplain(ctx, full)
		}
	}

	// DML: INSERT/UPDATE/DELETE (VALUES and SELECT) all execute through the
	// single Cascades path. ExecContext reads RowsAffected; QueryContext
	// rejects update plans (it returns rows, not counts).
	if dml := stmt.DmlStatement(); dml != nil {
		return g.planDML(ctx, dml)
	}

	// SELECT: route through Cascades pipeline.
	if sel := stmt.SelectStatement(); sel != nil {
		return g.planSelect(ctx, sel)
	}

	// SHOW → driver.Rows plan (via admin dispatch).
	if admin := stmt.AdministrationStatement(); admin != nil {
		if show := admin.ShowStatement(); show != nil {
			return &query.PlanFunc{
				ExecFn: func(execCtx context.Context) (query.Result, error) {
					rows, showErr := c.execShowStatement(execCtx, show)
					if showErr != nil {
						return query.Result{}, showErr
					}
					return query.Result{Rows: rows}, nil
				},
				UpdateFn:  func() bool { return false },
				ExplainFn: func() string { return explainStatement("SHOW", show) },
			}, nil
		}
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"only SHOW administration statements are supported")
	}

	// DDL → update plan through execStatement.
	if ddl := stmt.DdlStatement(); ddl != nil {
		return g.planDDL(ctx, stmt)
	}

	// Transaction statements (COMMIT / ROLLBACK / START TRANSACTION).
	if stmt.TransactionStatement() != nil {
		return g.planDDL(ctx, stmt)
	}

	return nil, api.NewError(api.ErrCodeUnsupportedOperation, "unsupported statement type; supported: DDL, INSERT, UPDATE, DELETE")
}

// planSelect routes a SELECT statement through the Cascades pipeline.
func (g *cascadesGenerator) planSelect(ctx context.Context, sel antlrgen.ISelectStatementContext) (query.Plan, error) {
	c := g.c
	q := sel.Query()
	if q == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "malformed SELECT statement")
	}

	// Explain-only mode: no FDB available, produce logical plan text only.
	// Used by NewExplainOnlyGenerator / NewExplainOnlyGeneratorWithSchema.
	if c.sess == nil || c.sess.DB == nil {
		return g.planSelectExplainOnly(sel, q)
	}

	// INFORMATION_SCHEMA queries go through the connection's execSelect
	// path which handles catalog-based system table queries.
	if referencesInformationSchema(q) {
		return &query.PlanFunc{
			ExecFn: func(execCtx context.Context) (query.Result, error) {
				rows, selErr := c.execSelect(execCtx, sel)
				if selErr != nil {
					return query.Result{}, selErr
				}
				return query.Result{Rows: rows}, nil
			},
			UpdateFn: func() bool { return false },
			ExplainFn: func() string {
				md := c.cachedMetaData()
				if md != nil {
					if op, err := buildLogicalPlanForQueryWithCatalog(q, md); err == nil && op != nil {
						return op.Explain("")
					}
				}
				if op := buildLogicalPlanForQuery(q); op != nil {
					return op.Explain("")
				}
				return explainStatement("SELECT", sel)
			},
		}, nil
	}

	if err := g.c.ensureMetaData(ctx); err != nil {
		return nil, err
	}
	md := g.c.cachedMetaData()
	if md == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"no schema metadata available")
	}

	return g.planSelectCascades(ctx, q, md, true)
}

// planSelectExplainOnly produces a PlanFunc that renders a logical plan
// without touching FDB. Used by NewExplainOnlyGenerator and
// NewExplainOnlyGeneratorWithSchema for the plan-equivalence harness.
func (g *cascadesGenerator) planSelectExplainOnly(sel antlrgen.ISelectStatementContext, q antlrgen.IQueryContext) (query.Plan, error) {
	c := g.c
	return &query.PlanFunc{
		ExecFn: func(execCtx context.Context) (query.Result, error) {
			rows, selErr := c.execSelect(execCtx, sel)
			if selErr != nil {
				return query.Result{}, selErr
			}
			return query.Result{Rows: rows}, nil
		},
		UpdateFn: func() bool { return false },
		ExplainFn: func() string {
			md := c.cachedMetaData()
			if md != nil {
				if op, err := buildLogicalPlanForQueryWithCatalog(q, md); err == nil && op != nil {
					return op.Explain("")
				}
			}
			if op := buildLogicalPlanForQuery(q); op != nil {
				return op.Explain("")
			}
			return explainStatement("SELECT", sel)
		},
	}, nil
}

// planSelectCascades runs the full Cascades pipeline for a query.
// logMetrics gates the per-query planning-metrics hook (RFC-034). The real
// query path passes true; the EXPLAIN re-entry from computeExplainText passes
// false so EXPLAIN does not emit a phantom planning event (Java's getPlan
// funnel does not fire for EXPLAIN-internal planning).
func (g *cascadesGenerator) planSelectCascades(ctx context.Context, q antlrgen.IQueryContext, md *recordlayer.RecordMetaData, logMetrics bool) (plan query.Plan, err error) {
	sqlText := q.GetText()
	var ls *planLogScope
	if logMetrics {
		// Log the original whitespace-preserved SQL (canonicalTextOf), not
		// q.GetText() — the latter concatenates tokens without whitespace
		// ("SELECTid=1FROMorders"), which is useless to an operator. The cache
		// key still uses GetText() (RFC-029), a separate concern.
		ls = g.beginPlanLog(ctx, canonicalTextOf(q))
	}
	defer func() { ls.finish(err) }()

	if g.cache != nil {
		if cachedPlan, cachedSubs, ok := g.cache.Get(sqlText); ok {
			ls.setPlan(cachedPlan)
			ls.setCache(PlanCacheHit)
			return &cascadesPlan{
				conn:             g.c,
				md:               md,
				physicalPlan:     cachedPlan,
				explain:          cachedPlan.Explain(),
				scalarSubqueries: cachedSubs,
				sqlLimit:         -1,
			}, nil
		}
	}

	visitor := NewPlanVisitor(md)
	logicalOp, buildErr := visitor.VisitQuery(q)
	if buildErr != nil {
		return nil, buildErr
	}
	if logicalOp == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"Cascades planner could not plan query")
	}

	if fn := query.FindUnsupportedFunction(logicalOp); fn != "" {
		return nil, api.NewError(api.ErrCodeUndefinedFunction,
			"Unsupported operator "+fn)
	}

	if err := resolveQualifiedTableNames(logicalOp, g.c.sess.Schema); err != nil {
		return nil, err
	}

	if err := validateTablesAndColumns(logicalOp, md); err != nil {
		return nil, err
	}

	if msg := findDistinctAggregate(logicalOp); msg != "" {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, msg)
	}

	if msg := findFullOuterWithExists(logicalOp); msg != "" {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, msg)
	}

	ref, scalarSubqueryPlans := query.TranslateToCascadesWithSubqueries(logicalOp, md)
	if ref == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"Cascades planner could not plan query")
	}

	rules := cascades.DefaultExpressionRules()
	rules = append(rules, cascades.RewritingRules()...)
	planCtx := buildCascadesPlanContext(md)
	stats := g.fetchTableStatistics(ctx, md)
	planner := cascades.NewPlanner(rules, planCtx).
		WithImplementationRules(cascades.DefaultImplementationRules()).
		WithPlanningExpressionRules(cascades.BatchAExpressionRules()).
		WithStatistics(stats).
		WithMaxTasks(100_000)

	bestExpr, _, planErr := planner.Plan(ref)
	if planErr != nil || bestExpr == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"Cascades planner could not plan query")
	}

	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	ph, ok := bestExpr.(planExtractor)
	if !ok {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"Cascades planner could not plan query")
	}
	physPlan := ph.GetRecordQueryPlan()
	if physPlan == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"Cascades planner could not plan query")
	}
	// Plan scalar subqueries independently through the Cascades pipeline.
	var scalarSubs []scalarSubqueryBinding
	for _, ssq := range scalarSubqueryPlans {
		// Pass md so the scalar subquery's own join legs can anchor (RFC-077 7.6);
		// nested scalar subqueries are not collected here, so any they contain are
		// dropped — matching the previous behavior (TranslateToCascades discards
		// them too).
		subRef, _ := query.TranslateToCascadesWithSubqueries(ssq.Plan, md)
		if subRef == nil {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"Cascades planner could not plan scalar subquery")
		}
		subPlanner := cascades.NewPlanner(rules, planCtx).
			WithImplementationRules(cascades.DefaultImplementationRules()).
			WithPlanningExpressionRules(cascades.BatchAExpressionRules()).
			WithStatistics(stats).
			WithMaxTasks(100_000)
		subBest, _, subErr := subPlanner.Plan(subRef)
		if subErr != nil || subBest == nil {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"Cascades planner could not plan scalar subquery")
		}
		subPh, ok := subBest.(planExtractor)
		if !ok {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"scalar subquery plan extraction failed")
		}
		subPlan := subPh.GetRecordQueryPlan()
		if subPlan == nil {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"scalar subquery physical plan nil")
		}
		scalarSubs = append(scalarSubs, scalarSubqueryBinding{
			alias: ssq.Alias,
			plan:  subPlan,
		})
	}

	sqlLimit, sqlOffset := extractLimitOffset(logicalOp)

	ls.setPlan(physPlan)
	// Don't cache LIMIT/OFFSET queries — the limit is applied post-execution
	// and is not stored in the cached plan.
	if g.cache != nil && sqlLimit < 0 && sqlOffset == 0 {
		ls.setCache(PlanCacheMiss)
		g.cache.Put(sqlText, physPlan, scalarSubs)
	} else {
		// Planned but deliberately not cached (LIMIT/OFFSET) or no cache.
		ls.setCache(PlanCacheSkip)
	}
	return &cascadesPlan{
		conn:             g.c,
		md:               md,
		physicalPlan:     physPlan,
		explain:          physPlan.Explain(),
		scalarSubqueries: scalarSubs,
		sqlLimit:         sqlLimit,
		sqlOffset:        sqlOffset,
	}, nil
}

// planExplain handles `EXPLAIN <query|delete|insert|update>`.
// For SELECT queries, runs the full Cascades pipeline and returns
// physPlan.Explain() as the PLAN column. For DML, uses the existing
// buildLogicalPlanFor*WithCatalog functions for the explain text.
func (g *cascadesGenerator) planExplain(ctx context.Context, full antlrgen.IFullDescribeStatementContext) (query.Plan, error) {
	objClause := full.DescribeObjectClause()
	if objClause == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"EXPLAIN requires an inner statement")
	}
	descStmts, ok := objClause.(*antlrgen.DescribeStatementsContext)
	if !ok {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"EXPLAIN form not supported (only EXPLAIN <query|insert|update|delete>)")
	}
	planText := g.computeExplainText(ctx, descStmts)
	if planText == "" {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"EXPLAIN inner statement produced no plan text")
	}
	return &query.PlanFunc{
		ExecFn: func(_ context.Context) (query.Result, error) {
			return query.Result{Rows: &staticRows{
				cols: []string{"PLAN"},
				rows: [][]driver.Value{{planText}},
			}}, nil
		},
		UpdateFn:  func() bool { return false },
		ExplainFn: func() string { return "EXPLAIN: " + planText },
	}, nil
}

// computeExplainText builds the plan-tree text for the inner
// statement of an EXPLAIN. For SELECT queries, attempts to run
// the full Cascades pipeline to produce a physical plan explain.
// Falls back to logical plan text for DML and when Cascades can't
// plan the query (e.g. no metadata, INFORMATION_SCHEMA).
func (g *cascadesGenerator) computeExplainText(ctx context.Context, d *antlrgen.DescribeStatementsContext) string {
	c := g.c
	md := c.cachedMetaData()

	// SELECT: try Cascades pipeline for physical plan explain.
	if q := d.Query(); q != nil {
		// Try Cascades for a physical plan explain when FDB + metadata are available.
		if c.sess != nil && c.sess.DB != nil && !referencesInformationSchema(q) {
			if err := c.ensureMetaData(ctx); err == nil {
				if freshMd := c.cachedMetaData(); freshMd != nil {
					if plan, planErr := g.planSelectCascades(ctx, q, freshMd, false); planErr == nil {
						return plan.Explain()
					}
				}
			}
		}
		// Fallback to logical plan text.
		if md != nil {
			if op, err := buildLogicalPlanForQueryWithCatalog(q, md); err == nil && op != nil {
				return op.Explain("")
			}
		}
		if op := buildLogicalPlanForQuery(q); op != nil {
			return op.Explain("")
		}
	}
	if del := d.DeleteStatement(); del != nil {
		if md != nil {
			if op := buildLogicalPlanForDeleteWithCatalog(del, md); op != nil {
				return op.Explain("")
			}
		}
		if op := buildLogicalPlanForDelete(del); op != nil {
			return op.Explain("")
		}
	}
	if ins := d.InsertStatement(); ins != nil {
		if md != nil {
			if op := buildLogicalPlanForInsertWithCatalog(ins, md); op != nil {
				return op.Explain("")
			}
		}
		if op := buildLogicalPlanForInsert(ins); op != nil {
			return op.Explain("")
		}
	}
	if upd := d.UpdateStatement(); upd != nil {
		if md != nil {
			if op := buildLogicalPlanForUpdateWithCatalog(upd, md); op != nil {
				return op.Explain("")
			}
		}
		if op := buildLogicalPlanForUpdate(upd); op != nil {
			return op.Explain("")
		}
	}
	return ""
}

// planDDL wraps a DDL or transaction statement in a PlanFunc that
// delegates to connection.execStatement.
func (g *cascadesGenerator) planDDL(_ context.Context, stmt antlrgen.IStatementContext) (query.Plan, error) {
	c := g.c
	return &query.PlanFunc{
		ExecFn: func(execCtx context.Context) (query.Result, error) {
			n, execErr := c.execStatement(execCtx, stmt)
			if execErr != nil {
				return query.Result{}, execErr
			}
			return query.Result{RowsAffected: n}, nil
		},
		UpdateFn: func() bool { return true },
		ExplainFn: func() string {
			md := c.cachedMetaData()
			if dml := stmt.DmlStatement(); dml != nil {
				if del := dml.DeleteStatement(); del != nil {
					if md != nil {
						if op := buildLogicalPlanForDeleteWithCatalog(del, md); op != nil {
							return op.Explain("")
						}
					}
					if op := buildLogicalPlanForDelete(del); op != nil {
						return op.Explain("")
					}
				}
				if upd := dml.UpdateStatement(); upd != nil {
					if md != nil {
						if op := buildLogicalPlanForUpdateWithCatalog(upd, md); op != nil {
							return op.Explain("")
						}
					}
					if op := buildLogicalPlanForUpdate(upd); op != nil {
						return op.Explain("")
					}
				}
				if ins := dml.InsertStatement(); ins != nil {
					if md != nil {
						if op := buildLogicalPlanForInsertWithCatalog(ins, md); op != nil {
							return op.Explain("")
						}
					}
					if op := buildLogicalPlanForInsert(ins); op != nil {
						return op.Explain("")
					}
				}
			}
			return explainStatement(statementKind(stmt), stmt)
		},
	}, nil
}

func extractLimitOffset(op logical.LogicalOperator) (int64, int64) {
	for op != nil {
		if lim, ok := op.(*logical.LogicalLimit); ok {
			return lim.Limit, lim.Offset
		}
		children := op.Children()
		if len(children) == 0 {
			break
		}
		op = children[0]
	}
	return -1, 0
}

func (g *cascadesGenerator) planDML(ctx context.Context, dml antlrgen.IDmlStatementContext) (plan query.Plan, err error) {
	c := g.c

	// Explain-only mode: no FDB available, produce logical plan text only.
	// No planning happens here, so it is outside the metrics funnel.
	if c.sess == nil || c.sess.DB == nil {
		return g.planDMLExplainOnly(dml)
	}

	// DML is never cached; the cache event is always Skip on success.
	// Log the original whitespace-preserved SQL (see planSelectCascades).
	ls := g.beginPlanLog(ctx, canonicalTextOf(dml))
	defer func() { ls.finish(err) }()

	if err := c.ensureMetaData(ctx); err != nil {
		return nil, err
	}
	md := c.cachedMetaData()
	if md == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "no schema metadata available")
	}

	var logicalOp logical.LogicalOperator
	var insStmt antlrgen.IInsertStatementContext
	if del := dml.DeleteStatement(); del != nil {
		logicalOp = buildLogicalPlanForDeleteWithCatalog(del, md)
	} else if upd := dml.UpdateStatement(); upd != nil {
		logicalOp = buildLogicalPlanForUpdateWithCatalog(upd, md)
	} else if ins := dml.InsertStatement(); ins != nil {
		insStmt = ins
		logicalOp = buildLogicalPlanForInsertWithCatalog(ins, md)
	}
	if logicalOp == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "DML logical plan failed")
	}

	if err := resolveQualifiedTableNames(logicalOp, g.c.sess.Schema); err != nil {
		return nil, err
	}

	// INSERT … SELECT with an explicit column list is rejected (Java:
	// "setting column ordering for insert with select is not supported").
	if insOp, ok := logicalOp.(*logical.LogicalInsert); ok && insOp.Source != nil && len(insOp.Columns) > 0 {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"setting column ordering for insert with select is not supported")
	}

	// INSERT … SELECT promotion guard: reject when an aggregate-result column
	// cannot be promoted to its target column type (e.g. AVG(BIGINT)→DOUBLE into
	// a BIGINT column), matching Java's plan-time PromoteValue rejection
	// (SQLSTATE 22000), independent of how many rows the source yields.
	if insOp, ok := logicalOp.(*logical.LogicalInsert); ok && insOp.Source != nil {
		if err := checkInsertSelectPromotable(insOp, md); err != nil {
			return nil, err
		}
	}

	// INSERT … VALUES: build the literal rows into a Cascades array Value
	// (resolved table name is now available). translateInsert explodes it
	// as the InsertExpression inner, so VALUES rides the Cascades path.
	if insOp, ok := logicalOp.(*logical.LogicalInsert); ok && insOp.Source == nil && insOp.ValuesArray == nil && insStmt != nil {
		rt := md.GetRecordType(insOp.Table)
		if rt == nil {
			return nil, api.NewErrorf(api.ErrCodeUndefinedTable, "Unknown table %s", strings.ToUpper(insOp.Table))
		}
		arr, vErr := c.buildInsertValuesArray(ctx, insStmt, rt.Descriptor, insOp.Table)
		if vErr != nil {
			return nil, vErr
		}
		insOp.ValuesArray = arr
	}

	// UPDATE: reject unsupported functions in SET RHS (parse-tree scan, the
	// same mechanism the SELECT projection path uses — catches functions
	// the resolver can't build a Value for, e.g. UPPER), and SET col = NULL
	// on a NOT NULL column. Both at plan time, matching the naive path.
	if updOp, ok := logicalOp.(*logical.LogicalUpdate); ok {
		if upd := dml.UpdateStatement(); upd != nil {
			for _, el := range upd.AllUpdatedElement() {
				if el == nil || el.Expression() == nil {
					continue
				}
				if fn := findUnsupportedFunctionInParseTree(el.Expression()); fn != "" {
					return nil, api.NewError(api.ErrCodeUndefinedFunction, "Unsupported operator "+fn)
				}
			}
		}
		if err := validateUpdateAssignments(updOp, md); err != nil {
			return nil, err
		}
	}

	if fn := query.FindUnsupportedFunction(logicalOp); fn != "" {
		return nil, api.NewError(api.ErrCodeUndefinedFunction,
			"Unsupported operator "+fn)
	}

	// Pass md so DML join legs (e.g. UPDATE … FROM a JOIN b) anchor (RFC-077 7.6).
	ref, _ := query.TranslateToCascadesWithSubqueries(logicalOp, md)
	if ref == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "DML Cascades translation failed")
	}

	rules := cascades.DefaultExpressionRules()
	rules = append(rules, cascades.RewritingRules()...)
	planCtx := buildCascadesPlanContext(md)
	dmlStats := g.fetchTableStatistics(ctx, md)
	planningRules := append(cascades.BatchAExpressionRules(), cascades.DMLImplementationRules()...)
	planner := cascades.NewPlanner(rules, planCtx).
		WithImplementationRules(cascades.DefaultImplementationRules()).
		WithPlanningExpressionRules(planningRules).
		WithStatistics(dmlStats).
		WithMaxTasks(100_000)

	bestExpr, _, planErr := planner.Plan(ref)
	if planErr != nil || bestExpr == nil {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedQuery, "DML Cascades planning failed: %v", planErr)
	}

	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	ph, ok := bestExpr.(planExtractor)
	if !ok {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "DML plan extraction failed")
	}
	physPlan := ph.GetRecordQueryPlan()
	if physPlan == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "DML physical plan nil")
	}

	ls.setPlan(physPlan)
	ls.setCache(PlanCacheSkip)
	return &cascadesPlan{
		conn:         g.c,
		md:           md,
		physicalPlan: physPlan,
		explain:      logicalOp.Explain(""),
	}, nil
}

// planDMLExplainOnly produces a PlanFunc for DML (INSERT/UPDATE/DELETE) in
// explain-only mode (no live FDB): ExplainFn renders the logical plan
// without touching FDB, used by NewExplainOnlyGenerator /
// NewExplainOnlyGeneratorWithSchema where only ExplainFn is called.
// ExecFn is unreachable in this mode — DML with a live connection goes
// through planDML (the Cascades path) — so it returns an error rather
// than touch FDB.
func (g *cascadesGenerator) planDMLExplainOnly(dml antlrgen.IDmlStatementContext) (query.Plan, error) {
	c := g.c
	return &query.PlanFunc{
		ExecFn: func(ctx context.Context) (query.Result, error) {
			return query.Result{}, api.NewError(api.ErrCodeUnsupportedOperation,
				"DML execution requires a live connection (explain-only generator)")
		},
		UpdateFn: func() bool { return true },
		ExplainFn: func() string {
			md := c.cachedMetaData()
			if del := dml.DeleteStatement(); del != nil {
				if md != nil {
					if op := buildLogicalPlanForDeleteWithCatalog(del, md); op != nil {
						return op.Explain("")
					}
				}
				if op := buildLogicalPlanForDelete(del); op != nil {
					return op.Explain("")
				}
			}
			if upd := dml.UpdateStatement(); upd != nil {
				if md != nil {
					if op := buildLogicalPlanForUpdateWithCatalog(upd, md); op != nil {
						return op.Explain("")
					}
				}
				if op := buildLogicalPlanForUpdate(upd); op != nil {
					return op.Explain("")
				}
			}
			if ins := dml.InsertStatement(); ins != nil {
				if md != nil {
					if op := buildLogicalPlanForInsertWithCatalog(ins, md); op != nil {
						return op.Explain("")
					}
				}
				if op := buildLogicalPlanForInsert(ins); op != nil {
					return op.Explain("")
				}
			}
			return "DML"
		},
	}, nil
}

// scalarSubqueryBinding pairs a correlation alias with a planned
// inner RecordQueryPlan for a scalar subquery. The executor pre-runs
// each and binds the scalar result under the alias before running
// the outer plan.
type scalarSubqueryBinding struct {
	alias values.CorrelationIdentifier
	plan  plans.RecordQueryPlan
}

// cascadesPlan wraps a Cascades-planned SELECT query with a pre-computed
// physical plan. Planning happens at Plan-time; Execute only runs the plan.
type cascadesPlan struct {
	conn             *EmbeddedConnection
	md               *recordlayer.RecordMetaData
	physicalPlan     plans.RecordQueryPlan
	explain          string
	scalarSubqueries []scalarSubqueryBinding
	sqlLimit         int64 // Go extension: <0 means no limit
	sqlOffset        int64
}

// IsUpdate reports whether this is a DML plan (INSERT/UPDATE/DELETE),
// derived from the physical plan type rather than a stored flag —
// matching Java's QueryPlan.isUpdatePlan() (an instanceof check), so
// update-ness can never drift from the plan shape (DIVERGENCES Principle
// 10). cascadesPlan is only built for real execution (planDMLExplainOnly
// handles EXPLAIN separately), so there is no explain-mode case here.
func (p *cascadesPlan) IsUpdate() bool {
	switch p.physicalPlan.(type) {
	case *plans.RecordQueryInsertPlan, *plans.RecordQueryUpdatePlan, *plans.RecordQueryDeletePlan:
		return true
	default:
		return false
	}
}

func (p *cascadesPlan) Explain() string {
	if p.physicalPlan != nil {
		return p.physicalPlan.Explain()
	}
	return p.explain
}

// txPageTimeLimit is the per-transaction time budget for SQL query
// execution. Set below FDB's 5s hard wall to leave margin for commit
// and cleanup. Matches Java's ExecuteProperties.setTimeLimit pattern.
const txPageTimeLimit = 4 * time.Second

func (p *cascadesPlan) Execute(ctx context.Context) (query.Result, error) {
	c := p.conn
	ss, ssErr := c.sess.Keyspace.SchemaSubspace(c.sess.DBPath, c.sess.Schema)
	if ssErr != nil {
		return query.Result{}, ssErr
	}

	cols := deriveColumnsFromPlan(p.physicalPlan, p.md)

	// Each fetchPage creates a fresh cursor hierarchy from the plan +
	// continuation. The continuation carries all intermediate state
	// (aggregate accumulators, sort buffers) serialized as protobuf.
	// No cursor persists across transactions — this matches Java's
	// architecture.

	pr := &paginatingRows{
		ctx:              ctx,
		conn:             c,
		ss:               ss,
		plan:             p.physicalPlan,
		md:               p.md,
		scalarSubqueries: p.scalarSubqueries,
		sqlLimit:         p.sqlLimit,
		sqlOffset:        p.sqlOffset,
		cols:             cols,
		respectActiveTx:  p.IsUpdate(),
	}

	// Eagerly fetch the first page so execution errors (type mismatches,
	// plan failures) surface at QueryContext time, not during row iteration.
	if err := pr.fetchPage(); err != nil {
		return query.Result{}, err
	}

	// DML (INSERT/UPDATE/DELETE) plans emit one row per affected record;
	// the affected-row count is the JDBC update count, not a result set.
	// Drain and count, matching Java's AbstractEmbeddedStatement.countUpdates.
	// The mutations have already run inside fetchPage's transaction(s).
	if p.IsUpdate() {
		n, err := pr.countAll()
		pr.Close()
		if err != nil {
			return query.Result{}, err
		}
		return query.Result{RowsAffected: n}, nil
	}

	return query.Result{Rows: pr}, nil
}

// countAll drains every remaining row, returning the total count. Used
// for DML where the plan emits one row per affected record and the
// caller wants the count rather than the rows. nextRow drives
// cross-page fetching; LIMIT/OFFSET never apply to DML so counting the
// raw row stream is correct.
func (r *paginatingRows) countAll() (int64, error) {
	var n int64
	for {
		_, err := r.nextRow()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return 0, err
		}
		n++
	}
}

// paginatingRows implements driver.Rows with cross-transaction pagination.
// Each fetchPage creates a fresh cursor hierarchy from the plan +
// continuation. The continuation carries all intermediate state
// (aggregate accumulators, sort buffers) serialized as protobuf. No
// cursor persists across transactions — this matches Java's architecture.
type paginatingRows struct {
	ctx              context.Context
	conn             *EmbeddedConnection
	ss               subspace.Subspace
	plan             plans.RecordQueryPlan
	md               *recordlayer.RecordMetaData
	scalarSubqueries []scalarSubqueryBinding
	cols             []executor.ColumnDef

	// Go extension: SQL LIMIT/OFFSET applied post-execution.
	sqlLimit  int64 // <0 means no limit
	sqlOffset int64
	emitted   int64
	skipped   int64

	buf          [][]driver.Value
	bufPos       int
	continuation []byte
	exhausted    bool
	closed       bool
	fetchErr     error

	// respectActiveTx routes page execution through the connection's
	// open explicit transaction (runInTx) instead of a fresh auto-commit
	// transaction (DB.Run). Set for DML so INSERT/UPDATE/DELETE inside a
	// BeginTx block join that transaction and commit only on COMMIT —
	// matching the naive path. SELECT keeps the auto-commit snapshot.
	respectActiveTx bool
}

func (r *paginatingRows) Columns() []string {
	cols := make([]string, len(r.cols))
	for i, c := range r.cols {
		if c.Label != "" {
			cols[i] = c.Label
		} else {
			cols[i] = c.Name
		}
	}
	return cols
}

func (r *paginatingRows) Close() error {
	r.closed = true
	return nil
}

func (r *paginatingRows) ColumnTypeDatabaseTypeName(index int) string {
	if index < 0 || index >= len(r.cols) {
		return ""
	}
	return r.cols[index].TypeName
}

func (r *paginatingRows) ColumnTypeScanType(index int) reflect.Type {
	switch r.ColumnTypeDatabaseTypeName(index) {
	case "BIGINT":
		return reflect.TypeOf((*int64)(nil)).Elem()
	case "INTEGER":
		return reflect.TypeOf((*int32)(nil)).Elem()
	case "DOUBLE":
		return reflect.TypeOf((*float64)(nil)).Elem()
	case "FLOAT":
		return reflect.TypeOf((*float32)(nil)).Elem()
	case "STRING":
		return reflect.TypeOf((*string)(nil)).Elem()
	case "BOOLEAN":
		return reflect.TypeOf((*bool)(nil)).Elem()
	case "BYTES", "BINARY":
		return reflect.TypeOf((*[]byte)(nil)).Elem()
	case "DATE", "TIMESTAMP":
		return reflect.TypeOf((*time.Time)(nil)).Elem()
	default:
		return reflect.TypeOf((*any)(nil)).Elem()
	}
}

func (r *paginatingRows) ColumnTypeNullable(index int) (nullable, ok bool) {
	if index < 0 || index >= len(r.cols) {
		return true, true
	}
	return r.cols[index].Nullable != api.ColumnNoNulls, true
}

func (r *paginatingRows) ColumnTypeLength(index int) (length int64, ok bool) {
	switch r.ColumnTypeDatabaseTypeName(index) {
	case "STRING", "BYTES", "BINARY":
		return math.MaxInt64, true
	case "DATE":
		return 10, true
	case "TIMESTAMP":
		return 19, true
	}
	return 0, false
}

func (r *paginatingRows) ColumnTypePrecisionScale(index int) (precision, scale int64, ok bool) {
	return 0, 0, false
}

func (r *paginatingRows) Next(dest []driver.Value) error {
	if r.closed {
		return io.EOF
	}
	// LIMIT exhausted — stop early.
	if r.sqlLimit >= 0 && r.emitted >= r.sqlLimit {
		return io.EOF
	}

	for {
		row, err := r.nextRow()
		if err != nil {
			return err
		}
		// OFFSET: skip rows.
		if r.skipped < r.sqlOffset {
			r.skipped++
			continue
		}
		copy(dest, row)
		r.emitted++
		return nil
	}
}

func (r *paginatingRows) nextRow() ([]driver.Value, error) {
	if r.closed {
		return nil, io.EOF
	}

	// Serve from buffer if available.
	if r.bufPos < len(r.buf) {
		row := r.buf[r.bufPos]
		r.bufPos++
		return row, nil
	}

	// Buffer exhausted. If source is done, we're done.
	if r.exhausted {
		return nil, io.EOF
	}
	if r.fetchErr != nil {
		return nil, r.fetchErr
	}

	// Fetch pages until we have rows or the source is truly exhausted.
	// Blocking operators (aggregate, sort) may produce 0 result rows per
	// page while accumulating — they only emit after the inner scan is
	// fully drained. Keep fetching until rows appear or exhaustion.
	for {
		if err := r.fetchPage(); err != nil {
			r.fetchErr = err
			return nil, err
		}
		if len(r.buf) > 0 {
			break
		}
		if r.exhausted {
			return nil, io.EOF
		}
	}

	row := r.buf[r.bufPos]
	r.bufPos++
	return row, nil
}

// fetchPage opens a fresh FDB transaction, creates the cursor hierarchy
// (or recreates it from the continuation), drains the cursor until it
// stops, and buffers the results. Everything happens INSIDE DB.Run so
// FDB reads are against a live transaction.
//
// This matches Java's architecture: each transaction creates a fresh
// cursor hierarchy from the plan + continuation. The continuation
// carries ALL intermediate state (aggregate accumulators, sort buffers)
// serialized as protobuf. No cursor persists across transactions.
func (r *paginatingRows) fetchPage() error {
	c := r.conn

	// DML joins an open explicit transaction (runInTx); SELECT runs in a
	// fresh auto-commit transaction (DB.Run). runInTx falls back to DB.Run
	// when no explicit transaction is active, so auto-commit DML behaves
	// identically to before.
	runTx := c.sess.DB.Run
	if r.respectActiveTx {
		runTx = c.runInTx
	}

	_, txErr := runTx(r.ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		r.buf = r.buf[:0]
		r.bufPos = 0

		store, storeErr := c.newStoreBuilder().
			SetContext(rctx).
			SetSubspace(r.ss).
			SetMetaDataProvider(c.cachedMetaData()).
			Open()
		if storeErr != nil {
			return nil, storeErr
		}

		evalCtx := executor.EmptyEvaluationContext()
		if len(r.scalarSubqueries) > 0 {
			scalarResults := make(map[values.CorrelationIdentifier]any, len(r.scalarSubqueries))
			for _, ssq := range r.scalarSubqueries {
				result, ssqErr := executor.EvaluateScalarSubquery(r.ctx, ssq.plan, store, evalCtx)
				if ssqErr != nil {
					return nil, ssqErr
				}
				scalarResults[ssq.alias] = result
			}
			evalCtx = evalCtx.WithScalarSubqueries(scalarResults)
		}

		props := recordlayer.DefaultExecuteProperties().WithTimeLimit(txPageTimeLimit)
		cursor, execErr := executor.ExecutePlan(r.ctx, r.plan, store, evalCtx, r.continuation, props)
		if execErr != nil {
			return nil, translateExecError(execErr)
		}

		rs := executor.NewRecordLayerResultSet(r.ctx, cursor, r.cols)
		defer rs.Close()

		for rs.Next() {
			row := make([]driver.Value, len(r.cols))
			for i := range row {
				v, err := rs.Object(i + 1)
				if err != nil {
					return nil, err
				}
				row[i] = v
			}
			r.buf = append(r.buf, row)
		}
		if err := rs.Err(); err != nil {
			return nil, translateExecError(err)
		}

		cont := rs.GetContinuation()
		if cont == nil || cont.IsEnd() {
			r.exhausted = true
			r.continuation = nil
		} else {
			contBytes, contErr := cont.ToBytes()
			if contErr != nil {
				return nil, contErr
			}
			if contBytes == nil {
				r.exhausted = true
			} else {
				r.continuation = contBytes
			}
		}
		return nil, nil
	})

	if txErr != nil {
		return translateExecError(txErr)
	}
	return nil
}

func translateExecError(err error) error {
	if err == nil {
		return nil
	}
	var typeMismatch *predicates.TypeMismatchError
	if errors.As(err, &typeMismatch) {
		return api.NewError(api.ErrCodeDatatypeMismatch,
			"The operands of a comparison operator are not compatible.")
	}
	var depthExceeded *executor.RecursiveCTEDepthExceededError
	if errors.As(err, &depthExceeded) {
		return api.NewError(api.ErrCodeExecutionLimitReached, depthExceeded.Error())
	}
	var aggTypeMismatch *executor.AggregateTypeMismatchError
	if errors.As(err, &aggTypeMismatch) {
		return api.NewError(api.ErrCodeUnsupportedOperation, aggTypeMismatch.Error())
	}
	var rangeOverflow *executor.NumericRangeOverflowError
	if errors.As(err, &rangeOverflow) {
		return api.NewError(api.ErrCodeNumericValueOutOfRange, rangeOverflow.Error())
	}
	var sumOverflow *executor.SumOverflowError
	if errors.As(err, &sumOverflow) {
		return api.NewError(api.ErrCodeNumericValueOutOfRange, sumOverflow.Error())
	}
	var divZero *values.ArithmeticDivisionByZeroError
	if errors.As(err, &divZero) {
		return api.NewError(api.ErrCodeDivisionByZero, "/ by zero")
	}
	var overflow *values.ArithmeticOverflowError
	if errors.As(err, &overflow) {
		return api.NewError(api.ErrCodeNumericValueOutOfRange, "integer overflow")
	}
	var scalarMismatch *values.ScalarTypeMismatchError
	if errors.As(err, &scalarMismatch) {
		return api.NewError(api.ErrCodeCannotConvertType, scalarMismatch.Error())
	}
	var castErr *values.InvalidCastError
	if errors.As(err, &castErr) {
		return api.NewError(api.ErrCodeInvalidCast, castErr.Error())
	}
	return err
}

// fetchTableStatistics reads per-record-type row counts from FDB using a
// read-only snapshot transaction. Returns nil (use defaults) on any error —
// statistics are best-effort; a failed stats read should never prevent
// query planning.
//
// Only returns real statistics when the metadata uses RecordTypeKeyExpression
// as the count key (the default for multi-table SQL schemas). For intermingled
// schemas (EmptyKey), per-type counts are unavailable — returns nil rather
// than fabricating an equal distribution that would mislead the cost model.
func (g *cascadesGenerator) fetchTableStatistics(ctx context.Context, md *recordlayer.RecordMetaData) properties.StatisticsProvider {
	c := g.c
	if c.sess == nil || c.sess.DB == nil || md == nil {
		return nil
	}
	countKey := md.GetRecordCountKey()
	if countKey == nil {
		return nil
	}
	if !recordlayer.IsRecordTypeExpression(countKey) {
		return nil
	}
	ss, err := c.sess.Keyspace.SchemaSubspace(c.sess.DBPath, c.sess.Schema)
	if err != nil {
		return nil
	}

	countSubspace := ss.Sub(recordlayer.RecordCountKey)
	result, runErr := c.sess.DB.RunRead(ctx, func(rtx fdb.ReadTransaction) (any, error) {
		counts := make(map[string]float64)
		for name := range md.RecordTypes() {
			rt := md.GetRecordType(name)
			if rt == nil {
				continue
			}
			fdbKey := countSubspace.Pack(tuple.Tuple{rt.GetRecordTypeKey()})
			value, readErr := rtx.Snapshot().Get(fdbKey).Get()
			if readErr != nil {
				return nil, readErr
			}
			if len(value) >= 8 {
				counts[name] = float64(int64(binary.LittleEndian.Uint64(value)))
			}
		}
		return counts, nil
	})
	if runErr != nil || result == nil {
		return nil
	}
	counts := result.(map[string]float64)
	if len(counts) == 0 {
		return nil
	}
	return properties.MapStatistics{PerType: counts}
}

func buildCascadesPlanContext(md *recordlayer.RecordMetaData) cascades.PlanContext {
	if md == nil {
		return cascades.EmptyPlanContext()
	}
	return &metadataPlanContext{md: md}
}

type metadataPlanContext struct {
	md *recordlayer.RecordMetaData
}

func (c *metadataPlanContext) GetPlannerConfiguration() cascades.PlannerConfiguration {
	return cascades.DefaultPlannerConfiguration()
}

func (c *metadataPlanContext) GetMatchCandidates() []cascades.MatchCandidate {
	if c.md == nil {
		return nil
	}

	var candidates []cascades.MatchCandidate

	// Register PrimaryScanMatchCandidates for each record type's PK.
	// Mirrors Java's RecordStoreScope which creates a PrimaryScanMatchCandidate
	// from the common primary key.
	allTypes := c.md.RecordTypes()
	allTypeNames := make([]string, 0, len(allTypes))
	for name := range allTypes {
		allTypeNames = append(allTypeNames, name)
	}
	for _, rt := range allTypes {
		if rt.PrimaryKey == nil {
			continue
		}
		pkCols := rt.PrimaryKey.FieldNames()
		if len(pkCols) == 0 {
			continue
		}
		upperPK := make([]string, len(pkCols))
		aliases := make([]values.CorrelationIdentifier, len(pkCols))
		for i, col := range pkCols {
			upperPK[i] = strings.ToUpper(col)
			aliases[i] = values.UniqueCorrelationIdentifier()
		}
		candidates = append(candidates, cascades.NewPrimaryScanMatchCandidate(
			nil,
			aliases,
			allTypeNames,
			[]string{rt.Name},
			upperPK,
			values.UnknownType,
		))
	}

	// Register secondary index candidates.
	allIndexes := c.md.GetAllIndexes()
	defs := make([]cascades.IndexDef, 0, len(allIndexes))
	for _, idx := range allIndexes {
		if idx.RootExpression == nil {
			continue
		}
		if aggCand := tryAggregateIndexCandidate(idx, c.md); aggCand != nil {
			candidates = append(candidates, aggCand)
			continue
		}
		if vecCand := tryVectorIndexCandidate(idx, c.md); vecCand != nil {
			candidates = append(candidates, vecCand)
			continue
		}
		defs = append(defs, &metadataIndexDef{idx: idx, md: c.md})
	}
	if len(defs) > 0 {
		ctx := cascades.NewPlanContextFromIndexDefs(defs)
		candidates = append(candidates, ctx.GetMatchCandidates()...)
	}

	return candidates
}

type metadataIndexDef struct {
	idx *recordlayer.Index
	md  *recordlayer.RecordMetaData
}

func (d *metadataIndexDef) IndexName() string          { return d.idx.Name }
func (d *metadataIndexDef) IndexColumnNames() []string { return d.idx.RootExpression.FieldNames() }
func (d *metadataIndexDef) IndexIsUnique() bool        { return d.idx.IsUnique() }

func (d *metadataIndexDef) IndexRecordTypes() []string {
	rts := d.md.RecordTypesForIndex(d.idx)
	names := make([]string, len(rts))
	for i, rt := range rts {
		names[i] = rt.Name
	}
	return names
}

func (d *metadataIndexDef) IndexPrimaryKeyColumns() []string {
	rts := d.md.RecordTypesForIndex(d.idx)
	if len(rts) == 0 {
		return nil
	}
	pk := rts[0].PrimaryKey
	if pk == nil {
		return nil
	}
	return pk.FieldNames()
}

func (c *metadataPlanContext) GetPrimaryKeyColumns(recordType string) []string {
	if c.md == nil {
		return nil
	}
	rt := c.md.GetRecordType(recordType)
	if rt == nil || rt.PrimaryKey == nil {
		return nil
	}
	return rt.PrimaryKey.FieldNames()
}

// tryAggregateIndexCandidate checks if the index is an aggregate type
// (SUM, COUNT, MIN, MAX) and returns an AggregateIndexMatchCandidate,
// or nil if the index is not an aggregate type.
func tryAggregateIndexCandidate(idx *recordlayer.Index, md *recordlayer.RecordMetaData) *cascades.AggregateIndexMatchCandidate {
	var aggFunc expressions.AggregateFunction
	switch idx.Type {
	case recordlayer.IndexTypeSum:
		aggFunc = expressions.AggSum
	case recordlayer.IndexTypeCount, recordlayer.IndexTypeCountNotNull:
		aggFunc = expressions.AggCount
	case recordlayer.IndexTypeMaxEverLong, recordlayer.IndexTypeMaxEverTuple:
		aggFunc = expressions.AggMax
	case recordlayer.IndexTypeMinEverLong, recordlayer.IndexTypeMinEverTuple:
		aggFunc = expressions.AggMin
	default:
		return nil
	}

	gke, ok := idx.RootExpression.(*recordlayer.GroupingKeyExpression)
	if !ok {
		return nil
	}

	allCols := gke.FieldNames()
	groupingCount := gke.GetGroupingCount()
	groupedCount := gke.GetGroupedCount()

	if groupingCount == 0 {
		return nil
	}

	groupCols := make([]string, groupingCount)
	for i := 0; i < groupingCount; i++ {
		groupCols[i] = strings.ToUpper(allCols[i])
	}

	var aggColumn string
	if groupedCount > 0 && groupingCount+groupedCount <= len(allCols) {
		aggColumn = strings.ToUpper(allCols[groupingCount])
	}

	rts := md.RecordTypesForIndex(idx)
	rtNames := make([]string, len(rts))
	for i, rt := range rts {
		rtNames[i] = rt.Name
	}

	return cascades.NewAggregateIndexMatchCandidate(
		idx.Name,
		rtNames,
		groupCols,
		aggFunc,
		aggColumn,
	)
}

// tryVectorIndexCandidate builds a VectorIndexScanMatchCandidate for a VECTOR
// (HNSW) index, or nil if the index is not a vector index. columnNames are all
// index columns (partition prefix + the vector column); partitionCount is the
// KeyWithValue split point; the metric comes from the HNSW options.
func tryVectorIndexCandidate(idx *recordlayer.Index, md *recordlayer.RecordMetaData) *cascades.VectorIndexScanMatchCandidate {
	if idx.Type != recordlayer.IndexTypeVector || idx.RootExpression == nil {
		return nil
	}
	cols := idx.RootExpression.FieldNames()
	if len(cols) == 0 {
		return nil
	}
	upperCols := make([]string, len(cols))
	for i, col := range cols {
		upperCols[i] = strings.ToUpper(col)
	}
	partitionCount := 0
	if kwv, ok := idx.RootExpression.(*recordlayer.KeyWithValueExpression); ok {
		partitionCount = kwv.SplitPoint()
	}
	metric, ok := vectorMetricOperator(idx.Options[recordlayer.IndexOptionVectorMetric])
	if !ok {
		// Unrecognized metric (corrupt or newer-version metadata). Don't build
		// a candidate with a wrong default metric; without the candidate the
		// QUALIFY distance predicate stays uncompensatable and the query fails
		// to plan rather than returning wrong-metric results.
		return nil
	}

	rts := md.RecordTypesForIndex(idx)
	rtNames := make([]string, len(rts))
	for i, rt := range rts {
		rtNames[i] = rt.Name
	}
	var pkCols []string
	if len(rts) > 0 && rts[0].PrimaryKey != nil {
		pk := rts[0].PrimaryKey.FieldNames()
		pkCols = make([]string, len(pk))
		for i, col := range pk {
			pkCols[i] = strings.ToUpper(col)
		}
	}

	return cascades.NewVectorIndexScanMatchCandidate(
		idx.Name, rtNames, upperCols, partitionCount, metric,
		values.UnknownType, idx.IsUnique(), pkCols,
	)
}

// vectorMetricOperator maps the stored HNSW metric option (Java Metric enum
// name) to the cascades DistanceOperator used by the distance placeholder. An
// absent option defaults to Euclidean, matching Java's
// VectorIndexExpansionVisitor (`getOrDefault(HNSW_METRIC, Config.DEFAULT_METRIC)`
// where DEFAULT_METRIC == EUCLIDEAN_METRIC). It returns ok=false for an
// unrecognized non-empty metric: Java throws there; we instead skip the
// candidate so a corrupt or newer-version metric never silently maps to
// Euclidean and serves the wrong distance.
func vectorMetricOperator(name string) (values.DistanceOperator, bool) {
	switch name {
	case "", "EUCLIDEAN_METRIC", "euclidean":
		return values.DistanceEuclidean, true
	case "EUCLIDEAN_SQUARE_METRIC":
		return values.DistanceEuclideanSquare, true
	case "COSINE_METRIC", "cosine":
		return values.DistanceCosine, true
	case "DOT_PRODUCT_METRIC", "inner_product":
		return values.DistanceDotProduct, true
	default:
		return values.DistanceEuclidean, false
	}
}

func deriveColumnsFromPlan(plan plans.RecordQueryPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	if md == nil {
		return nil
	}
	if proj, ok := plan.(*plans.RecordQueryProjectionPlan); ok {
		return deriveColumnsFromProjection(proj, md)
	}
	if agg, ok := plan.(*plans.RecordQueryStreamingAggregationPlan); ok {
		return deriveColumnsFromAggregation(agg, md)
	}
	if aggIdx, ok := plan.(*plans.RecordQueryAggregateIndexPlan); ok {
		return deriveColumnsFromAggregateIndex(aggIdx, md)
	}
	if mi, ok := plan.(*plans.RecordQueryMultiIntersectionOnValuesPlan); ok {
		return deriveColumnsFromMultiIntersection(mi, md)
	}
	if nlj, ok := plan.(*plans.RecordQueryNestedLoopJoinPlan); ok {
		return deriveColumnsFromJoin(nlj, md)
	}
	if fm, ok := plan.(*plans.RecordQueryFlatMapPlan); ok {
		return deriveColumnsFromFlatMap(fm, md)
	}
	if u := findUnionPlan(plan); u != nil {
		return deriveColumnsFromPlan(u[0], md)
	}
	if ip, ok := plan.(innerPlan); ok {
		return deriveColumnsFromPlan(ip.GetInner(), md)
	}
	// Leaf plan: either a primary-key scan or an index scan. Both
	// carry GetRecordTypes(); the index scan's executor fetches the
	// full record via indexFetchCursor, so all columns are available.
	var recordTypes []string
	if scan := findScanPlan(plan); scan != nil {
		recordTypes = scan.GetRecordTypes()
	} else if idxPlan := findIndexPlan(plan); idxPlan != nil {
		recordTypes = idxPlan.GetRecordTypes()
	}
	if len(recordTypes) == 0 {
		return nil
	}
	rt := md.GetRecordType(recordTypes[0])
	if rt == nil || rt.Descriptor == nil {
		return nil
	}
	fields := rt.Descriptor.Fields()
	cols := make([]executor.ColumnDef, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		nullable := api.ColumnNullable
		if fd.Cardinality() == protoreflect.Required {
			nullable = api.ColumnNoNulls
		}
		cols[i] = executor.ColumnDef{
			Name:     strings.ToUpper(string(fd.Name())),
			TypeName: protoKindToTypeName(fd.Kind()),
			Nullable: nullable,
		}
	}
	return cols
}

type innerPlan interface {
	GetInner() plans.RecordQueryPlan
}

func findScanPlan(p plans.RecordQueryPlan) *plans.RecordQueryScanPlan {
	for {
		if s, ok := p.(*plans.RecordQueryScanPlan); ok {
			return s
		}
		if ip, ok := p.(innerPlan); ok {
			p = ip.GetInner()
		} else {
			return nil
		}
	}
}

// findIndexPlan walks through innerPlan wrappers (filters, type
// filters, etc.) to find a leaf RecordQueryIndexPlan.
func findIndexPlan(p plans.RecordQueryPlan) *plans.RecordQueryIndexPlan {
	for {
		if idx, ok := p.(*plans.RecordQueryIndexPlan); ok {
			return idx
		}
		if ip, ok := p.(innerPlan); ok {
			p = ip.GetInner()
		} else {
			return nil
		}
	}
}

// findLeafDescriptor locates the protoreflect.MessageDescriptor for
// the record type at the leaf of the plan tree. Works for both
// primary-key scans (RecordQueryScanPlan) and secondary-index scans
// (RecordQueryIndexPlan).
func findLeafDescriptor(p plans.RecordQueryPlan, md *recordlayer.RecordMetaData) protoreflect.MessageDescriptor {
	var recordTypes []string
	if scan := findScanPlan(p); scan != nil {
		recordTypes = scan.GetRecordTypes()
	} else if idx := findIndexPlan(p); idx != nil {
		recordTypes = idx.GetRecordTypes()
	}
	if len(recordTypes) == 0 {
		return nil
	}
	rt := md.GetRecordType(recordTypes[0])
	if rt == nil {
		return nil
	}
	return rt.Descriptor
}

// allLeafDescriptors collects the record-type descriptors of EVERY scan /
// index leaf reachable from p — both sides of a join. findLeafDescriptor
// only follows the single GetInner() chain and so misses the other join
// leg, which left a projected column from that leg (e.g. `o.total` in
// `SELECT u.name, o.total FROM Users u, Orders o ...`) with no descriptor
// to resolve its type against → reported as UNKNOWN. Resolving each
// projected column against all leaves recovers the correct column type.
func allLeafDescriptors(p plans.RecordQueryPlan, md *recordlayer.RecordMetaData) []protoreflect.MessageDescriptor {
	var out []protoreflect.MessageDescriptor
	seen := make(map[protoreflect.MessageDescriptor]struct{})
	var walk func(n plans.RecordQueryPlan)
	walk = func(n plans.RecordQueryPlan) {
		if n == nil {
			return
		}
		var rts []string
		switch leaf := n.(type) {
		case *plans.RecordQueryScanPlan:
			rts = leaf.GetRecordTypes()
		case *plans.RecordQueryIndexPlan:
			rts = leaf.GetRecordTypes()
		}
		// RecordQueryAggregateIndexPlan is intentionally omitted: aggregate
		// results are typed by deriveColumnsFromAggregateIndex, never by the
		// projection path that calls this. Add a case here if that changes.
		for _, name := range rts {
			if rt := md.GetRecordType(name); rt != nil && rt.Descriptor != nil {
				if _, dup := seen[rt.Descriptor]; !dup {
					seen[rt.Descriptor] = struct{}{}
					out = append(out, rt.Descriptor)
				}
			}
		}
		for _, c := range n.GetChildren() {
			walk(c)
		}
	}
	walk(p)
	return out
}

// descriptorForColumn picks the leaf descriptor that defines the given
// (possibly qualified) column. A projection over a join can reference
// same-named columns from different legs, so resolving every column against
// descs[0] (or first-match) mis-types the far leg. Resolution order:
//  1. the unique leaf descriptor that has the bare field;
//  2. among several, the leg whose record-type name matches the column's
//     qualifier (covers unqualified / table-name-qualified references);
//  3. otherwise the first match — deterministic. The genuinely ambiguous case
//     (a SQL *alias* qualifying same-named columns of DIFFERENT types across
//     legs) can't be resolved here: the physical plan's leaves carry record-
//     type names, not the query aliases, so the alias→type map is gone.
//     Correctly typing that case needs the value-level type derivation that
//     today leaves the FieldValue type UNKNOWN (the same gap that forces this
//     descriptor lookup in the first place). Returns nil when no leg has it.
func descriptorForColumn(name string, descs []protoreflect.MessageDescriptor) protoreflect.MessageDescriptor {
	ref := parseColRef(name)
	bare := protoreflect.Name(ref.bare())
	var matches []protoreflect.MessageDescriptor
	for _, d := range descs {
		if d.Fields().ByName(bare) != nil {
			matches = append(matches, d)
		}
	}
	if len(matches) <= 1 {
		if len(matches) == 1 {
			return matches[0]
		}
		return nil
	}
	if ref.table != "" {
		for _, d := range matches {
			if strings.EqualFold(string(d.Name()), ref.table) {
				return d
			}
		}
	}
	return matches[0]
}

func deriveColumnsFromAggregateIndex(aggIdx *plans.RecordQueryAggregateIndexPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	groupCols := aggIdx.GetGroupCols()
	aggCol := aggIdx.GetAggColumn()
	aggFunc := aggIdx.GetAggregateFunction()

	var desc protoreflect.MessageDescriptor
	if md != nil {
		rtName := aggIdx.GetRecordTypeName()
		if rt := md.GetRecordType(rtName); rt != nil && rt.Descriptor != nil {
			desc = rt.Descriptor
		}
	}

	cols := make([]executor.ColumnDef, 0, len(groupCols)+1)
	for _, gc := range groupCols {
		typeName := "STRING"
		if desc != nil {
			if t := protoFieldTypeName(desc, gc); t != "UNKNOWN" {
				typeName = t
			}
		}
		cols = append(cols, executor.ColumnDef{
			Name:     gc,
			TypeName: typeName,
			Nullable: api.ColumnNullable,
		})
	}

	var aggName string
	if aggCol == "" {
		aggName = aggFunc + "(*)"
	} else {
		aggName = aggFunc + "(" + aggCol + ")"
	}
	aggTypeName := "BIGINT"
	if aggCol != "" && desc != nil {
		if t := protoFieldTypeName(desc, aggCol); t != "UNKNOWN" {
			aggTypeName = t
		}
	}
	cols = append(cols, executor.ColumnDef{
		Name:     aggName,
		TypeName: aggTypeName,
		Nullable: api.ColumnNullable,
	})
	return cols
}

// deriveColumnsFromMultiIntersection derives result columns for a
// multi-aggregate intersection plan. The plan's result value is a record
// constructor whose field names are the output columns (grouping columns
// followed by one aggregate column per intersected stream). Grouping-column
// types resolve against the base record type; aggregate columns default to
// BIGINT (mirroring deriveColumnsFromAggregateIndex), with COUNT pinned to
// BIGINT and other column aggregates resolved against the descriptor.
func deriveColumnsFromMultiIntersection(mi *plans.RecordQueryMultiIntersectionOnValuesPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	rc, ok := mi.GetResultValue().(*values.RecordConstructorValue)
	if !ok {
		return nil
	}

	var desc protoreflect.MessageDescriptor
	if md != nil {
		for _, child := range mi.GetChildren() {
			if agg, ok := child.(*plans.RecordQueryAggregateIndexPlan); ok {
				if rt := md.GetRecordType(agg.GetRecordTypeName()); rt != nil && rt.Descriptor != nil {
					desc = rt.Descriptor
					break
				}
			}
		}
	}

	cols := make([]executor.ColumnDef, 0, len(rc.Fields))
	for _, f := range rc.Fields {
		name := strings.ToUpper(f.Name)
		// Aggregate columns are flowed under "FUNC(col)" / "FUNC(*)"; a
		// grouping column is a plain field name. Resolve grouping columns
		// against the record type, default aggregates to BIGINT.
		typeName := "BIGINT"
		if !strings.Contains(name, "(") && desc != nil {
			if t := protoFieldTypeName(desc, name); t != "UNKNOWN" {
				typeName = t
			}
		}
		cols = append(cols, executor.ColumnDef{
			Name:     name,
			TypeName: typeName,
			Nullable: api.ColumnNullable,
		})
	}
	return cols
}

func deriveColumnsFromProjection(proj *plans.RecordQueryProjectionPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	// A projection over a join references columns from MULTIPLE record types,
	// so resolve each column's type against every join leaf, not just the
	// first one (the single-leaf lookup left the other leg's columns UNKNOWN).
	descs := allLeafDescriptors(proj.GetInner(), md)
	aliases := proj.GetAliases()
	cols := make([]executor.ColumnDef, len(proj.GetProjections()))
	for i, v := range proj.GetProjections() {
		var name string
		if fv, ok := v.(*values.FieldValue); ok {
			if fv.Child != nil {
				name = values.ExplainValue(v)
			} else {
				name = fv.Field
			}
		} else {
			name = values.ExplainValue(v)
		}
		var label string
		if i < len(aliases) && aliases[i] != "" {
			label = strings.ToUpper(aliases[i])
		} else if _, isField := v.(*values.FieldValue); !isField {
			label = fmt.Sprintf("_%d", i)
		}
		// Resolve THIS column against the leg that defines it (a join
		// projects same-named columns from different legs; the qualifier
		// disambiguates). Falling back to descs[0] for non-FieldValue
		// expressions keeps the prior aggregate-operand behaviour.
		colDesc := descriptorForColumn(name, descs)
		typeDesc := colDesc
		if typeDesc == nil && len(descs) > 0 {
			typeDesc = descs[0]
		}
		typeName := valueTypeName(v, typeDesc)
		if typeName == "" && colDesc != nil {
			typeName = protoFieldTypeName(colDesc, name)
		}
		if typeName == "" {
			typeName = "UNKNOWN"
		}
		// Use the alias as the datum lookup key (Name) when available.
		// executeProjection stores values under both the original name
		// and the alias, so the alias is a valid lookup key and gives
		// CTE consumers the column name they reference.
		colName := strings.ToUpper(name)
		if label != "" {
			colName = label
		}
		// Display label — what ResultSetMetaData.getColumnLabel returns and
		// what database/sql Rows.Columns() surfaces to the caller. For an
		// unaliased field reference this is the UNQUALIFIED field name,
		// matching Java: `SELECT u.name` over a join yields column NAME, not
		// U.NAME. The datum key (colName/Name) stays qualified — a join
		// projects same-named columns from different legs and the qualifier
		// disambiguates the lookup — but the qualifier must never leak into
		// the user-visible metadata.
		displayLabel := label
		if label == "" {
			if fv, isField := v.(*values.FieldValue); isField && fv.Field != "" {
				// fv.Field is qualified ("U.NAME") for a join projection but
				// bare ("NAME") for a single source; the user-visible label is
				// always the bare column, matching Java.
				displayLabel = strings.ToUpper(parseColRef(fv.Field).bare())
			}
		}
		nullable := api.ColumnNullable
		if colDesc != nil {
			if fd := colDesc.Fields().ByName(protoreflect.Name(parseColRef(name).bare())); fd != nil && fd.Cardinality() == protoreflect.Required {
				nullable = api.ColumnNoNulls
			}
		}
		cols[i] = executor.ColumnDef{
			Name:     colName,
			Label:    displayLabel,
			TypeName: typeName,
			Nullable: nullable,
		}
	}
	return cols
}

func deriveColumnsFromAggregation(agg *plans.RecordQueryStreamingAggregationPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	desc := findLeafDescriptor(agg.GetInner(), md)
	return buildAggColumns(agg.GetGroupingKeys(), agg.GetAggregates(), desc)
}

type multiInnerPlan interface {
	GetInners() []plans.RecordQueryPlan
}

func findUnionPlan(p plans.RecordQueryPlan) []plans.RecordQueryPlan {
	for {
		if mi, ok := p.(multiInnerPlan); ok {
			inners := mi.GetInners()
			if len(inners) > 0 {
				return inners
			}
			return nil
		}
		if ip, ok := p.(innerPlan); ok {
			p = ip.GetInner()
		} else {
			return nil
		}
	}
}

func deriveColumnsFromJoin(nlj *plans.RecordQueryNestedLoopJoinPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	outerCols := deriveColumnsFromPlan(nlj.GetOuter(), md)
	if nlj.GetJoinType() == plans.JoinExists || nlj.GetJoinType() == plans.JoinNotExists {
		return outerCols
	}
	innerCols := deriveColumnsFromPlan(nlj.GetInner(), md)
	if outerCols == nil && innerCols == nil {
		return nil
	}

	outerAlias := strings.ToUpper(nlj.GetOuterAlias())
	innerAlias := strings.ToUpper(nlj.GetInnerAlias())

	firstCols, secondCols := outerCols, innerCols
	firstAlias, secondAlias := outerAlias, innerAlias
	if joinResultValueIsReversed(nlj.GetResultValue(), outerAlias, innerAlias) {
		firstCols, secondCols = innerCols, outerCols
		firstAlias, secondAlias = innerAlias, outerAlias
	}

	return qualifyAndMergeColumns(firstCols, secondCols, firstAlias, secondAlias)
}

func deriveColumnsFromFlatMap(fm *plans.RecordQueryFlatMapPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	outerCols := deriveColumnsFromPlan(fm.GetOuter(), md)
	innerCols := deriveColumnsFromPlan(fm.GetInner(), md)
	if outerCols == nil && innerCols == nil {
		return nil
	}

	outerAlias := strings.ToUpper(fm.GetOuterAlias().Name())
	innerAlias := strings.ToUpper(fm.GetInnerAlias().Name())

	firstCols, secondCols := outerCols, innerCols
	firstAlias, secondAlias := outerAlias, innerAlias
	if joinResultValueIsReversed(fm.GetResultValue(), outerAlias, innerAlias) {
		firstCols, secondCols = innerCols, outerCols
		firstAlias, secondAlias = innerAlias, outerAlias
	}

	return qualifyAndMergeColumns(firstCols, secondCols, firstAlias, secondAlias)
}

// joinResultValueIsReversed checks whether the plan's resultValue
// indicates that the SQL-level column order is opposite to the physical
// outer/inner assignment. The translator builds the binary join seed in SQL
// order [outer, inner]; comparing the SQL-first leg against the physical
// outerAlias tells us whether columns need to be emitted in reversed order.
//
// The SQL-first leg is carried by the source-anchored RecordConstructorValue
// (AnchoredJoin) — its FIELDS are declared in SQL column order (outer leg's
// columns first), so the SQL-first leg is the correlation of the FIRST field's
// anchored leg QOV. (The retired opaque merge seed it replaced was removed in
// RFC-077 7.6.)
func joinResultValueIsReversed(rv values.Value, physOuterAlias, physInnerAlias string) bool {
	if first, ok := anchoredJoinFirstLeg(rv); ok {
		return first != "" && first == physInnerAlias
	}
	return false
}

// anchoredJoinFirstLeg returns the upper-cased correlation name of the SQL-first
// (outer) leg of a source-anchored join result value (RFC-077 7.6): the leg QOV
// the first field is anchored to. Reports false for any other value shape. The
// first field is FieldValue(QOV(outerLeg), col); a nested-join leg's field value
// may be a FieldValue whose child is another anchored RC, so descend the leftmost
// FieldValue chain until a QuantifiedObjectValue is found.
func anchoredJoinFirstLeg(rv values.Value) (string, bool) {
	rc, ok := rv.(*values.RecordConstructorValue)
	if !ok || !rc.AnchoredJoin || len(rc.Fields) == 0 {
		return "", false
	}
	cur := rc.Fields[0].Value
	for {
		switch v := cur.(type) {
		case *values.QuantifiedObjectValue:
			return strings.ToUpper(v.Correlation.Name()), true
		case *values.FieldValue:
			if v.Child == nil {
				return "", true
			}
			cur = v.Child
		default:
			// e.g. a nested anchored RC reached directly — recurse into its first leg.
			if inner, ok := anchoredJoinFirstLeg(cur); ok {
				return inner, true
			}
			return "", true
		}
	}
}

func qualifyAndMergeColumns(firstCols, secondCols []executor.ColumnDef, firstAlias, secondAlias string) []executor.ColumnDef {
	cols := make([]executor.ColumnDef, 0, len(firstCols)+len(secondCols))
	for _, c := range firstCols {
		qual := c
		if firstAlias != "" && !parseColRef(c.Name).isQualified() {
			// Name carries the FROM-alias qualifier so same-named columns
			// across legs stay distinct as datum-map keys; the display Label
			// stays the UNQUALIFIED column name to match Java — `SELECT *`
			// over a join yields bare column names (with duplicates), never
			// U.NAME (verified against fdb-relational 4.11.1.0).
			if qual.Label == "" {
				qual.Label = strings.ToUpper(c.Name)
			}
			qual.Name = firstAlias + "." + strings.ToUpper(c.Name)
		}
		cols = append(cols, qual)
	}
	for _, c := range secondCols {
		qual := c
		if secondAlias != "" && !parseColRef(c.Name).isQualified() {
			if qual.Label == "" {
				qual.Label = strings.ToUpper(c.Name)
			}
			qual.Name = secondAlias + "." + strings.ToUpper(c.Name)
		}
		cols = append(cols, qual)
	}
	return cols
}

func buildAggColumns(
	groupKeys []values.Value,
	aggregates []expressions.AggregateSpec,
	desc protoreflect.MessageDescriptor,
) []executor.ColumnDef {
	cols := make([]executor.ColumnDef, 0, len(groupKeys)+len(aggregates))
	for _, k := range groupKeys {
		name := values.ExplainValue(k)
		typeName := "UNKNOWN"
		if desc != nil {
			typeName = protoFieldTypeName(desc, name)
		}
		nullable := api.ColumnNullable
		if desc != nil {
			if fd := desc.Fields().ByName(protoreflect.Name(parseColRef(name).bare())); fd != nil && fd.Cardinality() == protoreflect.Required {
				nullable = api.ColumnNoNulls
			}
		}
		cols = append(cols, executor.ColumnDef{
			Name:     strings.ToUpper(name),
			TypeName: typeName,
			Nullable: nullable,
		})
	}
	for _, a := range aggregates {
		name := aggregateSpecName(a)
		typeName := aggregateResultType(a, desc)
		cols = append(cols, executor.ColumnDef{
			Name:     strings.ToUpper(name),
			TypeName: typeName,
			Nullable: api.ColumnNullable,
		})
	}
	return cols
}

func aggregateSpecName(a expressions.AggregateSpec) string {
	operand := aggOperandName(a)
	switch a.Function {
	case expressions.AggCount:
		return "COUNT(" + operand + ")"
	case expressions.AggSum:
		return "SUM(" + operand + ")"
	case expressions.AggAvg:
		return "AVG(" + operand + ")"
	case expressions.AggMin:
		return "MIN(" + operand + ")"
	case expressions.AggMax:
		return "MAX(" + operand + ")"
	default:
		return "AGG(" + operand + ")"
	}
}

func aggOperandName(a expressions.AggregateSpec) string {
	if cv, ok := a.Operand.(*values.ConstantValue); ok && cv.Value == nil {
		return "*"
	}
	if a.OperandName != "" {
		return strings.ReplaceAll(a.OperandName, " ", "")
	}
	return values.ExplainValue(a.Operand)
}

// aggregateResultType derives the SQL type name of an aggregate's result
// column. It routes through valueTypeName so the function-determined facts
// (AVG→DOUBLE, COUNT→BIGINT) have a SINGLE source — AggregateValue.Type() —
// rather than a second hardcoded copy that could silently drift. SUM/MIN/MAX
// stay operand-derived (resolved against desc inside valueTypeName). Mirrors
// Java's per-operator resultTypeCode.
func aggregateResultType(a expressions.AggregateSpec, desc protoreflect.MessageDescriptor) string {
	op := valueAggOp(a.Function)
	if op == values.AggInvalid {
		return "UNKNOWN"
	}
	// Construct the node directly (not NewAggregateValue, which panics on
	// shape mismatches) purely to derive its result type via valueTypeName.
	return valueTypeName(&values.AggregateValue{Op: op, Operand: a.Operand}, desc)
}

// valueAggOp bridges the planner's expressions.AggregateFunction to the
// values.AggregateOp used by AggregateValue, so aggregate result-type
// derivation has one home.
func valueAggOp(f expressions.AggregateFunction) values.AggregateOp {
	switch f {
	case expressions.AggCount:
		return values.AggCount
	case expressions.AggSum:
		return values.AggSum
	case expressions.AggMin:
		return values.AggMin
	case expressions.AggMax:
		return values.AggMax
	case expressions.AggAvg:
		return values.AggAvg
	}
	return values.AggInvalid
}

// valueTypeName resolves the SQL type name for a Value. For
// AggregateValue nodes, it inspects the typed Op field instead of
// string-parsing the ExplainValue output. For plain field references,
// it falls through and returns "".
func valueTypeName(v values.Value, desc protoreflect.MessageDescriptor) string {
	// Arithmetic result type is the numeric promotion of its operand types.
	// The operand FieldValues aren't type-bound at projection time, so resolve
	// them against the record descriptor here rather than via Value.Type()
	// (which defaults to BIGINT for unbound operands).
	if arith, ok := v.(*values.ArithmeticValue); ok {
		if n := arithTypeNameViaDesc(arith, desc); n != "" {
			return n
		}
	}
	if av, ok := v.(*values.AggregateValue); ok {
		// SUM/MIN/MAX inherit the operand type, resolved against the record
		// descriptor (av.Type() defaults unbound operands to BIGINT, so the
		// descriptor is the reliable source for these). AVG (→DOUBLE) and
		// COUNT/COUNT(*) (→BIGINT) are function-determined: fall through to the
		// v.Type() block below so AggregateValue.Type() is the single source of
		// truth and the two SQL-name derivations cannot drift.
		switch av.Op {
		case values.AggSum, values.AggMin, values.AggMax:
			if av.Operand != nil && desc != nil {
				operandName := values.ExplainValue(av.Operand)
				if t := protoFieldTypeName(desc, operandName); t != "UNKNOWN" {
					return t
				}
			}
			return "BIGINT"
		}
	}
	if t := v.Type(); t != nil {
		switch t.Code() {
		case values.TypeCodeInt:
			return "INTEGER"
		case values.TypeCodeLong:
			return "BIGINT"
		case values.TypeCodeFloat:
			return "FLOAT"
		case values.TypeCodeDouble:
			return "DOUBLE"
		case values.TypeCodeString:
			return "STRING"
		case values.TypeCodeBoolean:
			return "BOOLEAN"
		case values.TypeCodeDate:
			return "DATE"
		case values.TypeCodeTimestamp:
			return "TIMESTAMP"
		}
	}
	return ""
}

// arithTypeNameViaDesc resolves an arithmetic value's result type NAME by
// numeric promotion (DOUBLE > FLOAT > BIGINT > INTEGER) of its operand type
// names, resolving FieldValue operands against the record descriptor. Returns
// "" when no operand type can be resolved (caller falls back).
func arithTypeNameViaDesc(a *values.ArithmeticValue, desc protoreflect.MessageDescriptor) string {
	return widerNumericTypeName(
		operandTypeNameViaDesc(a.Left, desc),
		operandTypeNameViaDesc(a.Right, desc),
	)
}

func operandTypeNameViaDesc(v values.Value, desc protoreflect.MessageDescriptor) string {
	switch t := v.(type) {
	case *values.FieldValue:
		if desc != nil {
			if n := protoFieldTypeName(desc, t.Field); n != "UNKNOWN" {
				return n
			}
		}
		// The operand may belong to a different join leg than `desc` (the
		// caller only threads the first leaf descriptor). Fall back to the
		// value's own semantic type rather than dropping it from the
		// numeric promotion (codex P2).
		return valueTypeName(v, desc)
	case *values.ArithmeticValue:
		return arithTypeNameViaDesc(t, desc)
	default:
		return valueTypeName(v, desc)
	}
}

// widerNumericTypeName returns the wider of two numeric SQL type names, or ""
// when neither is a recognised numeric type.
func widerNumericTypeName(a, b string) string {
	rank := func(s string) int {
		switch s {
		case "DOUBLE":
			return 4
		case "FLOAT":
			return 3
		case "BIGINT":
			return 2
		case "INTEGER":
			return 1
		}
		return 0
	}
	ra, rb := rank(a), rank(b)
	if ra == 0 && rb == 0 {
		return ""
	}
	if ra >= rb {
		return a
	}
	return b
}

func protoFieldTypeName(desc protoreflect.MessageDescriptor, name string) string {
	fields := desc.Fields()
	fd := fields.ByName(protoreflect.Name(parseColRef(name).bare()))
	if fd != nil {
		// UUID columns are stored as the tuple_fields.UUID message and reported
		// as JDBC's catch-all OTHER type name (matches Java's java.sql.Types.OTHER).
		if fd.Kind() == protoreflect.MessageKind {
			if msg := fd.Message(); msg != nil && string(msg.FullName()) == functions.UUIDProtoMessageName {
				return "OTHER"
			}
		}
		return protoKindToTypeName(fd.Kind())
	}
	return "UNKNOWN"
}

func protoKindToTypeName(k protoreflect.Kind) string {
	switch k {
	case protoreflect.BoolKind:
		return "BOOLEAN"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return "INTEGER"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return "BIGINT"
	case protoreflect.FloatKind:
		return "FLOAT"
	case protoreflect.DoubleKind:
		return "DOUBLE"
	case protoreflect.StringKind:
		return "STRING"
	case protoreflect.BytesKind:
		// JDBC type name for SQL binary columns is BINARY (matches Java
		// fdb-relational). The DDL keyword stays "BYTES".
		return "BINARY"
	default:
		return "UNKNOWN"
	}
}

// cascadesRows wraps a RecordLayerResultSet as driver.Rows.
type cascadesRows struct {
	rs *executor.RecordLayerResultSet
}

func newCascadesRows(rs *executor.RecordLayerResultSet) *cascadesRows {
	return &cascadesRows{rs: rs}
}

func (r *cascadesRows) Columns() []string {
	md := r.rs.MetaData()
	cols := make([]string, md.ColumnCount())
	for i := range cols {
		cols[i], _ = md.ColumnLabel(i + 1)
	}
	return cols
}

func (r *cascadesRows) Close() error {
	return r.rs.Close()
}

func (r *cascadesRows) ColumnTypeDatabaseTypeName(index int) string {
	md := r.rs.MetaData()
	name, err := md.ColumnTypeName(index + 1)
	if err != nil {
		return ""
	}
	return name
}

func (r *cascadesRows) ColumnTypeScanType(index int) reflect.Type {
	typeName := r.ColumnTypeDatabaseTypeName(index)
	switch typeName {
	case "BIGINT":
		return reflect.TypeOf((*int64)(nil)).Elem()
	case "INTEGER":
		return reflect.TypeOf((*int32)(nil)).Elem()
	case "DOUBLE":
		return reflect.TypeOf((*float64)(nil)).Elem()
	case "FLOAT":
		return reflect.TypeOf((*float32)(nil)).Elem()
	case "STRING":
		return reflect.TypeOf((*string)(nil)).Elem()
	case "BOOLEAN":
		return reflect.TypeOf((*bool)(nil)).Elem()
	case "BYTES", "BINARY":
		return reflect.TypeOf((*[]byte)(nil)).Elem()
	case "DATE", "TIMESTAMP":
		return reflect.TypeOf((*time.Time)(nil)).Elem()
	default:
		return reflect.TypeOf((*any)(nil)).Elem()
	}
}

func (r *cascadesRows) ColumnTypeNullable(index int) (nullable, ok bool) {
	md := r.rs.MetaData()
	n, err := md.ColumnNullable(index + 1)
	if err != nil {
		return true, true
	}
	return n != api.ColumnNoNulls, true
}

func (r *cascadesRows) ColumnTypeLength(index int) (length int64, ok bool) {
	typeName := r.ColumnTypeDatabaseTypeName(index)
	switch typeName {
	case "STRING", "BYTES", "BINARY":
		return math.MaxInt64, true
	case "DATE":
		return 10, true // "2006-01-02"
	case "TIMESTAMP":
		return 19, true // "2006-01-02 15:04:05"
	}
	return 0, false
}

func (r *cascadesRows) ColumnTypePrecisionScale(index int) (precision, scale int64, ok bool) {
	return 0, 0, false
}

func (r *cascadesRows) Next(dest []driver.Value) error {
	if !r.rs.Next() {
		if err := r.rs.Err(); err != nil {
			return translateExecError(err)
		}
		return io.EOF
	}
	for i := range dest {
		v, err := r.rs.Object(i + 1)
		if err != nil {
			return err
		}
		dest[i] = v
	}
	return nil
}

func findDistinctAggregate(op logical.LogicalOperator) string {
	if op == nil {
		return ""
	}
	if agg, ok := op.(*logical.LogicalAggregate); ok && agg.HasDistinctAggregate {
		return "DISTINCT aggregates are not supported"
	}
	for _, ch := range op.Children() {
		if msg := findDistinctAggregate(ch); msg != "" {
			return msg
		}
	}
	return ""
}

// findFullOuterWithExists rejects FULL OUTER JOIN combined with an
// EXISTS / NOT EXISTS subquery in the same WHERE. The join+EXISTS
// flatten path (translateJoinWithExists) builds a semi-join shape that
// cannot carry the FULL-outer drain, so such a query would otherwise be
// silently mistranslated to an inner join. FULL OUTER is a Go-only query
// extension; Java's SQL layer has no outer joins at all.
func findFullOuterWithExists(op logical.LogicalOperator) string {
	if op == nil {
		return ""
	}
	if f, ok := op.(*logical.LogicalFilter); ok && len(f.ExistsSubqueries) > 0 {
		if j, ok := f.Input.(*logical.LogicalJoin); ok && j.Kind == logical.JoinFull {
			return "FULL OUTER JOIN combined with an EXISTS subquery is not supported"
		}
	}
	for _, ch := range op.Children() {
		if msg := findFullOuterWithExists(ch); msg != "" {
			return msg
		}
	}
	return ""
}

// resolveQualifiedTableNames walks the logical plan tree and resolves
// schema-qualified table names (schema.table → table) in LogicalScan
// nodes. Mirrors Java's SemanticAnalyzer.tableExists qualifier validation.
func resolveQualifiedTableNames(op logical.LogicalOperator, schemaName string) error {
	if op == nil {
		return nil
	}
	if scan, ok := op.(*logical.LogicalScan); ok {
		resolved, err := functions.ResolveQualifiedTableName(scan.Table, schemaName)
		if err != nil {
			return err
		}
		scan.Table = resolved
	}
	if ins, ok := op.(*logical.LogicalInsert); ok {
		resolved, err := functions.ResolveQualifiedTableName(ins.Table, schemaName)
		if err != nil {
			return err
		}
		ins.Table = resolved
	}
	if del, ok := op.(*logical.LogicalDelete); ok {
		resolved, err := functions.ResolveQualifiedTableName(del.Target, schemaName)
		if err != nil {
			return err
		}
		del.Target = resolved
	}
	if upd, ok := op.(*logical.LogicalUpdate); ok {
		resolved, err := functions.ResolveQualifiedTableName(upd.Target, schemaName)
		if err != nil {
			return err
		}
		upd.Target = resolved
	}
	for _, ch := range op.Children() {
		if err := resolveQualifiedTableNames(ch, schemaName); err != nil {
			return err
		}
	}
	return nil
}

func validateTablesAndColumns(op logical.LogicalOperator, md *recordlayer.RecordMetaData) error {
	cteNames := collectCTENames(op)
	return validateTablesAndColumnsInner(op, md, cteNames)
}

func validateTablesAndColumnsInner(op logical.LogicalOperator, md *recordlayer.RecordMetaData, cteNames map[string]bool) error {
	if op == nil {
		return nil
	}
	if scan, ok := op.(*logical.LogicalScan); ok {
		if !cteNames[strings.ToUpper(scan.Table)] {
			rt := md.GetRecordType(scan.Table)
			if rt == nil {
				return api.NewErrorf(api.ErrCodeUndefinedTable, "table %q does not exist", scan.Table)
			}
		}
	}
	if proj, ok := op.(*logical.LogicalProject); ok && !hasJoin(op) && !hasAggregate(op) {
		scan := findLogicalScan(op)
		if scan != nil && !cteNames[strings.ToUpper(scan.Table)] {
			rt := md.GetRecordType(scan.Table)
			if rt != nil && rt.Descriptor != nil {
				for i, col := range proj.Projections {
					if i < len(proj.IsComputed) && proj.IsComputed[i] {
						continue
					}
					if i < len(proj.ProjectedValues) && proj.ProjectedValues[i] != nil {
						continue
					}
					upper := strings.ToUpper(col)
					ref := parseColRef(upper)
					if ref.isQualified() {
						qual := ref.table
						scanName := strings.ToUpper(scan.Table)
						if scan.Alias != "" {
							scanName = strings.ToUpper(scan.Alias)
						}
						if qual != scanName {
							return api.NewErrorf(api.ErrCodeUndefinedColumn,
								"column reference with qualifier %q cannot be resolved", qual)
						}
						upper = ref.bare()
					}
					if rt.Descriptor.Fields().ByName(protoreflect.Name(upper)) == nil {
						return api.NewErrorf(api.ErrCodeUndefinedColumn, "column %q does not exist", col)
					}
				}
			}
		}
	}
	for _, child := range op.Children() {
		if err := validateTablesAndColumnsInner(child, md, cteNames); err != nil {
			return err
		}
	}
	return nil
}

func collectCTENames(op logical.LogicalOperator) map[string]bool {
	names := make(map[string]bool)
	collectCTENamesInner(op, names)
	return names
}

func collectCTENamesInner(op logical.LogicalOperator, names map[string]bool) {
	if op == nil {
		return
	}
	if cte, ok := op.(*logical.LogicalCTE); ok {
		names[strings.ToUpper(cte.Name)] = true
	}
	for _, ch := range op.Children() {
		collectCTENamesInner(ch, names)
	}
}

func hasAggregate(op logical.LogicalOperator) bool {
	if op == nil {
		return false
	}
	if _, ok := op.(*logical.LogicalAggregate); ok {
		return true
	}
	for _, ch := range op.Children() {
		if hasAggregate(ch) {
			return true
		}
	}
	return false
}

func hasJoin(op logical.LogicalOperator) bool {
	if op == nil {
		return false
	}
	if _, ok := op.(*logical.LogicalJoin); ok {
		return true
	}
	for _, ch := range op.Children() {
		if hasJoin(ch) {
			return true
		}
	}
	return false
}

func findLogicalScan(op logical.LogicalOperator) *logical.LogicalScan {
	if op == nil {
		return nil
	}
	if s, ok := op.(*logical.LogicalScan); ok {
		return s
	}
	for _, ch := range op.Children() {
		if s := findLogicalScan(ch); s != nil {
			return s
		}
	}
	return nil
}

// referencesInformationSchema walks the ANTLR parse tree and returns
// true if any table name references the INFORMATION_SCHEMA. Walks
// typed FullId → Uid nodes — no GetText on the table name.
func referencesInformationSchema(ctx antlr.Tree) bool {
	if ctx == nil {
		return false
	}
	if atom, ok := ctx.(*antlrgen.AtomTableItemContext); ok {
		if tn := atom.TableName(); tn != nil {
			if fid := tn.FullId(); fid != nil {
				for _, uid := range fid.AllUid() {
					if strings.EqualFold(functions.StripIdentifierQuotes(uid.GetText()), "INFORMATION_SCHEMA") {
						return true
					}
				}
			}
		}
	}
	for i := 0; i < ctx.GetChildCount(); i++ {
		if referencesInformationSchema(ctx.GetChild(i)) {
			return true
		}
	}
	return false
}

// findUnsupportedFunctionInParseTree walks an ANTLR expression tree
// and returns the name of the first scalar function call that isn't
// in the Cascades-safe set. Uses typed parse tree nodes — no text
// matching.
func findUnsupportedFunctionInParseTree(ctx antlr.Tree) string {
	if ctx == nil {
		return ""
	}
	switch n := ctx.(type) {
	case *antlrgen.FunctionCallExpressionAtomContext:
		if fc := n.FunctionCall(); fc != nil {
			if name := extractFunctionNameFromCall(fc); name != "" {
				if !isAllowedFunction(name) {
					return name
				}
			}
		}
	case *antlrgen.BitExpressionAtomContext:
		if bo := n.BitOperator(); bo != nil {
			boc, _ := bo.(*antlrgen.BitOperatorContext)
			if boc != nil && len(boc.AllLESS_SYMBOL()) >= 2 {
				return "<<"
			}
			if boc != nil && len(boc.AllGREATER_SYMBOL()) >= 2 {
				return ">>"
			}
		}
	}
	for i := 0; i < ctx.GetChildCount(); i++ {
		if fn := findUnsupportedFunctionInParseTree(ctx.GetChild(i)); fn != "" {
			return fn
		}
	}
	return ""
}

func extractFunctionNameFromCall(fc antlrgen.IFunctionCallContext) string {
	switch f := fc.(type) {
	case *antlrgen.ScalarFunctionCallContext:
		if f.ScalarFunctionName() != nil {
			return strings.ToUpper(f.ScalarFunctionName().GetText())
		}
	case *antlrgen.UserDefinedScalarFunctionCallContext:
		if f.UserDefinedScalarFunctionName() != nil {
			return strings.ToUpper(f.UserDefinedScalarFunctionName().GetText())
		}
	case *antlrgen.NonAggregateFunctionCallContext:
		if wf := f.NonAggregateWindowedFunction(); wf != nil {
			if wfc, ok := wf.(*antlrgen.NonAggregateWindowedFunctionContext); ok {
				switch {
				case wfc.ROW_NUMBER() != nil:
					return "ROW_NUMBER"
				case wfc.RANK() != nil:
					return "RANK"
				case wfc.DENSE_RANK() != nil:
					return "DENSE_RANK"
				case wfc.PERCENT_RANK() != nil:
					return "PERCENT_RANK"
				default:
					return "WINDOW_FUNCTION"
				}
			}
		}
	case *antlrgen.SpecificFunctionCallContext:
		if f.SpecificFunction() != nil {
			switch sf := f.SpecificFunction().(type) {
			case *antlrgen.SimpleFunctionCallContext:
				if sf.CURRENT_DATE() != nil {
					return "CURRENT_DATE"
				}
				if sf.CURRENT_TIME() != nil {
					return "CURRENT_TIME"
				}
				if sf.CURRENT_TIMESTAMP() != nil {
					return "CURRENT_TIMESTAMP"
				}
				if sf.LOCALTIME() != nil {
					return "LOCALTIME"
				}
				if sf.CURRENT_USER() != nil {
					return "CURRENT_USER"
				}
			}
		}
	}
	return ""
}

func isAllowedFunction(name string) bool {
	switch name {
	case "COUNT", "SUM", "MIN", "MAX", "AVG",
		"CASE", "CAST", "IF",
		"CURRENT_DATE", "CURRENT_TIME", "CURRENT_TIMESTAMP", "LOCALTIME",
		"CURRENT_USER":
		return true
	}
	return values.IsCascadesSafeScalarFunction(name)
}

// findUnsupportedFunctionInSelectQuery walks the ANTLR expression
// contexts in a selectQuery's projections and returns the first
// unsupported function name, or "".
func findUnsupportedFunctionInSelectQuery(sq *selectQuery) string {
	if sq == nil {
		return ""
	}
	for _, expr := range sq.projExprs {
		if fn := findUnsupportedFunctionInParseTree(expr); fn != "" {
			return fn
		}
	}
	return ""
}

// NewExplainOnlyGenerator constructs a Generator suitable for capturing
// Plan.Explain() output without executing. The returned Generator is
// backed by a zero-value EmbeddedConnection — Plan.Execute on the
// returned plans is unsupported (no FDB, no catalog, no session
// state). Used by the plan-equivalence harness (RFC-022 section 4.-1) to
// produce plan trees for diffing against Java's planner output.
//
// Catalog-aware predicate trees (buildLogicalPlanFor*WithCatalog
// paths) require non-nil RecordMetaData; this constructor always
// produces text-only logical plans. Use NewExplainOnlyGeneratorWithSchema
// to unlock the catalog-aware branch.
func NewExplainOnlyGenerator() query.Generator {
	return newCascadesGenerator(&EmbeddedConnection{})
}

// NewExplainOnlyGeneratorWithSchema is the catalog-aware companion to
// NewExplainOnlyGenerator. It parses the supplied CREATE SCHEMA
// TEMPLATE DDL into an in-memory RecordLayerSchemaTemplate (no FDB
// write), wraps it in an api.Schema bound to a synthetic database +
// schema, and seeds the connection's SchemaCache. Subsequent
// statements planned through the returned Generator route through the
// buildLogicalPlanFor*WithCatalog paths so WHERE clauses appear as
// real cascades.predicates.QueryPredicate trees in the Explain output.
//
// schemaDDL must contain exactly one CREATE SCHEMA TEMPLATE
// statement. Multiple-statement DDL or any non-CREATE-SCHEMA-TEMPLATE
// shape returns an error — callers should isolate the schema DDL from
// the SELECT/DML they intend to plan.
func NewExplainOnlyGeneratorWithSchema(schemaDDL string) (query.Generator, error) {
	tmpl, err := buildSchemaTemplateFromDDL(schemaDDL)
	if err != nil {
		return nil, err
	}
	const dbPath = "/explain"
	const schemaName = "s"
	sess := &session.Session{
		DBPath: dbPath,
		Schema: schemaName,
		SchemaCache: map[string]api.Schema{
			session.SchemaCacheKey(dbPath, schemaName): tmpl.GenerateSchema(dbPath, schemaName),
		},
	}
	return newCascadesGenerator(&EmbeddedConnection{sess: sess}), nil
}

// startsWithCreateSchemaTemplate reports whether ddl begins (after
// leading whitespace) with the case-insensitive "CREATE SCHEMA
// TEMPLATE" header. Used to decide whether buildSchemaTemplateFromDDL
// must auto-wrap a bare body.
func startsWithCreateSchemaTemplate(ddl string) bool {
	t := strings.TrimSpace(ddl)
	if len(t) < len("CREATE SCHEMA TEMPLATE") {
		return false
	}
	return strings.EqualFold(t[:len("CREATE SCHEMA TEMPLATE")], "CREATE SCHEMA TEMPLATE")
}

// buildSchemaTemplateFromDDL parses schemaDDL as a single
// CREATE SCHEMA TEMPLATE statement and builds a
// RecordLayerSchemaTemplate without performing any catalog write.
func buildSchemaTemplateFromDDL(schemaDDL string) (*metadata.RecordLayerSchemaTemplate, error) {
	wrapped := schemaDDL
	if !startsWithCreateSchemaTemplate(schemaDDL) {
		wrapped = "CREATE SCHEMA TEMPLATE auto_template " + schemaDDL
	}
	root, err := parser.Parse(wrapped)
	if err != nil {
		return nil, fmt.Errorf("parse schema DDL: %w", err)
	}
	stmts := root.Statements()
	if stmts == nil {
		return nil, fmt.Errorf("schema DDL must contain exactly one statement, got 0")
	}
	if len(stmts.AllStatement()) != 1 {
		return nil, fmt.Errorf("schema DDL must contain exactly one statement, got %d",
			len(stmts.AllStatement()))
	}
	create := stmts.AllStatement()[0].DdlStatement()
	if create == nil {
		return nil, fmt.Errorf("schema DDL must be a CREATE SCHEMA TEMPLATE statement")
	}
	cs := create.CreateStatement()
	if cs == nil {
		return nil, fmt.Errorf("schema DDL must be a CREATE SCHEMA TEMPLATE statement")
	}
	stCtx, ok := cs.(*antlrgen.CreateSchemaTemplateStatementContext)
	if !ok {
		return nil, fmt.Errorf("schema DDL must be a CREATE SCHEMA TEMPLATE statement, got %T", cs)
	}

	templateID := stCtx.SchemaTemplateId().GetText()
	b := metadata.NewSchemaTemplateBuilder().SetName(templateID)
	for _, clause := range stCtx.AllTemplateClause() {
		td := clause.TableDefinition()
		if td == nil {
			continue
		}
		// Normalize the table name the same way execCreateSchemaTemplate and
		// the column/index parsers do (StripIdentifierQuotes upper-cases
		// unquoted identifiers), so index lookups by table name match.
		tableName := functions.StripIdentifierQuotes(td.Uid().GetText())
		cols, pkCols, tdErr := parseTableDefinition(td)
		if tdErr != nil {
			return nil, fmt.Errorf("table %q: %w", tableName, tdErr)
		}
		b.AddTable(tableName, cols, pkCols)
	}
	for _, clause := range stCtx.AllTemplateClause() {
		idxDef := clause.IndexDefinition()
		if idxDef == nil {
			continue
		}
		if idxErr := parseIndexDefinition(idxDef, b); idxErr != nil {
			return nil, fmt.Errorf("index: %w", idxErr)
		}
	}
	return b.Build()
}

// explainStatement returns a trivial textual description of a parsed
// statement: the kind (SELECT / INSERT / UPDATE / DELETE / DDL / SHOW)
// followed by its source text.
func explainStatement(kind string, node interface {
	GetText() string
},
) string {
	txt := ""
	if node != nil {
		txt = node.GetText()
	}
	if txt == "" {
		return kind
	}
	return fmt.Sprintf("%s: %s", kind, txt)
}

// statementKind returns a short human-readable tag for a parsed top-
// level statement.
func statementKind(stmt antlrgen.IStatementContext) string {
	if stmt == nil {
		return "STATEMENT"
	}
	if ddl := stmt.DdlStatement(); ddl != nil {
		return "DDL"
	}
	if dml := stmt.DmlStatement(); dml != nil {
		switch {
		case dml.InsertStatement() != nil:
			return "INSERT"
		case dml.DeleteStatement() != nil:
			return "DELETE"
		case dml.UpdateStatement() != nil:
			return "UPDATE"
		}
		return "DML"
	}
	if stmt.TransactionStatement() != nil {
		return "TX"
	}
	return "STATEMENT"
}

// rowsOrEmpty returns rows or a non-nil empty driver.Rows when rows
// is nil. The driver layer expects a non-nil driver.Rows for Query-
// shaped calls.
func rowsOrEmpty(rows driver.Rows) driver.Rows {
	if rows == nil {
		return emptyRows{}
	}
	return rows
}
