// derivations_evaluator.go — evaluates DerivationsProperty for
// physical plan wrapper expressions. Mirrors Java's
// DerivationsProperty.DerivationsVisitor.
//
// The evaluator walks the expression tree bottom-up, computing
// properties.Derivations for each node. For each wrapper type it
// computes what the result values and local values are by
// inlining child result values into the current node's value
// expressions (decorrelation).
package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ComputeDerivations evaluates the derivations property for a
// physical plan wrapper expression. Dispatches on the concrete
// wrapper type, matching Java's DerivationsVisitor per-plan methods.
//
// Exported for use by plan_properties.go and tests.
func ComputeDerivations(expr expressions.RelationalExpression) *properties.Derivations {
	if expr == nil {
		return properties.EmptyDerivations()
	}

	// Try the interface first — allows external wrappers to
	// provide custom derivation logic.
	if de, ok := expr.(properties.DerivationsEvaluable); ok {
		return de.EvaluateDerivations()
	}

	// Dispatch on wrapper type — the wrappers are unexported but
	// visible within the cascades package.
	switch w := expr.(type) {

	// --- Leaf scans ---

	case *physicalScanWrapper:
		return derivationsForScan(w.plan)

	case *physicalIndexScanWrapper:
		return derivationsForIndexScan(w.plan)

	case *scanPlanExpression:
		if sp, ok := w.plan.(*plans.RecordQueryScanPlan); ok {
			return derivationsForScan(sp)
		}
		if ip, ok := w.plan.(*plans.RecordQueryIndexPlan); ok {
			return derivationsForIndexScan(ip)
		}
		return properties.EmptyDerivations()

	// --- Single-child passthrough ---

	case *physicalFilterWrapper:
		return derivationsFromSingleChildExpr(w)

	case *physicalDistinctWrapper:
		return derivationsFromSingleChildExpr(w)

	case *physicalDeleteWrapper:
		return derivationsFromSingleChildExpr(w)

	case *physicalFetchFromPartialRecordWrapper:
		return derivationsFromSingleChildExpr(w)

	case *physicalTempTableInsertWrapper:
		return derivationsFromSingleChildExpr(w)

	case *physicalLimitWrapper:
		return derivationsFromSingleChildExpr(w)

	case *physicalInMemorySortWrapper:
		return derivationsFromSingleChildExpr(w)

	// --- PredicatesFilter: child results + predicate values ---

	case *physicalPredicatesFilterWrapper:
		return derivationsForPredicatesFilter(w)

	// --- Map: translate result value through child results ---

	case *physicalMapWrapper:
		return derivationsForMap(w)

	// --- Projection: translate through child results ---

	case *physicalProjectionWrapper:
		return derivationsFromSingleChildExpr(w)

	// --- TypeFilter: restrict QueriedValue record types ---

	case *physicalTypeFilterWrapper:
		return derivationsForTypeFilter(w)

	// --- Set operations (union, intersection) ---

	case *physicalUnionWrapper:
		// RecordQueryUnionPlan is a simple UNION ALL — no comparison keys.
		return derivationsForSetPlan(w, nil)

	case *physicalMergeSortUnionWrapper:
		return derivationsForSetPlan(w, w.plan.GetComparisonKeys())

	case *physicalUnorderedUnionWrapper:
		return derivationsForSetPlan(w, nil)

	case *physicalIntersectionWrapper:
		return derivationsForSetPlan(w, w.plan.GetComparisonKeyValues())

	case *physicalMultiIntersectionWrapper:
		return derivationsForMultiIntersection(w)

	// --- InJoin: decorrelate inner against in-source ---

	case *physicalInJoinWrapper:
		return derivationsForInJoin(w)

	// --- InUnion: decorrelate inner against multiple bindings ---

	case *physicalInUnionWrapper:
		return derivationsForInUnion(w)

	// --- FirstOrDefault / DefaultOnEmpty ---

	case *physicalFirstOrDefaultWrapper:
		return derivationsForFirstOrDefault(w)

	case *physicalDefaultOnEmptyWrapper:
		return derivationsForDefaultOnEmpty(w)

	// --- StreamingAggregation ---

	case *physicalStreamingAggWrapper:
		return derivationsForStreamingAgg(w)

	// --- Explode ---

	case *physicalExplodeWrapper:
		return derivationsForExplode(w)

	// --- TableFunction ---

	case *physicalTableFunctionWrapper:
		return derivationsForTableFunction(w)

	// --- TempTableScan ---

	case *physicalTempTableScanWrapper:
		return derivationsForTempTableScan(w)

	// --- Values ---

	case *physicalValuesWrapper:
		return derivationsForValues(w)

	// --- DML: Insert / Update ---

	case *physicalInsertWrapper:
		return derivationsFromSingleChildExpr(w)

	case *physicalUpdateWrapper:
		return derivationsFromSingleChildExpr(w)

	// --- NestedLoopJoin ---

	case *physicalNestedLoopJoinWrapper:
		return derivationsForNestedLoopJoin(w)

	// --- Recursive plans: empty derivations ---

	case *physicalRecursiveDfsJoinWrapper:
		return properties.EmptyDerivations()

	case *physicalRecursiveLevelUnionWrapper:
		return properties.EmptyDerivations()

	default:
		// Unknown expression type: generic fallback via quantifiers.
		return properties.EvaluateDerivations(expr)
	}
}

// --- Leaf scan derivations ---

func derivationsForScan(plan *plans.RecordQueryScanPlan) *properties.Derivations {
	if plan == nil {
		return properties.EmptyDerivations()
	}
	resultValue := values.NewQueriedValue(plan.GetRecordTypes(), plan.GetFlowedType())
	return &properties.Derivations{
		ResultValues: []values.Value{resultValue},
	}
}

func derivationsForIndexScan(plan *plans.RecordQueryIndexPlan) *properties.Derivations {
	if plan == nil {
		return properties.EmptyDerivations()
	}
	// Mirrors Java's visitIndexPlan: QueriedValue with the index's
	// record types as result, scan comparison values as locals.
	resultValue := values.NewQueriedValue(plan.GetRecordTypes(), plan.GetFlowedType())
	var localVals []values.Value
	for _, cr := range plan.GetScanComparisons() {
		if cr == nil {
			continue
		}
		// Extract comparison operand values from the range.
		if cr.IsEquality() {
			eq := cr.GetEqualityComparison()
			if eq != nil && eq.Operand != nil {
				localVals = append(localVals, eq.Operand)
			}
		} else if cr.IsInequality() {
			for _, ineq := range cr.GetInequalityComparisons() {
				if ineq != nil && ineq.Operand != nil {
					localVals = append(localVals, ineq.Operand)
				}
			}
		}
	}
	return &properties.Derivations{
		ResultValues: []values.Value{resultValue},
		LocalValues:  localVals,
	}
}

// --- Single-child passthrough ---

func derivationsFromSingleChildExpr(expr expressions.RelationalExpression) *properties.Derivations {
	return properties.DerivationsFromSingleChild(expr)
}

// --- PredicatesFilter ---

func derivationsForPredicatesFilter(w *physicalPredicatesFilterWrapper) *properties.Derivations {
	childDerivs := properties.DerivationsFromQuantifier(w.innerQuant)
	childResults := childDerivs.ResultValues

	var localVals []values.Value
	localVals = append(localVals, childDerivs.LocalValues...)

	alias := w.innerQuant.GetAlias()
	for _, pred := range w.plan.GetPredicates() {
		predValues := collectValuesFromPredicate(pred)
		for _, childResult := range childResults {
			for _, pv := range predValues {
				translated := properties.TranslateCorrelation(pv, alias, childResult)
				localVals = append(localVals, translated)
			}
		}
	}

	return &properties.Derivations{
		ResultValues: childResults,
		LocalValues:  localVals,
	}
}

// collectValuesFromPredicate extracts Value trees from a predicate.
// Mirrors Java's valuesInPredicate + combineValuesInChildren fold.
func collectValuesFromPredicate(pred predicates.QueryPredicate) []values.Value {
	var result []values.Value
	collectValuesFromPredicateRecursive(pred, &result)
	return result
}

func collectValuesFromPredicateRecursive(pred predicates.QueryPredicate, out *[]values.Value) {
	// Extract values from this predicate node.
	switch p := pred.(type) {
	case *predicates.ValuePredicate:
		if p.Value != nil {
			*out = append(*out, p.Value)
		}
	case *predicates.ComparisonPredicate:
		if p.Operand != nil {
			*out = append(*out, p.Operand)
		}
		if p.Comparison.Operand != nil {
			*out = append(*out, p.Comparison.Operand)
		}
	}
	// Recurse into children.
	for _, child := range pred.Children() {
		collectValuesFromPredicateRecursive(child, out)
	}
}

// --- Map ---

func derivationsForMap(w *physicalMapWrapper) *properties.Derivations {
	childDerivs := properties.DerivationsFromQuantifier(w.innerQuant)
	childResults := childDerivs.ResultValues
	resultValue := w.plan.GetResultValue()

	var resultVals []values.Value
	var localVals []values.Value
	localVals = append(localVals, childDerivs.LocalValues...)

	alias := w.innerQuant.GetAlias()
	for _, childResult := range childResults {
		translated := properties.TranslateCorrelation(resultValue, alias, childResult)
		resultVals = append(resultVals, translated)
	}

	return &properties.Derivations{
		ResultValues: resultVals,
		LocalValues:  localVals,
	}
}

// --- TypeFilter ---

func derivationsForTypeFilter(w *physicalTypeFilterWrapper) *properties.Derivations {
	childDerivs := properties.DerivationsFromSingleChild(w)
	childResults := childDerivs.ResultValues

	filteredTypes := make(map[string]struct{})
	for _, rt := range w.plan.GetRecordTypes() {
		filteredTypes[rt] = struct{}{}
	}

	var resultVals []values.Value
	for _, childResult := range childResults {
		replaced := values.ReplaceLeavesMaybe(childResult, func(leaf values.Value) values.Value {
			qv, ok := leaf.(*values.QueriedValue)
			if !ok {
				return leaf
			}
			if len(qv.RecordTypes) == 0 {
				return leaf
			}
			var intersected []string
			for _, rt := range qv.RecordTypes {
				if _, ok := filteredTypes[rt]; ok {
					intersected = append(intersected, rt)
				}
			}
			return values.NewQueriedValue(intersected, w.plan.GetResultType())
		})
		resultVals = append(resultVals, replaced)
	}

	return &properties.Derivations{
		ResultValues: resultVals,
		LocalValues:  childDerivs.LocalValues,
	}
}

// --- Set operations (Union, Intersection, UnorderedUnion) ---

func derivationsForSetPlan(expr expressions.RelationalExpression, comparisonKeyValues []values.Value) *properties.Derivations {
	var resultVals []values.Value
	var localVals []values.Value

	for _, q := range expr.GetQuantifiers() {
		childDerivs := properties.DerivationsFromQuantifier(q)
		resultVals = append(resultVals, childDerivs.ResultValues...)
		localVals = append(localVals, childDerivs.LocalValues...)
	}

	// Translate comparison key values through each result value.
	// Java's derivationsFromComparisonKeyValues: for each comparison
	// key, translate Quantifier.current() → resultValue.
	if len(comparisonKeyValues) > 0 {
		for _, ckv := range comparisonKeyValues {
			for _, rv := range resultVals {
				translated := properties.TranslateCorrelation(ckv, values.CurrentAlias, rv)
				localVals = append(localVals, translated)
			}
		}
	}

	return &properties.Derivations{
		ResultValues: resultVals,
		LocalValues:  localVals,
	}
}

// --- MultiIntersection ---

func derivationsForMultiIntersection(w *physicalMultiIntersectionWrapper) *properties.Derivations {
	intersectionResultValue := w.plan.GetResultValue()
	var resultVals []values.Value
	var localVals []values.Value

	// Collect per-child result derivations.
	quantifiers := w.GetQuantifiers()
	childResultLists := make([][]values.Value, len(quantifiers))
	for i, q := range quantifiers {
		childDerivs := properties.DerivationsFromQuantifier(q)
		childResultLists[i] = childDerivs.ResultValues
		localVals = append(localVals, childDerivs.LocalValues...)
	}

	// Cross-product of child result values, translating the
	// intersection result value through each combination.
	crossProduct(childResultLists, func(combo []values.Value) {
		aliasMap := make(map[values.CorrelationIdentifier]values.Value)
		for i, q := range quantifiers {
			aliasMap[q.GetAlias()] = combo[i]
		}
		translated := properties.TranslateCorrelations(intersectionResultValue, aliasMap)
		resultVals = append(resultVals, translated)
	})

	// Comparison key values.
	compKeys := w.plan.GetComparisonKey()
	if len(compKeys) > 0 {
		for _, ckv := range compKeys {
			for _, rv := range resultVals {
				translated := properties.TranslateCorrelation(ckv, values.CurrentAlias, rv)
				localVals = append(localVals, translated)
			}
		}
	}

	return &properties.Derivations{
		ResultValues: resultVals,
		LocalValues:  localVals,
	}
}

// crossProduct calls fn with every combination from the cartesian
// product of the input lists. Used by multi-intersection derivations.
func crossProduct(lists [][]values.Value, fn func(combo []values.Value)) {
	if len(lists) == 0 {
		return
	}
	combo := make([]values.Value, len(lists))
	crossProductHelper(lists, combo, 0, fn)
}

func crossProductHelper(lists [][]values.Value, combo []values.Value, depth int, fn func(combo []values.Value)) {
	if depth == len(lists) {
		fn(combo)
		return
	}
	for _, v := range lists[depth] {
		combo[depth] = v
		crossProductHelper(lists, combo, depth+1, fn)
	}
}

// --- InJoin ---

func derivationsForInJoin(w *physicalInJoinWrapper) *properties.Derivations {
	innerDerivs := properties.DerivationsFromQuantifier(w.innerQuant)

	// The outer alias is the IN-source binding name. Java uses
	// inJoinPlan.getInAlias() which returns a CorrelationIdentifier.
	// Go's InJoinPlan uses a string binding name. Convert it.
	outerAlias := values.NamedCorrelationIdentifier(w.plan.GetBindingName())

	// Decorrelate inner values against the in-source.
	localVals := decorrelateValues(innerDerivs.LocalValues, outerAlias)
	resultVals := decorrelateValues(innerDerivs.ResultValues, outerAlias)

	return &properties.Derivations{
		ResultValues: resultVals,
		LocalValues:  localVals,
	}
}

// decorrelateValues replaces references to outerAlias with a fresh
// QueriedValue of the same type, effectively removing the correlation.
// Mirrors Java's InJoin decorrelation logic.
func decorrelateValues(vals []values.Value, outerAlias values.CorrelationIdentifier) []values.Value {
	result := make([]values.Value, 0, len(vals))
	for _, v := range vals {
		if properties.IsCorrelatedTo(v, outerAlias) {
			translated := values.ReplaceLeavesMaybe(v, func(leaf values.Value) values.Value {
				qov, ok := leaf.(*values.QuantifiedObjectValue)
				if !ok {
					return leaf
				}
				if qov.Correlation == outerAlias {
					return values.NewQueriedValue(nil, leaf.Type())
				}
				return leaf
			})
			result = append(result, translated)
		} else {
			result = append(result, v)
		}
	}
	return result
}

// --- InUnion ---

func derivationsForInUnion(w *physicalInUnionWrapper) *properties.Derivations {
	innerDerivs := properties.DerivationsFromQuantifier(w.innerQuant)

	// Collect all outer aliases (binding names).
	outerAliases := make(map[values.CorrelationIdentifier]struct{})
	for _, bn := range w.plan.GetBindingNames() {
		outerAliases[values.NamedCorrelationIdentifier(bn)] = struct{}{}
	}

	// Decorrelate inner locals.
	var localVals []values.Value
	for _, v := range innerDerivs.LocalValues {
		corr := values.GetCorrelatedToOfValue(v)
		needsTranslate := false
		for alias := range outerAliases {
			if _, ok := corr[alias]; ok {
				needsTranslate = true
				break
			}
		}
		if needsTranslate {
			translated := values.ReplaceLeavesMaybe(v, func(leaf values.Value) values.Value {
				qov, ok := leaf.(*values.QuantifiedObjectValue)
				if !ok {
					return leaf
				}
				if _, isOuter := outerAliases[qov.Correlation]; isOuter {
					return values.NewQueriedValue(nil, leaf.Type())
				}
				return leaf
			})
			localVals = append(localVals, translated)
		} else {
			localVals = append(localVals, v)
		}
	}

	// Decorrelate inner results.
	var resultVals []values.Value
	for _, v := range innerDerivs.ResultValues {
		corr := values.GetCorrelatedToOfValue(v)
		needsTranslate := false
		for alias := range outerAliases {
			if _, ok := corr[alias]; ok {
				needsTranslate = true
				break
			}
		}
		if needsTranslate {
			translated := values.ReplaceLeavesMaybe(v, func(leaf values.Value) values.Value {
				qov, ok := leaf.(*values.QuantifiedObjectValue)
				if !ok {
					return leaf
				}
				if _, isOuter := outerAliases[qov.Correlation]; isOuter {
					return values.NewQueriedValue(nil, leaf.Type())
				}
				return leaf
			})
			resultVals = append(resultVals, translated)
		} else {
			resultVals = append(resultVals, v)
		}
	}

	// Translate comparison key values.
	compKeys := w.plan.GetComparisonKeys()
	for _, ckv := range compKeys {
		for _, rv := range resultVals {
			translated := properties.TranslateCorrelation(ckv, values.CurrentAlias, rv)
			localVals = append(localVals, translated)
		}
	}

	return &properties.Derivations{
		ResultValues: resultVals,
		LocalValues:  localVals,
	}
}

// --- FirstOrDefault ---

func derivationsForFirstOrDefault(w *physicalFirstOrDefaultWrapper) *properties.Derivations {
	childDerivs := properties.DerivationsFromSingleChild(w)
	childResults := childDerivs.ResultValues

	var localVals []values.Value
	localVals = append(localVals, childDerivs.LocalValues...)

	alias := w.innerQuant.GetAlias()
	onEmptyValue := w.plan.GetDefaultValue()
	if onEmptyValue != nil {
		for _, childResult := range childResults {
			translated := properties.TranslateCorrelation(onEmptyValue, alias, childResult)
			localVals = append(localVals, translated)
		}
	}

	return &properties.Derivations{
		ResultValues: childResults,
		LocalValues:  localVals,
	}
}

// --- DefaultOnEmpty ---

func derivationsForDefaultOnEmpty(w *physicalDefaultOnEmptyWrapper) *properties.Derivations {
	childDerivs := properties.DerivationsFromSingleChild(w)
	childResults := childDerivs.ResultValues

	var localVals []values.Value
	localVals = append(localVals, childDerivs.LocalValues...)

	alias := w.innerQuant.GetAlias()
	onEmptyValue := w.plan.GetDefaultValue()
	if onEmptyValue != nil {
		for _, childResult := range childResults {
			translated := properties.TranslateCorrelation(onEmptyValue, alias, childResult)
			localVals = append(localVals, translated)
		}
	}

	return &properties.Derivations{
		ResultValues: childResults,
		LocalValues:  localVals,
	}
}

// --- StreamingAggregation ---

func derivationsForStreamingAgg(w *physicalStreamingAggWrapper) *properties.Derivations {
	childDerivs := properties.DerivationsFromSingleChild(w)

	// The streaming aggregation plan doesn't carry the Java-style
	// resultValue / groupingKeyAlias / aggregateAlias / groupingValue /
	// aggregateValue distinction in Go's plan type. Return child
	// derivations (the aggregated output derivations are the child's
	// result values passed through the aggregation).
	return childDerivs
}

// --- Explode ---

func derivationsForExplode(w *physicalExplodeWrapper) *properties.Derivations {
	collectionValue := w.plan.GetCollectionValue()
	if collectionValue == nil {
		return properties.EmptyDerivations()
	}
	resultType := collectionValue.Type()
	// Java: the element type from the array type. In Go, the
	// collection value's type is already the element type for
	// the explode result.
	elementType := resultType
	if at, ok := resultType.(*values.ArrayType); ok && at.ElementType != nil {
		elementType = at.ElementType
	}
	resultVal := values.NewFirstOrDefaultValue(
		collectionValue,
		values.NewThrowsValue(elementType),
		elementType,
	)
	vals := []values.Value{resultVal}
	return &properties.Derivations{
		ResultValues: vals,
		LocalValues:  vals,
	}
}

// --- TableFunction ---

func derivationsForTableFunction(w *physicalTableFunctionWrapper) *properties.Derivations {
	streamingValue := w.plan.GetStreamValue()
	if streamingValue == nil {
		return properties.EmptyDerivations()
	}
	elementType := streamingValue.Type()
	resultVal := values.NewFirstOrDefaultStreamingValue(
		streamingValue,
		values.NewThrowsValue(elementType),
	)
	vals := []values.Value{resultVal}
	return &properties.Derivations{
		ResultValues: vals,
		LocalValues:  vals,
	}
}

// --- TempTableScan ---

func derivationsForTempTableScan(w *physicalTempTableScanWrapper) *properties.Derivations {
	resultValue := w.GetResultValue()
	return &properties.Derivations{
		ResultValues: []values.Value{resultValue},
	}
}

// --- Values ---

func derivationsForValues(w *physicalValuesWrapper) *properties.Derivations {
	resultValue := w.GetResultValue()
	vals := []values.Value{resultValue}
	return &properties.Derivations{
		ResultValues: vals,
		LocalValues:  vals,
	}
}

// --- NestedLoopJoin ---

func derivationsForNestedLoopJoin(w *physicalNestedLoopJoinWrapper) *properties.Derivations {
	// NestedLoopJoin has outer + inner quantifiers. Collect
	// derivations from both and union them.
	var resultVals []values.Value
	var localVals []values.Value

	for _, q := range w.GetQuantifiers() {
		childDerivs := properties.DerivationsFromQuantifier(q)
		resultVals = append(resultVals, childDerivs.ResultValues...)
		localVals = append(localVals, childDerivs.LocalValues...)
	}

	return &properties.Derivations{
		ResultValues: resultVals,
		LocalValues:  localVals,
	}
}
