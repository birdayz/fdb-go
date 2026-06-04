package cascades

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

type physicalFlatMapWrapper struct {
	plan       plans.RecordQueryPlan
	outerQuant expressions.Quantifier
	innerQuant expressions.Quantifier
}

func newPhysicalFlatMapWrapper(
	plan plans.RecordQueryPlan,
	outerQuant, innerQuant expressions.Quantifier,
) *physicalFlatMapWrapper {
	return &physicalFlatMapWrapper{
		plan:       plan,
		outerQuant: outerQuant,
		innerQuant: innerQuant,
	}
}

func (w *physicalFlatMapWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalFlatMapWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalFlatMapWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.outerQuant, w.innerQuant}
}

func (w *physicalFlatMapWrapper) CanCorrelate() bool  { return true }
func (w *physicalFlatMapWrapper) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns empty for join FlatMaps. This is
// correct for joins — correlations flow through the quantifier children, and a
// join's result is a merge of those children's bound rows, not an external
// correlation. GetCorrelatedToOfValue reports nothing for a translator seed
// merge (the Seed=true gate in value_correlation.go, matching the retired
// JoinMergeResultValue); a re-enumeration merge would report its aliases, but
// those name the join's own (bound) legs, not external correlations. When
// correlated subqueries are ported, the merge value's correlation contribution
// must propagate genuinely-external aliases.
func (w *physicalFlatMapWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalFlatMapWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalFlatMapWrapper)
	if !ok {
		return false
	}
	if w.plan == nil || o.plan == nil {
		return w.plan == nil && o.plan == nil
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalFlatMapWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physflatmap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalFlatMapWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) < 2 {
		return properties.Cost{}
	}
	// Single source of truth (cost_formulas.go) — shared with concretePlanCost.
	return flatMapCost(child[0], child[1])
}

// HintOrdering conservatively reports NO known ordering.
//
// A FlatMap (nested loop) IS ordered by its outer (the inner only sub-orders
// within each outer group), so it is tempting to propagate the outer's ordering
// to enable ORDER-BY-on-outer-key sort elimination. But scanning the outer
// Reference's members and returning the first KNOWN ordering is unsound (codex
// P1 / @claude finding 4): `w.plan` executes a SPECIFIC outer plan captured when
// the FlatMap was built, which need not be the member whose ordering is reported
// — so an ORDER BY could be considered satisfied and the sort removed while the
// emitted rows are not actually ordered. Reporting Unknown is the correct,
// conservative behavior (it never removes a sort that is actually needed).
//
// The ordering-constraint pass (RFC-076 step 3a) makes a requested ordering reach the
// SCAN through a residual filter, which is enough for single-input sort elimination
// (TestEndToEnd_SortElimThroughResidualFilter) — no join-ordering propagation needed.
// Sound outer-ordering propagation THROUGH a join — derived from w.plan's actual outer,
// not an arbitrary Reference member — is a NET-NEW capability (the retired
// ImplementIndexScanRule never provided it either, so leaving it off is not a
// retirement regression). It is deliberately deferred: reporting Unknown is always
// sound (it only ever leaves a redundant sort, never removes a needed one), whereas a
// wrong propagated ordering removes a needed sort and silently mis-orders rows. Keeping
// Unknown is the conservative, correct choice; enabling join-sort-elimination is future
// work with its own test bar.
func (w *physicalFlatMapWrapper) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

func (w *physicalFlatMapWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 2 {
		return nil, fmt.Errorf("physicalFlatMapWrapper.WithChildren: expected 2 children, got %d", len(qs))
	}
	return &physicalFlatMapWrapper{plan: w.plan, outerQuant: qs[0], innerQuant: qs[1]}, nil
}

func (w *physicalFlatMapWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

// IsPhysicalFlatMap reports whether expr is a physicalFlatMapWrapper.
func IsPhysicalFlatMap(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalFlatMapWrapper)
	return ok
}

var (
	_ expressions.RelationalExpression = (*physicalFlatMapWrapper)(nil)
	_ physicalPlanExpression           = (*physicalFlatMapWrapper)(nil)
)
