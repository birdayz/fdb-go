package executor

import (
	"context"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// flatMapCursor implements RecordCursor[QueryResult] matching Java's
// FlatMapPipelinedCursor. For each outer row, re-executes the inner
// plan with the outer row bound as a correlation.
//
// Go simplification: no async pipelining (Java uses pipeline depth 5
// for overlapping FDB I/O). The semantics and continuation format are
// identical.
type flatMapCursor struct {
	outerCursor recordlayer.RecordCursor[QueryResult]
	innerPlan   plans.RecordQueryPlan
	store       *recordlayer.FDBRecordStore
	evalCtx     *EvaluationContext
	outerAlias  values.CorrelationIdentifier
	innerAlias  values.CorrelationIdentifier
	props       recordlayer.ExecuteProperties

	innerCursor    recordlayer.RecordCursor[QueryResult]
	currentOuter   *QueryResult
	outerExhausted bool
	closed         bool
}

func newFlatMapCursor(
	outerCursor recordlayer.RecordCursor[QueryResult],
	innerPlan plans.RecordQueryPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	outerAlias, innerAlias values.CorrelationIdentifier,
	props recordlayer.ExecuteProperties,
) *flatMapCursor {
	return &flatMapCursor{
		outerCursor: outerCursor,
		innerPlan:   innerPlan,
		store:       store,
		evalCtx:     evalCtx,
		outerAlias:  outerAlias,
		innerAlias:  innerAlias,
		props:       props,
	}
}

func (c *flatMapCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.closed {
		return recordlayer.RecordCursorResult[QueryResult]{}, fmt.Errorf("cursor is closed")
	}

	for {
		// If we have an active inner cursor, pull from it.
		if c.innerCursor != nil {
			result, err := c.innerCursor.OnNext(ctx)
			if err != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, err
			}
			if result.HasNext() {
				innerRow := result.GetValue()
				merged := mergeRows(*c.currentOuter, innerRow, c.outerAlias.Name(), c.innerAlias.Name())
				return recordlayer.NewResultWithValue(merged, nonEndContinuation), nil
			}
			// Inner exhausted for this outer row — close and advance outer.
			reason := result.GetNoNextReason()
			c.innerCursor.Close()
			c.innerCursor = nil

			if reason != recordlayer.SourceExhausted {
				// Inner hit time limit — propagate.
				return recordlayer.NewResultNoNext[QueryResult](
					reason, result.GetContinuation(),
				), nil
			}
		}

		// Advance the outer cursor.
		if c.outerExhausted {
			return recordlayer.NewResultNoNext[QueryResult](
				recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
			), nil
		}

		outerResult, err := c.outerCursor.OnNext(ctx)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		if !outerResult.HasNext() {
			c.outerExhausted = true
			reason := outerResult.GetNoNextReason()
			return recordlayer.NewResultNoNext[QueryResult](
				reason, outerResult.GetContinuation(),
			), nil
		}

		outerRow := outerResult.GetValue()
		c.currentOuter = &outerRow

		// Bind the outer row as a correlation and execute the inner plan.
		outerDatum, _ := outerRow.Datum.(map[string]any)
		correlatedCtx := c.evalCtx.WithBinding(c.outerAlias, outerDatum)
		innerCursor, err := ExecutePlan(ctx, c.innerPlan, c.store, correlatedCtx, nil, c.props)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		c.innerCursor = innerCursor
	}
}

func (c *flatMapCursor) Close() error {
	c.closed = true
	if c.innerCursor != nil {
		c.innerCursor.Close()
	}
	return c.outerCursor.Close()
}

func (c *flatMapCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*flatMapCursor)(nil)
