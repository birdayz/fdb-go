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
	"fdb.dev/pkg/dst"
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
	"fdb.dev/pkg/relational/core/rowstruct"
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
	// The logging scope opens FIRST, before anything that can fail.
	//
	// PlanGenerationLogger's contract is ONE callback per Plan() call
	// (plan_logging.go:73-77), and a contract that holds only on the paths that
	// reach the end of the function is not a contract — an operator watching for
	// planning failures would see silence from exactly the failures worth
	// watching. The store open below is the first fallible step and it fails
	// CLOSED, so opening the scope after it would have made every one of those
	// failures invisible. planDML already orders it this way.
	//
	// Consequence, and it is the correct one: PlanningDuration now includes the
	// index-state store open. That open IS planning work — it decides which
	// indexes may back the plan — and excluding the dominant cost on this path
	// from the metric that exists to report planning cost would be the bug.
	//
	// Log the original whitespace-preserved SQL (canonicalTextOf), not
	// q.GetText() — the latter concatenates tokens without whitespace
	// ("SELECTid=1FROMorders"), which is useless to an operator. The plan-cache
	// key below is built off canonicalTextOf too, so both are injective.
	var ls *planLogScope
	if logMetrics {
		ls = g.beginPlanLog(ctx, canonicalTextOf(q))
	}
	defer func() { ls.finish(err) }()

	popts := plannerOptionsFrom(g.c.Options())
	// ONE index-state read serves both the readable-index VIEW and the plan's
	// index DEPENDENCIES, and it is taken before the cache key is built.
	//
	// The view decides which indexes may back a plan and is PART of the cache
	// key; Java keys its plan cache the same way (PlannerConfiguration carries
	// readableIndexes, QueryCacheKey carries the whole configuration —
	// QueryCacheKey.java:127,142). The dependencies are the indexes the finished
	// plan's correctness rests on, revalidated inside every execution
	// transaction; the cache key cannot do that job, because an auto-commit
	// statement's pages are separate transactions and the key is consulted once
	// per statement. See index_state_planning.go.
	//
	// One open, not two: the open is the dominant cost on this path
	// (TestFDB_ReadableIndexViewLatency measures ~1.28 ms of a 2.71 ms cached
	// point-lookup SELECT), and it must be one read anyway — the view decides
	// which indexes become candidates, and the dependencies are read off the
	// plan those candidates produce, so two reads could have the plan built
	// against one moment's states and its dependencies pinned to another's.
	//
	// Opening the store, rather than reading the index-state subspace directly,
	// is what makes the state the one checkVersion has already reconciled — see
	// fetchIndexStateSnapshot.
	indexStateSnapshot, stateErr := g.fetchIndexStateSnapshot(ctx, md)
	if stateErr != nil {
		return nil, stateErr
	}
	popts.config.ReadableIndexes = readableIndexesFrom(md, indexStateSnapshot)
	// A cross-row uniqueness proof is a statement about an INSTANT, so it only
	// licenses anything when the WHOLE result comes from one read version.
	// fetchPage routes on exactly this condition: with an explicit transaction
	// every page joins it and shares its read version; without one each page
	// runs its own auto-commit transaction and takes a fresh one, so a value
	// can move between pages and be emitted twice. See
	// PlannerConfiguration.SingleReadVersion.
	popts.config.SingleReadVersion = g.c.activeTx != nil
	// Plan-cache key parts: a VERBATIM schema+version+planner-options scope
	// (case-sensitive, never normalized) and the injective canonical query
	// text. NOT q.GetText() — that concatenated tokens with no separator,
	// colliding `SELECT AB` with `SELECT A B`. PlanCache normalizes only the
	// query text (see planCacheScope / PlanCache.Get).
	cacheScope := planCacheScope(g.c.sess.DBPath, g.c.sess.Schema, md.Version(), popts.cacheKeyPart())
	cacheSQL := canonicalTextOf(q)

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
				sql:              g.c.execLogSQL(q),
				// Dependencies are a function of the PLAN, so a cache hit derives
				// them from the cached plan rather than carrying anything in the
				// cache entry. A cached plan gets the same guarantee as a freshly
				// planned one, which is what the cache exists to be transparent
				// about.
				indexDependencies: collectPlanIndexDependencies(md, cachedPlan, cachedSubs),
			}, nil
		}
	}

	// A windowed aggregate (`SUM(v) OVER (PARTITION BY g)`) is not supported. The
	// aggregate planner ignores the OVER clause and computes a bare aggregate,
	// which silently returns WRONG results, so reject it up front — a true
	// front-end pre-pass (before building a soon-discarded logical plan). Detected
	// on the parse tree because the OVER clause is dropped during lowering, so the
	// logical plan provably cannot carry it (mirrors findAggregateInTree).
	if err := rejectWindowedAggregate(q); err != nil {
		return nil, err
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

	if err := runFromResolutionPostPasses(logicalOp, g.c.sess.Schema, md, g.c.cachedMetaData()); err != nil {
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

	// COLLECTED statistics (RFC-236) take precedence when the connection opted in
	// and every gate passes; otherwise the legacy record-count-key source, which
	// is inert for SQL-created schemas since RFC-204 removed that key. Both are
	// best-effort and both degrade to the cost model's constant.
	stats := g.fetchCollectedStatistics(ctx, md, popts)
	if stats == nil {
		stats = g.fetchTableStatistics(ctx, md)
	}
	planner := newCascadesPlanner(md, popts, cascades.BatchAExpressionRules(), stats)

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
	// RFC-204 §4.5.1: bake one type repository into the plan's record
	// constructors. This is the plan-cache MISS path — the hit path above
	// returns the same plan pointer to concurrent executions, so this is the
	// only point at which stamping is not a data race.
	cascades.FinalizePlan(physPlan)
	// Plan scalar subqueries independently through the Cascades pipeline
	// (planScalarSubqueryPlans — the one planning path, shared with the
	// plan harness).
	scalarSubs, subErr := planScalarSubqueryPlans(ctx, scalarSubqueryPlans, md, stats, popts)
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
		sql:              g.c.execLogSQL(q),
		// The fourth argument is the PROOF-ONLY dependency set: indexes whose
		// metadata property licensed a transformation without the index being
		// scanned. It is nil because no rule currently produces one — the only
		// such proof this engine had, a secondary UNIQUE index licensing a
		// DISTINCT elision, is declined outright today
		// (rule_implement_distinct_final.go), and every other consumer of an
		// index property reads it FOR the index plan it is building, which the
		// leaf walk already collects. The seam is real and unit-tested rather
		// than notional: whoever lifts that decline records the proving index
		// here, and TestDistinctFinal_SecondaryUniqueIndexIsNeverAnEliminationProof
		// is what fails if they lift it without doing so.
		indexDependencies: collectPlanIndexDependencies(md, physPlan, scalarSubs),
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

// explainLogicalQuery renders the logical-plan text for a query, preferring
// the catalog-aware builder when metadata is available. It is the EXPLAIN half
// of the ExplainFn that planSelect installs for the two query shapes that never
// reach Cascades, and reproduces all three of its steps — including the
// echo-the-statement last resort, which both logical builders can fall through
// to (buildLogicalPlanForQuery returns nil on an out-of-scope query body or CTE
// body). Returning "" there instead would make planExplain raise
// "produced no plan text" for a query whose own plan renders that echo, which
// is the same failure this file exists to remove, pointed the other way.
//
// planSelect echoes the SelectStatement node while EXPLAIN hands us the Query
// node; the grammar is `selectStatement : query`, a single child, so the two
// GetText() renderings are identical.
func (g *cascadesGenerator) explainLogicalQuery(ctx context.Context, q antlrgen.IQueryContext, md *recordlayer.RecordMetaData) (string, error) {
	if err := contextCancellationError(ctx); err != nil {
		return "", err
	}
	if md != nil {
		if op, err := buildLogicalPlanForQueryWithCatalog(q, md); err == nil && op != nil {
			return explainWithContext(ctx, func() string { return op.Explain("") })
		}
	}
	if op := buildLogicalPlanForQuery(q); op != nil {
		return explainWithContext(ctx, func() string { return op.Explain("") })
	}
	return explainWithContext(ctx, func() string { return explainStatement("SELECT", q) })
}

// computeExplainText builds the plan-tree text for the inner
// statement of an EXPLAIN.
//
// The SELECT branch holds this invariant: EXPLAIN renders the plan the engine
// would actually run, or fails with the error running the query itself would
// raise — it never renders a plan the engine cannot execute. It mirrors
// planSelect's routing one-for-one, error returns included: once Cascades is
// attempted, its failure IS the answer. Java behaves the same way — an
// unplannable EXPLAIN lets UnableToPlanException propagate as 0AF00, and its
// relational layer has no logical-plan renderer on the EXPLAIN path at all.
// The logical-text arms that remain are the shapes where planSelect itself
// yields no physical plan, so EXPLAIN and the executed query still agree on
// what they describe; each is annotated at its branch.
//
// The DML branches do NOT hold it, and knowingly so. They render logical text
// while planDML builds a real Cascades plan, so EXPLAIN describes a different
// tree than the one that executes. That is a weaker defect than the SELECT one
// — the statement does run — but it is a divergence from Java, which renders
// the physical RecordQueryPlan for DML too. Tracked in TODO.md; see the note
// above the DML arms.
func (g *cascadesGenerator) computeExplainText(ctx context.Context, d *antlrgen.DescribeStatementsContext) (string, error) {
	if err := contextCancellationError(ctx); err != nil {
		return "", err
	}
	c := g.c
	md := c.cachedMetaData()

	if q := d.Query(); q != nil {
		// Explain-only mode (no FDB session): planSelect routes to
		// planSelectExplainOnly, whose plan renders this same logical text and
		// refuses to execute. There is no physical plan in this mode to hide,
		// so matching it is the accurate answer, not a degrade.
		if c.sess == nil || c.sess.DB == nil {
			return g.explainLogicalQuery(ctx, q, md)
		}
		// INFORMATION_SCHEMA is a Go-only extension served off the catalog by
		// execSystemTableQuery, never by Cascades. planSelect's PlanFunc runs
		// the same three-step rendering as its own Explain, so EXPLAIN reports
		// the plan that really runs.
		if referencesInformationSchema(q) {
			return g.explainLogicalQuery(ctx, q, md)
		}
		// From here the Cascades plan IS the plan. Every failure below is the
		// failure `SELECT ...` would raise, so it is surfaced verbatim.
		if err := c.ensureMetaData(ctx); err != nil {
			return "", err
		}
		freshMd := c.cachedMetaData()
		if freshMd == nil {
			return "", api.NewError(api.ErrCodeUnsupportedQuery,
				"no schema metadata available")
		}
		plan, planErr := g.planSelectCascades(ctx, q, freshMd, false)
		if planErr != nil {
			return "", planErr
		}
		return explainWithContext(ctx, plan.Explain)
	}
	// DML renders logical text. Java's EXPLAIN of a DML statement produces the
	// physical RecordQueryPlan instead (the mutation is not executed); closing
	// that gap is tracked in TODO.md, not done here.
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
	if err := rejectWindowedAggregate(dml); err != nil {
		return nil, err
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
	ref, dmlScalarSubqueryPlans, translateErr := query.TranslateToCascadesWithError(logicalOp, md)
	if translateErr != nil {
		return nil, translateErr
	}
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

	// DML reads through the same match candidates a SELECT does (the WHERE of an
	// UPDATE/DELETE is planned identically), so it needs the same readable-index
	// view — Java applies the filter in MetaDataPlanContext, below both — and it
	// carries the same index-state dependency and the same staleness check. One
	// snapshot supplies both, as on the SELECT path.
	dmlIndexStateSnapshot, dmlStateErr := g.fetchIndexStateSnapshot(ctx, md)
	if dmlStateErr != nil {
		return nil, dmlStateErr
	}
	planningRules := append(cascades.BatchAExpressionRules(), cascades.DMLImplementationRules()...)
	popts := plannerOptionsFrom(g.c.Options())
	popts.config.ReadableIndexes = readableIndexesFrom(md, dmlIndexStateSnapshot)
	// A cross-row uniqueness proof is a statement about an INSTANT, so it only
	// licenses anything when the WHOLE result comes from one read version.
	// fetchPage routes on exactly this condition: with an explicit transaction
	// every page joins it and shares its read version; without one each page
	// runs its own auto-commit transaction and takes a fresh one, so a value
	// can move between pages and be emitted twice. See
	// PlannerConfiguration.SingleReadVersion.
	popts.config.SingleReadVersion = g.c.activeTx != nil
	// Collected statistics take precedence; the legacy count-key source is the
	// fallback. Placed after popts exists, since gate 1 reads the flag from it.
	dmlStats := g.fetchCollectedStatistics(ctx, md, popts)
	if dmlStats == nil {
		dmlStats = g.fetchTableStatistics(ctx, md)
	}
	planner := newCascadesPlanner(md, popts, planningRules, dmlStats)

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
	cascades.FinalizePlan(physPlan)

	// Plan the DML statement's scalar subqueries (`DELETE … WHERE v > (SELECT
	// …)`) through the same shared pipeline as SELECT and carry them on the
	// plan so fetchPage pre-binds their results. This path historically
	// DISCARDED them (`ref, _ :=`), and the unbound value silently evaluated
	// NULL — the DELETE compared v > NULL (UNKNOWN) and removed NOTHING, with
	// both differential models identically wrong; the loud
	// values.UnboundScalarSubqueryError is what surfaced it.
	dmlScalarSubs, dmlSubErr := planScalarSubqueryPlans(ctx, dmlScalarSubqueryPlans, md, dmlStats, popts)
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
		sql:              g.c.execLogSQL(dml),

		indexDependencies: collectPlanIndexDependencies(md, physPlan, dmlScalarSubs),
		dryRun:            dryRun,
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

	// The indexes this plan depends on, revalidated inside every execution
	// transaction — including cache hits and every continuation page. Java's
	// continuation plan constraint (QueryPlan.java:726-735) does the same job;
	// see index_state_planning.go.
	indexDependencies planIndexDependencies

	// sql is the canonical whitespace-preserved query text, carried from
	// planning to execution so an ExecutionStats record can name its statement
	// (RFC-211). It is "" whenever no execution-stats logger is installed —
	// execLogSQL gates the substring materialization, so the disabled path
	// pays nothing. Never GetText(): that concatenates tokens without
	// separators.
	sql string

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

	// RFC-211: start the execution-stats scope BEFORE the first page, so the
	// duration spans the work rather than reporting on it afterwards. nil when
	// no logger is installed. It is handed to the paginatingRows below, whose
	// Close is the single emission funnel every path reaches.
	execLog := c.beginExecLog(ctx, p.sql, p.physicalPlan)

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
		execLog:          execLog,
		ss:               ss,
		plan:             p.physicalPlan,
		md:               p.md,
		scalarSubqueries: p.scalarSubqueries,

		indexDependencies: p.indexDependencies,

		maxRows:        optInt64(c.Options(), api.OptMaxRows, math.MaxInt32),
		maxResultBytes: c.maxResultBytes,
		cols:           cols,
		tx:             c.activeTx,
		isUpdate:       p.IsUpdate(),
		dryRun:         p.dryRun,
		// The statement-stable CURRENT_TIMESTAMP-family instant is stamped
		// ONCE here, while the statement is in flight (the driver entry
		// point's session-clock stamp is still live). It must be captured on
		// the result set, not read per page: page 1 is fetched eagerly below,
		// but pages 2+ are fetched lazily from rows.Next() AFTER the driver
		// call returned and its deferred clock-restore zeroed the session
		// stamp — reading the session clock per page would fall back to wall
		// clock and drift across page boundaries. Internal page resume
		// therefore carries the ORIGINAL stamp; an external continuation
		// resume is rejected before this point and stamps afresh by design.
		statementTime: c.statementNow(),
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
		// The statement-scoped scratch, minted ONCE for the same reason
		// execState is: each page rebuilds the cursor hierarchy, and an
		// operator whose resume state is O(rows already emitted) — the
		// unordered hash DISTINCT's seen-set — must hand that state to the next
		// page instead of serializing it into every page's continuation, which
		// costs O(pages^2) to write and re-parse. These continuations never
		// leave the statement (statement continuations are rejected outright,
		// see cascadesPlan's continuation check), so the scratch has exactly
		// their lifetime.
		scratch: executor.NewExecutionScratch(),
	}

	// Eagerly fetch the first page so execution errors (type mismatches,
	// plan failures) surface at QueryContext time, not during row iteration.
	if err := pr.fetchPage(); err != nil {
		// The statement is over and it FAILED; Close is the emission funnel,
		// so the error has to reach it. A statement killed here by a scan
		// limit still reports what it consumed — the counters were charged
		// per attempt on the way out, not at a success-only checkpoint.
		pr.statsErr = err
		pr.Close()
		return query.Result{}, err
	}

	// DML (INSERT/UPDATE/DELETE) plans emit one row per affected record;
	// the affected-row count is the JDBC update count, not a result set.
	// Drain and count, matching Java's AbstractEmbeddedStatement.countUpdates.
	// The mutations have already run inside fetchPage's transaction(s).
	if p.IsUpdate() {
		n, err := pr.countAll()
		// DML never passes through Next, so its row outcome is the affected
		// count, not RowsReturned (RFC-211). Recorded before Close, which is
		// where the record is emitted.
		pr.execLog.setRows(0, n)
		pr.statsErr = err
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

	// Carried from cascadesPlan; revalidated at the top of every page's
	// transaction. Empty means the plan depends on no index.
	indexDependencies planIndexDependencies

	// execLog accumulates this statement's execution record (RFC-211) and is
	// emitted from Close, the one funnel every completion path reaches. nil
	// when no execution-stats logger is installed, and every method on it is
	// nil-safe.
	execLog *execLogScope

	// statsErr is the error the statement ended with, staged for the
	// execLog.finish that Close performs. It exists because Close is the
	// emission point but takes no error: Next records its own failures here,
	// and Execute's two early-return paths set it before closing. io.EOF is
	// never recorded — exhaustion is how a successful result set ends.
	statsErr error

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

	// statementTime is the statement-stable CURRENT_TIMESTAMP-family
	// instant, captured once in Execute while the statement's session-clock
	// stamp is live. Every fetchPage (including lazy pages fetched from
	// rows.Next after the driver call returned) builds its EvaluationContext
	// from THIS field so all pages of one statement observe one instant.
	statementTime time.Time

	// execState is the statement-wide RFC-130 ExecuteState (the memory byte
	// budget counter). Minted ONCE in Execute and shared across all pages —
	// each fetchPage rebuilds the cursor hierarchy but assigns this same
	// pointer into the page's ExecuteProperties.State, so the in-memory
	// buffering budget accumulates across the whole statement. Never nil.
	execState *recordlayer.ExecuteState

	// scratch is the statement-wide home for operator resume state too large
	// to ride every page's continuation (executor.ExecutionScratch). Minted
	// ONCE in Execute and stamped onto every page's EvaluationContext, exactly
	// as execState is stamped onto every page's ExecuteProperties. Never nil.
	scratch *executor.ExecutionScratch

	buf          [][]driver.Value
	bufPos       int
	continuation []byte
	exhausted    bool
	closed       bool
	fetchErr     error

	// tx is the explicit transaction that was open when Execute ran, or nil in
	// auto-commit mode. EVERY page of EVERY statement kind executes on it —
	// SELECT included, which is what gives an explicit transaction
	// read-your-writes and read conflict ranges (RFC-198 Decision 1; Java
	// reads through conn.getTransaction() at BackingRecordStore.java:235).
	// Captured HERE, at Execute time, rather than resolved per page: pages are
	// fetched after QueryContext returned, so resolving c.activeTx at fetch
	// time would let a result set whose transaction ended resume in a fresh
	// auto-commit transaction, silently (Decision 3). A dead captured
	// transaction is a loud 25F01 in runInCapturedTx.
	//
	// Routing through the transaction REMOVES automatic retry: DB.Run's retry
	// loop is not in this path, so a conflict reaches the application, which
	// re-runs the transaction — the driver cannot, because it does not hold
	// the statements the application has not issued yet.
	tx *embeddedTx

	// isUpdate is the statement-kind fact that used to share a field with the
	// routing decision above (`respectActiveTx`), conflating two independent
	// questions. It answers exactly one: is this a DML plan whose page scan
	// must never be bounded by the returned-row cap (pageRowBudget)? Keying
	// that off the routing field would silently unbound the page scan of every
	// in-transaction SELECT that sets MAX_ROWS (RFC-198 Decision 4).
	isUpdate bool
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
	// RFC-211: the statement is over, so its execution record goes out here.
	// Close is the single funnel — database/sql closes an exhausted or
	// abandoned result set, and Execute's error and DML paths close
	// explicitly — and execLogScope.finish is idempotent, so the repeated
	// Close database/sql may issue emits once. A DML statement recorded its
	// affected count in Execute; it never passes through Next, so r.emitted
	// would overwrite that with a zero.
	if !r.isUpdate {
		r.execLog.setRows(r.emitted, 0)
	}
	r.execLog.finish(r.statsErr)
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
		// RFC-211: stage whatever ended this statement for the record Close
		// emits. io.EOF is exhaustion, not failure. Registered in the SAME
		// defer as the panic recovery and AFTER it, so a recovered panic is
		// recorded as the statement's outcome too.
		if err != nil && err != io.EOF && r.statsErr == nil {
			r.statsErr = err
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
	if r.isUpdate { // DML (INSERT/UPDATE/DELETE): never bound the scan
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
	// Anchor the scan/time budget on the database's env clock. This path ALWAYS arms a time
	// limit (txPageTimeLimit below), and that limit decides where a page ends and therefore
	// which continuation the caller gets — so a wall-clock anchor would make a simulated run
	// page differently depending on how fast the machine was. A nil env (production) is the
	// wall clock, unchanged.
	props := recordlayer.DefaultExecutePropertiesIn(r.env())

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

	// Inside an explicit transaction the scan/time/byte counters are
	// TRANSACTION-scoped (RFC-198 Decision 5): every page of every statement
	// charges one shared ScanLimiterState held on the embeddedTx, so N pages
	// get one budget against FDB's 5-second wall instead of N fresh 4s
	// budgets — Java's transaction-scoped ExecuteState plus its
	// transactionCreateTime anchor, reproduced by one object. The state's
	// time anchor is corrected to the read-version instant by
	// preflightTxBudget before each page. Auto-commit keeps the fresh
	// per-page state DefaultExecutePropertiesIn minted above — a page IS a
	// transaction there. The armed record/byte limits are opt-in, so the
	// whole-transaction tightening reaches only callers who armed them
	// (Decision 5a).
	if r.tx != nil {
		props.ScanState = r.tx.scanStateIn(r.env())
	}

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
// Reachability: the out-of-band branch is LIVE, and its one producer is deliberate. Every Go LEAF cursor
// reports an out-of-band stop only after scanned>0 (key_value_cursor.go:164/174/181,
// record_key_cursor.go:64/69/78), at which point its continuation is set → a BytesContinuation; most
// composite cursors likewise carry a serialized BytesContinuation. The exception is positionReplayCursor
// (position_replay_cursor.go:130), which resumes by deterministic replay rather than by position and so
// reports a no-next out-of-band+START when it has emitted nothing yet — it has no partial position to
// hand back. Its two callers are the recursive-CTE DISTINCT shapes, whose whole contract is that they
// cannot paginate a partial traversal, so routing them to 54F01 here is the intended answer rather than
// an accident (pinned by TestFDB_TimeBudgetCeiling_RecursionErrorsNotPartial). Apart from that producer
// the only nil-bytes+non-end case is LIMIT 0. Deciding exhaustion from IsEnd rather than from bytes is
// what keeps the two apart.
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
// (RFC-162, decision (b)). A fixed [16]byte / tuple.UUID at this boundary
// is unambiguously a UUID: BYTES columns surface as a []byte slice, never a
// 16-array, so the type switch never misfires.
// A STRUCT column arrives here as the raw proto message the value layer
// carries (values.protoScalarToRowValue keeps nested non-UUID messages raw
// for further descent) and must NOT leave as one: Java hands a struct column
// to the client as a RelationalStruct, built at exactly this boundary —
// RowStruct.getObject's Types.STRUCT arm wraps the Message in an
// ImmutableRowStruct (RowStruct.java:184-197, :293-294). An ARRAY of structs
// is the same conversion per element (Java: getArray → the array's element
// materialization → getStruct).
func materializeDriverValue(v any) any {
	switch u := v.(type) {
	case [16]byte:
		return tuple.UUID(u).String()
	case tuple.UUID:
		return u.String()
	case rowstruct.TypedOrdinalRow:
		s, err := rowstruct.NewOrdinal(u)
		if err != nil {
			// Match the protobuf arm below: never discard the value when its
			// declared STRUCT shape is malformed. A valid record-valued ordinal
			// row becomes api.Struct; an invalid internal value stays visible as
			// itself rather than being guessed into a different public shape.
			return v
		}
		return s
	case protoreflect.ProtoMessage:
		s, err := rowstruct.New(u.ProtoReflect())
		if err != nil {
			// The descriptor could not be described as a struct type. Left
			// as the raw message rather than dropped: the row still carries
			// the value, and the client sees an unconverted message instead
			// of a silent NULL.
			return v
		}
		return s
	case []any:
		out := make([]any, len(u))
		for i, e := range u {
			out[i] = materializeDriverValue(e)
		}
		return out
	default:
		return v
	}
}

func (r *paginatingRows) fetchPage() error {
	c := r.conn

	// RFC-211 page/retry accounting. attempts counts CLOSURE ENTRIES, which is
	// what DB.Run's retry loop re-executes; everything past the first is a
	// retry. runInCapturedTx calls the closure directly for an explicit
	// transaction (no retry loop), so that path always contributes 0 — and if
	// the closure never runs at all (a terminated transaction), attempts stays
	// 0 and the subtraction below cannot go negative.
	r.execLog.addPage()
	attempts := 0

	// The page's OUTCOME is staged in locals and published to r only after the
	// page's transaction has succeeded.
	//
	// In auto-commit the closure below is the body of a DB.Run retry loop, so it
	// may run more than once, and it RESUMES FROM r.continuation while clearing
	// r.buf at its top. Any position it writes to r before its transaction
	// commits is therefore position a re-execution inherits — and a re-execution
	// that inherits an already-advanced continuation resumes PAST the rows the
	// failed attempt drained, whose buffer it has just discarded. Those rows are
	// silently gone: no error, no duplicate, a short result set.
	//
	// The invariant is the ordinary transactional one — a result set's position
	// advances with the transaction that produced it, or not at all — and it is
	// kept structurally, by the assignment site, rather than by argument about
	// which failures can reach it.
	//
	// It is REACHED, not merely defended against. A SELECT page is not
	// necessarily read-only: storeIn opens the record store per page and does
	// NOT SetSkipPossiblyRebuild, so every page runs checkPossiblyRebuild, which
	// WRITES the store header when it upgrades a below-current format version,
	// when the record-count key changed, or when the metadata version moved (that
	// arm rebuilds indexes inline). A store written by an older writer — Java at
	// a lower FormatVersion is the ordinary case — therefore makes the FIRST page
	// of a plain auto-commit SELECT a write-carrying transaction, whose commit
	// goes to the resolver and can come back not_committed. Before this staging,
	// that conflict cost the caller page 1's rows with no error raised.
	//
	// r.buf needs no staging on the SUCCESS path: the closure truncates it at the
	// top of every attempt, so a retry cannot see a previous attempt's rows. It
	// does need clearing on the FAILURE path — see the error branch below.
	var pageExhausted bool
	var pageCont []byte

	// The statement-wide ExecuteState carries one more piece of PAGE POSITION,
	// and it lives too deep in the executor to stage as an outcome: the
	// recursive-CTE level count. A resumed recursive cursor deliberately does not
	// reset it (newRecursiveUnionCursor resets only on a nil continuation, or a
	// cyclic CTE that pages mid-recursion would never trip its cap), so an
	// attempt that walks k levels and then fails leaves those k counted and the
	// re-execution walks the same k again. A finite CTE near the 1000-level cap
	// then fails 54F01 with a depth it never actually reached.
	//
	// So it is snapshotted here and rolled back at the top of every attempt —
	// the same treatment r.buf gets, and for the same reason. The rollback must
	// be INSIDE the closure: the retry loop re-enters the closure without ever
	// returning here, so an error-path rollback would never run.
	//
	// The state's other member, the memory budget, is deliberately NOT rolled
	// back: it gauges LIVE bytes with paired release on teardown, so a failed
	// attempt returns it to its entry value on its own. See
	// ExecuteState.SnapshotRecursionLevels for the full statement-cumulative vs
	// page-positional split and where a future member belongs.
	recursionAtPageStart := r.execState.SnapshotRecursionLevels()

	// Every statement kind joins the explicit transaction captured at Execute
	// time (r.tx) — SELECT included, which is what gives an explicit
	// transaction read-your-writes and read conflict ranges (RFC-198
	// Decision 1). With no explicit transaction (r.tx == nil) each page runs
	// in its own auto-commit transaction via DB.Run, unchanged. A captured
	// transaction that has since ended is a loud 25F01, never a silent fresh
	// transaction (Decision 3).
	_, txErr := c.runInCapturedTx(r.ctx, r.tx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		attempts++
		if r.tx != nil {
			if err := r.preflightTxBudget(rctx); err != nil {
				return nil, err
			}
		}
		r.buf = r.buf[:0]
		r.bufPos = 0
		r.execState.RestoreRecursionLevels(recursionAtPageStart)
		// Fresh per-page scratch bookkeeping, for the same reason the recursion
		// levels are restored above: this closure is the FDB retry loop's body
		// and runs again from the UNCHANGED r.continuation after a retryable
		// error, so a failed attempt's adoptions must not survive into its
		// retry.
		r.scratch.BeginPage()

		// One store per subspace per transaction, reused by every page
		// (RFC-198 Decision 10) — in auto-commit this still builds a fresh
		// store per page, because there each page IS a transaction.
		store, storeErr := c.storeIn(rctx, r.tx, r.ss)
		if storeErr != nil {
			return nil, storeErr
		}
		// The plan's index-state dependency is checked HERE, inside the page's
		// own transaction and before any row is produced — Java's continuation
		// plan constraint position (QueryPlan.java:667,726-735). Doing it per
		// PAGE rather than once per statement is what makes a resumed page
		// safe: an auto-commit statement's pages are separate transactions, so
		// a transition between them would otherwise be invisible.
		//
		// Validated against the STORE's metadata, not the plan's: execution
		// opens the store with the connection's current metadata, so this is
		// where an index dropped or redefined since planning is observable.
		if stateErr := validatePlanIndexDependencies(
			r.indexDependencies, store.GetRecordMetaData(), store.GetAllIndexStates(),
		); stateErr != nil {
			return nil, stateErr
		}

		evalCtx := executor.EmptyEvaluationContext().
			WithStatementTime(r.statementTime).
			WithExecutionScratch(r.scratch)
		// The statement-stable CURRENT_TIMESTAMP-family instant was stamped
		// ONCE in Execute, from the session clock (Session.BeginStatement /
		// StatementNow — the same authority the INSERT…VALUES fold reads),
		// and is carried on paginatingRows: every row of every PAGE of the
		// main plan AND of every subquery observes the same instant, per SQL.
		// Never read the session clock here — lazy pages run after the
		// driver call's deferred clock-restore.
		// Compute the statement's execution props BEFORE evaluating scalar
		// subqueries so the configured scan limits apply to them too
		// (RFC-106a): an uncorrelated subquery must not scan past the statement
		// cap while the outer plan would fail/paginate. (The statement timeout
		// already reaches them via r.ctx.)
		props := r.executeProps()
		// RFC-211: charge this ATTEMPT's scan consumption to the statement's
		// stats, as a DELTA rather than an absolute read.
		//
		// The delta is not defensive bookkeeping — the two lifetimes demand
		// it. In auto-commit executeProps mints a fresh ScanLimiterState per
		// call, so the entry counts are 0 and delta == absolute. Inside an
		// explicit transaction it assigns the TRANSACTION-scoped state
		// (RFC-198 Decision 5), which is cumulative across every page of every
		// statement — reading the absolute counter per page there would
		// re-charge all previous pages on each one.
		//
		// The defer is what makes the error path honest: it fires on EVERY
		// exit from this attempt, including the 54F01 a scan limit raises and
		// a retryable failure that discards the attempt. So a statement killed
		// by EXECUTION_SCANNED_ROWS_LIMIT still reports what it consumed, and
		// a retried page reports both attempts — the cluster served both. This
		// is Java's guarantee too: the limiter object lives on the caller's
		// ExecuteState and outlives ScanLimitReachedException, so
		// getRecordsScanned() still reads after the trip (ExecuteState.java:114).
		scanRecordsAtEntry := int64(props.ScanState.RecordsScanned())
		scanBytesAtEntry := props.ScanState.BytesScanned()
		defer func() {
			r.execLog.addScanned(
				int64(props.ScanState.RecordsScanned())-scanRecordsAtEntry,
				props.ScanState.BytesScanned()-scanBytesAtEntry)
		}()
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
			row, materializeErr := materializePageRow(rs, r.cols, r.isUpdate)
			if materializeErr != nil {
				return nil, materializeErr
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
		// Staged, NOT published: pageExhausted/pageCont are locals that the
		// caller copies onto r only after the transaction succeeded. See the
		// comment above the declarations.
		pageExhausted, pageCont = exhausted, contBytes
		return nil, nil
	})

	// RFC-211: every closure entry past the first was a retry.
	r.execLog.addRetries(attempts - 1)

	if txErr != nil {
		// The page did not happen, so its partial rows are not results. They are
		// still sitting in r.buf with bufPos at 0, and nextRow SERVES THE BUFFER
		// BEFORE it consults r.exhausted or r.fetchErr — so a caller that reaches
		// nextRow again would be handed rows from a transaction that never
		// committed, as though they were a page.
		//
		// Nothing reaches it today only because database/sql stops iterating at
		// the first error. That is a property of the CALLER, not of this loop,
		// and it is not something reordering the checks in nextRow would fix:
		// buffered rows must not outlive the transaction that produced them, and
		// dropping them here is what makes that true.
		r.buf = r.buf[:0]
		r.bufPos = 0
		return translateExecErrorCtx(r.ctx, txErr)
	}
	// The page's transaction committed. Only now does the result set's position
	// move — this is the assignment that must not happen anywhere else.
	r.exhausted = pageExhausted
	r.continuation = pageCont
	// Retiring scratch entries is statement-scoped state moving, so it belongs
	// HERE with the position and nowhere earlier. Inside the closure it would
	// run on an attempt whose transaction can still fail: the retry re-executes
	// from the UNCHANGED r.continuation, and entries this attempt judged
	// unreachable are exactly the ones that continuation may name. What makes
	// the judgement sound is that r.continuation has now advanced, so the
	// entries only the PREVIOUS one could name are genuinely unreachable.
	r.scratch.SweepAfterPage(pageExhausted)
	return nil
}

// materializePageRow converts one executor row into the public SELECT row.
// DML result rows are instead an executor-private mutation echo (UPDATE is
// exact {OLD, NEW}; INSERT/DELETE have their corresponding internal carriers).
// Embedded SQL exposes only their count, never those columns, so the DML arm
// deliberately consumes the cursor row without consulting ResultSet.Object.
// Asking the SELECT adapter to align an UPDATE echo to the target table's
// public columns made a successful write fail afterwards with "no positional
// output row aligned to column ID".
func materializePageRow(
	rs *executor.RecordLayerResultSet,
	cols []executor.ColumnDef,
	isUpdate bool,
) ([]driver.Value, error) {
	if isUpdate {
		return []driver.Value{}, nil
	}
	row := make([]driver.Value, len(cols))
	for i := range row {
		v, err := rs.Object(i + 1)
		if err != nil {
			return nil, err
		}
		row[i] = materializeDriverValue(v)
	}
	return row, nil
}

// preflightTxBudget enforces the whole-transaction time budget before a page
// runs (RFC-198 Decisions 5 and 6, interim state). The budget is anchored on
// the CLIENT'S READ-VERSION INSTANT — when FDB's 5-second MVCC window actually
// opened — never on statement start (the refuted proxy: a first statement need
// not take a read version at all) and never on BeginTx (an idle transaction
// has no window yet).
//
// Three arms:
//   - the backend cannot report the instant (the cgo escape hatch has no such
//     accessor), or no read version has been taken yet: no window is open, the
//     page proceeds on the fresh statement-anchored budget;
//   - the window is open and the budget remains: re-anchor the shared
//     transaction ScanLimiterState on the instant so every leaf cursor's
//     elapsed measurement counts from the true window start (idempotent
//     between GRVs), and proceed;
//   - the budget is exhausted: fail 40001 NAMING THE 5-SECOND WINDOW. It is
//     40001 and not 54F01 because no limit the caller can raise makes an FDB
//     transaction live longer than five seconds, and it is the same code
//     translateFDBCode assigns to FDB's own transaction_too_old (1007), so
//     retry logic is uniform whether the driver pre-empts or FDB does.
//     Pre-empting here also keeps the zero-progress liveness tripwire honest:
//     without it an exhausted budget produces a rowless unadvanced page and a
//     54F01 telling the user to raise limits that are not the problem.
//
// The code being SHARED with a genuine conflict is what makes the retry logic
// uniform, and it is also what makes the code alone insufficient to identify this
// condition — a conflict is retried as-is and usually succeeds, an exhausted
// window is retried identically forever. So the error carries a typed cause,
// api.TransactionTimeLimitError, which is what code matches; the message is for
// the human. Because the driver pre-empts at four seconds, this — not FDB's own
// 1007 — is the carrier a caller inside an explicit transaction actually sees when
// the MVCC window is spent.
//
// INTERIM, by RFC-198 Decision 6: the end state is Java's clean stop — a
// transaction-bound continuation with reason TRANSACTION_LIMIT_REACHED and the
// transaction left open — which needs RFC-203's in-transaction continuation
// surface (OptContinuation is still rejected at this head). The stated
// retirement condition: when RFC-203's G12a/G12b land, this error becomes a
// boundary and the test written for it is rewritten, not deleted.
func (r *paginatingRows) preflightTxBudget(rctx *recordlayer.FDBRecordContext) error {
	rep, ok := rctx.Transaction().(fdb.ReadVersionInstantReporter)
	if !ok {
		return nil
	}
	instant, ok := rep.ReadVersionInstant()
	if !ok {
		return nil
	}
	env := r.env()
	r.tx.scanStateIn(env).AnchorAt(instant)
	if elapsed := env.Since(instant); elapsed >= txPageTimeLimit {
		return api.NewTransactionTimeLimitError(elapsed, txPageTimeLimit)
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
	// WrapError (not NewError) so *recordlayer.ScanLimitReachedError survives as
	// the api.Error's Cause — errors.As can then distinguish an ACTUAL scan/
	// byte/time limit hit from the unrelated liveness-tripwire 54F01
	// (pageContinuationState's caller, "query cannot progress...") that also
	// carries this same SQLSTATE but is a plain api.NewError with no Cause.
	// The rendered message is unchanged in substance — scanLimit.Error() is
	// still fully present, now as the Cause suffix instead of the sole Message.
	var scanLimit *recordlayer.ScanLimitReachedError
	if errors.As(err, &scanLimit) {
		return api.WrapError(api.ErrCodeExecutionLimitReached, "leaf cursor scan limit exceeded", scanLimit)
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
	// Sparse/filtered indexes are incomplete by definition. Planning excludes
	// them until predicate implication is implemented; the executor repeats the
	// invariant for hand-built or stale plans and this arm makes the decline a
	// stable, user-facing unsupported-query error rather than a generic failure.
	var filteredIndexPlan *executor.FilteredIndexPlanError
	if errors.As(err, &filteredIndexPlan) {
		return api.NewError(api.ErrCodeUnsupportedQuery, filteredIndexPlan.Error())
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
	// FDB tail: page ≥ 2 errors reach the application through THIS function,
	// not through the QueryContext/ExecContext translateFDBError wrap (which
	// only sees the eagerly-fetched first page). An in-transaction page fetch
	// has no DB.Run retry loop to absorb FDB codes, so 1020/1007/1025/1021
	// would otherwise escape raw (RFC-198 criterion 9). translateFDBError is
	// idempotent on *api.Error, so double translation is harmless.
	return translateFDBError(err)
}

// fetchTableStatistics reads per-record-type row counts from FDB using a
// read-only snapshot transaction. Returns nil (use defaults) on any error —
// statistics are best-effort; a failed stats read should never prevent
// query planning.
//
// DEAD ON EVERY PRODUCTION PATH for SQL-created schemas: relational
// templates carry NO record count key — Java's RecordMetadataSerializer
// never sets one (the stored bytes must match Java's, and Java core marks
// getRecordCountKey @API(DEPRECATED), superseded by COUNT-type indexes) —
// so the countKey==nil arm below returns nil and the cost model runs on
// defaults. The function is kept because it is still live for metadata
// that DOES carry a RecordTypeKeyExpression count key (hand-built stores,
// Java-authored legacy metadata opened through this engine). The
// Java-sanctioned replacement — COUNT-type index reads +
// CardinalitiesProperty — is booked in TODO.md.
//
// Only returns real statistics when the metadata uses RecordTypeKeyExpression
// as the count key. For an EmptyKey count key, per-type counts are
// unavailable — returns nil rather than fabricating an equal distribution
// that would mislead the cost model.
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

// fetchIndexStateSnapshot opens the record store and returns the state of every
// index the METADATA names, with an index carrying no stored state defaulted to
// READABLE — Java's RecordStoreState default. The domain is the metadata's
// index set, deliberately and load-bearingly so; see the invariant at the
// GetAllIndexStates call below. A nil map means there is no authoritative
// snapshot (offline planning, or a schema with no indexes at all).
//
// This is the ONE store open on the planning path, shared by the readable-index
// view and the plan's index-state signature. Both must come from the same read:
// two reads could straddle a transition and disagree, and a view that disagrees
// with the signature guarding it is worse than either alone.
//
// WHERE THE STATE COMES FROM, and why it must be an OPENED store. Java reads
// the state off a store that is already open: PlanContext.Builder.fromRecordStore
// (PlanContext.java:249-252) passes recordStore.getRecordStoreState(), and the
// store reached that call through checkVersion, which has already reconciled
// every index added since the header's recorded metadata version
// (FDBRecordStore.checkRebuildIndexes, FDBRecordStore.java:4743-4767). So in
// Java the planner never sees an index whose state is still undecided.
//
// Reading the index-state subspace directly instead — cheaper — reproduces
// Java's answer only for indexes the metadata has already been reconciled
// against. An index added by a metadata EVOLUTION has no state key at all until
// some store open writes one, and "no stored state" means READABLE, so the
// planner would hand the query an index that holds no entries. Opening the
// store makes that structurally impossible.
//
// It also runs through the same storeIn as execution, so an explicit
// transaction reuses one open store across planning and every page rather than
// opening a second one.
//
// A FAILED OPEN IS AN ERROR, not a best-effort empty like fetchTableStatistics.
// It costs no availability: fetchPage opens this exact store in this exact way
// before it can return a row, so a store that cannot be opened here is a query
// that fails one step later regardless. Planning against a GUESSED all-readable
// state is precisely what the signature exists to prevent, so guessing it here
// and then validating the guess downstream would be incoherent.
func (g *cascadesGenerator) fetchIndexStateSnapshot(
	ctx context.Context,
	md *recordlayer.RecordMetaData,
) (map[string]recordlayer.IndexState, error) {
	c := g.c
	// planSelectCascades is also the package's DB-less planning harness entry
	// point. The public live SELECT/DML routes reject or divert before reaching
	// it when no DB exists, so nil here is an explicitly offline convention,
	// never a production fallback after an FDB failure.
	if c == nil || c.sess == nil || c.sess.DB == nil || md == nil {
		return nil, nil
	}
	// No indexes means no index-state dependency and nothing to restrict, so
	// the open is skipped entirely — the healthy fast path this had before.
	if len(md.GetAllIndexes()) == 0 {
		return nil, nil
	}
	ss, err := c.sess.Keyspace.SchemaSubspace(c.sess.DBPath, c.sess.Schema)
	if err != nil {
		return nil, err
	}
	result, runErr := c.runInCapturedTx(ctx, c.activeTx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		store, storeErr := c.storeIn(rctx, c.activeTx, ss)
		if storeErr != nil {
			return nil, storeErr
		}
		// GetAllIndexStates, NOT GetAllIndexStatesMap. The two answer in
		// different DOMAINS: this one iterates the METADATA's indexes and
		// defaults an absent entry to READABLE; the raw map returns whatever
		// keys the index-state subspace happens to hold, including a key for a
		// name the metadata no longer has.
		//
		// THE INVARIANT: the signature comparison is ONE function evaluated
		// TWICE — here and again at execution — never two functions that
		// usually agree. Two domains that agree on healthy stores is not a
		// weaker version of that property, it is a different property, and the
		// disagreement is unbounded in consequence: a single stray state key
		// for a dropped index appears on the planning side and never on the
		// execution side, so EVERY query fails 40001, forever. 40001 tells the
		// client to retry, and the replan re-derives the same mismatch.
		//
		// Java scopes its equivalent to metadata objects for the same reason —
		// DatabaseObjectDependenciesPredicate.eval walks the plan's used
		// indexes and asks recordMetaData.hasIndex first
		// (DatabaseObjectDependenciesPredicate.java:90-101). Storage that
		// metadata does not name is not part of the dependency.
		return store.GetAllIndexStates(), nil
	})
	if runErr != nil {
		return nil, runErr
	}
	states, ok := result.(map[string]recordlayer.IndexState)
	if !ok || states == nil {
		return nil, errors.New("embedded planner: record store returned no index-state snapshot")
	}
	return states, nil
}

// readableIndexesFrom turns an index-state snapshot into the planner's
// allow-list of scannable index names. Port of Java's
// PlanContext.Builder.getReadableIndexes (PlanContext.java:236-247):
//
//	if (storeState.allIndexesReadable()) return Optional.empty();
//	else return Optional.of(metaData.getAllIndexes().stream()
//	        .filter(storeState::isReadable)
//	        .filter(index -> !universalIndexes.contains(index))
//	        .map(Index::getName).collect(...));
//
// Two properties are load-bearing and both are Java's:
//
//   - The common case is UNRESTRICTED. When every index is readable the answer
//     is "no allow-list", not "an allow-list naming everything", so a healthy
//     store plans through exactly the code path it always did and the plan
//     cache is not keyed on a set that never varies.
//   - READABLE is strict. Java filters on `isReadable()`, not `isScannable()`,
//     so READABLE_UNIQUE_PENDING is excluded even though it can be scanned:
//     the planner may assume uniqueness from a unique index, and a
//     unique-pending one has not yet proven it.
//
// Only the path that actually FETCHED a snapshot and found every named index
// READABLE may mint the affirmative AllIndexesReadable form. The degenerate
// early returns below mint UNKNOWN instead: an affirmative claim manufactured
// from the absence of information is the same collapse ReadableIndexes' third
// state exists to prevent, one layer up. Neither changes a plan — UNKNOWN is
// equally permissive for scanning — but a downstream proof that asks "was
// index state established?" must not be answered yes by a nil snapshot.
//
// A nil snapshot is the offline convention and yields UNKNOWN.
func readableIndexesFrom(
	md *recordlayer.RecordMetaData,
	states map[string]recordlayer.IndexState,
) cascades.ReadableIndexes {
	if md == nil || len(states) == 0 {
		return cascades.IndexStatesUnknown()
	}
	allIndexes := md.GetAllIndexes()
	if len(allIndexes) == 0 {
		return cascades.IndexStatesUnknown()
	}
	allReadable := true
	for name := range allIndexes {
		if st, stored := states[name]; stored && st != recordlayer.IndexStateReadable {
			allReadable = false
			break
		}
	}
	if allReadable {
		return cascades.AllIndexesReadable()
	}
	readable := make(map[string]struct{}, len(allIndexes))
	for name := range allIndexes {
		if st, stored := states[name]; stored && st != recordlayer.IndexStateReadable {
			continue
		}
		readable[name] = struct{}{}
	}
	return cascades.OnlyReadableIndexes(readable)
}

// buildCascadesPlanContext builds the plan context for one planning run. cfg
// carries the option-driven PlannerConfiguration (see plannerOptions); pass
// cascades.DefaultPlannerConfiguration() where no options apply. It is a
// parameter rather than a constant read inside the context so that
// PLAN_RIGHT_DEEP reaches PartitionSelectRule, which consults the
// configuration through PlanContext and has no other route to it.
//
// A nil md still yields a metadataPlanContext (every accessor handles nil
// md): an EmptyPlanContext would silently discard cfg.
func buildCascadesPlanContext(
	md *recordlayer.RecordMetaData,
	cfg cascades.PlannerConfiguration,
) cascades.PlanContext {
	return &metadataPlanContext{md: md, cfg: cfg}
}

type metadataPlanContext struct {
	md  *recordlayer.RecordMetaData
	cfg cascades.PlannerConfiguration
	// The readable-index view lives on cfg (cascades.PlannerConfiguration).
	// Its zero value is UNRESTRICTED, which is what offline and unit planning
	// get; live SQL planning resolves it from the store's index states before
	// the plan-cache key is built, so a non-readable index never reaches
	// buildMatchCandidates.
	candidatesOnce sync.Once
	candidates     []cascades.MatchCandidate
}

func (c *metadataPlanContext) GetPlannerConfiguration() cascades.PlannerConfiguration {
	return c.cfg
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
		pkCols, keyTypes := primaryCandidateKeyComponents(rt)
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
			// Metadata-aware layout: version-storing stores extend every row
			// with the trailing __ROW_VERSION pseudo-slot (Java's
			// RecordMetaData.getPlannerType, RecordMetaData.java:732-739).
			flowed = executor.PositionalTypeForRecordLayout(rt.Descriptor, c.md.IsStoreRecordVersions())
		}
		primaryCandidate := cascades.NewPrimaryScanMatchCandidate(
			nil,
			aliases,
			allTypeNames,
			[]string{rt.Name},
			upperPK,
			rt.PrimaryKeyHasRecordTypePrefix(),
			flowed,
		)
		primaryCandidate.WithKeyComponentTypes(keyTypes)
		candidates = append(candidates, primaryCandidate)
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
		// An index the store cannot READ must not become a match candidate.
		// This is Java's MetaDataPlanContext.forRootReference filter
		// (MetaDataPlanContext.java:194-199,
		// `indexList.removeIf(index -> !allowedIndexes.contains(index.getName()))`),
		// applied at the same place: on the index list, BEFORE any candidate is
		// created, so no downstream rule has to remember to ask.
		//
		// Without it a WRITE_ONLY or DISABLED index is planned into a scan that
		// then fails at execution with IndexNotReadableError — a query that
		// errors where a correct, slower plan was available. Java has never had
		// that hole; Go's PlannerConfiguration simply carried no readable-index
		// view until now (planner_options.go recorded the gap).
		if !c.cfg.ReadableIndexes.Allows(idx.Name) {
			continue
		}
		// Sparseness is asked here too, ahead of the candidate boundary, and it
		// must be the SAME question that boundary answers: an index whose stored
		// predicate provably rejects nothing holds an entry for every record, so
		// its aggregates cover the whole table and the family suppression below
		// has nothing to protect against.
		//
		// Ask recordlayer.Index rather than reading the predicate proto here.
		// The two agree today on everything this path can see, and the reason is
		// worth stating because it is the only thing making them agree: an index
		// can carry a predicate as a serialized proto OR as a programmatic Go
		// closure, but SetPredicateProto publishes BOTH representations at once
		// (index.go), so metadata loaded from a store never presents a
		// closure-only predicate. Deriving sparseness from the proto alone was
		// therefore correct by a property of the loader, not by anything stated
		// here.
		//
		// It is not a property worth depending on. A closure-only predicate is
		// assignable through the record-layer Go API, and reading such an index
		// as DENSE would hand it an aggregate candidate that ignores the filter
		// — an aggregate over the rows the index happens to contain, reported as
		// the aggregate over the group. That is a wrong answer, not a missed
		// optimization, and it is one field assignment away. HasFilteringPredicate
		// covers both representations and treats a proved tautology as
		// non-filtering; a closure is opaque and cannot be proved tautological,
		// so it fails closed.
		sparse := idx.HasFilteringPredicate()
		if !sparse {
			// A SPARSE aggregate/vector index must not become a candidate that
			// ignores its predicate: the maintained aggregates cover only the
			// predicate-matching records, so serving a whole-table aggregate
			// from them returns wrong values. The aggregate/vector candidate
			// builders carry no predicate arm yet, so a sparse index of those
			// families gets NO candidate and the query falls back to the
			// base-scan aggregate — correct, if slower. The value-index path
			// below DOES thread the predicate (IndexDefWithPredicate), where
			// the expansion attaches it to the candidate graph
			// (ValueIndexExpansionVisitor.java:138-162).
			if aggCand := tryAggregateIndexCandidate(idx, c.md); aggCand != nil {
				candidates = append(candidates, aggCand)
				continue
			}
			if vecCand := tryVectorIndexCandidate(idx, c.md); vecCand != nil {
				candidates = append(candidates, vecCand)
				continue
			}
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
		if sparse && idx.Type != recordlayer.IndexTypeValue && idx.Type != recordlayer.IndexTypeVersion {
			// A sparse non-value index (e.g. a filtered PERMUTED_MIN/MAX)
			// must not degrade into a value-scan candidate either — no
			// candidate at all is the safe failure mode.
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

// runFromResolutionPostPasses is the SINGLE ordered sequence of mandatory
// FROM-resolution post-passes over a freshly built logical plan. It has
// exactly two callers — the production query path (buildCascadesPlan) and the
// AS-SELECT index DDL front end (parseAsSelectIndexDefinition), which must
// stay behaviourally identical (RFC-202 D4: the index's SELECT is planned by
// the ordinary front end). Add a sixth pass HERE, never at one call site —
// a pass added to only one side silently forks the two pipelines.
//
// schema is the resolution schema (the session's for queries,
// defaultEmbeddedSchema for template DDL); md the metadata the plan was built
// against; unnestMD the metadata for unnest-alias validation (the session's
// cached metadata in production; the same md for DDL).
func runFromResolutionPostPasses(logicalOp logical.LogicalOperator, schema string, md, unnestMD *recordlayer.RecordMetaData) error {
	// Java's generateAccess resolves a FROM identifier as a CTE/table/view/
	// function BEFORE treating it as a correlated array field. The parser, which
	// has no metadata, may classify a schema-qualified table (`FROM PA AS s,
	// s.PB`, where the alias `s` also equals the schema name) as a lateral
	// unnest; demote it back to a table scan so the table branch wins (or reject
	// AT-on-a-table with WRONG_OBJECT_TYPE). RFC-142.
	if err := demoteSchemaQualifiedUnnest(logicalOp, schema, md); err != nil {
		return err
	}
	// Backstop for AT-on-a-table sources (`FROM t, U AT O`, present-scalar field,
	// …) that the per-FROM-scope early pass in VisitQuery cannot reach — namely an
	// AT-on-table inside an EXISTS / scalar subquery, whose plan is attached to the
	// tree only after VisitQuery returns. Run before validateTablesAndColumns so the
	// WRONG_OBJECT_TYPE is not masked by a column-validation error. RFC-142.
	if err := rejectAtOrdinalityOnTable(logicalOp, md); err != nil {
		return err
	}
	// Reject a lateral unnest's AS/AT alias colliding with ANY other FROM-source
	// alias (earlier OR later) in the same scope — the later-source collision the
	// translator's bottom-up lowering cannot see (`FROM T1, T1.arr AS V, U AS V`).
	// Run before column resolution so the duplicate-alias error is not masked.
	// RFC-142.
	if err := rejectDuplicateUnnestAlias(logicalOp, unnestMD); err != nil {
		return err
	}
	if err := resolveQualifiedTableNames(logicalOp, schema); err != nil {
		return err
	}
	return validateTablesAndColumns(logicalOp, md)
}

type metadataIndexDef struct {
	idx *recordlayer.Index
	md  *recordlayer.RecordMetaData
}

// recordTypes returns the metadata association when the index is registered.
// A Java-authored Index can also be supplied directly while its RecordMetaData
// contains exactly one record type (the deserialization/candidate-construction
// boundary exercised by RFC-202). In that unambiguous case the sole record
// type is the index's carrier; declining merely because the Go metadata map
// has not registered the detached Index would make a valid persisted covering
// index unreadable. Multi-type metadata still fails closed.
func (d *metadataIndexDef) recordTypes() []*recordlayer.RecordType {
	if d == nil || d.idx == nil || d.md == nil {
		return nil
	}
	if associated := d.md.RecordTypesForIndex(d.idx); len(associated) > 0 {
		return associated
	}
	all := d.md.RecordTypes()
	if len(all) != 1 {
		return nil
	}
	for _, recordType := range all {
		return []*recordlayer.RecordType{recordType}
	}
	return nil
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
	// The fallback must respect the covering split too: FieldNames delegates
	// through a KeyWithValue root into the FULL inner key, so an unguarded
	// fallback re-arms the wrong-column-set defect (RFC-202 D10(a)) for
	// exactly the roots the structured walk declined. ColumnSize() IS the
	// split point for a KeyWithValueExpression; nested leaves make the name
	// count exceed the column count, in which case truncation would be a
	// guess — return the untruncated list and let the candidate's
	// flat-descriptor check decline it (NewPlanContextFromIndexDefs refuses
	// nested-leaf roots outright before that).
	names := d.idx.RootExpression.FieldNames()
	if kwv, ok := d.idx.RootExpression.(*recordlayer.KeyWithValueExpression); ok {
		inner := kwv.InnerKey()
		if len(names) == inner.ColumnSize() && kwv.SplitPoint() <= len(names) {
			return names[:kwv.SplitPoint()]
		}
	}
	return names
}

// IndexValueColumnNames returns the covering-only (FDB VALUE part) column
// names of a KeyWithValue root — inner columns past the split point — and nil
// for every other root. Implements cascades.IndexDefWithValueColumns: these
// columns are available to covering translation but are never sargable and
// never order the scan (the entry key ends at the split point + primary key).
// Java models the same split as the expansion's valueValues list
// (ValueIndexExpansionVisitor.java:109-121).
func (d *metadataIndexDef) IndexValueColumnNames() []string {
	kwv, ok := d.idx.RootExpression.(*recordlayer.KeyWithValueExpression)
	if !ok {
		return nil
	}
	root := d.idx.RootExpression.ToKeyExpression()
	if root == nil || root.KeyWithValue == nil {
		return nil
	}
	names, okNames := indexKeyColumnNames(root.KeyWithValue.GetInnerKey())
	if !okNames || kwv.SplitPoint() > len(names) {
		return nil
	}
	return names[kwv.SplitPoint():]
}

func (d *metadataIndexDef) IndexIsUnique() bool { return d.idx.IsUnique() }

// IndexPredicateProto exposes the sparse-index predicate to the candidate
// builder (cascades.IndexDefWithPredicate): the expansion attaches it to the
// candidate graph so a query never matches the filtered index as if it were
// full (ValueIndexExpansionVisitor.java:138-162). Nil for a full index.
//
// Handed over RAW: normalization and the tautology classification belong to the
// candidate boundary (ValueIndexScanMatchCandidate.WithPredicateProto), which
// every producer of a candidate goes through — this adapter is only one of
// them, and normalizing here would leave the direct WithPredicateProto callers
// classified differently.
func (d *metadataIndexDef) IndexPredicateProto() *gen.Predicate {
	return d.idx.GetPredicateProto()
}

// IndexHasOpaqueFilter reports the case IndexPredicateProto structurally cannot:
// the index FILTERS, but through a Go closure with no serialized form, so there
// is no proto to hand over and nothing the matcher could account for.
//
// Both answers come from the same index and they disagree exactly here — proto
// nil, filter present. Sparseness is the FACT (HasFilteringPredicate, which the
// candidate loop above already consults for the aggregate/vector families);
// predicateProto is one REPRESENTATION of it. Reading the second as the first
// makes a closure-filtered index look complete, which is a wrong-rows failure:
// its UNIQUE declaration would license a DISTINCT elision covering records it
// never held, and a scan over it would stand in for a base table it does not
// cover.
func (d *metadataIndexDef) IndexHasOpaqueFilter() bool {
	return d.idx.HasFilteringPredicate() && d.idx.GetPredicateProto() == nil
}

// IndexKeyComponentTypes derives one authoritative physical tuple type per
// index-key component across every record type served by the index.
func (d *metadataIndexDef) IndexKeyComponentTypes() []values.Type {
	return physicalKeyComponentTypes(d.idx.RootExpression, d.recordTypes())
}

// IndexPrimaryKeyComponentTypes derives authoritative carriers aligned with
// IndexPrimaryKeyColumns. The index entry appends the trimmed PK after its own
// key; these types prevent raw FLOAT/DOUBLE NaN ordering from being advertised
// through that suffix.
func (d *metadataIndexDef) IndexPrimaryKeyComponentTypes() []values.Type {
	pkCols := d.IndexPrimaryKeyColumns()
	if len(pkCols) == 0 {
		return nil
	}
	unknown := unknownPhysicalTypes(len(pkCols))
	rts := d.recordTypes()
	if len(rts) == 0 {
		return unknown
	}
	leadingRecordTypeKey := false
	for _, rt := range rts {
		flatColumns, hasRecordTypeKey, ok := coveredPrimaryKeyColumns(rt)
		if !ok || len(flatColumns) != len(pkCols) {
			return unknown
		}
		if rt == rts[0] {
			leadingRecordTypeKey = hasRecordTypeKey
		} else if hasRecordTypeKey != leadingRecordTypeKey {
			return unknown
		}
		for i := range flatColumns {
			if !strings.EqualFold(flatColumns[i], pkCols[i]) {
				return unknown
			}
		}
	}
	// A leading RecordTypeKey is physically appended before the visible PK
	// fields. It is harmless for one type-specific index because it is constant
	// throughout that stream. In a shared index it partitions the stream by
	// type before the visible fields, so those fields are not globally ordered.
	if leadingRecordTypeKey && len(rts) != 1 {
		return unknown
	}
	// The planner trims the visible PK name list against visible index names.
	// Require that selection to match Index.TrimPrimaryKey's physical position
	// map exactly; otherwise an untrimmed hidden/function coordinate could be
	// skipped and a later field falsely advertised as ordered.
	positions := d.idx.PrimaryKeyComponentPositions()
	physicalOffset := 0
	if leadingRecordTypeKey {
		physicalOffset = 1
	}
	physicalSize := physicalOffset + len(pkCols)
	if positions != nil && len(positions) != physicalSize {
		return unknown
	}
	if positions != nil {
		indexSize := d.idx.RootExpression.ColumnSize()
		for _, position := range positions {
			if position >= indexSize {
				return unknown
			}
		}
	}
	actualSuffix := make([]string, 0, len(pkCols))
	for i, column := range pkCols {
		if positions == nil || positions[physicalOffset+i] < 0 {
			actualSuffix = append(actualSuffix, column)
		}
	}
	nameTrimmed := plans.TrimmedPKSuffix(d.IndexColumnNames(), pkCols)
	if len(actualSuffix) != len(nameTrimmed) {
		return unknown
	}
	for i := range actualSuffix {
		if !strings.EqualFold(actualSuffix[i], nameTrimmed[i]) {
			return unknown
		}
	}
	physicalTypes := physicalKeyComponentTypes(rts[0].PrimaryKey, rts)
	if len(physicalTypes) != physicalSize {
		return unknown
	}
	return append([]values.Type(nil), physicalTypes[physicalOffset:]...)
}

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
	case *recordlayer.FunctionKeyExpression:
		// An order-function wrapper (order_desc_nulls_last, …) is a
		// single-column key whose entry bytes are the TupleOrdering encoding;
		// the tag tells the candidate its Value is
		// ToOrderedBytesValue(field, direction) rather than a plain field
		// (Java: OrderFunctionKeyExpression.toValue,
		// OrderFunctionKeyExpression.java:99-103). An unrecognized function
		// stays "" — reported as a plain field, and the candidate's
		// flat-descriptor check declines the mismatch fail-closed.
		if _, isOrder := cascades.OrderFunctionDirection(e.Name()); isOrder {
			return []string{e.Name()}
		}
		n := e.ColumnSize()
		if n == 0 {
			n = 1
		}
		return make([]string, n)
	case *recordlayer.KeyWithValueExpression:
		// Tags stay parallel to IndexColumnNames — the KEY part only.
		tags := indexColumnFunctionTags(e.InnerKey())
		if e.SplitPoint() <= len(tags) {
			return tags[:e.SplitPoint()]
		}
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
		// The NullableArrayWrapper hop is storage-only: field(X).nest(
		// field("values", FAN_OUT/CONCATENATE)) is the stored spelling of the
		// wrapped nullable array column X — the LOGICAL key column is X, not
		// "values" (Java collapses the hop in
		// KeyExpressionExpansionVisitor via NullableArrayTypeUtils.matchArrayWrapper).
		if p := expression.Nesting.GetParent(); p != nil && p.GetFanType() == gen.Field_SCALAR {
			if cf := expression.Nesting.GetChild().GetField(); cf != nil &&
				cf.GetFieldName() == values.WrappedArrayValuesFieldName &&
				(cf.GetFanType() == gen.Field_FAN_OUT || cf.GetFanType() == gen.Field_CONCATENATE) {
				return []string{p.GetFieldName()}, true
			}
		}
		return indexKeyColumnNames(expression.Nesting.GetChild())
	case expression.Function != nil:
		return indexKeyColumnNames(expression.Function.GetArguments())
	case expression.Grouping != nil:
		return indexKeyColumnNames(expression.Grouping.GetWholeKey())
	case expression.KeyWithValue != nil:
		// Only the KEY part of a covering (KeyWithValue) root names key
		// columns: the split point is the key/value boundary
		// (KeyWithValueExpression.getColumnSize() returns it, and Java's
		// expansion visits the inner key under exactly that split —
		// ValueIndexExpansionVisitor.java:109-115). Recursing without the
		// truncation reported every VALUE column as a key column, which is
		// the wrong-column-set defect RFC-202 D10(a) names: sargable aliases
		// and scan-prefix positions past the split would address entry
		// columns that live in the FDB VALUE, not the key.
		names, ok := indexKeyColumnNames(expression.KeyWithValue.GetInnerKey())
		if !ok {
			return nil, false
		}
		split := int(expression.KeyWithValue.GetSplitPoint())
		if split < 0 || split > len(names) {
			return nil, false
		}
		return names[:split], true
	case expression.Version != nil:
		// A VERSION index's version key column IS the __ROW_VERSION
		// pseudo-column of the (extended) base record type — Java's
		// VersionKeyExpression.toValue resolves it as
		// FieldValue.ofFieldName(base, PseudoField.ROW_VERSION.getFieldName())
		// (VersionKeyExpression.java:119-121), so the match candidate's
		// column list carries the pseudo-field name at the key's version
		// position and stays parallel to ColumnSize.
		return []string{values.PseudoFieldRowVersion}, true
	case expression.Dimensions != nil:
		return indexKeyColumnNames(expression.Dimensions.GetWholeKey())
	case expression.List != nil:
		var names []string
		for _, child := range expression.List.GetChild() {
			childNames, ok := indexKeyColumnNames(child)
			if !ok {
				return nil, false
			}
			names = append(names, childNames...)
		}
		return names, true
	// Version is NOT listed here: it has its own arm above, which names the
	// __ROW_VERSION pseudo-column rather than leaving the coordinate unbound.
	case expression.Value != nil, expression.RecordTypeKey != nil:
		// Implicit components still occupy a physical coordinate. An empty
		// display name deliberately leaves their alias unbound, preventing a
		// later field from being shifted into this key position.
		return []string{""}, true
	case expression.Split != nil:
		return make([]string, int(expression.Split.GetSplitSize())), true
	case expression.Empty != nil:
		return nil, true
	default:
		return nil, false
	}
}

// primaryCandidateKeyComponents returns the physical coordinates the current
// flat primary-candidate model can represent semantically: top-level scalar
// fields, optionally after the leading RecordTypeKey supplied by executeScan.
// Nested/function/literal/version coordinates cannot be expressed as the
// candidate's bare FieldValues, so the whole candidate is declined rather than
// matching a different top-level field or shifting a later coordinate.
func primaryCandidateKeyComponents(rt *recordlayer.RecordType) ([]string, []values.Type) {
	names, leadingRecordTypeKey, ok := coveredPrimaryKeyColumns(rt)
	if !ok {
		return nil, nil
	}
	types := physicalKeyComponentTypes(rt.PrimaryKey, []*recordlayer.RecordType{rt})
	if leadingRecordTypeKey {
		if len(names) == 0 || len(types) == 0 {
			return nil, nil
		}
		types = types[1:]
	}
	if len(names) != len(types) {
		return nil, nil
	}
	return append([]string(nil), names...), append([]values.Type(nil), types...)
}

// IndexRowType flows the descriptor-shaped positional type for
// single-record-type indexes — the SAME layout the runtime rows carry
// (executor.PositionalTypeForDescriptor is the single authority), so
// plan-time ordinal baking (the intersection's comparison keys) matches
// the runtime slots by construction. Multi-type indexes flow Unknown:
// their rows have no single layout.
func (d *metadataIndexDef) IndexRowType() values.Type {
	rts := d.recordTypes()
	if len(rts) != 1 || rts[0].Descriptor == nil {
		return values.UnknownType
	}
	return executor.PositionalTypeForRecordLayout(rts[0].Descriptor, d.md.IsStoreRecordVersions())
}

// singleRecordTypeRowType is that derivation as a free function, so the
// aggregate-index candidate can carry the same layout without minting a second
// mapping from descriptor to declared type. There is exactly one such mapping
// in the engine (executor.PositionalTypeForDescriptor, which
// PositionalTypeForRecordLayout wraps, over values.FieldTypeForProtoField) and
// the ordering-claim predicate is only as trustworthy as that staying true.
func singleRecordTypeRowType(md *recordlayer.RecordMetaData, idx *recordlayer.Index) values.Type {
	rts := md.RecordTypesForIndex(idx)
	if len(rts) != 1 || rts[0].Descriptor == nil {
		return values.UnknownType
	}
	return executor.PositionalTypeForRecordLayout(rts[0].Descriptor, md.IsStoreRecordVersions())
}

// IndexRecordTypeRowTypes flows ONE descriptor-shaped positional type per
// record type the index serves — what covering-column resolution needs when
// IndexRowType has degraded to Unknown (RFC-197 item 1). Same single authority
// (executor.PositionalTypeForDescriptor), applied per type instead of only to
// the single-type case; a type without a descriptor contributes nothing.
func (d *metadataIndexDef) IndexRecordTypeRowTypes() []values.Type {
	rts := d.recordTypes()
	out := make([]values.Type, 0, len(rts))
	for _, rt := range rts {
		if rt.Descriptor == nil {
			continue
		}
		out = append(out, executor.PositionalTypeForRecordLayout(rt.Descriptor, d.md.IsStoreRecordVersions()))
	}
	return out
}

func (d *metadataIndexDef) IndexRecordTypes() []string {
	rts := d.recordTypes()
	names := make([]string, len(rts))
	for i, rt := range rts {
		names[i] = rt.Name
	}
	return names
}

func (d *metadataIndexDef) IndexPrimaryKeyColumns() []string {
	rts := d.recordTypes()
	// Coverage reconstructs visible fields from the tail of IndexEntry.PrimaryKey.
	// Expose names only for an exactly coordinate-aligned tail: flat scalar
	// fields, optionally preceded by the one RecordTypeKey coordinate that the
	// relational schema builder adds. FieldNames alone is not sufficient:
	// nested/function keys add logical names, while literal/version coordinates
	// add no name and can shift the tail heuristic onto the wrong value.
	pkCols, _, ok := commonCoveredPrimaryKeyColumns(rts)
	if !ok {
		return nil
	}
	return pkCols
}

// commonCoveredPrimaryKeyColumns proves that every supplied record type has
// the same coordinate-safe visible primary-key tail and the same leading
// RecordTypeKey topology. Value and vector candidates share this authority;
// neither may reconstruct coverage from FieldNames, which loses hidden tuple
// coordinates and can shift a later field onto the wrong physical value.
func commonCoveredPrimaryKeyColumns(
	recordTypes []*recordlayer.RecordType,
) ([]string, bool, bool) {
	if len(recordTypes) == 0 {
		return nil, false, false
	}
	common, leadingRecordTypeKey, ok := coveredPrimaryKeyColumns(recordTypes[0])
	if !ok {
		return nil, false, false
	}
	for _, rt := range recordTypes[1:] {
		other, otherLeadingRecordTypeKey, otherOK := coveredPrimaryKeyColumns(rt)
		if !otherOK || otherLeadingRecordTypeKey != leadingRecordTypeKey ||
			len(other) != len(common) {
			return nil, false, false
		}
		for i := range other {
			if !strings.EqualFold(other[i], common[i]) {
				return nil, false, false
			}
		}
	}
	return append([]string(nil), common...), leadingRecordTypeKey, true
}

// coveredPrimaryKeyColumns recognizes the only PK topologies the current flat
// coverage representation can map without losing tuple coordinates: scalar
// top-level fields, optionally after one leading RecordTypeKey. The boolean
// reports that prefix so ordering can distinguish a constant single-type
// coordinate from a varying shared-index partition.
func coveredPrimaryKeyColumns(rt *recordlayer.RecordType) ([]string, bool, bool) {
	if rt == nil || rt.PrimaryKey == nil {
		return nil, false, false
	}
	expression := rt.PrimaryKey.ToKeyExpression()
	if expression == nil {
		return nil, false, false
	}
	var components []*gen.KeyExpression
	var flattenThen func(*gen.KeyExpression) bool
	flattenThen = func(current *gen.KeyExpression) bool {
		if current == nil {
			return false
		}
		if current.Then == nil {
			components = append(components, current)
			return true
		}
		for _, child := range current.Then.GetChild() {
			if !flattenThen(child) {
				return false
			}
		}
		return true
	}
	if !flattenThen(expression) || len(components) == 0 ||
		len(components) != rt.PrimaryKey.ColumnSize() {
		return nil, false, false
	}
	leadingRecordTypeKey := components[0].RecordTypeKey != nil
	start := 0
	if leadingRecordTypeKey {
		start = 1
	}
	if start == len(components) {
		return nil, false, false
	}
	columns := make([]string, 0, len(components)-start)
	for _, component := range components[start:] {
		if component.Field == nil || component.Field.FieldName == nil ||
			component.Field.GetFanType() != gen.Field_SCALAR {
			return nil, false, false
		}
		columns = append(columns, component.Field.GetFieldName())
	}
	return columns, leadingRecordTypeKey, true
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
	rts := d.recordTypes()
	// A multi-type index has no single flowed object type. Keep its raw key
	// expressions as metadata, but do not manufacture unresolved FieldValues;
	// the structural PK property conservatively abstains until a concrete
	// per-type alternative supplies one exact row layout.
	if len(rts) != 1 || rts[0].PrimaryKey == nil {
		return nil
	}
	return recordlayer.TranslatePrimaryKeyToValues(
		rts[0].PrimaryKey,
		strings.ToUpper,
		d.IndexRowType(),
	)
}

func (c *metadataPlanContext) GetPrimaryKeyColumns(recordType string) []string {
	if c.md == nil {
		return nil
	}
	rt := c.md.GetRecordType(recordType)
	columns, _, ok := coveredPrimaryKeyColumns(rt)
	if !ok {
		return nil
	}
	return columns
}

// tryAggregateIndexCandidate checks if the index is an aggregate type
// (SUM, COUNT, MIN, MAX) and returns an AggregateIndexMatchCandidate,
// or nil if the index is not an aggregate type.
func tryAggregateIndexCandidate(idx *recordlayer.Index, md *recordlayer.RecordMetaData) *cascades.AggregateIndexMatchCandidate {
	var aggFunc expressions.AggregateFunction
	// Canonicalized like every other behaviour-deriving switch on an index type.
	// A no-op for the arms below (the deprecated bare _EVER spellings fold onto
	// _LONG, which is equally unmatched here, and deliberately so per the note in
	// the PermutedMax arm) — uniform because a switch that looks like it does not
	// need canonicalizing is exactly how the bare spellings were missed.
	switch idx.CanonicalType() {
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

	allCols := gke.FieldNames()
	groupingCount := gke.GetGroupingCount()
	groupedCount := gke.GetGroupedCount()

	if groupingCount == 0 {
		return nil
	}
	permutedSize := 0
	if idx.Type == recordlayer.IndexTypePermutedMax || idx.Type == recordlayer.IndexTypePermutedMin {
		if raw, ok := idx.Options[recordlayer.IndexOptionPermutedSize]; ok {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 0 || parsed > groupingCount {
				return nil
			}
			permutedSize = parsed
		}
		if permutedSize > 0 {
			// The physical key inserts the aggregate value before the permuted
			// grouping suffix. The current aggregate rule has neither residual
			// compensation nor a truthful logical ordering for that shape, so a
			// positive permutation must fall back to base-record aggregation.
			return nil
		}
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

	allTypes := physicalKeyComponentTypes(gke, rts)
	groupTypes := alignPhysicalTypes(allTypes, groupingCount)

	// RFC-209: carry the two structural facts the group-existence machinery
	// needs. countsRows distinguishes a COUNT(*) index (record-layer type
	// `count`, whose stored value is the group's row count) from a COUNT(col)
	// one (`count_not_null`) — both arrive here as AggCount with the same
	// grouping, and only the former makes a stored zero mean "vacated group".
	return cascades.NewAggregateIndexMatchCandidate(
		idx.Name,
		rtNames,
		groupCols,
		aggFunc,
		aggColumn,
		// The declared layout the grouping-column NAMES resolve against. Without
		// it the plan over this index advertises group order for every column
		// type, including a DOUBLE whose key order is not its value order.
		singleRecordTypeRowType(md, idx),
		groupTypes,
		groupingCount,
	).WithGroupExistence(idx.Type == recordlayer.IndexTypeCount, recordlayer.GroupingSignature(gke)).
		WithGroupExistenceCompanionNeed(
			recordlayer.PredicateSignature(idx),
			recordlayer.NeedsGroupCountCompanion(idx),
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
	if pk, _, safe := commonCoveredPrimaryKeyColumns(rts); safe {
		pkCols = make([]string, len(pk))
		for i, col := range pk {
			pkCols[i] = strings.ToUpper(col)
		}
	}

	partitionTypes := alignPhysicalTypes(
		physicalKeyComponentTypes(idx.RootExpression, rts),
		partitionCount,
	)

	baseRowType := singleRecordTypeRowType(md, idx)
	if values.IsUnresolved(baseRowType) {
		return nil
	}
	return cascades.NewVectorIndexScanMatchCandidate(
		idx.Name, rtNames, upperCols, partitionCount, metric,
		baseRowType, idx.IsUnique(), pkCols,
	).WithPartitionKeyComponentTypes(partitionTypes)
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
	if explode, ok := plan.(*plans.RecordQueryExplodePlan); ok {
		return deriveColumnsFromProjectionlessExplode(explode)
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
			Name: values.FieldNameForProtoField(fd),
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

// deriveColumnsFromProjectionlessExplode publishes the SQL-visible columns of
// a bare record-valued Explode leaf. Inline VALUES is the canonical owner:
// `SELECT * FROM VALUES (42)` deliberately has no Projection above its Explode,
// so no catalog scan exists from which the generic leaf fallback can recover
// metadata. The Explode's constructor-time result snapshot and admitted output
// layout are the authority instead.
//
// This arm is deliberately all-or-nothing and record-only. A scalar Explode is
// one scalar source consumed by a surrounding FlatMap, not a projection-less
// table row. WITH ORDINALITY emits a two-slot box (element, ordinal), whose SQL
// AS/AT names are likewise assigned by that surrounding operator; flattening a
// record element here would describe a different row than execution emits.
func deriveColumnsFromProjectionlessExplode(explode *plans.RecordQueryExplodePlan) []executor.ColumnDef {
	if explode == nil || explode.IsWithOrdinality() {
		return nil
	}
	rowType, ok := explode.GetElementType().(*values.RecordType)
	if !ok || rowType == nil {
		return nil
	}
	if _, err := values.SnapshotExactType(rowType); err != nil {
		return nil
	}
	layout, err := explode.ProvidedOutputLayout()
	if err != nil || layout == nil || layout.Carrier() == nil ||
		!values.FlowedTypeEquals(layout.Carrier(), rowType) {
		return nil
	}

	cols := make([]executor.ColumnDef, len(rowType.Fields))
	for ordinal, field := range rowType.Fields {
		typeName := cascadesTypeName(field.FieldType)
		if typeName == "" {
			typeName = "UNKNOWN"
		}
		nullable := api.ColumnNoNulls
		if field.FieldType.IsNullable() {
			nullable = api.ColumnNullable
		}
		cols[ordinal] = executor.ColumnDef{
			Name:     field.Name,
			TypeName: typeName,
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
		// A covering scan HOLDS its index plan as a field rather than as a
		// child (RFC-220 criterion C1), so neither the type assertion above nor
		// the innerPlan chain below reaches it — the walk would run off the end
		// and report "no index leaf". That answer is not an error anywhere: the
		// caller reads it as "no record types" and returns an empty column list,
		// so `SELECT *` over a plan whose leaves are covering scans reports ZERO
		// columns and every database/sql Scan fails with "expected 0 destination
		// arguments". Unwrapping here is the same explicit arm Java's plan
		// visitors each carry for the covering type.
		if cov, ok := p.(*plans.RecordQueryCoveringIndexPlan); ok {
			return cov.GetIndexPlan()
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
		case *plans.RecordQueryCoveringIndexPlan:
			// A covering scan is a LEAF here, not a parent of one: it holds its
			// index plan as a field (RFC-220 criterion C1), so the GetChildren()
			// recursion below never descends into it and it would otherwise
			// contribute no descriptor.
			//
			// This walk is reached with a covering leaf constantly (436 times
			// across the sqldriver target, measured), but removing this arm
			// currently changes NO test outcome: when the descriptor is absent,
			// projection type resolution falls through to its type-inheritance
			// chain and arrives at the same answer. So the arm is correct rather
			// than demonstrably load-bearing, and it is kept on that basis — a
			// leaf that reports no record types is wrong on its own terms, and
			// the fallback that currently hides it is not guaranteed to cover
			// every future caller of this walk.
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
//  3. among several the qualifier cannot separate (the physical plan's leaves
//     carry record-type names, not query aliases, so "B.VAL" matches no
//     descriptor): the answer is first-match ONLY while every candidate AGREES
//     on what it would report — same SQL type, same cardinality. Then there is
//     nothing to guess and the shared answer is the answer.
//  4. when the candidates DISAGREE, DECLINE (nil). This is the case that
//     produced wrong client metadata: two legs declaring the same column at
//     different types made first-match report the far leg's column as the near
//     leg's type, and because a non-empty answer looks resolved it also
//     PREEMPTED the caller's type-inheritance chain — which reads the actual
//     flowed type off the inner plan's own output and answers correctly.
//
// Declining on disagreement is what lets the type FLOW instead of being
// re-derived. Java never searches for a column's type at all: client metadata is
// positional over the plan's flowed record type (RelationalStructMetaData.getField
// is List.get(i)), so a name that cannot identify one answer must fall through to
// the flowed type rather than resolve to a guess. Scoping the decline to genuine
// DISAGREEMENT keeps every case where the search was never really ambiguous —
// notably a join whose legs share a NOT-NULL PK name — reporting exactly what it
// reported before, so the decline cannot silently move nullability.
//
// Returns nil when no leg has the field, and when several do and they disagree.
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
	// Do the candidates agree on everything this lookup is consulted for?
	// descriptorForColumn has FOUR consumers. Three read only the field's SQL
	// type name and its cardinality (the NOT-NULL bit) off the returned
	// descriptor — deriveProjectionColumnDef, columnDefFromRef, and the
	// GROUP-BY key derivation in buildAggColumns — and for those,
	// when every candidate answers both identically, the choice among them is
	// not observable and first-match is exact.
	//
	// The fourth consumer reads the descriptor's IDENTITY, which this
	// agreement gate does NOT cover: the null-born upgrade in
	// deriveColumnsFromProjection tests nullBorn[d.FullName()] — whether the
	// returned LEAF is an outer join's null-supplying leg. Two legs can agree
	// on type+cardinality and still differ on null-born membership:
	// LEFTT(VAL BIGINT NOT NULL) LEFT JOIN RIGHTT(VAL BIGINT NOT NULL) agrees
	// on (BIGINT, required) for VAL, first-match answers LEFTT, and RIGHTT's
	// null-born upgrade never fires — B.VAL reports NoNulls where Java (#4274)
	// reports the null-supplying column nullable. No choice function HERE can
	// repair that: BOTH result slots receive the SAME candidate list while the
	// correct answer differs per slot — (NoNulls, Nullable).
	//
	// So that consumer no longer asks this function. A QUANTIFIER-ADDRESSED
	// read resolves its leg structurally instead (legRead: the leg plan the
	// correlation names, then that leg's own column at the read's leg-relative
	// ordinal). That answers PER SLOT, which is the property this function
	// cannot have. It is not name-free — legRead resolves the leg by
	// correlation ALIAS and falls back to a leaf-name match for an unbaked
	// read — but neither of those is a COLUMN name searched across legs, which
	// is the specific thing that collapses two slots onto one answer here.
	// TestCrossLegNullBorn_RequiredColumnOnNullSupplyingLeg pins exactly the
	// shape above. What still arrives here is the FLAT (childless) read, which
	// carries no correlation to resolve a leg from; for that form the
	// first-match hole stands, and positional metadata flowed from the plan's
	// own result type (the D3 deliverable) is still the general answer.
	// TestFDB_CrossLegAgreementGate_NullBornNotCovered pins the fact that no
	// SQL-DDL-expressible column can reach either path — the emitter produces
	// no REQUIRED field, so nothing derives NoNulls and the upgrade is vacuous
	// through the driver.
	//
	// STRUCT nested-field disagreement is likewise uncovered by this gate —
	// two candidates agreeing on ("STRUCT", cardinality) can nest entirely
	// different shapes — and is currently unobservable only because ColumnDef
	// carries no nested metadata; it becomes live the moment
	// getStructMetaData-style nested metadata is threaded through ColumnDef.
	first := matches[0].Fields().ByName(bare)
	firstType := protoFieldTypeName(matches[0], string(bare))
	for _, d := range matches[1:] {
		fd := d.Fields().ByName(bare)
		if fd.Cardinality() != first.Cardinality() ||
			protoFieldTypeName(d, string(bare)) != firstType {
			return nil // the candidates disagree — refuse to pick one
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
		if fv, isCol := values.AsFieldValue(f.Value); isCol && desc != nil {
			if t := protoFieldTypeName(desc, strings.ToUpper(fv.DisplayName())); t != "UNKNOWN" {
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
			if strings.EqualFold(n.GetOuterAlias().Name(), alias) {
				out = append(out, legMatch{n.GetOuter(), outerNS || legHasDefaultOnEmpty(n.GetOuter())})
			}
			out = collectLegMatches(n.GetOuter(), alias, outerNS, out)
			if strings.EqualFold(n.GetInnerAlias().Name(), alias) {
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

// legRead is the STRUCTURAL resolution of a QUANTIFIER-ADDRESSED projected
// read: the join leg its correlation names, that leg's OWN derived columns,
// and whether the leg is null-supplying (an outer join's null-extended side).
//
// Both arms of deriveColumnsFromProjection that need a QOV read's leg column —
// the null-born nullability upgrade and the type inheritance — go through here,
// so the ADDRESSING is derived once. They used to derive it independently, and
// they had already drifted: the type arm addressed the leg structurally while
// the nullability arm composed a "CORR.FIELD" string for a descriptor lookup
// that cannot separate legs at all.
//
// Sharing the addressing is NOT the same as the two arms behaving alike, and
// this comment used to claim it was. They consume the result differently and
// deliberately: the nullability arm short-circuits on nullSupplying before it
// ever addresses a column (see nullExtended), and the type arm has its own
// fallbacks after this returns. What is guaranteed here is one derivation of
// "which leg, which slot" — nothing about what each caller then does with it.
type legRead struct {
	cols          []executor.ColumnDef
	nullSupplying bool
}

// resolveLegRead resolves a correlation to its leg and derives that leg's own
// columns. found=false when the alias names no unambiguous leg of this plan
// (legPlanFor declines on a duplicated alias), which leaves callers on their
// name-keyed fallbacks.
func resolveLegRead(inner plans.RecordQueryPlan, md *recordlayer.RecordMetaData, corr string) (legRead, bool) {
	legPlan, nullSupplying, found := legPlanFor(inner, corr)
	if !found {
		return legRead{}, false
	}
	return legRead{cols: deriveColumnsFromPlan(legPlan, md), nullSupplying: nullSupplying}, true
}

// column returns the leg column this read addresses, by one of two keys that
// mirror Java's own two accessor kinds exactly:
//
//   - the BAKED LEG-RELATIVE ordinal, tried first. Java's RESOLVED accessor
//     compares getOrdinal() ALONE — the name is not part of identity
//     (FieldValue.java:684,:689).
//   - the leaf NAME. Java's UNRESOLVED accessor compares ordinal AND name
//     (FieldValue.java:633, hashCode :638), which is what a carrier with no
//     usable ordinal falls back to here.
//
// So the two-key split is not an ad-hoc fallback ladder; it is the same split
// Java draws between a resolved and an unresolved accessor.
//
// The name key serves an UNBAKED (lazy) read, which carries no ordinal — and
// ALSO a baked read whose ordinal is out of range for this leg, which the
// earlier wording denied. It is a genuinely weaker key either way, since a leg
// that duplicates an output name keeps only one of them under it.
//
// Accessors[0] is the whole story for the single-accessor case: the leaf
// derivation emits one column per TOP-LEVEL proto field, so a struct occupies
// exactly one slot and root ordinals stay aligned with r.cols. No re-anchoring
// is needed to reach the right slot.
func (r legRead) column(fv values.FieldValue) (executor.ColumnDef, bool) {
	if path := fv.Path(); path != nil {
		// A reference that is not exactly single-accessor is not addressable
		// against this leg's columns by EITHER key. A multi-accessor root
		// ordinal is not an index into the leg's flattened columns, and the
		// only NAME such a reference carries is its leaf — a member of the
		// enclosing struct's namespace, offered to columns keyed by the
		// RECORD's, where a shared spelling answers with an unrelated column.
		// Declining leaves the reference's own resolved type and nullability
		// standing, which is Java's answer (FieldValue.computeResultType is
		// fieldPath.getLastFieldType, FieldValue.java:143-148).
		//
		// WHAT THIS DECLINE ACTUALLY CHANGES, per arm — it is not one guard
		// covering both, and describing it as merely "moved here" was wrong:
		//
		//   - MULTI-accessor, TYPE arm: already declined before this existed.
		//     deriveColumnsFromProjection sets inherited=true for
		//     len(Accessors) > 1 and the whole QOV block sits under
		//     `if !inherited`, so such a read never reaches this function.
		//     Redundant here, kept because this function must be correct on
		//     its own terms rather than on its caller's.
		//   - ZERO-accessor, TYPE arm: a NEW restriction. A Resolved carrying
		//     no accessors passed both of those upstream tests and did reach
		//     the leg's leaf-name loop; now it declines.
		//   - EITHER, NULLABILITY arm: new, and this is the only guard there
		//     is — that arm reaches this function through nullExtended with no
		//     upstream arity test at all.
		//
		// MEASURED over the sqldriver suite (6159 tests) at the commit that
		// added this: 13 entries into this function, ALL single-accessor —
		// zero multi-accessor, zero zero-accessor, zero unbaked. So the new
		// restriction is LATENT, not a live behaviour change, and this decline
		// is fail-safe rather than corpus-proven. Recorded rather than dressed
		// up as coverage.
		if path.Len() != 1 {
			return executor.ColumnDef{}, false
		}
		accessor, ok := path.Accessor(0)
		if !ok {
			return executor.ColumnDef{}, false
		}
		if ord := accessor.Ordinal(); ord >= 0 && ord < len(r.cols) {
			return r.cols[ord], true
		}
	}
	for _, ic := range r.cols {
		if strings.EqualFold(parseColRef(ic.Name).bare(), fv.DisplayName()) {
			return ic, true
		}
	}
	return executor.ColumnDef{}, false
}

// nullExtended reports whether the addressed column serves SQL NULL on this
// plan's rows: either the whole leg is null-supplying (the outer join pads
// unmatched rows) or the leg's own derivation already made the column
// nullable. Callers only ever UPGRADE off this answer — a false here is "no
// evidence of nullability", never "provably NOT NULL".
//
// THE ORDER OF THE TWO TESTS IS LOAD-BEARING, and it is why a struct-descent
// read on a null-extended leg still upgrades even though column() would
// decline it: nullSupplying is answered FIRST, so the leg's null extension
// never depends on resolving a leaf. That mirrors Java, where the flowed
// nullability is DISJUNCTIVE with the path rather than gated on it —
// computeResultType's `childValue.getResultType().isNullable() ||
// fieldPath.areAnyFieldTypesNullable()` (FieldValue.java:147). A consequence
// worth stating plainly: on a null-supplying leg this function bypasses
// column() entirely, so the decline there is NOT in force for the case this
// arm most cares about.
func (r legRead) nullExtended(fv values.FieldValue) bool {
	if r.nullSupplying {
		return true
	}
	ic, ok := r.column(fv)
	return ok && ic.Nullable == api.ColumnNullable
}

// projectionLabelAlreadyUsed reports whether an earlier projected slot already
// publishes this display label. A repeated label is legal and expected
// (`SELECT c.id, o.id`); it is also the signal that the output SCHEMA — which
// must stay name-addressable — carries a deduplicating suffix the user never
// wrote.
func projectionLabelAlreadyUsed(earlier []executor.ColumnDef, label string) bool {
	if label == "" {
		return false
	}
	for _, c := range earlier {
		if strings.EqualFold(columnDefDisplayName(c), label) {
			return true
		}
	}
	return false
}

func deriveColumnsFromProjection(proj *plans.RecordQueryProjectionPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	// A projection over a join references columns from MULTIPLE record types,
	// so resolve each column's type against every join leaf, not just the
	// first one (the single-leaf lookup left the other leg's columns UNKNOWN).
	descs := allLeafDescriptors(proj.GetInner(), md)
	aliases := proj.GetAliases()
	aliasProvenance := proj.GetAliasMinted()
	aliasSources := proj.GetAliasSources()
	projections := proj.GetProjections()
	outputNames := proj.GetOutputNames()

	// Leaf descriptors under a NULL-SUPPLYING (DefaultOnEmpty) subtree: a
	// column defined by one of these serves NULL on the outer join's padded
	// rows, so its metadata reports NULLABLE regardless of the proto's
	// Required (Java gets this from
	// the flowed result type; the name-model lazy projections here flow
	// none). Keyed by descriptor FullName. Coarse by design in the safe
	// direction: a self-joined table on both sides marks the preserved read
	// nullable too — clients then handle a NULL that never comes, never the
	// reverse.
	//
	// THAT COARSENESS IS NOW NARROWED, NOT SCOPED AWAY — an earlier wording
	// here said "the FLAT (childless) read ONLY", which the fallback about
	// fifty lines down contradicts. Two kinds of read still land on this map:
	//
	//   - the FLAT (childless) read, which carries no correlation at all and
	//     so has no leg to resolve;
	//   - a QUANTIFIER-ADDRESSED read whose alias names no UNAMBIGUOUS leg,
	//     where legPlanFor declines (a folded block's duplicated alias) and
	//     the structural path cannot answer.
	//
	// What did change is that a QOV read WITH a resolvable leg no longer
	// consults this map, so the self-join case described above — both sides
	// sharing one descriptor — is answered exactly for that form: the
	// preserved leg reports its own nullability and only the null-supplying
	// leg is upgraded.
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
		// A slot past the provenance vector reads as a USER alias: a machinery
		// mint is the exceptional case and states itself explicitly.
		aliasMinted := i < len(aliasProvenance) && aliasProvenance[i]
		aliasSource := values.ProjectionAliasSource{}
		if i < len(aliasSources) {
			aliasSource = aliasSources[i]
		}
		cd := deriveProjectionColumnDef(v, alias, aliasMinted, aliasSource, i, descs)
		// The projection's frozen result schema is the emitted row-key
		// authority. In particular, a scalar QOV can compute an UNNEST ordinal
		// whose SQL name (AT) differs from the scalar leg's display name (VAL).
		// Re-deriving that key from the Value here would make metadata disagree
		// with the row the executor emits.
		if i < len(outputNames) && outputNames[i] != "" {
			cd.Name = outputNames[i]
			if alias == "" {
				if values.QuantifierFlowsAScalarRow(v) {
					cd.Label = outputNames[i]
				} else if _, isReference := values.AsFieldValue(v); isReference &&
					!projectionLabelAlreadyUsed(cols[:i], cd.Label) {
					// A plain column REFERENCE can be RENAMED by the projection's
					// frozen output schema with no SELECT alias attached: a CTE
					// column list (`WITH r(n) AS …`) renames the leg's output and
					// never appears as an alias on a projected item, and a
					// rebuild that preserves the output schema does not always
					// carry the alias vector with it. The executor names the
					// emitted slot from that schema, so a label left on the read's
					// own field name (`ID`) describes a slot the row calls `N` and
					// the positional read refuses to align — every column of the
					// result then goes loud.
					//
					// NOT when the derived label already occurs at an earlier
					// slot. The output schema is name-addressable, so a repeated
					// label is DEDUPLICATED there (`X`, `X_2`) while the
					// user-visible labels stay `[X X]` — Java's layout, and what
					// the alignment check already tolerates by ordinal. Following
					// the schema there would publish the machinery's suffix as a
					// column name.
					//
					// Bare leaf and upper-cased, to stay in the derivation's own
					// spelling: an output name over a gated ordinal join is
					// qualified (`C.NAME`) while the label is bare, and a quoted
					// inner alias reaches the schema in its written case (`did`)
					// where the column list reports `DID`.
					cd.Label = strings.ToUpper(parseColRef(outputNames[i]).bare())
				}
			} else if !aliasMinted {
				cd.Label = outputNames[i]
			}
		}
		// Exact Value nullability describes the expression before a surrounding
		// join edge. Overlay the selected input's null-extension after that
		// derivation, even when the expression type itself is already known (a
		// projected EXISTS is exact NOT NULL and therefore never enters the
		// UNKNOWN-only inheritance block below).
		if fv, isField := values.AsFieldValue(v); isField {
			if qov, viaQOV := values.AsQuantifiedObjectValue(fv.ChildValue()); viaQOV {
				if qov.Correlation() == proj.GetInnerQuantifier().GetAlias() && fv.Path().Len() == 1 &&
					projectionInputNullExtendsOutput(proj.GetInner()) {
					deriveInner()
					ordinals := fv.Path().Ordinals()
					if ordinal := ordinals[0]; ordinal >= 0 && ordinal < len(innerCols) && innerCols[ordinal].Nullable == api.ColumnNullable {
						cd.Nullable = api.ColumnNullable
					}
				} else if _, nullSupplying, found := legPlanFor(proj.GetInner(), qov.Correlation().Name()); found && nullSupplying {
					cd.Nullable = api.ColumnNullable
				} else {
					deriveInner()
					qualified := strings.ToUpper(qov.Correlation().Name()) + "." + strings.ToUpper(fv.DisplayName())
					if inherited, ok := innerByName[qualified]; ok && inherited.Nullable == api.ColumnNullable {
						cd.Nullable = api.ColumnNullable
					}
				}
			}
		}
		if cd.Nullable == api.ColumnNoNulls {
			// The projected reference is either a FLAT (childless) read — its
			// Field may carry the "LEG.COL" qualifier — or the resolver's
			// QUANTIFIER-ADDRESSED bake (FieldValue{Child: QOV(leg), COL}).
			//
			// The QOV form resolves its leg STRUCTURALLY, through the same
			// legRead the type-inheritance arm below uses. It previously
			// composed "CORR.FIELD" and handed the string to
			// descriptorForColumn, which cannot answer it across legs: that
			// lookup matches by BARE name over every join-leaf descriptor and
			// consults the qualifier only as a tie-break against d.Name() —
			// the PROTO/TABLE name — so a correlation never matches for an
			// aliased source (`FROM orders o` composed "O.VAL" against a
			// descriptor named ORDERS). First-match then answered the
			// PRESERVED leg and a null-supplying window's column reported
			// NoNulls. Java addresses the same reference by ordinal identity
			// alone — ResolvedAccessor.equals/hashCode compare getOrdinal()
			// and the name is not part of identity (FieldValue.java:684,:689).
			if fv, ok := values.AsFieldValue(v); ok {
				structural := false
				if qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue()); isQOV {
					if r, found := resolveLegRead(proj.GetInner(), md, qov.Correlation().Name()); found {
						structural = true
						if r.nullExtended(fv) {
							cd.Nullable = api.ColumnNullable
						}
					}
				}
				// A FLAT read carries no correlation to resolve a leg from, and
				// a QOV read whose alias names no unambiguous leg (a folded
				// block's duplicated alias, where legPlanFor declines) has none
				// either. Both fall back to the descriptor-identity test on the
				// reference's OWN name — coarse across legs, but composing
				// nothing: the qualifier the mint used to add could only ever
				// have tie-broken against a table name.
				if !structural {
					if d := descriptorForColumn(fv.DisplayName(), descs); d != nil {
						if _, born := nullBorn[d.FullName()]; born {
							cd.Nullable = api.ColumnNullable
						}
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
			fv, isField := values.AsFieldValue(v)
			if isField {
				if qov, viaQOV := values.AsQuantifiedObjectValue(fv.ChildValue()); viaQOV {
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
					// A MULTI-ACCESSOR reference inherits from nothing here. Every
					// arm below recovers a column by a NAME, and the only name a
					// fused reference has is its leaf — a member of the enclosing
					// struct's namespace, offered to maps keyed by the RECORD's. A
					// shared spelling then types this column from an unrelated
					// column. The reference already carries the leaf's own type on
					// its resolved path (Java's FieldValue.computeResultType is
					// fieldPath.getLastFieldType, FieldValue.java:143-148), so
					// declining to inherit leaves the RIGHT answer standing rather
					// than a guess.
					if fv.Path().Len() > 1 {
						inherited = true
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
						if qov != nil {
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
							if r, found := resolveLegRead(proj.GetInner(), md, qov.Correlation().Name()); found {
								if ic, ok := r.column(fv); ok && ic.TypeName != "" && ic.TypeName != "UNKNOWN" {
									cd.TypeName = ic.TypeName
									cd.Nullable = ic.Nullable
									if r.nullSupplying {
										cd.Nullable = api.ColumnNullable
									}
								}
								inherited = true
							}
							// THE DECLINE IN legRead.column ONLY NARROWS THIS ARM, it
							// does not close it. When column() refuses, the lookup
							// below still recovers a column by NAME — `legPrefix +
							// fv.Field` against innerByName — which is the same
							// leaf-vs-record-namespace hazard the decline exists to
							// avoid, one map further out. The NULLABILITY arm has no
							// such tail: a resolved leg there sets structural=true
							// and suppresses every fallback, so for that arm the
							// hazard really is closed. This tail is pre-existing and
							// unreachable today for the multi-accessor case
							// (deriveColumnsFromProjection's upstream
							// `len(Accessors) > 1` sets inherited, and this block
							// sits under `if !inherited`); it shuts entirely when the
							// qualified-name inner keys go away.
							if !inherited {
								legPrefix := strings.ToUpper(qov.Correlation().Name()) + "."
								if ic, found := innerByName[legPrefix+strings.ToUpper(fv.DisplayName())]; found && ic.TypeName != "" && ic.TypeName != "UNKNOWN" {
									inheritFrom(ic)
								}
							}
						}
						if !inherited {
							if ic, found := innerByName[strings.ToUpper(fv.DisplayName())]; found && ic.TypeName != "" && ic.TypeName != "UNKNOWN" {
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

// projectionInputNullExtendsOutput reports whether a whole-row read through a
// projection's sole physical edge crosses a join that can replace one of its
// output slots with SQL NULL. An INNER/CROSS FlatMap may contain a
// FirstOrDefault solely to compute EXISTS; its folded output Value remains
// exact NOT NULL, so the wrapper alone is not evidence of null extension.
func projectionInputNullExtendsOutput(plan plans.RecordQueryPlan) bool {
	for plan != nil {
		switch p := plan.(type) {
		case *plans.RecordQueryNestedLoopJoinPlan:
			return p.GetJoinType() == plans.JoinLeftOuter || p.GetJoinType() == plans.JoinFullOuter
		case *plans.RecordQueryProjectionPlan:
			return false
		default:
			inner, ok := plan.(innerPlan)
			if !ok {
				return false
			}
			plan = inner.GetInner()
		}
	}
	return false
}

// deriveProjectionColumnDef derives the ResultSet ColumnDef (datum-lookup Name,
// user-visible display Label, type, nullability) for a single projected column
// from its Value + optional SELECT-list alias. Its one caller is the normal
// projection path (deriveColumnsFromProjection).
//
// The RFC-141 projected-EXISTS fold does NOT come through here, despite what
// this comment claimed until the claim was checked: deriveColumnsFromFlatMap
// derives its columns through foldedColumnDef, which takes Name+Label from the
// field NAME the fold set rather than re-deriving them from the field VALUE.
// The two are consistent by construction, not by sharing code, and
// foldedColumnDef's own doc is the one that explains why.
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
//
// aliasMinted is the slot's alias PROVENANCE (RecordQueryProjectionPlan's
// GetAliasMinted): true when the machinery wrote the alias as an internal datum
// key, false when it is the user's `AS`. It is what separates the two — they are
// spelled alike — and the separation is the whole difference between reporting
// `SELECT u.name AS "U.NAME"` as Java does (verbatim) and degrading it.
func deriveProjectionColumnDef(
	v values.Value,
	alias string,
	aliasMinted bool,
	aliasSource values.ProjectionAliasSource,
	idx int,
	descs []protoreflect.MessageDescriptor,
) executor.ColumnDef {
	// A NESTED reference is named by its resolved PATH, read from the one
	// predicate every naming authority shares, and it is tested FIRST because it
	// subsumes both arms below: `Field` is ONE segment of the path, so it cannot
	// name a nested column no matter which segment it holds. It held the struct
	// ROOT when this arm was written and `n.sk` and `n.co` were both named `N`
	// here — duplicate labels over correct data, measured; it holds the LEAF now
	// and `SELECT t1.n.sk, sk` would collide the other way. Java names the fused
	// reference by the requested identifier `n.sk` for exactly this reason
	// (SemanticAnalyzer.java:598-599) and the top-level projection then clears
	// the qualifier, so the user sees SK and CO.
	//
	// It subsumes the Child arm too, and that is the point rather than an
	// accident: NestedResolvedPath renders THROUGH the child, so a nested
	// reference over a ≥2-source FROM takes `T1.N.SK` here — the same qualified
	// shape the `Child != nil` arm below produces for a flat reference, reached
	// by one predicate instead of two. `Child == nil` is not the nested arm's
	// precondition; the multi-accessor resolved path is. The Label computed from
	// this Name is the unqualified leaf either way (`SK`), measured over
	// `SELECT n.sk, n.co FROM t1, t2`, so the qualifier stays an internal slot
	// key exactly as Java's does (Identifier.withoutQualifier, Identifier.java:101).
	var name string
	if path, nested := values.NestedResolvedPath(v); nested {
		name = path
	} else if _, ok := values.AsFieldValue(v); ok {
		name = values.ColumnNameValue(v)
	} else {
		name = values.ColumnNameValue(v)
	}
	var label string
	if alias != "" {
		label = strings.ToUpper(alias)
	} else if _, isField := values.AsFieldValue(v); !isField {
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
	if _, isField := values.AsFieldValue(v); isField && colDesc != nil {
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
		if _, isField := values.AsFieldValue(v); isField && name != "" {
			// The label is the bare LEAF of the column's NAME, derived above,
			// never of fv.Field. The two happen to agree for a fused nested
			// reference today — the mint names it after its leaf — and that
			// agreement is exactly what must not be relied on: it did not hold
			// when this arm was written (Field carried the struct ROOT, so
			// `n.sk` and `n.co` both labelled `N`), and Field is one segment
			// where the label is a function of the whole NAME, qualifier
			// included. Java takes the leaf of the resolved identifier
			// (Identifier.withoutQualifier, Identifier.java:101-106) applied by
			// the top-level clearQualifier, which is SK and CO.
			displayLabel = strings.ToUpper(parseColRef(name).bare())
		}
	} else if aliasMinted {
		// A MACHINERY-pinned alias — the duplicated-bare-leaf dedup pins the
		// projected reference's QUALIFIED spelling ("A.NAME" for QOV(A).NAME)
		// as the alias so the two same-named datum keys do not collapse — is
		// an INTERNAL key, not a user label: Java reports the bare column for
		// `SELECT c.name, p.name` (both NAME, JDBC allows duplicate labels).
		// So the qualifier comes off, and the datum key (colName, below) keeps
		// it.
		//
		// The provenance is CARRIED here, never recovered from the string. It
		// used to be recovered — a dotted label whose leaf matched the
		// projected reference's leaf was read as machinery — and a user is
		// perfectly entitled to write that exact spelling: `SELECT u.name AS
		// "U.NAME"` reported the label NAME, as did the SINGLE-TABLE form where
		// no machinery alias can exist at all. Java never inspects an alias for
		// a dot (its clearQualifier, LogicalOperator.java:484-487, strips the
		// structural qualifier LIST, and a delimited `"U.NAME"` is one
		// Identifier with an EMPTY qualifier list), so no spelling can be the
		// discriminator.
		// The counterparty is the frozen structured alias source captured when
		// the machinery minted this key. The projected Value may since have been
		// reanchored onto `_current`, so it is evaluation authority rather than
		// authored display identity. Whether the qualifier sliced out of the
		// label equals the frozen source is this site's conversion question. The
		// parenthesis heuristic is recorded as its own DECLINE rather than
		// folded into "bare", because a rejection made by looking for `()` in a
		// rendering is the thing under measurement, not a clean non-split.
		if stripped, did := stripDisplayLabelQualifier(label, aliasSource); did {
			displayLabel = stripped
		}
	}
	nullable := api.ColumnNullable
	if colDesc != nil {
		if fd := colDesc.Fields().ByName(protoreflect.Name(parseColRef(name).bare())); fd != nil && fd.Cardinality() == protoreflect.Required {
			nullable = api.ColumnNoNulls
		}
	}
	// The proto descriptor says what the STORED column is; the exact FLOWED
	// value says what this query serves. It is authoritative in both
	// directions: outer-null extension widens a field to nullable, while EXISTS
	// and other definite expressions remain NOT NULL even without a descriptor.
	// UNKNOWN carries no claim and leaves the descriptor/default untouched.
	if flowed := v.Type(); flowed != nil && flowed.Code() != values.TypeCodeUnknown {
		if flowed.IsNullable() {
			nullable = api.ColumnNullable
		} else {
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
	if _, ok := values.AsFieldValue(f.Value); ok {
		typeRef = strings.ToUpper(values.ColumnNameValue(f.Value))
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

// mergedInRVOrder re-sequences the name-model leg-merge into the ordinal RC's
// authoritative output order, keeping each column's qualified datum key.
//
// A divergence between the two sequences is a statement about ORDER, never
// about the names. The merge walks the PHYSICAL leg tree, and which grouping
// the planner picks for equal-cost legs is arbitrary — a plain three-way
// `SELECT *` plans `TA ⋈ (TB ⋈ TC)` or `TB ⋈ (TA ⋈ TC)` on a tie — while the RC
// always carries FROM order. Answering the divergence by falling back to the
// RC's own bare labels fixes the order and throws the qualifiers away with it,
// dropping the `TA.K`/`TB.K` datum keys that by-name reads use. The permutation
// is what was actually needed, and the RC states it.
//
// Each RC field names its LEG by root correlation and its position by baked
// ordinal path; slotIndex resolves that pair to the leg's derived-column index
// (legSlotIndex in production, which walks the path down the leg's own join
// tree). Nothing here re-derives a column: the merge already carries the
// qualification, the outer-join null extension and the per-leg recursion, and
// this only says WHERE each of its entries belongs.
//
// The resolver is a parameter so the permutation assembly and its refusals can
// be driven directly, without standing up a plan tree for each one.
//
// Reports false unless the RC accounts for every merged column exactly once,
// under exactly the two leg roots. Anything else means the merge is not simply
// misordered, and the caller keeps its existing RC-derived answer rather than
// guessing at an alignment.
func mergedInRVOrder(
	rc *values.RecordConstructorValue,
	merged []executor.ColumnDef,
	firstAlias, secondAlias string,
	firstWidth int,
	slotIndex func(alias string, path []int) (int, bool),
) ([]executor.ColumnDef, bool) {
	if len(rc.Fields) != len(merged) ||
		firstWidth < 0 || firstWidth > len(merged) ||
		firstAlias == "" || secondAlias == "" || firstAlias == secondAlias {
		return nil, false
	}
	out := make([]executor.ColumnDef, len(merged))
	taken := make([]bool, len(merged))
	for i, f := range rc.Fields {
		field, isField := values.AsFieldValue(f.Value)
		if !isField {
			return nil, false
		}
		root, isRoot := values.AsQuantifiedObjectValue(field.ChildValue())
		if !isRoot {
			return nil, false
		}
		var position int
		var resolved bool
		switch name := strings.ToUpper(root.Correlation().Name()); name {
		case firstAlias:
			position, resolved = slotIndex(name, field.Path().Ordinals())
		case secondAlias:
			position, resolved = slotIndex(name, field.Path().Ordinals())
			position += firstWidth
		}
		if !resolved || position < 0 || position >= len(merged) || taken[position] {
			return nil, false
		}
		taken[position] = true
		out[i] = merged[position]
	}
	return out, true
}

// legSlotIndex resolves one ordinal path within a leg's emitted row to that
// leg's DERIVED-COLUMN index.
//
// The two orders are not the same and cannot be assumed to be. A join leg emits
// a nested positional-merge row whose slot order is its PHYSICAL leg order,
// while its derived columns come back flat in SQL order — deriveColumnsFromJoin
// may already have reversed them. `SELECT * FROM p, q, p` planned `(P ⋈ Q) ⋈ P`
// is the shape where they part company: the sub-join's row is `<_0 Q, _1 P>`
// and its columns are `[P.ID P.V Q.QID]`. So the path is followed through the
// join's OWN result value, which names the leg at each slot, and the leg order
// is re-derived exactly as the merge derived it.
//
// A leg that is not a join emits a flat row: slot i is column i.
func legSlotIndex(
	leg plans.RecordQueryPlan, md *recordlayer.RecordMetaData, path []int,
) (int, bool) {
	if leg == nil || len(path) == 0 {
		return 0, false
	}
	// Pass-through wrappers (Fetch, Limit, DefaultOnEmpty) keep their input's
	// row, so descend to the node that shapes it. definesOutputSchema is the
	// same stop set deriveColumnsFromPlan's descent uses.
	for !definesOutputSchema(leg) {
		inner, wrapped := leg.(innerPlan)
		if !wrapped || inner.GetInner() == nil {
			break
		}
		leg = inner.GetInner()
	}
	nlj, isJoin := leg.(*plans.RecordQueryNestedLoopJoinPlan)
	if !isJoin {
		if len(path) != 1 {
			return 0, false
		}
		if path[0] < 0 || path[0] >= len(deriveColumnsFromPlan(leg, md)) {
			return 0, false
		}
		return path[0], true
	}
	rc, isRC := nlj.GetResultValue().(*values.RecordConstructorValue)
	if !isRC || path[0] < 0 || path[0] >= len(rc.Fields) {
		return 0, false
	}
	slotLeg, isSlotLeg := values.AsQuantifiedObjectValue(rc.Fields[path[0]].Value)
	if !isSlotLeg {
		return 0, false
	}
	firstLeg, secondLeg, firstAlias, secondAlias := joinLegDerivationOrder(nlj)
	switch strings.ToUpper(slotLeg.Correlation().Name()) {
	case firstAlias:
		return legSlotIndex(firstLeg, md, path[1:])
	case secondAlias:
		index, ok := legSlotIndex(secondLeg, md, path[1:])
		if !ok {
			return 0, false
		}
		return len(deriveColumnsFromPlan(firstLeg, md)) + index, true
	}
	return 0, false
}

// joinLegDerivationOrder returns the join's legs in the order
// deriveColumnsFromJoin merges their columns, with their aliases uppercased.
// Both sites must make the same first/second decision or an index computed
// against one describes the other.
func joinLegDerivationOrder(
	nlj *plans.RecordQueryNestedLoopJoinPlan,
) (firstLeg, secondLeg plans.RecordQueryPlan, firstAlias, secondAlias string) {
	outerAlias := strings.ToUpper(nlj.GetOuterAlias().Name())
	innerAlias := strings.ToUpper(nlj.GetInnerAlias().Name())
	if joinResultValueIsReversed(nlj.GetResultValue(), outerAlias, innerAlias) {
		return nlj.GetInner(), nlj.GetOuter(), innerAlias, outerAlias
	}
	return nlj.GetOuter(), nlj.GetInner(), outerAlias, innerAlias
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
	if qov, ok := values.AsQuantifiedObjectValue(v); ok {
		return qov.Correlation(), true
	}
	if field, ok := values.AsFieldValue(v); ok {
		return valueRootCorrelation(field.ChildValue())
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
	// Null extension is a property of the join edge, not of the stored table
	// descriptor nor of a derived leg's pre-join exact type. Apply it before
	// any SQL-order reversal so it stays attached to the physical role that
	// supplies NULLs. This is especially important for synthesized NOT NULL
	// columns such as projected EXISTS flags.
	switch nlj.GetJoinType() {
	case plans.JoinLeftOuter:
		innerCols = columnsWithNullable(innerCols)
	case plans.JoinFullOuter:
		outerCols = columnsWithNullable(outerCols)
		innerCols = columnsWithNullable(innerCols)
	}

	outerAlias := strings.ToUpper(nlj.GetOuterAlias().Name())

	firstCols, secondCols := outerCols, innerCols
	firstLeg, secondLeg, firstAlias, secondAlias := joinLegDerivationOrder(nlj)
	if firstAlias != outerAlias {
		firstCols, secondCols = innerCols, outerCols
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
	elemAlias, elemTypeName, elemValue := gatheredExplodeElement(nlj, md)
	// A merge that is merely MISORDERED is re-sequenced, not discarded: the RC's
	// baked ordinals name each slot's position in the merged physical row, so
	// the permutation is stated rather than guessed, and the qualified datum
	// keys survive. Only when the two cannot be aligned that way does the
	// RC-derived (bare-label) answer below take over. The gathered-unnest arm is
	// NOT re-sequenced — its merge is missing the element column outright, so
	// there is no permutation to find.
	// No baked-ordinal requirement: the mapping is by RESOLVED PATH through the
	// leg tree, which a resolved read carries whether or not the RC also wears
	// the baked marker. Requiring the marker left a four-way `SELECT *` (whose
	// top RC resolves but is not marked) reporting its columns in physical leg
	// order while the row it describes was in FROM order — metadata and row
	// disagreeing, which the positional read refuses outright.
	if mergedDivergesFromRV && elemAlias == "" {
		resolveSlot := func(alias string, path []int) (int, bool) {
			if alias == firstAlias {
				return legSlotIndex(firstLeg, md, path)
			}
			return legSlotIndex(secondLeg, md, path)
		}
		if resequenced, ok := mergedInRVOrder(
			rc, merged, firstAlias, secondAlias, len(firstCols), resolveSlot); ok {
			return resequenced
		}
	}
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
			// message element types), so fall back on the element type
			// gatheredExplodeElement resolved from the array column's own
			// proto field — the ground truth: a repeated field's Kind IS its
			// element kind, with non-UUID messages reporting STRUCT
			// (java.sql.Types.STRUCT), never the BIGINT fallback that silently
			// mistyped struct elements.
			if strings.EqualFold(f.Name, elemAlias) && unknownTypedValue(f.Value) {
				tn := ""
				if elemValue != nil {
					tn = valueTypeName(elemValue, nil)
				}
				if tn == "" || tn == "UNKNOWN" {
					tn = elemTypeName
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

func columnsWithNullable(columns []executor.ColumnDef) []executor.ColumnDef {
	result := append([]executor.ColumnDef(nil), columns...)
	for i := range result {
		result[i].Nullable = api.ColumnNullable
	}
	return result
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
// binding alias (the FlatMap's inner correlation, the AS alias), the element's
// TYPE NAME as resolved from the array column's own proto field, and a value
// typed as the Explode's collection ELEMENT when the plan-level type survived
// (a STRUCT element's values.Type is Unknown — 7.6 does not model message
// element types — so the caller falls back on the resolved type name).
// ("", "", nil) when no such leg exists.
//
// The second result is a TYPE name, never a COLUMN name. It used to be the
// collection reference's display field, and the caller then re-resolved that
// string against every join leg's descriptor — so two legs carrying an array
// column of the same leaf name and different element kinds reported the FIRST
// leg's kind for the OTHER leg's element (`SELECT * FROM WS AS A, WX AS B,
// B."SITEMS" AS "EL"` typed a struct element BIGINT). Resolution happens once,
// here, by identity, against the leg the Explode actually reads (RFC-197).
func gatheredExplodeElement(p plans.RecordQueryPlan, md *recordlayer.RecordMetaData) (string, string, values.Value) {
	if fm, ok := p.(*plans.RecordQueryFlatMapPlan); ok {
		if exp := findExplodePlan(fm.GetInner()); exp != nil {
			elemTypeName := explodeElementTypeName(exp, fm.GetOuter(), md)
			collType := exp.GetCollectionValue().Type()
			if arr, isArr := collType.(*values.ArrayType); isArr && arr.ElementType != nil {
				element, err := values.NewQuantifiedObjectValue(
					fm.GetInnerAlias(), arr.ElementType,
				)
				if err == nil {
					return fm.GetInnerAlias().Name(), elemTypeName, element
				}
			}
			return fm.GetInnerAlias().Name(), elemTypeName, nil
		}
	}
	if nlj, ok := p.(*plans.RecordQueryNestedLoopJoinPlan); ok {
		if a, tn, v := gatheredExplodeElement(nlj.GetOuter(), md); a != "" {
			return a, tn, v
		}
		return gatheredExplodeElement(nlj.GetInner(), md)
	}
	if ip, ok := p.(innerPlan); ok {
		return gatheredExplodeElement(ip.GetInner(), md)
	}
	return "", "", nil
}

// explodeElementTypeName resolves the ELEMENT type name of the array column an
// Explode iterates, from that column's own proto field — the ground truth when
// the plan-level element type was erased. Java never loses it: a repeated
// field's element type is derived structurally at Type construction
// (Type.java:452-455 → fromProtoTypeToArray :492-533, reached from
// Record.Field.fromDescriptor :2866), so the element type rides the Type and
// is never re-derived from a column name later. Go's leg-row builder collapses
// every repeated field to UNKNOWN (cascades_translator.go fieldTypeForFD), and
// this is where that erasure is repaired.
//
// The array column is identified by IDENTITY, not by its display name: the
// collection reference's ordinal, checked against the LEG ROW LAYOUT it
// indexes. Two structural facts make the ordinal usable against the proto
// descriptor, and both are checked rather than assumed:
//
//   - The leg row's column order IS the descriptor's declared field order —
//     the layout is built by iterating that descriptor
//     (cascades_translator.go tableColumns). The check is a whole-layout
//     signature match (descriptorOrdinalDomain against the reference's own
//     domain token), not a per-column name match, so a leg whose row type was
//     derived some other way declines instead of indexing the wrong slot.
//   - Only the legs the Explode's own FlatMap reads are consulted. The value
//     reads a column off that FlatMap's outer quantifier, so the far side of
//     the join is not a candidate at all.
//
// Anything the identity cannot answer for — a lazy or fused reference, a
// childless one, an unknown domain, a layout no leg descriptor matches — fails
// closed and returns "", leaving the element column's type as derived
// elsewhere. A declined type refinement is recoverable; a wrong column's kind
// is not.
//
// Measured, so the next reader does not re-litigate it: once the search is
// scoped to the reference's own leg AND the layouts are checked to agree,
// indexing by ordinal and looking the field up by name pick the SAME field —
// proto field names are unique within a descriptor, and the signature check
// has already required the two ordered name lists to be equal. The behavioural
// defect came from the name ESCAPING to a caller that held every leg of the
// join and no way to tell which one the reference read. That is what the
// ordinal removes: not a different answer here, but the possibility of the
// question being asked anywhere else.
func explodeElementTypeName(exp *plans.RecordQueryExplodePlan, leg plans.RecordQueryPlan, md *recordlayer.RecordMetaData) string {
	fv, isFV := values.AsFieldValue(exp.GetCollectionValue())
	if !isFV {
		return ""
	}
	qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue())
	if !isQOV {
		return ""
	}
	id, ok := values.CorrelatedFieldIdentityIn(fv, values.OrdinalDomainOfQuantified(qov))
	if !ok {
		return ""
	}
	for _, d := range allLeafDescriptors(leg, md) {
		if descriptorOrdinalDomain(d) != id.Domain {
			continue
		}
		if id.Ordinal >= d.Fields().Len() {
			return ""
		}
		return arrayElementTypeNameOfField(d.Fields().Get(id.Ordinal))
	}
	return ""
}

// descriptorOrdinalDomain derives a record type's ordinal domain from its proto
// descriptor — the CONSUMER-side derivation the token exists for: the signature
// is the layout's ordered column-name list, derivable independently by the
// producer that resolved a name against a declared column order and by the
// consumer that holds the descriptor-shaped row type. Equality of the two is
// the soundness condition for reusing an ordinal across them.
func descriptorOrdinalDomain(d protoreflect.MessageDescriptor) values.OrdinalDomain {
	fields := d.Fields()
	names := make([]string, fields.Len())
	for i := range names {
		names[i] = string(fields.Get(i).Name())
	}
	return values.OrdinalDomainOfColumnNames(names)
}

// arrayElementTypeNameOfField reports the JDBC type name of a repeated proto
// field's ELEMENT. A repeated field's Kind IS its element kind; a non-UUID
// message element is a STRUCT column (java.sql.Types.STRUCT).
func arrayElementTypeNameOfField(fd protoreflect.FieldDescriptor) string {
	// The EFFECTIVE repeated field: a NullableArrayWrapper column's element
	// kind lives on the wrapper's `values` field.
	inner, _, ok := values.EffectiveListField(fd)
	if !ok {
		return ""
	}
	fd = inner
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
	if qov, ok := values.AsQuantifiedObjectValue(fm.GetResultValue()); ok &&
		strings.EqualFold(qov.Correlation().Name(), fm.GetOuterAlias().Name()) {
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
	fv, ok := values.AsFieldValue(f.Value)
	if !ok {
		return false
	}
	qov, ok := values.AsQuantifiedObjectValue(fv.ChildValue())
	if !ok || !strings.EqualFold(qov.Correlation().Name(), innerAlias) {
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
		//
		// THE THIRD MIRROR NOW READS THE AUTHORITY INSTEAD OF RE-DERIVING IT.
		// It used to hand-copy the FieldValue-vs-ColumnNameValue rule, and a
		// hand-copied rule is one that can be corrected in two places and
		// missed in the third — which is exactly what a nested key would have
		// done: the authority takes the resolved PATH, and a mirror still
		// reading the flat root would key this column `N` against a row the
		// cursor wrote under `N.SK`, serving NULL.
		name := expressions.AggregateKeyColumnName(k)
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
	return cascadesTypeName(v.Type())
}

// cascadesTypeName is the SQL type NAME of a cascades Type — the tail of
// valueTypeName, split out because the ARRAY arm needs to ask the same question
// of its element type and a value is the wrong thing to synthesize for that.
//
// "" means "this type has no ResultSet name here"; every caller has its own
// fallback for that and none of them wants a guess.
func cascadesTypeName(t values.Type) string {
	if t == nil {
		return ""
	}
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
	case values.TypeCodeBytes:
		// BINARY, not "BYTES": the JDBC type name for a SQL binary column, which
		// is what the descriptor-side authority protoKindToTypeName already
		// returns for BytesKind. The DDL keyword stays BYTES. Missing, this
		// branch cost the same as the ARRAY one below and for the same reason —
		// a BYTES member of a struct has no descriptor to fall back on, so it
		// reported UNKNOWN where the identical column at top level reported
		// BINARY.
		return "BINARY"
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
	case values.TypeCodeArray:
		// The ELEMENT's name, which is CQ-74's truncation and NOT a fresh
		// decision: a TOP-LEVEL array column already reports the bare element
		// type, because its stored descriptor resolves and protoFieldTypeName
		// reads the repeated field's kind (TestFDB_ArrayColumnMetadataIsTruncated
		// is that behaviour's live sentinel, and it is where this changes back).
		// An array leaf reached through a STRUCT PATH has no descriptor to
		// resolve — descriptorForColumn matches BARE names against the join-leaf
		// descriptors and a struct member is not a top-level field of any of
		// them — so without this arm it fell to "" and then to "UNKNOWN", and one
		// array answered two ways depending on how it was addressed.
		if at, ok := t.(*values.ArrayType); ok {
			return cascadesTypeName(at.ElementType)
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
	if field, ok := values.AsFieldValue(v); ok {
		if field.Path().Len() > 1 {
			return valueTypeName(v, desc)
		}
		if desc != nil {
			if n := protoFieldTypeName(desc, field.DisplayName()); n != "UNKNOWN" {
				return n
			}
		}
		return valueTypeName(v, desc)
	}
	switch t := v.(type) {
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
		// A NULLABLE array column stores through the NullableArrayWrapper;
		// the reported type name stays the measured CQ-74 truncation (the
		// bare ELEMENT kind), same as a flat repeated field — the wrapper is
		// storage shape, not a type.
		if inner, wrapped, _ := values.EffectiveListField(fd); wrapped {
			fd = inner
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
	// An exact inline VALUES leaf is another real correlated owner. It has no
	// catalog descriptor by design; its frozen logical row is the authority
	// that translateUnnestJoin uses to classify the array path. Do not let this
	// early table-error echo mask that exact classification with 42809.
	if logical.FindOwnerInlineValues(left, u.Segments[0]) != nil {
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
	// Present field: an array (flat repeated OR NullableArrayWrapper) is a
	// genuine unnest (not rejected); a scalar is the "repeated type" assert
	// → WRONG_OBJECT_TYPE.
	_, _, isArr := values.EffectiveListField(fd)
	return !isArr
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
					// This projection carries ProjectionRefs, so the split has a
					// counterparty and the census can say whether it is
					// redundant. It matters more here than anywhere else in the
					// family: a disagreement does not merely resolve the wrong
					// row, it RAISES ErrCodeUndefinedColumn on a column the
					// parser saw perfectly well.
					recordProjQualVsScan(proj, i, upper, ref)
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
					// The __ROW_VERSION pseudo-column resolves whenever the
					// metadata stores row versions (Java appends it to every
					// planner-facing type — RecordMetaData.getPlannerType,
					// RecordMetaData.java:732-739); when the descriptor
					// declares a REAL field of that name the descriptor check
					// below accepts it anyway (real-column-wins). With
					// store_row_versions=false the pseudo-column does not
					// exist and the ordinary 42703 below fires — Java:
					// "Attempting to query non existing column __ROW_VERSION"
					// (IndexTest.java:952-960).
					if upper == values.PseudoFieldRowVersion && md.IsStoreRecordVersions() {
						continue
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

// BuildSchemaTemplateFromDDL parses schemaDDL as a single CREATE SCHEMA
// TEMPLATE statement (auto-wrapping bare CREATE TABLE/INDEX clauses) and
// builds the RecordLayerSchemaTemplate without any catalog write. It is the
// programmatic entry to the exact metadata the DDL path produces — used by
// wire-level index tests that need the DDL-generated metadata against a real
// record store.
func BuildSchemaTemplateFromDDL(schemaDDL string) (*metadata.RecordLayerSchemaTemplate, error) {
	return buildSchemaTemplateFromDDL(schemaDDL)
}

// BuildSchemaTemplateFromDDLNamed is BuildSchemaTemplateFromDDL with an
// explicit template name for bare clause bodies. The name matters at the
// wire level: it is the descriptor FILE name inside the persisted
// RecordMetaData, so a cross-engine byte comparison must build under the
// same name Java persisted. Quoted to preserve case (Java's harness sets
// the name programmatically, case intact).
func BuildSchemaTemplateFromDDLNamed(schemaDDL, name string) (*metadata.RecordLayerSchemaTemplate, error) {
	if startsWithCreateSchemaTemplate(schemaDDL) {
		return buildSchemaTemplateFromDDL(schemaDDL)
	}
	return buildSchemaTemplateFromDDL(`CREATE SCHEMA TEMPLATE "` + name + `" ` + schemaDDL)
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

	templateID := trimIdentifierQuotes(stCtx.SchemaTemplateId().GetText())
	b := metadata.NewSchemaTemplateBuilder().SetName(templateID)
	// WITH OPTIONS(...) — the same three options execCreateSchemaTemplate
	// applies, parsed BEFORE the table/index passes because they change how
	// Build() compiles primary keys (intermingle) and whether the
	// __ROW_VERSION pseudo-column exists for index planning
	// (store_row_versions). Silently dropping them here built metadata that
	// DIVERGED from what the production DDL path builds for the same text.
	if oc := stCtx.OptionsClause(); oc != nil {
		for _, opt := range oc.AllOption() {
			switch {
			case opt.ENABLE_LONG_ROWS() != nil:
				b.SetEnableLongRows(opt.BooleanLiteral().TRUE() != nil)
			case opt.INTERMINGLE_TABLES() != nil:
				b.SetIntermingleTables(opt.BooleanLiteral().TRUE() != nil)
			case opt.STORE_ROW_VERSIONS() != nil:
				b.SetStoreRowVersions(opt.BooleanLiteral().TRUE() != nil)
			default:
				return nil, fmt.Errorf("unknown option in schema template creation: %s", opt.GetText())
			}
		}
	}
	if rejErr := rejectUnsupportedTemplateClauses(stCtx.AllTemplateClause()); rejErr != nil {
		return nil, rejErr
	}
	if serr := registerStructDefinitions(stCtx.AllTemplateClause(), b); serr != nil {
		return nil, serr
	}
	for _, clause := range stCtx.AllTemplateClause() {
		td := clause.TableDefinition()
		if td == nil {
			continue
		}
		// Normalize the table name the same way execCreateSchemaTemplate and
		// the column/index parsers do (StripIdentifierQuotes upper-cases
		// unquoted identifiers), so index lookups by table name match.
		tableName := functions.StripIdentifierQuotes(td.Uid().GetText())
		cols, pkCols, tdErr := parseTableDefinition(td, b)
		if tdErr != nil {
			return nil, fmt.Errorf("table %q: %w", tableName, tdErr)
		}
		b.AddTablePrimaryKeyPaths(tableName, cols, pkCols)
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

// env returns the DST environment of the database this page reads from, or nil when the
// connection has no session/database (a construction the resource-limit unit tests use). nil is
// production: wall clock, unchanged behaviour.
func (r *paginatingRows) env() *dst.Env {
	if r.conn == nil || r.conn.sess == nil || r.conn.sess.DB == nil {
		return nil
	}
	return r.conn.sess.DB.Env()
}

// statisticsMaxAgeVersions bounds how stale a collected set may be, expressed in
// FDB VERSIONS rather than wall-clock nanoseconds.
//
// Versions are the cluster's own clock: monotone within it, and immune to skew
// between the host that ran `frl stats collect` and the host planning the query.
// A wall-clock comparison across two machines can make an entry immortal if the
// collector's clock runs fast, which would quietly defeat the whole freshness
// gate — and with it the argument that an orphaned entry is harmless because it
// is old.
//
// FDB targets ~1e6 versions/second, so this is ~24h.
const statisticsMaxAgeVersions int64 = 24 * 60 * 60 * 1_000_000

// fetchCollectedStatistics returns per-record-type row counts collected by the
// offline collector (RFC-236), or nil to plan on the cost model's constant.
//
// FOUR GATES, cheapest refusal first. Absent, expired, incomplete and failed all
// produce the same outcome — today's plan — because a partial answer here is
// worse than none: a refusal returns LeafScanCardinality = 1e6, larger than
// almost any real count, so one missing type standing beside a real one ranks
// the missing table as the biggest in the schema and drives the join from the
// wrong side.
//
// COMPLETENESS IS SCHEMA-WIDE, not query-wide, and that is deliberate on two
// counts. It is undecidable here — this runs before the planner exists, so which
// types a query will touch is unknown. And it would be insufficient even if it
// were decidable: FullUnorderedScanExpression SUMS per-type cardinalities
// (properties/cost.go), so one absent type inside one scan node yields
// 1e6 + realCount, an inversion BELOW the granularity a per-query gate is
// defined at. The cost, stated rather than discovered: one uncollected type
// disables statistics for every query in that schema.
//
// It opens no record store. The store's subspace is the schema subspace, which
// the keyspace already yields — so this read is independent of
// fetchIndexStateSnapshot, including its early return when a schema has no
// indexes at all. That case matters: a join between two index-less tables is
// exactly where cardinality alone decides the order.
func (g *cascadesGenerator) fetchCollectedStatistics(
	ctx context.Context,
	md *recordlayer.RecordMetaData,
	popts plannerOptions,
) properties.StatisticsProvider {
	c := g.c
	// GATE 1 — opt-in. Also in the plan-cache key, both halves (planner_options.go).
	if !popts.useCollectedStatistics {
		return nil
	}
	if c == nil || c.sess == nil || c.sess.DB == nil || md == nil {
		return nil
	}
	storeSubspace, err := c.sess.Keyspace.SchemaSubspace(c.sess.DBPath, c.sess.Schema)
	if err != nil {
		return nil
	}

	// GATE 2 — the read. Snapshot-only inside ReadStatistics: a planner read must
	// never add a conflict range, or planning could make a transaction retry.
	stats, ok, err := recordlayer.ReadStatistics(ctx, c.sess.DB,
		recordlayer.NewStatisticsSubspace(c.sess.Keyspace.StatisticsSubspace()), storeSubspace)
	if err != nil || !ok {
		return nil
	}

	// GATE 3 — freshness, judged on VERSIONS.
	readVersion, vErr := readCurrentVersion(ctx, c)
	if vErr != nil {
		return nil
	}
	age := readVersion - stats.CollectedAtVersion
	// A NEGATIVE age is not infinite freshness. A cluster restored from backup
	// moves versions backwards, and an entry stamped in the abandoned future
	// would otherwise never expire. Fail safe.
	if age < 0 || age > statisticsMaxAgeVersions {
		return nil
	}

	// GATE 4 — completeness over the whole schema.
	perType := make(map[string]float64, len(stats.PerType))
	for name := range md.RecordTypes() {
		st, present := stats.PerType[name]
		if !present {
			return nil
		}
		perType[name] = float64(st.Count)
	}
	if len(perType) == 0 {
		return nil
	}
	return properties.NewCollectedStatistics(perType)
}

// readCurrentVersion reads the cluster's current version for the freshness gate.
// Snapshot semantics: it is a read version, not a write, and adds no conflict.
func readCurrentVersion(ctx context.Context, c *EmbeddedConnection) (int64, error) {
	v, err := c.sess.DB.RunRead(ctx, func(rtx fdb.ReadTransaction) (any, error) {
		return rtx.GetReadVersion().Get()
	})
	if err != nil {
		return 0, err
	}
	n, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("read version has type %T", v)
	}
	return n, nil
}
