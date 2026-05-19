package executor

import (
	"context"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

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
	cursors := make([]recordlayer.RecordCursor[QueryResult], 0, len(inners))
	for _, inner := range inners {
		c, err := ExecutePlan(ctx, inner, store, evalCtx, continuation, props)
		if err != nil {
			for _, prev := range cursors {
				_ = prev.Close()
			}
			return nil, err
		}
		cursors = append(cursors, c)
	}
	return newConcatResultCursor(cursors), nil
}

func executePredicatesFilter(
	ctx context.Context,
	p *plans.RecordQueryPredicatesFilterPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inner, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props)
	if err != nil {
		return nil, err
	}
	preds := p.GetPredicates()
	needsRowCtx := evalCtx != nil && (len(evalCtx.params) > 0 || len(evalCtx.scalarSubqueries) > 0 || len(evalCtx.bindings) > 0)
	return &filterResultCursor{
		inner: inner,
		pred: func(qr QueryResult) bool {
			var rowCtx any = qr.Datum
			if needsRowCtx {
				if m, ok := qr.Datum.(map[string]any); ok {
					rowCtx = evalCtx.RowContext(m)
				}
			}
			for _, pred := range preds {
				if pred.Eval(rowCtx) != predicates.TriTrue {
					return false
				}
			}
			return true
		},
	}, nil
}

func executeMap(
	ctx context.Context,
	p *plans.RecordQueryMapPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inner, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props)
	if err != nil {
		return nil, err
	}
	resultValue := p.GetResultValue()
	return recordlayer.MapCursor(inner, func(qr QueryResult) QueryResult {
		mapped := resultValue.Evaluate(qr.Datum)
		return QueryResult{Datum: mapped, Record: qr.Record, PrimaryKey: qr.PrimaryKey}
	}), nil
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
	return ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props)
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
	closed    bool
}

func (m *mergeSortCursor) IsClosed() bool { return m.closed }

func (m *mergeSortCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	for {
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
			if key == m.lastKey && m.lastKey != "" {
				continue
			}
			m.lastKey = key
		}

		return recordlayer.NewResultWithValue(result, &recordlayer.EndContinuation{}), nil
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
	var b [64]byte
	buf := b[:0]
	for _, key := range m.compKeys {
		v := key.Evaluate(qr.Datum)
		buf = append(buf, []byte(fmt.Sprintf("%v|", v))...)
	}
	return string(buf)
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
		bv := toFloat64(b)
		af := float64(av)
		if af < bv {
			return -1
		}
		if af > bv {
			return 1
		}
		return 0
	case int32:
		bv := toFloat64(b)
		af := float64(av)
		if af < bv {
			return -1
		}
		if af > bv {
			return 1
		}
		return 0
	case float64:
		bv := toFloat64(b)
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
		return 0
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
			recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
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

// concatResultCursor yields all results from cursors[0], then
// cursors[1], etc.
type concatResultCursor struct {
	cursors []recordlayer.RecordCursor[QueryResult]
	current int
	closed  bool
}

func newConcatResultCursor(cursors []recordlayer.RecordCursor[QueryResult]) *concatResultCursor {
	return &concatResultCursor{cursors: cursors}
}

func (c *concatResultCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	for c.current < len(c.cursors) {
		result, err := c.cursors[c.current].OnNext(ctx)
		if err != nil {
			return result, err
		}
		if result.HasNext() {
			return result, nil
		}
		c.current++
	}
	return recordlayer.NewResultNoNext[QueryResult](
		recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
}

func (c *concatResultCursor) Close() error {
	c.closed = true
	for _, cursor := range c.cursors {
		_ = cursor.Close()
	}
	return nil
}

func (c *concatResultCursor) IsClosed() bool { return c.closed }
