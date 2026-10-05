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
	"fdb.dev/pkg/relational/core/query/semantic"
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
	// args are the statement's driver arguments; paramKey renders their
	// bindings for the plan-cache key, since bound constants are planned in.
	args     []driver.NamedValue
	paramKey string
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
	if err := checkDecimalConstants(root); err != nil {
		return nil, err
	}
	if mayCallSQLFunction(root) && g.c.ensureMetaData(ctx) == nil {
		expanded, changed, err := expandSQLFunctions(sql, root, metaDataFunctions(g.c.cachedMetaData()))
		if err != nil {
			return nil, err
		}
		if changed {
			if root, err = parser.Parse(expanded); err != nil {
				return nil, err
			}
		}
	}
	paramKey, release, err := bindStatementParameters(root, g.args)
	if err != nil {
		return nil, err
	}
	g.paramKey = paramKey
	g.c.releaseParams = append(g.c.releaseParams, release)

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
	if statementOptionsFor(stmt, c.Options()).snapshot && !snapshotAdmits(stmt) {
		return nil, errSnapshotOnlySelect()
	}

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
					if op, err := buildLogicalPlanForQueryWithTemplate(q, md, g.sessionTemplate()); err == nil && op != nil {
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

	return g.planSelectCascades(ctx, q, md, true, statementOptionsFor(sel, g.c.Options()))
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
				if op, err := buildLogicalPlanForQueryWithTemplate(q, md, g.sessionTemplate()); err == nil && op != nil {
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
func (g *cascadesGenerator) planSelectCascades(ctx context.Context, q antlrgen.IQueryContext, md *recordlayer.RecordMetaData, logMetrics bool, so statementOptions) (plan query.Plan, err error) {
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
		ls.setLogQuery(so.logQuery)
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
	// (case-sensitive) and the token-rendered query text.
	if so.rightDeep {
		popts.config.ShouldJoinRightDeep = true
	}
	cacheScope := planCacheScope(g.c.sess.DBPath, g.c.sess.Schema, md.Version(), popts.cacheKeyPart())
	cacheSQL := planCacheText(q) + g.paramKey
	cache := g.cache
	// A temporary function is not part of the schema version the key names.
	if so.noCache || (g.c.activeTx != nil && len(g.c.activeTx.tempFunctions) > 0) {
		cache = nil
	}

	if cache != nil {
		if cachedPlan, cachedSubs, cachedLabels, ok := cache.GetWithOutputLabels(cacheScope, cacheSQL); ok {
			ls.setPlan(cachedPlan)
			ls.setCache(PlanCacheHit)
			return &cascadesPlan{
				snapshot:         so.snapshot,
				conn:             g.c,
				md:               md,
				physicalPlan:     cachedPlan,
				explain:          cachedPlan.Explain(),
				scalarSubqueries: cachedSubs,
				outputLabels:     cachedLabels,
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

	visitor := NewPlanVisitorWithTemplate(md, g.sessionTemplate())
	logicalOp, buildErr := visitor.VisitQuery(q)
	if buildErr == nil {
		buildErr = rejectArrayAggOrderBy(q)
	}
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

	if err := runFromResolutionPostPasses(logicalOp, g.sessionTemplate(), md, g.c.cachedMetaData()); err != nil {
		return nil, err
	}
	outputLabels, labelErr := query.ExactLogicalOutputLabels(logicalOp, md, nil)
	if labelErr != nil {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"query has no exact output-label contract: %v", labelErr)
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
	stats, structurallyRefused := g.fetchCollectedStatistics(ctx, md, popts)
	if stats == nil && !structurallyRefused {
		// Only ABSENCE falls through to the legacy source. A structural refusal
		// is a statement about the SCHEMA, so no other count source is safe for
		// it either -- and the legacy path is live precisely for the hand-built
		// metadata that can declare synthetic types.
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
	if err := cascades.FinalizePlan(physPlan); err != nil {
		return nil, api.NewError(api.ErrCodeInternalError, "result descriptor: "+err.Error())
	}
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
	if cache != nil {
		ls.setCache(PlanCacheMiss)
		cache.PutWithOutputLabels(cacheScope, cacheSQL, physPlan, scalarSubs, outputLabels)
	} else {
		ls.setCache(PlanCacheSkip)
	}
	return &cascadesPlan{
		snapshot:         so.snapshot,
		conn:             g.c,
		md:               md,
		physicalPlan:     physPlan,
		explain:          physPlan.Explain(),
		scalarSubqueries: scalarSubs,
		outputLabels:     outputLabels,
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
		if op, err := buildLogicalPlanForQueryWithTemplate(q, md, g.sessionTemplate()); err == nil && op != nil {
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
		plan, planErr := g.planSelectCascades(ctx, q, freshMd, false, statementOptionsFor(d, g.c.Options()))
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
			if op, _ := buildLogicalPlanForDeleteWithCatalog(del, md, g.sessionTemplate()); op != nil {
				return explainWithContext(ctx, func() string { return op.Explain("") })
			}
		}
		if op := buildLogicalPlanForDelete(del); op != nil {
			return explainWithContext(ctx, func() string { return op.Explain("") })
		}
	}
	if ins := d.InsertStatement(); ins != nil {
		if md != nil {
			if op, _ := buildLogicalPlanForInsertWithCatalog(ins, md, g.sessionTemplate()); op != nil {
				return explainWithContext(ctx, func() string { return op.Explain("") })
			}
		}
		if op := buildLogicalPlanForInsert(ins); op != nil {
			return explainWithContext(ctx, func() string { return op.Explain("") })
		}
	}
	if upd := d.UpdateStatement(); upd != nil {
		if md != nil {
			if op, _ := buildLogicalPlanForUpdateWithCatalog(upd, md, g.sessionTemplate()); op != nil {
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
						if op, _ := buildLogicalPlanForDeleteWithCatalog(del, md, g.sessionTemplate()); op != nil {
							return op.Explain("")
						}
					}
					if op := buildLogicalPlanForDelete(del); op != nil {
						return op.Explain("")
					}
				}
				if upd := dml.UpdateStatement(); upd != nil {
					if md != nil {
						if op, _ := buildLogicalPlanForUpdateWithCatalog(upd, md, g.sessionTemplate()); op != nil {
							return op.Explain("")
						}
					}
					if op := buildLogicalPlanForUpdate(upd); op != nil {
						return op.Explain("")
					}
				}
				if ins := dml.InsertStatement(); ins != nil {
					if md != nil {
						if op, _ := buildLogicalPlanForInsertWithCatalog(ins, md, g.sessionTemplate()); op != nil {
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
	if err := rejectArrayAggOrderBy(dml); err != nil {
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
	if ls != nil {
		ls.setLogQuery(statementOptionsFor(dml, c.Options()).logQuery)
	}
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
	// dryRunSave/DeleteRecordAsync. The flag is the statement's typed OPTIONS clause
	// merged with the connection's DRY_RUN (Java's PlanGenerator merge, :170), decided
	// here per statement (DML plans are never cached) and carried on the cascadesPlan →
	// paginatingRows.dryRun → ExecuteProperties.DryRun, where executeInsert/Update/Delete
	// branch onto the store DryRun* primitives. A connection DRY_RUN set through SetOption
	// lasts one pool borrow: ResetSession restores the connector's options, so the next
	// borrower's plain DML never silently no-ops. NOCACHE/LOG
	// QUERY remain accepted-and-ignored hints. Detection walks the whole DML subtree so the
	// INSERT…SELECT spelling — whose OPTIONS the grammar attaches to the inner SELECT, not
	// insertStatement.queryOptions — cannot silently bypass DRY RUN and commit.
	dryRun := statementOptionsFor(dml, g.c.Options()).dryRun

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
		logicalOp, delErr = buildLogicalPlanForDeleteWithCatalog(del, md, g.sessionTemplate())
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
		logicalOp, updErr = buildLogicalPlanForUpdateWithCatalog(upd, md, g.sessionTemplate())
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
		logicalOp, insErr = buildLogicalPlanForInsertWithCatalog(ins, md, g.sessionTemplate())
		if insErr != nil {
			// A carried SQLSTATE from the INSERT … SELECT body build (RFC-142:
			// AT-on-a-table comma source → WRONG_OBJECT_TYPE) — surface it.
			return nil, insErr
		}
	}
	if logicalOp == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "DML logical plan failed")
	}

	if err := resolveQualifiedTableNames(logicalOp, g.sessionTemplate()); err != nil {
		return nil, err
	}

	// DML target-table existence: surface a clean 42F01 (matching INSERT INTO <missing>
	// and the SELECT path), not a downstream generic 0AF00 "DML Cascades translation
	// failed". Run AFTER resolveQualifiedTableNames so (a) a BAD template qualifier's 42F00
	// already errored above and takes precedence, and (b) a VALID qualifier (or none) has
	// been stripped to the bare Target, which is checked here — so `DELETE FROM
	// <session_schema>.missing` and `DELETE FROM missing` both get 42F01, while
	// `DELETE FROM badschema.missing` keeps its 42F00.
	var dmlTarget string
	switch dop := logicalOp.(type) {
	case *logical.LogicalDelete:
		dmlTarget = dop.Target
	case *logical.LogicalUpdate:
		dmlTarget = dop.Target
	case *logical.LogicalInsert:
		// INSERT belongs here too, and its absence was the worst of the three.
		// INSERT ... VALUES has its own strict check further down, but
		// INSERT ... SELECT had NO planning-time target check at all: the target
		// is resolved lazily per row in the executor, so a source that produces
		// rows raises a raw non-SQLSTATE `executor: INSERT target record type %q
		// not found`, and a source that produces NONE never reaches the lookup
		// and the statement reports SUCCESS against a table that does not exist.
		dmlTarget = dop.Table
	}
	// STRICT resolution for a DML target -- GetRecordType, not the case-folding
	// recordTypeCI. An unquoted `DELETE FROM customer` against a table declared
	// `"Customer"` is UNDEFINED, and every other path already says so: the SELECT
	// path rejects it, INSERT ... VALUES rejects it (`md.GetRecordType(insOp.Table)`
	// below), and Java rejects it -- `select * from restaurant` against a table
	// declared `"Restaurant"` throws UNDEFINED_TABLE / "Unknown table RESTAURANT"
	// (CaseSensitivityQueryTests.caseSensitiveConnectionTestCase3).
	//
	// Folding here was worse than a validation divergence. It ACCEPTED the
	// statement and then carried the SQL-normalised `CUSTOMER` into the plan,
	// where no record type answers to it: the scan matched nothing and the DELETE
	// reported success having removed no rows. Canonicalising the name instead
	// would be worse still -- it would let an unquoted write mutate a table that
	// can only be named with quotes.
	// The target is bare here: resolveQualifiedTableNames stripped any
	// qualifier from its segments, and a dot left in it belongs to the name.
	if dmlTarget != "" && md.GetRecordType(dmlTarget) == nil {
		return nil, api.NewErrorf(api.ErrCodeUndefinedTable, "Unknown table %s", strings.ToUpper(dmlTarget))
	}

	// SOURCE tables too, and AFTER the target check so the target keeps its own
	// wording. Both raise 42F01, but they say it differently -- "Unknown table X"
	// here versus the SELECT path's `table "X" does not exist` -- and the
	// cross-engine corpus pins the target one. Running the source sweep first
	// silently re-worded every `DELETE FROM nosuchtable`, which the corpus caught
	// as an error-wording divergence rather than as anything about sources.
	if err := validateScanTables(logicalOp, md); err != nil {
		return nil, err
	}

	// INSERT … SELECT with an explicit column list is rejected (Java:
	// "setting column ordering for insert with select is not supported").
	if insOp, ok := logicalOp.(*logical.LogicalInsert); ok && insOp.Source != nil && len(insOp.Columns) > 0 {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"setting column ordering for insert with select is not supported")
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
	// the resolver can't build a Value for, e.g. UPPER). A NULL assigned to a
	// field that cannot hold it is Java's run-time NULL_ASSIGNMENT, refused
	// by the executor when a row is transformed (MessageHelpers.coerceObject),
	// not here.
	if _, ok := logicalOp.(*logical.LogicalUpdate); ok {
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
	}

	if fn := query.FindUnsupportedFunction(logicalOp); fn != "" {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"Unsupported operator "+fn)
	}

	var outputLabels []string
	if elements := returningSelectElements(dml); elements != nil {
		returning, err := buildReturning(logicalOp, elements, md, g.sessionTemplate())
		if err != nil {
			return nil, err
		}
		if fn := query.FindUnsupportedFunction(returning); fn != "" {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery, "Unsupported operator "+fn)
		}
		if outputLabels, err = query.ExactLogicalOutputLabels(returning, md, nil); err != nil {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"RETURNING has no exact output-label contract: %v", err)
		}
		logicalOp = returning
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
	dmlStats, dmlStructurallyRefused := g.fetchCollectedStatistics(ctx, md, popts)
	if dmlStats == nil && !dmlStructurallyRefused {
		// Same rule as the SELECT path: a structural refusal suppresses the
		// legacy source too. This second site was NOT named in the review that
		// found the first -- the compiler surfaced it when the signature changed,
		// which is the argument for changing the signature rather than adding a
		// guard at the one call site somebody happened to look at.
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
	if err := cascades.FinalizePlan(physPlan); err != nil {
		return nil, api.NewError(api.ErrCodeInternalError, "result descriptor: "+err.Error())
	}

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
		outputLabels:      outputLabels,
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
					if op, _ := buildLogicalPlanForDeleteWithCatalog(del, md, g.sessionTemplate()); op != nil {
						return op.Explain("")
					}
				}
				if op := buildLogicalPlanForDelete(del); op != nil {
					return op.Explain("")
				}
			}
			if upd := dml.UpdateStatement(); upd != nil {
				if md != nil {
					if op, _ := buildLogicalPlanForUpdateWithCatalog(upd, md, g.sessionTemplate()); op != nil {
						return op.Explain("")
					}
				}
				if op := buildLogicalPlanForUpdate(upd); op != nil {
					return op.Explain("")
				}
			}
			if ins := dml.InsertStatement(); ins != nil {
				if md != nil {
					if op, _ := buildLogicalPlanForInsertWithCatalog(ins, md, g.sessionTemplate()); op != nil {
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
	// outputLabels is the top-level SQL publication contract. It is parallel to
	// the physical result row but intentionally not the same as its protobuf-safe,
	// deduplicated field names (for example [G,G] over physical [G,G_2]).
	outputLabels []string

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

	// dryRun carries the statement's merged DRY RUN (its OPTIONS clause or the
	// connection's DRY_RUN) from planDML to execution: one cascadesPlan per
	// statement → paginatingRows.dryRun → ExecuteProperties.DryRun, so the DML
	// executor previews via the store DryRun* primitives instead of mutating.
	dryRun bool
	// snapshot is the statement-scoped OPTIONS (ISOLATION LEVEL SNAPSHOT) flag:
	// the statement's reads take no read-conflict ranges.
	snapshot bool
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

	cols := resultColumns(p.physicalPlan)
	if p.outputLabels != nil {
		if len(p.outputLabels) != len(cols) {
			return query.Result{}, api.NewErrorf(api.ErrCodeInternalError,
				"result label width %d does not match physical row width %d", len(p.outputLabels), len(cols))
		}
		for i, label := range p.outputLabels {
			if label == "" {
				label = values.OrdinalFieldName(i)
			}
			cols[i].Label = label
		}
	}

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
		opts:             c.Options(),
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
		snapshot:       p.snapshot,
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

	// SQL DML owns a single commit boundary, not one retrying transaction per
	// page. The captured transaction and ownership are separate: a statement
	// must never commit or abort an application's explicit transaction.
	if pr.isUpdate && pr.tx == nil {
		tx, err := c.beginTransaction()
		if err != nil {
			pr.statsErr = err
			pr.Close()
			return query.Result{}, err
		}
		pr.tx, pr.ownsTx = tx, true
		cancelDone := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			tx.rctx.Cancel()
			close(cancelDone)
		})
		pr.stopTxCancellation = func() {
			if !stop() {
				<-cancelDone
			}
		}
		defer pr.Close()
		if err := c.ensureMetaData(ctx); err != nil {
			pr.statsErr = err
			return query.Result{}, err
		}
	}

	// Eagerly fetch the first page so execution errors (type mismatches,
	// plan failures) surface at QueryContext time, not during row iteration.
	if err := pr.fetchPage(); err != nil {
		// The statement is over and it FAILED; Close is the emission funnel,
		// so the error has to reach it. A statement killed here by a scan
		// limit still reports what it consumed — the counters were charged
		// per attempt on the way out, not at a success-only checkpoint.
		if pr.ownsTx && ctx.Err() != nil {
			err = translateExecErrorCtx(ctx, ctx.Err())
		}
		pr.statsErr = err
		pr.Close()
		return query.Result{}, err
	}

	// DML (INSERT/UPDATE/DELETE) plans emit one row per affected record;
	// the affected-row count is the JDBC update count, not a result set.
	// Drain and count, matching Java's AbstractEmbeddedStatement.countUpdates.
	// An owned transaction commits only after the complete drain succeeds.
	if pr.isUpdate {
		n, err := pr.countAll()
		if pr.ownsTx {
			pr.detachTxCancellation()
			if err != nil && ctx.Err() != nil {
				err = translateExecErrorCtx(ctx, ctx.Err())
			}
			if err == nil {
				if ctx.Err() != nil {
					err = translateExecErrorCtx(ctx, ctx.Err())
				} else {
					// Cancellation cannot cancel a dispatched commit: its actual
					// result decides success/failure/ambiguity, not ctx.Err().
					err = pr.tx.Commit()
				}
			}
		}
		// A failed statement cannot publish a successful affected-row count.
		if err == nil {
			pr.execLog.setRows(0, n)
		}
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
	ctx    context.Context
	cancel context.CancelFunc // statement-timeout cancel; nil when no timeout
	conn   *EmbeddedConnection
	// opts is the connection's option set captured when the statement
	// executed; every page reads it, so a SetOption between two pages of one
	// result does not change the rest of it (RFC-257 WS-E 6.2).
	opts             *api.Options
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

	// retryTimeLimit retains the reduced budget across successful pages. Zero
	// means the initial ceiling; explicit transactions never adapt or retry.
	retryTimeLimit time.Duration

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

	// dryRun is the statement's merged DRY RUN, propagated from the cascadesPlan
	// at construction and read in executeProps() into ExecuteProperties.DryRun.
	// A fresh paginatingRows per statement keeps it to that statement.
	dryRun bool
	// snapshot is the statement-scoped OPTIONS (ISOLATION LEVEL SNAPSHOT) flag:
	// the statement's reads take no read-conflict ranges.
	snapshot bool

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

	// tx is either the explicit transaction that was open when Execute ran, the
	// statement-owned auto-commit DML transaction, or nil for auto-commit SELECT.
	// EVERY page of EVERY statement kind executes on it —
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

	// ownsTx marks a directly-created statement transaction. It commits once
	// after the complete DML drain; ambiguity is reported, never replayed.
	// Borrowed explicit transactions remain application-owned.
	ownsTx bool
	// stopTxCancellation detaches and drains the pre-commit cancellation hook.
	// It is consumed exactly once before commit or during terminal cleanup.
	stopTxCancellation func()

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

func (r *paginatingRows) detachTxCancellation() {
	if stop := r.stopTxCancellation; stop != nil {
		r.stopTxCancellation = nil
		stop()
	}
}

func (r *paginatingRows) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.detachTxCancellation()
	if r.ownsTx && !r.tx.terminated.Load() {
		// Commit terminates on every outcome, including ambiguity. Reaching
		// this arm means execution ended before commit and must be aborted.
		if err := r.tx.Rollback(); err != nil {
			r.statsErr = errors.Join(r.statsErr, err)
		}
	}
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
	case "STRING", "DATE", "TIMESTAMP":
		// A DATE or TIMESTAMP value is its canonical text.
		return reflect.TypeOf((*string)(nil)).Elem()
	case "BOOLEAN":
		return reflect.TypeOf((*bool)(nil)).Elem()
	case "BYTES", "BINARY":
		return reflect.TypeOf((*[]byte)(nil)).Elem()
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

// options is the option set captured when the statement executed (the
// connection's, for a result set built without one).
func (r *paginatingRows) options() *api.Options {
	if r.opts != nil {
		return r.opts
	}
	return r.conn.Options()
}

func (r *paginatingRows) pageTimeLimit() time.Duration {
	limit := txPageTimeLimit
	if r.retryTimeLimit > 0 && r.retryTimeLimit < limit {
		limit = r.retryTimeLimit
	}
	if millis := optInt64(r.options(), api.OptExecutionTimeLimit, 0); millis > 0 && millis <= txPageTimeLimit.Milliseconds() {
		limit = min(limit, time.Duration(millis)*time.Millisecond)
	}
	return limit
}

func (r *paginatingRows) executeProps() recordlayer.ExecuteProperties {
	// Anchor the scan/time budget on the database's env clock. This path ALWAYS arms a time
	// limit (txPageTimeLimit below), and that limit decides where a page ends and therefore
	// which continuation the caller gets — so a wall-clock anchor would make a simulated run
	// page differently depending on how fast the machine was. A nil env (production) is the
	// wall clock, unchanged.
	props := recordlayer.DefaultExecutePropertiesIn(r.env())

	// DRY RUN is the statement's merged flag (carried on paginatingRows from
	// the cascadesPlan), read from the field so a later change of the
	// connection's DRY_RUN cannot change a statement already executing.
	props = props.WithDryRun(r.dryRun)
	if r.snapshot {
		props.IsolationLevel = recordlayer.SnapshotIsolation
	}

	opts := r.options()

	// Per-page time limit. The connection option (if set) is intersected
	// with the per-transaction CAP (txPageTimeLimit, 4s) so the FDB 5s hard
	// wall is never exceeded: the 4s cap is the ceiling and a smaller user
	// limit only narrows it — a larger user value can never raise the page
	// budget past the cap.
	props = props.WithTimeLimit(r.pageTimeLimit())

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
		if attempts > 0 {
			// Java's ThrottledRetryingIterator decreases work on every failed
			// attempt. SQL pages use time rather than its scanned-row quota:
			// reduce by 10%, retaining a positive budget and cleanup margin.
			// A fast version clock can make FDB's MVCC window shorter than
			// four wall-clock seconds. Retrying the same budget then repeats
			// 1007 forever. Closure re-entry observes commit failures too,
			// which happen outside this callback; it also conservatively
			// reduces work for other retryable failures such as conflicts.
			r.retryTimeLimit = max(time.Millisecond, r.pageTimeLimit()*9/10)
		}
		attempts++
		// The driver budget governs an application's multi-statement explicit
		// transaction. An internally owned auto-commit DML transaction may span
		// pages, but its statement clock and FDB's own MVCC limit govern it; treating
		// it as explicit makes a backend clock injection pre-empt setup/seed DML.
		if r.tx != nil && !r.ownsTx {
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
		//
		// PeekIndexStates, the conflict-free read planning used: like Java's
		// plan-constraint check (DatabaseObjectDependenciesPredicate.java:98),
		// it adds no conflict. The scans this plan opens take a conflict key
		// for each index they scan; an index the statement never touches can
		// change state without aborting its transaction.
		if stateErr := validatePlanIndexDependencies(
			r.indexDependencies, store.GetRecordMetaData(), store.PeekIndexStates(),
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
				props.ScanState.BytesScanned()-scanBytesAtEntry,
			)
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
		row[i] = materializeDriverValue(enumValuesAsNames(v, rs.ColumnType(i+1)))
	}
	return row, nil
}

// enumValuesAsNames hands an enum column to the client as its value's name, as
// Java's RowStruct.getString does (ProtoUtils.toUserIdentifier of the value
// descriptor's name, RowStruct.java:214-215): in the value layer an enum
// carries its declared number, an int64 only its type tells apart from a
// BIGINT. An array of enums is the same per element. A number the enum does
// not declare is left as it is (a closed enum's undeclared number reads unset,
// so none reaches here).
func enumValuesAsNames(v any, t values.Type) any {
	switch typed := t.(type) {
	case *values.EnumType:
		if n, ok := v.(int64); ok {
			if member, found := typed.LookupValueByNumber(int32(n)); found { //nolint:gosec
				return member.Name
			}
		}
	case *values.ArrayType:
		if elems, ok := v.([]any); ok && typed.ElementType != nil {
			if _, isEnum := typed.ElementType.(*values.EnumType); isEnum {
				out := make([]any, len(elems))
				for i, e := range elems {
					out[i] = enumValuesAsNames(e, typed.ElementType)
				}
				return out
			}
		}
	}
	return v
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
	var nullElem *values.NullArrayElementError
	if errors.As(err, &nullElem) {
		return api.NewError(api.ErrCodeUnsupportedOperation, nullElem.Error())
	}
	var likeErr *values.LikeError
	if errors.As(err, &likeErr) {
		return api.NewError(likeErrorCode(likeErr.Kind), likeErr.Error())
	}
	var divZero *values.ArithmeticDivisionByZeroError
	if errors.As(err, &divZero) {
		return api.NewError(api.ErrCodeDivisionByZero, "/ by zero")
	}
	var overflow *values.ArithmeticOverflowError
	if errors.As(err, &overflow) {
		return api.NewError(api.ErrCodeNumericValueOutOfRange, overflow.Error())
	}
	var scalarMismatch *values.ScalarTypeMismatchError
	if errors.As(err, &scalarMismatch) {
		return api.NewError(api.ErrCodeCannotConvertType, scalarMismatch.Error())
	}
	var castErr *values.InvalidCastError
	if errors.As(err, &castErr) {
		return api.NewError(api.ErrCodeInvalidCast, castErr.Error())
	}
	// Java fails the same write with an unmapped VerifyException or
	// IllegalArgumentException, both XXXXX.
	var slotErr *values.SlotAssignmentError
	if errors.As(err, &slotErr) {
		return api.NewError(api.ErrCodeUnknown, slotErr.Error())
	}
	var enumErr *values.InvalidEnumValueError
	if errors.As(err, &enumErr) {
		return api.WrapError(api.ErrCodeInternalError, enumErr.Error(), err)
	}
	var uuidErr *values.InvalidUUIDValueError
	if errors.As(err, &uuidErr) {
		return api.WrapError(api.ErrCodeInternalError, uuidErr.Error(), err)
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

// likeErrorCode is ExceptionUtil's mapping of the LIKE semantic errors
// (ExceptionUtil.java:95-102).
func likeErrorCode(kind values.LikeErrorKind) api.ErrorCode {
	switch kind {
	case values.LikeEscapeNotSingleChar:
		return api.ErrCodeInvalidEscapeCharacter
	case values.LikeEscapeConflict:
		return api.ErrCodeEscapeCharacterConflict
	case values.LikeInvalidEscapeSequence:
		return api.ErrCodeInvalidEscapeSequence
	}
	return api.ErrCodeInvalidArgumentForFunction
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
		// PeekIndexStates, NOT GetAllIndexStatesMap. The two answer in
		// different DOMAINS: this one iterates the METADATA's indexes and
		// defaults an absent entry to READABLE; the raw map returns whatever
		// keys the index-state subspace happens to hold, including a key for a
		// name the metadata no longer has.
		//
		// And PeekIndexStates, NOT GetAllIndexStates: planning reads the states
		// the store loaded at open and adds NO conflict, as Java's PlanContext
		// does (PlanContext.java:237-260). Each scan takes its own index's
		// conflict key; a key for every index made a reader abort (1020) on a
		// state change of an index it never scanned, where Java commits
		// (RFC-257 WS-E 6.4, measured by indexStateReadScopeProbe).
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
		return store.PeekIndexStates(), nil
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
		// Physical names verbatim, as the index candidates take them: a quoted
		// lowercase key column is named "id", and folding it matches no field.
		aliases := make([]values.CorrelationIdentifier, len(pkCols))
		for i := range pkCols {
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
		// rt.Name is the STORED protobuf name, and that is correct here: it is
		// what Java's PrimaryScanMatchCandidate carries, it is injective by
		// construction, and it flows into physical plans and the continuation
		// salt. The DEFECT is on the other side -- cascades_translator.go builds
		// the query's FullUnorderedScanExpression from the SQL table name, and
		// FullUnorderedScanExpression.EqualsWithoutChildren compares the two
		// lists as strings, so a table whose name escapes matches NO candidate
		// and gets no access path at all. RFC-238 §7c decides the fix: the scan
		// leaf translates once, here nothing changes.
		primaryCandidate := cascades.NewPrimaryScanMatchCandidate(
			nil,
			aliases,
			allTypeNames,
			[]string{rt.Name},
			pkCols,
			rt.PrimaryKeyHasRecordTypePrefix(),
			flowed,
		)
		primaryCandidate.WithKeyComponentTypes(keyTypes)
		if rt.PrimaryKey != nil && rt.Descriptor != nil {
			primaryCandidate.WithCommonPrimaryKey(recordlayer.TranslatePrimaryKeyToValues(rt.PrimaryKey, strings.ToUpper, flowed))
		}
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
		} else if idx.GetPredicateProto().GetRowNumberWindowPredicate() != nil {
			// A sliding-window vector index answers K-NN over the base table from
			// its window: Java plans it with the window as TRUE
			// (RowNumberWindowPredicate.toPredicate), which is the index's intent
			// (sliding-window-semantic-search.yamsql). Only the vector candidate
			// takes it; a value scan would read a top-N index as the table.
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
		// aggregate use was already claimed by tryAggregateIndexCandidate above.
		// Dropping the remaining atomic types here leaves a plain MAX/MIN over only such
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
// defaultEmbeddedTemplate for template DDL); md the metadata the plan was built
// against; unnestMD the metadata for unnest-alias validation (the session's
// cached metadata in production; the same md for DDL).
func runFromResolutionPostPasses(logicalOp logical.LogicalOperator, schema string, md, unnestMD *recordlayer.RecordMetaData) error {
	// Java's generateAccess resolves a FROM identifier as a CTE/table/view/
	// function BEFORE treating it as a correlated array field. The parser, which
	// has no metadata, may classify a template-qualified table (`FROM PA AS s,
	// s.PB`, where the alias `s` also equals the schema name) as a lateral
	// unnest; demote it back to a table scan so the table branch wins (or reject
	// AT-on-a-table with WRONG_OBJECT_TYPE). RFC-142.
	if err := demoteQualifiedTableUnnest(logicalOp, schema, md); err != nil {
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
	// column check decline it (NewPlanContextFromIndexDefs refuses
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
	// THIS CROSS-CHECK NOW DECLINES A SHAPE IT USED TO ACCEPT. `actualSuffix` is
	// derived from the physical positions and `nameTrimmed` from the column
	// names; for a multi-type or universal index positions are always nil --
	// Java assigns them only to single-type indexes -- so `actualSuffix` is the
	// FULL primary key. WHEN THE INDEX KEY OVERLAPS THAT PRIMARY KEY,
	// `nameTrimmed` drops the shared columns, the two lengths disagree, and this
	// returns `unknown`. Without an overlap they agree and nothing changes; the
	// affected shape is the overlap case alone.
	//
	// THE COST IS NOT PURELY A LOST OPTIMISATION, and an earlier revision of
	// this comment said it was. `unknown` is `unknownPhysicalTypes`, i.e.
	// `values.UnknownType` per column, and `values.TypeTerminatesOrderingClaim`
	// answers FALSE for a type it cannot identify -- so a consumer that walks
	// these types to decide where an ordering claim ends (rowdiff/ordering.go)
	// does not stop at them. Returning `unknown` therefore widens a claim rather
	// than narrowing it, for a FLOAT or DOUBLE primary-key column that a known
	// type would have terminated on.
	//
	// That direction is the predicate's DOCUMENTED trade, not a defect
	// introduced here: it is deliberately positive ("prove it is a float")
	// because the alternative deletes sort elimination everywhere a layout is
	// absent, including where the column is provably an integer. What this
	// change does is route one more shape into that trade. Narrowing it means
	// teaching this function that the trim is name-based for these indexes,
	// which is a separate change with its own plan-shape review.
	nameTrimmed := plans.TrimmedPKSuffix(indexTrimmableKeyColumnNames(d.idx.RootExpression.ToKeyExpression()), pkCols)
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
		// A function key column is tagged with the function whose Value the
		// candidate's expansion builds (FunctionKeyExpression.toValue): an
		// order function's ToOrderedBytesValue, a long-arithmetic function's
		// ArithmeticValue. Any other function, an application's of the same
		// name included, stays "" and the candidate declines the mismatch.
		if _, isOrder := cascades.OrderFunctionDirection(e.Name()); isOrder ||
			recordlayer.IsLongArithmeticFunction(e.Name()) {
			return []string{e.Name()}
		}
		return []string{""}
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

// indexTrimmableKeyColumnNames are the key columns that are top-level fields:
// the only ones a primary-key field equals, as Index.TrimPrimaryKey compares
// key expressions (a nested S.X, CARDINALITY(X) or a version is not field X).
func indexTrimmableKeyColumnNames(expression *gen.KeyExpression) []string {
	var names []string
	for _, name := range indexKeyColumnFields(expression) {
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// indexKeyColumnFields is one entry per key column: the top-level field it is,
// or "" for any other column.
func indexKeyColumnFields(expression *gen.KeyExpression) []string {
	switch {
	case expression == nil:
		return nil
	case expression.Field != nil:
		return []string{expression.Field.GetFieldName()}
	case expression.Then != nil:
		var names []string
		for _, child := range expression.Then.GetChild() {
			names = append(names, indexKeyColumnFields(child)...)
		}
		return names
	case expression.KeyWithValue != nil:
		names := indexKeyColumnFields(expression.KeyWithValue.GetInnerKey())
		if split := int(expression.KeyWithValue.GetSplitPoint()); split >= 0 && split < len(names) {
			return names[:split]
		}
		return names
	default:
		key, err := recordlayer.KeyExpressionFromProto(expression)
		if err != nil || key == nil {
			return []string{""}
		}
		return make([]string, key.ColumnSize())
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
		// One column, named by the field its single argument reads.
		return []string{cascades.FunctionKeyColumnName(expression.Function)}, true
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

// IndexPrimaryKeyEntryOrdinals is, per IndexPrimaryKeyColumns column, its KEY
// tuple position in an entry of this single-type index: past the index's own
// columns, counting only the primary-key components the index key does not
// already hold (Index.trimPrimaryKey), or -1 for one it does. Java's value
// expansion visitor reads the trimmed primary key from exactly these positions.
func (d *metadataIndexDef) IndexPrimaryKeyEntryOrdinals() []int {
	rts := d.recordTypes()
	if len(rts) != 1 || d.idx.RootExpression == nil {
		return nil
	}
	columns, leadingRecordTypeKey, ok := coveredPrimaryKeyColumns(rts[0])
	if !ok {
		return nil
	}
	components := len(columns)
	if leadingRecordTypeKey {
		components++
	}
	positions := d.idx.PrimaryKeyComponentPositions()
	if positions != nil && len(positions) != components {
		return nil
	}
	ordinals := make([]int, 0, len(columns))
	next := d.idx.RootExpression.ColumnSize()
	for j := 0; j < components; j++ {
		ordinal := -1
		if positions == nil || positions[j] < 0 {
			ordinal = next
			next++
		}
		if j == 0 && leadingRecordTypeKey {
			continue
		}
		ordinals = append(ordinals, ordinal)
	}
	return ordinals
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

// GetCommonPrimaryKeyValues is the record type's primary key translated as an
// index over the type translates it (IndexCommonPrimaryKeyValues), over the
// same row layout, so scan and index plans report one primary-key property.
func (c *metadataPlanContext) GetCommonPrimaryKeyValues(recordType string) []values.Value {
	if c.md == nil {
		return nil
	}
	rt := c.md.GetRecordType(recordType)
	if rt == nil || rt.PrimaryKey == nil || rt.Descriptor == nil {
		return nil
	}
	return recordlayer.TranslatePrimaryKeyToValues(rt.PrimaryKey, strings.ToUpper,
		executor.PositionalTypeForRecordLayout(rt.Descriptor, c.md.IsStoreRecordVersions()))
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
	// The deprecated bare "max_ever"/"min_ever" types are maintained as _LONG
	// (CanonicalType) but are no candidate: Java's aggregate map holds only the
	// suffixed types (AggregateIndexExpansionVisitor.supportsAggregateIndexType).
	if idx.Type == recordlayer.IndexTypeMaxEver || idx.Type == recordlayer.IndexTypeMinEver {
		return nil
	}
	switch idx.CanonicalType() {
	case recordlayer.IndexTypeBitmapValue:
		aggFunc = expressions.AggBitmapConstructAgg
	case recordlayer.IndexTypeSum:
		aggFunc = expressions.AggSum
	case recordlayer.IndexTypeCount, recordlayer.IndexTypeCountNotNull:
		aggFunc = expressions.AggCount
	case recordlayer.IndexTypePermutedMax:
		// Plain SQL MAX(col) resolves to a PERMUTED_MAX index (Java's
		// NumericAggregationValue.Max.getIndexTypeName()), which tracks the true
		// current maximum under deletes/updates. A plain MAX/MIN never reaches a
		// monotone _EVER index, which would answer stale extrema: those carry
		// their own aggregates below, which a MIN/MAX does not equal.
		aggFunc = expressions.AggMax
	case recordlayer.IndexTypePermutedMin:
		aggFunc = expressions.AggMin
	case recordlayer.IndexTypeMaxEverLong, recordlayer.IndexTypeMaxEverTuple:
		// max_ever(col) / min_ever(col), the index-only aggregates Java's
		// AggregateIndexExpansionVisitor maps these types to
		// (IndexOnlyAggregateValue.MaxEverFn / MinEverFn,
		// AggregateIndexExpansionVisitor.java:369-380).
		aggFunc = expressions.AggMaxEver
	case recordlayer.IndexTypeMinEverLong, recordlayer.IndexTypeMinEverTuple:
		aggFunc = expressions.AggMinEver
	default:
		return nil
	}

	gke, ok := idx.RootExpression.(*recordlayer.GroupingKeyExpression)
	if !ok {
		return nil
	}

	// The candidate identifies each column by the full field path it reads,
	// the FieldValue the expansion visitor builds for it. A key the visitor
	// would expand into any other Value (a function, a version, a fan-out)
	// declines: MAX(add(v, 1)) must not match MAX(v). More than one grouped
	// column is Java's UnsupportedOperationException (constructGroupBy).
	groupingCount := gke.GetGroupingCount()
	groupedCount := gke.GetGroupedCount()
	groupPaths, groupedPaths, err := cascades.DescribeAggregateIndexKey(
		gke.ToKeyExpression().GetGrouping().GetWholeKey(), groupingCount)
	if err != nil || len(groupPaths) != groupingCount || len(groupedPaths) != groupedCount || groupedCount > 1 {
		return nil
	}

	// An ungrouped index is one group, the whole table, and serves the
	// ungrouped aggregate as Java's does (aggregate-empty-table.yamsql plans
	// `select sum(col1) from T2` as `AISCAN(T2_I5 <,> BY_GROUP ...)`). The rule
	// extends it to a NULL row when the index holds no entry (Java's ON EMPTY
	// NULL); a table whose rows were all deleted reads the stored 0, as in Java.
	permutedSize := 0
	if idx.Type == recordlayer.IndexTypePermutedMax || idx.Type == recordlayer.IndexTypePermutedMin {
		// Absent is 0 and present is Integer.parseInt, as Java's
		// AggregateIndexMatchCandidate.getPermutedCount reads it.
		if _, ok := idx.Options[recordlayer.IndexOptionPermutedSize]; ok {
			parsed, err := recordlayer.PermutedSizeOption(idx)
			if err != nil || parsed < 0 || parsed > groupingCount {
				return nil
			}
			permutedSize = parsed
		}
	}

	// VERBATIM: these names arrive from the index's key expression, which
	// carries the DESCRIPTOR's spelling, and they are looked up EXACTLY
	// downstream. Folding them here is a silent decline, not an error — an
	// aggregate index declared `AS SELECT SUM("Amount") FROM sales GROUP BY
	// "Region"` was never chosen, because the query row declares `Region` and
	// this offered `REGION`. Right rows, full scan plus an in-memory sort,
	// nothing red.
	//
	// Name the exact consumer, because the obvious candidate is the wrong one:
	// it is NOT AccessorNamePathMatchesNames (values/accessor_name_path.go),
	// which folds the candidate and therefore matched fine. It is
	// rule_aggregate_data_access.go's LookupFieldUnique on the aggregate
	// operand, which bottoms out in RecordType.fieldNameScan's `f.Name == name`
	// (values/type.go) — a byte comparison with no fold anywhere near it.
	groupCols := make([]string, groupingCount)
	for i, path := range groupPaths {
		groupCols[i] = path[len(path)-1]
	}
	var aggColumn string
	var aggPath []string
	if groupedCount > 0 {
		aggPath = groupedPaths[0]
		aggColumn = aggPath[len(aggPath)-1]
	}

	rts := md.RecordTypesForIndex(idx)
	rtNames := make([]string, len(rts))
	for i, rt := range rts {
		rtNames[i] = rt.Name
	}

	allTypes := physicalKeyComponentTypes(gke, rts)
	groupTypes := alignPhysicalTypes(allTypes, groupingCount)

	if aggFunc == expressions.AggBitmapConstructAgg {
		// BitmapAggregateIndexExpansionVisitor adds the bucket offset as an
		// implicit final grouping coordinate; the metadata stores the raw field.
		size, err := recordlayer.BitmapValueEntrySizeOption(idx)
		if err != nil || groupedCount != 1 || aggColumn == "" || idx.HasFilteringPredicate() {
			return nil
		}
		row, ok := singleRecordTypeRowType(md, idx).(*values.RecordType)
		if !ok {
			return nil
		}
		field, ok := values.LookupFieldPathUnique(row, aggPath)
		if !ok {
			return nil
		}
		bucketName := "__bitmap_bucket"
		for _, name := range groupCols {
			if name == bucketName {
				return nil
			}
		}
		groupCols = append(groupCols, bucketName)
		groupPaths = append(groupPaths, []string{bucketName})
		groupTypes = append(groupTypes, field.FieldType)
		return cascades.NewAggregateIndexMatchCandidate(idx.Name, rtNames, groupCols, aggFunc, aggColumn,
			row, groupTypes, len(groupCols)).WithColumnPaths(groupPaths, aggPath).WithBitmapEntrySize(size)
	}

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
		groupingCount-permutedSize,
	).WithColumnPaths(groupPaths, aggPath).
		WithPermutedOrdering(idx.Type == recordlayer.IndexTypePermutedMax || idx.Type == recordlayer.IndexTypePermutedMin)
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
	partitionCount := 0
	if kwv, ok := idx.RootExpression.(*recordlayer.KeyWithValueExpression); ok {
		partitionCount = kwv.SplitPoint()
	}
	if idx.Type == recordlayer.IndexTypeVectorSPFresh && partitionCount > 0 {
		// The SPFresh maintainer rejects prefixed (grouped) scans; a
		// partitioned candidate would plan queries the executor cannot run.
		// The DDL already rejects PARTITION BY USING SPFRESH — this guards
		// directly-constructed metadata.
		return nil
	}
	parsed, err := recordlayer.VectorIndexMetric(idx)
	if err != nil {
		// A metric the maintainer refuses (corrupt or newer-version metadata):
		// no candidate, so the QUALIFY distance predicate stays
		// uncompensatable and the query fails to plan rather than returning
		// wrong-metric results. Java's expansion throws there; the index's
		// writes fail in both engines.
		return nil
	}
	metric := vectorDistanceOperator(parsed)

	rts := md.RecordTypesForIndex(idx)
	rtNames := make([]string, len(rts))
	for i, rt := range rts {
		rtNames[i] = rt.Name
	}
	// Physical names verbatim, as for the primary-scan candidate.
	var pkCols []string
	if pk, _, safe := commonCoveredPrimaryKeyColumns(rts); safe {
		pkCols = pk
	}

	partitionTypes := alignPhysicalTypes(
		physicalKeyComponentTypes(idx.RootExpression, rts),
		partitionCount,
	)

	baseRowType := singleRecordTypeRowType(md, idx)
	if values.IsUnresolved(baseRowType) {
		return nil
	}
	engine, err := recordlayer.VectorEngineOf(idx)
	if err != nil {
		return nil
	}
	return cascades.NewVectorIndexScanMatchCandidate(
		idx.Name, rtNames, cols, partitionCount, metric,
		baseRowType, idx.IsUnique(), pkCols,
	).WithPartitionKeyComponentTypes(partitionTypes).WithIndexEngine(engine.String())
}

// vectorDistanceOperator is the distance placeholder's operator for the metric
// the index is maintained with (recordlayer.VectorIndexMetric).
func vectorDistanceOperator(m recordlayer.VectorMetric) values.DistanceOperator {
	switch m {
	case recordlayer.VectorMetricEuclideanSquare:
		return values.DistanceEuclideanSquare
	case recordlayer.VectorMetricCosine:
		return values.DistanceCosine
	case recordlayer.VectorMetricInnerProduct:
		return values.DistanceDotProduct
	default:
		return values.DistanceEuclidean
	}
}

// resultColumns are the columns Java reports for a plan: the fields of its
// result row type (QueryPlan.getResultType). The SQL labels come from the
// logical output row and are applied by Execute.
func resultColumns(plan plans.RecordQueryPlan) []executor.ColumnDef {
	switch t := plan.GetResultType().(type) {
	case *values.RecordType:
		return columnsOfRowType(t)
	case *values.RelationType:
		row, _ := t.InnerType.(*values.RecordType)
		return columnsOfRowType(row)
	}
	return nil
}

func columnsOfRowType(rowType *values.RecordType) []executor.ColumnDef {
	if rowType == nil {
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

type multiInnerPlan interface {
	GetInners() []plans.RecordQueryPlan
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
	case values.TypeCodeUuid, values.TypeCodeEnum:
		// JDBC getColumnTypeName for UUID and ENUM is the catch-all "OTHER"
		// (Java DataType's JDBC map → Types.OTHER → "OTHER"), matching the
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

// demoteQualifiedTableUnnest enforces Java's `LogicalOperator.generateAccess`
// resolution ORDER on a lateral-unnest candidate: a FROM identifier is resolved
// as a CTE / TABLE / view / function FIRST, and only falls through to
// `resolveCorrelatedIdentifier` (an in-scope correlated array field) when none
// of those match. The parser classifies a dotted comma source as a
// LogicalUnnest whenever segment 0 names a VISIBLE in-scope FROM-source alias —
// but it has no metadata, so it cannot run the table-first check. When the prior
// alias HAPPENS to equal the schema template's name (`FROM PA AS s, s.PB AS B`
// over template s), `s.PB` is in truth a qualified TABLE (`tableExists` in
// Java: qualifier == the template's name AND table `PB` exists), so the table
// branch must win — it is a
// plain cross join, never a correlated unnest. This pass walks the tree and, for
// any LogicalJoin whose Right is a template-qualified-table LogicalUnnest, demotes
// it back to a LogicalScan of the resolved bare table name (mirroring
// `resolveQualifiedTableNames` stripping `schema.` off a normal scan).
//
// When the template-qualified table carries an AT ordinal alias (`FROM PA AS s,
// s.PB AT ord`), Java's table branch still wins — but it asserts
// `atAlias.isEmpty()` and throws WRONG_OBJECT_TYPE ("'PB' is a table"). We surface
// that code HERE (early, before scope binding tries to resolve a projection
// against the would-be unnest), rather than leaving the source on the unnest path
// where the projection scope binding could fail first with a misleading
// undefined-column error. A genuine correlated array (`FROM T1, T1.arr`, where the
// qualifier `T1` is NOT the schema name) is left untouched — it is not a schema-
// qualified table, so it correctly falls through to the correlated-field path.
// RFC-142 (P2b).
func demoteQualifiedTableUnnest(op logical.LogicalOperator, templateName string, md *recordlayer.RecordMetaData) error {
	if op == nil || md == nil {
		return nil
	}
	if j, ok := op.(*logical.LogicalJoin); ok {
		if u, ok := j.Right.(*logical.LogicalUnnest); ok {
			if table, alias, isTable := qualifiedUnnestTable(u, templateName, md); isTable {
				if u.AtAlias != "" {
					// AT on a template-qualified TABLE → Java's table-branch
					// atAlias.isEmpty() assert → WRONG_OBJECT_TYPE.
					return atOnNonArrayError(strings.Join(u.Segments, "."), "a table")
				}
				demoted := logical.NewScan(table, alias, table)
				demoted.Binding = u.Binding
				j.Right = demoted
			}
		}
	}
	for _, ch := range op.Children() {
		if err := demoteQualifiedTableUnnest(ch, templateName, md); err != nil {
			return err
		}
	}
	// Children() exposes only the operator's primary input tree; the nested
	// logical plans for EXISTS / scalar subqueries are carried as side fields on
	// LogicalFilter / LogicalProject / LogicalAggregate and are NOT children. A
	// template-qualified-table LogicalUnnest can live INSIDE such a subquery
	// (`… WHERE EXISTS (SELECT 1 FROM PA AS s, s.PB AS B)`), so the table-first
	// demotion — Java's generateAccess runs at EVERY FROM-source resolution
	// point, including inside subqueries — must reach those plans too, else
	// `s.PB` is wrongly translated as a correlated unnest of the missing field
	// `PB` on source `s`. RFC-142 (P2).
	for _, sub := range subqueryPlans(op) {
		if err := demoteQualifiedTableUnnest(sub, templateName, md); err != nil {
			return err
		}
	}
	return nil
}

// rejectAtOrdinalityOnTable enforces Java's `generateAccess` AT-on-a-table
// rejection EARLY — at FROM-source analysis time, before the SELECT/WHERE column
// resolution — so the faithful WRONG_OBJECT_TYPE (42809) or UNDEFINED_TABLE
// (42F01) is the surfaced error and is NOT masked by a scope-level
// undefined-column (42703) / ambiguous (42702) raised while resolving a
// projection.
//
// The masking bug: a SINGLE-segment AT comma source (`FROM T1, U AT O`, the
// bare-source `T1, T1 AT O`) stays a LogicalUnnest (the AT shortcut in
// unnestCandidateShape) so the AT survives to a clean rejection, and the SELECT
// scope registers a VIRTUAL unnest binding (correlation = the AT alias). A
// reference to the REAL table's own column (`U.ID`) then fails to resolve at the
// scope level (the real table `U` is shadowed by the virtual binding) with a
// MASKING 42703 BEFORE translation. Running the rejection here — before any
// projection column resolution — surfaces the intended error regardless of what
// the query references.
//
// Only the single-segment item is decided here (atOnJoinSourceError): a dotted
// item is a template-qualified table (demoteQualifiedTableUnnest) or a
// correlated path the semantic collection binding resolves, where an array
// takes the AT and a non-array is refused as it is without one.
// RFC-142.
func rejectAtOrdinalityOnTable(op logical.LogicalOperator, md *recordlayer.RecordMetaData) error {
	return rejectAtOrdinalityOnTableWithCTEs(op, md, logical.CTERegistry{}, nil)
}

// Resolve constructor inputs once, then validate the retained producer graph.
// Both selected CTEs and captured physical sources keep their defining ownership.
// enclosing is the scope of the blocks enclosing op, whose operators Java's
// CTE branch reads by name; nil where the caller has none.
func rejectAtOrdinalityOnTableWithCTEs(op logical.LogicalOperator, md *recordlayer.RecordMetaData, registry logical.CTERegistry, enclosing *semantic.Scope) error {
	logical.BindCTESources(op, registry)
	return rejectAtOrdinalityOnTableInGraph(op, md, make(map[*logical.CTEProducer]bool), registry, enclosing)
}

// rejectAtOrdinalityOnTableInGraph walks op's block with its registry and
// enclosing scope. A CTE body or a subquery plan it descends into is its own
// block, whose enclosing scope this walk does not have (nil): its own build ran
// this pass with that scope before it was attached, and this is the backstop.
func rejectAtOrdinalityOnTableInGraph(op logical.LogicalOperator, md *recordlayer.RecordMetaData, producers map[*logical.CTEProducer]bool, registry logical.CTERegistry, enclosing *semantic.Scope) error {
	if op == nil || md == nil {
		return nil
	}
	if scan, ok := op.(*logical.LogicalScan); ok {
		if producer := scan.Source.Producer(); producer != nil && !producers[producer] {
			producers[producer] = true
			if err := rejectAtOrdinalityOnTableInGraph(producer.Body(), md, producers, registry, nil); err != nil {
				return err
			}
		}
	}
	if j, ok := op.(*logical.LogicalJoin); ok {
		if u, ok := j.Right.(*logical.LogicalUnnest); ok && u.AtAlias != "" {
			if err := atOnJoinSourceError(j.Left, u, md, registry, enclosing); err != nil {
				return err
			}
		}
	}
	for _, ch := range op.Children() {
		if err := rejectAtOrdinalityOnTableInGraph(ch, md, producers, registry, enclosing); err != nil {
			return err
		}
	}
	// AT-on-a-table can appear inside an EXISTS / scalar subquery's own FROM scope
	// (carried on side fields, not Children()) — Java's generateAccess runs at every
	// FROM point. Reach those plans too, like demoteQualifiedTableUnnest. RFC-142.
	for _, sub := range subqueryPlans(op) {
		if err := rejectAtOrdinalityOnTableInGraph(sub, md, producers, registry, nil); err != nil {
			return err
		}
	}
	return nil
}

// atOnJoinSourceError is Java's generateAccess for a comma or INNER join FROM
// item carrying AT (LogicalOperator.java:180-226), decided before binding so a
// reference the AT source's virtual binding shadows cannot mask it with 42703.
//
// Only a SINGLE-segment item is decided here. Java reads it as a CTE — which,
// for a name, is a WITH CTE or an operator of ANY fragment, the block's prior
// FROM sources and every enclosing block's (findCteMaybe: `FROM w, w AT p` and
// `EXISTS (SELECT 1 FROM h, w AT p)` under an outer w are both "'W' is a
// common table expression", measured) — or as a table, and either refuses the
// AT, WRONG_OBJECT_TYPE; anything else is an unqualified correlated
// identifier, which resolveCorrelatedIdentifier refuses as an unknown table,
// UNDEFINED_TABLE (`FROM w, nosuch AT p`, measured). registry supplies the
// WITH CTEs in scope and enclosing the enclosing blocks' sources; either may
// be empty where a caller has none.
//
// A DOTTED item is not decided here: a template-qualified table is refused by
// demoteQualifiedTableUnnest, and every other dotted item is a correlated path
// the binder resolves across every enclosing level — an array takes the AT
// (`FROM t2, n.arr AS x AT p`, `EXISTS (SELECT 1 FROM h, w.arr AS v AT p)`,
// both measured), a non-array is refused INVALID_COLUMN_REFERENCE with or
// without AT (`FROM w, w.f AS x AT p`, measured), as
// generateCorrelatedFieldAccess refuses it.
// RFC-142.
func atOnJoinSourceError(left logical.LogicalOperator, u *logical.LogicalUnnest, md *recordlayer.RecordMetaData, registry logical.CTERegistry, enclosing *semantic.Scope) error {
	if len(u.Segments) != 1 {
		return nil
	}
	name := u.Segments[0]
	probe := logical.NewScan(name, "", name)
	logical.BindCTESources(probe, registry)
	cteNamed := logical.FindVisibleScan(left, name) != nil ||
		logical.OuterSourceIsDerivedTable(left, name) ||
		logical.FindOwnerUnnest(left, name) != nil ||
		logical.FindOwnerInlineValues(left, name) != nil ||
		logical.FindOuterScanTable(left, name) != "" ||
		probe.Source.Producer() != nil ||
		scopeNamesSource(enclosing, name)
	return singleSegmentAtError(name, cteNamed, md)
}

// scopeNamesSource reports that some level of scope has a source aliased name:
// the operators of every enclosing query block, which Java's findCteMaybe
// reads as CTEs of that name. A nil scope names nothing.
func scopeNamesSource(scope *semantic.Scope, name string) bool {
	if scope == nil {
		return false
	}
	for _, src := range scope.AllSourcesRecursive() {
		if src.Alias.Name() == name {
			return true
		}
	}
	return false
}

// singleSegmentAtError is the verdict atOnJoinSourceError describes, for a
// single-name FROM item carrying AT: cteNamed reports that Java's CTE branch
// reads the name (a WITH CTE, or an operator of this or an enclosing block).
//
// The table test folds case (recordTypeExistsFold) where Java's tableExists
// compares exactly: it asks the question Go's own table resolution answers for
// the same name without the AT (a hand-written descriptor's lower-case record
// type is a table to `FROM orders`), so the AT verdict and the scan agree on
// what is a table. A DDL catalog stores the normalized spelling, where the two
// comparisons coincide.
func singleSegmentAtError(name string, cteNamed bool, md *recordlayer.RecordMetaData) error {
	switch {
	case cteNamed:
		return atOnNonArrayError(name, "a common table expression")
	case md != nil && recordTypeExistsFold(md, name):
		return atOnNonArrayError(name, "a table")
	}
	return api.NewErrorf(api.ErrCodeUndefinedTable, "Unknown table %s", name)
}

// atOnNonArrayError is Java's WRONG_OBJECT_TYPE for an AT on a FROM item that is
// not a correlated array (LogicalOperator.java:187-216), in its wording.
func atOnNonArrayError(name, kind string) error {
	return api.NewErrorf(api.ErrCodeWrongObjectType,
		"AT clause requires an array-typed column, but '%s' is %s", name, kind)
}

// subqueryPlans returns the nested logical plans an operator carries on its
// side fields (EXISTS / scalar subqueries) — the plans NOT reachable via
// Children(). These are the FROM scopes that a template-qualified-table unnest
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

// qualifiedUnnestTable reports whether a lateral-unnest candidate is in
// truth a template-qualified TABLE reference (Java's `tableExists` precedence),
// and if so returns the resolved bare table name and the FROM alias to scan it
// under. It is a template-qualified table IFF its segments are exactly
// `[qualifier, table]`, the qualifier case-insensitively equals the session
// schema name, and `table` resolves to a real record type — precisely Java's
// `tableExists` (one qualifier segment == schema-template name + table in the
// catalog). An AT alias does NOT change whether it is a TABLE (the caller handles
// AT separately: a table cross join when AT is absent, WRONG_OBJECT_TYPE when
// present). RFC-142.
func qualifiedUnnestTable(u *logical.LogicalUnnest, templateName string, md *recordlayer.RecordMetaData) (table, alias string, ok bool) {
	if len(u.Segments) != 2 {
		return "", "", false
	}
	if u.Segments[0] != templateName {
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

// defaultEmbeddedTemplate is the table qualifier the embedded planner accepts
// when it plans with metadata and no schema template (the FDB test and
// planner harnesses): the name such a harness's tables are qualified by. A
// session plans with its schema's template name (sessionTemplate). RFC-142.
const defaultEmbeddedTemplate = "S"

// sessionTemplate returns the name a table's qualifier must carry: the name of
// the session schema's TEMPLATE (functions.ResolveTargetTablePath), read from
// the same cached schema the plan's metadata comes from (cachedMetaData). With
// no session schema, or none cached (an explain-only generator without
// metadata), it is defaultEmbeddedTemplate.
func (g *cascadesGenerator) sessionTemplate() string {
	if g.c != nil {
		if tmpl := g.c.cachedSchemaTemplate(); tmpl != nil {
			return tmpl.MetadataName()
		}
	}
	return defaultEmbeddedTemplate
}

// newUnnestTableResolver builds the table-first resolver (Java's `tableExists`
// precedence) the lateral-unnest classifier consults: a dotted FROM-source name
// resolves to a template-qualified TABLE — and is therefore NOT a correlated
// unnest — when its segments are exactly `[qualifier, name]`, `qualifier`
// equals the schema template's name exactly (both normalized), and `name` is a
// real record type. This mirrors Java's `tableExists`: one qualifier segment
// equal to metadataCatalog.getName() plus a table found in the catalog.
//
// A dotted reference whose qualifier is a CTE/derived alias (`cte.col`,
// `d.col`) is NOT matched here: a CTE reference in Java's `findCteMaybe` matches
// only an UNQUALIFIED name, so a qualified `cte.col` never resolves to a CTE. The
// CTE-output unnest case (`FROM cte, cte.arr`) is handled on the correlated path
// and validated against the CTE OUTPUT type — P2a (translateUnnestJoin's
// outerSourceIsCTE rejection). RFC-142.
func newUnnestTableResolver(md *recordlayer.RecordMetaData, templateName string) tableResolver {
	return func(segments []string) bool {
		if len(segments) != 2 {
			return false
		}
		if segments[0] != templateName {
			return false
		}
		return recordTypeExistsFold(md, segments[1])
	}
}

// resolveQualifiedTableNames walks the logical plan tree and resolves each
// qualified table name (template.table -> table): a scan as Java's
// SemanticAnalyzer.tableExists reads a FROM source, a DML target as its
// getTable reads a statement's table (functions.ResolveSourceTablePath and
// ResolveTargetTablePath). A scan keeps its TablePath, so a qualified name
// whose table does not exist is refused by validateTablesAndColumns as Java
// refuses it.
func resolveQualifiedTableNames(op logical.LogicalOperator, templateName string) error {
	if op == nil {
		return nil
	}
	if scan, ok := op.(*logical.LogicalScan); ok {
		path := scan.TablePath
		if path == nil {
			path = strings.Split(scan.Table, ".")
		}
		// A qualified source that is not the template's table is, in Java, no
		// table: generateAccess goes on to a view, a function and a correlated
		// field, and the last refuses the path. Go's builders have already
		// taken an alias-qualified path as a correlated field (an unnest), so
		// what reaches a scan is refused here.
		resolved, ok, err := functions.ResolveSourceTablePath(path, templateName)
		if err != nil {
			return err
		}
		if !ok {
			return functions.UnknownSourceReferenceError(path)
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
	// A DML target resolves from its parse-time segments, as a scan does, so a
	// quoted target holding a dot (`"foo.tableA"`) is one name, not a schema
	// `foo` and a table `tableA`. Every builder of a DML operator sets the
	// segments; a target without them has lost them, and splitting its joined
	// name would read a quoted dot as a qualifier.
	resolveTarget := func(name string, path []string) (string, error) {
		if len(path) == 0 {
			return "", fmt.Errorf("DML target %q carries no identifier segments", name)
		}
		return functions.ResolveTargetTablePath(path, templateName)
	}
	if ins, ok := op.(*logical.LogicalInsert); ok {
		resolved, err := resolveTarget(ins.Table, ins.TablePath)
		if err != nil {
			return err
		}
		ins.Table = resolved
	}
	if del, ok := op.(*logical.LogicalDelete); ok {
		resolved, err := resolveTarget(del.Target, del.TargetPath)
		if err != nil {
			return err
		}
		del.Target = resolved
	}
	if upd, ok := op.(*logical.LogicalUpdate); ok {
		resolved, err := resolveTarget(upd.Target, upd.TargetPath)
		if err != nil {
			return err
		}
		upd.Target = resolved
	}
	for _, ch := range op.Children() {
		if err := resolveQualifiedTableNames(ch, templateName); err != nil {
			return err
		}
	}
	// Subquery plans (EXISTS / scalar) carried on side fields are not Children();
	// a template-qualified table scan can live inside one (`… EXISTS (SELECT 1 FROM
	// PA, s.PB AS B)`), so strip its `schema.` qualifier there too — the same
	// structural gap the subquery-aware demoteQualifiedTableUnnest walk covers
	// for the unnest variant. RFC-142 (P2).
	for _, sub := range subqueryPlans(op) {
		if err := resolveQualifiedTableNames(sub, templateName); err != nil {
			return err
		}
	}
	return nil
}

func validateTablesAndColumns(op logical.LogicalOperator, md *recordlayer.RecordMetaData) error {
	logical.BindCTESources(op, logical.CTERegistry{})
	return validateTablesAndColumnsInner(op, md, make(map[*logical.CTEProducer]bool))
}

func validateTablesAndColumnsInner(op logical.LogicalOperator, md *recordlayer.RecordMetaData, cteNames map[*logical.CTEProducer]bool) error {
	if op == nil {
		return nil
	}
	if scan, ok := op.(*logical.LogicalScan); ok {
		if scan.Source.Producer() == nil {
			rt := md.GetRecordType(scan.Table)
			if rt == nil {
				// A qualified name that names no table ends in Java's
				// correlated-field reading, which refuses the path; only an
				// unqualified one is "Unknown table" (resolveCorrelatedIdentifier
				// requires a qualifier).
				if len(scan.TablePath) > 1 {
					return functions.UnknownSourceReferenceError(scan.TablePath)
				}
				return api.NewErrorf(api.ErrCodeUndefinedTable, "table %q does not exist", scan.Table)
			}
		}
	}
	if scan, ok := op.(*logical.LogicalScan); ok && scan.Source.Producer() != nil {
		producer := scan.Source.Producer()
		if !cteNames[producer] {
			cteNames[producer] = true
			if err := validateTablesAndColumnsInner(producer.Body(), md, cteNames); err != nil {
				return err
			}
		}
	}
	if proj, ok := op.(*logical.LogicalProject); ok && !hasJoin(op) && !hasAggregate(op) &&
		!projectionInputRedefinesColumns(proj.Input) {
		scan := findLogicalScan(op)
		if scan != nil && scan.Source.Producer() == nil {
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
					if strings.EqualFold(functions.NormalizeIdentifier(uid.GetText()), "INFORMATION_SCHEMA") {
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
		// A bare-name call may be a schema macro; the resolver decides.
		if _, udf := n.FunctionCall().(*antlrgen.UserDefinedScalarFunctionCallContext); udf {
			break
		}
		if fc := n.FunctionCall(); fc != nil {
			// The name as the query spells it: Java's resolveFunction reports
			// "Unsupported operator <name>" with the caller's spelling
			// (SemanticAnalyzer.java:1105-1107), so `bitmap_bucket_number(x)`
			// is refused in lower case. The allow-list reads it upper-cased.
			if name := extractFunctionNameFromCall(fc); name != "" {
				if !isAllowedFunction(strings.ToUpper(name)) {
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
			return f.ScalarFunctionName().GetText()
		}
	case *antlrgen.UserDefinedScalarFunctionCallContext:
		if f.UserDefinedScalarFunctionName() != nil {
			return f.UserDefinedScalarFunctionName().GetText()
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
	case "COUNT", "SUM", "MIN", "MAX", "AVG", "ARRAY_AGG",
		"CASE", "CAST", "IF",
		"CURRENT_DATE", "CURRENT_TIME", "CURRENT_TIMESTAMP", "LOCALTIME",
		"CURRENT_USER",
		// CARDINALITY is a dedicated by-name built-in (expr.walkCardinality
		// → CardinalityValue), not a generic ScalarFunctionValue, so it lives
		// here rather than in IsCascadesSafeScalarFunction — the Cascades walk
		// builds its own Value with nullable-INT typing and array validation.
		"CARDINALITY",
		// The bitmap functions are ArithmeticValues (expr.walkScalarFunction
		// → ResolveArithmetic), as Java's are, not catalogue entries.
		"BITMAP_BUCKET_OFFSET", "BITMAP_BIT_POSITION",
		// Vector distances are DistanceValues (expr.distanceOperatorForFunc).
		"EUCLIDEAN_DISTANCE", "EUCLIDEAN_SQUARE_DISTANCE", "COSINE_DISTANCE", "DOT_PRODUCT_DISTANCE",
		`"` + expr.SQLFunctionArgument + `"`:
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
// RecordLayerSchemaTemplate without performing any catalog write, through
// buildSchemaTemplate, the front end CREATE SCHEMA TEMPLATE executes.
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
	// The production front end, the one CREATE SCHEMA TEMPLATE executes.
	return buildSchemaTemplate(stCtx)
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

// validateScanTables is validateTablesAndColumns' TABLE half, for the DML path,
// and it walks the same tree the original does -- Children() only.
//
// The SELECT path runs the full validation in runFromResolutionPostPasses; the
// DML path never did, so a table that does not exist escaped every check when
// it appeared as a SOURCE rather than as a target. `INSERT INTO "Customer"
// SELECT id, name FROM customer` against a table declared `"Customer"` reported
// SUCCESS with zero rows -- the same silent shape as the target-side defect and
// one word away from it -- while the identical bare SELECT answered 42F01. A
// subquery source was loud but wrong: `DELETE ... WHERE id IN (SELECT id FROM
// customer)` gave 0AF00 "DML Cascades translation failed".
//
// TABLES ONLY, deliberately. The column half of that validation would also
// change which SQLSTATE a DML statement reports for a bad COLUMN, and the
// ordering of column-vs-table diagnostics on the DML path is a separate open
// question with its own measurements (RFC-238 section 7e). Widening the fix to
// carry that decision along is how one change becomes two.
//
// AND IT WALKS SUBQUERY PLANS AS WELL AS Children(), which the function it
// halves does not. Three claims were made about that walk in three different
// places and all three were wrong; these are the measurements.
//
// IT IS NOT LOAD-BEARING FOR ANY SQL-VISIBLE SHAPE TESTED. Removing it leaves
// every arm of unquoted_dml_against_a_quoted_table.yaml green, including
// `DELETE FROM t WHERE id = (SELECT MAX(id) FROM nosuchtable)` and the EXISTS
// form. Production rejects both -- translateScan raises ErrCodeUndefinedTable
// for a scan with no catalog row type, and the EXISTS shape fails its own
// check. What the walk guards is
// the HARNESS path: planPhysicalDMLWithMetadata is a hand-maintained copy of
// planDML that explain-differ plans the whole corpus through, and there the
// scalar-subquery source does reach it -- plan_shape.golden records the
// resulting PLAN-ERROR in this function's own wording.
//
// THAT MAKES IT THE ODD ONE OUT AND THE NOTE IS DELIBERATE. Every other
// harness-vs-production divergence here was closed by making the HARNESS mirror
// production -- the target guard and this sweep. This
// walk is the reverse: production carries it for a path only the harness
// exercises. Someone will eventually read that as dead weight and simplify it
// away, so the reason is written down rather than inferred.
//
// Source ownership is sealed before this walk. Retained producers are checked
// by identity, while captured physical sources always require a catalog table.
//
// A DML EXISTS SUBQUERY IS NOT WHY THIS EXISTS. `DELETE ... WHERE EXISTS
// (SELECT 1 FROM nosuchtable)` answers 0AF00 from the unsupported-shape check
// with the walk or without it, and upgradeDMLWhereWithCatalog does INSTALL
// EXISTS subqueries on its success path -- what it cannot install is one whose
// inner build already failed. An earlier comment said it dropped them
// unconditionally; that was wrong, and the conclusion it supported was right
// for a different reason.
func validateScanTables(op logical.LogicalOperator, md *recordlayer.RecordMetaData) error {
	if op == nil || md == nil {
		return nil
	}
	logical.BindCTESources(op, logical.CTERegistry{})
	return validateScanTablesInner(op, md, make(map[*logical.CTEProducer]bool))
}

func validateScanTablesInner(op logical.LogicalOperator, md *recordlayer.RecordMetaData, producers map[*logical.CTEProducer]bool) error {
	if op == nil {
		return nil
	}
	if scan, ok := op.(*logical.LogicalScan); ok {
		if scan.Source.Producer() == nil && md.GetRecordType(scan.Table) == nil {
			return api.NewErrorf(api.ErrCodeUndefinedTable, "table %q does not exist", scan.Table)
		}
	}
	if scan, ok := op.(*logical.LogicalScan); ok {
		if producer := scan.Source.Producer(); producer != nil && !producers[producer] {
			producers[producer] = true
			if err := validateScanTablesInner(producer.Body(), md, producers); err != nil {
				return err
			}
		}
	}
	for _, child := range op.Children() {
		if err := validateScanTablesInner(child, md, producers); err != nil {
			return err
		}
	}
	for _, sub := range subqueryPlans(op) {
		if err := validateScanTablesInner(sub, md, producers); err != nil {
			return err
		}
	}
	return nil
}
