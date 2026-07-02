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

	// birth is the RFC-173 Slice 2 ordinal-BIRTH state, probed ONCE at
	// construction from resultValue (nil = name-model flatMap, today's path
	// bit-identically). When enabled, computeResult births the positional
	// row from the RC with per-leg bindings and derives the coexistence
	// Datum FROM it (datumFromPositional) — the RC's baked references can no
	// longer evaluate over name contexts. Gated per emission on the §5
	// DisablePositionalEmission oracle (which falls back to today's Evaluate
	// path, bridged by values.OracleBakedNameFallback).
	birth *ordinalJoinBirth

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
) (*flatMapCursor, error) {
	birth, err := newOrdinalJoinBirth(resultValue, nil)
	if err != nil {
		return nil, err
	}
	// The FlatMap half of the codex PR-447 P1 (@claude final-pass catch): the
	// correlated implementation pushes the join's baked ON references INTO
	// the inner plan (SARGs, residual filters), so LegTypes must be widened
	// from the inner plan's predicate surfaces — a folded result value can
	// drop a leg those references still need (see widenLegTypesFromPlan).
	birth.widenLegTypesFromPlan(innerPlan)
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
		birth:       birth,
	}, nil
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
			// The nil inner pointer is the RFC-173 NULL leg for an
			// ordinal-birth cursor; the name-model path reconstructs the
			// empty-Datum inner row (bit-identical to before).
			if c.leftOuter && !c.innerHadMatch {
				outputRow, err := c.computeResultLegs(*c.currentOuter, nil)
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
		//
		// RFC-173 Slice 2 W3b: for an ORDINAL-birth flatMap (a gated join on
		// the correlated implementation path) the outer binds as its
		// POSITIONAL leg row — the inner plan's baked SARG operands
		// (ofOrdinal over QOV(outer), pushed down as scan-range/index-probe
		// comparisons) resolve by ordinal through the binder's OrdinalRow
		// arm, and lazy outer references resolve leg-relative against the
		// same row. Binding the Datum map here fed baked operands a
		// name-keyed context — the loud BakedNameContextError the W3b flip
		// caught on every correlated-probe join. Name-model cursors (and the
		// §5 oracle) keep the Datum binding bit-identically.
		outerDatum, _ := outerRow.Datum.(map[string]any)
		var outerBinding any = outerDatum
		if c.birth.enabled() && !DisablePositionalEmission {
			row, aerr := adaptLegPositional(outerRow, c.birth.legType(c.outerAlias))
			if aerr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, aerr
			}
			outerBinding = row
		}
		correlatedCtx := c.evalCtx.WithBinding(c.outerAlias, outerBinding)
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
	return c.computeResultLegs(outerRow, &innerRow)
}

// computeResultLegs is computeResult with the inner leg as a pointer: nil is
// the LEFT-OUTER null-inner emission. For an ordinal-birth cursor (RFC-173
// S2, oracle off) it births the positional row from the RC with per-leg
// bindings — the nil inner pointer becomes the NULL leg (QOV(inner)→nil,
// contract ruling #3) — and derives the coexistence Datum FROM the positional
// row (datumFromPositional, last-wins on duplicate names): evaluating an
// ordinal RC over the name-model row context would hit baked references over
// name reads, a loud BakedNameContextError. Name-model cursors (and the §5
// oracle, where values.OracleBakedNameFallback bridges the baked reads) keep
// today's Evaluate path bit-identically, reconstructing the empty-Datum inner
// row for the null-inner emission.
func (c *flatMapCursor) computeResultLegs(outerRow QueryResult, inner *QueryResult) (QueryResult, error) {
	if c.birth.enabled() && !DisablePositionalEmission {
		pos, err := c.birth.evaluate(c.outerAlias.Name(), c.innerAlias.Name(), &outerRow, inner, correlationBase(c.evalCtx))
		if err != nil {
			return QueryResult{}, err
		}
		// The coexistence Datum: a SEED-shaped RV mirrors the anchored RC's
		// bare+qualified key set (downstream name-model consumers — sort
		// Datum fallback, aggregate group keys — resolve dotted references
		// against it; the W3b flip's bare-only Datum silently NULLed them).
		// A folded projection RV keeps bare-only (its name-model counterpart,
		// the projection map, never carried qualified keys).
		datum := datumFromPositional(pos)
		if c.birth.WindowsOK {
			datum = datumFromSpans(pos, c.birth.Spans)
		}
		return QueryResult{Datum: datum, Positional: pos}, nil
	}
	innerRow := QueryResult{Datum: map[string]any{}}
	if inner != nil {
		innerRow = *inner
	}
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
	if c.birth.enabled() {
		// §5 NAME-MODEL ORACLE over an ordinal-birth plan (birth enabled but
		// DisablePositionalEmission on — the only way to reach here with a
		// birth): the ordinal RC's bare-named fields would evaluate to a
		// bare-keys-only map, dropping the ALIAS.COL keys the pre-flip
		// anchored seed RC carried — every dotted downstream read (projection
		// over sort, sort comparators) silently NULLed. Reconstruct the
		// anchored key set with genuine name-model per-field resolution.
		datum, err := c.birth.oracleNameDatum(rowCtx)
		if err != nil {
			return QueryResult{}, err
		}
		return QueryResult{Datum: datum}, nil
	}
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
