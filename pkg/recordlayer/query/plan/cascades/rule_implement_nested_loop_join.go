package cascades

import (
	"errors"
	"fmt"
	"reflect"

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
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("select_for_nlj").WithRootPredicate(
			func(sel *expressions.SelectExpression) bool { return len(sel.GetQuantifiers()) == 2 }),
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

// ConstraintDependencies is Java's ImmutableSet.of(REQUESTED_ORDERING).
func (r *ImplementNestedLoopJoinRule) ConstraintDependencies() []any {
	return []any{RequestedOrderingConstraintKey}
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

	// Every select this rule implements is BINARY, matching Java, whose matcher is
	// `exactlyInAnyOrder(outerQuantifierMatcher, innerQuantifierMatcher)`
	// (ImplementNestedLoopJoinRule.java:98). An N-ary select — including
	// [ForEach, ForEach, Existential] — is decomposed by PartitionSelectRule
	// before it ever reaches here.
	//
	// A three-quantifier arm used to live at this spot, implementing that shape
	// directly as a two-level NLJ→FlatMap over a merged outer row. It existed
	// because PartitionSelectRule could not decompose the shape, and it could not
	// because Java's Case-1 existential peel produced a plan that returned zero
	// rows in Go — the peeled lower's EXISTS predicate reached a PredicatesFilter
	// in its structural form. With the residual conversion in place the peel works
	// and the arm is gone, and with it the merged outer row that had no ordinal
	// layout and the runtime alias namespace the executor kept to address it
	// (RFC-235).
	if len(quants) != 2 {
		return
	}

	if quants[0].Kind() == expressions.QuantifierExistential || quants[1].Kind() == expressions.QuantifierExistential {
		leftDepends := referenceIsCorrelatedTo(quants[0].GetRangesOver(), quants[1].GetAlias())
		rightDepends := referenceIsCorrelatedTo(quants[1].GetRangesOver(), quants[0].GetAlias())
		if leftDepends && rightDepends {
			return
		}
		aliases := sel.GetSourceAliases()
		// Dependencies take precedence over the usual ForEach-outer orientation:
		// a correlated ForEach must execute after its existential witness.
		if leftDepends || (!rightDepends && quants[0].Kind() == expressions.QuantifierExistential && quants[1].Kind() == expressions.QuantifierForEach) {
			quants = []expressions.Quantifier{quants[1], quants[0]}
			if len(aliases) >= 2 {
				aliases = []string{aliases[1], aliases[0]}
			}
		}
		// The existential lowering wraps only a null-on-empty OUTER in its
		// DefaultOnEmpty; an inner one would lose its null-extended row.
		if quants[1].IsNullOnEmpty() {
			return
		}
		r.implementExistentialSelect(call, sel, quants, aliases)
		return
	}

	leftRef := quants[0].GetRangesOver()
	rightRef := quants[1].GetRangesOver()
	if leftRef == nil || rightRef == nil {
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
		leftQ := expressions.NewPhysicalQuantifier(call.MemoizeExpression(leftExpr))
		rightQ := expressions.NewPhysicalQuantifier(call.MemoizeExpression(rightExpr))
		joinPredicates, joinResultValue, err := normalizeMaterializedJoinPrograms(
			sel.GetPredicates(), sel.GetResultValue(),
			leftPlan, leftCorr, rightPlan, rightCorr)
		if err != nil {
			call.Fail(err)
			return
		}
		// The materialized NLJ is its own cascades expression carrying its two leg
		// edges directly (RFC-184 W2, no physicalNestedLoopJoinWrapper) — both legs
		// are the live shared-group edges over the memoized leg exprs.
		plan, err := plans.NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
			leftQ, rightQ,
			joinPredicates,
			joinType,
			leftCorr, rightCorr,
			joinResultValue,
		)
		if err != nil {
			call.Fail(err)
			return
		}
		call.Yield(plan)
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
	// A leg that FLOWS a scalar (not a record) is joined by a FlatMap, as every
	// join is in Java (ImplementNestedLoopJoinRule yields
	// RecordQueryFlatMapPlan): the FlatMap binds each quantifier's alias to its
	// flowed object itself, where the materialized NLJ binds each leg as a
	// record and would read a scalar as its one-slot row. The flowed type is the
	// property, whatever produces it — a scalar Explode (a lateral unnest whose
	// collection an enclosing query owns, `EXISTS (SELECT 1 FROM h, w.arr AS
	// v)`) or a filter or projection over one. (An uncorrelated constant or
	// parameter list never gets here: the IN-join rule owns it, above.) The
	// scalar leg becomes the FlatMap's inner, re-evaluated
	// per outer row; when it is a LEFT OUTER's preserved leg, which cannot move
	// inside, it stays the outer and the other leg becomes the inner. (FULL
	// OUTER returned above: it has only the materialized implementation.)
	switch {
	case scalarFlowingLeg(quants[1]) && !leftNeedsRightEvaluation:
		rightNeedsLeftEvaluation = true
	case scalarFlowingLeg(quants[0]) && !rightNeedsLeftEvaluation:
		if canSwap {
			leftNeedsRightEvaluation = true
		} else {
			rightNeedsLeftEvaluation = true
		}
	}
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
			// Both legs read one alias neither provides. As a materialized NLJ
			// this select is only valid INSIDE the operator that binds that
			// alias, which a materialized plan cannot demand. A FlatMap can:
			// its inner is evaluated per outer row, with the enclosing binding
			// in scope, and it reports that correlation upward so it is never a
			// root with the alias unbound. It is Java's only implementation
			// of a binary select (ImplementNestedLoopJoinRule yields nothing
			// else), and for two lateral siblings correlated to one earlier
			// leg (`FROM w, (… w.f …) AS d, (… w.f …) AS e`, the lower {d, e})
			// it is the only one.
			// No leg is strict-single here (such a leg requires the FlatMap
			// above). The left leg is the outer of this orientation; an INNER
			// select is also fired swapped. A null-on-empty leg is one
			// RewriteOuterJoinRule produced (its ON conjuncts inside the leg),
			// which a partition of that rule's output select can separate from
			// the leg it reads — on either side; an output select whose inner
			// reads the preserved leg takes the correlated branch below. The
			// lowering wraps the flagged leg in DefaultOnEmpty where it sits, as
			// outer or as inner: bag-equivalent orientations, which Java
			// enumerates through its matcher (exactlyInAnyOrder,
			// ImplementNestedLoopJoinRule.java:99) and wraps in
			// planPartitionToPhysical. A LEFT OUTER input — the select the
			// rewrite starts from, which stays a member beside its output (`…
			// (… w.f …) AS a LEFT JOIN (… w.id …) AS b ON b.k > 1`) — has its
			// ON conjuncts in its own predicate list, and the lowering filters
			// them below the DefaultOnEmpty; the materialized join cannot bind
			// the sibling both legs read, so this is its implementation.
			r.yieldGeneralFlatMap(call, sel,
				leftPlan, rightPlan, leftCorr, rightCorr,
				leftExpr, rightExpr, leftRef, rightRef, joinType,
				quants[0].IsNullOnEmpty(), quants[1].IsNullOnEmpty(), false)
			return
		}
		// RewriteOuterJoinRule represents LEFT OUTER as an INNER Select whose
		// null-supplying edge is marked NullOnEmpty.  That marker has meaning only
		// in the FlatMap lowering, where DefaultOnEmpty wraps the leg where it
		// sits. An ordinary materialized NLJ neither reads nor preserves the edge
		// flag: yielding one here with JoinInner would drop unmatched preserved
		// rows. Legs that read nothing of each other are bag-equivalent in either
		// orientation, so the FlatMap (Java's only implementation) serves.
		if quants[0].IsNullOnEmpty() || quants[1].IsNullOnEmpty() {
			r.yieldGeneralFlatMap(call, sel,
				leftPlan, rightPlan, leftCorr, rightCorr,
				leftExpr, rightExpr, leftRef, rightRef, joinType,
				quants[0].IsNullOnEmpty(), quants[1].IsNullOnEmpty(), false)
			return
		}
		// NOTE: a ChildrenAsSet-swapped firing (fireExprRuleOnMember) reuses
		// sel's resultValue verbatim under the swapped orientation, which would
		// be unsound for a pristine ordinal seed IF some downstream consumer
		// later read that seed's baked ordinals expecting the UNSWAPPED physical
		// layout. No such consumer exists at THIS construction site — this plan
		// is embedded and yielded as-is, nothing here reads its resultValue's
		// ordinals — so no orientation check belongs here; declining on a
		// mismatch nothing will act on rejected working, tested plans
		// (TestFDB_QuotedMachineShapedAliases/join_legs' swapped cross-join
		// orientation, which never consumes its own seed's ordinals downstream).
		//
		// There is now no such consumer ANYWHERE, which is a change worth stating
		// rather than leaving implied: the one site that did read this seed's
		// ordinal windows — to rebase EXISTS predicates onto baked slots — was the
		// three-quantifier arm, retired by RFC-235 along with the orientation
		// check it carried. So this is not "the check lives elsewhere"; it is
		// "nothing reads these ordinals". A future consumer must bring its own
		// check, and the argument above is the reason one cannot simply be added
		// here on suspicion.
		leftQ := expressions.NewPhysicalQuantifier(call.MemoizeExpression(leftExpr))
		rightQ := expressions.NewPhysicalQuantifier(call.MemoizeExpression(rightExpr))
		joinPredicates, joinResultValue, err := normalizeMaterializedJoinPrograms(
			sel.GetPredicates(), sel.GetResultValue(),
			leftPlan, leftCorr, rightPlan, rightCorr)
		if err != nil {
			call.Fail(err)
			return
		}
		// The materialized NLJ is its own cascades expression carrying its two leg
		// edges directly (RFC-184 W2, no physicalNestedLoopJoinWrapper) — both legs
		// are the live shared-group edges over the memoized leg exprs.
		plan, err := plans.NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
			leftQ, rightQ,
			joinPredicates,
			joinType,
			leftCorr, rightCorr,
			joinResultValue,
		)
		if err != nil {
			call.Fail(err)
			return
		}
		call.Yield(plan)
	}

	// Correlated FlatMap: for PartitionBinarySelectRule / RewriteOuterJoinRule output
	// where predicates are absorbed into sub-Selects creating correlation. Each leg's
	// null-on-empty flag drives a DefaultOnEmpty null-extension of THAT leg inside
	// yieldGeneralFlatMap, whichever side it lands on — Java's planPartitionToPhysical
	// wraps the outer exactly as it wraps the inner. RewriteOuterJoinRule marks a LEFT
	// OUTER's inner; a partition that makes that quantifier the OUTER of a lower with
	// a leg correlated to it lands here with the flag on the outer
	// (TestImplementNestedLoopJoin_NullOnEmptyOuterIsExtended drives it). The SQL
	// shapes around it take other routes: `… LEFT JOIN e … WHERE NOT EXISTS (… e.id
	// …)` is declined by the existential lowering and plans with the DefaultOnEmpty
	// the simple-select rule puts over a one-quantifier lower, and `… LEFT JOIN e …,
	// (SELECT … e.x …) AS f` binds e inside the preserved side's FlatMap, whose inner
	// carries its own DefaultOnEmpty.
	if leftNeedsRightEvaluation && !rightNeedsLeftEvaluation && canSwap {
		r.yieldGeneralFlatMap(call, sel,
			rightPlan, leftPlan, rightCorr, leftCorr,
			rightExpr, leftExpr, rightRef, leftRef, joinType,
			selQuantifierIsNullOnEmpty(sel, rightCorr),
			selQuantifierIsNullOnEmpty(sel, leftCorr),
			leftStrictSingle)
	} else if rightNeedsLeftEvaluation && !leftNeedsRightEvaluation {
		r.yieldGeneralFlatMap(call, sel,
			leftPlan, rightPlan, leftCorr, rightCorr,
			leftExpr, rightExpr, leftRef, rightRef, joinType,
			selQuantifierIsNullOnEmpty(sel, leftCorr),
			selQuantifierIsNullOnEmpty(sel, rightCorr),
			rightStrictSingle)
	}
}

// scalarFlowingLeg reports whether a join leg's quantifier flows a value that
// is not a record — what the materialized NLJ cannot bind (see the FlatMap
// choice in onMatch). An unknown flowed type is not claimed scalar.
func scalarFlowingLeg(q expressions.Quantifier) bool {
	t, err := q.GetFlowedObjectType()
	if err != nil || t == nil {
		return false
	}
	_, isRecord := t.(*values.RecordType)
	return !isRecord
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
//
// A physical join binds its legs' rows under its EXECUTABLE aliases
// (GetOuterAlias/GetInnerAlias), which need not be its memo quantifiers' — a
// materialized NLJ over a dissolved LEFT box ranges fresh quantifiers (`q$N`)
// while its rows, and every predicate and correlation over them, are `D`/`E`.
// Those aliases are bound inside the leg as surely as the quantifiers are, so
// they are provided too; left out, a leg reading the box's `E` looked
// correlated to nothing the box provides, and the box itself looked correlated
// to its own `D`/`E` from outside.
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
		if ph, ok := e.(physicalPlanExpression); ok {
			switch join := ph.GetRecordQueryPlan().(type) {
			case *plans.RecordQueryNestedLoopJoinPlan:
				out[join.GetOuterAlias()] = struct{}{}
				out[join.GetInnerAlias()] = struct{}{}
			case *plans.RecordQueryFlatMapPlan:
				out[join.GetOuterAlias()] = struct{}{}
				out[join.GetInnerAlias()] = struct{}{}
			}
		}
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
		// `_current` names the row local to a physical operator edge. Two
		// independently planned legs therefore both report it without sharing a
		// producer: their exact carrier QOVs (and often their row types) are
		// distinct. Treating the reserved spelling as an external sibling makes
		// every pair of projected CTE legs look like an incomplete three-way
		// bipartition and declines the only valid materialized cross join.
		//
		// A real excluded sibling is a named/unique correlation that survives
		// outside both legs. Keep those in the set; only the phase-local current
		// root is categorically incapable of naming the same sibling.
		if a == values.CurrentCorrelation() {
			continue
		}
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
	outerNullOnEmpty, innerNullOnEmpty bool,
	innerStrictSingle bool,
) {
	flatMapPlan, _, _, ok, err := buildCorrelatedFlatMapPlan(
		call,
		flattenAndPredicates(sel.GetPredicates()), sel.GetResultValue(),
		outerPlan, innerPlan, outerCorr, innerCorr, outerExpr, innerExpr,
		joinType, outerNullOnEmpty, innerNullOnEmpty, innerStrictSingle, false,
	)
	if err != nil {
		call.Fail(err)
		return
	}
	if !ok {
		return
	}
	// The FlatMap plan already carries its outer/inner memo quantifiers (RFC-184
	// W2, no physicalFlatMapWrapper) — yield it directly.
	rebuild := func(
		orderedOuter, orderedInner expressions.RelationalExpression,
	) (expressions.RelationalExpression, error) {
		outerPhysical, outerOK := orderedOuter.(physicalPlanExpression)
		innerPhysical, innerOK := orderedInner.(physicalPlanExpression)
		if !outerOK || !innerOK {
			return nil, nil
		}
		rebuilt, _, _, rebuiltOK, err := buildCorrelatedFlatMapPlan(
			call,
			flattenAndPredicates(sel.GetPredicates()), sel.GetResultValue(),
			outerPhysical.GetRecordQueryPlan(), innerPhysical.GetRecordQueryPlan(),
			outerCorr, innerCorr, orderedOuter, orderedInner,
			joinType, outerNullOnEmpty, innerNullOnEmpty, innerStrictSingle, true,
		)
		if err != nil {
			return nil, err
		}
		if !rebuiltOK {
			return nil, nil
		}
		return rebuilt, nil
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
	rebuild func(outer, inner expressions.RelationalExpression) (expressions.RelationalExpression, error),
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
	outerAlias := quantifiers[0].GetAlias()
	innerAlias := quantifiers[1].GetAlias()
	if flatMap, ok := base.(*plans.RecordQueryFlatMapPlan); ok {
		outerAlias = flatMap.GetOuterAlias()
		innerAlias = flatMap.GetInnerAlias()
		outerOrderingResultValue = flatMapOrderingResultForChild(
			flatMap, outerAlias, true)
	}
	localAliases := map[values.CorrelationIdentifier]struct{}{
		outerAlias: {},
		innerAlias: {},
	}

	less := lessWithHashTieBreak(call.CostModel())
	for _, requested := range requestedOrderings {
		if requested == nil || requested.IsPreserve() {
			continue
		}

		outerRequested := pushRequestedOrderingToSelectChild(
			requested, outerOrderingResultValue,
			outerAlias, localAliases)
		innerRequested := pushRequestedOrderingToSelectChild(
			requested, resultValue, innerAlias, localAliases)

		// The raw sets supply the leg whose ordering is irrelevant in a case.
		// The ordered sets pin each unary delegation spine against the
		// child-space request before the join freezes the selected top member.
		rawOuters, err := collectJoinLegOrderingVariants(
			call,
			outerRef, properties.PreserveOrdering(), outerOrderingResultValue,
			outerAlias, localAliases, less, false, call.Context)
		if err != nil {
			call.Fail(err)
			return
		}
		rawInners, err := collectJoinLegOrderingVariants(
			call,
			innerRef, properties.PreserveOrdering(), resultValue,
			innerAlias, localAliases, less, false, call.Context)
		if err != nil {
			call.Fail(err)
			return
		}
		orderedOuters, err := collectJoinLegOrderingVariants(
			call,
			outerRef, outerRequested, outerOrderingResultValue,
			outerAlias, localAliases, less, true, call.Context)
		if err != nil {
			call.Fail(err)
			return
		}
		orderedInners, err := collectJoinLegOrderingVariants(
			call,
			innerRef, innerRequested, resultValue,
			innerAlias, localAliases, less, true, call.Context)
		if err != nil {
			call.Fail(err)
			return
		}
		for _, pair := range orderedJoinLegPairs(
			rawOuters, rawInners, orderedOuters, orderedInners,
			requested, less,
		) {
			r.yieldVerifiedOrderedJoin(
				call, base, pair.outer, pair.inner, requested, rebuild)
			if call.Err() != nil {
				return
			}
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
	memoizer Memoizer,
	ref *expressions.Reference,
	requestedInChildSpace *properties.RequestedOrdering,
	resultValue values.Value,
	resultAlias values.CorrelationIdentifier,
	localAliases map[values.CorrelationIdentifier]struct{},
	less func(a, b expressions.RelationalExpression) bool,
	pinOrdering bool,
	ctx PlanContext,
) ([]joinLegOrderingVariant, error) {
	if ref == nil {
		return nil, nil
	}
	members := make([]expressions.RelationalExpression, 0, len(ref.AllMembers()))
	members = append(members, ref.FinalMembers()...)
	members = append(members, ref.Members()...)
	if pinOrdering && requestedInChildSpace != nil &&
		!requestedInChildSpace.IsPreserve() && ctx != nil {
		orderedAlternatives, err := orderedFullScanAlternatives(
			ref, requestedInChildSpace, ctx)
		if err != nil {
			return nil, err
		}
		members = append(members, orderedAlternatives...)
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
				memoizer,
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
		pulled, err := pullChildOrderingThroughResult(
			provided, ph, resultValue, resultAlias, localAliases)
		if err != nil {
			return nil, err
		}
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
	return result, nil
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
) (expressions.RelationalExpression, error) {
	quantifiers := base.GetQuantifiers()
	if len(quantifiers) != 2 || outer == nil || inner == nil {
		return nil, nil
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
		return nil, nil
	}
}

func existentialProgramCorrelations(
	preds []predicates.QueryPredicate,
	inner plans.RecordQueryPlan,
	resultValues ...values.Value,
) map[values.CorrelationIdentifier]struct{} {
	referenced := make(map[values.CorrelationIdentifier]struct{})
	for _, predicate := range preds {
		for correlation := range predicates.GetCorrelatedToOfPredicate(predicate) {
			referenced[correlation] = struct{}{}
		}
	}
	if inner != nil {
		plans.Walk(inner, func(plan plans.RecordQueryPlan) bool {
			for correlation := range plan.GetCorrelatedToWithoutChildren() {
				referenced[correlation] = struct{}{}
			}
			return true
		})
	}
	for _, resultValue := range resultValues {
		for correlation := range values.GetCorrelatedToOfValue(resultValue) {
			referenced[correlation] = struct{}{}
		}
	}
	return referenced
}

// existentialProgramsRequireRetainedOuterLayout reports whether an executable
// existential program reads a source whose runtime object is supplied by an
// outer layout window. Predicate normalization above is exact-type checked;
// physical plan nodes expose only their free correlation set, so the plan arm
// deliberately treats a same-named foreign exact type conservatively and pins
// the current outer member. That can retain one extra alternative, but it
// cannot turn a foreign value into an admitted binding or weaken evaluation.
func existentialProgramsRequireRetainedOuterLayout(
	preds []predicates.QueryPredicate,
	inner plans.RecordQueryPlan,
	outerLayout values.OrdinalLayout,
	resultValues ...values.Value,
) bool {
	if outerLayout == nil {
		return false
	}
	referenced := existentialProgramCorrelations(preds, inner, resultValues...)
	for _, source := range outerLayout.WindowSources() {
		if source == nil || source.Correlation().IsZero() {
			continue
		}
		if _, used := referenced[source.Correlation()]; used {
			return true
		}
	}
	return false
}

// selectedExistentialOuterLayoutAuthority rebuilds one FlatMap level over the
// exact physical winners already stamped on its two child groups. A live memo
// member was constructed before those winners existed, so its immutable output
// layout can omit a retained source which FlatMap.WithQuantifiers will prove at
// extraction (for example a record-valued UNNEST source X carried through a
// later join with V). Existential collision selection and correlated predicate
// normalization happen before extraction; consuming the stale layout there
// freezes an unbindable logical X RECORD operand even though execution later
// publishes exact X ELEM.
//
// A stamped child winner is authoritative and must equal the preserve-order
// winner. Before winner stamping, a sole physical member is also admissible,
// but the caller pins the resulting outer clone into a private final edge so a
// later alternative cannot separate the operand proof from execution. Multiple
// unstamped alternatives decline. FlatMap currently has no input-layout
// requirements; should that change, the clone declines rather than guessing a
// compatible child.
//
// The clone is used only when it adds an exact retained source referenced by
// this existential. Otherwise the original member remains the sole authority.
func selectedExistentialOuterLayoutAuthority(
	call *ExpressionRuleCall,
	outer plans.RecordQueryPlan,
	preds []predicates.QueryPredicate,
	inner plans.RecordQueryPlan,
	resultValues ...values.Value,
) (plans.RecordQueryPlan, bool, error) {
	flatMap, ok := outer.(*plans.RecordQueryFlatMapPlan)
	if !ok || call == nil {
		return outer, false, nil
	}
	propertiesView, err := flatMap.OrdinalPhysicalProperties()
	if err != nil {
		return nil, false, err
	}
	if len(propertiesView.RequiredInputLayouts()) != 0 {
		return outer, false, nil
	}
	quantifiers := flatMap.GetQuantifiers()
	if len(quantifiers) != 2 {
		return outer, false, nil
	}
	winners := make([]expressions.RelationalExpression, len(quantifiers))
	for i, quantifier := range quantifiers {
		ref := quantifier.GetRangesOver()
		if ref == nil {
			return outer, false, nil
		}
		winner, _ := getWinnerForOrdering(
			ref, properties.PreserveOrdering(), call.CostModel())
		if winner == nil {
			return outer, false, nil
		}
		if stamped := ref.Winner(); stamped != nil {
			if winner != stamped {
				return outer, false, nil
			}
		} else if len(ref.AllMembers()) != 1 {
			// A sole physical member can be pinned into the exact outer clone below.
			// Multiple unstamped alternatives have no stable selection yet.
			return outer, false, nil
		}
		physical, physicalOK := winner.(physicalPlanExpression)
		if !physicalOK || physical.GetRecordQueryPlan() == nil {
			return outer, false, nil
		}
		winners[i] = winner
	}
	rebuilt, err := rebuildJoinWithExactLegs(
		call, flatMap, winners[0], winners[1])
	if err != nil {
		return nil, false, err
	}
	rebuiltPhysical, ok := rebuilt.(physicalPlanExpression)
	if !ok || rebuiltPhysical.GetRecordQueryPlan() == nil {
		return nil, false, fmt.Errorf(
			"existential selected outer FlatMap rebuild produced %T", rebuilt)
	}
	rebuiltPlan := rebuiltPhysical.GetRecordQueryPlan()
	originalLayout, err := outer.ProvidedOutputLayout()
	if err != nil {
		return nil, false, err
	}
	rebuiltLayout, err := rebuiltPlan.ProvidedOutputLayout()
	if err != nil {
		return nil, false, err
	}
	referenced := existentialProgramCorrelations(preds, inner, resultValues...)
	for _, source := range rebuiltLayout.WindowSources() {
		if source == nil || source.Correlation().IsZero() {
			continue
		}
		if _, used := referenced[source.Correlation()]; !used {
			continue
		}
		provided, provideErr := values.LayoutProvides(originalLayout, source)
		if provideErr == nil && provided {
			continue
		}
		return rebuiltPlan, true, nil
	}
	return outer, false, nil
}

func rebuildOrderedJoin(
	call *ExpressionRuleCall,
	base expressions.RelationalExpression,
	outer, inner expressions.RelationalExpression,
	rebuild func(outer, inner expressions.RelationalExpression) (expressions.RelationalExpression, error),
) (expressions.RelationalExpression, error) {
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
	rebuild func(outer, inner expressions.RelationalExpression) (expressions.RelationalExpression, error),
) {
	candidate, err := rebuildOrderedJoin(call, base, outer, inner, rebuild)
	if err != nil {
		call.Fail(err)
		return
	}
	if candidate != nil && orderedJoinSatisfies(candidate, requested) {
		call.Yield(candidate)
	}
}

// buildCorrelatedFlatMapPlan constructs the correlated-FlatMap join plan —
// the per-quantifier-property lowering shared by the 2-quantifier
// leftDepsRight/rightDepsLeft branches (yieldGeneralFlatMap) and the
// 3-quantifier existential arm's step-1 (the existential peel): the
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
// translatePredicateLogicalSource retargets one logical source declaration in
// a predicate list to the exact physical edge selected for that source. The
// declaration is recovered from the predicates themselves rather than
// fabricated from the physical plan: the whole defect this bridge closes is
// that logical and physical root record identities can differ while their
// resolved ordinal paths and leaf types agree.
//
// retainedWindows are the exact types the SELECTED plan retains as SOURCES
// INSIDE this row under the same correlation. One alias legitimately denotes
// two different objects there: the row itself, and a source the row retains
// which happens to be spelled the same. A chained unnest is the standing case —
// in `FROM t, t.arr AS x, x.sub AS y` the merged row is bound as Y while still
// retaining Y's own scalar element, so `t.id > y` carries both `QOV(Y, row).ID`
// and a bare `QOV(Y, INT)`. Exact type is part of QOV identity, so both bind at
// runtime; only the ROW is the logical source this bridge retargets, and a
// retained window must be left exactly as it is. Without that separation the
// two readings looked like one alias with two irreconcilable types and the
// whole join declined.
// retainedWindowTypesAt returns the exact types of the sources a selected
// plan's layout retains INSIDE its row under `alias`. They are the readings at
// that correlation which are NOT the row, and they are what keeps a
// same-spelled retained source from looking like a second, irreconcilable
// declaration of the row itself.
//
// A nil layout yields nothing, which restores the pre-layout behaviour: every
// QOV at the alias is then a row candidate.
func retainedWindowTypesAt(
	layout values.OrdinalLayout,
	alias values.CorrelationIdentifier,
) []values.Type {
	if layout == nil {
		return nil
	}
	var out []values.Type
	for _, source := range layout.WindowSources() {
		if source == nil || source.Correlation() != alias {
			continue
		}
		out = append(out, source.FlowedType())
	}
	return out
}

func translatePredicateLogicalSource(
	preds []predicates.QueryPredicate,
	alias values.CorrelationIdentifier,
	target values.QuantifiedObjectValue,
	retainedWindows []values.Type,
) ([]predicates.QueryPredicate, error) {
	isRetainedWindow := func(root values.QuantifiedObjectValue) bool {
		for _, window := range retainedWindows {
			if window != nil && values.FlowedTypeEquals(root, window) {
				return true
			}
		}
		return false
	}
	var declaration values.QuantifiedObjectValue
	var conflicting values.QuantifiedObjectValue
	for predicateIndex, predicate := range preds {
		_, err := predicates.TransformEmbeddedValuesChecked(
			predicate,
			func(value values.Value) (values.Value, error) {
				values.WalkValue(value, func(node values.Value) bool {
					if conflicting != nil {
						return false
					}
					root, ok := values.AsQuantifiedObjectValue(node)
					if !ok || root.Correlation() != alias || isRetainedWindow(root) {
						return true
					}
					if declaration == nil {
						declaration = root
						return true
					}
					if !values.FlowedTypesEqual(declaration, root) {
						// Stop this Value walk; the error is returned after it.
						conflicting = root
						return false
					}
					return true
				})
				return value, nil
			},
		)
		if err != nil {
			return nil, fmt.Errorf("predicate %d logical source scan: %w", predicateIndex, err)
		}
		if conflicting != nil {
			return nil, fmt.Errorf(
				"predicate logical source %s has conflicting exact types %s and %s",
				alias.Name(), declaration.FlowedType(), conflicting.FlowedType())
		}
	}
	if declaration == nil {
		return append([]predicates.QueryPredicate(nil), preds...), nil
	}
	translated := make([]predicates.QueryPredicate, len(preds))
	for i, predicate := range preds {
		var err error
		translated[i], err = predicates.TransformEmbeddedValuesChecked(
			predicate,
			func(value values.Value) (values.Value, error) {
				return values.TranslateLogicalSourceRoot(value, declaration, target)
			},
		)
		if err != nil {
			return nil, fmt.Errorf("predicate %d logical source %s: %w", i, alias.Name(), err)
		}
	}
	return translated, nil
}

// normalizeMaterializedJoinPrograms validates that a materialized NLJ's two
// selected legs can each state the carrier its programs will be bound against,
// and residualizes its predicates without changing their Value programs.
//
// IT USED TO REWRITE THEM, and the rewrite has been REMOVED rather than left
// unreachable. Its whole job was to cross the record-name divergence: a logical
// leg root kept the table's nominal row (I ITEMS) while the selected scan
// published the same row anonymously, those counted as two different exact
// types, and the exact pair binder rejected a program that still named the
// logical one. Record names are provenance in Java (Type.Record.equals compares
// typeCode, nullability and fields) and now in Go, so the two are ONE type and
// there is nothing left to cross.
//
// Re-rooting them anyway — onto the same exact row, purely to swap which QOV
// INSTANCE the program hangs from — is not a smaller version of the old job, it
// is a different and unsafe one: a leg's QOV also carries the buried-source leg
// table that identifies gathered/box sub-windows, exact identity deliberately
// excludes it, and the physical carrier's table is not the logical root's. So
// an instance swap silently retargets those sub-windows. Measured: it made a
// three-leg `ORDER BY` over a gathered unnest and a DISJOINT multi-source
// unnest GROUP BY fail to plan at all, with a nested buried source pointing at
// a slot that is not a record.
//
// The legs are still checked, because a leg that cannot state a carrier is a
// malformed plan and this is where that was caught.
func normalizeMaterializedJoinPrograms(
	preds []predicates.QueryPredicate,
	result values.Value,
	leftPlan plans.RecordQueryPlan,
	leftAlias values.CorrelationIdentifier,
	rightPlan plans.RecordQueryPlan,
	rightAlias values.CorrelationIdentifier,
) ([]predicates.QueryPredicate, values.Value, error) {
	for _, leg := range []struct {
		plan  plans.RecordQueryPlan
		alias values.CorrelationIdentifier
		label string
	}{
		{plan: leftPlan, alias: leftAlias, label: "left"},
		{plan: rightPlan, alias: rightAlias, label: "right"},
	} {
		if leg.plan == nil || leg.alias.IsZero() {
			return nil, nil, fmt.Errorf(
				"materialized join %s result leg is missing its selected plan or alias", leg.label)
		}
		layout, layoutErr := leg.plan.ProvidedOutputLayout()
		if layoutErr != nil {
			return nil, nil, fmt.Errorf(
				"materialized join %s result selected layout: %w", leg.label, layoutErr)
		}
		if _, targetErr := values.NewQuantifiedObjectValue(
			leg.alias, values.PhysicalCarrierType(layout)); targetErr != nil {
			return nil, nil, fmt.Errorf(
				"materialized join %s exact binding: %w", leg.label, targetErr)
		}
	}
	// The materialized join evaluates these predicates against each row pair,
	// just as a FlatMap's physical filters do. Select's partitioned ranges are
	// structural and must become executable comparisons at this boundary too.
	residuals, err := predicates.ToResidualPredicates(preds)
	if err != nil {
		return nil, nil, err
	}
	return residuals, result, nil
}

// normalizeCorrelatedExplodeCollectionPlan retargets the collection program of
// a correlated physical Explode onto the exact runtime row type that its
// enclosing FlatMap binds for sourceAlias. Logical UNNEST keeps the storage
// record's nominal name (T1), while the selected physical scan publishes the
// executor carrier for the same row (normally anonymous). Once runtime binding
// admission is exact, leaving the collection on the logical declaration makes
// T1.ARR unbindable even though every resolved ordinal and leaf type agrees.
//
// This walk is deliberately narrow. Explode owns the correlation-bearing
// program; PredicatesFilter is the one transparent wrapper produced for a
// filtered WITH ORDINALITY leg. Other plans are unchanged. The checked logical
// source bridge permits only top-level record-name normalization with the same
// alias, complete ordinal path, and exact leaf types.
func normalizeCorrelatedExplodeCollectionPlan(
	plan plans.RecordQueryPlan,
	sourceAlias values.CorrelationIdentifier,
	target values.QuantifiedObjectValue,
) (plans.RecordQueryPlan, bool, error) {
	if plan == nil || sourceAlias.IsZero() || target == nil {
		return plan, false, nil
	}
	switch typed := plan.(type) {
	case *plans.RecordQueryExplodePlan:
		collection := typed.GetCollectionValue()
		normalized, err := values.TranslateLogicalSourceNameNormalization(
			collection, sourceAlias, target)
		if err != nil {
			return nil, false, fmt.Errorf(
				"correlated Explode source %s: %w", sourceAlias.Name(), err)
		}
		normalized, err = values.TranslateProjectionInputNameNormalizationToCorrelation(
			normalized, sourceAlias, target.FlowedType())
		if err != nil {
			return nil, false, fmt.Errorf(
				"correlated Explode projection input %s: %w", sourceAlias.Name(), err)
		}
		if normalized == collection {
			return plan, false, nil
		}
		rebuilt, err := typed.WithCollection(normalized)
		if err != nil {
			return nil, false, err
		}
		return rebuilt, true, nil

	case *plans.RecordQueryPredicatesFilterPlan:
		inner, changed, err := normalizeCorrelatedExplodeCollectionPlan(
			typed.GetInner(), sourceAlias, target)
		if err != nil || !changed {
			return plan, changed, err
		}
		rebuilt, err := plans.NewRecordQueryPredicatesFilterPlanWithAlias(
			inner, typed.GetPredicates(), typed.GetInnerAlias())
		if err != nil {
			return nil, false, err
		}
		return rebuilt, true, nil

	default:
		return plan, false, nil
	}
}

// normalizeCorrelatedScanComparisonPlan aligns correlated access programs with
// the selected outer carrier without admitting field, width, or type drift.
func normalizeCorrelatedScanComparisonPlan(
	plan plans.RecordQueryPlan,
	sourceAlias values.CorrelationIdentifier,
	target values.QuantifiedObjectValue,
) (plans.RecordQueryPlan, bool, error) {
	if plan == nil || sourceAlias.IsZero() || target == nil {
		return plan, false, nil
	}
	comparisonTransform := func(value values.Value) (values.Value, error) {
		return values.TranslateLogicalSourceNameNormalization(value, sourceAlias, target)
	}
	programTransform := func(value values.Value) (values.Value, error) {
		normalized, err := comparisonTransform(value)
		if err != nil {
			return nil, err
		}
		return values.TranslateProjectionInputNameNormalizationToCorrelation(
			normalized, sourceAlias, target.FlowedType())
	}
	return translateCorrelatedAccessPrograms(plan, comparisonTransform, programTransform)
}

// translateCorrelatedAccessPrograms translates scan ranges, filters and projections
// through selected child edges; relinking preserves their aliases, kinds and stages.
func translateCorrelatedAccessPrograms(
	plan plans.RecordQueryPlan,
	comparisonTransform, programTransform func(values.Value) (values.Value, error),
) (plans.RecordQueryPlan, bool, error) {
	if plan == nil {
		return nil, false, nil
	}
	children, quantifiers := plan.GetChildren(), plan.GetQuantifiers()
	if len(children) != len(quantifiers) {
		return nil, false, fmt.Errorf("correlated access translation: %T has %d children and %d quantifiers",
			plan, len(children), len(quantifiers))
	}
	changed := false
	for i, child := range children {
		translated, childChanged, err := translateCorrelatedAccessPrograms(child, comparisonTransform, programTransform)
		if err != nil {
			return nil, false, err
		}
		if !childChanged {
			continue
		}
		stage := expressions.StageCanonical
		if ref := quantifiers[i].GetRangesOver(); ref != nil {
			stage = ref.Stage()
		}
		quantifiers[i] = expressions.RebuildQuantifier(quantifiers[i], expressions.FinalOfAtStage(translated, stage))
		changed = true
	}
	if changed {
		relinked, err := plan.WithQuantifiers(quantifiers)
		if err != nil {
			return nil, false, fmt.Errorf("correlated access translation relinking %T: %w", plan, err)
		}
		relinkedPlan, ok := relinked.(plans.RecordQueryPlan)
		if !ok {
			return nil, false, fmt.Errorf("correlated access translation relinking %T produced %T", plan, relinked)
		}
		plan = relinkedPlan
	}
	switch typed := plan.(type) {
	case *plans.RecordQueryScanPlan:
		comparisons, moved, err := translateCorrelatedComparisonRanges(typed.GetScanComparisons(), comparisonTransform)
		if err != nil {
			return nil, false, err
		}
		if moved {
			return typed.WithScanComparisons(comparisons), true, nil
		}
	case *plans.RecordQueryIndexPlan:
		comparisons, moved, err := translateCorrelatedComparisonRanges(typed.GetScanComparisons(), comparisonTransform)
		if err != nil {
			return nil, false, err
		}
		if moved {
			return typed.WithScanComparisons(comparisons), true, nil
		}
	case *plans.RecordQueryCoveringIndexPlan:
		// The index is a field, not a quantifier child (as in Java).
		index, ok := plans.IndexPlanOf(typed)
		if !ok {
			return nil, false, fmt.Errorf("correlated covering access has no index plan")
		}
		translated, moved, err := translateCorrelatedAccessPrograms(index, comparisonTransform, programTransform)
		if err != nil {
			return nil, false, err
		}
		if moved {
			return typed.WithIndexPlan(translated.(*plans.RecordQueryIndexPlan)), true, nil
		}
	case *plans.RecordQueryMapPlan:
		// A query block's result, as Java's RecordQueryMapPlan translates its
		// result value with the rest of the plan.
		translated, err := programTransform(typed.GetResultValue())
		if err != nil {
			return nil, false, fmt.Errorf("correlated access map result: %w", err)
		}
		if translated != typed.GetResultValue() {
			rebuilt, err := plans.NewRecordQueryMapPlanFromQuantifier(typed.GetInnerQuantifier(), translated)
			return rebuilt, true, err
		}
	case *plans.RecordQueryPredicatesFilterPlan:
		translated, moved, err := translateCorrelatedAccessPredicates(typed.GetPredicates(), programTransform)
		if err != nil {
			return nil, false, err
		}
		if moved {
			rebuilt, err := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
				typed.GetQuantifiers()[0], translated, typed.GetInnerAlias())
			return rebuilt, true, err
		}
	case *plans.RecordQueryFilterPlan:
		translated, moved, err := translateCorrelatedAccessPredicates(typed.GetPredicates(), programTransform)
		if err != nil {
			return nil, false, err
		}
		if moved {
			rebuilt, err := plans.NewRecordQueryFilterPlanFromQuantifier(translated, typed.GetQuantifiers()[0])
			return rebuilt, true, err
		}
	}
	return plan, changed, nil
}

func translateCorrelatedAccessPredicates(
	original []predicates.QueryPredicate,
	transform func(values.Value) (values.Value, error),
) ([]predicates.QueryPredicate, bool, error) {
	translated := make([]predicates.QueryPredicate, len(original))
	changed := false
	for i, predicate := range original {
		var err error
		translated[i], err = predicates.TransformEmbeddedValuesChecked(predicate, transform)
		if err != nil {
			return nil, false, fmt.Errorf("correlated access predicate %d: %w", i, err)
		}
		changed = changed || translated[i] != predicate
	}
	return translated, changed, nil
}

// normalizeCorrelatedScanComparisonPlanForOuterLayout normalizes every exact
// outer object which the selected FlatMap input can bind at runtime. The whole
// outer row is bound under bindingAlias, while a gathered/materialized join may
// additionally retain buried source windows (for example PA and B inside
// B$BOX). Correlated scan comparisons can read either namespace. Restricting
// normalization to bindingAlias leaves a nominal PA QOV beside the selected
// anonymous PA window and the exact runtime binder correctly rejects it.
//
// Each rewrite still uses normalizeCorrelatedScanComparisonPlan's checked
// top-level-name bridge. The selected layout is only the target authority; it
// does not authorize foreign aliases, narrower windows, path drift, or leaf
// type drift.
func normalizeCorrelatedScanComparisonPlanForOuterLayout(
	plan plans.RecordQueryPlan,
	bindingAlias values.CorrelationIdentifier,
	bindingTarget values.QuantifiedObjectValue,
	outerLayout values.OrdinalLayout,
) (plans.RecordQueryPlan, bool, error) {
	if plan == nil || bindingAlias.IsZero() || bindingTarget == nil || outerLayout == nil {
		return plan, false, nil
	}
	normalized, changed, err := normalizeCorrelatedScanComparisonPlan(
		plan, bindingAlias, bindingTarget)
	if err != nil {
		return nil, false, err
	}
	for _, source := range outerLayout.WindowSources() {
		if source == nil || source.Correlation().IsZero() {
			continue
		}
		var sourceChanged bool
		normalized, sourceChanged, err = normalizeCorrelatedScanComparisonPlan(
			normalized, source.Correlation(), source)
		if err != nil {
			return nil, false, fmt.Errorf(
				"correlated retained source %s comparison: %w",
				source.Correlation().Name(), err)
		}
		changed = changed || sourceChanged
	}
	return normalized, changed, nil
}

// normalizeCorrelatedPredicatesForOuterLayout is the predicate twin of
// normalizeCorrelatedScanComparisonPlanForOuterLayout. It retargets logical
// top-level record names onto the complete row binding and every exact retained
// source the selected outer layout will install. Collision handling runs first,
// so a fresh whole-row binding and an authored retained-source alias are
// disjoint before this walk; no correlation-only choice is made here.
func normalizeCorrelatedPredicatesForOuterLayout(
	preds []predicates.QueryPredicate,
	bindingAlias values.CorrelationIdentifier,
	bindingTarget values.QuantifiedObjectValue,
	outerLayout values.OrdinalLayout,
) ([]predicates.QueryPredicate, bool, error) {
	if len(preds) == 0 || bindingAlias.IsZero() || bindingTarget == nil || outerLayout == nil {
		return preds, false, nil
	}
	normalized := append([]predicates.QueryPredicate(nil), preds...)
	changed := false
	for i, predicate := range preds {
		if predicate == nil {
			continue
		}
		rebuilt, err := predicates.TransformEmbeddedValuesChecked(
			predicate,
			func(value values.Value) (values.Value, error) {
				return values.TranslateLogicalSourceNameNormalization(
					value, bindingAlias, bindingTarget)
			})
		if err != nil {
			return nil, false, fmt.Errorf(
				"correlated predicate %d whole-row normalization: %w", i, err)
		}
		for _, source := range outerLayout.WindowSources() {
			if source == nil || source.Correlation().IsZero() {
				continue
			}
			rebuilt, err = predicates.TransformEmbeddedValuesChecked(
				rebuilt,
				func(value values.Value) (values.Value, error) {
					return values.TranslateLogicalSourceNameNormalization(
						value, source.Correlation(), source)
				})
			if err != nil {
				return nil, false, fmt.Errorf(
					"correlated predicate %d retained source %s normalization: %w",
					i, source.Correlation().Name(), err)
			}
		}
		if rebuilt != predicate {
			normalized[i] = rebuilt
			changed = true
		}
	}
	return normalized, changed, nil
}

// normalizeCorrelatedValueForOuterLayout applies the same exact, name-only
// phase bridge to one executable Value program. Existential result programs
// are evaluated in the outer FlatMap context just like correlated predicates;
// leaving a logical RECORD root there while the selected window publishes ELEM
// would make a projected retained field unbindable even when no predicate reads
// that source.
func normalizeCorrelatedValueForOuterLayout(
	value values.Value,
	bindingAlias values.CorrelationIdentifier,
	bindingTarget values.QuantifiedObjectValue,
	outerLayout values.OrdinalLayout,
) (values.Value, error) {
	if value == nil || bindingAlias.IsZero() || bindingTarget == nil || outerLayout == nil {
		return value, nil
	}
	normalized, err := values.TranslateLogicalSourceNameNormalization(
		value, bindingAlias, bindingTarget)
	if err != nil {
		return nil, fmt.Errorf("existential result whole-row normalization: %w", err)
	}
	for _, source := range outerLayout.WindowSources() {
		if source == nil || source.Correlation().IsZero() {
			continue
		}
		normalized, err = values.TranslateLogicalSourceNameNormalization(
			normalized, source.Correlation(), source)
		if err != nil {
			return nil, fmt.Errorf(
				"existential result retained source %s normalization: %w",
				source.Correlation().Name(), err)
		}
	}
	return normalized, nil
}

// admitCorrelatedFastPathOuterValue proves the outer operand against exactly
// one runtime object installed by the selected outer FlatMap: either its whole
// row binding or one retained source window. A logical source may differ from
// that object only in its top-level record name. Correlation, record
// nullability, field ordinals/names, nested types, complete path, and result
// type remain exact through TranslateLogicalSourceNameNormalization.
//
// A whole row and retained source can intentionally share a correlation. If
// both are shape-compatible with the request, the owner is ambiguous and the
// shortcut declines. The general correlated path remains available; the
// binder is never asked to guess.
func admitCorrelatedFastPathOuterValue(
	outerValue values.FieldValue,
	wholeBinding values.QuantifiedObjectValue,
	outerLayout values.OrdinalLayout,
) (values.FieldValue, values.CorrelationIdentifier, bool, bool, error) {
	if outerValue == nil || wholeBinding == nil || outerLayout == nil {
		return nil, values.CorrelationIdentifier{}, false, false, nil
	}
	root, ok := values.AsQuantifiedObjectValue(outerValue.ChildValue())
	if !ok || root.Correlation().IsZero() {
		return nil, values.CorrelationIdentifier{}, false, false, nil
	}
	if path := outerValue.Path(); path != nil && path.IsFrontierPinned() {
		authorizedCorrelation := root.Correlation() == wholeBinding.Correlation()
		if !authorizedCorrelation {
			for _, source := range outerLayout.WindowSources() {
				if source != nil && source.Correlation() == root.Correlation() {
					authorizedCorrelation = true
					break
				}
			}
		}
		if !authorizedCorrelation {
			return nil, values.CorrelationIdentifier{}, false, false, nil
		}
		// Preserve the old ordinal-seed contract. matchJoinPKPredicate has
		// already proved this root belongs to one admitted outer correlation;
		// its frontier-pinned path is the complete ownership proof and must not
		// be re-derived through a retained object window.
		return outerValue, root.Correlation(), false, true, nil
	}
	if root.Correlation() == values.CurrentCorrelation() {
		return nil, values.CorrelationIdentifier{}, false, false, nil
	}
	type candidate struct {
		root   values.QuantifiedObjectValue
		window bool
	}
	candidates := []candidate{{root: wholeBinding}}
	for _, source := range outerLayout.WindowSources() {
		if source == nil || source.Correlation().IsZero() {
			continue
		}
		candidates = append(candidates, candidate{root: source, window: true})
	}

	var admitted values.FieldValue
	var admittedCorrelation values.CorrelationIdentifier
	admittedWindow := false
	claims := 0
	for _, candidate := range candidates {
		if candidate.root.Correlation() != root.Correlation() {
			continue
		}
		normalized, err := values.TranslateLogicalSourceNameNormalization(
			outerValue, root.Correlation(), candidate.root)
		if err != nil {
			return nil, values.CorrelationIdentifier{}, false, false, err
		}
		normalizedField, isField := values.AsFieldValue(normalized)
		if !isField {
			return nil, values.CorrelationIdentifier{}, false, false, fmt.Errorf(
				"correlated fast-path normalization produced %T", normalized)
		}
		normalizedRoot, hasRoot := values.AsQuantifiedObjectValue(
			normalizedField.ChildValue())
		claimed := normalized != outerValue
		if !claimed && values.FlowedTypesEqual(root, candidate.root) {
			claimed = true
		}
		if !claimed || !hasRoot ||
			normalizedRoot.Correlation() != candidate.root.Correlation() ||
			!values.FlowedTypesEqual(normalizedRoot, candidate.root) ||
			!normalizedField.ResultType().Equals(outerValue.ResultType()) {
			continue
		}
		claims++
		admitted = normalizedField
		admittedCorrelation = candidate.root.Correlation()
		admittedWindow = candidate.window
	}
	if claims != 1 {
		return nil, values.CorrelationIdentifier{}, false, false, nil
	}
	return admitted, admittedCorrelation, admittedWindow, true, nil
}

func translateCorrelatedComparisonRanges(
	ranges []*predicates.ComparisonRange,
	transform func(values.Value) (values.Value, error),
) ([]*predicates.ComparisonRange, bool, error) {
	normalized := make([]*predicates.ComparisonRange, len(ranges))
	changed := false
	for i, comparisonRange := range ranges {
		if comparisonRange == nil || comparisonRange.IsEmpty() {
			normalized[i] = comparisonRange
			continue
		}
		var comparisons []*predicates.Comparison
		if comparisonRange.IsEquality() {
			comparisons = []*predicates.Comparison{comparisonRange.GetEqualityComparison()}
		} else {
			comparisons = comparisonRange.GetInequalityComparisons()
		}
		rebuilt := predicates.EmptyComparisonRange()
		rangeChanged := false
		for _, comparison := range comparisons {
			normalizedComparison := comparison
			if comparison != nil && comparison.Operand != nil {
				normalizedOperand, err := transform(comparison.Operand)
				if err != nil {
					return nil, false, fmt.Errorf("comparison %d operand: %w", i, err)
				}
				if normalizedOperand != comparison.Operand {
					copyOfComparison := *comparison
					copyOfComparison.Operand = normalizedOperand
					normalizedComparison = &copyOfComparison
					rangeChanged = true
				}
			}
			merged := rebuilt.Merge(normalizedComparison)
			if !merged.Complete() {
				return nil, false, fmt.Errorf(
					"comparison %d could not be rebuilt after translation", i)
			}
			rebuilt = merged.Range
		}
		if rangeChanged {
			normalized[i] = rebuilt
			changed = true
		} else {
			normalized[i] = comparisonRange
		}
	}
	if !changed {
		return ranges, false, nil
	}
	return normalized, true, nil
}

func buildCorrelatedFlatMapPlan(
	call *ExpressionRuleCall,
	preds []predicates.QueryPredicate,
	resultValue values.Value,
	outerPlan, innerPlan plans.RecordQueryPlan,
	outerCorr, innerCorr values.CorrelationIdentifier,
	outerExpr, innerExpr expressions.RelationalExpression,
	joinType plans.JoinType,
	outerNullOnEmpty, innerNullOnEmpty bool,
	innerStrictSingle bool,
	freezeLegs bool,
) (*plans.RecordQueryFlatMapPlan, expressions.Quantifier, expressions.Quantifier, bool, error) {
	// Java's ImplementNestedLoopJoinRule residualizes the select predicates
	// before constructing either leg's filter.
	preds, err := predicates.ToResidualPredicates(preds)
	if err != nil {
		return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
	}
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

	memoizeLeg := call.MemoizeExpression
	if freezeLegs {
		memoizeLeg = call.MemoizeFinalExpression
	}

	// A null-on-empty OUTER null-extends exactly as a null-on-empty inner does
	// below (Java's planPartitionToPhysical treats the two legs alike):
	// DefaultOnEmpty first, the outer's select-level predicates filtering ABOVE
	// it. It is wrapped HERE, before anything reads the outer's layout, because
	// the wrapped plan is the outer this FlatMap binds: the inner's correlated
	// references are normalized against its carrier below, so the inner reads
	// the row the executor hands it — the NULL row DefaultOnEmpty supplies for an
	// empty input included.
	outerLayoutNullSupplying := false
	if outerNullOnEmpty {
		baseQ := expressions.NamedPhysicalQuantifier(outerCorr, memoizeLeg(outerExpr))
		flowedType, err := baseQ.GetFlowedObjectType()
		if err != nil {
			return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
		}
		doePlan, err := plans.NewRecordQueryDefaultOnEmptyPlanFromQuantifier(baseQ, values.NewNullValue(flowedType))
		if err != nil {
			return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
		}
		outerPlan, outerExpr = doePlan, doePlan
		outerLayoutNullSupplying = true
	}

	innerExprForMemo := innerExpr

	// The inner Explode evaluates its collection once per outer row. Normalize
	// that correlated program to the exact physical row the FlatMap will bind,
	// then memoize the rebuilt plan itself so the memo edge and executable child
	// cannot diverge.
	outerLayout, err := outerPlan.ProvidedOutputLayout()
	if err != nil {
		return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
	}
	// The PHYSICAL type, not the public one. FlowedType deliberately withholds
	// leg boundaries so layout cannot reach the semantic surface; this re-mint
	// is the physical side of the same value and needs them, because
	// NewQuantifiedObjectValue snapshots its source layout from the type it is
	// handed. Handed the public type, the merged row forgets where each leg
	// starts — which downstream does not read as "no legs" but as ONE run over
	// the whole concat keyed by the box's rightmost leaf, so a qualified read
	// lands in the first leg. Measured on `FOA FULL OUTER FOB`: `FOB.K` read
	// FOA's K.
	physicalOuterType := values.PhysicalCarrierType(outerLayout)
	if layoutBearing := values.PhysicalFlowedRecordTypeOf(outerLayout.Carrier()); layoutBearing != nil {
		physicalOuterType = layoutBearing
	}
	physicalOuter, err := values.NewQuantifiedObjectValue(outerCorr, physicalOuterType)
	if err != nil {
		return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
	}
	normalizedInner, innerChanged, err := normalizeCorrelatedExplodeCollectionPlan(
		innerPlan, outerCorr, physicalOuter)
	if err != nil {
		return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
	}
	if innerChanged {
		innerPlan = normalizedInner
		innerExprForMemo = &scanPlanExpression{plan: innerPlan}
	}
	normalizedInner, innerChanged, err = normalizeCorrelatedScanComparisonPlanForOuterLayout(
		innerPlan, outerCorr, physicalOuter, outerLayout)
	if err != nil {
		return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
	}
	if innerChanged {
		innerPlan = normalizedInner
		innerExprForMemo = &scanPlanExpression{plan: innerPlan}
	}
	innerLayout, err := innerPlan.ProvidedOutputLayout()
	if err != nil {
		return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
	}
	physicalInnerType := values.PhysicalCarrierType(innerLayout)
	physicalInner, err := values.NewQuantifiedObjectValue(innerCorr, physicalInnerType)
	if err != nil {
		return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
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
	outerQ := expressions.NamedPhysicalQuantifier(outerCorr, memoizeLeg(outerExpr))
	innerQ := expressions.NamedPhysicalQuantifier(innerCorr, memoizeLeg(innerExprForMemo))
	// Predicates arrive from the logical join seed, whose source QOV retains a
	// nominal leg type (B RECORD<...>). The selected physical plans emit the
	// executor carriers for those rows (normally unnamed RECORD<...>). The memo
	// quantifiers above still range over the logical expressions, so asking them
	// for a flowed type would merely reproduce the nominal declaration. Build
	// exact runtime declarations from the selected plans instead, and translate
	// BOTH legs: a residual such as E.SALARY > M.SALARY evaluates inside M's
	// filter and reads the local physical edge as well as the bound outer row.
	// Retyping only M leaves E nominal and fails one lookup later in the same
	// predicate. A later extraction rebase may change the local edge alias, but
	// it then preserves these already-physical exact types.
	//
	// The two legs' own retained-source windows come along, because a leg alias
	// can name BOTH the leg's row and a source that row retains (a chained
	// unnest binds the merged row under the same correlation as the element it
	// keeps). Only the ROW is retargeted; see translatePredicateLogicalSource.
	outerWindows := retainedWindowTypesAt(outerLayout, outerCorr)
	innerWindows := retainedWindowTypesAt(innerLayout, innerCorr)
	// A null-on-empty inner's select-level predicates filter ABOVE its
	// DefaultOnEmpty (below), so they read the null-extended row, whose type is
	// nullable.
	innerPredsTarget := physicalInner
	if innerNullOnEmpty && joinType != plans.JoinLeftOuter && !innerStrictSingle {
		innerPredsTarget, err = values.NewQuantifiedObjectValue(innerCorr, values.WithNullability(physicalInnerType, true))
		if err != nil {
			return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
		}
	}
	joinPreds, err = translatePredicateLogicalSource(joinPreds, innerCorr, innerPredsTarget, innerWindows)
	if err != nil {
		return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
	}
	joinPreds, err = translatePredicateLogicalSource(joinPreds, outerCorr, physicalOuter, outerWindows)
	if err != nil {
		return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
	}
	outerPreds, err = translatePredicateLogicalSource(outerPreds, outerCorr, physicalOuter, outerWindows)
	if err != nil {
		return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
	}

	// LEFT-OUTER null-extension, the Java way (ImplementNestedLoopJoinRule.java:310-330
	// / ImplementSimpleSelectRule:100-109): wrap the inner in DefaultOnEmpty so a
	// non-matching outer row yields one all-NULL inner row instead of being dropped.
	// The FlatMap stays a PURE map (no leftOuter flag) — the outer-join semantics are
	// emergent from this wrapper, exactly like Java's FlatMap, and DefaultOnEmpty's
	// OrElse continuation carries the empty-vs-nonempty decision so the null-extension
	// is resume-safe across pages (the prior in-memory leftOuter flag re-decided this
	// from scratch on every resume → spurious null rows / paging that never advanced).
	// Two sources land here: a quantifier RewriteOuterJoinRule marked null-on-empty
	// (innerNullOnEmpty), and a directly LEFT-OUTER join type — both are LEFT OUTER,
	// and they differ in where their ON-predicates are.
	//
	// The rewritten member's ON-predicates already sit BELOW this boundary (inside
	// the correlated inner SUBSEL / pushed onto the inner probe), so they filter
	// before the null-fill, and its select-level predicates are WHERE-class.
	// Predicate placement then follows Java's planPartitionToPhysical: the wrap
	// (DefaultOnEmpty) comes FIRST, the select-level predicates filter ABOVE it,
	// seeing the null-extended row and dropping it on a non-matching comparison
	// (`… LEFT JOIN e ON … WHERE e.fname = 'x'` drops the null-extended rows;
	// placed below the wrap, the filter ran before the null-fill and the extended
	// row survived unfiltered — rows Java drops).
	//
	// A LEFT-OUTER-typed select (RewriteOuterJoinRule's input, which Java does not
	// have: its null-on-empty leg's ON conjuncts always live inside that leg) keeps
	// its ON conjuncts in its own predicate list — every predicate it has is one.
	// They all filter the inner BELOW the wrap, whichever legs they read: one
	// reading only the preserved leg empties the inner for that row, which the
	// wrap then null-extends, as the outer join does. Above the wrap they dropped
	// the preserved rows the outer join keeps (INNER rows).
	//
	// The strict-single (scalar subquery) wrap keeps its predicates BELOW too:
	// they are the subquery's own correlation, part of the subquery body the
	// at-most-one-row check applies to.
	if joinType == plans.JoinLeftOuter && !innerStrictSingle {
		joinPreds = append(joinPreds, outerPreds...)
		outerPreds = nil
	}
	onPredsBelowWrap := joinType == plans.JoinLeftOuter
	nullOnEmpty := innerNullOnEmpty || joinType == plans.JoinLeftOuter
	innerLayoutNullSupplying := false
	var innerWrapped plans.RecordQueryPlan = innerPlan
	if innerStrictSingle {
		if len(joinPreds) > 0 {
			fpInnerQ := expressions.NamedPhysicalQuantifier(innerQ.GetAlias(),
				call.MemoizeFinalExpression(innerWrapped))
			filterPlan, err := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
				fpInnerQ, joinPreds, innerCorr,
			)
			if err != nil {
				return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
			}
			innerWrapped = filterPlan
			innerQ = expressions.NamedPhysicalQuantifier(innerCorr,
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
		fodInnerQ := expressions.NamedPhysicalQuantifier(innerQ.GetAlias(),
			call.MemoizeFinalExpression(innerWrapped))
		flowedType, err := fodInnerQ.GetFlowedObjectType()
		if err != nil {
			return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
		}
		fodPlan, err := plans.NewRecordQueryFirstOrDefaultPlanStrictFromQuantifier(
			fodInnerQ, values.NewNullValue(flowedType),
		)
		if err != nil {
			return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
		}
		innerWrapped = fodPlan
		innerQ = expressions.NamedPhysicalQuantifier(innerCorr,
			call.MemoizeFinalExpression(fodPlan))
	} else if nullOnEmpty {
		if onPredsBelowWrap && len(joinPreds) > 0 {
			// The LEFT-OUTER-typed select's ON conjuncts, below the wrap.
			fpInnerQ := expressions.NamedPhysicalQuantifier(innerQ.GetAlias(),
				call.MemoizeFinalExpression(innerWrapped))
			filterPlan, err := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
				fpInnerQ, joinPreds, innerCorr,
			)
			if err != nil {
				return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
			}
			innerWrapped = filterPlan
			innerQ = expressions.NamedPhysicalQuantifier(innerCorr,
				call.MemoizeFinalExpression(filterPlan))
			joinPreds = nil
		}
		// The DefaultOnEmpty is its own cascades expression carrying the live innerQ
		// edge (RFC-184 W2) — no physicalDefaultOnEmptyWrapper.
		flowedType, err := innerQ.GetFlowedObjectType()
		if err != nil {
			return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
		}
		doePlan, err := plans.NewRecordQueryDefaultOnEmptyPlanFromQuantifier(
			innerQ, values.NewNullValue(flowedType),
		)
		if err != nil {
			return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
		}
		innerWrapped = doePlan
		innerQ = expressions.NamedPhysicalQuantifier(innerCorr,
			call.MemoizeFinalExpression(doePlan))
		// This is the exact physical fact ordinal_join.configureNullSupplying
		// previously rediscovered by walking the inner wrapper spine. Thread it
		// into the plan while lowering owns it. Strict FirstOrDefault above is
		// deliberately not marked: execution does not classify that wrapper as
		// a DefaultOnEmpty null-supplying edge.
		innerLayoutNullSupplying = true
		if len(joinPreds) > 0 {
			fpInnerQ := expressions.NamedPhysicalQuantifier(innerQ.GetAlias(),
				call.MemoizeFinalExpression(innerWrapped))
			filterPlan, err := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
				fpInnerQ, joinPreds, innerCorr,
			)
			if err != nil {
				return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
			}
			innerWrapped = filterPlan
			innerQ = expressions.NamedPhysicalQuantifier(innerCorr,
				call.MemoizeFinalExpression(filterPlan))
		}
	} else if len(joinPreds) > 0 {
		fpInnerQ := expressions.NamedPhysicalQuantifier(innerQ.GetAlias(),
			call.MemoizeFinalExpression(innerWrapped))
		filterPlan, err := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
			fpInnerQ, joinPreds, innerCorr,
		)
		if err != nil {
			return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
		}
		innerWrapped = filterPlan
		innerQ = expressions.NamedPhysicalQuantifier(innerCorr,
			call.MemoizeFinalExpression(filterPlan))
	}

	if len(outerPreds) > 0 {
		ofInnerQ := expressions.NamedPhysicalQuantifier(outerQ.GetAlias(),
			call.MemoizeFinalExpression(outerPlan))
		outerFilter, err := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
			ofInnerQ, outerPreds, outerCorr,
		)
		if err != nil {
			return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
		}
		outerQ = expressions.NamedPhysicalQuantifier(outerCorr,
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
	flatMapPlan, err := plans.NewRecordQueryFlatMapPlanFromQuantifiersWithNullSupplying(
		outerQ, innerQ,
		outerCorr, innerCorr,
		resultValue, false,
		outerLayoutNullSupplying, innerLayoutNullSupplying,
	)
	if err != nil {
		return nil, expressions.Quantifier{}, expressions.Quantifier{}, false, err
	}
	return flatMapPlan, outerQ, innerQ, true, nil
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
) (expressions.Quantifier, error) {
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
		filter, err := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
			filterInnerQ, belowFODPredicates, innerCorrelation)
		if err != nil {
			return expressions.Quantifier{}, err
		}
		belowFOD = filter
		innerQ = advance(filter)
	}

	// Freeze the concrete correlated inner in a private final reference. The
	// child quantifier keeps the current bookkeeping alias, while advance()
	// applies the caller's fresh-vs-preserved policy above the wrapper.
	fodInnerQ := expressions.NamedPhysicalQuantifier(
		innerQ.GetAlias(), call.MemoizeFinalExpression(belowFOD))
	flowedType, err := fodInnerQ.GetFlowedObjectType()
	if err != nil {
		return expressions.Quantifier{}, err
	}
	fod, err := plans.NewRecordQueryFirstOrDefaultPlanFromQuantifier(
		fodInnerQ, values.NewNullValue(flowedType))
	if err != nil {
		return expressions.Quantifier{}, err
	}
	innerQ = advance(fod)

	if hasExistsFilter {
		comparisonType := predicates.ComparisonIsNotNull
		if negated {
			comparisonType = predicates.ComparisonIsNull
		}
		filterInnerQ := expressions.NamedPhysicalQuantifier(
			innerQ.GetAlias(), call.MemoizeFinalExpression(fod))
		// The existential residual tests the complete FirstOrDefault row, not a
		// retained source window inside that row. Reusing innerCorrelation here
		// is ambiguous when a multi-table inner already publishes a buried source
		// under the same alias (for example W/AWARDS): the layout binder correctly
		// selects that narrow window and rejects the whole-row type. Root the
		// residual in the declared FOD edge instead. PredicatesFilter admission
		// then reanchors it to the exact FOD current carrier/presence channel, so
		// typed whole-row NULL remains distinguishable from a matched all-NULL
		// record without weakening source-window identity.
		innerObject, err := filterInnerQ.RequireFlowedObjectValue()
		if err != nil {
			return expressions.Quantifier{}, err
		}
		residual := predicates.NewComparisonPredicate(
			innerObject,
			predicates.Comparison{Type: comparisonType},
		)
		filter, err := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
			filterInnerQ, []predicates.QueryPredicate{residual}, innerCorrelation)
		if err != nil {
			return expressions.Quantifier{}, err
		}
		innerQ = advance(filter)
	}

	return innerQ, nil
}

// implementExistentialSelect handles a binary SelectExpression with at least
// one existential edge; each such edge gets its own FirstOrDefault witness.
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
	aliases []string,
) {
	outerRef := quants[0].GetRangesOver()
	innerRef := quants[1].GetRangesOver()
	if outerRef == nil || innerRef == nil {
		return
	}
	// Java rejects this orientation before selecting plans: the outer executes
	// before the existential inner has installed its binding.
	if referenceIsCorrelatedTo(outerRef, quants[1].GetAlias()) {
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
	if quants[0].Kind() == expressions.QuantifierExistential {
		outerQ := expressions.NamedPhysicalQuantifier(quants[0].GetAlias(), call.MemoizeFinalExpression(outerPlan))
		flowedType, err := outerQ.GetFlowedObjectType()
		if err != nil {
			call.Fail(err)
			return
		}
		wrapped, err := plans.NewRecordQueryFirstOrDefaultPlanFromQuantifier(outerQ, values.NewNullValue(flowedType))
		if err != nil {
			call.Fail(err)
			return
		}
		outerPlan, outerExpr = wrapped, wrapped
	} else if quants[0].IsNullOnEmpty() {
		// Java's planPartitionToPhysical: a null-on-empty leg flows one NULL row
		// when its input is empty.
		outerQ := expressions.NamedPhysicalQuantifier(quants[0].GetAlias(), call.MemoizeFinalExpression(outerPlan))
		flowedType, err := outerQ.GetFlowedObjectType()
		if err != nil {
			call.Fail(err)
			return
		}
		wrapped, err := plans.NewRecordQueryDefaultOnEmptyPlanFromQuantifier(outerQ, values.NewNullValue(flowedType))
		if err != nil {
			call.Fail(err)
			return
		}
		outerPlan, outerExpr = wrapped, wrapped
	}

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
	innerIsExistential := quants[1].Kind() == expressions.QuantifierExistential
	allPreds := sel.GetPredicates()
	var regularPreds []predicates.QueryPredicate
	hasExistsFilter := false
	negated := false
	for _, p := range flattenAndPredicates(allPreds) {
		if alias, ok := predicates.IsExistentialPredicate(p); ok && innerIsExistential && alias == quants[1].GetAlias() {
			hasExistsFilter = true
			continue
		}
		if alias, ok := predicates.IsNotExistentialPredicate(p); ok && innerIsExistential && alias == quants[1].GetAlias() {
			hasExistsFilter = true
			negated = true
			continue
		}
		regularPreds = append(regularPreds, p)
	}
	// Preserve boolean existential consumers until placement is decided:
	// their FALSE branch exists only after FirstOrDefault supplies NULL.
	hasBooleanExists := false
	for i, p := range regularPreds {
		if predicates.ContainsExistentialPredicate(p) {
			hasBooleanExists = true
			continue
		}
		residual, err := predicates.ToResidualPredicate(p)
		if err != nil {
			call.Fail(err)
			return
		}
		regularPreds[i] = residual
	}
	// A single PVR range can hold an equality AND additional bounds. Expose
	// those conjuncts to the PK/index shortcut so it consumes only the equality
	// it proves, leaving the other bounds as residuals below FirstOrDefault.
	// Multiple ranges remain an OR; neither arm is an unconditional probe key.
	regularPreds = predicates.FlattenConjunction(regularPreds)

	// Extract source aliases (parallel to quants) for datum qualification.
	var outerAlias, innerAlias string
	if len(aliases) >= 1 {
		outerAlias = aliases[0]
	}
	if len(aliases) >= 2 {
		innerAlias = aliases[1]
	}

	// Source aliases are optional display metadata. The quantifier aliases are
	// the binding authority when no source name was recorded; constructing a
	// named correlation from "" would make an otherwise valid projected EXISTS
	// fail before the FlatMap is built.
	outerCorr := correlationForSourceAlias(quants[0].GetAlias(), outerAlias)
	if referenceIsCorrelatedTo(quants[1].GetRangesOver(), quants[0].GetAlias()) {
		// The inner reads the outer by its quantifier; nothing rebases it.
		outerCorr = quants[0].GetAlias()
	}
	innerCorr := correlationForSourceAlias(quants[1].GetAlias(), innerAlias)
	// Resolve the Select result onto the runtime source aliases before choosing
	// the outer layout authority. A projected retained source can be the only
	// consumer of a late-discovered window; omitting it here would let the helper
	// treat the stale outer member as sufficient and lose that object later.
	resultValue, err := remapExistentialResultValue(sel.GetResultValue(),
		quants[0].GetAlias(), outerCorr, quants[1].GetAlias(), innerCorr)
	if err != nil {
		call.Fail(err)
		return
	}

	// The chosen outer FlatMap can predate its child winners. Rebuild one level
	// privately when those exact winners add a retained source this existential
	// reads; collision selection and predicate normalization must see the same
	// layout extraction will construct later. Only when the new retained source
	// is actually referenced do we execute the exact clone through a private
	// edge; unrelated outer alternatives stay live.
	outerLayoutPlan, exactOuterAuthority, err := selectedExistentialOuterLayoutAuthority(
		call, outerPlan, regularPreds, innerPlan, resultValue)
	if err != nil {
		call.Fail(err)
		return
	}
	if exactOuterAuthority {
		// The correlated program below is now normalized to this clone's exact
		// retained source. Execute that same clone through a private final edge;
		// allowing extraction to swap in another live-group member would separate
		// the operand proof from the object installed at runtime.
		outerExpr = outerLayoutPlan
		outerPlan = outerLayoutPlan
	}

	// The inner-leg alias set — the below-FOD-vs-outer routing authority (see the
	// detailed note at the below-FOD rebase). Computed here so the hoist can rebase
	// exactly the inner-residual preds.
	innerLegs := collectInnerLegAliases(innerRef, innerCorr)

	// The rendered outer source alias can also name a retained UNNEST source
	// inside the selected outer row. In that exact shape the two meanings must
	// remain distinct: the FlatMap binds its complete outer row, while the inner
	// predicate reads the scalar/AS+AT source window. Reusing VAL for both makes
	// the whole-row QOV shadow the narrower exact VAL binding at runtime and also
	// makes the ordinal rebase below mistake VAL.<field> for an already-rebased
	// whole-row path. Mint an internal whole-row binding only when the selected
	// output layout proves this same-correlation/different-exact-type collision.
	originalOuterCorr := outerCorr
	outerCorr, err = collisionFreeExistentialOuterCorrelation(
		call, outerLayoutPlan, outerCorr)
	if err != nil {
		call.Fail(err)
		return
	}
	var originalOuterRoot, reboundOuterRoot values.QuantifiedObjectValue
	if outerCorr != originalOuterCorr {
		originalOuterRoot, reboundOuterRoot, err = existentialOuterWholeRowRoots(
			outerLayoutPlan, originalOuterCorr, outerCorr)
		if err != nil {
			call.Fail(err)
			return
		}
		regularPreds, err = translateExistentialWholeRowPredicates(
			regularPreds, originalOuterRoot, reboundOuterRoot)
		if err != nil {
			call.Fail(err)
			return
		}
		innerPlan, err = translateExistentialWholeRowPlanPredicates(
			innerPlan, originalOuterRoot, reboundOuterRoot)
		if err != nil {
			call.Fail(err)
			return
		}
		// Keep the memo edge and the concrete plan in lockstep. The rewritten
		// filter is the executable existential child; retaining innerExpr here
		// would memoize the stale pre-collision predicate graph again.
		innerExpr = innerPlan
	}

	// Normalize every correlated executable program against the exact selected
	// outer layout before either the fast scan shortcut or the general
	// compensation chain freezes it. A retained record element can carry a
	// physical nominal name (ELEM) while SQL resolution produced RECORD with the
	// same fields; leaving that phase-local name in the scan/predicate creates a
	// third exact QOV which no runtime context can bind.
	outerLayout, err := outerLayoutPlan.ProvidedOutputLayout()
	if err != nil {
		call.Fail(err)
		return
	}
	physicalOuter, err := values.NewQuantifiedObjectValue(
		outerCorr, values.PhysicalCarrierType(outerLayout))
	if err != nil {
		call.Fail(err)
		return
	}
	regularPreds, _, err = normalizeCorrelatedPredicatesForOuterLayout(
		regularPreds, outerCorr, physicalOuter, outerLayout)
	if err != nil {
		call.Fail(err)
		return
	}
	normalizedInner, innerChanged, err := normalizeCorrelatedScanComparisonPlanForOuterLayout(
		innerPlan, outerCorr, physicalOuter, outerLayout)
	if err != nil {
		call.Fail(err)
		return
	}
	if innerChanged {
		innerPlan = normalizedInner
		innerExpr = innerPlan
	}

	// The result was already moved from the Select quantifier aliases onto the
	// runtime source aliases before selected-layout reconstruction. Collision
	// handling now retargets only the complete outer-row declaration; a retained
	// same-spelled source stays on its authored correlation and is normalized to
	// the exact window below.
	if reboundOuterRoot != nil {
		resultValue, err = translateExistentialWholeRowValue(
			resultValue, originalOuterRoot, reboundOuterRoot)
		if err != nil {
			call.Fail(err)
			return
		}
	}
	resultValue, err = normalizeCorrelatedValueForOuterLayout(
		resultValue, outerCorr, physicalOuter, outerLayout)
	if err != nil {
		call.Fail(err)
		return
	}
	outerLayoutDependent := existentialProgramsRequireRetainedOuterLayout(
		regularPreds, innerPlan, outerLayout, resultValue)

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
			if predicates.ContainsExistentialPredicate(p) || !predicateReferencesInnerLeg(p, innerLegs) {
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
		expectedOuterRoots := map[values.CorrelationIdentifier]struct{}{outerCorr: {}}
		for _, predicate := range regularPreds {
			if predicates.ContainsExistentialPredicate(predicate) || !predicateReferencesInnerLeg(predicate, innerLegs) {
				continue
			}
			scalarAliases := scalarSubqueryAliasesOfPredicate(predicate)
			for correlation := range predicates.GetCorrelatedToOfPredicate(predicate) {
				if _, isInner := innerLegs[correlation]; isInner {
					continue
				}
				if _, isScalar := scalarAliases[correlation]; isScalar {
					continue
				}
				expectedOuterRoots[correlation] = struct{}{}
			}
		}
		mergedQOV, err := values.NewQuantifiedObjectValue(outerCorr, mergedRowType)
		if err != nil {
			call.Fail(err)
			return
		}
		for i, p := range regularPreds {
			if predicates.ContainsExistentialPredicate(p) || !predicateReferencesInnerLeg(p, innerLegs) {
				continue // not a child-row predicate
			}
			np, ok := rebaseOuterLegRefsOrdinal(p, windows, mergedQOV, expectedOuterRoots)
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
	if innerIsExistential && len(regularPreds) > 0 && !hasBooleanExists && !sel.IsQuantifiersSwapped() {
		if r.tryExistsFlatMap(
			call, resultValue, outerPlan, innerPlan,
			originalOuterCorr, outerCorr, innerCorr,
			physicalOuter, outerLayout,
			exactOuterAuthority, outerLayoutDependent,
			outerExpr, innerExpr, hasExistsFilter, negated, regularPreds) {
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
	var existentialResiduals []predicates.QueryPredicate
	for _, p := range regularPreds {
		if predicates.ContainsExistentialPredicate(p) {
			residual, err := predicates.ToResidualPredicate(p)
			if err != nil {
				call.Fail(err)
				return
			}
			existentialResiduals = append(existentialResiduals, residual)
		} else if predicateReferencesInnerLeg(p, innerLegs) {
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
	// counterpart of the existential peel's 3-quantifier (2 ForEach +
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
				// A collision-free whole-row alias deliberately leaves an
				// exact retained scalar source at its authored correlation. The
				// selected outer layout is the only authority that can distinguish
				// that source from an unbindable buried leg. Admit the correlation
				// only when every exact QOV with that identity in this predicate is
				// provided by the layout; a same-spelled foreign type stays loud.
				if predicateCorrelationProvidedByOuterLayout(p, outerLayoutPlan, a) {
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
	if innerIsExistential {
		innerQ, err = buildExistsCompensationChain(
			call, innerQ, innerPlan, innerCorr, joinPreds,
			hasExistsFilter, negated, false,
		)
		if err != nil {
			call.Fail(err)
			return
		}
	} else {
		// An ordinary inner retains all its rows, including an empty result.
		innerQ = expressions.NamedPhysicalQuantifier(quants[1].GetAlias(), call.MemoizeFinalExpression(innerPlan))
		existentialResiduals = append(joinPreds, existentialResiduals...)
	}

	if len(existentialResiduals) > 0 {
		filter, err := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(innerQ, existentialResiduals, innerCorr)
		if err != nil {
			call.Fail(err)
			return
		}
		innerQ = expressions.NewPhysicalQuantifier(call.MemoizeFinalExpression(filter))
	}

	// outerOnlyPreds deliberately keep the buried-leg references the
	// below-FOD rebase above rewrites: this filter runs ABOVE the FlatMap on
	// the outer's OWN row, where the merged-row producer doesn't alias-bind
	// (producesMergedRows suppresses the binding) and a buried QOV(D).ID
	// resolves through the row's QUALIFIED "D.ID" Datum key directly — the
	// masked-conjunct pin (`d.id = 3 AND NOT EXISTS …`) exercises exactly
	// this path. The below-FOD preds needed the rebase because THEIR row
	// context is the inner scan's frontier row, not the outer's.
	outerExpressionRef := call.MemoizeExpression(outerExpr)
	if exactOuterAuthority || outerLayoutDependent || outerCorr != originalOuterCorr {
		// The correlated programs below are rooted in this member's exact
		// retained-source layout. A later outer-group alternative must not replace
		// the member after collision selection or name normalization has frozen
		// those roots.
		outerExpressionRef = call.MemoizeFinalExpression(outerExpr)
	}
	outerQ := expressions.NamedPhysicalQuantifier(
		quants[0].GetAlias(), outerExpressionRef)

	if len(outerOnlyPreds) > 0 {
		ofInnerQ := expressions.NamedPhysicalQuantifier(outerQ.GetAlias(),
			call.MemoizeFinalExpression(outerPlan))
		outerFilter, err := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(ofInnerQ, outerOnlyPreds, outerCorr)
		if err != nil {
			call.Fail(err)
			return
		}
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
	flatMapPlan, err := plans.NewRecordQueryFlatMapPlanFromQuantifiersWithNullSupplying(
		outerQ, innerQ,
		outerCorr, innerCorr,
		resultValue, innerIsExistential,
		quants[0].IsNullOnEmpty(), false,
	)
	if err != nil {
		call.Fail(err)
		return
	}

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
	if exactOuterAuthority || outerLayoutDependent || outerCorr != originalOuterCorr {
		// The executable predicate is normalized against the preserve-order
		// winner's exact retained-source layout. An ordered alternative can expose
		// a different window set; keep the correct fallback plan and its enclosing
		// sort rather than pairing that predicate with unproven ownership.
		call.Yield(flatMapPlan)
	} else {
		r.yieldBinaryJoinWithOrderingVariants(call, flatMapPlan)
	}
}

// correlationForSourceAlias preserves the quantifier's exact identifier when
// source metadata repeats its rendered spelling. Reconstructing that spelling
// with NamedCorrelationIdentifier changes a planner-minted Unique q$ alias into
// a distinct user-named alias; predicates then miss the existential leg and can
// be routed to the outer filter. A genuinely different source alias remains an
// explicit named binding and is remapped by the caller.
func correlationForSourceAlias(
	quantifierAlias values.CorrelationIdentifier,
	sourceAlias string,
) values.CorrelationIdentifier {
	if sourceAlias == "" || sourceAlias == quantifierAlias.Name() {
		return quantifierAlias
	}
	return values.NamedCorrelationIdentifier(sourceAlias)
}

// collisionFreeExistentialOuterCorrelation separates the FlatMap's complete
// outer-row binding from a retained source window which happens to use the same
// correlation. Exact type is part of QOV identity, so EvaluationContext can
// carry both, but the FlatMap bookkeeping alias is also used to route ordinal
// rebases; using the retained source alias for the whole row conflates those
// roles before execution. The selected output layout is the only authority:
// same text without an admitted window, or the exact same source object type,
// is not a collision and preserves the original identifier.
func collisionFreeExistentialOuterCorrelation(
	call *ExpressionRuleCall,
	outer plans.RecordQueryPlan,
	candidate values.CorrelationIdentifier,
) (values.CorrelationIdentifier, error) {
	if outer == nil || candidate.IsZero() {
		return candidate, nil
	}
	layout, err := outer.ProvidedOutputLayout()
	if err != nil {
		var unavailable *plans.OrdinalLayoutUnavailableError
		if errors.As(err, &unavailable) {
			return candidate, nil
		}
		return values.CorrelationIdentifier{}, err
	}
	carrier := layout.Carrier()
	if carrier == nil {
		return values.CorrelationIdentifier{}, fmt.Errorf(
			"existential outer layout has no exact carrier")
	}
	for _, source := range layout.WindowSources() {
		if source == nil || source.Correlation() != candidate ||
			sameExactType(source.FlowedType(), carrier.FlowedType()) {
			continue
		}
		if call != nil && call.memo != nil {
			return call.memo.NextMergeAlias(), nil
		}
		return values.UniqueCorrelationIdentifier(), nil
	}
	return candidate, nil
}

// predicateCorrelationProvidedByOuterLayout proves that one otherwise-buried
// predicate correlation is an exact retained source of the selected outer
// plan. Correlation text alone is insufficient: a whole-row QOV and a scalar
// UNNEST source may intentionally share it. Every QOV occurrence at the
// correlation must therefore be admitted by exact type through LayoutProvides.
func predicateCorrelationProvidedByOuterLayout(
	predicate predicates.QueryPredicate,
	outer plans.RecordQueryPlan,
	correlation values.CorrelationIdentifier,
) bool {
	if predicate == nil || outer == nil || correlation.IsZero() {
		return false
	}
	layout, err := outer.ProvidedOutputLayout()
	if err != nil || layout == nil {
		return false
	}
	seen := false
	provided := true
	_, err = predicates.TransformEmbeddedValuesChecked(
		predicate,
		func(value values.Value) (values.Value, error) {
			values.WalkValue(value, func(node values.Value) bool {
				qov, ok := values.AsQuantifiedObjectValue(node)
				if !ok || qov.Correlation() != correlation {
					return true
				}
				seen = true
				matches, provideErr := values.LayoutProvides(layout, qov)
				if provideErr != nil || !matches {
					provided = false
					return false
				}
				return true
			})
			return value, nil
		})
	return err == nil && seen && provided
}

// existentialOuterWholeRowRoots declares the exact whole-row identity on both
// sides of an existential collision rename. The selected outer layout is the
// authority for the complete row type; retained source windows may use the
// same correlation for a narrower scalar or record and must not be rewritten.
func existentialOuterWholeRowRoots(
	outer plans.RecordQueryPlan,
	source, target values.CorrelationIdentifier,
) (values.QuantifiedObjectValue, values.QuantifiedObjectValue, error) {
	if outer == nil || source.IsZero() || target.IsZero() {
		return nil, nil, fmt.Errorf("existential whole-row translation requires plan and aliases")
	}
	layout, err := outer.ProvidedOutputLayout()
	if err != nil {
		return nil, nil, err
	}
	carrier := layout.Carrier()
	if carrier == nil {
		return nil, nil, fmt.Errorf("existential outer layout has no exact carrier")
	}
	declaration, err := values.NewQuantifiedObjectValue(source, carrier.FlowedType())
	if err != nil {
		return nil, nil, err
	}
	replacement, err := values.NewQuantifiedObjectValue(target, carrier.FlowedType())
	if err != nil {
		return nil, nil, err
	}
	return declaration, replacement, nil
}

// translateExistentialWholeRowPredicates moves only the complete outer-row
// declaration onto the fresh FlatMap binding. TranslateDeclaredEdgeRoot
// matches correlation AND exact type, so a retained UNNEST scalar such as
// X(INT) remains X while the stale pre-collision X(RECORD<...>) becomes the
// fresh whole-row QOV. Rebuilding is copy-on-write and leaves the logical
// predicate graph untouched.
func translateExistentialWholeRowPredicates(
	preds []predicates.QueryPredicate,
	declaration, replacement values.QuantifiedObjectValue,
) ([]predicates.QueryPredicate, error) {
	translated := make([]predicates.QueryPredicate, len(preds))
	for i, predicate := range preds {
		var err error
		translated[i], err = predicates.TransformEmbeddedValuesChecked(
			predicate,
			func(value values.Value) (values.Value, error) {
				return translateExistentialWholeRowValue(value, declaration, replacement)
			},
		)
		if err != nil {
			return nil, fmt.Errorf("existential predicate %d whole-row translation: %w", i, err)
		}
	}
	return translated, nil
}

// translateExistentialWholeRowPlanPredicates includes child scan comparisons:
// access matching may already have absorbed the existential WHERE predicate.
func translateExistentialWholeRowPlanPredicates(
	plan plans.RecordQueryPlan,
	declaration, replacement values.QuantifiedObjectValue,
) (plans.RecordQueryPlan, error) {
	transform := func(value values.Value) (values.Value, error) {
		return translateExistentialWholeRowValue(value, declaration, replacement)
	}
	translated, _, err := translateCorrelatedAccessPrograms(plan, transform, transform)
	return translated, err
}

func translateExistentialWholeRowValue(
	value values.Value,
	declaration, replacement values.QuantifiedObjectValue,
) (values.Value, error) {
	return values.TranslateDeclaredEdgeRoot(value, declaration, replacement)
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
) (values.Value, error) {
	if rv == nil {
		return nil, nil
	}
	pairs := make([]values.AliasPair, 0, 2)
	if outerQAlias != outerCorr {
		pairs = append(pairs, values.AliasPair{Source: outerQAlias, Target: outerCorr})
	}
	if innerQAlias != innerCorr {
		pairs = append(pairs, values.AliasPair{Source: innerQAlias, Target: innerCorr})
	}
	if len(pairs) == 0 {
		return rv, nil
	}
	am, err := values.NewAliasMap(pairs)
	if err != nil {
		return nil, err
	}
	return values.RebaseValueChecked(rv, am)
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

// typeUnstated reports whether t carries no inferred type — nil, or the
// UnknownType placeholder. Both mean the same thing to a comparison: this side
// has nothing to say, so it can neither confirm nor contradict the other.
func typeUnstated(t values.Type) bool {
	if t == nil {
		return true
	}
	pt, ok := t.(*values.PrimitiveType)
	return ok && pt.TypeCode == values.TypeCodeUnknown
}

// rebaseOuterLegRefsToMerged walks a predicate's values so that references to the
// original join-outer leg IDENTITIES (legAliases, e.g. ["E","D"]) can be placed
// against the inner-join's MERGED row bound under mergedCorr (RFC-141 Phase 2,
// P1a). It is the predicate-shape recursion only — Comparison/And/Or/Not, the
// shapes that can appear in existPreds; other shapes are returned unchanged, and
// what actually happens to a matched reference is rebaseOuterLegValue's three
// arms.
//
// It used to be accurate to say a leg reference `FieldValue{Field:"ID",
// Child:QOV("E")}` BECOMES `FieldValue{Field:"E.ID", Child:QOV(mergedCorr)}`,
// targeting the merged row's qualified "LEG.COL" key. That is no longer what
// happens: the qualified mint is deleted, and where a merged layout IS stated
// the re-anchor is by ORDINAL (Java's PartitionSelectRule.java:296-303), while
// where it is not the reference is handed back on its own leg correlation.
// References to any other alias (the existential inner P, parameters, constants)
// pass through untouched, as before.
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

// collectInnerLegAliases admits declared inner sources only when innerCorr names
// one of them; an opaque existential binding must not capture a shadowed outer.
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

	// SQL-owned inputs keep their WHERE predicates inside the child; only legacy
	// explicit source bindings expose child aliases to the parent's predicates.
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

func correlatedExistsComparisonRange(
	outerValue values.FieldValue,
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
	return mergeResult.Range, mergeResult.Complete()
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
	outerSourceCorrelation, outerCorrelation, innerCorrelation values.CorrelationIdentifier,
	wholeOuterBinding values.QuantifiedObjectValue,
	outerLayout values.OrdinalLayout,
	exactOuterAuthority, outerLayoutDependent bool,
	outerExpr, innerExpr expressions.RelationalExpression,
	hasExistsFilter, negated bool,
	preds []predicates.QueryPredicate,
) bool {
	innerScan, ok := innerPlan.(*plans.RecordQueryScanPlan)
	if !ok {
		return false
	}
	// Both access substitutions below replace the scan's bounds. A selected
	// constrained scan must instead keep its conditions and let the generic
	// existential path apply the correlated predicate residually.
	if len(innerScan.GetScanComparisons()) != 0 {
		return false
	}
	recordTypes := innerScan.GetRecordTypes()
	if len(recordTypes) != 1 {
		return false
	}

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
			if outerVal == nil && outerSourceCorrelation != outerCorrelation {
				outerVal = r.matchJoinPKPredicate(
					cp, outerSourceCorrelation, innerCorrelation, pkIdent, innerFrontier)
			}
			if outerVal == nil {
				continue
			}
			normalizedOuter, operandCorrelation, retainedWindow, admitted, admitErr := admitCorrelatedFastPathOuterValue(
				outerVal, wholeOuterBinding, outerLayout)
			if admitErr != nil {
				call.Fail(admitErr)
				return true
			}
			if !admitted {
				continue
			}
			comparisonRange, ok := correlatedExistsComparisonRange(
				normalizedOuter, operandCorrelation)
			if !ok {
				return false
			}
			correlatedScan := innerScan.WithScanComparisons(
				[]*predicates.ComparisonRange{comparisonRange})
			requiresExactOuter := exactOuterAuthority || outerLayoutDependent || retainedWindow ||
				outerCorrelation != outerSourceCorrelation
			r.yieldExistsFlatMap(
				call, resultValue, outerPlan, correlatedScan,
				outerCorrelation, innerCorrelation, outerExpr, innerExpr,
				hasExistsFilter, negated,
				requiresExactOuter,
				requiresExactOuter,
				pred, preds,
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
			if outerVal == nil && outerSourceCorrelation != outerCorrelation {
				outerVal = r.matchJoinPKPredicate(
					cp, outerSourceCorrelation, innerCorrelation, idxIdent, innerFrontier)
			}
			if outerVal == nil {
				continue
			}
			normalizedOuter, operandCorrelation, retainedWindow, admitted, admitErr := admitCorrelatedFastPathOuterValue(
				outerVal, wholeOuterBinding, outerLayout)
			if admitErr != nil {
				call.Fail(admitErr)
				return true
			}
			if !admitted {
				continue
			}
			// Build correlated index scan.
			comparisonRange, ok := correlatedExistsComparisonRange(
				normalizedOuter, operandCorrelation)
			if !ok {
				continue
			}
			indexPlan, indexErr := plans.NewRecordQueryIndexPlan(
				cand.CandidateName(),
				[]*predicates.ComparisonRange{comparisonRange},
				recordTypes, innerScan.GetFlowedType(), false,
			)
			if indexErr != nil {
				call.Fail(indexErr)
				return true
			}
			correlatedIndexScan := stampIndexMetadata(cand, indexPlan)

			requiresExactOuter := exactOuterAuthority || outerLayoutDependent || retainedWindow ||
				outerCorrelation != outerSourceCorrelation
			r.yieldExistsFlatMap(
				call, resultValue, outerPlan, correlatedIndexScan,
				outerCorrelation, innerCorrelation, outerExpr, innerExpr,
				hasExistsFilter, negated,
				requiresExactOuter,
				requiresExactOuter,
				pred, preds,
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
	pinOuter bool,
	restrictOrderingVariants bool,
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
	// pinOuter is the narrow exception: a correlated operand has been normalized
	// to a retained source proved by this exact outer member (including a source
	// revealed only by a selected-child FlatMap relink), or collision handling
	// has minted a distinct whole-row binding from this member's layout. The
	// member is then part of executable identity, so keeping a live replaceable
	// edge would be a plan/operand mismatch rather than useful enumeration.
	rightQ := expressions.NamedPhysicalQuantifier(innerCorrelation, call.MemoizeExpression(innerExpr))
	rightQ, err := buildExistsCompensationChain(
		call, rightQ, correlatedInner, innerCorrelation, innerResiduals,
		hasExistsFilter, negated, true,
	)
	if err != nil {
		call.Fail(err)
		return
	}

	outerRef := call.MemoizeExpression(outerExpr)
	if pinOuter {
		outerRef = call.MemoizeFinalExpression(outerExpr)
	}
	leftQ := expressions.NamedPhysicalQuantifier(outerCorrelation, outerRef)

	if len(outerResiduals) > 0 {
		ofInnerQ := expressions.NamedPhysicalQuantifier(leftQ.GetAlias(),
			call.MemoizeFinalExpression(outerPlan))
		outerFilter, err := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(ofInnerQ, outerResiduals, outerCorrelation)
		if err != nil {
			call.Fail(err)
			return
		}
		leftQ = expressions.NamedPhysicalQuantifier(outerCorrelation,
			call.MemoizeFinalExpression(outerFilter))
	}

	// The EXISTS FlatMap is its own cascades expression carrying its outer (leftQ)
	// and inner (rightQ) memo edges directly (RFC-184 W2, no physicalFlatMapWrapper).
	flatMapPlan, err := plans.NewRecordQueryFlatMapPlanFromQuantifiers(
		leftQ, rightQ,
		outerCorrelation, innerCorrelation,
		resultValue, true,
	)
	if err != nil {
		call.Fail(err)
		return
	}
	if restrictOrderingVariants {
		call.Yield(flatMapPlan)
	} else {
		r.yieldBinaryJoinWithOrderingVariants(call, flatMapPlan)
	}
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
// sub-join. The exact fast-path admission now names only the complete outer
// binding and the selected layout's declared retained windows; an unrelated
// buried leg is not one of those authorities. Re-arming a deeper capability
// must therefore go through the buried-leg rebase rather than a silent
// bare-name read of the merged row.
func (r *ImplementNestedLoopJoinRule) matchJoinPKPredicate(
	cp *predicates.ComparisonPredicate,
	outerCorr, innerCorr values.CorrelationIdentifier,
	keyIdent values.ColumnIdentity,
	frontier values.OrdinalDomain,
) values.FieldValue {
	lhsFV, lhsOk := values.AsFieldValue(cp.Operand)
	rhsFV, rhsOk := values.AsFieldValue(cp.Comparison.Operand)
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
func legCorrelationOf(fv values.FieldValue) (values.CorrelationIdentifier, bool) {
	if fv == nil {
		return values.CorrelationIdentifier{}, false
	}
	if qov, ok := values.AsQuantifiedObjectValue(fv.ChildValue()); ok {
		if fv.Path() == nil || fv.Path().Len() != 1 {
			return values.CorrelationIdentifier{}, false
		}
		return qov.Correlation(), true
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
	// Failing closed here lets matchJoinPKPredicate decline the rewrite rather
	// than read a wrong slot. Predicate pushdown uses the complete predicate
	// correlation set instead: ignoring an unrecognized field in that decision
	// could hide a sibling dependency while another field licenses the push.
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
	fv values.FieldValue,
	legCorr values.CorrelationIdentifier,
	keyIdent values.ColumnIdentity,
	frontier values.OrdinalDomain,
) bool {
	id, ok := values.CorrelatedFieldIdentityIn(fv, frontier)
	if !ok {
		return false
	}
	return values.SameLeg(id.Correlation, legCorr) &&
		id.Domain == keyIdent.Domain && id.Ordinal == keyIdent.Ordinal
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
	outerVal values.FieldValue,
	outerCorrelation values.CorrelationIdentifier,
) (values.Value, bool) {
	path := outerVal.Path()
	if path != nil && path.IsFrontierPinned() {
		return outerVal, true
	}
	// A SOURCE-RELATIVE bake (the resolver's construction-time
	// ordinal, addressed to a real SQL source) transfers to the rebuilt operand
	// verbatim. matchJoinPKPredicate first restricts the operand to an explicit
	// whole-row or authored source correlation, and
	// admitCorrelatedFastPathOuterValue then proves that exact object against the
	// selected layout. The declared-column-order ordinal therefore reads the
	// right source slot without a name lookup.
	// SourceRelativeBaked() requires len(Accessors) == 1, so this ADMITS only a FLAT outer reference; a nested descent on the outer source is not eligible for the fast path.
	if path != nil && path.Len() == 1 && path.RootDomain().IsKnown() {
		// The rebuilt operand's DISPLAY name. Its identity is the ordinal
		// passed beside it; this string is never compared, keyed or resolved,
		// and the constructor is the one place RFC-197 leaves a name
		// legitimate. It is taken VERBATIM: there used to be a branch here
		// that sliced a qualifier out of a CHILDLESS reference's dotted
		// display name (the qualified-name channel, RFC-197 bucket 6), and it
		// is unreachable now because exact fast-path admission requires a direct
		// QuantifiedObjectValue child before this helper runs.
		qov, ok := values.AsQuantifiedObjectValue(outerVal.ChildValue())
		if !ok || qov.Correlation() != outerCorrelation {
			return nil, false
		}
		return outerVal, true
	}
	return nil, false
}

var _ ExpressionRule = (*ImplementNestedLoopJoinRule)(nil)
