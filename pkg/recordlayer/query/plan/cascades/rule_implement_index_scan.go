package cascades

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementIndexScanRule implements a LogicalFilterExpression over a
// FullUnorderedScanExpression as a physical index scan when the
// PlanContext contains a MatchCandidate whose key columns match the
// filter's ComparisonPredicates.
//
//	Filter([P1, P2, ...], FullUnorderedScan)
//	  →  IndexScan(prefix)               [all predicates consumed]
//	  →  FilterPlan(residual, IndexScan)  [partial consumption]
//
// For each candidate in PlanContext.GetMatchCandidates():
//  1. Check record-type overlap with the scan.
//  2. Walk the filter's ComparisonPredicates: map each predicate's
//     FieldValue.Field to the candidate's column names, then merge
//     the comparison into the corresponding sargable alias's range.
//  3. Compute the bound prefix map via the candidate.
//  4. If the prefix is non-empty, yield the index scan (wrapped as
//     physicalIndexScanWrapper). If there are residual predicates
//     (ones not consumed by the index), wrap in a physicalFilterWrapper.
//
// Java equivalent: the combined effect of ImplementPhysicalScanRule +
// ValueIndexScanMatchCandidate's predicate matching in the OPTIMIZE
// phase.
type ImplementIndexScanRule struct {
	matcher matching.BindingMatcher
}

func NewImplementIndexScanRule() *ImplementIndexScanRule {
	return &ImplementIndexScanRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter_for_index"),
	}
}

func (r *ImplementIndexScanRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementIndexScanRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)

	innerRef := f.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	scan := findFullScan(innerRef)
	if scan == nil {
		return
	}

	candidates := call.Context.GetMatchCandidates()
	if len(candidates) == 0 {
		return
	}

	preds := f.GetPredicates()
	if len(preds) == 0 {
		return
	}

	scanTypes := scan.GetRecordTypes()

	for _, cand := range candidates {
		if !recordTypesOverlap(scanTypes, cand.GetRecordTypes()) {
			continue
		}

		colNames := cand.GetColumnNames()
		aliases := cand.GetSargableAliases()
		if len(colNames) != len(aliases) {
			continue
		}

		colToIdx := buildColumnIndex(colNames)

		bindings := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
		poisoned := make(map[values.CorrelationIdentifier]bool)
		var consumed []int

		for i, p := range preds {
			cp, ok := p.(*predicates.ComparisonPredicate)
			if !ok {
				continue
			}
			fv, ok := cp.Operand.(*values.FieldValue)
			if !ok {
				continue
			}
			colIdx, found := colToIdx[strings.ToUpper(fv.Field)]
			if !found {
				continue
			}
			alias := aliases[colIdx]
			if poisoned[alias] {
				continue
			}
			cr := bindings[alias]
			if cr == nil {
				cr = predicates.EmptyComparisonRange()
			}
			res := cr.Merge(&cp.Comparison)
			if !res.Ok {
				delete(bindings, alias)
				poisoned[alias] = true
				continue
			}
			bindings[alias] = res.Range
			consumed = append(consumed, i)
		}

		prefix := cand.ComputeBoundParameterPrefixMap(bindings)
		if len(prefix) == 0 {
			continue
		}

		idxPlan := cand.ToScanPlan(prefix, false)
		idxPlanTyped := extractIndexPlan(idxPlan)
		if idxPlanTyped == nil {
			continue
		}

		wrapper := &physicalIndexScanWrapper{plan: idxPlanTyped, columnNames: colNames, unique: cand.IsUnique()}

		residual := residualPredicates(preds, consumed, prefix, aliases, colToIdx)
		if len(residual) == 0 {
			call.Yield(wrapper)
		} else {
			filterPlan := plans.NewRecordQueryFilterPlan(residual, idxPlan)
			innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(wrapper))
			call.Yield(NewPhysicalFilterWrapper(filterPlan, innerQ))
		}
	}
}

// extractIndexPlan extracts a *RecordQueryIndexPlan from a plan that
// may be either an IndexPlan directly or a FetchFromPartialRecordPlan
// wrapping one.
func extractIndexPlan(p plans.RecordQueryPlan) *plans.RecordQueryIndexPlan {
	if ip, ok := p.(*plans.RecordQueryIndexPlan); ok {
		return ip
	}
	if fp, ok := p.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
		if inner := fp.GetInner(); inner != nil {
			if ip, ok := inner.(*plans.RecordQueryIndexPlan); ok {
				return ip
			}
		}
	}
	return nil
}

// findFullScan looks for a FullUnorderedScanExpression in a Reference.
func findFullScan(ref *expressions.Reference) *expressions.FullUnorderedScanExpression {
	for _, m := range ref.Members() {
		if s, ok := m.(*expressions.FullUnorderedScanExpression); ok {
			return s
		}
	}
	return nil
}

// recordTypesOverlap returns true if any element of a appears in b.
func recordTypesOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	for _, ta := range a {
		for _, tb := range b {
			if strings.EqualFold(ta, tb) {
				return true
			}
		}
	}
	return false
}

// buildColumnIndex maps column name (upper-cased) → positional index.
func buildColumnIndex(cols []string) map[string]int {
	m := make(map[string]int, len(cols))
	for i, c := range cols {
		m[strings.ToUpper(c)] = i
	}
	return m
}

// residualPredicates returns the predicates NOT consumed by the index
// scan. A predicate is consumed if it was successfully merged AND its
// corresponding alias appears in the final prefix map.
func residualPredicates(
	preds []predicates.QueryPredicate,
	consumed []int,
	prefix map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	aliases []values.CorrelationIdentifier,
	colToIdx map[string]int,
) []predicates.QueryPredicate {
	consumedInPrefix := make(map[int]bool)
	for _, idx := range consumed {
		cp := preds[idx].(*predicates.ComparisonPredicate)
		fv := cp.Operand.(*values.FieldValue)
		colIdx := colToIdx[strings.ToUpper(fv.Field)]
		alias := aliases[colIdx]
		if _, inPrefix := prefix[alias]; inPrefix {
			consumedInPrefix[idx] = true
		}
	}
	var residual []predicates.QueryPredicate
	for i, p := range preds {
		if !consumedInPrefix[i] {
			residual = append(residual, p)
		}
	}
	return residual
}

var _ ExpressionRule = (*ImplementIndexScanRule)(nil)
