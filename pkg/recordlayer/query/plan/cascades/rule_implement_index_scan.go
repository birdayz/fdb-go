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

	preds := flattenFilterPredicates(f.GetPredicates())
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
			// Only push range-safe comparison types into scan ranges.
			// ComparisonIn, ComparisonIsNull, ComparisonNotEquals, etc. cannot
			// be correctly represented as simple FDB range scans — they must
			// remain as residual predicates evaluated by the executor.
			if !isScanRangeCompatible(cp.Comparison.Type) {
				continue
			}
			colIdx, found := colToIdx[strings.ToUpper(fv.Field)]
			if !found {
				continue
			}
			// Don't push type-incompatible comparisons into scan ranges.
			// A BIGINT column compared to a string constant must surface
			// as a type mismatch error during predicate evaluation, not
			// silently produce an empty scan range.
			if !comparisonTypesCompatible(fv, &cp.Comparison) {
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

		// The plan is either a RecordQueryIndexPlan (secondary index) or
		// a RecordQueryScanPlan (PK scan from PrimaryScanMatchCandidate),
		// optionally wrapped in a TypeFilterPlan. Handle all cases —
		// Java's ImplementPhysicalScanRule covers both via the shared
		// RecordQueryPlanWithComparisons interface.
		residual := residualPredicates(preds, consumed, prefix, aliases, colToIdx)

		if fetchPlan, ok := idxPlan.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
			if innerIdx, ok := fetchPlan.GetInner().(*plans.RecordQueryIndexPlan); ok {
				idxWrapper := &physicalIndexScanWrapper{plan: innerIdx, columnNames: colNames, unique: cand.IsUnique()}
				fetchQ := expressions.ForEachQuantifier(call.MemoizeExpression(idxWrapper))
				fetchWrapper := NewPhysicalFetchFromPartialRecordWrapper(fetchPlan, fetchQ)
				if len(residual) == 0 {
					call.Yield(fetchWrapper)
				} else {
					filterPlan := plans.NewRecordQueryFilterPlan(residual, fetchPlan)
					innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(fetchWrapper))
					call.Yield(NewPhysicalFilterWrapper(filterPlan, innerQ))
				}
			}
		} else if idxPlanTyped := extractIndexPlan(idxPlan); idxPlanTyped != nil {
			wrapper := &physicalIndexScanWrapper{plan: idxPlanTyped, columnNames: colNames, unique: cand.IsUnique()}
			if len(residual) == 0 {
				call.Yield(wrapper)
			} else {
				filterPlan := plans.NewRecordQueryFilterPlan(residual, idxPlan)
				innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(wrapper))
				call.Yield(NewPhysicalFilterWrapper(filterPlan, innerQ))
			}
		} else if scanPlan := extractScanPlan(idxPlan); scanPlan != nil {
			wrapper := &physicalScanWrapper{plan: scanPlan}
			if len(residual) == 0 {
				call.Yield(wrapper)
			} else {
				filterPlan := plans.NewRecordQueryFilterPlan(residual, scanPlan)
				innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(wrapper))
				call.Yield(NewPhysicalFilterWrapper(filterPlan, innerQ))
			}
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

// isScanRangeCompatible reports whether a ComparisonType can be safely
// pushed into an FDB key-range scan. Simple scalar comparisons
// (=, <, <=, >, >=) and STARTS_WITH (prefix range) map cleanly to
// range bounds. ComparisonIn and others must stay as residual predicates.
func isScanRangeCompatible(t predicates.ComparisonType) bool {
	switch t {
	case predicates.ComparisonEquals,
		predicates.ComparisonLessThan,
		predicates.ComparisonLessThanOrEq,
		predicates.ComparisonGreaterThan,
		predicates.ComparisonGreaterThanEq,
		predicates.ComparisonStartsWith:
		return true
	}
	return false
}

// comparisonTypesCompatible checks whether a field's type and the
// comparison's operand type are compatible for index pushdown. Returns
// false for obvious mismatches like BIGINT column vs STRING literal —
// these must remain as residual predicates so the executor surfaces
// the type error. Unknown types (from unresolved columns) pass through.
func comparisonTypesCompatible(fv *values.FieldValue, cmp *predicates.Comparison) bool {
	if cmp.Operand == nil {
		return true
	}
	fieldType := fv.Type()
	if fieldType == nil || fieldType == values.UnknownType {
		return true
	}
	rhsType := cmp.Operand.Type()
	if rhsType == nil || rhsType == values.UnknownType {
		return true
	}
	// Numeric ↔ String is always a type mismatch.
	fieldIsNum := isNumericType(fieldType)
	rhsIsNum := isNumericType(rhsType)
	fieldIsStr := isStringType(fieldType)
	rhsIsStr := isStringType(rhsType)
	if (fieldIsNum && rhsIsStr) || (fieldIsStr && rhsIsNum) {
		return false
	}
	return true
}

func isNumericType(t values.Type) bool {
	if pt, ok := t.(*values.PrimitiveType); ok {
		return pt.TypeCode.IsNumeric()
	}
	return false
}

func isStringType(t values.Type) bool {
	if pt, ok := t.(*values.PrimitiveType); ok {
		return pt.TypeCode == values.TypeCodeString
	}
	return false
}

// extractScanPlan extracts a *RecordQueryScanPlan from a plan that may
// be a ScanPlan directly or a TypeFilterPlan wrapping one. This handles
// PrimaryScanMatchCandidate.ToScanPlan() which returns either a bare
// ScanPlan or a TypeFilterPlan(ScanPlan).
func extractScanPlan(p plans.RecordQueryPlan) *plans.RecordQueryScanPlan {
	if sp, ok := p.(*plans.RecordQueryScanPlan); ok {
		return sp
	}
	if tfp, ok := p.(*plans.RecordQueryTypeFilterPlan); ok {
		if inner := tfp.GetInner(); inner != nil {
			if sp, ok := inner.(*plans.RecordQueryScanPlan); ok {
				return sp
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

func flattenFilterPredicates(preds []predicates.QueryPredicate) []predicates.QueryPredicate {
	var result []predicates.QueryPredicate
	for _, p := range preds {
		if and, ok := p.(*predicates.AndPredicate); ok {
			result = append(result, flattenFilterPredicates(and.SubPredicates)...)
		} else {
			result = append(result, p)
		}
	}
	return result
}

var _ ExpressionRule = (*ImplementIndexScanRule)(nil)
