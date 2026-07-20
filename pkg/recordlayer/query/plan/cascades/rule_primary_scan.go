package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// PrimaryScanRule implements a logical FullUnorderedScanExpression as
// a physical RecordQueryScanPlan.
//
//	FullUnorderedScan({records}, type)  →  Scan({records}, type, false)
//
// "Implements" rules in Cascades go from logical to physical. The
// rule output is YIELDED into the same Reference as the logical
// input, so the Reference holds BOTH alternatives — the logical and
// the physical — and downstream rule chains operate on whichever
// members they pattern-match; cost extraction picks the physical
// (the logical has no Execute path).
//
// The rule yields a `*plans.RecordQueryScanPlan` wrapped in a
// `physicalScanWrapper` (the plan/expression hierarchies are separate
// in Go — see physical_wrapper.go).
//
// Java equivalent: `PrimaryScanRule`. This rule is the unindexed
// fallback; sargable primary-key access goes through
// PrimaryScanMatchCandidate on the data-access path
// (primary_scan_match_candidate.go).
type PrimaryScanRule struct {
	matcher matching.BindingMatcher
}

// NewPrimaryScanRule constructs the rule.
func NewPrimaryScanRule() *PrimaryScanRule {
	return &PrimaryScanRule{
		matcher: NewExpressionMatcher[*expressions.FullUnorderedScanExpression]("full_unordered_scan"),
	}
}

// Matcher returns the pattern.
func (r *PrimaryScanRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires on every FullUnorderedScanExpression and yields a
// RecordQueryScanPlan over the same record types.
func (r *PrimaryScanRule) OnMatch(call *ExpressionRuleCall) {
	scan := matching.Get[*expressions.FullUnorderedScanExpression](call.Bindings, r.matcher)
	plan := plans.NewRecordQueryScanPlan(scan.GetRecordTypes(), scan.GetFlowedType(), false)

	if call.Context != nil && len(scan.GetRecordTypes()) == 1 {
		pkCols := call.Context.GetPrimaryKeyColumns(scan.GetRecordTypes()[0])
		if len(pkCols) > 0 {
			pkVals := make([]values.Value, len(pkCols))
			for i, col := range pkCols {
				pkVals[i] = &values.FieldValue{Field: col, Typ: values.UnknownType}
			}
			plan = plan.WithPrimaryKey(pkVals)
		}
	}

	// Yield the BARE scan: RecordQueryScanPlan is its own physical Cascades
	// expression now (RFC-184 W2), so the physicalScanWrapper adapter is no
	// longer needed for a leaf scan.
	call.Yield(plan)
}

var _ ExpressionRule = (*PrimaryScanRule)(nil)
