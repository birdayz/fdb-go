package embedded

// PlanVisitor walks ANTLR parse tree nodes and builds a
// logical.LogicalOperator tree for the Cascades planner path.
//
// Architecture: Java's QueryVisitor.visitSimpleTable builds the plan in
// this order: FROM -> WHERE -> GROUP BY + SELECT + HAVING -> ORDER BY ->
// (final projection) -> DISTINCT. Each step takes the current operator and
// wraps it. SQL LIMIT/OFFSET is a Go-only extension Java's fdb-relational
// lacks; it is wrapped LAST (outermost), after DISTINCT, so it applies after
// dedup per SQL semantics (RFC-128).
//
// PlanVisitor mirrors this incremental wrapping: visitFrom builds the
// scan/join subtree, visitWhere wraps it with a filter, visitSelectGroupBy
// wraps it with aggregate/projection, visitOrderBy wraps it with sort,
// visitFinalProjection wraps it with the non-aggregate projection and
// DISTINCT, and finally visitLimit wraps the whole thing with the limit.
//
// The complex aggregate classification (SELECT element parsing, GROUP BY
// interaction, HAVING harvesting) delegates to classifySelectElements
// which returns a selectClassification — NOT a selectQuery. The operator
// tree is built directly by the visit methods. When metadata is available,
// a selectQuery is constructed from the selectClassification (which it
// embeds) + fromSource so the upgrade functions can run. The catalog-
// aware upgrades (predicate resolution, column validation, Value
// resolution, subquery planning) are inlined into VisitSimpleTable
// rather than delegated to the monolithic _postBuild function.
//
// The selectQuery parse path (extractFromSimpleTable) remains the other
// consumer of classifySelectElements; both builders share the same
// classification so their aggregate layouts agree.

import (
	"errors"
	"maps"
	"strconv"
	"strings"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
)

// PlanVisitor builds LogicalOperator trees from ANTLR parse nodes.
// It holds the metadata needed for catalog-aware resolution (predicate
// upgrade, column validation, sort-key resolution) and any CTE column
// schemas accumulated from WITH clause processing.
type PlanVisitor struct {
	// preparedQueryBodies is construction-only memoization for recursive seeds.
	// The seed used to establish the temporary row is reused in the final UNION.
	preparedQueryBodies map[antlrgen.IQueryExpressionBodyContext]logical.LogicalOperator
	bindings            *bindingAllocator
	enclosingScope      *semantic.Scope
	md                  *recordlayer.RecordMetaData
	cteScopes           map[string]semantic.ScopeSource
	cteProducers        logical.CTERegistry
	// cteOnScopes carries the ON-resolution-only sources for declared CTEs
	// whose schema derivation declined the global cteScopes (join/unnest
	// bodies) — consumed ONLY by upgradeJoinOnPredicates so an enclosing
	// explicit join's ON resolves (or fails LOUD via the nil-Table marker)
	// instead of being silently dropped. Prepared bodies publish exact schemas;
	// an unrepresentable row retains a nil-Table marker.
	cteOnScopes map[string]semantic.ScopeSource

	// schemaName is the session schema (e.g. "s"). It is used ONLY to run Java's
	// table-first resolution order in the lateral-unnest classifier: a dotted
	// FROM source `schemaName.Table` is a schema-qualified TABLE, not a correlated
	// unnest, even when the qualifier also names a prior FROM-source alias
	// (`FROM PA AS s, s.PB`). RFC-142 (P2b).
	schemaName string

	// inRecursiveCTEBody is set while building the body of a recursive
	// CTE so the union builder permits UNION DISTINCT (bare UNION)
	// which is valid for cycle detection. Outside of recursive CTEs,
	// UNION DISTINCT is rejected (only UNION ALL is supported).
	inRecursiveCTEBody bool
}

// collectSelectNames does a lightweight scan of the SELECT element list
// to extract output column names and aliases. It does NOT perform
// aggregate classification — it simply returns the surface-level name
// for each SELECT element position, used by ORDER BY positional
// reference resolution.
//
// For COUNT(*) or aggregate functions, it returns the canonical
// reconstructed name (e.g. "COUNT(*)", "SUM(v)"). For plain columns,
// it returns the column name. For computed expressions, it returns
// either the alias or the canonical expression text. SELECT * and
// SELECT qualifier.* return nil (positional refs are invalid).
func collectSelectNames(simpleTable *antlrgen.SimpleTableContext) (cols []string, aliases []string) {
	selElems := simpleTable.SelectElements()
	if selElems == nil {
		return nil, nil
	}
	elems := selElems.AllSelectElement()
	for _, elem := range elems {
		switch e := elem.(type) {
		case *antlrgen.SelectStarElementContext:
			// SELECT * — positional refs invalid (no named columns)
			return nil, nil
		case *antlrgen.SelectQualifierStarElementContext:
			if len(elems) == 1 {
				// sole qualifier.* — no positional refs
				return nil, nil
			}
			// mixed: placeholder slot
			cols = append(cols, "")
			aliases = append(aliases, "")
		case *antlrgen.SelectExpressionElementContext:
			alias := selectOutputAlias(e)
			// Try plain column name first.
			colName, nameErr := columnNameFromExpr(e.Expression(), "SELECT expression")
			if nameErr != nil {
				// Computed expression: use alias if present, else
				// canonical expression text.
				if alias != "" {
					cols = append(cols, alias)
				} else {
					cols = append(cols, canonicalTextOf(e.Expression()))
				}
				aliases = append(aliases, alias)
			} else {
				cols = append(cols, colName)
				aliases = append(aliases, alias)
			}
		}
	}
	return cols, aliases
}

// NewPlanVisitor creates a PlanVisitor with the given metadata, defaulting the
// session schema to the embedded planner's "s". md may be nil; all catalog-aware
// upgrades degrade to text fallback.
func NewPlanVisitor(md *recordlayer.RecordMetaData) *PlanVisitor {
	return &PlanVisitor{md: md, schemaName: defaultEmbeddedSchema}
}

// NewPlanVisitorWithSchema creates a PlanVisitor bound to a specific session
// schema (the real CONNECT schema on the session path). RFC-142.
func NewPlanVisitorWithSchema(md *recordlayer.RecordMetaData, schemaName string) *PlanVisitor {
	if schemaName == "" {
		schemaName = defaultEmbeddedSchema
	}
	return &PlanVisitor{md: md, schemaName: schemaName}
}

// VisitQuery is the top-level entry point. It handles WITH (CTE)
// wrapping and then delegates to VisitQueryBody for the main query.
//
// Mirrors buildLogicalPlanForQueryWithCatalog: pre-scans CTE
// definitions to extract column schemas, then recursively builds the
// main query body with CTE scopes in context.
func (v *PlanVisitor) VisitQuery(q antlrgen.IQueryContext) (logical.LogicalOperator, error) {
	if q == nil {
		return nil, nil
	}
	if v.md == nil {
		plan := buildLogicalPlanForQuery(q)
		logical.BindCTESources(plan, v.cteProducers)
		return plan, nil
	}
	ctesCtx := q.Ctes()
	var declarations []*logical.CTEProducer
	if ctesCtx != nil {
		if v.cteScopes == nil {
			v.cteScopes = make(map[string]semantic.ScopeSource)
		}
		if v.cteOnScopes == nil {
			v.cteOnScopes = make(map[string]semantic.ScopeSource)
		}
		predeclared := make(map[string]struct{})
		for _, nq := range ctesCtx.AllNamedQuery() {
			name := functions.FullIdToName(nq.GetName())
			upper := strings.ToUpper(name)
			if _, exists := predeclared[upper]; exists {
				return nil, api.NewErrorf(api.ErrCodeDuplicateAlias, "found '%s' more than once", name)
			}
			predeclared[upper] = struct{}{}
		}
		recursive := ctesCtx.RECURSIVE() != nil
		traversal := logical.TraversalAnyOrder
		if toc := ctesCtx.TraversalOrderClause(); toc != nil {
			traversal = logical.TraversalLevelOrder
			if toc.PRE_ORDER() != nil {
				traversal = logical.TraversalPreOrder
			} else if toc.POST_ORDER() != nil {
				traversal = logical.TraversalPostOrder
			}
		}
		for _, nq := range ctesCtx.AllNamedQuery() {
			name := functions.FullIdToName(nq.GetName())
			upper := strings.ToUpper(name)
			var aliases []string
			if list, ok := nq.GetColumnAliases().(*antlrgen.FullIdListContext); ok && list != nil {
				for _, id := range list.AllFullId() {
					aliases = append(aliases, functions.FullIdToName(id))
				}
			}
			var seedContext antlrgen.IQueryExpressionBodyContext
			var seed logical.LogicalOperator
			if recursive {
				if !containsTableRef(nq.Query().QueryExpressionBody(), upper) {
					return nil, api.NewError(api.ErrCodeUnsupportedOperation, "condition is not met!")
				}
				if _, isSet := nq.Query().QueryExpressionBody().(*antlrgen.SetQueryContext); !isSet {
					return nil, api.NewError(api.ErrCodeUnsupportedOperation, "recursive CTE requires UNION ALL body")
				}
				if nq.Query().Ctes() != nil {
					return nil, api.NewError(api.ErrCodeUnsupportedQuery, "nested WITH inside a recursive CTE body is not supported")
				}
				setQuery := nq.Query().QueryExpressionBody().(*antlrgen.SetQueryContext)
				seedContext = setQuery.GetLeft()
				previousRecursive := v.inRecursiveCTEBody
				v.inRecursiveCTEBody = true
				var seedErr error
				seed, seedErr = v.visitUnionBranch(seedContext)
				v.inRecursiveCTEBody = previousRecursive
				if seedErr != nil {
					return nil, seedErr
				}
				logical.BindCTESources(seed, v.cteProducers)
				source, exact := exactVirtualScopeSource(name, seed, v.md, nil, v.cteScopes)
				if exact {
					if len(aliases) > 0 && len(aliases) != len(source.Table.Columns()) {
						return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference, "cte query has %d column(s), however %d aliases defined", len(source.Table.Columns()), len(aliases))
					}
					v.cteScopes[upper] = applyCTEColumnAliases(source, nq.GetColumnAliases())
					delete(v.cteOnScopes, upper)
				} else {
					v.cteScopes[upper] = semantic.ScopeSource{}
					v.cteOnScopes[upper] = semantic.ScopeSource{}
				}
			}

			producer, err := logical.PrepareCTE(name, recursive, v.cteProducers, func(registry logical.CTERegistry) (logical.LogicalOperator, error) {
				previous, wasRecursive, previousBodies := v.cteProducers, v.inRecursiveCTEBody, v.preparedQueryBodies
				v.cteProducers, v.inRecursiveCTEBody = registry, recursive
				if recursive {
					v.preparedQueryBodies = map[antlrgen.IQueryExpressionBodyContext]logical.LogicalOperator{seedContext: seed}
					source := v.cteScopes[upper]
					source.CTE = registry.Lookup(name, fullIDSegments(nq.GetName())...)
					v.cteScopes[upper] = source
				}
				defer func() {
					v.cteProducers, v.inRecursiveCTEBody, v.preparedQueryBodies = previous, wasRecursive, previousBodies
				}()
				return v.buildCTEBodyQuery(nq.Query())
			}, logical.CTEColumns(aliases...), logical.CTETraversal(traversal), logical.CTENamePath(fullIDSegments(nq.GetName())...))
			if err != nil {
				return nil, err
			}
			if producer.Body() == nil {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery, "CTE body has no logical plan")
			}
			if !recursive {
				source, exact := exactVirtualScopeSource(name, producer.Body(), v.md, nil, v.cteScopes)
				if !exact {
					delete(v.cteScopes, upper)
					v.cteOnScopes[upper] = semantic.ScopeSource{}
				} else {
					if len(aliases) > 0 && len(aliases) != len(source.Table.Columns()) {
						return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference, "cte query has %d column(s), however %d aliases defined", len(source.Table.Columns()), len(aliases))
					}
					v.cteScopes[upper] = applyCTEColumnAliases(source, nq.GetColumnAliases())
					delete(v.cteOnScopes, upper)
				}
			}
			source, ok := v.cteScopes[upper]
			if ok {
				source.CTE = producer
				v.cteScopes[upper] = source
			}
			onSource, ok := v.cteOnScopes[upper]
			if ok {
				onSource.CTE = producer
				v.cteOnScopes[upper] = onSource
			}
			v.cteProducers = v.cteProducers.With(producer)
			declarations = append(declarations, producer)
		}
	}
	main, err := v.VisitQueryBody(q.QueryExpressionBody())
	if err != nil || main == nil {
		return main, err
	}
	logical.BindCTESources(main, v.cteProducers)
	for i := len(declarations) - 1; i >= 0; i-- {
		main = logical.NewCTEReference(declarations[i], main)
	}
	if err := bindExactCTEOutputMetadata(main, v.md); err != nil {
		return nil, err
	}
	return main, nil
}

// VisitQueryBody dispatches simple SELECT vs UNION, threading
// metadata and CTE scopes through both arms.
func (v *PlanVisitor) VisitQueryBody(body antlrgen.IQueryExpressionBodyContext) (logical.LogicalOperator, error) {
	if prepared := v.preparedQueryBodies[body]; prepared != nil {
		return prepared, nil
	}
	if body == nil {
		return nil, nil
	}
	switch b := body.(type) {
	case *antlrgen.QueryTermDefaultContext:
		return v.VisitSimpleTable(b)
	case *antlrgen.SetQueryContext:
		return v.visitUnion(b)
	}
	return nil, nil
}

// VisitSimpleTable is the main SELECT visitor. It walks the ANTLR tree
// incrementally, building the LogicalOperator tree step by step in the
// same order as Java's QueryVisitor.visitSimpleTable:
//
//  1. FROM clause  → visitFrom     → scan/derived/join operator
//  2. WHERE clause → visitWhere    → wrap with filter
//  3. SELECT+GROUP BY+HAVING → visitSelectGroupBy → wrap with aggregate
//  4. ORDER BY     → visitOrderBy  → wrap with sort (ANTLR direct)
//  5. LIMIT/OFFSET → visitLimit    → wrap with limit (ANTLR direct)
//  6. Projection   → visitFinalProjection + DISTINCT (ANTLR direct)
//  7. Catalog-aware upgrades (inline) → predicate resolution, column
//     validation, Value resolution for projections/aggregates/sort keys,
//     qualified star expansion, EXISTS/scalar subquery planning.
//
// Aggregate classification delegates to classifySelectElements which
// returns a selectClassification. When metadata is available, the
// classification is bridged to a selectQuery for the upgrade functions
// that consume it — the operator tree itself is built directly by the
// visit methods.
func (v *PlanVisitor) VisitSimpleTable(termCtx *antlrgen.QueryTermDefaultContext) (logical.LogicalOperator, error) {
	if termCtx == nil {
		return nil, nil
	}
	simpleTable, ok := termCtx.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		return nil, nil
	}
	return v.visitSimpleTableBody(simpleTable)
}

// VisitQueryTerm plans a bare queryTerm parse node — the shape a
// `CREATE INDEX … AS <queryTerm>` definition carries (RFC-202). It is the
// same planning path as VisitSimpleTable, entered without the enclosing
// queryExpressionBody wrapper a full query has. Mirrors Java's
// DdlVisitor.visitIndexAsSelectDefinition, where
// `indexDefinitionContext.queryTerm().accept(this)` reaches the ordinary
// query visitor (DdlVisitor.java:211).
func (v *PlanVisitor) VisitQueryTerm(qt antlrgen.IQueryTermContext) (logical.LogicalOperator, error) {
	simpleTable, ok := qt.(*antlrgen.SimpleTableContext)
	if !ok {
		return nil, nil
	}
	return v.visitSimpleTableBody(simpleTable)
}

func (v *PlanVisitor) visitSimpleTableBody(simpleTable *antlrgen.SimpleTableContext) (logical.LogicalOperator, error) {
	op, err := v.visitSimpleTableBodyUnfolded(simpleTable)
	if err != nil {
		return nil, err
	}
	if simpleTable.QualifyClause() != nil {
		if err := retainQualifyProvenance(op); err != nil {
			return nil, err
		}
	}
	// The block's last step: an inner join's ON-clause EXISTS becomes a
	// WHERE-EXISTS (on_exists_fold.go), so no plan leaves the builder with a
	// join carrying its own existential.
	return foldInnerOnExistsIntoWhere(op)
}

// visitSimpleTableBodyUnfolded builds the block; visitSimpleTableBody folds
// its ON-clause EXISTS afterwards.
func (v *PlanVisitor) visitSimpleTableBodyUnfolded(simpleTable *antlrgen.SimpleTableContext) (logical.LogicalOperator, error) {
	// Step 1: FROM → parse the source first. Java's QueryVisitor
	// rejects FROM-less SELECTs before any function dispatch, so
	// parseFromSource must run before classification/validation.
	fs, err := parseFromSource(simpleTable)
	if err != nil {
		return nil, err
	}
	fs.enclosingScope = v.enclosingScope
	v.assignDerivedSourceBindings(fs)
	if err := v.prepareDerivedSourceBodies(fs); err != nil {
		return nil, err
	}

	// Classify SELECT elements, GROUP BY, HAVING, ORDER BY.
	//
	// The classifier is handed a star expander built from the FROM clause
	// alone. A `SELECT *` under GROUP BY has to be expanded BEFORE the
	// grouping rules are applied to it — Java generates the select-where
	// operator first (QueryVisitor.java:275) and expands the star against it
	// (QueryVisitor.java:286) before generateGroupBy validates the expansion
	// (LogicalOperator.java:436-439). Everything else in the classifier is
	// still pure parse-tree work.
	// The expander is built only when the branch that consumes it can be
	// reached at all, and even then it builds its scope LAZILY.
	//
	// This path runs for EVERY SELECT while the star-under-GROUP-BY branch
	// needs a GROUP BY clause to fire, so the parse-tree presence of one is a
	// free and exact precondition. MEASURED, because laziness alone was not
	// free: deferring the scope saved 12 allocs / ~600 B per plan on the
	// star-free shapes but allocated a closure on every call here, which a
	// join-heavy plan makes many of — two_table_join went +51 allocs. Gating on
	// the GROUP BY removes both costs, since a query with no GROUP BY now
	// allocates nothing at all for star expansion.
	//
	// A nil expander is the classifier's "cannot expand" signal and keeps the
	// asserted 42803 refusal. That is correct here rather than merely cheap: with
	// no GROUP BY clause the classifier never consults the expander, so nil and
	// a working expander are indistinguishable to it.
	var expandStar starExpander
	if simpleTable.GroupByClause() != nil {
		expandStar = starExpanderFor(fs, v.md, v.schemaName, v.cteScopes)
	}
	cls, err := classifySelectElements(simpleTable, expandStar)
	if err != nil {
		return nil, err
	}

	// Validate unsupported functions before building the plan.
	for _, expr := range cls.projExprs {
		if fn := findUnsupportedFunctionInParseTree(expr); fn != "" {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"Unsupported operator "+fn)
		}
	}

	resolvesToTable := newUnnestTableResolver(v.md, v.schemaName)
	if err := retargetUsingJoins(fs.tableName, fs.tableAlias,
		fs.derivedQuery == nil && fs.inlineValues == nil && fs.tableName != "",
		fs.derivedQuery, fs.catalogAwareInnerPlan, fs.joins, v.md, v.schemaName,
		cteNamePredicate(v.cteScopes), v.cteScopes); err != nil {
		return nil, err
	}

	op, err := v.visitFrom(simpleTable, fs)
	if err != nil {
		return nil, err
	}
	if op == nil {
		return nil, nil
	}

	// AT-on-a-table rejection (Java's generateAccess, at FROM-source analysis
	// time): a comma source carrying an AT ordinal alias that is in truth a TABLE /
	// non-array source (`FROM T1, U AT O`, `FROM T1, T1.ID AS X AT O`, …) is
	// WRONG_OBJECT_TYPE. Surfacing it HERE — before the SELECT/WHERE column
	// resolution below — prevents a scope-level undefined-column (the AT source's
	// virtual unnest binding shadows the real table, so a `U.ID` reference fails to
	// resolve) from MASKING the intended 42809. Mirrors the translator's
	// translateUnnestJoin AT-rejection exactly. RFC-142.
	if v.md != nil {
		if err := rejectAtOrdinalityOnTableWithCTEs(op, v.md, v.cteProducers); err != nil {
			return nil, err
		}
	}
	// Repeated SQL labels (including AS X AT X) are legal here. Resolution
	// reports ambiguity only when a reference selects competing attributes;
	// the element and ordinal retain separate physical slots. RFC-256.

	// Step 2: WHERE → wrap with filter directly from ANTLR.
	op = v.visitWhere(op, simpleTable)

	// Step 3: SELECT + GROUP BY + HAVING → aggregate classification
	// and operator building.
	op, stripPrefix, err := v.visitSelectGroupBy(op, cls, fs)
	if err != nil {
		return nil, err
	}

	// Collect SELECT column names and aliases from ANTLR for ORDER BY
	// positional reference resolution. This is a lightweight scan —
	// aggregate classification stays in the selectClassification.
	selectCols, selectAliases := collectSelectNames(simpleTable)

	// Step 4: ORDER BY → wrap with sort directly from ANTLR. Reads
	// simpleTable.OrderByClause() and resolves positional references
	// against the SELECT column list.
	hasAggregate := cls.countStar || len(cls.aggCols) > 0
	op = v.visitOrderBy(op, simpleTable, selectCols, selectAliases, cls.aggCols, stripPrefix, groupKeyRefDisplays(cls.groupBy), cls.groupByAliases, cls.postSortStripProj, cls.postSortSQLNames, cls.postSortAggregateOutputOrdinals)

	// Post-sort strip projection: when hasSortOnly is true in the
	// aggregate path, the visible-only projection is deferred past
	// Sort so sort-key columns remain accessible.
	if len(cls.postSortStripProj) > 0 {
		proj := logical.NewProject(op, cls.postSortStripProj, cls.postSortStripAliases)
		proj.AggregateOutputOrdinals = append([]int(nil), cls.postSortAggregateOutputOrdinals...)
		proj.IsComputed = append([]bool(nil), cls.postSortIsComputed...)
		proj.SQLNames = append([]string(nil), cls.postSortSQLNames...)
		op = proj
	}

	// Step 5: Projection (non-aggregate) + DISTINCT → directly from
	// ANTLR. Only builds a projection for non-aggregate queries;
	// aggregate queries have their projection handled in visitSelectGroupBy.
	op = v.visitFinalProjection(op, simpleTable, hasAggregate, stripPrefix)
	if simpleTable.DISTINCT() != nil {
		op = logical.NewDistinct(op)
	}

	// Step 6: LIMIT/OFFSET → wrap with limit directly from ANTLR. The LIMIT
	// is the OUTERMOST operator so it applies LAST — after the final
	// projection AND DISTINCT — matching SQL semantics (FROM→WHERE→GROUP BY→
	// HAVING→SELECT/DISTINCT→ORDER BY→LIMIT). RFC-128: with the LIMIT now a
	// real RecordQueryLimitPlan operator at its built position (no
	// post-execution hoist), stacking it below DISTINCT would dedup AFTER the
	// cap and return the wrong rows. It must wrap everything.
	op, limitErr := v.visitLimit(op, simpleTable)
	if limitErr != nil {
		return nil, limitErr
	}

	if v.md == nil {
		return op, nil
	}

	// --- Catalog-aware upgrades (inlined from _postBuild) ---
	//
	// Build a selectQuery from the classification + FROM source for the
	// upgrade functions. The operator tree was already built by the
	// visit methods above; the selectQuery carries parse-tree metadata
	// that the upgrade functions need for semantic resolution.
	sq := selectQueryFromClassification(cls, fs)
	sq.enclosingScope = v.enclosingScope
	rememberSchemaAliasTableQualifiers(sq, resolvesToTable)
	queryCTEScopes := singleSourceQueryBlockCTEScopes(sq, v.cteScopes, v.cteOnScopes)

	// Build the semantic scope once. All identifier resolution goes
	// through this scope — same architecture as Java's QueryVisitor
	// holding a SemanticAnalyzer.
	resolver := buildSelectScope(sq, v.md, v.schemaName, queryCTEScopes)

	// (1) Expand qualified stars (a.*) in the projection list.
	needRebuild := false
	if sq.projQualifier != "" && sq.projCols == nil {
		normalizeSoleQualifiedStar(sq)
		needRebuild = true
	}
	// A bare `SELECT *` over version-storing base tables expands into an
	// explicit non-ephemeral projection (the __ROW_VERSION pseudo-field must
	// not surface through the star — Java's nonEphemeralVisible star over the
	// ephemeral table-access attribute).
	if expanded, err := expandBareStarFromScope(sq, v.md, v.schemaName, queryCTEScopes); err != nil {
		return nil, err
	} else if expanded {
		needRebuild = true
	}
	if hasAnyQualifiedStar(sq) {
		if starErr := expandQualifiedStars(sq, v.md, v.schemaName, queryCTEScopes); starErr != nil {
			return nil, starErr
		}
		needRebuild = true
	}
	if needRebuild {
		// The rebuild REPLACES op, discarding the LIMIT wrapper visitLimit
		// applied above — and sq (from selectQueryFromClassification) carries
		// limit:-1. Carry the clause's LIMIT/OFFSET into sq so buildSelectShell
		// re-applies it (RFC-128: with the post-execution hoist removed, the
		// in-tree operator is the only carrier; without this `SELECT a.* … LIMIT
		// 5` returned all rows). Only the rebuild path needs it — the non-rebuild
		// path keeps the visitLimit wrapper.
		// visitLimit above already validated the clause (a bad literal would
		// have returned early), so this re-read cannot error — but propagate
		// it rather than discard, keeping the reject total.
		var limitErr error
		if sq.limit, sq.offset, limitErr = parseLimitClause(simpleTable); limitErr != nil {
			return nil, limitErr
		}
		op = buildLogicalPlanForSelect(sq)
		if op == nil {
			return op, nil
		}
	}

	if err := bindLateralCollections(op, sq, v.md, v.schemaName, queryCTEScopes); err != nil {
		return nil, err
	}

	// (2) Resolve projection columns through the scope.
	if resolver != nil && sq.projCols != nil && len(sq.aggCols) == 0 && !sq.countStar {
		proj := findProjection(op)
		for i, col := range sq.projCols {
			if col.bound != nil {
				if proj == nil || i >= len(proj.Projections) {
					return nil, api.NewError(api.ErrCodeInternalError, "star attribute has no logical projection slot")
				}
				if proj.ProjectedValues == nil {
					proj.ProjectedValues = make([]values.Value, len(proj.Projections))
				}
				proj.ProjectedValues[i] = col.bound
				continue
			}

			if i < len(sq.projExprs) && sq.projExprs[i] != nil {
				if proj != nil {
					wv, walkErr := resolver.WalkExpression(sq.projExprs[i])
					if walkErr != nil {
						var corrErr *CorrelatedExistsError
						if errors.As(walkErr, &corrErr) {
							return nil, corrErr
						}
						// A failed identifier is a semantic error, not an
						// unsupported-shape decline. Keep the first projection's
						// full reference instead of reporting a later column.
						var missing *semantic.ColumnNotFoundError
						if errors.As(walkErr, &missing) {
							return nil, mapColumnResolveError(walkErr, missing.Reference())
						}
						// The plan-time cast-pair gate's own rejection
						// (ResolveCast → 22F3H "No cast defined") must
						// surface verbatim: the name-channel fallback
						// below cannot plan the cast either and would
						// die later as an opaque 0AF00. Other walk
						// failures keep the fallback — they are
						// unwalkable-shape declines the legacy channel
						// may still plan.
						var apiErr *api.Error
						if errors.As(walkErr, &apiErr) && apiErr.Code == api.ErrCodeInvalidCast {
							return nil, apiErr
						}
					}
					if walkErr == nil && wv != nil {
						if proj.ProjectedValues == nil {
							proj.ProjectedValues = make([]values.Value, len(proj.Projections))
						}
						if i < len(proj.ProjectedValues) {
							proj.ProjectedValues[i] = wv
						}
					}
				}
				continue
			}
			if col.bare != "" {
				if err := resolveColumnRefStructural(resolver, col.bare, col.qualifier, col.qualified, col.segs); err != nil {
					return nil, err
				}
			} else if err := resolveColumnName(resolver, col.name); err != nil {
				return nil, err
			}
			// A BARE column that binds to a lateral-unnest SHADOWING source
			// (`FROM t, t.arr AS v, …`) must be projected QUALIFIED to the unnest
			// correlation (`v.v`), not as a bare `v`. The unnest element flows the
			// merged row under both bare `v` and qualified `v.v`, but a LATER FROM
			// item with its own `v` overwrites the bare key last-leg-wins in
			// mergeRows; the qualified `v.v` survives (dotted keys are preserved
			// verbatim). Without this the bare projection reads the wrong column
			// (P2, silent-wrong). RFC-142.
			if !col.qualified && col.bare != "" && proj != nil {
				id := semantic.FromNormalized(col.bare)
				qv, ok, qerr := resolver.ResolveColumnShadowingQualified(semantic.Identifier{}, id)
				if qerr == nil && ok {
					if proj.ProjectedValues == nil {
						proj.ProjectedValues = make([]values.Value, len(proj.Projections))
					}
					if i < len(proj.ProjectedValues) {
						proj.ProjectedValues[i] = qv
					}
				}
				var unresShadow *expr.UnresolvableOrdinalError
				if errors.As(qerr, &unresShadow) {
					// Born-baked (slice 2): the scope bound the name but the
					// source cannot answer a plan-time ordinal — never fall
					// through to the name channel.
					return nil, unresShadow
				}
				// A BARE non-shadowed column resolves through the scope so the
				// projection carries the construction-bound ordinal, by the SAME
				// shape rule the qualified site uses (resolveBaked). An
				// unresolvable name or a lazy result keeps the translator's name
				// emission unchanged.
				//
				// This used to accept ONLY the childless single-source bind, and
				// that is what made a bare reference lose an ordinal a qualified
				// one kept: over a multi-source FROM the resolver takes its
				// CORRELATED arm and hands back a QOV-child value, which the old
				// test rejected — leaving the slot nil with a good ordinal in
				// hand, for translateProjectOverExistsFilter to mint a lazy
				// carrier it has no bake pass to recover (RFC-223 §2).
				//
				// childlessOK=true preserves the single-source bind exactly. It
				// cannot admit a childless bake over a join: `needsQualification`
				// routes every multi-source bare resolution through the
				// correlated arm, so no childless value is produced there in the
				// first place.
				if proj.ProjectedValues == nil || (i < len(proj.ProjectedValues) && proj.ProjectedValues[i] == nil) {
					rv, rerr := resolveBareProjectionValue(resolver, col.bare)
					if rerr == nil {
						if resolved := resolveProjectionValue(rv); resolved != nil {
							if proj.ProjectedValues == nil {
								proj.ProjectedValues = make([]values.Value, len(proj.Projections))
							}
							if i < len(proj.ProjectedValues) {
								proj.ProjectedValues[i] = resolved
							}
						}
					}
					var unresIdent *expr.UnresolvableOrdinalError
					if errors.As(rerr, &unresIdent) {
						return nil, unresIdent
					}
				}
			}
			if col.qualified && proj != nil {
				if proj.ProjectedValues == nil {
					proj.ProjectedValues = make([]values.Value, len(proj.Projections))
				}
				if len(sq.joins) > 0 {
					// Qualified projections over joins run the per-attribute
					// check (Java's 42702), and a reference binding to a
					// LATER duplicate-alias leg is emitted QOV-correlated to
					// that leg's binding so the ordinal bake addresses the
					// right quantifier. Every other reference keeps the
					// alias-keyed merged-row read.
					qv, qerr := resolver.ResolveQualifiedProjectionPath(
						colRefIdentifiers(col.bare, col.qualifier, col.qualified, col.segs))
					if qerr != nil {
						var ambigErr *semantic.AmbiguousColumnError
						if errors.As(qerr, &ambigErr) {
							return nil, api.NewErrorf(api.ErrCodeAmbiguousColumn,
								"Ambiguous reference %s", ambigErr.Reference())
						}
						return nil, qerr
					}
					if i < len(proj.ProjectedValues) {
						if qv != nil {
							proj.ProjectedValues[i] = qv
						} else if bv := resolveQualifiedProjectionValuePath(resolver,
							colRefIdentifiers(col.bare, col.qualifier, col.qualified, col.segs)); bv != nil {
							// A qualified projection over a join emits the
							// resolver's QUANTIFIER-ADDRESSED source-relative
							// baked reference — the executor binds the leg
							// window off the merged row's own leg boundaries
							// (rowLegsBinder), so the read is positional. A
							// DUPLICATED bare leaf keeps its QUALIFIED datum
							// key (alias-pinned) so the two same-named columns
							// do not collapse; a unique leaf keys bare.
							proj.ProjectedValues[i] = bv
							if bareLeafDuplicated(sq.projCols, sq.projAliases, i) {
								mintQualifiedDatumKey(proj, i, col)
							}
						} else {
							// Born-baked (slice 3; the dup-alias flat-name
							// carve-out is RETIRED — a duplicated qualifier
							// bakes QOV(binding) per-attribute above, first
							// leg included, since only later duplicates were
							// renamed and QOV(alias) addresses exactly one
							// leg; ambiguous dup reads die 42702 upstream):
							// a validated qualified projection that cannot
							// bake a leg-window ordinal must fail the plan,
							// never mint a lazy name read.
							return nil, &expr.UnresolvableOrdinalError{Field: col.bare, Source: col.qualifier}
						}
					}
				} else {
					if rv, err := resolver.ResolveIdentifierPath(
						colRefIdentifiers(colBareOrName(col), col.qualifier, col.qualified, col.segs)); err == nil {
						if i < len(proj.ProjectedValues) {
							proj.ProjectedValues[i] = rv
						}
					}
				}
			}
		}
	}

	// (3) Validate ORDER BY columns (ambiguous/undefined, scalar subquery rejection).
	projAliasSet := make(map[string]bool)
	if sq.projAliases != nil {
		for _, a := range sq.projAliases {
			if a != "" {
				projAliasSet[strings.ToUpper(a)] = true
			}
		}
	}
	for _, ac := range sq.aggCols {
		if ac.outName != "" {
			projAliasSet[strings.ToUpper(ac.outName)] = true
		}
	}
	for _, ob := range sq.orderBy {
		if ob.rawExpr != nil {
			hasSubquery := false
			walkScalarSubqueries(ob.rawExpr, func(_ antlrgen.IQueryContext) {
				hasSubquery = true
			})
			if hasSubquery {
				return nil, api.NewError(api.ErrCodeUnsupportedSort,
					"ORDER BY with scalar subquery is not supported")
			}
			// RFC-141 R4: EXISTS in an ORDER BY key is NOT a
			// directly-handled position. The sort-key resolver carries no
			// SubqueryPlanner, so the EXISTS fails to resolve, the key keeps its
			// raw text form, and the existential is never evaluated → a silent
			// WRONG ORDERING (every row ties on a constant). Reject cleanly rather
			// than mis-order (mirrors the scalar-subquery rejection above).
			if expr.ContainsExistsAtom(ob.rawExpr) {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery,
					"EXISTS in an ORDER BY clause is not yet supported")
			}
		}
	}
	if resolver != nil {
		for _, ob := range sq.orderBy {
			if ob.rawExpr != nil {
				if _, walkErr := resolver.WalkExpression(ob.rawExpr); walkErr != nil {
					var ambigErr *semantic.AmbiguousColumnError
					if errors.As(walkErr, &ambigErr) {
						// A BARE key naming exactly ONE projection output
						// alias takes precedence over FROM-scope ambiguity —
						// the sort executes over the projected row, where
						// that alias key is unambiguous (the output-first
						// rule the ColumnNotFound arm below applies).
						// orderByOutputAliasBinding enforces both restrictions:
						// the raw key must BE a bare identifier, and the name
						// must bind exactly one output column; everything else
						// surfaces the scope's 42702.
						if bare, n := orderByOutputAliasBinding(ob.rawExpr, ob.colName, sq); bare && n == 1 {
							continue
						}
						// Java's exact SemanticAnalyzer text — the reference as
						// written, byte-equal in the conformance harness
						// (verified for duplicate AND distinct aliases).
						return nil, api.NewErrorf(api.ErrCodeAmbiguousColumn,
							"Ambiguous reference %s", ambigErr.Reference())
					}
					var srcNotFound *semantic.SourceNotFoundError
					if errors.As(walkErr, &srcNotFound) {
						return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
							"column reference with qualifier %q cannot be resolved", srcNotFound.Alias.Name())
					}
					var notFoundErr *semantic.ColumnNotFoundError
					if errors.As(walkErr, &notFoundErr) {
						if projAliasSet[strings.ToUpper(ob.colName)] {
							continue
						}
						if ob.bare != "" {
							if resolveColumnRefStructural(resolver, ob.bare, ob.qualifier, ob.qualified, ob.segs) == nil {
								continue
							}
						} else if ob.colName != "" && resolveColumnName(resolver, ob.colName) == nil {
							continue
						}
						return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
							"column %q does not exist", ob.colName)
					}
				}
			}
		}
	}

	// (4) Validate GROUP BY columns.
	if resolver != nil {
		for _, gb := range sq.groupBy {
			if gb.expr != nil {
				continue
			}
			if gb.bare != "" {
				if err := resolveColumnRefStructural(resolver, gb.bare, gb.qualifier, gb.qualified, gb.segs); err != nil {
					return nil, err
				}
			} else if err := resolveColumnName(resolver, gb.display); err != nil {
				return nil, err
			}
		}
	}

	// (5) Validate aggregate argument columns.
	if resolver != nil {
		for _, ac := range sq.aggCols {
			if ac.aggArg != "" && ac.aggExpr == nil {
				if ac.aggArgBare != "" {
					if err := resolveColumnRefStructural(resolver, ac.aggArgBare, ac.aggArgQualifier, ac.aggArgQualified, ac.aggArgSegs); err != nil {
						return nil, err
					}
				} else if err := resolveColumnName(resolver, ac.aggArg); err != nil {
					return nil, err
				}
			}
		}
	}

	// (5b) Validate SELECT-list group-column re-reads through the scope: a
	// BARE re-read that is ambiguous across sources (GROUP BY po.id, pi.id
	// re-read as `id`) is 42702 (Java AMBIGUOUS_COLUMN) — the aggregate
	// output-name table matches keys qualifier-stripped, so an unvalidated
	// bare re-read would silently bind ONE leg's key last-wins.
	// Expression-redirected entries (groupCol = the GROUP BY expression's
	// display) carry no column reference and are skipped.
	if resolver != nil {
		exprKeyDisplays := map[string]bool{}
		for _, gn := range sq.groupBy {
			if gn.expr != nil {
				exprKeyDisplays[gn.display] = true
			}
		}
		for _, ac := range sq.aggCols {
			if ac.groupCol == "" || ac.groupColBare == "" || exprKeyDisplays[ac.groupCol] {
				continue
			}
			if err := resolveColumnRefStructural(resolver, ac.groupColBare, ac.groupColQualifier, ac.groupColQualified, ac.groupColSegs); err != nil {
				return nil, err
			}
		}
	}

	// (6) Validate GROUP BY projection constraints (42803).
	if len(sq.groupBy) > 0 && !sq.countStar {
		if err := validateGroupByProjection(sq, v.md); err != nil {
			return nil, err
		}
	}

	// (7) Detect overflow numeric literals and correlated-subquery
	// rejections in projection expressions.
	if resolver != nil && len(sq.projExprs) > 0 {
		for _, e := range sq.projExprs {
			if e == nil {
				continue
			}
			if _, walkErr := resolver.WalkExpressionForProjection(e); walkErr != nil {
				var overflow *expr.NumericOverflowLiteralError
				if errors.As(walkErr, &overflow) {
					return nil, api.NewError(api.ErrCodeNumericValueOutOfRange, overflow.Error())
				}
				var binErr *expr.InvalidBinaryLiteralError
				if errors.As(walkErr, &binErr) {
					return nil, api.NewError(api.ErrCodeInvalidBinaryRepresentation, binErr.Error())
				}
				var corrErr *CorrelatedExistsError
				if errors.As(walkErr, &corrErr) {
					// Route through the SAME classifier the WHERE-EXISTS path uses so
					// both forms agree on the SQLSTATE: a GENUINE resolution failure in
					// the ON (missing column/source) unwraps to 42703/42702, while only a
					// DELIBERATE Unsupported decline (nested-subquery / OUTER-ON /
					// collision / RIGHT-FULL) reports 0A000. Without this the projected
					// path reported 0A000 for a genuine missing column, diverging from the
					// WHERE-EXISTS path's 42703.
					if mapped := mapPredicateWalkError(walkErr); mapped != nil {
						return nil, mapped
					}
					return nil, api.NewError(api.ErrCodeUnsupportedOperation, corrErr.Error())
				}
				// RFC-141 R4 (P1b): a SELECT item with a NESTED EXISTS is
				// not foldable; reject cleanly (never a silent constant-false / NULL).
				var nestedExists *expr.NestedExistsProjectionError
				if errors.As(walkErr, &nestedExists) {
					return nil, api.NewError(api.ErrCodeUnsupportedQuery, nestedExists.Error())
				}
			}
		}
	}

	// (8) Derived-table references resolve to the OUTPUT column name verbatim;
	// the projection already emits under that name — nothing to rewrite.

	// (9) Upgrade JOIN ON predicates.
	if len(sq.joins) > 0 {
		if err := upgradeJoinOnPredicates(op, sq, v.md, v.schemaName, queryCTEScopes, v.cteOnScopes, v.cteProducers); err != nil {
			return nil, err
		}
	}

	// (10) Upgrade aggregate operands + GROUP BY key values.
	if len(sq.aggCols) > 0 {
		if uerr := upgradeAggregateOperands(op, sq, v.md, v.schemaName, queryCTEScopes); uerr != nil {
			return nil, uerr
		}
	}

	// (11) Create a unified SubqueryPlanner for EXISTS/scalar subqueries.
	existsPlanner := &existsSubqueryPlanner{
		bindings:     v.bindings,
		md:           v.md,
		schemaName:   v.schemaName,
		outerScope:   resolverScope(resolver),
		outerScopes:  buildOuterScopeSources(sq, v.md, v.schemaName, queryCTEScopes),
		cteScopes:    v.cteScopes,
		cteOnScopes:  v.cteOnScopes,
		cteProducers: v.cteProducers,
	}

	// (12) Upgrade projection values.
	if len(sq.projExprs) > 0 || len(sq.postAggExprs) > 0 {
		if err := upgradeProjectionValues(op, sq, v.md, v.schemaName, queryCTEScopes, existsPlanner); err != nil {
			return nil, err
		}
	}
	// (13) Attach scalar subqueries from projections.
	if len(existsPlanner.scalarSubqueries) > 0 {
		if proj := findProjection(op); proj != nil {
			proj.ScalarSubqueries = existsPlanner.scalarSubqueries
		}
		existsPlanner.scalarSubqueries = nil
	}
	if len(existsPlanner.correlatedScalarSubqueries) > 0 {
		if proj := findProjection(op); proj != nil {
			proj.CorrelatedScalarSubqueries = existsPlanner.correlatedScalarSubqueries
		}
		existsPlanner.correlatedScalarSubqueries = nil
	}

	// (14) Upgrade HAVING predicate.
	if sq.havingExpr != nil {
		if herr := upgradeHavingPredicate(op, sq, v.md, v.schemaName, queryCTEScopes, existsPlanner); herr != nil {
			return nil, herr
		}
	}

	// (15) Upgrade sort key values.
	if err := upgradeSortKeyValues(op, sq, v.md, v.schemaName, queryCTEScopes); err != nil {
		return nil, err
	}

	// (15a) RFC-142 (P2a): a BARE ORDER BY sort key that binds to a
	// lateral-unnest SHADOWING source (`FROM t, t.arr AS v, …`) must sort by the
	// key QUALIFIED to the unnest correlation (`v.v`), exactly as the bare
	// PROJECTION column is qualified at step (2) above. The unnest element flows
	// the merged row under both bare `v` and qualified `v.v`, but a LATER FROM item
	// with its own `v` overwrites the bare sort key last-leg-wins in mergeRows; the
	// qualified `v.v` survives (dotted keys preserved verbatim). Without this the
	// SORT reads the wrong column (the projection reads `v.v`, the sort reads the
	// clobbered bare key) → rows in the WRONG ORDER (silent-wrong). Reuses the same
	// scope resolver and ResolveColumnShadowingQualified helper as the projection
	// path, so the two never diverge. RFC-142.
	if resolver != nil {
		if qerr := qualifyShadowedSortKeys(op, resolver); qerr != nil {
			return nil, qerr
		}
	}

	// (15b) RFC-141 Phase 2: register projected-EXISTS subqueries so the
	// translator attaches the existential quantifier and builds the FlatMap
	// even with no WHERE clause. upgradeProjectionValues already ran BuildExists
	// for projected EXISTS (populating existsPlanner.subqueries); synthesize a
	// filter to hold them. The existential boolean is computed by the
	// projection's ExistsValue inside the SelectExpression result value.
	if sq.whereExpr == nil && len(existsPlanner.subqueries) > 0 && projectionHasExistsValue(op) {
		op = attachOrSynthesizeExistsFilter(op, existsPlanner.subqueries)
		existsPlanner.subqueries = nil
	}

	// (16) Upgrade WHERE predicate.
	if sq.whereExpr == nil {
		// No WHERE, but a QUALIFY filter (vector K-NN ROW_NUMBER() <= K) must
		// still be attached — synthesize a filter above the scan if none exists.
		qualPred, qErr := buildQualifyPredicate(v.md, v.schemaName, sq, queryCTEScopes)
		if qErr != nil {
			return nil, qErr
		}
		if qualPred != nil {
			op = attachOrSynthesizeFilter(op, qualPred)
			op = wrapGlobalRankVectorLimit(op, qualPred)
		}
		return op, nil
	}

	// RFC-141 R4: an EXISTS atom in the WHERE clause is directly-handled
	// only when it is a top-level boolean term (the whole WHERE, an AND conjunct,
	// or a single-NOT). An EXISTS nested inside a SCALAR expression — `WHERE CASE
	// WHEN EXISTS(...) THEN 1 ELSE 0 END = 1`, `WHERE (EXISTS(...)) = true`,
	// `WHERE f(EXISTS(...))` — is lowered into a scalar Value (a CASE / comparison
	// operand) with no existential quantifier driving it, so it evaluates to a
	// constant false → a silent wrong result (every row dropped). Detect such a
	// buried EXISTS structurally on the parse tree (the WHERE companion to the
	// projected nested-EXISTS guard) and reject cleanly. (A top-level EXISTS under
	// an OR is separately rejected below.)
	if expr.WhereExistsInScalarPosition(sq.whereExpr.Expression()) {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"EXISTS nested in a scalar expression is not yet supported")
	}

	var preWalkPred predicates.QueryPredicate
	if resolver != nil && sq.whereExpr.Expression() != nil {
		walked, walkErr := walkSubqueryPredicate(resolver, existsPlanner, sq.whereExpr.Expression())
		if walkErr != nil {
			// Classify the walk failure through the SHARED mapper (the same one the
			// projected-EXISTS path and every mapPredicateWalkError caller use) so
			// every path agrees on the SQLSTATE: a genuine resolution error
			// (Ambiguous/ColumnNotFound/SourceNotFound/Shadow/…) → 42703/42702, a
			// deliberate unsupported-shape decline → 0A000, and a structured
			// *api.Error (a nested subquery build's own already-classified rejection,
			// e.g. RFC-141 R4's ErrCodeUnsupportedQuery) verbatim rather than the
			// generic "could not plan" of the text-fallback below. A single ladder
			// so a new arm can never reach one EXISTS path but not the other.
			if mapped := mapPredicateWalkError(walkErr); mapped != nil {
				return nil, mapped
			}
		} else {
			preWalkPred = walked
		}
	}

	hasSubqueries := len(existsPlanner.subqueries) > 0 ||
		len(existsPlanner.scalarSubqueries) > 0 ||
		len(existsPlanner.correlatedScalarSubqueries) > 0
	if hasSubqueries && preWalkPred != nil {
		pred := predicates.SimplifyPredicateValues(preWalkPred)
		// EXISTS is lowered to a conjunctive semi-join; under an OR that loses
		// the disjunction and silently returns empty. Reject rather than
		// return wrong rows (RFC-082; inline-EXISTS-under-OR is future work).
		if existsUnderDisjunction(pred) {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation,
				"EXISTS within an OR (disjunction) is not supported")
		}
		combined, qErr := combineQualifyPred(v.md, v.schemaName, sq, queryCTEScopes, pred)
		if qErr != nil {
			return nil, qErr
		}
		if installErr := installFirstWherePredicate(op, combined); installErr != nil {
			return nil, installErr
		}
		if len(existsPlanner.subqueries) > 0 {
			if !upgradeFirstFilterExistsSubqueries(op, existsPlanner.subqueries) {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery,
					"WHERE subqueries could not be installed on the logical plan")
			}
		}
		if len(existsPlanner.scalarSubqueries) > 0 {
			if !upgradeFirstFilterScalarSubqueries(op, existsPlanner.scalarSubqueries) {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery,
					"WHERE scalar subqueries could not be installed on the logical plan")
			}
		}
		if len(existsPlanner.correlatedScalarSubqueries) > 0 {
			if !upgradeFirstFilterCorrelatedScalarSubqueries(op, existsPlanner.correlatedScalarSubqueries) {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery,
					"WHERE correlated scalar subqueries could not be installed on the logical plan")
			}
		}
		op = wrapGlobalRankVectorLimit(op, combined)
		return op, nil
	}

	if preWalkPred != nil {
		pred := predicates.SimplifyPredicateValues(preWalkPred)
		combined, qErr := combineQualifyPred(v.md, v.schemaName, sq, queryCTEScopes, pred)
		if qErr != nil {
			return nil, qErr
		}
		if installErr := installFirstWherePredicate(op, combined); installErr != nil {
			return nil, installErr
		}
		op = wrapGlobalRankVectorLimit(op, combined)
		return op, nil
	}

	var pred predicates.QueryPredicate
	var predOk bool
	if queryCTEScopes != nil && len(sq.joins) == 0 {
		if src, found := queryCTEScopes[strings.ToUpper(sq.tableName)]; found && src.Table != nil {
			// A TOMBSTONE entry (nil Table: declared CTE, schema underivable)
			// must not reach scope construction — ResolveColumn nil-derefs.
			pred, predOk = buildWherePredicateFromCTEScope(src, sq.tableAlias, sq.whereExpr, v.md)
		}
	}
	if !predOk && queryCTEScopes != nil && len(sq.joins) > 0 {
		pred, predOk = buildWherePredicateForJoinsWithCTEScopes(v.md, v.schemaName, sq, sq.whereExpr, queryCTEScopes)
	}
	if !predOk {
		pred, predOk = buildWherePredicate(v.md, v.schemaName, sq, sq.whereExpr)
	}
	if !predOk {
		// Keep the canonical text filter created by visitWhere. The Cascades
		// translator deliberately declines text-only filters, so this remains
		// fail closed while downstream table/CTE validation can report a more
		// specific SQLSTATE for an unresolved source.
		return op, nil
	}
	if installErr := installFirstWherePredicate(op, pred); installErr != nil {
		return nil, installErr
	}
	return op, nil
}

// assignDerivedSourceBindings finalizes derived identities before any scope or
// logical source is built. SQL aliases remain lexical names; nested derived
// sources cannot share runtime correlations with any enclosing frame. A local
// deterministic mint avoids exposing global planning history in carrier labels.
func (v *PlanVisitor) assignDerivedSourceBindings(fs *fromSource) {
	if fs == nil {
		return
	}
	if v.bindings == nil {
		v.bindings = &bindingAllocator{}
	}
	if fs.bindings == v.bindings {
		return
	}
	fs.bindings = v.bindings
	reserve := v.bindings.reserve
	for frame := v.enclosingScope; frame != nil; frame = frame.Parent() {
		for _, source := range frame.Sources() {
			reserve(source.Alias.Name())
			reserve(source.CorrelationName)
			for _, name := range source.AdditionalQualifiers {
				reserve(name.Name())
			}
		}
	}
	for name := range v.cteScopes {
		reserve(name)
	}
	for name := range v.cteOnScopes {
		reserve(name)
	}
	for _, name := range v.cteProducers.Names() {
		reserve(name)
	}
	reserve(fs.tableName)
	reserve(fs.tableAlias)
	reserve(fs.bindingID)
	for _, segment := range fs.sourceSegments {
		reserve(segment)
	}
	for _, j := range fs.joins {
		reserve(j.tableName)
		reserve(j.alias)
		reserve(j.bindingID)
		for _, segment := range j.segments {
			reserve(segment)
		}
	}
	if v.enclosingScope == nil {
		return
	}
	fs.bindingID = strings.ToUpper(v.bindings.mint().Name())
	for i := range fs.joins {
		fs.joins[i].bindingID = strings.ToUpper(v.bindings.mint().Name())
	}
}

// prepareDerivedSourceBodies builds each derived query once, before even the
// GROUP BY star classifier needs its schema. Later carrier and scope rebuilds
// share this exact body rather than independently resolving the same parse tree.
func (v *PlanVisitor) prepareDerivedSourceBodies(fs *fromSource) error {
	build := func(inner antlrgen.IQueryContext, body *logical.LogicalOperator) error {
		if inner == nil || *body != nil {
			return nil
		}
		var err error
		*body, err = v.buildCTEBodyQuery(inner)
		if err != nil {
			return err
		}
		if *body == nil {
			return api.NewError(api.ErrCodeUnsupportedQuery, "derived source has no logical body")
		}
		return nil
	}
	if err := build(fs.derivedQuery, &fs.catalogAwareInnerPlan); err != nil {
		return err
	}
	for i := range fs.joins {
		if err := build(fs.joins[i].derivedQuery, &fs.joins[i].catalogAwareInnerPlan); err != nil {
			return err
		}
	}
	return nil
}

// visitFrom builds the FROM-source subtree from the pre-parsed
// fromSource. Derived queries retain their complete, catalog-aware bodies and
// lexical CTE environment, including the body's own WITH clause.
//
// Pre-built derived table inner plans are written back to
// fs.joins[i].catalogAwareInnerPlan so that the selectQuery bridge
// carries them into the upgrade functions.
func (v *PlanVisitor) visitFrom(simpleTable *antlrgen.SimpleTableContext, fs *fromSource) (logical.LogicalOperator, error) {
	if err := v.prepareDerivedSourceBodies(fs); err != nil {
		return nil, err
	}
	var op logical.LogicalOperator
	if fs.inlineValues != nil {
		var err error
		op, err = buildInlineValuesLogical(fs.inlineValues, fs.tableAlias, fs.bindingID, v.md)
		if err != nil {
			return nil, err
		}
	} else if fs.derivedQuery != nil {
		innerOp := fs.catalogAwareInnerPlan
		// The CTE wrapper is the logical tree's ONLY carrier of a derived
		// table's alias — wrap the no-joins case too. Bare innerOp loses the
		// alias: sourceAlias() then walks through to the BASE table, a
		// correlated EXISTS on the derived alias (`FROM (SELECT …) e WHERE
		// EXISTS(… = e.id)`) binds the outer row under the WRONG name, and
		// the correlation reads NULL (silently wrong on a column-name
		// collision, loud OrdinalResolutionError without one). The
		// qualified-star rebuild (needRebuild → buildLogicalPlanForSelect)
		// DOES fire for derived-no-joins and re-enters the plain builder —
		// every derived arm (this one, the plain builder, the catalog
		// rebuild's buildOuterPlanOnDerived) must carry the same wrapper.
		// Recorded for the same reason the join legs below are: a rebuild
		// (qualified or USING-hidden star expansion) re-enters
		// buildLogicalPlanForSelect, and without this the PRIMARY source alone
		// would be rebuilt by the text-only builder — its body's projections
		// then carry no resolved Values and the translator refuses the whole
		// query with "projection slot 0 has no resolved Value".
		op = derivedSourceCarrier(fs.tableName, fs.bindingID, innerOp)
	} else {
		scan := logical.NewScan(fs.tableName, fs.tableAlias, fs.sourceSegments...)
		scan.Source = fs.resolvedSource
		logical.BindCTESources(scan, v.cteProducers)
		fs.resolvedSource = scan.Source
		scan.Binding = fs.bindingID
		op = scan
	}

	// JOINs chain left-to-right from the primary scan. Each join wraps
	// the current op as Left and scans the joined table as Right.
	resolvesToTable := newUnnestTableResolver(v.md, v.schemaName)
	for i, j := range fs.joins {
		var right logical.LogicalOperator
		if j.inlineValues != nil {
			var err error
			right, err = buildInlineValuesLogical(j.inlineValues, j.alias, j.bindingID, v.md)
			if err != nil {
				return nil, err
			}
		} else if j.catalogAwareInnerPlan != nil {
			// Use the pre-built inner plan from the visitor.
			if j.alias != "" {
				right = derivedSourceCarrier(j.alias, j.bindingID, j.catalogAwareInnerPlan)
			} else {
				right = j.catalogAwareInnerPlan
			}
		} else if u := lateralUnnestCandidate(j, visibleFromAliases(fs.tableName, fs.tableAlias, fs.joins[:i], resolvesToTable), resolvesToTable); u != nil {
			// A comma source that may be a lateral array unnest
			// (`FROM t, t.arr AS x [AT ord]`). The translator classifies it
			// against the scope (segment 0 = an in-scope source with an array
			// field named by the rest → unnest, else a table) — the parser
			// preserved the uid segments + AT alias for exactly this. RFC-142.
			right = u
		} else {
			sc := logical.NewScan(j.tableName, j.alias, j.segments...)
			sc.Source = j.resolvedSource
			logical.BindCTESources(sc, v.cteProducers)
			fs.joins[i].resolvedSource = sc.Source
			sc.Binding = j.bindingID
			right = sc
		}
		var kind logical.JoinKind
		switch j.joinType {
		case joinTypeLeft:
			kind = logical.JoinLeft
		case joinTypeRight:
			kind = logical.JoinRight
		case joinTypeFull:
			kind = logical.JoinFull
		default:
			kind = logical.JoinInner
		}
		onText := ""
		if j.onExpr != nil {
			onText = canonicalTextOf(j.onExpr)
		}
		op = logical.NewJoin(op, right, kind, onText)
	}

	return op, nil
}

// visitWhere wraps the current operator with a LogicalFilter when the
// FROM clause contains a WHERE expression. Reads the WHERE directly from
// the ANTLR parse tree rather than from selectQuery.whereExpr.
func (v *PlanVisitor) visitWhere(op logical.LogicalOperator, simpleTable *antlrgen.SimpleTableContext) logical.LogicalOperator {
	fromClause := simpleTable.FromClause()
	if fromClause == nil {
		return op
	}
	whereExpr := fromClause.WhereExpr()
	if whereExpr == nil {
		return op
	}
	return logical.NewFilter(op, canonicalTextOf(whereExpr))
}

// visitSelectGroupBy builds the aggregate/GROUP BY/HAVING shell around
// the current operator using the selectClassification from
// classifySelectElements.
//
// Returns the wrapped operator and a stripPrefix (non-empty for derived
// table queries where column names need prefix stripping).
func (v *PlanVisitor) visitSelectGroupBy(op logical.LogicalOperator, cls *selectClassification, fs *fromSource) (logical.LogicalOperator, string, error) {
	if cls == nil {
		return op, "", nil
	}

	// Determine strip prefix for derived tables and table aliases.
	stripPrefix := ""
	if fs != nil && fs.derivedQuery != nil {
		stripPrefix = strings.ToUpper(fs.tableName) + "."
	}
	aliasPrefix := ""
	if fs != nil && fs.tableAlias != "" && len(fs.joins) == 0 {
		aliasPrefix = strings.ToUpper(fs.tableAlias) + "."
	}

	strip := func(s string) string {
		upper := strings.ToUpper(s)
		if stripPrefix != "" && strings.HasPrefix(upper, stripPrefix) {
			return s[len(stripPrefix):]
		}
		if aliasPrefix != "" && strings.HasPrefix(upper, aliasPrefix) {
			return s[len(aliasPrefix):]
		}
		return s
	}

	// Three aggregate shapes collapse here:
	//   - Bare COUNT(*): no group keys, single COUNT(*) aggregate.
	//   - GROUP BY without aggregates: just the group keys.
	//   - Mixed: aggCols carries both group-col and agg-function entries.
	if !cls.countStar && len(cls.aggCols) == 0 && len(cls.groupBy) == 0 {
		return op, stripPrefix, nil
	}

	keys := logicalGroupKeys(cls.groupBy)
	for i := range keys {
		stripped := strip(keys[i].Display)
		if stripped != keys[i].Display {
			keys[i] = stripGroupKeyLeadingSegment(keys[i], stripped)
		}
	}
	// DUPLICATE GROUPING EXPRESSIONS reject 42702 (Java: the grouping
	// value is pulled up over the GroupByExpression's result when the
	// output is rebound — LogicalOperator.generateGroupBy,
	// LogicalOperator.java:454 — and a grouping value containing the same
	// expression twice maps it to TWO output columns, failing
	// Expressions.pullUp's one-column assertion with AMBIGUOUS_COLUMN,
	// Expressions.java:112). The identity is the RESOLVED VALUE, which is
	// why `GROUP BY category, T_G1.category` rejects too — the strip()
	// above has already folded a single-source qualifier away, so column
	// keys compare by the same post-strip identity the slot-matching loop
	// uses (buildAggregateOutputSlots). EXPRESSION keys resolve through
	// the semantic scope and compare by VALUE equality — key.Display is
	// the RAW SOURCE SLICE (canonicalTextOf keeps original whitespace),
	// so a text comparison let `GROUP BY amount+1, amount + 1` through
	// while catching only the byte-identical spelling (measured live:
	// Java 42702s both). When no resolver exists or a walk declines, the
	// fallback identity is the token-concatenated GetText — token-stream
	// equality, whitespace-invariant. Measured live
	// (DuplicateGroupByJavaProbe): Java 42702s every duplicate shape
	// BEFORE planning; the check therefore sits at aggregate BUILD time,
	// not in the planner.
	exprKeyVals := make([]values.Value, len(cls.groupBy))
	if exprKeyCount := func() (n int) {
		for _, k := range cls.groupBy {
			if k.bare == "" && k.expr != nil {
				n++
			}
		}
		return n
	}(); exprKeyCount > 1 {
		if resolver := buildSelectScope(selectQueryFromClassification(cls, fs), v.md, v.schemaName, v.cteScopes); resolver != nil {
			for i, k := range cls.groupBy {
				if k.bare != "" || k.expr == nil {
					continue
				}
				if val, walkErr := resolver.WalkExpression(k.expr); walkErr == nil {
					exprKeyVals[i] = val
				}
			}
		}
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			same := false
			switch {
			case exprKeyVals[i] != nil && exprKeyVals[j] != nil:
				// Both walks resolved in the SAME scope, so leaf
				// correlations already agree — the identity alias map.
				same = values.SemanticEqualsUnderAliasMap(exprKeyVals[i], exprKeyVals[j], nil)
			case i < len(cls.groupBy) && j < len(cls.groupBy) &&
				cls.groupBy[i].bare == "" && cls.groupBy[i].expr != nil &&
				cls.groupBy[j].bare == "" && cls.groupBy[j].expr != nil:
				// Resolver unavailable/declined: token-stream identity
				// (GetText concatenates tokens, dropping whitespace).
				same = strings.EqualFold(cls.groupBy[i].expr.GetText(), cls.groupBy[j].expr.GetText())
			default:
				same = groupKeysEquivalent(keys[i], keys[j])
			}
			if same {
				return nil, "", api.NewErrorf(api.ErrCodeAmbiguousColumn,
					"Ambiguous columns for %s", keys[j].Display)
			}
		}
	}
	aggCalls, aggProvenance, hasDistinct := logicalAggregateCalls(cls.aggCols, cls.countStar, strip)
	outputAggCols := visibleAggregateOutputColumns(cls.aggCols, cls.countStar, cls.countStarAlias)
	// Every aggregate's internal ABI is canonical and alias-free:
	// [group keys..., aggregate calls...]. SQL aliases belong exclusively to
	// the final visible Project. Besides preventing key/alias collisions, this
	// keeps memo identity honest because AggregateSpec aliases are not part of
	// GroupByExpression equality/hash.
	aggAliases := make([]string, len(aggCalls))
	aggOp := logical.NewAggregate(op, keys, aggCalls, aggAliases, cls.havingExpr != nil)
	aggOp.CallProvenance = aggProvenance
	aggOp.HasCallProvenance = true
	aggOp.CallProvenanceCols = len(cls.aggCols)
	aggOp.HasDistinctAggregate = hasDistinct
	aggOp.OutputSlots = buildAggregateOutputSlots(keys, outputAggCols, strip)
	op = aggOp

	// Every SQL aggregate has one public output boundary. It is deliberately
	// deferred until after ORDER BY so hidden sort/HAVING accumulators remain
	// available on the canonical [keys..., calls...] row.
	if proj, antlr := buildPostAggregateProjection(op, outputAggCols, strip); proj != nil {
		cls.postSortStripProj = append([]string(nil), proj.Projections...)
		cls.postSortStripAliases = append([]string(nil), proj.Aliases...)
		cls.postSortAggregateOutputOrdinals = append([]int(nil), proj.AggregateOutputOrdinals...)
		cls.postSortIsComputed = append([]bool(nil), proj.IsComputed...)
		cls.postSortSQLNames = append([]string(nil), proj.SQLNames...)
		cls.postAggExprs = antlr
	}

	return op, stripPrefix, nil
}

// groupKeysEquivalent reports whether two COLUMN group keys are the same
// grouping column under the identity buildAggregateOutputSlots matches
// with: (qualified, qualifier, bare) case-insensitively. Two equivalent
// keys duplicate a grouping value column, which Java rejects 42702
// (Expressions.pullUp's ambiguity assertion). A bare key and a
// differently-QUALIFIED key of another source are NOT equivalent — Java's
// value identity keeps `a.k, b.k` legal. EXPRESSION keys never decide
// here: their identity is the resolved VALUE (or the token stream when no
// resolver exists) at the caller — Display is the raw source slice, and
// comparing it treats `amount+1` and `amount + 1` as different
// expressions. An expression pair that somehow reaches this comparison
// fails OPEN (no 42702) rather than guessing from text.
func groupKeysEquivalent(a, b logical.GroupKey) bool {
	if a.Bare != "" && b.Bare != "" {
		return a.Qualified == b.Qualified &&
			strings.EqualFold(a.Bare, b.Bare) &&
			(!a.Qualified || strings.EqualFold(a.Qualifier, b.Qualifier))
	}
	return false
}

// buildCTEBodyQuery builds a CTE body plan, PRESERVING the body's own WITH
// clause. A body that is itself `WITH inner AS (…) SELECT … FROM inner` must
// build as a FULL query: its nested CTEs register and wrap (LogicalCTE) with
// lexical shadowing — the inner name wins INSIDE the body, the enclosing
// query's same-named CTE stays bound outside it. Before this helper both
// body-build sites called VisitQueryBody(inner.QueryExpressionBody()),
// silently DROPPING the body's ctes: the nested WITH vanished, its filter
// with it, and `FROM c1` inside c2's body resolved to the ENCLOSING c1 —
// wrong rows (yamsql cte_error_codes: 9 rows for 6; Java's cte.yamsql pins
// the shadowing). The visitor's CTE maps are snapshotted and restored so the
// nested registrations stay scoped to the body build.
func (v *PlanVisitor) buildCTEBodyQuery(inner antlrgen.IQueryContext) (logical.LogicalOperator, error) {
	previousScopes, previousOn, previousRegistry := v.cteScopes, v.cteOnScopes, v.cteProducers
	v.cteScopes, v.cteOnScopes = maps.Clone(previousScopes), maps.Clone(previousOn)
	defer func() { v.cteScopes, v.cteOnScopes, v.cteProducers = previousScopes, previousOn, previousRegistry }()
	if inner.Ctes() == nil {
		body, err := v.VisitQueryBody(inner.QueryExpressionBody())
		if err == nil {
			logical.BindCTESources(body, v.cteProducers)
		}
		return body, err
	}
	return v.VisitQuery(inner)
}

// visitOrderBy builds the LogicalSort operator by reading ORDER BY
// expressions directly from the ANTLR parse tree. Handles positional
// references (ORDER BY 1), plain column names, aggregate function
// references, expression ORDER BY, direction (ASC/DESC), NULLS
// FIRST/LAST, and duplicate detection.
//
// selectCols/selectAliases are the pre-aggregate-classification
// column names from the SELECT list, used for positional reference
// resolution. aggCols is the aggregate classification from
// classifySelectElements, used as a fallback when the SELECT list
// was reclassified (projCols nil, aggCols non-nil).
func (v *PlanVisitor) visitOrderBy(op logical.LogicalOperator, simpleTable *antlrgen.SimpleTableContext, selectCols, selectAliases []string, aggCols []aggSelectCol, stripPrefix string, groupBy []string, groupByAliases map[string]int, deferredStripProj, deferredStripAliases []string, deferredOutputOrdinals []int) logical.LogicalOperator {
	orderByCtx := simpleTable.OrderByClause()
	if orderByCtx == nil {
		return op
	}

	strip := func(s string) string {
		if stripPrefix != "" && strings.HasPrefix(strings.ToUpper(s), stripPrefix) {
			return s[len(stripPrefix):]
		}
		return s
	}

	// resolveGroupByAlias rewrites a GROUP BY alias to the underlying
	// column name. Returns the resolved name and true when the alias
	// matched; otherwise returns the original name and false.
	resolveGroupByAlias := func(name string) (string, bool) {
		if groupByAliases == nil {
			return name, false
		}
		idx, ok := groupByAliases[strings.ToUpper(name)]
		if !ok || idx >= len(groupBy) {
			return name, false
		}
		return groupBy[idx], true
	}

	// rebaseToInternal rewrites a sort key for the DEFERRED-strip case: the
	// sort sits BELOW the reshaping projection, over the aggregate's
	// internal layout, so a key naming a SELECT alias (which exists only
	// ABOVE the projection) is rebased to its underlying expression. The
	// alias is checked FIRST — SQL resolves ORDER BY names against output
	// columns before source columns, so `SELECT id AS v … GROUP BY id, v
	// ORDER BY v` sorts by id (the alias), not the hidden group key v.
	rebaseToInternal := func(name string, bareRef bool) string {
		// Output aliases bind BARE one-segment identifiers only: a
		// qualified key (`d.x`) or an aggregate/computed key names source
		// data, never the SELECT alias. The parse tree decides (bareRef),
		// not the name text — delimited aliases can spell "x.y" or
		// "SUM(S)".
		if !bareRef {
			return name
		}
		for i, al := range deferredStripAliases {
			if al != "" && strings.EqualFold(al, name) && i < len(deferredStripProj) {
				return deferredStripProj[i]
			}
		}
		return name
	}

	obExprs := orderByCtx.AllOrderByExpression()
	if len(obExprs) == 0 {
		return op
	}

	// Java errors 42701 (COLUMN_ALREADY_EXISTS) on `ORDER BY b, b`
	// with the same column repeated. Stricter than Postgres, but
	// matching Java's behavior for 100% alignment.
	seenOrderCols := make(map[string]bool)
	keys := make([]logical.SortKey, 0, len(obExprs))

	for _, obExpr := range obExprs {
		ascending := true
		var nullsFirst *bool
		if oc := obExpr.OrderClause(); oc != nil {
			if oc.DESC() != nil {
				ascending = false
			}
			if oc.NULLS() != nil {
				f := oc.FIRST() != nil
				nullsFirst = &f
			}
		}

		dir := logical.SortAsc
		if !ascending {
			dir = logical.SortDesc
		}
		nf := ascending // default: ASC → NULLS FIRST, DESC → NULLS LAST
		if nullsFirst != nil {
			nf = *nullsFirst
		}

		// Handle positional references `ORDER BY N`.
		posName, pos, isPos, posErr := resolveSelectListPosition("ORDER BY", obExpr.Expression(), selectCols, selectAliases, aggCols, false)
		if posErr != nil {
			// Error during positional resolution — this was already
			// validated by classifySelectElements, so this shouldn't
			// happen. Build what we have so far.
			break
		}
		if isPos {
			key := strings.ToUpper(posName)
			if seenOrderCols[key] {
				// Duplicate — classifySelectElements already errors on
				// this, so we'll never reach here in practice. Skip to
				// match the validated behavior.
				continue
			}
			seenOrderCols[key] = true
			// Pos carries the SELECT-list position: a positional key IS an
			// output ordinal by SQL definition, so the translator bakes it
			// directly to the projection's output slot. Under a DEFERRED
			// strip the sort input is the aggregate's INTERNAL layout whose
			// slots differ from the visible ones — bake the underlying
			// expression text instead and drop the positional binding.
			if len(deferredStripProj) > 0 && pos >= 1 && pos <= len(deferredStripProj) {
				sk := logical.SortKey{Expr: deferredStripProj[pos-1], Dir: dir, NullsFirst: nf, Pos: pos}
				if pos <= len(deferredOutputOrdinals) && deferredOutputOrdinals[pos-1] >= 0 {
					sk.AggregateOutputOrdinal = deferredOutputOrdinals[pos-1]
					sk.HasAggregateOutputOrdinal = true
				}
				keys = append(keys, sk)
				continue
			}
			// The positional name is ALIAS-preferred but this sort sits
			// BELOW the final projection, where the alias does not exist
			// and may collide with a same-named SOURCE column. Rebase to
			// the item's UNDERLYING text (selectCols) as the no-catalog
			// fallback. Pos is pure INFORMATION (the ordinal into THIS
			// select's list): upgradeSortKeyValues resolves it into the
			// OUTER projection's typed item Value (clearing Pos), and the
			// translator bakes a surviving Pos only into a select-list-
			// carrying input (aggregate reshaping projection or union).
			expr := strip(posName)
			if pos >= 1 && pos <= len(selectCols) && selectCols[pos-1] != "" {
				expr = strip(selectCols[pos-1])
			}
			keys = append(keys, logical.SortKey{Expr: expr, Dir: dir, NullsFirst: nf, Pos: pos})
			continue
		}

		// Prefer plain column / aggregate lookup.
		colName, nameErr := columnNameFromExpr(obExpr.Expression(), "ORDER BY expression")
		if nameErr == nil {
			bareRef := exprIsBareColumnRef(obExpr.Expression())
			kb, kq, kqf, ksegs := splitColumnRef(obExpr.Expression())
			origColName := colName
			if len(deferredStripProj) > 0 {
				colName = rebaseToInternal(colName, bareRef)
			}
			// Resolve GROUP BY alias (`ORDER BY z` where `GROUP BY
			// x.col1 AS z`) to the underlying column before building
			// the sort key, so the Cascades planner sees a field that
			// actually exists in the aggregate output schema.
			if resolved, ok := resolveGroupByAlias(colName); ok {
				colName = resolved
			}
			key := strings.ToUpper(colName)
			if seenOrderCols[key] {
				continue
			}
			seenOrderCols[key] = true
			sk := logical.SortKey{Expr: strip(colName), Dir: dir, NullsFirst: nf, BareRef: bareRef, Bare: kb, Qualifier: kq, Qualified: kqf, Segs: ksegs}
			if kb != "" && (colName != origColName || sk.Expr != colName) {
				// A COLUMN key rebased/alias-resolved to an internal OUTPUT
				// name, or with its prefix stripped — BARE from here on
				// (the group-key strip rule); the parse-tree segments
				// describe the original reference, not this name.
				// Expression keys keep zero segments.
				sk.Bare, sk.Qualifier, sk.Qualified = sk.Expr, "", false
			}
			keys = append(keys, sk)
		} else {
			// Expression ORDER BY — use canonical text to get
			// proper spacing (GetText concatenates without whitespace).
			keys = append(keys, logical.SortKey{Expr: canonicalTextOf(obExpr.Expression()), Dir: dir, NullsFirst: nf})
		}
	}

	if len(keys) == 0 {
		return op
	}
	return logical.NewSort(op, keys)
}

// qualifyShadowedSortKeys redirects a BARE ORDER BY sort key that binds to a
// lateral-unnest SHADOWING scope source to the key QUALIFIED to that source's
// correlation (`FieldValue(QOV(v), v)`), the SORT-key analog of the bare-column
// PROJECTION qualification in buildSelectShell step (2). Without it, a bare sort
// key over `FROM t, t.arr AS v, u` (where a LATER FROM item `u` also has a column
// `v`) sorts by the merged row's BARE `v` key — which mergeRows overwrites
// last-leg-wins with `u.v` — instead of the unnest element under the protected
// qualified `v.v` key. The projection reads `v.v` (P2) but the sort read
// the clobbered bare key, so the rows came back in the WRONG ORDER (P2a,
// silent-wrong). Only a key whose Value is still UNSET (a bare column, not an
// alias/computed/raw-expr key already resolved by upgradeSortKeyValues) and that
// resolves to a Shadowing source is rewritten; everything else is untouched, so
// an explicitly-qualified `u.v` sort key and non-unnest queries are unaffected.
// Reuses ResolveColumnShadowingQualified — the same helper the projection path
// uses — so the two cannot diverge. RFC-142.
//
// PRE- vs POST-aggregate distinction (P2b). For a GROUPED /
// aggregate query (`SELECT V, COUNT(*) … GROUP BY V ORDER BY V DESC`) the sort
// sits ABOVE the aggregate, so the group-key sort key must read the aggregate's
// EXPOSED group-key column (the bare name `V`), NOT the FROM-scope-qualified
// `V.V`. That post-aggregate resolution is handled UPSTREAM in
// upgradeSortKeyValues (step 15): when the group key resolves to a lateral-unnest
// FieldValue it sets the sort key's Value to the aggregate OUTPUT column name
// (aggregateGroupKeyOutputName → the bare field), so the key arrives here already
// carrying a Value and is skipped by the `Value != nil` guard below. This
// function therefore only ever qualifies a PRE-aggregate (non-grouped) bare
// ORDER BY over an unnest — the shadowing case where the sort sits
// BELOW the merge and a later FROM item could clobber the bare key. RFC-142.
// A QUALIFIED sort key has the dup-alias twin of the same silent-wrong-order
// hazard: the sort sits BELOW the projection over the JOIN row, whose
// namespace carries the BINDING correlation (`Q$DUP1.QID`) for a later
// duplicate-alias leg — a key left as the SQL alias (`A.QID`)
// silently misses and the rows come back in scan order (the projection reads
// the binding, the sort read the display alias). Route qualified keys through
// ResolveQualifiedProjection — the SAME helper the projection path uses, so
// the two cannot diverge: it returns a value ONLY when the reference binds a
// later duplicate leg (binding != alias); every other qualified key (distinct
// aliases, first-occurrence legs) is untouched.
// bareLeafDuplicated reports whether the BARE leaf of projection column i
// collides (case-insensitive) with another projection column's EFFECTIVE
// output label (its alias when aliased, else its bare leaf) — the shape whose
// OUTPUT must stay QUALIFIED (`SELECT a.k, b.k` → columns A.K/B.K;
// `SELECT t1.id AS id, t2.id` → the second stays T2.ID, colliding with the
// alias), the name model's disambiguation rule that keeps two same-named leg
// columns from collapsing in the datum map. A UNIQUE leaf keys bare.
func bareLeafDuplicated(projCols []projCol, projAliases []string, i int) bool {
	if i >= len(projCols) {
		return false
	}
	// Structured bare segments — a delimited identifier containing a literal
	// dot is one leaf, never a last-dot split of the rendering.
	leaf := colBareOrName(projCols[i])
	for j, c := range projCols {
		if j == i {
			continue
		}
		other := colBareOrName(c)
		if j < len(projAliases) && projAliases[j] != "" {
			other = projAliases[j]
		}
		if strings.EqualFold(other, leaf) {
			return true
		}
	}
	return false
}

// mintQualifiedDatumKey pins projection slot i's QUALIFIED spelling as its
// output alias so a duplicated bare leaf does not collapse two legs' columns
// into one entry of the executor's name-keyed row map, and RECORDS that the
// machinery — not the user's `AS` — wrote that name.
//
// The two halves are one act and must stay one act: an alias whose provenance
// went unrecorded is indistinguishable from `AS "A.K"`, and the result-set
// label site then has nothing to decide on but the spelling. Both callers (the
// PlanVisitor's qualified-projection bind and its logical-predicate twin) go
// through here for exactly that reason.
//
// An existing alias is left alone: the user named the slot, so there is no
// collision to break and nothing to mark.
func mintQualifiedDatumKey(proj *logical.LogicalProject, i int, col projCol) {
	if proj.Aliases == nil {
		proj.Aliases = make([]string, len(proj.Projections))
	}
	if proj.AliasMinted == nil {
		proj.AliasMinted = make([]bool, len(proj.Projections))
	}
	if proj.AliasSources == nil {
		proj.AliasSources = make([]values.ProjectionAliasSource, len(proj.Projections))
	}
	if i < len(proj.Aliases) && proj.Aliases[i] == "" {
		proj.Aliases[i] = strings.ToUpper(col.name)
		if i < len(proj.AliasMinted) {
			proj.AliasMinted[i] = true
		}
		// The parse-tree segment vector is the authority. In particular,
		// `A.N.SK` was authored against source A; splitting the rendered alias
		// at its last dot would manufacture source A.N, while reading the later
		// physical Value can produce `_current`. If segments were not captured,
		// leave the source absent rather than guess from the alias bytes.
		if i < len(proj.AliasSources) && col.qualified && len(col.segs) > 1 {
			proj.AliasSources[i] = values.NewProjectionAliasSource(
				values.NamedCorrelationIdentifier(col.segs[0]))
		}
	}
}

// resolveBaked is the ONE shape test for "did the resolver hand back a
// plan-time-baked reference this site may keep". Both the bare and the qualified
// resolution sites call it, because a bare spelling and a qualified spelling of
// the same column must resolve by the same rule — Java's
// SemanticAnalyzer.resolveCorrelatedIdentifier asserts qualification and then
// delegates to the very same resolveIdentifier, so qualification is a
// PRECONDITION and never a different algorithm (RFC-223 §3). Two inline copies of
// this predicate are how the two sites drifted into accepting disjoint shapes,
// which is the defect RFC-223 removes; keeping it one function is what stops
// them drifting apart again.
//
// It owns the SHAPE test ONLY. The explicit-qualifier precondition stays at the
// qualified call site, where it is a precondition rather than a property of the
// value, and the two must not fuse: importing the shape test without its
// precondition is exactly how a widened predicate can admit a population it was
// never measured against.
//
// The two admissible shapes:
//
//   - CHILD-BEARING + leg-relative unpinned — the merged-row shape. The executor
//     binds the leg's window off the merged row's own leg boundaries
//     (rowLegsBinder), so the ordinal reads positionally over the composed row.
//     Always admissible.
//   - CHILDLESS + source-relative — an ordinal relative to the reference's OWN
//     source row, correct where there is no leg choice to lose. Over a merged
//     row it would address another leg's slot. `upgradeAggregateOperands`
//     (`logical_predicate.go`, the aggregate GROUP-key filler) states that same
//     rule inline as `len(sq.joins) == 0`, with "on a join, childless would lose
//     the defining leg and remains forbidden".
//
// childlessOK IS AN UNMEASURED PRECAUTION, AND SAYING SO IS THE POINT. Setting
// it true at BOTH call sites — the context-free union — was tried and produces a
// byte-identical plan-shape golden and a fully green suite, so no measurement
// says the parameter is load-bearing. It is kept as defence-in-depth for the one
// failure it prevents, which is the expensive kind: a childless ordinal read
// over a merged row is a SILENT wrong-slot read, not a loud decline. A guard
// against a silent failure is worth keeping without a reproducer; it is not
// worth claiming a reproducer it does not have.
//
// It is also a GO-ONLY artifact and does not follow from Java.
// `SemanticAnalyzer.resolveIdentifier` is a lookup plus two asserts — no fork,
// no context parameter — so the "Java has one function, Go gets one function"
// argument (RFC-223 §3) is only half-honoured by a single function that applies
// different rules to its two callers. The honest statement is that Go has one
// SITE for the rule, which is what stops the two spellings drifting apart, and
// that the context parameter is a Go-side addition.
//
// A non-FieldValue, a lazy result, or a shape the caller did not admit returns
// nil, leaving the caller's existing emission — loud at runtime, never a silent
// wrong-slot read.
func resolveBaked(rv values.Value, _ bool) values.FieldValue {
	fv, isFV := values.AsFieldValue(rv)
	if !isFV {
		return nil
	}
	if _, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue()); isQOV && !fv.Path().IsFrontierPinned() {
		return fv
	}
	return nil
}

// resolveProjectionValue keeps either exact resolver shape a SELECT slot may
// own: a field access baked against a declared row, or the whole-object QOV of
// a scalar lateral-unnest binding. WITH ORDINALITY takes the first form (AS and
// AT are distinct fields of one exact two-slot row); non-ordinal UNNEST takes
// the second (the element itself is the flowed object).
func resolveProjectionValue(rv values.Value) values.Value {
	if baked := resolveBaked(rv, true); baked != nil {
		return baked
	}
	if qov, ok := values.AsQuantifiedObjectValue(rv); ok && qov.FlowedType().Code() != values.TypeCodeRecord {
		return qov
	}
	return nil
}

// resolveBareProjectionValue resolves the parser's normalized bare spelling
// first, then applies the same folded fallback used by
// resolveColumnRefStructural. Synthesized derived/virtual sources currently
// register some DDL-backed names folded while preserving explicit AS/AT names;
// resolving the Value must make exactly the same choice as the preceding
// validation or a validated SELECT slot can be left nil.
func resolveBareProjectionValue(resolver *expr.Resolver, bare string) (values.Value, error) {
	rv, err := resolver.ResolveIdentifier(semantic.Identifier{}, semantic.FromNormalized(bare))
	if err == nil {
		return rv, nil
	}
	var notFound *semantic.ColumnNotFoundError
	if !errors.As(err, &notFound) {
		return nil, err
	}
	return resolver.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted(bare))
}

// resolveQualifiedBaked resolves a QUALIFIED column reference through the scope
// and returns it ONLY when the resolver produced a QUANTIFIER-ADDRESSED BAKED
// reference (a QOV-child SourceRelativeBaked node — a construction-bound
// ordinal). The executor binds the leg's window off the
// merged row's own leg boundaries (rowLegsBinder), so the source-relative
// ordinal reads positionally over the composed row. A CHILDLESS bake carries an
// ordinal relative to its OWN source row (would misread another leg's slot over
// a merged row) — excluded. A DUPLICATE plain alias (`FROM p AS a, q AS a`)
// declines (stays display-keyed, loud). Any resolution error or lazy result
// returns nil, keeping the caller's legacy emission (loud at runtime, never a
// silent wrong-slot read).
func resolveQualifiedBaked(resolver *expr.Resolver, ref colRef) values.Value {
	if !ref.isQualified() {
		return nil
	}
	return resolveQualifiedBakedPath(resolver,
		[]semantic.Identifier{semantic.FromNormalized(ref.table), semantic.FromNormalized(ref.bare())})
}

// resolveQualifiedBakedPath is resolveQualifiedBaked over the full segment
// list. A reference that DESCENDS into a struct on the join path must resolve
// through every segment: collapsing `a.n.sk` to a (qualifier, name) pair asks
// the scope for a source named "A.N", which resolves to nothing and left the
// projection with no baked value at all.
func resolveQualifiedBakedPath(resolver *expr.Resolver, segs []semantic.Identifier) values.Value {
	if len(segs) < 2 {
		return nil
	}
	rv, err := resolver.ResolveIdentifierPath(segs)
	if err != nil || rv == nil {
		return nil
	}
	// childlessOK=false: a qualified reference is resolved here precisely
	// because a leg choice exists, so a source-row-relative ordinal would lose
	// the leg it names. The single-source case, where the qualifier is redundant
	// and childless IS correct, is handled by the caller that can prove it —
	// `logical_predicate.go`'s `len(sq.joins) == 0` arm.
	if fv := resolveBaked(rv, false); fv != nil {
		return fv
	}
	return nil
}

// resolveQualifiedProjectionValuePath is the qualified projection caller's
// exact-value gate. Most sources resolve to a FieldValue baked against their
// declared row, but a non-ordinal scalar lateral unnest flows the element as
// the whole QuantifiedObjectValue. Rejecting that second exact shape made a
// correctly expanded `V.*` report that V declared no column order even though
// the source's declared contract is precisely the scalar QOV.
func resolveQualifiedProjectionValuePath(resolver *expr.Resolver, segs []semantic.Identifier) values.Value {
	if len(segs) < 2 {
		return nil
	}
	rv, err := resolver.ResolveIdentifierPath(segs)
	if err != nil || rv == nil {
		return nil
	}
	return resolveProjectionValue(rv)
}

func qualifyShadowedSortKeys(op logical.LogicalOperator, resolver *expr.Resolver) error {
	sort := findSort(op)
	if sort == nil {
		return nil
	}
	for i := range sort.Keys {
		// A key already carrying a resolved Value (a projection alias, a computed
		// expression, an aggregate group key) is not a bare unnest column — leave it.
		if sort.Keys[i].Value != nil {
			continue
		}
		// Structured segments — an expression key has none (nothing to
		// resolve as an unnest column) and skips; the retired text re-parse
		// split a canonical rendering on its last dot.
		bare := sort.Keys[i].Bare
		if bare == "" {
			continue
		}
		id := semantic.FromNormalized(bare)
		if sort.Keys[i].Qualified {
			// An AmbiguousColumnError here is DISCARDED on purpose: the
			// upstream sort-key reference validation already terminated an
			// ambiguous key with 42702 before this qualification pass runs
			// (the ladder's >=2 arm is owned there, not here).
			qv, err := resolver.ResolveQualifiedProjection(semantic.FromNormalized(sort.Keys[i].Qualifier), id)
			if err == nil && qv != nil {
				sort.Keys[i].Value = qv
				continue
			}
			var unres *expr.UnresolvableOrdinalError
			if errors.As(err, &unres) {
				// Born-baked (slice 2): an unbindable ordinal must not
				// fall through to the name channel — that is the
				// reads-by-name failure the retirement killed.
				return err
			}
			// Every other QUALIFIED sort key resolves through the scope to
			// the quantifier-addressed source-relative baked reference so
			// the key resolves positionally through its leg window
			// (rowLegsBinder) instead of a flat dotted name read.
			if bv := resolveQualifiedBaked(resolver, colRef{table: sort.Keys[i].Qualifier, col: bare}); bv != nil {
				sort.Keys[i].Value = bv
			}
			continue
		}
		qv, ok, err := resolver.ResolveColumnShadowingQualified(semantic.Identifier{}, id)
		if err != nil || !ok {
			var unres *expr.UnresolvableOrdinalError
			if errors.As(err, &unres) {
				return err
			}
			continue
		}
		sort.Keys[i].Value = qv
	}
	return nil
}

// resolveLimitAtom resolves one LIMIT/OFFSET atom to an integer. A
// decimalLiteral that is not a valid integer (`0.0`, `0L`) is a hard 42601
// syntax error: the grammar accepts any decimalLiteral here, but a
// non-integer one is invalid and must be REJECTED, never silently dropped
// (the old code left the no-limit / zero-offset sentinel, so `LIMIT 0.0`
// returned ALL rows instead of none). Driver parameters are substituted before
// query construction. A remaining parameter is unresolved and must fail here,
// never become the absent-limit/zero-offset sentinel on another builder path.
func resolveLimitAtom(atom antlrgen.ILimitClauseAtomContext) (val int64, ok bool, err error) {
	if atom == nil {
		return 0, false, nil
	}
	if atom.PreparedStatementParameter() != nil {
		return 0, false, api.NewError(api.ErrCodeUnsupportedQuery, "a query with a planning-time unresolved LIMIT/OFFSET is not supported")
	}
	text := atom.GetText()
	v, perr := strconv.ParseInt(text, 10, 64)
	if perr != nil {
		return 0, false, api.NewErrorf(api.ErrCodeSyntaxError,
			"LIMIT/OFFSET value must be an integer literal, got %q", text)
	}
	return v, true, nil
}

// parseLimitClause reads the LIMIT/OFFSET values from simpleTable's clause.
// Returns (-1, 0, nil) when absent (limit == -1 means "no LIMIT clause"; a pure
// OFFSET still returns limit -1 with offset > 0), or a 42601 error when a
// LIMIT/OFFSET literal is present but not a valid integer. Shared by visitLimit
// (the live ANTLR-direct path) and extractFromSimpleTable (the selectQuery path
// for union branches / derived tables) so a LIMIT in either position is parsed
// identically and never silently dropped (RFC-128).
//
// Grammar: `LIMIT limit=limitClauseAtom (OFFSET offset=limitClauseAtom)?` —
// both atoms are LABELED; there is no positional `LIMIT a, b` form, so the
// two labeled accessors cover every atom.
func parseLimitClause(simpleTable *antlrgen.SimpleTableContext) (limit, offset int64, err error) {
	limit = -1
	limitClauseCtx := simpleTable.LimitClause()
	if limitClauseCtx == nil {
		return limit, offset, nil
	}
	if v, ok, e := resolveLimitAtom(limitClauseCtx.GetLimit()); e != nil {
		return limit, offset, e
	} else if ok {
		limit = v
	}
	if v, ok, e := resolveLimitAtom(limitClauseCtx.GetOffset()); e != nil {
		return limit, offset, e
	} else if ok {
		offset = v
	}
	return limit, offset, nil
}

// visitLimit checks the ANTLR parse tree for a LIMIT clause. Go
// extension: LIMIT/OFFSET are supported (most-requested feature).
// Builds a LogicalLimit node; the Cascades translator turns it into a
// RecordQueryLimitPlan operator applied at its pipeline position (RFC-128).
func (v *PlanVisitor) visitLimit(op logical.LogicalOperator, simpleTable *antlrgen.SimpleTableContext) (logical.LogicalOperator, error) {
	limit, offset, err := parseLimitClause(simpleTable)
	if err != nil {
		return nil, err
	}
	if limit >= 0 || offset > 0 {
		return logical.NewLimit(op, limit, offset), nil
	}
	return op, nil
}

// visitFinalProjection builds the non-aggregate projection by reading
// SELECT elements directly from the ANTLR parse tree. Aggregate
// queries have their projection handled in visitSelectGroupBy; this
// only fires when hasAggregate is false.
//
// SELECT * (projCols nil) and SELECT qualifier.* (sole qualifier-star)
// skip the projection node — the downstream scan delivers all columns.
// Mixed qualifier-star + named columns are handled as regular slots.
func (v *PlanVisitor) visitFinalProjection(op logical.LogicalOperator, simpleTable *antlrgen.SimpleTableContext, hasAggregate bool, stripPrefix string) logical.LogicalOperator {
	if hasAggregate {
		return op
	}

	selElems := simpleTable.SelectElements()
	if selElems == nil {
		return op
	}

	strip := func(s string) string {
		if stripPrefix != "" && strings.HasPrefix(strings.ToUpper(s), stripPrefix) {
			return s[len(stripPrefix):]
		}
		return s
	}

	elems := selElems.AllSelectElement()
	if len(elems) == 0 {
		return op
	}

	// Check for SELECT * or sole SELECT qualifier.* — no projection.
	if len(elems) == 1 {
		switch elems[0].(type) {
		case *antlrgen.SelectStarElementContext:
			return op
		case *antlrgen.SelectQualifierStarElementContext:
			return op
		}
	}

	var projs []string
	var aliases []string
	var computed []bool
	var refs []logical.ColumnRef

	for _, elem := range elems {
		switch e := elem.(type) {
		case *antlrgen.SelectStarElementContext:
			// Mixed * with other elements — already rejected by
			// classifySelectElements. Defensive no-op.
			return op
		case *antlrgen.SelectQualifierStarElementContext:
			// Mixed qualifier.* slot — placeholder. The downstream
			// execution expands it.
			projs = append(projs, "")
			aliases = append(aliases, "")
			computed = append(computed, false)
			refs = append(refs, logical.ColumnRef{})
		case *antlrgen.SelectExpressionElementContext:
			alias := selectOutputAlias(e)
			// Try plain column name first.
			colName, nameErr := columnNameFromExpr(e.Expression(), "SELECT expression")
			if nameErr != nil {
				// Computed expression: use the raw expression text.
				exprText := canonicalTextOf(e.Expression())
				projs = append(projs, exprText)
				aliases = append(aliases, alias)
				computed = append(computed, true)
				refs = append(refs, logical.ColumnRef{})
			} else {
				rendered := strip(colName)
				projs = append(projs, rendered)
				aliases = append(aliases, alias)
				computed = append(computed, false)
				// The SEGMENTS behind the rendering this visitor just joined.
				// columnNameFromExpr and splitColumnRef read the same FullId
				// with the same per-segment quote stripping, so the triple
				// spells colName exactly — reconciled against the emitted name
				// because the derived-table shell may have stripped a
				// qualifier prefix off it. An aggregate item (`SUM(v)`) is not
				// a FullColumnName, so it captures nothing and its rendered
				// name is never read as qualified.
				bare, qual, qualified, segs := splitColumnRef(e.Expression())
				refs = append(refs, projColRef(
					projCol{name: colName, bare: bare, qualifier: qual, qualified: qualified, segs: segs},
					rendered))
			}
		}
	}

	if len(projs) == 0 {
		return op
	}

	proj := logical.NewProject(op, projs, aliases)
	proj.IsComputed = computed
	proj.ProjectionRefs = refs
	return proj
}

// visitUnion handles UNION ALL queries, threading CTE scopes through
// both branches with the same retained producer registry.
// Inside a recursive CTE body, UNION DISTINCT (bare UNION) is also
// permitted for cycle detection.
func (v *PlanVisitor) visitUnion(setQ *antlrgen.SetQueryContext) (logical.LogicalOperator, error) {
	if setQ == nil {
		return nil, nil
	}
	// Branches are built by this same owner: schemas alone cannot preserve an
	// enclosing CTE definition or the lexical parent of a scalar in a branch.
	return v.buildLogicalPlanForUnion(setQ, v.inRecursiveCTEBody)
}
