// Portions derived from FoundationDB Record Layer (MessageHelpers.java,
// QueryResult.java, IndexOrphanBehavior.java, RecordCursor.java,
// UnionCursor.java, MergeCursorState.java, UnorderedUnionCursor.java,
// UnionCursorContinuation.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"context"
	"fmt"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func executeAggregation(
	ctx context.Context,
	plan *plans.RecordQueryStreamingAggregationPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	if plan == nil || plan.OutputRecordType() == nil {
		return nil, fmt.Errorf("executor: streaming aggregation has no exact output type")
	}
	inner := plan.GetInner()
	groupingKeys := plan.GetGroupingKeys()
	aggregates := plan.GetAggregates()
	outputType := plan.OutputRecordType()
	inputQOV, err := requireSoleInputQOV(plan)
	if err != nil {
		return nil, err
	}
	// buildAgg deserializes an aggregate continuation (if resuming from a
	// previous transaction), extracts the inner continuation for the leaf
	// cursor plus the single in-progress group's partial state, and builds the
	// aggregate cursor. Mirrors Java's
	// RecordQueryStreamingAggregationPlan.executePlan().
	for _, agg := range aggregates {
		// finalizeGroup has a case per accumulated function and nothing else:
		// an aggregate it has no accumulator for would come out NULL on every
		// group. ImplementStreamingAggregationRule never builds such a plan; a
		// plan arriving another way is refused here, loudly.
		if !agg.Function.HasStreamingAccumulator() {
			return nil, fmt.Errorf("streaming aggregation has no accumulator for %s", agg.Function)
		}
	}
	buildAgg := func(aggCont []byte) (recordlayer.RecordCursor[QueryResult], error) {
		var innerContinuation []byte
		var priorGroupKey string
		var priorState *groupState

		if aggCont != nil {
			var md *recordlayer.RecordMetaData
			if store != nil {
				md = store.GetRecordMetaData()
			}
			resolve := continuationMessageResolver(md, plan)
			ic, gk, gs, decErr := decodeAggregateContinuation(aggCont, groupingKeys, aggregates, resolve)
			if decErr != nil {
				return nil, fmt.Errorf("invalid aggregate continuation: %w", decErr)
			}
			innerContinuation = ic
			priorGroupKey = gk
			priorState = gs
		}

		innerCursor, err := ExecutePlan(ctx, inner, store, evalCtx, innerContinuation, props.ClearSkipAndLimit())
		if err != nil {
			return nil, err
		}

		cursor := newAggregateCursorWithOutputType(
			innerCursor, groupingKeys, aggregates, inner, evalCtx, inputQOV, outputType)
		if len(innerContinuation) > 0 {
			// A resumed cursor's initial resume point is the decoded INNER
			// position: if its first row group-breaks against restored partial
			// state (the prior page's time/scan limit landed exactly on a group
			// boundary), the emitted group's continuation is (this inner
			// position, NO state) — resume reads the next group fresh. Java
			// instead re-wraps the WHOLE parsed AggregateCursorContinuation as
			// the initial previousContinuationInGroup (constructor param), whose
			// re-emit NESTS the aggregate proto inside itself and would hand
			// aggregate bytes to the leaf plan on a second resume — Go encodes
			// the intended flat position (Java's own movement-table comment).
			cursor.previousContinuationInGroup = recordlayer.NewBytesContinuation(innerContinuation)
		}
		if priorState != nil {
			cursor.withPartialState(priorGroupKey, priorState.keyVals, priorState)
		}
		return cursor, nil
	}

	if len(groupingKeys) > 0 {
		cursor, err := buildAgg(continuation)
		if err != nil {
			return nil, err
		}
		return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
	}

	// Scalar aggregation (no grouping keys): Java plans it as
	// DefaultOnEmpty(StreamingAggregation) — RecordQueryStreamingAggregationPlan
	// REFUSES an inline default (fromProto throws on isCreateDefaultOnEmpty) and
	// the COUNT(*)-on-empty default row rides RecordCursor.orElse in
	// RecordQueryDefaultOnEmptyPlan.executePlan. Mirror that structure: the
	// OrElse state machine (UNDECIDED/USE_INNER/USE_OTHER) is what makes BOTH
	// emitted-row shapes resumable — a resume from the aggregate row's
	// continuation stays USE_INNER (the exhausted aggregate reports
	// SourceExhausted, never fabricating a duplicate default), and a resume from
	// the default row's continuation stays USE_OTHER (the single-row list cursor
	// is past its row). The child clears skip/limit; skip/limit apply to the
	// whole OrElse — Java's clearSkipAndLimit-on-child + skipThenLimit split.
	primaryFactory := func(cont []byte) recordlayer.RecordCursor[QueryResult] {
		cursor, err := buildAgg(cont)
		if err != nil {
			return &errResultCursor{err: err}
		}
		return cursor
	}
	alternativeFactory := func(cont []byte) recordlayer.RecordCursor[QueryResult] {
		return recordlayer.FromListWithContinuation(
			[]QueryResult{emptyScalarAggregateRow(aggregates, outputType)}, cont)
	}
	orElse := recordlayer.OrElseWithContinuation(primaryFactory, alternativeFactory, continuation)
	return applySkipLimit(orElse, props.Skip, props.ReturnedRowLimit), nil
}

// aggKeyName delegates to the single naming authority
// (expressions.AggregateKeyColumnName): the executor's emitted slot name and the
// translator's baked ordinal must derive from ONE rule or they drift.
func aggKeyName(k values.Value) string { return expressions.AggregateKeyColumnName(k) }

func isNumeric(v any) bool {
	switch v.(type) {
	case int64, int32, int, float64, float32:
		return true
	}
	return false
}

// aggResultName derives the canonical group-result slot key for an aggregate,
// delegating to the single naming authority (expressions.AggregateResultColumnName)
// so the executor's emitted slot name and the translator's baked ordinal derive
// from ONE rule.
//
// The authority preserves case (RFC-237 §8): folding a case-sensitive token
// such as the literal in `COUNT(CASE WHEN s='x' …)` would conflate it with
// `…'X'…` and let finalizeGroup write distinct aggregates under one slot key.
func aggResultName(agg expressions.AggregateSpec) string {
	return expressions.AggregateResultColumnName(agg)
}

// executeAggregateIndexScan scans an aggregate index (SUM, COUNT, etc.)
// and produces rows with grouping columns + aggregate value. The index
// maintainer (atomicMutationIndexMaintainer) returns entries where:
//   - Key = grouping column values (tuple-encoded)
//   - Value = aggregate result (little-endian int64 → tuple.Tuple{int64})
//
// No record fetch needed — the index entries ARE the aggregated result.
func executeAggregateIndexScan(
	ctx context.Context,
	p *plans.RecordQueryAggregateIndexPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	idxPlan := p.GetIndexPlan()
	idx := store.GetMetaData().GetIndex(idxPlan.GetIndexName())
	if idx == nil {
		return nil, fmt.Errorf("executor: aggregate index %q not found", idxPlan.GetIndexName())
	}
	if err := requireReadableQueryIndex(store, idx); err != nil {
		return nil, err
	}
	maintainer, err := store.GetIndexMaintainer(idx)
	if err != nil {
		return nil, fmt.Errorf("executor: getting index maintainer for %q: %w", idxPlan.GetIndexName(), err)
	}

	groupCols := p.GetGroupCols()
	physicalGroupingPrefixCount := p.GetPhysicalGroupingPrefixCount()
	physicalGroupingCount := len(groupCols)
	isPermuted := idx.Type == recordlayer.IndexTypePermutedMin || idx.Type == recordlayer.IndexTypePermutedMax
	scanType := recordlayer.IndexScanByValue
	if idx.Type == recordlayer.IndexTypeBitmapValue {
		scanType = recordlayer.IndexScanByGroup
	}
	if isPermuted {
		var verr error
		physicalGroupingCount, verr = validatePermutedAggregateScan(p, idx)
		if verr != nil {
			return nil, verr
		}
		scanType = recordlayer.IndexScanByGroup
	}

	fingerprintSalt, err := aggregateScanRangeFingerprintSalt(
		p,
		idx.Type,
		scanType,
		physicalGroupingCount,
		physicalGroupingPrefixCount,
	)
	if err != nil {
		return nil, fmt.Errorf("executor: building aggregate scan execution identity for %q: %w", idxPlan.GetIndexName(), err)
	}
	rangeSet, err := bindScanComparisonsToRangeSet(
		idxPlan.GetScanComparisons(),
		p.GetKeyComponentTypes(),
		scanBindContext(evalCtx),
		idxPlan.IsReverse(),
		fingerprintSalt,
	)
	if err != nil {
		return nil, fmt.Errorf("executor: building scan ranges for %q: %w", idxPlan.GetIndexName(), err)
	}

	scanProps := recordlayer.NewScanProperties(props).WithReverse(idxPlan.IsReverse())

	posType, err := validateAggregateIndexResultType(p, len(groupCols))
	if err != nil {
		return nil, err
	}

	readerLeaves, err := aggregateReaderLeaves(p.GetEntryReader(), posType)
	if err != nil {
		return nil, fmt.Errorf("executor: aggregate index %q: %w", idxPlan.GetIndexName(), err)
	}

	// PERMUTED_MIN/MAX indexes keep the current extremum per group in the
	// SECONDARY (permuted) subspace, not the primary VALUE tree; the aggregate
	// value lives inside the entry KEY, not entry.Value. Scan BY_GROUP — the same
	// scan Java's AggregateIndexMatchCandidate builds (IndexScanType.BY_GROUP) —
	// so a plain SQL MAX/MIN served from a permuted index reflects the true
	// current extremum under deletes/updates (a monotone _EVER index goes stale).
	cursor, err := openAggregateIndexCursor(
		p, store, idx, maintainer, rangeSet, continuation, scanProps, isPermuted, posType, readerLeaves,
	)
	if err != nil {
		return nil, err
	}
	return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
}

func openAggregateIndexCursor(
	p *plans.RecordQueryAggregateIndexPlan,
	store *recordlayer.FDBRecordStore,
	idx *recordlayer.Index,
	maintainer recordlayer.IndexMaintainer,
	rangeSet scanRangeSetSpec,
	continuation []byte,
	scanProps recordlayer.ScanProperties,
	isPermuted bool,
	posType *values.RecordType,
	readerLeaves []*values.IndexEntryObjectValue,
) (recordlayer.RecordCursor[QueryResult], error) {
	idxPlan := p.GetIndexPlan()
	groupCols := p.GetGroupCols()
	physicalGroupingPrefixCount := p.GetPhysicalGroupingPrefixCount()
	if isPermuted {
		cursor, openErr := newScanRangeSetCursor(
			rangeSet,
			continuation,
			scanProps,
			func(
				scanRange recordlayer.TupleRange,
				innerContinuation []byte,
				childProperties recordlayer.ScanProperties,
			) (recordlayer.RecordCursor[QueryResult], error) {
				return newPermutedAggregateIndexCursor(
					store,
					idx,
					scanRange,
					innerContinuation,
					childProperties,
					len(groupCols),
					physicalGroupingPrefixCount,
					posType,
					readerLeaves,
				)
			},
		)
		if openErr != nil {
			return nil, fmt.Errorf("executor: opening permuted aggregate scan ranges for %q: %w", idxPlan.GetIndexName(), openErr)
		}
		return cursor, nil
	}

	indexCursor, err := newScanRangeSetCursor(
		rangeSet,
		continuation,
		scanProps,
		func(
			scanRange recordlayer.TupleRange,
			innerContinuation []byte,
			childProperties recordlayer.ScanProperties,
		) (recordlayer.RecordCursor[*recordlayer.IndexEntry], error) {
			if idx.Type == recordlayer.IndexTypeBitmapValue {
				return store.ScanIndexByType(idx, recordlayer.IndexScanByGroup, scanRange, innerContinuation, childProperties), nil
			}
			// Instrumented here because this path never touches FDBRecordStore.ScanIndex:
			// the per-range factory needs the maintainer directly, so the store method
			// that would otherwise count the scan is bypassed entirely.
			return recordlayer.InstrumentIndexScanCursor(store.Context().Timer(),
				maintainer.Scan(scanRange, innerContinuation, childProperties)), nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("executor: opening aggregate scan ranges for %q: %w", idxPlan.GetIndexName(), err)
	}

	return &aggregateIndexCursor{
		inner:     indexCursor,
		groupCols: groupCols,
		posType:   posType,
		reader:    readerLeaves,
		// Stamped at mint time so the output boundary checks the row instead of
		// copying it to attach the layout — see mintedRowLayout.
		layout: mintedRowLayout(p),
	}, nil
}

func validatePermutedAggregateScan(
	p *plans.RecordQueryAggregateIndexPlan,
	idx *recordlayer.Index,
) (int, error) {
	idxPlan := p.GetIndexPlan()
	groupCols := p.GetGroupCols()
	physicalGroupingPrefixCount := p.GetPhysicalGroupingPrefixCount()
	groupingCount, actualPhysicalPrefix, layoutErr := permutedAggregateGroupingLayout(idx)
	if layoutErr != nil {
		return 0, layoutErr
	}
	if len(groupCols) != groupingCount || physicalGroupingPrefixCount != actualPhysicalPrefix {
		return 0, &invalidPermutedAggregateScanError{
			indexName:                   idxPlan.GetIndexName(),
			comparisonCount:             len(idxPlan.GetScanComparisons()),
			groupingCount:               len(groupCols),
			physicalGroupingPrefixCount: physicalGroupingPrefixCount,
			actualGroupingCount:         groupingCount,
			actualPhysicalPrefixCount:   actualPhysicalPrefix,
			layoutMismatch:              true,
		}
	}
	if physicalGroupingPrefixCount < 0 || physicalGroupingPrefixCount > len(groupCols) {
		return 0, &invalidPermutedAggregateScanError{
			indexName:                   idxPlan.GetIndexName(),
			comparisonCount:             len(idxPlan.GetScanComparisons()),
			groupingCount:               len(groupCols),
			physicalGroupingPrefixCount: physicalGroupingPrefixCount,
		}
	}
	if len(idxPlan.GetScanComparisons()) > physicalGroupingPrefixCount {
		// PERMUTED_MIN/MAX inserts the aggregate value between the physical
		// grouping prefix and suffix. Applying a logical grouping comparison
		// past that boundary would scan a different tuple coordinate and can
		// silently return the wrong groups. Production planning declines these
		// candidates; retain this executor guard for hand-built plans.
		return 0, &invalidPermutedAggregateScanError{
			indexName:                   idxPlan.GetIndexName(),
			comparisonCount:             len(idxPlan.GetScanComparisons()),
			groupingCount:               len(groupCols),
			physicalGroupingPrefixCount: physicalGroupingPrefixCount,
		}
	}
	return groupingCount, nil
}

func validateAggregateIndexResultType(
	p *plans.RecordQueryAggregateIndexPlan,
	groupCount int,
) (*values.RecordType, error) {
	idxPlan := p.GetIndexPlan()
	// The aggregate-index row's authoritative ordinal
	// schema is the GROUP columns in scan order followed by the aggregate column
	// — the exact order the row's slots are filled below (entry.Key then
	// entry.Value). Row-invariant, so it is built once here and shared across
	// rows. Its names are the plan's: the GroupBy row the planner published.
	posType, ok := p.GetResultType().(*values.RecordType)
	if !ok || posType == nil || len(posType.Fields) != groupCount+1 {
		actualWidth := -1
		if posType != nil {
			actualWidth = len(posType.Fields)
		}
		return nil, fmt.Errorf(
			"executor: aggregate index %q has result type %T with %d columns, want exact record width %d",
			idxPlan.GetIndexName(), p.GetResultType(), actualWidth, groupCount+1)
	}
	for i := range posType.Fields {
		if posType.Fields[i].Ordinal != i {
			return nil, fmt.Errorf(
				"executor: aggregate index %q result field %d is %q#%d, want ordinal %d",
				idxPlan.GetIndexName(), i, posType.Fields[i].Name, posType.Fields[i].Ordinal, i)
		}
		if _, exactErr := values.SnapshotExactType(posType.Fields[i].FieldType); exactErr != nil {
			return nil, fmt.Errorf(
				"executor: aggregate index %q result field %d has unresolved type: %w",
				idxPlan.GetIndexName(), i, exactErr)
		}
	}
	return posType, nil
}

// aggregateReaderLeaves checks the plan's entry reader against the row it
// fills: one entry leaf per field, each of the field's type. A plan without a
// reader gets nil and its cursor decodes the layout itself.
func aggregateReaderLeaves(reader *values.RecordConstructorValue, posType *values.RecordType) ([]*values.IndexEntryObjectValue, error) {
	if reader == nil {
		return nil, nil
	}
	if len(reader.Fields) != len(posType.Fields) {
		return nil, fmt.Errorf("entry reader has %d fields, the result row %d", len(reader.Fields), len(posType.Fields))
	}
	leaves := make([]*values.IndexEntryObjectValue, len(reader.Fields))
	for i, field := range reader.Fields {
		leaf, ok := field.Value.(*values.IndexEntryObjectValue)
		if !ok || !leaf.Type().Equals(posType.Fields[i].FieldType) {
			return nil, fmt.Errorf("entry reader field %d (%s) does not read the result's %s",
				i, values.ExplainValue(field.Value), posType.Fields[i].FieldType)
		}
		leaves[i] = leaf
	}
	return leaves, nil
}

// readAggregateEntry fills slots from the reader's leaves.
func readAggregateEntry(slots []any, leaves []*values.IndexEntryObjectValue, entry *recordlayer.IndexEntry) error {
	for i, leaf := range leaves {
		value, err := leaf.ReadTuples(entry.Key, entry.Value)
		if err != nil {
			return fmt.Errorf("executor: aggregate entry field %d: %w", i, err)
		}
		slots[i] = value
	}
	return nil
}

type aggregateIndexCursor struct {
	inner     recordlayer.RecordCursor[*recordlayer.IndexEntry]
	groupCols []string
	posType   *values.RecordType
	// reader is the plan's entry reader, when it carries one.
	reader []*values.IndexEntryObjectValue
	closed bool
	// layout is the plan's provided output layout, carried by every row this
	// cursor mints.
	layout values.OrdinalLayout
}

func (c *aggregateIndexCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	result, err := c.inner.OnNext(ctx)
	if err != nil {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), err
	}
	if !result.HasNext() {
		return recordlayer.NewResultNoNext[QueryResult](result.GetNoNextReason(), result.GetContinuation()), nil
	}
	entry := result.GetValue()

	// Emit the authoritative ordinal row (real slots read
	// from the index entry). Slot order matches c.posType: group cols then the
	// aggregate column.
	slots := make([]any, len(c.posType.Fields))
	if c.reader != nil {
		if err := readAggregateEntry(slots, c.reader, entry); err != nil {
			return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), err
		}
		qr := QueryResult{Positional: &PositionalRow{Type: c.posType, Slots: slots, Layout: c.layout}}
		return recordlayer.NewResultWithValue(qr, result.GetContinuation()), nil
	}

	for i := range c.groupCols {
		if i < len(entry.Key) {
			// Normalize the group key into the row domain, matching the
			// covering cursor: tuple.UUID → [16]byte (otherwise a residual
			// HAVING/filter compare in cmpAny, which only knows [16]byte,
			// would miss it) and float32 → float64 (the base path widens
			// FLOAT; a raw float32 group key would split sorts/dedup/joins
			// against base-sourced rows).
			slots[i] = tupleElementToRowValue(entry.Key[i])
		}
	}

	if len(entry.Value) > 0 {
		if aggOrd := len(c.groupCols); aggOrd < len(slots) {
			slots[aggOrd] = entry.Value[0]
		}
	}

	// The row is minted here and has one owner, so it carries the layout its
	// output boundary will hold it to rather than being copied to acquire it —
	// see mintedRowLayout.
	qr := QueryResult{Positional: &PositionalRow{Type: c.posType, Slots: slots, Layout: c.layout}}
	return recordlayer.NewResultWithValue(qr, result.GetContinuation()), nil
}

func (c *aggregateIndexCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *aggregateIndexCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*aggregateIndexCursor)(nil)
