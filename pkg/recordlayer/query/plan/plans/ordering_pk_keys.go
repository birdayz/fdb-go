// Portions derived from FoundationDB Record Layer (
// ValueIndexLikeMatchCandidate.java, OrderingProperty.java,
// AggregateIndexMatchCandidate.java, RecordQueryIndexPlan.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package plans

import (
	"slices"

	"fdb.dev/internal/unicode15"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// --- producers --------------------------------------------------------------

// HintOrdering: a primary scan produces rows in primary-key order.
func (p *RecordQueryScanPlan) HintOrdering() properties.Ordering {
	return PKScanOrdering(p)
}

// PKScanOrdering returns a primary scan's PK ordering. Shared with the
// data-access path's plan-backed leaf, which memoizes a SARGed PK scan.
//
// PK positions bound by an equality comparison do not consume a sort
// position: mirrors Java's
// ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons, whose
// equality-bound prefix (i < scanComparisons.getEqualitySize()) only
// populates the binding map with Binding.fixed entries and is never
// appended to orderingSequenceBuilder — the ordering sequence starts at the
// first non-equality-bound key. Without this, a per-binding equality scan
// (id Fixed) and an unbound scan over the same PK columns (id
// Sorted-ascending) report the identical Keys, so plan partitioning
// (expression_partition.go's orderingsEqual) cannot tell them apart and
// co-partitions them. Compare RecordQueryScanPlan.HintRichOrdering below,
// which already carries this distinction via FixedBinding/SortedBinding but
// is not consulted by plain-ordering partitioning.
func PKScanOrdering(plan *RecordQueryScanPlan) properties.Ordering {
	if plan == nil {
		return properties.Ordering{}
	}
	pk := plan.GetPrimaryKeyValues()
	if len(pk) == 0 {
		return properties.Ordering{}
	}
	comps := plan.GetScanComparisons()
	firstNonEq := equalityPrefixLen(comps, len(pk))
	keys := resolveOrderingColumns(pk[firstNonEq:])
	// Terminate the claim at the first coordinate whose physical key order is
	// not its logical order (a FLOAT/DOUBLE primary-key column — see
	// keyCanExtendOrderingClaim). A float PK is reachable: `PRIMARY KEY (e)`
	// with e DOUBLE makes the scan itself claim an order it does not deliver.
	//
	// This truncation is NOT what keeps the sort on such a query, and saying so
	// matters because it decides where the test has to live. MEASURED: deleting
	// this line leaves every plan-shape assertion in
	// embedded_test.TestFloatPrimaryKeyScanClaimsNoOrder green — SQL sort
	// elision on the primary-key path is decided by OrderedPrimaryScanRule's
	// decline. What this line protects is the OTHER consumer of PKScanOrdering:
	// plan partitioning (expression_partition.go's orderingsEqual) and the cost
	// model, which would otherwise co-partition scans that deliver different
	// orders. Its own coverage is therefore direct, at this level —
	// TestPKScanOrdering_FloatPKColumnTerminatesTheClaim — rather than borrowed
	// from a plan-shape test that would stay green without it.
	keys = keys[:claimableKeyLimit(plan.GetFlowedType(), keys)]
	if len(keys) == 0 {
		return properties.Ordering{}
	}
	desc := make([]bool, len(keys))
	if plan.IsReverse() {
		for i := range desc {
			desc[i] = true
		}
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: desc}
}

// keyOrderSplit is how a scan's key order is read off its comparisons: the
// leading coordinates the scan FIXES, and the SORTED tail it delivers in key
// order. One derivation, shared by the value index (plain and rich forms) and
// the aggregate index's grouping key, because a second hand-rolled copy of it
// is how the plain and rich forms once disagreed on the same plan.
//
//   - fixedLen: the leading prefix bound by ANY equality (ownOrderPrefixLen).
//   - pins[i], for i < fixedLen: whether coordinate i is PINNED to one
//     physical key — EqualityPinsSinglePhysicalKeyOnColumn, asked with the
//     coordinate's type so a FLOAT bound by an untyped operand (an IN binding
//     that may be zero at runtime) does not pin. Only a pinned coordinate may
//     be bound FIXED: a fixed binding claims "no order, any requested
//     direction is satisfied", which is true of exactly one physical key. A
//     prefix coordinate that does NOT pin — a signed-zero constant, or a
//     possibly-zero operand on a float column — keeps its OWN order in the
//     scan's direction (the range set opens its blocks in key order), but
//     nothing after it is globally ordered. This is a PER-COORDINATE fact,
//     not a prefix length: under `d = 0.0 AND b = 1` the widened D does not
//     pin, and B still does — every admitted row carries b = 1, one physical
//     key within each of D's blocks — so B binds FIXED and `ORDER BY b DESC`
//     is free over a forward scan. Reading pins as the length of the leading
//     pinned run demoted B to SORTED and cost that plan.
//   - tail: the sorted coordinates after the prefix — the remaining key
//     columns then suffix — truncated at the first FLOAT/DOUBLE (NaN packs
//     into two disjoint blocks, and the tie class it forms leaves every later
//     coordinate unordered within it). Empty when tailDropped.
//   - tailDropped: some prefix coordinate does not pin. The tail is dropped
//     WHOLESALE rather than truncated by type: a later coordinate restarts at
//     each block boundary of the widened prefix coordinate.
//   - untruncated: the tail's length before the float truncation, for callers
//     that must know whether the claim covers the whole storage key.
type keyOrderSplit struct {
	fixedLen    int
	pins        []bool
	tail        []string
	tailDropped bool
	untruncated int
}

func splitKeyOrder(
	comps []*predicates.ComparisonRange,
	keyColumns, suffix []string,
	keyTypes []values.Type, layout values.Type,
	nestedPath func(int) []string, duplicateColumns []bool,
) keyOrderSplit {
	split := keyOrderSplit{fixedLen: ownOrderPrefixLen(comps, len(keyColumns))}
	// A nested leaf's name is not a top-level column of the layout, so it is
	// never asked by name: unresolvable, it could be a float.
	floatNames := keyColumns
	if nestedPath != nil {
		floatNames = slices.Clone(keyColumns)
		for i := range floatNames {
			if nestedPath(i) != nil {
				floatNames[i] = ""
			}
		}
	}
	couldBeFloat := IndexColumnCouldBeFloat(keyTypes, layout, floatNames, duplicateColumns)
	split.pins = make([]bool, split.fixedLen)
	for i := range split.pins {
		split.pins[i] = EqualityPinsSinglePhysicalKeyOnColumn(comps[i], couldBeFloat(i))
		if !split.pins[i] {
			split.tailDropped = true
		}
	}
	if split.tailDropped {
		return split
	}
	tail := make([]string, 0, len(keyColumns)-split.fixedLen+len(suffix))
	tail = append(tail, keyColumns[split.fixedLen:]...)
	tail = append(tail, suffix...)
	split.untruncated = len(tail)
	limit := len(tail)
	for j, name := range tail {
		column := split.fixedLen + j
		var path []string
		if nestedPath != nil && column < len(keyColumns) {
			path = nestedPath(column)
		}
		claims := values.ColumnCanExtendOrderingClaim(layout, name)
		if path != nil {
			claims = nestedColumnCanExtendOrderingClaim(keyTypes, column, layout, path)
		}
		if !claims {
			limit = j
			break
		}
	}
	split.tail = tail[:limit]
	return split
}

// nestedColumnCanExtendOrderingClaim answers the claim question for a nested
// leaf from its physical key type, else from the type its path resolves to in
// the layout; a path that does not resolve cannot claim.
func nestedColumnCanExtendOrderingClaim(keyTypes []values.Type, i int, layout values.Type, path []string) bool {
	if i < len(keyTypes) && keyTypes[i] != nil && keyTypes[i].Code() != values.TypeCodeUnknown {
		return !values.TypeTerminatesOrderingClaim(keyTypes[i])
	}
	record, _ := layout.(*values.RecordType)
	var leaf values.Type
	for _, name := range path {
		if record == nil {
			return false
		}
		ordinal := -1
		for j, field := range record.Fields {
			if unicode15.EqualFold(field.Name, name) {
				if ordinal >= 0 {
					return false
				}
				ordinal = j
			}
		}
		if ordinal < 0 {
			return false
		}
		leaf = record.Fields[ordinal].FieldType
		record, _ = leaf.(*values.RecordType)
	}
	return leaf != nil && !values.TypeTerminatesOrderingClaim(leaf)
}

// HintRichOrdering returns a primary scan's PK ordering with bindings: PK
// positions bound by an equality comparison become FixedBinding entries, the
// rest SortedBinding. A primary scan is a value-index-like candidate in Java
// (PrimaryScanMatchCandidate implements ValueIndexLikeMatchCandidate), so its
// ordering comes from the same computeOrderingFromScanComparisons: the
// equality prefix is Binding.fixed, which is compatible with ANY requested
// direction.
func (p *RecordQueryScanPlan) HintRichOrdering() *properties.RichOrdering {
	if p == nil {
		return properties.EmptyOrdering()
	}
	pk := p.GetPrimaryKeyValues()
	if len(pk) == 0 {
		return properties.EmptyOrdering()
	}
	comps := p.GetScanComparisons()
	prefixLen := equalityPrefixLen(comps, len(pk))
	bm := make(map[values.Value][]properties.OrderingBinding, len(pk))
	keys := make([]values.Value, 0, len(pk))
	dir := properties.ProvidedSortOrderAscending
	if p.IsReverse() {
		dir = properties.ProvidedSortOrderDescending
	}
	resolved := resolveOrderingColumns(pk)
	// Truncate the SORTED tail at the first coordinate whose physical order is
	// not its logical order. The equality-bound prefix is exempt: a FixedBinding
	// pins one physical point and states no order, so a float there is harmless
	// and the columns after it stay claimable.
	limit := prefixLen + claimableKeyLimit(p.GetFlowedType(), resolved[prefixLen:])
	// The ordering covers the WHOLE storage key exactly when nothing was
	// truncated away above and the key's physical uniqueness is also logical
	// uniqueness. Both facts are known here and nowhere else, so both are
	// stamped here rather than re-derived by whoever consumes the ordering.
	storageComplete := limit == len(resolved) &&
		len(p.GetRecordTypes()) == 1 &&
		properties.TupleKeyUniquenessMatchesLogicalEquality(
			p.GetKeyComponentTypes(), len(pk))
	for i, key := range resolved[:limit] {
		keys = append(keys, key)
		if i < prefixLen {
			bm[key] = []properties.OrderingBinding{properties.FixedBinding(comps[i])}
		} else {
			bm[key] = []properties.OrderingBinding{properties.SortedBinding(dir)}
		}
	}
	if len(keys) == 0 {
		return properties.EmptyOrdering()
	}
	ordering := properties.NewRichOrdering(bm, keys, properties.NotDistinct()).
		WithStorageKeyComplete(storageComplete)
	if storageComplete {
		// A primary scan's whole storage key is the primary key.
		ordering = ordering.WithRecordIdentity(keys)
	}
	return ordering
}

// columnCanExtendOrderingClaim / keyCanExtendOrderingClaim are the plans-side
// door to the ONE ordering-claim predicate (values.TypeTerminatesOrderingClaim),
// so no two derivations can classify the same column differently.
//
// Which producers ask, and why the rest need not, is enumerated in this file's
// header.
//
// The rule they enforce: an ordering claim TERMINATES at a coordinate whose
// physical FDB key order is not its logical order. For FLOAT/DOUBLE that is
// NaN — negative NaN payloads pack BEFORE -Inf and positive ones AFTER +Inf,
// while the comparator (values.CompareFloat64, faithful to
// java.lang.Double.compare) collapses every NaN to one value ranked GREATEST.
// So the column is misordered AND every NaN is one logical tie class split
// across two disjoint physical ranges, which leaves any LATER sort column
// unordered within the tie. Hence terminate, not reorder.
//
// Only the SORTED coordinates are subject to this. An equality-bound float
// prefix column is FIXED, not sorted: it pins one physical point, so it makes
// no order claim and the columns after it stay claimable. (The one equality
// that does NOT pin a point — a zero-valued float, which spans both signed
// zeros — is already excluded upstream by equalityPrefixLen.)
// keyCanExtendOrderingClaim is the Value-shaped form, for derivations that
// already hold ordering keys rather than metadata names. A flat field is
// resolved by name against the layout (its declared type is usually
// UnknownType at this point, so the layout is the authority); any other key
// shape is judged on its own type.
func keyCanExtendOrderingClaim(layout values.Type, k values.Value) bool {
	if k == nil {
		return true
	}
	return !values.TypeTerminatesOrderingClaim(k.Type())
}

// claimableKeyLimit returns how many leading keys may be claimed as an
// ordering against layout — the index of the first key that terminates the
// claim, or len(keys) when none does.
func claimableKeyLimit(layout values.Type, keys []values.Value) int {
	for i, k := range keys {
		if !keyCanExtendOrderingClaim(layout, k) {
			return i
		}
	}
	return len(keys)
}

// claimableTypedKeyLimit is claimableKeyLimit for keys that carry their OWN
// declared type, with no flowed layout to resolve against.
//
// It cannot go through keyCanExtendOrderingClaim: that one resolves a flat
// FieldValue by NAME against the layout, because a scan's ordering keys are
// minted with UnknownType and the layout is the only authority. A grouping key
// arrives from the translator already typed, and there is no layout — so
// routing it through the name path with a nil layout would ask a question that
// always answers "not a float" and would silently claim every ordering.
//
// It is the plans-side door to values.ClaimableTypedKeyPrefix, which the
// planner's StreamingAggFromIndexRule also asks — for a different reason (input
// CLUSTERING, not advertised order) but off the same predicate, so the rule
// that BUILDS the plan and the derivation that DESCRIBES it cannot classify the
// same grouping key differently.
func claimableTypedKeyLimit(keys []values.Value) int {
	return values.ClaimableTypedKeyPrefix(keys)
}

// orderingColumnOfName mints an ordering key for a METADATA column name
// resolved against the layout the plan flows.
//
// This is RFC-197's one sanctioned direction for a name: the index's column
// list names its columns, the name is resolved ONCE against the row type the
// scan produces, and it dies there — every consumer downstream compares the
// ORDINAL and the layout token, never the spelling. The display name rides
// along for Explain only.
//
// A layout with no declared column order (a multi-record-type index's degraded
// row type, UnknownType) or a name the layout does not declare yields a LAZY
// key, which no identity-keyed consumer can address. That is the fail-closed
// direction: the ordering claim is dropped, costing a sort, rather than being
// stated in terms nothing can verify.
func orderingColumnOfName(root values.Value, layout values.Type, name string) values.Value {
	if ident, ok := values.OrdinalOfNameIn(layout, name); ok {
		request, err := values.FieldByNameAndOrdinal(name, ident.Ordinal)
		if err != nil {
			return nil
		}
		resolved, err := values.ResolveFieldAccess(root, []values.FieldRequest{request})
		if err != nil {
			return nil
		}
		return resolved
	}
	return nil
}

// orderingColumnOfPath is orderingColumnOfName for a nested leaf: each step
// resolved by its unique case-insensitive name in the record it descends.
func orderingColumnOfPath(root values.Value, layout values.Type, path []string) values.Value {
	record, _ := layout.(*values.RecordType)
	requests := make([]values.FieldRequest, len(path))
	for i, name := range path {
		if record == nil {
			return nil
		}
		ordinal := -1
		for j, field := range record.Fields {
			if unicode15.EqualFold(field.Name, name) {
				if ordinal >= 0 {
					return nil
				}
				ordinal = j
			}
		}
		if ordinal < 0 {
			return nil
		}
		request, err := values.FieldByNameAndOrdinal(name, ordinal)
		if err != nil {
			return nil
		}
		requests[i] = request
		record, _ = record.Fields[ordinal].FieldType.(*values.RecordType)
	}
	resolved, err := values.ResolveFieldAccess(root, requests)
	if err != nil {
		return nil
	}
	return resolved
}

// resolveOrderingColumns re-mints an already-Value-shaped key list against the
// layout the plan flows, so a key that arrived as a LAZY flat field carries its
// ordinal and layout token like every other provided key.
//
// A key that is not a bare flat field (a RecordTypeValue constant, a nested or
// composite component) is passed through untouched: it has no single-ordinal
// identity to state, and inventing one is the ordinal conflation the domain
// token exists to prevent. An ALREADY-baked key is left alone — re-resolving it
// by name would undo whatever producer knew better.
func resolveOrderingColumns(keys []values.Value) []values.Value {
	out := make([]values.Value, len(keys))
	copy(out, keys)
	return out
}

// TrimmedPKSuffix returns the primary-key columns not already present in the
// index key columns, in PK order. Ports Java's Index.trimPrimaryKey semantics
// as used by ValueIndexExpansionVisitor.fullKey: PK components that appear in
// the index key are trimmed, the remainder is appended after the index key.
func TrimmedPKSuffix(columnNames, pkColumnNames []string) []string {
	if len(pkColumnNames) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(columnNames))
	for _, c := range columnNames {
		seen[c] = struct{}{}
	}
	suffix := make([]string, 0, len(pkColumnNames))
	for _, col := range pkColumnNames {
		if _, dup := seen[col]; dup {
			continue
		}
		seen[col] = struct{}{}
		suffix = append(suffix, col)
	}
	return suffix
}
