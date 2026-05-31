package cascades

import (
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// UnplannableIndexOnlyResidualError reports that the only physical plan the
// planner could produce evaluates an index-only predicate (e.g. a vector K-NN
// DistanceRank over a DistanceRowNumberValue) as a residual filter over a base
// scan. Such a predicate can ONLY be produced by an index scan that binds it;
// as a residual it is not evaluable (Comparison.EvalAgainst would panic). This
// means no index could serve the predicate — e.g. a `QUALIFY ... ORDER BY
// cosine_distance(...)` against an index declared with a different metric, or a
// distance query on a column with no vector index — so the query is not
// plannable, exactly as Java leaves it (its single Compensation path stamps the
// match impossible). The query must fail to plan rather than build a plan that
// panics at execution.
type UnplannableIndexOnlyResidualError struct {
	// Predicate is the offending predicate's string form, for diagnostics.
	Predicate string
}

func (e *UnplannableIndexOnlyResidualError) Error() string {
	return fmt.Sprintf("query is not plannable: index-only predicate %q cannot be "+
		"evaluated as a residual filter and no index serves it", e.Predicate)
}

// validateNoIndexOnlyResidual rejects a final physical plan that carries an
// index-only predicate as a residual filter.
//
// Go reaches a physical filter via more rules than Java does — notably
// ImplementFilterRule, which synthesizes a RecordQueryPredicatesFilterPlan over
// a base scan without routing through Compensation. When an index DOES serve the
// index-only predicate (the common path) a vector/aggregate scan wins and no
// residual survives; this guard then never fires. It only triggers when the
// leaking filter is the sole survivor — i.e. nothing could bind the index-only
// value — and converts what would be an execution-time panic into a clean
// planning error. The structural fix (route filter implementation through
// Compensation so the bad plan is never built) is tracked as TODO 7.7 /
// DIVERGENCES.md "ImplementIndexScanRule is a Go-only second index-scan path".
func validateNoIndexOnlyResidual(expr expressions.RelationalExpression) error {
	ph, ok := expr.(interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	})
	if !ok {
		return nil // Not a physical plan; the caller surfaces that separately.
	}
	plan := ph.GetRecordQueryPlan()
	if plan == nil {
		return nil
	}
	if bad := findIndexOnlyResidual(plan); bad != nil {
		return &UnplannableIndexOnlyResidualError{Predicate: bad.Explain()}
	}
	return nil
}

// findIndexOnlyResidual walks a physical plan tree and returns the first filter
// predicate that contains an index-only (uncompensatable) value, or nil.
func findIndexOnlyResidual(plan plans.RecordQueryPlan) predicates.QueryPredicate {
	var found predicates.QueryPredicate
	var walk func(p plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		if found != nil || p == nil {
			return
		}
		type predicateCarrier interface {
			GetPredicates() []predicates.QueryPredicate
		}
		if pc, ok := p.(predicateCarrier); ok {
			for _, pr := range pc.GetPredicates() {
				if predicateContainsUncompensatableValues(pr) {
					found = pr
					return
				}
			}
		}
		for _, c := range p.GetChildren() {
			walk(c)
		}
	}
	walk(plan)
	return found
}
