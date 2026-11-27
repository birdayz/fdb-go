// Portions derived from FoundationDB Record Layer (AggregateCursor.java,
// StreamGrouping.java, FlatMapPipelinedCursor.java,
// NumericAggregationValue.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"context"

	"fdb.dev/internal/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/pkg/recordlayer"
)

// ---------------------------------------------------------------------------
// aggregateCursor — streaming GROUP BY (Java-aligned)
// ---------------------------------------------------------------------------

// aggregateCursor implements RecordCursor[QueryResult] for GROUP BY.
// Input MUST be sorted by grouping keys (guaranteed by the planner
// inserting a sort when no index provides the ordering).
//
// Processes inner records one-by-one. Detects group breaks (grouping
// key change). On group break: emits the completed group. On
// TimeLimitReached from inner: serializes the single in-progress
// group's partial state into PartialAggregationResult proto — exactly
// matching Java's AggregateCursor + StreamGrouping.
//
// The continuation carries:
//   - inner cursor position (leaf scan FDB key)
//   - partial accumulator state (ONE group key + running aggregates)
//
// This is wire-compatible with Java's AggregateCursorContinuation proto.
type aggregateCursor struct {
	inner        recordlayer.RecordCursor[QueryResult]
	groupingKeys []values.Value
	aggregates   []expressions.AggregateSpec

	// scalarInputLayout is the input's provided layout when that input flows a
	// SCALAR (a non-ordinal Explode's element: a correlated array as a block's
	// first FROM item, `(SELECT SUM(x) FROM p.arr x)`), nil otherwise. A key or
	// operand over such an input reads the element as the input QOV itself, so
	// it is bound to the unwrapped scalar, as executeFilter and executeMap
	// bind it.
	scalarInputLayout values.OrdinalLayout

	// evalCtx carries params/subqueries/outer bindings so a group-key / operand
	// reference resolves against the inner PositionalRow the SAME way
	// executeFilter / executeMap do (frontierRowContext → evaluateOrdinal,
	// by the baked plan-time ordinal), robust to a covering-index layout — never
	// a name-keyed read. flatFrontierInput is true when the input bottoms out at
	// a SINGLE-SOURCE flat producer (base-table scan/index, a nested
	// StreamingAgg) beneath any number of layout-preserving / reshaping
	// single-child nodes — the group-by SORT, a filter, a LIMIT/fetch, a
	// projection / derived table / CTE (RecordQueryMapPlan), a
	// DISTINCT, a WHERE-EXISTS semi-join (identity-over-outer FlatMap). Every such
	// producer emits a flat single-source output row with an unambiguous plan-time
	// layout, so keys/operands resolve positionally. It is FALSE the moment the
	// chain crosses a JOIN / multi-source (a non-identity FlatMap, an NLJ, a
	// union): a raw 2-way ordinal JOIN merge resolves through its LEG WINDOWS
	// (joinWindowsOK) instead — a flat input must never be mis-routed through leg
	// windows. Probed once at construction from the inner plan.
	evalCtx           *EvaluationContext
	flatFrontierInput bool
	needsRowCtx       bool
	// inputEdges declares the streaming aggregate's physical child edge. An
	// admitted child row resolves an aggregate operand through its own exact
	// QOV, not through the child's current-only output layout; threading this
	// edge into frontierRowContext keeps that declaration explicit at runtime.
	inputEdges []values.QuantifiedObjectValue

	// When the input flows a 2-way ordinal join's MERGED positional row
	// (downstreamLegWindows unwraps the layout-preserving passthroughs down to
	// the join and derives its leg windows), a QUALIFIED group-key / operand
	// reference (D.DNAME, E.SALARY) resolves LEG-LOCALLY off the merged row
	// through legWindowRowContext — the SAME spanAwareRow resolver executeMap /
	// executeFilter use over the same merge.
	// joinWindowsOK is true only for a genuine gated ordinal join input; a reshaping /
	// non-join input keeps windowsOK false and resolves through the general
	// positional frontier. Probed once at construction from the inner plan.
	joinLegSpans  []legSpan
	joinWindowsOK bool

	// Current in-progress group state (streaming — only ONE group at a time).
	currentGroupKey string
	currentKeyVals  []any
	current         *groupState

	// For the no-grouping-keys case (scalar aggregation like COUNT(*)).
	scalarMode bool

	// Inner cursor tracking.
	innerExhausted        bool
	lastInnerContinuation recordlayer.RecordCursorContinuation
	emittedFinal          bool
	closed                bool

	// lastNoNext replays a prior terminal result on a contract-violating
	// re-call (Java AggregateCursor inherits the cached no-next result:
	// `if (nextResult != null && !nextResult.hasNext()) return nextResult`).
	// CRITICAL here: without it, a re-call after a mid-group out-of-band stop
	// re-pulls the inner and consumes rows into an accumulator whose partial
	// state was already serialized into the reported continuation — those rows
	// are silently lost on resume.
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]

	// previousContinuationInGroup is the inner continuation of the last row
	// ACCEPTED into the current group — Java AggregateCursor's
	// previousContinuationInGroup (AggregateCursor.java:116-146). Every
	// non-group-break row advances it; a group-break / final-group emit carries
	// it (with NO partial state) as the row's continuation, then the break row's
	// continuation replaces it (single-element-group correctness — Java's
	// movement-table comment block). Fresh cursors start at StartContinuation;
	// executeAggregation initializes a RESUMED cursor to the decoded INNER
	// position (never the whole aggregate continuation re-wrapped as its own
	// inner, which is Java's latent nesting bug in fromRawBytes → toProto:
	// re-emitting it would hand aggregate proto bytes to the LEAF plan).
	previousContinuationInGroup recordlayer.RecordCursorContinuation
	// outputType is the plan-authoritative exact aggregate result schema.
	// finalizeGroup and the scalar-empty alternative must publish this exact
	// type; reconstructing names-only rows would reintroduce UNKNOWN fields and
	// fail the plan's provided-layout contract.
	outputType *values.RecordType
}

type groupState struct {
	keyVals []any
	count   int64
	counts  []int64
	sums    []float64
	sumsI   []int64
	allInt  []bool
	mins    []any
	maxs    []any
	// arrays holds each ARRAY_AGG's collected elements.
	arrays  [][]any
	bitmaps [][]byte
}

func newAggregateCursorWithOutputType(
	inner recordlayer.RecordCursor[QueryResult],
	groupingKeys []values.Value,
	aggregates []expressions.AggregateSpec,
	innerPlan plans.RecordQueryPlan,
	evalCtx *EvaluationContext,
	inputQOV values.QuantifiedObjectValue,
	outputType *values.RecordType,
) *aggregateCursor {
	legSpans, windowsOK := downstreamLegWindows(innerPlan)
	var inputEdges []values.QuantifiedObjectValue
	if inputQOV != nil {
		inputEdges = []values.QuantifiedObjectValue{inputQOV}
	}
	var scalarInputLayout values.OrdinalLayout
	if innerPlan != nil {
		if layout, err := innerPlan.ProvidedOutputLayout(); err == nil && layout != nil &&
			layout.CarrierKind() == values.OrdinalCarrierScalar {
			scalarInputLayout = layout
		}
	}
	return &aggregateCursor{
		scalarInputLayout: scalarInputLayout,
		inner:             inner,
		groupingKeys:      groupingKeys,
		aggregates:        aggregates,
		scalarMode:        len(groupingKeys) == 0,
		evalCtx:           evalCtx,
		flatFrontierInput: aggregateInputIsFlatFrontier(innerPlan),
		// A CURRENT_TIMESTAMP-family group key / aggregate operand needs the
		// statement clock a RowEvalContext carries — a bare frontier row
		// would drift per row.
		needsRowCtx: hasBindingContext(evalCtx) ||
			valuesNeedStatementContext(groupingKeys...) ||
			aggregatesNeedStatementContext(aggregates),
		inputEdges:    inputEdges,
		joinLegSpans:  legSpans,
		joinWindowsOK: windowsOK,
		outputType:    outputType,
		// Java: this.previousContinuationInGroup = continuation (the constructor
		// param, START on a fresh cursor). executeAggregation overwrites it with
		// the decoded inner position on a resume.
		previousContinuationInGroup: &recordlayer.StartContinuation{},
	}
}

// aggregateInputIsFlatFrontier reports whether the streaming aggregate's input
// bottoms out at a SINGLE-SOURCE flat producer whose emitted PositionalRow's
// columns are unambiguous — so a group-key / operand name resolves against it by
// its baked plan-time ordinal exactly as executeFilter / executeMap resolve
// theirs (robust to a covering-index column order).
//
// It walks single-child nodes down to the leaf:
//   - RETURNS TRUE at a base-table SCAN / INDEX scan, or a nested StreamingAgg
//     (single-source flat producers).
//   - PEELS THROUGH the layout-preserving wrappers (the group-by SORT, a filter, a
//     type filter, a LIMIT, the covering-index fetch) AND the reshaping single-source
//     producers (projection / derived table / CTE, DISTINCT, Map) — each re-emits a
//     flat single-source positional row, and what matters for layout safety is the
//     absence of a JOIN below, not the exact reshaping node.
//   - For a FlatMap: a WHERE-EXISTS identity-over-outer semi-join is a row-preserving
//     passthrough toward its OUTER (the outer scan row flows through unchanged), so it
//     peels to the outer; any OTHER FlatMap (a lateral join / merge producer) is
//     multi-source and RETURNS FALSE.
//   - RETURNS FALSE at every JOIN / multi-source node (a non-identity FlatMap, an NLJ,
//     a union). A raw 2-way ordinal JOIN merge resolves through its leg windows
//     (joinWindowsOK); other multi-source inputs resolve through the general
//     positional frontier.
func aggregateInputIsFlatFrontier(input plans.RecordQueryPlan) bool {
	for input != nil {
		switch p := input.(type) {
		case *plans.RecordQueryScanPlan,
			*plans.RecordQueryIndexPlan,
			// A COVERING index scan is a single-source flat producer for
			// exactly the reason the bare one is: it emits one positional row
			// per index entry, from one index, with no join below. It is also
			// the ORDINARY shape reached here — the access path emits
			// Fetch(Covering(IndexScan)) for every index-backed access, and the
			// fetch arm below peels straight onto it — so omitting it does not
			// lose an exotic case, it turns the flat-frontier answer off for
			// every index-backed aggregate.
			*plans.RecordQueryCoveringIndexPlan,
			*plans.RecordQueryTextIndexPlan,
			*plans.RecordQueryVectorIndexPlan,
			*plans.RecordQueryStreamingAggregationPlan,
			// A UNION (ALL / recursive-CTE level) emits a FLAT positional
			// row per leg, and every leg flows the union's one exact row
			// (RFC-242), so an aggregate over it resolves its group key /
			// operand against that row — the flat frontier.
			*plans.RecordQueryUnionPlan,
			*plans.RecordQueryUnorderedUnionPlan,
			*plans.RecordQueryRecursiveLevelUnionPlan:
			return true
		case *plans.RecordQueryFetchFromPartialRecordPlan:
			input = p.GetInner()
		case *plans.RecordQueryInMemorySortPlan:
			input = p.GetInner()
		case *plans.RecordQueryLimitPlan:
			input = p.GetInner()
		case *plans.RecordQueryTypeFilterPlan:
			input = p.GetInner()
		case *plans.RecordQueryFilterPlan:
			input = p.GetInner()
		case *plans.RecordQueryPredicatesFilterPlan:
			input = p.GetInner()
		case *plans.RecordQueryDistinctPlan:
			input = p.GetInner()
		case *plans.RecordQueryUnorderedPrimaryKeyDistinctPlan:
			input = p.GetInner()
		case *plans.RecordQueryMapPlan:
			input = p.GetInner()
		case *plans.RecordQueryFlatMapPlan:
			if qov, ok := values.AsQuantifiedObjectValue(p.GetResultValue()); ok && qov.Correlation() == p.GetOuterAlias() {
				input = p.GetOuter()
				continue
			}
			return false
		default:
			return false
		}
	}
	return false
}

// withPartialState restores accumulator state from a previous transaction's
// continuation. Mirrors Java's StreamGrouping constructor with
// PartialAggregationResult parameter.
//
// The group key is re-derived from the decoded keyVals, never taken from a
// legacy token's packed bytes: those were packed under the encoding of the
// binary that minted the token (NaN canonicalization changed it once), and a
// stale key reports a false group break that emits the group twice. Packing
// fails only for a keyVal no token can carry; the decoded key is kept then.
func (c *aggregateCursor) withPartialState(groupKey string, keyVals []any, gs *groupState) {
	c.currentGroupKey = groupKey
	if !c.scalarMode && len(keyVals) > 0 {
		if rederived, err := packGroupKey(keyVals); err == nil {
			c.currentGroupKey = rederived
		}
	}
	c.currentKeyVals = keyVals
	c.current = gs
}

func (c *aggregateCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.closed {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
	}
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}

	// If inner is exhausted, emit the final group (if any).
	if c.innerExhausted {
		return c.emitFinal()
	}

	// Pull records from inner, accumulate, detect group breaks.
	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		result, err := c.inner.OnNext(ctx)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}

		if !result.HasNext() {
			reason := result.GetNoNextReason()
			c.lastInnerContinuation = result.GetContinuation()

			if reason == recordlayer.SourceExhausted {
				c.innerExhausted = true
				return c.emitFinal()
			}

			// TimeLimitReached — serialize the single in-progress group
			// into the continuation proto. Matches Java's
			// AggregateCursorContinuation with PartialAggregationResult.
			contBytes, encErr := encodeAggregateContinuation(
				result.GetContinuation(),
				c.groupingKeys, c.currentKeyVals, c.current,
				c.aggregates,
			)
			if encErr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, encErr
			}
			res := recordlayer.NewResultNoNext[QueryResult](
				reason, recordlayer.NewBytesContinuation(contBytes),
			)
			c.lastNoNext = &res
			return res, nil
		}

		row := result.GetValue()
		groupKey, keyVals, gkErr := c.computeGroupKey(row)
		if gkErr != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, gkErr
		}

		if c.current == nil {
			// First row — start the first group.
			c.currentGroupKey = groupKey
			c.currentKeyVals = keyVals
			c.current = c.newGroupState()
		} else if !c.scalarMode && groupKey != c.currentGroupKey {
			// Group break — finalize the current group and start a new one.
			completed := c.finalizeGroup()
			c.currentGroupKey = groupKey
			c.currentKeyVals = keyVals
			c.current = c.newGroupState()

			// Accumulate the new row into the new group, then emit the
			// completed group with (previousContinuationInGroup, NO partial
			// state) — Java AggregateCursor.java:131-136: the emitted row's
			// resume point is the last row ACCEPTED into the completed group,
			// so a resume re-reads the break row (this row) as the next
			// group's first row, by design. AFTER capturing the emit
			// continuation, advance previousContinuationInGroup to the BREAK
			// row's continuation (Java :137-146's post-emit update) so a
			// single-element group's emit carries its own (only) row.
			if err := c.accumulateRow(row); err != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, err
			}
			emitCont := &aggregateCursorContinuation{innerCont: c.previousContinuationInGroup}
			c.previousContinuationInGroup = result.GetContinuation()
			return recordlayer.NewResultWithValue(completed, emitCont), nil
		}

		if err := c.accumulateRow(row); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		// A row accepted into the current group advances the group's resume
		// point (Java AggregateCursor.java:116-130: previousValidResult /
		// previousContinuationInGroup move on every non-group-break row).
		c.previousContinuationInGroup = result.GetContinuation()
	}
}

func (c *aggregateCursor) emitFinal() (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.emittedFinal {
		return recordlayer.NewResultNoNext[QueryResult](
			recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
		), nil
	}
	c.emittedFinal = true

	if c.current == nil {
		// No rows at all (and no restored partial state) — exhausted with no
		// emit, in BOTH modes. Java AggregateCursor.isNoRecords() →
		// RecordCursorResult.exhausted(): the streaming-aggregate cursor never
		// fabricates a row on empty input. The scalar COUNT(*)-on-empty default
		// row is executeAggregation's OrElse alternative (mirroring Java's
		// DefaultOnEmpty(StreamingAggregation) plan structure), whose OrElse
		// state machine makes the default row itself resumable — an inline emit
		// here would be re-fabricated by any resume that lands past the last
		// input row (a duplicate with no error).
		return recordlayer.NewResultNoNext[QueryResult](
			recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
		), nil
	}

	// Emit the final group with (previousContinuationInGroup, NO partial
	// state) — Java AggregateCursor.java:149-155 (the SOURCE_EXHAUSTED arm):
	// a resume from this row's continuation re-opens the inner after the last
	// accepted row, finds it exhausted with no rows and no state, and reports
	// SourceExhausted (isNoRecords) — no duplicate final row.
	completed := c.finalizeGroup()
	return recordlayer.NewResultWithValue(
		completed, &aggregateCursorContinuation{innerCont: c.previousContinuationInGroup},
	), nil
}

// aggregateCursorContinuation lazily encodes the AggregateCursorContinuation
// proto at ToBytes() time, propagating a child encode failure through the
// RecordCursorContinuation contract (the flatMapCursorContinuation pattern).
// It is the continuation of an EMITTED aggregate row — Java
// AggregateCursor.AggregateCursorContinuation's 2-arg (stateless) form: the
// inner continuation of the last row ACCEPTED into the emitted group, NO
// partial state. Resuming from it re-aggregates the next group from scratch,
// deliberately re-reading that group's first (break) row — Java's movement
// table. IsEnd() is false even when the wrapped inner encodes to empty bytes,
// matching Java's isEnd() delegating to the inner continuation OBJECT, never
// its byte form.
type aggregateCursorContinuation struct {
	innerCont recordlayer.RecordCursorContinuation
}

func (w *aggregateCursorContinuation) ToBytes() ([]byte, error) {
	return encodeAggregateContinuation(w.innerCont, nil, nil, nil, nil)
}

func (w *aggregateCursorContinuation) IsEnd() bool { return false }

func (c *aggregateCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *aggregateCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*aggregateCursor)(nil)
