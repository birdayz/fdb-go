package cascades

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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

// validateNoIndexOnlyResidual is the CATCH-ALL backstop: it rejects a final
// PHYSICAL plan that carries an index-only predicate as a residual filter,
// regardless of which rule produced the filter.
//
// The Java !isIndexOnly() gate on ImplementFilterRule (rule_implement_filter.go)
// prevents that ONE Go-only filter producer from building such a plan. But Go has
// several OTHER physical-filter builders that Java routes through compensation
// instead — ImplementSimpleSelectRule (builds a filter from a SelectExpression's
// predicates), the NLJ residual builder, ImplementIndexScanRule's residual loop.
// An index-only DistanceRank in the ORIGINAL query (e.g. a metric-mismatched
// distance inside a join → a Select, not a LogicalFilter) reaches a physical
// residual through one of those, which the gate does not see. This walk is the one
// place that covers EVERY physical-filter path, so it stays until all builders are
// gated (Graefe: a cheap catch-all is the correct backstop while Go has more
// filter builders than Java's single compensation-gated path). Pinned by
// TestVectorPlan_MetricMismatchDoesNotMatchVector (single-table, LogicalFilter) +
// the join-vector regression (Select path).
func validateNoIndexOnlyResidual(expr expressions.RelationalExpression) error {
	ph, ok := expr.(interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	})
	if !ok {
		return nil // Not a physical plan; the logical-side check handles that case.
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

// findIndexOnlyResidual walks a physical plan tree (incl. nested under union /
// intersection arms — leaks at depth > 0) and returns the first filter predicate
// that contains an index-only (uncompensatable) value, or nil.
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

// findIndexOnlyLogicalResidual walks the logical expression DAG rooted at `ref`
// and returns the first LogicalFilterExpression predicate that contains an
// index-only (uncompensatable) value, or nil.
//
// This complements the physical-side validateNoIndexOnlyResidual backstop. When
// the Java !isIndexOnly() ImplementFilterRule gate keeps a LogicalFilter
// unimplemented and no other producer realizes it, the best plan is NON-physical
// (e.g. a LogicalProjection), so the physical walk sees nothing. This walk runs on
// that case to recover the precise cause and surface the same clean
// UnplannableIndexOnlyResidualError (matching Java leaving the match impossible),
// rather than the caller reporting the internal expression type.
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
