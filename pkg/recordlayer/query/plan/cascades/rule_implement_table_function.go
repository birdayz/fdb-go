package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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
	// The table function is its own Cascades expression now (RFC-184 W2) — a bare
	// leaf plan, no physicalTableFunctionWrapper adapter needed.
	plan := plans.NewRecordQueryTableFunctionPlan(tableFn.GetValue())
	call.Yield(plan)
}

var _ ExpressionRule = (*ImplementTableFunctionRule)(nil)
