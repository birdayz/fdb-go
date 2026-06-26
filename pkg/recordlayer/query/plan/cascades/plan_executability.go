package cascades

import (
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
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

// findIndexOnlyLogicalResidual walks the logical expression DAG rooted at `ref`
// and returns the first LogicalFilterExpression predicate that contains an
// index-only (uncompensatable) value, or nil.
//
// With the Java !isIndexOnly() ImplementFilterRule gate in place, an index-only
// predicate (a vector DistanceRank / UnmatchedAggregateValue) that NO index can
// serve is never realized to a physical residual filter — the gate keeps such a
// LogicalFilterExpression unimplemented, so ExtractBestPlanFromSelector fails
// with a generic "not a physical plan". This walk runs only on that extraction
// failure to recover the precise cause and surface the clean
// UnplannableIndexOnlyResidualError (matching Java leaving the match impossible),
// rather than leaking the internal expression type. It replaces the old
// physical-plan-side validateNoIndexOnlyResidual net, which is dead behind the
// gate (the bad plan is never built — exactly the structural fix TODO 7.7 asked
// for).
func findIndexOnlyLogicalResidual(ref *expressions.Reference) predicates.QueryPredicate {
	if ref == nil {
		return nil
	}
	visited := make(map[*expressions.Reference]bool)
	var found predicates.QueryPredicate
	var walk func(r *expressions.Reference)
	walk = func(r *expressions.Reference) {
		if r == nil || found != nil || visited[r] {
			return
		}
		visited[r] = true
		for _, m := range r.AllMembers() {
			if lf, ok := m.(*expressions.LogicalFilterExpression); ok {
				for _, pr := range lf.GetPredicates() {
					if predicateContainsUncompensatableValues(pr) {
						found = pr
						return
					}
				}
			}
			for _, q := range m.GetQuantifiers() {
				walk(q.GetRangesOver())
			}
		}
	}
	walk(ref)
	return found
}
