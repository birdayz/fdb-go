package embedded

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antlr4-go/antlr/v4"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	cascades "fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	"fdb.dev/pkg/relational/core/metadata"
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/session"
	"google.golang.org/protobuf/proto"
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

func contextCancellationError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("embedded planner: nil context")
	}
	err := ctx.Err()
	if err == nil {
		return nil
	}
	cause := context.Cause(ctx)
	if cause == nil || cause == err {
		return err
	}
	return fmt.Errorf("%w: %w", err, cause)
}

func isContextCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func explainWithContext(ctx context.Context, explain func() string) (string, error) {
	if err := contextCancellationError(ctx); err != nil {
		return "", err
	}
	text := explain()
	if err := contextCancellationError(ctx); err != nil {
		return "", err
	}
	return text, nil
}

func (g *cascadesGenerator) Plan(ctx context.Context, sql string) (query.Plan, error) {
	if err := contextCancellationError(ctx); err != nil {
		return nil, err
	}
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
		if err := contextCancellationError(ctx); err != nil {
			return nil, err
		}
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
		// the legacy embedded interpreter, which RFC-145 detached (Phase 1) and
		// deleted (Phase 2). This stub remains as the unreachable ExecFn.
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
	if err := contextCancellationError(ctx); err != nil {
		return nil, err
	}
	// Plan-cache key parts: a VERBATIM schema+version scope (case-sensitive,
	// never normalized) and the injective canonical query text. NOT q.GetText()
	// — that concatenated tokens with no separator, colliding `SELECT AB` with
	// `SELECT A B`. PlanCache normalizes only the query text (see planCacheScope
	// / PlanCache.Get).
	cacheScope := planCacheScope(g.c.sess.Schema, md.Version())
	cacheSQL := canonicalTextOf(q)
	var ls *planLogScope
	if logMetrics {
		// Log the original whitespace-preserved SQL (canonicalTextOf), not
		// q.GetText() — the latter concatenates tokens without whitespace
		// ("SELECTid=1FROMorders"), which is useless to an operator. The cache
		// key (cacheScope + cacheSQL, built above) is also off canonicalTextOf,
		// so both are injective.
		ls = g.beginPlanLog(ctx, canonicalTextOf(q))
	}
	defer func() { ls.finish(err) }()

	if g.cache != nil {
		if cachedPlan, cachedSubs, ok := g.cache.Get(cacheScope, cacheSQL); ok {
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

	// A windowed aggregate (`SUM(v) OVER (PARTITION BY g)`) is not supported. The
	// aggregate planner ignores the OVER clause and computes a bare aggregate,
	// which silently returns WRONG results, so reject it up front — a true
	// front-end pre-pass (before building a soon-discarded logical plan). Detected
	// on the parse tree because the OVER clause is dropped during lowering, so the
	// logical plan provably cannot carry it (mirrors findAggregateInTree).
	if windowedAggregateInTree(q) {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"windowed aggregate (aggregate function with an OVER clause) is not supported")
	}

	visitor := NewPlanVisitorWithSchema(md, g.c.sess.Schema)
	logicalOp, buildErr := visitor.VisitQuery(q)
	if buildErr != nil {
		return nil, buildErr
	}
	if logicalOp == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, plannerUnableToPlanMessage)
	}

	if fn := query.FindUnsupportedFunction(logicalOp); fn != "" {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
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
	if err := rejectDuplicateUnnestAlias(logicalOp, g.c.cachedMetaData()); err != nil {
		return nil, err
	}

	if err := resolveQualifiedTableNames(logicalOp, g.c.sess.Schema); err != nil {
		return nil, err
	}

	if err := validateTablesAndColumns(logicalOp, md); err != nil {
		return nil, err
	}

	if msg := findDistinctAggregate(logicalOp); msg != "" {
		// Java rejects DISTINCT aggregates with UNSUPPORTED_QUERY (0AF00) in
		// ExpressionVisitor.visitAggregateWindowedFunction; match that SQLSTATE.
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, msg)
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

	// Point-of-truth CTE alias arity over the WHOLE built tree — covers
	// unused CTEs (whose bodies the translator registers lazily and never
	// descends into) and CTEs nested inside another CTE's body.
	if arityErr := query.ValidateCTEAliasArities(logicalOp); arityErr != nil {
		return nil, arityErr
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
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, plannerUnableToPlanMessage)
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

	bestExpr, _, planErr := planner.PlanWithContext(ctx, ref)
	if planErr != nil {
		return nil, translatePlannerError(planErr, plannerUnableToPlanMessage)
	}
	if bestExpr == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, plannerUnableToPlanMessage)
	}

	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	ph, ok := bestExpr.(planExtractor)
	if !ok {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, plannerUnableToPlanMessage)
	}
	physPlan := ph.GetRecordQueryPlan()
	if physPlan == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, plannerUnableToPlanMessage)
	}
	// RFC-164 WS-2: structural plan invariants on the PRODUCTION path — a relink
	// that dropped a child fails loudly here rather than executing to wrong/zero
	// rows (the IN-LIMIT symptom).
	if err := cascades.ValidatePlanInvariants(physPlan); err != nil {
		return nil, api.NewError(api.ErrCodeInternalError, "malformed query plan: "+err.Error())
	}
	// Plan scalar subqueries independently through the Cascades pipeline
	// (planScalarSubqueryPlans — the one planning path, shared with the
	// plan harness).
	scalarSubs, subErr := planScalarSubqueryPlans(ctx, scalarSubqueryPlans, md, stats)
	if subErr != nil {
		return nil, subErr
	}

	ls.setPlan(physPlan)
	// LIMIT/OFFSET queries are cacheable: the limit is now carried by the
	// RecordQueryLimitPlan operator inside the cached physical plan (RFC-128),
	// not applied post-execution, so the cached plan is complete.
	if g.cache != nil {
		ls.setCache(PlanCacheMiss)
		g.cache.Put(cacheScope, cacheSQL, physPlan, scalarSubs)
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
	planText, explainErr := g.computeExplainText(ctx, descStmts)
	if explainErr != nil {
		return nil, explainErr
	}
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
func (g *cascadesGenerator) computeExplainText(ctx context.Context, d *antlrgen.DescribeStatementsContext) (string, error) {
	if err := contextCancellationError(ctx); err != nil {
		return "", err
	}
	c := g.c
	md := c.cachedMetaData()

	// SELECT: try Cascades pipeline for physical plan explain.
	if q := d.Query(); q != nil {
		// Try Cascades for a physical plan explain when FDB + metadata are available.
		if c.sess != nil && c.sess.DB != nil && !referencesInformationSchema(q) {
			if err := c.ensureMetaData(ctx); err == nil {
				if freshMd := c.cachedMetaData(); freshMd != nil {
					if plan, planErr := g.planSelectCascades(ctx, q, freshMd, false); planErr == nil {
						return explainWithContext(ctx, plan.Explain)
					} else if isContextCancellation(planErr) {
						return "", planErr
					}
				}
			} else if isContextCancellation(err) {
				return "", err
			}
		}
		if err := contextCancellationError(ctx); err != nil {
			return "", err
		}
		// Fallback to logical plan text.
		if md != nil {
			if op, err := buildLogicalPlanForQueryWithCatalog(q, md); err == nil && op != nil {
				return explainWithContext(ctx, func() string { return op.Explain("") })
			}
		}
		if op := buildLogicalPlanForQuery(q); op != nil {
			return explainWithContext(ctx, func() string { return op.Explain("") })
		}
	}
	if del := d.DeleteStatement(); del != nil {
		if md != nil {
			if op, _ := buildLogicalPlanForDeleteWithCatalog(del, md, g.sessionSchema()); op != nil {
				return explainWithContext(ctx, func() string { return op.Explain("") })
			}
		}
		if op := buildLogicalPlanForDelete(del); op != nil {
			return explainWithContext(ctx, func() string { return op.Explain("") })
		}
	}
	if ins := d.InsertStatement(); ins != nil {
		if md != nil {
			if op, _ := buildLogicalPlanForInsertWithCatalog(ins, md, g.sessionSchema()); op != nil {
				return explainWithContext(ctx, func() string { return op.Explain("") })
			}
		}
		if op := buildLogicalPlanForInsert(ins); op != nil {
			return explainWithContext(ctx, func() string { return op.Explain("") })
		}
	}
	if upd := d.UpdateStatement(); upd != nil {
		if md != nil {
			if op, _ := buildLogicalPlanForUpdateWithCatalog(upd, md, g.sessionSchema()); op != nil {
				return explainWithContext(ctx, func() string { return op.Explain("") })
			}
		}
		if op := buildLogicalPlanForUpdate(upd); op != nil {
			return explainWithContext(ctx, func() string { return op.Explain("") })
		}
	}
	if err := contextCancellationError(ctx); err != nil {
		return "", err
	}
	return "", nil
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

// dmlHasDryRunOption reports whether a DML statement carries OPTIONS (DRY RUN) ANYWHERE in
// its parse subtree. DRY RUN is a statement-level directive, but depending on the spelling
// the grammar attaches the trailing OPTIONS clause to different nodes: a VALUES insert puts
// it on insertStatement.queryOptions, while an `INSERT … SELECT … OPTIONS (DRY RUN)` is
// consumed by the inner SELECT's queryTerm.queryOptions (#simpleTable), leaving
// insertStatement.queryOptions nil. Checking only the statement-level clause therefore
// MISSES the INSERT…SELECT spelling — and a missed DRY RUN COMMITS the mutation, the exact
// data-loss the option exists to prevent.
//
// So this walks the whole DML subtree, matching Java's AstNormalizer, which visits every
// queryOptions node and accumulates them into one statement-level Options (DRY_RUN set at
// AstNormalizer.java:281). Over-detection only ever previews (no mutation), so a tree-wide
// walk fails safe; the grammar's queryOption alternatives are
// `NOCACHE | LOG QUERY | DRY RUN | EF_SEARCH n` and DRY RUN is the only one whose omission
// changes whether data is mutated.
func dmlHasDryRunOption(tree antlr.Tree) bool {
	if tree == nil {
		return false
	}
	if qo, ok := tree.(antlrgen.IQueryOptionsContext); ok {
		for _, opt := range qo.AllQueryOption() {
			if opt != nil && opt.DRY() != nil && opt.RUN() != nil {
				return true
			}
		}
	}
	for i := 0; i < tree.GetChildCount(); i++ {
		if dmlHasDryRunOption(tree.GetChild(i)) {
			return true
		}
	}
	return false
}

// updateHasDefaultAssignment reports whether an UPDATE has a `SET col = DEFAULT`
// assignment. The grammar is `updatedElement : fullColumnName '=' (expression | DEFAULT)`,
// so the DEFAULT alternative is detected via the DEFAULT() terminal.
func updateHasDefaultAssignment(upd antlrgen.IUpdateStatementContext) bool {
	if upd == nil {
		return false
	}
	for _, el := range upd.AllUpdatedElement() {
		if el != nil && el.DEFAULT() != nil {
			return true
		}
	}
	return false
}

// updateHasSubqueryAssignment reports whether any UPDATE ... SET RHS contains
// a query-bearing form: the scalar atom `(SELECT ...)`, its sibling atom
// `EXISTS(SELECT ...)`, or an `IN (SELECT ...)` list carrying a query body —
// three DISTINCT grammar contexts, each probed writing its literal text.
// Subqueries in SET are unsupported, and without this guard the builder
// treated the RHS as a plain expression whose CANONICAL TEXT became the
// written string value (silent data corruption, review probes; identical on
// master). Java's ExpressionVisitor has no subquery arm for the SET RHS
// either, so per the conformance principle the shape gets a CLEAN error,
// never a corrupt write. Contains-anywhere by design: the corruption
// mechanism is whole-RHS text canonicalization, so a subquery nested in
// CASE/arithmetic in SET hits the identical path. A plain value-list IN
// (`IN (1,2,3)`, no query body) passes through untouched.
func updateHasSubqueryAssignment(upd antlrgen.IUpdateStatementContext) bool {
	if upd == nil {
		return false
	}
	var containsSubquery func(t antlr.Tree) bool
	containsSubquery = func(t antlr.Tree) bool {
		if t == nil {
			return false
		}
		switch n := t.(type) {
		case *antlrgen.SubqueryExpressionAtomContext, *antlrgen.ExistsExpressionAtomContext:
			return true
		case *antlrgen.InListContext:
			if n.QueryExpressionBody() != nil {
				return true
			}
		}
		for i := 0; i < t.GetChildCount(); i++ {
			if containsSubquery(t.GetChild(i)) {
				return true
			}
		}
		return false
	}
	for _, el := range upd.AllUpdatedElement() {
		if el != nil && el.Expression() != nil && containsSubquery(el.Expression()) {
			return true
		}
	}
	return false
}

func (g *cascadesGenerator) planDML(ctx context.Context, dml antlrgen.IDmlStatementContext) (plan query.Plan, err error) {
	if err := contextCancellationError(ctx); err != nil {
		return nil, err
	}
	c := g.c

	// Keep DML on the same pre-lowering correctness boundary as SELECT.
	// Aggregate lowering discards OVER, so allowing a windowed aggregate through
	// an EXISTS in DELETE/UPDATE would make the correlated fallback test raw-row
	// existence instead of the query's aggregate/window cardinality. Reject the
	// unsupported construct while its parse-tree distinction is still present.
	// This runs before the explain-only split so every DML planning surface has
	// identical correct-or-loud semantics.
	if windowedAggregateInTree(dml) {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"windowed aggregate (aggregate function with an OVER clause) is not supported")
	}

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

	// DML … OPTIONS (DRY RUN): preview the would-be-affected rows without committing.
	// Java honors it — AstNormalizer.visitQueryOptions → Options.DRY_RUN →
	// ExecuteProperties.setDryRun (QueryPlan.java:435) → the DML plans branch to
	// dryRunSave/DeleteRecordAsync. The flag is STATEMENT-scoped: parsed from the typed
	// OPTIONS clause here, carried on the cascadesPlan → paginatingRows.dryRun →
	// ExecuteProperties.DryRun, where executeInsert/Update/Delete branch onto the store
	// DryRun* primitives. It must NEVER ride a connection option — that would go sticky
	// across pooled statements (the next plain DML would silently no-op). NOCACHE/LOG
	// QUERY remain accepted-and-ignored hints. Detection walks the whole DML subtree so the
	// INSERT…SELECT spelling — whose OPTIONS the grammar attaches to the inner SELECT, not
	// insertStatement.queryOptions — cannot silently bypass DRY RUN and commit.
	dryRun := dmlHasDryRunOption(dml)

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
		// `SET col = DEFAULT` is rejected. The grammar accepts it, but this schema system
		// has no column DEFAULT definitions, and Java doesn't support it either —
		// ExpressionVisitor.visitUpdatedElement (:1089) calls ctx.expression().accept(this),
		// which NPEs when the RHS is DEFAULT (expression() is null). Per the conformance
		// principle (Java NPE → Go emits a CLEAN error, not a crash or silent no-op), reject
		// it: the builder would otherwise silently DROP the assignment (logical_builder.go's
		// `el.Expression()==nil` continue), leaving the column UNCHANGED while reporting
		// success — a misleading silent ignore.
		if updateHasDefaultAssignment(upd) {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"DEFAULT is not supported in UPDATE ... SET")
		}
		if updateHasSubqueryAssignment(upd) {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"subqueries are not supported in UPDATE ... SET")
		}
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

	// DML target-table existence: surface a clean 42F01 (matching INSERT INTO <missing>
	// and the SELECT path), not a downstream generic 0AF00 "DML Cascades translation
	// failed". Run AFTER resolveQualifiedTableNames so (a) a BAD schema qualifier's 42F00
	// already errored above and takes precedence, and (b) a VALID qualifier (or none) has
	// been stripped to the bare Target, which is checked here — so `DELETE FROM
	// <session_schema>.missing` and `DELETE FROM missing` both get 42F01, while
	// `DELETE FROM badschema.missing` keeps its 42F00. recordTypeCI
	// resolves case-insensitively, matching the WHERE/SELECT analyzer.
	var dmlTarget string
	switch dop := logicalOp.(type) {
	case *logical.LogicalDelete:
		dmlTarget = dop.Target
	case *logical.LogicalUpdate:
		dmlTarget = dop.Target
	}
	if dmlTarget != "" && recordTypeCI(md, bareTableName(dmlTarget)) == nil {
		return nil, api.NewErrorf(api.ErrCodeUndefinedTable, "Unknown table %s", strings.ToUpper(bareTableName(dmlTarget)))
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
	if err := rejectDuplicateUnnestAlias(logicalOp, g.c.cachedMetaData()); err != nil {
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
		arr, vErr := c.buildInsertValuesArray(insStmt, rt.Descriptor, insOp.Table, md)
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
					return nil, api.NewError(api.ErrCodeUnsupportedQuery, "Unsupported operator "+fn)
				}
			}
		}
		if err := validateUpdateAssignments(updOp, md); err != nil {
			return nil, err
		}
	}

	if fn := query.FindUnsupportedFunction(logicalOp); fn != "" {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"Unsupported operator "+fn)
	}

	// Pass md so DML join legs (e.g. UPDATE … FROM a JOIN b) anchor (RFC-077 7.6).
	ref, dmlScalarSubqueryPlans := query.TranslateToCascadesWithSubqueries(logicalOp, md)
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

	bestExpr, _, planErr := planner.PlanWithContext(ctx, ref)
	if planErr != nil {
		return nil, translatePlannerError(planErr, plannerUnableToPlanMessage)
	}
	if bestExpr == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"DML Cascades planning returned no expression")
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
	// RFC-164 WS-2: structural plan invariants on the production DML path.
	if err := cascades.ValidatePlanInvariants(physPlan); err != nil {
		return nil, api.NewError(api.ErrCodeInternalError, "malformed DML plan: "+err.Error())
	}

	// Plan the DML statement's scalar subqueries (`DELETE … WHERE v > (SELECT
	// …)`) through the same shared pipeline as SELECT and carry them on the
	// plan so fetchPage pre-binds their results. This path historically
	// DISCARDED them (`ref, _ :=`), and the unbound value silently evaluated
	// NULL — the DELETE compared v > NULL (UNKNOWN) and removed NOTHING, with
	// both differential models identically wrong; the loud
	// values.UnboundScalarSubqueryError is what surfaced it.
	dmlScalarSubs, dmlSubErr := planScalarSubqueryPlans(ctx, dmlScalarSubqueryPlans, md, dmlStats)
	if dmlSubErr != nil {
		return nil, dmlSubErr
	}

	ls.setPlan(physPlan)
	ls.setCache(PlanCacheSkip)
	return &cascadesPlan{
		conn:             g.c,
		md:               md,
		physicalPlan:     physPlan,
		explain:          logicalOp.Explain(""),
		scalarSubqueries: dmlScalarSubs,
		dryRun:           dryRun,
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

// cascadesPlan wraps a Cascades-planned SELECT query with a pre-computed
// physical plan. Planning happens at Plan-time; Execute only runs the plan.
type cascadesPlan struct {
	conn             *EmbeddedConnection
	md               *recordlayer.RecordMetaData
	physicalPlan     plans.RecordQueryPlan
	explain          string
	scalarSubqueries []PlannedScalarSubquery

	// dryRun carries the SQL OPTIONS (DRY RUN) flag from planDML to execution.
	// Statement-scoped (one cascadesPlan per statement) → paginatingRows.dryRun
	// → ExecuteProperties.DryRun, so the DML executor previews via the store
	// DryRun* primitives instead of mutating. Never a connection option.
	dryRun bool
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
//     PER-REQUEST, not per-logical-statement: one Execute() is
//     bounded. A continuation resumed by a NEW request (a fresh Execute on a
//     new plan) starts a fresh deadline — there is no cross-continuation
//     wall-clock, matching Java's per-ExecuteState TimeScanLimiter (reset on
//     every resume). The per-tx FDB timeout is unaffected.
func (p *cascadesPlan) Execute(ctx context.Context) (query.Result, error) {
	c := p.conn
	// Go SQL statement tokens are ENGINE-PRIVATE (no ContinuationProto
	// envelope, no version/plan/binding hashes, no resume entry point) —
	// paging is internal to one statement execution. Until a real resume
	// surface exists, a caller-supplied CONTINUATION option must reject
	// LOUDLY: consuming it silently would re-run the statement from row 1
	// while the caller believes they resumed (duplicate rows), and a
	// JAVA-minted token could never be honored here anyway (its envelope
	// binds to Java's plan serialization hashes).
	if c.Options().Get(api.OptContinuation) != nil {
		return query.Result{}, api.NewError(api.ErrCodeUnsupportedOperation,
			"statement continuations are not supported: Go SQL tokens are engine-private and no resume entry point exists")
	}
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
		dryRun:           p.dryRun,
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
	scalarSubqueries []PlannedScalarSubquery
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

	// dryRun is the statement-scoped SQL OPTIONS (DRY RUN) flag, propagated from
	// the cascadesPlan at construction and read in executeProps() into
	// ExecuteProperties.DryRun. A fresh paginatingRows per statement means it can
	// never leak to a subsequent plain DML on the same (pooled) connection.
	dryRun bool

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

	// DRY RUN is statement-scoped (carried on paginatingRows from the
	// cascadesPlan), NOT a connection option — read the field, never
	// r.conn.Options(), so it can't leak to a later plain statement.
	props = props.WithDryRun(r.dryRun)

	opts := r.conn.Options()

	// Per-page time limit. The connection option (if set) is intersected
	// with the per-transaction CAP (txPageTimeLimit, 4s) so the FDB 5s hard
	// wall is never exceeded: the 4s cap is the ceiling and a smaller user
	// limit only narrows it — a larger user value can never raise the page
	// budget past the cap.
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

// materializeDriverValue converts the neutral in-engine representation of a
// UUID (a [16]byte, or a tuple.UUID read straight off a covering index) into
// the canonical 36-char string the SQL client expects. This is the ONE place a
// UUID leaves the value layer as a string — every internal path (filter
// compare, index-scan-range pack, INL join key) keeps it as [16]byte so
// equality/ordering stay wire-consistent with the tuple.UUID index encoding
// (RFC-162, reviewer decision (b)). A fixed [16]byte / tuple.UUID at this boundary
// is unambiguously a UUID: BYTES columns surface as a []byte slice, never a
// 16-array, so the type switch never misfires.
func materializeDriverValue(v any) any {
	switch u := v.(type) {
	case [16]byte:
		return tuple.UUID(u).String()
	case tuple.UUID:
		return u.String()
	default:
		return v
	}
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
				result, ssqErr := executor.EvaluateScalarSubquery(r.ctx, ssq.Plan, store, evalCtx, props)
				if ssqErr != nil {
					// Route the subquery error through the same translation as the
					// outer plan so a subquery scan-limit/deadline hit surfaces as
					// 54F01, not a raw *ScanLimitReachedError (RFC-106a).
					return nil, translateExecErrorCtx(r.ctx, ssqErr)
				}
				scalarResults[ssq.Alias] = result
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
				row[i] = materializeDriverValue(v)
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
		// LIVENESS tripwire: a page that produced ZERO rows and did not
		// advance its continuation would repeat forever — the per-page
		// resume cost exceeded the page's own resource budget (e.g. a
		// recursive DFS whose re-descent depth outweighs a tiny
		// scanned-rows limit; the checkpoint stores pre-yield positions,
		// so such a page cannot make progress). Correct-or-loud: surface
		// the stall as the resource-limit error it is, never an infinite
		// internal retry loop.
		if len(r.buf) == 0 && !exhausted && contBytes != nil && bytes.Equal(contBytes, r.continuation) {
			return nil, api.NewError(api.ErrCodeExecutionLimitReached,
				"query cannot progress under the configured per-page resource limits (a page produced no rows and no continuation advance); raise the scan/row limits")
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
	// A cursor shape with no continuation support yet (RFC-180 WS-A
	// follow-ups) declines resume typed — 0A000, not an internal error.
	var unsupCont *executor.UnsupportedContinuationError
	if errors.As(err, &unsupCont) {
		return api.NewError(api.ErrCodeUnsupportedOperation, unsupCont.Error())
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
	md             *recordlayer.RecordMetaData
	candidatesOnce sync.Once
	candidates     []cascades.MatchCandidate
}

func (c *metadataPlanContext) GetPlannerConfiguration() cascades.PlannerConfiguration {
	return cascades.DefaultPlannerConfiguration()
}

// GetMatchCandidates returns stable candidate identities for the lifetime of
// the plan context. Partial matches are keyed by MatchCandidate identity on
// memo References; rebuilding pointer-backed candidates on every call makes a
// leaf match invisible to the parent MatchIntermediateRule. That used to be
// masked by direct index rules, but Java-shaped fanout matching necessarily
// climbs several candidate graph levels and therefore requires one identity.
func (c *metadataPlanContext) GetMatchCandidates() []cascades.MatchCandidate {
	c.candidatesOnce.Do(func() {
		c.candidates = c.buildMatchCandidates()
	})
	return append([]cascades.MatchCandidate(nil), c.candidates...)
}

func (c *metadataPlanContext) buildMatchCandidates() []cascades.MatchCandidate {
	if c.md == nil {
		return nil
	}

	var candidates []cascades.MatchCandidate

	// Register PrimaryScanMatchCandidates for each record type's PK.
	// Mirrors Java's RecordStoreScope which creates a PrimaryScanMatchCandidate
	// from the common primary key.
	// Deterministic (name-sorted) iteration: RecordTypes() is a Go map; ranging it
	// directly left both the availableRecordTypes list handed to every primary-scan
	// candidate AND the candidate order dependent on Go's randomised map order — the
	// same nondeterminism class as the index map below (RFC-164 NONDETERMINISM / see
	// RFC-167). Moot for single-table queries; this hardens the multi-table path.
	allTypes := c.md.RecordTypes()
	allTypeNames := make([]string, 0, len(allTypes))
	for name := range allTypes {
		allTypeNames = append(allTypeNames, name)
	}
	sort.Strings(allTypeNames)
	for _, name := range allTypeNames {
		rt := allTypes[name]
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
		// Flow the descriptor-shaped positional type, like the index
		// candidates: a layout-less leg disqualifies itself from plans
		// that bind comparison keys at plan time (the pk-merge
		// intersection), and the primary scan serves exactly one type.
		flowed := values.Type(values.UnknownType)
		if rt.Descriptor != nil {
			flowed = executor.PositionalTypeForDescriptor(rt.Descriptor)
		}
		candidates = append(candidates, cascades.NewPrimaryScanMatchCandidate(
			nil,
			aliases,
			allTypeNames,
			[]string{rt.Name},
			upperPK,
			flowed,
		))
	}

	// Register secondary index candidates. Iterate in a deterministic
	// (name-sorted) order: GetAllIndexes returns a Go map, and ranging it directly
	// made the match-candidate order — and thus equal-cost tie resolution — depend
	// on Go's randomised map iteration, producing 2-3 distinct plans for one query
	// (RFC-164 NONDETERMINISM / see RFC-167). Java keeps indexes in a stable order; sorting by
	// index name restores that. (The partialMatchMap insertion-order fix in
	// reference.go is downstream of this; both are needed.)
	allIndexes := c.md.GetAllIndexes()
	indexNames := make([]string, 0, len(allIndexes))
	for name := range allIndexes {
		indexNames = append(indexNames, name)
	}
	sort.Strings(indexNames)
	defs := make([]cascades.IndexDef, 0, len(allIndexes))
	for _, name := range indexNames {
		idx := allIndexes[name]
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
		// Atomic-mutation / aggregate-only index types (COUNT/SUM totals,
		// MAX_EVER/MIN_EVER running extrema, BITMAP_VALUE bitsets) must not become
		// VALUE-index scan candidates: their entries are aggregated/running values,
		// not per-record values, so a plain ordered scan (e.g. StreamingAgg over the
		// index) reads stale data. In Java these types have no value-scan candidate
		// (AtomicMutationIndexMaintainerFactory / BitmapValueIndexMaintainerFactory
		// never call expandValueIndexMatchCandidate). The subset with a legitimate
		// aggregate use — COUNT/SUM and permuted MIN/MAX — was already claimed as an
		// aggregate candidate by tryAggregateIndexCandidate above and never reaches
		// here; the running-extremum (_EVER) and bitmap types get no candidate at
		// all. Either way, dropping them here leaves a plain MAX/MIN over only such
		// an index to fall back to a base-record StreamingAgg, which computes the
		// correct current extremum.
		if idx.IsAtomicMutationIndex() {
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

func (d *metadataIndexDef) IndexName() string { return d.idx.Name }

// IndexColumnNames returns one name per physical key column. A nesting parent
// is a path segment, not a tuple component: recordlayer.KeyExpression.FieldNames
// intentionally includes that parent for metadata introspection, while
// Cascades' sargable alias list must stay parallel to ColumnSize. Walking the
// proto topology preserves Then order and drops nesting parents.
func (d *metadataIndexDef) IndexColumnNames() []string {
	if root := d.idx.RootExpression.ToKeyExpression(); root != nil {
		if names, ok := indexKeyColumnNames(root); ok &&
			len(names) == d.idx.RootExpression.ColumnSize() {
			return names
		}
	}
	return d.idx.RootExpression.FieldNames()
}

func (d *metadataIndexDef) IndexIsUnique() bool { return d.idx.IsUnique() }

// IndexCreatesDuplicates reports whether the index's root key expression fans
// out (Java index.getRootExpression().createsDuplicates()) — satisfies
// cascades.IndexDefWithCreatesDuplicates so the DistinctRecordsProperty is
// correct for fan-out indexes (RFC-188 M4).
func (d *metadataIndexDef) IndexCreatesDuplicates() bool { return d.idx.CreatesDuplicates() }

// IndexRootKeyExpression exposes a caller-owned KeyExpression AST to the
// Cascades adapter. The candidate clones it again on attachment, so neither
// metadata nor a caller can mutate an already-built match candidate.
func (d *metadataIndexDef) IndexRootKeyExpression() *gen.KeyExpression {
	root := d.idx.RootExpression.ToKeyExpression()
	if root == nil {
		return nil
	}
	return proto.Clone(root).(*gen.KeyExpression)
}

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
		n := e.ColumnSize()
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
		// A nesting parent contributes a path segment but no tuple column,
		// so ColumnSize — not FieldNames length — is the parallel-list
		// authority.
		columnSize := expr.ColumnSize()
		if columnSize == 0 {
			return []string{""}
		}
		return make([]string, columnSize)
	}
}

func indexKeyColumnNames(expression *gen.KeyExpression) ([]string, bool) {
	if expression == nil {
		return nil, false
	}
	switch {
	case expression.Field != nil:
		if expression.Field.FieldName == nil {
			return nil, false
		}
		return []string{expression.Field.GetFieldName()}, true
	case expression.Then != nil:
		var names []string
		for _, child := range expression.Then.GetChild() {
			childNames, ok := indexKeyColumnNames(child)
			if !ok {
				return nil, false
			}
			names = append(names, childNames...)
		}
		return names, true
	case expression.Nesting != nil:
		return indexKeyColumnNames(expression.Nesting.GetChild())
	case expression.Function != nil:
		return indexKeyColumnNames(expression.Function.GetArguments())
	case expression.Grouping != nil:
		return indexKeyColumnNames(expression.Grouping.GetWholeKey())
	case expression.KeyWithValue != nil:
		return indexKeyColumnNames(expression.KeyWithValue.GetInnerKey())
	default:
		return nil, false
	}
}

// IndexRowType flows the descriptor-shaped positional type for
// single-record-type indexes — the SAME layout the runtime rows carry
// (executor.PositionalTypeForDescriptor is the single authority), so
// plan-time ordinal baking (the intersection's comparison keys) matches
// the runtime slots by construction. Multi-type indexes flow Unknown:
// their rows have no single layout.
func (d *metadataIndexDef) IndexRowType() values.Type {
	rts := d.md.RecordTypesForIndex(d.idx)
	if len(rts) != 1 || rts[0].Descriptor == nil {
		return values.UnknownType
	}
	return executor.PositionalTypeForDescriptor(rts[0].Descriptor)
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
	// The PK ordering suffix is claimable ONLY when every record type the
	// index covers has the IDENTICAL primary-key column list: a multi-type /
	// universal index interleaves entries whose PK suffixes differ per type,
	// so no single suffix orders the stream (claiming the first map-iterated
	// type's PK was both wrong for the other types — a shared index on
	// `status` would elide `ORDER BY a` on a type whose entries are ordered
	// by `b` — and NONDETERMINISTIC, since RecordTypesForIndex iterates a
	// map). All-equal is order-independent; anything else returns nil and the
	// sort is simply kept (conservative). Java scopes the expansion per
	// candidate record type (ValueIndexExpansionVisitor), which the
	// per-queried-type refinement would mirror — tracked follow-up.
	first := rts[0].PrimaryKey
	if first == nil {
		return nil
	}
	pkCols := first.FieldNames()
	for _, rt := range rts[1:] {
		if rt.PrimaryKey == nil {
			return nil
		}
		other := rt.PrimaryKey.FieldNames()
		if len(other) != len(pkCols) {
			return nil
		}
		for i := range other {
			if !strings.EqualFold(other[i], pkCols[i]) {
				return nil
			}
		}
	}
	return pkCols
}

// IndexCommonPrimaryKeyValues returns the index's common primary key translated
// to structure-encoding Values (RFC-189 B3) — for PrimaryKeyProperty. Only
// non-nil when EVERY record type the index covers has a STRUCTURALLY IDENTICAL
// primary key (translated equal); a multi-type index whose types' PKs differ, or
// any un-translatable PK (fan-out/version/function), yields nil so the property
// abstains (no DistinctUnion dedup). The translation encodes the record-type-key
// prefix, so two legs over different record types with the same PK STRUCTURE
// dedup on a key that includes the type discriminator — never dropping rows.
func (d *metadataIndexDef) IndexCommonPrimaryKeyValues() []values.Value {
	rts := d.md.RecordTypesForIndex(d.idx)
	if len(rts) == 0 || rts[0].PrimaryKey == nil {
		return nil
	}
	common := recordlayer.TranslatePrimaryKeyToValues(rts[0].PrimaryKey, strings.ToUpper)
	if common == nil {
		return nil
	}
	for _, rt := range rts[1:] {
		if rt.PrimaryKey == nil {
			return nil
		}
		other := recordlayer.TranslatePrimaryKeyToValues(rt.PrimaryKey, strings.ToUpper)
		if !commonPKValuesStructurallyEqual(common, other) {
			return nil
		}
	}
	return common
}

func commonPKValuesStructurallyEqual(a, b []values.Value) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !values.ValuesStructurallyEqual(a[i], b[i]) {
			return false
		}
	}
	return true
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
	case recordlayer.IndexTypePermutedMax:
		// Plain SQL MAX(col) resolves to a PERMUTED_MAX index (Java's
		// NumericAggregationValue.Max.getIndexTypeName()), which tracks the true
		// current maximum under deletes/updates. The monotone MAX_EVER/MIN_EVER
		// index types are intentionally NOT matched here: a plain MAX/MIN query
		// served from a monotone _EVER index would return stale extrema. The
		// separate max_ever()/min_ever() aggregate would match those — but Go's
		// read side does not expose them as query aggregates (only MAX/MIN).
		aggFunc = expressions.AggMax
	case recordlayer.IndexTypePermutedMin:
		aggFunc = expressions.AggMin
	default:
		return nil
	}

	gke, ok := idx.RootExpression.(*recordlayer.GroupingKeyExpression)
	if !ok {
		return nil
	}

	// A permuted index with permutedSize > 0 stores its BY_GROUP keys as
	// [prefix-groups, extremum, permuted-suffix-groups] — NOT logical group
	// order. The SQL aggregate candidate models neither of the two consequences:
	// a group-column scan range built in logical order would bind the extremum
	// slot (missing rows), and the advertised groupCols ordering hint is false
	// for the physical stream (ORDER BY elimination / multi-aggregate
	// intersection would mis-merge). Go's own DDL always writes permutedSize=0
	// (Java MaterializedViewIndexGenerator with no aggregate ORDER BY), so a
	// nonzero permutation only arrives via record-layer API / Java-written
	// shared-cluster metadata — decline candidacy for it and let the query fall
	// back to a base-record StreamingAgg (correct rows, slower). The record-layer
	// aggregate-function API path (evaluatePermutedMinMaxAggregate) serves
	// permuted reads with proper prefix-trimming and stays available.
	// Permutation-aware SQL candidacy (bounds translation + true ordering model,
	// as Java's planner does) is the tracked follow-up.
	if idx.Type == recordlayer.IndexTypePermutedMax || idx.Type == recordlayer.IndexTypePermutedMin {
		if v, ok := idx.Options[recordlayer.IndexOptionPermutedSize]; ok {
			if n, err := strconv.Atoi(v); err != nil || n != 0 {
				return nil
			}
		}
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
	// A recursive CTE consumed bare (`SELECT * FROM cte`) has the recursive
	// plan at top — no projection above it. Its output schema is the SEED
	// leg's: standard SQL defines the CTE's columns from the seed (plus any
	// column-alias list, which the translator bakes into the seed leg's
	// normalization projection), and the recursive leg is normalized onto the
	// same names. Recurse into the seed (through its TempTableInsert wrapper)
	// exactly like the plain-UNION arm recurses into its first leg — without
	// this arm the walk fell through to the leaf handler, found no scan, and
	// returned NO columns: rows flowed but Rows.Columns() was empty, so every
	// database/sql Scan failed with "expected 0 destination arguments".
	if rdj, ok := plan.(*plans.RecordQueryRecursiveDfsJoinPlan); ok {
		return deriveColumnsFromPlan(rdj.GetRoot(), md)
	}
	if rlu, ok := plan.(*plans.RecordQueryRecursiveLevelUnionPlan); ok {
		return deriveColumnsFromPlan(rlu.GetInitialState(), md)
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
			Name: strings.ToUpper(string(fd.Name())),
			// Route through protoFieldTypeName (descriptor-aware) so a bare
			// SELECT * reports a UUID column as "OTHER" — the same JDBC name the
			// projection path (valueTypeName) and Java (Types.OTHER) use — rather
			// than the "UNKNOWN" protoKindToTypeName's MessageKind default gives.
			TypeName: protoFieldTypeName(rt.Descriptor, string(fd.Name())),
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

// allLeafDescriptors collects the record-type descriptors of EVERY scan /
// index leaf reachable from p — both sides of a join. A single-leaf lookup
// (following only the GetInner() chain) misses the other join leg, which left
// a projected column from that leg (e.g. `o.total` in
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
		// A grouping slot flows a plain column read (FieldValue); every
		// other slot is an aggregate output. Classify by the RESOLVED
		// Value, never by "(" in the rendered name (RFC-180 F-2): a
		// delimited column literally named "SUM(X)" would misclassify.
		// Grouping columns resolve their type against the record type;
		// aggregates default to BIGINT.
		typeName := "BIGINT"
		if fv, isCol := f.Value.(*values.FieldValue); isCol && desc != nil {
			if t := protoFieldTypeName(desc, strings.ToUpper(fv.Field)); t != "UNKNOWN" {
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

// legPlanFor resolves a QOV correlation alias to the JOIN LEG PLAN it
// addresses, walking nested NLJ/FlatMap shapes through order-preserving
// wrappers. Returns the leg's subplan and whether that leg is
// null-supplying (its subtree carries a DefaultOnEmpty — the LEFT-JOIN
// null-extension; coarse in the safe direction, like the descriptor-level
// nullBorn detection). found=false when the alias doesn't name a leg of
// this plan — callers fall back to the name-keyed lookups.
func legPlanFor(p plans.RecordQueryPlan, alias string) (leg plans.RecordQueryPlan, nullSupplying bool, found bool) {
	// UNIQUE-match-or-decline: the plan-level walk is not query-scope-aware
	// — a FOLDED query block (a projected-EXISTS CTE lowered to a FlatMap)
	// has no RecordQueryProjectionPlan for the opaque-boundary guard to
	// stop at, so an interior block reusing a top-block alias would
	// shadow-match and attach the WRONG branch's null extension. Every
	// candidate is therefore collected; only an unambiguous single match is
	// used, and a duplicated alias declines to the name-keyed fallbacks
	// (which degrade toward nullable — the safe direction — never toward a
	// foreign branch's metadata). Exact scoping needs resolver-carried leg
	// provenance (the RFC-142 model), not plan-side alias search.
	matches := collectLegMatches(p, alias, false, nil)
	if len(matches) != 1 {
		return nil, false, false
	}
	return matches[0].leg, matches[0].nullSupplying, true
}

type legMatch struct {
	leg           plans.RecordQueryPlan
	nullSupplying bool
}

func collectLegMatches(p plans.RecordQueryPlan, alias string, acc bool, out []legMatch) []legMatch {
	for p != nil {
		switch n := p.(type) {
		case *plans.RecordQueryNestedLoopJoinPlan:
			jt := n.GetJoinType()
			outerNS := acc || jt == plans.JoinFullOuter
			innerNS := acc || jt == plans.JoinLeftOuter || jt == plans.JoinFullOuter
			// A matched leg's SUBTREE is still searched: shallow-wins scope
			// shadowing would be sound only if plan nesting faithfully
			// mirrored SQL scoping, and FOLDED query blocks are exactly
			// where that mirror breaks — an interior duplicate therefore
			// counts as a second match and the caller declines to the name
			// fallbacks rather than trusting either binding.
			if strings.EqualFold(n.GetOuterAlias(), alias) {
				out = append(out, legMatch{n.GetOuter(), outerNS || legHasDefaultOnEmpty(n.GetOuter())})
			}
			out = collectLegMatches(n.GetOuter(), alias, outerNS, out)
			if strings.EqualFold(n.GetInnerAlias(), alias) {
				out = append(out, legMatch{n.GetInner(), innerNS || legHasDefaultOnEmpty(n.GetInner())})
			}
			out = collectLegMatches(n.GetInner(), alias, innerNS, out)
			return out
		case *plans.RecordQueryFlatMapPlan:
			if strings.EqualFold(n.GetOuterAlias().Name(), alias) {
				out = append(out, legMatch{n.GetOuter(), acc || legHasDefaultOnEmpty(n.GetOuter())})
			}
			out = collectLegMatches(n.GetOuter(), alias, acc, out)
			if strings.EqualFold(n.GetInnerAlias().Name(), alias) {
				out = append(out, legMatch{n.GetInner(), acc || legHasDefaultOnEmpty(n.GetInner())})
			}
			out = collectLegMatches(n.GetInner(), alias, acc, out)
			return out
		case *plans.RecordQueryDefaultOnEmptyPlan:
			acc = true
			p = n.GetInner()
		case *plans.RecordQueryProjectionPlan:
			// Query-block boundary — aliases below belong to another scope.
			return out
		default:
			ip, ok := p.(innerPlan)
			if !ok {
				return out
			}
			p = ip.GetInner()
		}
	}
	return out
}

func legHasDefaultOnEmpty(p plans.RecordQueryPlan) bool {
	has := false
	plans.Walk(p, func(n plans.RecordQueryPlan) bool {
		if _, ok := n.(*plans.RecordQueryDefaultOnEmptyPlan); ok {
			has = true
			return false
		}
		return true
	})
	return has
}

func deriveColumnsFromProjection(proj *plans.RecordQueryProjectionPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	// A projection over a join references columns from MULTIPLE record types,
	// so resolve each column's type against every join leaf, not just the
	// first one (the single-leaf lookup left the other leg's columns UNKNOWN).
	descs := allLeafDescriptors(proj.GetInner(), md)
	aliases := proj.GetAliases()
	projections := proj.GetProjections()

	// Leaf descriptors under a NULL-SUPPLYING (DefaultOnEmpty) subtree: a
	// column defined by one of these serves NULL on the outer join's padded
	// rows, so its metadata reports NULLABLE regardless of the proto's
	// Required (Java gets this from
	// the flowed result type; the name-model lazy projections here flow
	// none). Keyed by descriptor FullName. Coarse by design in the safe
	// direction: a self-joined table on both sides marks the preserved read
	// nullable too — clients then handle a NULL that never comes, never the
	// reverse.
	nullBorn := map[protoreflect.FullName]struct{}{}
	plans.Walk(proj.GetInner(), func(n plans.RecordQueryPlan) bool {
		if doe, ok := n.(*plans.RecordQueryDefaultOnEmptyPlan); ok {
			for _, d := range allLeafDescriptors(doe, md) {
				nullBorn[d.FullName()] = struct{}{}
			}
		}
		return true
	})

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
	var innerCols []executor.ColumnDef
	var innerDerived bool
	var innerByName map[string]executor.ColumnDef
	deriveInner := func() {
		if innerDerived {
			return
		}
		innerDerived = true
		innerCols = deriveColumnsFromPlan(proj.GetInner(), md)
		innerByName = make(map[string]executor.ColumnDef, len(innerCols))
		for _, ic := range innerCols {
			innerByName[strings.ToUpper(ic.Name)] = ic
		}
	}
	cols := make([]executor.ColumnDef, len(projections))
	for i, v := range projections {
		alias := ""
		if i < len(aliases) {
			alias = aliases[i]
		}
		cd := deriveProjectionColumnDef(v, alias, i, descs)
		if cd.Nullable == api.ColumnNoNulls {
			// The projected reference is either a FLAT (childless) read — its
			// Field may carry the "LEG.COL" qualifier — or the resolver's
			// QUANTIFIER-ADDRESSED bake (FieldValue{Child: QOV(leg), COL});
			// compose the qualified lookup for the latter so the null-born
			// upgrade fires for both emissions (the QOV form skipped it and
			// reported a null-supplying window's column NoNulls).
			lookup := ""
			if fv, ok := v.(*values.FieldValue); ok {
				switch child := fv.Child.(type) {
				case nil:
					lookup = fv.Field
				case *values.QuantifiedObjectValue:
					lookup = child.Correlation.Name() + "." + fv.Field
				}
			}
			if lookup != "" {
				if d := descriptorForColumn(lookup, descs); d != nil {
					if _, born := nullBorn[d.FullName()]; born {
						cd.Nullable = api.ColumnNullable
					}
				}
			}
		}
		if cd.TypeName == "" || cd.TypeName == "UNKNOWN" {
			// Inherit from the inner column the projection reads (matched by the
			// projected FieldValue's field = the inner output key). BOTH read
			// emissions inherit: the FLAT (childless) form AND the resolver's
			// QUANTIFIER-ADDRESSED bake (FieldValue{Child: QOV(inner)}) — a
			// projection has exactly one input, so the QOV form reads the same
			// inner output the flat form does. The QOV form arrives when the
			// planner KEEPS a projection spine unmerged (e.g. an ordering-pinned
			// spine under an elided sort): the outer read of a derived column
			// like `val * 2 AS doubled` is then a plain rename of the inner
			// output, and refusing to inherit reported UNKNOWN where Java types
			// it from the flowed result type regardless of plan shape.
			fv, isField := v.(*values.FieldValue)
			if isField {
				_, viaQOV := fv.Child.(*values.QuantifiedObjectValue)
				if fv.Child == nil || viaQOV {
					deriveInner()
					// The BAKED ordinal is the structural linkage (RFC-142): a
					// plan-time reference reads the inner output SLOT, and the
					// inner's column NAME for that slot may be a positional
					// label ("_1" for an unaliased computed column) that no
					// name lookup can hit — e.g. an unmerged projection spine
					// where the outer reads `DOUBLED#1` while the inner derives
					// slot 1 as "_1" (correctly typed). Resolve by ordinal
					// first; the name map serves lazy (unbaked) reads.
					inherited := false
					// Ordinal inheritance only for the FLAT read: a
					// QUANTIFIER-ADDRESSED read over a JOIN carries a
					// LEG-relative ordinal, which is not an index into the
					// flattened inner columns — inheriting by it would type
					// the column from an unrelated leg's slot. QOV reads use
					// the name path below.
					if fv.Child == nil && fv.Resolved != nil && len(fv.Resolved.Accessors) == 1 {
						if ord := fv.Resolved.Accessors[0].Ordinal; ord >= 0 && ord < len(innerCols) {
							if ic := innerCols[ord]; ic.TypeName != "" && ic.TypeName != "UNKNOWN" {
								cd.TypeName = ic.TypeName
								// Upgrade-only, like every inherit site: the
								// null-born adjustment ran before inheritance.
								if ic.Nullable == api.ColumnNullable {
									cd.Nullable = api.ColumnNullable
								}
								inherited = true
							}
						}
					}
					if !inherited {
						// A QOV-addressed read over a JOIN resolves against
						// QUALIFIED inner keys ("D.FOO" — deriveColumnsFromJoin
						// keys per-leg columns by their leg alias). When the
						// read also carries a BAKED ordinal, resolve it WITHIN
						// the leg's columns (qualified-prefix slice, leg order
						// preserved by the join derivation): a name lookup
						// alone loses slot identity when a leg duplicates an
						// output name — the map keeps only the last "D.FOO"
						// while the accessor addresses a specific slot. The
						// name lookup serves unbaked QOV reads; the bare key
						// serves single-source shapes.
						inheritFrom := func(ic executor.ColumnDef) {
							cd.TypeName = ic.TypeName
							// Nullability only ever UPGRADES here: the
							// null-born (LEFT-JOIN null-extension) adjustment
							// ran before inheritance, and copying an inner
							// NoNulls back would un-null-extend the column —
							// unmatched outer rows still serve NULL.
							if ic.Nullable == api.ColumnNullable {
								cd.Nullable = api.ColumnNullable
							}
							inherited = true
						}
						if qov, ok := fv.Child.(*values.QuantifiedObjectValue); ok {
							// Resolve the LEG STRUCTURALLY: reconstructing leg
							// membership from qualified-name prefixes both
							// miscounts (an already-qualified output like a
							// quoted "X.Y" identifier stays unprefixed in the
							// merge, shifting every later slot) and loses slot
							// identity under duplicate names. The leg's own
							// derived columns are 1:1 positional with the
							// leg-relative baked ordinal, carry EXACT
							// nullability (a synthesized NOT NULL such as a
							// projected EXISTS stays NoNulls on a CROSS join),
							// and the null-supplying flag applies the LEFT-JOIN
							// null extension only where it exists.
							if legPlan, nullSupplying, found := legPlanFor(proj.GetInner(), qov.Correlation.Name()); found {
								legCols := deriveColumnsFromPlan(legPlan, md)
								if fv.Resolved != nil && len(fv.Resolved.Accessors) == 1 {
									if ord := fv.Resolved.Accessors[0].Ordinal; ord >= 0 && ord < len(legCols) {
										if ic := legCols[ord]; ic.TypeName != "" && ic.TypeName != "UNKNOWN" {
											cd.TypeName = ic.TypeName
											cd.Nullable = ic.Nullable
											if nullSupplying {
												cd.Nullable = api.ColumnNullable
											}
											inherited = true
										}
									}
								}
								if !inherited {
									for _, ic := range legCols {
										if strings.EqualFold(parseColRef(ic.Name).bare(), fv.Field) && ic.TypeName != "" && ic.TypeName != "UNKNOWN" {
											cd.TypeName = ic.TypeName
											cd.Nullable = ic.Nullable
											if nullSupplying {
												cd.Nullable = api.ColumnNullable
											}
											inherited = true
											break
										}
									}
								}
							}
							if !inherited {
								legPrefix := strings.ToUpper(qov.Correlation.Name()) + "."
								if ic, found := innerByName[legPrefix+strings.ToUpper(fv.Field)]; found && ic.TypeName != "" && ic.TypeName != "UNKNOWN" {
									inheritFrom(ic)
								}
							}
						}
						if !inherited {
							if ic, found := innerByName[strings.ToUpper(fv.Field)]; found && ic.TypeName != "" && ic.TypeName != "UNKNOWN" {
								inheritFrom(ic)
							}
						}
					}
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
			name = values.ColumnNameValue(v)
		} else {
			name = fv.Field
		}
	} else {
		name = values.ColumnNameValue(v)
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
	// For a PLAIN column read the stored descriptor is the metadata
	// authority: the flowed seed type conflates INT/BIGINT and FLOAT/DOUBLE
	// (evaluation widths), which must not leak into ResultSet metadata —
	// Java reports the DECLARED column type (INTEGER, FLOAT). The flowed
	// type serves columns the descriptor cannot resolve (derived/CTE
	// outputs) and every non-FieldValue expression.
	typeName := ""
	if _, isField := v.(*values.FieldValue); isField && colDesc != nil {
		// The stored descriptor is the metadata authority for a BASE column read —
		// but ONLY to recover the eval-width the flowed seed conflates (INTEGER↔
		// BIGINT, FLOAT↔DOUBLE). A DERIVED/CTE OUTPUT alias whose name coincidentally
		// matches a stored field flows a genuinely DIFFERENT type (`SELECT q.id FROM
		// (SELECT x AS id FROM B) q`: q.id is x's DOUBLE, but the derived leg carries
		// B's descriptor so descriptorForColumn finds B.ID's BIGINT). Let the
		// descriptor override only when it REFINES the flowed type within its family.
		if t := protoFieldTypeName(colDesc, name); t != "UNKNOWN" && descriptorRefinesFlowed(t, valueTypeName(v, typeDesc)) {
			typeName = t
		}
	}
	if typeName == "" {
		typeName = valueTypeName(v, typeDesc)
	}
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
	} else if fv, isField := v.(*values.FieldValue); isField && fv.Field != "" {
		// A MACHINERY-pinned alias — the duplicated-bare-leaf dedup pins the
		// projected reference's QUALIFIED spelling ("A.NAME" for QOV(A).NAME)
		// as the alias so the two same-named datum keys do not collapse — is
		// an INTERNAL key, not a user label: Java reports the bare column for
		// `SELECT c.name, p.name` (both NAME, JDBC allows duplicate labels).
		// Detect it by a DOTTED label whose leaf equals the projected
		// reference's own leaf (the reference may since have been rebased —
		// a projected-EXISTS fold re-anchors it onto the merged row — so the
		// qualifier cannot be compared, only the leaf). A user alias that is
		// dotted AND leaf-matches the projected column degrades to the bare
		// leaf too — a pathological corner traded for the duplicated-leaf
		// class matching Java's metadata.
		if ref := parseColRef(label); isPlainQualifiedColumnReference(label) && ref.isQualified() &&
			strings.EqualFold(ref.bare(), parseColRef(fv.Field).bare()) {
			displayLabel = strings.ToUpper(ref.bare())
		}
	}
	nullable := api.ColumnNullable
	if colDesc != nil {
		if fd := colDesc.Fields().ByName(protoreflect.Name(parseColRef(name).bare())); fd != nil && fd.Cardinality() == protoreflect.Required {
			nullable = api.ColumnNoNulls
		}
	}
	// The proto descriptor says what the STORED column is; the FLOWED value
	// type says what this query serves — a NOT NULL column read through a
	// LEFT-outer null-supplying window is nullable (the padded rows serve
	// NULL), which Java reports via the plan's result type
	// (FieldValue.computeResultType's nullable override).
	// A KNOWN-typed nullable flowed value
	// therefore upgrades NoNulls — never the reverse; an UNKNOWN type
	// (name-model lazy dotted refs never flow one) says nothing.
	if nullable == api.ColumnNoNulls {
		if fv, isField := v.(*values.FieldValue); isField && fv.Typ != nil &&
			fv.Typ.Code() != values.TypeCodeUnknown && fv.Typ.IsNullable() {
			nullable = api.ColumnNullable
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
			typeRef = strings.ToUpper(values.ColumnNameValue(f.Value))
		} else {
			typeRef = strings.ToUpper(fv.Field)
		}
	}
	return columnDefFromRef(name, label, typeRef, f.Value, descs)
}

// columnDefFromRef is the shared type+nullability derivation for a single
// result-set column: name is the datum-map lookup key, label the user-visible
// display name, typeRef the string descriptorForColumn keys on (the value's
// qualified reference for a fold, the bare column name for an ordinal seed), and
// value the defining Value (its Type() supplies a synthesized column's type and
// nullability). Extracted from foldedColumnDef so the ordinal-unnest arm can
// reuse the identical resolution while keying the descriptor lookup on the
// BARE field name (a baked ofOrdinal renders "T1.ID#0" under ExplainValue — the
// "#0" suffix misses the proto descriptor).
func columnDefFromRef(name, label, typeRef string, value values.Value, descs []protoreflect.MessageDescriptor) executor.ColumnDef {
	colDesc := descriptorForColumn(typeRef, descs)
	typeDesc := colDesc
	if typeDesc == nil && len(descs) > 0 {
		typeDesc = descs[0]
	}
	typeName := valueTypeName(value, typeDesc)
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
	} else if value != nil {
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
		if t := value.Type(); t != nil && !t.IsNullable() {
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

// ordinalUnnestColumnDef derives ONE result-set column of a lateral-unnest
// ORDINAL seed (the WITH-ORDINALITY seed). The seed's OUTER leg columns are
// BAKED ofOrdinal FieldValues whose ExplainValue carries the "#ordinal" suffix
// (e.g. "T1.ID#0"), which misses the proto descriptor and mis-reports a stored
// column's type/nullability (a pk drops from NOT NULL to nullable). Because an
// ordinal seed's field NAMES are exactly the bare column / AS / AT alias
// names, key the descriptor lookup on the bare name — so an outer stored
// column resolves its descriptor (pk NOT NULL) exactly as a name-keyed lookup
// would, while the descriptor-less element/ordinal still types from its own
// Value (element from the array element, ordinal INT NOT NULL). RFC-142.
// columnDefDisplayName is the column's unqualified user-visible name — the
// exact value RecordLayerResultSet.positionalAligned compares each positional
// slot's field name against: the Label (alias) when set, else the bare leaf of
// the qualified datum-key Name ("Q$DUP2.ID" → "ID"). Kept in lockstep with the
// executor's columnDisplayName so deriveColumnsFromJoin's divergence check (does
// the name-model merge render the same output sequence the positional row does?)
// asks exactly the question positionalAligned will answer at serve time.
func columnDefDisplayName(c executor.ColumnDef) string {
	if c.Label != "" {
		return c.Label
	}
	return parseColRef(c.Name).bare()
}

// mergedRVSequenceDiverges reports whether the name-model leg-merge (merged)
// fails to render the ordinal RC's authoritative output sequence: a different
// column count, or any position whose merged DISPLAY name (the value
// positionalAligned compares) differs from the RC field's bare name. It is the
// trigger for `SELECT *` over a duplicate-alias cluster: the planner may
// group same-table dup legs (physical `P ⋈ (P ⋈ Q)`),
// so the structural merge reorders to `[ID V ID V QID]` while the RC — which
// the positional row mirrors — keeps FROM order `[ID V QID ID V]` with duplicate
// bare labels. When the sequences AGREE (every non-reordered case, incl.
// distinct-alias `A.K`/`B.K`), the merge path is authoritative-equivalent and
// kept byte-identical.
func mergedRVSequenceDiverges(rc *values.RecordConstructorValue, merged []executor.ColumnDef) bool {
	if len(rc.Fields) != len(merged) {
		return true
	}
	for i, f := range rc.Fields {
		if !strings.EqualFold(parseColRef(f.Name).bare(), columnDefDisplayName(merged[i])) {
			return true
		}
	}
	return false
}

func ordinalUnnestColumnDef(f values.RecordConstructorField, descs []protoreflect.MessageDescriptor) executor.ColumnDef {
	name := strings.ToUpper(f.Name)
	label := strings.ToUpper(parseColRef(f.Name).bare())
	return columnDefFromRef(name, label, name, f.Value, descs)
}

// valueRootCorrelation reports the correlation a seed field ultimately reads
// from: a bare QuantifiedObjectValue's own correlation (the NO-AT scalar element
// leg), or the root correlation of a baked ofOrdinal FieldValue chain (an outer
// column or the WITH-ORDINALITY element/ordinal). Reports false for any other
// shape. Used to classify an ordinal-unnest seed field as an OUTER column vs an
// element/ordinal (which reference the FlatMap's INNER correlation).
func valueRootCorrelation(v values.Value) (values.CorrelationIdentifier, bool) {
	switch t := v.(type) {
	case *values.QuantifiedObjectValue:
		return t.Correlation, true
	case *values.FieldValue:
		if t.Child != nil {
			return valueRootCorrelation(t.Child)
		}
	}
	return values.CorrelationIdentifier{}, false
}

func deriveColumnsFromAggregation(agg *plans.RecordQueryStreamingAggregationPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	// ALL leaf descriptors, not just the first: a GROUP BY over a JOIN can key
	// on a column from any leg, and its type must resolve against the leg it
	// lives on (Java reads the type off the flowed join-output record). A single
	// first-leaf descriptor served UNKNOWN for a far-leg group key.
	descs := allLeafDescriptors(agg.GetInner(), md)
	return buildAggColumns(agg.GetGroupingKeys(), agg.GetAggregates(), descs)
}

type multiInnerPlan interface {
	GetInners() []plans.RecordQueryPlan
}

func findUnionPlan(p plans.RecordQueryPlan) []plans.RecordQueryPlan {
	for {
		// STOP at a node that defines the output schema itself. The descent
		// exists to reach a top-level set operation wearing unary hats
		// (Fetch/Limit/…), whose legs then supply the columns — but a set
		// operation BELOW a projection or aggregation is an input to it, not
		// the output shape. Descending past one derived the columns from an
		// intersection leg's full record (every field of the table) while the
		// plan emitted the projection's single slot, so every read failed the
		// positional-alignment guard: `SELECT DISTINCT id … WHERE a=? AND b=?
		// LIMIT k` over two indexes planned `Limit(Project([ID], Intersection))`
		// and reported 3 columns for a 1-column row. Returning nil here lets
		// deriveColumnsFromPlan's unary recursion reach the schema-defining
		// node, which has a dedicated arm.
		if definesOutputSchema(p) {
			return nil
		}
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

// definesOutputSchema reports whether a plan node determines the result's
// column list rather than passing its input's through. It lists every type
// deriveColumnsFromPlan handles with a dedicated arm BEFORE it consults
// findUnionPlan — including the two recursive-CTE arms, which derive from
// their seed / initial state. Keeping the two in sync is what makes the
// descent safe; a type with a dedicated arm that is missing here would be
// descended past and lose its schema.
func definesOutputSchema(p plans.RecordQueryPlan) bool {
	switch p.(type) {
	case *plans.RecordQueryProjectionPlan,
		*plans.RecordQueryStreamingAggregationPlan,
		*plans.RecordQueryAggregateIndexPlan,
		*plans.RecordQueryMultiIntersectionOnValuesPlan,
		*plans.RecordQueryNestedLoopJoinPlan,
		*plans.RecordQueryFlatMapPlan,
		*plans.RecordQueryRecursiveDfsJoinPlan,
		*plans.RecordQueryRecursiveLevelUnionPlan:
		return true
	}
	return false
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

	merged := qualifyAndMergeColumns(firstCols, secondCols, firstAlias, secondAlias)

	// The GATHERED multi-source unnest star (`SELECT * FROM A, B, A.arr AS x`)
	// plans as an NLJ whose FlatMap leg is a PARTITION SUB-PRODUCT — a
	// positional-merge RC whose fields are planner-internal `_N` names — so
	// the leg-merge above leaks `_0`/`_1` into the user-visible columns (and
	// misses the element entirely). The translated ordinal TOP RV carries the
	// true SQL-order output names (each f.Name IS the datum/positional key by
	// construction — the same rule the FlatMap fold arm relies on), and the
	// positional-aligned read then serves the VALUES from the positional
	// row's matching slots. Derive from the RV, keyed on bare names against
	// BOTH legs' leaf descriptors (ordinalUnnestColumnDef). Scoped by the
	// STRUCTURAL discriminator — a leg subplan whose RV is the
	// positional-merge RC (the sub-product that folds to `_N` columns) —
	// never by the derived NAMES: a user column literally named `_0` over a
	// plain gated join is a legal identifier and must keep the merge path's
	// qualified metadata byte-identical.
	// AND the GATHERED-UNNEST signature — an Explode-bearing FlatMap leg
	// (gatheredExplodeElement): a PLAIN multi-way join's partition also
	// leaves a positional-merge subplan, but ITS fold keeps qualified
	// duplicate-name keys (deriveColumnsFromJoin handles an NLJ-shaped
	// sub-product), so rerouting it would drop the `A.K`/`B.K` names by-name
	// reads rely on.
	// The second structural trigger: the name-model merge DIVERGES from the
	// ordinal RV's authoritative output sequence. A duplicate-alias
	// `SELECT *` (`SELECT * FROM p, q, p`) lets the planner GROUP the
	// same-table legs (physical `P ⋈ (P ⋈ Q)`), so the structural leg-merge
	// reorders to `[ID V ID V QID]`, while the ordinal TOP RV carries every
	// slot in FROM order with duplicate BARE labels (Java's exact star
	// layout: `[ID V QID ID V]`) — the sequence the positional row mirrors
	// and positionalAligned reads by slot. The binding-keyed qualification
	// makes each dup leg's qualified name DISTINCT (`P.ID` vs `Q$DUP2.ID`),
	// so a same-qualified-name collision check can no longer catch this
	// case; the divergence of the DISPLAY sequences is the faithful signal.
	// Distinct-alias duplicates ("A.K" / "B.K") whose merge is NOT reordered
	// keep the byte-identical merge path (their display sequence equals the
	// RV's), exactly as before.
	rc, isOrdinalRC := nlj.GetResultValue().(*values.RecordConstructorValue)
	mergedDivergesFromRV := isOrdinalRC && mergedRVSequenceDiverges(rc, merged)
	elemAlias, collField, elemValue := gatheredExplodeElement(nlj)
	if isOrdinalRC && len(rc.Fields) > 0 && values.ContainsBakedOrdinal(rc) &&
		((hasPositionalMergeLeg(nlj) && elemAlias != "") || mergedDivergesFromRV) {
		descs := allLeafDescriptors(nlj.GetOuter(), md)
		descs = append(descs, allLeafDescriptors(nlj.GetInner(), md)...)
		cols := make([]executor.ColumnDef, 0, len(rc.Fields))
		for _, f := range rc.Fields {
			col := ordinalUnnestColumnDef(f, descs)
			// The MIXED (no-AT) element rides a single-accessor merge-slot ref
			// whose type the partition collapse erased (the Explode quantifier
			// flows untyped); no descriptor names it either. Its authoritative
			// type is the Explode's own collection element (the AS+AT form's
			// refs stay typed through fusion and never reach this). A STRUCT
			// element's values.Type is ALSO unknown (7.6 does not model
			// message element types), so fall through to the array column's
			// own proto descriptor — the ground truth: a repeated field's
			// Kind IS its element kind, with non-UUID messages reporting
			// STRUCT (java.sql.Types.STRUCT), never the BIGINT fallback that
			// silently mistyped struct elements (review finding, pinned).
			if strings.EqualFold(f.Name, elemAlias) && unknownTypedValue(f.Value) {
				tn := ""
				if elemValue != nil {
					tn = valueTypeName(elemValue, nil)
				}
				if tn == "" || tn == "UNKNOWN" {
					tn = arrayElementTypeNameFromDescs(collField, descs)
				}
				if tn != "" && tn != "UNKNOWN" {
					col.TypeName = tn
				}
			}
			cols = append(cols, col)
		}
		return cols
	}
	return merged
}

// hasPositionalMergeLeg reports whether a leg subplan (transitively, through
// inner-plan wrappers and nested join plans) carries the POSITIONAL-MERGE
// RC as its result value — the partition sub-product whose column fold
// renders planner-internal `_N` names. The STRUCTURAL twin of the retired
// name-based check: keying on derived names misfired on a user column
// literally named `_0` (a legal identifier), rerouting a today-working
// join's metadata off the qualified merge path.
func hasPositionalMergeLeg(p plans.RecordQueryPlan) bool {
	var legHas func(plans.RecordQueryPlan) bool
	legHas = func(leg plans.RecordQueryPlan) bool {
		switch tp := leg.(type) {
		case *plans.RecordQueryFlatMapPlan:
			if rc, isRC := tp.GetResultValue().(*values.RecordConstructorValue); isRC && values.IsPositionalMergeRC(rc) {
				return true
			}
			return legHas(tp.GetOuter()) || legHas(tp.GetInner())
		case *plans.RecordQueryNestedLoopJoinPlan:
			if rc, isRC := tp.GetResultValue().(*values.RecordConstructorValue); isRC && values.IsPositionalMergeRC(rc) {
				return true
			}
			return legHas(tp.GetOuter()) || legHas(tp.GetInner())
		}
		if ip, isIP := leg.(innerPlan); isIP {
			return legHas(ip.GetInner())
		}
		return false
	}
	nlj, isNLJ := p.(*plans.RecordQueryNestedLoopJoinPlan)
	if !isNLJ {
		return false
	}
	return legHas(nlj.GetOuter()) || legHas(nlj.GetInner())
}

// unknownTypedValue reports whether a value's own type is absent/unknown —
// the shape whose column type needs an out-of-band source.
func unknownTypedValue(v values.Value) bool {
	t := v.Type()
	return t == nil || t.Code() == values.TypeCodeUnknown
}

// gatheredExplodeElement finds the gathered unnest's Explode leg under an NLJ
// (the FlatMap pairing the owning source with its Explode — possibly nested
// under further NLJ levels for a wider cluster) and returns the element's
// binding alias (the FlatMap's inner correlation, the AS alias), the ARRAY
// COLUMN's bare field name (the baked collection reference's display field —
// the descriptor key for element-kind resolution), and a value typed as the
// Explode's collection ELEMENT when the plan-level type survived (a STRUCT
// element's values.Type is Unknown — 7.6 does not model message element
// types — so the caller falls through to the descriptor). ("", "", nil) when
// no such leg exists.
func gatheredExplodeElement(p plans.RecordQueryPlan) (string, string, values.Value) {
	if fm, ok := p.(*plans.RecordQueryFlatMapPlan); ok {
		if exp := findExplodePlan(fm.GetInner()); exp != nil {
			collField := ""
			if fv, isFV := exp.GetCollectionValue().(*values.FieldValue); isFV {
				collField = fv.Field
			}
			collType := exp.GetCollectionValue().Type()
			if arr, isArr := collType.(*values.ArrayType); isArr && arr.ElementType != nil {
				return fm.GetInnerAlias().Name(), collField, values.NewQuantifiedObjectValueOfType(
					fm.GetInnerAlias(), arr.ElementType,
				)
			}
			return fm.GetInnerAlias().Name(), collField, nil
		}
	}
	if nlj, ok := p.(*plans.RecordQueryNestedLoopJoinPlan); ok {
		if a, cf, v := gatheredExplodeElement(nlj.GetOuter()); a != "" {
			return a, cf, v
		}
		return gatheredExplodeElement(nlj.GetInner())
	}
	if ip, ok := p.(innerPlan); ok {
		return gatheredExplodeElement(ip.GetInner())
	}
	return "", "", nil
}

// arrayElementTypeNameFromDescs resolves an array column's ELEMENT type name
// from its proto descriptor — the ground truth when the plan-level element
// type was erased (message elements). A repeated field's Kind IS its element
// kind; a non-UUID message element is a STRUCT column (java.sql.Types.STRUCT).
func arrayElementTypeNameFromDescs(collField string, descs []protoreflect.MessageDescriptor) string {
	if collField == "" {
		return ""
	}
	colDesc := descriptorForColumn(collField, descs)
	if colDesc == nil {
		return ""
	}
	fd := colDesc.Fields().ByName(protoreflect.Name(parseColRef(collField).bare()))
	if fd == nil || !fd.IsList() {
		return ""
	}
	if fd.Kind() == protoreflect.MessageKind {
		if msg := fd.Message(); msg != nil && string(msg.FullName()) == functions.UUIDProtoMessageName {
			return "OTHER"
		}
		return "STRUCT"
	}
	return protoKindToTypeName(fd.Kind())
}

func deriveColumnsFromFlatMap(fm *plans.RecordQueryFlatMapPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	// An ORDINAL lateral-unnest seed (a NON-anchored RC over a
	// FlatMap-over-Explode, carrying baked ofOrdinal outer columns) replaces
	// the name-model anchored seed for a single-source unnest. It lands here
	// exactly like the anchored arm below, but two things differ: its baked
	// outer fields render "T1.ID#0" under ExplainValue (foldedColumnDef's
	// value-derived descriptor lookup then misses and mis-reports a pk's
	// nullability), and its FULL outer run KEEPS a column the element AS/AT
	// alias SHADOWS (the name model dropped it in buildUnnestResultValue).
	// Derive the SELECT-* columns to MATCH the name model: outer columns
	// resolved against the scan descriptor by their BARE name (pk NOT NULL),
	// the shadowed outer column dropped, the element/ordinal typed from their
	// own Value (element nullable from the array element, ordinal INT NOT
	// NULL). Scoped by findExplodePlan (the unnest signature — excludes the
	// correlated-scalar-subquery ordinal seed below, whose inner is not an
	// Explode) AND ContainsBakedOrdinal (excludes name-model /
	// projected-EXISTS folds). RFC-142.
	if rc, ok := fm.GetResultValue().(*values.RecordConstructorValue); ok &&
		len(rc.Fields) > 0 && findExplodePlan(fm.GetInner()) != nil && values.ContainsBakedOrdinal(rc) {
		descs := allLeafDescriptors(fm.GetOuter(), md)
		innerCorr := fm.GetInnerAlias()
		// The element/ordinal columns reference the INNER correlation; a same-named
		// OUTER column is SHADOWED by the AS/AT alias (name-model rule).
		shadowed := map[string]struct{}{}
		for _, f := range rc.Fields {
			if corr, ok := valueRootCorrelation(f.Value); ok && corr == innerCorr {
				shadowed[strings.ToUpper(f.Name)] = struct{}{}
			}
		}
		cols := make([]executor.ColumnDef, 0, len(rc.Fields))
		for _, f := range rc.Fields {
			corr, hasCorr := valueRootCorrelation(f.Value)
			isInner := hasCorr && corr == innerCorr
			if !isInner {
				if _, clash := shadowed[strings.ToUpper(f.Name)]; clash {
					continue // outer column shadowed by the element/ordinal alias
				}
			}
			cols = append(cols, ordinalUnnestColumnDef(f, descs))
		}
		return cols
	}

	// RFC-141 Phase 2: a projected-EXISTS FlatMap folds the SELECT projection
	// into its result value — an ordinary (non-anchored-join) RecordConstructor.
	// Its field names ARE the output columns (e.g. ID, HAS_T2), so derive from
	// them directly rather than merging the outer+inner table columns.
	if rc, ok := fm.GetResultValue().(*values.RecordConstructorValue); ok && len(rc.Fields) > 0 {
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

		// The correlated-scalar-subquery-in-projection ordinal seed
		// (scalarSubqueryOrdinalSeed) is ALSO a raw (non-anchored) RC and
		// lands here. Unlike a regular gated-join ordinal seed — whose legs
		// are BOTH typed via ordinalLegType — its INNER scalar leg is typed
		// UnknownType at translation (Go quantifier flowed types are
		// untyped). foldedColumnDef resolves types only against the OUTER
		// leaf descriptors, so it cannot reach the inner subquery's type and
		// falls back to BIGINT — regressing a DOUBLE (AVG) / STRING / etc.
		// scalar to BIGINT. Derive that one field's type from the INNER plan
		// (a scalar subquery exposes exactly ONE output column), exactly as
		// the retired name-model path did via its outer+inner merge.
		// Scoping: IsOrdinalJoinRV excludes RFC-141 projected-EXISTS folds
		// (their result value is not an ordinal-join RC), and the per-field
		// untyped-inner-leg test excludes regular gated-join seeds (their inner
		// legs are already typed, so isCorrelatedScalarInnerLeg is false for them).
		innerScalarType := ""
		if values.IsOrdinalJoinRV(rc) {
			if innerCols := deriveColumnsFromPlan(fm.GetInner(), md); len(innerCols) == 1 {
				innerScalarType = innerCols[0].TypeName
			}
		}
		innerAlias := fm.GetInnerAlias().Name()

		cols := make([]executor.ColumnDef, 0, len(rc.Fields))
		for _, f := range rc.Fields {
			col := foldedColumnDef(f, descs)
			if innerScalarType != "" && isCorrelatedScalarInnerLeg(f, innerAlias) {
				// Only the TYPE is corrected; Name/Label/Nullable from
				// foldedColumnDef stay (the inner scalar leg is LEFT-OUTER
				// null-supplying, so it must remain nullable regardless of the
				// inner column's own nullability).
				col.TypeName = innerScalarType
			}
			cols = append(cols, col)
		}
		// DUPLICATE bare labels stay BARE — Java's rule (the SELECT-list
		// Identifier post-clearQualifier: `SELECT t1.id, t2.id` labels both
		// columns ID; JDBC allows duplicate labels), pinned by the
		// cross-engine conformance metadata corpus. The datum Name keeps the
		// QUALIFIED form (bareLeafDuplicated) so internal reads never
		// collapse the two columns — only the user-visible label is bare.
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

// isCorrelatedScalarInnerLeg reports whether an ordinal-seed field is the INNER
// scalar leg of a correlated-scalar-subquery ordinal seed: a
// FieldValue over the inner-alias QOV whose flowed type is UnknownType (the
// scalarSubqueryOrdinalSeed types this one leg UnknownType because Go quantifier
// flowed types are untyped at translation). A regular gated-join seed's inner
// legs are typed via ordinalLegType, so this is false for them — keeping the
// type correction scoped to the correlated-scalar seed. Caller has already
// gated on values.IsOrdinalJoinRV.
func isCorrelatedScalarInnerLeg(f values.RecordConstructorField, innerAlias string) bool {
	fv, ok := f.Value.(*values.FieldValue)
	if !ok {
		return false
	}
	qov, ok := fv.Child.(*values.QuantifiedObjectValue)
	if !ok || !strings.EqualFold(qov.Correlation.Name(), innerAlias) {
		return false
	}
	t := fv.Type()
	return t == nil || t.Code() == values.TypeCodeUnknown
}

// joinResultValueIsReversed checks whether the plan's resultValue
// indicates that the SQL-level column order is opposite to the physical
// outer/inner assignment. The translator builds the binary join seed in SQL
// order [outer, inner]; comparing the SQL-first leg against the physical
// outerAlias tells us whether columns need to be emitted in reversed order.
func joinResultValueIsReversed(rv values.Value, physOuterAlias, physInnerAlias string) bool {
	_ = physOuterAlias
	// The gated LEFT/RIGHT ordinal seed keeps DECLARATION order while the
	// physical legs run in EXECUTION (swapped) order — the SQL-first leg is
	// the FIRST field's root baked QOV. Without this arm a RIGHT join's
	// SELECT * metadata derived in execution order while the positional row
	// followed the seed: the driver scanned dept values against emp columns
	// (caught by the parity matrix).
	if rc, isRC := rv.(*values.RecordConstructorValue); isRC &&
		len(rc.Fields) > 0 && values.ContainsBakedOrdinal(rc) {
		if corr, ok := valueRootCorrelation(rc.Fields[0].Value); ok {
			return strings.EqualFold(corr.Name(), physInnerAlias)
		}
	}
	return false
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
	descs []protoreflect.MessageDescriptor,
) []executor.ColumnDef {
	var firstDesc protoreflect.MessageDescriptor
	if len(descs) > 0 {
		firstDesc = descs[0]
	}
	cols := make([]executor.ColumnDef, 0, len(groupKeys)+len(aggregates))
	for _, k := range groupKeys {
		// The datum lookup key (ColumnDef.Name) MUST be the name the aggregate
		// cursor writes — executor aggKeyName: a FieldValue keys by its bare
		// Field, everything else by ExplainValue. A resolved group key carrying
		// a correlation Child (the duplicate-alias binding FieldValue(
		// QOV(Q$DUP1), QID); the RFC-142 shadow-qualified twin) explains as the
		// QUALIFIED "Q$DUP1.QID" while the cursor keys the output row by the
		// bare "QID" — deriving the column Name from ExplainValue read the
		// missing qualified key off the bare-named row and served NULL for a
		// correctly-grouped result. This bare-Field-vs-ExplainValue convention
		// has three mirrors that must agree: executor aggKeyName,
		// aggregateGroupKeyOutputName (logical_predicate.go), and this
		// derivation. So the qualified Name is load-bearing and stays.
		name := values.ColumnNameValue(k)
		if fv, ok := k.(*values.FieldValue); ok {
			name = fv.Field
		}
		// The DISPLAY label is always BARE: Java clears the qualifier on the
		// top-level projection (Expression.clearQualifier), so a qualified group
		// key `d.dname` labels the output column `DNAME`, never `D.DNAME`. Carry
		// it as Label (bare) and keep Name qualified for the datum lookup — the
		// driver's Rows.Columns() surfaces Label when set (paginatingRows).
		bare := parseColRef(name).bare()
		label := ""
		if !strings.EqualFold(name, bare) {
			label = strings.ToUpper(bare)
		}
		// The TYPE resolves against ALL join-leaf descriptors, not just the
		// first: a group key from a FAR join leg (`GROUP BY d.dname` over
		// `emp JOIN dept`) lives on the second leg and served UNKNOWN under a
		// single first-leaf descriptor. Java reads the type off the flowed
		// join-output record's field.
		typeName := "UNKNOWN"
		nullable := api.ColumnNullable
		if d := descriptorForColumn(bare, descs); d != nil {
			typeName = protoFieldTypeName(d, bare)
			if fd := d.Fields().ByName(protoreflect.Name(bare)); fd != nil && fd.Cardinality() == protoreflect.Required {
				nullable = api.ColumnNoNulls
			}
		}
		cols = append(cols, executor.ColumnDef{
			Name:     strings.ToUpper(name),
			Label:    label,
			TypeName: typeName,
			Nullable: nullable,
		})
	}
	for _, a := range aggregates {
		name := aggregateSpecName(a)
		// Aggregate result type stays first-leaf-resolved (unchanged): COUNT/AVG
		// are operator-fixed and a SUM/MIN/MAX operand is overwhelmingly the
		// aggregated leg's own column. Far-leg aggregate-operand typing is a
		// separate axis outside this rider's group-key scope.
		typeName := aggregateResultType(a, firstDesc)
		// A user-written alias is the OUTPUT column name and must win over the
		// generated `MAX(A)` spelling. A GROUPED aggregate keeps a projection
		// above it that already carries the alias, so this arm was only ever
		// reached for a SCALAR aggregate — whose plan is the bare
		// StreamingAgg, with nowhere else for the alias to live. Without this,
		// `SELECT MAX(a) AS agg FROM t` reported the column as `MAX(A)` while
		// the grouped form of the same query correctly reported `AGG`.
		// Name stays the generated spelling: it is the datum lookup key the
		// aggregate cursor writes (see the group-key comment above); Label is
		// what Rows.Columns() surfaces.
		label := ""
		if a.Alias != "" {
			label = strings.ToUpper(a.Alias)
		}
		cols = append(cols, executor.ColumnDef{
			Name:     strings.ToUpper(name),
			Label:    label,
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
	return values.ColumnNameValue(a.Operand)
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
				operandName := values.ColumnNameValue(av.Operand)
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
		case values.TypeCodeUuid:
			// JDBC getColumnTypeName for a UUID is the catch-all "OTHER"
			// (Java: DataType.Code.UUID → Types.OTHER → "OTHER"), matching the
			// field-path protoFieldTypeName so all metadata paths agree.
			return "OTHER"
		case values.TypeCodeRecord:
			// A STRUCT column (java.sql.Types.STRUCT; api.SQLTypeNameStruct) —
			// without this case a record-typed value (a struct-array unnest
			// ELEMENT) fell through to "" and the BIGINT fallback silently
			// mistyped it (review finding, pinned).
			return "STRUCT"
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

// descriptorRefinesFlowed reports whether the stored-descriptor type `descType`
// legitimately REFINES the flowed value type `flowed`. The descriptor override in
// column-metadata derivation exists only to recover the eval-width the flowed seed
// conflates within a numeric family (INTEGER↔BIGINT, FLOAT↔DOUBLE). A derived/CTE
// output alias colliding with a stored field name flows a genuinely different type,
// so the descriptor must NOT override across families — only within one, or on an
// exact match.
func descriptorRefinesFlowed(descType, flowed string) bool {
	if flowed == "" {
		return true // no flowed type to trust — the descriptor is the sole authority
	}
	if df := numericFamily(descType); df != "" && df == numericFamily(flowed) {
		return true // same numeric family: the descriptor recovers the conflated width
	}
	return strings.EqualFold(descType, flowed)
}

// numericFamily buckets a JDBC type name into its width-conflation family, or ""
// for a non-numeric type (which must match exactly).
func numericFamily(t string) string {
	switch strings.ToUpper(t) {
	case "INTEGER", "BIGINT":
		return "int"
	case "FLOAT", "DOUBLE":
		return "float"
	}
	return ""
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
		// — a LEFT-OUTER join select over the outer row (the ordinal scalar seed)
		// with its own per-row LIMIT-peel. Composing
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
		gkVals := make([]values.Value, len(agg.GroupKeys))
		for i, k := range agg.GroupKeys {
			gkVals[i] = k.Value
		}
		if projectValuesReferenceExists(gkVals) || projectValuesReferenceExists(agg.AggregateOperands) {
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
				demoted := logical.NewScan(table, alias)
				demoted.Binding = u.Binding
				j.Right = demoted
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
	// Segment 0 names a PRIOR lateral unnest's element (a CHAINED unnest,
	// `… t.arr AS x, x.sub AS y AT o`). The translator lowers it
	// (translateChainedUnnestJoin) with ordinality support, so AT here is VALID,
	// not "AT on a table" — leave it to the translator's per-case disposition
	// (array→plan, scalar sub→UNDEFINED_COLUMN, present-scalar sub→INVALID_
	// COLUMN_REFERENCE). Checked before the outerTable=="" reject below, which
	// would otherwise mistake the unnest-element owner for a bare table source.
	if logical.FindOwnerUnnest(left, u.Segments[0]) != nil {
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
func rejectDuplicateUnnestAlias(op logical.LogicalOperator, md *recordlayer.RecordMetaData) error {
	return rejectDuplicateUnnestAliasInner(op, md, nil)
}

// rejectDuplicateUnnestAliasInner carries the in-scope WITH definitions so a
// FROM chain's Scan-on-a-CTE-name legs can derive their output columns from
// the CTE Body (the ambiguity check is column-aware for them, exactly like
// base tables).
func rejectDuplicateUnnestAliasInner(op logical.LogicalOperator, md *recordlayer.RecordMetaData, ctes map[string]*logical.LogicalCTE) error {
	if op == nil {
		return nil
	}
	// A WITH definition is visible to its Main subtree (and to its own Body —
	// recursive CTEs self-reference; for a non-recursive Body the extra
	// visibility is inert, its scans can't name the CTE).
	if cte, ok := op.(*logical.LogicalCTE); ok {
		sub := make(map[string]*logical.LogicalCTE, len(ctes)+1)
		for k, v := range ctes {
			sub[k] = v
		}
		sub[strings.ToUpper(cte.Name)] = cte
		ctes = sub
	}
	// A LogicalJoin is the root of a FROM-scope join chain. Collect every source
	// alias in that chain and reject any unnest whose AS/AT alias duplicates
	// another source's. The chain walk stops at a derived/CTE Body — a derived
	// source is its own FROM scope — exactly like outerBoundAliases /
	// buriedUnnestLegs; the recursion below then re-enters those nested scopes.
	if j, ok := op.(*logical.LogicalJoin); ok {
		if err := checkFromScopeUnnestAliases(j, md, ctes); err != nil {
			return err
		}
	}
	for _, ch := range op.Children() {
		if err := rejectDuplicateUnnestAliasInner(ch, md, ctes); err != nil {
			return err
		}
	}
	for _, sub := range subqueryPlans(op) {
		if err := rejectDuplicateUnnestAliasInner(sub, md, ctes); err != nil {
			return err
		}
	}
	return nil
}

// fromLegSchema describes one FROM-chain leaf source for the RFC-142
// duplicate unnest-alias check: its UPPER alias. (The column-derivation
// fields died when ambiguity checking moved to per-attribute reference
// resolution, replacing an earlier FROM-level 42702 approximation.)
type fromLegSchema struct {
	alias string
}

// checkFromScopeUnnestAliases gathers every leaf source alias of the FROM-scope
// join chain rooted at `j` (Scan aliases, prior-unnest AS/AT aliases, derived/CTE
// leg OUTER aliases — never descending into a derived/CTE Body, which is a separate
// scope) and rejects any lateral-unnest leg in that chain whose AS or AT alias also
// names another source in the same chain. The check is symmetric across the chain,
// so it catches both an earlier and a later collision. RFC-142.
func checkFromScopeUnnestAliases(j *logical.LogicalJoin, md *recordlayer.RecordMetaData, ctes map[string]*logical.LogicalCTE) error {
	var legs []fromLegSchema // every leaf source (Scan / derived leg) in the chain
	var unnests []*logical.LogicalUnnest
	var walk func(logical.LogicalOperator, map[string]*logical.LogicalCTE)
	walk = func(o logical.LogicalOperator, ctes map[string]*logical.LogicalCTE) {
		switch n := o.(type) {
		case *logical.LogicalScan:
			a := n.Alias
			if a == "" {
				a = n.Table
			}
			if a == "" {
				return
			}
			legs = append(legs, fromLegSchema{alias: strings.ToUpper(a)})
		case *logical.LogicalUnnest:
			unnests = append(unnests, n)
		case *logical.LogicalCTE:
			// A derived/CTE leg contributes only its OUTER alias (its Main is
			// a Scan on the definition name); its Body is a separate FROM
			// scope, not descended here. Registering the definition makes the
			// Scan arm derive the leg's OUTPUT columns from the Body — a
			// derived leg is column-aware, exactly like a base table.
			sub := make(map[string]*logical.LogicalCTE, len(ctes)+1)
			for k, v := range ctes {
				sub[k] = v
			}
			sub[strings.ToUpper(n.Name)] = n
			walk(n.Main, sub)
		default:
			for _, c := range o.Children() {
				walk(c, ctes)
			}
		}
	}
	walk(j, ctes)
	// Duplicate FROM aliases register freely (the parser mints per-leg
	// binding ids, assignFromLegBindingIDs), every reference resolves
	// per-ATTRIBUTE at the semantic scope (Scope.ResolveQualifiedColumn/
	// ResolveColumn — ≥2 matches raise Java's exact `Ambiguous reference X`
	// 42702), and the cluster gate admits binding-distinguished duplicate
	// legs into the ordinal seed, matching Java's live model. Undefined
	// tables keep failing through validateTablesAndColumns (42F01 —
	// resolution declines on unknowable tables, so the ambiguity path cannot
	// mask it). Only the RFC-142 unnest-alias half below remains: Java
	// genuinely forbids a duplicate unnest AS/AT alias at FROM.
	if len(unnests) == 0 {
		return nil
	}
	// Build, for each unnest, the set of OTHER sources' aliases: every scan alias
	// plus every OTHER unnest's AS/AT aliases. A collision against any of them is a
	// duplicate range-variable name.
	for _, u := range unnests {
		others := make(map[string]struct{}, len(legs)+2*len(unnests))
		for _, leg := range legs {
			others[leg.alias] = struct{}{}
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
		for _, csq := range o.CorrelatedScalarSubqueries {
			plans = append(plans, csq.InnerPlan)
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
	// Delegates to recordTypeCI (the value-returning form) so the case-insensitive
	// record-type resolution lives in exactly one place.
	return recordTypeCI(md, name) != nil
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
		// Keep a DEFAULTED alias in lockstep with the strip — the same
		// alias-desync root fix the catalog sub-build path applies by
		// normalizing sq before building (logical_predicate.go, the
		// normalize-first comment): a no-alias `s.LB` parses with
		// alias == tableName == "S.LB", and leaving the dotted alias on the
		// scan while the ON-upgrade scope registers the bare "LB" makes the
		// upgraded predicate's QOV(LB) miss the leg at translation — the
		// INNER form failed leg attribution loud and the LEFT form silently
		// padded every row (review-caught by the Q37 pin family).
		if scan.Alias == scan.Table {
			scan.Alias = resolved
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
	if proj, ok := op.(*logical.LogicalProject); ok && !hasJoin(op) && !hasAggregate(op) &&
		!projectionInputRedefinesColumns(proj.Input) {
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
					// Try the VERBATIM name before the folded one: a quoted
					// lowercase column ("x") declares a lower-case proto
					// field, and folding it here mis-rejected a legal
					// projection with 42703 (WS-N quoting-blindness; the
					// resolution path itself handles the quoted name fine).
					if rt.Descriptor.Fields().ByName(protoreflect.Name(upper)) == nil &&
						rt.Descriptor.Fields().ByName(protoreflect.Name(parseColRef(col).bare())) == nil {
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

// projectionInputRedefinesColumns reports whether a projection's input chain
// introduces a NEW column namespace (a nested derived-table projection) before
// reaching a base scan. When it does, the projection's column names are the
// derived (possibly renamed) OUTPUT names — validating them against the base
// scan's record type would spuriously reject a legitimately renamed column
// (e.g. `SELECT v AS y FROM (SELECT id AS v FROM a) i`, where `v` is `i`'s
// output column, not a field of `a`). Pass-through ops (Filter/Sort/Limit/
// Distinct) don't rename columns, so we descend through them. An unknown op is
// treated conservatively as redefining (skip the base-scan check; the resolver
// and runtime still catch genuinely undefined columns).
func projectionInputRedefinesColumns(input logical.LogicalOperator) bool {
	for cur := input; cur != nil; {
		switch o := cur.(type) {
		case *logical.LogicalScan:
			return false
		case *logical.LogicalFilter:
			cur = o.Input
		case *logical.LogicalSort:
			cur = o.Input
		case *logical.LogicalLimit:
			cur = o.Input
		case *logical.LogicalDistinct:
			cur = o.Input
		default:
			// LogicalProject (derived-table rename), or any op that changes
			// the column namespace.
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
