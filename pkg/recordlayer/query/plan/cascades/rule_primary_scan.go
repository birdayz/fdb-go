package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// PrimaryScanRule is the first B5 Batch A rule: implements a logical
// FullUnorderedScanExpression as a physical RecordQueryScanPlan.
//
//	FullUnorderedScan({records}, type)  →  Scan({records}, type, false)
//
// "Implements" rules in Cascades go from logical to physical. The
// rule output is YIELDED into the same Reference as the logical
// input, so the Reference holds BOTH alternatives — the logical and
// the physical. Cost extraction picks the physical (because the
// logical has no Execute path; in practice the cost comparator
// would prefer physical). For the seed, both members coexist in the
// Reference; downstream rule chains operate on whichever members
// they pattern-match.
//
// CAVEAT: this rule yields a `*plans.RecordQueryScanPlan`, but the
// existing RelationalExpression hierarchy is the only thing the rule
// engine knows how to dedup / store. To make the physical plan
// addressable as a member of the Reference, we wrap it in a
// `physicalScanWrapper` that adapts RecordQueryScanPlan → minimal
// RelationalExpression surface. Once a proper "MixedReference" /
// physical-plan-aware Memo lands, this wrapper goes away.
//
// Java equivalent: `PrimaryScanRule` (drives PrimaryScanMatchCandidate
// for index-pushdown). The seed implements only the unindexed
// fallback path.
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

	call.Yield(&physicalScanWrapper{plan: plan})
}

var _ ExpressionRule = (*PrimaryScanRule)(nil)
