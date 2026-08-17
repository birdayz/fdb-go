package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementUpdateRule implements a logical UpdateExpression as a
// physical RecordQueryUpdatePlan.
//
//	Update(target, [transforms], stored-record-access-path)
//	  →  UpdatePlan(target, [transforms],
//	         UnorderedPrimaryKeyDistinct(stored-record-access-path))
//
// Ports Java's ImplementUpdateRule 1:1 on both of its structural decisions:
//
//   - the inner reference is bound only through partitions whose
//     StoredRecordProperty holds (ImplementUpdateRule.java:54-57), and
//   - a primary-key dedup is interposed UNCONDITIONALLY between the access path
//     and the mutation (ImplementUpdateRule.java:79-80). The update rule does
//     not consult DistinctRecordsProperty at all — only ImplementDeleteRule
//     does — so neither does this one.
//
// Both are handled by storedRecordDMLCandidates / dmlDedupedInnerQuantifier,
// where the reasoning for each lives.
//
// Per-row transform application happens at execution time (not rule-fire
// time) — transforms pass through unchanged.
type ImplementUpdateRule struct {
	matcher matching.BindingMatcher
}

// NewImplementUpdateRule constructs the rule.
func NewImplementUpdateRule() *ImplementUpdateRule {
	return &ImplementUpdateRule{
		matcher: NewExpressionMatcher[*expressions.UpdateExpression]("update"),
	}
}

// Matcher returns the pattern.
func (r *ImplementUpdateRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires on every UpdateExpression, once per stored-record access path
// the inner reference offers.
func (r *ImplementUpdateRule) OnMatch(call *ExpressionRuleCall) {
	upd := matching.Get[*expressions.UpdateExpression](call.Bindings, r.matcher)
	innerRef := upd.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}
	for _, candidate := range storedRecordDMLCandidates(innerRef) {
		// The UPDATE plan is its own cascades expression (RFC-184 W2) — it
		// carries the live child edge directly, no physicalUpdateWrapper.
		innerQ, err := dmlDedupedInnerQuantifier(call, candidate, false)
		if err != nil {
			call.Fail(err)
			return
		}
		updPlan, err := plans.NewRecordQueryUpdatePlanFromQuantifierWithTargetType(
			innerQ, upd.GetTargetRecordType(), upd.GetTargetType(), upd.GetTransforms())
		if err != nil {
			call.Fail(err)
			return
		}
		call.Yield(updPlan)
	}
}

var _ ExpressionRule = (*ImplementUpdateRule)(nil)
