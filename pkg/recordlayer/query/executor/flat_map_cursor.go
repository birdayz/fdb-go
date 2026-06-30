package executor

import (
	"bytes"
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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
	outerCursor recordlayer.RecordCursor[QueryResult]
	innerPlan   plans.RecordQueryPlan
	store       *recordlayer.FDBRecordStore
	evalCtx     *EvaluationContext
	outerAlias  values.CorrelationIdentifier
	innerAlias  values.CorrelationIdentifier
	resultValue values.Value
	leftOuter   bool
	props       recordlayer.ExecuteProperties

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
	props recordlayer.ExecuteProperties,
) *flatMapCursor {
	return &flatMapCursor{
		outerCursor: outerCursor,
		innerPlan:   innerPlan,
		store:       store,
		evalCtx:     evalCtx,
		outerAlias:  outerAlias,
		innerAlias:  innerAlias,
		resultValue: resultValue,
		leftOuter:   leftOuter,
		props:       props,
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

				outputRow, err := c.computeResult(*c.currentOuter, innerRow)
				if err != nil {
					return recordlayer.RecordCursorResult[QueryResult]{}, err
				}
				cont := c.buildContinuation(result.GetContinuation())
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
				cont := c.buildContinuation(innerCont)
				return recordlayer.NewResultNoNext[QueryResult](reason, cont), nil
			}

			// LEFT OUTER: emit outer row with NULLs when inner had no match.
			if c.leftOuter && !c.innerHadMatch {
				outputRow, err := c.computeResult(*c.currentOuter, QueryResult{Datum: map[string]any{}})
				if err != nil {
					return recordlayer.RecordCursorResult[QueryResult]{}, err
				}
				cont := c.buildContinuation(innerCont)
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
func (c *flatMapCursor) computeResult(outerRow, innerRow QueryResult) (QueryResult, error) {
	// Build evaluation context with both correlations bound.
	outerDatum, _ := outerRow.Datum.(map[string]any)
	// The inner binding is the RAW inner Datum, not a forced map cast. A
	// correlated array UNNEST (RFC-142) flows a BARE SCALAR element (e.g.
	// int64(101)) as the inner row; binding QOV(inner) to it lets the AS
	// alias read the whole element. A row-shaped inner (a scan/EXISTS
	// subquery) binds its map[string]any unchanged — FieldValue.evaluateCorrelated
	// reads the map by key, QOV(inner) reads the whole map.
	nestedCtx := c.evalCtx.
		WithBinding(c.outerAlias, outerDatum).
		WithBinding(c.innerAlias, innerRow.Datum)

	// Evaluate against a RowEvalContext whose Datum is the outer row, so a BARE
	// outer FieldValue (e.g. a projected `ID` with no QOV qualifier — RFC-141
	// projected EXISTS folds the SELECT list into the result value) resolves
	// against the outer row, while QOV references to the outer/inner aliases
	// resolve through the correlation bindings (Correlations).
	rowCtx := nestedCtx.RowContext(outerDatum)
	computed, err := c.resultValue.Evaluate(rowCtx)
	if err != nil {
		return QueryResult{}, err
	}
	// Identity-over-outer FlatMap (the result value is exactly the outer
	// quantifier's object — the WHERE-EXISTS pass-through, RFC-141): the output
	// IS the outer record flowed under the outer quantifier, so qualify its keys
	// under the outer alias. Downstream projections reference the outer columns
	// as `ALIAS.COL` (a FieldValue over QOV(outer)); a bare-keyed map would not
	// resolve them. Mirrors the prior semi-join cursor's qualifyOuterRow and
	// Java's outer-record-under-outer-quantifier flow.
	if qov, ok := c.resultValue.(*values.QuantifiedObjectValue); ok && qov.Correlation == c.outerAlias {
		if m, ok := computed.(map[string]any); ok {
			return qualifyOuterRow(QueryResult{Datum: m, Record: outerRow.Record, PrimaryKey: outerRow.PrimaryKey}, c.outerAlias.Name()), nil
		}
	}
	return QueryResult{Datum: computed}, nil
}

// buildContinuation creates a FlatMapContinuation proto. The decision is purely
// on the inner cursor's state (matching Java FlatMapPipelinedCursor.toByteString,
// :413-430): if the inner has a resumable position (not END) — a value emit or an
// inner out-of-band stop mid-row — encode the prior outer position + inner
// position so resume continues THIS outer's inner. If the inner is exhausted
// (END), encode the advanced outer position with no inner (next outer on resume).
func (c *flatMapCursor) buildContinuation(innerCont recordlayer.RecordCursorContinuation) recordlayer.RecordCursorContinuation {
	if innerCont != nil && innerCont.IsEnd() && c.lastOuterContinuation != nil && c.lastOuterContinuation.IsEnd() {
		return &recordlayer.EndContinuation{}
	}

	fmc := &gen.FlatMapContinuation{}

	if c.currentOuter != nil && c.currentOuter.PrimaryKey != nil {
		fmc.CheckValue = c.currentOuter.PrimaryKey.Pack()
	}

	// Java FlatMapPipelinedCursor.Continuation (FlatMapPipelinedCursor.java:373)
	// ALWAYS pairs priorOuterContinuation (the position AT the current outer row)
	// with the inner continuation — there is no "value emit vs limit emit"
	// distinction. The decision is purely whether the inner has a resumable
	// position:
	//   - inner NOT exhausted (a value emit mid-inner, or an inner out-of-band
	//     stop): encode (priorOuter, inner) so resume re-opens THIS outer and
	//     continues its inner after the last row. Encoding the ADVANCED outer
	//     position here (as a prior Go-only innerTimeLimited flag did for the
	//     value-emit path) skips the rest of this outer's inner rows on resume —
	//     a silent row-drop on any mid-inner page boundary.
	//   - inner exhausted (END): advance to the next outer (lastOuter, no inner).
	//     Equivalent to Java's (priorOuter, inner=END), which re-opens the outer
	//     and immediately advances.
	if innerCont != nil && !innerCont.IsEnd() {
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
