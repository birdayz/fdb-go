package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementTableFunctionRule converts a TableFunctionExpression into a
// physical RecordQueryTableFunctionPlan. Direct translation — the
// streaming Value passes through unchanged.
//
// Mirrors Java's ImplementTableFunctionRule.
type ImplementTableFunctionRule struct {
	matcher matching.BindingMatcher
}

func NewImplementTableFunctionRule() *ImplementTableFunctionRule {
	return &ImplementTableFunctionRule{
		matcher: NewExpressionMatcher[*expressions.TableFunctionExpression]("table_function"),
	}
}

func (r *ImplementTableFunctionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementTableFunctionRule) OnMatch(call *ExpressionRuleCall) {
	tableFn := matching.Get[*expressions.TableFunctionExpression](call.Bindings, r.matcher)
	plan := plans.NewRecordQueryTableFunctionPlan(tableFn.GetValue())
	call.Yield(newPhysicalTableFunctionWrapper(plan))
}

var _ ExpressionRule = (*ImplementTableFunctionRule)(nil)
