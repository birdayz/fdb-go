package embedded

import (
	"sort"
	"strings"

	"fdb.dev/pkg/relational/core/functions"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/logical"
	"github.com/antlr4-go/antlr/v4"
)

const (
	computedAggregateOutputOrdinal = -1
	invalidAggregateOutputOrdinal  = -2
)

// aggregateColumnsInSelectOrder returns visible aggregate-query SELECT items
// ordered by their immutable SQL SELECT ordinal. aggCols itself is an internal
// work list: classification may prepend group keys and append hidden
// HAVING/ORDER BY accumulators, so its slice order is never an output contract.
func aggregateColumnsInSelectOrder(aggCols []aggSelectCol) []int {
	indices := make([]int, 0, len(aggCols))
	for i := range aggCols {
		if aggCols[i].visible {
			indices = append(indices, i)
		}
	}
	sort.SliceStable(indices, func(i, j int) bool {
		return aggCols[indices[i]].selectOrdinal < aggCols[indices[j]].selectOrdinal
	})
	return indices
}

func visibleAggregateOutputColumns(aggCols []aggSelectCol, countStar bool, countStarAlias string) []aggSelectCol {
	if !countStar {
		return aggCols
	}
	return []aggSelectCol{{
		outName:       countStarAlias,
		selectOrdinal: 1,
		aggFunc:       "COUNT",
		aggArg:        "*",
		visible:       true,
	}}
}

// logicalAggregateCalls is the single aggregate-call construction authority
// shared by both SQL builders. A sole visible COUNT(*) is synthetic in parser
// state, but HAVING/ORDER BY may have harvested additional hidden aggCols. Keep
// native order [visible COUNT(*), hidden calls in parser order], suppressing a
// harvested duplicate COUNT(*) only; the public OutputSlots are built
// separately from visibleAggregateOutputColumns and therefore never expose a
// hidden call.
func logicalAggregateCalls(
	aggCols []aggSelectCol,
	countStar bool,
	strip func(string) string,
) ([]logical.AggregateCall, []int, bool) {
	calls := make([]logical.AggregateCall, 0, len(aggCols)+1)
	// callToAggCol stays PARALLEL to calls: every append below appends to both,
	// and every `continue` skips both. Letting them drift is the same class of
	// silent misbinding RFC-241 removed, so the two appends are deliberately
	// adjacent at each site rather than factored apart.
	callToAggCol := make([]int, 0, len(aggCols)+1)
	hasDistinct := false
	if countStar {
		calls = append(calls, logical.AggregateCall{Func: "COUNT", Operand: "*", Star: true})
		callToAggCol = append(callToAggCol, -1) // synthesized: no parsed column
	}
	for aggColIdx, ac := range aggCols {
		if ac.aggFunc == "" {
			continue
		}
		arg := ac.aggArg
		if arg == "" && ac.aggExpr != nil {
			arg = aggOperandCanonicalText(ac.aggExpr)
		}
		if arg == "" {
			arg = "*"
		}
		arg = strip(arg)
		call := logical.AggregateCall{
			Func:       strings.ToUpper(ac.aggFunc),
			Operand:    arg,
			Star:       arg == "*",
			Distinct:   ac.aggDistinct,
			BareColumn: ac.aggArg != "" && ac.aggExpr == nil,
			Qualified:  ac.aggArgQualified,
			// The SEGMENTS behind arg, reconciled downstream against the
			// rendering above (`strip` may have removed a qualifier prefix, in
			// which case the triple no longer spells what Operand carries).
			Bare:      ac.aggArgBare,
			Qualifier: ac.aggArgQualifier,
		}
		if countStar && call.Func == "COUNT" && call.Star && !call.Distinct {
			continue
		}
		hasDistinct = hasDistinct || call.Distinct
		calls = append(calls, call)
		callToAggCol = append(callToAggCol, aggColIdx)
	}
	return calls, callToAggCol, hasDistinct
}

func aggregateProjectionItem(ac aggSelectCol, strip func(string) string) (name, alias string, expr antlrgen.IExpressionContext) {
	switch {
	case ac.outExpr != nil && ac.aggFunc == "":
		name = canonicalTextOf(ac.outExpr)
		expr = ac.outExpr
		if ac.outName != "" && !strings.EqualFold(ac.outName, name) {
			alias = ac.outName
		}
	case ac.aggFunc != "":
		arg := ac.aggArg
		if arg == "" && ac.aggExpr != nil {
			arg = aggOperandCanonicalText(ac.aggExpr)
		}
		if arg == "" {
			arg = "*"
		}
		name = ac.aggFunc + "(" + strip(arg) + ")"
		if ac.outName != "" && !strings.EqualFold(ac.outName, name) {
			alias = ac.outName
		}
	case ac.groupCol != "":
		name = strip(ac.groupCol)
		if ac.outName != "" && !strings.EqualFold(ac.outName, ac.groupCol) {
			alias = ac.outName
		}
	}
	return name, alias, expr
}

// buildAggregateOutputSlots captures the public SELECT contract while the
// parser's structural key/call identities are still available. The aggregate's
// runtime ABI remains [keys..., calls...]; every direct visible item points to
// that ABI by ordinal, never by its possibly-colliding label.
func buildAggregateOutputSlots(keys []logical.GroupKey, aggCols []aggSelectCol, strip func(string) string) []logical.AggregateOutputSlot {
	callOrdinal := make(map[int]int, len(aggCols))
	nextCall := 0
	for i, ac := range aggCols {
		if ac.aggFunc == "" {
			continue
		}
		callOrdinal[i] = len(keys) + nextCall
		nextCall++
	}

	indices := aggregateColumnsInSelectOrder(aggCols)
	slots := make([]logical.AggregateOutputSlot, 0, len(indices))
	for _, colIdx := range indices {
		ac := aggCols[colIdx]
		native := invalidAggregateOutputOrdinal
		switch {
		case ac.outExpr != nil && ac.aggFunc == "":
			native = computedAggregateOutputOrdinal
		case ac.aggFunc != "":
			native = callOrdinal[colIdx]
		case ac.groupCol != "":
			groupName := strip(ac.groupCol)
			qualifierStripped := !strings.EqualFold(groupName, ac.groupCol)
			for i, key := range keys {
				same := false
				switch {
				case qualifierStripped:
					// The builder deliberately collapsed a same-source
					// qualifier on both the GroupKey and aggregate output, so
					// the two POST-STRIP renderings are the identity. It is
					// Display rather than Bare because a key that DESCENDS into
					// a struct keeps its segments through the strip
					// (`a.r.v.z` -> Display "R.V.Z", Bare "Z"), and Bare alone
					// would then name only the leaf — the slot went unbound and
					// the projection failed its native-ordinal contract. For a
					// flat stripped key Display and Bare are the same string,
					// so this is the previous rule at that shape.
					same = strings.EqualFold(groupName, key.Display)
				case ac.groupColBare != "" && key.Bare != "":
					same = ac.groupColQualified == key.Qualified &&
						strings.EqualFold(ac.groupColBare, key.Bare) &&
						(!ac.groupColQualified || strings.EqualFold(ac.groupColQualifier, key.Qualifier))
				default:
					// Expression-redirected GROUP BY items have no column
					// segments. Their parse-derived canonical rendering is
					// the remaining identity channel.
					same = strings.EqualFold(strings.TrimSpace(strip(ac.groupCol)), strings.TrimSpace(key.Display))
				}
				if same {
					// FIRST-MATCH, AND THE REASON IT IS SAFE IS NOT THE ONE THAT
					// USED TO BE WRITTEN HERE. The old justification was that
					// duplicate grouping keys are rejected 42702 at aggregate
					// build time by the NAME-based gate, so at most one key can
					// match. That gate does not fire under a join:
					// visitSelectGroupBy computes its alias-strip prefix only
					// when `len(fs.joins) == 0`, so `GROUP BY a.r.v.z, r.v.z`
					// passes it, and this loop then binds A.R.V.Z to key 0 and
					// R.V.Z to key 1 — one match each by a name predicate that
					// cannot see they are the same column.
					//
					// What makes it safe NOW is groupByOutputConstructionPullUp,
					// which refuses two SEMANTICALLY equal grouping keys with
					// 42702 — the check Java performs at LogicalOperator.java:454
					// through the asserting Expressions.pullUp. It depends on
					// neither the alias strip, the presence of a join, nor any
					// post-aggregate reference existing.
					//
					// IT RUNS AFTER THIS LOOP, NOT BEFORE, and saying otherwise
					// would misdescribe the only thing keeping this `break`
					// honest. This builder runs inside visitSelectGroupBy at
					// plan_visitor.go:546 (step 3); the guard runs at
					// plan_visitor.go:1023 (step 10, via
					// upgradeAggregateOperands), later in the SAME function. So a
					// conflatable pair DOES reach this loop and DOES get bound to
					// slots 0 and 1 — measured — and what makes that harmless is
					// that the guard then fails the statement and the whole plan
					// is discarded. The wrong binding is built and thrown away; it
					// is never executed.
					//
					// ARMING CONDITION, stated so it is not already satisfied the
					// day it is written: this `break` must become a
					// collect-and-raise if the construction guard is REMOVED, made
					// conditional, or moved off this path — not if it is "moved
					// after slot construction", which is where it already is.
					native = i
					break
				}
			}
		}
		slots = append(slots, logical.AggregateOutputSlot{
			SelectOrdinal: ac.selectOrdinal,
			NativeOrdinal: native,
		})
	}
	return slots
}

// buildLogicalPlanForQuery is the outer-most builder entry point.
// It handles WITH (CTE) wrapping, then delegates the main query
// body to buildLogicalPlanForQueryBody. Each CTE in the WITH list
// wraps the result in a LogicalCTE; the outermost CTE ends up at
// the root.
func buildLogicalPlanForQuery(q antlrgen.IQueryContext) logical.LogicalOperator {
	if q == nil {
		return nil
	}
	main := buildLogicalPlanForQueryBody(q.QueryExpressionBody())
	if main == nil {
		return nil
	}
	ctesCtx := q.Ctes()
	if ctesCtx == nil {
		return main
	}
	recursive := ctesCtx.RECURSIVE() != nil
	// Wrap each named CTE around the accumulated main. Reverse
	// iteration so the first-declared CTE ends up at the root (read
	// top-down in Explain output, matching the SQL text order).
	ctes := ctesCtx.AllNamedQuery()
	for i := len(ctes) - 1; i >= 0; i-- {
		nq := ctes[i]
		name := functions.FullIdToName(nq.GetName())
		var body logical.LogicalOperator
		if inner := nq.Query(); inner != nil {
			body = buildLogicalPlanForQueryBody(inner.QueryExpressionBody())
		}
		if body == nil {
			// CTE body out of builder scope — bail rather than emit
			// a partial tree.
			return nil
		}
		main = logical.NewCTE(name, body, main, recursive)
	}
	return main
}

// buildLogicalPlanForQueryBody is the general entry point that
// handles both simple SELECT and UNION shapes. It dispatches on the
// query-body shape: QueryTermDefaultContext → simple SELECT;
// SetQueryContext → UNION (N-ary via nested SetQuery). Called from the
// SELECT ExplainFn.
func buildLogicalPlanForQueryBody(body antlrgen.IQueryExpressionBodyContext) logical.LogicalOperator {
	if body == nil {
		return nil
	}
	switch b := body.(type) {
	case *antlrgen.QueryTermDefaultContext:
		simpleTable, ok := b.QueryTerm().(*antlrgen.SimpleTableContext)
		if !ok {
			return nil
		}
		sq, err := extractFromSimpleTable(simpleTable)
		if err != nil {
			return nil
		}
		return buildLogicalPlanForSelect(sq)
	case *antlrgen.SetQueryContext:
		return buildLogicalPlanForUnion(b)
	}
	return nil
}

// buildLogicalPlanForUnion walks a SetQueryContext (UNION ALL or
// UNION DISTINCT) recursively, producing a LogicalUnion whose
// Inputs hold the flattened left-and-right subtrees. The grammar
// nests SetQuery(SetQuery(A, B), C) for A UNION B UNION C; we
// flatten into [A, B, C] when all levels share the same quantifier
// — makes the Explain output read left-to-right. Mixed quantifiers
// keep the original nesting.
//
// Trailing ORDER BY: the ANTLR grammar greedily attaches a trailing
// ORDER BY to the rightmost SimpleTable, but SQL standard says it
// applies to the whole UNION result. Strip it from the right branch
// and wrap the union in a LogicalSort.
func buildLogicalPlanForUnion(setQ *antlrgen.SetQueryContext) logical.LogicalOperator {
	if setQ == nil {
		return nil
	}
	if setQ.ALL() == nil {
		return nil
	}
	left := buildLogicalPlanForQueryBody(setQ.GetLeft())

	// Lift ORDER BY from the right branch before building it.
	var liftedOrder []orderByClause
	var right logical.LogicalOperator
	if rb, ok := setQ.GetRight().(*antlrgen.QueryTermDefaultContext); ok {
		if simpleTable, ok := rb.QueryTerm().(*antlrgen.SimpleTableContext); ok {
			if sq, err := extractFromSimpleTable(simpleTable); err == nil {
				liftedOrder = sq.orderBy
				sq.orderBy = nil
				// A trailing LIMIT on the rightmost branch applies to the
				// whole union, not the right branch alone — drop it here so
				// extractFromSimpleTable's newly-populated sq.limit isn't
				// mis-applied to the right branch (RFC-128). This md==nil
				// path is Explain-only and does not page, so the lift is a
				// no-op; matching the prior behavior (sq.limit was always -1).
				sq.limit = -1
				sq.offset = 0
				right = buildLogicalPlanForSelect(sq)
			}
		}
	}
	if right == nil && liftedOrder == nil {
		right = buildLogicalPlanForQueryBody(setQ.GetRight())
	}
	if left == nil || right == nil {
		return nil
	}
	inputs := []logical.LogicalOperator{left, right}
	if innerUnion, ok := left.(*logical.LogicalUnion); ok && !innerUnion.Distinct {
		inputs = append(append([]logical.LogicalOperator(nil), innerUnion.Inputs...), right)
	}
	return logical.NewUnion(inputs, false)
}

// Phase 3 logical-plan builder — narrow-scope seed.
//
// Converts a parsed `*selectQuery` (the internal struct the embedded
// engine builds today) into a `logical.LogicalOperator` tree. This is
// the first bridge between the parse tree and the Phase 3 skeleton:
// `Plan.Explain()` on the naive Generator now returns a real plan
// tree instead of canonical SQL text, for the query shapes this
// builder recognises.
//
// **Scope.** SELECT (single-table / JOIN / aggregate+GROUP BY+HAVING
// / derived table / UNION) and all DML (INSERT VALUES / INSERT SELECT
// / UPDATE / DELETE). Returns nil for CTE (WITH …) and SELECT
// without FROM. Nil shapes fall through to the canonical-SQL explain
// (the pre-existing Phase 1a placeholder).
//
// Predicates + expressions are carried as canonical source text for
// now (LogicalFilter.PredicateText, LogicalProject.Projections).
// RFC-021 Phase 2 replaces those with real `Value` / `QueryPredicate`
// nodes from the cascades package — at which point the builder grows
// a translation pass from antlrgen.IExpressionContext to Value /
// QueryPredicate.
//
// Output tree shape (innermost to outermost):
//
//	LogicalScan (or derived subtree)
//	  → LogicalFilter    (if WHERE)
//	    → LogicalJoin*   (if joins; chained left-to-right)
//	      → LogicalAggregate (if GROUP BY / aggregates / HAVING)
//	        → LogicalSort    (if ORDER BY)
//	          → LogicalLimit (if LIMIT or OFFSET)
//	            → LogicalProject (unless SELECT *)

// buildLogicalPlanForSelect returns a LogicalOperator tree for the
// parsed selectQuery, or nil when the shape is out of current scope
// (SELECT without FROM; derived-table builds that recursively fail).
func buildLogicalPlanForSelect(sq *selectQuery) logical.LogicalOperator {
	if sq == nil {
		return nil
	}
	if sq.tableName == "" && sq.derivedQuery == nil && sq.inlineValues == nil {
		// SELECT without FROM — emit LogicalValues (single-row
		// constant projection). Carries the projection expression
		// text per column (future: real Value nodes per RFC-021
		// Phase 2).
		rows := make([]string, len(sq.projCols))
		aliases := make([]string, len(sq.projCols))
		for i, col := range sq.projCols {
			expr := col.name
			if sq.projExprs != nil && i < len(sq.projExprs) && sq.projExprs[i] != nil {
				expr = strings.TrimSpace(canonicalTextOf(sq.projExprs[i]))
			}
			rows[i] = expr
			if sq.projAliases != nil && i < len(sq.projAliases) {
				aliases[i] = sq.projAliases[i]
			}
		}
		return logical.NewValues(rows, aliases)
	}

	// Build the FROM-source subtree. Either a plain table scan or a
	// derived table (subquery in FROM). For derived tables we
	// recursively build the inner logical plan and wrap it as a CTE
	// so that the user-supplied alias (e.g. "sq1") is preserved in
	// the logical tree. The CTE wrapper ensures that sourceAlias
	// (used by the NLJ rule to set outerAlias/innerAlias on the
	// physical plan) returns the derived-table alias rather than the
	// underlying table name. Without this, column qualification in
	// mergeRows uses the wrong qualifier and projections like
	// "sq1.x" resolve to NULL.
	var op logical.LogicalOperator
	if sq.inlineValues != nil {
		var err error
		op, err = buildInlineValuesLogical(sq.inlineValues, sq.tableAlias, "", nil)
		if err != nil {
			return nil
		}
	} else if sq.derivedQuery != nil {
		// The catalog-aware inner plan when one was pre-built, exactly as the
		// join legs below do. The recursive text-only build is the fallback for
		// a caller that never went through the catalog path; taking it while a
		// resolved plan exists loses every ProjectedValue in the body.
		innerOp := sq.catalogAwareInnerPlan
		if innerOp == nil {
			body := sq.derivedQuery.QueryExpressionBody()
			if termDefault, ok := body.(*antlrgen.QueryTermDefaultContext); ok {
				if simpleTable, ok := termDefault.QueryTerm().(*antlrgen.SimpleTableContext); ok {
					if inner, err := extractFromSimpleTable(simpleTable); err == nil {
						innerOp = buildLogicalPlanForSelect(inner)
					}
				}
			}
		}
		if innerOp == nil {
			// Derived query is out of the inner builder's scope — bail
			// rather than emit a misleading partial tree.
			return nil
		}
		// Wrap as CTE so the alias surfaces in the logical tree — for
		// the no-joins case too: the wrapper is the tree's one alias
		// carrier for derived tables, and a bare innerOp loses it
		// (sourceAlias walks to the BASE table; a correlated EXISTS on
		// the derived alias then binds the outer row under the wrong
		// name). The qualified-star rebuild re-enters THIS builder, so
		// dropping the wrapper here silently undid the visitor path's
		// alias fidelity.
		op = logical.NewCTE(sq.tableName, innerOp,
			logical.NewScan(sq.tableName, ""), false)
	} else {
		op = logical.NewScan(sq.tableName, sq.tableAlias)
	}

	// JOINs chain left-to-right from the primary scan. Each join wraps
	// the current op as Left and scans the joined table as Right.
	// Produces `InnerJoin(on ...) → LeftScan → RightScan` nested as
	// the logical operator tree expects.
	for i, j := range sq.joins {
		var right logical.LogicalOperator
		if j.inlineValues != nil {
			var err error
			right, err = buildInlineValuesLogical(j.inlineValues, j.alias, j.bindingID, nil)
			if err != nil {
				return nil
			}
		} else if j.catalogAwareInnerPlan != nil {
			// catalogAwareInnerPlan is the inner plan built through the
			// catalog-aware path. Wrap it in a CTE so the join alias
			// is preserved (same logic as the primary source above).
			if j.alias != "" {
				cte := logical.NewCTE(j.alias, j.catalogAwareInnerPlan,
					logical.NewScan(j.alias, ""), false)
				cte.Binding = j.bindingID
				right = cte
			} else {
				right = j.catalogAwareInnerPlan
			}
		} else if j.derivedQuery != nil {
			var innerRight logical.LogicalOperator
			body := j.derivedQuery.QueryExpressionBody()
			if termDefault, ok := body.(*antlrgen.QueryTermDefaultContext); ok {
				if simpleTable, ok := termDefault.QueryTerm().(*antlrgen.SimpleTableContext); ok {
					if inner, err := extractFromSimpleTable(simpleTable); err == nil {
						innerRight = buildLogicalPlanForSelect(inner)
					}
				}
			}
			if innerRight == nil {
				return nil
			}
			// Wrap as CTE so the alias surfaces in sourceAlias.
			if j.alias != "" {
				cte := logical.NewCTE(j.alias, innerRight,
					logical.NewScan(j.alias, ""), false)
				cte.Binding = j.bindingID
				right = cte
			} else {
				right = innerRight
			}
		} else if u := lateralUnnestCandidate(j, visibleFromAliases(sq.tableName, sq.tableAlias, sq.joins[:i], nil), nil); u != nil {
			// A comma source that may be a lateral array unnest
			// (`FROM t, t.arr AS x [AT ord]`); the translator classifies it
			// against the scope. This metadata-less path cannot run Java's
			// table-first check (nil resolver); a schema-qualified table
			// mis-classified here is demoted to a scan by
			// demoteSchemaQualifiedUnnest once metadata is in scope. RFC-142.
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

	if sq.whereExpr != nil {
		// Carry the canonical WHERE text — renders in Explain as
		// `Filter(<text>)`. Future: translate to QueryPredicate tree.
		op = logical.NewFilter(op, canonicalTextOf(sq.whereExpr))
	}

	return buildSelectShell(op, sq, "")
}

// buildPostAggregateProjection builds the LogicalProject that sits on top of a
// LogicalAggregate when the SELECT list mixes aggregate outputs with computed
// post-aggregate expressions (e.g. COUNT(*)+1 AS x) and/or group columns. It
// lists ALL visible columns in SELECT-list order: aggregate outputs and group
// columns as plain column references, computed expressions as expressions to
// evaluate, and carries each column's output alias.
//
// This is the SINGLE source of post-aggregate projection truth shared by both
// SELECT builders — visitSelectGroupBy (standalone SELECTs) and buildSelectShell
// (the legacy UNION-branch / derived-table path). Keeping it shared prevents the
// alias-handling divergence RFC-079 fixed: the legacy path used to build the
// projection with nil aliases, so a UNION branch's `COUNT(*)+1 AS x` lost its `x`
// alias and a by-name read of the union output returned NULL.
//
// Returns (nil, nil) when no visible column produces a projection. strip removes
// the caller's column qualifier (derived-table / table-alias prefix); it differs
// between the two callers, so it is passed in. The returned antlr slice is the
// per-column post-aggregate expression contexts (nil for plain references), which
// the caller stores as postAggExprs for Value resolution.
func buildPostAggregateProjection(op logical.LogicalOperator, aggCols []aggSelectCol, strip func(string) string) (*logical.LogicalProject, []antlrgen.IExpressionContext) {
	var allProj []string
	var allAliases []string
	var allAntlr []antlrgen.IExpressionContext
	var outputOrdinals []int
	hasAlias := false
	var slots []logical.AggregateOutputSlot
	if agg, ok := op.(*logical.LogicalAggregate); ok {
		slots = agg.OutputSlots
	}
	ordered := aggregateColumnsInSelectOrder(aggCols)
	for i, colIdx := range ordered {
		ac := aggCols[colIdx]
		name, alias, expr := aggregateProjectionItem(ac, strip)
		if name == "" {
			continue
		}
		if alias == "" && ac.aggFunc != "" && strings.Contains(name, ".") {
			// The projection Value is an ordinal-bound FieldValue for the
			// private aggregate slot. A dotted SQL rendering needs a machinery
			// alias so metadata treats the whole expression as its label,
			// never as a qualified base column (`MAX(E.SALARY)` must not be
			// truncated to `SALARY)`). Non-dotted renderings already survive
			// FieldValue label derivation verbatim and need no alias.
			alias = name
		}
		allProj = append(allProj, name)
		allAntlr = append(allAntlr, expr)
		allAliases = append(allAliases, alias)
		if alias != "" {
			hasAlias = true
		}
		ordinal := invalidAggregateOutputOrdinal
		if i < len(slots) {
			ordinal = slots[i].NativeOrdinal
		}
		outputOrdinals = append(outputOrdinals, ordinal)
	}
	if len(allProj) == 0 {
		return nil, nil
	}
	var aliases []string
	if hasAlias {
		aliases = allAliases
	}
	proj := logical.NewProject(op, allProj, aliases)
	// Mark outExpr slots as computed (need Value resolution). Aggregate output
	// and groupCol slots are column references to the aggregate's output — NOT
	// computed.
	computed := make([]bool, len(allProj))
	for i, e := range allAntlr {
		computed[i] = e != nil
	}
	proj.IsComputed = computed
	proj.AggregateOutputOrdinals = outputOrdinals
	return proj, allAntlr
}

// buildSelectShell builds the Aggregate/Sort/Limit/Projection/Distinct
// shell on top of an already-built FROM source. Shared between
// buildLogicalPlanForSelect (plain tables) and the derived-table path.
// stripPrefix, when non-empty, is removed from column names (derived
// table qualifier like "X.").
func buildSelectShell(op logical.LogicalOperator, sq *selectQuery, stripPrefix string) logical.LogicalOperator {
	strip := func(s string) string {
		if stripPrefix != "" && strings.HasPrefix(strings.ToUpper(s), stripPrefix) {
			return s[len(stripPrefix):]
		}
		return s
	}

	// Aggregate / GROUP BY. Three shapes collapse here:
	//   - Bare COUNT(*): no group keys, single COUNT(*) aggregate.
	//   - GROUP BY without aggregates: just the group keys.
	//   - Mixed: aggCols carries both group-col and agg-function
	//     entries with outName.
	if sq.countStar || len(sq.aggCols) > 0 || len(sq.groupBy) > 0 {
		keys := logicalGroupKeys(sq.groupBy)
		for i := range keys {
			stripped := strip(keys[i].Display)
			if stripped != keys[i].Display {
				keys[i] = stripGroupKeyLeadingSegment(keys[i], stripped)
			}
		}
		aggCalls, callToAggCol, hasDistinct := logicalAggregateCalls(sq.aggCols, sq.countStar, strip)
		outputAggCols := visibleAggregateOutputColumns(sq.aggCols, sq.countStar, sq.countStarAlias)
		aggAliases := make([]string, len(aggCalls))
		aggOp := logical.NewAggregate(op, keys, aggCalls, aggAliases, sq.havingExpr != nil)
		aggOp.CallToAggCol = callToAggCol
		aggOp.CallToAggColLen = len(sq.aggCols)
		aggOp.HasDistinctAggregate = hasDistinct
		aggOp.OutputSlots = buildAggregateOutputSlots(keys, outputAggCols, strip)
		op = aggOp

		// Every SQL aggregate has one public output boundary. It stays above
		// ORDER BY so hidden accumulators remain available on the private
		// canonical [keys..., calls...] row.
		if proj, antlr := buildPostAggregateProjection(op, outputAggCols, strip); proj != nil {
			sq.postSortStripProj = append([]string(nil), proj.Projections...)
			sq.postSortStripAliases = append([]string(nil), proj.Aliases...)
			sq.postSortAggregateOutputOrdinals = append([]int(nil), proj.AggregateOutputOrdinals...)
			sq.postSortIsComputed = append([]bool(nil), proj.IsComputed...)
			sq.postAggExprs = antlr
		}
	}

	if len(sq.orderBy) > 0 && len(sq.postSortStripProj) > 0 {
		// The sort sits BELOW the deferred reshaping projection, over the
		// aggregate's internal layout: rebase keys naming SELECT aliases
		// (alias first — SQL resolves output names before source columns)
		// and positional keys (visible slots differ from internal ones) to
		// the underlying expressions.
		for i := range sq.orderBy {
			ob := &sq.orderBy[i]
			if ob.pos >= 1 && ob.pos <= len(sq.postSortStripProj) {
				ob.colName = sq.postSortStripProj[ob.pos-1]
				// The rebased name is internal projection text — the
				// original reference's segments no longer describe it
				// (stale segments silently mis-resolve against a
				// same-spelled source column).
				ob.bare, ob.qualifier, ob.qualified, ob.segs = ob.colName, "", false, nil
				continue
			}
			// Output aliases bind BARE one-segment identifiers only: a
			// qualified key (`d.x`) or an aggregate/computed key
			// (`SUM(s.score)`) names source data, never the SELECT alias —
			// text matching rebased both onto same-spelled aliases and
			// silently mis-sorted. The parse tree decides (bareRef), not
			// the name text.
			if !ob.bareRef {
				continue
			}
			for j, al := range sq.postSortStripAliases {
				if al != "" && strings.EqualFold(al, ob.colName) && j < len(sq.postSortStripProj) {
					ob.colName = sq.postSortStripProj[j]
					// Same rule as the positional rebase above: internal
					// text, segments cleared to the rebased bare.
					ob.bare, ob.qualifier, ob.qualified, ob.segs = ob.colName, "", false, nil
					break
				}
			}
		}
	}
	if len(sq.orderBy) > 0 {
		keys := make([]logical.SortKey, 0, len(sq.orderBy))
		for _, ob := range sq.orderBy {
			dir := logical.SortAsc
			if !ob.ascending {
				dir = logical.SortDesc
			}
			expr := strip(ob.colName)
			if expr == "" && ob.rawExpr != nil {
				expr = canonicalTextOf(ob.rawExpr)
			}
			// A POSITIONAL key's resolved name is ALIAS-preferred
			// (resolveSelectListPosition), but this sort sits BELOW the
			// projection: the alias does not exist there and may collide
			// with a same-named SOURCE column (`SELECT id AS score …
			// ORDER BY 1` must sort by id, never the SCORE column).
			// Rebase to the item's UNDERLYING text; Pos still rides along
			// for output-slot baking where the input IS a projection.
			if ob.pos >= 1 && ob.pos <= len(sq.projCols) && sq.projCols[ob.pos-1].name != "" {
				expr = strip(sq.projCols[ob.pos-1].name)
				// Rebased to the underlying projection text — segments
				// follow the same internal-name rule.
				ob.bare, ob.qualifier, ob.qualified, ob.segs = expr, "", false, nil
			}
			nullsFirst := ob.ascending
			if ob.nullsFirst != nil {
				nullsFirst = *ob.nullsFirst
			}
			// Pos is pure INFORMATION (the ordinal into THIS select's
			// list), never a bake directive: upgradeSortKeyValues resolves
			// it into the OUTER projection's typed item Value (clearing
			// Pos), and the translator bakes a surviving Pos only into a
			// select-list-carrying input (the aggregate reshaping
			// projection or a union) — never a derived source's slots.
			sk := logical.SortKey{
				Expr:       expr,
				Dir:        dir,
				NullsFirst: nullsFirst,
				Pos:        ob.pos,
				BareRef:    ob.bareRef,
				Bare:       ob.bare,
				Qualifier:  ob.qualifier,
				Qualified:  ob.qualified,
				Segs:       append([]string(nil), ob.segs...),
			}
			if ob.pos >= 1 && ob.pos <= len(sq.postSortAggregateOutputOrdinals) &&
				sq.postSortAggregateOutputOrdinals[ob.pos-1] >= 0 {
				sk.AggregateOutputOrdinal = sq.postSortAggregateOutputOrdinals[ob.pos-1]
				sk.HasAggregateOutputOrdinal = true
			}
			if ob.bare != "" && expr != ob.colName {
				// A COLUMN key stripped/rebased to an internal name — bare
				// from here on (the group-key strip rule). Expression keys
				// keep zero segments: their Expr is a rendering, never a
				// reference.
				sk.Bare, sk.Qualifier, sk.Qualified, sk.Segs = expr, "", false, nil
			}
			keys = append(keys, sk)
		}
		op = logical.NewSort(op, keys)
	}

	if len(sq.postSortStripProj) > 0 {
		proj := logical.NewProject(op, sq.postSortStripProj, sq.postSortStripAliases)
		proj.AggregateOutputOrdinals = append([]int(nil), sq.postSortAggregateOutputOrdinals...)
		proj.IsComputed = append([]bool(nil), sq.postSortIsComputed...)
		op = proj
	}

	// Projection: skip when the projection is SELECT * (projCols is
	// nil per the selectQuery doc).
	if len(sq.projCols) > 0 {
		projs := make([]string, len(sq.projCols))
		aliases := make([]string, len(sq.projCols))
		computed := make([]bool, len(sq.projCols))
		refs := make([]logical.ColumnRef, len(sq.projCols))
		for i, col := range sq.projCols {
			projs[i] = strip(col.name)
			refs[i] = projColRef(col, projs[i])
			if sq.projExprs != nil && i < len(sq.projExprs) && sq.projExprs[i] != nil {
				projs[i] = strings.TrimSpace(canonicalTextOf(sq.projExprs[i]))
				computed[i] = true
				refs[i] = logical.ColumnRef{}
			}
			if sq.projAliases != nil && i < len(sq.projAliases) {
				aliases[i] = sq.projAliases[i]
			}
		}
		proj := logical.NewProject(op, projs, aliases)
		proj.IsComputed = computed
		proj.ProjectionRefs = refs
		op = proj
	}

	if sq.distinct {
		op = logical.NewDistinct(op)
	}

	// LIMIT is the OUTERMOST operator — applied LAST, after projection and
	// DISTINCT, per SQL semantics (RFC-128). sq.limit < 0 means "no limit";
	// offset alone (LIMIT -1 OFFSET N) renders via LogicalLimit's
	// negative-limit Offset(N) branch.
	if sq.limit >= 0 || sq.offset > 0 {
		op = logical.NewLimit(op, sq.limit, sq.offset)
	}

	return op
}

// aggOperandCanonicalText renders an aggregate's non-bare-column argument into
// the operand segment of that aggregate's public output-column name
// (`SUM(<here>)`). It is the SOLE mint for that segment.
//
// It exists because canonicalTextOf is the wrong tool for a NAME. That helper
// returns the raw source slice, which carries the user's spelling — quotes,
// case and whitespace — and the two repairs applied downstream then destroyed
// the one part of the spelling that is load-bearing. `SUM("qty" * "price")`
// against a table whose columns really are named `qty` and `price` labelled the
// column `SUM("QTY"*"PRICE")`: quotes around names that contain none, and the
// case of both columns folded away. The same query's GROUP BY key labelled
// `qty`, verbatim, in the same row — two naming authorities over one table
// disagreeing about what its columns are called.
//
// So the operand is rendered ONCE, here, at the parse boundary. Every token is
// upper-cased — which is exactly what the downstream fold did, and is why an
// unquoted operand's label does not move — EXCEPT a delimited identifier, which
// contributes its inner text verbatim and without its quotes, because that text
// IS the name. Whitespace is dropped, matching the space-strip this replaces.
//
// Both properties matter and only together: upper-casing alone would keep the
// quotes, and stripping the quotes alone would still fold `"qty"` to `QTY`.
func aggOperandCanonicalText(ctx antlr.Tree) string {
	if ctx == nil {
		return ""
	}
	var b strings.Builder
	var walk func(t antlr.Tree)
	walk = func(t antlr.Tree) {
		if tn, ok := t.(antlr.TerminalNode); ok {
			sym := tn.GetSymbol()
			if sym == nil || sym.GetTokenType() == antlr.TokenEOF {
				return
			}
			switch sym.GetTokenType() {
			case antlrgen.RelationalParserDOUBLE_QUOTE_ID:
				// NormalizeIdentifier on a delimited token is the strip; it
				// cannot fold, because the token still carries its quotes.
				b.WriteString(functions.NormalizeIdentifier(sym.GetText()))
				return
			case antlrgen.RelationalParserSTRING_LITERAL:
				// A STRING LITERAL IS DATA, NOT A NAME, and folding it here
				// collided two aggregates that compute different things:
				// `COUNT(CASE WHEN s='x' …)` and `COUNT(CASE WHEN s='X' …)`
				// both rendered COUNT(CASEWHENS='X'THEN1END), and the executor
				// keys its group slots BY THAT NAME (aggResultName), so one
				// wrote over the other. That collision is an old known hazard
				// at the previous naming site; this mint reintroduced it at the
				// new one, which is what an "upper-case everything else" rule
				// buys if `everything else` is never enumerated.
				b.WriteString(sym.GetText())
				return
			}
			b.WriteString(strings.ToUpper(sym.GetText()))
			return
		}
		for i := 0; i < t.GetChildCount(); i++ {
			walk(t.GetChild(i))
		}
	}
	walk(ctx)
	return b.String()
}

// canonicalTextOf renders an antlr context as source text. When
// possible (ctx is a ParserRuleContext with resolvable token
// positions), returns the ORIGINAL source text with whitespace
// intact — `WHERE id > 5` stays as `id > 5`, not `id>5`. Falls back
// to `GetText()` (token concatenation, whitespace stripped) when
// the context doesn't expose its token range.
//
// NOT for anything that becomes a user-visible NAME — see
// aggOperandCanonicalText for why the raw slice is the wrong input to a label.
//
// Until Phase 4.0 ports real QueryPredicates this is the surface
// LogicalFilter.PredicateText etc. carry into the Explain tree.
func canonicalTextOf(ctx any) string {
	if ctx == nil {
		return ""
	}
	// Fast path: parser rule context with start/stop tokens.
	if prc, ok := ctx.(antlr.ParserRuleContext); ok {
		start := prc.GetStart()
		stop := prc.GetStop()
		if start != nil && stop != nil && start.GetInputStream() != nil {
			return start.GetInputStream().GetTextFromInterval(
				antlr.NewInterval(start.GetStart(), stop.GetStop()),
			)
		}
	}
	// Fallback: GetText() concatenates tokens (no whitespace).
	if gt, ok := ctx.(interface{ GetText() string }); ok {
		return gt.GetText()
	}
	return ""
}

// buildLogicalPlanForDelete returns a LogicalDelete-rooted tree for
// a DELETE statement. Input is the parse-tree context; output wraps
// a LogicalScan(table) with an optional LogicalFilter (the WHERE).
// Returns nil on a malformed parse (missing table).
func buildLogicalPlanForDelete(del antlrgen.IDeleteStatementContext) logical.LogicalOperator {
	if del == nil || del.TableName() == nil {
		return nil
	}
	tableName := functions.FullIdToName(del.TableName().FullId())
	var scan logical.LogicalOperator = logical.NewScan(tableName, "")
	if w := del.WhereExpr(); w != nil {
		scan = logical.NewFilter(scan, canonicalTextOf(w))
	}
	return logical.NewDelete(tableName, scan)
}

// buildLogicalPlanForInsert returns a LogicalInsert-rooted tree for
// an INSERT statement. Two INSERT shapes:
//
//  1. `INSERT INTO t VALUES (…)` — Source is nil (values are not
//     represented as operators today). The rendered plan is just
//     `Insert(t[(col, col, …)])`.
//  2. `INSERT INTO t SELECT …` — Source is the nested SELECT's
//     logical plan when buildLogicalPlanForSelect succeeds.
//     Complex SELECT shapes (JOIN / CTE / …) cause the Source to
//     be nil, but the Insert node itself still renders.
//
// Returns nil on a malformed parse.
func buildLogicalPlanForInsert(ins antlrgen.IInsertStatementContext) logical.LogicalOperator {
	if ins == nil || ins.TableName() == nil {
		return nil
	}
	tableName := functions.FullIdToName(ins.TableName().FullId())

	var cols []string
	if colCtx := ins.UidListWithNestingsInParens(); colCtx != nil {
		if ul := colCtx.UidListWithNestings(); ul != nil {
			for _, uw := range ul.AllUidWithNestings() {
				if uw == nil || uw.Uid() == nil {
					continue
				}
				cols = append(cols, functions.NormalizeIdentifier(uw.Uid().GetText()))
			}
		}
	}

	// INSERT … SELECT: try to build the inner SELECT's logical plan.
	// If the SELECT shape is out of the builder's scope the inner
	// plan is nil and the Insert renders without a Source subtree
	// (same behaviour as VALUES).
	var source logical.LogicalOperator
	if selCtx, ok := ins.InsertStatementValue().(*antlrgen.InsertStatementValueSelectContext); ok {
		if body := selCtx.QueryExpressionBody(); body != nil {
			if termDefault, ok := body.(*antlrgen.QueryTermDefaultContext); ok {
				if simpleTable, ok := termDefault.QueryTerm().(*antlrgen.SimpleTableContext); ok {
					if sq, err := extractFromSimpleTable(simpleTable); err == nil {
						source = buildLogicalPlanForSelect(sq)
					}
				}
			}
		}
	}

	return logical.NewInsert(tableName, cols, source)
}

// buildLogicalPlanForUpdate returns a LogicalUpdate-rooted tree for
// an UPDATE statement. SET assignments render as (col, expr-text)
// pairs; WHERE wraps the scan in a LogicalFilter. Returns nil on a
// malformed parse.
func buildLogicalPlanForUpdate(upd antlrgen.IUpdateStatementContext) logical.LogicalOperator {
	if upd == nil || upd.TableName() == nil {
		return nil
	}
	tableName := functions.FullIdToName(upd.TableName().FullId())
	var scan logical.LogicalOperator = logical.NewScan(tableName, "")
	if w := upd.WhereExpr(); w != nil {
		scan = logical.NewFilter(scan, canonicalTextOf(w))
	}
	var sets []logical.Assignment
	for _, el := range upd.AllUpdatedElement() {
		if el == nil || el.FullColumnName() == nil || el.Expression() == nil {
			continue
		}
		// UPDATE SET uses bare col names at the logical level — the LAST
		// FullId segment per the parse tree, never a dot split of the
		// rendering (a delimited identifier may contain a literal dot).
		uids := el.FullColumnName().FullId().AllUid()
		col := functions.NormalizeIdentifier(uids[len(uids)-1].GetText())
		sets = append(sets, logical.Assignment{
			Column: col,
			Expr:   strings.TrimSpace(canonicalTextOf(el.Expression())),
		})
	}
	return logical.NewUpdate(tableName, sets, scan)
}
