package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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

		// Match each sort key against the candidate's i-th column Value by
		// Value-tree equality (alias-invariant), mirroring the predicate path
		// (valuesMatchColumn). A candidate that supplies per-column Values
		// (columnValueProvider) lets a function-keyed column — e.g.
		// CardinalityValue(FieldValue(arr)) for an `ORDER BY CARDINALITY(arr)`
		// over a CARDINALITY() index — bind to the index order, not just a
		// bare FieldValue. Candidates without a provider (primary scan) keep
		// the historical FieldValue-name string comparison.
		provider, _ := cand.(columnValueProvider)
		matches := true
		reverse := false
		for i, sk := range sortKeys {
			if !sortKeyMatchesColumn(sk.Value, provider, i, colNames[i]) {
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

		// The index scan is its own cascades expression now (RFC-184 W2) — a bare
		// leaf carrying its index metadata (columns/pk/unique) on the plan.
		call.Yield(idxPlan.WithIndexMetadata(colNames, candidatePKColumns(cand), cand.IsUnique()))
	}
}

// sortKeyMatchesColumn reports whether a sort key's Value binds to the i-th
// index key column. Both branches route through valuesMatchColumn (the
// match-domain name-path comparison): the sort key's full accessor path, not
// its leaf name, so a nested sort key `addr.city` never binds a same-leaf-named
// top-level `city` column. When the candidate supplies per-column Values it
// compares against ColumnValue(i, base) — this is how a CardinalityValue sort
// key binds to a CARDINALITY()-keyed column. Without a provider (primary scan)
// the candidate column is a plain FieldValue built from the declared column name
// (base alias irrelevant — the comparison is alias-invariant at the root).
func sortKeyMatchesColumn(skValue values.Value, provider columnValueProvider, i int, colName string) bool {
	base := values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
	var colValue values.Value
	if provider != nil {
		colValue = provider.ColumnValue(i, base)
	} else {
		colValue = values.NewFieldValue(base, colName, values.UnknownType)
	}
	return valuesMatchColumn(skValue, colValue)
}

var _ ExpressionRule = (*OrderedIndexScanRule)(nil)
