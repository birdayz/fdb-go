package executor

import (
	"bytes"
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// flatMapCursor implements RecordCursor[QueryResult] matching Java's
// FlatMapPipelinedCursor. For each outer row, re-executes the inner
// plan with the outer row bound as a correlation.
//
// Go simplification: no async pipelining (Java uses pipeline depth 5
// for overlapping FDB I/O). Continuation: Go uses FlatMapContinuation
// proto (outer+inner+check_value). check_value stores the outer row's
// PK bytes; on resume, verifies the outer row hasn't changed between
// transactions (concurrent-modification detection).
type flatMapCursor struct {
	outerCursor   recordlayer.RecordCursor[QueryResult]
	innerPlan     plans.RecordQueryPlan
	store         *recordlayer.FDBRecordStore
	evalCtx       *EvaluationContext
	outerAlias    values.CorrelationIdentifier
	innerAlias    values.CorrelationIdentifier
	resultValue   values.Value
	leftOuter     bool
	existsMode    bool
	notExistsMode bool
	props         recordlayer.ExecuteProperties

	innerCursor    recordlayer.RecordCursor[QueryResult]
	currentOuter   *QueryResult
	innerHadMatch  bool
	outerExhausted bool
	closed         bool

	// Continuation state for cross-transaction resume.
	priorOuterContinuation recordlayer.RecordCursorContinuation
	lastOuterContinuation  recordlayer.RecordCursorContinuation
	initialInnerCont       []byte
	hasPendingInner        bool
	pendingCheckValue      []byte
}

func newFlatMapCursor(
	outerCursor recordlayer.RecordCursor[QueryResult],
	innerPlan plans.RecordQueryPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	outerAlias, innerAlias values.CorrelationIdentifier,
	resultValue values.Value,
	leftOuter bool,
	existsMode bool,
	notExistsMode bool,
	props recordlayer.ExecuteProperties,
) *flatMapCursor {
	return &flatMapCursor{
		outerCursor:   outerCursor,
		innerPlan:     innerPlan,
		store:         store,
		evalCtx:       evalCtx,
		outerAlias:    outerAlias,
		innerAlias:    innerAlias,
		resultValue:   resultValue,
		leftOuter:     leftOuter,
		existsMode:    existsMode,
		notExistsMode: notExistsMode,
		props:         props,
	}
}

func (c *flatMapCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.closed {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		// If we have an active inner cursor, pull from it.
		if c.innerCursor != nil {
			result, err := c.innerCursor.OnNext(ctx)
			if err != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, err
			}
			if result.HasNext() {
				c.innerHadMatch = true
				innerRow := result.GetValue()

				// EXISTS: inner has a match → emit outer row, skip rest.
				if c.existsMode {
					c.innerCursor.Close()
					c.innerCursor = nil
					cont := c.buildContinuation(result.GetContinuation(), false)
					row := qualifyOuterRow(*c.currentOuter, c.outerAlias.Name())
					return recordlayer.NewResultWithValue(row, cont), nil
				}

				// NOT EXISTS: inner has a match → outer row excluded.
				// Close inner and move to next outer.
				if c.notExistsMode {
					c.innerCursor.Close()
					c.innerCursor = nil
					continue
				}

				outputRow := c.computeResult(*c.currentOuter, innerRow)
				cont := c.buildContinuation(result.GetContinuation(), false)
				return recordlayer.NewResultWithValue(outputRow, cont), nil
			}
			// Inner exhausted for this outer row — close and advance outer.
			reason := result.GetNoNextReason()
			innerCont := result.GetContinuation()
			c.innerCursor.Close()
			c.innerCursor = nil

			if reason.IsOutOfBand() {
				// Inner hit a scan/time/byte limit — serialize
				// FlatMapContinuation with current outer + inner
				// position so the next page resumes correctly.
				cont := c.buildContinuation(innerCont, true)
				return recordlayer.NewResultNoNext[QueryResult](reason, cont), nil
			}

			// NOT EXISTS: inner exhausted with no match → emit outer row.
			if c.notExistsMode && !c.innerHadMatch {
				cont := c.buildContinuation(innerCont, false)
				row := qualifyOuterRow(*c.currentOuter, c.outerAlias.Name())
				return recordlayer.NewResultWithValue(row, cont), nil
			}

			// LEFT OUTER: emit outer row with NULLs when inner had no match.
			if c.leftOuter && !c.innerHadMatch {
				outputRow := c.computeResult(*c.currentOuter, QueryResult{Datum: map[string]any{}})
				cont := c.buildContinuation(innerCont, false)
				return recordlayer.NewResultWithValue(outputRow, cont), nil
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
			cont := c.wrapOuterContinuation(outerResult.GetContinuation())
			return recordlayer.NewResultNoNext[QueryResult](reason, cont), nil
		}

		outerRow := outerResult.GetValue()
		c.currentOuter = &outerRow
		c.innerHadMatch = false
		c.priorOuterContinuation = c.lastOuterContinuation
		c.lastOuterContinuation = outerResult.GetContinuation()

		if len(c.pendingCheckValue) > 0 && outerRow.PrimaryKey != nil {
			currentCheck := outerRow.PrimaryKey.Pack()
			if !bytes.Equal(currentCheck, c.pendingCheckValue) {
				return recordlayer.RecordCursorResult[QueryResult]{},
					fmt.Errorf("flatMap: outer row changed between transactions (check_value mismatch)")
			}
			c.pendingCheckValue = nil
		}

		// Bind the outer row as a correlation and execute the inner plan.
		// Use initialInnerCont for the first outer row on resume.
		outerDatum, _ := outerRow.Datum.(map[string]any)
		correlatedCtx := c.evalCtx.WithBinding(c.outerAlias, outerDatum)
		var innerContBytes []byte
		if c.initialInnerCont != nil {
			innerContBytes = c.initialInnerCont
			c.initialInnerCont = nil
			c.hasPendingInner = false
		}
		innerCursor, err := ExecutePlan(ctx, c.innerPlan, c.store, correlatedCtx, innerContBytes, c.props)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		c.innerCursor = innerCursor
	}
}

// computeResult evaluates the resultValue with both outer and inner
// bound as correlations. Mirrors Java's FlatMapPipelinedCursor:
//
//	nestedContext = fromOuterContext.withBinding(CORRELATION, innerAlias, innerResult)
//	computed = resultValue.eval(store, nestedContext)
//	return inheritOuter ? outerResult.withComputed(computed) : QueryResult.ofComputed(computed)
func (c *flatMapCursor) computeResult(outerRow, innerRow QueryResult) QueryResult {
	// Build evaluation context with both correlations bound.
	outerDatum, _ := outerRow.Datum.(map[string]any)
	innerDatum, _ := innerRow.Datum.(map[string]any)
	nestedCtx := c.evalCtx.
		WithBinding(c.outerAlias, outerDatum).
		WithBinding(c.innerAlias, innerDatum)

	computed := c.resultValue.Evaluate(nestedCtx)
	return QueryResult{Datum: computed}
}

// buildContinuation creates a FlatMapContinuation proto. When innerTimeLimited
// is true, the inner cursor hit the time limit mid-row — encode the prior outer
// position + inner position for resume. Otherwise encode the current outer
// position (inner exhausted, next outer row on resume).
func (c *flatMapCursor) buildContinuation(innerCont recordlayer.RecordCursorContinuation, innerTimeLimited bool) recordlayer.RecordCursorContinuation {
	if innerCont != nil && innerCont.IsEnd() && c.lastOuterContinuation != nil && c.lastOuterContinuation.IsEnd() {
		return &recordlayer.EndContinuation{}
	}

	fmc := &gen.FlatMapContinuation{}

	if c.currentOuter != nil && c.currentOuter.PrimaryKey != nil {
		fmc.CheckValue = c.currentOuter.PrimaryKey.Pack()
	}

	if innerTimeLimited && innerCont != nil && !innerCont.IsEnd() {
		if c.priorOuterContinuation != nil && !c.priorOuterContinuation.IsEnd() {
			fmc.OuterContinuation, _ = c.priorOuterContinuation.ToBytes()
		}
		fmc.InnerContinuation, _ = innerCont.ToBytes()
	} else {
		if c.lastOuterContinuation != nil && !c.lastOuterContinuation.IsEnd() {
			fmc.OuterContinuation, _ = c.lastOuterContinuation.ToBytes()
		}
	}

	data, err := proto.Marshal(fmc)
	if err != nil {
		return nonEndContinuation
	}
	return recordlayer.NewBytesContinuation(data)
}

// wrapOuterContinuation wraps the outer cursor's continuation in a
// FlatMapContinuation proto. Used when the outer cursor stops (e.g.,
// TimeLimitReached) before producing a value.
func (c *flatMapCursor) wrapOuterContinuation(outerCont recordlayer.RecordCursorContinuation) recordlayer.RecordCursorContinuation {
	if outerCont != nil && outerCont.IsEnd() {
		return &recordlayer.EndContinuation{}
	}
	fmc := &gen.FlatMapContinuation{}
	if outerCont != nil {
		fmc.OuterContinuation, _ = outerCont.ToBytes()
	}
	if c.hasPendingInner {
		fmc.InnerContinuation = c.initialInnerCont
	}
	data, err := proto.Marshal(fmc)
	if err != nil {
		return nonEndContinuation
	}
	return recordlayer.NewBytesContinuation(data)
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
