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
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/expr"
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

	// INFORMATION_SCHEMA queries go through a minimal, executor-free
	// system-table handler that serves the simple
	// `SELECT [*|cols] FROM INFORMATION_SCHEMA.X [WHERE] [ORDER BY] [LIMIT]`
	// shape directly off the catalog (no legacy embedded interpreter).
	// INFORMATION_SCHEMA is a Go-only extension Java rejects entirely, so this
	// path has no cross-engine reference; RFC-145 Phase 1 detached it from the
	// executor island so Phase 2 can delete the island.
	if referencesInformationSchema(q) {
		return &query.PlanFunc{
			ExecFn: func(execCtx context.Context) (query.Result, error) {
				rows, selErr := c.execSystemTableQuery(execCtx, sel, q)
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
		// Explain-only mode renders the Cascades logical plan via ExplainFn and
		// is never executed: the plan-equivalence harness (plandiff) calls only
		// Plan().Explain(). The ExecFn is therefore dead — it formerly re-entered
		// the legacy embedded interpreter (execSelect). RFC-145 Phase 1 stubs it
		// so the executor island can be deleted in Phase 2.
		ExecFn: func(_ context.Context) (query.Result, error) {
			return query.Result{}, api.NewError(api.ErrCodeUnsupportedOperation,
				"explain-only generator does not execute queries")
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
			}, nil
		}
	}

	visitor := NewPlanVisitorWithSchema(md, g.c.sess.Schema)
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

	// Java's generateAccess resolves a FROM identifier as a CTE/table/view/
	// function BEFORE treating it as a correlated array field. The parser, which
	// has no metadata, may classify a schema-qualified table (`FROM PA AS s,
	// s.PB`, where the alias `s` also equals the schema name) as a lateral
	// unnest; demote it back to a table scan so the table branch wins (or reject
	// AT-on-a-table with WRONG_OBJECT_TYPE). RFC-142.
	if err := demoteSchemaQualifiedUnnest(logicalOp, g.c.sess.Schema, md); err != nil {
		return nil, err
	}
	// Backstop for AT-on-a-table sources (`FROM t, U AT O`, present-scalar field,
	// …) that the per-FROM-scope early pass in VisitQuery cannot reach — namely an
	// AT-on-table inside an EXISTS / scalar subquery, whose plan is attached to the
	// tree only after VisitQuery returns. Run before validateTablesAndColumns so the
	// WRONG_OBJECT_TYPE is not masked by a column-validation error. RFC-142.
	if err := rejectAtOrdinalityOnTable(logicalOp, md); err != nil {
		return nil, err
	}
	// Reject a lateral unnest's AS/AT alias colliding with ANY other FROM-source
	// alias (earlier OR later) in the same scope — the later-source collision the
	// translator's bottom-up lowering cannot see (`FROM T1, T1.arr AS V, U AS V`).
	// Run before column resolution so the duplicate-alias error is not masked.
	// RFC-142.
	if err := rejectDuplicateUnnestAlias(logicalOp); err != nil {
		return nil, err
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

	// RFC-141 §8 safety guard (logical half): a projected EXISTS in a shape the
	// fold cannot thread through (GROUP BY / aggregate / DISTINCT / UNION between
	// the projection and the existential filter) is dropped before translation —
	// the post-translation guard below cannot see a value that no longer exists,
	// so catch it here.
	if msg := findUnfoldableProjectedExists(logicalOp); msg != "" {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, msg)
	}

	ref, scalarSubqueryPlans, translateErr := query.TranslateToCascadesWithError(logicalOp, md)
	if translateErr != nil {
		// A translation error carrying a specific SQL error code (RFC-142:
		// AT-ordinality on a non-array source → WRONG_OBJECT_TYPE) takes
		// precedence over the generic "could not plan" so the user sees the
		// faithful diagnostic.
		return nil, translateErr
	}
	if ref == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"Cascades planner could not plan query")
	}

	// RFC-141 §8 safety guard: a projected ExistsValue is correct ONLY when it is
	// folded into the result value of the SelectExpression that owns its
	// existential quantifier (evaluated by the FlatMap with the inner binding
	// live). If the fold's structural pattern-matching did NOT recognize the
	// query shape, the projected ExistsValue is left in a Map above the FlatMap
	// where its binding is dead — ExistsValue.Evaluate would silently return
	// false for every row. Reject such a plan cleanly rather than ship wrong rows.
	if existsErr := query.CheckProjectedExistsFolded(ref); existsErr != nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, existsErr.Error())
	}

	// RFC-141 R4 convergence backstop (P1a): a WHERE existential
	// predicate buried under a wrapper the NLJ rule's IsExistentialPredicate /
	// IsNotExistentialPredicate routing does not recognize (`WHERE NOT (NOT
	// EXISTS(...))`, deeper AND/OR/NOT nesting) falls into the regular-predicate
	// bucket, where the empty FirstOrDefault inner's NULL default is never removed
	// and every outer row silently passes. Detect any such buried existential
	// structurally and reject cleanly rather than mis-evaluate it.
	if buriedErr := query.CheckBuriedExistentialPredicate(ref); buriedErr != nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, buriedErr.Error())
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

	ls.setPlan(physPlan)
	// LIMIT/OFFSET queries are cacheable: the limit is now carried by the
	// RecordQueryLimitPlan operator inside the cached physical plan (RFC-128),
	// not applied post-execution, so the cached plan is complete.
	if g.cache != nil {
		ls.setCache(PlanCacheMiss)
		g.cache.Put(sqlText, physPlan, scalarSubs)
	} else {
		ls.setCache(PlanCacheSkip)
	}
	return &cascadesPlan{
		conn:             g.c,
		md:               md,
		physicalPlan:     physPlan,
		explain:          physPlan.Explain(),
		scalarSubqueries: scalarSubs,
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
			if op, _ := buildLogicalPlanForDeleteWithCatalog(del, md, g.sessionSchema()); op != nil {
				return op.Explain("")
			}
		}
		if op := buildLogicalPlanForDelete(del); op != nil {
			return op.Explain("")
		}
	}
	if ins := d.InsertStatement(); ins != nil {
		if md != nil {
			if op, _ := buildLogicalPlanForInsertWithCatalog(ins, md, g.sessionSchema()); op != nil {
				return op.Explain("")
			}
		}
		if op := buildLogicalPlanForInsert(ins); op != nil {
			return op.Explain("")
		}
	}
	if upd := d.UpdateStatement(); upd != nil {
		if md != nil {
			if op, _ := buildLogicalPlanForUpdateWithCatalog(upd, md, g.sessionSchema()); op != nil {
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
						if op, _ := buildLogicalPlanForDeleteWithCatalog(del, md, g.sessionSchema()); op != nil {
							return op.Explain("")
						}
					}
					if op := buildLogicalPlanForDelete(del); op != nil {
						return op.Explain("")
					}
				}
				if upd := dml.UpdateStatement(); upd != nil {
					if md != nil {
						if op, _ := buildLogicalPlanForUpdateWithCatalog(upd, md, g.sessionSchema()); op != nil {
							return op.Explain("")
						}
					}
					if op := buildLogicalPlanForUpdate(upd); op != nil {
						return op.Explain("")
					}
				}
				if ins := dml.InsertStatement(); ins != nil {
					if md != nil {
						if op, _ := buildLogicalPlanForInsertWithCatalog(ins, md, g.sessionSchema()); op != nil {
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
		// RFC-141 R4: an EXISTS buried in a SCALAR expression in the DML
		// WHERE clause (`DELETE … WHERE CASE WHEN EXISTS(...) THEN 1 ELSE 0 END =
		// 1`) is lowered to a constant in the DML WHERE-build path (which differs
		// from the SELECT PlanVisitor path), so it silently affects the wrong rows.
		// Detect the buried EXISTS structurally and reject, same as the SELECT path.
		if w := del.WhereExpr(); w != nil && expr.WhereExistsInScalarPosition(w.Expression()) {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"EXISTS nested in a scalar expression is not yet supported")
		}
		var delErr error
		logicalOp, delErr = buildLogicalPlanForDeleteWithCatalog(del, md, g.c.sess.Schema)
		if delErr != nil {
			// A carried SQLSTATE from a WHERE-EXISTS subquery plan failure (RFC-142:
			// AT-on-a-table → WRONG_OBJECT_TYPE) — surface it as the SELECT path does.
			return nil, delErr
		}
	} else if upd := dml.UpdateStatement(); upd != nil {
		if w := upd.WhereExpr(); w != nil && expr.WhereExistsInScalarPosition(w.Expression()) {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"EXISTS nested in a scalar expression is not yet supported")
		}
		var updErr error
		logicalOp, updErr = buildLogicalPlanForUpdateWithCatalog(upd, md, g.c.sess.Schema)
		if updErr != nil {
			return nil, updErr
		}
	} else if ins := dml.InsertStatement(); ins != nil {
		// RFC-141 R4: an INSERT … SELECT whose SELECT-body WHERE buries an
		// EXISTS in a scalar (`INSERT … SELECT … WHERE CASE WHEN EXISTS(...) …`) is
		// rebuilt through a path that bypasses the per-statement WHERE guard, so the
		// buried EXISTS folds to a constant and the wrong rows are inserted. Scan the
		// INSERT subtree for any such WHERE and reject (the SELECT body's other
		// EXISTS positions are guarded when its body plans through the SELECT path).
		if expr.AnyWhereExistsInScalarPosition(ins) {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"EXISTS nested in a scalar expression is not yet supported")
		}
		insStmt = ins
		var insErr error
		logicalOp, insErr = buildLogicalPlanForInsertWithCatalog(ins, md, g.c.sess.Schema)
		if insErr != nil {
			// A carried SQLSTATE from the INSERT … SELECT body build (RFC-142:
			// AT-on-a-table comma source → WRONG_OBJECT_TYPE) — surface it.
			return nil, insErr
		}
	}
	if logicalOp == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "DML logical plan failed")
	}

	if err := resolveQualifiedTableNames(logicalOp, g.c.sess.Schema); err != nil {
		return nil, err
	}

	// Reject a lateral unnest's AS/AT alias colliding with ANY other FROM-source
	// alias (earlier OR later) in the same scope — the DML twin of the SELECT-path
	// guard. An `INSERT INTO dst SELECT V FROM T1, T1.arr AS V, U AS V` reaches the
	// DML planner whose INSERT … SELECT body the SELECT-path rejectDuplicateUnnest
	// Alias never runs over, so without this the later `U AS V` overwrites the
	// unnest's V keys (mergeRows last-leg-wins) and the INSERT writes the WRONG rows
	// instead of raising the duplicate-alias error. The pass recurses through
	// LogicalInsert.Source / LogicalUpdate.Input / LogicalDelete.Input (their
	// Children) and subquery plans, so a colliding alias anywhere in the DML's FROM
	// scope is rejected. RFC-142.
	if err := rejectDuplicateUnnestAlias(logicalOp); err != nil {
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

	// RFC-141 §8 / R4: the same EXISTS safety guards as the SELECT path
	// must run for DML (`DELETE/UPDATE … WHERE NOT (NOT EXISTS(...))`) — the DML
	// planner reuses the existential NLJ rule, so a buried WHERE existential is
	// just as silently-wrong (every targeted row matches) without the guard.
	if existsErr := query.CheckProjectedExistsFolded(ref); existsErr != nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, existsErr.Error())
	}
	if buriedErr := query.CheckBuriedExistentialPredicate(ref); buriedErr != nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, buriedErr.Error())
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
					if op, _ := buildLogicalPlanForDeleteWithCatalog(del, md, g.sessionSchema()); op != nil {
						return op.Explain("")
					}
				}
				if op := buildLogicalPlanForDelete(del); op != nil {
					return op.Explain("")
				}
			}
			if upd := dml.UpdateStatement(); upd != nil {
				if md != nil {
					if op, _ := buildLogicalPlanForUpdateWithCatalog(upd, md, g.sessionSchema()); op != nil {
						return op.Explain("")
					}
				}
				if op := buildLogicalPlanForUpdate(upd); op != nil {
					return op.Explain("")
				}
			}
			if ins := dml.InsertStatement(); ins != nil {
				if md != nil {
					if op, _ := buildLogicalPlanForInsertWithCatalog(ins, md, g.sessionSchema()); op != nil {
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

// Execute runs the planned query. RFC-106a per-statement resource
// governance applies here:
//
//   - Statement timeout (§4): when the connection sets statementTimeout>0,
//     the whole-statement ctx is wrapped in context.WithTimeout. Every
//     cursor gates on ctx.Err() (CollectAllBounded, the sort/hash buffers),
//     so the deadline bounds the work with no per-operator plumbing. The
//     cancel func is tied to the RESULT-SET lifetime (paginatingRows.Close),
//     not this function's return, because the ctx must stay live for the
//     whole iteration across pages.
//
//     PER-REQUEST, not per-logical-statement (Graefe Q1): one Execute() is
//     bounded. A continuation resumed by a NEW request (a fresh Execute on a
//     new plan) starts a fresh deadline — there is no cross-continuation
//     wall-clock, matching Java's per-ExecuteState TimeScanLimiter (reset on
//     every resume). The per-tx FDB timeout is unaffected.
func (p *cascadesPlan) Execute(ctx context.Context) (query.Result, error) {
	c := p.conn
	ss, ssErr := c.sess.Keyspace.SchemaSubspace(c.sess.DBPath, c.sess.Schema)
	if ssErr != nil {
		return query.Result{}, ssErr
	}

	cols := deriveColumnsFromPlan(p.physicalPlan, p.md)

	// Statement timeout: bound this whole Execute (all its pages). cancel
	// is carried on the paginatingRows so it fires on Close (the result-set
	// lifetime), not when Execute returns.
	var cancel context.CancelFunc
	if c.statementTimeout > 0 {
		// Tag the internal deadline with errStatementTimeout as its cause so the error
		// translator can tell THIS timeout (→ 54F01) apart from a caller-supplied
		// QueryContext/ExecContext deadline (which must keep propagating as
		// context.DeadlineExceeded so errors.Is(err, context.DeadlineExceeded) holds).
		ctx, cancel = context.WithTimeoutCause(ctx, c.statementTimeout, errStatementTimeout)
	}

	// Each fetchPage creates a fresh cursor hierarchy from the plan +
	// continuation. The continuation carries all intermediate state
	// (aggregate accumulators, sort buffers) serialized as protobuf.
	// No cursor persists across transactions — this matches Java's
	// architecture.

	pr := &paginatingRows{
		ctx:              ctx,
		cancel:           cancel,
		conn:             c,
		ss:               ss,
		plan:             p.physicalPlan,
		md:               p.md,
		scalarSubqueries: p.scalarSubqueries,
		maxRows:          optInt64(c.Options(), api.OptMaxRows, math.MaxInt32),
		maxResultBytes:   c.maxResultBytes,
		cols:             cols,
		respectActiveTx:  p.IsUpdate(),
		// RFC-130: mint the statement-wide ExecuteState ONCE here (never nil),
		// with the memory byte budget from OptMaxStatementMemoryBytes (0/unset
		// → unlimited). It is held on paginatingRows so it survives across the
		// per-page cursor hierarchies (each fetchPage rebuilds the cursors but
		// shares this one counter) and is assigned into every page's
		// ExecuteProperties in executeProps(). The "no budget" case is
		// memLimit<=0, not a nil state, so a missed accumulation site charges
		// an unlimited counter rather than silently no-oping.
		execState: recordlayer.NewExecuteState(
			optInt64(c.Options(), api.OptMaxStatementMemoryBytes, 0),
		),
	}

	// Eagerly fetch the first page so execution errors (type mismatches,
	// plan failures) surface at QueryContext time, not during row iteration.
	if err := pr.fetchPage(); err != nil {
		pr.Close()
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
	cancel           context.CancelFunc // statement-timeout cancel; nil when no timeout
	conn             *EmbeddedConnection
	ss               subspace.Subspace
	plan             plans.RecordQueryPlan
	md               *recordlayer.RecordMetaData
	scalarSubqueries []scalarSubqueryBinding
	cols             []executor.ColumnDef

	// emitted counts rows actually returned to the caller across all pages.
	// Shared by the MAX_ROWS cap and pageRowBudget. SQL LIMIT/OFFSET is NOT
	// here anymore — it is carried by the RecordQueryLimitPlan operator
	// inside the plan (RFC-128), applied at its correct pipeline position.
	emitted int64

	// maxRows is the statement-wide returned-row cap from
	// api.OptMaxRows (RFC-106a §3) — JDBC setMaxRows semantics: a TOTAL
	// cap across all pages, NOT a per-page size. math.MaxInt32 (the option
	// default) means effectively unlimited.
	maxRows int64

	// maxResultBytes is the statement-wide returned-row byte cap from the
	// connection's Go-local config (RFC-106a §5). 0 = off. resultBytes
	// accumulates the cheap tuple-encoded size of each emitted row; when it
	// would exceed maxResultBytes the next emit errors (54F01).
	maxResultBytes int64
	resultBytes    int64

	// execState is the statement-wide RFC-130 ExecuteState (the memory byte
	// budget counter). Minted ONCE in Execute and shared across all pages —
	// each fetchPage rebuilds the cursor hierarchy but assigns this same
	// pointer into the page's ExecuteProperties.State, so the in-memory
	// buffering budget accumulates across the whole statement. Never nil.
	execState *recordlayer.ExecuteState

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
	// Release the statement-timeout context (RFC-106a §4). The deadline
	// must live for the whole result-set lifetime, so cancel fires here on
	// Close — not when Execute returns. Idempotent: cancel is safe to call
	// repeatedly and Close may be invoked more than once.
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
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

func (r *paginatingRows) Next(dest []driver.Value) (err error) {
	// RFC-091 / P0.2: pages iterate AFTER QueryContext/ExecContext have returned, so
	// this sits OUTSIDE their boundary recover. A panic during later-page planning or
	// execution (an invariant trip, or any residual eval panic) must become an error
	// here, not crash the shared multi-tenant process.
	defer func() {
		if rec := recover(); rec != nil {
			err = recoveredPanicError(rec)
		}
	}()
	if r.closed {
		return io.EOF
	}
	// MAX_ROWS statement-wide cap (RFC-106a §3): a TOTAL returned-row
	// budget across ALL pages. math.MaxInt32 (the option default) is
	// effectively unlimited. A clean stop (io.EOF), not an error — JDBC
	// setMaxRows semantics. SQL LIMIT is no longer applied here; it is the
	// RecordQueryLimitPlan operator's job inside the plan (RFC-128).
	if r.maxRows > 0 && r.emitted >= r.maxRows {
		return io.EOF
	}

	row, err := r.nextRow()
	if err != nil {
		return err
	}
	// Result-size byte cap (RFC-106a §5): accumulate the cheap tuple-encoded
	// size of each row that is actually returned to the caller. Erroring
	// BEFORE the copy means the row that would breach the cap is not handed
	// back — a hard egress ceiling. (OFFSET is no longer applied here; the
	// RecordQueryLimitPlan operator drops skipped rows before they reach
	// nextRow, RFC-128 — so every row nextRow yields is a real result row.)
	if r.maxResultBytes > 0 {
		r.resultBytes += estimateRowBytes(row)
		if r.resultBytes > r.maxResultBytes {
			return api.NewErrorf(api.ErrCodeExecutionLimitReached,
				"result size limit exceeded: %d bytes returned exceeds cap %d",
				r.resultBytes, r.maxResultBytes)
		}
	}
	copy(dest, row)
	r.emitted++
	return nil
}

// estimateRowBytes returns a cheap encoded-length estimate of a result
// row for the RFC-106a §5 result-size cap. It is intentionally NOT exact
// heap size — a non-exact egress ceiling. Per-value cost:
//
//   - []byte / string: the byte length
//   - numbers / bool / time: a fixed 8-byte estimate
//   - nil: 1 byte (the encoded null marker)
//
// Fast and allocation-free; good enough to bound how many bytes a single
// statement streams back to the client.
func estimateRowBytes(row []driver.Value) int64 {
	var n int64
	for _, v := range row {
		switch x := v.(type) {
		case nil:
			n++
		case []byte:
			n += int64(len(x))
		case string:
			n += int64(len(x))
		default:
			n += 8
		}
	}
	return n
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

// optInt64 reads an option as an int64, accepting either an int or an
// int64 stored value (the option-default map uses both — MAX_ROWS /
// scanned-rows are int, scanned-bytes / time are int64). Returns fallback
// when the option is absent or of an unexpected type.
func optInt64(opts *api.Options, name api.OptionName, fallback int64) int64 {
	switch v := opts.Get(name).(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case int32:
		return int64(v)
	default:
		return fallback
	}
}

// executeProps builds the per-page ExecuteProperties for one fetchPage
// from the connection's api.Options (RFC-106a). All of these are PER-PAGE
// (a fresh cursor + transaction per page), matching Java's
// ExecuteProperties.setScannedRecordsLimit / setScannedBytesLimit /
// setTimeLimit. The statement-wide MAX_ROWS cap and the result-size byte
// cap are NOT here — they are enforced across pages in paginatingRows.Next.
//
// Defaults are inert: with no options set, OptExecutionScannedRowsLimit
// defaults to MaxInt32 and OptExecutionScannedBytesLimit to MaxInt64 — both
// sentinels that mean "no limit", so the produced ScannedRecordsLimit /
// ScannedBytesLimit are left 0 (the recordlayer "unlimited" value). This
// keeps the no-option path identical to the pre-RFC behavior.
// pageRowBudget returns the maximum number of rows the MAIN plan's current page
// must produce given the active JDBC MAX_ROWS returned-row cap, or 0 when no cap
// is active (unbounded page). Bounding the page cursor's ReturnedRowLimit to
// this stops fetchPage from materializing the entire underlying result into
// r.buf when a returned-row cap is set without a per-page scan limit
// (RFC-106a §3). The budget is EXACT — remaining emit is precisely the rows this
// statement can still consume — so it never under-produces (no row loss). SQL
// LIMIT/OFFSET no longer participates here (RFC-128): scan-bounding for a plain
// LIMIT is carried by the RecordQueryLimitPlan operator's ReturnedRowLimit =
// offset+limit (executor.go executeLimit). DML plans report an affected-row
// count, not a result set, so the cap must NOT bound their scan.
func (r *paginatingRows) pageRowBudget() int {
	if r.respectActiveTx { // DML (INSERT/UPDATE/DELETE): never bound the scan
		return 0
	}
	rowCap := int64(math.MaxInt64)
	if r.maxRows > 0 && r.maxRows < math.MaxInt32 && r.maxRows < rowCap {
		rowCap = r.maxRows
	}
	if rowCap == math.MaxInt64 {
		return 0 // no active returned-row cap → leave the page unbounded
	}
	remainingEmit := rowCap - r.emitted
	if remainingEmit <= 0 {
		return 0 // cap already reached; Next() EOFs before this is used
	}
	if remainingEmit > math.MaxInt32 {
		return 0
	}
	return int(remainingEmit)
}

func (r *paginatingRows) executeProps() recordlayer.ExecuteProperties {
	props := recordlayer.DefaultExecuteProperties()

	opts := r.conn.Options()

	// Per-page time limit. The connection option (if set) is intersected
	// with the per-transaction CAP (txPageTimeLimit, 4s) so the FDB 5s hard
	// wall is never exceeded: the 4s cap is the ceiling and a smaller user
	// limit only narrows it — a larger user value can never raise the page
	// budget past the cap (Graefe review).
	timeLimit := txPageTimeLimit
	if userMillis := optInt64(opts, api.OptExecutionTimeLimit, 0); userMillis > 0 {
		if ut := time.Duration(userMillis) * time.Millisecond; ut < timeLimit {
			timeLimit = ut
		}
	}
	props = props.WithTimeLimit(timeLimit)

	// Per-page scanned-records limit. MaxInt32 is the "no limit" sentinel
	// (api default) — only wire a real (smaller) limit through.
	if rows := optInt64(opts, api.OptExecutionScannedRowsLimit, math.MaxInt32); rows > 0 && rows < math.MaxInt32 {
		props = props.WithScannedRecordsLimit(int(rows))
	}

	// Per-page scanned-bytes limit. MaxInt64 is the "no limit" sentinel.
	if bytesLimit := optInt64(opts, api.OptExecutionScannedBytesLimit, math.MaxInt64); bytesLimit > 0 && bytesLimit < math.MaxInt64 {
		props = props.WithScannedBytesLimit(bytesLimit)
	}

	// FailOnScanLimitReached: when set, a leaf cursor that hits its scan /
	// byte limit errors (54F01) instead of paginating (Java's
	// setFailOnScanLimitReached(true)). Default off.
	props.FailOnScanLimitReached = r.conn.failOnScanLimitReached

	// RFC-130: thread the statement-wide ExecuteState into this page's props so
	// the in-memory buffering operators charge the shared memory byte budget.
	// The SAME pointer is assigned every page, so the budget survives the
	// per-page cursor rebuild — exactly as Java's ExecuteState survives
	// clearSkipAndLimit by being held by reference.
	props.State = r.execState

	return props
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
// pageContinuationState decides, from a drained page's terminal continuation + NoNextReason, whether the
// paginatingRows internal drain is (a) exhausted, (b) has a resumable byte continuation, or (c) must
// surface ScanLimitReachedError (→ 54F01). It is the PAGINATING counterpart to errIfDrainTruncated
// (recordlayer/cursor_util.go): the value-only drains there discard the continuation so they need only
// the IsOutOfBand() check; paginatingRows additionally consumes the resumable bytes.
//
// Exhaustion is decided by IsEnd() (≡ NoNextReason.SourceExhausted) — NEVER by ToBytes()==nil (RFC-127).
// A non-end StartContinuation has ToBytes()==nil, byte-identical to an EndContinuation; treating its nil
// bytes as exhaustion (the old code) would silently truncate the result set. This aligns Go with Java's
// invariant (RecordLayerIterator.java:91 gates end-of-results on SOURCE_EXHAUSTED, never bytes). For a
// non-end continuation with no resumable bytes, the internal drain re-executes the plan from
// r.continuation and so cannot resume-from-BEGIN like Java's client-driven iterator (it would re-buffer →
// infinite loop), so:
//   - out-of-band (scan/time/byte limit before any resumable progress) → 54F01 (avoids data loss + loop);
//   - in-band ReturnLimitReached with zero rows ⟹ a row limit of 0 (LIMIT 0): clean exhaustion, no data
//     lost. (SourceExhausted+nil-bytes is impossible — it is isEnd()==true, the first branch.)
//
// Reachability: the out-of-band branch is presently a DEFENSIVE guard, not a live path. Every Go leaf
// cursor reports an out-of-band stop only after scanned>0 (key_value_cursor.go:164/174/181,
// record_key_cursor.go:64/69/78), at which point its continuation is set → a BytesContinuation; and
// composite cursors either carry a serialized BytesContinuation (merge/intersection) or error-first with
// 54F01 (mergeSort, RFC-106a). So no current cursor emits a no-next out-of-band+StartContinuation, and the
// only reachable nil-bytes+non-end case is LIMIT 0. The guard exists because the OLD logic was wrong in
// principle (exhaustion from bytes, not IsEnd) — a latent landmine the moment any future cursor emits the
// out-of-band+START state Java's Union/Intersection/MapWhile cursors legitimately produce.
func pageContinuationState(cont recordlayer.RecordCursorContinuation, reason recordlayer.NoNextReason) (exhausted bool, contBytes []byte, err error) {
	if cont == nil || cont.IsEnd() {
		return true, nil, nil // SourceExhausted
	}
	b, e := cont.ToBytes()
	if e != nil {
		return false, nil, e
	}
	if b != nil {
		return false, b, nil // resumable position → keep draining
	}
	if reason.IsOutOfBand() {
		return false, nil, &recordlayer.ScanLimitReachedError{Reason: reason}
	}
	return true, nil, nil // ReturnLimitReached (LIMIT 0) — clean done
}

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
		// Compute the statement's execution props BEFORE evaluating scalar
		// subqueries so the configured scan limits apply to them too
		// (RFC-106a): an uncorrelated subquery must not scan past the statement
		// cap while the outer plan would fail/paginate. (The statement timeout
		// already reaches them via r.ctx.)
		props := r.executeProps()
		if len(r.scalarSubqueries) > 0 {
			scalarResults := make(map[values.CorrelationIdentifier]any, len(r.scalarSubqueries))
			for _, ssq := range r.scalarSubqueries {
				result, ssqErr := executor.EvaluateScalarSubquery(r.ctx, ssq.plan, store, evalCtx, props)
				if ssqErr != nil {
					// Route the subquery error through the same translation as the
					// outer plan so a subquery scan-limit/deadline hit surfaces as
					// 54F01, not a raw *ScanLimitReachedError (RFC-106a).
					return nil, translateExecErrorCtx(r.ctx, ssqErr)
				}
				scalarResults[ssq.alias] = result
			}
			evalCtx = evalCtx.WithScalarSubqueries(scalarResults)
		}
		// Bound the MAIN plan's page to the remaining returned-row budget so a
		// MAX_ROWS / SQL-LIMIT statement without a per-page scan limit does not
		// materialize the entire underlying result into r.buf (RFC-106a).
		// Applied ONLY here, not to the shared props the scalar subqueries use —
		// a budget of 1 would otherwise cap a subquery at one row and defeat its
		// >1-row cardinality check.
		mainProps := props
		if budget := r.pageRowBudget(); budget > 0 {
			mainProps = props.WithReturnedRowLimit(budget)
		}
		cursor, execErr := executor.ExecutePlan(r.ctx, r.plan, store, evalCtx, r.continuation, mainProps)
		if execErr != nil {
			return nil, translateExecErrorCtx(r.ctx, execErr)
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
			return nil, translateExecErrorCtx(r.ctx, err)
		}

		exhausted, contBytes, classifyErr := pageContinuationState(rs.GetContinuation(), rs.GetNoNextReason())
		if classifyErr != nil {
			return nil, classifyErr
		}
		r.exhausted = exhausted
		r.continuation = contBytes
		return nil, nil
	})

	if txErr != nil {
		return translateExecErrorCtx(r.ctx, txErr)
	}
	return nil
}

// errStatementTimeout is the cause stamped on the internal RFC-106a §4 statement-timeout
// context (Execute's context.WithTimeoutCause). It lets translateExecErrorCtx map ONLY
// that timeout to 54F01, leaving a caller's own context deadline to propagate.
var errStatementTimeout = errors.New("statement timeout")

// translateExecErrorCtx is translateExecError plus statement-timeout awareness. ctx is
// the statement-scoped context (Execute's, possibly WithTimeoutCause-wrapped). A deadline
// error is mapped to 54F01 ONLY when it came from the INTERNAL statement timeout
// (context.Cause(ctx) == errStatementTimeout); a caller-supplied QueryContext/ExecContext
// deadline falls through to translateExecError, which returns it unchanged so that
// errors.Is(err, context.DeadlineExceeded) keeps working and a client cancellation is not
// misreported as a Go-local statement timeout (RFC-106a, PR #291).
func translateExecErrorCtx(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) && errors.Is(context.Cause(ctx), errStatementTimeout) {
		return api.NewError(api.ErrCodeExecutionLimitReached, "statement timeout")
	}
	return translateExecError(err)
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
	// Eager-buffer caps (RFC-106a): in-memory materialization and sort
	// buffers throw Go error structs that, like the recursive-CTE depth
	// cap above, are per-statement resource limits — surface them as
	// 54F01 (ErrCodeExecutionLimitReached) rather than letting them fall
	// through as a generic internal error.
	var matLimit *executor.MaterializationLimitExceededError
	if errors.As(err, &matLimit) {
		return api.NewError(api.ErrCodeExecutionLimitReached, matLimit.Error())
	}
	var sortLimit *executor.SortBufferExceededError
	if errors.As(err, &sortLimit) {
		return api.NewError(api.ErrCodeExecutionLimitReached, sortLimit.Error())
	}
	// Statement-wide memory byte budget (RFC-130): the accounted in-memory
	// buffers (CollectAllBounded, sort/distinct/NLJ-hash/temp-table/DML-echo)
	// charge a shared per-statement counter; a breach is a per-statement
	// resource limit in the same family — surface it as 54F01.
	var memLimit *recordlayer.MemoryLimitExceededError
	if errors.As(err, &memLimit) {
		return api.NewError(api.ErrCodeExecutionLimitReached, memLimit.Error())
	}
	// Leaf-cursor scan limit hit with FailOnScanLimitReached set
	// (RFC-106a parity): Java throws ScanLimitReachedException (54F01).
	var scanLimit *recordlayer.ScanLimitReachedError
	if errors.As(err, &scanLimit) {
		return api.NewError(api.ErrCodeExecutionLimitReached, scanLimit.Error())
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
	var invalidArg *values.InvalidArgumentError
	if errors.As(err, &invalidArg) {
		return api.NewError(api.ErrCodeInvalidParameter, invalidArg.Error())
	}
	var aggEval *values.AggregateEvalError
	if errors.As(err, &aggEval) {
		return api.NewError(api.ErrCodeGroupingError, aggEval.Error())
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

// IndexColumnFunctions returns the per-column function tags parallel to
// IndexColumnNames: "" for a plain field, cascades.FunctionKindCardinality for
// a CARDINALITY()-keyed column. Returns nil when every column is a plain field
// (the common case, avoiding an allocation). This is the recordlayer→cascades
// half of the KeyExpression→Value bridge: it tells the match candidate which
// column's Value is CardinalityValue(FieldValue(col)) rather than a bare field,
// so a CARDINALITY() predicate/sort binds to the index (Java: the candidate
// carries CardinalityFunctionKeyExpression.toValue()).
func (d *metadataIndexDef) IndexColumnFunctions() []string {
	cols := indexColumnFunctionTags(d.idx.RootExpression)
	for _, fn := range cols {
		if fn != "" {
			return cols
		}
	}
	return nil
}

// indexColumnFunctionTags flattens a key expression into per-column function
// tags, parallel to KeyExpression.FieldNames(). A *CardinalityFunctionKeyExpression
// contributes one cardinality-tagged column (its argument's single field name);
// every other atomic key contributes a "" (plain) tag per field name it
// produces. Composite keys concatenate their children's tags, mirroring
// FieldNames()'s flattening so the two slices stay index-aligned.
func indexColumnFunctionTags(expr recordlayer.KeyExpression) []string {
	switch e := expr.(type) {
	case *recordlayer.CardinalityFunctionKeyExpression:
		// One key column; its FieldNames() may yield >1 name only for the
		// Java wrapper shape (arr.values), which Go never writes — so a single
		// cardinality tag suffices and stays aligned with FieldNames().
		n := len(e.FieldNames())
		if n == 0 {
			n = 1
		}
		tags := make([]string, n)
		tags[0] = cascades.FunctionKindCardinality
		return tags
	case *recordlayer.CompositeKeyExpression:
		var tags []string
		for _, child := range e.SubKeyExpressions() {
			tags = append(tags, indexColumnFunctionTags(child)...)
		}
		return tags
	default:
		// Plain field / nesting / everything else: one "" tag per produced
		// field name, keeping the slice aligned with FieldNames().
		names := expr.FieldNames()
		if len(names) == 0 {
			return []string{""}
		}
		return make([]string, len(names))
	}
}

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

// tryVectorIndexCandidate builds a VectorIndexScanMatchCandidate for a vector
// index (HNSW or SPFresh — the two share the logical match shape and the
// BY_DISTANCE physical contract; RFC-094 §10), or nil if the index is not a
// vector index. columnNames are all index columns (partition prefix + the
// vector column); partitionCount is the KeyWithValue split point; the metric
// comes from the method's own option namespace.
func tryVectorIndexCandidate(idx *recordlayer.Index, md *recordlayer.RecordMetaData) *cascades.VectorIndexScanMatchCandidate {
	if (idx.Type != recordlayer.IndexTypeVector && idx.Type != recordlayer.IndexTypeVectorSPFresh) || idx.RootExpression == nil {
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
	metricOption := idx.Options[recordlayer.IndexOptionVectorMetric]
	if idx.Type == recordlayer.IndexTypeVectorSPFresh {
		metricOption = idx.Options[recordlayer.IndexOptionSPFreshMetric]
		// The SPFresh maintainer rejects prefixed (grouped) scans; a
		// partitioned candidate would plan queries the executor cannot run.
		// The DDL already rejects PARTITION BY USING SPFRESH — this guards
		// directly-constructed metadata.
		if partitionCount > 0 {
			return nil
		}
	}
	metric, ok := vectorMetricOperator(metricOption)
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

// findExplodePlan walks through innerPlan wrappers (a PredicatesFilter pushed
// down for a WHERE-on-element, etc.) to find a leaf RecordQueryExplodePlan — the
// structural marker of a lateral-unnest FlatMap's inner leg (`FROM t, t.arr AS
// x`). RFC-142.
func findExplodePlan(p plans.RecordQueryPlan) *plans.RecordQueryExplodePlan {
	for {
		if e, ok := p.(*plans.RecordQueryExplodePlan); ok {
			return e
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
	projections := proj.GetProjections()

	// A pull-up / pass-through projection (e.g. the RFC-141 projected-EXISTS fold's
	// cleanup re-projection that drops a hidden ORDER BY column) references its
	// columns by the INNER plan's OUTPUT key — which for an aliased column is the
	// alias (`THE_ID`), not a proto field. The descriptor-based type lookup then
	// can't resolve it (there is no proto field `THE_ID`) and yields UNKNOWN. The
	// projection never RE-TYPES a column it merely renames/drops, so inherit the
	// type+nullability from the inner plan's same-named derived column. This keeps
	// the cleanup a true metadata pass-through, consistent-by-construction with the
	// folded FlatMap's own column derivation (foldedColumnDef). The inner columns
	// are derived lazily — only when a column's type is genuinely unresolved — so
	// the ordinary projection path pays nothing.
	var innerByName map[string]executor.ColumnDef
	cols := make([]executor.ColumnDef, len(projections))
	for i, v := range projections {
		alias := ""
		if i < len(aliases) {
			alias = aliases[i]
		}
		cd := deriveProjectionColumnDef(v, alias, i, descs)
		if cd.TypeName == "" || cd.TypeName == "UNKNOWN" {
			// Inherit from the inner column the projection reads (matched by the
			// projected FieldValue's field = the inner output key).
			if fv, ok := v.(*values.FieldValue); ok && fv.Child == nil {
				if innerByName == nil {
					innerByName = make(map[string]executor.ColumnDef)
					for _, ic := range deriveColumnsFromPlan(proj.GetInner(), md) {
						innerByName[strings.ToUpper(ic.Name)] = ic
					}
				}
				if ic, found := innerByName[strings.ToUpper(fv.Field)]; found && ic.TypeName != "" && ic.TypeName != "UNKNOWN" {
					cd.TypeName = ic.TypeName
					cd.Nullable = ic.Nullable
				}
			}
		}
		cols[i] = cd
	}
	return cols
}

// deriveProjectionColumnDef derives the ResultSet ColumnDef (datum-lookup Name,
// user-visible display Label, type, nullability) for a single projected column
// from its Value + optional SELECT-list alias. This is the SHARED derivation
// reused by BOTH the normal projection path (deriveColumnsFromProjection) AND
// the RFC-141 projected-EXISTS fold (deriveColumnsFromFlatMap), so the two can
// never diverge — adding a projected EXISTS must not change the labels of the
// other projected columns.
//
// The derivation matches Java's ResultSetMetaData:
//   - Name (datum lookup key): the alias when aliased, else the column's
//     reference name — QUALIFIED ("U.NAME") for a join projection so same-named
//     columns of different legs stay disambiguable in the row map.
//   - Label (getColumnLabel — what Rows.Columns() surfaces): the alias when
//     aliased; for an unaliased field reference the UNQUALIFIED leaf name
//     (`SELECT u.name` → label NAME, never U.NAME); for an unaliased non-field
//     expression the positional `_i`. The qualifier must NEVER leak into the
//     user-visible metadata.
//
// idx is the column's position (for the `_i` positional label of an unaliased
// computed expression). descs are the leaf descriptors the column type/nullable
// is resolved against (the leg that defines the column).
func deriveProjectionColumnDef(v values.Value, alias string, idx int, descs []protoreflect.MessageDescriptor) executor.ColumnDef {
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
	if alias != "" {
		label = strings.ToUpper(alias)
	} else if _, isField := v.(*values.FieldValue); !isField {
		label = fmt.Sprintf("_%d", idx)
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
	return executor.ColumnDef{
		Name:     colName,
		Label:    displayLabel,
		TypeName: typeName,
		Nullable: nullable,
	}
}

// foldedColumnDef derives the ResultSet ColumnDef for ONE field of a
// projected-EXISTS fold's RecordConstructor (RFC-141 ROOT FIX). It is the
// consistent-by-construction counterpart of the normal projection path's
// deriveProjectionColumnDef, but it takes its Name+Label from the field NAME the
// fold set rather than re-deriving them from the field VALUE.
//
// The contract is dictated by execution: RecordConstructorValue.Evaluate keys the
// executed row by `f.Name` (one map key per field), and a positional/named Scan
// looks the column up by `ColumnDef.Name`. Therefore:
//
//   - Name (datum lookup key) = f.Name, ALWAYS. The fold set f.Name to the
//     SELECT-list alias when the column was explicitly aliased, else to the
//     column's reference (bare `ID` for a single-table column, qualified
//     `T1.ID`/`T2.ID` for a JOIN leg so same-named legs stay disambiguable). It
//     cannot diverge from the record key, so a Scan never reads NULL.
//   - Label (the user-visible getColumnLabel) = the BARE LEAF of f.Name —
//     matching Java exactly (the SELECT-list Identifier after clearQualifier):
//     `SELECT t1.id` → ID, `t1.id AS id` → ID, `id AS the_id` → THE_ID,
//     `t2.id` over a JOIN → ID (never the qualified T2.ID).
//   - Type resolves from the field VALUE (the EXISTS boolean → BOOLEAN via
//     ExistsValue.Type(); a leg column against its defining descriptor). The
//     value's column reference (ExplainValue — qualified `T2.ID` for a JOIN
//     composite) is what the descriptor lookup keys on, so the type resolves
//     against the correct leg even though the public label is the bare leaf.
//
// No alias inference, no value-derived Name: the divergences found
// (explicit-alias==bare-leaf reading NULL, JOIN composite leaking a qualified
// label) are impossible by construction.
func foldedColumnDef(f values.RecordConstructorField, descs []protoreflect.MessageDescriptor) executor.ColumnDef {
	name := strings.ToUpper(f.Name)
	label := strings.ToUpper(parseColRef(f.Name).bare())

	// Resolve the column TYPE against the leg that defines it. Use the VALUE's
	// reference name (qualified for a JOIN composite) so descriptorForColumn keys
	// the right leg; fall back to the field Name for a non-FieldValue value.
	typeRef := name
	if fv, ok := f.Value.(*values.FieldValue); ok {
		if fv.Child != nil {
			typeRef = strings.ToUpper(values.ExplainValue(f.Value))
		} else {
			typeRef = strings.ToUpper(fv.Field)
		}
	}
	colDesc := descriptorForColumn(typeRef, descs)
	typeDesc := colDesc
	if typeDesc == nil && len(descs) > 0 {
		typeDesc = descs[0]
	}
	typeName := valueTypeName(f.Value, typeDesc)
	if typeName == "" && colDesc != nil {
		typeName = protoFieldTypeName(colDesc, typeRef)
	}
	// A column the leaf descriptors couldn't resolve (genuinely unknown type)
	// flows under the FlatMap's merged outer row where a numeric BIGINT is the
	// safe default; the EXISTS boolean and other resolved columns keep their real
	// type (valueTypeName returns it). This preserves the fold's prior behaviour
	// for genuinely-unresolved columns.
	if typeName == "" || typeName == "UNKNOWN" {
		typeName = "BIGINT"
	}

	nullable := api.ColumnNullable
	if colDesc != nil {
		if fd := colDesc.Fields().ByName(protoreflect.Name(parseColRef(typeRef).bare())); fd != nil && fd.Cardinality() == protoreflect.Required {
			nullable = api.ColumnNoNulls
		}
	} else if f.Value != nil {
		// No proto descriptor field resolves for this column — it is a
		// SYNTHESIZED value, not a stored field. The unnest WITH-ORDINALITY ordinal
		// (`AT o`) is the canonical case: its FieldValue carries Type values.NotNullInt
		// (Java's Type.primitiveType(INT, false)) but has NO descriptor field, so the
		// colDesc-only path above would default it to ColumnNullable and the result-set
		// metadata would wrongly report the NOT-NULL ordinal as nullable. Derive
		// nullability from the VALUE's own type instead (the same place valueTypeName
		// reads the TYPE), so a NOT-NULL synthesized column (the ordinal, an EXISTS
		// boolean) reports ColumnNoNulls while a genuinely nullable element column
		// (a nullable array element type, an UnknownType fallback) still reports
		// ColumnNullable. RFC-142.
		if t := f.Value.Type(); t != nil && !t.IsNullable() {
			nullable = api.ColumnNoNulls
		}
	}
	return executor.ColumnDef{
		Name:     name,
		Label:    label,
		TypeName: typeName,
		Nullable: nullable,
	}
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
	// RFC-141 Phase 2: a projected-EXISTS FlatMap folds the SELECT projection
	// into its result value — an ordinary (non-anchored-join) RecordConstructor.
	// Its field names ARE the output columns (e.g. ID, HAS_T2), so derive from
	// them directly rather than merging the outer+inner table columns.
	if rc, ok := fm.GetResultValue().(*values.RecordConstructorValue); ok && !rc.AnchoredJoin && len(rc.Fields) > 0 {
		// RFC-141 ROOT FIX: derive each folded column's metadata DIRECTLY
		// from the RecordConstructorField's Name — the SAME name the fold set as the
		// output column key and that RecordConstructorValue.Evaluate keys the
		// executed row by (`out[f.Name] = …`). The earlier code re-derived the datum
		// Name from the field's VALUE (a since-removed bare-name inference heuristic),
		// which DIVERGED from f.Name in two cases:
		//   - an explicit alias equal to the bare leaf (`t1.id AS id`): inferred
		//     UNALIASED, datum Name became the qualified value name `T1.ID` while the
		//     record key is the alias `ID` → a Scan of that column read NULL;
		//   - an unaliased qualified column over a JOIN (`t2.id`): the NLJ rule
		//     rebases the value to the composite FieldValue{Field:ID, Child:QOV} so
		//     the old bare-name compare was skipped → the qualified f.Name was
		//     returned as a fake alias → label leaked `T2.ID`.
		// Using f.Name as the datum Name is correct BY CONSTRUCTION (it cannot
		// diverge from the record key), and the display label is the bare leaf of
		// f.Name — exactly Java's rule (the SELECT-list Identifier post-clearQualifier:
		// `SELECT t1.id` → label ID, `t1.id AS id` → ID, `id AS the_id` → THE_ID),
		// with no value inference. The value is used ONLY for the column TYPE (the
		// EXISTS boolean reports BOOLEAN via ExistsValue.Type(); a column resolves
		// against its defining leg descriptor).
		descs := allLeafDescriptors(fm.GetOuter(), md)
		cols := make([]executor.ColumnDef, 0, len(rc.Fields))
		for _, f := range rc.Fields {
			cols = append(cols, foldedColumnDef(f, descs))
		}
		return cols
	}

	// RFC-141: a plain `WHERE EXISTS` / `WHERE NOT EXISTS` is planned as an
	// IDENTITY FlatMap — its result value is the OUTER row's QuantifiedObjectValue
	// (the existential level only filters; the row that flows out is the outer row
	// unchanged), with the semi-join boolean dropped by a PredicatesFilter above.
	// The cursor emits ONLY the outer row, so the columns are EXACTLY the outer
	// plan's columns. Falling through to the outer+inner merge below would report
	// the inner subquery's columns too (a metadata leak: `SELECT * FROM t1 WHERE
	// EXISTS(SELECT … FROM t2 …)` would advertise t1's AND t2's columns even though
	// only t1's row is returned). Detect the identity-over-outer shape and return
	// the outer columns alone. Projected EXISTS (a RecordConstructor result value)
	// was already handled above; this covers the WHERE-only case where the result
	// value is the bare outer QOV.
	if qov, ok := fm.GetResultValue().(*values.QuantifiedObjectValue); ok &&
		strings.EqualFold(qov.Correlation.Name(), fm.GetOuterAlias().Name()) {
		return deriveColumnsFromPlan(fm.GetOuter(), md)
	}

	// RFC-142: a lateral array unnest (`FROM t, t.arr AS x [AT o]`) lowers to a
	// FlatMap(outer, Explode) whose result value is a source-anchored join record
	// (buildUnnestResultValue → NewAnchoredJoinRecord) carrying the outer leg's
	// columns PLUS the unnested element x (and, under ordinality, the ordinal o).
	// The inner Explode plan has NO derivable record columns, so the outer+inner
	// merge below would report ONLY the outer columns — dropping the element (and
	// ordinal) from the result-set metadata, so `SELECT *` omitted them. Derive the
	// columns directly from the result value's BARE (user-visible, non-dotted)
	// fields instead: those ARE the SELECT-* column set (outer columns + element +
	// ordinal), in declaration order — the same per-field derivation the RFC-141
	// projected-EXISTS fold uses (foldedColumnDef), restricted to the bare keys (the
	// qualified ALIAS.COL forms are resolution-convenience duplicates, exactly as a
	// normal join's `SELECT *` reports bare labels via qualifyAndMergeColumns).
	if rc, ok := fm.GetResultValue().(*values.RecordConstructorValue); ok && rc.AnchoredJoin &&
		findExplodePlan(fm.GetInner()) != nil {
		descs := allLeafDescriptors(fm.GetOuter(), md)
		var cols []executor.ColumnDef
		for _, f := range rc.Fields {
			if strings.Contains(f.Name, ".") {
				continue // qualified ALIAS.COL duplicate key — not a user column
			}
			cols = append(cols, foldedColumnDef(f, descs))
		}
		return cols
	}

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
		// numeric promotion (P2).
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
// findUnfoldableProjectedExists rejects a PROJECTED EXISTS (an ExistsValue in a
// SELECT-list value) in a query shape the RFC-141 fold cannot thread through, so
// the EXISTS would otherwise be silently dropped before translation (the
// post-translation §8 guard can't see a value that no longer exists). This is
// the logical-tree half of the safety guard.
//
// The fold (translateProject → findExistsFilterUnderUnaryChain) folds the
// projection into the existential SelectExpression only when the existential
// filter is reachable from the project's input through transparent unary
// operators — Sort / Limit — or sits directly over a JOIN in FROM. A GROUP BY /
// aggregate, DISTINCT, UNION, or a second Project between the projection and the
// existential filter changes the row shape and is NOT foldable; the projected
// ExistsValue cannot be evaluated with the existential binding live. Reject those
// cleanly with ErrCodeUnsupportedQuery (returned as a message) rather than
// returning constant-false rows.
func findUnfoldableProjectedExists(op logical.LogicalOperator) string {
	if op == nil {
		return ""
	}
	if proj, ok := op.(*logical.LogicalProject); ok && projectValuesReferenceExists(proj.ProjectedValues) {
		if !existsFilterReachableForFold(proj.Input) {
			return "projected EXISTS in this query shape is not yet supported"
		}
		// A projected EXISTS alongside a CORRELATED scalar subquery in the SAME
		// SELECT list (`SELECT id, EXISTS(...), (SELECT v FROM t2 WHERE t2.fk =
		// t1.id) FROM t1`) cannot be folded: the projected-EXISTS fold builds an
		// existential SelectExpression whose result value is the projection
		// RecordConstructor evaluated by the FlatMap, while the correlated-scalar
		// path (translateProjectWithCorrelatedScalar) builds a DIFFERENT structure
		// — a LEFT-OUTER join select anchored on the outer row with
		// NewScalarSubqueryAnchoredRecord and its own per-row LIMIT-peel. Composing
		// both into one SelectExpression is a 3-way quantifier nest the NLJ rule
		// does not implement (the multi-quantifier boundary the port rejects).
		// Without this check the fold's early return in translateProject bypasses
		// the correlated-scalar dispatch and the correlated ScalarSubqueryValue is
		// left unbound → that column silently reads NULL every row. Reject cleanly.
		// (Uncorrelated scalar subqueries DO compose — they are pre-evaluated and
		// collected before the fold's early return, so they are not rejected here.)
		if len(proj.CorrelatedScalarSubqueries) > 0 {
			return "projected EXISTS in this query shape is not yet supported"
		}
	}
	// A projected EXISTS that also appears as a GROUP BY key or an aggregate
	// operand lands in the LogicalAggregate's resolved Value trees, NOT the
	// project's — the aggregate never folds an existential, so the EXISTS would
	// be silently dropped. Reject.
	if agg, ok := op.(*logical.LogicalAggregate); ok {
		if projectValuesReferenceExists(agg.GroupKeyValues) || projectValuesReferenceExists(agg.AggregateOperands) {
			return "projected EXISTS in this query shape is not yet supported"
		}
	}
	for _, ch := range op.Children() {
		if msg := findUnfoldableProjectedExists(ch); msg != "" {
			return msg
		}
	}
	return ""
}

// projectValuesReferenceExists reports whether any projected Value is (or
// contains) an ExistsValue — structurally, no text matching.
func projectValuesReferenceExists(vals []values.Value) bool {
	for _, v := range vals {
		if v == nil {
			continue
		}
		found := false
		values.WalkValue(v, func(node values.Value) bool {
			if _, ok := node.(*values.ExistsValue); ok {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// existsFilterReachableForFold reports whether a LogicalFilter carrying
// existential subqueries is reachable from `input` through ONLY fold-transparent
// unary operators (Sort/Limit). It consults logical.FoldTransparentUnaryInput —
// the SAME shared transparency set the translator's findExistsFilterUnderUnaryChain
// folds through — so a shape this accepts is exactly a shape the translator folds,
// and the two can never silently diverge. Any other intervening operator (Project,
// Aggregate, Distinct, Union) means the projected EXISTS cannot be folded.
func existsFilterReachableForFold(input logical.LogicalOperator) bool {
	cur := input
	for {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			return len(f.ExistsSubqueries) > 0
		}
		next, ok := logical.FoldTransparentUnaryInput(cur)
		if !ok {
			return false
		}
		cur = next
	}
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

// demoteSchemaQualifiedUnnest enforces Java's `LogicalOperator.generateAccess`
// resolution ORDER on a lateral-unnest candidate: a FROM identifier is resolved
// as a CTE / TABLE / view / function FIRST, and only falls through to
// `resolveCorrelatedIdentifier` (an in-scope correlated array field) when none
// of those match. The parser classifies a dotted comma source as a
// LogicalUnnest whenever segment 0 names a VISIBLE in-scope FROM-source alias —
// but it has no metadata, so it cannot run the table-first check. When the prior
// alias HAPPENS to equal the session schema name (`FROM PA AS s, s.PB AS B`),
// `s.PB` is in truth a schema-qualified TABLE (`tableExists` in Java: qualifier
// == schema name AND table `PB` exists), so the table branch must win — it is a
// plain cross join, never a correlated unnest. This pass walks the tree and, for
// any LogicalJoin whose Right is a schema-qualified-table LogicalUnnest, demotes
// it back to a LogicalScan of the resolved bare table name (mirroring
// `resolveQualifiedTableNames` stripping `schema.` off a normal scan).
//
// When the schema-qualified table carries an AT ordinal alias (`FROM PA AS s,
// s.PB AT ord`), Java's table branch still wins — but it asserts
// `atAlias.isEmpty()` and throws WRONG_OBJECT_TYPE ("'PB' is a table"). We surface
// that code HERE (early, before scope binding tries to resolve a projection
// against the would-be unnest), rather than leaving the source on the unnest path
// where the projection scope binding could fail first with a misleading
// undefined-column error. A genuine correlated array (`FROM T1, T1.arr`, where the
// qualifier `T1` is NOT the schema name) is left untouched — it is not a schema-
// qualified table, so it correctly falls through to the correlated-field path.
// RFC-142 (P2b).
func demoteSchemaQualifiedUnnest(op logical.LogicalOperator, schemaName string, md *recordlayer.RecordMetaData) error {
	if op == nil || md == nil {
		return nil
	}
	if j, ok := op.(*logical.LogicalJoin); ok {
		if u, ok := j.Right.(*logical.LogicalUnnest); ok {
			if table, alias, isTable := schemaQualifiedUnnestTable(u, schemaName, md); isTable {
				if u.AtAlias != "" {
					// AT on a schema-qualified TABLE → Java's table-branch
					// atAlias.isEmpty() assert → WRONG_OBJECT_TYPE.
					return api.NewError(api.ErrCodeWrongObjectType,
						"AT ordinality is only valid on a correlated array source, not a table")
				}
				j.Right = logical.NewScan(table, alias)
			}
		}
	}
	for _, ch := range op.Children() {
		if err := demoteSchemaQualifiedUnnest(ch, schemaName, md); err != nil {
			return err
		}
	}
	// Children() exposes only the operator's primary input tree; the nested
	// logical plans for EXISTS / scalar subqueries are carried as side fields on
	// LogicalFilter / LogicalProject / LogicalAggregate and are NOT children. A
	// schema-qualified-table LogicalUnnest can live INSIDE such a subquery
	// (`… WHERE EXISTS (SELECT 1 FROM PA AS s, s.PB AS B)`), so the table-first
	// demotion — Java's generateAccess runs at EVERY FROM-source resolution
	// point, including inside subqueries — must reach those plans too, else
	// `s.PB` is wrongly translated as a correlated unnest of the missing field
	// `PB` on source `s`. RFC-142 (P2).
	for _, sub := range subqueryPlans(op) {
		if err := demoteSchemaQualifiedUnnest(sub, schemaName, md); err != nil {
			return err
		}
	}
	return nil
}

// rejectAtOrdinalityOnTable enforces Java's `generateAccess` AT-on-a-table
// rejection EARLY — at FROM-source analysis time, before the SELECT/WHERE column
// resolution — so the faithful WRONG_OBJECT_TYPE (42809) is the surfaced error and
// is NOT masked by a scope-level undefined-column (42703) / ambiguous (42702)
// raised while resolving a projection.
//
// The masking bug: for an AT comma source that is in truth a TABLE — a
// SINGLE-segment `FROM T1, U AT O` (U a real table), the bare-source `T1, T1 AT O`,
// a present-but-scalar correlated field `T1.ID AS X AT O`, or a schema-qualified
// `s.PB AT O` — the parser keeps it a LogicalUnnest (the AT shortcut in
// unnestCandidateShape) so the AT survives to a clean rejection, and the SELECT
// scope registers a VIRTUAL unnest binding (correlation = the AT alias). A
// reference to the REAL table's own column (`U.ID`) then fails to resolve at the
// scope level (the real table `U` is shadowed by the virtual binding) with a
// MASKING 42703 BEFORE translation. Running the rejection here — before any
// projection column resolution — surfaces the intended 42809 regardless of what the
// query references.
//
// This MIRRORS the translator's `translateUnnestJoin` AT-rejection EXACTLY (it is
// the authority; the early pass is a faithful echo): an AT source is WRONG_OBJECT_
// TYPE when
//
//	(1) segment 0 does NOT resolve to a visible in-scope SCAN in the outer leg
//	    (a table / schema-qualified / unknown qualifier — Java's findOuterScanTable
//	    == "" → unnestFallbackOrReject), OR
//	(2) segment 0 resolves to a REAL base table whose remaining segment(s) name a
//	    field that is MISSING / a single-segment bare source / a PRESENT SCALAR
//	    (Java's generateCorrelatedFieldAccess "repeated type" assert).
//
// A GENUINE array (planned), a CTE/derived-output source (record type not in md, OR
// an in-scope WITH-CTE / derived-table source shadowing a real same-named table →
// left to the translator's outerSourceIsCTE / outerSourceIsDerivedTable
// UNSUPPORTED_QUERY), and a missing field on a real table (the translator's
// UNDEFINED_COLUMN — distinct from a present scalar) are NOT rejected here, so the
// early pass never DIVERGES from the translator's per-case code. RFC-142.
func rejectAtOrdinalityOnTable(op logical.LogicalOperator, md *recordlayer.RecordMetaData) error {
	return rejectAtOrdinalityOnTableWithCTEs(op, md, nil)
}

// rejectAtOrdinalityOnTableWithCTEs is the recursion carrying the set of WITH-CTE
// names in scope at `op`. A FROM source whose segment 0 names an in-scope CTE binds
// to the CTE's OUTPUT type, not a base-table descriptor — so it is the translator's
// outerSourceIsCTE territory and is left to its UNSUPPORTED_QUERY rejection, never
// the base-table AT check here (which would, when the CTE name ALSO matches a real
// table, raise a WRONG_OBJECT_TYPE keyed on the SHADOWED base table and diverge from
// the translator). A WITH CTE wraps the SELECT's join tree in an enclosing
// LogicalCTE, so the CTE name is not visible from `j.Left` (only Scan(name) is
// there) — it must be threaded down from the wrapper. Derived tables `(…) AS d`
// instead lower to a LogicalCTE leg INSIDE j.Left and are caught structurally by
// atOnNonArraySource's OuterSourceIsDerivedTable check.
func rejectAtOrdinalityOnTableWithCTEs(op logical.LogicalOperator, md *recordlayer.RecordMetaData, cteNames map[string]struct{}) error {
	if op == nil || md == nil {
		return nil
	}
	if cte, ok := op.(*logical.LogicalCTE); ok {
		// Extend the in-scope CTE set for this subtree (the CTE name is visible to
		// its Main projection and any nested CTEs).
		next := make(map[string]struct{}, len(cteNames)+1)
		for k := range cteNames {
			next[k] = struct{}{}
		}
		next[strings.ToUpper(cte.Name)] = struct{}{}
		cteNames = next
	}
	if j, ok := op.(*logical.LogicalJoin); ok {
		if u, ok := j.Right.(*logical.LogicalUnnest); ok && u.AtAlias != "" {
			if atOnNonArraySource(j.Left, u, md, cteNames) {
				return api.NewError(api.ErrCodeWrongObjectType,
					"AT ordinality is only valid on a correlated array source, not a table")
			}
		}
	}
	for _, ch := range op.Children() {
		if err := rejectAtOrdinalityOnTableWithCTEs(ch, md, cteNames); err != nil {
			return err
		}
	}
	// AT-on-a-table can appear inside an EXISTS / scalar subquery's own FROM scope
	// (carried on side fields, not Children()) — Java's generateAccess runs at every
	// FROM point. Reach those plans too, like demoteSchemaQualifiedUnnest. RFC-142.
	for _, sub := range subqueryPlans(op) {
		if err := rejectAtOrdinalityOnTableWithCTEs(sub, md, cteNames); err != nil {
			return err
		}
	}
	return nil
}

// atOnNonArraySource reports whether an AT-bearing LogicalUnnest is in truth an
// AT on a TABLE / non-array source (cases (1)/(2) of rejectAtOrdinalityOnTable),
// resolving segment 0 against the outer leg's visible scans (the shared
// logical.FindOuterScanTable walk the translator's findOuterScanTable also uses).
// RFC-142.
func atOnNonArraySource(left logical.LogicalOperator, u *logical.LogicalUnnest, md *recordlayer.RecordMetaData, cteNames map[string]struct{}) bool {
	if len(u.Segments) == 0 {
		return false
	}
	// A CTE / derived-table source bound to segment 0 is the translator's
	// outerSourceIsCTE / outerSourceIsDerivedTable territory: its OUTPUT type — not a
	// base-table descriptor — governs whether the AT field is an array, and the
	// translator rejects a CTE/derived-output unnest with UNSUPPORTED_QUERY. Detect
	// that BEFORE the md.GetRecordType lookup below, so a CTE/derived source whose
	// alias ALSO names a REAL same-named base table does NOT fall through to the
	// base-table AT-on-non-array check (which would raise a 42809 keyed on the
	// SHADOWED base table instead of the translator's intended UNSUPPORTED_QUERY).
	// Two shapes:
	//   - segment 0 names an in-scope WITH CTE (threaded down from the enclosing
	//     LogicalCTE wrapper) — the translator's outerSourceIsCTE arm;
	//   - segment 0 binds to a derived-table LogicalCTE leg INSIDE the outer plan
	//     (`(SELECT …) AS d`) — the translator's structural outerSourceIsDerivedTable
	//     arm (OuterSourceIsDerivedTable).
	// Only a genuine REAL base table reaches the WRONG_OBJECT_TYPE check below.
	if _, ok := cteNames[strings.ToUpper(u.Segments[0])]; ok {
		return false
	}
	if logical.OuterSourceIsDerivedTable(left, u.Segments[0]) {
		return false
	}
	outerTable := logical.FindOuterScanTable(left, u.Segments[0])
	if outerTable == "" {
		// (1) segment 0 not a visible in-scope scan: a table / schema-qualified /
		//     unknown qualifier — the translator's unnestFallbackOrReject AT path.
		return true
	}
	rt := md.GetRecordType(outerTable)
	if rt == nil || rt.Descriptor == nil {
		// segment 0 binds to a source whose record type is not a base table (a
		// CTE / derived output): the translator handles it (outerSourceIsCTE →
		// UNSUPPORTED_QUERY). Leave it — do NOT raise WRONG_OBJECT_TYPE.
		return false
	}
	// (2) Real base table. A bare source (single segment, no field) or a field
	//     that is MISSING / a PRESENT SCALAR is not an array.
	if len(u.Segments) < 2 {
		// AT on a bare real-table source (`FROM T1, T1 AT O`) — no field segment.
		return true
	}
	if len(u.Segments[1:]) != 1 {
		// A multi-segment field path is not a top-level array unnest shape (mirrors
		// unnestArrayElementType's single-segment requirement) — let the translator
		// table-fallback / reject it; do not raise WRONG_OBJECT_TYPE here.
		return false
	}
	fd := lookupFieldFold(rt.Descriptor, u.Segments[1])
	if fd == nil {
		// Missing field on a real table → the translator's clean UNDEFINED_COLUMN,
		// NOT WRONG_OBJECT_TYPE. Leave it to the translator.
		return false
	}
	// Present field: an array is a genuine unnest (not rejected); a scalar is the
	// "repeated type" assert → WRONG_OBJECT_TYPE.
	return !fd.IsList()
}

// lookupFieldFold returns the proto field descriptor named `name` on `desc`
// case-insensitively (SQL identifiers are case-folded; proto names are often
// lower/snake), mirroring unnestArrayElementType's field lookup. RFC-142.
func lookupFieldFold(desc protoreflect.MessageDescriptor, name string) protoreflect.FieldDescriptor {
	if fd := desc.Fields().ByName(protoreflect.Name(strings.ToLower(name))); fd != nil {
		return fd
	}
	fields := desc.Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		if strings.EqualFold(string(f.Name()), name) {
			return f
		}
	}
	return nil
}

// rejectDuplicateUnnestAlias enforces — at FROM-source analysis time, before any
// projection/WHERE column resolution — that a lateral array unnest's AS / AT alias
// does not collide with ANY OTHER source alias in the SAME FROM scope, EARLIER OR
// LATER. Java's SemanticAnalyzer registers each FROM range-variable into one scope
// and forbids two sources sharing a name (a duplicate quantifier alias is a binding
// error); the unnest's AS (element) / AT (ordinal) names participate in that same
// uniqueness rule.
//
// The translator's translateUnnestJoin already rejects the EARLIER collision
// (the unnest alias vs an outer / already-bound source) and the AS == AT case, but
// it lowers a left-deep join chain bottom-up: when it processes the unnest's join
// (`FROM T1, T1.arr AS V`) it cannot see a LATER comma source (`, U AS V`), which is
// the RIGHT child of an ANCESTOR join. So `FROM T1, T1.arr AS V, U AS V` was planned
// with BOTH legs under alias V; the outer NestedLoopJoin's mergeRows overwrites the
// unnest's bare/qualified V keys last-leg-wins with U's keys → a projection of V
// reads U.V (the wrong source) instead of the unnested element — silent-wrong rows,
// never the duplicate-alias error. This pass closes the gap: it sees the WHOLE FROM
// chain, so a later source reusing the unnest alias is rejected cleanly here.
//
// Running it over the full tree (and into subquery plans, like rejectAtOrdinalityOn
// Table) covers an unnest whose colliding later source lives in an EXISTS / scalar
// subquery's own FROM scope. RFC-142.
func rejectDuplicateUnnestAlias(op logical.LogicalOperator) error {
	if op == nil {
		return nil
	}
	// A LogicalJoin is the root of a FROM-scope join chain. Collect every source
	// alias in that chain and reject any unnest whose AS/AT alias duplicates
	// another source's. The chain walk stops at a derived/CTE Body — a derived
	// source is its own FROM scope — exactly like outerBoundAliases /
	// buriedUnnestLegs; the recursion below then re-enters those nested scopes.
	if j, ok := op.(*logical.LogicalJoin); ok {
		if err := checkFromScopeUnnestAliases(j); err != nil {
			return err
		}
	}
	for _, ch := range op.Children() {
		if err := rejectDuplicateUnnestAlias(ch); err != nil {
			return err
		}
	}
	for _, sub := range subqueryPlans(op) {
		if err := rejectDuplicateUnnestAlias(sub); err != nil {
			return err
		}
	}
	return nil
}

// checkFromScopeUnnestAliases gathers every leaf source alias of the FROM-scope
// join chain rooted at `j` (Scan aliases, prior-unnest AS/AT aliases, derived/CTE
// leg OUTER aliases — never descending into a derived/CTE Body, which is a separate
// scope) and rejects any lateral-unnest leg in that chain whose AS or AT alias also
// names another source in the same chain. The check is symmetric across the chain,
// so it catches both an earlier and a later collision. RFC-142.
func checkFromScopeUnnestAliases(j *logical.LogicalJoin) error {
	var scans []string // every leaf source alias (Scan / derived leg) in the chain
	var unnests []*logical.LogicalUnnest
	var walk func(logical.LogicalOperator)
	walk = func(o logical.LogicalOperator) {
		switch n := o.(type) {
		case *logical.LogicalScan:
			a := n.Alias
			if a == "" {
				a = n.Table
			}
			if a != "" {
				scans = append(scans, strings.ToUpper(a))
			}
		case *logical.LogicalUnnest:
			unnests = append(unnests, n)
		case *logical.LogicalCTE:
			// A derived/CTE leg contributes only its OUTER alias (its Main is a
			// Scan(name)); its Body is a separate FROM scope, not descended here.
			walk(n.Main)
		default:
			for _, c := range o.Children() {
				walk(c)
			}
		}
	}
	walk(j)
	if len(unnests) == 0 {
		return nil
	}
	// Build, for each unnest, the set of OTHER sources' aliases: every scan alias
	// plus every OTHER unnest's AS/AT aliases. A collision against any of them is a
	// duplicate range-variable name.
	for _, u := range unnests {
		others := make(map[string]struct{}, len(scans)+2*len(unnests))
		for _, a := range scans {
			others[a] = struct{}{}
		}
		for _, ou := range unnests {
			if ou == u {
				continue
			}
			if ou.Alias != "" {
				others[strings.ToUpper(ou.Alias)] = struct{}{}
			}
			if ou.AtAlias != "" {
				others[strings.ToUpper(ou.AtAlias)] = struct{}{}
			}
		}
		for _, name := range []string{u.Alias, u.AtAlias} {
			if name == "" {
				continue
			}
			if _, dup := others[strings.ToUpper(name)]; dup {
				return api.NewError(api.ErrCodeDuplicateAlias,
					"lateral unnest alias collides with another FROM-source alias; use a distinct AS/AT alias")
			}
		}
	}
	return nil
}

// subqueryPlans returns the nested logical plans an operator carries on its
// side fields (EXISTS / scalar subqueries) — the plans NOT reachable via
// Children(). These are the FROM scopes that a schema-qualified-table unnest
// (or any per-source resolution) can appear in beyond the operator's primary
// input. Mirrors the set of subquery-plan fields the cascades translator walks
// (LogicalFilter / LogicalProject / LogicalAggregate). RFC-142.
func subqueryPlans(op logical.LogicalOperator) []logical.LogicalOperator {
	var plans []logical.LogicalOperator
	switch o := op.(type) {
	case *logical.LogicalFilter:
		for _, esq := range o.ExistsSubqueries {
			plans = append(plans, esq.Plan)
		}
		for _, ssq := range o.ScalarSubqueries {
			plans = append(plans, ssq.Plan)
		}
	case *logical.LogicalProject:
		for _, ssq := range o.ScalarSubqueries {
			plans = append(plans, ssq.Plan)
		}
		for _, csq := range o.CorrelatedScalarSubqueries {
			plans = append(plans, csq.InnerPlan)
		}
	case *logical.LogicalAggregate:
		for _, esq := range o.HavingExistsSubqueries {
			plans = append(plans, esq.Plan)
		}
		for _, ssq := range o.HavingScalarSubqueries {
			plans = append(plans, ssq.Plan)
		}
	}
	return plans
}

// schemaQualifiedUnnestTable reports whether a lateral-unnest candidate is in
// truth a schema-qualified TABLE reference (Java's `tableExists` precedence),
// and if so returns the resolved bare table name and the FROM alias to scan it
// under. It is a schema-qualified table IFF its segments are exactly
// `[qualifier, table]`, the qualifier case-insensitively equals the session
// schema name, and `table` resolves to a real record type — precisely Java's
// `tableExists` (one qualifier segment == schema-template name + table in the
// catalog). An AT alias does NOT change whether it is a TABLE (the caller handles
// AT separately: a table cross join when AT is absent, WRONG_OBJECT_TYPE when
// present). RFC-142.
func schemaQualifiedUnnestTable(u *logical.LogicalUnnest, schemaName string, md *recordlayer.RecordMetaData) (table, alias string, ok bool) {
	if len(u.Segments) != 2 {
		return "", "", false
	}
	if !strings.EqualFold(u.Segments[0], schemaName) {
		return "", "", false
	}
	tableName := u.Segments[1]
	if !recordTypeExistsFold(md, tableName) {
		return "", "", false
	}
	a := u.Alias
	if a == "" || a == strings.Join(u.Segments, ".") {
		// No explicit AS: scan under the bare table name (Java defaults the
		// quantifier alias to the table name).
		a = tableName
	}
	return tableName, a, true
}

// recordTypeExistsFold reports whether md has a record type named `name`
// case-insensitively (SQL identifiers are case-folded; proto names may be mixed
// case). Mirrors cascadesTranslator.resolveRecordType's fallback. RFC-142.
func recordTypeExistsFold(md *recordlayer.RecordMetaData, name string) bool {
	if md == nil {
		return false
	}
	if md.GetRecordType(name) != nil {
		return true
	}
	for n := range md.RecordTypes() {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

// defaultEmbeddedSchema is the schema name the embedded planner uses when no
// session schema is supplied (the FDB test / EXPLAIN harnesses). The session
// path passes the real CONNECT schema (g.c.sess.Schema). RFC-142.
const defaultEmbeddedSchema = "s"

// sessionSchema returns the active CONNECT schema, falling back to
// defaultEmbeddedSchema when there is no session (explain-only generator) or
// the session never set one. EXPLAIN / DDL explain paths use this so the DML
// catalog builders classify a schema-qualified comma source against the SAME
// active schema the live planSelect/planDML paths use (g.c.sess.Schema). RFC-142.
func (g *cascadesGenerator) sessionSchema() string {
	if g.c != nil && g.c.sess != nil && g.c.sess.Schema != "" {
		return g.c.sess.Schema
	}
	return defaultEmbeddedSchema
}

// newUnnestTableResolver builds the table-first resolver (Java's `tableExists`
// precedence) the lateral-unnest classifier consults: a dotted FROM-source name
// resolves to a schema-qualified TABLE — and is therefore NOT a correlated
// unnest — when its segments are exactly `[qualifier, name]`, `qualifier`
// case-insensitively equals the session schema name, and `name` is a real record
// type. This mirrors Java's `tableExists`: one qualifier segment == the
// schema-template name plus a table found in the catalog.
//
// A dotted reference whose qualifier is a CTE/derived alias (`cte.col`,
// `d.col`) is NOT matched here: a CTE reference in Java's `findCteMaybe` matches
// only an UNQUALIFIED name, so a qualified `cte.col` never resolves to a CTE. The
// CTE-output unnest case (`FROM cte, cte.arr`) is handled on the correlated path
// and validated against the CTE OUTPUT type — P2a (translateUnnestJoin's
// outerSourceIsCTE rejection). RFC-142.
func newUnnestTableResolver(md *recordlayer.RecordMetaData, schemaName string) tableResolver {
	return func(segments []string) bool {
		if len(segments) != 2 {
			return false
		}
		if !strings.EqualFold(segments[0], schemaName) {
			return false
		}
		return recordTypeExistsFold(md, segments[1])
	}
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
	// Subquery plans (EXISTS / scalar) carried on side fields are not Children();
	// a schema-qualified table scan can live inside one (`… EXISTS (SELECT 1 FROM
	// PA, s.PB AS B)`), so strip its `schema.` qualifier there too — the same
	// structural gap the subquery-aware demoteSchemaQualifiedUnnest walk covers
	// for the unnest variant. RFC-142 (P2).
	for _, sub := range subqueryPlans(op) {
		if err := resolveQualifiedTableNames(sub, schemaName); err != nil {
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
		"CURRENT_USER",
		// CARDINALITY is a dedicated by-name built-in (expr.walkCardinality
		// → CardinalityValue), not a generic ScalarFunctionValue, so it lives
		// here rather than in IsCascadesSafeScalarFunction — the Cascades walk
		// builds its own Value with nullable-INT typing and array validation.
		"CARDINALITY":
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
