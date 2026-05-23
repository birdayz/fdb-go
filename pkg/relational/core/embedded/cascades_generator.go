package embedded

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/antlr4-go/antlr/v4"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/executor"
	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
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
// statements. SELECT and DML (INSERT/UPDATE/DELETE) route through
// the Cascades planner. EXPLAIN, SHOW, DDL, and transaction
// statements are handled directly via PlanFunc wrappers around
// the connection's exec* methods.
//
// When execMode is true (set by ExecContext), DML is routed through
// execStatement rather than the Cascades pipeline, matching the
// pre-existing ExecContext behavior.
type cascadesGenerator struct {
	c        *EmbeddedConnection
	cache    *PlanCache
	execMode bool // true when called from ExecContext
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

func newCascadesGeneratorForExec(c *EmbeddedConnection) *cascadesGenerator {
	g := newCascadesGenerator(c)
	g.execMode = true
	return g
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

	// DML: route INSERT/UPDATE/DELETE through Cascades (QueryContext)
	// or through execStatement (ExecContext).
	if dml := stmt.DmlStatement(); dml != nil {
		if g.execMode {
			return g.planDDL(ctx, stmt)
		}
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

	return g.planSelectCascades(q, md)
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
func (g *cascadesGenerator) planSelectCascades(q antlrgen.IQueryContext, md *recordlayer.RecordMetaData) (query.Plan, error) {
	// Check plan cache before running the full Cascades pipeline.
	// Use the query text for hashing since we don't have the original
	// full SQL here — GetText() is stable for cache-key purposes.
	sqlText := q.GetText()
	sqlHash := QueryHash(sqlText)
	if g.cache != nil {
		if cachedPlan, cachedSubs, ok := g.cache.Get(sqlHash); ok {
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

	ref, scalarSubqueryPlans := query.TranslateToCascadesWithSubqueries(logicalOp)
	if ref == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"Cascades planner could not plan query")
	}

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	rules = append(rules, cascades.RewritingRules()...)
	rules = append(rules, cascades.MatchingRules()...)
	planCtx := buildCascadesPlanContext(md)
	planner := cascades.NewPlanner(rules, planCtx).
		WithImplementationRules(cascades.DefaultImplementationRules()).
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
		subRef := query.TranslateToCascades(ssq.Plan)
		if subRef == nil {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"Cascades planner could not plan scalar subquery")
		}
		subPlanner := cascades.NewPlanner(rules, planCtx).
			WithImplementationRules(cascades.DefaultImplementationRules()).
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

	// Cache the planned result for future queries with the same SQL.
	// Don't cache LIMIT/OFFSET queries — the limit is applied post-execution
	// and not stored in the cached plan.
	if g.cache != nil && sqlLimit < 0 && sqlOffset == 0 {
		g.cache.Put(sqlHash, sqlText, physPlan, scalarSubs)
	}
	return &cascadesPlan{
		conn:             g.c,
		md:               md,
		physicalPlan:     physPlan,
		explain:          logicalOp.Explain(""),
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
					if plan, planErr := g.planSelectCascades(q, freshMd); planErr == nil {
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

func (g *cascadesGenerator) planDML(ctx context.Context, dml antlrgen.IDmlStatementContext) (query.Plan, error) {
	c := g.c

	// Explain-only mode: no FDB available, produce logical plan text only.
	if c.sess == nil || c.sess.DB == nil {
		return g.planDMLExplainOnly(dml)
	}

	if err := c.ensureMetaData(ctx); err != nil {
		return nil, err
	}
	md := c.cachedMetaData()
	if md == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "no schema metadata available")
	}

	var logicalOp logical.LogicalOperator
	if del := dml.DeleteStatement(); del != nil {
		logicalOp = buildLogicalPlanForDeleteWithCatalog(del, md)
	} else if upd := dml.UpdateStatement(); upd != nil {
		logicalOp = buildLogicalPlanForUpdateWithCatalog(upd, md)
	} else if ins := dml.InsertStatement(); ins != nil {
		logicalOp = buildLogicalPlanForInsertWithCatalog(ins, md)
	}
	if logicalOp == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "DML logical plan failed")
	}

	if err := resolveQualifiedTableNames(logicalOp, g.c.sess.Schema); err != nil {
		return nil, err
	}

	if fn := query.FindUnsupportedFunction(logicalOp); fn != "" {
		return nil, api.NewError(api.ErrCodeUndefinedFunction,
			"Unsupported operator "+fn)
	}

	ref := query.TranslateToCascades(logicalOp)
	if ref == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "DML Cascades translation failed")
	}

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	rules = append(rules, cascades.RewritingRules()...)
	rules = append(rules, cascades.MatchingRules()...)
	planCtx := buildCascadesPlanContext(md)
	planner := cascades.NewPlanner(rules, planCtx).
		WithImplementationRules(cascades.DefaultImplementationRules()).
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

	return &cascadesPlan{
		conn:         g.c,
		md:           md,
		physicalPlan: physPlan,
		explain:      logicalOp.Explain(""),
		isUpdate:     true,
	}, nil
}

// planDMLExplainOnly produces a PlanFunc for DML (INSERT/UPDATE/DELETE).
// ExecFn delegates to exec* methods (requires a live connection).
// ExplainFn renders the logical plan without touching FDB. Used by
// NewExplainOnlyGenerator / NewExplainOnlyGeneratorWithSchema where
// only ExplainFn is called.
func (g *cascadesGenerator) planDMLExplainOnly(dml antlrgen.IDmlStatementContext) (query.Plan, error) {
	c := g.c
	return &query.PlanFunc{
		ExecFn: func(ctx context.Context) (query.Result, error) {
			if ins := dml.InsertStatement(); ins != nil {
				n, err := c.execInsert(ctx, ins)
				return query.Result{RowsAffected: n}, err
			}
			if del := dml.DeleteStatement(); del != nil {
				n, err := c.execDelete(ctx, del)
				return query.Result{RowsAffected: n}, err
			}
			if upd := dml.UpdateStatement(); upd != nil {
				n, err := c.execUpdate(ctx, upd)
				return query.Result{RowsAffected: n}, err
			}
			return query.Result{}, api.NewError(api.ErrCodeUnsupportedOperation, "unsupported DML statement")
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
	isUpdate         bool
	scalarSubqueries []scalarSubqueryBinding
	sqlLimit         int64 // Go extension: <0 means no limit
	sqlOffset        int64
}

func (p *cascadesPlan) IsUpdate() bool { return p.isUpdate }

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
	}

	// Eagerly fetch the first page so execution errors (type mismatches,
	// plan failures) surface at QueryContext time, not during row iteration.
	if err := pr.fetchPage(); err != nil {
		return query.Result{}, err
	}

	return query.Result{Rows: pr}, nil
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
	case "BYTES":
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
	case "STRING", "BYTES":
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

	_, txErr := c.sess.DB.Run(r.ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
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

func deriveColumnsFromProjection(proj *plans.RecordQueryProjectionPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	desc := findLeafDescriptor(proj.GetInner(), md)
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
		typeName := valueTypeName(v, desc)
		if typeName == "" && desc != nil {
			typeName = protoFieldTypeName(desc, name)
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
		nullable := api.ColumnNullable
		if desc != nil {
			if fd := desc.Fields().ByName(protoreflect.Name(parseColRef(name).bare())); fd != nil && fd.Cardinality() == protoreflect.Required {
				nullable = api.ColumnNoNulls
			}
		}
		cols[i] = executor.ColumnDef{
			Name:     colName,
			Label:    label,
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
	// When outer and inner share column names (e.g. SELECT * FROM a, b
	// where both have "id"), the merged row map's bare keys hold the
	// inner side's values (last-write-wins in mergeRows). To read the
	// correct values for each side, qualify bare column names with the
	// join alias so the ResultSet reads "A.ID" / "B.ID" from the map
	// instead of both hitting the bare "ID" key. Already-qualified
	// columns (from a nested NLJ) are left as-is to avoid double-
	// qualification like "B.A.ID".
	outerAlias := strings.ToUpper(nlj.GetOuterAlias())
	innerAlias := strings.ToUpper(nlj.GetInnerAlias())

	// Determine SQL-level first/second based on whether the physical
	// join direction was swapped by the ChildrenAsSet optimization.
	// When reversed, inner columns come first in SQL order.
	firstCols, secondCols := outerCols, innerCols
	firstAlias, secondAlias := outerAlias, innerAlias
	if nlj.IsSQLColumnOrderReversed() {
		firstCols, secondCols = innerCols, outerCols
		firstAlias, secondAlias = innerAlias, outerAlias
	}

	cols := make([]executor.ColumnDef, 0, len(firstCols)+len(secondCols))
	for _, c := range firstCols {
		qual := c
		if firstAlias != "" && !parseColRef(c.Name).isQualified() {
			qual.Name = firstAlias + "." + strings.ToUpper(c.Name)
		}
		cols = append(cols, qual)
	}
	for _, c := range secondCols {
		qual := c
		if secondAlias != "" && !parseColRef(c.Name).isQualified() {
			qual.Name = secondAlias + "." + strings.ToUpper(c.Name)
		}
		cols = append(cols, qual)
	}
	return cols
}

func deriveColumnsFromFlatMap(fm *plans.RecordQueryFlatMapPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	outerCols := deriveColumnsFromPlan(fm.GetOuter(), md)
	innerCols := deriveColumnsFromPlan(fm.GetInner(), md)
	if outerCols == nil && innerCols == nil {
		return nil
	}

	outerAlias := strings.ToUpper(fm.GetOuterAlias().Name())
	innerAlias := strings.ToUpper(fm.GetInnerAlias().Name())

	cols := make([]executor.ColumnDef, 0, len(outerCols)+len(innerCols))
	for _, c := range outerCols {
		qual := c
		if outerAlias != "" && !parseColRef(c.Name).isQualified() {
			qual.Name = outerAlias + "." + strings.ToUpper(c.Name)
		}
		cols = append(cols, qual)
	}
	for _, c := range innerCols {
		qual := c
		if innerAlias != "" && !parseColRef(c.Name).isQualified() {
			qual.Name = innerAlias + "." + strings.ToUpper(c.Name)
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
	return values.ExplainValue(a.Operand)
}

func aggregateResultType(a expressions.AggregateSpec, desc protoreflect.MessageDescriptor) string {
	switch a.Function {
	case expressions.AggCount:
		return "BIGINT"
	case expressions.AggAvg:
		return "DOUBLE"
	case expressions.AggSum, expressions.AggMin, expressions.AggMax:
		if desc != nil {
			operandName := values.ExplainValue(a.Operand)
			if t := protoFieldTypeName(desc, operandName); t != "UNKNOWN" {
				return t
			}
		}
		return "BIGINT"
	default:
		return "UNKNOWN"
	}
}

// valueTypeName resolves the SQL type name for a Value. For
// AggregateValue nodes, it inspects the typed Op field instead of
// string-parsing the ExplainValue output. For plain field references,
// it falls through and returns "".
func valueTypeName(v values.Value, desc protoreflect.MessageDescriptor) string {
	if av, ok := v.(*values.AggregateValue); ok {
		switch av.Op {
		case values.AggCount, values.AggCountStar:
			return "BIGINT"
		case values.AggAvg:
			return "DOUBLE"
		case values.AggSum, values.AggMin, values.AggMax:
			if av.Operand != nil && desc != nil {
				operandName := values.ExplainValue(av.Operand)
				if t := protoFieldTypeName(desc, operandName); t != "UNKNOWN" {
					return t
				}
			}
			return "BIGINT"
		default:
			return "UNKNOWN"
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

func protoFieldTypeName(desc protoreflect.MessageDescriptor, name string) string {
	fields := desc.Fields()
	fd := fields.ByName(protoreflect.Name(parseColRef(name).bare()))
	if fd != nil {
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
		return "BYTES"
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
	case "BYTES":
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
	case "STRING", "BYTES":
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
		tableName := td.Uid().GetText()
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
