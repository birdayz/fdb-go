package executor

import (
	"context"

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
	hasParams := evalCtx != nil && len(evalCtx.params) > 0
	return &filterResultCursor{
		inner: inner,
		pred: func(qr QueryResult) bool {
			var rowCtx any = qr.Datum
			if hasParams {
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
	return executeUnorderedUnion(ctx,
		plans.NewRecordQueryUnorderedUnionPlan(p.GetInners()),
		store, evalCtx, continuation, props)
}

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
	return recordlayer.NewResultWithValue(c.value, &recordlayer.EndContinuation{}), nil
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
