package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PredicatePushDownRule pushes the predicates of a final SelectExpression that
// read only one of its ForEach quantifiers into that quantifier's pruned child,
// all such quantifiers in one firing.
//
// Ports Java's PredicatePushDownRule: a REWRITING implementation rule over a
// final Select whose inputs are pruned (OnPrunedInputsRule), run after
// SelectMergeRule makes no progress. Each child is its reference's single final
// expression; a child the visitor cannot push into keeps its finals.
type PredicatePushDownRule struct {
	matcher matching.BindingMatcher
}

func NewPredicatePushDownRule() *PredicatePushDownRule {
	return &PredicatePushDownRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("predicate_push_down"),
	}
}

func (r *PredicatePushDownRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnlyOnPrunedInputs: Java's PredicatePushDownRule implements OnPrunedInputsRule.
func (r *PredicatePushDownRule) OnlyOnPrunedInputs() bool { return true }

// pushDownMemoizer is the reference factory the push visitors build children
// with: final references from the rule, exploratory ones from tests.
type pushDownMemoizer interface {
	MemoizeExpression(expressions.RelationalExpression) *expressions.Reference
}

// finalPushDownMemoizer is Java's FinalMemoizer.memoizeFinalExpression.
type finalPushDownMemoizer struct{ call *ImplementationRuleCall }

func (m finalPushDownMemoizer) MemoizeExpression(expr expressions.RelationalExpression) *expressions.Reference {
	return m.call.MemoizeFinalExpression(expr)
}

func (r *PredicatePushDownRule) OnMatch(call *ImplementationRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	if !isFinalMember(call.Reference, sel) {
		return
	}
	// An outer join's predicates are its ON conditions; RewriteOuterJoinRule
	// moves them below the null extension. Java's rule never sees one.
	if !sel.ChildrenAsSet() || len(sel.GetPredicates()) == 0 {
		return
	}
	if _, isRecord := sel.GetResultValue().Type().(*values.RecordType); !isRecord {
		return
	}
	quantifiers := sel.GetQuantifiers()
	residual := append([]predicates.QueryPredicate(nil), sel.GetPredicates()...)
	newQuantifiers := make([]expressions.Quantifier, len(quantifiers))
	pushedAny := false
	memoizer := finalPushDownMemoizer{call: call}
	for qIdx, pushQ := range quantifiers {
		// Both flags are semantic barriers. NullOnEmpty must see predicates
		// after null extension; StrictSingle must count rows before any
		// scalar-dependent predicate can hide a second row.
		if pushQ.Kind() != expressions.QuantifierForEach || pushQ.IsNullOnEmpty() || pushQ.IsStrictSingle() {
			continue
		}
		childRef := pushQ.GetRangesOver()
		if childRef == nil {
			continue
		}
		finals := childRef.FinalMembers()
		if len(finals) != 1 {
			continue
		}
		otherAliases := map[values.CorrelationIdentifier]struct{}{}
		for j, q := range quantifiers {
			if j != qIdx {
				otherAliases[q.GetAlias()] = struct{}{}
			}
		}
		var pushable, kept []predicates.QueryPredicate
		for _, pred := range residual {
			canPush := true
			for alias := range predicates.GetCorrelatedToOfPredicate(pred) {
				if _, isOther := otherAliases[alias]; isOther {
					canPush = false
					break
				}
			}
			if canPush {
				pushable = append(pushable, pred)
			} else {
				kept = append(kept, pred)
			}
		}
		if len(pushable) == 0 {
			continue
		}
		pushed, err := pushPredicateToExpression(memoizer, pushable, pushQ, finals[0])
		if err != nil {
			call.Fail(err)
			return
		}
		if pushed == nil {
			continue
		}
		newQuantifiers[qIdx] = expressions.NamedForEachQuantifier(pushQ.GetAlias(), call.MemoizeFinalExpression(pushed))
		residual = kept
		pushedAny = true
	}
	if !pushedAny {
		return
	}
	for i, q := range quantifiers {
		if newQuantifiers[i].GetRangesOver() != nil {
			continue
		}
		ref := q.GetRangesOver()
		newQuantifiers[i] = expressions.RebuildQuantifier(q, call.MemoizeFinalExpressionsFromOther(ref, ref.FinalMembers()))
	}
	newSel, err := expressions.NewSelectExpressionWithJoinType(
		sel.GetResultValue(),
		newQuantifiers,
		residual,
		sel.GetSourceAliases(),
		sel.GetJoinType(),
	)
	if err != nil {
		call.Fail(err)
		return
	}
	call.Yield(newSel)
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
	if err != nil || flowed == nil || values.FlowedExactType(flowed) == nil {
		return false
	}
	return values.FlowedRowShapeEquals(flowed, resultValue.Type())
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
	return values.FlowedRowShapesAgree(fromFlowed, toFlowed)
}

// pushPredicateToExpression is the Go equivalent of Java's PushToVisitor.
// It visits the child expression and returns a new expression with the
// predicates pushed in, or nil if the expression type doesn't support
// predicate push-down.
func pushPredicateToExpression(
	call pushDownMemoizer,
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
	// An OUTER-join child is opaque to predicate absorption, except that a
	// LEFT join under a predicate rejecting its null-extended row is an inner
	// join (outer join simplification). Java dissolves every outer join in
	// REWRITING and EliminateNullOnEmptyRule drops the null-on-empty after the
	// push; the LEFT select Go keeps past REWRITING is one RewriteOuterJoinRule
	// declines (a scalar subquery's strict edge), so the push decides it here.
	joinType := selectExpr.GetJoinType()
	if !selectExpr.ChildrenAsSet() {
		if joinType != expressions.JoinLeftOuter || len(selectExpr.GetQuantifiers()) != 2 {
			return nil, nil
		}
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
	rejectsNullSupplied := false
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
		if joinType == expressions.JoinLeftOuter && !rejectsNullSupplied {
			rejects, err := rejectsNull(translated, selectExpr.GetQuantifiers()[1].GetAlias())
			if err != nil {
				return nil, err
			}
			rejectsNullSupplied = rejects
		}
		newPredicates = append(newPredicates, translated)
	}
	if joinType == expressions.JoinLeftOuter {
		if !rejectsNullSupplied {
			return nil, nil
		}
		joinType = expressions.JoinInner
	}

	return expressions.NewSelectExpressionWithJoinType(
		selectExpr.GetResultValue(),
		selectExpr.GetQuantifiers(),
		newPredicates,
		selectExpr.GetSourceAliases(),
		joinType,
	)
}

// pushOverChild creates a new SelectExpression wrapping the child
// quantifier with the pushed-down predicates. Used when pushing through
// expressions that don't directly absorb predicates. Returns a new
// ForEach quantifier ranging over the new SelectExpression.
// Ports Java's PushToVisitor.pushOverChild.
func pushOverChild(
	call pushDownMemoizer,
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	child expressions.Quantifier,
) (expressions.Quantifier, bool, error) {
	// Same precondition as pushIntoLogicalFilter's rebase: ordinals survive the
	// alias change untouched, so the two aliases must name the same row.
	//
	// THE DECLINE IS ITS OWN RESULT, not a zero Quantifier. Returning
	// (Quantifier{}, nil) made "do not push this predicate" -- a lawful,
	// expected answer -- indistinguishable from success at every caller, since
	// they all tested only the error. The zero value then reached an expression
	// constructor, RequireFlowedObjectValue rejected a quantifier with no
	// Reference, and the rule call FAILED THE WHOLE PLANNING RUN over a
	// predicate it merely should not have pushed.
	if !rebasedAliasesDenoteOneRow(pushQuantifier, child) {
		return expressions.Quantifier{}, false, nil
	}
	// Rebase: pushQuantifier.alias -> child.alias
	aliasMap, err := values.NewAliasMap([]values.AliasPair{{
		Source: pushQuantifier.GetAlias(),
		Target: child.GetAlias(),
	}})
	if err != nil {
		return expressions.Quantifier{}, false, err
	}

	newPredicates := make([]predicates.QueryPredicate, len(originalPredicates))
	for i, p := range originalPredicates {
		// CHECKED — see the sibling loop above for why a nil element here is a
		// silently dropped predicate rather than a reported failure.
		rebased, rerr := predicates.RebasePredicateChecked(p, aliasMap)
		if rerr != nil {
			return expressions.Quantifier{}, false, rerr
		}
		newPredicates[i] = rebased
	}

	flowed, err := child.RequireFlowedObjectValue()
	if err != nil {
		return expressions.Quantifier{}, false, err
	}
	newSelect, err := expressions.NewSelectExpression(
		flowed,
		[]expressions.Quantifier{child},
		newPredicates,
	)
	if err != nil {
		return expressions.Quantifier{}, false, err
	}
	return expressions.ForEachQuantifier(call.MemoizeExpression(newSelect)), true, nil
}

// pushThroughUnion pushes predicates through a LogicalUnionExpression by
// creating a new SelectExpression over each union leg with the pushed
// predicates. Ports Java's PushToVisitor.visitLogicalUnionExpression.
func pushThroughUnion(
	call pushDownMemoizer,
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
		newChild, pushed, err := pushOverChild(call, originalPredicates, pushQuantifier, q)
		if err != nil {
			return nil, err
		}
		if !pushed {
			// One leg that cannot take the predicate means the union cannot:
			// pushing into the others only would change what the union returns.
			return nil, nil
		}
		newChildren[i] = newChild
	}
	return expressions.NewLogicalUnionExpression(newChildren)
}

// pushThroughSort pushes predicates through a LogicalSortExpression by
// creating a new SelectExpression below the sort's single child.
// Ports Java's PushToVisitor.visitLogicalSortExpression.
func pushThroughSort(
	call pushDownMemoizer,
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	sort *expressions.LogicalSortExpression,
) (expressions.RelationalExpression, error) {
	inner := sort.GetInner()
	if inner.Kind() != expressions.QuantifierForEach {
		return nil, nil
	}
	newChild, pushed, err := pushOverChild(call, originalPredicates, pushQuantifier, inner)
	if err != nil {
		return nil, err
	}
	if !pushed {
		return nil, nil
	}
	return expressions.NewLogicalSortExpression(sort.GetSortKeys(), newChild)
}

// pushThroughDistinct pushes predicates through a LogicalDistinctExpression
// by creating a new SelectExpression below the distinct's single child.
// Ports Java's PushToVisitor.visitLogicalDistinctExpression.
func pushThroughDistinct(
	call pushDownMemoizer,
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	distinct *expressions.LogicalDistinctExpression,
) (expressions.RelationalExpression, error) {
	inner := distinct.GetInner()
	if inner.Kind() != expressions.QuantifierForEach {
		return nil, nil
	}
	newChild, pushed, err := pushOverChild(call, originalPredicates, pushQuantifier, inner)
	if err != nil {
		return nil, err
	}
	if !pushed {
		return nil, nil
	}
	return expressions.NewLogicalDistinctExpression(newChild)
}

// pushThroughUnique pushes predicates through a LogicalUniqueExpression
// by creating a new SelectExpression below the unique's single child.
// Ports Java's PushToVisitor.visitLogicalUniqueExpression.
func pushThroughUnique(
	call pushDownMemoizer,
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	unique *expressions.LogicalUniqueExpression,
) (expressions.RelationalExpression, error) {
	inner := unique.GetInner()
	if inner.Kind() != expressions.QuantifierForEach {
		return nil, nil
	}
	newChild, pushed, err := pushOverChild(call, originalPredicates, pushQuantifier, inner)
	if err != nil {
		return nil, err
	}
	if !pushed {
		return nil, nil
	}
	return unique.WithQuantifiers([]expressions.Quantifier{newChild})
}

var _ ImplementationRule = (*PredicatePushDownRule)(nil)
