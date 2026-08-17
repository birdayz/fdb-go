package executor

import (
	"container/heap"
	"context"
	"fmt"
	"slices"
	"strconv"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
)

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
	if isPermuted {
		groupingCount, actualPhysicalPrefix, layoutErr := permutedAggregateGroupingLayout(idx)
		if layoutErr != nil {
			return nil, layoutErr
		}
		if len(groupCols) != groupingCount || physicalGroupingPrefixCount != actualPhysicalPrefix {
			return nil, &invalidPermutedAggregateScanError{
				indexName:                   idxPlan.GetIndexName(),
				comparisonCount:             len(idxPlan.GetScanComparisons()),
				groupingCount:               len(groupCols),
				physicalGroupingPrefixCount: physicalGroupingPrefixCount,
				actualGroupingCount:         groupingCount,
				actualPhysicalPrefixCount:   actualPhysicalPrefix,
				layoutMismatch:              true,
			}
		}
		physicalGroupingCount = groupingCount
		scanType = recordlayer.IndexScanByGroup
	}
	if isPermuted && (physicalGroupingPrefixCount < 0 || physicalGroupingPrefixCount > len(groupCols)) {
		return nil, &invalidPermutedAggregateScanError{
			indexName:                   idxPlan.GetIndexName(),
			comparisonCount:             len(idxPlan.GetScanComparisons()),
			groupingCount:               len(groupCols),
			physicalGroupingPrefixCount: physicalGroupingPrefixCount,
		}
	}
	if isPermuted && len(idxPlan.GetScanComparisons()) > physicalGroupingPrefixCount {
		// PERMUTED_MIN/MAX inserts the aggregate value between the physical
		// grouping prefix and suffix. Applying a logical grouping comparison
		// past that boundary would scan a different tuple coordinate and can
		// silently return the wrong groups. Production planning declines these
		// candidates; retain this executor guard for hand-built plans.
		return nil, &invalidPermutedAggregateScanError{
			indexName:                   idxPlan.GetIndexName(),
			comparisonCount:             len(idxPlan.GetScanComparisons()),
			groupingCount:               len(groupCols),
			physicalGroupingPrefixCount: physicalGroupingPrefixCount,
		}
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

	scanProps := recordlayer.ScanProperties{
		ExecuteProperties:   props,
		Reverse:             idxPlan.IsReverse(),
		CursorStreamingMode: recordlayer.StreamingModeIterator,
	}

	canonicalName := p.CanonicalAggColumnName()
	// The aggregate-index row's authoritative ordinal
	// schema is the GROUP columns in scan order followed by the aggregate column
	// — the exact order the row's slots are filled below (entry.Key then
	// entry.Value). Row-invariant, so it is built once here and shared across
	// rows. Named by the canonical output names (uppercase group
	// cols, canonical agg name) — the exact schema plan-time bakes bind
	// against.
	posType, ok := p.GetResultType().(*values.RecordType)
	if !ok || posType == nil || len(posType.Fields) != len(groupCols)+1 {
		actualWidth := -1
		if posType != nil {
			actualWidth = len(posType.Fields)
		}
		return nil, fmt.Errorf(
			"executor: aggregate index %q has result type %T with %d columns, want exact record width %d",
			idxPlan.GetIndexName(), p.GetResultType(), actualWidth, len(groupCols)+1)
	}
	for i, name := range append(append([]string(nil), groupCols...), canonicalName) {
		if posType.Fields[i].Name != name || posType.Fields[i].Ordinal != i {
			return nil, fmt.Errorf(
				"executor: aggregate index %q result field %d is %q#%d, want %q#%d",
				idxPlan.GetIndexName(), i, posType.Fields[i].Name, posType.Fields[i].Ordinal, name, i)
		}
		if _, exactErr := values.SnapshotExactType(posType.Fields[i].FieldType); exactErr != nil {
			return nil, fmt.Errorf(
				"executor: aggregate index %q result field %d has unresolved type: %w",
				idxPlan.GetIndexName(), i, exactErr)
		}
	}

	// PERMUTED_MIN/MAX indexes keep the current extremum per group in the
	// SECONDARY (permuted) subspace, not the primary VALUE tree; the aggregate
	// value lives inside the entry KEY, not entry.Value. Scan BY_GROUP — the same
	// scan Java's AggregateIndexMatchCandidate builds (IndexScanType.BY_GROUP) —
	// so a plain SQL MAX/MIN served from a permuted index reflects the true
	// current extremum under deletes/updates (a monotone _EVER index goes stale).
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
				)
			},
		)
		if openErr != nil {
			return nil, fmt.Errorf("executor: opening permuted aggregate scan ranges for %q: %w", idxPlan.GetIndexName(), openErr)
		}
		return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
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

	result := &aggregateIndexCursor{
		inner:     indexCursor,
		groupCols: groupCols,
		// Single source for the aggregate column key: the plan's
		// CanonicalAggColumnName is also what planColumnNamesWithMD reports via
		// OutputColumnNames, so the cursor's row key and the reported name can't
		// drift (RFC-081).
		canonicalName: canonicalName,
		posType:       posType,
		// RFC-209 §5.3(a): the plan decides, the cursor obeys.
		liveGroupsOnly: p.IsLiveGroupsOnly(),
		// Stamped at mint time so the output boundary checks the row instead of
		// copying it to attach the layout — see mintedRowLayout.
		layout: mintedRowLayout(p),
	}
	return applySkipLimit(result, props.Skip, props.ReturnedRowLimit), nil
}

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
	permutedSize := 0
	if raw, exists := idx.Options[recordlayer.IndexOptionPermutedSize]; exists {
		parsed, parseErr := strconv.Atoi(raw)
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
) (recordlayer.RecordCursor[QueryResult], error) {
	gke, ok := idx.RootExpression.(*recordlayer.GroupingKeyExpression)
	if !ok {
		return nil, fmt.Errorf("executor: permuted index %q root is %T, want *GroupingKeyExpression",
			idx.Name, idx.RootExpression)
	}
	totalSize := gke.ColumnSize()
	inner := store.ScanIndexByType(idx, recordlayer.IndexScanByGroup, scanRange, continuation, scanProps)
	return &permutedAggregateIndexCursor{
		inner:      inner,
		groupCount: groupCount,
		valueStart: physicalGroupingPrefixCount,
		valueEnd:   totalSize - (gke.GetGroupingCount() - physicalGroupingPrefixCount),
		posType:    posType,
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
	closed     bool
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

	// Grouping key in original order: the first valueStart columns come straight
	// from the key; the remaining grouping columns are the permuted suffix, which
	// sits after the value at key[valueEnd:]. Normalize into the row domain the
	// same way the covering/aggregate cursors do (UUID → [16]byte, float32 →
	// float64) so residual HAVING/sort/joins compare consistently.
	for i := 0; i < c.groupCount && i < len(slots); i++ {
		ki := i
		if i >= c.valueStart {
			ki = c.valueEnd + (i - c.valueStart)
		}
		if ki < len(key) {
			slots[i] = tupleElementToRowValue(key[ki])
		}
	}

	// Aggregate value: the single grouped column at key[valueStart:valueEnd].
	if aggOrd := c.groupCount; aggOrd < len(slots) && c.valueStart < c.valueEnd && c.valueStart < len(key) {
		slots[aggOrd] = tupleElementToRowValue(key[c.valueStart])
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

type aggregateIndexCursor struct {
	inner         recordlayer.RecordCursor[*recordlayer.IndexEntry]
	groupCols     []string
	canonicalName string
	posType       *values.RecordType
	// liveGroupsOnly drops entries whose stored aggregate is zero. Set only for
	// a grouped COUNT(*) scan, where the stored value is the group's row count
	// and a zero can therefore only be the residue of a vacated group (the
	// atomic ADD that emptied it left the key behind). See
	// plans.RecordQueryAggregateIndexPlan.liveGroupsOnly — the plan carries the
	// property, EXPLAIN renders it, and this cursor merely obeys it. Deciding
	// existence here rather than at plan time would make the plan a lie.
	liveGroupsOnly bool
	closed         bool
	// layout is the plan's provided output layout, carried by every row this
	// cursor mints.
	layout values.OrdinalLayout
}

func (c *aggregateIndexCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	// A vacated group's entry is dropped before it becomes a row, and the scan
	// keeps advancing rather than returning a "no value" result: a dropped entry
	// is not an end-of-stream condition, the next live group may be one key
	// away. The loop (rather than a recursive re-entry) matters because the run
	// of consecutive vacated groups is unbounded — a table emptied wholesale
	// leaves one zero entry per group that ever existed. The emitted row carries
	// the continuation of the entry it came from, so a resume never re-reads a
	// group already returned and the zero entries in between are simply
	// re-skipped.
	var result recordlayer.RecordCursorResult[*recordlayer.IndexEntry]
	var entry *recordlayer.IndexEntry
	for {
		var err error
		result, err = c.inner.OnNext(ctx)
		if err != nil {
			return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), err
		}
		if !result.HasNext() {
			return recordlayer.NewResultNoNext[QueryResult](result.GetNoNextReason(), result.GetContinuation()), nil
		}
		entry = result.GetValue()
		if !c.liveGroupsOnly || !isVacatedGroupEntry(entry) {
			break
		}
	}

	// Emit the authoritative ordinal row (real slots read
	// from the index entry). Slot order matches c.posType: group cols then the
	// aggregate column.
	slots := make([]any, len(c.posType.Fields))

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

// isVacatedGroupEntry reports whether a COUNT(*) index entry denotes a group
// that no longer has any rows. The maintainer decrements the accumulator with
// an atomic ADD and never removes the key, so an emptied group leaves an entry
// holding zero. Because the stored value counts ROWS, and a live group has at
// least one, zero is decidable: it can only be residue.
//
// Anything that is not an explicit integer zero keeps the row. Dropping a live
// group is a brand-new wrong answer and strictly worse than the phantom this
// filter removes, so an unrecognised value shape errs toward emitting.
func isVacatedGroupEntry(entry *recordlayer.IndexEntry) bool {
	if entry == nil || len(entry.Value) == 0 {
		return false
	}
	switch v := entry.Value[0].(type) {
	case int64:
		return v == 0
	case int:
		return v == 0
	default:
		return false
	}
}

func (c *aggregateIndexCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *aggregateIndexCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*aggregateIndexCursor)(nil)

func executeMultiIntersection(
	ctx context.Context,
	p *plans.RecordQueryMultiIntersectionOnValuesPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	children := p.GetChildren()
	if len(children) == 0 {
		return recordlayer.Empty[QueryResult](), nil
	}

	// Decode the per-child IntersectionContinuation and resume each child from
	// its saved position (RFC-071) — shared with executeIntersection. Replaces
	// the prior loud-error guard on a non-nil continuation.
	cursors, resume, err := buildIntersectionChildCursors(ctx, children, store, evalCtx, continuation, props)
	if err != nil {
		return nil, err
	}

	keyVals := p.GetComparisonKey()
	compKeyFunc := multiIntersectionCompKeyFunc(keyVals)
	outputType, ok := p.GetResultType().(*values.RecordType)
	if !ok || outputType == nil {
		return nil, fmt.Errorf("multi-intersection result type is %T, want exact record", p.GetResultType())
	}

	var innerCursor recordlayer.RecordCursor[[]QueryResult]
	if p.HasDrivingAlias() {
		// RFC-209 §5.3(b): the group-existence merge. Its correctness rests
		// entirely on the driving stream being the one the planner designated, so
		// a designation that no longer resolves must fail the query rather than
		// quietly run as an intersection — an intersection here silently drops
		// every all-NULL group, which is one of the two defects this plan exists
		// to fix.
		driving := p.DrivingStreamIndex()
		if driving < 0 {
			return nil, fmt.Errorf("group-existence merge: driving stream %q resolves to "+
				"no child; the plan's quantifiers were relinked without carrying the "+
				"designation", p.GetDrivingAlias().Name())
		}
		if len(keyVals) == 0 {
			return nil, fmt.Errorf("group-existence merge: no comparison key; the merge " +
				"has no grouping key to align the streams on")
		}
		// Every child of this plan is an aggregate-index scan whose row is
		// [groupCols..., aggregate], so a child spans len(keyVals)+1 slots. An
		// absent child must occupy exactly that many, or the result value's baked
		// ordinals address the wrong slots in every later child.
		width := len(keyVals) + 1
		innerCursor = recordlayer.OuterMergeMultiResume(cursors, compKeyFunc, false, resume,
			driving, func(int) QueryResult { return absentAggregateRow(width) })
	} else {
		// IntersectionMulti returns, per matching comparison key, the list of
		// matching rows (one per child). Mirrors Java's IntersectionMultiCursor;
		// the regular intersection keeps only the first child, which would drop
		// every aggregate but the first.
		innerCursor = recordlayer.IntersectionMultiResume(cursors, compKeyFunc, false, resume)
	}

	merged := &multiIntersectionMergeCursor{
		inner:       innerCursor,
		resultValue: p.GetResultValue(),
		outputType:  outputType,
		// The same len(keyVals)+1 the absent-child filler is sized to, carried to
		// the concatenation so the PRESENT children are held to it as well. The
		// filler being right is worth nothing if a real child disagrees.
		childWidth: len(keyVals) + 1,
	}
	return applySkipLimit(merged, props.Skip, props.ReturnedRowLimit), nil
}

// absentAggregateRow is the row a non-driving aggregate-index stream
// contributes for a group it holds no entry for (RFC-209 §5.3(b)).
//
// All slots are NULL, including the grouping columns: the merged row takes its
// grouping values from the DRIVING stream, so the absent child's copies are
// never read. What matters is the WIDTH — the result value's field ordinals are
// baked at plan time against the flat concatenation of the child rows, so a
// short row would shift every later child's aggregate by however many slots
// were missing and hand each group another group's value.
//
// NULL, not the aggregate's identity: the identity is the result value's job.
// SUM's empty group is NULL and COUNT(col)'s is 0, and only the plan knows
// which aggregate a stream carries. Keeping the cursor's filler uniform is the
// same split Java uses for the ungrouped case, where the plan supplies NULL on
// an empty stream and a coalesce above it turns that into 0 for COUNT.
// The field types are UnknownType, and here that is not discarded knowledge:
// there is no type to state. This row stands in for a group the child index
// holds NO entry for, so no stored value exists whose type could be read, and
// the child plan it substitutes for carries UnknownType as its own result type
// — deriving from it would launder Unknown through one more hop rather than
// answer. Nothing downstream reads these types either: the merged row takes its
// grouping values from the driving stream and its aggregate through the plan's
// result value, so only the WIDTH is load-bearing.
func absentAggregateRow(width int) QueryResult {
	fields := make([]values.Field, width)
	slots := make([]any, width)
	for i := range fields {
		fields[i] = values.Field{Ordinal: i, FieldType: values.UnknownType}
	}
	return QueryResult{Positional: &PositionalRow{
		Type:  &values.RecordType{Fields: fields},
		Slots: slots,
	}}
}

func multiIntersectionCompKeyFunc(keyVals []values.Value) recordlayer.ComparisonKeyFunc[QueryResult] {
	var programs comparisonKeyPrograms
	return func(qr QueryResult) (tuple.Tuple, error) {
		if len(keyVals) > 0 {
			evalKeys, inputEdges, err := programs.forRow(keyVals, qr.Positional)
			if err != nil {
				return nil, fmt.Errorf("multi-intersection comparison key program: %w", err)
			}
			arg, err := compKeyEvalArg(qr, inputEdges...)
			if err != nil {
				return nil, err
			}
			t := make(tuple.Tuple, len(keyVals))
			for i, kv := range evalKeys {
				// Comparison/merge keys are field extractions; an eval
				// failure means the plan and the row disagree (a planner
				// bug), which must fail the QUERY, not the process — Java
				// surfaces it as a RecordCoreException through the cursor
				// future chain. Resolves against the ordinal row via
				// compKeyEvalArg.
				v, err := kv.Evaluate(arg)
				if err != nil {
					return nil, fmt.Errorf("multi-intersection comparison key: %w", err)
				}
				// widenInt32 (RFC-092) + uuidToTupleElement: a UUID group/PK
				// comparison key arrives as a neutral [16]byte the tuple packer
				// can't encode; convert it to tuple.UUID so compareKeys' Pack
				// doesn't panic on a multi-aggregate intersection over a UUID
				// GROUP BY key (RFC-162). Mirrors packedDedupKey's UUID arm.
				t[i] = uuidToTupleElement(widenInt32(v))
			}
			return t, nil
		}
		if qr.PrimaryKey != nil {
			return qr.PrimaryKey, nil
		}
		// No comparison keys and no PK: match rows by their FULL positional
		// content, encoded LOSSLESSLY via the continuation codec — the
		// retired fmt %v rendering collapsed distinct composite rows and
		// collapsed every nil-layout row into one.
		if qr.Positional == nil {
			// A row with no keys, no PK and no positional content cannot be
			// matched against anything; a constant key would intersect ALL
			// such rows.
			return nil, fmt.Errorf("intersection row carries no comparison keys, primary key, or positional content — malformed plan")
		}
		b, err := appendContValue(nil, qr.Positional.Slots)
		if err != nil {
			return nil, fmt.Errorf("multi-intersection positional comparison key: %w", err)
		}
		return tuple.Tuple{b}, nil
	}
}

// multiIntersectionMergeCursor combines each set of matching child rows
// into a single output row. It concatenates every child's positional row
// (grouping columns are identical across children; each child contributes its
// own aggregate column) and evaluates the plan's result value against the
// concatenation to produce the final record. Mirrors Java's
// RecordQueryMultiIntersectionOnValuesPlan.executePlan, which binds each
// child result to its quantifier and evaluates the resultValue.
type multiIntersectionMergeCursor struct {
	inner       recordlayer.RecordCursor[[]QueryResult]
	resultValue values.Value
	// outputType is the plan-admitted result row. The result constructor owns
	// exact field types; deriving a type from display names here would erase
	// them to Unknown and make layout attachment reject an otherwise-correct
	// row after it had already been computed.
	outputType *values.RecordType
	// childWidth is the per-child span the resultValue's ordinals were baked
	// against. It has NO usable zero value: leaving it unset is an error at
	// evaluation time, not a silently disabled check. Set
	// mergeChildWidthUnchecked to opt out deliberately. See mergeChildEvalArg.
	childWidth int
	closed     bool
	// lastNoNext replays the terminal result on a contract-violating re-call
	// (Java's cached no-next result on the map/merge stage) — never re-pulls
	// the inner intersection.
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]
}

func (c *multiIntersectionMergeCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}
	result, err := c.inner.OnNext(ctx)
	if err != nil {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), err
	}
	if !result.HasNext() {
		res := recordlayer.NewResultNoNext[QueryResult](result.GetNoNextReason(), result.GetContinuation())
		c.lastNoNext = &res
		return res, nil
	}

	childResults := result.GetValue()

	// Resolve the resultValue against the CONCATENATED ordinal rows of
	// the matched children — the aggregate-index producer builds a Positional per
	// child; the grouping columns are identical across children and the aggregate
	// columns are distinct, so a plan-time bake against the flat concatenation
	// resolves each resultValue column to the correct slot.
	evalArg, err := mergeChildEvalArg(childResults, c.childWidth)
	if err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, err
	}

	qr := QueryResult{}
	// Emit the authoritative ordinal OUTPUT row. The resultValue is a
	// RecordConstructorValue whose Fields ARE the output columns in output order
	// (the same rc.Fields deriveColumnsFromMultiIntersection names the ColumnDefs
	// from), so evaluating each field against the concatenated child positional row
	// produces a per-slot output row whose names/order match the result-set columns.
	if rc, ok := c.resultValue.(*values.RecordConstructorValue); ok {
		if c.outputType == nil || len(c.outputType.Fields) != len(rc.Fields) {
			return recordlayer.RecordCursorResult[QueryResult]{}, fmt.Errorf(
				"multi-intersection result type width does not match result constructor")
		}
		posSlots := make([]any, len(rc.Fields))
		for i, f := range rc.Fields {
			fv, ferr := f.Value.Evaluate(evalArg)
			if ferr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, ferr
			}
			posSlots[i] = fv
		}
		qr.Positional = &PositionalRow{Type: c.outputType, Slots: posSlots}
	} else if c.resultValue != nil {
		// Non-RC resultValue: a scalar output row.
		datum, derr := c.resultValue.Evaluate(evalArg)
		if derr != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, derr
		}
		qr.Positional = scalarPositionalRow(datum)
	} else if p, ok := evalArg.(*PositionalRow); ok {
		// No resultValue: the concatenated child row IS the output.
		qr.Positional = p
	}
	return recordlayer.NewResultWithValue(qr, result.GetContinuation()), nil
}

// mergeChildWidthUnchecked is the explicit opt-out from the per-child width
// assertion, for callers that genuinely cannot state a width. It is a NEGATIVE
// sentinel rather than zero so that forgetting to set the width can never be
// mistaken for choosing to skip the check, and it is matched by EQUALITY so a
// computed negative cannot opt out by accident.
const mergeChildWidthUnchecked = -1

// mergeChildEvalArg builds the eval argument the MultiIntersection resultValue is
// evaluated against: the CONCATENATED PositionalRow of the matched child rows
// (a bare OrdinalRow whose plan-produced concatenated type the baked
// references read). Fields are concatenated in child order; grouping
// columns repeat across children with the same value, so a first-match bind is
// order-safe. Returns nil if any child lacks a Positional (should not happen — the
// aggregate-index producer builds one per child).
// expectedWidth is the per-child span the result value's ordinals were BAKED
// against at plan time (len(groupCols)+1). It is verified here rather than
// assumed because the assumption is derived independently in three places — the
// planning rule that bakes the ordinals, the cursor that sizes the absent-child
// filler, and this concatenation — and nothing made the three agree. A child
// arriving at a different width does not fail: it shifts every LATER child's
// aggregate by the difference and hands each group another group's value, with
// every row well-formed and the row count unchanged. That is the silent
// wrong-rows class, so it is an error, not a best-effort nil.
//
// The contract is INVERTED from the obvious one, deliberately: any width that
// is not a real width fails CLOSED. An unset (or otherwise nonsensical) width
// is an error, and opting out takes the exact mergeChildWidthUnchecked
// sentinel -- not merely "some negative number". A -2 arriving from arithmetic
// on an empty grouping key is a bug and must be loud; only the constant means
// "I have considered this and there is no width to state".
//
// The obvious spelling — "0 disables the check" — made the assertion fail OPEN,
// and it did so invisibly. Deleting the caller's `childWidth:` line left the
// struct field at its zero value, the check silently disabled, and the entire
// gate green: the tests below call this function with an explicit width, so
// they prove the function CHECKS but never that the caller ASKS. An assertion
// whose disarmed state is also its default is not an assertion; it is a
// suggestion that happens to be followed.
func mergeChildEvalArg(childResults []QueryResult, expectedWidth int) (any, error) {
	if expectedWidth != mergeChildWidthUnchecked && expectedWidth <= 0 {
		return nil, fmt.Errorf(
			"group-existence merge: no per-child width was stated, so the plan's baked " +
				"result-value ordinals cannot be validated against the rows actually " +
				"flowed. This is a wiring bug in the cursor's construction, not a " +
				"property of the data: set childWidth from the plan, or " +
				"mergeChildWidthUnchecked to opt out on purpose")
	}
	var fields []values.Field
	var slots []any
	ord := 0
	for ci, cr := range childResults {
		p := cr.Positional
		if p == nil || p.Type == nil {
			return nil, nil
		}
		if expectedWidth != mergeChildWidthUnchecked && len(p.Type.Fields) != expectedWidth {
			return nil, fmt.Errorf(
				"group-existence merge: child %d flowed %d columns, but the plan's result "+
					"value was baked against %d per child; every later child's aggregate "+
					"would be read from the wrong slot and each group would report another "+
					"group's value",
				ci, len(p.Type.Fields), expectedWidth)
		}
		for i, f := range p.Type.Fields {
			nf := f
			nf.Ordinal = ord
			fields = append(fields, nf)
			var v any
			if i < len(p.Slots) {
				v = p.Slots[i]
			}
			slots = append(slots, v)
			ord++
		}
	}
	if len(fields) == 0 {
		return nil, nil
	}
	return &PositionalRow{Type: &values.RecordType{Fields: fields}, Slots: slots}, nil
}

func (c *multiIntersectionMergeCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *multiIntersectionMergeCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*multiIntersectionMergeCursor)(nil)

func executeLoadByKeys(
	_ context.Context,
	p *plans.RecordQueryLoadByKeysPlan,
	store *recordlayer.FDBRecordStore,
	_ *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	keys := p.GetKeysSource().GetPrimaryKeys()
	if len(keys) == 0 {
		return recordlayer.Empty[QueryResult](), nil
	}
	// Java's shape (RecordQueryLoadByKeysPlan.executePlan): the KEY list
	// is the resumable source — RecordCursor.fromList(keys, continuation)
	// — with each key loaded lazily and missing records skipped. The
	// eager buffered form DROPPED the incoming continuation while still
	// emitting resumable-looking list tokens, so a resume replayed every
	// row from 0. Lazy loading also holds one record at a time, so no
	// statement-memory charge is needed (the RFC-130 buffer accumulated
	// every loaded record).
	return applySkipLimit(
		&loadByKeysCursor{
			keys:  recordlayer.FromListWithContinuation(keys, continuation),
			store: store,
		},
		props.Skip, props.ReturnedRowLimit,
	), nil
}

// loadByKeysCursor loads records for a resumable key-list cursor,
// skipping keys with no record (Java's .filter(Objects::nonNull)). Each
// emitted row carries the KEY cursor's continuation — the wire format is
// the plain 4-byte list-index token, byte-identical to Java's.
type loadByKeysCursor struct {
	keys   recordlayer.RecordCursor[tuple.Tuple]
	store  *recordlayer.FDBRecordStore
	closed bool
}

func (c *loadByKeysCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	for {
		r, err := c.keys.OnNext(ctx)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		if !r.HasNext() {
			return recordlayer.NewResultNoNext[QueryResult](r.GetNoNextReason(), r.GetContinuation()), nil
		}
		pk := r.GetValue()
		rec, err := c.store.LoadRecord(pk)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, fmt.Errorf("executor: LoadByKeys pk=%v: %w", pk, err)
		}
		if rec == nil {
			continue
		}
		return recordlayer.NewResultWithValue(FromStoredRecord(rec), r.GetContinuation()), nil
	}
}

func (c *loadByKeysCursor) Close() error {
	c.closed = true
	return c.keys.Close()
}

func (c *loadByKeysCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*loadByKeysCursor)(nil)

func executeUnorderedUnion(
	ctx context.Context,
	p *plans.RecordQueryUnorderedUnionPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inners := p.GetInners()
	if len(inners) == 0 {
		return recordlayer.Empty[QueryResult](), nil
	}
	if len(inners) == 1 {
		c, err := ExecutePlan(ctx, inners[0], store, evalCtx, continuation, props.ClearSkipAndLimit())
		if err != nil {
			return nil, err
		}
		return applySkipLimit(c, props.Skip, props.ReturnedRowLimit), nil
	}
	// Java UnorderedUnionCursor: per-child slots in UnionCursorContinuation,
	// order unspecified (a deterministic serial order is a legal
	// realization), a limit-stopped child does not stop the union — the
	// remaining children keep emitting — and the union's terminal carries
	// the strongest child reason with every child's resume slot. This
	// replaces the RFC-180 A2 loud decline (which itself replaced feeding
	// the parent token raw to every child).
	resume, derr := decodeUnionContinuation(continuation, len(inners))
	if derr != nil {
		return nil, derr
	}
	var md *recordlayer.RecordMetaData
	if store != nil {
		md = store.GetRecordMetaData()
	}
	// SQL exposes a UNION's column names from the FIRST branch; later branches union
	// by POSITION. RecordQueryUnionPlan normalizes this (executeUnionStreaming), but
	// the unordered concat did NOT — so a branch whose output columns are named
	// differently from the first branch (e.g. mismatched aggregate aliases X vs Y)
	// flowed its rows under its OWN keys, and a downstream by-name read of the union's
	// (first-branch) column dropped them (TODO 7.6-union-remap / RFC-078). Remap each
	// later branch's keys to the first branch's, position-wise, exactly as the sibling
	// concat RecordQueryUnionPlan does. A no-op when names already agree (the common case).
	firstBranchKeys := planColumnNamesWithMD(inners[0], md)
	childProps := props.ClearSkipAndLimit()
	u := &unorderedUnionCursor{
		children:  make([]recordlayer.RecordCursor[QueryResult], len(inners)),
		states:    make([]recordlayer.RecordCursorContinuation, len(inners)),
		reasons:   make([]recordlayer.NoNextReason, len(inners)),
		stopped:   make([]bool, len(inners)),
		exhausted: make([]bool, len(inners)),
	}
	for i, inner := range inners {
		if resume[i].exhausted {
			u.exhausted[i] = true
			u.states[i] = &recordlayer.EndContinuation{}
			continue
		}
		c, err := ExecutePlan(ctx, inner, store, evalCtx, resume[i].continuation, childProps)
		if err != nil {
			_ = u.Close()
			return nil, err
		}
		if i > 0 && firstBranchKeys != nil {
			srcKeys := planColumnNamesWithMD(inner, md)
			if srcKeys != nil && !slices.Equal(srcKeys, firstBranchKeys) {
				target := firstBranchKeys
				c = recordlayer.MapCursor(c, func(qr QueryResult) QueryResult {
					return remapUnionColumnsByPosition(qr, srcKeys, target)
				})
			}
		}
		u.children[i] = c
		if len(resume[i].continuation) > 0 {
			u.states[i] = recordlayer.NewBytesContinuation(resume[i].continuation)
		}
	}
	return applySkipLimit(u, props.Skip, props.ReturnedRowLimit), nil
}

// unorderedUnionCursor is the serial realization of Java's
// UnorderedUnionCursor: children emit in deterministic index order (Java's
// order is explicitly unspecified), a child's out-of-band stop parks that
// child and the union keeps emitting from the rest (a shared scan/time
// limiter parks them all in short order), and the terminal result carries the
// STRONGEST stopped-child reason with a per-child UnionContinuation slot —
// exhausted children marked, stopped children at their positions. Every
// emitted row's continuation snapshots all children's current states.
type unorderedUnionCursor struct {
	children   []recordlayer.RecordCursor[QueryResult] // nil = exhausted at resume
	states     []recordlayer.RecordCursorContinuation  // last-known per child; nil = START
	reasons    []recordlayer.NoNextReason              // valid where stopped
	stopped    []bool
	exhausted  []bool
	idx        int
	closed     bool
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]
}

func (u *unorderedUnionCursor) snapshotContinuation() recordlayer.RecordCursorContinuation {
	children := make([]recordlayer.RecordCursorContinuation, len(u.states))
	for i, c := range u.states {
		if c == nil {
			// A not-yet-pulled child is at START (Java's START_PROTO); a
			// raw nil interface would panic the encoder's IsEnd probe.
			children[i] = &recordlayer.StartContinuation{}
			continue
		}
		children[i] = c
	}
	return &mergeSortContinuation{children: children}
}

func (u *unorderedUnionCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if u.closed {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
	}
	if u.lastNoNext != nil {
		return *u.lastNoNext, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		// Find the next live child from the current position.
		live := -1
		for off := 0; off < len(u.children); off++ {
			i := (u.idx + off) % len(u.children)
			if u.children[i] != nil && !u.exhausted[i] && !u.stopped[i] {
				live = i
				break
			}
		}
		if live < 0 {
			// Terminal: all children exhausted → genuine end; any stopped
			// child → strongest reason + per-child resume slots.
			anyStopped := false
			reason := recordlayer.SourceExhausted
			found := false
			for i := range u.children {
				if !u.stopped[i] {
					continue
				}
				anyStopped = true
				if !found || u.reasons[i].IsOutOfBand() || reason.IsSourceExhausted() {
					reason = u.reasons[i]
					found = true
				}
			}
			if !anyStopped {
				return recordlayer.NewResultNoNext[QueryResult](
					recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
				), nil
			}
			res := recordlayer.NewResultNoNext[QueryResult](reason, u.snapshotContinuation())
			u.lastNoNext = &res
			return res, nil
		}
		u.idx = live
		result, err := u.children[live].OnNext(ctx)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		if result.HasNext() {
			u.states[live] = result.GetContinuation()
			return recordlayer.NewResultWithValue(result.GetValue(), u.snapshotContinuation()), nil
		}
		reason := result.GetNoNextReason()
		if reason == recordlayer.SourceExhausted {
			u.exhausted[live] = true
			u.states[live] = &recordlayer.EndContinuation{}
		} else {
			u.stopped[live] = true
			u.reasons[live] = reason
			u.states[live] = result.GetContinuation()
		}
		u.idx = (live + 1) % len(u.children)
	}
}

func (u *unorderedUnionCursor) Close() error {
	if u.closed {
		return nil
	}
	u.closed = true
	var firstErr error
	for _, c := range u.children {
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (u *unorderedUnionCursor) IsClosed() bool { return u.closed }

// producesMergedRows reports whether a plan emits merged join rows
// (multiple quantifiers' columns concatenated into one positional row with
// per-leg windows) rather than single-table rows. A filter over such a plan
// resolves QOV predicates leg-locally through the windows, not via an alias
// binding.
//
// This list must stay in sync with the merged-row producers: mergeRows
// (executor.go) for the NLJ cursor, and the FlatMap cursor's merge output
// (flat_map_cursor.go). Those are the only two sites that emit merged rows.
// A future merged-row operator (e.g. a hash/merge join) MUST be added here,
// or a filter over it would bind the merged row under one alias and bare-
// resolve qov(b).col to the wrong quantifier (see DIVERGENCES.md).
func producesMergedRows(p plans.RecordQueryPlan) bool {
	switch p.(type) {
	case *plans.RecordQueryNestedLoopJoinPlan, *plans.RecordQueryFlatMapPlan:
		return true
	}
	return false
}

func executePredicatesFilter(
	ctx context.Context,
	p *plans.RecordQueryPredicatesFilterPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inner, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}
	inputQOV, err := requireSoleInputQOV(p)
	if err != nil {
		return nil, err
	}
	preds := p.GetPredicates()
	innerAlias := p.GetInnerAlias()
	// Bind the current row under innerAlias only when the inner plan
	// produces single-table rows (bare-keyed scans/index scans). For such
	// rows a QOV predicate qov(alias).col must resolve via the binding
	// (bare lookup), since the row carries no "ALIAS.COL" qualified key.
	//
	// When the inner plan produces MERGED rows (NLJ / FlatMap join output),
	// the row is an ordinal merged row whose per-leg windows resolve a
	// qov(alias).col reference leg-locally. We must NOT bind the merged row
	// under a single alias: a qov(b).col lookup would
	// then bare-resolve to whichever quantifier last wrote the bare key —
	// e.g. on a null-filled LEFT JOIN row (b absent), qov(b).id would wrongly
	// pick up the outer row's bare ID instead of NULL.
	bindAlias := innerAlias.Name() != "" && !producesMergedRows(p.GetInner())
	inputEdges := []values.QuantifiedObjectValue{inputQOV}
	if bindAlias && innerAlias != inputQOV.Correlation() {
		aliasQOV, aliasErr := values.NewQuantifiedObjectValue(innerAlias, inputQOV.FlowedType())
		if aliasErr != nil {
			return nil, fmt.Errorf("predicates filter input alias %q: %w", innerAlias.Name(), aliasErr)
		}
		inputEdges = append(inputEdges, aliasQOV)
	}
	// On the positional frontier a QOV(innerAlias).col resolves
	// via the bare-positional fallback in evaluateCorrelated (Correlations miss →
	// Positional), so bindAlias is NOT a reason to wrap — only a genuine
	// param/subquery/outer-binding is, or a CURRENT_TIMESTAMP-family
	// reference that needs the statement clock a RowEvalContext carries.
	// When neither is present, flow the bare row.
	posNeedsCtx := hasBindingContext(evalCtx) || predicatesDependOnStatementClock(preds)
	// When the input flows a 2-way ordinal join's merged
	// positional row, predicates evaluate under the LEG WINDOWS — computed
	// once, from the input plan's result value.
	legSpans, windowsOK := downstreamLegWindows(p.GetInner())
	filtered := &filterResultCursor{
		inner: inner,
		pred: func(qr QueryResult) (bool, error) {
			var rowCtx any
			switch {
			case qr.Positional != nil && qr.Positional.Layout != nil:
				rowCtx, err = frontierRowContext(qr.Positional, evalCtx, posNeedsCtx, inputEdges...)
				if err != nil {
					return false, err
				}
			case qr.Positional != nil && windowsOK:
				// The merged positional row of a gated 2-way
				// ordinal join — a leg reference QOV(leg).col needs
				// its leg window (unconditional: even with no binding context,
				// the leg bindings are required; the bare merged row misreads
				// leg-relative ordinals — a wrong-slot hazard).
				rowCtx = legWindowRowContext(qr.Positional, evalCtx, legSpans)
			case qr.Positional != nil && bindAlias && isBareScalarRow(qr.Positional):
				// A BARE SCALAR inner row (a non-ordinal lateral-array UNNEST's
				// Explode flows a raw int64, wrapped by scalarPositionalRow into a
				// 1-slot `_0` row — RFC-142). A WHERE on the element references the
				// whole QuantifiedObjectValue(innerAlias) (Java binds the primitive
				// flowed value directly, not a FieldValue — see
				// generateCorrelatedFieldAccess), so bind the UNWRAPPED scalar under
				// innerAlias so QOV(innerAlias) resolves to it. Without this the QOV
				// whole-row fallback returns the 1-slot row, not the scalar.
				layout, layoutErr := p.GetInner().ProvidedOutputLayout()
				if layoutErr != nil {
					return false, fmt.Errorf("predicates filter scalar input layout: %w", layoutErr)
				}
				rowCtx, err = scalarLayoutRowContext(layout, qr.Positional, evalCtx, inputEdges...)
				if err != nil {
					return false, err
				}
			case qr.Positional != nil:
				// The non-join frontier flows an authoritative
				// ordinal row — resolve predicates by ordinal (loud on a miss, no
				// name-map fallback). A QOV(innerAlias).col resolves via the
				// bare-positional fallback in evaluateCorrelated, so no alias
				// binding is needed here.
				rowCtx, err = frontierRowContext(qr.Positional, evalCtx, posNeedsCtx, inputEdges...)
				if err != nil {
					return false, err
				}
			}
			for _, pred := range preds {
				res, err := pred.Eval(rowCtx)
				if err != nil {
					return false, err
				}
				if res != predicates.TriTrue {
					return false, nil
				}
			}
			return true, nil
		},
	}
	return applySkipLimit(filtered, props.Skip, props.ReturnedRowLimit), nil
}

func executeMap(
	ctx context.Context,
	p *plans.RecordQueryMapPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inner, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}
	inputQOV, err := requireSoleInputQOV(p)
	if err != nil {
		return nil, err
	}
	resultValue := p.GetResultValue()
	// On the positional frontier an outer correlation resolves via
	// the eval context's binder before the bare-positional frontier fallback.
	// A CURRENT_TIMESTAMP-family result value needs the statement clock a
	// RowEvalContext carries — a bare frontier row would drift per row.
	posNeedsCtx := hasBindingContext(evalCtx) || values.DependsOnStatementClock(resultValue)
	// The Map output schema is row-invariant — derive the emitted
	// positional row's OUTPUT names once from the result value's record type. When
	// the result is a RecordConstructorValue, evaluate its Fields INDIVIDUALLY into
	// dense slots (never through the collapsing name map — a duplicate output name
	// keeps both slots by ordinal), mirroring executeProjection.
	var mapPosType *values.RecordType
	mapRC, _ := resultValue.(*values.RecordConstructorValue)
	if rt, ok := resultValue.Type().(*values.RecordType); ok {
		mapPosType = rt
	}
	// When the input flows a 2-way ordinal join's merged
	// positional row, the result value evaluates under the LEG WINDOWS —
	// computed once, from the input plan's result value.
	legSpans, windowsOK := downstreamLegWindows(p.GetInner())
	var evalErr error
	mapped := recordlayer.MapCursor(inner, func(qr QueryResult) QueryResult {
		if evalErr != nil {
			return qr
		}
		var rowCtx any
		if qr.Positional != nil && qr.Positional.Layout != nil {
			rowCtx, evalErr = frontierRowContext(qr.Positional, evalCtx, posNeedsCtx, inputQOV)
			if evalErr != nil {
				return qr
			}
		} else if qr.Positional != nil && windowsOK {
			// The merged positional row of a gated 2-way
			// ordinal join — a leg reference QOV(leg).col needs its
			// leg window (unconditional; see executePredicatesFilter).
			rowCtx = legWindowRowContext(qr.Positional, evalCtx, legSpans)
		} else if qr.Positional != nil {
			// The non-join frontier flows an authoritative ordinal
			// row — resolve the result value by ordinal (loud on a miss).
			rowCtx, evalErr = frontierRowContext(qr.Positional, evalCtx, posNeedsCtx, inputQOV)
			if evalErr != nil {
				return qr
			}
		}
		// A Map's output IS a PositionalRow. When the result value is a
		// RecordConstructor, evaluate each field individually into dense slots; a
		// scalar result wraps in a 1-slot row.
		var pos *PositionalRow
		if mapRC != nil && mapPosType != nil {
			slots := make([]any, len(mapRC.Fields))
			for i, f := range mapRC.Fields {
				fv, ferr := f.Value.Evaluate(rowCtx)
				if ferr != nil {
					evalErr = ferr
					return qr
				}
				slots[i] = fv
			}
			pos = &PositionalRow{Type: mapPosType, Slots: slots}
		} else {
			m, err := resultValue.Evaluate(rowCtx)
			if err != nil {
				evalErr = err
				return qr
			}
			pos = scalarPositionalRowOfType(m, resultValue.Type())
		}
		return QueryResult{Positional: pos, Record: qr.Record, PrimaryKey: qr.PrimaryKey}
	})
	return &errCheckCursor{inner: applySkipLimit(mapped, props.Skip, props.ReturnedRowLimit), err: &evalErr}, nil
}

func executeFirstOrDefault(
	ctx context.Context,
	p *plans.RecordQueryFirstOrDefaultPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	resume, innerCont, decodeErr := decodeFirstOrDefaultContinuation(continuation)
	if decodeErr != nil {
		return nil, decodeErr
	}
	// A resume from the emitted row's own continuation: the single row
	// was already consumed — the cursor is exhausted, never re-executed
	// (re-execution would re-emit the row: a duplicate under any parent
	// that snapshots child positions).
	if resume == fodResumeConsumed {
		return recordlayer.Empty[QueryResult](), nil
	}
	inner, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, innerCont, props)
	if err != nil {
		return nil, err
	}
	result, err := inner.OnNext(ctx)
	if err != nil {
		_ = inner.Close()
		return nil, err
	}
	if result.HasNext() {
		first := result.GetValue()
		if p.IsStrict() {
			// SQL scalar-subquery cardinality: a second row is a violation (21000),
			// not a silent truncation. Probe exactly one more row. This runs fresh
			// per outer row under the driving FlatMap, so at-most-one is enforced
			// per outer (mirrors the uncorrelated executor.EvaluateScalarSubquery).
			second, serr := inner.OnNext(ctx)
			if serr != nil {
				_ = inner.Close()
				return nil, serr
			}
			if second.HasNext() {
				_ = inner.Close()
				return nil, api.NewErrorf(api.ErrCodeCardinalityViolation,
					"scalar subquery returned more than one row")
			}
			// An out-of-band (resource-limit) stop after the first row means the
			// input was TRUNCATED — a second matching row may exist beyond the
			// cap, so at-most-one is not yet proven. That is a PAGE BOUNDARY, not
			// a failure: hand the stop up so the parent pages.
			//
			// Whole-leg restart, NOT a checkpoint on the inner's position: the
			// operator is holding `first`, and a stop cursor carries no value, so
			// resuming the inner past `first` would lose it. Restarting re-derives
			// it. This is the one branch where the restart is the only correct
			// resume — see restartAfterOutOfBand.
			if second.GetNoNextReason().IsOutOfBand() {
				_ = inner.Close()
				return restartAfterOutOfBand(second.GetNoNextReason()), nil
			}
		}
		_ = inner.Close()
		return newSingleResultCursor(first), nil
	}
	_ = inner.Close()
	// An out-of-band (resource-limit) stop before the first row means the input
	// was TRUNCATED — we cannot tell whether a matching row would have followed,
	// so the default must NOT be fabricated (that is Java's
	// RecordQueryFirstOrDefaultPlan.java:100-106 / issue 3220, which treats the
	// truncated inner as an empty one and answers EXISTS wrongly). Hand the stop
	// up as a CHECKPOINT on the inner's own position (see
	// checkpointAfterOutOfBand): at this point the operator's entire state is
	// "zero rows seen", which the inner's continuation captures exactly, so the
	// resumed leg carries on from where it stopped instead of starting over.
	if result.GetNoNextReason().IsOutOfBand() {
		return checkpointAfterOutOfBand(result)
	}
	qr, err := firstOrDefaultResultFromValue(p, p.GetDefaultValue())
	if err != nil {
		return nil, err
	}
	return newSingleResultCursor(qr), nil
}

// firstOrDefaultResultFromValue materializes the empty arm in the plan's exact
// output carrier. A record NULL is not a scalar `_0 UNKNOWN` wrapper and it is
// not a matched record whose fields merely happen to be NULL: it is an exact
// record-shaped physical shell with an explicit absent-current marker.
func firstOrDefaultResultFromValue(
	p *plans.RecordQueryFirstOrDefaultPlan,
	defaultValue values.Value,
) (QueryResult, error) {
	if p == nil {
		return QueryResult{}, fmt.Errorf("FirstOrDefault default has no plan")
	}
	resultType := p.GetResultType()
	if recordType, isRecord := resultType.(*values.RecordType); isRecord {
		if constructor, ok := defaultValue.(*values.RecordConstructorValue); ok {
			result, err := resultFromValue(constructor)
			if err != nil {
				return QueryResult{}, err
			}
			if result.Positional == nil || result.Positional.Type == nil ||
				!result.Positional.Type.Equals(recordType) || len(result.Positional.Slots) != len(recordType.Fields) {
				return QueryResult{}, fmt.Errorf(
					"FirstOrDefault RECORD constructor type %v is incompatible with result type %s",
					result.Positional, recordType)
			}
			return result, nil
		}
		if defaultValue != nil {
			value, err := defaultValue.Evaluate(nil)
			if err != nil {
				return QueryResult{}, err
			}
			if value != nil {
				return QueryResult{}, fmt.Errorf(
					"FirstOrDefault non-constructor RECORD default evaluated to %T", value)
			}
		}
		layout, err := p.ProvidedOutputLayout()
		if err != nil {
			return QueryResult{}, fmt.Errorf("FirstOrDefault provided output layout: %w", err)
		}
		row, err := NewLayoutPositionalRow(recordType, layout)
		if err != nil {
			return QueryResult{}, fmt.Errorf("FirstOrDefault default carrier: %w", err)
		}
		presence, err := values.NewOrdinalCarrierMatchPresence(layout, false)
		if err != nil {
			return QueryResult{}, fmt.Errorf("FirstOrDefault default presence: %w", err)
		}
		row.LayoutPresence = presence
		return QueryResult{Positional: row}, nil
	}

	var value any
	if defaultValue != nil {
		var err error
		value, err = defaultValue.Evaluate(nil)
		if err != nil {
			return QueryResult{}, err
		}
	}
	return QueryResult{Positional: scalarPositionalRowOfType(value, resultType)}, nil
}

// An out-of-band stop underneath a first-or-default is a resumable PAGE
// BOUNDARY, not a failure. The per-page time budget exists precisely so a page
// ends before FDB's 5s transaction wall, and every Java operator propagates such
// a stop upward with a continuation rather than throwing
// (FlatMapPipelinedCursor.java:325-367 forwards the inner's reason verbatim;
// MemorySortCursor.java:78-93 checkpoints its sorter; CursorLimitManager.java:144
// is the ONLY place in the Java engine that turns a limit into an exception, and
// only under failOnScanLimitReached). Collapsing it into an error here made a
// slow-but-healthy cluster fail a legitimate query with 54F01 — the cluster was
// not out of budget, the PAGE was.
//
// This operator is EAGER: unlike a streaming cursor it must drain its inner leg
// to decide what to flow, so it meets the stop itself instead of passing a
// no-next result through. It therefore has to mint the resume position, and
// which position is correct depends on what it was holding when the stop
// arrived — hence two functions below, not one.
//
// The asymmetry is LOAD-BEARING, not stylistic, and it is the thing to read
// before collapsing the two branches into one. Point the empty branch's
// checkpoint at the strict branch and the operator silently drops the row it was
// holding: the resumed inner starts PAST that row, finds nothing, and the
// default is fabricated in its place. A correlated scalar subquery then answers
// NULL where it had a value — the observable is a caller failing to scan NULL
// into a non-nullable column, and TestFDB_NotExistsOutOfBandStop_PaginatesNotErrors
// /ScannedRowsLimitStrictScalarSubquery is what catches it. Point the strict
// branch's restart at the empty branch and nothing is wrong, only slow —
// unboundedly so, which the livelock backstop turns into a loud failure.
//
// Neither is what Java does here. RecordQueryFirstOrDefaultPlan.java:100-106
// hands its child a null continuation and reads a truncated inner as an EMPTY
// one, silently answering NOT EXISTS = true from a partial scan — a wrong answer
// its own comment flags as unfixed (FoundationDB/fdb-record-layer#3220), and
// RecordCursor.java:920-925 names the fix in the abstract: when the future is
// backed by a cursor that may stop out-of-band, "it may be desirable to include
// the progress included in the underlying cursor in the final continuation …
// something more bespoke may be required". checkpointAfterOutOfBand is that
// bespoke thing.
//
// Consumers that cannot page still fail loud: any eager drain routes through
// errIfBufferTruncated / errIfDrainTruncated, which turn this same out-of-band
// stop into 54F01. Only a parent with a real resume point — the nested-loop
// flat map, or the paginating SQL driver — converts it into a page boundary.

// checkpointAfterOutOfBand handles the stop that arrives BEFORE any inner row.
// The operator's entire state at that instant is "zero rows seen", and the inner
// leg's own continuation captures exactly where it got to — so the two together
// are a complete resume position. The resumed leg carries on from the checkpoint
// and the first row it then finds is still the leg's first row.
//
// This is what makes the fix work on a genuinely slow cluster rather than only
// on a slightly slow one: progress becomes per INNER ROW instead of per outer
// row. A restart here would re-scan the same prefix every page, so a leg that
// cannot fit one page's budget would never finish — the driver's no-progress
// tripwire would fire and the query would fail with 54F01 anyway, just with a
// different message.
//
// Falls back to a whole-leg restart only when the inner has no resumable
// position to checkpoint (a start/end continuation), which is not a state any
// current leaf produces after an out-of-band stop but is cheap to be right about.
func checkpointAfterOutOfBand(result recordlayer.RecordCursorResult[QueryResult]) (recordlayer.RecordCursor[QueryResult], error) {
	reason := result.GetNoNextReason()
	cont := result.GetContinuation()
	if cont == nil || cont.IsEnd() {
		return restartAfterOutOfBand(reason), nil
	}
	inner, err := cont.ToBytes()
	if err != nil {
		return nil, err
	}
	if len(inner) == 0 {
		return restartAfterOutOfBand(reason), nil
	}
	return recordlayer.Stopped[QueryResult](reason,
		recordlayer.NewBytesContinuation(encodeFirstOrDefaultCheckpoint(inner))), nil
}

// restartAfterOutOfBand handles the stop that arrives while the operator is
// already holding its first row — the strict at-most-one probe. A stop cursor
// carries no value, so resuming the inner PAST that row would lose it; the whole
// leg has to run again. Nothing is lost by that: no row of this operator was
// emitted, and the re-run re-derives both the row and the cardinality proof.
//
// The livelock this can produce is real and deliberate: if one page's budget
// cannot reach the first row and probe past it, every page restarts at the same
// position and the driver's no-progress tripwire fires. That is the correct-or-
// loud outcome for a budget too small to answer the query at all, and it is the
// backstop the empty-branch checkpoint above no longer needs.
func restartAfterOutOfBand(reason recordlayer.NoNextReason) recordlayer.RecordCursor[QueryResult] {
	return recordlayer.Stopped[QueryResult](reason,
		recordlayer.NewBytesContinuation([]byte{fodTagRestart}))
}

func executeDefaultOnEmpty(
	ctx context.Context,
	p *plans.RecordQueryDefaultOnEmptyPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	// Java RecordQueryDefaultOnEmptyPlan.executePlan delegates to
	//   RecordCursor.orElse(cont -> child.executePlan(store, ctx, cont, clearSkipAndLimit),
	//                        (executor, cont) -> RecordCursor.fromList(executor, [default], cont),
	//                        continuation)
	//     .skipThenLimit(skip, limit)
	// OrElseCursor serializes state UNDECIDED/USE_INNER/USE_OTHER, so once the
	// inner has produced a row the resumed cursor stays on USE_INNER (never
	// fabricating the default), and EVERY emitted row's continuation carries the
	// inner's real position — which fixes both a paging loop that never advanced
	// (a single-result default cursor emitted a nil StartContinuation forever) and
	// a spurious null-extension on a mid-stream resume past the last inner row.
	//
	// The inner (primary) clears skip/limit; the default (alternative) is a
	// single-row list cursor that respects its continuation; skip/limit apply to
	// the whole OrElse — mirroring Java's clearSkipAndLimit-on-child +
	// skipThenLimit-on-orElse split. An out-of-band inner stop before the first
	// row flows through OrElse's UNDECIDED no-next (Java's behaviour): the caller
	// resumes and the default is NEVER fabricated on truncation. FirstOrDefault
	// reaches the same outcome by a different route (restartAfterOutOfBand) —
	// being eager, it has to convert the stop into one explicitly. The
	// uncorrelated scalar-subquery path has no parent that can page and so keeps
	// its RFC-106a truncation error.
	nestedProps := props.ClearSkipAndLimit()
	primaryFactory := func(cont []byte) recordlayer.RecordCursor[QueryResult] {
		inner, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, cont, nestedProps)
		if err != nil {
			return &errResultCursor{err: err}
		}
		return recordlayer.MapErrCursor(inner, func(result QueryResult) (QueryResult, error) {
			return normalizeDefaultOnEmptyResult(p, result)
		})
	}
	alternativeFactory := func(cont []byte) recordlayer.RecordCursor[QueryResult] {
		// Evaluate the default lazily (only when the inner is empty), matching
		// Java's else-lambda: onEmptyResultValue.eval runs inside fromList's
		// supplier. A nil default flows a scalar NULL row.
		var defaultRow QueryResult
		if defaultVal := p.GetDefaultValue(); defaultVal != nil {
			qr, err := defaultOnEmptyResultFromValue(p, defaultVal)
			if err != nil {
				return &errResultCursor{err: err}
			}
			defaultRow = qr
		} else {
			defaultRow = QueryResult{Positional: scalarPositionalRow(nil)}
		}
		return recordlayer.FromListWithContinuation([]QueryResult{defaultRow}, cont)
	}
	orElse := recordlayer.OrElseWithContinuation(primaryFactory, alternativeFactory, continuation)
	return applySkipLimit(orElse, props.Skip, props.ReturnedRowLimit), nil
}

// normalizeDefaultOnEmptyResult publishes the operator's reconciled output
// type on an inner row. DefaultOnEmpty is a UNION of two result alternatives:
// a non-null child row and a nullable default can produce a nullable RECORD.
// Forwarding the child's narrower runtime type unchanged makes the row disagree
// with the plan's exact provided layout even though its values are valid for
// that layout. Retag only the root-nullability widening admitted by the plan
// constructor; every field, ordinal, and slot count must still agree exactly.
func normalizeDefaultOnEmptyResult(
	p *plans.RecordQueryDefaultOnEmptyPlan,
	result QueryResult,
) (QueryResult, error) {
	resultType, isRecord := p.GetResultType().(*values.RecordType)
	if !isRecord {
		return result, nil
	}
	if result.Positional == nil || result.Positional.Type == nil {
		return QueryResult{}, fmt.Errorf("DefaultOnEmpty record result has no typed positional row")
	}
	actualType := result.Positional.Type
	if !values.WithNullability(actualType, true).Equals(values.WithNullability(resultType, true)) ||
		len(result.Positional.Slots) != len(resultType.Fields) {
		return QueryResult{}, fmt.Errorf(
			"DefaultOnEmpty runtime row type %s is incompatible with result type %s",
			actualType, resultType)
	}
	if actualType.Equals(resultType) && result.Positional.Layout == nil {
		return result, nil
	}
	row := *result.Positional
	row.Type = resultType
	row.Slots = append([]any(nil), result.Positional.Slots...)
	// The child's layout describes the child type. DefaultOnEmpty publishes a
	// fresh identity layout for the reconciled union type; ExecutePlan attaches
	// that parent layout after the OrElse cursor chooses its branch.
	row.Layout = nil
	row.LayoutPresence = nil
	result.Positional = &row
	return result, nil
}

func defaultOnEmptyResultFromValue(
	p *plans.RecordQueryDefaultOnEmptyPlan,
	defaultValue values.Value,
) (QueryResult, error) {
	resultType, isRecord := p.GetResultType().(*values.RecordType)
	if isRecord {
		if _, isConstructor := defaultValue.(*values.RecordConstructorValue); !isConstructor {
			value, err := defaultValue.Evaluate(nil)
			if err != nil {
				return QueryResult{}, err
			}
			if value == nil {
				return QueryResult{Positional: NewPositionalRow(resultType)}, nil
			}
			return QueryResult{}, fmt.Errorf(
				"DefaultOnEmpty non-constructor RECORD default evaluated to %T", value)
		}
	}
	result, err := resultFromValue(defaultValue)
	if err != nil {
		return QueryResult{}, err
	}
	return normalizeDefaultOnEmptyResult(p, result)
}

func executeInJoin(
	ctx context.Context,
	p *plans.RecordQueryInJoinPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inValues := p.GetInValues()
	if len(inValues) == 0 {
		return ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props)
	}

	// Java RecordQueryInJoinPlan.executePlan (:112-131):
	//   RecordCursor.flatMapPipelined(
	//       outerCont -> RecordCursor.fromList(getValues(context), outerCont),
	//       (outerValue, innerCont) -> inner.executePlan(context.withBinding(...),
	//                                       innerCont, props.clearSkipAndLimit()),
	//       checkFunc, continuation, pipelineSize)
	//     .skipThenLimit(skip, limit)
	// The continuation is the flatMap machinery's (outer list index, inner
	// continuation, check value) triple, so a resume lands on the SAME in-value
	// (validated by the check bytes) and continues its inner mid-stream. The
	// pre-A5 eager concat discarded the incoming continuation entirely — every
	// resumed page replayed the whole IN-join from value 0.
	bindingID := p.GetBindingAlias()
	outerFactory := func(cont []byte) recordlayer.RecordCursor[any] {
		return recordlayer.FromListWithContinuation(inValues, cont)
	}
	innerFactory := func(val any, innerCont []byte) recordlayer.RecordCursor[QueryResult] {
		boundCtx := evalCtx.WithBinding(bindingID, val)
		cursor, err := ExecutePlan(ctx, p.GetInner(), store, boundCtx, innerCont, props.ClearSkipAndLimit())
		if err != nil {
			return &errResultCursor{err: err}
		}
		return cursor
	}
	cursor := recordlayer.FlatMapPipelinedWithCheck(outerFactory, innerFactory, inValueCheckBytes, continuation, 1)
	return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
}

// inValueCheckBytes is the IN-join flatMap check value — a deterministic byte
// form of the current in-value validating on resume that the outer list
// element at the saved index is still the same value. Java packs
// Tuple.from(ScanComparisons.toTupleItem(outerValue)) (RecordQueryInJoinPlan
// :123-129, DynamicMessage → toByteArray). Go in-values are plan-time-evaluated
// scalars; the tuple packer covers all of them after the UUID/int32 wire
// canonicalizations, gated by an explicit whitelist (the packer panics on
// anything else — RFC-134 forbids a recover boundary here). A value outside
// the whitelist yields NO check bytes (nil) — the resume then skips the
// outer-identity check, exactly the checker==null degradation Java's Cascades
// path runs with everywhere.
func inValueCheckBytes(val any) []byte {
	v := uuidToTupleElement(widenInt32(val))
	switch v.(type) {
	case nil, int, int64, uint, uint64, float32, float64, bool, string, []byte, tuple.UUID:
		return tuple.Tuple{v}.Pack()
	}
	return nil
}

func executeInUnion(
	ctx context.Context,
	p *plans.RecordQueryInUnionPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inSources := p.GetInSources()
	bindingAliases := p.GetBindingAliases()
	if len(inSources) == 0 {
		return ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props)
	}
	if len(bindingAliases) == 0 || len(inSources) != len(bindingAliases) {
		return nil, fmt.Errorf(
			"executeInUnion: binding/source dimension mismatch (%d bindings, %d sources)",
			len(bindingAliases),
			len(inSources),
		)
	}
	for _, source := range inSources {
		if source != nil && len(source) == 0 {
			// The Cartesian product of the binding dimensions is empty, so the
			// inner must not execute. A nil source is different: its values were
			// unavailable at planning time and remain an unsupported runtime
			// binding, not a known-empty list.
			return recordlayer.Empty[QueryResult](), nil
		}
	}
	if fanout, known := p.LiteralFanout(); known && fanout == 1 {
		// Java binds the complete Cartesian context before its size==1
		// fast path. This matters for multiple singleton dimensions: there is
		// one child execution, but every binding must be present.
		childContext := evalCtx
		for i, source := range inSources {
			childContext = childContext.WithBinding(
				bindingAliases[i],
				source[0],
			)
		}
		cursor, err := ExecutePlan(
			ctx,
			p.GetInner(),
			store,
			childContext,
			continuation,
			props.ClearSkipAndLimit(),
		)
		if err != nil {
			return nil, err
		}
		return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
	}

	// Single binding dimension: execute inner once per IN value,
	// merge-sort if comparison keys exist, otherwise concat. Mirrors Java
	// RecordQueryInUnionPlan.executePlan (:150-175): size==1 hands the
	// continuation straight to the sole child; otherwise the children are
	// CURSOR FACTORIES and the continuation is the UnionCursor's per-child
	// UnionContinuation, decoded into each child's start state.
	if len(bindingAliases) == 1 && len(inSources[0]) > 0 {
		bindingID := bindingAliases[0]
		vals := inSources[0]
		// childFactory tags each value's execution context with its own
		// index (withRecursionInvocationBranch): when compKeys is non-empty
		// below, newMergeSortCursorFromFactories constructs and drives ALL
		// len(vals) factories CONCURRENTLY (every leg pulled from in
		// lockstep, not one after another), so if p.GetInner() is itself a
		// recursive-union plan, its statement-scoped depth guard
		// (recordlayer.ExecuteState.recursionLevels) would otherwise key
		// every one of these concurrently-live invocations by the SAME
		// plan pointer — see recursionInvocationKey's doc comment for why
		// that under- and over-counts. The index-tagged ctx gives each
		// value's invocation of the same inner plan a distinct, resume-
		// stable identity.
		childFactory := func(idx int, val any) recordlayer.CursorFactory[QueryResult] {
			return func(cont []byte) recordlayer.RecordCursor[QueryResult] {
				boundCtx := evalCtx.WithBinding(bindingID, val)
				childCtx := withRecursionInvocationBranch(ctx, idx)
				cursor, err := ExecutePlan(childCtx, p.GetInner(), store, boundCtx, cont, props.ClearSkipAndLimit())
				if err != nil {
					return &errResultCursor{err: err}
				}
				return cursor
			}
		}
		if len(vals) == 1 {
			// Java: size == 1 → childPlan.executePlan(store, childContext,
			// continuation, executeProperties) — the raw child continuation IS
			// the in-union continuation.
			return applySkipLimit(childFactory(0, vals[0])(continuation), props.Skip, props.ReturnedRowLimit), nil
		}
		factories := make([]recordlayer.CursorFactory[QueryResult], len(vals))
		for i, val := range vals {
			factories[i] = childFactory(i, val)
		}
		compKeys := p.GetComparisonKeys()
		if len(compKeys) > 0 {
			merged, err := newMergeSortCursorFromFactories(factories, compKeys, p.IsReverse(), true, continuation)
			if err != nil {
				return nil, err
			}
			return applySkipLimit(merged, props.Skip, props.ReturnedRowLimit), nil
		}
		// No comparison keys (order-free IN union): a resumable branch-tagged
		// concat chain (concatFactories, shared with executeUnionStreaming)
		// instead of the pre-A5 eager concat that discarded the continuation.
		return applySkipLimit(concatFactories(factories, continuation), props.Skip, props.ReturnedRowLimit), nil
	}

	return nil, fmt.Errorf("executeInUnion: multi-binding IN union (%d bindings) not yet implemented", len(bindingAliases))
}

// concatFactories folds N cursor factories into a right-nested chain of binary
// recordlayer.ConcatCursors (Java's ConcatCursor with the branch-tagged
// ConcatContinuation proto), so the concat resumes the branch the continuation
// names. The single concat fold — both executeUnionStreaming (UNION ALL) and
// executeInUnion's order-free arm delegate here.
func concatFactories(factories []recordlayer.CursorFactory[QueryResult], continuation []byte) recordlayer.RecordCursor[QueryResult] {
	if len(factories) == 1 {
		return factories[0](continuation)
	}
	tail := factories[len(factories)-1]
	for i := len(factories) - 2; i >= 1; i-- {
		fi, next := factories[i], tail
		tail = func(cont []byte) recordlayer.RecordCursor[QueryResult] {
			return recordlayer.ConcatCursors(fi, next, cont)
		}
	}
	return recordlayer.ConcatCursors(factories[0], tail, continuation)
}

func executeMergeSortUnion(
	ctx context.Context,
	p *plans.RecordQueryMergeSortUnionPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inners := p.GetInners()
	if len(inners) == 0 {
		return newEmptyCursor[QueryResult](), nil
	}

	// Java RecordQueryUnionPlanBase.executePlan: children are CURSOR FACTORIES
	// (childContinuation → executePlan(child, childContinuation, childProps)),
	// and the union continuation decodes into per-child start states. The
	// pre-A5 eager construction discarded the incoming continuation — a
	// resumed merge replayed every leg from row 0.
	factories := make([]recordlayer.CursorFactory[QueryResult], len(inners))
	for i, inner := range inners {
		inner := inner
		factories[i] = func(cont []byte) recordlayer.RecordCursor[QueryResult] {
			c, err := ExecutePlan(ctx, inner, store, evalCtx, cont, props.ClearSkipAndLimit())
			if err != nil {
				return &errResultCursor{err: err}
			}
			return c
		}
	}
	if len(factories) == 1 {
		// A 1-leg merge union cannot occur from the planner (the distinct-union
		// rule requires >= 2 legs, matching Java UnionCursor.create's >= 2
		// check); defensively hand the continuation straight to the sole leg —
		// Java's InUnion size==1 arm.
		return applySkipLimit(factories[0](continuation), props.Skip, props.ReturnedRowLimit), nil
	}
	merged, err := newMergeSortCursorFromFactories(
		factories, p.GetComparisonKeys(), p.IsReverse(), p.RemovesDuplicates(), continuation)
	if err != nil {
		return nil, err
	}
	return applySkipLimit(merged, props.Skip, props.ReturnedRowLimit), nil
}

// mergeSortChildState tracks one child of the merge-sort union — a faithful
// port of Java MergeCursorState / KeyedMergeCursorState
// (provider/foundationdb/cursors/MergeCursorState.java,
// KeyedMergeCursorState.java). cont is the child's cached resume point: it is
// initialized to the child's START state (or its decoded resume position) and
// advances ONLY when
//   - the child yields NO value (handleNextCursorResult, MergeCursorState
//     .java:58-63 — a stop's continuation IS the safe resume point), or
//   - the child's held value is CONSUMED into an emitted merge row (consume,
//     :76-81).
//
// A PEEKED-but-unconsumed row never advances cont — the emitted-row snapshot
// then re-reads that row on resume instead of silently skipping it.
type mergeSortChildState struct {
	cursor  recordlayer.RecordCursor[QueryResult]
	cont    recordlayer.RecordCursorContinuation
	result  recordlayer.RecordCursorResult[QueryResult]
	pulled  bool  // an OnNext result is cached and not yet consumed
	keyVals []any // comparison-key values of the held row (KeyedMergeCursorState.comparisonKey)

	// origIndex is this state's position in mergeSortCursor.states, assigned
	// once by mergeSortCursor's lazy init. It is the heap's tie-break: Java's
	// UnionCursor.chooseStates (:101-131) visits cursorStates in that fixed
	// list order and keeps the FIRST leg seen at a tied key, so
	// mergeSortHeap.Less must reproduce that same total order (key, then
	// origIndex) rather than leaving heap ties to container/heap's internal
	// sift order, which has no relation to leg position.
	origIndex int
}

// pull fetches the child's next result if none is cached (Java
// MergeCursorState.getOnNextFuture + handleNextCursorResult: the onNext future
// is created once and cleared only by consume, so a stopped child's cached
// result is re-served without re-pulling the cursor).
func (s *mergeSortChildState) pull(ctx context.Context, m *mergeSortCursor) error {
	if s.pulled {
		return nil
	}
	result, err := s.cursor.OnNext(ctx)
	if err != nil {
		return err
	}
	s.result = result
	s.pulled = true
	if result.HasNext() {
		kv, kerr := m.evalCompKeys(result.GetValue())
		if kerr != nil {
			return kerr
		}
		s.keyVals = kv
	} else {
		s.keyVals = nil
		// No value: the child advanced to a stop — its continuation is the
		// safe resume point (MergeCursorState.java:61).
		s.cont = result.GetContinuation()
	}
	return nil
}

// consume records that the held row was emitted as part of a merge result,
// advancing the cached continuation past it (MergeCursorState.java:76-81).
func (s *mergeSortChildState) consume() {
	s.cont = s.result.GetContinuation()
	s.pulled = false
	s.keyVals = nil
}

// mergeSortCursor merges N compatibly-ordered children — Java's UnionCursor
// (dedup=true: per-step advance-all-equal on the comparison key,
// UnionCursor.java:100-131) riding the MergeCursor state machine. dedup=false
// is the ordered UNION-ALL merge (Go's RecordQueryMergeSortUnionPlan with
// removesDuplicates=false): only the first minimum child is consumed per step,
// so cross-child ties emit every tied row.
//
// Leg selection uses a binary min-heap (mergeSortHeap), not Java's per-row
// linear rescan of every leg (UnionCursor.chooseStates, :101-131). Java can
// afford O(N) per row because its N is bounded by query shape (two-cursor
// UNION, a handful of OR branches); Go's InUnion plan turns a SQL IN list
// into one leg per value, so N can run into the hundreds or thousands and
// the per-row rescan dominates. The heap keeps selection O(log N) after an
// unavoidable O(N) initial build, while reproducing Java's result and
// tie-break exactly: see mergeSortHeap's doc comment and OnNext.
type mergeSortCursor struct {
	states   []*mergeSortChildState
	compKeys []values.Value
	// compKeyPrograms holds the comparison-key program specialized per leg row
	// shape, so a merge builds one per LEG rather than one per row.
	compKeyPrograms comparisonKeyPrograms
	reverse         bool
	dedup           bool
	closed          bool
	// lastNoNext caches a terminal result (Java MergeCursor.onNext:291-293:
	// once stopped, every later onNext returns the same result).
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]

	heap        mergeSortHeap
	initialized bool // states[*].origIndex assigned + heap built from the first pull
	// pendingAdmit is the QUEUE of legs that must be pulled and (if they
	// still have a value) pushed into the heap before the NEXT row can be
	// chosen — every leg on the first call, or the legs just consumed on
	// every call after. See OnNext's admission comment for why this must
	// happen at the START of the following call rather than eagerly at the
	// end of the round that consumed them. pullAndAdmit consumes this queue
	// leg-by-leg from the front, popping a leg only once its pull succeeds,
	// so a pull error leaves the failing leg (and everything behind it)
	// still queued here for the next call to retry — no leg is ever dropped
	// on the floor by a partial admit.
	pendingAdmit []*mergeSortChildState
	// pendingStop/pendingStopReason latch a limit stop discovered while
	// admitting a batch of freshly-pulled legs (init, or the legs consumed
	// last round) — Java's computeNextResultStates :81-84: any leg stopping
	// for a non-exhaustion reason stops the WHOLE merge before the next row
	// is chosen. What is deferred is the ADMIT itself (the pull of the legs
	// consumed at the end of round R), pushed out to the top of round R+1's
	// OnNext call instead of happening eagerly at the end of round R — see
	// pendingAdmit's comment for why. The pendingStop check that follows the
	// admit fires in that SAME round-R+1 call, immediately after — it is not
	// itself deferred to a later call — so the row round R already chose and
	// returned is unaffected; only round R+1 short-circuits into stopWith
	// instead of choosing another row.
	pendingStop       bool
	pendingStopReason recordlayer.NoNextReason
	// heapErr latches a comparison error surfaced from within
	// mergeSortHeap.Less, which cannot itself return an error (the
	// heap.Interface contract). Every call site reads it via takeHeapErr
	// immediately after the heap mutation that could have triggered it,
	// which also CLEARS it — deliberately, so the error is a one-shot signal
	// tied to that specific mutation rather than a permanent poison flag: an
	// uncleared heapErr would trip every later, otherwise-successful
	// takeHeapErr check on stale state, turning one bad comparison into a
	// cursor that errors forever even on rounds whose own comparisons are
	// fine. A retried comparison of the SAME two keys is still deterministic
	// and will set it again on its own, exactly as the replaced linear scan
	// re-errored deterministically on a repeat call.
	heapErr error
}

// takeHeapErr returns and clears m.heapErr — see its field doc for why
// clearing on read (rather than latching permanently) is deliberate.
func (m *mergeSortCursor) takeHeapErr() error {
	err := m.heapErr
	m.heapErr = nil
	return err
}

// unionChildResume is one child's decoded start state from a
// RecordCursorProto.UnionContinuation — START (zero value), MID (continuation
// bytes), or END (exhausted).
type unionChildResume struct {
	continuation []byte
	exhausted    bool
}

// decodeUnionContinuation splits a UnionContinuation into n per-child resume
// states — the exact inverse of mergeSortContinuation.ToBytes and a faithful
// port of Java UnionCursorContinuation.from(bytes, n): absent fields mean
// START, first_exhausted/second_exhausted (and other_child_state.exhausted)
// mean END, continuation bytes mean MID. A corrupt token or a child-count
// mismatch fails loudly (Java's RecordCoreException / the
// "expected continuation count does not match read" RecordCoreArgumentException)
// — never a silent fresh restart, which would re-emit rows the caller already
// consumed.
func decodeUnionContinuation(data []byte, n int) ([]unionChildResume, error) {
	out := make([]unionChildResume, n)
	if len(data) == 0 {
		return out, nil // all children fresh (START)
	}
	msg := &gen.UnionContinuation{}
	if err := msg.UnmarshalVT(data); err != nil {
		return nil, &recordlayer.ContinuationParseError{Message: "invalid continuation", RawBytes: data, Cause: err}
	}
	// Java UnionCursorContinuation.from(parsed, n) always reads first + second
	// + every other_child_state entry and requires the total to equal n.
	if read := 2 + len(msg.OtherChildState); read != n {
		return nil, fmt.Errorf("invalid continuation (expected continuation count does not match read): expected %d, read %d", n, read)
	}
	if msg.FirstContinuation != nil {
		out[0] = unionChildResume{continuation: msg.FirstContinuation}
	} else if msg.GetFirstExhausted() {
		out[0] = unionChildResume{exhausted: true}
	}
	if msg.SecondContinuation != nil {
		out[1] = unionChildResume{continuation: msg.SecondContinuation}
	} else if msg.GetSecondExhausted() {
		out[1] = unionChildResume{exhausted: true}
	}
	for i, cs := range msg.OtherChildState {
		if cs.Continuation != nil {
			out[i+2] = unionChildResume{continuation: cs.Continuation}
		} else if cs.GetExhausted() {
			out[i+2] = unionChildResume{exhausted: true}
		}
	}
	return out, nil
}

// newMergeSortCursorFromFactories constructs the merge over child CURSOR
// FACTORIES, decoding the incoming continuation into per-child start states —
// Java UnionCursor.createCursorStates + KeyedMergeCursorState.from: an END
// child is NEVER re-opened (an empty cursor with an END continuation stands in
// for it); a MID child resumes from its own bytes; a START child begins fresh.
func newMergeSortCursorFromFactories(
	factories []recordlayer.CursorFactory[QueryResult],
	compKeys []values.Value,
	reverse, dedup bool,
	continuation []byte,
) (*mergeSortCursor, error) {
	resumes, err := decodeUnionContinuation(continuation, len(factories))
	if err != nil {
		return nil, err
	}
	states := make([]*mergeSortChildState, len(factories))
	for i, factory := range factories {
		if resumes[i].exhausted {
			// Java MergeCursorState.from: continuation.isEnd() →
			// (RecordCursor.empty(), END) — the exhausted child stays exhausted.
			states[i] = &mergeSortChildState{cursor: newEmptyCursor[QueryResult](), cont: &recordlayer.EndContinuation{}}
			continue
		}
		var cont recordlayer.RecordCursorContinuation = &recordlayer.StartContinuation{}
		if len(resumes[i].continuation) > 0 {
			cont = recordlayer.NewBytesContinuation(resumes[i].continuation)
		}
		states[i] = &mergeSortChildState{cursor: factory(resumes[i].continuation), cont: cont}
	}
	return &mergeSortCursor{
		states:   states,
		compKeys: compKeys,
		reverse:  reverse,
		dedup:    dedup,
	}, nil
}

func (m *mergeSortCursor) IsClosed() bool { return m.closed }

// OnNext is Java MergeCursor.onNext (:288-305) with UnionCursor's
// computeNextResultStates (:74-98) and chooseStates (:101-131) reworked to
// select the minimum leg via mergeSortHeap instead of Java's per-row linear
// rescan of every leg — see mergeSortCursor's and mergeSortHeap's doc
// comments for why and for the exact-equivalence argument. Behaviorally
// this is still: admit every leg's held row once (whenAll), stop the whole
// merge if ANY leg stopped for a limit (in-band ReturnLimitReached included
// — treating it as exhaustion would silently drop that leg's remaining
// rows), otherwise emit the minimum-key row, consume the chosen legs, and
// snapshot every leg's cached continuation into the emitted row's lazy
// UnionContinuation.
func (m *mergeSortCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if m.lastNoNext != nil {
		return *m.lastNoNext, nil
	}
	if m.closed {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
	}
	if err := ctx.Err(); err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, err
	}

	if !m.initialized {
		m.initialized = true
		m.heap.m = m
		for i, s := range m.states {
			s.origIndex = i
		}
		// The very first round has no choice but to look at every leg
		// (Java's initial whenAll is the same O(N) — there is no smaller
		// set of legs to consider before anything has been chosen yet).
		m.pendingAdmit = m.states
	}
	// pendingAdmit is the legs consumed last round (or, on the first call,
	// every leg). Pulling and admitting them HERE — before this round's
	// choice — rather than eagerly at the end of the round that consumed
	// them is what keeps the emitted row's snapshot correct: consume()
	// already set each chosen leg's continuation to "right after this row"
	// (MergeCursorState.java:76-81), and that must be what gets snapshotted
	// for THIS row. Pulling those legs' NEXT value before returning THIS
	// row would let a leg that turns out exhausted overwrite its
	// just-consumed continuation with an END one, and if every leg ends up
	// admitted-as-END in the same round a row is returned, the snapshot
	// would wrongly claim the whole merge is done while also carrying a
	// value — exactly the invariant NewResultWithValue rejects. Java avoids
	// this by construction: consume() clears the cached onNext future
	// (MergeCursorState.java:76-81) but does not resolve a new one; the
	// next future is only resolved by the FOLLOWING round's whenAll.
	if len(m.pendingAdmit) > 0 {
		if err := m.pullAndAdmit(ctx); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
	}
	if m.pendingStop {
		// A leg admitted at the end of the previous round (or during init)
		// stopped for a non-exhaustion reason. UnionCursor.computeNextResultStates
		// (:81-84) stops the WHOLE merge before choosing another row.
		return m.stopWith(m.pendingStopReason), nil
	}
	if m.heap.Len() == 0 {
		// Every leg exhausted: the snapshot is all-END (IsEnd() true),
		// satisfying the SourceExhausted invariant.
		return m.stopWith(recordlayer.SourceExhausted), nil
	}

	// chooseStates (UnionCursor.java:101-131): collect every leg holding the
	// minimum comparison key (maximum for reverse — the comparator flip).
	// heap[0] is that minimum by construction (mergeSortHeap.Less), so the
	// first pop is free; ties are found by repeatedly comparing the new
	// root against the established minimum and popping while equal. Every
	// comparison happens BEFORE the corresponding pop, and any already-popped
	// tied leg is pushed back on a comparison error, so an error leaves the
	// heap exactly as it was on entry — matching the pre-heap linear scan,
	// which never mutated m.states before returning an error. This holds for
	// BOTH error sources below: the direct peek compare against minKeyVals,
	// and heap.Pop's own internal sift-down (which, with 3+ legs remaining,
	// compares two OTHER legs to pick the smaller child and can latch an
	// error into m.heapErr with no relation to the leg heapPop just
	// returned) — an error return must leave the heap holding every leg it
	// held on entry, so a leg m.heapPop() already extracted from the heap
	// gets pushed back right alongside any earlier ties in chosen before the
	// error propagates; otherwise that leg is gone from both the heap and
	// chosen the moment this call returns, orphaned exactly like the legs
	// pullAndAdmit's doc comment describes.
	minKeyVals := m.heap.items[0].keyVals
	chosen := make([]*mergeSortChildState, 0, 1)
	for m.heap.Len() > 0 {
		top := m.heap.items[0]
		if len(chosen) > 0 {
			cmp, cmpErr := m.compareKeyVals(top.keyVals, minKeyVals)
			if cmpErr != nil {
				for _, s := range chosen {
					m.heapPush(s)
				}
				// cmpErr is the error being surfaced; discard anything the
				// push-back sift latched into m.heapErr so it cannot poison
				// an unrelated later call (see takeHeapErr).
				m.heapErr = nil
				return recordlayer.RecordCursorResult[QueryResult]{}, cmpErr
			}
			if cmp != 0 {
				break
			}
			if !m.dedup {
				// Ordered UNION-ALL merge: consume ONLY the first minimum
				// leg; the other tied legs re-offer their rows on later
				// steps, so every tied row is emitted. (Java's UnionCursor
				// always dedups — the ALL variant is the Go
				// merge-sort-union with removesDuplicates=false.)
				break
			}
		}
		chosen = append(chosen, m.heapPop())
		if err := m.takeHeapErr(); err != nil {
			// Same invariant as the cmpErr branch above, and for the same
			// reason: chosen (including the leg m.heapPop() just returned)
			// holds legs no longer present in m.heap, so they must go back
			// in before this error reaches the caller.
			for _, s := range chosen {
				m.heapPush(s)
			}
			m.heapErr = nil
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
	}

	// MergeCursor.onNext (:299-301): take the first chosen leg's value
	// (origIndex-least among ties — see mergeSortHeap's doc comment),
	// consume EVERY chosen leg (the advance-all-equal dedup — one emitted
	// row per distinct comparison key, no cross-page state), snapshot, and
	// defer pulling each consumed leg's next row to the top of the
	// following OnNext call (see the pendingAdmit comment above).
	result := chosen[0].result.GetValue()
	for _, s := range chosen {
		s.consume()
	}
	m.pendingAdmit = chosen
	return recordlayer.NewResultWithValue(result, m.snapshotContinuation()), nil
}

// pullAndAdmit pulls and admits the legs queued in m.pendingAdmit, one leg at
// a time, popping a leg off the FRONT of the queue only AFTER its pull
// succeeds. This makes a mid-queue pull error non-destructive: the failing
// leg (and every leg still behind it) stays in m.pendingAdmit, so the very
// next OnNext call re-enters here and retries that SAME leg first —
// mergeSortChildState.pull is idempotent (a no-op once s.pulled is set), so
// a leg that actually succeeded before the error is never re-pulled or
// double-pushed into the heap.
//
// This replaced an all-or-nothing version that captured the whole batch into
// a local and cleared m.pendingAdmit up front, before pulling anything: a
// pull error partway through the batch then permanently orphaned every leg
// from the failure point onward — not in the heap (never pushed), not in
// pendingAdmit (already cleared), and never pulled again on any later call.
// A caller that kept driving OnNext after the error either silently lost
// rows (an orphaned leg's remaining values simply never resurfaced) or, if
// every remaining leg ended up orphaned, hit a panic out of stopWith:
// SourceExhausted requires an all-END continuation snapshot, but an orphaned
// leg's continuation was never advanced past its last successful pull, so
// the snapshot was not actually END.
//
// A leg that stopped (this call) is folded into the pending-stop bookkeeping
// instead of being scanned again next round: a leg's stop state can only
// change when it is (re-)pulled, so only the legs actually pulled THIS round
// can introduce a new stop reason — Java's getStrongestNoNextReason
// (MergeCursor.java:172-194) collapses to considering just this batch
// because the merge always stops the very round a non-exhaustion reason
// first appears (see mergeSortCursor.pendingStop's doc comment), so no
// earlier-stopped leg can still be "new" information by a later round.
func (m *mergeSortCursor) pullAndAdmit(ctx context.Context) error {
	for len(m.pendingAdmit) > 0 {
		s := m.pendingAdmit[0]
		if err := s.pull(ctx, m); err != nil {
			return err
		}
		m.pendingAdmit = m.pendingAdmit[1:]
		if s.result.HasNext() {
			m.heapPush(s)
			if err := m.takeHeapErr(); err != nil {
				return err
			}
			continue
		}
		reason := s.result.GetNoNextReason()
		if !reason.IsLimitReached() {
			continue // plain exhaustion: this leg just drops out of the merge
		}
		// getStrongestNoNextReason's combine rule, restricted to this batch
		// (see the doc comment above): the first non-exhaustion reason wins
		// unless a later one in the same batch is out-of-band, in which case
		// out-of-band always wins.
		if !m.pendingStop || reason.IsOutOfBand() {
			m.pendingStopReason = reason
			m.pendingStop = true
		}
	}
	return nil
}

// heapPush pushes s onto the heap. Any comparison error the resulting sift
// hits is latched into m.heapErr (mergeSortHeap.Less cannot return one
// itself) — callers read it via takeHeapErr immediately after.
func (m *mergeSortCursor) heapPush(s *mergeSortChildState) {
	heap.Push(&m.heap, s)
}

// heapPop pops the minimum leg. Any comparison error the resulting sift hits
// is latched into m.heapErr (mergeSortHeap.Less cannot return one itself) —
// callers read it via takeHeapErr immediately after.
func (m *mergeSortCursor) heapPop() *mergeSortChildState {
	return heap.Pop(&m.heap).(*mergeSortChildState)
}

// stopWith caches and returns the merge's terminal no-next result.
func (m *mergeSortCursor) stopWith(reason recordlayer.NoNextReason) recordlayer.RecordCursorResult[QueryResult] {
	res := recordlayer.NewResultNoNext[QueryResult](reason, m.snapshotContinuation())
	m.lastNoNext = &res
	return res
}

// evalCompKeys evaluates the comparison keys against a held row's ordinal row
// (KeyedMergeCursorState.handleNextCursorResult's comparisonKeyFunction.apply).
// Comparison keys are plan-baked field extractions; an evaluation error is a
// planner invariant violation surfaced as a loud cursor error.
func (m *mergeSortCursor) evalCompKeys(qr QueryResult) ([]any, error) {
	kv := make([]any, len(m.compKeys))
	evalKeys, inputEdges, err := m.compKeyPrograms.forRow(m.compKeys, qr.Positional)
	if err != nil {
		return nil, fmt.Errorf("merge comparison key program: %w", err)
	}
	arg, err := compKeyEvalArg(qr, inputEdges...)
	if err != nil {
		return nil, err
	}
	for i, key := range evalKeys {
		v, err := key.Evaluate(arg)
		if err != nil {
			return nil, fmt.Errorf("merge comparison key %d: %w", i, err)
		}
		kv[i] = v
	}
	return kv, nil
}

// compareKeyVals compares two evaluated comparison-key vectors in merge order
// (Java KeyComparisons.KEY_COMPARATOR with the reverse flip,
// UnionCursor.java:114). compareValues is the same per-element ordering
// authority the sort/merge paths share (Java-faithful float total order, FDB
// tuple order for bytes/UUID/bool).
func (m *mergeSortCursor) compareKeyVals(a, b []any) (int, error) {
	for i := range m.compKeys {
		cmp, err := compareValues(a[i], b[i])
		if err != nil {
			return 0, err
		}
		if cmp == 0 {
			continue
		}
		if m.reverse {
			return -cmp, nil
		}
		return cmp, nil
	}
	return 0, nil
}

// mergeSortHeap is a container/heap binary min-heap over the legs currently
// holding a comparison key, giving mergeSortCursor.OnNext O(log N) leg
// selection instead of Java UnionCursor.chooseStates's O(N) rescan of every
// leg on every emitted row (UnionCursor.java:101-131).
//
// Ordering is (comparison key via mergeSortCursor.compareKeyVals, which
// folds in the reverse flip; then origIndex) — a strict total order, so the
// classic heap invariant pins the root to a UNIQUE element: the leg with the
// smallest key and, among ties, the smallest original leg index. That is
// exactly chosen[0] from the replaced linear scan: chooseStates makes a
// single forward pass over cursorStates in list order and only replaces the
// running "chosen" set on a STRICTLY smaller key (`compare < 0`), so among
// legs tied at the minimum key the first one encountered — i.e., smallest
// index — is always chosen[0], and Java's getNextResult returns exactly
// chosenStates.get(0) (UnionCursorBase.java:70-72). Folding origIndex into
// Less reproduces that tie-break exactly rather than leaving it to
// container/heap's internal sift order, which has no relation to leg
// position and would silently pick a different (still-correct-as-a-*set*,
// but wrong-as-a-*value*) leg on ties.
//
// Less cannot return an error, but compareKeyVals can (a cross-type
// comparison-key pair across legs — a planner invariant violation): on
// error, Less latches it into m.heapErr and falls back to the origIndex
// order alone, which keeps the heap internally consistent (no panic, no
// infinite loop) until the caller reads it via takeHeapErr, which every
// production call site does immediately after the heap operation that
// could have triggered it.
//
// Note this also means the heap can SURFACE a cross-type comparison-key
// violation on a leg pair the replaced linear scan never happened to
// compare directly (chooseStates only ever compares each leg against the
// running minimum, not every pair). That is not a new bug — the keys were
// already invariant-violating — just an earlier, heap-shape-dependent
// discovery point for one that already existed.
type mergeSortHeap struct {
	items []*mergeSortChildState
	m     *mergeSortCursor
}

func (h *mergeSortHeap) Len() int { return len(h.items) }

func (h *mergeSortHeap) Less(i, j int) bool {
	a, b := h.items[i], h.items[j]
	cmp, err := h.m.compareKeyVals(a.keyVals, b.keyVals)
	if err != nil {
		if h.m.heapErr == nil {
			h.m.heapErr = err
		}
		return a.origIndex < b.origIndex
	}
	if cmp != 0 {
		return cmp < 0
	}
	return a.origIndex < b.origIndex
}

func (h *mergeSortHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }

func (h *mergeSortHeap) Push(x any) { h.items = append(h.items, x.(*mergeSortChildState)) }

func (h *mergeSortHeap) Pop() any {
	old := h.items
	n := len(old)
	item := old[n-1]
	old[n-1] = nil // drop the reference so a shrunk-but-not-reallocated backing array doesn't pin it
	h.items = old[:n-1]
	return item
}

// snapshotContinuation captures every child's CURRENT cached continuation
// (MergeCursor.getChildContinuations, called via getContinuationObject AFTER
// the chosen states were consumed): consumed children sit past their emitted
// rows, peeked-but-unconsumed children sit BEFORE their held rows, stopped
// children sit at their own stop positions.
func (m *mergeSortCursor) snapshotContinuation() *mergeSortContinuation {
	children := make([]recordlayer.RecordCursorContinuation, len(m.states))
	for i, s := range m.states {
		c := s.cont
		if c == nil {
			c = &recordlayer.StartContinuation{}
		}
		children[i] = c
	}
	return &mergeSortContinuation{children: children}
}

// mergeSortContinuation lazily encodes the merge's per-child state snapshot as
// Java's RecordCursorProto.UnionContinuation (UnionCursorContinuation +
// MergeCursorContinuation): child 0 → first_continuation/first_exhausted,
// child 1 → second_continuation/second_exhausted, children 2+ →
// other_child_state (exhausted=true for END, exhausted=false for START, bytes
// for MID; for the first two children START is simply the absent field). A
// child encode failure propagates through the RecordCursorContinuation
// contract (the flatMapCursorContinuation lazy-encode pattern). IsEnd mirrors
// UnionCursorContinuation.isEnd: end only when EVERY child is at end, and
// MergeCursorContinuation.toBytes returns nil at end.
type mergeSortContinuation struct {
	children []recordlayer.RecordCursorContinuation
}

func (u *mergeSortContinuation) IsEnd() bool {
	for _, c := range u.children {
		if !c.IsEnd() {
			return false
		}
	}
	return true
}

func (u *mergeSortContinuation) ToBytes() ([]byte, error) {
	if u.IsEnd() {
		return nil, nil
	}
	msg := &gen.UnionContinuation{}
	for i, c := range u.children {
		end := c.IsEnd()
		var b []byte
		if !end {
			var err error
			b, err = c.ToBytes()
			if err != nil {
				return nil, fmt.Errorf("union continuation child %d: %w", i, err)
			}
		}
		switch i {
		case 0:
			if end {
				msg.FirstExhausted = proto.Bool(true)
			} else if len(b) > 0 {
				msg.FirstContinuation = b
			}
		case 1:
			if end {
				msg.SecondExhausted = proto.Bool(true)
			} else if len(b) > 0 {
				msg.SecondContinuation = b
			}
		default:
			cs := &gen.UnionContinuation_CursorState{}
			switch {
			case end:
				cs.Exhausted = proto.Bool(true)
			case len(b) == 0:
				// Java's START_PROTO writes exhausted=false explicitly.
				cs.Exhausted = proto.Bool(false)
			default:
				cs.Continuation = b
			}
			msg.OtherChildState = append(msg.OtherChildState, cs)
		}
	}
	return msg.MarshalVT()
}

func (m *mergeSortCursor) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	var firstErr error
	for _, s := range m.states {
		if err := s.cursor.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// compareValues delegates to values.CompareOrdered, the single Java-faithful
// natural order this codebase uses for sort/merge/dedup ranking of runtime
// scalar values (see that function's doc for the exact semantics: NaN
// greatest, -0.0 < 0.0, FDB tuple byte order for []byte/UUID, nil least, a
// loud error on a cross-type pair). Kept as a thin package-local alias so the
// merge-sort cursor call sites and the differential fuzz test below don't
// need the values. qualifier at every use.
func compareValues(a, b any) (int, error) {
	return values.CompareOrdered(a, b)
}

func newEmptyCursor[T any]() recordlayer.RecordCursor[T] {
	return &emptyCursor[T]{}
}

type emptyCursor[T any] struct{ closed bool }

func (c *emptyCursor[T]) OnNext(context.Context) (recordlayer.RecordCursorResult[T], error) {
	return recordlayer.NewResultNoNext[T](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
}
func (c *emptyCursor[T]) IsClosed() bool { return c.closed }
func (c *emptyCursor[T]) Close() error   { c.closed = true; return nil }

// errResultCursor yields a stored error on the first OnNext. Used to defer a
// cursor-construction error through a factory that cannot itself return an error
// (e.g. OrElse's primary/alternative factories, which have signature
// func([]byte) RecordCursor[QueryResult]). Mirrors recordlayer.errorCursor.
type errResultCursor struct {
	err error
}

func (c *errResultCursor) OnNext(context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	return recordlayer.RecordCursorResult[QueryResult]{}, c.err
}

func (c *errResultCursor) Close() error   { return nil }
func (c *errResultCursor) IsClosed() bool { return false }

// A first-or-default's continuation namespace is TAGGED. The operator is eager —
// it drains its inner leg to decide whether to flow the first row or the default
// — so the byte string it hands its parent has to say WHICH of three states it
// stopped in, and one of those states carries the inner leg's own continuation
// bytes as a payload.
//
// An untagged namespace cannot express that safely. The "one row was consumed"
// marker is the single byte 0x01, and the operator used to pass anything else
// straight through to its inner as that inner's continuation — so an inner
// continuation that happened to BE 0x01 would have resumed as already-consumed.
// That is a silent wrong answer, not a crash: the operator flows nothing where
// the inner still had rows, and an EXISTS built on it inverts. The collision was
// unreachable only because nothing ever fed this operator a real inner
// continuation; checkpointing (checkpointAfterOutOfBand) is exactly what starts
// doing so, which is why the namespace gets tagged in the same change.
//
// The tag byte makes the three states disjoint by construction. 0x01 keeps its
// meaning so continuations issued before the tagging still decode correctly.
const (
	fodTagConsumed   byte = 0x01 // the single row was emitted and consumed
	fodTagCheckpoint byte = 0x02 // zero rows seen; payload is the inner leg's position
	fodTagRestart    byte = 0x03 // re-run the whole leg from the beginning
)

// fodResume is what a decoded first-or-default continuation says to do.
type fodResume int

const (
	fodResumeStart    fodResume = iota // run the inner leg from the beginning
	fodResumeConsumed                  // the row is gone; the cursor is exhausted
	fodResumeInner                     // run the inner leg from the decoded position
)

// singleResultConsumedToken is the continuation a singleResultCursor's
// emitted row carries: "the one row was consumed". A resume from it is
// EXHAUSTED (executeFirstOrDefault returns Empty). The retired
// value+nil-continuation shape was impossible in Java (every value
// result carries a resumable continuation) and made every parent
// snapshot the child as START — safe only while the shape stayed ≤1 row
// under a per-outer-row FlatMap; any other parent would have silently
// replayed the row.
var singleResultConsumedToken = []byte{fodTagConsumed}

// encodeFirstOrDefaultCheckpoint wraps an inner leg's continuation bytes so they
// cannot be mistaken for any other state in this operator's namespace.
func encodeFirstOrDefaultCheckpoint(inner []byte) []byte {
	out := make([]byte, 0, len(inner)+1)
	out = append(out, fodTagCheckpoint)
	return append(out, inner...)
}

// decodeFirstOrDefaultContinuation reads this operator's namespace. An
// unrecognised tag is a corrupt continuation and must surface as one — silently
// treating it as a fresh start would re-emit rows the caller already consumed
// (the reason executeFlatMap raises ContinuationParseError on a bad envelope).
func decodeFirstOrDefaultContinuation(b []byte) (fodResume, []byte, error) {
	if len(b) == 0 {
		return fodResumeStart, nil, nil
	}
	switch b[0] {
	case fodTagConsumed:
		if len(b) != 1 {
			return fodResumeStart, nil, &recordlayer.ContinuationParseError{
				Message:  "first-or-default consumed token carries a payload",
				RawBytes: b,
			}
		}
		return fodResumeConsumed, nil, nil
	case fodTagRestart:
		if len(b) != 1 {
			return fodResumeStart, nil, &recordlayer.ContinuationParseError{
				Message:  "first-or-default restart token carries a payload",
				RawBytes: b,
			}
		}
		return fodResumeStart, nil, nil
	case fodTagCheckpoint:
		// An empty payload is a checkpoint at the very beginning — the same
		// thing as a restart, and handled as one rather than rejected.
		if len(b) == 1 {
			return fodResumeStart, nil, nil
		}
		return fodResumeInner, b[1:], nil
	default:
		return fodResumeStart, nil, &recordlayer.ContinuationParseError{
			Message:  "unknown first-or-default continuation tag",
			RawBytes: b,
		}
	}
}

// singleResultCursor yields one result then ends.
type singleResultCursor struct {
	value  QueryResult
	done   bool
	closed bool
}

func newSingleResultCursor(v QueryResult) *singleResultCursor {
	return &singleResultCursor{value: v}
}

func (c *singleResultCursor) OnNext(_ context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.done || c.closed {
		return recordlayer.NewResultNoNext[QueryResult](
			recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
		), nil
	}
	c.done = true
	return recordlayer.NewResultWithValue(
		c.value, recordlayer.NewBytesContinuation(singleResultConsumedToken),
	), nil
}

func (c *singleResultCursor) Close() error   { c.closed = true; return nil }
func (c *singleResultCursor) IsClosed() bool { return c.closed }
