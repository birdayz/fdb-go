package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementValuesRule implements a logical LogicalValuesExpression as a
// physical RecordQueryValuesPlan. Trivial leaf rule — no inner to gate on.
type ImplementValuesRule struct {
	matcher matching.BindingMatcher
}

func NewImplementValuesRule() *ImplementValuesRule {
	return &ImplementValuesRule{
		matcher: NewExpressionMatcher[*expressions.LogicalValuesExpression]("logical_values"),
	}
}

func (r *ImplementValuesRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementValuesRule) OnMatch(call *ExpressionRuleCall) {
	ve := matching.Get[*expressions.LogicalValuesExpression](call.Bindings, r.matcher)
	plan := plans.NewRecordQueryValuesPlan(ve.GetColumns())
	call.Yield(NewPhysicalValuesWrapper(plan))
}

var _ ExpressionRule = (*ImplementValuesRule)(nil)
