package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PredicatePushDownRule pushes predicates from an outer SelectExpression
// into its child quantifiers when the predicate only references a
// single child's aliases. This is the generic predicate push-down rule
// intended for the REWRITING phase — it handles arbitrary
// SelectExpression shapes, unlike the specific Push*Through* rules
// which target Filter/Projection/Sort/etc.
//
// Algorithm:
//  1. Match SelectExpression.
//  2. For each ForEach quantifier, identify predicates that can be
//     pushed — those whose correlation set has no overlap with any
//     OTHER quantifier's alias.
//  3. For each pushable-predicate + quantifier pair, visit the child
//     expression through the quantifier's Reference and create a new
//     expression with the predicate absorbed or pushed through.
//  4. Build a new outer SelectExpression with the remaining (non-pushed)
//     predicates and the rewritten quantifiers.
//
// Existential quantifiers are handled correctly: the per-quantifier
// loop skips non-ForEach quantifiers, so predicates are only pushed
// into ForEach children. Existential siblings remain untouched.
// Matches Java's behavior (no global existential guard).
//
// Convergence: each firing strictly reduces the set of pushable
// predicates in the outer SelectExpression. A SelectExpression with no
// pushable predicates causes zero yields.
//
// Ports Java's PredicatePushDownRule (ExplorationCascadesRule, 444 LOC).
type PredicatePushDownRule struct {
	matcher matching.BindingMatcher
}

func NewPredicatePushDownRule() *PredicatePushDownRule {
	return &PredicatePushDownRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("predicate_push_down"),
	}
}

func (r *PredicatePushDownRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PredicatePushDownRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	quantifiers := sel.GetQuantifiers()

	// Guard: don't push predicates into SelectExpressions containing
	allPredicates := sel.GetPredicates()
	if len(allPredicates) == 0 {
		return
	}

	// For each ForEach quantifier, try to push predicates into it.
	// We iterate quantifiers one at a time: for each, we identify
	// predicates that reference ONLY that quantifier (and no other
	// sibling quantifier). Java's rule matches on a single ForEach
	// quantifier at a time (via the matcher) and fires once per
	// quantifier; Go's rule iterates all quantifiers in one pass.
	for qIdx, pushQ := range quantifiers {
		if pushQ.Kind() != expressions.QuantifierForEach {
			continue
		}
		// Both flags are semantic barriers. NullOnEmpty must see predicates
		// after null extension; StrictSingle must count rows before any
		// scalar-dependent predicate can hide a second row. This rule rebuilds
		// the pushed edge as a plain ForEach, so either flag also has to block
		// the rewrite to prevent carrier erasure.
		if pushQ.IsNullOnEmpty() || pushQ.IsStrictSingle() {
			continue
		}

		// Compute the set of "other" quantifier aliases.
		otherAliases := map[values.CorrelationIdentifier]struct{}{}
		for j, q := range quantifiers {
			if j == qIdx {
				continue
			}
			otherAliases[q.GetAlias()] = struct{}{}
		}

		// Partition predicates: pushable vs fixed.
		var pushable, fixed []predicates.QueryPredicate
		for _, pred := range allPredicates {
			correlated := predicates.GetCorrelatedToOfPredicate(pred)
			canPush := true
			for alias := range correlated {
				if _, isOther := otherAliases[alias]; isOther {
					canPush = false
					break
				}
			}
			if canPush {
				pushable = append(pushable, pred)
			} else {
				fixed = append(fixed, pred)
			}
		}

		if len(pushable) == 0 {
			continue
		}

		// Try to push the pushable predicates into/through the child
		// expression. Visit the child Reference's members.
		childRef := pushQ.GetRangesOver()
		if childRef == nil {
			continue
		}

		var newBelowExpressions []expressions.RelationalExpression
		for _, member := range childRef.AllMembers() {
			pushed, err := pushPredicateToExpression(call, pushable, pushQ, member)
			if err != nil {
				call.Fail(err)
				return
			}
			if pushed != nil {
				newBelowExpressions = append(newBelowExpressions, pushed)
			}
		}

		if len(newBelowExpressions) == 0 {
			continue
		}

		// Memoize the new below expressions into a new Reference.
		var newChildRef *expressions.Reference
		if len(newBelowExpressions) == 1 {
			newChildRef = call.MemoizeExpression(newBelowExpressions[0])
		} else {
			newChildRef = expressions.InitialOf(newBelowExpressions[0])
			for i := 1; i < len(newBelowExpressions); i++ {
				newChildRef.Insert(newBelowExpressions[i])
			}
		}

		// Build new quantifier with the same alias but ranging over
		// the new child Reference.
		newPushQ := expressions.NamedForEachQuantifier(pushQ.GetAlias(), newChildRef)

		// Build the new quantifier list, replacing the push quantifier.
		newQuantifiers := make([]expressions.Quantifier, len(quantifiers))
		for i, q := range quantifiers {
			if i == qIdx {
				newQuantifiers[i] = newPushQ
			} else {
				newQuantifiers[i] = q
			}
		}

		// Build the new SelectExpression with the remaining (fixed) predicates.
		newSel, err := expressions.NewSelectExpressionWithJoinType(
			sel.GetResultValue(),
			newQuantifiers,
			fixed,
			sel.GetSourceAliases(),
			sel.GetJoinType(),
		)
		if err != nil {
			call.Fail(err)
			return
		}
		call.Yield(newSel)
		return // One quantifier per rule firing, matching Java's behavior.
	}
}

// pushedAliasDenotesSelectRow reports whether the row produced by a Select is
// the same exact row the pushed quantifier's alias names.
//
// It is the precondition for substituting the alias by that Select's result
// value, and it is a TYPE question rather than a naming one: two rows that
// agree on shape agree on what every ordinal in a pushed predicate addresses,
// and two that do not cannot both be what the alias means. An edge that cannot
// state its flowed row answers no — the substitution has nothing to check
// against, and an unchecked one is the silent wrong-column read.
func pushedAliasDenotesSelectRow(
	pushQuantifier expressions.Quantifier,
	resultValue values.Value,
) bool {
	if resultValue == nil {
		return false
	}
	flowed, err := pushQuantifier.RequireFlowedObjectValue()
	if err != nil || flowed == nil || flowed.FlowedType() == nil {
		return false
	}
	return values.QuantifiedRowShapesAgree(flowed.FlowedType(), resultValue.Type())
}

// rebasedAliasesDenoteOneRow reports whether two quantifiers flow the same
// exact row, which is what an alias-only predicate rebase between them assumes.
// A quantifier that cannot state its flowed row answers no.
func rebasedAliasesDenoteOneRow(from, to expressions.Quantifier) bool {
	fromFlowed, fromErr := from.RequireFlowedObjectValue()
	toFlowed, toErr := to.RequireFlowedObjectValue()
	if fromErr != nil || toErr != nil || fromFlowed == nil || toFlowed == nil {
		return false
	}
	return values.QuantifiedRowShapesAgree(fromFlowed.FlowedType(), toFlowed.FlowedType())
}

// pushPredicateToExpression is the Go equivalent of Java's PushToVisitor.
// It visits the child expression and returns a new expression with the
// predicates pushed in, or nil if the expression type doesn't support
// predicate push-down.
func pushPredicateToExpression(
	call *ExpressionRuleCall,
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	belowExpression expressions.RelationalExpression,
) (expressions.RelationalExpression, error) {
	switch expr := belowExpression.(type) {
	case *expressions.LogicalFilterExpression:
		return pushIntoLogicalFilter(originalPredicates, pushQuantifier, expr)
	case *expressions.SelectExpression:
		return pushIntoSelect(originalPredicates, pushQuantifier, expr)
	case *expressions.LogicalUnionExpression:
		return pushThroughUnion(call, originalPredicates, pushQuantifier, expr)
	case *expressions.LogicalSortExpression:
		return pushThroughSort(call, originalPredicates, pushQuantifier, expr)
	case *expressions.LogicalDistinctExpression:
		return pushThroughDistinct(call, originalPredicates, pushQuantifier, expr)
	case *expressions.LogicalUniqueExpression:
		return pushThroughUnique(call, originalPredicates, pushQuantifier, expr)
	default:
		// By default, we cannot push things down. Return nil.
		return nil, nil
	}
}

// pushIntoLogicalFilter absorbs predicates into a LogicalFilterExpression
// by combining the original predicates (rebased to the filter's inner
// alias) with the filter's existing predicates. Returns a new
// SelectExpression. Ports Java's PushToVisitor.visitLogicalFilterExpression.
func pushIntoLogicalFilter(
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	filter *expressions.LogicalFilterExpression,
) (expressions.RelationalExpression, error) {
	inner := filter.GetInner()
	if inner.Kind() != expressions.QuantifierForEach {
		return nil, nil
	}
	// An alias-only rebase keeps every ORDINAL and changes only which row they
	// are read from, so the two rows have to be the same row. Reached on
	// `… LEFT JOIN emp e ON … WHERE e.id IS NULL AND NOT EXISTS (…)`, where
	// `E.ID#0 IS NULL` was rebased onto a preserved leg aliased D and became
	// `D.ID#0 IS NULL` — a predicate on a DIFFERENT column, which the access
	// path then turned into a scan range on DEPT's primary key.
	if !rebasedAliasesDenoteOneRow(pushQuantifier, inner) {
		return nil, nil
	}

	// Rebase: pushQuantifier.alias -> inner.alias
	aliasMap, err := values.NewAliasMap([]values.AliasPair{{
		Source: pushQuantifier.GetAlias(),
		Target: inner.GetAlias(),
	}})
	if err != nil {
		return nil, err
	}

	// Combine: existing filter predicates + rebased original predicates.
	newPredicates := make([]predicates.QueryPredicate, 0, len(filter.GetPredicates())+len(originalPredicates))
	newPredicates = append(newPredicates, filter.GetPredicates()...)
	for _, p := range originalPredicates {
		// CHECKED. The error-less spelling returns nil on a failed rebase, and a
		// nil appended here is not a dropped rebase — it is a nil element in a
		// predicate list, which downstream reads as a predicate that is not
		// there. Losing a pushed-down predicate returns rows the query excluded.
		rebased, rerr := predicates.RebasePredicateChecked(p, aliasMap)
		if rerr != nil {
			return nil, rerr
		}
		newPredicates = append(newPredicates, rebased)
	}

	flowed, err := inner.RequireFlowedObjectValue()
	if err != nil {
		return nil, err
	}
	return expressions.NewSelectExpression(
		flowed,
		[]expressions.Quantifier{inner},
		newPredicates,
	)
}

// pushIntoSelect absorbs predicates into a SelectExpression by
// translating them to reference the select's result value and combining
// with the select's existing predicates. Returns a new SelectExpression.
// Ports Java's PushToVisitor.visitSelectExpression — UNCONDITIONAL there
// (PredicatePushDownRule.java:378-392) because Java's SelectExpression can
// never carry outer-join semantics: Java routes LEFT OUTER through a
// null-on-empty quantifier (RewriteOuterJoinRule) and has no Cascades
// representation for FULL OUTER at all, so every Java SelectExpression is
// ChildrenAsSet-equivalent (inner/cross) and absorbing a predicate into its
// own list is always sound.
//
// Go's SelectExpression is a wider, Go-only extension that ALSO carries
// FULL/LEFT/RIGHT OUTER directly via joinType (select.go's ChildrenAsSet
// doc, RewriteOuterJoinRule's header) — a child in THIS shape is not the
// shape visitSelectExpression assumed. Absorbing a WHERE-class predicate
// into such a child's OWN predicate list turns it into an ON-condition:
// the join's null-extension drain for an unmatched row runs regardless of
// that extra condition, so the predicate stops filtering the padded row it
// was meant to reject (WHERE-above-OUTER silently degrades into
// ON-below-OUTER — full/left/right outer join drain bypasses it). Every
// sibling rule that can reach into a child SelectExpression already guards
// this exact Go-only shape (PushFilterBelowJoinRule's JoinInner check,
// PartitionBinarySelectRule's same check, SelectMergeRule's
// ChildrenAsSet() gate) — this rule is the one that was missing it.
func pushIntoSelect(
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	selectExpr *expressions.SelectExpression,
) (expressions.RelationalExpression, error) {
	// An OUTER-join child (FULL/LEFT/RIGHT) is opaque to predicate
	// absorption: see the function doc. ChildrenAsSet() is Go's existing
	// commutative/inner-equivalent marker (select.go) — false for every
	// outer join type, matching the opacity gate every sibling rule uses.
	if !selectExpr.ChildrenAsSet() {
		return nil, nil
	}

	// A plain parent edge can still range over a Select that owns the strict
	// scalar edge internally. Absorbing the parent's predicate into that Select
	// would move it below the strict FirstOrDefault boundary, allowing the
	// predicate to hide a second row before cardinality is checked. Treat the
	// nested carrier as opaque just like a directly flagged push quantifier.
	if hasStrictSingleQuantifier(selectExpr.GetQuantifiers()) {
		return nil, nil
	}

	// Build a TranslationMap: pushQuantifier.alias -> selectExpr.resultValue.
	resultValue := selectExpr.GetResultValue()
	// The substitution replaces the alias's whole ROW, so the row this Select
	// produces has to BE the row the alias denotes. When it is not, the
	// ordinals travel unchanged into a different layout and the predicate
	// silently reads a different column: pushing `E.ID#0 IS NULL` into a Select
	// whose result is a join box `{D.ID, D.DNAME, E.ID, …}` rewrites it to
	// `D.ID#0 IS NULL`, which then matched DEPT's primary key and became a scan
	// range on the PRESERVED leg. `SELECT d.dname FROM dept d LEFT JOIN emp e ON
	// e.dept_id = d.id WHERE e.id IS NULL AND NOT EXISTS (…)` returned no rows
	// instead of the one department with no employees, and the plan showed no
	// trace of the conjunct at all.
	if !pushedAliasDenotesSelectRow(pushQuantifier, resultValue) {
		return nil, nil
	}
	tmBuilder := NewTranslationMapBuilder()
	tmBuilder.When(pushQuantifier.GetAlias()).Then(func(_ values.CorrelationIdentifier, _ values.LeafValue) values.Value {
		return resultValue
	})
	tm := tmBuilder.Build()

	// Combine: existing select predicates + translated original predicates.
	newPredicates := make([]predicates.QueryPredicate, 0, len(selectExpr.GetPredicates())+len(originalPredicates))
	newPredicates = append(newPredicates, selectExpr.GetPredicates()...)
	for _, p := range originalPredicates {
		// A predicate that cannot be re-expressed against the child Select's
		// result value simply does not push. Declining leaves it where it is,
		// above this Select, where it is still correct — the alternative is a
		// predicate rebuilt around a nil operand, which is not a worse plan but
		// an impossible one.
		translated, ok := translatePredicateCorrelations(p, tm)
		if !ok {
			return nil, nil
		}
		newPredicates = append(newPredicates, translated)
	}

	return expressions.NewSelectExpressionWithJoinType(
		selectExpr.GetResultValue(),
		selectExpr.GetQuantifiers(),
		newPredicates,
		selectExpr.GetSourceAliases(),
		selectExpr.GetJoinType(),
	)
}

// pushOverChild creates a new SelectExpression wrapping the child
// quantifier with the pushed-down predicates. Used when pushing through
// expressions that don't directly absorb predicates. Returns a new
// ForEach quantifier ranging over the new SelectExpression.
// Ports Java's PushToVisitor.pushOverChild.
func pushOverChild(
	call *ExpressionRuleCall,
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	child expressions.Quantifier,
) (expressions.Quantifier, error) {
	// Same precondition as pushIntoLogicalFilter's rebase: ordinals survive the
	// alias change untouched, so the two aliases must name the same row.
	if !rebasedAliasesDenoteOneRow(pushQuantifier, child) {
		return expressions.Quantifier{}, nil
	}
	// Rebase: pushQuantifier.alias -> child.alias
	aliasMap, err := values.NewAliasMap([]values.AliasPair{{
		Source: pushQuantifier.GetAlias(),
		Target: child.GetAlias(),
	}})
	if err != nil {
		return expressions.Quantifier{}, err
	}

	newPredicates := make([]predicates.QueryPredicate, len(originalPredicates))
	for i, p := range originalPredicates {
		// CHECKED — see the sibling loop above for why a nil element here is a
		// silently dropped predicate rather than a reported failure.
		rebased, rerr := predicates.RebasePredicateChecked(p, aliasMap)
		if rerr != nil {
			return expressions.Quantifier{}, rerr
		}
		newPredicates[i] = rebased
	}

	flowed, err := child.RequireFlowedObjectValue()
	if err != nil {
		return expressions.Quantifier{}, err
	}
	newSelect, err := expressions.NewSelectExpression(
		flowed,
		[]expressions.Quantifier{child},
		newPredicates,
	)
	if err != nil {
		return expressions.Quantifier{}, err
	}
	return expressions.ForEachQuantifier(call.MemoizeExpression(newSelect)), nil
}

// pushThroughUnion pushes predicates through a LogicalUnionExpression by
// creating a new SelectExpression over each union leg with the pushed
// predicates. Ports Java's PushToVisitor.visitLogicalUnionExpression.
func pushThroughUnion(
	call *ExpressionRuleCall,
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	union *expressions.LogicalUnionExpression,
) (expressions.RelationalExpression, error) {
	qs := union.GetQuantifiers()
	newChildren := make([]expressions.Quantifier, len(qs))
	for i, q := range qs {
		if q.Kind() != expressions.QuantifierForEach {
			return nil, nil
		}
		newChild, err := pushOverChild(call, originalPredicates, pushQuantifier, q)
		if err != nil {
			return nil, err
		}
		newChildren[i] = newChild
	}
	return expressions.NewLogicalUnionExpression(newChildren)
}

// pushThroughSort pushes predicates through a LogicalSortExpression by
// creating a new SelectExpression below the sort's single child.
// Ports Java's PushToVisitor.visitLogicalSortExpression.
func pushThroughSort(
	call *ExpressionRuleCall,
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	sort *expressions.LogicalSortExpression,
) (expressions.RelationalExpression, error) {
	inner := sort.GetInner()
	if inner.Kind() != expressions.QuantifierForEach {
		return nil, nil
	}
	newChild, err := pushOverChild(call, originalPredicates, pushQuantifier, inner)
	if err != nil {
		return nil, err
	}
	return expressions.NewLogicalSortExpression(sort.GetSortKeys(), newChild)
}

// pushThroughDistinct pushes predicates through a LogicalDistinctExpression
// by creating a new SelectExpression below the distinct's single child.
// Ports Java's PushToVisitor.visitLogicalDistinctExpression.
func pushThroughDistinct(
	call *ExpressionRuleCall,
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	distinct *expressions.LogicalDistinctExpression,
) (expressions.RelationalExpression, error) {
	inner := distinct.GetInner()
	if inner.Kind() != expressions.QuantifierForEach {
		return nil, nil
	}
	newChild, err := pushOverChild(call, originalPredicates, pushQuantifier, inner)
	if err != nil {
		return nil, err
	}
	return expressions.NewLogicalDistinctExpression(newChild)
}

// pushThroughUnique pushes predicates through a LogicalUniqueExpression
// by creating a new SelectExpression below the unique's single child.
// Ports Java's PushToVisitor.visitLogicalUniqueExpression.
func pushThroughUnique(
	call *ExpressionRuleCall,
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	unique *expressions.LogicalUniqueExpression,
) (expressions.RelationalExpression, error) {
	inner := unique.GetInner()
	if inner.Kind() != expressions.QuantifierForEach {
		return nil, nil
	}
	newChild, err := pushOverChild(call, originalPredicates, pushQuantifier, inner)
	if err != nil {
		return nil, err
	}
	return unique.WithQuantifiers([]expressions.Quantifier{newChild})
}

var _ ExpressionRule = (*PredicatePushDownRule)(nil)
