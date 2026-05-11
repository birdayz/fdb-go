package properties

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
)

// ScanComparisonProvider is the interface plan nodes implement to
// expose their per-column scan comparison ranges. Matches Java's
// RecordQueryPlanWithComparisons.getComparisons / hasComparisons.
type ScanComparisonProvider interface {
	GetScanComparisons() []*predicates.ComparisonRange
}

// EvaluateComparisons walks the expression tree bottom-up and
// collects all ComparisonRange objects from plan nodes that implement
// ScanComparisonProvider. Matches Java's ComparisonsProperty.evaluate
// — the result is the union of all scan comparisons in the subtree.
//
// For intersection plans, Java intersects the comparison sets of
// children. This evaluator takes the simpler union approach matching
// the cost model's collectScanComparisons; full intersection semantics
// can be added when intersection-aware plan selection requires it.
func EvaluateComparisons(expr expressions.RelationalExpression) []*predicates.ComparisonRange {
	if expr == nil {
		return nil
	}
	var result []*predicates.ComparisonRange
	evaluateComparisonsRec(expr, &result)
	return result
}

func evaluateComparisonsRec(expr expressions.RelationalExpression, out *[]*predicates.ComparisonRange) {
	if expr == nil {
		return
	}
	// Check if this expression provides comparisons directly.
	if scp, ok := expr.(ScanComparisonProvider); ok {
		comps := scp.GetScanComparisons()
		if len(comps) > 0 {
			*out = append(*out, comps...)
		}
	}
	// Recurse into children.
	for _, q := range expr.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		for _, m := range ref.Members() {
			evaluateComparisonsRec(m, out)
		}
	}
}
