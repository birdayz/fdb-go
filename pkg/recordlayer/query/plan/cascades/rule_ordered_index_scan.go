package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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
		// This shortcut replaces the base-record scan with a raw full-range
		// index scan and has no data-access compensation above it. A fan-out
		// candidate can emit a base record multiple times (and omit records
		// whose repeated field is empty), so it cannot preserve the Sort's
		// input cardinality.
		if !candidatePreservesBaseRecordCardinality(cand) {
			continue
		}
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
		valueCand, _ := cand.(*ValueIndexScanMatchCandidate)
		matches := true
		reverse := false
		for i, sk := range sortKeys {
			// The column's PHYSICAL entry direction under a forward scan:
			// tuple-natural ascending (nulls first) for a plain column, the
			// wrapper's direction for an order-function column — whose entry
			// bytes ARE that direction's encoding
			// (OrderFunctionKeyExpression.evaluateFunction → TupleOrdering.pack).
			colDir := values.OrderedBytesAscNullsFirst
			isOrderColumn := false
			if valueCand != nil && i < len(valueCand.columnFunctions) {
				if dir, isOrder := OrderFunctionDirection(valueCand.columnFunctions[i]); isOrder {
					colDir = dir
					isOrderColumn = true
				}
			}
			// An order-wrapped column ORDERS BY its underlying field; the
			// wrapper carries only encoding + direction, which colDir accounts
			// for — so the sort key matches the BARE field, never the
			// ToOrderedBytes value (Java: OrderingValueComputationRuleSet
			// simplifies ToOrderedBytesValue(fv) to (fv, direction)).
			matchProvider := provider
			if isOrderColumn {
				matchProvider = nil
			}
			if !sortKeyMatchesColumn(sk.Value, matchProvider, i, colNames[i]) {
				matches = false
				break
			}
			// Binding a sort key to an index column proves the scan visits
			// that column's keys in KEY order, not in VALUE order. For a
			// FLOAT/DOUBLE column the two differ: NaN payloads pack into two
			// disjoint blocks (negative NaN before -Inf, positive NaN after
			// +Inf) while the comparator collapses them to one value ranked
			// greatest. Eliding the sort here is a wrong-answer bug, so
			// decline and let a materialized sort serve the query.
			if !values.ColumnCanExtendOrderingClaim(scan.GetFlowedType(), colNames[i]) {
				matches = false
				break
			}
			// The requested direction for this key: DESC defaults NULLS LAST,
			// ASC defaults NULLS FIRST; an explicit NULLS choice may run
			// counterflow.
			reqDir := requestedOrderedBytesDirection(sk.Reverse, sk.NullsFirst)
			// One scan direction must serve every key: forward when the
			// request equals the column's physical direction, reverse when it
			// equals its byte-order flip (direction AND null placement flip
			// together), no match otherwise.
			var needReverse bool
			switch reqDir {
			case colDir:
				needReverse = false
			case flipOrderedBytesDirection(colDir):
				needReverse = true
			default:
				matches = false
			}
			if !matches {
				break
			}
			if i == 0 {
				reverse = needReverse
			} else if needReverse != reverse {
				matches = false
				break
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

		// Name-matching the sort keys against the index columns proves the scan
		// visits those columns, NOT that it visits them in the order the query
		// asked for. The two differ on a raw FLOAT/DOUBLE key coordinate, whose
		// physical tuple order puts the negative NaNs below -Inf while the
		// comparator makes every NaN greatest. Ask the plan how much of its key
		// it genuinely returns in logical order — which also keeps an aggregate
		// index, that cannot enumerate the NaN blocks at all, from claiming it.
		// The index scan is its own cascades expression now (RFC-184 W2) — a bare
		// leaf carrying its index metadata (columns/pk/unique/fan-out) on the plan.
		stamped := stampIndexMetadata(cand, idxPlan)

		// Ask the STAMPED plan — before stamping it does not yet know its key
		// columns and would report a zero-length ordered prefix for everything.
		if len(sortKeys) > stamped.OrderedKeyColumnPrefixLen() {
			continue
		}
		call.Yield(stamped)
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

// requestedOrderedBytesDirection expresses a sort key's requested direction in
// TupleOrdering terms: DESC defaults NULLS LAST and ASC defaults NULLS FIRST
// (SQL's defaults, ParseHelpers.isNullsLast); an explicit NULLS choice may run
// counterflow.
func requestedOrderedBytesDirection(reverse bool, nullsFirst *bool) values.OrderedBytesDirection {
	nf := !reverse
	if nullsFirst != nil {
		nf = *nullsFirst
	}
	switch {
	case !reverse && nf:
		return values.OrderedBytesAscNullsFirst
	case !reverse && !nf:
		return values.OrderedBytesAscNullsLast
	case reverse && !nf:
		return values.OrderedBytesDescNullsLast
	default:
		return values.OrderedBytesDescNullsFirst
	}
}

// flipOrderedBytesDirection is the byte-order reversal of a direction:
// reading the encoded entries backwards flips the direction AND the null
// placement together (ASC_NULLS_FIRST ↔ DESC_NULLS_LAST,
// ASC_NULLS_LAST ↔ DESC_NULLS_FIRST) — Java TupleOrdering.Direction's
// reverseDirection pairing.
func flipOrderedBytesDirection(d values.OrderedBytesDirection) values.OrderedBytesDirection {
	switch d {
	case values.OrderedBytesAscNullsFirst:
		return values.OrderedBytesDescNullsLast
	case values.OrderedBytesAscNullsLast:
		return values.OrderedBytesDescNullsFirst
	case values.OrderedBytesDescNullsFirst:
		return values.OrderedBytesAscNullsLast
	default:
		return values.OrderedBytesAscNullsFirst
	}
}

// orderedFullScanAlternatives applies the existing ordered-secondary-index and
// ordered-primary-scan rules to a requested ordering that has been propagated
// to a bare scan group. A FlatMap child receives an ordering constraint rather
// than a synthetic LogicalSortExpression, while both ordered scan rules match
// the latter. Building that expression in a private reference lets the join
// boundary reuse the exact same eligibility checks (record types, fan-out,
// function keys, direction, and NULL placement) without mutating the shared
// scan group.
func orderedFullScanAlternatives(
	ref *expressions.Reference,
	requested *properties.RequestedOrdering,
	ctx PlanContext,
) []expressions.RelationalExpression {
	if ref == nil || requested == nil || requested.IsPreserve() || ctx == nil {
		return nil
	}
	scanRef := ref
	if findFullScan(scanRef) == nil {
		// An existential FlatMap can be assembled after its bare source group
		// has been reduced to the physical forward primary scan. Recreate only
		// the equivalent unbounded logical scan as a private rule input so a
		// DESC request can obtain the reverse primary alternative. A bounded
		// scan is not a full-scan equivalent and must decline here.
		var physicalScan *plans.RecordQueryScanPlan
		for _, member := range ref.AllMembers() {
			scan, ok := member.(*plans.RecordQueryScanPlan)
			if !ok || len(scan.GetScanComparisons()) != 0 {
				continue
			}
			physicalScan = scan
			break
		}
		if physicalScan == nil {
			return nil
		}
		scanRef = expressions.InitialOf(
			expressions.NewFullUnorderedScanExpression(
				physicalScan.GetRecordTypes(),
				physicalScan.GetFlowedType(),
			),
		)
	}

	parts := requested.GetParts()
	sortKeys := make([]expressions.SortKey, len(parts))
	for i, part := range parts {
		if !part.SortOrder.IsDirectional() {
			return nil
		}
		sortKeys[i] = expressions.SortKey{
			Value:   part.Value,
			Reverse: part.SortOrder.IsAnyDescending(),
		}
		if part.SortOrder.IsCounterflowNulls() {
			nullsFirst := part.SortOrder ==
				properties.RequestedSortOrderDescendingNullsFirst
			sortKeys[i].NullsFirst = &nullsFirst
		}
	}

	privateSortRef := expressions.InitialOf(
		expressions.NewLogicalSortExpression(
			sortKeys,
			expressions.ForEachQuantifier(scanRef),
		),
	)
	var result []expressions.RelationalExpression
	result = append(result, FireExpressionRuleWithMemo(
		NewOrderedIndexScanRule(), privateSortRef, ctx, nil)...)
	result = append(result, FireExpressionRuleWithMemo(
		NewOrderedPrimaryScanRule(), privateSortRef, ctx, nil)...)
	return result
}

var _ ExpressionRule = (*OrderedIndexScanRule)(nil)
