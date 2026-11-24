// Portions derived from FoundationDB Record Layer (
// ValueIndexLikeMatchCandidate.java, OrderingProperty.java,
// AggregateIndexMatchCandidate.java, RecordQueryIndexPlan.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package plans

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// HintOrdering: an index scan produces rows in index-key order for the
// non-equality-bound suffix columns, extended by the trimmed primary-key
// suffix (index entries are (index key, primary key), so the PK columns
// continue the sort order). E.g. index(a, b, c) with a = 1 over PK (id)
// produces output sorted by (b, c, id). Mirrors the full-key ordering of
// Java's ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons.
func (p *RecordQueryIndexPlan) HintOrdering() properties.Ordering {
	if p == nil || len(p.GetColumnNames()) == 0 {
		return properties.Ordering{}
	}
	if !p.orderingKeyNamesKnown || !p.orderingKeyNamesSafe {
		return properties.Ordering{}
	}
	columnNames, suffix, _, ok := p.orderingKeyColumns()
	if !ok {
		return properties.Ordering{}
	}
	rev := p.IsReverse()
	split := splitKeyOrder(p.GetScanComparisons(), columnNames, suffix, p.GetKeyComponentTypes(), p.GetFlowedType(),
		p.NestedKeyColumnPath, p.keyColumnDuplicates)
	sorted := split.tail
	if len(sorted) == 0 {
		return properties.Ordering{}
	}
	keys := make([]values.Value, 0, len(sorted))
	desc := make([]bool, 0, len(sorted))
	nullsFirst := make([]bool, 0, len(sorted))
	for j, col := range sorted {
		key := p.orderingKeyOf(split.fixedLen+j, col)
		if key == nil {
			return properties.Ordering{}
		}
		keys = append(keys, key)
		order := properties.ProvidedSortOrderAscending
		if rev {
			order = properties.ProvidedSortOrderDescending
		}
		if column := split.fixedLen + j; column < len(columnNames) {
			order = p.columnSortOrder(column)
		}
		desc = append(desc, order == properties.ProvidedSortOrderDescending ||
			order == properties.ProvidedSortOrderDescendingNullsFirst)
		nullsFirst = append(nullsFirst, order == properties.ProvidedSortOrderAscending ||
			order == properties.ProvidedSortOrderDescendingNullsFirst)
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: desc, NullsFirst: nullsFirst}
}

// HintRichOrdering returns the index scan's full ordering with bindings:
// equality-bound prefix columns become FixedBinding entries (carrying the
// comparison), non-equality suffix columns become SortedBinding entries. The
// trimmed primary-key suffix continues the sorted keys — this is what lets an
// equality-prefixed scan (status = ?) satisfy ORDER BY pk, exactly as Java's
// ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons derives the
// ordering over getFullKeyExpression() (index key + trimmed PK) with
// Binding.fixed for the equality prefix and Binding.sorted for the rest.
//
// Note this differs from HintOrdering, which DROPS the equality prefix
// entirely; here the prefix is retained as fixed, which is strictly more
// information.
func (p *RecordQueryIndexPlan) HintRichOrdering() *properties.RichOrdering {
	if p == nil || p.orderingCache == nil {
		return p.hintRichOrdering()
	}
	if entry := p.orderingCache.entry.Load(); entry != nil && entry.owner == p {
		return entry.ordering
	}
	ordering := p.hintRichOrdering()
	p.orderingCache.entry.Store(&indexOrderingEntry{owner: p, ordering: ordering})
	return ordering
}

func (p *RecordQueryIndexPlan) hintRichOrdering() *properties.RichOrdering {
	if p == nil || len(p.GetColumnNames()) == 0 {
		return properties.EmptyOrdering()
	}
	if !p.orderingKeyNamesKnown || !p.orderingKeyNamesSafe {
		return properties.EmptyOrdering()
	}
	columnNames, suffix, fixedSkip, ok := p.orderingKeyColumns()
	if !ok {
		return properties.EmptyOrdering()
	}
	pkColumnNames := p.GetPKColumnNames()
	comps := p.GetScanComparisons()
	bm := make(map[values.Value][]properties.OrderingBinding)
	keys := make([]values.Value, 0, len(columnNames)+len(pkColumnNames))

	dir := properties.ProvidedSortOrderAscending
	if p.IsReverse() {
		dir = properties.ProvidedSortOrderDescending
	}
	// Same split as the plain form (splitKeyOrder): the equality-bound prefix
	// is retained here rather than dropped, and the SORTED tail — the remaining
	// index columns plus the trimmed PK suffix — is truncated at the first
	// FLOAT/DOUBLE. A coordinate bound by an equality spanning BOTH signed zeros
	// belongs in that leading prefix too — but as a SORTED entry, not a FIXED
	// one. Its ground is RFC-208's range set opening the two zero blocks in KEY
	// ORDER (reversed wholesale under a reverse scan), NOT that it admits one
	// logical value: it admits two distinct sort values, so `ORDER BY` on it is
	// satisfied only in the direction the scan runs. The loop below binds it
	// accordingly; EqualityBoundCoordinateClaimsOwnOrder explains why tie-class
	// vacuity does not apply.
	split := splitKeyOrder(comps, columnNames, suffix,
		p.GetKeyComponentTypes(), p.GetFlowedType(), p.NestedKeyColumnPath, p.keyColumnDuplicates)
	tail := split.tail
	// The coordinates below are the whole storage key exactly when the tail was
	// neither dropped wholesale (a signed-zero equality that restarts the order
	// at a block boundary) nor truncated at a FLOAT, AND the index key and its
	// primary-key suffix both make physical uniqueness mean logical uniqueness.
	// Every one of those facts is settled right here; stamping it is what keeps
	// a consumer from asking a second property to agree with this one by hand.
	storageComplete := fixedSkip == nil && !split.tailDropped && len(tail) == split.untruncated &&
		len(p.GetRecordTypes()) == 1 &&
		properties.TupleKeyUniquenessMatchesLogicalEquality(
			p.GetKeyComponentTypes(), len(columnNames)) &&
		properties.TupleKeyUniquenessMatchesLogicalEquality(
			p.GetPrimaryKeyComponentTypes(), len(pkColumnNames))
	for i, col := range columnNames[:split.fixedLen] {
		if i < len(fixedSkip) && fixedSkip[i] {
			continue
		}
		key := p.orderingKeyOf(i, col)
		if key == nil {
			return properties.EmptyOrdering()
		}
		keys = append(keys, key)
		// FIXED means "states no order, so ANY requested direction is
		// satisfied". That is only true of a coordinate pinned to ONE physical
		// key, where every admitted row shares one SORT value — the
		// column-aware question splitKeyOrder already asked, per coordinate
		// (pins), never the operand-only one: a FLOAT coordinate bound by an
		// untyped non-constant operand (an IN binding) is not provably
		// nonzero, widens at runtime across both signed-zero blocks, and
		// binding it FIXED would let a request in the opposite direction elide
		// its sort.
		//
		// A signed-zero-widened equality is not such a coordinate either. The
		// PREDICATE comparator makes -0.0 and +0.0 equal, which is why the
		// equality admits both; the SORT comparator (values.CompareFloat64,
		// faithful to java.lang.Double.compare) ranks -0.0 BELOW +0.0, which is
		// why they are two distinct ORDER BY values. The coordinate is ordered,
		// but only in the direction the scan runs — so it binds SORTED, not
		// FIXED. Calling it FIXED elides the sort on `WHERE z = 0.0 ORDER BY z
		// DESC` and answers it from a FORWARD scan, returning the two zero
		// blocks ascending.
		if split.pins[i] {
			bm[key] = []properties.OrderingBinding{properties.FixedBinding(comps[i])}
		} else {
			bm[key] = []properties.OrderingBinding{properties.SortedBinding(p.columnSortOrder(i))}
		}
	}
	// The primary-key suffix of a key whose primary key begins with the
	// record-type coordinate lies past that coordinate in the entry: Java's
	// full key is (index key, record type, trimmed primary key), and its match
	// ordering stops at the record-type part, which no comparison binds. The
	// ordering claims these keys as before; the mark only tells the in-union
	// rule that the target never builds a plan from them (WS-F 4.3 item 2).
	var pastHorizon []values.Value
	recordTypeHorizon := p.primaryKeyBeginsWithRecordType()
	for j, col := range tail {
		key := p.orderingKeyOf(split.fixedLen+j, col)
		if key == nil {
			return properties.EmptyOrdering()
		}
		keys = append(keys, key)
		order := dir
		if column := split.fixedLen + j; column < len(columnNames) {
			order = p.columnSortOrder(column)
		} else if recordTypeHorizon {
			pastHorizon = append(pastHorizon, key)
		}
		bm[key] = []properties.OrderingBinding{properties.SortedBinding(order)}
	}
	if len(keys) == 0 {
		return properties.EmptyOrdering()
	}
	// Java passes RecordQueryIndexPlan.isStrictlySorted(), not the index
	// metadata's UNIQUE bit. A unique index is only a distinct ordering once
	// the chosen ordering keys cover that uniqueness proof; RemoveSort marks
	// that exact plan strictly sorted after checking the coverage.
	return properties.NewRichOrdering(bm, keys,
		properties.DistinctOverAllKeysIf(p.IsStrictlySorted())).
		WithStorageKeyComplete(storageComplete).
		WithRecordIdentity(p.recordIdentityCoordinates()).
		WithPastRecordTypeHorizon(pastHorizon)
}

// primaryKeyBeginsWithRecordType reports whether the scanned records' primary
// key starts with the record-type coordinate, as a relational table's does.
func (p *RecordQueryIndexPlan) primaryKeyBeginsWithRecordType() bool {
	pk := p.GetCommonPrimaryKeyValues()
	if len(pk) == 0 {
		return false
	}
	_, isRecordType := pk[0].(*values.RecordTypeValue)
	return isRecordType
}

// recordIdentityCoordinates are the primary-key coordinates of a scan that
// flows each record of one type at most once, or nil. The merge dedups compare
// them with the sort comparator, so the key types must make that comparison
// record identity.
func (p *RecordQueryIndexPlan) recordIdentityCoordinates() []values.Value {
	pkColumnNames := p.GetPKColumnNames()
	if !(p.ProducesDistinctRecords() || p.fanOutPinned()) || len(p.GetRecordTypes()) != 1 || len(pkColumnNames) == 0 ||
		!properties.TupleKeyUniquenessMatchesLogicalEquality(p.GetPrimaryKeyComponentTypes(), len(pkColumnNames)) {
		return nil
	}
	pk := make([]values.Value, 0, len(pkColumnNames))
	for _, name := range pkColumnNames {
		key := orderingColumnOfName(p.GetResultValue(), p.GetFlowedType(), name)
		if key == nil {
			return nil
		}
		pk = append(pk, key)
	}
	return pk
}

// injectiveKeyColumnNames are the key columns whose entry determines a
// top-level field, so a primary-key column of the same name is not stored
// again: all but a function column and a nested leaf, as Java trims the
// primary key by key-expression equality.
func (p *RecordQueryIndexPlan) injectiveKeyColumnNames() []string {
	names := make([]string, 0, len(p.GetColumnNames()))
	for i, name := range p.GetColumnNames() {
		if i < len(p.keyColumnDuplicates) && p.keyColumnDuplicates[i] {
			continue
		}
		if i < len(p.orderingColumns) && (p.orderingColumns[i].Cardinality || p.orderingColumns[i].Key != nil) {
			continue
		}
		if p.NestedKeyColumnPath(i) != nil {
			continue
		}
		names = append(names, name)
	}
	return names
}

// orderingKeyColumns skips physically pinned fan-out elements, stopping at
// unbound/range elements; unknown fan-out classification fails closed.
func (p *RecordQueryIndexPlan) orderingKeyColumns() (columns, suffix []string, skip []bool, ok bool) {
	columns = p.GetColumnNames()
	if !p.distinctRecordsKnown || !p.createsDuplicates {
		return columns, TrimmedPKSuffix(p.injectiveKeyColumnNames(), p.GetPKColumnNames()), nil, true
	}
	if len(p.keyColumnDuplicates) != len(columns) {
		return nil, nil, nil, false
	}
	comps := p.GetScanComparisons()
	prefix := ownOrderPrefixLen(comps, len(columns))
	couldBeFloat := IndexColumnCouldBeFloat(p.GetKeyComponentTypes(), p.GetFlowedType(), columns, p.keyColumnDuplicates)
	limit := len(columns)
	for i, duplicate := range p.keyColumnDuplicates {
		if duplicate && (i >= prefix || !EqualityPinsSinglePhysicalKeyOnColumn(comps[i], couldBeFloat(i))) {
			limit = i
			break
		}
	}
	if limit == len(columns) {
		suffix = TrimmedPKSuffix(p.injectiveKeyColumnNames(), p.GetPKColumnNames())
	}
	return columns[:limit], suffix, p.keyColumnDuplicates, true
}

// fanOutPinned requires one physical key per fan-out coordinate, so entries
// for two different element values cannot both match a record.
func (p *RecordQueryIndexPlan) fanOutPinned() bool {
	columns := p.GetColumnNames()
	if !p.distinctRecordsKnown || !p.createsDuplicates || len(p.keyColumnDuplicates) != len(columns) {
		return false
	}
	comps := p.GetScanComparisons()
	prefix := ownOrderPrefixLen(comps, len(columns))
	couldBeFloat := IndexColumnCouldBeFloat(p.GetKeyComponentTypes(), p.GetFlowedType(), columns, p.keyColumnDuplicates)
	for i, duplicate := range p.keyColumnDuplicates {
		if !duplicate {
			continue
		}
		if i >= prefix || !EqualityPinsSinglePhysicalKeyOnColumn(comps[i], couldBeFloat(i)) {
			return false
		}
	}
	return true
}

// orderingKeyOf is the value key column i (or, past the key, the primary-key
// column name) orders the scan by.
func (p *RecordQueryIndexPlan) orderingKeyOf(i int, name string) values.Value {
	if i < len(p.GetColumnNames()) && i < len(p.orderingColumns) && p.orderingColumns[i].Key != nil {
		column := p.orderingColumns[i]
		root := p.GetResultValue()
		translation := values.NewTranslationMapBuilder().
			When(column.KeyRoot).
			Then(func(_ values.CorrelationIdentifier, _ values.Value) values.Value { return root }).
			Build()
		key, err := values.TranslateCorrelationsChecked(column.Key, translation)
		if err != nil {
			return nil
		}
		return key
	}
	var key values.Value
	if path := p.NestedKeyColumnPath(i); path != nil && i < len(p.GetColumnNames()) {
		key = orderingColumnOfPath(p.GetResultValue(), p.GetFlowedType(), path)
	} else {
		key = orderingColumnOfName(p.GetResultValue(), p.GetFlowedType(), name)
	}
	if key != nil && i < len(p.GetColumnNames()) && i < len(p.orderingColumns) && p.orderingColumns[i].Cardinality {
		return values.NewCardinalityValue(key)
	}
	return key
}

// columnSortOrder is the provided order of key column i under this scan's
// direction; a reverse scan flips direction and null placement together.
func (p *RecordQueryIndexPlan) columnSortOrder(i int) properties.ProvidedSortOrder {
	order := properties.ProvidedSortOrderAscending
	if i < len(p.orderingColumns) {
		switch p.orderingColumns[i].Direction {
		case values.OrderedBytesAscNullsLast:
			order = properties.ProvidedSortOrderAscendingNullsLast
		case values.OrderedBytesDescNullsFirst:
			order = properties.ProvidedSortOrderDescendingNullsFirst
		case values.OrderedBytesDescNullsLast:
			order = properties.ProvidedSortOrderDescending
		}
	}
	if !p.IsReverse() {
		return order
	}
	switch order {
	case properties.ProvidedSortOrderAscending:
		return properties.ProvidedSortOrderDescending
	case properties.ProvidedSortOrderDescending:
		return properties.ProvidedSortOrderAscending
	case properties.ProvidedSortOrderAscendingNullsLast:
		return properties.ProvidedSortOrderDescendingNullsFirst
	default:
		return properties.ProvidedSortOrderAscendingNullsLast
	}
}

// HintOrdering: an aggregate index is stored grouped, so it emits one row per
// group in group-column KEY order — which is the group's VALUE order only for
// coordinates whose tuple encoding is order-preserving under the comparator.
// The claim is therefore truncated at the first FLOAT/DOUBLE grouping column,
// by the same authority every other producer here asks: a negative-NaN group
// is the physically first row and the logically last one, and the NaN groups
// form one tie class split across two disjoint physical ranges, so no later
// grouping column is ordered within it either.
//
// The grouping columns are NAMES, so answering this needs a layout — carried
// as groupColLayout from the match candidate's base record type. With none
// (a multi-record-type index), the predicate's fail-open direction applies and
// the claim stands, exactly as it does for an untyped index scan.
//
// A grouping prefix the scan binds by EQUALITY contributes no sort position:
// every emitted group shares that value, so the group order is the order of
// the columns AFTER it, and `WHERE b = 1 GROUP BY b, a ORDER BY a` is served
// by the scan as it stands. Java's AggregateIndexMatchCandidate.
// computeOrderingFromScanComparisons is the reference — Binding.fixed for
// i < scanComparisons.getEqualitySize(), and the ordering sequence starting
// there — and RecordQueryIndexPlan.HintOrdering is the value-index twin of the
// same split. The prefix/tail rules (a signed-zero equality keeps its own order
// but drops the tail; the tail is truncated at the first FLOAT/DOUBLE) are the
// index plan's, applied to the grouping key.
func (p *RecordQueryAggregateIndexPlan) HintOrdering() properties.Ordering {
	groupCols, ordinals, keyTypes := p.aggregateOrderingColumns()
	if len(groupCols) == 0 {
		return properties.Ordering{IsKnown: len(p.groupCols) == 0}
	}
	split := p.aggregateOrderSplit(groupCols, ordinals, keyTypes)
	fixedLen, sortedCols := split.fixedLen, split.tail
	if len(sortedCols) == 0 {
		if fixedLen == len(groupCols) && !split.tailDropped && (!p.permuted || len(groupCols) == len(p.groupCols)+1) {
			// Every grouping column is pinned to one physical key: at most one
			// group flows, which is ordered by anything.
			return properties.Ordering{IsKnown: true}
		}
		return properties.Ordering{IsKnown: false}
	}
	// Grouping column i IS slot i of the row this plan flows
	// ([groupCols..., FUNC(col)] — aggregateIndexCursor's layout, the row the
	// plan's result type states), so the ordering key states that ordinal and the
	// layout it indexes. The streaming-aggregation provider states the same
	// thing about its own output row; both must, because a requested ORDER BY
	// key on an aggregate output is baked against that row and an ordinal with
	// no domain is one no consumer may compare.
	keys := make([]values.Value, 0, len(sortedCols))
	desc := make([]bool, 0, len(sortedCols))
	for i := range sortedCols {
		key := p.groupingOrderingKey(ordinals[fixedLen+i])
		if key == nil {
			return properties.Ordering{IsKnown: false}
		}
		keys = append(keys, key)
		desc = append(desc, p.IsReverse())
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: desc}
}

// groupingScanComparisons is the scan's bound prefix over the physical group
// key, positionally aligned with groupCols. A plan built without an index scan
// (struct-literal test plans) binds nothing.
func (p *RecordQueryAggregateIndexPlan) groupingScanComparisons() []*predicates.ComparisonRange {
	if p.indexPlan == nil {
		return nil
	}
	return p.indexPlan.GetScanComparisons()
}

// aggregateOrderSplit applies physical key ordering to output coordinates;
// ordinals[i] is the output slot of coordinate i.
func (p *RecordQueryAggregateIndexPlan) aggregateOrderSplit(cols []string, ordinals []int, types []values.Type) keyOrderSplit {
	layout := p.GetGroupColumnLayout()
	nestedPath := func(i int) []string { return p.nestedGroupColumnPath(ordinals[i]) }
	if p.permuted {
		// The permuted coordinates are named by the result row's slots.
		layout, nestedPath = p.GetResultType(), nil
	}
	return splitKeyOrder(p.groupingScanComparisons(), cols, nil, types, layout, nestedPath, nil)
}

// aggregateOrderingColumns maps physical key coordinates to logical output
// ordinals, as AggregateIndexMatchCandidate.indexWithPermutation does in Java.
func (p *RecordQueryAggregateIndexPlan) aggregateOrderingColumns() ([]string, []int, []values.Type) {
	cols := p.GetGroupCols()
	types := p.GetKeyComponentTypes()
	ordinals := make([]int, len(cols))
	for i := range ordinals {
		ordinals[i] = i
	}
	if !p.permuted {
		return cols, ordinals, types
	}
	row, ok := p.GetResultType().(*values.RecordType)
	if !ok || len(row.Fields) != len(cols)+1 {
		return nil, nil, nil
	}
	prefix := p.GetPhysicalGroupingPrefixCount()
	if prefix < 0 || prefix > len(cols) {
		return nil, nil, nil
	}
	order := append([]int(nil), ordinals[:prefix]...)
	// SQL MIN repairs stored NULL extrema by looking up a non-NULL value.
	// That changes the key's aggregate coordinate, so only its preceding
	// grouping prefix is ordered when the operand can contain NULLs.
	aggregateSorted := true
	if p.aggregateFunction == "MIN" {
		if field, ok := p.aggColumnField(p.GetGroupColumnLayout()); !ok || field.FieldType.IsNullable() {
			aggregateSorted = false
		}
	}
	if aggregateSorted {
		order = append(order, len(cols))
		order = append(order, ordinals[prefix:]...)
	}
	physicalCols := make([]string, len(order))
	physicalTypes := make([]values.Type, len(order))
	for i, ordinal := range order {
		physicalCols[i] = row.Fields[ordinal].Name
		physicalTypes[i] = row.Fields[ordinal].FieldType
	}
	return physicalCols, order, physicalTypes
}

// groupingOrderingKey mints the ordering key for the grouping column at slot
// ordinal of the row this plan flows, named as that row names it: the row is
// the GroupBy's, whose names are not the index's column labels.
func (p *RecordQueryAggregateIndexPlan) groupingOrderingKey(ordinal int) values.Value {
	row, ok := p.GetResultValue().Type().(*values.RecordType)
	if !ok || ordinal < 0 || ordinal >= len(row.Fields) {
		return nil
	}
	request, err := values.FieldByNameAndOrdinal(row.Fields[ordinal].Name, ordinal)
	if err != nil {
		return nil
	}
	key, err := values.ResolveFieldAccess(p.GetResultValue(), []values.FieldRequest{request})
	if err != nil {
		return nil
	}
	return key
}

// HintRichOrdering is the binding-carrying form: the equality-bound grouping
// prefix is retained as FixedBinding entries (carrying the comparison) and the
// sorted tail as SortedBinding entries — Java's Binding.fixed / Binding.sorted
// split in AggregateIndexMatchCandidate.computeOrderingFromScanComparisons. A
// consumer that reasons about set operations over aggregate scans (an in-union
// over `WHERE b IN (…) GROUP BY b, a`) needs the fixed entry to promote the
// bound column into its comparison key; the plain form above drops it.
func (p *RecordQueryAggregateIndexPlan) HintRichOrdering() *properties.RichOrdering {
	groupCols, ordinals, keyTypes := p.aggregateOrderingColumns()
	if len(groupCols) == 0 {
		return properties.EmptyOrdering()
	}
	comps := p.groupingScanComparisons()
	split := p.aggregateOrderSplit(groupCols, ordinals, keyTypes)
	dir := properties.ProvidedSortOrderAscending
	if p.IsReverse() {
		dir = properties.ProvidedSortOrderDescending
	}
	bm := make(map[values.Value][]properties.OrderingBinding, len(groupCols))
	keys := make([]values.Value, 0, len(groupCols))
	for i := range groupCols[:split.fixedLen] {
		key := p.groupingOrderingKey(ordinals[i])
		if key == nil {
			return properties.EmptyOrdering()
		}
		keys = append(keys, key)
		// FIXED only for a coordinate provably pinned to ONE physical key
		// (split.pins, the column-aware question, per coordinate). A
		// signed-zero equality, or a possibly-zero untyped operand on a FLOAT
		// grouping column, admits two distinct sort values ordered only in the
		// scan's direction, so it binds SORTED — the same reasoning as
		// RecordQueryIndexPlan.HintRichOrdering.
		if split.pins[i] {
			bm[key] = []properties.OrderingBinding{properties.FixedBinding(comps[i])}
		} else {
			bm[key] = []properties.OrderingBinding{properties.SortedBinding(dir)}
		}
	}
	for i := range split.tail {
		key := p.groupingOrderingKey(ordinals[split.fixedLen+i])
		if key == nil {
			return properties.EmptyOrdering()
		}
		keys = append(keys, key)
		bm[key] = []properties.OrderingBinding{properties.SortedBinding(dir)}
	}
	if len(keys) == 0 {
		return properties.EmptyOrdering()
	}
	// Java hands computeOrderingFromScanComparisons the inner scan's
	// isStrictlySorted() (OrderingProperty.visitAggregateIndexPlan), the same
	// flag the value index reads for its own claim.
	strictlySorted := p.indexPlan != nil && p.indexPlan.IsStrictlySorted()
	return properties.NewRichOrdering(bm, keys, properties.DistinctOverAllKeysIf(strictlySorted))
}
