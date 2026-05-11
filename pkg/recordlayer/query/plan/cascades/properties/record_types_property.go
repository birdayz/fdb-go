package properties

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
)

// RecordTypeProvider is the interface expressions implement to
// expose the record types they produce. Matches Java's
// RecordQueryScanPlan.getRecordTypes(), FullUnorderedScanExpression.
// getRecordTypes(), etc.
type RecordTypeProvider interface {
	GetRecordTypes() []string
}

// EvaluateRecordTypes walks the expression tree bottom-up and
// collects the set of record type names the expression could produce.
// Matches Java's RecordTypesProperty.evaluate.
//
// Semantics:
//   - Leaf nodes that implement RecordTypeProvider contribute their
//     record types directly.
//   - TypeFilter expressions (which also implement RecordTypeProvider)
//     contribute only their filtered type set.
//   - Non-leaf expressions with a single child propagate that child's
//     record types unchanged.
//   - Multi-child expressions (union, intersection, select, etc.)
//     union all children's record types.
//   - Unknown leaf nodes contribute nothing.
func EvaluateRecordTypes(expr expressions.RelationalExpression) map[string]struct{} {
	if expr == nil {
		return nil
	}

	// If this node directly provides record types, use them.
	// TypeFilter is special: it intersects with children (but since
	// it declares the types directly, we use its declaration).
	if rtp, ok := expr.(RecordTypeProvider); ok {
		rts := rtp.GetRecordTypes()
		if len(rts) > 0 {
			result := make(map[string]struct{}, len(rts))
			for _, rt := range rts {
				result[rt] = struct{}{}
			}
			// For type filter: intersect with child record types.
			// In Java, TypeFilterExpression filters the child's set.
			// Since the type filter declares its allowed types, and
			// it's the intersection of child types with allowed types,
			// we still return the declared types (they're already the
			// intersection result from planning).
			return result
		}
	}

	// Collect from children.
	childResults := collectChildRecordTypes(expr)
	if len(childResults) == 0 {
		return map[string]struct{}{}
	}
	if len(childResults) == 1 {
		return childResults[0]
	}
	// Multi-child: union all child record types.
	result := make(map[string]struct{})
	for _, cr := range childResults {
		for rt := range cr {
			result[rt] = struct{}{}
		}
	}
	return result
}

// collectChildRecordTypes recurses into quantifiers and collects
// record types from each child reference.
func collectChildRecordTypes(expr expressions.RelationalExpression) []map[string]struct{} {
	qs := expr.GetQuantifiers()
	if len(qs) == 0 {
		return nil
	}
	results := make([]map[string]struct{}, 0, len(qs))
	for _, q := range qs {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		// Union across all members of the reference (Java's
		// evaluateAtRef unions member results).
		refResult := make(map[string]struct{})
		for _, m := range ref.Members() {
			for rt := range EvaluateRecordTypes(m) {
				refResult[rt] = struct{}{}
			}
		}
		results = append(results, refResult)
	}
	return results
}
