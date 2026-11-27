// Portions derived from FoundationDB Record Layer (UnionCursor.java,
// MergeCursorState.java, UnorderedUnionCursor.java,
// UnionCursorContinuation.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"context"
	"fmt"

	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

type invalidPermutedAggregateScanError struct {
	indexName                   string
	comparisonCount             int
	groupingCount               int
	physicalGroupingPrefixCount int
	actualGroupingCount         int
	actualPhysicalPrefixCount   int
	layoutMismatch              bool
}

func (e *invalidPermutedAggregateScanError) Error() string {
	if e.layoutMismatch {
		return fmt.Sprintf(
			"executor: aggregate index %q plan claims grouping layout g=%d, prefix=%d, but metadata has g=%d, prefix=%d",
			e.indexName,
			e.groupingCount,
			e.physicalGroupingPrefixCount,
			e.actualGroupingCount,
			e.actualPhysicalPrefixCount,
		)
	}
	return fmt.Sprintf(
		"executor: aggregate index %q has %d logical grouping comparisons, but only %d of %d grouping columns precede the permuted aggregate value",
		e.indexName,
		e.comparisonCount,
		e.physicalGroupingPrefixCount,
		e.groupingCount,
	)
}

func permutedAggregateGroupingLayout(idx *recordlayer.Index) (groupingCount, physicalPrefixCount int, err error) {
	gke, ok := idx.RootExpression.(*recordlayer.GroupingKeyExpression)
	if !ok {
		return 0, 0, fmt.Errorf(
			"executor: permuted index %q root is %T, want *GroupingKeyExpression",
			idx.Name, idx.RootExpression,
		)
	}
	groupingCount = gke.GetGroupingCount()
	// Read as Java's query side reads it (AggregateIndexMatchCandidate.
	// getPermutedCount: absent is 0, present is Integer.parseInt), so the
	// scan splits the key where the maintainer, which parses the same way,
	// wrote the entries. The range is Build's check, repeated for an index
	// changed after Build.
	permutedSize := 0
	if raw, exists := idx.Options[recordlayer.IndexOptionPermutedSize]; exists {
		parsed, parseErr := recordlayer.PermutedSizeOption(idx)
		if parseErr != nil || parsed < 0 || parsed > groupingCount {
			return 0, 0, fmt.Errorf(
				"executor: permuted index %q has invalid %s=%q for grouping count %d",
				idx.Name,
				recordlayer.IndexOptionPermutedSize,
				raw,
				groupingCount,
			)
		}
		permutedSize = parsed
	}
	return groupingCount, groupingCount - permutedSize, nil
}

// newPermutedAggregateIndexCursor builds a cursor over a PERMUTED_MIN/MAX
// index's secondary (permuted) subspace. Each permuted entry's KEY has the
// layout [groupPrefix..., value..., groupSuffix...] where groupPrefix is the
// first (groupingCount - permutedSize) grouping columns, value is the grouped
// aggregate column, and groupSuffix is the trailing permutedSize grouping
// columns (Java's PermutedMinMaxIndexMaintainer). The grouping key is
// reconstructed in its original order as prefix ++ suffix, and the aggregate
// value is read from the middle. For the plain SQL MAX/MIN DDL path permutedSize
// is 0, so the layout collapses to [group..., value] with an empty suffix.
func newPermutedAggregateIndexCursor(
	store *recordlayer.FDBRecordStore,
	idx *recordlayer.Index,
	scanRange recordlayer.TupleRange,
	continuation []byte,
	scanProps recordlayer.ScanProperties,
	groupCount int,
	physicalGroupingPrefixCount int,
	posType *values.RecordType,
	reader []*values.IndexEntryObjectValue,
) (recordlayer.RecordCursor[QueryResult], error) {
	gke, ok := idx.RootExpression.(*recordlayer.GroupingKeyExpression)
	if !ok {
		return nil, fmt.Errorf("executor: permuted index %q root is %T, want *GroupingKeyExpression",
			idx.Name, idx.RootExpression)
	}
	totalSize := gke.ColumnSize()
	inner := store.ScanIndexByType(idx, recordlayer.IndexScanByGroup, scanRange, continuation, scanProps)
	// The repair constructs a new scan from these execution properties. Retain
	// the effective mode even when WithStreamingMode overrode the default.
	repairProps := scanProps.ExecuteProperties
	repairProps.DefaultCursorStreamingMode = scanProps.CursorStreamingMode
	return &permutedAggregateIndexCursor{
		inner:      inner,
		groupCount: groupCount,
		valueStart: physicalGroupingPrefixCount,
		valueEnd:   totalSize - (gke.GetGroupingCount() - physicalGroupingPrefixCount),
		posType:    posType,
		reader:     reader,
		// The MIN flavour needs the stored extremum repaired when it is NULL —
		// see recordlayer.PermutedMinIgnoringNulls. The ordinary subspace's
		// value span is stated in ORIGINAL column order, which is where the
		// repair reads, and differs from the permuted span above whenever
		// permutedSize is non-zero.
		store:             store,
		index:             idx,
		repairNullMin:     recordlayer.IsPermutedMinIndex(idx),
		ordinaryValueFrom: gke.GetGroupingCount(),
		ordinaryValueTo:   totalSize,
		scanProps:         repairProps,
	}, nil
}

// permutedAggregateIndexCursor emits one (group..., extremum) row per permuted
// secondary entry, reading both the grouping key and the aggregate value out of
// the entry KEY (the permuted subspace stores an empty value).
type permutedAggregateIndexCursor struct {
	inner      recordlayer.RecordCursor[*recordlayer.IndexEntry]
	groupCount int
	valueStart int
	valueEnd   int
	posType    *values.RecordType
	// reader is the plan's entry reader, when it carries one.
	reader []*values.IndexEntryObjectValue
	closed bool

	// The MIN null repair. store/index/scanProps are what the repair reads
	// with; ordinaryValueFrom/To bound the aggregated columns in the ORDINARY
	// subspace's key order, which is not the permuted order valueStart/valueEnd
	// describe. repairNullMin is false for PERMUTED_MAX, where a stored NULL
	// extremum is already the right answer.
	//
	// scanProps is the CALLER'S whole ExecuteProperties, not just its isolation
	// level, because it carries the shared ScanState. The repair performs one
	// extra index read per NULL-extremum group, and a read that does not carry
	// that state is charged to nothing — the statement's record, byte and time
	// budgets would silently not see reads whose count scales with the data.
	store             *recordlayer.FDBRecordStore
	index             *recordlayer.Index
	repairNullMin     bool
	ordinaryValueFrom int
	ordinaryValueTo   int
	scanProps         recordlayer.ExecuteProperties
}

func (c *permutedAggregateIndexCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	result, err := c.inner.OnNext(ctx)
	if err != nil {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), err
	}
	if !result.HasNext() {
		return recordlayer.NewResultNoNext[QueryResult](result.GetNoNextReason(), result.GetContinuation()), nil
	}

	key := result.GetValue().Key
	slots := make([]any, len(c.posType.Fields))
	if c.reader != nil {
		if err := readAggregateEntry(slots, c.reader, result.GetValue()); err != nil {
			return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), err
		}
	}

	// Grouping key in original order: the first valueStart columns come straight
	// from the key; the remaining grouping columns are the permuted suffix, which
	// sits after the value at key[valueEnd:]. Normalize into the row domain the
	// same way the covering/aggregate cursors do (UUID → [16]byte, float32 →
	// float64) so residual HAVING/sort/joins compare consistently.
	//
	// groupKey collects the same columns as a tuple, because the MIN null repair
	// below reads the ordinary subspace, which is keyed in this original order.
	groupKey := make(tuple.Tuple, 0, c.groupCount)
	for i := 0; i < c.groupCount; i++ {
		ki := i
		if i >= c.valueStart {
			ki = c.valueEnd + (i - c.valueStart)
		}
		if ki >= len(key) {
			continue
		}
		groupKey = append(groupKey, key[ki])
		if c.reader == nil && i < len(slots) {
			slots[i] = tupleElementToRowValue(key[ki])
		}
	}

	// Aggregate value: the single grouped column at key[valueStart:valueEnd].
	if aggOrd := c.groupCount; aggOrd < len(slots) && c.valueStart < c.valueEnd && c.valueStart < len(key) {
		extremum := key[c.valueStart]
		// A NULL extremum on a MIN index does not mean the group has no value —
		// NULL merely outranks every value in the stored comparison. Resolve the
		// smallest non-NULL value from the ordinary subspace; a nil answer there
		// means the group really is all-NULL and the stored NULL stands.
		if extremum == nil && c.repairNullMin && len(groupKey) == c.groupCount {
			repaired, err := recordlayer.PermutedMinIgnoringNulls(
				ctx, recordlayer.OrdinaryIndexScanner(c.store, c.index), c.index.Name,
				groupKey, c.ordinaryValueFrom, c.ordinaryValueTo, c.scanProps)
			if err != nil {
				return recordlayer.NewResultNoNext[QueryResult](
					recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), err
			}
			if len(repaired) > 0 {
				extremum = repaired[0]
			}
			slots[aggOrd] = tupleElementToRowValue(extremum)
		} else if c.reader == nil {
			slots[aggOrd] = tupleElementToRowValue(extremum)
		}
	}

	qr := QueryResult{Positional: &PositionalRow{Type: c.posType, Slots: slots}}
	return recordlayer.NewResultWithValue(qr, result.GetContinuation()), nil
}

func (c *permutedAggregateIndexCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *permutedAggregateIndexCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*permutedAggregateIndexCursor)(nil)
