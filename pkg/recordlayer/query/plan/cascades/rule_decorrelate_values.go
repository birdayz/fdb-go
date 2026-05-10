package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// DecorrelateValuesRule is an exploration rule that inlines "values
// boxes" into the parent SelectExpression. A values box is a
// quantifier over a SelectExpression whose result value is constant
// (uncorrelated to its own child quantifier) — typically produced by
// parameterized function inlining or constant subqueries.
//
// Pattern:
//
//	SelectExpression(
//	  ForEach(alias=A) → Ref[SelectExpression(constResult, [ForEach → valuesSource], [])],
//	  ForEach(alias=B) → Ref[other],
//	  predicates referencing A
//	)
//
// Rewrite: replace references to alias A with the constant result
// value from the values box, remove the values box quantifier.
//
//	SelectExpression(
//	  ForEach(alias=B) → Ref[other],
//	  predicates with A→constResult substitution
//	)
//
// Ports Java's DecorrelateValuesRule (ExplorationCascadesRule).
type DecorrelateValuesRule struct {
	matcher matching.BindingMatcher
}

func NewDecorrelateValuesRule() *DecorrelateValuesRule {
	return &DecorrelateValuesRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("decorrelate_values"),
	}
}

func (r *DecorrelateValuesRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *DecorrelateValuesRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	quantifiers := sel.GetQuantifiers()
	if len(quantifiers) < 2 {
		return
	}

	// Identify values box quantifiers: ForEach quantifiers whose child
	// Reference holds a SelectExpression with an uncorrelated result
	// value (constant w.r.t. its own child).
	type valuesBoxInfo struct {
		idx         int
		alias       values.CorrelationIdentifier
		resultValue values.Value
	}
	var valuesBoxes []valuesBoxInfo

	for i, q := range quantifiers {
		if q.Kind() != expressions.QuantifierForEach {
			continue
		}
		childRef := q.GetRangesOver()
		if childRef == nil {
			continue
		}
		for _, member := range childRef.AllMembers() {
			childSel, ok := member.(*expressions.SelectExpression)
			if !ok {
				continue
			}
			childQs := childSel.GetQuantifiers()
			if len(childQs) != 1 {
				continue
			}
			rv := childSel.GetResultValue()
			if rv == nil {
				continue
			}
			// Check that the result value is uncorrelated to the child's
			// own quantifier — i.e., it's a constant expression.
			childAlias := childQs[0].GetAlias()
			correlated := values.GetCorrelatedToOfValue(rv)
			if _, refsChild := correlated[childAlias]; refsChild {
				continue
			}
			// Check for sideways correlations: the values box must not
			// reference any sibling quantifier.
			hasSidewaysCorrelation := false
			for j, siblingQ := range quantifiers {
				if j == i {
					continue
				}
				if _, refs := correlated[siblingQ.GetAlias()]; refs {
					hasSidewaysCorrelation = true
					break
				}
			}
			if hasSidewaysCorrelation {
				continue
			}
			valuesBoxes = append(valuesBoxes, valuesBoxInfo{
				idx:         i,
				alias:       q.GetAlias(),
				resultValue: rv,
			})
			break
		}
	}

	if len(valuesBoxes) == 0 {
		return
	}

	// Build TranslationMap: each values box alias → its result value.
	tmBuilder := NewTranslationMapBuilder()
	valuesBoxIdxSet := map[int]bool{}
	aliasMap := values.AliasMap{}

	for _, vb := range valuesBoxes {
		valuesBoxIdxSet[vb.idx] = true
		capturedResult := vb.resultValue
		tmBuilder.When(vb.alias).Then(func(_ values.CorrelationIdentifier, _ values.LeafValue) values.Value {
			return capturedResult
		})
	}
	tm := tmBuilder.Build()

	// Build new quantifier list (excluding values boxes).
	newQuantifiers := make([]expressions.Quantifier, 0, len(quantifiers)-len(valuesBoxes))
	for i, q := range quantifiers {
		if !valuesBoxIdxSet[i] {
			newQuantifiers = append(newQuantifiers, q)
		}
	}

	if len(newQuantifiers) == 0 {
		return
	}

	// Translate result value: replace values box alias references with
	// the constant result values.
	newResultValue := sel.GetResultValue()
	if newResultValue != nil {
		newResultValue = translateValueCorrelations(newResultValue, tm, aliasMap)
	}

	// Translate predicates.
	newPredicates := make([]predicates.QueryPredicate, len(sel.GetPredicates()))
	for i, p := range sel.GetPredicates() {
		newPredicates[i] = translatePredicateCorrelations(p, tm, aliasMap)
	}

	// Rebuild source aliases.
	var newAliases []string
	if srcAliases := sel.GetSourceAliases(); len(srcAliases) > 0 {
		for i := range quantifiers {
			if !valuesBoxIdxSet[i] && i < len(srcAliases) {
				newAliases = append(newAliases, srcAliases[i])
			}
		}
	}

	var merged *expressions.SelectExpression
	if len(newAliases) > 0 {
		merged = expressions.NewSelectExpressionWithJoinType(
			newResultValue, newQuantifiers, newPredicates, newAliases, sel.GetJoinType(),
		)
	} else {
		merged = expressions.NewSelectExpressionWithJoinType(
			newResultValue, newQuantifiers, newPredicates, nil, sel.GetJoinType(),
		)
	}
	call.Yield(merged)
}

// translateValueCorrelations applies the TranslationMap to a Value
// tree by replacing QuantifiedObjectValue leaves whose correlation is
// mapped in the TranslationMap.
func translateValueCorrelations(v values.Value, tm TranslationMap, aliasMap values.AliasMap) values.Value {
	if v == nil {
		return nil
	}
	// Try alias-based rebase first.
	if len(aliasMap) > 0 {
		v = values.RebaseValue(v, aliasMap)
	}
	// Replace QOV leaves that reference translated aliases.
	return values.Replace(v, func(val values.Value) values.Value {
		qov, ok := val.(*values.QuantifiedObjectValue)
		if !ok {
			return val
		}
		if !tm.ContainsSourceAlias(qov.Correlation) {
			return val
		}
		lv, ok := val.(values.LeafValue)
		if !ok {
			return val
		}
		return tm.ApplyTranslationFunction(qov.Correlation, lv)
	})
}

// translatePredicateCorrelations applies the TranslationMap to a
// predicate by walking its Value trees and replacing mapped aliases.
func translatePredicateCorrelations(p predicates.QueryPredicate, tm TranslationMap, aliasMap values.AliasMap) predicates.QueryPredicate {
	if p == nil {
		return nil
	}
	switch pred := p.(type) {
	case *predicates.ComparisonPredicate:
		newOperand := translateValueCorrelations(pred.Operand, tm, aliasMap)
		newCompOperand := translateValueCorrelations(pred.Comparison.Operand, tm, aliasMap)
		if newOperand == pred.Operand && newCompOperand == pred.Comparison.Operand {
			return p
		}
		return &predicates.ComparisonPredicate{
			Operand: newOperand,
			Comparison: predicates.Comparison{
				Type:    pred.Comparison.Type,
				Operand: newCompOperand,
				Escape:  pred.Comparison.Escape,
			},
		}
	case *predicates.ValuePredicate:
		newVal := translateValueCorrelations(pred.Value, tm, aliasMap)
		if newVal == pred.Value {
			return p
		}
		return predicates.NewValuePredicate(newVal)
	case *predicates.AndPredicate:
		changed := false
		newSubs := make([]predicates.QueryPredicate, len(pred.SubPredicates))
		for i, s := range pred.SubPredicates {
			newSubs[i] = translatePredicateCorrelations(s, tm, aliasMap)
			if newSubs[i] != s {
				changed = true
			}
		}
		if !changed {
			return p
		}
		return predicates.NewAnd(newSubs...)
	case *predicates.OrPredicate:
		changed := false
		newSubs := make([]predicates.QueryPredicate, len(pred.SubPredicates))
		for i, s := range pred.SubPredicates {
			newSubs[i] = translatePredicateCorrelations(s, tm, aliasMap)
			if newSubs[i] != s {
				changed = true
			}
		}
		if !changed {
			return p
		}
		return predicates.NewOr(newSubs...)
	case *predicates.NotPredicate:
		newChild := translatePredicateCorrelations(pred.Child, tm, aliasMap)
		if newChild == pred.Child {
			return p
		}
		return predicates.NewNot(newChild)
	default:
		return p
	}
}

var _ ExpressionRule = (*DecorrelateValuesRule)(nil)
