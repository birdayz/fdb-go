package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementDeleteRule implements a logical DeleteExpression as a
// physical RecordQueryDeletePlan.
//
//	Delete(target, stored-record-access-path)
//	  →  DeletePlan(target,
//	         UnorderedPrimaryKeyDistinct(stored-record-access-path))
//
// Ports Java's ImplementDeleteRule 1:1 on both of its structural decisions:
//
//   - the inner reference is bound only through partitions whose
//     StoredRecordProperty holds (ImplementDeleteRule.java:55-57), and
//   - a primary-key dedup is interposed between the access path and the
//     mutation UNLESS the access path already proves
//     DistinctRecordsProperty.distinctRecords() (ImplementDeleteRule.java:79-82).
//
// The short-circuit is the ONE place the two DML rules differ: an UPDATE gets
// the dedup unconditionally (ImplementUpdateRule.java:79-80). Reading that as
// an inconsistency and unifying it either way would be wrong in one direction
// or the other, so it is preserved exactly.
type ImplementDeleteRule struct {
	matcher matching.BindingMatcher
}

// NewImplementDeleteRule constructs the rule.
func NewImplementDeleteRule() *ImplementDeleteRule {
	return &ImplementDeleteRule{
		matcher: NewExpressionMatcher[*expressions.DeleteExpression]("delete"),
	}
}

// Matcher returns the pattern.
func (r *ImplementDeleteRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires on every DeleteExpression, once per stored-record access path
// the inner reference offers.
func (r *ImplementDeleteRule) OnMatch(call *ExpressionRuleCall) {
	del := matching.Get[*expressions.DeleteExpression](call.Bindings, r.matcher)
	innerRef := del.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}
	for _, candidate := range storedRecordDMLCandidates(innerRef) {
		// The DELETE plan is its own cascades expression (RFC-184 W2) — it
		// carries the live child edge directly, no physicalDeleteWrapper.
		innerQ := dmlDedupedInnerQuantifier(call, candidate, candidate.distinctRecords)
		delPlan := plans.NewRecordQueryDeletePlanFromQuantifier(innerQ, del.GetTargetRecordType())
		call.Yield(delPlan)
	}
}

var _ ExpressionRule = (*ImplementDeleteRule)(nil)
