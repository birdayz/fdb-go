package executor

import (
	"context"
	"fmt"
	"math"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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

	scanRange, err := scanComparisonsToTupleRange(idxPlan.GetScanComparisons(), evalCtx)
	if err != nil {
		return nil, fmt.Errorf("executor: building scan range for %q: %w", idxPlan.GetIndexName(), err)
	}

	scanProps := recordlayer.ScanProperties{
		ExecuteProperties:   props,
		Reverse:             idxPlan.IsReverse(),
		CursorStreamingMode: recordlayer.StreamingModeIterator,
	}

	indexCursor := maintainer.Scan(scanRange, continuation, scanProps)

	return &aggregateIndexCursor{
		inner:     indexCursor,
		groupCols: p.GetGroupCols(),
		aggColumn: p.GetAggColumn(),
		aggFunc:   p.GetAggregateFunction(),
	}, nil
}

type aggregateIndexCursor struct {
	inner     recordlayer.RecordCursor[*recordlayer.IndexEntry]
	groupCols []string
	aggColumn string
	aggFunc   string
	closed    bool
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
	datum := make(map[string]any, len(c.groupCols)+1)

	for i, col := range c.groupCols {
		if i < len(entry.Key) {
			datum[col] = entry.Key[i]
		}
	}

	if len(entry.Value) > 0 {
		var col string
		if c.aggColumn == "" {
			col = c.aggFunc + "(*)"
		} else {
			col = c.aggFunc + "(" + c.aggColumn + ")"
		}
		datum[col] = entry.Value[0]
	}

	return recordlayer.NewResultWithValue(QueryResult{Datum: datum}, result.GetContinuation()), nil
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

	cursors := make([]recordlayer.RecordCursor[QueryResult], len(children))
	for i, child := range children {
		c, err := ExecutePlan(ctx, child, store, evalCtx, nil, props.ClearSkipAndLimit())
		if err != nil {
			for _, prev := range cursors[:i] {
				prev.Close()
			}
			return nil, err
		}
		cursors[i] = c
	}

	keyVals := p.GetComparisonKey()
	compKeyFunc := multiIntersectionCompKeyFunc(keyVals)

	// IntersectionMulti returns, per matching comparison key, the list of
	// matching rows (one per child). Mirrors Java's IntersectionMultiCursor;
	// the regular intersection keeps only the first child, which would drop
	// every aggregate but the first.
	innerCursor := recordlayer.IntersectionMulti(cursors, compKeyFunc, false)

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
				t[i] = kv.Evaluate(qr.Datum)
			}
			return t
		}
		if qr.PrimaryKey != nil {
			return qr.PrimaryKey
		}
		return tuple.Tuple{fmt.Sprintf("%v", qr.Datum)}
	}
}

// multiIntersectionMergeCursor combines each set of matching child rows
// into a single output row. It merges every child's Datum map (grouping
// columns are identical across children; each child contributes its own
// aggregate column) and then evaluates the plan's result value against the
// merged row to produce the final record. Mirrors Java's
// RecordQueryMultiIntersectionOnValuesPlan.executePlan, which binds each
// child result to its quantifier and evaluates the resultValue.
type multiIntersectionMergeCursor struct {
	inner       recordlayer.RecordCursor[[]QueryResult]
	resultValue values.Value
	closed      bool
}

func (c *multiIntersectionMergeCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	result, err := c.inner.OnNext(ctx)
	if err != nil {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), err
	}
	if !result.HasNext() {
		return recordlayer.NewResultNoNext[QueryResult](result.GetNoNextReason(), result.GetContinuation()), nil
	}

	childResults := result.GetValue()
	merged := make(map[string]any)
	for _, cr := range childResults {
		if m, ok := cr.Datum.(map[string]any); ok {
			for k, v := range m {
				merged[k] = v
			}
		}
	}

	var datum any = merged
	if c.resultValue != nil {
		datum = c.resultValue.Evaluate(merged)
	}
	return recordlayer.NewResultWithValue(QueryResult{Datum: datum, Complete: true}, result.GetContinuation()), nil
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

	results := make([]QueryResult, 0, len(keys))
	for _, pk := range keys {
		rec, err := store.LoadRecord(pk)
		if err != nil {
			return nil, fmt.Errorf("executor: LoadByKeys pk=%v: %w", pk, err)
		}
		if rec == nil {
			continue
		}
		results = append(results, FromStoredRecord(rec))
	}
	return applySkipLimit(
		recordlayer.FromList(results),
		props.Skip, props.ReturnedRowLimit,
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
	childProps := props.ClearSkipAndLimit()
	cursors := make([]recordlayer.RecordCursor[QueryResult], 0, len(inners))
	for _, inner := range inners {
		c, err := ExecutePlan(ctx, inner, store, evalCtx, continuation, childProps)
		if err != nil {
			for _, prev := range cursors {
				_ = prev.Close()
			}
			return nil, err
		}
		cursors = append(cursors, c)
	}
	return applySkipLimit(newConcatCursor[QueryResult](cursors), props.Skip, props.ReturnedRowLimit), nil
}

// producesMergedRows reports whether a plan emits merged join rows
// (multiple quantifiers' columns under qualified "ALIAS.COL" keys) rather
// than single-table rows. A filter over such a plan resolves QOV
// predicates through the qualified-key path, not an alias binding.
//
// This list must stay in sync with the code that WRITES qualified
// "ALIAS.COL" keys: mergeRows (executor.go) for the NLJ cursor, and
// qualifyOuterRow (used by the FlatMap cursor in flat_map_cursor.go).
// Those are the only two sites that emit merged rows today. A future
// merged-row operator (e.g. a hash/merge join) MUST be added here, or a
// filter over it would bind the merged row under one alias and bare-
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
	// the row already carries qualified "ALIAS.COL" keys, so the predicate
	// resolves through the RowEvalContext.Datum qualified path. We must NOT
	// bind the merged row under a single alias: a qov(b).col lookup would
	// then bare-resolve to whichever quantifier last wrote the bare key —
	// e.g. on a null-filled LEFT JOIN row (b absent), qov(b).id would wrongly
	// pick up the outer row's bare ID instead of NULL.
	bindAlias := innerAlias.Name() != "" && !producesMergedRows(p.GetInner())
	needsRowCtx := bindAlias || (evalCtx != nil && (len(evalCtx.params) > 0 || len(evalCtx.scalarSubqueries) > 0 || len(evalCtx.bindings) > 0))
	filtered := &filterResultCursor{
		inner: inner,
		pred: func(qr QueryResult) (keep bool) {
			defer func() {
				if r := recover(); r != nil {
					switch r.(type) {
					case *predicates.TypeMismatchError, *values.ArithmeticOverflowError, *values.ArithmeticDivisionByZeroError, *values.ScalarTypeMismatchError, *values.InvalidCastError:
						panic(r)
					}
					keep = false
				}
			}()
			var rowCtx any = qr.Datum
			// RFC-048 W1: a HAVING/filter reference to a name absent from a
			// complete row (aggregate output) is a bug, not a NULL.
			strict := StrictReferenceCheck && qr.Complete
			if m, ok := qr.Datum.(map[string]any); ok && (strict || needsRowCtx) {
				ec := evalCtx
				if ec == nil {
					ec = EmptyEvaluationContext()
				}
				if bindAlias {
					ec = ec.WithBinding(innerAlias, m)
				}
				if strict {
					rowCtx = ec.RowContextStrict(m)
				} else {
					rowCtx = ec.RowContext(m)
				}
			}
			for _, pred := range preds {
				if pred.Eval(rowCtx) != predicates.TriTrue {
					return false
				}
			}
			return true
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
	mapped := recordlayer.MapCursor(inner, func(qr QueryResult) QueryResult {
		var rowCtx any = qr.Datum
		// RFC-048 W1: a projection reading a name absent from a complete row
		// (aggregate output) is a bug, not a NULL. Production passes the raw
		// Datum map here (no parameter binder / scalar-subquery resolver), so
		// the strict context must carry ONLY Datum + Strict — adding a Binder or
		// ScalarSubqueries would let a param/subquery resolve in the test binary
		// while it returns NULL in production, i.e. strict mode would change
		// results. Bare strict context = identical resolution + miss reporting.
		if StrictReferenceCheck && qr.Complete {
			if m, ok := qr.Datum.(map[string]any); ok {
				rowCtx = &values.RowEvalContext{Datum: m, Strict: true}
			}
		}
		m := resultValue.Evaluate(rowCtx)
		return QueryResult{Datum: m, Record: qr.Record, PrimaryKey: qr.PrimaryKey}
	})
	return applySkipLimit(mapped, props.Skip, props.ReturnedRowLimit), nil
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
	_ = inner.Close()
	if err != nil {
		return nil, err
	}
	if result.HasNext() {
		return newSingleResultCursor(result.GetValue()), nil
	}
	defaultVal := p.GetDefaultValue()
	var datum any
	if defaultVal != nil {
		datum = defaultVal.Evaluate(nil)
	}
	return newSingleResultCursor(QueryResult{Datum: datum}), nil
}

func executeDefaultOnEmpty(
	ctx context.Context,
	p *plans.RecordQueryDefaultOnEmptyPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inner, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props)
	if err != nil {
		return nil, err
	}
	firstResult, err := inner.OnNext(ctx)
	if err != nil {
		_ = inner.Close()
		return nil, err
	}
	if firstResult.HasNext() {
		return newPrependResultCursor(firstResult.GetValue(), inner), nil
	}
	_ = inner.Close()
	defaultVal := p.GetDefaultValue()
	var datum any
	if defaultVal != nil {
		datum = defaultVal.Evaluate(nil)
	}
	return newSingleResultCursor(QueryResult{Datum: datum}), nil
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

	bindingID := values.NamedCorrelationIdentifier(p.GetBindingName())
	var cursors []recordlayer.RecordCursor[QueryResult]
	for _, val := range inValues {
		boundCtx := evalCtx.WithBinding(bindingID, val)
		cursor, err := ExecutePlan(ctx, p.GetInner(), store, boundCtx, nil, props)
		if err != nil {
			for _, c := range cursors {
				c.Close()
			}
			return nil, err
		}
		cursors = append(cursors, cursor)
	}

	if len(cursors) == 1 {
		return cursors[0], nil
	}

	return newConcatCursor(cursors), nil
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
	// merge-sort if comparison keys exist, otherwise concat.
	if len(bindingNames) == 1 && len(inSources[0]) > 0 {
		bindingID := values.NamedCorrelationIdentifier(bindingNames[0])
		var cursors []recordlayer.RecordCursor[QueryResult]
		for _, val := range inSources[0] {
			boundCtx := evalCtx.WithBinding(bindingID, val)
			cursor, err := ExecutePlan(ctx, p.GetInner(), store, boundCtx, nil, props.ClearSkipAndLimit())
			if err != nil {
				for _, c := range cursors {
					c.Close()
				}
				return nil, err
			}
			cursors = append(cursors, cursor)
		}
		if len(cursors) == 1 {
			return applySkipLimit(cursors[0], props.Skip, props.ReturnedRowLimit), nil
		}
		compKeys := p.GetComparisonKeys()
		if len(compKeys) > 0 {
			merged := &mergeSortCursor{
				cursors:   cursors,
				compKeys:  compKeys,
				reverse:   p.IsReverse(),
				dedup:     true,
				peeked:    make([]QueryResult, len(cursors)),
				hasPeeked: make([]bool, len(cursors)),
				exhausted: make([]bool, len(cursors)),
			}
			return applySkipLimit(merged, props.Skip, props.ReturnedRowLimit), nil
		}
		return applySkipLimit(newConcatCursor(cursors), props.Skip, props.ReturnedRowLimit), nil
	}

	return nil, fmt.Errorf("executeInUnion: multi-binding IN union (%d bindings) not yet implemented", len(bindingNames))
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

	cursors := make([]recordlayer.RecordCursor[QueryResult], len(inners))
	for i, inner := range inners {
		c, err := ExecutePlan(ctx, inner, store, evalCtx, nil, props.ClearSkipAndLimit())
		if err != nil {
			for _, prev := range cursors[:i] {
				prev.Close()
			}
			return nil, err
		}
		cursors[i] = c
	}

	return &mergeSortCursor{
		cursors:   cursors,
		compKeys:  p.GetComparisonKeys(),
		reverse:   p.IsReverse(),
		dedup:     p.RemovesDuplicates(),
		peeked:    make([]QueryResult, len(cursors)),
		hasPeeked: make([]bool, len(cursors)),
		exhausted: make([]bool, len(cursors)),
	}, nil
}

type mergeSortCursor struct {
	cursors   []recordlayer.RecordCursor[QueryResult]
	compKeys  []values.Value
	reverse   bool
	dedup     bool
	peeked    []QueryResult
	hasPeeked []bool
	exhausted []bool
	lastKey   string
	hasLast   bool
	closed    bool
}

func (m *mergeSortCursor) IsClosed() bool { return m.closed }

func (m *mergeSortCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		if err := m.fillPeekBuffers(ctx); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}

		bestIdx := -1
		for i := range m.cursors {
			if !m.hasPeeked[i] {
				continue
			}
			if bestIdx < 0 || m.isBetter(m.peeked[i], m.peeked[bestIdx]) {
				bestIdx = i
			}
		}

		if bestIdx < 0 {
			return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
		}

		result := m.peeked[bestIdx]
		m.hasPeeked[bestIdx] = false

		if m.dedup {
			key := m.extractKey(result)
			if m.hasLast && key == m.lastKey {
				continue
			}
			m.lastKey = key
			m.hasLast = true
		}

		return recordlayer.NewResultWithValue(result, &recordlayer.StartContinuation{}), nil
	}
}

func (m *mergeSortCursor) fillPeekBuffers(ctx context.Context) error {
	for i := range m.cursors {
		if m.hasPeeked[i] || m.exhausted[i] {
			continue
		}
		result, err := m.cursors[i].OnNext(ctx)
		if err != nil {
			return err
		}
		if result.HasNext() {
			m.peeked[i] = result.GetValue()
			m.hasPeeked[i] = true
		} else {
			m.exhausted[i] = true
		}
	}
	return nil
}

func (m *mergeSortCursor) isBetter(a, b QueryResult) bool {
	for _, key := range m.compKeys {
		va := key.Evaluate(a.Datum)
		vb := key.Evaluate(b.Datum)
		cmp := compareValues(va, vb)
		if cmp == 0 {
			continue
		}
		if m.reverse {
			return cmp > 0
		}
		return cmp < 0
	}
	return false
}

func (m *mergeSortCursor) extractKey(qr QueryResult) string {
	if len(m.compKeys) == 0 {
		return ""
	}
	t := make(tuple.Tuple, len(m.compKeys))
	for i, key := range m.compKeys {
		v := key.Evaluate(qr.Datum)
		switch tv := v.(type) {
		case nil, int64, int, uint, uint64, float32, float64, string, []byte, bool:
			t[i] = tv
		case int32:
			t[i] = int64(tv)
		default:
			t[i] = fmt.Sprintf("%T:%v", v, v)
		}
	}
	return string(t.Pack())
}

func (m *mergeSortCursor) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	var firstErr error
	for _, c := range m.cursors {
		if err := c.Close(); err != nil && firstErr == nil {
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
			bf := toFloat64(b)
			if !math.IsNaN(bf) {
				af := float64(av)
				if af < bf {
					return -1
				}
				if af > bf {
					return 1
				}
				return 0
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
			bf := toFloat64(b)
			if !math.IsNaN(bf) {
				af := float64(av)
				if af < bf {
					return -1
				}
				if af > bf {
					return 1
				}
				return 0
			}
		}
	case float64:
		if math.IsNaN(av) {
			break
		}
		bv := toFloat64(b)
		if !math.IsNaN(bv) {
			if av < bv {
				return -1
			}
			if av > bv {
				return 1
			}
			return 0
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
	}
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

type concatCursor[T any] struct {
	cursors []recordlayer.RecordCursor[T]
	idx     int
	closed  bool
}

func newConcatCursor[T any](cursors []recordlayer.RecordCursor[T]) *concatCursor[T] {
	return &concatCursor[T]{cursors: cursors}
}

func (c *concatCursor[T]) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[T], error) {
	for c.idx < len(c.cursors) {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[T]{}, err
		}
		result, err := c.cursors[c.idx].OnNext(ctx)
		if err != nil {
			return result, err
		}
		if result.HasNext() {
			return result, nil
		}
		c.idx++
	}
	return recordlayer.NewResultNoNext[T](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
}

func (c *concatCursor[T]) IsClosed() bool { return c.closed }

func (c *concatCursor[T]) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	var firstErr error
	for _, cur := range c.cursors {
		if err := cur.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
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
	// Use nil continuation — a single-result cursor doesn't support
	// resumption. EndContinuation is rejected by NewResultWithValue
	// (a value result must have a resumable continuation).
	return recordlayer.NewResultWithValue(c.value, nil), nil
}

func (c *singleResultCursor) Close() error   { c.closed = true; return nil }
func (c *singleResultCursor) IsClosed() bool { return c.closed }

// prependResultCursor yields one value then delegates to inner.
type prependResultCursor struct {
	first   QueryResult
	inner   recordlayer.RecordCursor[QueryResult]
	yielded bool
	closed  bool
}

func newPrependResultCursor(first QueryResult, inner recordlayer.RecordCursor[QueryResult]) *prependResultCursor {
	return &prependResultCursor{first: first, inner: inner}
}

func (c *prependResultCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if !c.yielded {
		c.yielded = true
		return recordlayer.NewResultWithValue(c.first, &recordlayer.StartContinuation{}), nil
	}
	return c.inner.OnNext(ctx)
}

func (c *prependResultCursor) Close() error   { c.closed = true; return c.inner.Close() }
func (c *prependResultCursor) IsClosed() bool { return c.closed }
