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
	if len(quantifiers) == 0 {
		return
	}

	// Identify values box quantifiers and build per-alias maps.
	type valuesBoxInfo struct {
		idx              int
		alias            values.CorrelationIdentifier
		resultValue      values.Value
		quantifierToPush expressions.Quantifier
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
			if len(childSel.GetPredicates()) > 0 {
				continue
			}
			if !isRangeOneQuantifier(childQs[0]) {
				continue
			}
			rv := childSel.GetResultValue()
			if rv == nil {
				continue
			}
			childAlias := childQs[0].GetAlias()
			correlated := values.GetCorrelatedToOfValue(rv)
			if _, refsChild := correlated[childAlias]; refsChild {
				continue
			}
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
				idx:              i,
				alias:            q.GetAlias(),
				resultValue:      rv,
				quantifierToPush: q,
			})
			break
		}
	}

	if len(valuesBoxes) == 0 {
		return
	}

	// Build TranslationMap and qunsToPushDown map.
	tmBuilder := NewTranslationMapBuilder()
	valuesBoxIdxSet := map[int]bool{}
	qunsToPushDown := make(map[values.CorrelationIdentifier]expressions.Quantifier, len(valuesBoxes))

	for _, vb := range valuesBoxes {
		valuesBoxIdxSet[vb.idx] = true
		capturedResult := vb.resultValue
		tmBuilder.When(vb.alias).Then(func(_ values.CorrelationIdentifier, _ values.LeafValue) values.Value {
			return capturedResult
		})
		qunsToPushDown[vb.alias] = vb.quantifierToPush
	}
	tm := tmBuilder.Build()

	// Translate result value and predicates.
	newResultValue := sel.GetResultValue()
	if newResultValue != nil {
		newResultValue = translateValueCorrelations(newResultValue, tm)
	}
	newPredicates := make([]predicates.QueryPredicate, len(sel.GetPredicates()))
	for i, p := range sel.GetPredicates() {
		newPredicates[i] = translatePredicateCorrelations(p, tm)
	}

	// Push values boxes into child quantifiers and build the new
	// quantifier list.
	pushDownAliasSet := make(map[values.CorrelationIdentifier]struct{}, len(qunsToPushDown))
	for alias := range qunsToPushDown {
		pushDownAliasSet[alias] = struct{}{}
	}

	newQuantifiers := make([]expressions.Quantifier, 0, max(1, len(quantifiers)-len(valuesBoxes)))
	if len(quantifiers) == len(valuesBoxes) {
		// All quantifiers are values boxes. Introduce a range(1)
		// placeholder to avoid creating a Select with no children.
		rangeOneExpr := expressions.NewTableFunctionExpression(
			values.NewRangeValue(
				&values.ConstantValue{Value: int64(0)},
				&values.ConstantValue{Value: int64(1)},
				&values.ConstantValue{Value: int64(1)},
			),
		)
		rangeRef := call.MemoizeExpression(rangeOneExpr)
		rangeQ := expressions.ForEachQuantifier(rangeRef)
		newQuantifiers = append(newQuantifiers, rangeQ)
	}

	for i, q := range quantifiers {
		if valuesBoxIdxSet[i] {
			continue
		}

		childRef := q.GetRangesOver()
		if childRef == nil {
			newQuantifiers = append(newQuantifiers, q)
			continue
		}

		anyChanged := false
		var newMembers []expressions.RelationalExpression
		for _, member := range childRef.AllMembers() {
			lowerCorrelatedTo := expressionCorrelatedToAliases(member, pushDownAliasSet)
			if len(lowerCorrelatedTo) == 0 {
				newMembers = append(newMembers, member)
			} else {
				pushed := pushValuesIntoExpression(member, qunsToPushDown, tm, call)
				newMembers = append(newMembers, pushed)
				anyChanged = true
			}
		}
		if anyChanged {
			newRef := call.MemoizeExpression(newMembers[0])
			for _, extra := range newMembers[1:] {
				newRef.Insert(extra)
			}
			rebuilt := expressions.RebuildQuantifier(q, newRef)
			newQuantifiers = append(newQuantifiers, rebuilt)
		} else {
			newQuantifiers = append(newQuantifiers, q)
		}
	}

	// Preserve source aliases, dropping entries for removed values boxes.
	oldAliases := sel.GetSourceAliases()
	var newAliases []string
	if len(oldAliases) > 0 {
		for i := range quantifiers {
			if valuesBoxIdxSet[i] {
				continue
			}
			if i < len(oldAliases) {
				newAliases = append(newAliases, oldAliases[i])
			}
		}
	}

	merged := expressions.NewSelectExpressionWithJoinType(newResultValue, newQuantifiers, newPredicates, newAliases, sel.GetJoinType())
	call.Yield(merged)
}

// expressionCorrelatedToAliases returns the subset of aliases that the
// given expression (including its transitive children) is correlated to.
func expressionCorrelatedToAliases(
	expr expressions.RelationalExpression,
	aliases map[values.CorrelationIdentifier]struct{},
) map[values.CorrelationIdentifier]struct{} {
	allCorr := make(map[values.CorrelationIdentifier]struct{})
	expressionCorrelationSet(expr, allCorr)
	result := make(map[values.CorrelationIdentifier]struct{})
	for alias := range aliases {
		if _, found := allCorr[alias]; found {
			result[alias] = struct{}{}
		}
	}
	return result
}

// pushValuesIntoExpression rewrites a child expression by pushing
// values box quantifiers into it. Mirrors Java's PushValuesIntoVisitor.
func pushValuesIntoExpression(
	expr expressions.RelationalExpression,
	qunsToPushDown map[values.CorrelationIdentifier]expressions.Quantifier,
	tm TranslationMap,
	call *ExpressionRuleCall,
) expressions.RelationalExpression {
	switch e := expr.(type) {
	case *expressions.SelectExpression:
		return selectWithQuantifiersPushed(
			e,
			qunsToPushDown,
			e.GetResultValue(),
			e.GetQuantifiers(),
			e.GetPredicates(),
			e.GetSourceAliases(),
			e.GetJoinType(),
		)
	case *expressions.LogicalFilterExpression:
		return selectWithQuantifiersPushed(
			e,
			qunsToPushDown,
			e.GetResultValue(),
			e.GetQuantifiers(),
			e.GetPredicates(),
			nil,
			expressions.JoinInner,
		)
	default:
		return pushValuesDefault(expr, qunsToPushDown, tm, call)
	}
}

// selectWithQuantifiersPushed prepends the relevant values box
// quantifiers into a SelectExpression or LogicalFilterExpression,
// returning a new SelectExpression. Only pushes values boxes that
// the expression is actually correlated to.
func selectWithQuantifiersPushed(
	expr expressions.RelationalExpression,
	qunsToPushDown map[values.CorrelationIdentifier]expressions.Quantifier,
	resultValue values.Value,
	quantifierBase []expressions.Quantifier,
	preds []predicates.QueryPredicate,
	sourceAliases []string,
	joinType expressions.JoinType,
) *expressions.SelectExpression {
	exprCorr := make(map[values.CorrelationIdentifier]struct{})
	expressionCorrelationSet(expr, exprCorr)

	newQs := make([]expressions.Quantifier, 0, len(quantifierBase)+len(qunsToPushDown))
	for _, q := range qunsToPushDown {
		if _, found := exprCorr[q.GetAlias()]; found {
			newQs = append(newQs, q)
		}
	}
	newQs = append(newQs, quantifierBase...)
	return expressions.NewSelectExpressionWithJoinType(resultValue, newQs, preds, sourceAliases, joinType)
}

// pushValuesDefault handles the visitDefault case: for each child
// quantifier that is correlated to a values box, wrap it in a new
// SelectExpression with the relevant values boxes prepended. Then
// rebuild the expression with the new quantifiers and translated
// correlations.
func pushValuesDefault(
	expr expressions.RelationalExpression,
	qunsToPushDown map[values.CorrelationIdentifier]expressions.Quantifier,
	tm TranslationMap,
	call *ExpressionRuleCall,
) expressions.RelationalExpression {
	pushDownAliasSet := make(map[values.CorrelationIdentifier]struct{}, len(qunsToPushDown))
	for alias := range qunsToPushDown {
		pushDownAliasSet[alias] = struct{}{}
	}

	origQs := expr.GetQuantifiers()
	newQs := make([]expressions.Quantifier, 0, len(origQs))
	for _, childQ := range origQs {
		newQs = append(newQs, pushOnTopOfQuantifier(childQ, qunsToPushDown, pushDownAliasSet, call))
	}

	// Rebuild the expression with new quantifiers. For expression types
	// that carry correlation-bearing node information (result values,
	// predicates, grouping keys, etc.), translateCorrelations would also
	// need to be applied. WithQuantifiers preserves node information
	// (predicates, result values) unchanged — correlations in those
	// fields still reference the values box aliases, which is correct
	// because the values boxes have been pushed into the children where
	// those aliases are now locally available.
	return expr.WithQuantifiers(newQs)
}

// pushOnTopOfQuantifier wraps a child quantifier in a SelectExpression
// with values boxes prepended, if the quantifier is correlated to any
// values box. Returns the original quantifier if uncorrelated.
func pushOnTopOfQuantifier(
	childQ expressions.Quantifier,
	qunsToPushDown map[values.CorrelationIdentifier]expressions.Quantifier,
	pushDownAliasSet map[values.CorrelationIdentifier]struct{},
	call *ExpressionRuleCall,
) expressions.Quantifier {
	childCorr := quantifierCorrelationSetLocal(childQ)
	hasCorrelation := false
	for alias := range pushDownAliasSet {
		if _, found := childCorr[alias]; found {
			hasCorrelation = true
			break
		}
	}
	if !hasCorrelation {
		return childQ
	}

	newChild := expressions.ForEachQuantifier(childQ.GetRangesOver())

	exprCorr := make(map[values.CorrelationIdentifier]struct{})
	ref := newChild.GetRangesOver()
	if ref != nil {
		for _, m := range ref.AllMembers() {
			expressionCorrelationSet(m, exprCorr)
		}
	}

	newQs := make([]expressions.Quantifier, 0, 1+len(qunsToPushDown))
	for _, q := range qunsToPushDown {
		if _, found := exprCorr[q.GetAlias()]; found {
			newQs = append(newQs, q)
		}
	}
	newQs = append(newQs, newChild)

	newSelect := expressions.NewSelectExpression(
		newChild.GetFlowedObjectValue(),
		newQs,
		nil,
	)
	newRef := call.MemoizeExpression(newSelect)
	return expressions.RebuildQuantifier(childQ, newRef)
}

// quantifierCorrelationSetLocal computes the correlation set of a
// quantifier by walking the inner expression tree.
func quantifierCorrelationSetLocal(q expressions.Quantifier) map[values.CorrelationIdentifier]struct{} {
	ref := q.GetRangesOver()
	if ref == nil {
		return map[values.CorrelationIdentifier]struct{}{}
	}
	result := map[values.CorrelationIdentifier]struct{}{}
	for _, member := range ref.AllMembers() {
		expressionCorrelationSet(member, result)
	}
	return result
}

// translateValueCorrelations applies the TranslationMap to a Value
// tree by replacing QuantifiedObjectValue leaves whose correlation is
// mapped in the TranslationMap.
func translateValueCorrelations(v values.Value, tm TranslationMap) values.Value {
	if v == nil {
		return nil
	}
	// Replace all correlation-bearing LEAF values whose alias is in the
	// translation map. Ports Java's Value.translateCorrelations, which uses
	// replaceLeavesMaybe(op, visitNewLeaves=false): a leaf replaced with a value
	// that ITSELF references the same alias must NOT be re-translated (the
	// self-referential cycle — the source-anchored join RC anchors its right-leg
	// columns to QOV(B) while the parent quantifier over the join is also aliased
	// B; re-descending the substituted RC would re-match B forever). Use
	// ReplaceLeavesOnceMaybe so substituted leaves are skipped on the re-descent.
	return values.ReplaceLeavesOnceMaybe(v, func(val values.Value) values.Value {
		var alias values.CorrelationIdentifier
		switch n := val.(type) {
		case *values.QuantifiedObjectValue:
			alias = n.Correlation
		case *values.QuantifiedRecordValue:
			alias = n.Alias
		// ExistsValue is a transparent composite (RFC-141), not a leaf —
		// its child QuantifiedObjectValue is the leaf that gets translated
		// by the *QuantifiedObjectValue case above.
		case *values.ScalarSubqueryValue:
			alias = n.Alias
		case *values.ObjectValue:
			alias = n.Alias
		default:
			return val
		}
		if !tm.ContainsSourceAlias(alias) {
			return val
		}
		lv, ok := val.(values.LeafValue)
		if !ok {
			return val
		}
		return tm.ApplyTranslationFunction(alias, lv)
	})
}

// translatePredicateCorrelations applies the TranslationMap to a
// predicate by walking its Value trees and replacing mapped aliases.
func translatePredicateCorrelations(p predicates.QueryPredicate, tm TranslationMap) predicates.QueryPredicate {
	if p == nil {
		return nil
	}
	switch pred := p.(type) {
	case *predicates.ComparisonPredicate:
		newOperand := translateValueCorrelations(pred.Operand, tm)
		newCompOperand := translateValueCorrelations(pred.Comparison.Operand, tm)
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
		newVal := translateValueCorrelations(pred.Value, tm)
		if newVal == pred.Value {
			return p
		}
		return predicates.NewValuePredicate(newVal)
	case *predicates.AndPredicate:
		changed := false
		newSubs := make([]predicates.QueryPredicate, len(pred.SubPredicates))
		for i, s := range pred.SubPredicates {
			newSubs[i] = translatePredicateCorrelations(s, tm)
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
			newSubs[i] = translatePredicateCorrelations(s, tm)
			if newSubs[i] != s {
				changed = true
			}
		}
		if !changed {
			return p
		}
		return predicates.NewOr(newSubs...)
	case *predicates.NotPredicate:
		newChild := translatePredicateCorrelations(pred.Child, tm)
		if newChild == pred.Child {
			return p
		}
		return predicates.NewNot(newChild)
	case *predicates.ExistentialValuePredicate:
		// RFC-141: translate the QuantifiedObjectValue operand's correlation
		// via the shared value path (which remaps the QOV alias). The
		// comparison (NOT_NULL) is carried unchanged.
		newVal := translateValueCorrelations(pred.Value, tm)
		if newVal == pred.Value {
			return p
		}
		return predicates.NewExistentialValuePredicate(newVal, pred.Comparison)
	case *predicates.Placeholder:
		newVal := translateValueCorrelations(pred.Value, tm)
		newAlias := pred.ParameterAlias
		if tm.ContainsSourceAlias(newAlias) {
			if target, ok := tm.GetTargetAlias(newAlias); ok {
				newAlias = target
			}
		}
		if newVal == pred.Value && newAlias == pred.ParameterAlias {
			return p
		}
		return &predicates.Placeholder{
			ParameterAlias: newAlias,
			Value:          newVal,
			CompRange:      pred.CompRange,
		}
	case *predicates.ConstantPredicate:
		return p
	default:
		return p
	}
}

// isRangeOneQuantifier checks if a quantifier ranges over a Reference
// containing a TableFunctionExpression with cardinality exactly 1.
// Mirrors Java's rangeOneMatcher which checks
// `CardinalitiesProperty.Cardinalities.exactlyOne()`.
func isRangeOneQuantifier(q expressions.Quantifier) bool {
	ref := q.GetRangesOver()
	if ref == nil {
		return false
	}
	for _, m := range ref.AllMembers() {
		tfe, ok := m.(*expressions.TableFunctionExpression)
		if !ok {
			continue
		}
		rv, ok := tfe.GetValue().(*values.RangeValue)
		if !ok {
			continue
		}
		begin, ok := rv.BeginInclusive.(*values.ConstantValue)
		if !ok {
			continue
		}
		end, ok := rv.EndExclusive.(*values.ConstantValue)
		if !ok {
			continue
		}
		step, ok := rv.Step.(*values.ConstantValue)
		if !ok {
			continue
		}
		b, bOk := toInt64(begin.Value)
		e, eOk := toInt64(end.Value)
		s, sOk := toInt64(step.Value)
		if !bOk || !eOk || !sOk || s <= 0 {
			continue
		}
		rows := (e - b + s - 1) / s
		if rows == 1 {
			return true
		}
	}
	return false
}

func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int32:
		return int64(x), true
	case int:
		return int64(x), true
	default:
		return 0, false
	}
}

var _ ExpressionRule = (*DecorrelateValuesRule)(nil)
