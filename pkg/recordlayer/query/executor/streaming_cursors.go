package executor

import (
	"context"
	"fmt"
	"math"
	"strings"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

var nonEndContinuation = recordlayer.NewBytesContinuation([]byte{0})

// SortBufferExceededError is returned when an in-memory sort
// materializes more rows than the configured limit. Prevents OOM
// on unbounded ORDER BY without LIMIT.
type SortBufferExceededError struct {
	Rows  int
	Limit int
}

func (e *SortBufferExceededError) Error() string {
	return fmt.Sprintf("sort buffer exceeded: %d rows materialized (limit %d); add LIMIT or use an ordered index", e.Rows, e.Limit)
}

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

	// evalCtx carries params/subqueries/outer bindings so a group-key / operand
	// reference resolves against the inner PositionalRow the SAME way
	// executeFilter / executeProjection do (frontierRowContext → evaluateOrdinal,
	// by the baked plan-time ordinal), robust to a covering-index layout — never
	// a name-keyed read. flatFrontierInput is true when the input bottoms out at
	// a SINGLE-SOURCE flat producer (base-table scan/index, a nested
	// StreamingAgg) beneath any number of layout-preserving / reshaping
	// single-child nodes — the group-by SORT, a filter, a LIMIT/fetch, a
	// projection / derived table / CTE (RecordQueryProjectionPlan/MapPlan), a
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

	// When the input flows a 2-way ordinal join's MERGED positional row
	// (downstreamLegWindows unwraps the layout-preserving passthroughs down to
	// the join and derives its leg windows), a QUALIFIED group-key / operand
	// reference (D.DNAME, E.SALARY) resolves LEG-LOCALLY off the merged row
	// through legWindowRowContext — the SAME spanAwareRow resolver executeProjection /
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
}

func newAggregateCursor(
	inner recordlayer.RecordCursor[QueryResult],
	groupingKeys []values.Value,
	aggregates []expressions.AggregateSpec,
	innerPlan plans.RecordQueryPlan,
	evalCtx *EvaluationContext,
) *aggregateCursor {
	legSpans, windowsOK := downstreamLegWindows(innerPlan)
	return &aggregateCursor{
		inner:             inner,
		groupingKeys:      groupingKeys,
		aggregates:        aggregates,
		scalarMode:        len(groupingKeys) == 0,
		evalCtx:           evalCtx,
		flatFrontierInput: aggregateInputIsFlatFrontier(innerPlan),
		needsRowCtx:       hasBindingContext(evalCtx),
		joinLegSpans:      legSpans,
		joinWindowsOK:     windowsOK,
	}
}

// aggregateInputIsFlatFrontier reports whether the streaming aggregate's input
// bottoms out at a SINGLE-SOURCE flat producer whose emitted PositionalRow's
// columns are unambiguous — so a group-key / operand name resolves against it by
// its baked plan-time ordinal exactly as executeFilter / executeProjection resolve
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
			*plans.RecordQueryTextIndexPlan,
			*plans.RecordQueryVectorIndexPlan,
			*plans.RecordQueryStreamingAggregationPlan,
			// A UNION (ALL / recursive-CTE level) emits a FLAT positional
			// row per leg, re-typed to the union's output column names
			// (remapUnionColumnsByPosition), so an aggregate over it resolves its
			// group key / operand against that row — the flat frontier.
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
		case *plans.RecordQueryProjectionPlan:
			input = p.GetInner()
		case *plans.RecordQueryMapPlan:
			input = p.GetInner()
		case *plans.RecordQueryFlatMapPlan:
			if qov, ok := p.GetResultValue().(*values.QuantifiedObjectValue); ok && qov.Correlation == p.GetOuterAlias() {
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
func (c *aggregateCursor) withPartialState(groupKey string, keyVals []any, gs *groupState) {
	c.currentGroupKey = groupKey
	c.currentKeyVals = keyVals
	c.current = gs
}

func (c *aggregateCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.closed {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
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
				c.currentGroupKey, c.currentKeyVals, c.current,
				c.aggregates,
			)
			if encErr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, encErr
			}
			return recordlayer.NewResultNoNext[QueryResult](
				reason, recordlayer.NewBytesContinuation(contBytes),
			), nil
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
			// completed group.
			if err := c.accumulateRow(row); err != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, err
			}
			return recordlayer.NewResultWithValue(completed, nonEndContinuation), nil
		}

		if err := c.accumulateRow(row); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
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
		// No rows at all.
		if c.scalarMode {
			// Scalar aggregation on empty input: COUNT(*)=0, SUM=nil, etc.
			result := c.emptyScalarResult()
			return recordlayer.NewResultWithValue(result, nonEndContinuation), nil
		}
		return recordlayer.NewResultNoNext[QueryResult](
			recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
		), nil
	}

	completed := c.finalizeGroup()
	return recordlayer.NewResultWithValue(completed, nonEndContinuation), nil
}

// aggregateEvalArg is the eval argument for a GROUP-BY key / aggregate operand.
//
// The streaming aggregate reads its keys / operands off the inner row exactly
// the way executeFilter / executeProjection read their predicate / projected
// values — the ONE frontier dispatch, so the aggregate input resolves
// positionally on the same shapes those do.
//
//   - A POSITIONALLY-BAKED value (a FieldValue with a resolved ordinal — the
//     gathered-seed un-collapse's qualifier-honoring group key/operand) reads the
//     flat seed row it was baked against: a bare positional context, no
//     leg re-windowing (the gathered seed emits a flat row and its bakes are flat
//     slots).
//   - On a FLAT single-source frontier (flatFrontierInput — a scan / index / filter /
//     projection / derived table / CTE / DISTINCT / semi-join / nested StreamingAgg
//     whose chain bottoms out at a single source, NOT a 2-way ordinal JOIN merge), a
//     name group-key / operand resolves against the inner PositionalRow by its baked
//     plan-time ordinal (frontierRowContext → evaluateOrdinal). This is robust to a
//     covering-index column layout (the bake binds the emitted layout, never a raw
//     table ordinal).
//   - A RAW 2-way ordinal JOIN merge (joinWindowsOK — the input flows the join's
//     MERGED positional row through passthroughs only): a qualified group-key /
//     operand reference QOV(leg).col (or its flat DOTTED "D.DNAME" spelling)
//     resolves LEG-LOCALLY through its window, exactly as
//     executeProjection / executeFilter resolve theirs over the same merge.
//     Unconditional on a windowed input: even with no param/subquery/outer binding,
//     the leg windows are required (the bare merged row misreads leg-relative
//     ordinals — a wrong-slot hazard).
//   - A row with NO Positional yields NULL: every live input flows an
//     ordinal PositionalRow; there is no name-map to read.
func (c *aggregateCursor) aggregateEvalArg(v values.Value, row QueryResult) any {
	if row.Positional == nil {
		return nil
	}
	if valueReadsBakedOrdinal(v) {
		// A baked ordinal operand reads plan-time-resolved slots directly off the
		// positional row (evaluateOrdinal), so pass it as the bare ordinal row.
		return &values.RowEvalContext{Positional: row.Positional}
	}
	if c.flatFrontierInput {
		return frontierRowContext(row.Positional, c.evalCtx, c.needsRowCtx)
	}
	if c.joinWindowsOK {
		// A RAW 2-way ordinal JOIN merge: a qualified group-key / operand
		// QOV(leg).col resolves LEG-LOCALLY through its window.
		return legWindowRowContext(row.Positional, c.evalCtx, c.joinLegSpans)
	}
	// The general positional-row consumer: a flat output-named positional
	// (projecting derived source, recursive-CTE / bare-projected-join row, or a
	// constant operand) resolves by its baked plan-time ordinal — authoritative.
	return frontierRowContext(row.Positional, c.evalCtx, c.needsRowCtx)
}

// valueReadsBakedOrdinal reports whether v (a group key / aggregate operand) reads
// through a plan-time-resolved ordinal ANYWHERE in its value tree — the un-collapse's
// positionally-baked FieldValue, possibly nested inside a compound operand
// (`SUM(A.K+B.K)`). Any baked leaf means the whole value must evaluate against the
// positional row; a value with no baked leaf resolves through the frontier dispatch.
func valueReadsBakedOrdinal(v values.Value) bool {
	// FrontierPinned specifically — the ordinal-frontier bake the group-by makes
	// (NewFieldValueOfOrdinal). An UNPINNED baked node (a recursive-CTE leg projection
	// column) is never an aggregate key/operand — matching intent, not over-broad on
	// `Resolved != nil`.
	if fv, ok := v.(*values.FieldValue); ok && fv.Resolved != nil && fv.Resolved.FrontierPinned {
		return true
	}
	for _, c := range v.Children() {
		if valueReadsBakedOrdinal(c) {
			return true
		}
	}
	return false
}

func (c *aggregateCursor) computeGroupKey(row QueryResult) (string, []any, error) {
	if c.scalarMode {
		return "", nil, nil
	}
	keyParts := make([]any, len(c.groupingKeys))
	t := make(tuple.Tuple, len(c.groupingKeys))
	for i, k := range c.groupingKeys {
		v, err := k.Evaluate(c.aggregateEvalArg(k, row))
		if err != nil {
			return "", nil, err
		}
		keyParts[i] = v
		// tuple.Pack handles nil, int64, float64, string, []byte, bool natively.
		// For types the tuple layer doesn't list here (e.g. a UUID [16]byte),
		// fall back to a deterministic %T:%v string so we still get a stable key.
		//
		// The packed key rides through the aggregate continuation VERBATIM
		// (encodeAggGroupKey stores it as raw bytes, not a JSON string) and is
		// compared byte-for-byte on resume to detect a group change, so any
		// deterministic packing is safe. The group's surfaced VALUE rides
		// separately in keyVals (typed, lossless), so a UUID key still materializes
		// as its [16]byte. A UUID could now pack natively since the raw bytes
		// survive; the %T:%v form is retained only to keep the key bytes stable.
		switch tv := v.(type) {
		case nil, int64, float64, string, []byte, bool:
			t[i] = tv
		default:
			t[i] = fmt.Sprintf("%T:%v", v, v)
		}
	}
	return string(t.Pack()), keyParts, nil
}

func (c *aggregateCursor) newGroupState() *groupState {
	allIntInit := make([]bool, len(c.aggregates))
	for j := range allIntInit {
		allIntInit[j] = true
	}
	return &groupState{
		keyVals: c.currentKeyVals,
		counts:  make([]int64, len(c.aggregates)),
		sums:    make([]float64, len(c.aggregates)),
		sumsI:   make([]int64, len(c.aggregates)),
		allInt:  allIntInit,
		mins:    make([]any, len(c.aggregates)),
		maxs:    make([]any, len(c.aggregates)),
	}
}

func (c *aggregateCursor) accumulateRow(row QueryResult) error {
	gs := c.current
	gs.count++

	for i, agg := range c.aggregates {
		val, err := agg.Operand.Evaluate(c.aggregateEvalArg(agg.Operand, row))
		if err != nil {
			return err
		}
		if val == nil {
			continue
		}
		gs.counts[i]++
		switch agg.Function {
		case expressions.AggSum, expressions.AggAvg:
			if !isNumeric(val) {
				return fmt.Errorf("cannot aggregate non-numeric value of type %T", val)
			}
			num := toFloat64(val)
			gs.sums[i] += num
			if intVal, ok := val.(int64); ok {
				s := gs.sumsI[i] + intVal
				if agg.OperandIntType == values.TypeCodeInt {
					// INT operand (SQL INTEGER, proto TYPE_INT32): Java SUM_I /
					// AVG_I sum the int component with Math.addExact((int)…),
					// overflowing at the int32 boundary and surfacing as
					// numeric-out-of-range (22003). Go widens the INTEGER value to
					// int64, so the int32-bounded running sum leaving that range is
					// the SAME overflow Java raises — check it here, not only at the
					// int64 boundary below.
					if s > math.MaxInt32 || s < math.MinInt32 {
						return &SumOverflowError{}
					}
				} else if (gs.sumsI[i]^intVal) >= 0 && (gs.sumsI[i]^s) < 0 {
					// LONG operand (SUM_L / AVG_L): Math.addExact on long — int64
					// overflow.
					return &SumOverflowError{}
				}
				gs.sumsI[i] = s
			} else {
				gs.allInt[i] = false
			}
		case expressions.AggMin, expressions.AggMax:
			if !isNumeric(val) {
				return &AggregateTypeMismatchError{
					Message: "unable to encapsulate aggregate operation due to type mismatch(es)",
				}
			}
			if gs.mins[i] == nil || compareAny(val, gs.mins[i]) < 0 {
				gs.mins[i] = val
			}
			if gs.maxs[i] == nil || compareAny(val, gs.maxs[i]) > 0 {
				gs.maxs[i] = val
			}
		}
	}
	return nil
}

func (c *aggregateCursor) finalizeGroup() QueryResult {
	gs := c.current
	// Build the OUTPUT ORDINAL ROW. Order + naming MUST match
	// streamingAggOutputNames (the plan's authoritative output schema the planner
	// baked downstream ordinals against): grouping keys in order (aggKeyName), then
	// aggregates in order (ALIAS-preferring, else aggResultName). A downstream ref
	// baked to this schema reads by Get(ord) — position is the authority, matched
	// to these exact names at plan time.
	posNames := make([]string, 0, len(c.groupingKeys)+len(c.aggregates))
	posSlots := make([]any, 0, len(c.groupingKeys)+len(c.aggregates))
	for i, k := range c.groupingKeys {
		posNames = append(posNames, aggKeyName(k))
		posSlots = append(posSlots, gs.keyVals[i])
	}
	for i, agg := range c.aggregates {
		name := aggResultName(agg)
		var val any
		switch agg.Function {
		case expressions.AggCount:
			if expressions.IsCountStar(agg) {
				val = gs.count
			} else {
				val = gs.counts[i]
			}
		case expressions.AggSum:
			if gs.counts[i] == 0 {
				val = nil
			} else if gs.allInt[i] {
				val = gs.sumsI[i]
			} else {
				val = gs.sums[i]
			}
		case expressions.AggMin:
			val = gs.mins[i]
		case expressions.AggMax:
			val = gs.maxs[i]
		case expressions.AggAvg:
			if gs.counts[i] > 0 {
				if gs.allInt[i] {
					// All-integer operands: divide the EXACT int64 running sum
					// (converted to double once), not the incrementally-added
					// float64 sum, which loses low-order bits past 2^53. Mirrors
					// Java AverageAccumulatorState.longAverageState (LongState
					// SUM via Math.addExact, finish = total.doubleValue()/count)
					// and Cascades AVG_L ((double)longSum / count).
					val = float64(gs.sumsI[i]) / float64(gs.counts[i])
				} else {
					val = gs.sums[i] / float64(gs.counts[i])
				}
			} else {
				val = nil
			}
		}
		// Alias-preferring output name (matches streamingAggOutputNames), so a downstream ref
		// over an ALIASED aggregate (SUM(a*b) AS foo, a derived-table projection) resolves.
		posName := name
		if agg.Alias != "" {
			posName = strings.ToUpper(agg.Alias)
		}
		posNames = append(posNames, posName)
		posSlots = append(posSlots, val)
	}
	return QueryResult{
		Positional: &PositionalRow{Type: positionalTypeFromNames(posNames), Slots: posSlots},
	}
}

func (c *aggregateCursor) emptyScalarResult() QueryResult {
	// Emit the authoritative ordinal row, SAME order + naming as
	// finalizeGroup's aggregate arm (there are no grouping keys on the empty-scalar
	// path — an empty GROUP BY yields zero rows, not this single all-groups row).
	posNames := make([]string, 0, len(c.aggregates))
	posSlots := make([]any, 0, len(c.aggregates))
	for _, agg := range c.aggregates {
		name := aggResultName(agg)
		var val any
		if agg.Function == expressions.AggCount {
			val = int64(0)
		}
		posName := name
		if agg.Alias != "" {
			posName = strings.ToUpper(agg.Alias)
		}
		posNames = append(posNames, posName)
		posSlots = append(posSlots, val)
	}
	return QueryResult{
		Positional: &PositionalRow{Type: positionalTypeFromNames(posNames), Slots: posSlots},
	}
}

func (c *aggregateCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *aggregateCursor) IsClosed() bool { return c.closed }

// ---------------------------------------------------------------------------
// customSortCursor — streaming sort with pluggable comparator
// ---------------------------------------------------------------------------

// customSortCursor implements RecordCursor[QueryResult] for ORDER BY.
// Two phases: LOAD (pull from inner into buffer) and EMIT (return
// sorted records one-by-one). When the inner cursor hits a limit
// during LOAD, the buffer and limit are propagated upward via the sort
// continuation so the next transaction can continue loading into the
// same buffer. Mirrors Java's MemorySortCursor; the comparator is a
// pluggable sortFn evaluating the plan's sort-key Values against each
// row's positional row.

type customSortCursor struct {
	inner  recordlayer.RecordCursor[QueryResult]
	sortFn func([]QueryResult) error

	buf     []QueryResult
	loaded  bool
	emitIdx int
	closed  bool
	maxBuf  int                       // 0 = use DefaultMaxSortBufferRows
	st      *recordlayer.ExecuteState // RFC-130 statement memory budget
}

// DefaultMaxSortBufferRows is the maximum number of rows the in-memory
// sort cursor will materialize before returning an error. Prevents OOM
// on queries that sort unbounded result sets without LIMIT. Override per
// cursor via the maxBuf field.
const DefaultMaxSortBufferRows = 5_000_000

func newCustomSortCursor(
	inner recordlayer.RecordCursor[QueryResult],
	sortFn func([]QueryResult) error,
	st *recordlayer.ExecuteState,
) *customSortCursor {
	return &customSortCursor{
		inner:  inner,
		sortFn: sortFn,
		st:     st,
	}
}

func (c *customSortCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.closed {
		return recordlayer.RecordCursorResult[QueryResult]{}, fmt.Errorf("cursor is closed")
	}
	if c.loaded {
		return c.emitNext()
	}
	limit := c.maxBuf
	if limit <= 0 {
		limit = DefaultMaxSortBufferRows
	}
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
			if reason == recordlayer.SourceExhausted {
				if sortErr := c.sortFn(c.buf); sortErr != nil {
					return recordlayer.RecordCursorResult[QueryResult]{}, sortErr
				}
				c.loaded = true
				return c.emitNext()
			}
			contBytes, encErr := encodeSortContinuation(
				result.GetContinuation(), c.buf,
			)
			if encErr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, encErr
			}
			return recordlayer.NewResultNoNext[QueryResult](
				reason, recordlayer.NewBytesContinuation(contBytes),
			), nil
		}
		v := result.GetValue()
		// RFC-130: charge each row's bytes against the statement memory budget
		// before keeping it in the sort buffer.
		if c.st.HasMemLimit() {
			if err := c.st.ChargeMemory(estimateQueryResultBytes(v)); err != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, err
			}
		}
		c.buf = append(c.buf, v)
		if len(c.buf) >= limit {
			return recordlayer.RecordCursorResult[QueryResult]{}, &SortBufferExceededError{
				Rows:  len(c.buf),
				Limit: limit,
			}
		}
	}
}

func (c *customSortCursor) emitNext() (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.emitIdx >= len(c.buf) {
		return recordlayer.NewResultNoNext[QueryResult](
			recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
		), nil
	}
	row := c.buf[c.emitIdx]
	c.emitIdx++
	return recordlayer.NewResultWithValue(row, nonEndContinuation), nil
}

func (c *customSortCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *customSortCursor) IsClosed() bool { return c.closed }

// ---------------------------------------------------------------------------
// nljCursor — streaming nested-loop join
// ---------------------------------------------------------------------------

// nljCursor implements RecordCursor[QueryResult] for nested-loop joins.
// Loads the inner side once, then streams outer rows one-by-one.
type nljCursor struct {
	outerInner recordlayer.RecordCursor[QueryResult]
	innerRows  []QueryResult
	joinType   plans.JoinType
	outerAlias string
	innerAlias string
	preds      []predicates.QueryPredicate
	evalCtx    *EvaluationContext

	// hashIndex is a hash index on inner rows keyed by the equijoin
	// inner operand's evaluated value. When non-nil, inner row lookup is
	// O(1) per outer row instead of O(N). Built by newNLJCursor when it
	// detects a single-equality equijoin over two baked leg references.
	hashIndex    map[any][]int // join-key → indices into innerRows
	hashOuterVal values.Value  // outer-side baked operand, probes the index
	allIdx       []int         // lazy 0..N-1 candidate list for a failed probe

	currentOuter   *QueryResult
	innerIdx       int
	innerMatches   []int // hash-matched inner row indices for current outer
	outerMatched   bool
	outerExhausted bool
	closed         bool

	// FULL OUTER JOIN drain state. matchedInner[i] is set when
	// innerRows[i] passes the join predicates against any outer row;
	// after the outer side is exhausted, the drain phase emits every
	// unmatched inner row NULL-padded on the left (symmetric to the
	// LEFT-OUTER unmatched-outer emission). Allocated only for
	// JoinFullOuter — other join types pay nothing. This state lives only
	// in memory and is NOT serialized into the continuation, so the
	// FULL-outer NLJ must complete within a single transaction;
	// executeNestedLoopJoin clears the outer's time/row limits for FULL to
	// guarantee that (see the comment there).
	matchedInner []bool
	drainIdx     int

	// st is the statement memory budget (RFC-130). buildErr captures a budget
	// breach during hash-index construction (newNLJCursor cannot return an
	// error) and is surfaced on the first OnNext, so the join fails fast rather
	// than silently dropping the index.
	st       *recordlayer.ExecuteState
	buildErr error

	// build is the join's ordinal-build state (an *ordinalJoinBuild), probed
	// ONCE at construction from the plan's result value (nil = un-gated merge:
	// the emitted row is mergeRows' leg-concat positional row). When enabled,
	// every emitted row carries the positional merged row evaluated from the RC
	// with per-leg bindings. Gates ALL ordinal-build work below via
	// build.enabled().
	build *ordinalJoinBuild
	// innerAdapted is the FIXED inner-rows slice adapted once at construction
	// (parallel to innerRows); outerAdapted is the current outer row adapted
	// once per outer-row advance. Together they make the per-candidate-pair
	// cost one small twoLegBinder — no re-adaptation, no map (per-pair adapter
	// work would be a structural O(rows²) perf regression).
	innerAdapted []values.OrdinalRow
	outerAdapted values.OrdinalRow
	// outerCorr/innerCorr are the leg correlation identifiers, resolved once.
	outerCorr, innerCorr values.CorrelationIdentifier
}

func newNLJCursor(
	outer recordlayer.RecordCursor[QueryResult],
	innerRows []QueryResult,
	joinType plans.JoinType,
	outerAlias, innerAlias string,
	preds []predicates.QueryPredicate,
	resultValue values.Value,
	evalCtx *EvaluationContext,
	st *recordlayer.ExecuteState,
) (*nljCursor, error) {
	build, err := newOrdinalJoinBuild(resultValue, preds)
	if err != nil {
		return nil, err
	}
	c := &nljCursor{
		outerInner: outer,
		innerRows:  innerRows,
		joinType:   joinType,
		outerAlias: outerAlias,
		innerAlias: innerAlias,
		preds:      preds,
		evalCtx:    evalCtx,
		st:         st,
		build:      build,
	}
	c.outerCorr = values.NamedCorrelationIdentifier(outerAlias)
	c.innerCorr = values.NamedCorrelationIdentifier(innerAlias)
	if build.enabled() {
		// Adapt the FIXED inner side once — never per pair, which would turn
		// every candidate-pair comparison into a re-adaptation and make the
		// join O(rows²).
		innerType := build.legType(c.innerCorr)
		c.innerAdapted = make([]values.OrdinalRow, len(innerRows))
		for i := range innerRows {
			row, aerr := adaptLegPositional(innerRows[i], innerType)
			if aerr != nil {
				return nil, aerr
			}
			c.innerAdapted[i] = row
		}
	}
	if joinType == plans.JoinFullOuter {
		c.matchedInner = make([]bool, len(innerRows))
	}
	c.tryBuildHashIndex(innerAlias)
	return c, nil
}

// pairBinder builds the per-candidate-pair leg binder from PRE-adapted leg
// rows. A nil OrdinalRow is the deliberately-NULL leg (LEFT/FULL padding —
// the binder returns (nil, true): a leg bound to nil evaluates as NULL, never
// an error). Only called when build is enabled.
func (c *nljCursor) pairBinder(outer, inner values.OrdinalRow) *twoLegBinder {
	return &twoLegBinder{
		outerID: c.outerCorr, innerID: c.innerCorr,
		outer: outer, inner: inner,
		base: correlationBase(c.evalCtx),
	}
}

// adaptOuter adapts a just-advanced outer row ONCE (shared by every candidate
// pair and the unmatched-outer emission for this outer row). No-op for
// un-gated (build-disabled) cursors.
func (c *nljCursor) adaptOuter(outerRow QueryResult) error {
	if !c.build.enabled() {
		return nil
	}
	row, err := adaptLegPositional(outerRow, c.build.legType(c.outerCorr))
	if err != nil {
		return err
	}
	c.outerAdapted = row
	return nil
}

// tryBuildHashIndex attempts to build a hash index on the inner rows
// for equijoin predicates. If exactly one predicate is an equality
// comparison between an outer-leg and an inner-leg column reference —
// both PLAN-TIME BAKED (source-relative ordinal over the leg's
// QuantifiedObjectValue) — builds a hash map keyed by
// the inner operand's evaluated value. Keys are extracted by EVALUATING
// the operand against its leg row alone (evalLegKey) — the exact
// resolution the linear predicate path performs per pair — and then
// CANONICALIZED (normalizeNLJHashKey) so map-key equality agrees with
// cmpAny's promoting comparison, so the hash pre-filter can never
// disagree with the predicate re-check the way an independent name
// lookup or a raw-dynamic-type key could (a divergence there silently
// DROPS matching rows, since the hash only pre-filters candidates).
func (c *nljCursor) tryBuildHashIndex(innerAlias string) {
	if len(c.preds) == 0 || len(c.innerRows) < 100 {
		return
	}
	outerVal, innerVal := extractEquijoinOperands(c.preds, c.outerAlias, innerAlias)
	if outerVal == nil || innerVal == nil {
		return
	}
	idx := make(map[any][]int, len(c.innerRows))
	for i, row := range c.innerRows {
		if row.Positional == nil {
			return
		}
		val, ok := evalLegHashKey(innerVal, c.innerCorr, c.innerLegRow(i))
		if !ok {
			// An operand that cannot resolve against the leg row — or whose
			// key has no hashable promotion-stable canonical form ([]byte,
			// message shapes, time.Time, NaN) — declines the fast path
			// entirely: the linear path evaluates (and loud-errors) the same
			// predicate per pair via cmpAny, which handles all of them.
			return
		}
		// RFC-130: the hash index is additional resident memory beyond the
		// already-charged innerRows — one int per inner row plus, for a new
		// key, the key's own bytes. Charge it; a budget breach is captured in
		// buildErr and surfaced on the first OnNext (this constructor cannot
		// return an error).
		var charge int64 = nljHashEntryBytes
		if _, existing := idx[val]; !existing {
			charge += scalarValueBytes(val)
		}
		if err := c.st.ChargeMemory(charge); err != nil {
			c.buildErr = err
			c.hashIndex = nil
			return
		}
		idx[val] = append(idx[val], i)
	}
	c.hashIndex = idx
	c.hashOuterVal = outerVal
}

// innerLegRow returns inner row i as the leg-local OrdinalRow the equijoin
// operand evaluates against: the pre-adapted leg row for an ordinal-build
// cursor, else the row's own positional (the un-gated merge concatenates leg
// rows verbatim, so a leg window over the merged row reads the same slots).
func (c *nljCursor) innerLegRow(i int) values.OrdinalRow {
	if c.innerAdapted != nil {
		return c.innerAdapted[i]
	}
	return c.innerRows[i].Positional
}

// allInnerIndices is the degraded hash-probe candidate list: every inner row.
// Built lazily, once.
func (c *nljCursor) allInnerIndices() []int {
	if c.allIdx == nil {
		c.allIdx = make([]int, len(c.innerRows))
		for i := range c.allIdx {
			c.allIdx[i] = i
		}
	}
	return c.allIdx
}

// oneLegBinder binds exactly one leg correlation to its leg-local row — the
// single-sided twin of twoLegBinder for hash-key extraction, where only the
// key operand's own leg is in scope.
type oneLegBinder struct {
	id  values.CorrelationIdentifier
	leg values.OrdinalRow
}

func (b oneLegBinder) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	if id == b.id {
		return b.leg, true
	}
	return nil, false
}

// evalLegKey evaluates a baked equijoin operand against its leg row alone.
// ok=false (never an error) on any resolution failure — the caller declines
// the hash fast path and the linear predicate path surfaces the failure.
func evalLegKey(val values.Value, corr values.CorrelationIdentifier, leg values.OrdinalRow) (any, bool) {
	if leg == nil {
		return nil, false
	}
	v, err := val.Evaluate(&values.RowEvalContext{Correlations: oneLegBinder{id: corr, leg: leg}})
	if err != nil {
		return nil, false
	}
	return v, true
}

// evalLegHashKey evaluates a baked equijoin operand against its leg row and
// canonicalizes the result into the NLJ hash-key form. The ONLY entry point
// for both the index build and the probe — the two sides MUST normalize
// identically or matching rows silently vanish. ok=false on resolution
// failure or a key outside normalizeNLJHashKey's whitelist: the builder then
// declines the fast path entirely and the probe degrades to the full
// candidate list, leaving the linear cmpAny evaluation as the semantics of
// record.
func evalLegHashKey(val values.Value, corr values.CorrelationIdentifier, leg values.OrdinalRow) (any, bool) {
	v, ok := evalLegKey(val, corr, leg)
	if !ok {
		return nil, false
	}
	return normalizeNLJHashKey(v)
}

// normalizeNLJHashKey canonicalizes an equijoin key so Go map-key equality on
// the result agrees with cmpAny — the promoting comparison the predicate
// re-check applies to every surviving candidate pair. The invariant: two keys
// the linear path treats as EQUAL must normalize to the SAME map key (a
// false negative silently drops matching rows); extra bucket collisions are
// harmless — the per-pair re-check filters them.
//
//   - Every numeric promotes to float64, mirroring cmpAny's
//     promoteInt/promoteFloat widening: integral pairs equal as int64 are
//     equal as float64, and any-float pairs compare as float64 already.
//     Distinct wide int64s that collide at float64 precision are false
//     POSITIVES only. Without this, int64(5) missed the float64(5) bucket
//     (BIGINT=DOUBLE dropped every match ≥100 inner rows) and a
//     covering-index float32 leg missed a stored-record float64 bucket.
//   - NaN declines: cmpAny's <,>-based equality makes NaN compare EQUAL to
//     every float, while a NaN map key matches nothing — no bucket can
//     represent it.
//   - string and bool pass through: cmpAny's arms for them are exact
//     equality, same as map ==.
//   - [16]byte (UUID) passes through: array == is exactly cmpAny's
//     bytes.Compare(av[:], bv[:]) == 0.
//   - nil (NULL) passes through: a NULL bucket only produces candidates the
//     re-check rejects (equality on NULL is UNKNOWN), and declining instead
//     would nuke the fast path for any leg with one NULL key.
//   - Everything else declines. []byte and message/list shapes are
//     unhashable map keys (a []byte key PANICKED the build at exactly 100
//     inner rows: "hash of unhashable type: []uint8"). time.Time is hashable
//     but its map == is wall+monotonic+location identity, not the instant
//     equality cmpAny uses — and strings cross-match time.Time via
//     ParseTimestamp, which no string-keyed bucket can represent.
func normalizeNLJHashKey(v any) (any, bool) {
	if v == nil {
		return nil, true
	}
	if f, _, numeric := values.ToFloat64(v); numeric {
		if math.IsNaN(f) {
			return nil, false
		}
		return f, true
	}
	switch v.(type) {
	case string, bool, [16]byte:
		return v, true
	}
	return nil, false
}

// nljHashEntryBytes is the per-inner-row cost charged for the NLJ hash index:
// the int slot in the bucket slice plus amortized map/slice-header overhead.
const nljHashEntryBytes int64 = 24

// extractEquijoinOperands extracts the outer- and inner-side operand VALUES
// from a single-equality equijoin predicate. Each operand must be a PLAN-TIME
// BAKED leg column reference — a single-accessor FieldValue over its leg's
// QuantifiedObjectValue (a source-relative bind) — whose
// correlation names one of the two legs. Anything else (a fused multi-accessor
// rebase, a computed operand, a reference to a buried leg of a lower join)
// declines the fast path: (nil, nil), the linear predicate path handles it.
func extractEquijoinOperands(preds []predicates.QueryPredicate, outerAlias, innerAlias string) (outerVal, innerVal values.Value) {
	var o, in values.Value
	eqCount := 0
	for _, p := range preds {
		cp, ok := p.(*predicates.ComparisonPredicate)
		if !ok {
			continue
		}
		if cp.Comparison.Type != predicates.ComparisonEquals {
			continue
		}
		lIsOuter, lOK := bakedLegOperand(cp.Operand, outerAlias, innerAlias)
		rIsOuter, rOK := bakedLegOperand(cp.Comparison.Operand, outerAlias, innerAlias)
		if !lOK || !rOK {
			continue
		}
		switch {
		case lIsOuter && !rIsOuter:
			o, in = cp.Operand, cp.Comparison.Operand
			eqCount++
		case !lIsOuter && rIsOuter:
			o, in = cp.Comparison.Operand, cp.Operand
			eqCount++
		}
	}
	if eqCount != 1 {
		return nil, nil
	}
	return o, in
}

// bakedLegOperand reports whether v is a baked SINGLE-accessor column
// reference over one of the join's two legs, and which side it names
// (isOuter). Pinned (seed-rebased) and source-relative bakes both qualify —
// either resolves by its leg-local ordinal against the leg row. A FUSED
// multi-accessor path declines: its root ordinal addresses a merge-shaped
// intermediate, not the leg row. ok=false for every other shape.
func bakedLegOperand(v values.Value, outerAlias, innerAlias string) (isOuter, ok bool) {
	fv, isFV := v.(*values.FieldValue)
	if !isFV || fv.Resolved == nil || len(fv.Resolved.Accessors) != 1 {
		return false, false
	}
	qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
	if !isQOV {
		return false, false
	}
	alias := qov.Correlation.Name()
	switch {
	case strings.EqualFold(alias, outerAlias):
		return true, true
	case strings.EqualFold(alias, innerAlias):
		return false, true
	}
	return false, false
}

func (c *nljCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.closed {
		return recordlayer.RecordCursorResult[QueryResult]{}, fmt.Errorf("cursor is closed")
	}
	// RFC-130: a memory-budget breach during hash-index construction is
	// surfaced here on the first pull (newNLJCursor cannot return an error).
	if c.buildErr != nil {
		err := c.buildErr
		c.buildErr = nil
		return recordlayer.RecordCursorResult[QueryResult]{}, err
	}

	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		if c.currentOuter == nil {
			if c.outerExhausted {
				// FULL OUTER drain: after the outer side is exhausted,
				// emit every inner row that matched no outer row,
				// NULL-padded on the left (outer columns absent →
				// resolve to NULL downstream). Symmetric to the
				// LEFT-OUTER unmatched-outer emission below.
				if c.joinType == plans.JoinFullOuter {
					for c.drainIdx < len(c.innerRows) {
						if err := ctx.Err(); err != nil {
							return recordlayer.RecordCursorResult[QueryResult]{}, err
						}
						i := c.drainIdx
						c.drainIdx++
						if c.matchedInner[i] {
							continue
						}
						// FULL drain: an unmatched INNER row, the OUTER leg is the
						// NULL leg. The ordinal null-pad row comes from the build RC
						// (evaluateBound with a nil outer leg — QOV(outer)→nil, the
						// outer slots fall out NULL) when active; otherwise the
						// inner's own positional qualified under its alias so a
						// downstream qualified ref to the inner side resolves, and
						// the absent outer side reads NULL.
						qr := QueryResult{Record: c.innerRows[i].Record, PrimaryKey: c.innerRows[i].PrimaryKey}
						qr.Positional = qualifyOuterPositional(c.innerRows[i].Positional, c.innerAlias)
						if c.build.enabled() {
							pos, berr := c.build.evaluateBound(c.pairBinder(nil, c.innerAdapted[i]))
							if berr != nil {
								return recordlayer.RecordCursorResult[QueryResult]{}, berr
							}
							qr.Positional = pos
						}
						return recordlayer.NewResultWithValue(qr, nonEndContinuation), nil
					}
				}
				return recordlayer.NewResultNoNext[QueryResult](
					recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
				), nil
			}
			result, err := c.outerInner.OnNext(ctx)
			if err != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, err
			}
			if !result.HasNext() {
				reason := result.GetNoNextReason()
				if reason == recordlayer.SourceExhausted {
					// Loop back to the top so the FULL OUTER drain
					// (above) runs before reporting exhaustion.
					c.outerExhausted = true
					continue
				}
				return recordlayer.NewResultNoNext[QueryResult](
					reason, result.GetContinuation(),
				), nil
			}
			outerRow := result.GetValue()
			c.currentOuter = &outerRow
			c.innerIdx = 0
			c.innerMatches = nil
			c.outerMatched = false
			// Adapt the new outer leg ONCE for all its candidate
			// pairs (no-op for un-gated cursors).
			if err := c.adaptOuter(outerRow); err != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, err
			}

			// Hash probe: resolve inner row candidates for this outer row by
			// evaluating the OUTER operand against the outer leg row — the
			// same resolution the predicate re-check performs per pair.
			if c.hashIndex != nil && outerRow.Positional != nil {
				outerLeg := c.outerAdapted
				if outerLeg == nil {
					outerLeg = outerRow.Positional
				}
				if key, ok := evalLegHashKey(c.hashOuterVal, c.outerCorr, outerLeg); ok {
					c.innerMatches = c.hashIndex[key]
				} else {
					// A probe that cannot resolve — or whose key has no
					// hashable promotion-stable canonical form (time.Time,
					// NaN, a cross-typed []byte) — degrades this outer row to
					// the full candidate list: the per-pair predicate
					// evaluation is the semantics of record and will surface
					// any real failure (or match, e.g. string-typed inner
					// keys against a time.Time probe via ParseTimestamp).
					c.innerMatches = c.allInnerIndices()
				}
			}
		}

		if c.hashIndex != nil {
			// Hash join path: iterate only matching inner rows.
			for c.innerIdx < len(c.innerMatches) {
				if err := ctx.Err(); err != nil {
					return recordlayer.RecordCursorResult[QueryResult]{}, err
				}
				idx := c.innerMatches[c.innerIdx]
				innerRow := c.innerRows[idx]
				c.innerIdx++
				combined := mergeRows(*c.currentOuter, innerRow, c.outerAlias, c.innerAlias)
				// For an ordinal-build cursor the predicate row
				// context carries the per-leg bindings from the PRE-adapted
				// legs (nil binder = today's path bit-identically); the same
				// binder then builds the positional row on a pass.
				var pair *twoLegBinder
				if c.build.enabled() {
					pair = c.pairBinder(c.outerAdapted, c.innerAdapted[idx])
				}
				passes, perr := passesJoinPredicatesLegs(combined, c.preds, c.evalCtx, pair)
				if perr != nil {
					return recordlayer.RecordCursorResult[QueryResult]{}, perr
				}
				if !passes {
					continue
				}
				if c.matchedInner != nil {
					c.matchedInner[idx] = true
				}
				c.outerMatched = true
				switch c.joinType {
				case plans.JoinInner, plans.JoinLeftOuter, plans.JoinCross, plans.JoinFullOuter:
					// A fused-top merge (a positional-merge RC) reshapes the merged legs into the
					// RC's OUTPUT columns; the build RC produces that ordinal output
					// directly (evaluateBound). A plain merge keeps mergeRows'
					// leg-concat positional row.
					if pair != nil {
						pos, berr := c.build.evaluateBound(pair)
						if berr != nil {
							return recordlayer.RecordCursorResult[QueryResult]{}, berr
						}
						combined.Positional = pos
					}
					return recordlayer.NewResultWithValue(combined, nonEndContinuation), nil
				}
				if c.currentOuter == nil {
					break
				}
			}
		} else {
			// Linear scan path (fallback).
			for c.innerIdx < len(c.innerRows) {
				if err := ctx.Err(); err != nil {
					return recordlayer.RecordCursorResult[QueryResult]{}, err
				}
				idx := c.innerIdx
				innerRow := c.innerRows[idx]
				c.innerIdx++

				combined := mergeRows(*c.currentOuter, innerRow, c.outerAlias, c.innerAlias)
				// Same ordinal-build dual emission as the hash
				// path above (nil binder = today's path bit-identically).
				var pair *twoLegBinder
				if c.build.enabled() {
					pair = c.pairBinder(c.outerAdapted, c.innerAdapted[idx])
				}
				passes, perr := passesJoinPredicatesLegs(combined, c.preds, c.evalCtx, pair)
				if perr != nil {
					return recordlayer.RecordCursorResult[QueryResult]{}, perr
				}
				if !passes {
					continue
				}
				if c.matchedInner != nil {
					c.matchedInner[idx] = true
				}
				c.outerMatched = true

				switch c.joinType {
				case plans.JoinInner, plans.JoinLeftOuter, plans.JoinCross, plans.JoinFullOuter:
					// See the hash path: a fused-top build reshapes to the RC's
					// output; a plain merge keeps mergeRows' leg-concat row.
					if pair != nil {
						pos, berr := c.build.evaluateBound(pair)
						if berr != nil {
							return recordlayer.RecordCursorResult[QueryResult]{}, berr
						}
						combined.Positional = pos
					}
					return recordlayer.NewResultWithValue(combined, nonEndContinuation), nil
				}
				if c.currentOuter == nil {
					break
				}
			}
		}

		if c.currentOuter == nil {
			continue
		}

		outerRow := *c.currentOuter
		matched := c.outerMatched
		c.currentOuter = nil

		if !matched {
			switch c.joinType {
			case plans.JoinLeftOuter, plans.JoinFullOuter:
				// Unmatched outer (LEFT/FULL): the INNER leg is the NULL leg. The
				// ordinal null-pad row comes from the build RC (evaluateBound with
				// a nil inner leg — QOV(inner)→nil, the inner slots fall out NULL,
				// never an error) when active; otherwise the outer's own
				// positional qualified under its alias, the absent inner side NULL.
				qr := QueryResult{Record: outerRow.Record, PrimaryKey: outerRow.PrimaryKey}
				qr.Positional = qualifyOuterPositional(outerRow.Positional, c.outerAlias)
				if c.build.enabled() {
					pos, berr := c.build.evaluateBound(c.pairBinder(c.outerAdapted, nil))
					if berr != nil {
						return recordlayer.RecordCursorResult[QueryResult]{}, berr
					}
					qr.Positional = pos
				}
				return recordlayer.NewResultWithValue(qr, nonEndContinuation), nil
			}
		}
	}
}

func (c *nljCursor) Close() error {
	c.closed = true
	return c.outerInner.Close()
}

func (c *nljCursor) IsClosed() bool { return c.closed }

var (
	_ recordlayer.RecordCursor[QueryResult] = (*aggregateCursor)(nil)
	_ recordlayer.RecordCursor[QueryResult] = (*nljCursor)(nil)
	_ recordlayer.RecordCursor[QueryResult] = (*customSortCursor)(nil)
)
