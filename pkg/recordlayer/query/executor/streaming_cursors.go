package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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

	// Current in-progress group state (streaming — only ONE group at a time).
	currentGroupKey string
	currentKeyVals  []any
	current         *groupState

	// Completed group waiting to be emitted (from the last group break).
	pending *QueryResult

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
) *aggregateCursor {
	return &aggregateCursor{
		inner:        inner,
		groupingKeys: groupingKeys,
		aggregates:   aggregates,
		scalarMode:   len(groupingKeys) == 0,
	}
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

	// If we have a pending completed group from a previous group break,
	// emit it now.
	if c.pending != nil {
		row := *c.pending
		c.pending = nil
		return recordlayer.NewResultWithValue(row, nonEndContinuation), nil
	}

	// If inner is exhausted, emit the final group (if any).
	if c.innerExhausted {
		return c.emitFinal()
	}

	// Pull records from inner, accumulate, detect group breaks.
	for {
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
		groupKey, keyVals := c.computeGroupKey(row)

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

func (c *aggregateCursor) computeGroupKey(row QueryResult) (string, []any) {
	if c.scalarMode {
		return "", nil
	}
	keyParts := make([]any, len(c.groupingKeys))
	t := make(tuple.Tuple, len(c.groupingKeys))
	for i, k := range c.groupingKeys {
		v := k.Evaluate(row.Datum)
		keyParts[i] = v
		// tuple.Pack handles nil, int64, float64, string, []byte natively.
		// For types the tuple layer doesn't support, fall back to the
		// string representation so we still get a deterministic key.
		switch tv := v.(type) {
		case nil, int64, float64, string, []byte, bool:
			t[i] = tv
		default:
			t[i] = fmt.Sprintf("%T:%v", v, v)
		}
	}
	return string(t.Pack()), keyParts
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
		val := agg.Operand.Evaluate(row.Datum)
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
				if (gs.sumsI[i]^intVal) >= 0 && (gs.sumsI[i]^s) < 0 {
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
	result := make(map[string]any)
	for i, k := range c.groupingKeys {
		name := aggKeyName(k)
		result[name] = gs.keyVals[i]
		if len(name) >= 2 && name[0] == '(' && name[len(name)-1] == ')' {
			stripped := name[1 : len(name)-1]
			if _, exists := result[stripped]; !exists {
				result[stripped] = gs.keyVals[i]
			}
		}
	}
	for i, agg := range c.aggregates {
		name := aggResultName(agg)
		var val any
		switch agg.Function {
		case expressions.AggCount:
			if isCountStar(agg) {
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
				val = gs.sums[i] / float64(gs.counts[i])
			} else {
				val = nil
			}
		}
		result[name] = val
		if agg.Alias != "" && agg.Alias != name {
			result[agg.Alias] = val
		}
	}
	return QueryResult{Datum: result}
}

func (c *aggregateCursor) emptyScalarResult() QueryResult {
	result := make(map[string]any)
	for _, agg := range c.aggregates {
		name := aggResultName(agg)
		var val any
		if agg.Function == expressions.AggCount {
			val = int64(0)
		}
		result[name] = val
		if agg.Alias != "" && agg.Alias != name {
			result[agg.Alias] = val
		}
	}
	return QueryResult{Datum: result}
}

func (c *aggregateCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *aggregateCursor) IsClosed() bool { return c.closed }

// ---------------------------------------------------------------------------
// memorySortCursor — streaming ORDER BY
// ---------------------------------------------------------------------------

// memorySortCursor implements RecordCursor[QueryResult] for ORDER BY.
// Two phases: LOAD (pull from inner into buffer) and EMIT (return
// sorted records one-by-one). When the inner cursor hits a limit
// during LOAD, the buffer and limit are propagated upward via
// MemorySortContinuation proto so the next transaction can continue
// loading into the same buffer.
//
// Mirrors Java's MemorySortCursor.
type memorySortCursor struct {
	inner recordlayer.RecordCursor[QueryResult]
	keys  []string
	dirs  []bool

	buf     []QueryResult
	loaded  bool
	emitIdx int
	closed  bool
}

func newMemorySortCursor(
	inner recordlayer.RecordCursor[QueryResult],
	keys []string,
	dirs []bool,
) *memorySortCursor {
	return &memorySortCursor{
		inner: inner,
		keys:  keys,
		dirs:  dirs,
	}
}

func (c *memorySortCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.closed {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
	}
	if c.loaded {
		return c.emitNext()
	}

	for {
		result, err := c.inner.OnNext(ctx)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		if !result.HasNext() {
			reason := result.GetNoNextReason()
			if reason == recordlayer.SourceExhausted {
				sortByKeys(c.buf, c.keys, c.dirs)
				c.loaded = true
				return c.emitNext()
			}
			contBytes, encErr := encodeSortContinuation(
				result.GetContinuation(), c.buf)
			if encErr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, encErr
			}
			return recordlayer.NewResultNoNext[QueryResult](
				reason, recordlayer.NewBytesContinuation(contBytes),
			), nil
		}
		c.buf = append(c.buf, result.GetValue())
	}
}

func (c *memorySortCursor) emitNext() (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.emitIdx >= len(c.buf) {
		return recordlayer.NewResultNoNext[QueryResult](
			recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
		), nil
	}
	row := c.buf[c.emitIdx]
	c.emitIdx++
	return recordlayer.NewResultWithValue(row, nonEndContinuation), nil
}

func (c *memorySortCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *memorySortCursor) IsClosed() bool { return c.closed }

// ---------------------------------------------------------------------------
// customSortCursor — streaming sort with pluggable comparator
// ---------------------------------------------------------------------------

type customSortCursor struct {
	inner  recordlayer.RecordCursor[QueryResult]
	sortFn func([]QueryResult)

	buf     []QueryResult
	loaded  bool
	emitIdx int
	closed  bool
	maxBuf  int // 0 = use DefaultMaxSortBufferRows
}

// DefaultMaxSortBufferRows is the maximum number of rows the in-memory
// sort cursor will materialize before returning an error. Prevents OOM
// on queries that sort unbounded result sets without LIMIT. Override per
// cursor via the maxBuf field.
const DefaultMaxSortBufferRows = 5_000_000

func newCustomSortCursor(
	inner recordlayer.RecordCursor[QueryResult],
	sortFn func([]QueryResult),
) *customSortCursor {
	return &customSortCursor{
		inner:  inner,
		sortFn: sortFn,
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
		result, err := c.inner.OnNext(ctx)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		if !result.HasNext() {
			reason := result.GetNoNextReason()
			if reason == recordlayer.SourceExhausted {
				c.sortFn(c.buf)
				c.loaded = true
				return c.emitNext()
			}
			contBytes, encErr := encodeSortContinuation(
				result.GetContinuation(), c.buf)
			if encErr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, encErr
			}
			return recordlayer.NewResultNoNext[QueryResult](
				reason, recordlayer.NewBytesContinuation(contBytes),
			), nil
		}
		c.buf = append(c.buf, result.GetValue())
		if len(c.buf) > limit {
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
	// column. When non-nil, inner row lookup is O(1) per outer row
	// instead of O(N). Built by newNLJCursor when it detects a
	// single-column equijoin predicate.
	hashIndex    map[any][]int // join-key → indices into innerRows
	hashJoinCol  string        // inner-side column name for hash lookup
	outerJoinCol string        // outer-side column name for hash lookup

	currentOuter   *QueryResult
	innerIdx       int
	innerMatches   []int // hash-matched inner row indices for current outer
	outerMatched   bool
	outerExhausted bool
	closed         bool
}

func newNLJCursor(
	outer recordlayer.RecordCursor[QueryResult],
	innerRows []QueryResult,
	joinType plans.JoinType,
	outerAlias, innerAlias string,
	preds []predicates.QueryPredicate,
	evalCtx *EvaluationContext,
) *nljCursor {
	c := &nljCursor{
		outerInner: outer,
		innerRows:  innerRows,
		joinType:   joinType,
		outerAlias: outerAlias,
		innerAlias: innerAlias,
		preds:      preds,
		evalCtx:    evalCtx,
	}
	c.tryBuildHashIndex(innerAlias)
	return c
}

// tryBuildHashIndex attempts to build a hash index on the inner rows
// for equijoin predicates. If exactly one predicate is an equality
// comparison between outer.col and inner.col, builds a hash map
// keyed by the inner column value.
func (c *nljCursor) tryBuildHashIndex(innerAlias string) {
	if len(c.preds) == 0 || len(c.innerRows) < 100 {
		return
	}
	outerCol, innerCol := extractEquijoinColumns(c.preds, c.outerAlias, innerAlias)
	if outerCol == "" || innerCol == "" {
		return
	}
	idx := make(map[any][]int, len(c.innerRows))
	for i, row := range c.innerRows {
		m, ok := row.Datum.(map[string]any)
		if !ok {
			return
		}
		val := lookupJoinKey(m, innerCol, innerAlias)
		idx[val] = append(idx[val], i)
	}
	c.hashIndex = idx
	c.hashJoinCol = innerCol
	c.outerJoinCol = outerCol
}

// extractEquijoinColumns extracts the outer and inner column names
// from a single-column equijoin predicate. Returns ("","") if the
// predicates don't match the pattern.
func extractEquijoinColumns(preds []predicates.QueryPredicate, outerAlias, innerAlias string) (string, string) {
	var outerCol, innerCol string
	eqCount := 0
	for _, p := range preds {
		cp, ok := p.(*predicates.ComparisonPredicate)
		if !ok {
			continue
		}
		if cp.Comparison.Type != predicates.ComparisonEquals {
			continue
		}
		lhs := fieldName(cp.Operand)
		rhs := fieldName(cp.Comparison.Operand)
		if lhs == "" || rhs == "" {
			continue
		}
		lhsTable, lhsCol := splitQualified(lhs)
		rhsTable, rhsCol := splitQualified(rhs)
		if matchesAlias(lhsTable, outerAlias) && matchesAlias(rhsTable, innerAlias) {
			outerCol = lhsCol
			innerCol = rhsCol
			eqCount++
		} else if matchesAlias(lhsTable, innerAlias) && matchesAlias(rhsTable, outerAlias) {
			outerCol = rhsCol
			innerCol = lhsCol
			eqCount++
		}
	}
	if eqCount != 1 {
		return "", ""
	}
	return outerCol, innerCol
}

func fieldName(v values.Value) string {
	if v == nil {
		return ""
	}
	if fv, ok := v.(*values.FieldValue); ok {
		return fv.Field
	}
	return ""
}

func splitQualified(name string) (table, col string) {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			return name[:i], name[i+1:]
		}
	}
	return "", name
}

func matchesAlias(table, alias string) bool {
	if table == "" {
		return true
	}
	return strings.EqualFold(table, alias)
}

func lookupJoinKey(m map[string]any, col, alias string) any {
	if v, ok := m[col]; ok {
		return v
	}
	qualified := alias + "." + col
	if v, ok := m[qualified]; ok {
		return v
	}
	for k, v := range m {
		_, c := splitQualified(k)
		if strings.EqualFold(c, col) {
			return v
		}
	}
	return nil
}

func (c *nljCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.closed {
		return recordlayer.RecordCursorResult[QueryResult]{}, fmt.Errorf("cursor is closed")
	}

	for {
		if c.currentOuter == nil {
			if c.outerExhausted {
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
					c.outerExhausted = true
					return recordlayer.NewResultNoNext[QueryResult](
						recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
					), nil
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

			// Hash probe: resolve inner row candidates for this outer row.
			if c.hashIndex != nil {
				outerMap, _ := outerRow.Datum.(map[string]any)
				if outerMap != nil {
					key := lookupJoinKey(outerMap, c.outerJoinCol, c.outerAlias)
					c.innerMatches = c.hashIndex[key]
				}
			}
		}

		if c.hashIndex != nil {
			// Hash join path: iterate only matching inner rows.
			for c.innerIdx < len(c.innerMatches) {
				innerRow := c.innerRows[c.innerMatches[c.innerIdx]]
				c.innerIdx++
				combined := mergeRows(*c.currentOuter, innerRow, c.outerAlias, c.innerAlias)
				if !passesJoinPredicates(combined, c.preds, c.evalCtx) {
					continue
				}
				c.outerMatched = true
				switch c.joinType {
				case plans.JoinInner, plans.JoinLeftOuter, plans.JoinCross:
					return recordlayer.NewResultWithValue(combined, nonEndContinuation), nil
				case plans.JoinExists:
					row := qualifyOuterRow(*c.currentOuter, c.outerAlias)
					c.currentOuter = nil
					return recordlayer.NewResultWithValue(row, nonEndContinuation), nil
				case plans.JoinNotExists:
					c.currentOuter = nil
				}
				if c.currentOuter == nil {
					break
				}
			}
		} else {
			// Linear scan path (fallback).
			for c.innerIdx < len(c.innerRows) {
				innerRow := c.innerRows[c.innerIdx]
				c.innerIdx++

				combined := mergeRows(*c.currentOuter, innerRow, c.outerAlias, c.innerAlias)
				if !passesJoinPredicates(combined, c.preds, c.evalCtx) {
					continue
				}
				c.outerMatched = true

				switch c.joinType {
				case plans.JoinInner, plans.JoinLeftOuter, plans.JoinCross:
					return recordlayer.NewResultWithValue(combined, nonEndContinuation), nil
				case plans.JoinExists:
					row := qualifyOuterRow(*c.currentOuter, c.outerAlias)
					c.currentOuter = nil
					return recordlayer.NewResultWithValue(row, nonEndContinuation), nil
				case plans.JoinNotExists:
					c.currentOuter = nil
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
			case plans.JoinLeftOuter:
				return recordlayer.NewResultWithValue(
					qualifyOuterRow(outerRow, c.outerAlias), nonEndContinuation,
				), nil
			case plans.JoinNotExists:
				qualified := outerRow
				if c.outerAlias != "" {
					qualified = qualifyOuterRow(outerRow, c.outerAlias)
				}
				return recordlayer.NewResultWithValue(qualified, nonEndContinuation), nil
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
	_ recordlayer.RecordCursor[QueryResult] = (*memorySortCursor)(nil)
	_ recordlayer.RecordCursor[QueryResult] = (*nljCursor)(nil)
	_ recordlayer.RecordCursor[QueryResult] = (*customSortCursor)(nil)
)
