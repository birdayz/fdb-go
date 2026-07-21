package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementTempTableScanRule converts a TempTableScanExpression to a
// physical TempTableScanPlan. Mirrors Java's ImplementTempTableScanRule.
type ImplementTempTableScanRule struct {
	matcher matching.BindingMatcher
}

func NewImplementTempTableScanRule() *ImplementTempTableScanRule {
	return &ImplementTempTableScanRule{
		matcher: NewExpressionMatcher[*expressions.TempTableScanExpression]("temp_table_scan"),
	}
}

func (r *ImplementTempTableScanRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementTempTableScanRule) OnMatch(call *ExpressionRuleCall) {
	scan := matching.Get[*expressions.TempTableScanExpression](call.Bindings, r.matcher)
	// The temp-table scan is its own Cascades expression now (RFC-184 W2) — a bare
	// leaf plan, no physicalTempTableScanWrapper adapter needed.
	plan := plans.NewRecordQueryTempTableScanPlan(scan.GetTempTableAlias())
	call.Yield(plan)
}

var _ ExpressionRule = (*ImplementTempTableScanRule)(nil)
