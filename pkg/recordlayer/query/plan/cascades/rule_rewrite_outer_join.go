package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
)

// RewriteOuterJoinRule canonicalizes a LEFT OUTER join SelectExpression into the
// nested form Java uses (rules/RewriteOuterJoinRule.java + expressions/OuterJoin
// Expression.java): the ON-predicates are pushed BELOW the null-extension boundary
// into a correlated null-supplying inner SELECT, and the join becomes an INNER
// SelectExpression over the preserved leg plus a NULL-on-empty quantifier carrying
// the outer-join semantics.
//
// Go encodes an outer join as a flat 2-quantifier SelectExpression with
// joinType=JoinLeftOuter and the ON-predicates in the top-level predicate list
// (the translator keeps WHERE *above* the join for LEFT OUTER, so every predicate
// here is an ON-predicate). Java's RewriteOuterJoinRule matches an
// OuterJoinExpression; Go matches the LEFT-OUTER SelectExpression directly, since
// Go carries outer-join on the flag rather than a dedicated logical box. That is
// the only deviation from a 1:1 port, forced by Go's representation.
//
// Why this is needed (RFC-150 Phase-2b Piece 2): without it, neither
// PartitionBinarySelectRule nor PushFilterBelowJoinRule fires (both guard on
// JoinInner), so no leg becomes correlated, and the data-access FlatMap path
// (yieldGeneralFlatMap) never produces a correlated LEFT-OUTER FlatMap. This rule
// creates the correlation Java's single path relies on — it is what let the former
// Go-only tryFlatMapPlan (a hand-rolled inner-pushed-residual shortcut) be retired:
// the correlated LEFT-OUTER FlatMap now emerges from the standard data-access path.
//
// Correctness (the LEFT-OUTER axis): the ON-predicates MUST filter the inner stream
// BEFORE the empty→NULL extension. Folding them into innerSelect (below the
// NULL-on-empty quantifier) does exactly that — applying an ON-predicate above the
// FlatMap, after the NULL row is injected, would drop that row and silently degrade
// LEFT OUTER into INNER. The outer SelectExpression carries NO predicates, so there
// is nothing left to apply above the join. Mirrors Java RewriteOuterJoinRule.
type RewriteOuterJoinRule struct {
	matcher matching.BindingMatcher
}

// NewRewriteOuterJoinRule constructs the rule.
func NewRewriteOuterJoinRule() *RewriteOuterJoinRule {
	return &RewriteOuterJoinRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("outer_join_select"),
	}
}

// Matcher returns the pattern.
func (r *RewriteOuterJoinRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch rewrites a LEFT OUTER SelectExpression into the INNER + null-on-empty form.
func (r *RewriteOuterJoinRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)

	// LEFT OUTER only. RIGHT OUTER is normalized to LEFT-with-swapped-children in the
	// translator; FULL OUTER stays on the materialized NestedLoopJoin (Java has no FULL
	// in Cascades — it tracks global inner-match state for the drain phase, which a
	// FlatMap cannot do).
	if sel.GetJoinType() != expressions.JoinLeftOuter {
		return
	}
	quantifiers := sel.GetQuantifiers()
	if len(quantifiers) != 2 {
		return
	}
	preserved := quantifiers[0]     // left leg = preserved side
	nullSupplying := quantifiers[1] // right leg = null-supplying side
	if preserved.Kind() != expressions.QuantifierForEach ||
		nullSupplying.Kind() != expressions.QuantifierForEach {
		return
	}

	// Only rewrite when there ARE ON-predicates to push below the null-extension; a
	// predicate-less LEFT OUTER is a degenerate cross-with-null-fill that the
	// materialized NLJ already handles. (Matches the "useful partition" guard in
	// PartitionBinarySelectRule.)
	preds := sel.GetPredicates()
	if len(preds) == 0 {
		return
	}

	// Only rewrite a CORRELATED LEFT OUTER — one whose ON-predicates actually
	// reference the preserved leg, so the rewritten inner SUBSEL becomes correlated and
	// the data-access FlatMap path (which replaced the retired tryFlatMapPlan) can fire. For
	// an UNCORRELATED LEFT OUTER (ON FALSE / ON NULL / a predicate local to the
	// null-supplying side), the rewrite would produce a null-on-empty inner with no
	// correlation → no FlatMap → the non-correlated NLJ path, which would use the now-
	// INNER join type and DROP the unmatched outer rows (silent degrade to INNER).
	// Those are left on the original LEFT-OUTER materialized NLJ, which null-extends
	// correctly. (tryFlatMapPlan likewise only handled the correlated case.)
	preservedAlias := preserved.GetAlias()
	correlated := false
	for _, p := range preds {
		if _, ok := predicates.GetCorrelatedToOfPredicate(p)[preservedAlias]; ok {
			correlated = true
			break
		}
	}
	if !correlated {
		return
	}

	// Idempotency: if this Reference already holds the rewritten form (an INNER
	// 2-quantifier SelectExpression with a null-on-empty quantifier), don't re-fire.
	// Mirrors PartitionBinarySelectRule's idempotency guard (prevents memo blow-up).
	for _, m := range call.Reference.Members() {
		if other, ok := m.(*expressions.SelectExpression); ok && other != sel {
			if other.GetJoinType() == expressions.JoinInner && len(other.GetQuantifiers()) == 2 {
				for _, q := range other.GetQuantifiers() {
					if q.IsNullOnEmpty() {
						return
					}
				}
			}
		}
	}

	// buildInnerSelect: wrap the null-supplying leg in a SUBSEL carrying ALL the
	// ON-predicates, then expose it through a NULL-on-empty quantifier that REUSES the
	// null-supplying alias (so the outer result value, which references that alias,
	// stays correctly correlated). The ON-predicate's reference to the preserved alias
	// makes innerSelect correlated to the preserved leg — exactly the rightDepsLeft
	// shape the data-access FlatMap path consumes.
	builder := NewGraphExpansionBuilder()
	builder.AddQuantifier(nullSupplying)
	for _, p := range preds {
		builder.AddPredicate(p)
	}
	innerSelect := builder.Build().Seal().BuildSelectWithResultValue(
		nullSupplying.GetFlowedObjectValue(),
	)
	nullOnEmptyQun := expressions.NamedForEachNullOnEmptyQuantifier(
		nullSupplying.GetAlias(),
		call.MemoizeExpression(innerSelect),
	)

	// Propagate source aliases in the (preserved, null-on-empty) order.
	la := preserved.GetAlias().Name()
	ra := nullSupplying.GetAlias().Name()
	var aliases []string
	if la != "" && ra != "" {
		aliases = []string{la, ra}
	}

	// The outer SelectExpression is INNER (outer-join semantics now live entirely in
	// the null-on-empty quantifier) and carries NO predicates.
	outerSelect := expressions.NewSelectExpressionWithJoinType(
		sel.GetResultValue(),
		[]expressions.Quantifier{preserved, nullOnEmptyQun},
		nil,
		aliases,
		expressions.JoinInner,
	)
	call.Yield(outerSelect)
}

var _ ExpressionRule = (*RewriteOuterJoinRule)(nil)
