package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// OrderedIndexScanRule matches a LogicalSort over a FullUnorderedScan
// (no filter in between) and produces an index scan when an index's
// column order provides the requested sort ordering. The index scan
// has no predicate bounds — it scans the full index but in the
// index's key order, eliminating the sort.
//
//	Sort([col1 ASC, col2 ASC]) over FullUnorderedScan
//	  → IndexScan(full-range, index on (col1, col2, ...))
//
// This complements ImplementIndexScanRule (which requires a Filter).
// When both a predicate and ordering are requested, PushFilterThroughSort
// moves the filter below the sort, and ImplementIndexScanRule handles
// the Filter(Scan) shape. This rule covers the pure ORDER BY case.
type OrderedIndexScanRule struct {
	matcher matching.BindingMatcher
}

func NewOrderedIndexScanRule() *OrderedIndexScanRule {
	return &OrderedIndexScanRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("sort_for_ordered_index"),
	}
}

func (r *OrderedIndexScanRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *OrderedIndexScanRule) OnMatch(call *ExpressionRuleCall) {
	s := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	if s.IsUnsorted() {
		return
	}

	sortKeys := s.GetSortKeys()
	if len(sortKeys) == 0 {
		return
	}

	innerRef := s.GetInner().GetRangesOver()
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

	scanTypes := scan.GetRecordTypes()

	for _, cand := range candidates {
		if !recordTypesOverlap(scanTypes, cand.GetRecordTypes()) {
			continue
		}

		colNames := cand.GetColumnNames()
		if len(colNames) < len(sortKeys) {
			continue
		}

		matches := true
		reverse := false
		for i, sk := range sortKeys {
			fv, ok := sk.Value.(*values.FieldValue)
			if !ok {
				matches = false
				break
			}
			if !eqFold(fv.Field, colNames[i]) {
				matches = false
				break
			}
			if i == 0 {
				reverse = sk.Reverse
			} else if sk.Reverse != reverse {
				matches = false
				break
			}
			if sk.NullsFirst != nil {
				defaultNF := !reverse
				if *sk.NullsFirst != defaultNF {
					matches = false
					break
				}
			}
		}
		if !matches {
			continue
		}

		emptyPrefix := map[values.CorrelationIdentifier]*predicates.ComparisonRange{}
		scanPlan := cand.ToScanPlan(emptyPrefix, reverse)
		idxPlan := extractIndexPlan(scanPlan)
		if idxPlan == nil {
			continue
		}

		wrapper := &physicalIndexScanWrapper{
			plan:        idxPlan,
			columnNames: colNames,
			unique:      cand.IsUnique(),
		}
		call.Yield(wrapper)
	}
}

var _ ExpressionRule = (*OrderedIndexScanRule)(nil)
