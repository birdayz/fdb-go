package properties

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ScanComparisonProvider is the interface plan nodes implement to
// expose their per-column scan comparison ranges. Matches Java's
// RecordQueryPlanWithComparisons.getComparisons / hasComparisons.
type ScanComparisonProvider interface {
	GetScanComparisons() []*predicates.ComparisonRange
}

// IntersectionExpression marks expressions whose children's comparison
// sets should be intersected rather than unioned. Matches Java's
// RecordQueryIntersectionPlan semantics in ComparisonsProperty.
type IntersectionExpression interface {
	IsIntersection()
}

// EvaluateComparisons walks the expression tree bottom-up and
// collects individual Comparison objects from plan nodes that implement
// ScanComparisonProvider. Matches Java's ComparisonsProperty.evaluate
// which returns Set<Comparisons.Comparison>.
//
// For intersection plans (expressions implementing IntersectionExpression),
// the result is the intersection of children's comparison sets — only
// comparisons present in ALL children are kept. For all other nodes,
// the result is the union.
func EvaluateComparisons(expr expressions.RelationalExpression) []predicates.Comparison {
	if expr == nil {
		return nil
	}
	return evaluateComparisonsRec(expr)
}

func evaluateComparisonsRec(expr expressions.RelationalExpression) []predicates.Comparison {
	if expr == nil {
		return nil
	}

	_, isIntersection := expr.(IntersectionExpression)

	var childResults [][]predicates.Comparison
	for _, q := range expr.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		for _, m := range ref.Members() {
			childResults = append(childResults, evaluateComparisonsRec(m))
		}
	}

	if isIntersection && len(childResults) > 0 {
		return intersectComparisons(childResults)
	}

	var result []predicates.Comparison
	if scp, ok := expr.(ScanComparisonProvider); ok {
		result = append(result, extractComparisons(scp.GetScanComparisons())...)
	}
	for _, cr := range childResults {
		result = append(result, cr...)
	}
	return result
}

func extractComparisons(ranges []*predicates.ComparisonRange) []predicates.Comparison {
	var out []predicates.Comparison
	for _, r := range ranges {
		if r == nil {
			continue
		}
		if r.IsEquality() {
			if c := r.GetEqualityComparison(); c != nil {
				out = append(out, *c)
			}
		} else if r.IsInequality() {
			for _, c := range r.GetInequalityComparisons() {
				if c != nil {
					out = append(out, *c)
				}
			}
		}
	}
	return out
}

type compKey struct {
	typ     predicates.ComparisonType
	operand string
}

func comparisonKey(c predicates.Comparison) compKey {
	var op string
	if c.Operand != nil {
		op = values.ExplainValue(c.Operand)
	}
	return compKey{typ: c.Type, operand: op}
}

func intersectComparisons(sets [][]predicates.Comparison) []predicates.Comparison {
	if len(sets) == 0 {
		return nil
	}
	if len(sets) == 1 {
		return sets[0]
	}

	counts := make(map[compKey]int)
	unique := make(map[compKey]predicates.Comparison)
	for _, c := range sets[0] {
		k := comparisonKey(c)
		counts[k] = 1
		unique[k] = c
	}
	for _, set := range sets[1:] {
		seen := make(map[compKey]bool)
		for _, c := range set {
			k := comparisonKey(c)
			if !seen[k] {
				seen[k] = true
				if _, exists := counts[k]; exists {
					counts[k]++
				}
			}
		}
	}
	var result []predicates.Comparison
	needed := len(sets)
	for k, c := range counts {
		if c >= needed {
			result = append(result, unique[k])
		}
	}
	return result
}
