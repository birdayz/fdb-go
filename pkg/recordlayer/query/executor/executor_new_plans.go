package executor

import (
	"bytes"
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
	maintainer, err := store.GetIndexMaintainer(idx)
	if err != nil {
		return nil, fmt.Errorf("executor: getting index maintainer for %q: %w", idxPlan.GetIndexName(), err)
	}

	scanRange, err := scanComparisonsToTupleRange(idxPlan.GetScanComparisons(), scanBindContext(evalCtx))
	if err != nil {
		return nil, fmt.Errorf("executor: building scan range for %q: %w", idxPlan.GetIndexName(), err)
	}

	scanProps := recordlayer.ScanProperties{
		ExecuteProperties:   props,
		Reverse:             idxPlan.IsReverse(),
		CursorStreamingMode: recordlayer.StreamingModeIterator,
	}

	canonicalName := p.CanonicalAggColumnName()
	groupCols := p.GetGroupCols()
	// The aggregate-index row's authoritative ordinal
	// schema is the GROUP columns in scan order followed by the aggregate column
	// — the exact order the row's slots are filled below (entry.Key then
	// entry.Value). Row-invariant, so it is built once here and shared across
	// rows. Named by the canonical output names (uppercase group
	// cols, canonical agg name) — the exact schema plan-time bakes bind
	// against.
	posNames := make([]string, 0, len(groupCols)+1)
	posNames = append(posNames, groupCols...)
	posNames = append(posNames, canonicalName)
	posType := positionalTypeFromNames(posNames)

	// PERMUTED_MIN/MAX indexes keep the current extremum per group in the
	// SECONDARY (permuted) subspace, not the primary VALUE tree; the aggregate
	// value lives inside the entry KEY, not entry.Value. Scan BY_GROUP — the same
	// scan Java's AggregateIndexMatchCandidate builds (IndexScanType.BY_GROUP) —
	// so a plain SQL MAX/MIN served from a permuted index reflects the true
	// current extremum under deletes/updates (a monotone _EVER index goes stale).
	if idx.Type == recordlayer.IndexTypePermutedMin || idx.Type == recordlayer.IndexTypePermutedMax {
		return newPermutedAggregateIndexCursor(store, idx, scanRange, continuation, scanProps, len(groupCols), posType)
	}

	indexCursor := maintainer.Scan(scanRange, continuation, scanProps)

	return &aggregateIndexCursor{
		inner:     indexCursor,
		groupCols: groupCols,
		// Single source for the aggregate column key: the plan's
		// CanonicalAggColumnName is also what planColumnNamesWithMD reports via
		// OutputColumnNames, so the cursor's row key and the reported name can't
		// drift (RFC-081).
		canonicalName: canonicalName,
		posType:       posType,
	}, nil
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
	posType *values.RecordType,
) (recordlayer.RecordCursor[QueryResult], error) {
	gke, ok := idx.RootExpression.(*recordlayer.GroupingKeyExpression)
	if !ok {
		return nil, fmt.Errorf("executor: permuted index %q root is %T, want *GroupingKeyExpression",
			idx.Name, idx.RootExpression)
	}
	totalSize := gke.ColumnSize()
	permutedSize := 0
	if v, ok := idx.Options[recordlayer.IndexOptionPermutedSize]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			permutedSize = n
		}
	}
	inner := store.ScanIndexByType(idx, recordlayer.IndexScanByGroup, scanRange, continuation, scanProps)
	return &permutedAggregateIndexCursor{
		inner:      inner,
		groupCount: groupCount,
		valueStart: gke.GetGroupingCount() - permutedSize,
		valueEnd:   totalSize - permutedSize,
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
	closed        bool
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

	qr := QueryResult{Positional: &PositionalRow{Type: c.posType, Slots: slots}}
	return recordlayer.NewResultWithValue(qr, result.GetContinuation()), nil
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

	// IntersectionMulti returns, per matching comparison key, the list of
	// matching rows (one per child). Mirrors Java's IntersectionMultiCursor;
	// the regular intersection keeps only the first child, which would drop
	// every aggregate but the first.
	innerCursor := recordlayer.IntersectionMultiResume(cursors, compKeyFunc, false, resume)

	merged := &multiIntersectionMergeCursor{
		inner:       innerCursor,
		resultValue: p.GetResultValue(),
	}
	return applySkipLimit(merged, props.Skip, props.ReturnedRowLimit), nil
}

func multiIntersectionCompKeyFunc(keyVals []values.Value) recordlayer.ComparisonKeyFunc[QueryResult] {
	return func(qr QueryResult) tuple.Tuple {
		if len(keyVals) > 0 {
			t := make(tuple.Tuple, len(keyVals))
			for i, kv := range keyVals {
				// Comparison/merge keys are field extractions; the runtime
				// typed-error family is unreachable and ComparisonKeyFunc
				// has no error channel, so a stray error is a planner
				// invariant violation (panic, matching prior no-recover).
				// Resolves against the ordinal row via compKeyEvalArg.
				v, err := kv.Evaluate(compKeyEvalArg(qr))
				if err != nil {
					panic(err)
				}
				// widenInt32 (RFC-092) + uuidToTupleElement: a UUID group/PK
				// comparison key arrives as a neutral [16]byte the tuple packer
				// can't encode; convert it to tuple.UUID so compareKeys' Pack
				// doesn't panic on a multi-aggregate intersection over a UUID
				// GROUP BY key (RFC-162). Mirrors packedDedupKey's UUID arm.
				t[i] = uuidToTupleElement(widenInt32(v))
			}
			return t
		}
		if qr.PrimaryKey != nil {
			return qr.PrimaryKey
		}
		// No comparison keys and no PK: match rows by their FULL positional
		// content, encoded LOSSLESSLY via the continuation codec — the
		// retired fmt %v rendering collapsed distinct composite rows and
		// collapsed every nil-layout row into one. Encode failure is a
		// planner invariant violation (this closure's documented no-error-
		// channel contract, as with Evaluate above).
		if qr.Positional == nil {
			// A row with no keys, no PK and no positional content cannot be
			// matched against anything; a constant key would intersect ALL
			// such rows. Planner invariant violation (documented contract).
			panic(fmt.Errorf("intersection row carries no comparison keys, primary key, or positional content — malformed plan"))
		}
		b, err := appendContValue(nil, qr.Positional.Slots)
		if err != nil {
			panic(err)
		}
		return tuple.Tuple{b}
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
	closed      bool
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
	evalArg := mergeChildEvalArg(childResults)

	qr := QueryResult{}
	// Emit the authoritative ordinal OUTPUT row. The resultValue is a
	// RecordConstructorValue whose Fields ARE the output columns in output order
	// (the same rc.Fields deriveColumnsFromMultiIntersection names the ColumnDefs
	// from), so evaluating each field against the concatenated child positional row
	// produces a per-slot output row whose names/order match the result-set columns.
	if rc, ok := c.resultValue.(*values.RecordConstructorValue); ok {
		posNames := make([]string, len(rc.Fields))
		posSlots := make([]any, len(rc.Fields))
		for i, f := range rc.Fields {
			posNames[i] = f.Name
			fv, ferr := f.Value.Evaluate(evalArg)
			if ferr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, ferr
			}
			posSlots[i] = fv
		}
		qr.Positional = &PositionalRow{Type: positionalTypeFromNames(posNames), Slots: posSlots}
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

// mergeChildEvalArg builds the eval argument the MultiIntersection resultValue is
// evaluated against: the CONCATENATED PositionalRow of the matched child rows
// (a bare OrdinalRow whose plan-produced concatenated type the baked
// references read). Fields are concatenated in child order; grouping
// columns repeat across children with the same value, so a first-match bind is
// order-safe. Returns nil if any child lacks a Positional (should not happen — the
// aggregate-index producer builds one per child).
func mergeChildEvalArg(childResults []QueryResult) any {
	var fields []values.Field
	var slots []any
	ord := 0
	for _, cr := range childResults {
		p := cr.Positional
		if p == nil || p.Type == nil {
			return nil
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
		return nil
	}
	return &PositionalRow{Type: &values.RecordType{Fields: fields}, Slots: slots}
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
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	keys := p.GetKeysSource().GetPrimaryKeys()
	if len(keys) == 0 {
		return recordlayer.Empty[QueryResult](), nil
	}

	// RFC-130: bounded by a plan-literal key count, but each element is a whole
	// stored record, so the resident bytes can grow. Charge each loaded record
	// against the statement memory budget via boundedBuffer.
	results := newBoundedBuffer[QueryResult](props.State, 0, "LoadByKeys", estimateQueryResultBytes)
	for _, pk := range keys {
		rec, err := store.LoadRecord(pk)
		if err != nil {
			props.State.ReleaseMemory(results.Charged())
			return nil, fmt.Errorf("executor: LoadByKeys pk=%v: %w", pk, err)
		}
		if rec == nil {
			continue
		}
		qr := FromStoredRecord(rec)
		if err := results.Append(qr); err != nil {
			props.State.ReleaseMemory(results.Charged())
			return nil, err
		}
	}
	// Live-bytes model: rebuilt (and re-charged) per page; released at page
	// teardown via the wrapping cursor's Close.
	return newChargeReleasingCursor(
		applySkipLimit(
			recordlayer.FromList(results.Items()),
			props.Skip, props.ReturnedRowLimit,
		),
		props.State, results.Charged(),
	), nil
}

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
		return ExecutePlan(ctx, inners[0], store, evalCtx, continuation, props.ClearSkipAndLimit())
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
	// later branch's keys to the first branch's, position-wise, exactly as the ordered
	// union does. A no-op when names already agree (the common case).
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
	copy(children, u.states)
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
	// On the positional frontier a QOV(innerAlias).col resolves
	// via the bare-positional fallback in evaluateCorrelated (Correlations miss →
	// Positional), so bindAlias is NOT a reason to wrap — only a genuine
	// param/subquery/outer-binding is. When none is present, flow the bare row.
	posNeedsCtx := hasBindingContext(evalCtx)
	// When the input flows a 2-way ordinal join's merged
	// positional row, predicates evaluate under the LEG WINDOWS — computed
	// once, from the input plan's result value.
	legSpans, windowsOK := downstreamLegWindows(p.GetInner())
	filtered := &filterResultCursor{
		inner: inner,
		pred: func(qr QueryResult) (bool, error) {
			var rowCtx any
			switch {
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
				ec := evalCtx
				if ec == nil {
					ec = EmptyEvaluationContext()
				}
				ec = ec.WithBinding(innerAlias, qr.Positional.Slots[0])
				rowCtx = ec.RowContext()
			case qr.Positional != nil:
				// The non-join frontier flows an authoritative
				// ordinal row — resolve predicates by ordinal (loud on a miss, no
				// name-map fallback). A QOV(innerAlias).col resolves via the
				// bare-positional fallback in evaluateCorrelated, so no alias
				// binding is needed here.
				rowCtx = frontierRowContext(qr.Positional, evalCtx, posNeedsCtx)
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
	resultValue := p.GetResultValue()
	// On the positional frontier an outer correlation resolves via
	// the eval context's binder before the bare-positional frontier fallback.
	posNeedsCtx := hasBindingContext(evalCtx)
	// The Map output schema is row-invariant — derive the emitted
	// positional row's OUTPUT names once from the result value's record type. When
	// the result is a RecordConstructorValue, evaluate its Fields INDIVIDUALLY into
	// dense slots (never through the collapsing name map — a duplicate output name
	// keeps both slots by ordinal), mirroring executeProjection.
	var mapPosType *values.RecordType
	mapRC, _ := resultValue.(*values.RecordConstructorValue)
	if rt, ok := resultValue.Type().(*values.RecordType); ok {
		mapPosNames := make([]string, len(rt.Fields))
		for i, fld := range rt.Fields {
			mapPosNames[i] = fld.Name
		}
		mapPosType = positionalTypeFromNames(mapPosNames)
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
		if qr.Positional != nil && windowsOK {
			// The merged positional row of a gated 2-way
			// ordinal join — a leg reference QOV(leg).col needs its
			// leg window (unconditional; see executePredicatesFilter).
			rowCtx = legWindowRowContext(qr.Positional, evalCtx, legSpans)
		} else if qr.Positional != nil {
			// The non-join frontier flows an authoritative ordinal
			// row — resolve the result value by ordinal (loud on a miss).
			rowCtx = frontierRowContext(qr.Positional, evalCtx, posNeedsCtx)
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
			pos = scalarPositionalRow(m)
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
	inner, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props)
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
			// An out-of-band (resource-limit) stop after the first row means the input
			// was TRUNCATED — a second matching row may exist beyond the cap, so we can
			// no longer prove at-most-one. Error (→ 54F01) rather than silently accept
			// the first row (RFC-106a; same guard as the uncorrelated path).
			if lerr := errIfBufferTruncated(second); lerr != nil {
				_ = inner.Close()
				return nil, lerr
			}
		}
		_ = inner.Close()
		return newSingleResultCursor(first), nil
	}
	_ = inner.Close()
	// An out-of-band (resource-limit) stop before the first row means the input was
	// TRUNCATED — we can't tell whether a matching row would have followed, so error
	// (→ 54F01) instead of fabricating the default and returning a wrong EXISTS/scalar
	// answer from a partial scan (RFC-106a).
	if lerr := errIfBufferTruncated(result); lerr != nil {
		return nil, lerr
	}
	defaultVal := p.GetDefaultValue()
	if defaultVal == nil {
		return newSingleResultCursor(QueryResult{Positional: scalarPositionalRow(nil)}), nil
	}
	qr, err := resultFromValue(defaultVal)
	if err != nil {
		return nil, err
	}
	return newSingleResultCursor(qr), nil
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
	// resumes and the default is NEVER fabricated on truncation, so the eager
	// RFC-106a truncation error is unnecessary here (DefaultOnEmpty is a streaming
	// cursor; the eager FirstOrDefault/scalar-subquery paths keep their guard).
	nestedProps := props.ClearSkipAndLimit()
	primaryFactory := func(cont []byte) recordlayer.RecordCursor[QueryResult] {
		inner, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, cont, nestedProps)
		if err != nil {
			return &errResultCursor{err: err}
		}
		return inner
	}
	alternativeFactory := func(cont []byte) recordlayer.RecordCursor[QueryResult] {
		// Evaluate the default lazily (only when the inner is empty), matching
		// Java's else-lambda: onEmptyResultValue.eval runs inside fromList's
		// supplier. A nil default flows a scalar NULL row.
		var defaultRow QueryResult
		if defaultVal := p.GetDefaultValue(); defaultVal != nil {
			qr, err := resultFromValue(defaultVal)
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
	bindingID := values.NamedCorrelationIdentifier(p.GetBindingName())
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
	bindingNames := p.GetBindingNames()
	if len(inSources) == 0 || len(bindingNames) == 0 {
		return ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props)
	}

	// Single binding dimension: execute inner once per IN value,
	// merge-sort if comparison keys exist, otherwise concat. Mirrors Java
	// RecordQueryInUnionPlan.executePlan (:150-175): size==1 hands the
	// continuation straight to the sole child; otherwise the children are
	// CURSOR FACTORIES and the continuation is the UnionCursor's per-child
	// UnionContinuation, decoded into each child's start state.
	if len(bindingNames) == 1 && len(inSources[0]) > 0 {
		bindingID := values.NamedCorrelationIdentifier(bindingNames[0])
		vals := inSources[0]
		childFactory := func(val any) recordlayer.CursorFactory[QueryResult] {
			return func(cont []byte) recordlayer.RecordCursor[QueryResult] {
				boundCtx := evalCtx.WithBinding(bindingID, val)
				cursor, err := ExecutePlan(ctx, p.GetInner(), store, boundCtx, cont, props.ClearSkipAndLimit())
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
			return applySkipLimit(childFactory(vals[0])(continuation), props.Skip, props.ReturnedRowLimit), nil
		}
		factories := make([]recordlayer.CursorFactory[QueryResult], len(vals))
		for i, val := range vals {
			factories[i] = childFactory(val)
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

	return nil, fmt.Errorf("executeInUnion: multi-binding IN union (%d bindings) not yet implemented", len(bindingNames))
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
type mergeSortCursor struct {
	states   []*mergeSortChildState
	compKeys []values.Value
	reverse  bool
	dedup    bool
	closed   bool
	// lastNoNext caches a terminal result (Java MergeCursor.onNext:291-293:
	// once stopped, every later onNext returns the same result).
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]
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
	return &mergeSortCursor{states: states, compKeys: compKeys, reverse: reverse, dedup: dedup}, nil
}

func (m *mergeSortCursor) IsClosed() bool { return m.closed }

// OnNext is Java MergeCursor.onNext (:288-305) with UnionCursor's
// computeNextResultStates (:74-98) and chooseStates (:101-131) inlined:
// pull ALL children (whenAll), stop the whole merge if ANY child stopped for a
// limit (in-band ReturnLimitReached included — treating it as exhaustion would
// silently drop that child's remaining rows), otherwise emit the minimum-key
// row, consume the chosen children, and snapshot every child's cached
// continuation into the emitted row's lazy UnionContinuation.
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

	for _, s := range m.states {
		if err := s.pull(ctx, m); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
	}

	// UnionCursor.computeNextResultStates (:77-96): any child stopped for a
	// LIMIT (isLimitReached == any non-exhaustion reason) stops the merge with
	// the strongest child reason and the snapshot continuation. The merge
	// cannot keep going: it needs every child's next value to know the minimum.
	anyHasNext := false
	for _, s := range m.states {
		if s.result.HasNext() {
			anyHasNext = true
		} else if s.result.GetNoNextReason().IsLimitReached() {
			return m.stopWith(m.strongestNoNextReason()), nil
		}
	}
	if !anyHasNext {
		// All children exhausted: the snapshot is all-END (IsEnd() true),
		// satisfying the SourceExhausted invariant.
		return m.stopWith(recordlayer.SourceExhausted), nil
	}

	// chooseStates (UnionCursor.java:101-131): collect every child holding the
	// minimum comparison key (maximum for reverse — the comparator flip).
	var chosen []*mergeSortChildState
	var minKey []any
	for _, s := range m.states {
		if !s.result.HasNext() {
			continue
		}
		cmp := -1 // first key seen is always chosen
		if chosen != nil {
			cmp = m.compareKeyVals(s.keyVals, minKey)
		}
		if cmp < 0 {
			chosen = chosen[:0]
			minKey = s.keyVals
		}
		if cmp <= 0 {
			chosen = append(chosen, s)
		}
	}
	if !m.dedup {
		// Ordered UNION-ALL merge: consume ONLY the first minimum child; the
		// other tied children re-offer their rows on later steps, so every
		// tied row is emitted. (Java's UnionCursor always dedups — the ALL
		// variant is the Go merge-sort-union with removesDuplicates=false.)
		chosen = chosen[:1]
	}

	// MergeCursor.onNext (:299-301): take the first chosen child's value,
	// consume EVERY chosen child (the advance-all-equal dedup — one emitted
	// row per distinct comparison key, no cross-page state), then snapshot.
	result := chosen[0].result.GetValue()
	for _, s := range chosen {
		s.consume()
	}
	return recordlayer.NewResultWithValue(result, m.snapshotContinuation()), nil
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
	for i, key := range m.compKeys {
		v, err := key.Evaluate(compKeyEvalArg(qr))
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
func (m *mergeSortCursor) compareKeyVals(a, b []any) int {
	for i := range m.compKeys {
		cmp := compareValues(a[i], b[i])
		if cmp == 0 {
			continue
		}
		if m.reverse {
			return -cmp
		}
		return cmp
	}
	return 0
}

// strongestNoNextReason is Java MergeCursor.getStrongestNoNextReason
// (:172-194): an out-of-band reason beats an in-band limit beats
// SOURCE_EXHAUSTED. Only called with at least one stopped child.
func (m *mergeSortCursor) strongestNoNextReason() recordlayer.NoNextReason {
	reason := recordlayer.SourceExhausted
	found := false
	for _, s := range m.states {
		if !s.pulled || s.result.HasNext() {
			continue
		}
		childReason := s.result.GetNoNextReason()
		if !found || childReason.IsOutOfBand() || reason.IsSourceExhausted() {
			reason = childReason
			found = true
		}
	}
	return reason
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

func compareValues(a, b any) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}
	switch av := a.(type) {
	case int64:
		switch bv := b.(type) {
		case int64:
			if av < bv {
				return -1
			}
			if av > bv {
				return 1
			}
			return 0
		case int32:
			bv64 := int64(bv)
			if av < bv64 {
				return -1
			}
			if av > bv64 {
				return 1
			}
			return 0
		default:
			// int64 vs a floating operand: promote and use the
			// Java-faithful float total order (NaN greatest, -0.0 < 0.0).
			// A non-numeric b falls through to the fmt fallback.
			if bf, ok := toFloat64Scalar(b); ok {
				return values.CompareFloat64(float64(av), bf)
			}
		}
	case int32:
		av64 := int64(av)
		switch bv := b.(type) {
		case int64:
			if av64 < bv {
				return -1
			}
			if av64 > bv {
				return 1
			}
			return 0
		case int32:
			if av < bv {
				return -1
			}
			if av > bv {
				return 1
			}
			return 0
		default:
			// int32 vs a floating operand: promote and use the
			// Java-faithful float total order (NaN greatest, -0.0 < 0.0).
			// A non-numeric b falls through to the fmt fallback.
			if bf, ok := toFloat64Scalar(b); ok {
				return values.CompareFloat64(float64(av), bf)
			}
		}
	case float64:
		// Java-faithful float total order (values.CompareFloat64):
		// NaN sorts greatest and NaN==NaN, -0.0 < 0.0 — matching the
		// FDB tuple order an indexed FLOAT column uses, so an in-memory
		// sort/merge/dedup agrees with an ordered index scan on both
		// edge values. A non-numeric b is a cross-type mismatch the
		// planner's type checking excludes; fall through to the fmt
		// fallback.
		if bf, ok := toFloat64Scalar(b); ok {
			return values.CompareFloat64(av, bf)
		}
	case float32:
		// Defense in depth: covering-index reads normalize float32 → float64 at
		// the row boundary (tupleElementToRowValue), so this arm should not be
		// reachable from production rows — but a float32 that slips through any
		// other path must still compare numerically, not by the fmt fallback's
		// lexical decimal string ("10.5" < "2.5"). Same Java-faithful float
		// total order as the float64 arm (NaN greatest, -0.0 < 0.0).
		if bf, ok := toFloat64Scalar(b); ok {
			return values.CompareFloat64(float64(av), bf)
		}
	case bool:
		// false < true — FDB tuple order (0x26 < 0x27), same as Java's
		// Boolean.compare. The fmt fallback happens to get this right
		// ("false" < "true" lexically), but only by accident; pin it.
		if bv, ok := b.(bool); ok {
			if av == bv {
				return 0
			}
			if !av {
				return -1
			}
			return 1
		}
	case string:
		if bv, ok := b.(string); ok {
			if av < bv {
				return -1
			}
			if av > bv {
				return 1
			}
			return 0
		}
	case []byte:
		// BYTES sorts by unsigned lexicographic byte order — the same order the
		// FDB tuple byte-string encoding gives an indexed BYTES column, so an
		// in-memory sort / merge / dedup of a non-indexed BYTES column agrees
		// with an ordered index scan of the same data. Without this arm the
		// fmt.Sprintf("%v") fallback below would compare decimal-list strings
		// ("[0 1]" < "[0]" because ' ' < ']'), putting {0x02} after {0x0A}.
		if bv, ok := b.([]byte); ok {
			return bytes.Compare(av, bv)
		}
	case [16]byte:
		// UUID sorts by unsigned big-endian bytes — the same order the tuple.UUID
		// wire encoding and the filter-path predicates.cmpAny use, so an
		// in-memory sort of a non-indexed UUID column agrees with an ordered
		// index scan. Without this arm the fmt.Sprintf("%v") fallback below would
		// compare decimal-list strings ("[85 14 …]") in lexical, not byte, order.
		if bv, ok := b.([16]byte); ok {
			return bytes.Compare(av[:], bv[:])
		}
	}
	// Should-never-happen fallback: every type the row domain can carry (nil,
	// int64/int32, float64/float32, string, []byte, [16]byte, bool) has a typed
	// arm above (the float arms totally order NaN), so for a well-typed query
	// this is only reachable by a cross-type mismatch the planner's type
	// checking excludes. It
	// compares fmt.Sprintf("%v") strings LEXICALLY, which is NOT tuple order for
	// anything numeric or binary — a typed arm must be added for any new row
	// type before it can be sorted/merged. No error channel exists here
	// (comparators return int); see F9b for making this loud.
	as := fmt.Sprintf("%v", a)
	bs := fmt.Sprintf("%v", b)
	if as < bs {
		return -1
	}
	if as > bs {
		return 1
	}
	return 0
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
	// Use nil continuation — a single-result cursor doesn't support
	// resumption. EndContinuation is rejected by NewResultWithValue
	// (a value result must have a resumable continuation).
	return recordlayer.NewResultWithValue(c.value, nil), nil
}

func (c *singleResultCursor) Close() error   { c.closed = true; return nil }
func (c *singleResultCursor) IsClosed() bool { return c.closed }
