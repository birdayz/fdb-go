package cascades

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
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

// hasStrictSingleQuantifier reports whether an expression owns a semantic
// at-most-one-row edge. Rewrites that do not explicitly preserve and implement
// this carrier must treat it as a barrier; otherwise an ordinary ForEach rebuild
// can silently turn a scalar cardinality violation into fan-out.
func hasStrictSingleQuantifier(quantifiers []expressions.Quantifier) bool {
	for _, q := range quantifiers {
		if q.IsStrictSingle() {
			return true
		}
	}
	return false
}

func (r *ImplementNestedLoopJoinRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)

	quants := sel.GetQuantifiers()

	// StrictSingle has exactly one physical authority in this rule: the SQL
	// translator's scalar-subquery shape, LEFT OUTER [plain outer, strict right].
	// That path routes the flagged inner leg through buildCorrelatedFlatMapPlan's
	// strict FirstOrDefault compensation. The orientation and join kind are part
	// of the contract: applying the same null-defaulting compensation to an
	// INNER/CROSS join would turn an empty inner into a NULL row, while a strict
	// left leg is not an inner-per-outer scalar evaluation. Every other shape
	// must therefore fail closed before it can yield a competing or
	// semantics-changing plan.
	hasStrictSingle := hasStrictSingleQuantifier(quants)
	if hasStrictSingle {
		isSupportedStrictSingleShape := len(quants) == 2 &&
			allForEach(quants) &&
			sel.GetJoinType() == expressions.JoinLeftOuter &&
			!quants[0].IsStrictSingle() &&
			!quants[0].IsNullOnEmpty() &&
			quants[1].IsStrictSingle() &&
			!quants[1].IsNullOnEmpty()
		if !isSupportedStrictSingleShape {
			return
		}
	}

	// Two ForEach quantifiers plus one trailing Existential use the retained
	// projected/WHERE-EXISTS fold. Flat selects with more than two ForEach legs
	// are decomposed by PartitionSelectRule; RFC-190 retired this rule's N-way
	// implementation arm.
	if len(quants) == 3 &&
		quants[len(quants)-1].Kind() == expressions.QuantifierExistential &&
		allForEach(quants[:len(quants)-1]) {
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
	// Select join children through the COST-BEST physical member rather than the
	// first-yielded one. The NLJ embeds the child's plan DIRECTLY
	// (GetRecordQueryPlan, never WithChildren), so whatever is picked here is what
	// executes — first-member order is not a selection criterion.
	// (RFC-150 B1a: the join-leg 0-row bug `... AND t.a>1 AND t.fk=o.id AND u.x=t.x`,
	// where the first member was a nil-inner Fetch shell; RFC-183 removed that state
	// at its source, and cost-best selection remains correct on its own terms.)
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
	// The plan's leg identities are the SELECT'S QUANTIFIERS', threaded verbatim.
	// (Not leftQ/rightQ below — those are fresh memo quantifiers over the leg
	// EXPRESSIONS and carry minted aliases; what qualifies the merged row's keys is
	// the source leg's own correlation.)
	//
	// The plan used to carry leftAlias/rightAlias, the select's source-alias TEXT,
	// and the executor minted an identifier from it at the plan boundary. Those two
	// spellings are an empirical agreement, not a structural one — the source-alias
	// slice is populated independently of the quantifiers — so the substitution is
	// recorded rather than asserted, and its zero is what makes the retyping
	// representation-only.
	leftCorr, rightCorr := quants[0].GetAlias(), quants[1].GetAlias()
	if values.LegIdentityCensusEnabled() {
		values.RecordLegIdentityComparison(values.LegSiteNLJPlanAlias, leftAlias, leftCorr.Name())
		values.RecordLegIdentityComparison(values.LegSiteNLJPlanAlias, rightAlias, rightCorr.Name())
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
		leftQ := expressions.ForEachQuantifier(call.MemoizeExpression(leftExpr))
		rightQ := expressions.ForEachQuantifier(call.MemoizeExpression(rightExpr))
		// The materialized NLJ is its own cascades expression carrying its two leg
		// edges directly (RFC-184 W2, no physicalNestedLoopJoinWrapper) — both legs
		// are the live shared-group edges over the memoized leg exprs.
		call.Yield(plans.NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
			leftQ, rightQ,
			sel.GetPredicates(),
			joinType,
			leftCorr, rightCorr,
			sel.GetResultValue(),
		))
		return
	}

	// Correlated INNER/LEFT joins are implemented as a FlatMap (O(N×logM) via
	// the inner's correlated PK/index probes) by the leftDepsRight/rightDepsLeft
	// branches below; uncorrelated joins fall through to the materialized NLJ.
	// This is the SINGLE data-access-driven join path, matching Java (which has
	// no hand-rolled shortcut): PartitionBinary/PartitionSelectRule absorb the
	// join predicates into correlated sub-Selects and the data-access path
	// (MatchIntermediateRule → bindOrientedComparison) SARGs them into bare
	// correlated probes, so correlated index-nested-loop chains emerge from the
	// standard Cascades machinery (RFC-150 §8).
	//
	// This branch and the materialized one above share ONE identifier per leg —
	// leftCorr/rightCorr, threaded from the quantifiers near the top. It used to
	// re-mint them here from the source-alias text, giving the same leg two
	// spellings inside a single function while `physicalProvidedAliases` just below
	// read the quantifier's identifier directly. A second spelling of one leg is
	// how a downstream exact comparison matches the wrong window.

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
	leftStrictSingle := quants[0].IsStrictSingle()
	rightStrictSingle := quants[1].IsStrictSingle()
	canSwap := joinType != plans.JoinLeftOuter

	// StrictSingle is a semantic edge contract, not merely a hint attached to
	// syntactic correlation. Predicate simplification can erase the inner
	// reference to the outer row (`inner.fk = outer.id OR 1 = 1`) while the scalar
	// subquery must still enforce at-most-one row. Route such an edge through the
	// same FlatMap + strict FirstOrDefault compensation as an explicitly
	// correlated leg. In particular, do not yield the ordinary materialized NLJ:
	// it has no cardinality barrier and would be a competing fan-out plan in the
	// memo even when today's cost model happens to choose the strict alternative.
	leftNeedsRightEvaluation := leftDepsRight || leftStrictSingle
	rightNeedsLeftEvaluation := rightDepsLeft || rightStrictSingle
	requiresFlatMap := leftNeedsRightEvaluation || rightNeedsLeftEvaluation
	if !requiresFlatMap {
		// Incomplete-bipartition guard: if BOTH legs reference (via re-exposed
		// merge seeds) the SAME external table that is neither leg's own provided
		// alias, the two legs are connected through a sibling that this bipartition
		// excluded — e.g. the 3-way `d,e,p WHERE d.id=e.dept_id AND d.id=p.dept_id`
		// surfaces a {(d⋈e), p}-shaped select where both legs read d through a merge
		// RC. As a materialized NLJ that select is only valid as the INNER of a
		// FlatMap(d, …) that binds d; the cost model can otherwise pick it as a
		// standalone root with d unbound → 0 rows (TestFDB_DerivedTableExistsJoin
		// three-way). The leg's d-correlation is bound inside the leg's own merge select,
		// so neither the ordinary leg-dependency check nor the planner's
		// root-correlation check sees it.
		// Skipping the materialized NLJ here leaves the COMPLETE bipartitions (which
		// keep d as a real quantifier and produce the correct correlated FlatMap
		// chain) to win. A legitimate leg-to-leg correlation is handled by the
		// correlated FlatMap branch above; a true OUTER correlation is bound by
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
		// NOTE: a ChildrenAsSet-swapped firing (fireExprRuleOnMember) reuses
		// sel's resultValue verbatim under the swapped orientation, which is
		// unsound for a pristine ordinal seed IF some downstream consumer
		// later reads that seed's baked ordinals expecting the UNSWAPPED
		// physical layout (see materializedNLJOrdinalLayoutMatches's doc
		// comment for the full mechanism and its two false-positive
		// histories). No such consumer exists at THIS construction site —
		// this plan is embedded and yielded as-is, nothing here reads its
		// resultValue's ordinals — so no orientation check belongs here;
		// declining based on a mismatch that nothing will ever act on
		// rejected working, tested plans (TestFDB_QuotedMachineShapedAliases/
		// join_legs' swapped cross-join orientation, which never consumes its
		// own seed's ordinals downstream). The check instead lives at the
		// ACTUAL consumption site: implementJoinWithExistential's materialized
		// branch below, which immediately uses this same resultValue's
		// ordinal windows to rebase EXISTS predicates onto baked slots.
		leftQ := expressions.ForEachQuantifier(call.MemoizeExpression(leftExpr))
		rightQ := expressions.ForEachQuantifier(call.MemoizeExpression(rightExpr))
		// The materialized NLJ is its own cascades expression carrying its two leg
		// edges directly (RFC-184 W2, no physicalNestedLoopJoinWrapper) — both legs
		// are the live shared-group edges over the memoized leg exprs.
		call.Yield(plans.NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
			leftQ, rightQ,
			sel.GetPredicates(),
			joinType,
			leftCorr, rightCorr,
			sel.GetResultValue(),
		))
	}

	// Correlated FlatMap: for PartitionBinarySelectRule / RewriteOuterJoinRule output
	// where predicates are absorbed into sub-Selects creating correlation. The inner's
	// null-on-empty flag (set by RewriteOuterJoinRule for LEFT OUTER) drives the
	// DefaultOnEmpty null-extension inside yieldGeneralFlatMap.
	if leftNeedsRightEvaluation && !rightNeedsLeftEvaluation && canSwap {
		r.yieldGeneralFlatMap(call, sel,
			rightPlan, leftPlan, rightCorr, leftCorr,
			rightExpr, leftExpr, rightRef, leftRef, joinType,
			selQuantifierIsNullOnEmpty(sel, leftCorr),
			leftStrictSingle)
	} else if rightNeedsLeftEvaluation && !leftNeedsRightEvaluation {
		r.yieldGeneralFlatMap(call, sel,
			leftPlan, rightPlan, leftCorr, rightCorr,
			leftExpr, rightExpr, leftRef, rightRef, joinType,
			selQuantifierIsNullOnEmpty(sel, rightCorr),
			rightStrictSingle)
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
// BURIED merge leg (not the merge alias itself) is invisible to the leg-dependency
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
// in targetSet (the OTHER leg's provided aliases) via Reference.GetCorrelatedTo.
// (An ordinal seed's correlations are reported directly by GetCorrelatedTo —
// no re-exposure walk is needed.)
func legReferencesAny(ref *expressions.Reference, targetSet map[values.CorrelationIdentifier]struct{}) bool {
	for a := range ref.GetCorrelatedTo() {
		if _, ok := targetSet[a]; ok {
			return true
		}
	}
	return false
}

// legExternalAliases returns the aliases a leg subtree REFERENCES that it does
// NOT itself provide — its dangling external dependencies. Used by the
// incomplete-bipartition guard: when BOTH legs of a would-be materialized NLJ
// share an external alias, that alias is an excluded sibling table the two legs
// join through, so the materialized NLJ is unsafe as a standalone root (the
// sibling is unbound).
func legExternalAliases(ref *expressions.Reference, provided map[values.CorrelationIdentifier]struct{}) map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	for a := range ref.GetCorrelatedTo() {
		if _, ok := provided[a]; !ok {
			out[a] = struct{}{}
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
	outerSourceRef, innerSourceRef *expressions.Reference,
	joinType plans.JoinType,
	innerNullOnEmpty bool,
	innerStrictSingle bool,
) {
	flatMapPlan, _, _, ok := buildCorrelatedFlatMapPlan(
		call,
		flattenAndPredicates(sel.GetPredicates()), sel.GetResultValue(),
		outerPlan, innerPlan, outerCorr, innerCorr, outerExpr, innerExpr,
		joinType, innerNullOnEmpty, innerStrictSingle, false,
	)
	if !ok {
		return
	}
	// The FlatMap plan already carries its outer/inner memo quantifiers (RFC-184
	// W2, no physicalFlatMapWrapper) — yield it directly.
	rebuild := func(
		orderedOuter, orderedInner expressions.RelationalExpression,
	) expressions.RelationalExpression {
		outerPhysical, outerOK := orderedOuter.(physicalPlanExpression)
		innerPhysical, innerOK := orderedInner.(physicalPlanExpression)
		if !outerOK || !innerOK {
			return nil
		}
		rebuilt, _, _, rebuiltOK := buildCorrelatedFlatMapPlan(
			call,
			flattenAndPredicates(sel.GetPredicates()), sel.GetResultValue(),
			outerPhysical.GetRecordQueryPlan(), innerPhysical.GetRecordQueryPlan(),
			outerCorr, innerCorr, orderedOuter, orderedInner,
			joinType, innerNullOnEmpty, innerStrictSingle, true,
		)
		if !rebuiltOK {
			return nil
		}
		return rebuilt
	}
	r.yieldBinaryJoinWithSourceOrderingVariants(
		call, flatMapPlan, outerSourceRef, innerSourceRef, rebuild)
}

// joinLegOrderingVariant is one concrete physical child candidate together
// with the ordering it exposes after being pulled through the join's result
// value. maxCardinalityOne is a per-expression proof: equivalent plans can
// differ in how strongly that semantic bound is proven.
type joinLegOrderingVariant struct {
	expr              expressions.RelationalExpression
	sourceOrdering    *properties.RichOrdering
	pulledOrdering    *properties.RichOrdering
	maxCardinalityOne bool
}

type joinLegOrderingPair struct {
	outer expressions.RelationalExpression
	inner expressions.RelationalExpression
}

// yieldBinaryJoinWithOrderingVariants keeps the existing cost-best join as
// Go's plannability/fallback alternative, then ports Java's requested-order
// sensitive FlatMap cases as additive, exact-child variants:
//
//  1. a max-one outer lets the inner determine the result ordering;
//     2a. otherwise the outer alone may satisfy the request;
//     2b. a distinct outer ordering may be concatenated with the inner.
//
// Each ordered variant freezes BOTH selected legs in private final references.
// A join is a two-child, non-delegating operator, so pinOrderedSpine cannot pin
// it after the fact; freezing at construction is what prevents extraction from
// swapping an ordered child for the shared group's cheaper unordered winner
// after an enclosing sort has been removed.
func (r *ImplementNestedLoopJoinRule) yieldBinaryJoinWithOrderingVariants(
	call *ExpressionRuleCall,
	base expressions.RelationalExpression,
) {
	quantifiers := base.GetQuantifiers()
	if len(quantifiers) != 2 {
		call.Yield(base)
		return
	}
	r.yieldBinaryJoinWithSourceOrderingVariants(
		call, base,
		quantifiers[0].GetRangesOver(),
		quantifiers[1].GetRangesOver(),
		nil,
	)
}

// yieldBinaryJoinWithSourceOrderingVariants is the source-aware core. The
// default path rebuilds a FlatMap by replacing its two child quantifiers. A
// caller that added compensation wrappers supplies the original source groups
// plus rebuild, which recreates the complete filter/FOD/DOE chain around each
// selected pair instead of trying to swap a leaf that the frozen chain no
// longer exposes.
func (r *ImplementNestedLoopJoinRule) yieldBinaryJoinWithSourceOrderingVariants(
	call *ExpressionRuleCall,
	base expressions.RelationalExpression,
	outerRef, innerRef *expressions.Reference,
	rebuild func(outer, inner expressions.RelationalExpression) expressions.RelationalExpression,
) {
	call.Yield(base)

	requestedOrderings := call.GetRequestedOrderings()
	if len(requestedOrderings) == 0 {
		return
	}
	basePhysical, ok := base.(physicalPlanExpression)
	if !ok || basePhysical.GetRecordQueryPlan() == nil {
		return
	}
	if materialized, ok := base.(*plans.RecordQueryNestedLoopJoinPlan); ok &&
		materialized.GetJoinType() == plans.JoinFullOuter {
		return
	}

	quantifiers := base.GetQuantifiers()
	if len(quantifiers) != 2 {
		return
	}
	if outerRef == nil || innerRef == nil {
		return
	}
	resultValue := base.GetResultValue()
	if resultValue == nil {
		return
	}
	outerOrderingResultValue := resultValue
	if flatMap, ok := base.(*plans.RecordQueryFlatMapPlan); ok {
		outerOrderingResultValue = flatMapOrderingResultForChild(
			flatMap, quantifiers[0].GetAlias(), true)
	}
	localAliases := map[values.CorrelationIdentifier]struct{}{
		quantifiers[0].GetAlias(): {},
		quantifiers[1].GetAlias(): {},
	}

	less := lessWithHashTieBreak(call.CostModel())
	for _, requested := range requestedOrderings {
		if requested == nil || requested.IsPreserve() {
			continue
		}

		outerRequested := pushRequestedOrderingToSelectChild(
			requested, outerOrderingResultValue,
			quantifiers[0].GetAlias(), localAliases)
		innerRequested := pushRequestedOrderingToSelectChild(
			requested, resultValue, quantifiers[1].GetAlias(), localAliases)

		// The raw sets supply the leg whose ordering is irrelevant in a case.
		// The ordered sets pin each unary delegation spine against the
		// child-space request before the join freezes the selected top member.
		rawOuters := collectJoinLegOrderingVariants(
			outerRef, properties.PreserveOrdering(), outerOrderingResultValue,
			quantifiers[0].GetAlias(), less, false, call.Context)
		rawInners := collectJoinLegOrderingVariants(
			innerRef, properties.PreserveOrdering(), resultValue,
			quantifiers[1].GetAlias(), less, false, call.Context)
		orderedOuters := collectJoinLegOrderingVariants(
			outerRef, outerRequested, outerOrderingResultValue,
			quantifiers[0].GetAlias(), less, true, call.Context)
		orderedInners := collectJoinLegOrderingVariants(
			innerRef, innerRequested, resultValue,
			quantifiers[1].GetAlias(), less, true, call.Context)
		for _, pair := range orderedJoinLegPairs(
			rawOuters, rawInners, orderedOuters, orderedInners,
			requested, less,
		) {
			r.yieldVerifiedOrderedJoin(
				call, base, pair.outer, pair.inner, requested, rebuild)
		}
	}
}

// collectJoinLegOrderingVariants returns physical members in deterministic
// final-then-exploratory order. When pinOrdering is true, each member's unary
// order-preserving spine is pinned against requestedInChildSpace. A preserve
// request means no top-level key mapped to this child; in that unusual case we
// pin against the member's own directional ordering before using it as an
// ordering contributor.
func collectJoinLegOrderingVariants(
	ref *expressions.Reference,
	requestedInChildSpace *properties.RequestedOrdering,
	resultValue values.Value,
	childAlias values.CorrelationIdentifier,
	less func(a, b expressions.RelationalExpression) bool,
	pinOrdering bool,
	ctx PlanContext,
) []joinLegOrderingVariant {
	if ref == nil {
		return nil
	}
	members := make([]expressions.RelationalExpression, 0, len(ref.AllMembers()))
	members = append(members, ref.FinalMembers()...)
	members = append(members, ref.Members()...)
	if pinOrdering && requestedInChildSpace != nil &&
		!requestedInChildSpace.IsPreserve() && ctx != nil {
		members = append(members, orderedFullScanAlternatives(
			ref, requestedInChildSpace, ctx)...)
		// A requested-order data-access alternative can be discovered after
		// this join leg's ordinary winner has already been pruned. The
		// Reference retains its PartialMatches, though, so realize the same
		// candidate-local scans the planner's data-access boundary would have
		// produced and consider physical results directly. This mirrors Java's
		// event-driven re-fire without mutating the shared child group's member
		// set or reviving unrelated pruned members.
		for _, candidate := range dataAccessCandidates(ref) {
			matches := GetPartialMatchesForCandidate(ref, candidate)
			for _, expr := range DataAccessForMatchPartition(
				[]*properties.RequestedOrdering{requestedInChildSpace},
				matches,
				ctx,
				nil,
			) {
				if isPhysical(expr) {
					members = append(members, expr)
				}
			}
		}
	}

	var result []joinLegOrderingVariant
	for _, member := range members {
		ph, ok := member.(physicalPlanExpression)
		if !ok || ph.GetRecordQueryPlan() == nil {
			continue
		}
		// Classify max-one on the original member, while its child reference
		// still carries the populated property map. pinOrderedSpine rebuilds
		// wrappers over private singleton refs (intentionally property-map-free);
		// recomputing a Filter's cardinality there would weaken a valid max-one
		// proof to unknown.
		cardinalities := computeCardinalities(ph, ph.GetRecordQueryPlan())
		maxCardinality := cardinalities.GetMaxCardinality()
		maxCardinalityOne := !maxCardinality.IsUnknown() &&
			maxCardinality.Value() == 1

		selected := member
		if pinOrdering {
			pinRequest := requestedInChildSpace
			if pinRequest == nil || pinRequest.IsPreserve() {
				pinRequest = requestedOrderingForProvided(
					computeWrapperRichOrdering(ph))
			}
			if pinRequest != nil && !pinRequest.IsPreserve() {
				selected = pinOrderedSpine(member, pinRequest, less)
				if selected == nil || !memberSatisfiesOrdering(selected, pinRequest) {
					continue
				}
				ph = selected.(physicalPlanExpression)
			}
		}

		provided := computeWrapperRichOrdering(ph)
		if provided == nil {
			continue
		}
		pulled := provided.PullUpThroughValue(resultValue, childAlias)
		if pulled == nil {
			pulled = properties.EmptyOrdering()
		}
		result = append(result, joinLegOrderingVariant{
			expr:              selected,
			sourceOrdering:    provided,
			pulledOrdering:    pulled,
			maxCardinalityOne: maxCardinalityOne,
		})
	}
	return result
}

// requestedOrderingForProvided produces one concrete child-space request that
// pins the directional sequence already exposed by a member. Fixed bindings
// are omitted because they consume no sort position.
func requestedOrderingForProvided(
	ordering *properties.RichOrdering,
) *properties.RequestedOrdering {
	if ordering == nil {
		return properties.PreserveOrdering()
	}
	var parts []properties.RequestedOrderingPart
	for _, key := range ordering.GetKeys() {
		sortOrder := properties.SortOrderOf(ordering.GetBindingMap()[key])
		if !sortOrder.IsDirectional() {
			continue
		}
		parts = append(parts, properties.RequestedOrderingPart{
			Value:     key,
			SortOrder: sortOrder.ToRequestedSortOrder(),
		})
	}
	if len(parts) == 0 {
		return properties.PreserveOrdering()
	}
	return properties.NewRequestedOrdering(
		parts, properties.DistinctnessPreserveDistinctness, false)
}

func bestJoinLegVariant(
	variants []joinLegOrderingVariant,
	eligible func(joinLegOrderingVariant) bool,
	less func(a, b expressions.RelationalExpression) bool,
) *joinLegOrderingVariant {
	var best *joinLegOrderingVariant
	for i := range variants {
		candidate := &variants[i]
		if !eligible(*candidate) {
			continue
		}
		if best == nil || less(candidate.expr, best.expr) {
			best = candidate
		}
	}
	return best
}

// orderedJoinLegPairs mirrors Java ImplementNestedLoopJoinRule's ordering
// partition matrix while freezing one cheapest exact expression per retained
// source-ordering partition:
//
//   - Case 1 rolls all max-one outers together; exhaustive requests retain
//     every satisfying inner ordering partition.
//   - Case 2a rolls satisfying outers together unless DISTINCT was requested
//     (exhaustiveness deliberately does not affect this case).
//   - Case 2b always retains every viable distinct-outer ordering partition;
//     exhaustive requests additionally retain every satisfying inner ordering
//     partition for each outer.
//
// Java partitions by the child's source Ordering property before pulling that
// ordering through the join result. sourceOrdering therefore drives grouping;
// pulledOrdering only decides which case and whether the top request is met.
func orderedJoinLegPairs(
	rawOuters, rawInners []joinLegOrderingVariant,
	orderedOuters, orderedInners []joinLegOrderingVariant,
	requested *properties.RequestedOrdering,
	less func(a, b expressions.RelationalExpression) bool,
) []joinLegOrderingPair {
	if requested == nil || requested.IsPreserve() {
		return nil
	}

	var result []joinLegOrderingPair
	add := func(outer, inner expressions.RelationalExpression) {
		if outer == nil || inner == nil {
			return
		}
		for _, existing := range result {
			if physicalExpressionsEqual(existing.outer, outer) &&
				physicalExpressionsEqual(existing.inner, inner) {
				return
			}
		}
		result = append(result, joinLegOrderingPair{outer: outer, inner: inner})
	}

	// Case 1: one rolled-up max-one outer combined with either one rolled-up
	// satisfying inner (non-exhaustive) or one cheapest inner per source
	// ordering partition (exhaustive).
	caseOneOuter := bestJoinLegVariant(rawOuters,
		func(v joinLegOrderingVariant) bool { return v.maxCardinalityOne }, less)
	caseOneInners := selectJoinLegOrderingVariants(
		orderedInners,
		func(v joinLegOrderingVariant) bool {
			return v.pulledOrdering != nil &&
				v.pulledOrdering.Satisfies(requested)
		},
		requested.IsExhaustive(),
		less,
	)
	if caseOneOuter != nil {
		for _, inner := range caseOneInners {
			add(caseOneOuter.expr, inner.expr)
		}
	}

	// Case 2a: the outer alone satisfies. Java retains one source-ordering
	// partition per satisfying outer only for a DISTINCT request; otherwise
	// all satisfying outers are rolled together, irrespective of exhaustive.
	caseTwoAOuters := selectJoinLegOrderingVariants(
		orderedOuters,
		func(v joinLegOrderingVariant) bool {
			return !v.maxCardinalityOne &&
				v.pulledOrdering != nil &&
				v.pulledOrdering.Satisfies(requested)
		},
		requested.IsDistinct(),
		less,
	)
	caseTwoAInner := bestJoinLegVariant(rawInners,
		func(v joinLegOrderingVariant) bool {
			// ConcatOrderings takes distinctness from the right ordering. Java
			// enumerates the rolled-up inner partition here; selecting a
			// concrete exact child must retain a distinct member when the top
			// request requires distinctness, or final verification would
			// discard a valid partition merely because its cheapest member was
			// non-distinct.
			return !requested.IsDistinct() ||
				(v.pulledOrdering != nil && v.pulledOrdering.IsDistinct())
		},
		less,
	)
	if caseTwoAInner != nil {
		for _, outer := range caseTwoAOuters {
			add(outer.expr, caseTwoAInner.expr)
		}
	}

	// Case 2b: every distinct outer ordering that fails alone remains a
	// separate alternative. For each outer, retain one rolled-up satisfying
	// inner or every satisfying inner ordering partition when exhaustive.
	caseTwoBOuters := selectJoinLegOrderingVariants(
		orderedOuters,
		func(v joinLegOrderingVariant) bool {
			return !v.maxCardinalityOne &&
				v.pulledOrdering != nil &&
				!v.pulledOrdering.Satisfies(requested) &&
				v.pulledOrdering.IsDistinct()
		},
		true,
		less,
	)
	for _, outer := range caseTwoBOuters {
		caseTwoBInners := selectJoinLegOrderingVariants(
			orderedInners,
			func(inner joinLegOrderingVariant) bool {
				if inner.pulledOrdering == nil {
					return false
				}
				return properties.ConcatOrderings(
					outer.pulledOrdering, inner.pulledOrdering,
				).Satisfies(requested)
			},
			requested.IsExhaustive(),
			less,
		)
		for _, inner := range caseTwoBInners {
			add(outer.expr, inner.expr)
		}
	}
	return result
}

// selectJoinLegOrderingVariants returns either the single cheapest eligible
// expression or, when retainPartitions is true, the cheapest expression from
// each structurally-equal source-ordering partition. It preserves first-seen
// partition order and replaces only the representative within that partition.
func selectJoinLegOrderingVariants(
	variants []joinLegOrderingVariant,
	eligible func(joinLegOrderingVariant) bool,
	retainPartitions bool,
	less func(a, b expressions.RelationalExpression) bool,
) []joinLegOrderingVariant {
	if !retainPartitions {
		best := bestJoinLegVariant(variants, eligible, less)
		if best == nil {
			return nil
		}
		return []joinLegOrderingVariant{*best}
	}

	var result []joinLegOrderingVariant
	for _, candidate := range variants {
		if !eligible(candidate) {
			continue
		}
		found := -1
		for i := range result {
			if richOrderingsStructurallyEqual(
				result[i].sourceOrdering, candidate.sourceOrdering,
			) {
				found = i
				break
			}
		}
		if found < 0 {
			result = append(result, candidate)
		} else if less(candidate.expr, result[found].expr) {
			result[found] = candidate
		}
	}
	return result
}

func richOrderingsStructurallyEqual(
	left, right *properties.RichOrdering,
) bool {
	if left == nil || right == nil {
		return left == right
	}
	if left.IsDistinct() != right.IsDistinct() ||
		!left.OrderingSet().Equal(right.OrderingSet()) ||
		len(left.GetBindingMap()) != len(right.GetBindingMap()) {
		return false
	}
	for _, key := range left.OrderingSet().Set() {
		leftValue := left.ValueForKey(key)
		rightValue := right.ValueForKey(key)
		if leftValue == nil || rightValue == nil ||
			!values.SemanticEqualsUnderAliasMap(leftValue, rightValue, nil) ||
			!orderingBindingsStructurallyEqual(
				left.GetBindingMap()[leftValue],
				right.GetBindingMap()[rightValue],
			) {
			return false
		}
	}
	return true
}

func orderingBindingsStructurallyEqual(
	left, right []properties.OrderingBinding,
) bool {
	if len(left) != len(right) {
		return false
	}
	used := make([]bool, len(right))
	for _, leftBinding := range left {
		found := false
		for i, rightBinding := range right {
			if used[i] ||
				!orderingBindingStructurallyEqual(leftBinding, rightBinding) {
				continue
			}
			used[i] = true
			found = true
			break
		}
		if !found {
			return false
		}
	}
	return true
}

func orderingBindingStructurallyEqual(
	left, right properties.OrderingBinding,
) bool {
	if left.IsSorted() != right.IsSorted() ||
		left.IsFixed() != right.IsFixed() ||
		left.IsChoose() != right.IsChoose() ||
		left.GetSortOrder() != right.GetSortOrder() {
		return false
	}
	leftComparison := left.GetComparison()
	rightComparison := right.GetComparison()
	switch typedLeft := leftComparison.(type) {
	case *predicates.Comparison:
		typedRight, ok := rightComparison.(*predicates.Comparison)
		return ok && comparisonsEqual(typedLeft, typedRight)
	case *predicates.ComparisonRange:
		typedRight, ok := rightComparison.(*predicates.ComparisonRange)
		return ok && partialMatchComparisonRangesEqual(typedLeft, typedRight)
	case values.Value:
		typedRight, ok := rightComparison.(values.Value)
		return ok && values.SemanticEqualsUnderAliasMap(
			typedLeft, typedRight, nil)
	default:
		return reflect.DeepEqual(leftComparison, rightComparison)
	}
}

func physicalExpressionsEqual(
	left, right expressions.RelationalExpression,
) bool {
	leftPhysical, leftOK := left.(physicalPlanExpression)
	rightPhysical, rightOK := right.(physicalPlanExpression)
	return leftOK && rightOK &&
		plans.Equals(
			leftPhysical.GetRecordQueryPlan(),
			rightPhysical.GetRecordQueryPlan(),
		)
}

func rebuildJoinWithExactLegs(
	call *ExpressionRuleCall,
	base expressions.RelationalExpression,
	outer, inner expressions.RelationalExpression,
) expressions.RelationalExpression {
	quantifiers := base.GetQuantifiers()
	if len(quantifiers) != 2 || outer == nil || inner == nil {
		return nil
	}
	exactQuantifiers := []expressions.Quantifier{
		expressions.RebuildQuantifier(
			quantifiers[0], call.MemoizeFinalExpression(outer)),
		expressions.RebuildQuantifier(
			quantifiers[1], call.MemoizeFinalExpression(inner)),
	}
	switch plan := base.(type) {
	case *plans.RecordQueryFlatMapPlan:
		return plan.WithQuantifiers(exactQuantifiers)
	default:
		return nil
	}
}

func rebuildOrderedJoin(
	call *ExpressionRuleCall,
	base expressions.RelationalExpression,
	outer, inner expressions.RelationalExpression,
	rebuild func(outer, inner expressions.RelationalExpression) expressions.RelationalExpression,
) expressions.RelationalExpression {
	if rebuild != nil {
		return rebuild(outer, inner)
	}
	return rebuildJoinWithExactLegs(call, base, outer, inner)
}

func orderedJoinSatisfies(
	candidate expressions.RelationalExpression,
	requested *properties.RequestedOrdering,
) bool {
	ph, ok := candidate.(physicalPlanExpression)
	if !ok {
		return false
	}
	ordering := computeWrapperRichOrdering(ph)
	return ordering != nil && ordering.Satisfies(requested)
}

func (r *ImplementNestedLoopJoinRule) yieldVerifiedOrderedJoin(
	call *ExpressionRuleCall,
	base expressions.RelationalExpression,
	outer, inner expressions.RelationalExpression,
	requested *properties.RequestedOrdering,
	rebuild func(outer, inner expressions.RelationalExpression) expressions.RelationalExpression,
) {
	candidate := rebuildOrderedJoin(call, base, outer, inner, rebuild)
	if candidate != nil && orderedJoinSatisfies(candidate, requested) {
		call.Yield(candidate)
	}
}

// buildCorrelatedFlatMapPlan constructs the correlated-FlatMap join plan —
// the per-quantifier-property lowering shared by the 2-quantifier
// leftDepsRight/rightDepsLeft branches (yieldGeneralFlatMap) and the
// 3-quantifier existential arm's step-1 (implementJoinWithExistential): the
// inner leg re-executes per outer row with the outer bound under outerCorr;
// a null-on-empty inner wraps in DefaultOnEmpty (Java's
// planPartitionToPhysical); a strict-single inner wraps in the strict
// FirstOrDefault.
//
// Returns the plan plus the outer and inner quantifiers the caller's FlatMap
// wrapper must range over, and ok=false when the fail-closed buried-reference
// verifier declines. The quantifiers are returned rather than built by the
// caller because every compensating operator this helper adds (the join-pred
// filter, the FirstOrDefault/DefaultOnEmpty wrap, the outer-pred filter) is
// MEMOIZED here and its quantifier ADVANCES in lockstep with the plan — so the
// memo costs the expression that actually executes. Building the quantifiers
// outside, over the raw outerExpr/innerExpr, is exactly the RFC-183 §11/§12
// defect: the plan pointer holds the compensated chain while the quantifier
// holds the uncompensated input, which both under-prices the join by the
// selectivity of the filters the memo cannot see and blocks the wrapper
// deletion (collapsing to the quantifier would silently drop the
// DefaultOnEmpty — wrong outer-join NULLs — and the residual filters).
func buildCorrelatedFlatMapPlan(
	call *ExpressionRuleCall,
	preds []predicates.QueryPredicate,
	resultValue values.Value,
	outerPlan, innerPlan plans.RecordQueryPlan,
	outerCorr, innerCorr values.CorrelationIdentifier,
	outerExpr, innerExpr expressions.RelationalExpression,
	joinType plans.JoinType,
	innerNullOnEmpty bool,
	innerStrictSingle bool,
	freezeLegs bool,
) (*plans.RecordQueryFlatMapPlan, expressions.Quantifier, expressions.Quantifier, bool) {
	var outerPreds, joinPreds []predicates.QueryPredicate
	for _, pred := range preds {
		corrSet := predicates.GetCorrelatedToOfPredicate(pred)
		if corrSet == nil {
			corrSet = map[values.CorrelationIdentifier]struct{}{}
		}
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
	// correlation the merge machinery never rebases. A
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
		legLayout := buriedLegOrdinalLayout(outerPlan)
		origInnerPlan := innerPlan
		innerPlan = rebasePlanBuriedRefs(innerPlan, buriedLegAliases, outerCorr, legLayout, nil)
		for i, p := range joinPreds {
			joinPreds[i] = rebaseOuterLegRefsToMerged(p, buriedLegAliases, outerCorr, legLayout, nil)
		}
		if planReferencesAnyBuriedAlias(innerPlan, buriedLegAliases) || predsReferenceAlias(joinPreds, buriedAliasUpperSet(buriedLegAliases)) {
			return nil, expressions.Quantifier{}, expressions.Quantifier{}, false
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

	// The base quantifiers, from which the compensating chains below advance.
	//
	// ALIAS CONTRACT — every quantifier here, base AND advancing, carries the
	// FlatMap plan's ACTUAL outer/inner correlation alias (outerCorr/innerCorr).
	// NOT fresh aliases. The inner probe reports (D.2) its correlation to the
	// bound outer alias; the Reference.GetCorrelatedTo aggregation subtracts each
	// member's quantifier aliases from its children's correlations, so a fresh
	// alias fails to subtract the bound outer/inner aliases and a COMPLETED
	// (self-contained) inner join leaks them as if externally correlated → an
	// upper multiway join sees the subplan as still correlated and skips/misroutes
	// valid alternatives. A FlatMap that binds X is not correlated to X; binding
	// with the real aliases makes the aggregation report so. (The correlated-
	// EXISTS paths use buildExistsCompensationChain's preserve-alias mode: outer
	// via the named outer alias, inner via NamedPhysicalQuantifier(inner alias)
	// over the FOD wrapper.)
	//
	// This is the OPPOSITE of implementExistentialSelect's fresh-alias mode, and
	// deliberately so — do not "harmonize" them. There the advancing quantifiers
	// must mint fresh aliases, because that chain stacks TWO PredicatesFilters
	// (below-FOD join preds, above-FOD existential residual) whose alias-aware
	// memo interning collapses them into one if they share an alias, silently
	// dropping a predicate. HERE each chain holds at most one filter plus one
	// distinct FOD/DOE wrap, so nothing can intern away, and the correlation
	// bookkeeping above requires the real aliases. Two sites, two contracts, both
	// load-bearing.
	//
	// The inner base ranges over innerExprForMemo, never innerExpr: when the
	// buried-leg rebase above rewrote the inner, the memoized expression must
	// report the REBASED correlations (see the block comment at that rebase).
	memoizeLeg := call.MemoizeExpression
	if freezeLegs {
		memoizeLeg = call.MemoizeFinalExpression
	}
	outerQ := expressions.NamedForEachQuantifier(outerCorr, memoizeLeg(outerExpr))
	innerQ := expressions.NamedForEachQuantifier(innerCorr, memoizeLeg(innerExprForMemo))

	// LEFT-OUTER null-extension, the Java way (ImplementNestedLoopJoinRule.java:310-330
	// / ImplementSimpleSelectRule:100-109): wrap the inner in DefaultOnEmpty so a
	// non-matching outer row yields one all-NULL inner row instead of being dropped.
	// The FlatMap stays a PURE map (no leftOuter flag) — the outer-join semantics are
	// emergent from this wrapper, exactly like Java's FlatMap, and DefaultOnEmpty's
	// OrElse continuation carries the empty-vs-nonempty decision so the null-extension
	// is resume-safe across pages (the prior in-memory leftOuter flag re-decided this
	// from scratch on every resume → spurious null rows / paging that never advanced).
	// Two sources land here: a quantifier RewriteOuterJoinRule marked null-on-empty
	// (innerNullOnEmpty), and a directly LEFT-OUTER join type — both are LEFT OUTER and
	// both lower identically.
	//
	// The ON-predicates already sit BELOW this boundary (inside the correlated inner
	// SUBSEL / pushed onto the inner probe), so they filter before the null-fill —
	// correct LEFT-OUTER semantics.
	//
	// Predicate placement follows Java's planPartitionToPhysical: the wrap
	// (DefaultOnEmpty) comes FIRST, the select-level predicates filter ABOVE
	// it. Every select-level predicate reaching a null-on-empty inner is
	// WHERE-class (the ON-predicates live inside the leg subsel / inner probe),
	// so it must see the null-extended row and drop it on a non-matching
	// comparison (`… LEFT JOIN e ON … WHERE e.fname = 'x'` drops the
	// null-extended rows; placed below the wrap, the filter ran before the
	// null-fill and the extended row survived unfiltered — rows Java drops). The
	// strict-single (scalar subquery) wrap keeps its predicates BELOW: they are
	// the subquery's own correlation, part of the subquery body the
	// at-most-one-row check applies to.
	nullOnEmpty := innerNullOnEmpty || joinType == plans.JoinLeftOuter
	var innerWrapped plans.RecordQueryPlan = innerPlan
	if innerStrictSingle {
		if len(joinPreds) > 0 {
			fpInnerQ := expressions.NamedForEachQuantifier(innerQ.GetAlias(),
				call.MemoizeFinalExpression(innerWrapped))
			filterPlan := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
				fpInnerQ, joinPreds, innerCorr,
			)
			innerWrapped = filterPlan
			innerQ = expressions.NamedForEachQuantifier(innerCorr,
				call.MemoizeFinalExpression(filterPlan))
		}
		// Correlated scalar subquery, no user LIMIT: enforce SQL at-most-one-row.
		// A strict FirstOrDefault collapses the inner to one row per outer (NULL
		// default on empty) AND raises 21000 on a second row. It is a non-pushable
		// barrier (unlike a LIMIT the planner could push into the scan) and, because
		// the FlatMap re-executes the inner per outer row, the check runs fresh per
		// outer row. The FirstOrDefault already supplies the empty→NULL row, so this
		// fully handles the strict scalar case without any leftOuter mechanism.
		// FirstOrDefault collapses onto a DISENTANGLED FINAL edge holding the
		// concrete correlated inner (constraint-preserving disentangle,
		// RFC-184 W2). The frozen edge keeps innerCorr — so GetResultValue and
		// derivations are unchanged — but ranges over a PRIVATE single-member
		// reference over innerWrapped, NOT the shared exploratory group. That is
		// the whole point on the correlated (DML DELETE/UPDATE-WHERE-EXISTS) path:
		// planFromQuantifier resolves the SARG/correlated member, never the bare
		// group winner the prior generic collapse floated to (which dropped the
		// filter and deleted all rows). Extraction recurses through the frozen
		// snapshot chain and reconstructs the correlated inner faithfully.
		//
		// The live-edge chain below the fod (base edge + belowFOD filter edge) is
		// still built, so every memo side effect is unchanged from the wrapper
		// path; the fod merely ignores its final edge for RESOLUTION (freezing
		// innerWrapped instead) while still consuming its alias, which equals
		// innerCorr.
		fodInnerQ := expressions.NamedForEachQuantifier(innerQ.GetAlias(),
			call.MemoizeFinalExpression(innerWrapped))
		fodPlan := plans.NewRecordQueryFirstOrDefaultPlanStrictFromQuantifier(
			fodInnerQ, values.NewNullValue(values.UnknownType),
		)
		innerWrapped = fodPlan
		innerQ = expressions.NamedForEachQuantifier(innerCorr,
			call.MemoizeFinalExpression(fodPlan))
	} else if nullOnEmpty {
		// The DefaultOnEmpty is its own cascades expression carrying the live innerQ
		// edge (RFC-184 W2) — no physicalDefaultOnEmptyWrapper.
		doePlan := plans.NewRecordQueryDefaultOnEmptyPlanFromQuantifier(
			innerQ, values.NewNullValue(values.UnknownType),
		)
		innerWrapped = doePlan
		innerQ = expressions.NamedForEachQuantifier(innerCorr,
			call.MemoizeFinalExpression(doePlan))
		if len(joinPreds) > 0 {
			fpInnerQ := expressions.NamedForEachQuantifier(innerQ.GetAlias(),
				call.MemoizeFinalExpression(innerWrapped))
			filterPlan := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
				fpInnerQ, joinPreds, innerCorr,
			)
			innerWrapped = filterPlan
			innerQ = expressions.NamedForEachQuantifier(innerCorr,
				call.MemoizeFinalExpression(filterPlan))
		}
	} else if len(joinPreds) > 0 {
		fpInnerQ := expressions.NamedForEachQuantifier(innerQ.GetAlias(),
			call.MemoizeFinalExpression(innerWrapped))
		filterPlan := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
			fpInnerQ, joinPreds, innerCorr,
		)
		innerWrapped = filterPlan
		innerQ = expressions.NamedForEachQuantifier(innerCorr,
			call.MemoizeFinalExpression(filterPlan))
	}

	if len(outerPreds) > 0 {
		ofInnerQ := expressions.NamedForEachQuantifier(outerQ.GetAlias(),
			call.MemoizeFinalExpression(outerPlan))
		outerFilter := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
			ofInnerQ, outerPreds, outerCorr,
		)
		outerQ = expressions.NamedForEachQuantifier(outerCorr,
			call.MemoizeFinalExpression(outerFilter))
	}

	// The correlated FlatMap is its own cascades expression carrying its two leg
	// edges directly (RFC-184 W2, no physicalFlatMapWrapper). It ranges over the
	// SAME memo quantifiers the compensating lockstep chains above built — outerQ
	// over the (optionally filtered) outer, innerQ over the DefaultOnEmpty/
	// FirstOrDefault/filter inner — so the plan and its quantifiers can no longer
	// diverge. The correlated inner leg is a frozen final singleton (the fod/filter
	// disentangle), so extraction resolves it faithfully and the correlation the
	// FlatMap binds is preserved.
	flatMapPlan := plans.NewRecordQueryFlatMapPlanFromQuantifiers(
		outerQ, innerQ,
		outerCorr, innerCorr,
		resultValue, false,
	)
	return flatMapPlan, outerQ, innerQ, true
}

// buildExistsCompensationChain adds the physical operators that turn an
// existential inner into the one-row input consumed by a pure-map FlatMap:
//
//	inner [| below-FOD predicates] | FirstOrDefault(NULL)
//	  [| QOV(inner) IS [NOT] NULL]
//
// The caller supplies the base quantifier because the three entry paths range
// over different memo groups. Every added operator is memoized and the returned
// quantifier advances in lockstep, so the memo costs and extracts the same plan
// that executes.
//
// preserveAlias is load-bearing. A completed correlated-EXISTS FlatMap must
// preserve its real inner correlation at each step so Reference.GetCorrelatedTo
// can subtract the bound alias. The direct existential-select path instead
// needs a fresh physical alias after every wrapper: reusing one alias lets
// alias-aware memo interning collapse the below-FOD and existential residual
// filters and silently drop a predicate. innerCorrelation remains the
// executable row-binding alias in both modes; only the Cascades bookkeeping
// alias changes.
func buildExistsCompensationChain(
	call *ExpressionRuleCall,
	baseQ expressions.Quantifier,
	inner plans.RecordQueryPlan,
	innerCorrelation values.CorrelationIdentifier,
	belowFODPredicates []predicates.QueryPredicate,
	hasExistsFilter bool,
	negated bool,
	preserveAlias bool,
) expressions.Quantifier {
	advance := func(plan plans.RecordQueryPlan) expressions.Quantifier {
		ref := call.MemoizeFinalExpression(plan)
		if preserveAlias {
			return expressions.NamedPhysicalQuantifier(innerCorrelation, ref)
		}
		return expressions.NewPhysicalQuantifier(ref)
	}

	innerQ := baseQ
	belowFOD := inner
	if len(belowFODPredicates) > 0 {
		filterInnerQ := expressions.NamedPhysicalQuantifier(
			innerQ.GetAlias(), call.MemoizeFinalExpression(inner))
		filter := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
			filterInnerQ, belowFODPredicates, innerCorrelation)
		belowFOD = filter
		innerQ = advance(filter)
	}

	// Freeze the concrete correlated inner in a private final reference. The
	// child quantifier keeps the current bookkeeping alias, while advance()
	// applies the caller's fresh-vs-preserved policy above the wrapper.
	fodInnerQ := expressions.NamedPhysicalQuantifier(
		innerQ.GetAlias(), call.MemoizeFinalExpression(belowFOD))
	fod := plans.NewRecordQueryFirstOrDefaultPlanFromQuantifier(
		fodInnerQ, values.NewNullValue(values.UnknownType))
	innerQ = advance(fod)

	if hasExistsFilter {
		comparisonType := predicates.ComparisonIsNotNull
		if negated {
			comparisonType = predicates.ComparisonIsNull
		}
		residual := predicates.NewComparisonPredicate(
			values.NewQuantifiedObjectValue(innerCorrelation),
			predicates.Comparison{Type: comparisonType},
		)
		filterInnerQ := expressions.NamedPhysicalQuantifier(
			innerQ.GetAlias(), call.MemoizeFinalExpression(fod))
		filter := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
			filterInnerQ, []predicates.QueryPredicate{residual}, innerCorrelation)
		innerQ = advance(filter)
	}

	return innerQ
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

	outerExpr, _ := getWinnerForOrdering(outerRef, properties.PreserveOrdering(), call.CostModel())
	if outerExpr == nil {
		return
	}
	outerPh, ok := outerExpr.(physicalPlanExpression)
	if !ok {
		return
	}
	outerPlan := outerPh.GetRecordQueryPlan()

	innerExpr, _ := getWinnerForOrdering(innerRef, properties.PreserveOrdering(), call.CostModel())
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

	// The inner-leg alias set — the below-FOD-vs-outer routing authority (see the
	// detailed note at the below-FOD rebase). Computed here so the hoist can rebase
	// exactly the inner-residual preds.
	innerLegs := collectInnerLegAliases(innerRef, innerCorr)

	// HOIST the below-FOD window rebase ABOVE the fast path.
	// A fully-baked (AS+AT) seed's outer-leg refs must rebase to baked ofOrdinals
	// over the merged positional row BEFORE tryExistsFlatMap. An equality on the
	// element/ordinal can be swallowed by the fast path (a parameterized inner
	// scan) which RETURNS before the old below-FOD rebase ran — leaving a SIBLING
	// inner-residual's outer-table ref (`JU.K < MA.ID + 1000`) unrebased, evaluated
	// against the inner row → wrong rows. Rebase, ONCE, only the INNER-RESIDUAL
	// preds (predicateReferencesInnerLeg): those are evaluated BELOW the FOD against
	// the merged positional row. An OUTER-ONLY pred is evaluated in the outer-row
	// context, whose row lacks the merged row's higher slots — baking it there
	// reads a non-existent ordinal (a LEFT-box residual on the null-supplied leg).
	// Both the fast path and the below-FOD path then see correct refs; the below-FOD
	// window branch is a no-op (single rebase authority — no double-rebase).
	// CORRECT-or-LOUD: an unmappable ref declines the yield.
	// PROPERTY-DRIVEN outer selection: a below-FOD predicate that references a
	// BURIED leg of the outer box (an alias that is neither the outer binding,
	// an inner leg, nor a pre-evaluated scalar binding) can only be bound
	// through the positional-seed window rebase — the outer's result value must
	// BE the baked ordinal-seed RecordConstructor. That is a REQUIRED PROPERTY
	// of the outer child for this shape, exactly like a requested ordering: the
	// cost winner is only usable if it satisfies it. Equivalence-class members
	// share row SEMANTICS, not result-value STRUCTURE — a name-keyed merged
	// select and the gathered seed coexist as finals of the same group — so
	// when the cost winner lacks the seed shape, reselect the cheapest final
	// that HAS it rather than failing closed on an arbitrary tie-break. (Blind
	// winner consumption only ever worked because kind/noe-blind memo identity
	// used to dedup the non-seed twins away; RFC-186's refined identity keeps
	// them, making the tie-break — and this reselection — load-bearing.)
	// If NO final carries the seed shape, the fail-closed buried-ref guard
	// below still declines the yield.
	if w, _ := ordinalSeedLegWindowsOf(planResultValue(outerPlan)); w == nil {
		needBuried := false
		for _, p := range regularPreds {
			if !predicateReferencesInnerLeg(p, innerLegs) {
				continue
			}
			scalarAliases := scalarSubqueryAliasesOfPredicate(p)
			for a := range predicates.GetCorrelatedToOfPredicate(p) {
				if a == outerCorr {
					continue
				}
				if _, ok := innerLegs[a]; ok {
					continue
				}
				if _, ok := scalarAliases[a]; ok {
					continue
				}
				needBuried = true
			}
		}
		if needBuried {
			less := call.CostModel()
			var bestSeed physicalPlanExpression
			for _, fm := range outerRef.FinalMembers() {
				ph, isPh := fm.(physicalPlanExpression)
				if !isPh {
					continue
				}
				if fw, _ := ordinalSeedLegWindowsOf(planResultValue(ph.GetRecordQueryPlan())); fw == nil {
					continue
				}
				if bestSeed == nil || less(ph, bestSeed) {
					bestSeed = ph
				}
			}
			if bestSeed != nil {
				outerExpr = bestSeed
				outerPh = bestSeed
				outerPlan = bestSeed.GetRecordQueryPlan()
			}
		}
	}
	windowsHoisted := false
	if windows, mergedRowType := ordinalSeedLegWindowsOf(planResultValue(outerPlan)); windows != nil {
		windowsHoisted = true
		mergedQOV := values.NewQuantifiedObjectValueOfType(outerCorr, mergedRowType)
		for i, p := range regularPreds {
			if !predicateReferencesInnerLeg(p, innerLegs) {
				continue // outer-only: evaluated in the outer context — do not bake
			}
			np, ok := rebaseOuterLegRefsOrdinal(p, windows, mergedQOV)
			if !ok {
				return
			}
			regularPreds[i] = np
		}
	}

	// Try correlated-scan FlatMap: if a correlated predicate matches the
	// inner table's PK or index, push the correlation into a parameterized
	// inner scan (fast path). This is the pure-map FlatMap with the FOD
	// inner; see yieldExistsFlatMap. The fast path MUST use the SAME rebased
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
	// read and the comparison actually filters the outer row). innerLegs is
	// computed above (the hoist uses the same routing authority).
	var joinPreds []predicates.QueryPredicate
	var outerOnlyPreds []predicates.QueryPredicate
	for _, p := range regularPreds {
		if predicateReferencesInnerLeg(p, innerLegs) {
			joinPreds = append(joinPreds, p)
		} else {
			outerOnlyPreds = append(outerOnlyPreds, p)
		}
	}

	// Below-FOD OUTER-LEG references. When the outer box's result value is a
	// baked ordinal seed, its output row is the merged POSITIONAL row: a
	// below-FOD buried-leg reference (`b.emp_id = e.id`, e a leg of the box)
	// was already rebased, above this point (windowsHoisted), to a BAKED
	// ofOrdinalNumber over the box's binding (outerCorr) at
	// legOffset+columnOrdinal. This is the 2-quantifier (1 outer + 1 inner)
	// counterpart of implementJoinWithExistential's 3-quantifier (2 ForEach +
	// 1 Existential) ordinal rebase; the executor binds the box row
	// positionally through the FlatMap's identity result value plus these
	// baked inner references over outerCorr. Re-running the window rebase
	// here would DOUBLE-rebase (the merged corr coincides with the inner-leg
	// window key) — the whole reason the rebase was hoisted.
	//
	// A NON-windowed outer (no ordinal seed windows) FAILS CLOSED: a
	// below-FOD predicate referencing any alias beyond the binding alias and
	// the existential inner's own legs is a buried reference this builder
	// cannot bind — it would silently resolve against the wrong row. Decline
	// the yield; do not gamble on the correlation-unchecked fallback.
	if len(joinPreds) > 0 && outerCorr.Name() != "" && !windowsHoisted {
		for _, p := range joinPreds {
			corrSet := predicates.GetCorrelatedToOfPredicate(p)
			if corrSet == nil {
				corrSet = map[values.CorrelationIdentifier]struct{}{}
			}
			// A scalar-subquery alias is NOT a buried leg: its value is
			// pre-evaluated into the ROOT evaluation context, which every
			// below-FOD filter arm threads (RowContext/positional/strict
			// and passesJoinPredicatesLegs all attach ScalarSubqueries) —
			// so the reference resolves without any rebase authority.
			// Declining on it turned a valid scalar-in-EXISTS conjunct
			// into a loud 0AF00 (the class-K limitation).
			scalarAliases := scalarSubqueryAliasesOfPredicate(p)
			for a := range corrSet {
				// EXACT identifier comparisons (like the innerLegs
				// lookup): CorrelationIdentifier is case-sensitive by
				// design, and a fold here would fail OPEN — a
				// case-variant alias would skip the decline this guard
				// exists for. A mismatch declines (fails closed).
				if a == outerCorr {
					continue
				}
				if _, ok := innerLegs[a]; ok {
					continue
				}
				if _, ok := scalarAliases[a]; ok {
					continue
				}
				return
			}
		}
	}

	// This path deliberately asks the shared chain builder for FRESH
	// bookkeeping aliases. The FlatMap's executable bindings remain
	// outerCorr/innerCorr; fresh wrapper aliases prevent alias-aware memo
	// interning from collapsing the below-FOD and existential residual filters.
	innerQ := expressions.NamedPhysicalQuantifier(
		quants[1].GetAlias(), call.MemoizeExpression(innerExpr))
	innerQ = buildExistsCompensationChain(
		call, innerQ, innerPlan, innerCorr, joinPreds,
		hasExistsFilter, negated, false,
	)

	// outerOnlyPreds deliberately keep the buried-leg references the
	// below-FOD rebase above rewrites: this filter runs ABOVE the FlatMap on
	// the outer's OWN row, where the merged-row producer doesn't alias-bind
	// (producesMergedRows suppresses the binding) and a buried QOV(D).ID
	// resolves through the row's QUALIFIED "D.ID" Datum key directly — the
	// masked-conjunct pin (`d.id = 3 AND NOT EXISTS …`) exercises exactly
	// this path. The below-FOD preds needed the rebase because THEIR row
	// context is the inner scan's frontier row, not the outer's.
	outerQ := expressions.NamedPhysicalQuantifier(
		quants[0].GetAlias(), call.MemoizeExpression(outerExpr))

	if len(outerOnlyPreds) > 0 {
		ofInnerQ := expressions.NamedPhysicalQuantifier(outerQ.GetAlias(),
			call.MemoizeFinalExpression(outerPlan))
		outerFilter := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(ofInnerQ, outerOnlyPreds, outerCorr)
		outerQ = expressions.NewPhysicalQuantifier(
			call.MemoizeFinalExpression(outerFilter))
	}

	// The pure-map existential FlatMap is its own cascades expression carrying
	// its outer/inner memo edges directly (RFC-184 W2, no physicalFlatMapWrapper).
	// The compensating operators (below-FOD filter, FirstOrDefault, residual
	// existential filter, outer-only filter) already advanced innerQ/outerQ in
	// lockstep with what executes, so the plan and its quantifiers no longer
	// diverge. The correlated inner is a frozen final singleton, so extraction
	// resolves it faithfully and the EXISTS correlation is preserved.
	flatMapPlan := plans.NewRecordQueryFlatMapPlanFromQuantifiers(
		outerQ, innerQ,
		outerCorr, innerCorr,
		resultValue, true,
	)

	// The quantifiers range over the SAME compensated expressions the plan holds
	// — that is what the lockstep chains above buy, and it is what makes this
	// collapse safe. The rule builds three compensating operators (the below-FOD
	// join-pred filter, the above-FOD existential residual, and the outer-only
	// filter) and memoizes each, advancing outerQ/innerQ in lockstep, so the plan
	// pointer and the quantifiers can no longer name different expressions (the
	// 472 semantically-divergent edges that once forced the wrapper). Collapsing
	// the FlatMap onto its quantifiers therefore keeps the DefaultOnEmpty (outer-
	// join NULLs) and the residuals (correct rows) — the drop RFC-183 P5's
	// terminal step feared cannot happen once the edges coincide.
	//
	// rule_implement_simple_select.go:97-117 always had this shape.
	r.yieldBinaryJoinWithOrderingVariants(call, flatMapPlan)
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

// mergedOuterLegAliases returns the set of source-leg aliases the inner-join's
// merged outer row anchors columns for. RFC-142.
//
// It takes BOTH the source-alias TEXT and the leg IDENTITIES, and emits both
// spellings, because its consumers match it against two different things. The
// dotted rebase (rebaseOuterLegValue) compares each entry against a reference's
// own QOV CORRELATION name, and those references are correlated to the select's
// QUANTIFIERS — so the identity spelling is the one that can match. The text
// spelling stays because the same set feeds the dotted "LEG.COL" key namespace,
// which is upper-folded.
//
// Building it from the text ALONE was a latent mismatch on two axes, and the
// verification set beside it (existLegCorrs) already worked around one of them by
// adding the quantifier aliases explicitly:
//
//   - the two channels can name different things. Measured over the FDB corpus,
//     the select's source-alias slice carries a RE-MINTED identifier while its
//     quantifier keeps the user alias on 12 of ~80k firings (witnesses "q$N vs
//     E"). There the text set matches no reference at all;
//   - the fold is one-way. Every entry was upper-folded while a reference's
//     correlation is verbatim, so a LOWERCASE machine-minted alias could not
//     match its own entry even when the two channels agreed.
//
// A miss is not loud here: the reference stays pointing at a leg alias that is no
// longer bound inside the FlatMap, evaluates to NULL, and an EXISTS correlation
// that never matches drops rows. Emitting both spellings can only make the rebase
// match MORE references, and every extra one is a reference that genuinely names
// a leg of this merge. The text half retires with the dotted channel (CQ-53's
// seed rebake); the identity half is what survives it.
func mergedOuterLegAliases(leftAlias, rightAlias string, legCorrs ...values.CorrelationIdentifier) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(a string) {
		if a == "" {
			return
		}
		if _, ok := seen[a]; ok {
			return
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	// The dotted-text namespace: upper-folded, as its keys are.
	add(strings.ToUpper(leftAlias))
	add(strings.ToUpper(rightAlias))
	// The identity namespace: VERBATIM. Folding it here is what would let a
	// quoted "q$5" and a minted q$5 be treated as one leg, and it is also what
	// kept a minted leg from matching itself.
	//
	// Consequence worth stating: the lazy dotted arm in rebaseOuterLegValue matches
	// a reference's correlation against these entries EXACTLY and then mints
	// `corr + "." + upper(field)`, so a lowercase machine-minted leg now mints a
	// LOWERCASE-qualified key ("c.COL") where the folded-only set produced "C.COL"
	// or nothing. That is inert on three independent counts, none of which is a
	// second namespace:
	//   - no dotted merged-row key reaches a WINNING plan on any covered shape
	//     (TestLazyLegMintReachesNoWinningPlan measures zero), so nothing executes
	//     against a key minted here;
	//   - the rebase is followed by a fail-closed verification — a reference that
	//     still names a leg alias DECLINES the yield rather than shipping
	//     (correct-or-loud), so a mis-keyed survivor costs a plan, never rows;
	//   - the runtime dotted lookup folds BOTH sides (rowSlotForLegColumn's
	//     EqualFold on qualifier and column), so a survivor that did reach execution
	//     addresses the same slot either spelling.
	// The arm retires with the dotted channel entirely (CQ-53's seed rebake).
	for _, c := range legCorrs {
		add(c.Name())
	}
	return out
}

// planResultValue unwraps ROW-SHAPE-PRESERVING single-child wrappers
// (predicate filters, first-or-default, default-on-empty) to the first plan
// carrying a non-nil result value — the merged-row schema authority for the
// buried-leg rebase, independent of which wrappers the winner accrued. The
// unwrap is an explicit WHITELIST, not a generic GetInner walk: a
// schema-CHANGING plan with an inner (aggregation, projection) must terminate
// the walk — its inner's result value is NOT the authority for the rows this
// plan emits, and handing it to the rebase would lie about the row schema.
// Returns nil when no whitelisted plan carries one (bare scans: single-table
// rows, bare keys; the caller then fails closed on buried references).
func planResultValue(p plans.RecordQueryPlan) values.Value {
	for p != nil {
		if rvp, ok := p.(interface{ GetResultValue() values.Value }); ok {
			if rv := rvp.GetResultValue(); rv != nil {
				return rv
			}
		}
		switch w := p.(type) {
		case *plans.RecordQueryPredicatesFilterPlan:
			p = w.GetInner()
		case *plans.RecordQueryFirstOrDefaultPlan:
			p = w.GetInner()
		case *plans.RecordQueryDefaultOnEmptyPlan:
			p = w.GetInner()
		default:
			return nil
		}
	}
	return nil
}

// legIsOrdinalSafe reports whether a projected-EXISTS fold leg
// can seed the step-1 ordinal merged row: a
// SINGLE-SOURCE leg whose rows are one namespace (a scan, through transparent
// Filter/Fetch/FOD wrappers), OR an ORDINAL bare-INNER nested-loop join — the
// ACCUMULATED INNER of the N-way left-deep chain (its own ordinal concat row read
// positionally by the next level through its buried-leaf windows). A
// MERGED-row leg an ordinal seed
// cannot position is declined — the executor twin of the translator gate's
// ordinalEligible (correct-or-conservative). A NON-INNER NLJ is declined (the
// INNER-first scope carries no null-extension; the LEFT follow-on widens it).
func legIsOrdinalSafe(p plans.RecordQueryPlan) bool {
	for p != nil {
		switch pl := p.(type) {
		case *plans.RecordQueryScanPlan, *plans.RecordQueryIndexPlan:
			return true
		case *plans.RecordQueryPredicatesFilterPlan:
			p = pl.GetInner()
		case *plans.RecordQueryFilterPlan:
			p = pl.GetInner()
		case *plans.RecordQueryFirstOrDefaultPlan:
			p = pl.GetInner()
		case *plans.RecordQueryDefaultOnEmptyPlan:
			p = pl.GetInner()
		case *plans.RecordQueryFetchFromPartialRecordPlan:
			p = pl.GetInner()
		case *plans.RecordQueryNestedLoopJoinPlan:
			if pl.GetJoinType() != plans.JoinInner {
				return false
			}
			// Only an ORDINAL box (a positional concat row) can be read by
			// ordinal — every RC box qualifies.
			if _, ok := pl.GetResultValue().(*values.RecordConstructorValue); !ok {
				return false
			}
			return legIsOrdinalSafe(pl.GetOuter()) && legIsOrdinalSafe(pl.GetInner())
		default:
			return false
		}
	}
	return false
}

// planBuriedLegConcat walks an ordinal-safe leg PLAN accumulating its buried
// scan leaves' fields (the flat concat) and each buried source's [Start,Width)
// window relative to `base` — the PLAN-LEVEL twin of the translator's
// buriedLegBounds. A bare INNER nested-loop join recurses into its two legs
// (keyed by the join's outer/inner aliases); a scan-family leaf (through
// transparent wrappers) contributes one leaf window keyed by `alias`. This
// windows the N-way chain's ACCUMULATED INNER so the next level reads its buried
// leaves positionally. ok=false for any non-ordinal-safe node. A single scan leg
// yields ONE leg (its own alias) — the caller uses .Legs only when len(legs) > 1.
func planBuriedLegConcat(p plans.RecordQueryPlan, alias values.CorrelationIdentifier, base int) ([]values.Field, []values.RecordTypeLeg, bool) {
	inner := p
	for {
		switch pl := inner.(type) {
		case *plans.RecordQueryScanPlan, *plans.RecordQueryIndexPlan:
			rt, isRT := inner.GetResultType().(*values.RecordType)
			if !isRT || len(rt.Fields) == 0 {
				return nil, nil, false
			}
			// The leg's identity is the identifier its own QuantifiedObjectValue
			// carries — threaded in, not manufactured here. This used to mint
			// NamedCorrelationIdentifier(ToUpper(alias)) from a plan-level string, and
			// that fold was a forgery generator rather than a normalization: the
			// machine namespace is LOWERCASE (UniqueCorrelationIdentifier mints q$N),
			// so upper-folding a minted q$5 produced Q$5 — precisely the spelling
			// SameLeg exists to keep out of the minted leg's window. Threading the
			// identifier removes the question. Name is its own spelling, so the text
			// channel's readers see what they always saw.
			return rt.Fields, []values.RecordTypeLeg{
				values.NewRecordTypeLeg(alias, alias.Name(), base, len(rt.Fields)),
			}, true
		case *plans.RecordQueryPredicatesFilterPlan:
			inner = pl.GetInner()
		case *plans.RecordQueryFilterPlan:
			inner = pl.GetInner()
		case *plans.RecordQueryFirstOrDefaultPlan:
			inner = pl.GetInner()
		case *plans.RecordQueryDefaultOnEmptyPlan:
			inner = pl.GetInner()
		case *plans.RecordQueryFetchFromPartialRecordPlan:
			inner = pl.GetInner()
		case *plans.RecordQueryNestedLoopJoinPlan:
			if pl.GetJoinType() != plans.JoinInner {
				return nil, nil, false
			}
			outerFields, outerLegs, ok := planBuriedLegConcat(pl.GetOuter(), pl.GetOuterAlias(), base)
			if !ok {
				return nil, nil, false
			}
			innerFields, innerLegs, ok := planBuriedLegConcat(pl.GetInner(), pl.GetInnerAlias(), base+len(outerFields))
			if !ok {
				return nil, nil, false
			}
			return append(outerFields, innerFields...), append(outerLegs, innerLegs...), true
		default:
			return nil, nil, false
		}
	}
}

// reconstructFoldStep1Seed rebuilds the full leg-concat ordinal seed for an
// ordinalized projected-EXISTS fold. The step-1 NLJ produces a
// positional merged row from this seed; the step-2 FlatMap then evaluates the
// folded projection over that row through legWindowRowContext (spanAwareRow +
// legWindowBinder), so the projection is NEVER rebased — its dotted and
// QOV-based leg refs resolve positionally at the cursor. The seed is a run of
// ofOrdinal references over each leg's typed QOV (leg type from GetResultType,
// NOT planResultValue which is nil for these leg plans), in declaration order,
// the QOV named by the leg's source alias — positionally equivalent to
// buildOrdinalJoinResultValue for the ordinal-safe scan legs in scope (the
// canonical builder types legs via ordinalLegType and asserts the seed shape;
// for a single-source scan the flowed types coincide, which the cross-agreement
// fixture covers). Returns nil when a leg is not ordinal-safe or its type is not
// a record (the caller keeps the original RV unchanged).
func reconstructFoldStep1Seed(leftPlan, rightPlan plans.RecordQueryPlan, leftAlias, rightAlias values.CorrelationIdentifier) values.Value {
	if !legIsOrdinalSafe(leftPlan) || !legIsOrdinalSafe(rightPlan) {
		return nil
	}
	var fields []values.RecordConstructorField
	for _, leg := range []struct {
		plan  plans.RecordQueryPlan
		alias values.CorrelationIdentifier
	}{{leftPlan, leftAlias}, {rightPlan, rightAlias}} {
		// Walk the leg to its buried scan leaves: the flat concat + each buried
		// source's window. A scan leg yields one leaf (its own alias); the N-way
		// chain's accumulated INNER (a bare INNER NLJ) yields its buried leaves.
		// GetResultType() cannot be used for a NLJ leg — it is a stub
		// (UnknownType); the walk reconstructs the concat from the scan leaves.
		concatFields, buriedLegs, ok := planBuriedLegConcat(leg.plan, leg.alias, 0)
		if !ok || len(concatFields) == 0 {
			return nil
		}
		// A BURIED box (>1 leaf) carries its buried sources' [Start,Width) windows
		// in .Legs so the layout authority (OrdinalSeedLegWindows →
		// finalizeSeedWindows) emits a per-buried-leaf sub-window and a qualified
		// buried read resolves positionally; a plain scan leg (one leaf) leaves
		// .Legs nil — its window is the top QOV correlation's. Struct literal (not
		// NewRecordType) so a cross-source duplicate column name cannot panic — the
		// ordinal reads are by slot and buried disambiguation is by .Legs windows.
		var legs []values.RecordTypeLeg
		if len(buriedLegs) > 1 {
			legs = buriedLegs
		}
		rt := &values.RecordType{Fields: concatFields, Legs: legs}
		// The QOV's correlation is the leg's identifier, carried. It must be the SAME
		// identifier planBuriedLegConcat stamped on the leg boundaries just above —
		// the seed's ordinal reads resolve by matching one against the other — and
		// the way to guarantee that is for both to be the one identifier the caller
		// threaded, rather than two independent folds of one string.
		qov := values.NewQuantifiedObjectValueOfType(leg.alias, rt)
		for i := range concatFields {
			fv, err := values.NewFieldValueOfOrdinal(qov, i)
			if err != nil {
				return nil
			}
			fields = append(fields, values.RecordConstructorField{Name: fv.Field, Value: fv})
		}
	}
	return values.NewRawRecordConstructorValue(fields...)
}

// materializedNLJOrdinalLayoutMatches reports whether resultValue is SAFE to
// use, verbatim, as a materialized RecordQueryNestedLoopJoinPlan's own result
// value when that plan's outer leg is outerPlan and its inner leg is
// innerPlan.
//
// The executor's merge for a materialized NLJ (executor.go's
// concatLegPositionals, called from mergeRows) ALWAYS concatenates the
// OUTER leg's fields first (offset 0) and the INNER leg's immediately after
// (offset outerWidth) — a PHYSICAL, execution-order fact. When resultValue
// is a pristine ORDINAL SEED (every field a BAKED FrontierPinned FieldValue —
// see ordinalSeedLegWindowsOf), its ordinals were baked assuming ONE FIXED
// physical field order at the time the seed was built. A join-commutativity
// exploration (fireExprRuleOnMember's ChildrenAsSet permutation,
// expressions.SelectExpression.WithSwappedQuantifiers) tries the SAME
// logical select with its first two quantifiers swapped so both outer/inner
// assignments get evaluated on cost — but WithSwappedQuantifiers reuses
// resultValue UNCHANGED (correct for an ordinary correlation-addressed
// result value, whose meaning does not depend on quantifier order at all).
// For an ordinal seed this is unsound: if the outer/inner legs have been
// swapped relative to the seed's OWN baked layout, the seed's ordinals no
// longer address the row this SPECIFIC physical plan will actually produce —
// a stale-cost-model victory for this orientation (see
// NestedLoopJoinUniqueKeyConjuncts) can promote it to the memo's winner, and
// every baked reference into it then reads the wrong slot (or, when out of
// range or genuinely unbound downstream, fails loud with an
// unbound-correlation error) — never a silent wrong VALUE reaching the user
// undetected, since the ordinal read is either right or loud, but still a
// plan the query cannot execute.
//
// Verification is STRUCTURAL, never by alias/correlation-identity STRING: an
// earlier version of this check compared outer/innerAlias strings against
// the windows map, tried first with sel.GetSourceAliases() and then with the
// quantifier's own GetAlias() as a fallback when the other was a synthetic
// unique ID. Both individually regressed real queries — sourceAliases can
// fall back to a synthetic ID for one SelectExpression member while
// GetAlias() carries the seed's real leg name (the original 3-quantifier
// EXISTS bug this check exists for), and the reverse can ALSO happen (a
// 2-quantifier RIGHT-OUTER-JOIN-normalized member). Worse, GetAlias()'s
// synthetic IDs are literally "q$N" strings (values.UniqueCorrelationIdentifier's
// format) which a user-supplied QUOTED alias can legitimately collide with
// byte-for-byte (values.CorrelationIdentifier is a bare wrapped string, so
// NamedCorrelationIdentifier("q$2") == a synthetic UniqueCorrelationIdentifier
// that happens to reach counter 2) — exactly the adversarial shape
// TestFDB_QuotedMachineShapedAliases pins, and exactly what a second
// alias-guessing "fallback-of-a-fallback" would still be exposed to. There is
// no string namespace that is safe to guess in. A leg's ROW SHAPE (RecordName
// + fields), by contrast, is never ambiguous: comparing outerPlan's/
// innerPlan's OWN GetResultType() against the seed's two top-level windows
// SORTED BY OFFSET (the seed's own physical order, independent of any name)
// identifies which physical leg occupies which slot with no naming
// involved at all.
//
// Declining (returning false) here is always safe: the join-commutativity
// exploration ADDS an alternative candidate, it never removes the
// non-swapped one, so the correctly-laid-out orientation remains available
// in the SAME memo group to compete on cost normally. A resultValue that is
// NOT an ordinal seed (the common case: a lazy, correlation-addressed
// RecordConstructorValue) is always safe regardless of orientation — its
// field reads resolve by correlation, not by physical position — and a
// self-join (both legs share the identical row shape) is also always safe:
// the ordinals address a structurally valid slot of either physical leg
// either way, so there is nothing for this check to distinguish.
func materializedNLJOrdinalLayoutMatches(resultValue values.Value, outerPlan, innerPlan plans.RecordQueryPlan) bool {
	windows, _ := ordinalSeedLegWindowsOf(resultValue)
	if len(windows) != 2 {
		// Not a pristine 2-leg ordinal seed (nil: not an ordinal seed at all;
		// any count other than 2 is outside a 2-quantifier join's own scope,
		// e.g. a buried/nested leg layout this check does not reason about)
		// — not this check's concern either way.
		return true
	}
	outerType := planRowRecordType(outerPlan)
	innerType := planRowRecordType(innerPlan)
	if outerType == nil || innerType == nil {
		return true // can't verify structurally — safe default
	}
	legs := make([]ordinalLegWindow, 0, 2)
	for _, w := range windows {
		legs = append(legs, w)
	}
	sort.Slice(legs, func(i, j int) bool { return legs[i].Offset < legs[j].Offset })
	return recordFieldsMatch(legs[0].Typ, outerType) && recordFieldsMatch(legs[1].Typ, innerType)
}

// recordFieldsMatch compares two RecordTypes by FIELDS only (name + ordinal +
// field type, via values.Field.Equals) — NOT the full values.RecordType.Equals,
// which also requires RecordName/Nullable to match. A window's own Typ
// (ordinalLegWindow, built by OrdinalSeedLegWindows from a positional slice
// of the merged seed's fields) never carries the leg's original RecordName —
// it is a synthesized sub-record, not the leg's declared type — so comparing
// full Equals against a real plan's GetResultType() (which DOES carry its
// RecordName, e.g. "DEPT") would spuriously mismatch every single leg,
// including the correctly-oriented one. Field shape is the information both
// sides actually carry and is exactly what distinguishes one base table's
// leg from another's (different tables have different column sets).
func recordFieldsMatch(a, b *values.RecordType) bool {
	if a == nil || b == nil {
		return a == b
	}
	if len(a.Fields) != len(b.Fields) {
		return false
	}
	for i := range a.Fields {
		if !a.Fields[i].Equals(b.Fields[i]) {
			return false
		}
	}
	return true
}

// planRowRecordType unwraps p's own GetResultType() down to the *RecordType
// describing its row shape — directly, or through a RelationType wrapper
// (values.RelationType.InnerType) when the plan reports a stream-of-rows
// type rather than the bare row type. nil when neither shape applies (an
// opaque/erased result type) — the caller treats that as "can't verify".
func planRowRecordType(p plans.RecordQueryPlan) *values.RecordType {
	if p == nil {
		return nil
	}
	switch t := p.GetResultType().(type) {
	case *values.RecordType:
		return t
	case *values.RelationType:
		if rt, ok := t.InnerType.(*values.RecordType); ok {
			return rt
		}
	}
	return nil
}

// foldStep1Seed is the step-1 result-value decision for the 3-quantifier
// join+EXISTS arm. It returns the FULL leg-concat
// ordinal seed (with the second result true) for an INDEPENDENT-legs
// PROJECTED-EXISTS fold over ordinal-safe legs — the step-1 NLJ then
// produces a positional merged row the step-2 FlatMap resolves the
// projection over (legWindowRowContext) — and otherwise returns the
// original RV unchanged (second result false).
// The conditions, in order: (1) NOT a correlated step 1 (a correlated
// FlatMap binds legs by NAME, where a baked seed hits the loud
// BakedNameContextError — an earlier, simpler version of this seed had to
// be reverted twice over exactly this trap); (2) the RV is a projected fold
// (references the existential quantifier — a WHERE-EXISTS pass-through
// does not); (3) the legs are ordinal-safe single sources and reconstruct a
// seed BOTH layout twins accept (full coverage). Extracted so the exact
// decision is white-box pinned: a functional test is BLIND to a silent
// revert of this decision.
func foldStep1Seed(rv values.Value, existAlias values.CorrelationIdentifier, correlatedStep1 bool, leftPlan, rightPlan plans.RecordQueryPlan, leftAlias, rightAlias values.CorrelationIdentifier) (values.Value, bool) {
	if correlatedStep1 || !resultValueReferencesAlias(rv, existAlias) {
		return rv, false
	}
	seed := reconstructFoldStep1Seed(leftPlan, rightPlan, leftAlias, rightAlias)
	if seed == nil {
		return rv, false
	}
	if w, _ := ordinalSeedLegWindowsOf(seed); w == nil {
		return rv, false
	}
	return seed, true
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
// buriedLegOrdinalLayout derives the merged outer row's COLUMN IDENTITY →
// global-ordinal map from the outer plan, so buried-leg references can be
// rebased to BAKED positional reads instead of lazy qualified-name mints
// (WS-N slice 4).
//
// Derivable from a FlatMap outer whose result value is the positional
// RecordConstructorValue concat: slot i's constructor value is a bare column
// read off a leg quantifier, so its (correlation, domain, ordinal) maps to i.
// First occurrence wins, matching the positional layout's first-fold.
//
// It used to key by 'CORR.LEAF' built from the display name — one of the seven
// wrong proofs the RFC-197 gate is named after. The Resolved.Single() guard
// under it declined FUSED accessors, but two same-named TOP-LEVEL columns of one
// leg still collided, and the reader below baked a buried reference to whatever
// ordinal the name-built key returned. The key is now the identity, so a slot
// that cannot state one is simply not in the map.
//
// The ordinal-safe scan/NLJ chain shape (planBuriedLegConcat's leg windows) is
// deliberately NOT derived any more: it has leg windows and column NAMES, no
// per-slot value to take an identity from, so every key it could mint would be a
// name. Declining costs the lazy qualified mint — the pre-slice-4 behaviour, and
// the same thing an underivable outer already gets.
//
// nil when the layout is not derivable.
func buriedLegOrdinalLayout(outerPlan plans.RecordQueryPlan) map[values.ColumnIdentity]int {
	fm, isFM := outerPlan.(*plans.RecordQueryFlatMapPlan)
	if !isFM {
		return nil
	}
	rc, isRC := fm.GetResultValue().(*values.RecordConstructorValue)
	if !isRC {
		return nil
	}
	layout := make(map[values.ColumnIdentity]int, len(rc.Fields))
	for i, f := range rc.Fields {
		fv, isFV := f.Value.(*values.FieldValue)
		if !isFV {
			continue
		}
		id, ok := legSlotIdentity(fv)
		if !ok {
			// A slot that cannot state its identity — a fused multi-accessor
			// path, a lazy carrier, a value with no leg quantifier, a leg
			// whose row type is not a record — mints no key. It used to mint
			// a name-built one, which is how a nested leg.address.id could
			// collide with a genuine top-level leg.id.
			continue
		}
		if _, dup := layout[id]; !dup {
			layout[id] = i
		}
	}
	if len(layout) == 0 {
		return nil
	}
	return layout
}

// legSlotIdentity is the identity of a bare column read off a join leg, stated
// in that leg's OWN row layout — the domain derived from the quantifier the
// value reads, which is the one place it is a proof rather than a claim (the
// derive-when-typed rule: the child is typed, so nothing has to be stored).
//
// Used by both ends of the buried-leg layout, so the writer and the reader
// cannot key it two different ways.
func legSlotIdentity(fv *values.FieldValue) (values.ColumnIdentity, bool) {
	qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
	if !isQOV {
		return values.ColumnIdentity{}, false
	}
	return fv.CorrelatedIdentityIn(values.OrdinalDomainOfType(qov.Typ))
}

// physicalLegRowTypes derives each join leg's flowed ROW type from the PHYSICAL
// plan chosen for it — the layout the leg's rows actually arrive in.
//
// This is the derivation Java's planner performs implicitly and Go had nowhere to
// read: `translateCorrelations` rebinds every FieldValue ordinal against the
// physical quantifier's own flowed type (Quantifier.java:801-803 —
// `getFlowedObjectValue()` is `QuantifiedObjectValue.of(alias,
// getFlowedObjectType())`, always typed), so a baked ordinal IS the physical slot.
// Go's leg-correlated references arrive carrying an UNTYPED quantifier object, so
// a leg-local ordinal had nothing to resolve against and the leg had to be packed
// into a column NAME instead. Reading the type off the leg's chosen plan is what
// gives the reference its layout back.
//
// It reuses `planBuriedLegConcat` — the same walk `reconstructFoldStep1Seed` uses
// to build the ordinal seed — so a leg's row type here and the seed's leg type are
// derived by ONE authority and cannot drift into two answers. A leg whose plan the
// walk cannot reduce to a concat contributes nothing rather than a guess.
func physicalLegRowTypes(legs ...struct {
	Plan  plans.RecordQueryPlan
	Alias values.CorrelationIdentifier
},
) map[values.CorrelationIdentifier]*values.RecordType {
	out := make(map[values.CorrelationIdentifier]*values.RecordType, len(legs))
	for _, leg := range legs {
		if leg.Plan == nil || leg.Alias.IsZero() {
			continue
		}
		concatFields, buriedLegs, ok := planBuriedLegConcat(leg.Plan, leg.Alias, 0)
		if !ok || len(concatFields) == 0 {
			// The scan-leaf walk reduces scans, filters and INNER nested-loop
			// joins. It has no arm for a leg that is itself a FlatMap — the
			// accumulated side of an N-way join — and that is the ENTIRE measured
			// residue of this derivation (32 underivable legs over the real-FDB
			// corpus, every one of them a *plans.RecordQueryFlatMapPlan).
			//
			// A FlatMap's row is whatever its result value computes, so there is
			// nothing to walk to; the layout is the result value's to state. These
			// legs' result values state nothing, because they flow an UNTYPED
			// quantifier object — which is the defect booked as CQ-63, and the
			// reason the qualified-name channel cannot close here. Reading the
			// layout off a seeded result value was tried and declines on every one
			// of them, so no arm is added on speculation.
			if values.LegIdentityCensusEnabled() {
				// The residue's CAUSE, recorded where it is decided. A leg whose
				// layout cannot be stated is why a read correlated to it still has
				// to be re-anchored, and knowing WHICH plan shape declines is the
				// difference between an actionable gap and a mystery.
				recordUnderivableLegLayout(leg.Alias, leg.Plan)
			}
			continue
		}
		var nested []values.RecordTypeLeg
		if len(buriedLegs) > 1 {
			nested = buriedLegs
		}
		out[leg.Alias] = &values.RecordType{Fields: concatFields, Legs: nested}
		addBuriedLegLayouts(out, concatFields, nested)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// addBuriedLegLayouts gives each BURIED source of a leg its own layout, sliced
// from the leg's concat at that source's window.
//
// A leg that is itself a join binds its buried sources under their own
// correlations too — the merged row's leg table carries every buried sub-window,
// so the runtime binder serves them — and a read correlated to a buried source
// must state its ordinal in the row it will actually be bound to, not in the box
// that carries it. A malformed or already-claimed window contributes nothing; a
// first-claim wins, matching the leg table's own first-match rule.
func addBuriedLegLayouts(out map[values.CorrelationIdentifier]*values.RecordType, concat []values.Field, legs []values.RecordTypeLeg) {
	for _, bl := range legs {
		end := bl.Start + bl.Width
		if bl.Alias.IsZero() || bl.Start < 0 || end > len(concat) || bl.Start >= end {
			continue
		}
		if _, taken := out[bl.Alias]; taken {
			continue
		}
		sub := make([]values.Field, end-bl.Start)
		for k := range sub {
			f := concat[bl.Start+k]
			f.Ordinal = k
			sub[k] = f
		}
		out[bl.Alias] = &values.RecordType{Fields: sub}
	}
}

func rebaseOuterLegRefsToMerged(
	p predicates.QueryPredicate,
	legAliases []string,
	mergedCorr values.CorrelationIdentifier,
	legLayout map[values.ColumnIdentity]int,
	legLocalTypes map[values.CorrelationIdentifier]*values.RecordType,
) predicates.QueryPredicate {
	if p == nil {
		return p
	}
	switch pred := p.(type) {
	case *predicates.ComparisonPredicate:
		newOperand := rebaseOuterLegValue(pred.Operand, legAliases, mergedCorr, legLayout, legLocalTypes)
		newCompOperand := rebaseOuterLegValue(pred.Comparison.Operand, legAliases, mergedCorr, legLayout, legLocalTypes)
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
		newVal := rebaseOuterLegValue(pred.Value, legAliases, mergedCorr, legLayout, legLocalTypes)
		if newVal == pred.Value {
			return p
		}
		return predicates.NewValuePredicate(newVal)
	case *predicates.AndPredicate:
		changed := false
		subs := make([]predicates.QueryPredicate, len(pred.SubPredicates))
		for i, s := range pred.SubPredicates {
			subs[i] = rebaseOuterLegRefsToMerged(s, legAliases, mergedCorr, legLayout, legLocalTypes)
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
			subs[i] = rebaseOuterLegRefsToMerged(s, legAliases, mergedCorr, legLayout, legLocalTypes)
			if subs[i] != s {
				changed = true
			}
		}
		if !changed {
			return p
		}
		return predicates.NewOr(subs...)
	case *predicates.NotPredicate:
		newChild := rebaseOuterLegRefsToMerged(pred.Child, legAliases, mergedCorr, legLayout, legLocalTypes)
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
	legLayout map[values.ColumnIdentity]int,
	legLocalTypes map[values.CorrelationIdentifier]*values.RecordType,
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
			// Exact: correlation-key namespace on both sides (B3b) — a
			// fold here would let a quoted user alias cross into the
			// lowercase machine namespace.
			corr := qov.Correlation.Name()
			// Bare-column allowlist (fieldValueAliasAndCol /
			// correlatedFieldOf / correlatedInnerField): a fused
			// multi-accessor bake (Child=QOV directly, Resolved carrying
			// more than one accessor) passes the Child==QOV check above
			// while fv.Field is only its LEAF name — a nested
			// `leg.address.id` would otherwise mint qualField "LEG.ID" and
			// impersonate a genuine top-level "leg.id" reference. Decline
			// (leave the node unrewritten) rather than mint that colliding
			// qualified name.
			bareChild := true
			if fv.Resolved != nil {
				_, bareChild = fv.Resolved.Single()
			}
			for _, leg := range legAliases {
				if bareChild && leg != "" && leg == corr {
					// This rewrite degrades the reference to a lazy dotted
					// name over a merge correlation — a silent baked→lazy
					// degradation for an eager ordinal node. It only fires
					// on the lazy EXISTS-over-join and RFC-153
					// buried-preserved-leg rebase paths; a PINNED baked node
					// reaching here means translation routed an ordinal
					// join into this lazy rebase machinery by mistake
					// (unpinned wrap nodes are childless and never reach
					// this arm, so the assert keys on the FrontierPinned
					// contract bit to catch exactly that misrouting).
					if fv.Resolved != nil && fv.Resolved.FrontierPinned {
						panic(fmt.Sprintf("rebaseOuterLegValue would re-anchor BAKED FieldValue %s#%d (leg %s) to merge alias %s — an ordinal join was routed into the lazy rebase machinery instead of the ordinal one (planner bug)",
							fv.Field, fv.Resolved.Root().Ordinal, corr, mergedCorr.Name()))
					}
					// LEG-LOCAL FIRST — Java's shape, where nothing is re-anchored.
					//
					// Java's FlatMap binds one correlation per quantifier
					// (RecordQueryFlatMapPlan.java:135-140), so a leg-correlated read
					// stays on its own alias and resolves as an ordinal against that
					// leg's own row (QuantifiedObjectValue.java:84-85 is a map lookup).
					// Go's two-level lowering bound only the join's alias, so the leg
					// had to be packed into a column NAME instead. With each leg of the
					// merged row bound under its own correlation
					// (executor.bindMergedOuterLegs), the read can keep its alias — all
					// it needs is the LAYOUT to state an ordinal in, and the leg's
					// chosen physical plan is where that layout lives.
					//
					// The reference's own quantifier object cannot supply it: measured
					// over the real-FDB corpus, every read reaching this arm carries an
					// UNTYPED leg QOV (leg-local bakeability census: 56 of 56). So the
					// type comes from the leg's plan, threaded in by the caller, and the
					// bake re-states the reference over a TYPED leg quantifier.
					legTypeFor, haveLegType := legLocalTypes[qov.Correlation]
					if haveLegType && legTypeFor != nil {
						if ord, found := legTypeFor.FieldIndex(strings.ToUpper(fv.Field)); found {
							if values.LegIdentityCensusEnabled() {
								recordLegLocalBakeability(qov.Correlation, legTypeFor, strings.ToUpper(fv.Field))
							}
							return values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
								values.NewQuantifiedObjectValueOfType(qov.Correlation, legTypeFor),
								fv.Field, ord, fv.Typ,
								values.OrdinalDomainOfType(legTypeFor),
							)
						}
					}
					if values.LegIdentityCensusEnabled() {
						// Fell through to the qualified-name mint: record WHY, against
						// the layout the bake would have used, so the residue names the
						// shapes that still need the name channel.
						recordLegLocalBakeability(qov.Correlation, legTypeOrUntyped(legTypeFor, haveLegType, qov.Typ), strings.ToUpper(fv.Field), legLocalTypeKeys(legLocalTypes)...)
					}
					qualField := corr + "." + strings.ToUpper(fv.Field)
					// Structural first (WS-N slice 4): when the merged
					// outer row's positional layout is derivable, the
					// rebased reference is BORN BAKED — the global
					// ordinal reads the merged row's slot directly
					// (the RC concat is positional; the qualified
					// display name rides along for Explain only).
					// The slot is found by the reference's own
					// IDENTITY in its leg's row layout — the same
					// derivation the layout was BUILT with
					// (legSlotIdentity), so the two ends cannot key it
					// differently, and a reference that cannot state
					// an identity finds nothing rather than finding
					// whatever its display name happens to spell.
					// WHAT IS DEAD HERE IS THE BAKE, NOT THE ARM.
					// This legLayout != nil lookup is dead-in-effect on
					// every covered surface (yamsql, embedded, full FDB
					// driver incl. the RFC-153 matrix): a panic wired into
					// it is reached only by
					// TestRebaseOuterLegValue_OrdinalFirst, because the
					// only callers holding a layout are the ordinal-seed
					// paths and those rebase through
					// rebaseOuterLegRefsOrdinal instead. It stays as the
					// fail-closed net for shapes that arrive layout-bearing.
					//
					// The ENCLOSING leg-match arm is live. It is reached
					// while planning the buried / N-way / four-leg
					// projected-EXISTS family, from
					// implementJoinWithExistential's nil-layout call for
					// non-windowed step-1 result values, and the lazy mint
					// below is what it produces there.
					//
					// Two facts, two pins, and neither follows from the
					// other — the arm's prose asserted the first one
					// backwards for a while precisely because the second
					// makes the first invisible:
					//   - The arm is REACHED:
					//     TestRebaseOuterLegValue_LazyMintIsLive fires this
					//     rule on the shape OnMatch routes here and asserts
					//     the lazy mint appears in a yielded plan.
					//   - Its product WINS NOTHING today:
					//     TestLazyLegMintReachesNoWinningPlan measures zero
					//     dotted merged-row keys in the winning plan for
					//     every shape above.
					// The candidate carrying the mint loses and
					// OptimizeGroup prunes finals to the winner, so the
					// mint survives in neither the winning plan nor the
					// post-planning memo. That is why no row-level test
					// over those shapes can detect this code being
					// deleted, and why the reachability pin fires the rule
					// directly instead of planning SQL.
					if legLayout != nil {
						if id, idOK := legSlotIdentity(fv); idOK {
							if ord, ok := legLayout[id]; ok {
								return values.NewCorrelatedFieldValueWithResolvedOrdinal(
									values.NewQuantifiedObjectValue(mergedCorr),
									qualField, ord, fv.Typ,
								)
							}
						}
					}
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
		newChildren[i] = rebaseOuterLegValue(c, legAliases, mergedCorr, legLayout, legLocalTypes)
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
// EXISTS-over-join path uses. The ordinal twin is
// rebasePlanOuterRefsOrdinal (left_outer_existential.go) — the two walks
// and planReferencesAnyBuriedAlias enumerate the same node kinds and must
// move together. Pass-through nodes are rebuilt around their rebased
// inner; an unhandled node is returned as-is and caught by the post-rebase
// verification (planReferencesAnyBuriedAlias) which declines the probe so the
// correct materialized NLJ fallback wins.
func rebasePlanBuriedRefs(p plans.RecordQueryPlan, legAliases []string, mergedCorr values.CorrelationIdentifier, legLayout map[values.ColumnIdentity]int, legLocalTypes map[values.CorrelationIdentifier]*values.RecordType) plans.RecordQueryPlan {
	if p == nil || len(legAliases) == 0 {
		return p
	}
	switch pl := p.(type) {
	case *plans.RecordQueryIndexPlan:
		newComps, changed := rebaseComparisonRanges(pl.GetScanComparisons(), legAliases, mergedCorr, legLayout, legLocalTypes)
		if !changed {
			return p
		}
		return pl.WithScanComparisons(newComps)
	case *plans.RecordQueryScanPlan:
		newComps, changed := rebaseComparisonRanges(pl.GetScanComparisons(), legAliases, mergedCorr, legLayout, legLocalTypes)
		if !changed {
			return p
		}
		return pl.WithScanComparisons(newComps)
	case *plans.RecordQueryPredicatesFilterPlan:
		inner := rebasePlanBuriedRefs(pl.GetInner(), legAliases, mergedCorr, legLayout, legLocalTypes)
		preds := pl.GetPredicates()
		newPreds := make([]predicates.QueryPredicate, len(preds))
		changed := inner != pl.GetInner()
		for i, pr := range preds {
			newPreds[i] = rebaseOuterLegRefsToMerged(pr, legAliases, mergedCorr, legLayout, legLocalTypes)
			if newPreds[i] != pr {
				changed = true
			}
		}
		if !changed {
			return p
		}
		return plans.NewRecordQueryPredicatesFilterPlanWithAlias(inner, newPreds, pl.GetInnerAlias())
	case *plans.RecordQueryFilterPlan:
		inner := rebasePlanBuriedRefs(pl.GetInner(), legAliases, mergedCorr, legLayout, legLocalTypes)
		preds := pl.GetPredicates()
		newPreds := make([]predicates.QueryPredicate, len(preds))
		changed := inner != pl.GetInner()
		for i, pr := range preds {
			newPreds[i] = rebaseOuterLegRefsToMerged(pr, legAliases, mergedCorr, legLayout, legLocalTypes)
			if newPreds[i] != pr {
				changed = true
			}
		}
		if !changed {
			return p
		}
		return plans.NewRecordQueryFilterPlan(newPreds, inner)
	case *plans.RecordQueryFetchFromPartialRecordPlan:
		inner := rebasePlanBuriedRefs(pl.GetInner(), legAliases, mergedCorr, legLayout, legLocalTypes)
		if inner == pl.GetInner() {
			return p
		}
		return plans.NewRecordQueryFetchFromPartialRecordPlan(inner, pl.GetTranslateValueFunction(), pl.GetResultType(), pl.GetFetchIndexRecords())
	case *plans.RecordQueryDefaultOnEmptyPlan:
		inner := rebasePlanBuriedRefs(pl.GetInner(), legAliases, mergedCorr, legLayout, legLocalTypes)
		if inner == pl.GetInner() {
			return p
		}
		return plans.NewRecordQueryDefaultOnEmptyPlan(inner, pl.GetDefaultValue())
	case *plans.RecordQueryFirstOrDefaultPlan:
		inner := rebasePlanBuriedRefs(pl.GetInner(), legAliases, mergedCorr, legLayout, legLocalTypes)
		if inner == pl.GetInner() {
			return p
		}
		if pl.IsStrict() {
			return plans.NewRecordQueryFirstOrDefaultPlanStrict(inner, pl.GetDefaultValue())
		}
		return plans.NewRecordQueryFirstOrDefaultPlan(inner, pl.GetDefaultValue())
	case *plans.RecordQueryTypeFilterPlan:
		inner := rebasePlanBuriedRefs(pl.GetInner(), legAliases, mergedCorr, legLayout, legLocalTypes)
		if inner == pl.GetInner() {
			return p
		}
		return plans.NewRecordQueryTypeFilterPlan(pl.GetRecordTypes(), inner)
	case *plans.RecordQueryMapPlan:
		inner := rebasePlanBuriedRefs(pl.GetInner(), legAliases, mergedCorr, legLayout, legLocalTypes)
		newResult := rebaseOuterLegValue(pl.GetResultValue(), legAliases, mergedCorr, legLayout, legLocalTypes)
		if inner == pl.GetInner() && newResult == pl.GetResultValue() {
			return p
		}
		return plans.NewRecordQueryMapPlan(inner, newResult)
	case *plans.RecordQueryProjectionPlan:
		inner := rebasePlanBuriedRefs(pl.GetInner(), legAliases, mergedCorr, legLayout, legLocalTypes)
		projs := pl.GetProjections()
		newProjs := make([]values.Value, len(projs))
		changed := inner != pl.GetInner()
		for i, v := range projs {
			newProjs[i] = rebaseOuterLegValue(v, legAliases, mergedCorr, legLayout, legLocalTypes)
			if newProjs[i] != v {
				changed = true
			}
		}
		if !changed {
			return p
		}
		// A rebase hands back "the same projection, moved": the output names and
		// WHO wrote them are both unchanged by moving where the ordinals point.
		return plans.NewRecordQueryProjectionPlanWithAliases(newProjs, pl.GetAliases(), inner).
			WithAliasProvenance(pl.GetAliasMinted())
	default:
		// Unhandled node — return unchanged. planReferencesAnyBuriedAlias will detect
		// any buried reference that survives here and decline the probe.
		return p
	}
}

// rebaseComparisonRanges rebases the buried-leg references in a SARG's per-column
// comparison ranges onto mergedCorr. Returns the new ranges and whether any changed.
func rebaseComparisonRanges(comps []*predicates.ComparisonRange, legAliases []string, mergedCorr values.CorrelationIdentifier, legLayout map[values.ColumnIdentity]int, legLocalTypes map[values.CorrelationIdentifier]*values.RecordType) ([]*predicates.ComparisonRange, bool) {
	out := make([]*predicates.ComparisonRange, len(comps))
	changed := false
	for i, cr := range comps {
		nc, ch := rebaseComparisonRange(cr, legAliases, mergedCorr, legLayout, legLocalTypes)
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
func rebaseComparisonRange(cr *predicates.ComparisonRange, legAliases []string, mergedCorr values.CorrelationIdentifier, legLayout map[values.ColumnIdentity]int, legLocalTypes map[values.CorrelationIdentifier]*values.RecordType) (*predicates.ComparisonRange, bool) {
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
		nc := rebaseComparison(c, legAliases, mergedCorr, legLayout, legLocalTypes)
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
func rebaseComparison(c *predicates.Comparison, legAliases []string, mergedCorr values.CorrelationIdentifier, legLayout map[values.ColumnIdentity]int, legLocalTypes map[values.CorrelationIdentifier]*values.RecordType) *predicates.Comparison {
	if c == nil || c.Operand == nil {
		return c
	}
	newOperand := rebaseOuterLegValue(c.Operand, legAliases, mergedCorr, legLayout, legLocalTypes)
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
		case *plans.RecordQueryProjectionPlan:
			// The rebase walkers rewrite the projection VALUES (the node's only
			// correlation-bearing fields), so inspect exactly those — an
			// existential inner is routinely Projection-wrapped (`SELECT 1
			// FROM …`), and the fail-closed default arm would spuriously
			// decline every such correlation-free inner.
			for _, v := range sp.GetProjections() {
				if valueReferencesAlias(v, upper) {
					found = true
					break
				}
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

// allForEach reports whether every quantifier in qs is a ForEach — the leading
// N ForEach legs of the `[ForEach×N, Existential]` fold shape.
func allForEach(qs []expressions.Quantifier) bool {
	for _, q := range qs {
		if q.Kind() != expressions.QuantifierForEach {
			return false
		}
	}
	return true
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
	// The helper owns exactly the retained two-ForEach fold. Flat N-way
	// existential selects are partitioned before implementation.
	if len(quants) != 3 {
		return
	}

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

	// An EXPLODE leg (a lateral array unnest merged into the existential
	// select during rewriting) is not this arm's shape: the unnest+EXISTS
	// composition has its own dedicated nested lowering
	// (translateUnnestExistsFilter — the unnest FlatMap as the existential's
	// outer), which binds the element under its own alias with the BARE row
	// key. The two-level plan built here addresses outer legs through the
	// merged row's QUALIFIED keys, which an exploded element's row never
	// carries — the existential correlation would read NULL and drop every
	// row. Decline; the dedicated nested member carries the query (it always
	// has: this arm's previous materialized NLJ over a correlated Explode
	// materialized the element against an unbound context, yielded zero
	// rows, and never won).
	if getExplodeExpression(leftRef) != nil || getExplodeExpression(rightRef) != nil {
		return
	}

	leftExpr, _ := getWinnerForOrdering(leftRef, properties.PreserveOrdering(), call.CostModel())
	rightExpr, _ := getWinnerForOrdering(rightRef, properties.PreserveOrdering(), call.CostModel())
	existExpr, _ := getWinnerForOrdering(existRef, properties.PreserveOrdering(), call.CostModel())
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
	if existAlias == "" {
		// The positional source-alias triple is USER-FACING naming and a
		// coexisting select form (the memo-identity refinement lets the
		// LEFT-box and dissolved forms coexist as members) may not carry a
		// third entry. The correlation authority for the FirstOrDefault
		// column and the EXISTS residual is the existential QUANTIFIER'S
		// OWN alias — fall back to it rather than minting a zero-value
		// correlation (a construction panic) or declining a shape with no
		// other implementer (a no-plan failure; the null-supplied-conjunct
		// LEFT+EXISTS pin is exactly that query).
		existAlias = quants[2].GetAlias().Name()
	}
	if existAlias == "" {
		return // no correlation authority at all — fail closed
	}

	// The materialized step-1 NLJ executes each leg INDEPENDENTLY, so it
	// cannot implement (a) a NULL-ON-EMPTY leg (a dissolved LEFT box pulled
	// up by SelectMergeRule carries its outer-join semantics entirely on
	// that quantifier flag — a materialized INNER execution drops the
	// per-outer-row null-extension, and the EXISTS/NOT EXISTS twins over
	// `dept LEFT JOIN emp` returned identical rows), nor (b) a leg whose
	// subtree references its sibling (the dissolved box's ON predicate lives
	// INSIDE the null-supplying leg's subselect; embedded standalone, the
	// sibling reference resolves against the leg's own row through the
	// correlation-unchecked frontier fallback — E.DEPT_ID = D.ID degenerated
	// to emp.dept_id = emp.id → cross product). A decline is NOT enough:
	// REWRITING promotes one winner per group, and when the merged select
	// wins, the unmerged alternative is gone at PLANNING — a `SELECT *`
	// root then fails to plan at all. Implement the shape the way Java's
	// per-quantifier-property lowering does (ImplementNestedLoopJoinRule.
	// planPartitionToPhysical: null-on-empty → DefaultOnEmpty wrap, join
	// executes the inner correlated): orient the dependent/null-extended
	// leg INNER and build step 1 as the correlated FlatMap — the identical
	// construction (and identical provided-alias authority) as the
	// 2-quantifier dependency/strict-single FlatMap branches. Orientation keys on the
	// correlation topology, so the ChildrenAsSet swapped firing converges
	// to the same plan.
	leftProvided := physicalProvidedAliases(leftExpr, quants[0].GetAlias())
	rightProvided := physicalProvidedAliases(rightExpr, quants[1].GetAlias())
	leftDeps := legReferencesAny(leftRef, rightProvided)
	rightDeps := legReferencesAny(rightRef, leftProvided)
	if leftDeps && rightDeps {
		// Mutually correlated legs: neither orientation binds both. No
		// translation produces this today; decline rather than mis-execute.
		return
	}
	q0, q1 := quants[0], quants[1]
	correlatedStep1 := leftDeps || rightDeps || q0.IsNullOnEmpty() || q1.IsNullOnEmpty()
	if correlatedStep1 && (leftDeps || (q0.IsNullOnEmpty() && !rightDeps)) {
		leftPlan, rightPlan = rightPlan, leftPlan
		leftExpr, rightExpr = rightExpr, leftExpr
		leftAlias, rightAlias = rightAlias, leftAlias
		q0, q1 = q1, q0
	}
	if correlatedStep1 && q0.IsNullOnEmpty() {
		// A null-extended OUTER leg after orientation (both legs
		// null-on-empty, or a null-on-empty leg the other leg depends on):
		// no translation produces this; decline rather than drop the
		// extension.
		return
	}
	if correlatedStep1 && sel.GetJoinType() != expressions.JoinInner {
		// A select-level OUTER join type in the correlated arm is not
		// constructible today (the flatten and EXISTS-in-ON are INNER-only;
		// an undissolved outer box never gains an existential quantifier),
		// and the orientation swap above would wrap the WRONG side's inner in
		// the DefaultOnEmpty null-extension boundary. Decline defensively
		// rather than null-extend the preserved leg.
		return
	}
	if correlatedStep1 && resultValueReferencesAlias(sel.GetResultValue(), quants[2].GetAlias()) {
		// A PROJECTED-EXISTS fold (the result value reads the existential
		// quantifier) over a correlated/null-extended step 1: the step-1
		// FlatMap would evaluate the folded ExistsValue with a DEAD
		// existential binding, and the step-2 rebase would read qualified
		// keys off the fold's bare-keyed row. Not constructible today (the
		// projected fold is INNER-only upstream; the outer-join variant is
		// rejected before planning); decline defensively rather than yield
		// either wrongness.
		return
	}
	if correlatedStep1 {
		// The correlated step-1 binds the outer row under leftAlias and the
		// inner under rightAlias — an empty alias would bind under the zero
		// correlation identifier and every leg reference would miss.
		// Backfill from the quantifier aliases (the 2-quantifier path's
		// convention); decline if genuinely unnamed.
		if leftAlias == "" {
			leftAlias = q0.GetAlias().Name()
		}
		if rightAlias == "" {
			rightAlias = q1.GetAlias().Name()
		}
		if leftAlias == "" || rightAlias == "" {
			return
		}
	}
	// One identifier per leg, threaded from the leg's own quantifier — q0/q1 follow
	// the same orientation swap leftAlias/rightAlias did, so they are the typed
	// counterparts of exactly those two names. Everything below that used to mint
	// its own identifier from that text now reads these: the plan's leg identities,
	// the seed's QOV correlations, the correlated FlatMap's leg correlations. The
	// substitution is recorded rather than assumed, because the source-alias slice
	// and the quantifiers are independently populated.
	leftCorr, rightCorr := q0.GetAlias(), q1.GetAlias()
	if values.LegIdentityCensusEnabled() {
		values.RecordLegIdentityComparison(values.LegSiteNLJPlanAlias, leftAlias, leftCorr.Name())
		values.RecordLegIdentityComparison(values.LegSiteNLJPlanAlias, rightAlias, rightCorr.Name())
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

	// A projected-EXISTS fold over SCAN legs produces the FULL leg-concat
	// ordinal seed at step 1; the step-2 FlatMap then evaluates the folded
	// projection over that positional merged row through
	// legWindowRowContext (spanAwareRow resolves dotted "T1.ID" reads
	// positionally, legWindowBinder resolves QOV refs), so the projection is
	// NEVER rebased — no planner rebase, no dotted-name split. SCOPED to the
	// independent-legs materialized NLJ: a correlated step-1 binds legs by
	// NAME, where a baked seed hits the loud BakedNameContextError (an
	// earlier, simpler version of this seed had to be reverted twice over
	// exactly this trap). Only reached when both legs are ordinal-safe
	// single sources (scan-family).
	step1RV, gatedSeedStep1 := foldStep1Seed(
		sel.GetResultValue(), quants[2].GetAlias(), correlatedStep1,
		leftPlan, rightPlan, leftCorr, rightCorr)
	// Step 1: build the inner join (left × right). Its merged row is the
	// outer of the existential FlatMap. Independent legs take the
	// materialized NLJ; a null-on-empty or sibling-correlated leg takes the
	// correlated FlatMap (oriented above), whose DefaultOnEmpty wrap and
	// per-outer-row re-execution carry the semantics the NLJ cannot.
	// step1Expr is what the step-2 FlatMap's outer quantifier ranges over.
	step1Expr := leftExpr
	if correlatedStep1 {
		fmPlan, _, _, ok := buildCorrelatedFlatMapPlan(
			call,
			joinPreds, sel.GetResultValue(),
			leftPlan, rightPlan, leftCorr, rightCorr, leftExpr, rightExpr,
			joinType, q1.IsNullOnEmpty(), false, false,
		)
		if !ok {
			return
		}
		// The FlatMap plan already carries its two leg memo quantifiers (RFC-184 W2,
		// no physicalFlatMapWrapper) — it IS the step-1 expression the step-2 outer
		// quantifier ranges over.
		step1Expr = fmPlan
	} else {
		// step1Expr was `leftExpr` — the LEFT LEG ALONE — while the plan held the
		// whole join, so the step-2 FlatMap's outer child was not reachable from
		// its quantifier's group (RFC-183 §14). The materialized NLJ is its own
		// cascades expression carrying BOTH legs' memoized quantifiers directly
		// (RFC-184 W2, no physicalNestedLoopJoinWrapper), so the join IS reachable
		// and the plan and its quantifiers can no longer diverge; the
		// correlatedStep1 branch above does the same with its FlatMap. Both legs
		// keep their own interned groups, so this only ADDS reachability — no group
		// is narrowed.
		//
		// step1RV can be an ordinal seed even when gatedSeedStep1 above declined
		// to (re)construct one — foldStep1Seed only decides whether to BUILD a
		// fresh seed for a projected-EXISTS fold; a WHERE-EXISTS pass-through
		// keeps sel's OWN resultValue verbatim, which an earlier pass (the box
		// dissolution that produced this 3-quantifier select) may already have
		// baked as a pristine ordinal seed. A ChildrenAsSet-swapped firing of
		// THIS rule reuses that seed under a SWAPPED (leftAlias, rightAlias)
		// without rebuilding it (expressions.SelectExpression.
		// WithSwappedQuantifiers's doc comment) — decline rather than yield a
		// plan whose baked existential rebase (below) would address the wrong
		// slot of the executor's actual outer-then-inner merged row; see
		// materializedNLJOrdinalLayoutMatches's doc comment (structural
		// verification against leftPlan/rightPlan's own row shape — never by
		// alias string).
		if !materializedNLJOrdinalLayoutMatches(step1RV, leftPlan, rightPlan) {
			return
		}
		// The plan's leg identities and its own memo quantifiers' aliases are the
		// SAME identifiers, which is the invariant Java holds by construction
		// (RecordQueryFlatMapPlan carries the Quantifier, so there is nothing to keep
		// in step). Two independent mints from one string could drift apart here the
		// moment either side gained a normalization.
		nljPlan := plans.NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
			expressions.NamedForEachQuantifier(leftCorr, call.MemoizeExpression(leftExpr)),
			expressions.NamedForEachQuantifier(rightCorr, call.MemoizeExpression(rightExpr)),
			joinPreds,
			joinType,
			leftCorr, rightCorr,
			step1RV,
		)
		step1Expr = nljPlan
	}

	// The inner-join's merged row is bound under a FRESH outer correlation in
	// the existential FlatMap (Go's 3-quantifier join+EXISTS is a two-level
	// plan: NLJ → FlatMap, unlike Java's single FlatMap that keeps both source
	// aliases bound). Allocate that correlation up front so the existential
	// predicates can be rebased onto it before they are pushed into the inner.
	mergedOuterCorr := values.UniqueCorrelationIdentifier()

	// The outer-leg alias set the merged row anchors columns for — the two
	// top-level quantifier aliases. (An alias buried inside a leg that is
	// itself a JOIN/UNNEST subtree resolves through the ordinal seed windows
	// below — the spliced buried-leg windows.) Every residual/projected
	// reference to one of these reads the merged row's verbatim "LEG.COL"
	// key. RFC-142.
	outerLegAliases := mergedOuterLegAliases(leftAlias, rightAlias, leftCorr, rightCorr)

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
	// When step 1 produced an ordinal seed, existential predicates take the
	// ORDINAL rebase — leg references become baked ofOrdinalNumber over the
	// merged POSITIONAL row (offsets from the seed's own runs); the lazy
	// qualified-key rewrite remains for non-windowed step-1 RVs and is DEAD
	// when an ordinal seed was used (its FrontierPinned panic polices
	// exactly that). An un-mappable reference DECLINES the yield
	// (CORRECT-or-LOUD, never a half-rebased tree).
	ordinalWindows, mergedRowType := ordinalSeedLegWindowsOf(step1RV)
	var mergedQOV *values.QuantifiedObjectValue
	if ordinalWindows != nil {
		mergedQOV = values.NewQuantifiedObjectValueOfType(mergedOuterCorr, mergedRowType)
	}
	// The leg-local layouts, read off the legs' CHOSEN PHYSICAL PLANS. They are
	// what lets a leg-correlated read keep its own alias instead of being
	// re-anchored onto the merged correlation with the leg packed into a column
	// name — Java's model, where every quantifier stays bound and a reference is
	// an alias plus an ordinal. Derivable here and only here: the rebase walk sees
	// references carrying untyped quantifier objects, while this frame holds the
	// plans the rows will actually arrive from.
	existLegRowTypes := physicalLegRowTypes(
		struct {
			Plan  plans.RecordQueryPlan
			Alias values.CorrelationIdentifier
		}{leftPlan, leftCorr},
		struct {
			Plan  plans.RecordQueryPlan
			Alias values.CorrelationIdentifier
		}{rightPlan, rightCorr},
	)
	if len(existPreds) > 0 {
		rebased := make([]predicates.QueryPredicate, len(existPreds))
		for i, p := range existPreds {
			if ordinalWindows != nil {
				np, ok := rebaseOuterLegRefsOrdinal(p, ordinalWindows, mergedQOV)
				if !ok {
					return
				}
				rebased[i] = np
				continue
			}
			// No merged-row layout: the step-1 result value is not an ordinal
			// seed, so there is no merged slot to bake against. That is not a
			// reason to pack the leg into a column name — a leg-correlated read
			// can stay on its OWN alias and carry its OWN leg's ordinal, which
			// is what Java does and what the leg bindings now make resolvable.
			// The layouts come from the legs' physical plans; a read the layouts
			// cannot place still falls through to the qualified-name mint, and
			// the census records which.
			rebased[i] = rebaseOuterLegRefsToMerged(p, outerLegAliases, mergedOuterCorr, nil, existLegRowTypes)
		}
		existPreds = rebased
	}

	// Outer-leg references BURIED INSIDE the existential subplan get the
	// SAME rebase as the lifted existPreds above. An EXISTS whose inner
	// WHERE references only outer legs (`EXISTS (SELECT 1 FROM q WHERE
	// a.v = 10)`) keeps that predicate in its own plan tree —
	// existsInnerCorrelation lifts only inner↔outer correlation predicates —
	// and the step-2 FlatMap binds ONLY mergedOuterCorr, so the buried
	// QOV(leg) is unbound at runtime: the frontier fallback then evaluates
	// the field against the inner scan's own row (loud OrdinalResolutionError
	// on the ordinal frontier; silent wrong rows pre-ordinal). Check the
	// EXPRESSION-level correlation set (the same authority the step-1
	// orientation reads): an existential subtree that references NO outer leg
	// skips both the rebase and the fail-closed verification, keeping every
	// leg-independent EXISTS — including plan shapes the rebase walk does not
	// enumerate — byte-identical. A leg-referencing subtree is rebased, then
	// verified: a surviving leg reference declines the yield
	// (CORRECT-or-LOUD; those shapes evaluated an unbound correlation
	// before, so a decline is strictly no worse). Window keys usually repeat
	// the leg aliases — duplicates and map order are irrelevant (every
	// consumer builds a set).
	verifyAliases := append([]string{}, outerLegAliases...)
	for alias := range ordinalWindows {
		verifyAliases = append(verifyAliases, alias)
	}
	existLegCorrs := map[values.CorrelationIdentifier]struct{}{
		q0.GetAlias(): {},
		q1.GetAlias(): {},
	}
	for _, a := range verifyAliases {
		if a != "" {
			existLegCorrs[values.NamedCorrelationIdentifier(a)] = struct{}{}
		}
	}
	if legReferencesAny(existRef, existLegCorrs) {
		if ordinalWindows != nil {
			np, ok := rebasePlanOuterRefsOrdinal(existPlan, ordinalWindows, mergedQOV)
			if !ok {
				return
			}
			existPlan = np
		} else {
			existPlan = rebasePlanBuriedRefs(existPlan, outerLegAliases, mergedOuterCorr, nil, existLegRowTypes)
		}
		if planReferencesAnyBuriedAlias(existPlan, verifyAliases) {
			return
		}
	}

	// Preserve existCorr at every compensating step: the completed FlatMap
	// binds that correlation and must not leak it as an external dependency.
	innerQ := expressions.NamedPhysicalQuantifier(existCorr, call.MemoizeExpression(existExpr))
	innerQ = buildExistsCompensationChain(
		call, innerQ, existPlan, existCorr, existPreds,
		hasExistsFilter, negated, true,
	)

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
		// projected-EXISTS result value) the set degenerates to
		// {leftAlias,rightAlias}, so this is a no-op for that path. RFC-142.
		var projected values.Value
		if gatedSeedStep1 {
			// The folded projection's leg references —
			// dotted frontier reads ("T1.ID") AND QOV refs, heterogeneous as the
			// resolver emits them — are NOT rebased here. The step-2 FlatMap
			// cursor evaluates the projection over the step-1 ordinal merged row
			// through legWindowRowContext (spanAwareRow resolves the dotted reads
			// positionally against the leg windows, legWindowBinder the QOV refs),
			// so no planner rebase and no dotted-name split is needed. Only the
			// existential quantifier alias is re-aliased below.
			projected = sel.GetResultValue()
		} else if ordinalWindows != nil {
			// Ordinal seed: the projected-EXISTS fold's leg references
			// rebase to baked merged ordinals (the lazy rewrite would
			// panic on the baked refs — same policing as the predicate side).
			var ok bool
			projected, ok = rebaseOuterLegValueOrdinal(sel.GetResultValue(), ordinalWindows, mergedQOV)
			if !ok {
				return
			}
		} else {
			projected = rebaseOuterLegValue(sel.GetResultValue(), outerLegAliases, mergedOuterCorr, nil, existLegRowTypes)
		}
		// Existential quantifier alias → the FlatMap inner binding (existCorr).
		if quants[2].GetAlias() != existCorr {
			projected = values.RebaseValue(projected, values.AliasMap{quants[2].GetAlias(): existCorr})
		}
		flatMapResult = projected
	}

	// ALIAS CONTRACT — PRESERVE the FlatMap plan's REAL outer/inner aliases
	// (mergedOuterCorr/existCorr), never fresh ones: same EXISTS correlation-leak
	// class as yieldExistsFlatMap — a fresh outer alias fails to subtract the FOD
	// inner's correlation to mergedOuterCorr, leaking it upward.
	//
	// The outer quantifier ranges over step1Expr (the step-1 inner join, its own
	// cascades expression since RFC-184 W2). In the correlatedStep1 branch that IS
	// the FlatMap; in the materialized-NLJ branch it is the NLJ built over both
	// legs' memoized quantifiers — so the join is reachable and resolves. The
	// FlatMap is its own cascades expression carrying its outer edge (over
	// step1Expr) and its inner edge (innerQ) directly (no physicalFlatMapWrapper).
	leftMemoRef := call.MemoizeExpression(step1Expr)
	flatMapPlan := plans.NewRecordQueryFlatMapPlanFromQuantifiers(
		expressions.NamedForEachQuantifier(mergedOuterCorr, leftMemoRef),
		innerQ,
		mergedOuterCorr, existCorr,
		flatMapResult, true,
	)
	call.Yield(flatMapPlan)
}

func correlatedExistsComparisonRange(
	outerValue *values.FieldValue,
	outerCorrelation values.CorrelationIdentifier,
) (*predicates.ComparisonRange, bool) {
	correlatedOperand, ok := correlatedFastPathOperand(outerValue, outerCorrelation)
	if !ok {
		return nil, false
	}
	comparison := &predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: correlatedOperand,
	}
	mergeResult := predicates.EmptyComparisonRange().Merge(comparison)
	return mergeResult.Range, mergeResult.Ok
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

	outerCorrelation := values.NamedCorrelationIdentifier(outerAlias)
	innerCorrelation := values.NamedCorrelationIdentifier(innerAlias)

	// The inner leg's own output row: the layout every comparand correlated to
	// innerAlias reads, and the layout the inner leg's metadata key-column
	// names are resolved against. Unknown means there is no declared column
	// order to state the proof in, so the whole fast path declines rather than
	// falling back to the names it still has.
	innerLayout := planRowLayout(innerScan)
	innerFrontier := values.OrdinalDomainOfType(innerLayout)
	if !innerFrontier.IsKnown() {
		return false
	}

	// Try PK first.
	pkCols := call.Context.GetPrimaryKeyColumns(recordTypes[0])
	if len(pkCols) > 0 {
		// The metadata name dies HERE — resolved once, against the layout its
		// ordinal will be compared in (Java's FieldValue.resolveFieldPath,
		// FieldValue.java:270-298). Nothing downstream sees a column name.
		pkIdent, pkResolved := values.OrdinalOfNameIn(innerLayout, pkCols[0])
		if !pkResolved {
			return false
		}
		for _, pred := range preds {
			cp, ok := pred.(*predicates.ComparisonPredicate)
			if !ok || cp.Comparison.Type != predicates.ComparisonEquals {
				continue
			}
			if cp.Operand == nil || cp.Comparison.Operand == nil {
				continue
			}
			outerVal := r.matchJoinPKPredicate(
				cp, outerCorrelation, innerCorrelation, pkIdent, innerFrontier)
			if outerVal == nil {
				continue
			}
			if outerValRefsBuriedLeg(outerVal, outerCorrelation) {
				continue
			}
			comparisonRange, ok := correlatedExistsComparisonRange(
				outerVal, outerCorrelation)
			if !ok {
				return false
			}
			correlatedScan := innerScan.WithScanComparisons(
				[]*predicates.ComparisonRange{comparisonRange})
			r.yieldExistsFlatMap(
				call, resultValue, outerPlan, correlatedScan,
				outerCorrelation, innerCorrelation, outerExpr, innerExpr,
				hasExistsFilter, negated, pred, preds,
			)
			return true
		}
	}

	// Try secondary indexes.
	for _, cand := range call.Context.GetMatchCandidates() {
		candCols, plainFields := candidatePlainFieldColumnsForShortcut(cand)
		if !plainFields {
			continue
		}
		// This fast path builds a raw correlated index scan from the flat
		// first-column metadata and bypasses the candidate traversal plus its
		// cardinality compensation. For a composite (FK, repeated FAN_OUT)
		// index, records with an empty repeated field have no index entry, so
		// probing FK through this shortcut can turn a true EXISTS into false.
		if !candidatePreservesBaseRecordCardinality(cand) {
			continue
		}
		if len(candCols) == 0 {
			continue
		}
		candTypes := cand.GetRecordTypes()
		if len(candTypes) == 0 || candTypes[0] != recordTypes[0] {
			continue
		}
		// Same boundary resolution as the PK arm: the index definition names
		// its first column, that name is resolved once against the inner leg's
		// row layout, and it dies there.
		idxIdent, idxResolved := values.OrdinalOfNameIn(innerLayout, candCols[0])
		if !idxResolved {
			continue
		}
		for _, pred := range preds {
			cp, ok := pred.(*predicates.ComparisonPredicate)
			if !ok || cp.Comparison.Type != predicates.ComparisonEquals {
				continue
			}
			if cp.Operand == nil || cp.Comparison.Operand == nil {
				continue
			}
			outerVal := r.matchJoinPKPredicate(
				cp, outerCorrelation, innerCorrelation, idxIdent, innerFrontier)
			if outerVal == nil {
				continue
			}
			if outerValRefsBuriedLeg(outerVal, outerCorrelation) {
				continue
			}
			// Build correlated index scan.
			comparisonRange, ok := correlatedExistsComparisonRange(
				outerVal, outerCorrelation)
			if !ok {
				continue
			}
			correlatedIndexScan := plans.NewRecordQueryIndexPlan(
				cand.CandidateName(),
				[]*predicates.ComparisonRange{comparisonRange},
				recordTypes, innerScan.GetFlowedType(), false,
			)

			innerCorrelation := values.NamedCorrelationIdentifier(innerAlias)
			r.yieldExistsFlatMap(
				call, resultValue, outerPlan, correlatedIndexScan,
				outerCorrelation, innerCorrelation, outerExpr, innerExpr,
				hasExistsFilter, negated, pred, preds,
			)
			return true
		}
	}
	return false
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
	matchedPredicate predicates.QueryPredicate,
	allPredicates []predicates.QueryPredicate,
) {
	var innerResiduals, outerResiduals []predicates.QueryPredicate
	for _, predicate := range allPredicates {
		if predicate == matchedPredicate {
			continue
		}
		if _, ok := predicates.GetCorrelatedToOfPredicate(
			predicate,
		)[innerCorrelation]; ok {
			innerResiduals = append(innerResiduals, predicate)
		} else {
			outerResiduals = append(outerResiduals, predicate)
		}
	}

	// ALIAS CONTRACT — PRESERVE (NamedPhysicalQuantifier over
	// outerCorrelation/innerCorrelation), never fresh. The FOD inner reports its
	// correlation to outerCorrelation; Reference.GetCorrelatedTo subtracts each
	// member's quantifier aliases from its children's correlations, so a fresh
	// alias fails to subtract it and a COMPLETED correlated-EXISTS FlatMap leaks
	// outerCorrelation upward → an enclosing multiway join sees the subplan as
	// still externally correlated and skips valid alternatives (the EXISTS twin
	// of the yieldGeneralFlatMap leak). This is the OPPOSITE of
	// implementExistentialSelect's chain, which must mint FRESH aliases — see the
	// two-sites-two-contracts note in buildCorrelatedFlatMapPlan. The distinction
	// is not cosmetic and neither site may be "harmonized" onto the other.
	//
	// The BASE quantifiers deliberately keep ranging over outerExpr/innerExpr's
	// interned groups. correlatedInner is a SARG-pushed rewrite of innerExpr's
	// scan, so the base edge still diverges — but repairing it means REPLACING
	// the reference with a fresh singleton rather than wrapping it, which
	// destroys the alternatives the group holds. That is the exact move that
	// regressed the IN-join rule (RFC-183 §13); only ADD reachability here.
	rightQ := expressions.NamedPhysicalQuantifier(innerCorrelation, call.MemoizeExpression(innerExpr))
	rightQ = buildExistsCompensationChain(
		call, rightQ, correlatedInner, innerCorrelation, innerResiduals,
		hasExistsFilter, negated, true,
	)

	leftQ := expressions.NamedForEachQuantifier(outerCorrelation, call.MemoizeExpression(outerExpr))

	if len(outerResiduals) > 0 {
		ofInnerQ := expressions.NamedForEachQuantifier(leftQ.GetAlias(),
			call.MemoizeFinalExpression(outerPlan))
		outerFilter := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(ofInnerQ, outerResiduals, outerCorrelation)
		leftQ = expressions.NamedForEachQuantifier(outerCorrelation,
			call.MemoizeFinalExpression(outerFilter))
	}

	// The EXISTS FlatMap is its own cascades expression carrying its outer (leftQ)
	// and inner (rightQ) memo edges directly (RFC-184 W2, no physicalFlatMapWrapper).
	flatMapPlan := plans.NewRecordQueryFlatMapPlanFromQuantifiers(
		leftQ, rightQ,
		outerCorrelation, innerCorrelation,
		resultValue, true,
	)
	r.yieldBinaryJoinWithOrderingVariants(call, flatMapPlan)
}

// scalarSubqueryAliasesOfPredicate collects the correlation aliases a
// predicate references through ScalarSubqueryValue nodes — STRUCTURAL
// detection (the node type carries its own alias), never name matching. These
// aliases are pre-evaluated external bindings resolved from the root
// evaluation context, not quantifier legs: rebase authorities and buried-ref
// declines must pass them through.
func scalarSubqueryAliasesOfPredicate(p predicates.QueryPredicate) map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	// ReplaceValues already visits every value node pre-order (the legRowTypes
	// idiom) — inspect v directly, no nested walk.
	predicates.ReplaceValues(p, func(v values.Value) values.Value {
		if ssv, ok := v.(*values.ScalarSubqueryValue); ok {
			out[ssv.Alias] = struct{}{}
		}
		return v
	})
	return out
}

// matchJoinPKPredicate reports the OUTER-side comparand of an equality
// predicate shaped `outer.FK = inner.<key>` (or reversed), where `<key>` is
// the inner leg's primary-key or index first column. nil means no match.
//
// The inner side is matched by column IDENTITY, not by leaf name: keyIdent is
// the metadata key column resolved ONCE against the inner leg's own row layout
// (the boundary rule), and frontier is that layout. See readsKeyColumn for the
// Java citations — Java matches a key column through Placeholder/semanticEquals
// on resolved ordinals and never compares a column name here at all.
//
// There is deliberately no third, "deep-flowed" arm. One existed, accepting an
// outer comparand on ANY leg other than the inner one so a re-enumerated
// multi-way chain could probe through a table buried inside the outer
// sub-join. It could not do that: both call sites pass every accepted
// comparand straight to outerValRefsBuriedLeg, which declines exactly the legs
// that arm alone admitted (anything that is neither the outer nor the inner
// leg), so no rewrite it accepted could ever be yielded. The arms are gone
// rather than left as reachable-looking dead code; jointBuriedLegDecline in the
// tests pins the decline that made them dead, so re-arming the capability has
// to go through the buried-leg rebase rather than through a silent bare-name
// read of the merged row.
func (r *ImplementNestedLoopJoinRule) matchJoinPKPredicate(
	cp *predicates.ComparisonPredicate,
	outerCorr, innerCorr values.CorrelationIdentifier,
	keyIdent values.ColumnIdentity,
	frontier values.OrdinalDomain,
) *values.FieldValue {
	lhsFV, lhsOk := cp.Operand.(*values.FieldValue)
	rhsFV, rhsOk := cp.Comparison.Operand.(*values.FieldValue)
	if !lhsOk || !rhsOk {
		return nil
	}
	lhsLeg, lhsHasLeg := legCorrelationOf(lhsFV)
	rhsLeg, rhsHasLeg := legCorrelationOf(rhsFV)
	if !lhsHasLeg || !rhsHasLeg {
		return nil
	}
	if values.SameLeg(lhsLeg, outerCorr) && readsKeyColumn(rhsFV, innerCorr, keyIdent, frontier) {
		return lhsFV
	}
	if values.SameLeg(rhsLeg, outerCorr) && readsKeyColumn(lhsFV, innerCorr, keyIdent, frontier) {
		return rhsFV
	}
	return nil
}

// legCorrelationOf returns the CORRELATION whose row a comparand reads its
// column off — RFC-197's first identity element, and the only thing the
// alias-only callers ever wanted out of the old fieldValueAliasAndCol.
//
// The correlation comes STRUCTURALLY from a direct QuantifiedObjectValue
// child. A fused multi-accessor path (Child=QOV directly, Resolved=[ADDRESS,
// ID]) declines: it reads a column of a NESTED record, so the quantifier's row
// is not the layout its leaf names, and reporting the root quantifier would
// let `inner.address.id` impersonate the table's real top-level `ID` — a
// wrong-rows rewrite, not merely a worse plan. Same allowlist as
// correlatedFieldOf (fk_chain_cardinality.go) and correlatedInnerField
// (plans/cost.go).
//
// Not-ok is the FAIL-CLOSED answer; every caller treats it as "cannot state
// which leg this reads" and declines the rewrite.
func legCorrelationOf(fv *values.FieldValue) (values.CorrelationIdentifier, bool) {
	if fv == nil {
		return values.CorrelationIdentifier{}, false
	}
	if qov, ok := fv.Child.(*values.QuantifiedObjectValue); ok {
		if fv.Resolved != nil {
			if _, single := fv.Resolved.Single(); !single {
				return values.CorrelationIdentifier{}, false
			}
		}
		return qov.Correlation, true
	}
	// A CHILDLESS value states no quantifier. There used to be an arm here that
	// recovered one by slicing the qualifier out of the display name — the
	// qualified-name CHANNEL (RFC-197 bucket 6). It is gone rather than
	// retagged, because keeping it would have moved a name decision somewhere
	// the build-time gate cannot see: the slice fed a CorrelationIdentifier
	// constructor, and the gate deliberately does not flag construction, so the
	// site would have gone quiet while still deciding which leg a value reads
	// from how it is spelled. A migration that launders a decision into
	// invisibility is worse than one that leaves it listed.
	//
	// Failing closed here is safe at every caller, which is why the arm could
	// go before its producers do: matchJoinPKPredicate declines the rewrite,
	// outerValRefsBuriedLeg reports "buried" and declines, and
	// predicateSingleSide attributes the reference to neither side and keeps
	// the predicate above the join. Each is a declined optimization, never a
	// wrong slot. Nothing in the explaindiff corpus takes the arm (0 of 1944
	// calls), so the optimization was theoretical.
	return values.CorrelationIdentifier{}, false
}

// readsKeyColumn reports whether fv is a bare reference to keyIdent's column,
// read off the leg bound under legCorr.
//
// This is the identity comparison that replaces the leaf-name match. Java
// never asks this question by name: an index/PK key column becomes a
// Placeholder at candidate-expansion time and the query predicate is matched
// to it by Value.semanticEquals under a ValueEquivalence
// (PredicateWithValueAndRanges.java:312-323, Value.java:763-771), where
// FieldValue.equalsWithoutChildren compares the resolved field PATH
// (FieldValue.java:214-216) and ResolvedAccessor.equals compares the ORDINAL
// ONLY (FieldValue.java:683-684). The metadata name is resolved to that
// ordinal exactly once, in FieldValue.resolveFieldPath
// (FieldValue.java:270-298), and is dead weight afterwards — Java even says so
// where it kept one for debugging (ScanWithFetchMatchCandidate.java:123: "field
// names are for debugging purposes only, we should probably use field ordinals
// here instead").
//
// keyIdent is that once-resolved metadata name, stated in the inner leg's own
// row layout; frontier is that same layout. A comparand whose ordinal indexes
// some OTHER layout fails OrdinalIn and declines rather than matching on the
// name it happens to share.
func readsKeyColumn(
	fv *values.FieldValue,
	legCorr values.CorrelationIdentifier,
	keyIdent values.ColumnIdentity,
	frontier values.OrdinalDomain,
) bool {
	id, ok := fv.CorrelatedIdentityIn(frontier)
	if !ok {
		return false
	}
	return values.SameLeg(id.Correlation, legCorr) &&
		id.Domain == keyIdent.Domain && id.Ordinal == keyIdent.Ordinal
}

// outerValRefsBuriedLeg reports whether a fast-path correlation's outer
// FieldValue references a BURIED leg of a merged-box outer — its alias differs
// from the binding alias (the box's rightmost-leg source alias). The fast
// path (tryExistsFlatMap's primary/index probes) builds
// QOV(outerAlias).<bareCol>, which reads the box's rightmost leg;
// for a correlation into a NON-rightmost buried leg (`a.gid` over a
// `la LEFT JOIN lb` box bound as B) the bare read is last-leg-wins and reads
// the WRONG leg on a colliding column name — a wrong-rows bug
// (matchJoinPKPredicate's deep-flowed arm accepted the buried ref without
// rebasing it). Declining routes it to the below-FOD rebase, which rewrites
// the buried reference to the merged row's QUALIFIED key or a
// BAKED ordinal (an ordinalized box) — both leg-correct. The optimization is only lost
// for existential correlations into a buried box leg; the common
// single-source and rightmost-leg cases still take the fast path.
// It FAILS CLOSED: a comparand whose leg cannot be stated structurally is
// treated as buried, because "which leg does this read" is precisely the
// question the fast path must answer before it may rebuild the probe as
// QOV(outerAlias).<col>.
func outerValRefsBuriedLeg(outerVal *values.FieldValue, outerCorr values.CorrelationIdentifier) bool {
	leg, ok := legCorrelationOf(outerVal)
	return !ok || !values.SameLeg(leg, outerCorr)
}

// correlatedFastPathOperand builds the outer-side operand pushed into the
// parameterized inner PK/index scan, or declines.
//
// A BAKED ordinal ref (FrontierPinned — the ordinal-seed contract) is used
// AS-IS: it already reads its outer column POSITIONALLY off the merged row
// bound under outerCorrelation, and re-deriving it by bare NAME would misread a
// SHADOWED or duplicate column name in the merged row — the unnest's AS/AT
// alias and an outer column can share a name, and a bare
// `QOV(outer).<name>` read then resolves to the element instead of the outer
// column (`FROM t, t.arr AS ID WHERE EXISTS(u.id = t.id)` probed U with the
// element and dropped every row).
//
// A LAZY ref (no resolved path at all) DECLINES. It used to be rebuilt as a
// bare `QOV(outer).<name>`, which is not a weaker operand but an UNEVALUABLE
// one: FieldValue.evaluateOrdinal has no runtime name-resolution fallback and
// answers OrdinalResolutionError{Ordinal: -1} for any unbaked reference
// (values.go:789-793, "There is NO runtime name-resolution fallback"). So that
// arm could only ever have manufactured a plan that fails loud at execution;
// declining hands the query to the general correlated path, which rebases the
// reference properly. Nothing in the explaindiff corpus reaches it.
func correlatedFastPathOperand(
	outerVal *values.FieldValue,
	outerCorrelation values.CorrelationIdentifier,
) (values.Value, bool) {
	if outerVal.Resolved != nil && outerVal.Resolved.FrontierPinned {
		return outerVal, true
	}
	// A SOURCE-RELATIVE bake (the resolver's construction-time
	// ordinal, addressed to a real SQL source) transfers to the rebuilt operand
	// verbatim — the fast path only admits references to the outer source
	// itself (outerValRefsBuriedLeg declines buried legs), so the row bound
	// under outerCorrelation IS that source's row and the declared-column-order
	// ordinal reads the right slot.
	if outerVal.SourceRelativeBaked() {
		// The rebuilt operand's DISPLAY name. Its identity is the ordinal
		// passed beside it; this string is never compared, keyed or resolved,
		// and the constructor is the one place RFC-197 leaves a name
		// legitimate. It is taken VERBATIM: there used to be a branch here
		// that sliced a qualifier out of a CHILDLESS reference's dotted
		// display name (the qualified-name channel, RFC-197 bucket 6), and it
		// was unreachable by the same construction that justified deleting
		// matchJoinPKPredicate's deep-flowed arms — both callers gate on
		// outerValRefsBuriedLeg, which routes through legCorrelationOf and
		// fails closed on a childless value, so outerVal always has a
		// QuantifiedObjectValue child by the time it arrives here.
		return values.NewCorrelatedFieldValueWithResolvedOrdinal(
			values.NewQuantifiedObjectValue(outerCorrelation),
			outerVal.Field,
			outerVal.Resolved.Root().Ordinal,
			outerVal.Typ,
		), true
	}
	return nil, false
}

var _ ExpressionRule = (*ImplementNestedLoopJoinRule)(nil)
