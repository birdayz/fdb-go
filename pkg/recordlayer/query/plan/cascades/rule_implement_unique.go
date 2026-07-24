package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementUniqueRule implements LogicalUniqueExpression one exact physical
// member at a time. Ordinary Unique is absorbed only when that same member is
// proven both record-distinct and to carry a primary key. Required Unique is
// never absorbed: every exact member with a primary key is frozen under a
// physical PK-distinct plan.
//
// Ports Java's ImplementUniqueRule.
type ImplementUniqueRule struct {
	matcher matching.BindingMatcher
}

func NewImplementUniqueRule() *ImplementUniqueRule {
	return &ImplementUniqueRule{
		matcher: NewExpressionMatcher[*expressions.LogicalUniqueExpression]("implement_unique"),
	}
}

func (r *ImplementUniqueRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementUniqueRule) OnMatch(call *ImplementationRuleCall) {
	expr := call.Bindings.Get(r.matcher).(*expressions.LogicalUniqueExpression)

	innerRef := expr.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	planProperties := GetRefPlanPropertiesMap(innerRef)
	if planProperties == nil {
		return
	}

	for _, member := range planProperties.Expressions() {
		if _, ok := member.(physicalPlanExpression); !ok {
			continue
		}
		memberProperties := planProperties.GetProperties(member)
		if memberProperties[properties.PropPrimaryKey] == nil {
			continue
		}

		if !expr.IsRequired() {
			if memberProperties.GetBool(properties.PropDistinctRecords) {
				call.YieldFinalExpression(member)
			}
			continue
		}

		// Required mode is an enforcer, even for an already-distinct member.
		// Freeze the exact member whose PK property was inspected in a detached
		// single-member final reference. A live edge could later float to a
		// sibling without that proof and make the wrapper deduplicate against a
		// different plan than the one verified above.
		innerQ := expressions.ForEachQuantifier(
			call.MemoizeFinalExpression(member),
		)
		call.YieldFinalExpression(
			plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlanFromQuantifier(
				innerQ,
			),
		)
	}
}

var _ ImplementationRule = (*ImplementUniqueRule)(nil)
