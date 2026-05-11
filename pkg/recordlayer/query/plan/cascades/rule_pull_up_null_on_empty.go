package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
)

// PullUpNullOnEmptyRule splits a SelectExpression with a null-on-empty
// quantifier into two parts:
//
//  1. A lower SelectExpression with the same predicates but a normal
//     (non-null-on-empty) ForEach quantifier. This shape has a better
//     chance of matching an index.
//  2. An upper SelectExpression with a null-on-empty quantifier over
//     the lower, carrying the parent's result value but no predicates.
//     This reapplies NULL-on-empty semantics on top.
//
// Pattern:
//
//	SelectExpression(
//	  ForEachNullOnEmpty(alias=A) → Ref[children...],
//	  predicates
//	)
//
// Guard: exactly 1 quantifier, and it is null-on-empty. The child
// Reference's members are classified:
//   - Not a SelectExpression → DO_NOT_CARE
//   - SelectExpression with >1 quantifiers → PULL_UP
//   - SelectExpression with 1 quantifier whose alias != A → PULL_UP
//   - SelectExpression with same predicates as parent → DO_NOT_PULL_UP
//   - SelectExpression with different predicates → PULL_UP
//
// If any child is DO_NOT_PULL_UP → no rewrite. If no child is PULL_UP
// → no rewrite. Otherwise, yield the split.
//
// Ports Java's PullUpNullOnEmptyRule (ExplorationCascadesRule).
type PullUpNullOnEmptyRule struct {
	matcher matching.BindingMatcher
}

func NewPullUpNullOnEmptyRule() *PullUpNullOnEmptyRule {
	return &PullUpNullOnEmptyRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("pull_up_null_on_empty"),
	}
}

func (r *PullUpNullOnEmptyRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PullUpNullOnEmptyRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	quantifiers := sel.GetQuantifiers()

	// Must have exactly 1 quantifier that is null-on-empty.
	if len(quantifiers) != 1 {
		return
	}
	quantifier := quantifiers[0]
	if !quantifier.IsNullOnEmpty() {
		return
	}
	if quantifier.Kind() != expressions.QuantifierForEach {
		return
	}

	childRef := quantifier.GetRangesOver()
	if childRef == nil {
		return
	}

	childExpressions := childRef.AllMembers()

	// Classify each child expression.
	pullUpDesired := false
	for _, childExpr := range childExpressions {
		cls := classifyExpressionForNullOnEmpty(sel, quantifier, childExpr)
		if cls == classDoNotPullUp {
			return
		}
		if cls == classPullUp {
			pullUpDesired = true
		}
	}
	if !pullUpDesired {
		return
	}

	// Lower quantifier: same alias, same Reference, NOT null-on-empty.
	// Mirrors Java: Quantifier.forEachBuilder().withAlias(quantifier.getAlias()).build(quantifier.getRangesOver())
	newChildrenQuantifier := expressions.NamedForEachQuantifier(quantifier.GetAlias(), quantifier.GetRangesOver())

	// Lower SelectExpression: predicates from parent, result = QOV(newChildrenQuantifier).
	// Mirrors Java: GraphExpansion.builder().addQuantifier(q).addAllPredicates(preds).build().buildSimpleSelectOverQuantifier(q)
	lowerBuilder := NewGraphExpansionBuilder()
	lowerBuilder.AddQuantifier(newChildrenQuantifier)
	for _, p := range sel.GetPredicates() {
		lowerBuilder.AddPredicate(p)
	}
	lowerSealed := lowerBuilder.Build().Seal()
	lowerSelect := lowerSealed.BuildSelectWithResultValue(newChildrenQuantifier.GetFlowedObjectValue())

	// Memoize the lower select.
	memoizedLowerRef := call.MemoizeExpression(lowerSelect)

	// Upper quantifier: same alias + nullOnEmpty from original, ranges over memoized lower.
	// Mirrors Java: Quantifier.forEachBuilder().from(quantifier).build(newSelectExpression)
	topLevelQuantifier := expressions.RebuildQuantifier(quantifier, memoizedLowerRef)

	// Upper SelectExpression: no predicates, result = parent's result value.
	// Mirrors Java: GraphExpansion.builder().addQuantifier(q).build().buildSelectWithResultValue(resultValue)
	upperBuilder := NewGraphExpansionBuilder()
	upperBuilder.AddQuantifier(topLevelQuantifier)
	upperSealed := upperBuilder.Build().Seal()
	topLevelSelect := upperSealed.BuildSelectWithResultValue(sel.GetResultValue())

	call.Yield(topLevelSelect)
}

// expressionClassification is the tri-state for child expression classification.
type expressionClassification int

const (
	classDoNotCare expressionClassification = iota
	classDoNotPullUp
	classPullUp
)

// classifyExpressionForNullOnEmpty determines whether a child expression
// should trigger the null-on-empty pull-up. Mirrors Java's
// PullUpNullOnEmptyRule.classifyExpression.
func classifyExpressionForNullOnEmpty(
	selectOnTop *expressions.SelectExpression,
	quantifier expressions.Quantifier,
	expr expressions.RelationalExpression,
) expressionClassification {
	childSel, ok := expr.(*expressions.SelectExpression)
	if !ok {
		return classDoNotCare
	}

	childQuantifiers := childSel.GetQuantifiers()
	if len(childQuantifiers) > 1 {
		return classPullUp
	}
	if len(childQuantifiers) == 1 && childQuantifiers[0].GetAlias() != quantifier.GetAlias() {
		return classPullUp
	}

	// Compare predicate lists positionally (same as Java's List.equals).
	parentPreds := selectOnTop.GetPredicates()
	childPreds := childSel.GetPredicates()
	if !predicateListsEqual(parentPreds, childPreds) {
		return classPullUp
	}
	return classDoNotPullUp
}

// predicateListsEqual compares two predicate slices positionally.
func predicateListsEqual(a, b []predicates.QueryPredicate) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !predicates.PredicateEquals(a[i], b[i]) {
			return false
		}
	}
	return true
}

var _ ExpressionRule = (*PullUpNullOnEmptyRule)(nil)
