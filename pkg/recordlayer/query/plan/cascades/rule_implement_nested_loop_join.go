package cascades

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementNestedLoopJoinRule implements a SelectExpression with
// exactly 2 quantifiers (a binary join) as a physical nested-loop join
// plan. The left (first) quantifier becomes the outer and the right
// (second) becomes the inner.
//
//	Select(predicates, [Q_left, Q_right])
//	  → NestedLoopJoin(outer=physical(Q_left), inner=physical(Q_right), predicates)
//
// This is the simplest and most general join implementation — it works
// for all join shapes without requiring sorted input or hash tables.
// Cost model: O(N_outer × N_inner) with predicate filtering.
//
// Mirrors Java's `ImplementNestedLoopJoinRule`.
type ImplementNestedLoopJoinRule struct {
	matcher matching.BindingMatcher
}

func NewImplementNestedLoopJoinRule() *ImplementNestedLoopJoinRule {
	return &ImplementNestedLoopJoinRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("select_for_nlj"),
	}
}

func (r *ImplementNestedLoopJoinRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementNestedLoopJoinRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)

	quants := sel.GetQuantifiers()

	// 3 quantifiers: 2 ForEach + 1 Existential = join with EXISTS filter.
	// Build the inner join first, then wrap with the EXISTS semi-join.
	if len(quants) == 3 &&
		quants[0].Kind() == expressions.QuantifierForEach &&
		quants[1].Kind() == expressions.QuantifierForEach &&
		quants[2].Kind() == expressions.QuantifierExistential {
		r.implementJoinWithExistential(call, sel, quants)
		return
	}

	if len(quants) != 2 {
		return
	}

	// EXISTS subquery: when the right quantifier is existential, wrap
	// the inner in FirstOrDefault and use a semi-join (EXISTS) plan
	// shape. The ExistentialValuePredicate in the predicate list
	// evaluates to TRUE when FirstOrDefault returns a non-null row.
	if quants[1].Kind() == expressions.QuantifierExistential {
		r.implementExistentialSelect(call, sel, quants)
		return
	}

	leftRef := quants[0].GetRangesOver()
	rightRef := quants[1].GetRangesOver()
	if leftRef == nil || rightRef == nil {
		return
	}

	// An UNCORRELATED Explode leg is the IN-list shape (col IN (v1,v2,…) →
	// SelectExpression with an Explode over a constant list); that is owned by
	// ImplementInJoinRule, not the NLJ rule — bail. But a CORRELATED Explode (a
	// lateral array UNNEST, `FROM t, t.arr AS x` → Explode of FieldValue{arr}
	// over the outer QOV) IS a correlated FlatMap: let it fall through to the
	// rightDepsLeft/leftDepsRight FlatMap path below, which builds
	// RecordQueryFlatMapPlan(outer, explode, …, resultValue, false) — the
	// non-existential, no-FirstOrDefault path (RFC-142). The guard fires only
	// when an Explode leg is not correlated to the OTHER leg.
	if le := getExplodeExpression(leftRef); le != nil && !referenceIsCorrelatedTo(leftRef, quants[1].GetAlias()) {
		return
	}
	if re := getExplodeExpression(rightRef); re != nil && !referenceIsCorrelatedTo(rightRef, quants[0].GetAlias()) {
		return
	}

	// Stats-aware child selection: with real cardinalities the cheaper join
	// order (drive from the smaller side) wins; under default stats every
	// table is LeafScanCardinality and selection ties to FROM-order (RFC-041).
	costModel := call.CostModel()
	// Select join children through the NIL-SAFE winner path (skips nil-inner Fetch
	// SHELLS — the RFC-070 extraction template `Fetch(nil, …)` whose real inner
	// lives in the wrapper quantifier and is resolved only via WithChildren). The
	// NLJ embeds the child's plan DIRECTLY (GetRecordQueryPlan, never WithChildren),
	// so a nil-inner shell selected here renders `Fetch(<nil>)` → 0 rows. Every
	// "best physical candidate" site must apply this guard (the wrapper's contract,
	// physical_fetch_from_partial_record_wrapper.go); the two sibling selectors
	// (getWinnerForOrdering, findBestValidPhysicalExpr) do — this one regressed.
	// (RFC-150 B1a: the join-leg 0-row bug `... AND t.a>1 AND t.fk=o.id AND u.x=t.x`.)
	leftExpr := findBestValidPhysicalExpr(leftRef, costModel)
	rightExpr := findBestValidPhysicalExpr(rightRef, costModel)
	if leftExpr == nil || rightExpr == nil {
		return
	}
	leftPlan := leftExpr.(physicalPlanExpression).GetRecordQueryPlan()
	rightPlan := rightExpr.(physicalPlanExpression).GetRecordQueryPlan()
	if leftPlan == nil || rightPlan == nil {
		return
	}

	aliases := sel.GetSourceAliases()
	var leftAlias, rightAlias string
	if len(aliases) >= 2 {
		leftAlias = aliases[0]
		rightAlias = aliases[1]
	}
	if leftAlias == "" {
		leftAlias = quants[0].GetAlias().Name()
	}
	if rightAlias == "" {
		rightAlias = quants[1].GetAlias().Name()
	}

	var joinType plans.JoinType
	switch sel.GetJoinType() {
	case expressions.JoinLeftOuter:
		joinType = plans.JoinLeftOuter
	case expressions.JoinCross:
		joinType = plans.JoinCross
	case expressions.JoinFullOuter:
		joinType = plans.JoinFullOuter
	default:
		joinType = plans.JoinInner
	}

	// FULL OUTER JOIN is implemented exclusively by the materialized
	// nested-loop cursor, which tracks global inner-match state to drive
	// the drain phase (emit inner rows that matched no outer row). The
	// correlated FlatMap path re-scans the inner per outer row and
	// structurally cannot observe which inner rows matched nothing, so it
	// is not a valid FULL implementation — yielding it would be silently
	// wrong, not merely suboptimal. A single explicit guard here is
	// cleaner than threading `joinType != JoinFullOuter` through both
	// correlated-FlatMap branches below (and makes the `canSwap` swap-logic
	// unreachable for FULL — FULL is symmetric but we keep the original
	// left/right column layout).
	if joinType == plans.JoinFullOuter {
		// Correlated FULL OUTER (inner ranges over the outer's alias) is
		// not standard SQL and cannot be materialized independently of the
		// outer; produce no plan rather than a wrong one.
		if referenceIsCorrelatedTo(leftRef, quants[1].GetAlias()) ||
			referenceIsCorrelatedTo(rightRef, quants[0].GetAlias()) {
			return
		}
		joinPlan := plans.NewRecordQueryNestedLoopJoinPlan(
			leftPlan, rightPlan,
			sel.GetPredicates(),
			joinType,
			leftAlias, rightAlias,
			sel.GetResultValue(),
		)
		leftQ := expressions.ForEachQuantifier(call.MemoizeExpression(leftExpr))
		rightQ := expressions.ForEachQuantifier(call.MemoizeExpression(rightExpr))
		call.Yield(newPhysicalNestedLoopJoinWrapper(joinPlan, leftQ, rightQ))
		return
	}

	// Correlated INNER/LEFT joins are implemented as a FlatMap (O(N×logM) via
	// the inner's correlated PK/index probes) by the leftDepsRight/rightDepsLeft
	// branches below; uncorrelated joins fall through to the materialized NLJ.
	// This is the single data-access-driven join path — the former Go-only
	// tryFlatMapPlan (a hand-rolled correlated-PK-probe shortcut, RFC-150
	// Phase-2b Piece 2) is retired: PartitionBinary/PartitionSelectRule absorb
	// the join predicates into correlated sub-Selects and the data-access path
	// (MatchIntermediateRule → bindOrientedComparison) SARGs them into bare
	// correlated probes, so the same correlated index-nested-loop chains now
	// emerge from the standard Cascades machinery (matching Java, which has no
	// such shortcut).
	leftCorr := values.NamedCorrelationIdentifier(leftAlias)
	rightCorr := values.NamedCorrelationIdentifier(rightAlias)

	// Provided-alias sets are computed from the actual EMBEDDED physical exprs
	// (leftExpr/rightExpr) — not the logical refs — because a re-enumerated merge
	// leg's logical alias (e.g. `E`) ranges over a ref whose chosen PHYSICAL plan
	// is a whole sub-join `(DEPT⋈EMP)` that PROVIDES buried tables (D) the logical
	// ref doesn't expose. The materialized NLJ embeds those physical plans
	// directly, so a predicate in the OTHER leg that reads a buried table is a
	// genuine cross-leg correlation that must route to the FlatMap branch, not a
	// materialized NLJ with the buried table unbound → 0 rows
	// (TestFDB_DerivedTableExistsJoin three-way).
	leftProvided := physicalProvidedAliases(leftExpr, quants[0].GetAlias())
	rightProvided := physicalProvidedAliases(rightExpr, quants[1].GetAlias())
	leftDepsRight := legReferencesAny(leftRef, rightProvided)
	rightDepsLeft := legReferencesAny(rightRef, leftProvided)
	canSwap := joinType != plans.JoinLeftOuter
	hasCorrelation := leftDepsRight || rightDepsLeft
	if !hasCorrelation {
		// Incomplete-bipartition guard: if BOTH legs reference (via re-exposed
		// merge seeds) the SAME external table that is neither leg's own provided
		// alias, the two legs are connected through a sibling that this bipartition
		// excluded — e.g. the 3-way `d,e,p WHERE d.id=e.dept_id AND d.id=p.dept_id`
		// surfaces a {(d⋈e), p}-shaped select where both legs read d through a merge
		// RC. As a materialized NLJ that select is only valid as the INNER of a
		// FlatMap(d, …) that binds d; the cost model can otherwise pick it as a
		// standalone root with d unbound → 0 rows (TestFDB_DerivedTableExistsJoin
		// three-way). The leg's d-correlation is hidden by the anchored-RC mechanism,
		// so neither hasCorrelation nor the planner's root-correlation check sees it.
		// Skipping the materialized NLJ here leaves the COMPLETE bipartitions (which
		// keep d as a real quantifier and produce the correct correlated FlatMap
		// chain) to win. A legitimate leg-to-leg correlation is handled by the
		// hasCorrelation FlatMap branch above; a true OUTER correlation is bound by
		// an enclosing FlatMap and reaches only ONE leg, so it does not trip this
		// both-legs-share guard.
		leftExternal := legExternalAliases(leftRef, leftProvided)
		rightExternal := legExternalAliases(rightRef, rightProvided)
		leftQAlias := quants[0].GetAlias()
		rightQAlias := quants[1].GetAlias()
		sharesExcludedSibling := false
		for a := range leftExternal {
			if a == leftQAlias || a == rightQAlias {
				continue
			}
			if _, ok := rightExternal[a]; ok {
				sharesExcludedSibling = true
				break
			}
		}
		if sharesExcludedSibling {
			return
		}
		joinPlan := plans.NewRecordQueryNestedLoopJoinPlan(
			leftPlan, rightPlan,
			sel.GetPredicates(),
			joinType,
			leftAlias, rightAlias,
			sel.GetResultValue(),
		)
		leftQ := expressions.ForEachQuantifier(call.MemoizeExpression(leftExpr))
		rightQ := expressions.ForEachQuantifier(call.MemoizeExpression(rightExpr))
		call.Yield(newPhysicalNestedLoopJoinWrapper(joinPlan, leftQ, rightQ))
	}

	// Correlated FlatMap: for PartitionBinarySelectRule / RewriteOuterJoinRule output
	// where predicates are absorbed into sub-Selects creating correlation. The inner's
	// null-on-empty flag (set by RewriteOuterJoinRule for LEFT OUTER) drives the
	// DefaultOnEmpty null-extension inside yieldGeneralFlatMap.
	if leftDepsRight && !rightDepsLeft && canSwap {
		r.yieldGeneralFlatMap(call, sel,
			rightPlan, leftPlan, rightCorr, leftCorr,
			rightExpr, leftExpr, joinType,
			selQuantifierIsNullOnEmpty(sel, leftCorr))
	} else if rightDepsLeft && !leftDepsRight {
		r.yieldGeneralFlatMap(call, sel,
			leftPlan, rightPlan, leftCorr, rightCorr,
			leftExpr, rightExpr, joinType,
			selQuantifierIsNullOnEmpty(sel, rightCorr))
	}
}

func referenceIsCorrelatedTo(ref *expressions.Reference, targetAlias values.CorrelationIdentifier) bool {
	_, ok := ref.GetCorrelatedTo()[targetAlias]
	return ok
}

// physicalProvidedAliases returns the correlation aliases a join leg subtree
// PROVIDES (binds) to a predicate referencing it: its own quantifier alias plus
// every table alias buried inside it — a MERGE leg `$m=(A⋈B)` provides {$m, A, B},
// so a spanning predicate in the OTHER leg that reads A's column (`p.x = a.y`) is
// seen as correlated to $m. Recurses through the leg's member quantifiers (the
// buried tables of a re-enumerated merge). Without this, a predicate referencing a
// BURIED merge leg (not the merge alias itself) is invisible to the hasCorrelation
// check, so a spanning 3-way join (a connects both b and c) emits a MATERIALIZED
// NLJ that embeds a leg with the buried table unbound → 0 rows
// (TestFDB_DerivedTableExistsJoin three-way; the GROUP-BY-wrapped twin of
// TestFDB_JoinMerge_OuterColumn_NotDropped). Cycle-breaking is by pointer-identity
// on visited expressions (RelationalExpression members are pointers → comparable):
// a fixed depth bound would silently return an INCOMPLETE alias set for a deeply
// nested leg, re-introducing the exact unbound-buried-table 0-row bug class.
func physicalProvidedAliases(expr expressions.RelationalExpression, ownAlias values.CorrelationIdentifier) map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{ownAlias: {}}
	visited := map[expressions.RelationalExpression]struct{}{}
	var walk func(e expressions.RelationalExpression)
	walk = func(e expressions.RelationalExpression) {
		if e == nil {
			return
		}
		if _, ok := visited[e]; ok {
			return
		}
		visited[e] = struct{}{}
		for _, q := range e.GetQuantifiers() {
			out[q.GetAlias()] = struct{}{}
			r := q.GetRangesOver()
			if r == nil {
				continue
			}
			for _, m := range r.AllMembers() {
				walk(m)
			}
		}
	}
	walk(expr)
	return out
}

// legReferencesAny reports whether the leg subtree ref is correlated to ANY alias
// in targetSet (the OTHER leg's provided aliases) — directly (Reference.GetCorrelatedTo)
// or through a source-anchored join RC whose leg QOVs GetCorrelatedTo deliberately
// HIDES (predicates.AddMergeSeedAliases re-exposes them). This is the seed-aware
// hasCorrelation check: a spanning predicate pushed into a merge leg reads the
// other leg's (possibly buried) column through a merge RC, so the correlation is
// hidden and must be re-exposed — otherwise ImplementNestedLoopJoinRule emits a
// materialized NLJ that embeds an unbound-outer-correlated leg → 0 rows
// (TestFDB_JoinMerge_OuterColumn_NotDropped / TestFDB_DerivedTableExistsJoin).
func legReferencesAny(ref *expressions.Reference, targetSet map[values.CorrelationIdentifier]struct{}) bool {
	for a := range ref.GetCorrelatedTo() {
		if _, ok := targetSet[a]; ok {
			return true
		}
	}
	for _, m := range ref.AllMembers() {
		se, ok := m.(*expressions.SelectExpression)
		if !ok {
			continue
		}
		for _, p := range se.GetPredicates() {
			seeds := map[values.CorrelationIdentifier]struct{}{}
			predicates.AddMergeSeedAliases(p, seeds)
			for a := range seeds {
				if _, ok := targetSet[a]; ok {
					return true
				}
			}
		}
	}
	return false
}

// legExternalAliases returns the aliases a leg subtree REFERENCES (directly or via
// re-exposed merge seeds) that it does NOT itself provide — its dangling external
// dependencies. Used by the incomplete-bipartition guard: when BOTH legs of a
// would-be materialized NLJ share an external alias, that alias is an excluded
// sibling table the two legs join through, so the materialized NLJ is unsafe as a
// standalone root (the sibling is unbound).
func legExternalAliases(ref *expressions.Reference, provided map[values.CorrelationIdentifier]struct{}) map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	add := func(a values.CorrelationIdentifier) {
		if _, ok := provided[a]; !ok {
			out[a] = struct{}{}
		}
	}
	for a := range ref.GetCorrelatedTo() {
		add(a)
	}
	for _, m := range ref.AllMembers() {
		se, ok := m.(*expressions.SelectExpression)
		if !ok {
			continue
		}
		for _, p := range se.GetPredicates() {
			seeds := map[values.CorrelationIdentifier]struct{}{}
			predicates.AddMergeSeedAliases(p, seeds)
			for a := range seeds {
				add(a)
			}
		}
	}
	return out
}

// selQuantifierIsNullOnEmpty reports whether sel's quantifier with the given alias is
// a NULL-on-empty ForEach (RewriteOuterJoinRule marks the LEFT-OUTER null-supplying
// leg this way). Drives the DefaultOnEmpty null-extension in yieldGeneralFlatMap.
func selQuantifierIsNullOnEmpty(sel *expressions.SelectExpression, alias values.CorrelationIdentifier) bool {
	for _, q := range sel.GetQuantifiers() {
		if q.GetAlias() == alias {
			return q.IsNullOnEmpty()
		}
	}
	return false
}

func (r *ImplementNestedLoopJoinRule) yieldGeneralFlatMap(
	call *ExpressionRuleCall,
	sel *expressions.SelectExpression,
	outerPlan, innerPlan plans.RecordQueryPlan,
	outerCorr, innerCorr values.CorrelationIdentifier,
	outerExpr, innerExpr expressions.RelationalExpression,
	joinType plans.JoinType,
	innerNullOnEmpty bool,
) {
	preds := flattenAndPredicates(sel.GetPredicates())

	var outerPreds, joinPreds []predicates.QueryPredicate
	for _, pred := range preds {
		corrSet := predicates.GetCorrelatedToOfPredicate(pred)
		if _, ok := corrSet[innerCorr]; ok {
			joinPreds = append(joinPreds, pred)
		} else {
			outerPreds = append(outerPreds, pred)
		}
	}

	// RFC-153: rebase BURIED-preserved-leg references in the
	// inner onto the merge correlation `outerCorr` ($m), which IS known at THIS layer
	// (Go assigns $m at PLANNING, after RewriteOuterJoinRule, so the rewrite rule could
	// not). When the preserved side is itself a join/merge (`A JOIN B ... LEFT JOIN C ON
	// C.a_id = A.id`), the null-supplying inner arrives with the ON-predicate baked in
	// as a SARG correlated to the buried source `A`; `A` is not bound below the FlatMap
	// (only $m is), so without the rebase the probe evaluates NULL → wrong null-extension
	// (RFC-153 §2). Rebasing `QOV(A).col` → `FieldValue(QOV($m),"A.col")`
	// (the authoritative qualified key the merged outer row carries) makes the comparand
	// a field of the BOUND merge row → it resolves AND SARGs Scan(C,[a_id=<$m.A_id>]).
	//
	// CRITICAL: the broadened RewriteOuterJoinRule guard and this
	// rewire are ONE unit. After rebasing, VERIFY no buried reference survives anywhere in
	// the inner via planReferencesAnyBuriedAlias, which is CONSERVATIVE — it fail-CLOSES on
	// any node type it does not fully understand (only Scan/Index SARGs, PredicatesFilter/
	// Filter preds, and Map result values are per-field inspected; Fetch/TypeFilter/
	// DefaultOnEmpty/FirstOrDefault are known correlation-free pass-throughs; EVERYTHING
	// else is treated as MIGHT-reference-buried and declines). The broadened guard's
	// correctness rests on this over-declining: a path that fires the guard but lands on an
	// inner the rebaser cannot fully rewrite DECLINES the probe → the materialized NLJ
	// (which resolves the buried predicate via the merged row's qualified keys) ships the
	// correct null-extended rows. It never under-catches a buried reference (the §2
	// wrong-rows trap); it may over-decline an unrecognized-but-buried-free inner into
	// correct-but-slow. So the unit is closed by the verifier's conservatism, not by
	// enumerating every inner shape.
	//
	// SCOPE — null-on-empty inners ONLY (innerNullOnEmpty). This buried-merge hazard is
	// SPECIFIC to RewriteOuterJoinRule's rewritten LEFT-OUTER inner: that rewrite pushes
	// the ON-predicate into a SEPARATELY-memoized inner SUBSEL whose buried-preserved
	// correlation the merge machinery (rebaseBuriedLowerReferences) never rebases. A
	// regular INNER multiway join's inner is built by the normal data-access path, where
	// the merge collapse already rebases buried references onto $m, so its correlation
	// targets $m (not a buried sub-alias) and needs neither the rebase NOR the
	// conservative verifier. Gating on innerNullOnEmpty keeps the fail-closed
	// over-declining from defeating the RFC-069 multiway index-probe (which has nested,
	// unrecognized FlatMap inners but NO buried reference) — without it the chain-interning
	// task count drops ~17% as valid INNER multiway probes are spuriously declined.
	buriedLegAliases := buriedPreservedAliases(outerExpr, outerCorr)
	innerExprForMemo := innerExpr
	if innerNullOnEmpty && len(buriedLegAliases) > 0 {
		origInnerPlan := innerPlan
		innerPlan = rebasePlanBuriedRefs(innerPlan, buriedLegAliases, outerCorr)
		for i, p := range joinPreds {
			joinPreds[i] = rebaseOuterLegRefsToMerged(p, buriedLegAliases, outerCorr)
		}
		if planReferencesAnyBuriedAlias(innerPlan, buriedLegAliases) || predsReferenceAlias(joinPreds, buriedAliasUpperSet(buriedLegAliases)) {
			return
		}
		if innerPlan != origInnerPlan {
			// The rebase rewrote the inner's buried-preserved correlation onto outerCorr
			// ($m) in the EXECUTABLE plan (innerPlan). The memoized inner EXPRESSION must
			// report the SAME rebased correlations — otherwise the original innerExpr still
			// reports the buried alias, the FlatMap wrapper aggregates a correlation to an
			// UNBOUND alias, and upper join/root/winner bookkeeping mis-routes (the
			// wrapper's logical correlations and the executable plan diverged).
			// Memoize a plan-backed expression over the rebased inner so its
			// GetCorrelatedTo reports outerCorr — which THIS FlatMap binds, so the
			// aggregation correctly subtracts it to nothing (not a dangling buried alias).
			innerExprForMemo = &scanPlanExpression{plan: innerPlan}
		}
	}

	var innerWrapped plans.RecordQueryPlan = innerPlan
	if len(joinPreds) > 0 {
		innerWrapped = plans.NewRecordQueryPredicatesFilterPlanWithAlias(
			innerPlan, joinPreds, innerCorr,
		)
	}
	// LEFT-OUTER null-extension, the Java way (ImplementNestedLoopJoinRule.java:317-322
	// / ImplementSimpleSelectRule:100-109): when the inner quantifier is null-on-empty
	// (produced by RewriteOuterJoinRule for a LEFT OUTER), wrap the inner in
	// DefaultOnEmpty so a non-matching outer row yields one all-NULL inner row instead
	// of being dropped. The FlatMap stays a PURE map (leftOuter flag NOT set) — the
	// outer-join semantics are emergent from this wrapper, exactly like Java's FlatMap.
	// The ON-predicates already sit BELOW this boundary (inside the rewritten inner
	// SUBSEL), so they filter before the null-fill — correct LEFT-OUTER semantics.
	if innerNullOnEmpty {
		innerWrapped = plans.NewRecordQueryDefaultOnEmptyPlan(
			innerWrapped, values.NewNullValue(values.UnknownType),
		)
	}

	var outerWrapped plans.RecordQueryPlan = outerPlan
	if len(outerPreds) > 0 {
		outerWrapped = plans.NewRecordQueryPredicatesFilterPlanWithAlias(
			outerPlan, outerPreds, outerCorr,
		)
	}

	flatMapPlan := plans.NewRecordQueryFlatMapPlan(
		outerWrapped, innerWrapped,
		outerCorr, innerCorr,
		sel.GetResultValue(), false,
	)
	switch joinType {
	case plans.JoinLeftOuter:
		flatMapPlan.SetLeftOuter(true)
	}

	// Bind the wrapper's quantifiers with the FlatMap plan's ACTUAL outer/inner
	// correlation aliases (outerCorr/innerCorr) — NOT fresh ForEach aliases. The
	// inner probe reports (D.2) its correlation to the bound outer alias; the
	// Reference.GetCorrelatedTo aggregation subtracts each member's quantifier
	// aliases from its children's correlations, so a fresh alias fails to subtract
	// the bound outer/inner aliases and a COMPLETED (self-contained) inner join
	// leaks them as if externally correlated → an upper multiway join sees the
	// subplan as still correlated and skips/misroutes valid alternatives. A
	// FlatMap that binds X is not correlated to X; binding with the real
	// aliases makes the aggregation report so. (The EXISTS / correlated-FOD builders
	// — implementExistentialSelect, tryExistsFlatMap, buildExistsFlatMap — bind their
	// wrapper quantifiers the same way: outer via the named outer alias, inner via
	// NamedPhysicalQuantifier(inner alias) over the FOD wrapper.)
	outerQ := expressions.NamedForEachQuantifier(outerCorr, call.MemoizeExpression(outerExpr))
	innerQ := expressions.NamedForEachQuantifier(innerCorr, call.MemoizeExpression(innerExprForMemo))
	call.Yield(newPhysicalFlatMapWrapper(flatMapPlan, outerQ, innerQ))
}

// implementExistentialSelect handles a SelectExpression with a
// ForEach outer and an Existential inner (EXISTS subquery).
//
// RFC-141: this matches Java's ImplementNestedLoopJoinRule exactly. The
// FlatMap is a PURE MAP — there is no EXISTS/NOT-EXISTS join mode. The
// existential semantics are emergent from what wraps the inner:
//
//   - The existential inner is wrapped in FirstOrDefault(inner, NULL) so it
//     yields EXACTLY ONE row (the first real inner row, or a NULL default on
//     an empty subquery), and that FOD plan is used AS THE FLATMAP INNER.
//   - WHERE-EXISTS is a SEPARATE residual filter on top of the FOD: Java's
//     ExistentialValuePredicate.toResidualPredicate() → ValuePredicate(QOV,
//     NOT_NULL). For an empty subquery FOD yields NULL → QOV IS NOT NULL is
//     FALSE → the inner yields zero rows → the pure-map FlatMap emits nothing
//     for that outer row (the semi-join). NOT-EXISTS flips the comparison to
//     IS NULL (the FlatMap inner yields the outer iff the subquery is empty).
//   - SELECT-EXISTS (projection) needs NO residual filter at all: the boolean
//     is computed by the map's resultValue (ExistsValue.eval reads the inner
//     binding — bound non-null ⇒ true, NULL ⇒ false).
//
// Correlation/join predicates that filter the inner rows (e.g. child.pid =
// parent.id) live INSIDE the inner subquery's plan already, or are pushed onto
// the inner scan range by tryExistsFlatMap; they filter the inner BELOW the
// FOD so FOD takes the first MATCHING inner row.
func (r *ImplementNestedLoopJoinRule) implementExistentialSelect(
	call *ExpressionRuleCall,
	sel *expressions.SelectExpression,
	quants []expressions.Quantifier,
) {
	outerRef := quants[0].GetRangesOver()
	innerRef := quants[1].GetRangesOver()
	if outerRef == nil || innerRef == nil {
		return
	}

	outerExpr := getWinnerForOrdering(outerRef, PreserveOrdering(), call.CostModel())
	if outerExpr == nil {
		return
	}
	outerPh, ok := outerExpr.(physicalPlanExpression)
	if !ok {
		return
	}
	outerPlan := outerPh.GetRecordQueryPlan()

	innerExpr := getWinnerForOrdering(innerRef, PreserveOrdering(), call.CostModel())
	if innerExpr == nil {
		return
	}
	innerPh, ok := innerExpr.(physicalPlanExpression)
	if !ok {
		return
	}
	innerPlan := innerPh.GetRecordQueryPlan()

	// Separate predicates into EXISTS-related and non-EXISTS. A bare
	// EXISTS (no surrounding NOT) gives a positive existential filter; a
	// NOT-EXISTS wraps it in NotPredicate, which flips the residual
	// comparison polarity below.
	allPreds := sel.GetPredicates()
	var regularPreds []predicates.QueryPredicate
	hasExistsFilter := false
	negated := false
	for _, p := range flattenAndPredicates(allPreds) {
		if _, ok := predicates.IsExistentialPredicate(p); ok {
			hasExistsFilter = true
			continue
		}
		if _, ok := predicates.IsNotExistentialPredicate(p); ok {
			hasExistsFilter = true
			negated = true
			continue
		}
		regularPreds = append(regularPreds, p)
	}

	// Extract source aliases for datum qualification.
	aliases := sel.GetSourceAliases()
	var outerAlias, innerAlias string
	if len(aliases) >= 1 {
		outerAlias = aliases[0]
	}
	if len(aliases) >= 2 {
		innerAlias = aliases[1]
	}

	outerCorr := values.NamedCorrelationIdentifier(outerAlias)
	innerCorr := values.NamedCorrelationIdentifier(innerAlias)

	// The SelectExpression's result value references the outer and existential
	// QUANTIFIER aliases (e.g. q$43), but the FlatMap binds the outer/inner rows
	// under the SOURCE aliases the rule uses (T1 / T2). Rebase the result value
	// so a projected ExistsValue's QOV(existential-quantifier) resolves against
	// the FlatMap's inner binding (RFC-141 projected EXISTS); WHERE-EXISTS keeps
	// its bare-outer-QOV result value, which rebases to QOV(outer) unchanged.
	resultValue := remapExistentialResultValue(sel.GetResultValue(),
		quants[0].GetAlias(), outerCorr, quants[1].GetAlias(), innerCorr)

	// Try correlated-scan FlatMap: if a correlated predicate matches the
	// inner table's PK or index, push the correlation into a parameterized
	// inner scan (fast path). This is the pure-map FlatMap with the FOD
	// inner; see buildExistsFlatMap. The fast path MUST use the SAME rebased
	// resultValue: it binds the inner under innerCorr, so a projected
	// ExistsValue's QOV(existential-quantifier) would otherwise stay unbound
	// and read FALSE for every matched row (the non-fast path's rebase is the
	// only thing that makes the projected boolean resolve).
	if len(regularPreds) > 0 && !sel.IsQuantifiersSwapped() {
		if r.tryExistsFlatMap(call, resultValue, outerPlan, innerPlan, outerAlias, innerAlias, outerExpr, innerExpr, hasExistsFilter, negated, regularPreds) {
			return
		}
	}

	// Split the non-EXISTS predicates: anything that references the INNER
	// subquery filters the inner rows BELOW the FOD (so the FOD picks the first
	// surviving match); a predicate that references ONLY the outer (or a
	// pre-evaluated external binding) filters the outer above the FlatMap.
	//
	// The discriminator is POSITIVE membership in the existential inner's
	// FROM-source-alias set (innerLegs): a predicate routes below the FOD iff it
	// references a correlation IN that set. innerLegs is `{innerCorr}` ∪ {all
	// FROM-source aliases the existential subplan declares} — for a single-table
	// inner the one renamed inner correlation, for a multi-table FROM inner like
	// `EXISTS (SELECT 1 FROM t2, t3 WHERE t2.t1_id = t1.id)` EVERY leg (t2, t3).
	//
	// Earlier rounds tested by ABSENCE — "references any correlation other than
	// the outer". That over-routed: an UNCORRELATED SCALAR SUBQUERY in a
	// predicate (`price > (SELECT MAX(x) FROM t2)`) has its OWN alias
	// (ScalarSubqueryValue.GetCorrelatedTo adds it) that is non-outer yet NOT an
	// inner leg — it is a pre-evaluated external binding. The absence test pushed
	// that scalar predicate BELOW the FOD; alongside an empty NOT-EXISTS it never
	// evaluated (the empty FOD's IS-NULL residual admitted every outer row), so
	// the scalar comparison was silently dropped (RFC-141 R4). Routing
	// by inner-leg-set MEMBERSHIP keeps the multi-table fix (all inner legs
	// route below, where the merged inner row's qualified leg keys T2.T1_ID and
	// the live outer binding both resolve) AND keeps scalar-subquery / parameter /
	// other external-binding predicates outer (where their pre-evaluated value is
	// read and the comparison actually filters the outer row).
	innerLegs := collectInnerLegAliases(innerRef, innerCorr)
	var joinPreds []predicates.QueryPredicate
	var outerOnlyPreds []predicates.QueryPredicate
	for _, p := range regularPreds {
		if predicateReferencesInnerLeg(p, innerLegs) {
			joinPreds = append(joinPreds, p)
		} else {
			outerOnlyPreds = append(outerOnlyPreds, p)
		}
	}

	// Build the inner: [inner subplan | join-pred filter] | FirstOrDefault(NULL)
	// | [residual existential filter]. The residual filter — Java's
	// toResidualPredicate of the ExistentialValuePredicate — is what makes the
	// pure-map FlatMap behave as a semi-join for WHERE-EXISTS.
	var belowFOD plans.RecordQueryPlan = innerPlan
	if len(joinPreds) > 0 {
		belowFOD = plans.NewRecordQueryPredicatesFilterPlanWithAlias(innerPlan, joinPreds, innerCorr)
	}
	fodPlan := plans.NewRecordQueryFirstOrDefaultPlan(belowFOD, values.NewNullValue(values.UnknownType))

	var flatMapInner plans.RecordQueryPlan = fodPlan
	if hasExistsFilter {
		// EXISTS ⇒ QOV(inner) IS NOT NULL drops empty-subquery (NULL) rows;
		// NOT-EXISTS ⇒ QOV(inner) IS NULL drops non-empty rows. Either way the
		// pure-map FlatMap emits the outer row iff the residual survives.
		cmp := predicates.Comparison{Type: predicates.ComparisonIsNotNull}
		if negated {
			cmp = predicates.Comparison{Type: predicates.ComparisonIsNull}
		}
		residual := predicates.NewComparisonPredicate(values.NewQuantifiedObjectValue(innerCorr), cmp)
		flatMapInner = plans.NewRecordQueryPredicatesFilterPlanWithAlias(fodPlan, []predicates.QueryPredicate{residual}, innerCorr)
	}

	var flatMapOuter plans.RecordQueryPlan = outerPlan
	if len(outerOnlyPreds) > 0 {
		flatMapOuter = plans.NewRecordQueryPredicatesFilterPlanWithAlias(outerPlan, outerOnlyPreds, outerCorr)
	}

	flatMapPlan := plans.NewRecordQueryFlatMapPlan(
		flatMapOuter, flatMapInner,
		outerCorr, innerCorr,
		resultValue, false,
	)

	// The FlatMap wrapper needs the outer + inner physical quantifiers for
	// Cascades bookkeeping and cost. Range the inner quantifier over the FOD
	// wrapper (the existential one-row inner) so cost/ordering see the
	// FirstOrDefault, not the raw subquery.
	outerQuant := expressions.NamedPhysicalQuantifier(quants[0].GetAlias(), call.MemoizeExpression(outerExpr))
	fodWrapper := NewPhysicalFirstOrDefaultWrapper(fodPlan,
		expressions.NamedPhysicalQuantifier(quants[1].GetAlias(), call.MemoizeExpression(innerExpr)))
	innerQuant := expressions.NewPhysicalQuantifier(call.MemoizeExpression(fodWrapper))
	call.Yield(newPhysicalFlatMapWrapper(flatMapPlan, outerQuant, innerQuant))
}

// remapExistentialResultValue rebases an existential SelectExpression's result
// value so quantifier-alias references resolve against the FlatMap's source
// correlations (RFC-141). A projected ExistsValue references the existential
// QUANTIFIER alias (e.g. q$43); the FlatMap binds the inner row under the rule's
// inner source correlation. Mapping q1.alias→innerCorr (and q0.alias→outerCorr)
// makes the projected boolean resolve. When the aliases already coincide (the
// bare-outer-QOV WHERE-EXISTS result), the rebase is an identity.
func remapExistentialResultValue(
	rv values.Value,
	outerQAlias, outerCorr, innerQAlias, innerCorr values.CorrelationIdentifier,
) values.Value {
	if rv == nil {
		return nil
	}
	am := values.AliasMap{}
	if outerQAlias != outerCorr {
		am[outerQAlias] = outerCorr
	}
	if innerQAlias != innerCorr {
		am[innerQAlias] = innerCorr
	}
	if len(am) == 0 {
		return rv
	}
	return values.RebaseValue(rv, am)
}

// resultValueReferencesAlias reports whether a SelectExpression result value's
// correlation set includes `alias` — the structural signal (RFC-141)
// that the result value is a PROJECTED EXISTS over a join (it reads the
// existential quantifier), not the WHERE-EXISTS pass-through (a bare merged-row
// identity, which is correlated only to the merged outer, never to the
// existential quantifier). Returns false for a nil value.
func resultValueReferencesAlias(rv values.Value, alias values.CorrelationIdentifier) bool {
	if rv == nil {
		return false
	}
	_, ok := values.GetCorrelatedToOfValue(rv)[alias]
	return ok
}

// mergedOuterLegAliases returns the COMPLETE set of source-leg aliases the
// inner-join's merged outer row anchors columns for: the two top-level quantifier
// aliases PLUS every alias BURIED inside a leg that is itself a JOIN/UNNEST subtree
// (`FROM T1, T1.arr AS V, U` anchors T1, V, U — T1 buried under the unnest leg whose
// row flows under V). The buried aliases are read ALGEBRAICALLY off the anchored
// result value's dotted field-name prefixes: that value IS the merged-row schema
// (NewAnchoredJoinRecord names each leg column "LEG.COL", dotted columns verbatim),
// so the prefixes are the exact source-alias inventory the merged binding carries.
// A residual referencing a buried source (QOV(T1).ID) must rebase to that verbatim
// "T1.ID" key; reading only {leftAlias,rightAlias} left it unbound → NULL → rows
// dropped. leftAlias/rightAlias are always included, so a non-anchored result value
// (a folded projection) and a plain `FROM A,B` join degenerate to the prior
// behaviour (a no-op for any already-bound reference). RFC-142.
func mergedOuterLegAliases(rv values.Value, leftAlias, rightAlias string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(a string) {
		if a == "" {
			return
		}
		up := strings.ToUpper(a)
		if _, ok := seen[up]; ok {
			return
		}
		seen[up] = struct{}{}
		out = append(out, up)
	}
	add(leftAlias)
	add(rightAlias)
	if rc, ok := rv.(*values.RecordConstructorValue); ok && rc.AnchoredJoin {
		for _, f := range rc.Fields {
			if dot := strings.IndexByte(f.Name, '.'); dot > 0 {
				add(f.Name[:dot])
			}
		}
	}
	return out
}

// rebaseOuterLegRefsToMerged rewrites references to the original join-outer leg
// aliases (legAliases, e.g. ["E","D"]) so they resolve against the inner-join's
// MERGED row bound under mergedCorr (RFC-141 Phase 2, P1a). A leg reference
// `FieldValue{Field:"ID", Child:QOV("E")}` becomes
// `FieldValue{Field:"E.ID", Child:QOV(mergedCorr)}` — i.e. it targets the merged
// row's QUALIFIED "LEG.COL" key (written by the NLJ cursor's mergeRows), not the
// last-leg-wins bare "ID" key. References to any other alias (the existential
// inner P, parameters, constants) pass through untouched. Mirrors the predicate
// shapes that can appear in existPreds (Comparison/And/Or/Not); other shapes are
// returned unchanged.
func rebaseOuterLegRefsToMerged(
	p predicates.QueryPredicate,
	legAliases []string,
	mergedCorr values.CorrelationIdentifier,
) predicates.QueryPredicate {
	if p == nil {
		return p
	}
	switch pred := p.(type) {
	case *predicates.ComparisonPredicate:
		newOperand := rebaseOuterLegValue(pred.Operand, legAliases, mergedCorr)
		newCompOperand := rebaseOuterLegValue(pred.Comparison.Operand, legAliases, mergedCorr)
		if newOperand == pred.Operand && newCompOperand == pred.Comparison.Operand {
			return p
		}
		// Copy the whole Comparison and replace ONLY the rebased RHS operand,
		// preserving Escape (the LIKE escape rune) AND every other Comparison
		// subclass field (ParameterName, the Text* fields, the DistanceRank
		// vector fields). A partial {Type, Operand, Escape} reconstruction would
		// silently drop the rest and change the comparison's semantics.
		cmp := pred.Comparison
		cmp.Operand = newCompOperand
		return &predicates.ComparisonPredicate{
			Operand:    newOperand,
			Comparison: cmp,
		}
	case *predicates.ValuePredicate:
		newVal := rebaseOuterLegValue(pred.Value, legAliases, mergedCorr)
		if newVal == pred.Value {
			return p
		}
		return predicates.NewValuePredicate(newVal)
	case *predicates.AndPredicate:
		changed := false
		subs := make([]predicates.QueryPredicate, len(pred.SubPredicates))
		for i, s := range pred.SubPredicates {
			subs[i] = rebaseOuterLegRefsToMerged(s, legAliases, mergedCorr)
			if subs[i] != s {
				changed = true
			}
		}
		if !changed {
			return p
		}
		return predicates.NewAnd(subs...)
	case *predicates.OrPredicate:
		changed := false
		subs := make([]predicates.QueryPredicate, len(pred.SubPredicates))
		for i, s := range pred.SubPredicates {
			subs[i] = rebaseOuterLegRefsToMerged(s, legAliases, mergedCorr)
			if subs[i] != s {
				changed = true
			}
		}
		if !changed {
			return p
		}
		return predicates.NewOr(subs...)
	case *predicates.NotPredicate:
		newChild := rebaseOuterLegRefsToMerged(pred.Child, legAliases, mergedCorr)
		if newChild == pred.Child {
			return p
		}
		return predicates.NewNot(newChild)
	default:
		return p
	}
}

// rebaseOuterLegValue is the value-tree half of rebaseOuterLegRefsToMerged. It
// recurses the value tree; a leaf `FieldValue{Field, Child:QOV(leg)}` whose leg
// matches (case-insensitively) one of legAliases is rewritten to a flat
// qualified field over mergedCorr, so the FlatMap inner's binding-path lookup
// resolves bm["LEG.COL"] (the merged row's unambiguous qualified key).
func rebaseOuterLegValue(
	v values.Value,
	legAliases []string,
	mergedCorr values.CorrelationIdentifier,
) values.Value {
	if v == nil {
		return v
	}
	if fv, ok := v.(*values.FieldValue); ok {
		// Only direct leg columns are rewritten. An already-dotted Field would
		// indicate a pre-qualified reference from a deeper join level — those do
		// not reach the EXISTS path (they are handled by the data-access correlated
		// probe machinery), and re-qualifying would invent a key like "E.A.B".
		if qov, ok := fv.Child.(*values.QuantifiedObjectValue); ok && !strings.Contains(fv.Field, ".") {
			corr := strings.ToUpper(qov.Correlation.String())
			for _, leg := range legAliases {
				if leg != "" && strings.ToUpper(leg) == corr {
					qualField := corr + "." + strings.ToUpper(fv.Field)
					return values.NewFieldValue(
						values.NewQuantifiedObjectValue(mergedCorr),
						qualField, fv.Typ,
					)
				}
			}
		}
	}
	children := v.Children()
	if len(children) == 0 {
		return v
	}
	changed := false
	newChildren := make([]values.Value, len(children))
	for i, c := range children {
		newChildren[i] = rebaseOuterLegValue(c, legAliases, mergedCorr)
		if newChildren[i] != c {
			changed = true
		}
	}
	if !changed {
		return v
	}
	return values.WithChildren(v, newChildren)
}

// rebasePlanBuriedRefs rewrites every reference to a BURIED preserved-leg alias in a
// built inner plan tree onto the merge correlation (RFC-153, approach a-implement).
// The null-supplying inner of a joined-preserved LEFT OUTER arrives here already
// implemented, with the ON-predicate baked in as a SARG (IndexScan/Scan comparison)
// or a residual (PredicatesFilter) correlated to a buried preserved source `A`. At
// this layer (yieldGeneralFlatMap) the preserved merge correlation `mergedCorr` ($m)
// IS known, so we rebase `QOV(A).col` → `FieldValue(QOV($m), "A.col")` — the
// authoritative qualified key the merged outer row carries (review condition 4) —
// using the exact rebaseOuterLegValue/rebaseOuterLegRefsToMerged machinery the
// EXISTS-over-join path uses. Pass-through nodes are rebuilt around their rebased
// inner; an unhandled node is returned as-is and caught by the post-rebase
// verification (planReferencesAnyBuriedAlias) which declines the probe so the
// correct materialized NLJ fallback wins.
func rebasePlanBuriedRefs(p plans.RecordQueryPlan, legAliases []string, mergedCorr values.CorrelationIdentifier) plans.RecordQueryPlan {
	if p == nil || len(legAliases) == 0 {
		return p
	}
	switch pl := p.(type) {
	case *plans.RecordQueryIndexPlan:
		newComps, changed := rebaseComparisonRanges(pl.GetScanComparisons(), legAliases, mergedCorr)
		if !changed {
			return p
		}
		return pl.WithScanComparisons(newComps)
	case *plans.RecordQueryScanPlan:
		newComps, changed := rebaseComparisonRanges(pl.GetScanComparisons(), legAliases, mergedCorr)
		if !changed {
			return p
		}
		return pl.WithScanComparisons(newComps)
	case *plans.RecordQueryPredicatesFilterPlan:
		inner := rebasePlanBuriedRefs(pl.GetInner(), legAliases, mergedCorr)
		preds := pl.GetPredicates()
		newPreds := make([]predicates.QueryPredicate, len(preds))
		changed := inner != pl.GetInner()
		for i, pr := range preds {
			newPreds[i] = rebaseOuterLegRefsToMerged(pr, legAliases, mergedCorr)
			if newPreds[i] != pr {
				changed = true
			}
		}
		if !changed {
			return p
		}
		return plans.NewRecordQueryPredicatesFilterPlanWithAlias(inner, newPreds, pl.GetInnerAlias())
	case *plans.RecordQueryFilterPlan:
		inner := rebasePlanBuriedRefs(pl.GetInner(), legAliases, mergedCorr)
		preds := pl.GetPredicates()
		newPreds := make([]predicates.QueryPredicate, len(preds))
		changed := inner != pl.GetInner()
		for i, pr := range preds {
			newPreds[i] = rebaseOuterLegRefsToMerged(pr, legAliases, mergedCorr)
			if newPreds[i] != pr {
				changed = true
			}
		}
		if !changed {
			return p
		}
		return plans.NewRecordQueryFilterPlan(newPreds, inner)
	case *plans.RecordQueryFetchFromPartialRecordPlan:
		inner := rebasePlanBuriedRefs(pl.GetInner(), legAliases, mergedCorr)
		if inner == pl.GetInner() {
			return p
		}
		return plans.NewRecordQueryFetchFromPartialRecordPlan(inner, pl.GetTranslateValueFunction(), pl.GetResultType(), pl.GetFetchIndexRecords())
	case *plans.RecordQueryDefaultOnEmptyPlan:
		inner := rebasePlanBuriedRefs(pl.GetInner(), legAliases, mergedCorr)
		if inner == pl.GetInner() {
			return p
		}
		return plans.NewRecordQueryDefaultOnEmptyPlan(inner, pl.GetDefaultValue())
	case *plans.RecordQueryFirstOrDefaultPlan:
		inner := rebasePlanBuriedRefs(pl.GetInner(), legAliases, mergedCorr)
		if inner == pl.GetInner() {
			return p
		}
		return plans.NewRecordQueryFirstOrDefaultPlan(inner, pl.GetDefaultValue())
	case *plans.RecordQueryTypeFilterPlan:
		inner := rebasePlanBuriedRefs(pl.GetInner(), legAliases, mergedCorr)
		if inner == pl.GetInner() {
			return p
		}
		return plans.NewRecordQueryTypeFilterPlan(pl.GetRecordTypes(), inner)
	case *plans.RecordQueryMapPlan:
		inner := rebasePlanBuriedRefs(pl.GetInner(), legAliases, mergedCorr)
		newResult := rebaseOuterLegValue(pl.GetResultValue(), legAliases, mergedCorr)
		if inner == pl.GetInner() && newResult == pl.GetResultValue() {
			return p
		}
		return plans.NewRecordQueryMapPlan(inner, newResult)
	default:
		// Unhandled node — return unchanged. planReferencesAnyBuriedAlias will detect
		// any buried reference that survives here and decline the probe.
		return p
	}
}

// rebaseComparisonRanges rebases the buried-leg references in a SARG's per-column
// comparison ranges onto mergedCorr. Returns the new ranges and whether any changed.
func rebaseComparisonRanges(comps []*predicates.ComparisonRange, legAliases []string, mergedCorr values.CorrelationIdentifier) ([]*predicates.ComparisonRange, bool) {
	out := make([]*predicates.ComparisonRange, len(comps))
	changed := false
	for i, cr := range comps {
		nc, ch := rebaseComparisonRange(cr, legAliases, mergedCorr)
		out[i] = nc
		if ch {
			changed = true
		}
	}
	return out, changed
}

// rebaseComparisonRange rebases the buried-leg references in one comparison range's
// equality/inequality comparison operands. Returns the (possibly rebuilt) range and
// whether it changed. A range whose rebuilt comparison cannot be re-merged is
// returned unchanged (the verification then declines the probe).
func rebaseComparisonRange(cr *predicates.ComparisonRange, legAliases []string, mergedCorr values.CorrelationIdentifier) (*predicates.ComparisonRange, bool) {
	if cr == nil || cr.IsEmpty() {
		return cr, false
	}
	var comparisons []*predicates.Comparison
	if cr.IsEquality() {
		comparisons = []*predicates.Comparison{cr.GetEqualityComparison()}
	} else {
		comparisons = cr.GetInequalityComparisons()
	}
	rebuilt := predicates.EmptyComparisonRange()
	changed := false
	for _, c := range comparisons {
		nc := rebaseComparison(c, legAliases, mergedCorr)
		if nc != c {
			changed = true
		}
		res := rebuilt.Merge(nc)
		if !res.Ok {
			return cr, false
		}
		rebuilt = res.Range
	}
	if !changed {
		return cr, false
	}
	return rebuilt, true
}

// rebaseComparison rebases a single comparison's RHS operand value onto mergedCorr,
// copying the comparison so every non-operand field (Type, Escape, ParameterName,
// the Text*/vector fields) is preserved verbatim.
func rebaseComparison(c *predicates.Comparison, legAliases []string, mergedCorr values.CorrelationIdentifier) *predicates.Comparison {
	if c == nil || c.Operand == nil {
		return c
	}
	newOperand := rebaseOuterLegValue(c.Operand, legAliases, mergedCorr)
	if newOperand == c.Operand {
		return c
	}
	nc := *c
	nc.Operand = newOperand
	return &nc
}

// planReferencesAnyBuriedAlias reports whether any SARG comparand, residual-filter
// predicate, or map result value in the plan tree STILL references one of the buried
// preserved-leg aliases (case-insensitive) — i.e. the rebase was incomplete and the
// probe would evaluate an unbound correlation at runtime (the §2 wrong-rows trap).
// yieldGeneralFlatMap declines the probe when this returns true.
func planReferencesAnyBuriedAlias(p plans.RecordQueryPlan, legAliases []string) bool {
	if p == nil || len(legAliases) == 0 {
		return false
	}
	upper := make(map[string]struct{}, len(legAliases))
	for _, a := range legAliases {
		if a != "" {
			upper[strings.ToUpper(a)] = struct{}{}
		}
	}
	found := false
	plans.Walk(p, func(n plans.RecordQueryPlan) bool {
		if found {
			return false
		}
		switch sp := n.(type) {
		// INSPECTED types — rebasePlanBuriedRefs rewrites these nodes' OWN
		// correlation-bearing fields (SARG comparands / residual preds / map result
		// value), so we do the real per-field check: a buried reference that survives
		// here means the rebase was incomplete (an alias mismatch).
		case *plans.RecordQueryScanPlan:
			if comparisonRangesReferenceAlias(sp.GetScanComparisons(), upper) {
				found = true
			}
		case *plans.RecordQueryIndexPlan:
			if comparisonRangesReferenceAlias(sp.GetScanComparisons(), upper) {
				found = true
			}
		case *plans.RecordQueryPredicatesFilterPlan:
			if predsReferenceAlias(sp.GetPredicates(), upper) {
				found = true
			}
		case *plans.RecordQueryFilterPlan:
			if predsReferenceAlias(sp.GetPredicates(), upper) {
				found = true
			}
		case *plans.RecordQueryMapPlan:
			if valueReferencesAlias(sp.GetResultValue(), upper) {
				found = true
			}
		// KNOWN correlation-free pass-throughs — these carry no buried correlation in
		// their OWN fields (default value / record types / fetch translation), so skip
		// them; the plans.Walk recursion still examines their children.
		case *plans.RecordQueryFetchFromPartialRecordPlan,
			*plans.RecordQueryTypeFilterPlan,
			*plans.RecordQueryDefaultOnEmptyPlan,
			*plans.RecordQueryFirstOrDefaultPlan:
			// skip — children examined by recursion
		default:
			// FAIL-CLOSED: any node whose OWN correlation-bearing fields the
			// rebaser's walker does NOT rewrite — a nested FlatMap/NLJ (preds + result
			// value), an InJoin/InUnion (the IN comparand), an Aggregate/GroupBy/Union/
			// Sort/Distinct (group/sort key values), or any future plan node — MIGHT carry
			// an unrewired buried-preserved correlation this verifier does not inspect.
			// Flag the node itself regardless of its children → DECLINE the probe → the
			// correct materialized NLJ fallback (which null-extends via the merged row's
			// qualified keys). This OVER-declines an unrecognized-but-buried-free inner into
			// correct-but-slow, but NEVER under-catches a buried reference (the §2
			// wrong-rows trap). The broadened RewriteOuterJoinRule guard's correctness rests
			// on this verifier being CONSERVATIVE — fail-closed on any node it does not
			// fully understand — which the default arm now enforces. E.g.
			// `LEFT JOIN C ON c.x IN (SELECT … WHERE z = a.id)`: the InJoin comparand
			// correlates to the buried A, the walker leaves it unrewired, and this arm
			// declines so the materialized NLJ ships correct null-extended rows.
			found = true
		}
		return !found
	})
	return found
}

func comparisonRangesReferenceAlias(comps []*predicates.ComparisonRange, upper map[string]struct{}) bool {
	for a := range scanComparisonCorrelations(comps) {
		if _, ok := upper[strings.ToUpper(a.Name())]; ok {
			return true
		}
	}
	return false
}

func predsReferenceAlias(preds []predicates.QueryPredicate, upper map[string]struct{}) bool {
	for _, pr := range preds {
		for a := range predicates.GetCorrelatedToOfPredicate(pr) {
			if _, ok := upper[strings.ToUpper(a.Name())]; ok {
				return true
			}
		}
	}
	return false
}

func valueReferencesAlias(v values.Value, upper map[string]struct{}) bool {
	if v == nil {
		return false
	}
	for a := range values.GetCorrelatedToOfValue(v) {
		if _, ok := upper[strings.ToUpper(a.Name())]; ok {
			return true
		}
	}
	return false
}

// buriedPreservedAliases returns the BURIED source aliases of a join's outer
// (preserved) leg — everything physicalProvidedAliases reports EXCEPT the leg's own
// merge correlation. These are the aliases a null-supplying inner correlation may
// target through the merge (RFC-153). Empty when the leg is a bare table.
func buriedPreservedAliases(outerExpr expressions.RelationalExpression, outerCorr values.CorrelationIdentifier) []string {
	if outerExpr == nil {
		return nil
	}
	var out []string
	for alias := range physicalProvidedAliases(outerExpr, outerCorr) {
		if alias != outerCorr && alias.Name() != "" {
			out = append(out, alias.Name())
		}
	}
	return out
}

// buriedAliasUpperSet returns the upper-cased name set of legAliases for the
// post-rebase verification's case-insensitive membership test.
func buriedAliasUpperSet(legAliases []string) map[string]struct{} {
	out := make(map[string]struct{}, len(legAliases))
	for _, a := range legAliases {
		if a != "" {
			out[strings.ToUpper(a)] = struct{}{}
		}
	}
	return out
}

// predicateReferencesInnerLeg reports whether a predicate references any
// correlation in the existential inner's FROM-source-alias set (innerLegs) —
// i.e. it touches the existential inner subquery and must be evaluated BELOW the
// FirstOrDefault (against the inner row), not above/around the FlatMap (against
// the outer row(s) alone).
//
// This is POSITIVE membership in the KNOWN inner-leg set, not the negation
// "references any correlation that is NOT the outer". The two disagree on a
// correlation that is neither outer NOR an inner leg: an UNCORRELATED SCALAR
// SUBQUERY in a predicate (`price > (SELECT MAX(x) FROM t2)`) carries its own
// ScalarSubqueryValue alias (a non-outer correlation), and a parameter marker
// may too. Those are pre-evaluated EXTERNAL bindings, never inner table legs;
// the absence test wrongly routed them below the FOD where, alongside an empty
// NOT-EXISTS, they never evaluated and the comparison was silently dropped
// (RFC-141 R4). Membership in innerLegs keeps the multi-table
// fix (every inner leg routes below) AND keeps such external-binding predicates
// outer-side (the comparison actually filters the outer row).
func predicateReferencesInnerLeg(p predicates.QueryPredicate, innerLegs map[values.CorrelationIdentifier]struct{}) bool {
	for corr := range predicates.GetCorrelatedToOfPredicate(p) {
		if _, ok := innerLegs[corr]; ok {
			return true
		}
	}
	return false
}

// collectInnerLegAliases computes the existential inner's FROM-source-alias set:
// the KNOWN set of correlations the existential subplan declares, against which a
// predicate is classified as inner (route below the FOD) vs. outer/external
// (route outer-side). innerCorr is the rule's inner correlation, under which the
// FlatMap binds the FOD inner.
//
// Two cases, distinguished by whether innerCorr is itself one of the subplan's
// declared FROM-source aliases:
//
//   - MULTI-TABLE inner (`EXISTS (SELECT 1 FROM t2, t3 WHERE …)`):
//     existsInnerCorrelation declines the rename, so innerCorr is the RIGHTMOST
//     leg (sourceAlias(esq.Plan)) — a declared leg. The correlation predicates
//     reference RAW leg aliases (t2, t3), resolved through the merged inner row's
//     qualified LEG.COL keys. The full inner-leg set is ALL declared legs, so
//     this returns innerCorr ∪ {t2, t3, …}.
//
//   - SINGLE-TABLE inner (`EXISTS (SELECT 1 FROM t WHERE …)`):
//     existsInnerCorrelation RENAMED the inner correlation to a UNIQUE alias
//     (the alias-shadow fix), and rebased the join predicate onto it. The
//     predicate references THAT unique alias = innerCorr, never the subplan's
//     own scan alias. innerCorr is NOT among the subplan's declared aliases, so
//     this returns {innerCorr} ALONE. Crucially it must NOT include the subplan's
//     raw scan alias: in the alias-shadow self-subquery (`FROM t … EXISTS (SELECT
//     1 FROM t …)`) the outer source and the inner scan share the name `T`, and
//     an outer-only predicate (`id > 1`, correlated to the shared `T`) would be
//     mis-routed below the FOD if `T` leaked into the inner-leg set (that
//     regression). Returning {innerCorr} keeps it outer-side.
//
// The walk gathers declared aliases from each SelectExpression's
// GetSourceAliases() and from ForEach/Physical quantifier aliases — never an
// EXTERNAL value-tree binding (a scalar-subquery / parameter alias is not a FROM
// quantifier), so such correlations never enter the inner-leg set.
func collectInnerLegAliases(innerRef *expressions.Reference, innerCorr values.CorrelationIdentifier) map[values.CorrelationIdentifier]struct{} {
	declared := map[values.CorrelationIdentifier]struct{}{}
	if innerRef != nil {
		visited := map[*expressions.Reference]struct{}{}
		var walk func(r *expressions.Reference)
		walk = func(r *expressions.Reference) {
			if r == nil {
				return
			}
			r = r.Canonical()
			if _, seen := visited[r]; seen {
				return
			}
			visited[r] = struct{}{}
			for _, m := range r.Members() {
				if sel, ok := m.(*expressions.SelectExpression); ok {
					for _, a := range sel.GetSourceAliases() {
						if a != "" {
							declared[values.NamedCorrelationIdentifier(a)] = struct{}{}
						}
					}
				}
				for _, q := range m.GetQuantifiers() {
					if q.Kind() == expressions.QuantifierForEach || q.Kind() == expressions.QuantifierPhysical {
						declared[q.GetAlias()] = struct{}{}
					}
					walk(q.GetRangesOver())
				}
			}
		}
		walk(innerRef)
	}

	// If innerCorr is itself a declared leg, the inner is multi-table (not
	// renamed) and predicates reference the raw leg aliases — return all declared
	// legs. Otherwise the inner correlation was renamed to a unique alias and
	// predicates reference ONLY that; the subplan's raw aliases are not referenced
	// and must not leak in (the alias-shadow case).
	out := map[values.CorrelationIdentifier]struct{}{innerCorr: {}}
	if _, ok := declared[innerCorr]; ok {
		for a := range declared {
			out[a] = struct{}{}
		}
	}
	return out
}

// flattenAndPredicates extracts individual predicates from an AND
// chain. If the list is a single AND predicate, returns its sub-
// predicates. Otherwise returns the list as-is.
func flattenAndPredicates(preds []predicates.QueryPredicate) []predicates.QueryPredicate {
	if len(preds) == 1 {
		if and, ok := preds[0].(*predicates.AndPredicate); ok {
			return and.SubPredicates
		}
	}
	return preds
}

// implementJoinWithExistential handles a flat SelectExpression with
// ForEach(left), ForEach(right), Existential(exists_scan). This shape
// comes from a cross-join + WHERE EXISTS filter. The method builds a
// two-level NLJ: an inner join for left × right, then an outer EXISTS
// semi-join wrapping the join result with the existential inner.
func (r *ImplementNestedLoopJoinRule) implementJoinWithExistential(
	call *ExpressionRuleCall,
	sel *expressions.SelectExpression,
	quants []expressions.Quantifier,
) {
	// FULL OUTER cannot be implemented through the join+EXISTS semi-join
	// shape (it cannot carry the FULL drain). FULL+EXISTS is rejected
	// upstream with a clear error, but guard here too so this rule never
	// silently yields an INNER plan (the join-type switch below defaults
	// JoinFullOuter → JoinInner).
	if sel.GetJoinType() == expressions.JoinFullOuter {
		return
	}

	leftRef := quants[0].GetRangesOver()
	rightRef := quants[1].GetRangesOver()
	existRef := quants[2].GetRangesOver()
	if leftRef == nil || rightRef == nil || existRef == nil {
		return
	}

	leftExpr := getWinnerForOrdering(leftRef, PreserveOrdering(), call.CostModel())
	rightExpr := getWinnerForOrdering(rightRef, PreserveOrdering(), call.CostModel())
	existExpr := getWinnerForOrdering(existRef, PreserveOrdering(), call.CostModel())
	if leftExpr == nil || rightExpr == nil || existExpr == nil {
		return
	}
	leftPh, ok1 := leftExpr.(physicalPlanExpression)
	rightPh, ok2 := rightExpr.(physicalPlanExpression)
	existPh, ok3 := existExpr.(physicalPlanExpression)
	if !ok1 || !ok2 || !ok3 {
		return
	}
	leftPlan := leftPh.GetRecordQueryPlan()
	rightPlan := rightPh.GetRecordQueryPlan()
	existPlan := existPh.GetRecordQueryPlan()

	aliases := sel.GetSourceAliases()
	var leftAlias, rightAlias, existAlias string
	if len(aliases) >= 1 {
		leftAlias = aliases[0]
	}
	if len(aliases) >= 2 {
		rightAlias = aliases[1]
	}
	if len(aliases) >= 3 {
		existAlias = aliases[2]
	}

	// Split predicates into join predicates (for the inner join) and
	// EXISTS-related predicates (for the outer existential level). EXISTS
	// predicates reference the existential alias and belong on the outer
	// level.
	allPreds := flattenAndPredicates(sel.GetPredicates())
	var joinPreds, existPreds []predicates.QueryPredicate
	hasExistsFilter := false
	negated := false
	existCorr := values.NamedCorrelationIdentifier(existAlias)
	// The inner-leg set of the EXISTS subquery (existCorr ∪ all FROM-source
	// aliases the existential subplan declares). A predicate that references a
	// member belongs on the existential level, BELOW the FOD; a predicate that
	// references ONLY the outer JOIN legs is the inner-join condition; an external
	// binding (uncorrelated scalar subquery alias / parameter) stays on the
	// left×right join, never pushed below the FOD (RFC-141 R4).
	existLegs := collectInnerLegAliases(existRef, existCorr)
	for _, p := range allPreds {
		if _, ok := predicates.IsExistentialPredicate(p); ok {
			// Pure EXISTS predicate — belongs on the outer level.
			hasExistsFilter = true
			continue
		}
		if _, ok := predicates.IsNotExistentialPredicate(p); ok {
			hasExistsFilter = true
			negated = true
			continue
		}
		// A predicate referencing a member of the EXISTS inner-leg set belongs on
		// the existential level, below the FOD. The earlier "any non-outer-leg
		// correlation" test misclassified a correlation predicate referencing a
		// NON-rightmost leg of a MULTI-TABLE EXISTS inner as an exist predicate but
		// also over-routed an uncorrelated scalar-subquery predicate below the FOD;
		// membership in existLegs is the precise discriminator (RFC-141 R4 P2a,
		// JOIN-in-FROM variant).
		if predicateReferencesInnerLeg(p, existLegs) {
			existPreds = append(existPreds, p)
		} else {
			joinPreds = append(joinPreds, p)
		}
	}

	// Map join type.
	var joinType plans.JoinType
	switch sel.GetJoinType() {
	case expressions.JoinLeftOuter:
		joinType = plans.JoinLeftOuter
	default:
		joinType = plans.JoinInner
	}

	// Step 1: build inner join (left × right). Its merged row is the outer
	// of the existential FlatMap.
	innerJoinPlan := plans.NewRecordQueryNestedLoopJoinPlan(
		leftPlan, rightPlan,
		joinPreds,
		joinType,
		leftAlias, rightAlias,
		sel.GetResultValue(),
	)

	// The inner-join's merged row is bound under a FRESH outer correlation in
	// the existential FlatMap (Go's 3-quantifier join+EXISTS is a two-level
	// plan: NLJ → FlatMap, unlike Java's single FlatMap that keeps both source
	// aliases bound). Allocate that correlation up front so the existential
	// predicates can be rebased onto it before they are pushed into the inner.
	mergedOuterCorr := values.UniqueCorrelationIdentifier()

	// The COMPLETE outer-leg alias set the merged row anchors columns for — the
	// two top-level quantifier aliases PLUS every alias buried inside a leg that
	// is itself a JOIN/UNNEST subtree (`FROM T1, T1.arr AS V, U` anchors T1, V, U).
	// Every residual/projected reference to ANY of these reads the merged row's
	// verbatim "LEG.COL" key; rebasing only {leftAlias,rightAlias} left a buried
	// reference (QOV(T1).ID) unbound below the FlatMap → NULL → dropped rows. RFC-142.
	outerLegAliases := mergedOuterLegAliases(sel.GetResultValue(), leftAlias, rightAlias)

	// Step 2: build the existential level as a PURE-MAP FlatMap (RFC-141 —
	// no EXISTS join mode). The inner is the existential subplan filtered by
	// any correlated EXISTS predicates, wrapped in FirstOrDefault(NULL), then
	// (for WHERE-EXISTS) a residual existential filter (QOV IS NOT NULL /
	// IS NULL). The FlatMap returns the inner-join's merged row unchanged.
	// (existCorr is computed above for the inner-leg-set classification.)

	// RFC-141 Phase 2 (P1a): the existential-filter predicates (e.g.
	// `proj.owner_id = e.id`) reference the ORIGINAL outer leg aliases (E / D),
	// but they run INSIDE the FlatMap inner where those leg aliases are no
	// longer bound — only `mergedOuterCorr` is (bound to the inner-join's
	// merged row). Rebase every outer-leg reference onto `mergedOuterCorr`,
	// resolving through the merged row's QUALIFIED "LEG.COL" key (the bare key
	// is last-leg-wins and would silently pick the wrong leg). The existential
	// alias references (P) are left untouched — they resolve against the inner
	// scan's own binding below the FOD. Without this rebase E.ID evaluates to
	// NULL ⇒ the correlation never matches ⇒ WHERE EXISTS drops every joined
	// row and NOT EXISTS admits all.
	if len(existPreds) > 0 {
		rebased := make([]predicates.QueryPredicate, len(existPreds))
		for i, p := range existPreds {
			rebased[i] = rebaseOuterLegRefsToMerged(p, outerLegAliases, mergedOuterCorr)
		}
		existPreds = rebased
	}

	var belowFOD plans.RecordQueryPlan = existPlan
	if len(existPreds) > 0 {
		belowFOD = plans.NewRecordQueryPredicatesFilterPlanWithAlias(existPlan, existPreds, existCorr)
	}
	fodPlan := plans.NewRecordQueryFirstOrDefaultPlan(belowFOD, values.NewNullValue(values.UnknownType))

	var flatMapInner plans.RecordQueryPlan = fodPlan
	if hasExistsFilter {
		cmp := predicates.Comparison{Type: predicates.ComparisonIsNotNull}
		if negated {
			cmp = predicates.Comparison{Type: predicates.ComparisonIsNull}
		}
		residual := predicates.NewComparisonPredicate(values.NewQuantifiedObjectValue(existCorr), cmp)
		flatMapInner = plans.NewRecordQueryPredicatesFilterPlanWithAlias(fodPlan, []predicates.QueryPredicate{residual}, existCorr)
	}

	// The FlatMap's result value.
	//
	//   - WHERE-EXISTS over a join (the original 3-quantifier shape): the
	//     existential level only FILTERS — a separate projection sits above. The
	//     result value is the identity over the merged outer row (QOV) so the
	//     inner-join's merged "ALIAS.COL" keys pass through unchanged.
	//
	//   - PROJECTED EXISTS over a JOIN in FROM (RFC-141): the projection
	//     (a RecordConstructor referencing the existential quantifier) was folded
	//     into sel.GetResultValue(). It MUST be computed HERE, at the FlatMap,
	//     with the existential FOD inner binding live — a projection above the
	//     FlatMap would read the ExistsValue with a dead binding (constant false +
	//     leaked columns). Rebase it onto the FlatMap's two bindings: leg columns
	//     (T1.ID, T2.ID) resolve against the merged outer row's QUALIFIED keys
	//     under mergedOuterCorr; the existential QOV (the projected ExistsValue's
	//     child, keyed by the existential QUANTIFIER alias) resolves against the
	//     inner FOD row under existCorr.
	flatMapResult := values.Value(values.NewQuantifiedObjectValue(mergedOuterCorr))
	if resultValueReferencesAlias(sel.GetResultValue(), quants[2].GetAlias()) {
		// Leg references → merged outer row's qualified keys. Use the COMPLETE
		// outer-leg alias set (not just {leftAlias,rightAlias}) so a projected
		// reference to a BURIED leg (a source under a non-rightmost lateral unnest)
		// resolves against the merged row's verbatim "LEG.COL" key, symmetrically with
		// the existential-residual rebase above. For a folded projection (the common
		// projected-EXISTS result value, AnchoredJoin=false) the set degenerates to
		// {leftAlias,rightAlias}, so this is a no-op for that path. RFC-142.
		projected := rebaseOuterLegValue(sel.GetResultValue(), outerLegAliases, mergedOuterCorr)
		// Existential quantifier alias → the FlatMap inner binding (existCorr).
		if quants[2].GetAlias() != existCorr {
			projected = values.RebaseValue(projected, values.AliasMap{quants[2].GetAlias(): existCorr})
		}
		flatMapResult = projected
	}

	flatMapPlan := plans.NewRecordQueryFlatMapPlan(
		innerJoinPlan, flatMapInner,
		mergedOuterCorr, existCorr,
		flatMapResult, false,
	)

	// Bind the wrapper quantifiers with the FlatMap plan's REAL outer/inner aliases
	// (mergedOuterCorr/existCorr), not fresh ones — same EXISTS correlation-leak fix
	// as buildExistsFlatMap above (the same leak class): a fresh outer alias fails to
	// subtract the FOD inner's correlation to mergedOuterCorr, leaking it upward.
	leftMemoRef := call.MemoizeExpression(leftExpr)
	fodWrapper := NewPhysicalFirstOrDefaultWrapper(fodPlan,
		expressions.NamedPhysicalQuantifier(existCorr, call.MemoizeExpression(existExpr)))
	innerQ := expressions.NamedPhysicalQuantifier(existCorr, call.MemoizeExpression(fodWrapper))
	call.Yield(newPhysicalFlatMapWrapper(
		flatMapPlan,
		expressions.NamedForEachQuantifier(mergedOuterCorr, leftMemoRef),
		innerQ,
	))
}

// tryExistsFlatMap implements an EXISTS subquery as a correlated FlatMap.
// It pushes the correlation predicate into a parameterized inner scan
// (PK or secondary index), then wraps that correlated inner in
// FirstOrDefault(NULL) and (for WHERE-EXISTS) a residual existential
// filter — the pure-map FlatMap shape (RFC-141). Inner residuals filter
// BELOW the FOD; the existential residual filters ABOVE it.
func (r *ImplementNestedLoopJoinRule) tryExistsFlatMap(
	call *ExpressionRuleCall,
	resultValue values.Value,
	outerPlan, innerPlan plans.RecordQueryPlan,
	outerAlias, innerAlias string,
	outerExpr, innerExpr expressions.RelationalExpression,
	hasExistsFilter, negated bool,
	preds []predicates.QueryPredicate,
) bool {
	innerScan, ok := innerPlan.(*plans.RecordQueryScanPlan)
	if !ok {
		return false
	}
	recordTypes := innerScan.GetRecordTypes()
	if len(recordTypes) != 1 {
		return false
	}

	innerPrefix := strings.ToUpper(innerAlias) + "."
	outerPrefix := strings.ToUpper(outerAlias) + "."

	// Try PK first.
	pkCols := call.Context.GetPrimaryKeyColumns(recordTypes[0])
	if len(pkCols) > 0 {
		pkCol := strings.ToUpper(pkCols[0])
		for _, pred := range preds {
			cp, ok := pred.(*predicates.ComparisonPredicate)
			if !ok || cp.Comparison.Type != predicates.ComparisonEquals {
				continue
			}
			if cp.Operand == nil || cp.Comparison.Operand == nil {
				continue
			}
			outerVal, _ := r.matchJoinPKPredicate(cp, outerPrefix, innerPrefix, pkCol)
			if outerVal == nil {
				continue
			}
			return r.buildExistsFlatMap(call, resultValue, outerPlan, innerScan, outerAlias, innerAlias, outerExpr, innerExpr, hasExistsFilter, negated, outerVal, pred, preds)
		}
	}

	// Try secondary indexes.
	for _, cand := range call.Context.GetMatchCandidates() {
		candCols := cand.GetColumnNames()
		if len(candCols) == 0 {
			continue
		}
		candTypes := cand.GetRecordTypes()
		if len(candTypes) == 0 || candTypes[0] != recordTypes[0] {
			continue
		}
		idxFirstCol := strings.ToUpper(candCols[0])
		for _, pred := range preds {
			cp, ok := pred.(*predicates.ComparisonPredicate)
			if !ok || cp.Comparison.Type != predicates.ComparisonEquals {
				continue
			}
			if cp.Operand == nil || cp.Comparison.Operand == nil {
				continue
			}
			outerVal, _ := r.matchJoinPKPredicate(cp, outerPrefix, innerPrefix, idxFirstCol)
			if outerVal == nil {
				continue
			}
			// Build correlated index scan.
			outerCorrelation := values.NamedCorrelationIdentifier(outerAlias)
			bareField := bareColumnName(outerVal, outerAlias)
			correlatedOperand := values.NewFieldValue(
				values.NewQuantifiedObjectValue(outerCorrelation),
				bareField, outerVal.Typ,
			)
			correlatedComp := &predicates.Comparison{Type: predicates.ComparisonEquals, Operand: correlatedOperand}
			cr := predicates.EmptyComparisonRange()
			mergeResult := cr.Merge(correlatedComp)
			if !mergeResult.Ok {
				continue
			}
			correlatedIndexScan := plans.NewRecordQueryIndexPlan(
				cand.CandidateName(),
				[]*predicates.ComparisonRange{mergeResult.Range},
				recordTypes, innerScan.GetFlowedType(), false,
			)

			existInnerCorr := values.NamedCorrelationIdentifier(innerAlias)
			var innerResiduals, outerResiduals []predicates.QueryPredicate
			for _, p := range preds {
				if p == pred {
					continue
				}
				if _, ok := predicates.GetCorrelatedToOfPredicate(p)[existInnerCorr]; ok {
					innerResiduals = append(innerResiduals, p)
				} else {
					outerResiduals = append(outerResiduals, p)
				}
			}

			r.yieldExistsFlatMap(call, resultValue, outerPlan, correlatedIndexScan,
				outerCorrelation, existInnerCorr, outerExpr, innerExpr,
				hasExistsFilter, negated, innerResiduals, outerResiduals)
			return true
		}
	}
	return false
}

func (r *ImplementNestedLoopJoinRule) buildExistsFlatMap(
	call *ExpressionRuleCall,
	resultValue values.Value,
	outerPlan plans.RecordQueryPlan, innerScan *plans.RecordQueryScanPlan,
	outerAlias, innerAlias string,
	outerExpr, innerExpr expressions.RelationalExpression,
	hasExistsFilter, negated bool,
	outerVal *values.FieldValue,
	matchedPred predicates.QueryPredicate,
	allPreds []predicates.QueryPredicate,
) bool {
	outerCorrelation := values.NamedCorrelationIdentifier(outerAlias)
	bareField := bareColumnName(outerVal, outerAlias)
	correlatedOperand := values.NewFieldValue(
		values.NewQuantifiedObjectValue(outerCorrelation),
		bareField, outerVal.Typ,
	)
	correlatedComp := &predicates.Comparison{Type: predicates.ComparisonEquals, Operand: correlatedOperand}
	cr := predicates.EmptyComparisonRange()
	mergeResult := cr.Merge(correlatedComp)
	if !mergeResult.Ok {
		return false
	}

	correlatedScan := innerScan.WithScanComparisons([]*predicates.ComparisonRange{mergeResult.Range})

	buildInnerCorr := values.NamedCorrelationIdentifier(innerAlias)
	var innerResiduals, outerResiduals []predicates.QueryPredicate
	for _, p := range allPreds {
		if p == matchedPred {
			continue
		}
		if _, ok := predicates.GetCorrelatedToOfPredicate(p)[buildInnerCorr]; ok {
			innerResiduals = append(innerResiduals, p)
		} else {
			outerResiduals = append(outerResiduals, p)
		}
	}

	r.yieldExistsFlatMap(call, resultValue, outerPlan, correlatedScan,
		outerCorrelation, buildInnerCorr, outerExpr, innerExpr,
		hasExistsFilter, negated, innerResiduals, outerResiduals)
	return true
}

// yieldExistsFlatMap assembles and yields the pure-map FlatMap for an
// EXISTS subquery whose correlation has been pushed into `correlatedInner`
// (a parameterized PK/index scan). The inner is wrapped:
//
//	correlatedInner [| inner-residual filter] | FirstOrDefault(NULL)
//	  [| residual existential filter (QOV IS NOT NULL / IS NULL)]
//
// The existential residual is omitted for a projected-only EXISTS
// (hasExistsFilter == false), where the boolean is computed by the map's
// resultValue instead.
func (r *ImplementNestedLoopJoinRule) yieldExistsFlatMap(
	call *ExpressionRuleCall,
	resultValue values.Value,
	outerPlan plans.RecordQueryPlan,
	correlatedInner plans.RecordQueryPlan,
	outerCorrelation, innerCorrelation values.CorrelationIdentifier,
	outerExpr, innerExpr expressions.RelationalExpression,
	hasExistsFilter, negated bool,
	innerResiduals, outerResiduals []predicates.QueryPredicate,
) {
	var belowFOD plans.RecordQueryPlan = correlatedInner
	if len(innerResiduals) > 0 {
		belowFOD = plans.NewRecordQueryPredicatesFilterPlanWithAlias(correlatedInner, innerResiduals, innerCorrelation)
	}
	fodPlan := plans.NewRecordQueryFirstOrDefaultPlan(belowFOD, values.NewNullValue(values.UnknownType))

	var flatMapInner plans.RecordQueryPlan = fodPlan
	if hasExistsFilter {
		cmp := predicates.Comparison{Type: predicates.ComparisonIsNotNull}
		if negated {
			cmp = predicates.Comparison{Type: predicates.ComparisonIsNull}
		}
		residual := predicates.NewComparisonPredicate(values.NewQuantifiedObjectValue(innerCorrelation), cmp)
		flatMapInner = plans.NewRecordQueryPredicatesFilterPlanWithAlias(fodPlan, []predicates.QueryPredicate{residual}, innerCorrelation)
	}

	var flatMapOuter plans.RecordQueryPlan = outerPlan
	if len(outerResiduals) > 0 {
		flatMapOuter = plans.NewRecordQueryPredicatesFilterPlanWithAlias(outerPlan, outerResiduals, outerCorrelation)
	}

	flatMapPlan := plans.NewRecordQueryFlatMapPlan(
		flatMapOuter, flatMapInner,
		outerCorrelation, innerCorrelation,
		resultValue, false,
	)
	// Bind the wrapper quantifiers with the FlatMap plan's REAL outer/inner aliases
	// (outerCorrelation/innerCorrelation), not fresh ones — the FOD inner reports
	// its correlation to outerCorrelation, so a fresh outer alias would fail to
	// subtract it and a completed correlated-EXISTS FlatMap would leak
	// outerCorrelation upward → misroute an enclosing multiway join (the EXISTS twin
	// of the yieldGeneralFlatMap:453-454 leak).
	leftQ := expressions.NamedForEachQuantifier(outerCorrelation, call.MemoizeExpression(outerExpr))
	fodWrapper := NewPhysicalFirstOrDefaultWrapper(fodPlan,
		expressions.NamedPhysicalQuantifier(innerCorrelation, call.MemoizeExpression(innerExpr)))
	rightQ := expressions.NamedPhysicalQuantifier(innerCorrelation, call.MemoizeExpression(fodWrapper))
	call.Yield(newPhysicalFlatMapWrapper(flatMapPlan, leftQ, rightQ))
}

// matchJoinPKPredicate checks if a comparison predicate matches the
// pattern outer.FK = inner.PK (or reversed). Returns the outer-side
// FieldValue and the inner column name if matched, nil otherwise.
func (r *ImplementNestedLoopJoinRule) matchJoinPKPredicate(
	cp *predicates.ComparisonPredicate,
	outerPrefix, innerPrefix, pkCol string,
) (*values.FieldValue, string) {
	lhsFV, lhsOk := cp.Operand.(*values.FieldValue)
	rhsFV, rhsOk := cp.Comparison.Operand.(*values.FieldValue)
	if !lhsOk || !rhsOk {
		return nil, ""
	}

	lhsAlias, lhsCol := fieldValueAliasAndCol(lhsFV)
	rhsAlias, rhsCol := fieldValueAliasAndCol(rhsFV)

	outerAlias := strings.TrimSuffix(outerPrefix, ".")
	innerAlias := strings.TrimSuffix(innerPrefix, ".")

	if lhsAlias == outerAlias && rhsAlias == innerAlias {
		if rhsCol == pkCol {
			return lhsFV, rhsCol
		}
	}
	if lhsAlias == innerAlias && rhsAlias == outerAlias {
		if lhsCol == pkCol {
			return rhsFV, lhsCol
		}
	}

	// Deep-flowed outer value: in a re-enumerated multi-way join the join key's
	// outer side may reference a table nested INSIDE the outer sub-join rather
	// than the outer quantifier itself (a 3-way chain's top join (T1⋈T2)⋈T3 has
	// predicate T3.t2_id = T2.id, where T2 is inside the (T1⋈T2) outer). The
	// outer's merged row exposes that column under its bare name
	// (the anchored join RC's last-leg-wins bare key, RFC-077 7.6; mergeRows
	// writes the same bare key at execution), and the caller
	// rebuilds the probe value as QOV(outerAlias).<bareCol> — which resolves
	// correctly per outer row. Accept when the INNER side is the index column
	// and the other side is any aliased (non-inner) field. This is what turns a
	// nested chain into a chain of index probes (RFC-042 L3).
	if lhsAlias == innerAlias && lhsCol == pkCol && rhsAlias != "" && rhsAlias != innerAlias {
		return rhsFV, lhsCol
	}
	if rhsAlias == innerAlias && rhsCol == pkCol && lhsAlias != "" && lhsAlias != innerAlias {
		return lhsFV, rhsCol
	}

	return nil, ""
}

func fieldValueAliasAndCol(fv *values.FieldValue) (alias, col string) {
	if qov, ok := fv.Child.(*values.QuantifiedObjectValue); ok {
		return strings.ToUpper(qov.Correlation.String()), strings.ToUpper(fv.Field)
	}
	upper := strings.ToUpper(fv.Field)
	if dot := strings.IndexByte(upper, '.'); dot >= 0 {
		return upper[:dot], upper[dot+1:]
	}
	return "", upper
}

// bareColumnName returns the unqualified column name from a FieldValue,
// stripping the table alias prefix when it matches expectedAlias. For
// QOV-based FieldValues the Field is already bare; for flat
// "ALIAS.col" strings, the alias is stripped via fieldValueAliasAndCol.
func bareColumnName(fv *values.FieldValue, expectedAlias string) string {
	if fv.Child != nil {
		return fv.Field
	}
	fvAlias, col := fieldValueAliasAndCol(fv)
	if fvAlias != "" && fvAlias == strings.ToUpper(expectedAlias) {
		return col
	}
	return fv.Field
}

var _ ExpressionRule = (*ImplementNestedLoopJoinRule)(nil)
