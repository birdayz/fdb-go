package embedded

import (
	"fmt"
	"strings"
	"sync"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	cascades "fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/parser"
	"fdb.dev/pkg/relational/core/query"
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
	physPlan, err := PlanPhysicalForTest(sql, schemaDDL, stats)
	if err != nil {
		return "", err
	}
	return physPlan.Explain(), nil
}

// PlanPhysicalForTest is PlanQueryForTest returning the TYPED physical plan
// instead of its Explain rendering. The rowdiff harness (RFC-182) buckets
// plan families over this tree — per the RFC, plan-family classification
// must come from a plan-type switch, never from string-matching EXPLAIN text.
func PlanPhysicalForTest(sql, schemaDDL string, stats properties.StatisticsProvider) (plans.RecordQueryPlan, error) {
	tmpl, err := buildSchemaTemplateFromDDL(schemaDDL)
	if err != nil {
		return nil, fmt.Errorf("schema DDL: %w", err)
	}
	md := tmpl.Underlying()

	root, err := parser.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("parse SQL: %w", err)
	}
	stmts := root.Statements()
	if stmts == nil || len(stmts.AllStatement()) == 0 {
		return nil, fmt.Errorf("no statements in SQL")
	}
	sel := stmts.AllStatement()[0].SelectStatement()
	if sel == nil {
		return nil, fmt.Errorf("not a SELECT statement")
	}
	q := sel.Query()
	if q == nil {
		return nil, fmt.Errorf("malformed SELECT")
	}

	visitor := NewPlanVisitor(md)
	logicalOp, buildErr := visitor.VisitQuery(q)
	if buildErr != nil {
		return nil, buildErr
	}
	if logicalOp == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "could not build logical plan")
	}
	if fn := query.FindUnsupportedFunction(logicalOp); fn != "" {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "Unsupported operator "+fn)
	}
	if err := resolveQualifiedTableNames(logicalOp, "s"); err != nil {
		return nil, err
	}
	if err := validateTablesAndColumns(logicalOp, md); err != nil {
		return nil, err
	}
	if msg := findDistinctAggregate(logicalOp); msg != "" {
		// Java rejects DISTINCT aggregates with UNSUPPORTED_QUERY (0AF00); match it.
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, msg)
	}

	if arityErr := query.ValidateCTEAliasArities(logicalOp); arityErr != nil {
		return nil, arityErr
	}
	ref, _, translateErr := query.TranslateToCascadesWithError(logicalOp, md)
	if translateErr != nil {
		// Surface the translator's typed diagnostic over the generic fallback —
		// same precedence the production generator applies.
		return nil, translateErr
	}
	if ref == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "Cascades translation failed")
	}

	rules := cascades.DefaultExpressionRules()
	rules = append(rules, cascades.RewritingRules()...)
	planCtx := buildCascadesPlanContext(md)
	planner := cascades.NewPlanner(rules, planCtx).
		WithImplementationRules(cascades.DefaultImplementationRules()).
		WithPlanningExpressionRules(cascades.BatchAExpressionRules()).
		WithStatistics(stats).
		WithMaxTasks(100_000)

	// RFC-183 P5 precondition: does every reference reachable at extraction
	// hold at most ONE physical final? That is what Java's getRangesOverPlan
	// assumes. Off by default (it walks the whole reference graph);
	// planAndVerifyOneFinal turns it on.
	if verifyOneFinalEnabled {
		planner.SetVerifyOneFinal(true)
	}

	bestExpr, _, planErr := planner.Plan(ref)
	if verifyOneFinalEnabled {
		lastOneFinalViolations = planner.OneFinalViolations()
	}
	if planErr != nil {
		return nil, fmt.Errorf("planning failed: %w", planErr)
	}
	if bestExpr == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "no plan found")
	}

	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	ph, ok := bestExpr.(planExtractor)
	if !ok {
		return nil, fmt.Errorf("best expression is not a physical plan: %T", bestExpr)
	}
	physPlan := ph.GetRecordQueryPlan()
	if physPlan == nil {
		return nil, fmt.Errorf("physical plan is nil")
	}
	// RFC-164 WS-2: structural plan invariants — an always-on backstop that fails
	// loudly on a malformed extracted plan (e.g. a relink that dropped a child).
	if err := cascades.ValidatePlanInvariants(physPlan); err != nil {
		return nil, fmt.Errorf("plan invariant violated: %w", err)
	}
	return physPlan, nil
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
		return "", api.NewError(api.ErrCodeUnsupportedQuery, "Unsupported operator "+fn)
	}
	if err := resolveQualifiedTableNames(logicalOp, "s"); err != nil {
		return "", err
	}
	if err := validateTablesAndColumns(logicalOp, md); err != nil {
		return "", err
	}
	if msg := findDistinctAggregate(logicalOp); msg != "" {
		// Java rejects DISTINCT aggregates with UNSUPPORTED_QUERY (0AF00); match it.
		return "", api.NewError(api.ErrCodeUnsupportedQuery, msg)
	}

	if arityErr := query.ValidateCTEAliasArities(logicalOp); arityErr != nil {
		return "", arityErr
	}
	ref, _, translateErr := query.TranslateToCascadesWithError(logicalOp, md)
	if translateErr != nil {
		// Surface the translator's typed diagnostic over the generic fallback —
		// same precedence the production generator applies.
		return "", translateErr
	}
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
	// RFC-164 WS-2: structural plan invariants — an always-on backstop that fails
	// loudly on a malformed extracted plan (e.g. a relink that dropped a child).
	if err := cascades.ValidatePlanInvariants(physPlan); err != nil {
		return "", fmt.Errorf("plan invariant violated: %w", err)
	}
	return physPlan.Explain(), nil
}

// PlanRecordQueryWithMetadata is like PlanQueryWithMetadata but returns the
// physical RecordQueryPlan (so callers can execute it against a real store),
// not just its Explain string. The session schema defaults to the embedded
// planner's "s".
//
// The query's scalar subqueries are planned but NOT returned — fine for
// plan-only callers (Explain/shape assertions); a caller that EXECUTES a
// subquery-bearing plan without pre-binding results fails loudly with
// values.UnboundScalarSubqueryError. Executing callers use
// PlanRecordQueryWithSubqueries.
func PlanRecordQueryWithMetadata(sql string, md *recordlayer.RecordMetaData, stats properties.StatisticsProvider) (plans.RecordQueryPlan, error) {
	return PlanRecordQueryWithMetadataSchema(sql, md, defaultEmbeddedSchema, stats)
}

// PlanRecordQueryWithSubqueries is PlanRecordQueryWithMetadata plus the
// query's planned scalar subqueries — planned through planScalarSubqueryPlans,
// the same pipeline the production generator uses. Callers that execute the
// returned plan must pre-evaluate each subquery
// (executor.EvaluateScalarSubquery) and bind the results via
// EvaluationContext.WithScalarSubqueries, exactly as the sql driver's
// fetchPage does; running the outer plan without the bindings fails loudly
// with values.UnboundScalarSubqueryError (never a silent NULL that vanishes
// rows — the bug this API closed).
func PlanRecordQueryWithSubqueries(sql string, md *recordlayer.RecordMetaData, stats properties.StatisticsProvider) (plans.RecordQueryPlan, []PlannedScalarSubquery, error) {
	return planRecordQueryAndSubqueries(sql, md, defaultEmbeddedSchema, stats)
}

// PlanRecordQueryWithMetadataSchema is PlanRecordQueryWithMetadata bound to a
// specific session schema (the real CONNECT schema on the session path —
// cascades_generator.go uses g.c.sess.Schema for the same threading). A
// non-default schema flows through NewPlanVisitorWithSchema AND the
// schema-qualified-table demotion/resolution, so a schema-qualified source —
// including INSIDE a subquery (`… EXISTS (SELECT 1 FROM PA AS main, main.PB AS
// B)` with session schema `main`) — is resolved against the ACTIVE schema, not
// the hardcoded default. RFC-142 (P2b).
func PlanRecordQueryWithMetadataSchema(sql string, md *recordlayer.RecordMetaData, schemaName string, stats properties.StatisticsProvider) (plans.RecordQueryPlan, error) {
	plan, _, err := planRecordQueryAndSubqueries(sql, md, schemaName, stats)
	return plan, err
}

// planRecordQueryAndSubqueries is the shared body of the record-plan harness
// entry points: parse → logical build → translate → Cascades-plan the main
// query AND its collected scalar subqueries.
func planRecordQueryAndSubqueries(sql string, md *recordlayer.RecordMetaData, schemaName string, stats properties.StatisticsProvider) (plans.RecordQueryPlan, []PlannedScalarSubquery, error) {
	if schemaName == "" {
		schemaName = defaultEmbeddedSchema
	}
	root, err := parser.Parse(sql)
	if err != nil {
		return nil, nil, fmt.Errorf("parse SQL: %w", err)
	}
	stmts := root.Statements()
	if stmts == nil || len(stmts.AllStatement()) == 0 {
		return nil, nil, fmt.Errorf("no statements in SQL")
	}
	sel := stmts.AllStatement()[0].SelectStatement()
	if sel == nil {
		return nil, nil, fmt.Errorf("not a SELECT statement")
	}
	q := sel.Query()
	if q == nil {
		return nil, nil, fmt.Errorf("malformed SELECT")
	}

	visitor := NewPlanVisitorWithSchema(md, schemaName)
	logicalOp, buildErr := visitor.VisitQuery(q)
	if buildErr != nil {
		return nil, nil, buildErr
	}
	if logicalOp == nil {
		return nil, nil, api.NewError(api.ErrCodeUnsupportedQuery, "could not build logical plan")
	}
	// Java table-first order: a schema-qualified table mis-classified as a
	// lateral unnest (`FROM PA AS s, s.PB`, alias `s` == schema name) is demoted
	// back to a table scan before validation/translation (or AT-on-a-table is
	// rejected with WRONG_OBJECT_TYPE). RFC-142 (P2b).
	if err := demoteSchemaQualifiedUnnest(logicalOp, schemaName, md); err != nil {
		return nil, nil, err
	}
	// Backstop for AT-on-a-table sources inside a subquery (the per-FROM-scope early
	// pass in VisitQuery runs before subquery plans are attached). Surfaces the
	// faithful WRONG_OBJECT_TYPE before validateTablesAndColumns can mask it with a
	// column-validation error. RFC-142.
	if err := rejectAtOrdinalityOnTable(logicalOp, md); err != nil {
		return nil, nil, err
	}
	// Reject a lateral unnest's AS/AT alias colliding with ANY other FROM-source
	// alias (earlier OR later) in the same scope — the later-source collision the
	// translator's bottom-up lowering cannot see. RFC-142.
	if err := rejectDuplicateUnnestAlias(logicalOp, md); err != nil {
		return nil, nil, err
	}
	if err := resolveQualifiedTableNames(logicalOp, schemaName); err != nil {
		return nil, nil, err
	}
	if err := validateTablesAndColumns(logicalOp, md); err != nil {
		return nil, nil, err
	}

	if arityErr := query.ValidateCTEAliasArities(logicalOp); arityErr != nil {
		return nil, nil, arityErr
	}
	ref, scalarSubqueryPlans, translateErr := query.TranslateToCascadesWithError(logicalOp, md)
	if translateErr != nil {
		// Surface a specific translation error code (RFC-142: AT ordinality on a
		// non-array source → WRONG_OBJECT_TYPE) over the generic fallback.
		return nil, nil, translateErr
	}
	if ref == nil {
		return nil, nil, api.NewError(api.ErrCodeUnsupportedQuery, "Cascades translation failed")
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
		return nil, nil, fmt.Errorf("planning failed: %w", planErr)
	}
	if bestExpr == nil {
		return nil, nil, api.NewError(api.ErrCodeUnsupportedQuery, "no plan found")
	}
	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	ph, ok := bestExpr.(planExtractor)
	if !ok {
		return nil, nil, fmt.Errorf("best expression is not a physical plan: %T", bestExpr)
	}
	physPlan := ph.GetRecordQueryPlan()
	if physPlan == nil {
		return nil, nil, fmt.Errorf("physical plan is nil")
	}
	// RFC-164 WS-2: structural plan invariants (see ValidatePlanInvariants).
	if err := cascades.ValidatePlanInvariants(physPlan); err != nil {
		return nil, nil, fmt.Errorf("plan invariant violated: %w", err)
	}
	// Plan the translator-collected scalar subqueries through the SAME pipeline
	// the production generator uses; PlanRecordQueryWithSubqueries surfaces
	// them, the plan-only entry points drop them (execution without the
	// bindings is loud — values.UnboundScalarSubqueryError).
	subs, subErr := planScalarSubqueryPlans(scalarSubqueryPlans, md, stats)
	if subErr != nil {
		return nil, nil, subErr
	}
	return physPlan, subs, nil
}

// ResultColumnLabelsForPlan returns the user-visible result-set column labels a
// plan would advertise — the metadata-only (no-FDB) analog of the driver's
// paginatingRows.Columns(): it runs the SAME production column derivation
// (deriveColumnsFromPlan, the function the live Execute() path calls) and maps
// each ColumnDef to its label exactly as Columns() does (Label, or Name when the
// label is empty), upper-cased. This lets the planner harness assert the result
// COLUMN SET — distinct from the per-row datum map (which carries extra
// resolution-convenience keys) — for shapes that cannot be seeded through the SQL
// driver (e.g. non-empty array columns for a lateral unnest, which have no SQL
// array-literal form). RFC-142.
func ResultColumnLabelsForPlan(plan plans.RecordQueryPlan, md *recordlayer.RecordMetaData) []string {
	cols := deriveColumnsFromPlan(plan, md)
	labels := make([]string, len(cols))
	for i, c := range cols {
		if c.Label != "" {
			labels[i] = strings.ToUpper(c.Label)
		} else {
			labels[i] = strings.ToUpper(c.Name)
		}
	}
	return labels
}

// ResultColumnTypesForPlan returns the SQL TYPE NAME advertised for each
// result-set column, in order — the metadata-only (no-FDB) analog of the
// driver's column-type metadata. It runs the SAME production column derivation
// (deriveColumnsFromPlan → ColumnDef.TypeName) the live Execute() path uses, so
// the harness can assert column types for shapes that cannot be seeded through
// the SQL driver (e.g. a lateral unnest over a non-empty array column, which has
// no SQL array-literal form). The element column of a non-ordinal unnest over a
// STRING array must report STRING here, not the UnknownType→BIGINT fallback.
// RFC-142.
func ResultColumnTypesForPlan(plan plans.RecordQueryPlan, md *recordlayer.RecordMetaData) []string {
	cols := deriveColumnsFromPlan(plan, md)
	types := make([]string, len(cols))
	for i, c := range cols {
		types[i] = strings.ToUpper(c.TypeName)
	}
	return types
}

// ResultColumnNullabilityForPlan returns the JDBC NULLABILITY flag advertised
// for each result-set column, in order — the metadata-only (no-FDB) analog of
// the driver's ResultSetMetaData.isNullable. It runs the SAME production column
// derivation (deriveColumnsFromPlan → ColumnDef.Nullable) the live Execute()
// path uses, so the harness can assert column nullability for shapes that cannot
// be seeded through the SQL driver (e.g. a lateral unnest over a non-empty array
// column, which has no SQL array-literal form). The WITH-ORDINALITY ordinal
// column must report api.ColumnNoNulls here (Java's INT NOT NULL ordinal), even
// though it has no backing proto descriptor field. RFC-142.
func ResultColumnNullabilityForPlan(plan plans.RecordQueryPlan, md *recordlayer.RecordMetaData) []int {
	cols := deriveColumnsFromPlan(plan, md)
	nulls := make([]int, len(cols))
	for i, c := range cols {
		nulls[i] = c.Nullable
	}
	return nulls
}

// ResultColumnDefsForPlan returns the FULL production ColumnDef set for a plan
// — the same deriveColumnsFromPlan output the live Execute() path hands to
// NewRecordLayerResultSet — so an FDB test can drive the REAL result-set read
// path (including the positional-aligned column read) for shapes
// that cannot be seeded through the SQL driver (no SQL array-literal form).
func ResultColumnDefsForPlan(plan plans.RecordQueryPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	return deriveColumnsFromPlan(plan, md)
}

// verifyOneFinalEnabled / lastOneFinalViolations back planAndVerifyOneFinal.
//
// Package-level rather than threaded through PlanPhysicalForTest's signature:
// the check is a diagnostic for one test, and widening the signature of a
// helper called from ~40 places to carry it would be the tail wagging the dog.
// Guarded by verifyOneFinalMu because tests run in parallel.
var (
	verifyOneFinalMu       sync.Mutex
	verifyOneFinalEnabled  bool
	lastOneFinalViolations []string
)

// planAndVerifyOneFinal plans sql and returns every reference reachable at
// extraction that holds more than one PHYSICAL final expression — RFC-183
// P5's precondition (Java's getRangesOverPlan is getOnlyElement over the
// final expressions, which throws on two).
//
// Serialized: it flips package-level planner state, so concurrent callers
// would read each other's results.
func planAndVerifyOneFinal(sql, schema string) ([]string, error) {
	verifyOneFinalMu.Lock()
	defer verifyOneFinalMu.Unlock()

	verifyOneFinalEnabled = true
	lastOneFinalViolations = nil
	defer func() {
		verifyOneFinalEnabled = false
		lastOneFinalViolations = nil
	}()

	if _, err := PlanPhysicalForTest(sql, schema, nil); err != nil {
		return nil, err
	}
	out := make([]string, len(lastOneFinalViolations))
	copy(out, lastOneFinalViolations)
	return out, nil
}
