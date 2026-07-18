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
	md        *recordlayer.RecordMetaData
	cteScopes map[string]semantic.ScopeSource
	cteBodies map[string]logical.LogicalOperator // CTE name → body plan, for scalar subqueries referencing outer CTEs
	// cteOnScopes carries the ON-resolution-only sources for declared CTEs
	// whose schema derivation declined the global cteScopes (join/unnest
	// bodies) — consumed ONLY by upgradeJoinOnPredicates so an enclosing
	// explicit join's ON resolves (or fails LOUD via the nil-Table marker)
	// instead of being silently dropped. See buildCTEOnOnlySource.
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
			alias := ""
			if e.Uid() != nil {
				alias = functions.StripIdentifierQuotes(e.Uid().GetText())
			}
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
		return buildLogicalPlanForQuery(q), nil
	}

	ctesCtx := q.Ctes()

	// Pre-scan CTE definitions to extract column schemas and eagerly
	// build CTE body plans. Process in declaration order so CTE B can
	// reference CTE A's derived schema. The eager body plans are needed
	// so scalar subqueries that reference outer CTEs can build
	// self-contained logical plans (wrapped with LogicalCTE).
	if ctesCtx != nil {
		if v.cteScopes == nil {
			v.cteScopes = make(map[string]semantic.ScopeSource)
		}
		if v.cteBodies == nil {
			v.cteBodies = make(map[string]logical.LogicalOperator)
		}
		if v.cteOnScopes == nil {
			v.cteOnScopes = make(map[string]semantic.ScopeSource)
		}
		// Duplicate detection is scoped to THIS WITH clause: a name already
		// registered from an ENCLOSING query's WITH is legal lexical
		// SHADOWING (a nested body's `WITH c1 …` shadows the outer c1 inside
		// the body — buildCTEBodyQuery snapshots/restores the maps), not a
		// duplicate. Consulting the shared maps here mis-fired 42712 on the
		// shadowing shape Java accepts (cte.yamsql). ON-only registrations
		// are declared names too, so one set covers both arms.
		declaredHere := make(map[string]struct{}, len(ctesCtx.AllNamedQuery()))
		for _, nq := range ctesCtx.AllNamedQuery() {
			name := functions.FullIdToName(nq.GetName())
			upper := strings.ToUpper(name)
			if _, exists := declaredHere[upper]; exists {
				return nil, api.NewErrorf(api.ErrCodeDuplicateAlias,
					"found '%s' more than once", name)
			}
			declaredHere[upper] = struct{}{}
			if src, ok := buildCTEColumnSource(v.md, name, nq.Query(), v.cteScopes); ok {
				// Apply CTE column aliases: WITH c1(x, y) AS (...)
				if colAliases := nq.GetColumnAliases(); colAliases != nil {
					if aliasList, ok := colAliases.(*antlrgen.FullIdListContext); ok && aliasList != nil {
						aliases := aliasList.AllFullId()
						if nAliases := len(aliases); nAliases > 0 && src.Table != nil {
							nCols := len(src.Table.Columns())
							if nAliases != nCols {
								return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference,
									"cte query has %d column(s), however %d aliases defined",
									nCols, nAliases)
							}
						}
					}
					src = applyCTEColumnAliases(src, colAliases)
				}
				v.cteScopes[upper] = src
				// SHADOWING an enclosing same-name CTE across derivability
				// classes: the inner registration must also EVICT the outer's
				// entry from the OPPOSITE map, or later resolution consults
				// the stale outer schema through the map this registration
				// did not write (false 42703 in JOIN ON / wrong slot —
				// silent misread). Mirrors the catalog path's opposite-map
				// deletions.
				delete(v.cteOnScopes, upper)
			} else {
				// Declared but not globally derivable (join/unnest body): the
				// ON-only registration keeps an enclosing explicit join's ON
				// resolvable — or LOUDLY dropped (marker) — never silent.
				if regErr := registerCTEOnOnlyScope(v.cteOnScopes, upper, nq.Query(), nq.GetColumnAliases(), v.md, v.schemaName, v.cteScopes); regErr != nil {
					return nil, regErr
				}
				// Opposite-map eviction, ON-only arm: an underivable inner
				// shadowing a DERIVABLE outer must displace the outer's
				// cteScopes entry, or the body/main resolves named reads
				// against the stale outer schema (the twin of the derivable
				// arm's eviction above). A TOMBSTONE (nil Table), not
				// deletion: absence falls back to the CATALOG and a
				// same-named base table would silently bind its ordinals
				// onto the CTE's rows; the tombstone keeps the name declared
				// and resolution loud.
				if _, hadOuter := v.cteScopes[upper]; hadOuter {
					v.cteScopes[upper] = semantic.ScopeSource{}
				}
			}
			// Eagerly build the CTE body plan so scalar subqueries
			// that reference this CTE can wrap themselves with it.
			// For recursive CTEs, set inRecursiveCTEBody so the
			// union builder permits UNION DISTINCT.
			if inner := nq.Query(); inner != nil {
				isRecBody := ctesCtx.RECURSIVE() != nil && containsTableRef(inner.QueryExpressionBody(), upper)
				if isRecBody {
					v.inRecursiveCTEBody = true
				}
				// Self-invisible body build (buildCTEBodySelfHidden, both
				// maps): SQL scoping makes `FROM T1` inside T1's body the
				// TABLE, never the CTE being defined — CTE-first scope
				// resolution would otherwise resolve the body against its
				// own OUTPUT schema (the R5a shadow pin 42703'd on ID).
				bodyOp, bodyErr := buildCTEBodySelfHidden(v.cteScopes, v.cteOnScopes, upper, nil, isRecBody, func() (logical.LogicalOperator, error) {
					if isRecBody {
						// Recursive bodies are SetQuery-shaped by contract and
						// this arm builds the body WITHOUT its own ctes — a
						// nested WITH inside one would be SILENTLY DROPPED
						// (the exact bug class buildCTEBodyQuery fixes), so
						// decline typed instead.
						if inner.Ctes() != nil {
							return nil, api.NewError(api.ErrCodeUnsupportedQuery,
								"nested WITH inside a recursive CTE body is not supported")
						}
						return v.VisitQueryBody(inner.QueryExpressionBody())
					}
					return v.buildCTEBodyQuery(inner)
				})
				if isRecBody {
					v.inRecursiveCTEBody = false
				}
				if bodyErr == nil && bodyOp != nil {
					v.cteBodies[upper] = bodyOp
				}
			}
		}
	}

	main, err := v.VisitQueryBody(q.QueryExpressionBody())
	if err != nil {
		return nil, err
	}
	if main == nil {
		return nil, nil
	}
	if ctesCtx == nil {
		return main, nil
	}
	recursive := ctesCtx.RECURSIVE() != nil
	// No clause = ANY (the planner picks); an explicit level_order pins
	// the level union (the clause's only remaining alternative).
	traversalOrder := logical.TraversalAnyOrder
	if toc := ctesCtx.TraversalOrderClause(); toc != nil {
		traversalOrder = logical.TraversalLevelOrder
		if toc.PRE_ORDER() != nil {
			traversalOrder = logical.TraversalPreOrder
		} else if toc.POST_ORDER() != nil {
			traversalOrder = logical.TraversalPostOrder
		}
	}
	ctes := ctesCtx.AllNamedQuery()
	for i := len(ctes) - 1; i >= 0; i-- {
		nq := ctes[i]
		name := functions.FullIdToName(nq.GetName())
		upper := strings.ToUpper(name)
		// Java alignment: fdb-relational's SemanticAnalyzer requires
		// every CTE under WITH RECURSIVE to actually self-reference.
		// Non-self-referencing bodies are rejected with "condition is
		// not met!" (0A000). This check must run before the eagerly-
		// built body is reused, because the eager builder doesn't
		// validate recursive semantics.
		if recursive {
			if inner := nq.Query(); inner != nil {
				qeb := inner.QueryExpressionBody()
				if !containsTableRef(qeb, upper) {
					return nil, api.NewError(api.ErrCodeUnsupportedOperation,
						"condition is not met!")
				}
			}
		}
		// Reuse eagerly-built body plans when available (non-recursive).
		body := v.cteBodies[upper]
		if body == nil {
			if inner := nq.Query(); inner != nil {
				if recursive {
					qeb := inner.QueryExpressionBody()
					if _, isSet := qeb.(*antlrgen.SetQueryContext); !isSet {
						return nil, api.NewError(api.ErrCodeUnsupportedOperation,
							"recursive CTE requires UNION ALL body")
					}
					v.inRecursiveCTEBody = true
				}
				// Self-invisible rebuild (buildCTEBodySelfHidden). NOT the
				// same scoping as the eager build: this arm runs after ALL
				// registrations, so a body referencing a LATER-declared
				// sibling resolves here where the eager build correctly
				// failed — the rebuild converts SQL's forward-reference
				// rejection into a non-SQL acceptance (review-caught,
				// pre-existing; booked as the forward-visibility
				// Java-conformance probe on the output-naming slice).
				body, err = buildCTEBodySelfHidden(v.cteScopes, v.cteOnScopes, upper, nil, recursive, func() (logical.LogicalOperator, error) {
					if recursive {
						// Same silent-drop hazard as the eager arm: decline a
						// nested WITH typed rather than build the body without
						// its ctes.
						if inner.Ctes() != nil {
							return nil, api.NewError(api.ErrCodeUnsupportedQuery,
								"nested WITH inside a recursive CTE body is not supported")
						}
						return v.VisitQueryBody(inner.QueryExpressionBody())
					}
					return v.buildCTEBodyQuery(inner)
				})
				if recursive {
					v.inRecursiveCTEBody = false
				}
				if err != nil {
					return nil, err
				}
			}
		}
		if body == nil {
			return nil, nil
		}
		cte := logical.NewCTE(name, body, main, recursive)
		cte.TraversalOrder = traversalOrder
		if colAliases := nq.GetColumnAliases(); colAliases != nil {
			if aliasList, ok := colAliases.(*antlrgen.FullIdListContext); ok && aliasList != nil {
				aliases := aliasList.AllFullId()
				names := make([]string, len(aliases))
				for j, fid := range aliases {
					names[j] = strings.ToUpper(functions.StripIdentifierQuotes(functions.FullIdToName(fid)))
				}
				cte.ColumnAliases = names
			}
		}
		main = cte
	}
	return main, nil
}

// VisitQueryBody dispatches simple SELECT vs UNION, threading
// metadata and CTE scopes through both arms.
func (v *PlanVisitor) VisitQueryBody(body antlrgen.IQueryExpressionBodyContext) (logical.LogicalOperator, error) {
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

	// Step 1: FROM → parse the source first. Java's QueryVisitor
	// rejects FROM-less SELECTs before any function dispatch, so
	// parseFromSource must run before classification/validation.
	fs, err := parseFromSource(simpleTable)
	if err != nil {
		return nil, err
	}

	// Classify SELECT elements, GROUP BY, HAVING, ORDER BY. This is
	// the pure parse-tree classification — no FROM, no selectQuery.
	cls, err := classifySelectElements(simpleTable)
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

	// Validate qualified star sources against FROM.
	if err := validateQualifiedStarSourcesFromClassification(cls, fs, v.md); err != nil {
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
		// Seed the in-scope CTE-name set from the WITH catalog: the FROM tree `op`
		// here is built BEFORE the enclosing LogicalCTE wrapper is applied, so a
		// segment-0 reference to an in-scope WITH CTE (`WITH D AS (…) … FROM D, D.arr
		// AS x AT o`) is only knowable from v.cteScopes. An AT over such a CTE source
		// is the translator's outerSourceIsCTE UNSUPPORTED_QUERY, NOT the base-table
		// WRONG_OBJECT_TYPE — even when the CTE name ALSO matches a real table. RFC-142.
		cteNames := make(map[string]struct{}, len(v.cteScopes))
		for name := range v.cteScopes {
			cteNames[strings.ToUpper(name)] = struct{}{}
		}
		if err := rejectAtOrdinalityOnTableWithCTEs(op, v.md, cteNames); err != nil {
			return nil, err
		}
	}

	// Step 2: WHERE → wrap with filter directly from ANTLR.
	op = v.visitWhere(op, simpleTable)

	// Step 3: SELECT + GROUP BY + HAVING → aggregate classification
	// and operator building.
	op, stripPrefix := v.visitSelectGroupBy(op, cls, fs)

	// Collect SELECT column names and aliases from ANTLR for ORDER BY
	// positional reference resolution. This is a lightweight scan —
	// aggregate classification stays in the selectClassification.
	selectCols, selectAliases := collectSelectNames(simpleTable)

	// Step 4: ORDER BY → wrap with sort directly from ANTLR. Reads
	// simpleTable.OrderByClause() and resolves positional references
	// against the SELECT column list.
	hasAggregate := cls.countStar || len(cls.aggCols) > 0
	op = v.visitOrderBy(op, simpleTable, selectCols, selectAliases, cls.aggCols, stripPrefix, groupKeyRefDisplays(cls.groupBy), cls.groupByAliases, cls.postSortStripProj, cls.postSortStripAliases)

	// Post-sort strip projection: when hasSortOnly is true in the
	// aggregate path, the visible-only projection is deferred past
	// Sort so sort-key columns remain accessible.
	if len(cls.postSortStripProj) > 0 {
		op = logical.NewProject(op, cls.postSortStripProj, cls.postSortStripAliases)
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
	op = v.visitLimit(op, simpleTable)

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

	// Build the semantic scope once. All identifier resolution goes
	// through this scope — same architecture as Java's QueryVisitor
	// holding a SemanticAnalyzer.
	resolver := buildSelectScope(sq, v.md, v.schemaName, v.cteScopes)

	// (1) Expand qualified stars (a.*) in the projection list.
	needRebuild := false
	if sq.projQualifier != "" && sq.projCols == nil {
		expandProjQualifier(sq, v.md, v.schemaName)
		needRebuild = true
	}
	if hasAnyQualifiedStar(sq) {
		if starErr := expandQualifiedStars(sq, v.md, v.schemaName, v.cteScopes); starErr != nil {
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
		sq.limit, sq.offset = parseLimitClause(simpleTable)
		op = buildLogicalPlanForSelect(sq)
		if op == nil {
			return op, nil
		}
	}

	// (2) Resolve projection columns through the scope.
	if resolver != nil && sq.projCols != nil && len(sq.aggCols) == 0 && !sq.countStar {
		proj := findProjection(op)
		for i, col := range sq.projCols {
			if i < len(sq.projExprs) && sq.projExprs[i] != nil {
				if proj != nil {
					wv, walkErr := resolver.WalkExpression(sq.projExprs[i])
					if walkErr != nil {
						var corrErr *CorrelatedExistsError
						if errors.As(walkErr, &corrErr) {
							return nil, corrErr
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
				if err := resolveColumnRefStructural(resolver, col.bare, col.qualifier, col.qualified); err != nil {
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
				id := semantic.NewUnquoted(col.bare)
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
				// projection carries the construction-bound ordinal (a
				// childless source-relative baked FieldValue — the resolver's
				// single-source bind). Anything else — a multi-source
				// QOV-correlated resolution, an unresolvable name, a lazy
				// result — keeps the translator's name emission unchanged.
				if proj.ProjectedValues == nil || (i < len(proj.ProjectedValues) && proj.ProjectedValues[i] == nil) {
					rv, rerr := resolver.ResolveIdentifier(semantic.Identifier{}, id)
					if rerr == nil {
						if fv, isFV := rv.(*values.FieldValue); isFV && fv.Child == nil && fv.SourceRelativeBaked() {
							if proj.ProjectedValues == nil {
								proj.ProjectedValues = make([]values.Value, len(proj.Projections))
							}
							if i < len(proj.ProjectedValues) {
								proj.ProjectedValues[i] = fv
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
					qv, qerr := resolver.ResolveQualifiedProjection(
						semantic.NewUnquoted(col.qualifier), semantic.NewUnquoted(col.bare))
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
						} else if bv := resolveQualifiedBaked(resolver, colRef{table: col.qualifier, col: col.bare}); bv != nil {
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
								if proj.Aliases == nil {
									proj.Aliases = make([]string, len(proj.Projections))
								}
								if i < len(proj.Aliases) && proj.Aliases[i] == "" {
									proj.Aliases[i] = strings.ToUpper(col.name)
								}
							}
						} else if resolver.QualifierIsDuplicated(semantic.NewUnquoted(col.qualifier)) {
							// The DUP-ALIAS face: QOV(alias) cannot
							// distinguish the two same-named legs, so the
							// reference stays display-keyed against the
							// merged row's alias-pinned datum key. Phase B's
							// machine-minted unique quantifier aliases make
							// QOV addressing unambiguous and retire this
							// carve-out — the last flat-name projection mint.
							proj.ProjectedValues[i] = &values.FieldValue{
								Field: strings.ToUpper(col.name),
								Typ:   values.UnknownType,
							}
						} else {
							// Born-baked (slice 3): a validated qualified
							// projection over a UNIQUE alias that cannot bake
							// a leg-window ordinal must fail the plan, never
							// mint a lazy name read (dead-in-effect across
							// yamsql, embedded, and the full FDB driver
							// suites before retirement).
							return nil, &expr.UnresolvableOrdinalError{Field: col.bare, Source: col.qualifier}
						}
					}
				} else {
					var qualifier semantic.Identifier
					id := semantic.NewUnquoted(colBareOrName(col))
					if col.qualified {
						qualifier = semantic.NewUnquoted(col.qualifier)
					}
					if rv, err := resolver.ResolveIdentifier(qualifier, id); err == nil {
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
							if resolveColumnRefStructural(resolver, ob.bare, ob.qualifier, ob.qualified) == nil {
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
				if err := resolveColumnRefStructural(resolver, gb.bare, gb.qualifier, gb.qualified); err != nil {
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
					if err := resolveColumnRefStructural(resolver, ac.aggArgBare, ac.aggArgQualifier, ac.aggArgQualified); err != nil {
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
			if err := resolveColumnRefStructural(resolver, ac.groupColBare, ac.groupColQualifier, ac.groupColQualified); err != nil {
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
		if err := upgradeJoinOnPredicates(op, sq, v.md, v.schemaName, v.cteScopes, v.cteOnScopes); err != nil {
			return nil, err
		}
	}

	// (10) Upgrade aggregate operands + GROUP BY key values.
	if len(sq.aggCols) > 0 {
		if uerr := upgradeAggregateOperands(op, sq, v.md, v.schemaName, v.cteScopes); uerr != nil {
			return nil, uerr
		}
	}

	// (11) Create a unified SubqueryPlanner for EXISTS/scalar subqueries.
	existsPlanner := &existsSubqueryPlanner{
		md:          v.md,
		schemaName:  v.schemaName,
		outerScopes: buildOuterScopeSources(sq, v.md, v.schemaName),
		cteScopes:   v.cteScopes,
		cteOnScopes: v.cteOnScopes,
		cteBodies:   v.cteBodies,
	}

	// (12) Upgrade projection values.
	if len(sq.projExprs) > 0 || len(sq.postAggExprs) > 0 {
		if err := upgradeProjectionValues(op, sq, v.md, v.schemaName, v.cteScopes, existsPlanner); err != nil {
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
		if herr := upgradeHavingPredicate(op, sq, v.md, v.schemaName, v.cteScopes, existsPlanner); herr != nil {
			return nil, herr
		}
		// RFC-180 correct-or-loud: a CORRELATED scalar subquery minted by the
		// HAVING walk has no lowering — ScalarSubqueryValue's evaluation
		// contract is pre-eval (uncorrelated) only, and the projection attach
		// (13) already ran and nil'd the lists, so anything here would reach
		// the executor as an unbindable alias (runtime
		// UnboundScalarSubqueryError on a valid query). Decline TYPED; Java's
		// quantifier lowering is the booked follow-up.
		if len(existsPlanner.correlatedScalarSubqueries) > 0 {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"correlated scalar subquery in a WHERE/HAVING predicate is not supported")
		}
	}

	// (15) Upgrade sort key values.
	upgradeSortKeyValues(op, sq, v.md, v.schemaName, v.cteScopes)

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
		qualPred, qErr := buildQualifyPredicate(v.md, v.schemaName, sq, v.cteScopes)
		if qErr != nil {
			return nil, qErr
		}
		if qualPred != nil {
			op = attachOrSynthesizeFilter(op, qualPred)
			op = wrapGlobalRankVectorLimit(op, qualPred)
		}
		return op, nil
	}

	if resolver != nil {
		resolver.SetSubqueryPlanner(existsPlanner)
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
		walked, walkErr := resolver.WalkPredicate(sq.whereExpr.Expression())
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

	// RFC-180 correct-or-loud: a CORRELATED scalar subquery minted by the
	// WHERE walk above has no lowering. ScalarSubqueryValue's evaluation
	// contract is pre-eval (uncorrelated) only — the uncorrelated list below
	// attaches to the filter (upgradeFirstFilterScalarSubqueries) and the
	// executor pre-binds it, but the correlated list's ONLY consumer is the
	// PROJECTION path (materialized per-row column), whose attach step (13)
	// already ran and nil'd the lists. A WHERE-position reference therefore
	// reached the executor with an unbindable alias — the loud runtime
	// UnboundScalarSubqueryError on a valid query (yamsql scalar_subquery_java:
	// `WHERE e.salary = (SELECT MAX(e2.salary) … WHERE e2.dept_id =
	// e.dept_id)`). Java lowers the subquery to a quantifier and lets the
	// predicate reference its result — the booked RFC-180 follow-up; until
	// then decline TYPED at plan time.
	if len(existsPlanner.correlatedScalarSubqueries) > 0 {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"correlated scalar subquery in a WHERE/HAVING predicate is not supported")
	}

	hasSubqueries := len(existsPlanner.subqueries) > 0 || len(existsPlanner.scalarSubqueries) > 0
	if hasSubqueries && preWalkPred != nil {
		pred := predicates.SimplifyPredicateValues(preWalkPred)
		// EXISTS is lowered to a conjunctive semi-join; under an OR that loses
		// the disjunction and silently returns empty. Reject rather than
		// return wrong rows (RFC-082; inline-EXISTS-under-OR is future work).
		if existsUnderDisjunction(pred) {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation,
				"EXISTS within an OR (disjunction) is not supported")
		}
		if len(sq.joins) > 0 && len(existsPlanner.subqueries) == 1 {
			esq := existsPlanner.subqueries[0]
			if esq.JoinPredicate != nil {
				if eliminated := eliminateRedundantCrossJoin(op, sq, pred, esq); eliminated {
					return op, nil
				}
			}
		}
		combined, qErr := combineQualifyPred(v.md, v.schemaName, sq, v.cteScopes, pred)
		if qErr != nil {
			return nil, qErr
		}
		_ = upgradeFirstFilter(op, combined)
		if len(existsPlanner.subqueries) > 0 {
			upgradeFirstFilterExistsSubqueries(op, existsPlanner.subqueries)
		}
		if len(existsPlanner.scalarSubqueries) > 0 {
			upgradeFirstFilterScalarSubqueries(op, existsPlanner.scalarSubqueries)
		}
		op = wrapGlobalRankVectorLimit(op, combined)
		return op, nil
	}

	if preWalkPred != nil {
		pred := predicates.SimplifyPredicateValues(preWalkPred)
		combined, qErr := combineQualifyPred(v.md, v.schemaName, sq, v.cteScopes, pred)
		if qErr != nil {
			return nil, qErr
		}
		_ = upgradeFirstFilter(op, combined)
		op = wrapGlobalRankVectorLimit(op, combined)
		return op, nil
	}

	var pred predicates.QueryPredicate
	var predOk bool
	if v.cteScopes != nil && len(sq.joins) == 0 {
		if src, found := v.cteScopes[strings.ToUpper(sq.tableName)]; found && src.Table != nil {
			// A TOMBSTONE entry (nil Table: declared CTE, schema underivable)
			// must not reach scope construction — ResolveColumn nil-derefs.
			pred, predOk = buildWherePredicateFromCTEScope(src, sq.tableAlias, sq.whereExpr, v.md)
		}
	}
	if !predOk && v.cteScopes != nil && len(sq.joins) > 0 {
		pred, predOk = buildWherePredicateForJoinsWithCTEScopes(v.md, v.schemaName, sq, sq.whereExpr, v.cteScopes)
	}
	if !predOk {
		pred, predOk = buildWherePredicate(v.md, v.schemaName, sq, sq.whereExpr)
	}
	if !predOk {
		return op, nil
	}
	_ = upgradeFirstFilter(op, pred)
	return op, nil
}

// visitFrom builds the FROM-source subtree from the pre-parsed
// fromSource. For derived tables (subquery in FROM), it recursively
// calls v.VisitQueryBody to build the inner plan — CTE scopes flow
// naturally through the visitor instance.
//
// Pre-built derived table inner plans are written back to
// fs.joins[i].catalogAwareInnerPlan so that the selectQuery bridge
// carries them into the upgrade functions.
func (v *PlanVisitor) visitFrom(simpleTable *antlrgen.SimpleTableContext, fs *fromSource) (logical.LogicalOperator, error) {
	var op logical.LogicalOperator
	if fs.derivedQuery != nil {
		// Derived table: recursively build inner plan via the visitor.
		// CTE scopes flow naturally through the visitor instance, and
		// inner plans get catalog-aware upgrades.
		innerOp, innerErr := v.VisitQueryBody(fs.derivedQuery.QueryExpressionBody())
		if innerErr != nil {
			return nil, innerErr
		}
		if innerOp == nil {
			return nil, nil
		}
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
		op = logical.NewCTE(fs.tableName, innerOp,
			logical.NewScan(fs.tableName, ""), false)
	} else {
		op = logical.NewScan(fs.tableName, fs.tableAlias)
	}

	// Pre-build derived table inner plans for JOIN sources through the
	// visitor. Write back to fs.joins[i].catalogAwareInnerPlan so
	// the selectQuery carries them into the upgrade functions. If
	// upgrades trigger a needRebuild (qualified star expansion),
	// buildLogicalPlanForSelect can use the already-built inner plan
	// rather than falling back to the old non-CTE-aware path.
	for i := range fs.joins {
		fj := &fs.joins[i]
		if fj.derivedQuery == nil {
			continue
		}
		innerOp, innerErr := v.VisitQueryBody(fj.derivedQuery.QueryExpressionBody())
		if innerErr != nil {
			return nil, innerErr
		}
		if innerOp != nil {
			fj.catalogAwareInnerPlan = innerOp
		}
	}

	// JOINs chain left-to-right from the primary scan. Each join wraps
	// the current op as Left and scans the joined table as Right.
	resolvesToTable := newUnnestTableResolver(v.md, v.schemaName)
	for i, j := range fs.joins {
		var right logical.LogicalOperator
		if j.catalogAwareInnerPlan != nil {
			// Use the pre-built inner plan from the visitor.
			if j.alias != "" {
				cte := logical.NewCTE(j.alias, j.catalogAwareInnerPlan,
					logical.NewScan(j.alias, ""), false)
				cte.Binding = j.bindingID
				right = cte
			} else {
				right = j.catalogAwareInnerPlan
			}
		} else if j.derivedQuery != nil {
			// Fallback: derived table without a pre-built inner plan
			// (shouldn't happen, but defensive).
			innerRight, innerErr := v.VisitQueryBody(j.derivedQuery.QueryExpressionBody())
			if innerErr != nil {
				return nil, innerErr
			}
			if innerRight == nil {
				return nil, nil
			}
			if j.alias != "" {
				cte := logical.NewCTE(j.alias, innerRight,
					logical.NewScan(j.alias, ""), false)
				cte.Binding = j.bindingID
				right = cte
			} else {
				right = innerRight
			}
		} else if u := lateralUnnestCandidate(j, visibleFromAliases(fs.tableName, fs.tableAlias, fs.joins[:i], resolvesToTable), resolvesToTable); u != nil {
			// A comma source that may be a lateral array unnest
			// (`FROM t, t.arr AS x [AT ord]`). The translator classifies it
			// against the scope (segment 0 = an in-scope source with an array
			// field named by the rest → unnest, else a table) — the parser
			// preserved the uid segments + AT alias for exactly this. RFC-142.
			right = u
		} else {
			sc := logical.NewScan(j.tableName, j.alias)
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
func (v *PlanVisitor) visitSelectGroupBy(op logical.LogicalOperator, cls *selectClassification, fs *fromSource) (logical.LogicalOperator, string) {
	if cls == nil {
		return op, ""
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
		return op, stripPrefix
	}

	var aggAliases []string
	var aggCalls []logical.AggregateCall
	hasDistinct := false
	keys := logicalGroupKeys(cls.groupBy)
	for i := range keys {
		stripped := strip(keys[i].Display)
		if stripped != keys[i].Display {
			// The single-source prefix was baked away: the key is
			// BARE from here on — stale qualification segments would
			// chase a qualifier the runtime row no longer carries.
			keys[i] = logical.GroupKey{Display: stripped, Bare: stripped}
		}
	}
	if cls.countStar {
		aggAliases = []string{cls.countStarAlias}
		aggCalls = []logical.AggregateCall{{Func: "COUNT", Operand: "*", Star: true}}
	} else {
		for _, ac := range cls.aggCols {
			if ac.aggFunc != "" {
				arg := ac.aggArg
				if arg == "" && ac.aggExpr != nil {
					arg = canonicalTextOf(ac.aggExpr)
				}
				if arg == "" {
					arg = "*"
				}
				arg = strip(arg)
				if ac.aggDistinct {
					hasDistinct = true
				}
				aggCalls = append(aggCalls, logical.AggregateCall{
					Func:       strings.ToUpper(ac.aggFunc),
					Operand:    arg,
					Star:       arg == "*",
					Distinct:   ac.aggDistinct,
					BareColumn: ac.aggArg != "" && ac.aggExpr == nil,
					Qualified:  ac.aggArgQualified,
				})
				aggAliases = append(aggAliases, ac.outName)
			}
		}
	}
	aggOp := logical.NewAggregate(op, keys, aggCalls, aggAliases, cls.havingExpr != nil)
	aggOp.HasDistinctAggregate = hasDistinct
	op = aggOp

	// Post-aggregation projection for mixed SELECT lists that contain
	// both aggregates and computed expressions / constants.
	hasOutExpr := false
	for _, ac := range cls.aggCols {
		if ac.outExpr != nil && ac.aggFunc == "" && ac.visible {
			hasOutExpr = true
			break
		}
	}
	if hasOutExpr {
		if proj, antlr := buildPostAggregateProjection(op, cls.aggCols, strip); proj != nil {
			op = proj
			cls.postAggExprs = antlr
		}
	} else if len(keys) > 0 {
		hasNonVisible := false
		for _, ac := range cls.aggCols {
			if !ac.visible {
				hasNonVisible = true
				break
			}
		}
		var visibleProj []string
		var visibleAliases []string
		hasAggAlias := false
		for _, ac := range cls.aggCols {
			if !ac.visible {
				continue
			}
			if ac.aggFunc != "" {
				arg := ac.aggArg
				if arg == "" && ac.aggExpr != nil {
					arg = canonicalTextOf(ac.aggExpr)
				}
				if arg == "" {
					arg = "*"
				}
				arg = strip(arg)
				canonical := ac.aggFunc + "(" + arg + ")"
				visibleProj = append(visibleProj, canonical)
				alias := ""
				if ac.outName != "" && !strings.EqualFold(ac.outName, canonical) {
					alias = ac.outName
					hasAggAlias = true
				}
				visibleAliases = append(visibleAliases, alias)
			} else if ac.groupCol != "" {
				visibleProj = append(visibleProj, strip(ac.groupCol))
				alias := ""
				if ac.outName != "" && !strings.EqualFold(ac.outName, ac.groupCol) {
					alias = ac.outName
				}
				visibleAliases = append(visibleAliases, alias)
			}
		}
		totalOutput := len(keys) + len(aggCalls)
		needsStrip := len(visibleProj) != totalOutput || hasAggAlias || hasNonVisible
		// != (not <): a DUPLICATE visible read of one group key
		// (`SELECT id, id … GROUP BY id`) makes the visible list WIDER
		// than the aggregate's deduplicated output — the reshaping
		// projection must materialize each visible slot or downstream
		// consumers (CTE column aliases, positional reads) see the
		// internal one-slot layout.
		if needsStrip {
			if hasNonVisible || groupKeyMissingFromVisible(groupKeyDisplayNames(keys), visibleProj) {
				cls.postSortStripProj = visibleProj
				cls.postSortStripAliases = visibleAliases
			} else {
				op = logical.NewProject(op, visibleProj, visibleAliases)
			}
		}
	}

	return op, stripPrefix
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
	if inner.Ctes() == nil {
		return v.VisitQueryBody(inner.QueryExpressionBody())
	}
	// Restore by MUTATING the original map objects, never by reassigning the
	// fields to clones: this build runs inside buildCTEBodySelfHidden's
	// self-hide window, whose own deferred restore writes the enclosing CTE's
	// entry back into the map OBJECT it captured. A field reassignment here
	// would strand that restore in an orphaned map — the enclosing CTE would
	// vanish from scope after its own body build (a later reference to it
	// resolves against the base catalog instead: unknown table, or a
	// same-named base table silently). The two restore mechanisms compose
	// only on shared object identity.
	origScopes, origOn, origBodies := v.cteScopes, v.cteOnScopes, v.cteBodies
	savedScopes := maps.Clone(origScopes)
	savedOn := maps.Clone(origOn)
	savedBodies := maps.Clone(origBodies)
	restoreScope := func(dst, src map[string]semantic.ScopeSource) {
		if dst == nil {
			return
		}
		clear(dst)
		maps.Copy(dst, src)
	}
	defer func() {
		restoreScope(origScopes, savedScopes)
		restoreScope(origOn, savedOn)
		if origBodies != nil {
			clear(origBodies)
			maps.Copy(origBodies, savedBodies)
		}
		// Field identity: the nested VisitQuery may have lazily allocated a
		// map onto a nil field; point the fields back at the originals.
		v.cteScopes, v.cteOnScopes, v.cteBodies = origScopes, origOn, origBodies
	}()
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
func (v *PlanVisitor) visitOrderBy(op logical.LogicalOperator, simpleTable *antlrgen.SimpleTableContext, selectCols, selectAliases []string, aggCols []aggSelectCol, stripPrefix string, groupBy []string, groupByAliases map[string]int, deferredStripProj, deferredStripAliases []string) logical.LogicalOperator {
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
		posName, pos, isPos, posErr := resolveSelectListPosition("ORDER BY", obExpr.Expression(), selectCols, selectAliases, aggCols)
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
				keys = append(keys, logical.SortKey{Expr: deferredStripProj[pos-1], Dir: dir, NullsFirst: nf})
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
			kb, kq, kqf := splitColumnRef(obExpr.Expression())
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
			sk := logical.SortKey{Expr: strip(colName), Dir: dir, NullsFirst: nf, BareRef: bareRef, Bare: kb, Qualifier: kq, Qualified: kqf}
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
	rv, err := resolver.ResolveIdentifier(semantic.NewUnquoted(ref.table), semantic.NewUnquoted(ref.bare()))
	if err != nil || rv == nil {
		return nil
	}
	if fv, isFV := rv.(*values.FieldValue); isFV && fv.Child != nil && fv.SourceRelativeBaked() {
		return fv
	}
	return nil
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
		id := semantic.NewUnquoted(bare)
		if sort.Keys[i].Qualified {
			// An AmbiguousColumnError here is DISCARDED on purpose: the
			// upstream sort-key reference validation already terminated an
			// ambiguous key with 42702 before this qualification pass runs
			// (the ladder's >=2 arm is owned there, not here).
			qv, err := resolver.ResolveQualifiedProjection(semantic.NewUnquoted(sort.Keys[i].Qualifier), id)
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

// parseLimitClause reads a LIMIT/OFFSET clause from a SimpleTableContext and
// returns (limit, offset). limit == -1 means "no LIMIT clause present" (a pure
// OFFSET still returns limit -1 with offset > 0). Shared by visitLimit (the
// live ANTLR-direct path) and extractFromSimpleTable (the selectQuery path used
// for union branches / derived tables) so a LIMIT in either position is parsed
// identically and never silently dropped (RFC-128).
func parseLimitClause(simpleTable *antlrgen.SimpleTableContext) (limit, offset int64) {
	limit = -1
	limitClauseCtx := simpleTable.LimitClause()
	if limitClauseCtx == nil {
		return limit, offset
	}
	if offsetCtx := limitClauseCtx.GetOffset(); offsetCtx != nil {
		if val, err := strconv.ParseInt(offsetCtx.GetText(), 10, 64); err == nil {
			offset = val
		}
	}
	if limitCtx := limitClauseCtx.GetLimit(); limitCtx != nil {
		if val, err := strconv.ParseInt(limitCtx.GetText(), 10, 64); err == nil {
			limit = val
		}
	}
	atoms := limitClauseCtx.AllLimitClauseAtom()
	if limit < 0 && offset == 0 && len(atoms) == 2 {
		if val, err := strconv.ParseInt(atoms[0].GetText(), 10, 64); err == nil {
			offset = val
		}
		if val, err := strconv.ParseInt(atoms[1].GetText(), 10, 64); err == nil {
			limit = val
		}
	} else if limit < 0 && offset == 0 && len(atoms) == 1 {
		if val, err := strconv.ParseInt(atoms[0].GetText(), 10, 64); err == nil {
			limit = val
		}
	}
	return limit, offset
}

// limitClauseKeepsSingleRow reports whether simpleTable's LIMIT/OFFSET clause
// (if any) provably preserves a single input row. It is STRICTER than reading
// the (limit, offset) ints parseLimitClause returns: those sentinels conflate an
// ABSENT clause with a syntactically-accepted-but-unparseable atom. Both `LIMIT
// 0.0` and `LIMIT 1 OFFSET 1.0` parse (REAL_LITERAL is a decimalLiteral) yet
// ParseInt fails, silently leaving limit=-1 / offset=0 — so a nonzero OFFSET
// that failed to parse is indistinguishable from no offset via sq.offset alone.
// A `LIMIT ?` parameter is likewise unknown at plan time. For the always-true
// aggregate fold an unverifiable clause must DECLINE, never silently fold to
// every row. So require EVERY atom to parse cleanly (no parameter, no non-integer
// literal), then trust the resolved values: limit >= 1 keeps the row, offset == 0
// does not skip it.
func limitClauseKeepsSingleRow(simpleTable *antlrgen.SimpleTableContext) bool {
	lc := simpleTable.LimitClause()
	if lc == nil {
		return true
	}
	for _, atom := range lc.AllLimitClauseAtom() {
		if _, err := strconv.ParseInt(atom.GetText(), 10, 64); err != nil {
			return false
		}
	}
	limit, offset := parseLimitClause(simpleTable)
	return limit >= 1 && offset == 0
}

// visitLimit checks the ANTLR parse tree for a LIMIT clause. Go
// extension: LIMIT/OFFSET are supported (most-requested feature).
// Builds a LogicalLimit node; the Cascades translator turns it into a
// RecordQueryLimitPlan operator applied at its pipeline position (RFC-128).
func (v *PlanVisitor) visitLimit(op logical.LogicalOperator, simpleTable *antlrgen.SimpleTableContext) logical.LogicalOperator {
	limit, offset := parseLimitClause(simpleTable)
	if limit >= 0 || offset > 0 {
		return logical.NewLimit(op, limit, offset)
	}
	return op
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
		case *antlrgen.SelectExpressionElementContext:
			alias := ""
			if e.Uid() != nil {
				alias = functions.StripIdentifierQuotes(e.Uid().GetText())
			}
			// Try plain column name first.
			colName, nameErr := columnNameFromExpr(e.Expression(), "SELECT expression")
			if nameErr != nil {
				// Computed expression: use the raw expression text.
				exprText := canonicalTextOf(e.Expression())
				projs = append(projs, exprText)
				aliases = append(aliases, alias)
				computed = append(computed, true)
			} else {
				projs = append(projs, strip(colName))
				aliases = append(aliases, alias)
				computed = append(computed, false)
			}
		}
	}

	if len(projs) == 0 {
		return op
	}

	proj := logical.NewProject(op, projs, aliases)
	proj.IsComputed = computed
	return proj
}

// visitUnion handles UNION ALL queries, threading CTE scopes through
// both branches. Mirrors buildLogicalPlanForUnionWithCTECatalog.
// Inside a recursive CTE body, UNION DISTINCT (bare UNION) is also
// permitted for cycle detection.
func (v *PlanVisitor) visitUnion(setQ *antlrgen.SetQueryContext) (logical.LogicalOperator, error) {
	if setQ == nil {
		return nil, nil
	}
	// BOTH maps must be empty to short-circuit to the scope-less variant — the
	// THIRD instance of the empty-scope hole (the review-proven union-branch
	// cross-product): a join/unnest-bodied CTE lives ONLY in cteOnScopes, and
	// dropping it here silently dropped a union branch's join ON.
	if len(v.cteScopes) == 0 && len(v.cteOnScopes) == 0 && (v.schemaName == "" || v.schemaName == defaultEmbeddedSchema) {
		return buildLogicalPlanForUnionWithCatalog(setQ, v.md)
	}
	return buildLogicalPlanForUnionWithCTECatalog(setQ, v.md, v.schemaName, v.cteScopes, v.cteOnScopes, v.inRecursiveCTEBody)
}
