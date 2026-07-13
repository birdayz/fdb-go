package executor

import (
	"context"
	"fmt"
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

	// probeErr carries a RequirePositional cap-probe hit from aggregateEvalArg
	// (which returns an `any` eval context and cannot itself return an error) up
	// to the finalizeGroup caller. Test-only; retires with the probe.
	probeErr error

	// RFC-173 input-edge ordinalization. evalCtx carries params/subqueries/outer
	// bindings so a group-key / operand reference resolves against the inner
	// PositionalRow the SAME way executeFilter / executeProjection do
	// (frontierRowContext → evaluateOrdinal → GetByName), robust to a covering-index
	// layout and NOT a name-keyed Datum read. flatFrontierInput is true when the input
	// bottoms out at a SINGLE-SOURCE flat producer (base-table scan/index, a nested
	// StreamingAgg) beneath any number of layout-preserving / reshaping single-child
	// nodes — the group-by SORT, a filter, a LIMIT/fetch, a projection / derived table
	// / CTE (RecordQueryProjectionPlan/MapPlan), a DISTINCT, a WHERE-EXISTS semi-join
	// (identity-over-outer FlatMap). Every such producer emits a flat single-source
	// output row whose GetByName is unambiguous, so keys/operands resolve positionally.
	// It is FALSE the moment the chain crosses a JOIN / multi-source (a non-identity
	// FlatMap, an NLJ, a union): a raw 2-way ordinal JOIN merge keeps its LEG WINDOWS
	// (joinWindowsOK), and a projection / aggregate OVER a LEFT JOIN — the ON-only
	// lenient CTE shadow path (Q51/Q54) whose out-of-schema read is a booked silent
	// NULL — keeps the name-keyed Datum path so it is NOT forced loud through the
	// positional frontier. Probed once at construction from the inner plan.
	evalCtx           *EvaluationContext
	flatFrontierInput bool
	needsRowCtx       bool

	// RFC-173 aggregate-over-JOIN input edge. When the input flows a 2-way ordinal
	// join's MERGED positional row (downstreamLegWindows unwraps the layout-preserving
	// passthroughs down to the join and derives its leg windows), a QUALIFIED group-key
	// / operand reference (D.DNAME, E.SALARY) resolves LEG-LOCALLY off the merged row
	// through legWindowRowContext — the SAME spanAwareRow resolver executeProjection /
	// executeFilter use over the same merge — instead of the name-keyed Datum map.
	// joinWindowsOK is true only for a genuine gated ordinal join input; a reshaping /
	// non-join input keeps windowsOK false and falls to the name path. Probed once at
	// construction from the inner plan.
	joinLegSpans  []legSpan
	joinWindowsOK bool

	// RFC-173 aggregate-over-PROJECTING-DERIVED-SOURCE input edge. When the input
	// flows a PROJECTION's re-laid-out flat positional row (a derived table / CTE
	// body over a multi-source base), a group-key / operand reference that names a
	// PROJECTED OUTPUT column resolves against that row by GetByName — leg-independent,
	// exactly as executeProjection resolves the projection itself — instead of the
	// name-keyed Datum map. projOutputNames is the projection's output-column-name set
	// (nil when no projection governs the input); an in-set reference takes the
	// positional arm, an out-of-set one (Q51/Q54 shadow read) stays on the name path.
	// Probed once at construction from the inner plan.
	projOutputNames map[string]struct{}

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
		projOutputNames:   aggregateInputProjectionOutputNames(innerPlan),
	}
}

// aggregateInputIsFlatFrontier reports whether the streaming aggregate's input
// bottoms out at a SINGLE-SOURCE flat producer whose emitted PositionalRow's
// columns are unambiguous — so a group-key / operand name resolves against it by
// GetByName exactly as executeFilter / executeProjection resolve theirs (robust to
// a covering-index column order), and is NOT a name-keyed Datum read.
//
// It walks single-child nodes down to the leaf:
//   - RETURNS TRUE at a base-table SCAN / INDEX scan, or a nested StreamingAgg
//     (single-source flat producers).
//   - PEELS THROUGH the layout-preserving wrappers (the group-by SORT, a filter, a
//     type filter, a LIMIT, the covering-index fetch) AND the reshaping single-source
//     producers (projection / derived table / CTE, DISTINCT, Map) — each re-emits a
//     flat single-source positional row, and what matters for GetByName safety is the
//     absence of a JOIN below, not the exact reshaping node.
//   - For a FlatMap: a WHERE-EXISTS identity-over-outer semi-join is a row-preserving
//     passthrough toward its OUTER (the outer scan row flows through unchanged), so it
//     peels to the outer; any OTHER FlatMap (a lateral join / merge producer) is
//     multi-source and RETURNS FALSE.
//   - RETURNS FALSE at every JOIN / multi-source node (a non-identity FlatMap, an NLJ,
//     a union). A raw 2-way ordinal JOIN merge keeps its leg windows (joinWindowsOK);
//     a projection / aggregate OVER a LEFT JOIN — the ON-only lenient CTE shadow path
//     (Q51/Q54) whose out-of-schema read is a booked silent NULL — keeps the name path
//     so it is not forced loud through the positional frontier.
func aggregateInputIsFlatFrontier(input plans.RecordQueryPlan) bool {
	for input != nil {
		switch p := input.(type) {
		case *plans.RecordQueryScanPlan,
			*plans.RecordQueryIndexPlan,
			*plans.RecordQueryTextIndexPlan,
			*plans.RecordQueryVectorIndexPlan,
			*plans.RecordQueryStreamingAggregationPlan,
			// RFC-173: a UNION (ALL / recursive-CTE level) emits a FLAT positional
			// row per leg, re-typed to the union's output column names
			// (remapUnionColumnsByPosition), so an aggregate over it resolves its
			// group key / operand by name-in-row against that row — the flat
			// frontier, not a name-keyed Datum. A birth-disabled leg (no Positional)
			// still declines to the name path safely (the arm requires Positional).
			*plans.RecordQueryUnionPlan,
			*plans.RecordQueryUnorderedUnionPlan,
			*plans.RecordQueryRecursiveLevelUnionPlan:
			return true
		case *plans.RecordQueryFetchFromPartialRecordPlan:
			input = p.GetInner()
		case *plans.RecordQuerySortPlan:
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

// aggregateInputProjectionOutputNames returns the OUTPUT-column-name set of the
// PROJECTION (derived table / CTE body) that GOVERNS the streaming aggregate's
// input row layout, or nil when no projection governs it (a plain scan, a raw
// join merge, or an aggregate directly over a join — those are handled by the
// flat-frontier / join-leg-window arms).
//
// It walks the layout-preserving wrappers (the group-by SORT, filters, LIMIT,
// the covering-index fetch, DISTINCT) down to the FIRST reshaping PROJECTION and
// returns its emitted positional-row column names via the SAME values.OutputColumnName
// authority executeProjection uses to name posNames — so membership here is
// exactly the set the projection's PositionalRow.GetByName resolves. The names
// are the alias-preferring output names (b AS L → "L"), NOT the source columns.
//
// Purpose: an aggregate over a PROJECTING derived source over a MULTI-SOURCE base
// (SELECT max(y) FROM (SELECT y, b AS L FROM t1,t2) AS q GROUP BY l — Java-parity,
// GroupByQueryTests:699) flows the projection's re-laid-out flat positional row
// [Y, L], not the pre-projection join merge. A group key / operand that names a
// projected OUTPUT column resolves LEG-INDEPENDENTLY against that row by GetByName,
// exactly as executeProjection resolves the projection itself — so the aggregate
// need not (and cannot) see the buried join's leg windows. A read NOT in the
// output set (a Q51/Q54 out-of-schema shadow read) is left to the name-keyed
// Datum path (its booked silent NULL), never forced loud through the frontier.
func aggregateInputProjectionOutputNames(input plans.RecordQueryPlan) map[string]struct{} {
	for input != nil {
		switch p := input.(type) {
		case *plans.RecordQueryProjectionPlan:
			projections := p.GetProjections()
			aliases := p.GetAliases()
			names := make(map[string]struct{}, len(projections))
			for i, proj := range projections {
				alias := ""
				if i < len(aliases) {
					alias = aliases[i]
				}
				names[strings.ToUpper(values.OutputColumnName(proj, alias))] = struct{}{}
			}
			return names
		case *plans.RecordQueryFetchFromPartialRecordPlan:
			input = p.GetInner()
		case *plans.RecordQuerySortPlan:
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
		default:
			return nil
		}
	}
	return nil
}

// valueFieldsAllInSet reports whether EVERY FieldValue in v's value tree is a BARE
// (childless) reference to a column in set. A value with no FieldValue (a constant /
// computed key over none) trivially qualifies; a COMPOUND operand (SUM(v*price))
// qualifies iff each of its bare field leaves is in set. A CHILD-BEARING FieldValue
// (a qualified QOV(alias).col or a nested record access) is NOT a bare
// projected-output-column read, so it DISQUALIFIES v — such a reference stays on the
// existing name / leg-window path rather than being forced onto the projection row.
//
// Used to gate the projection-output arm: only a group key / operand whose columns
// are ALL bare projected outputs resolves positionally against the projection row;
// anything else (a mixed value, an out-of-schema Q51/Q54 shadow read, a qualified
// access) falls through — correct-or-name, never a loud frontier miss.
func valueFieldsAllInSet(v values.Value, set map[string]struct{}) bool {
	all := true
	values.WalkValue(v, func(n values.Value) bool {
		fv, ok := n.(*values.FieldValue)
		if !ok {
			return true // not a field; descend into composites (arithmetic, cast, …)
		}
		if fv.Child != nil {
			all = false // a qualified / nested access, not a bare output-column ref
			return false
		}
		if _, in := set[strings.ToUpper(fv.Field)]; !in {
			all = false
		}
		return true
	})
	return all
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
// RFC-173 input-edge ordinalization: the streaming aggregate reads its keys /
// operands off the inner row exactly the way executeFilter / executeProjection
// read their predicate / projected values — the ONE frontier dispatch, so the
// aggregate input resolves positionally on the same shapes those do.
//
//   - A POSITIONALLY-BAKED value (a FieldValue with a resolved ordinal — the
//     gathered-seed un-collapse's qualifier-honoring group key/operand) reads the
//     flat seed row it was baked against: the simple {Datum,Positional} context, no
//     leg re-windowing (the gathered seed emits a flat row and its bakes are flat
//     slots).
//   - On a FLAT single-source frontier (flatFrontierInput — a scan / index / filter /
//     projection / derived table / CTE / DISTINCT / semi-join / nested StreamingAgg
//     whose chain bottoms out at a single source, NOT a 2-way ordinal JOIN merge), a
//     name group-key / operand resolves against the inner PositionalRow by name-in-row
//     (frontierRowContext → evaluateOrdinal → GetByName). This is robust to a
//     covering-index column layout (GetByName, never a plan-time table ordinal) and is
//     NOT a name-keyed Datum read, so it no longer depends on the name model.
//   - A RAW 2-way ordinal JOIN merge (joinWindowsOK — the input flows the join's
//     MERGED positional row through passthroughs only): a qualified group-key /
//     operand reference QOV(leg).col (or the flat DOTTED "D.DNAME" the name-model
//     pipeline spells it as) resolves LEG-LOCALLY through its window, exactly as
//     executeProjection / executeFilter resolve theirs over the same merge.
//     Unconditional on a windowed input: even with no param/subquery/outer binding,
//     the leg windows are required (the bare merged row misreads leg-relative
//     ordinals — the W3 wrong-slot hazard).
//   - A projection / aggregate OVER A JOIN (the ON-only lenient CTE shadow path —
//     neither flatFrontierInput nor joinWindowsOK) and a name-only row (no Positional)
//     keep the name-keyed Datum path — carrying the row's Sparse flag so an unset
//     optional / out-of-schema field stays a silent NULL rather than tripping
//     NameMissLoud (task #38, Q51/Q54 flip-sentinels).
func (c *aggregateCursor) aggregateEvalArg(v values.Value, row QueryResult) any {
	if row.Positional != nil && valueReadsBakedOrdinal(v) {
		m, _ := row.Datum.(map[string]any)
		return &values.RowEvalContext{Datum: m, Positional: row.Positional}
	}
	if row.Positional != nil && c.flatFrontierInput {
		return frontierRowContext(row.Positional, c.evalCtx, c.needsRowCtx)
	}
	if row.Positional != nil && c.joinWindowsOK {
		return legWindowRowContext(row.Positional, c.evalCtx, c.joinLegSpans)
	}
	if row.Positional != nil && c.projOutputNames != nil && valueFieldsAllInSet(v, c.projOutputNames) {
		// RFC-173: a PROJECTING derived source (SELECT y, b AS L FROM t1,t2) AS q
		// re-lays-out its multi-source base into a flat positional row [Y, L]; a
		// group-key / operand naming a PROJECTED OUTPUT column resolves against
		// that row by ordinal-in-row, exactly as executeProjection resolves the
		// projection itself — the buried join's leg windows are irrelevant to the
		// projection's own output schema. An out-of-schema reference (a Q51/Q54
		// shadow read) is excluded by valueFieldsAllInSet and keeps the name path.
		return frontierRowContext(row.Positional, c.evalCtx, c.needsRowCtx)
	}
	// RFC-173: a row that carries a FLAT Positional (not leg-windowed — joinWindowsOK
	// false) but matched none of the specific arms above (e.g. a COUNT(*) constant
	// operand, or a group key over a recursive-CTE / bare-projected-join row) still
	// resolves against that ordinal row by name-in-row (GetByName) — authoritative,
	// no name-keyed Datum. This is the general birth-of-Positional consumer: once a
	// producer emits an output-named Positional, the aggregate reads it ordinally.
	if row.Positional != nil {
		return frontierRowContext(row.Positional, c.evalCtx, c.needsRowCtx)
	}
	if m, ok := row.Datum.(map[string]any); ok {
		if perr := requirePositional("aggregate"); perr != nil {
			c.probeErr = perr
		}
		return &values.RowEvalContext{Datum: m, Sparse: row.Sparse}
	}
	return row.Datum
}

// valueReadsBakedOrdinal reports whether v (a group key / aggregate operand) reads
// through a plan-time-resolved ordinal ANYWHERE in its value tree — the un-collapse's
// positionally-baked FieldValue, possibly nested inside a compound operand
// (`SUM(A.K+B.K)`). Any baked leaf means the whole value must evaluate against the
// positional row; a fully name-model value reads the Datum map unchanged.
func valueReadsBakedOrdinal(v values.Value) bool {
	// FrontierPinned specifically — the ordinal-frontier bake the group-by makes
	// (NewFieldValueOfOrdinal). An UNPINNED baked node (a recursive-CTE leg projection
	// column) is never an aggregate key/operand, and reads the Datum map like any
	// name-model value — matching intent, not over-broad on `Resolved != nil`.
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
		if c.probeErr != nil {
			return "", nil, c.probeErr
		}
		keyParts[i] = v
		// tuple.Pack handles nil, int64, float64, string, []byte natively.
		// For types the tuple layer doesn't support, fall back to the
		// string representation so we still get a deterministic key.
		//
		// A UUID group key ([16]byte) DELIBERATELY takes the %T:%v fallback, NOT
		// tuple.UUID: this packed key is stored as a JSON string in the aggregate
		// continuation (encodeAggregateContinuation) and compared byte-for-byte on
		// resume to detect a group change. A tuple.UUID packs 16 raw bytes that
		// are often invalid UTF-8, which json.Marshal replaces with U+FFFD —
		// corrupting the key so a group straddling a page boundary would falsely
		// break. The %T:%v ASCII form is JSON-safe and equally deterministic, and
		// the group's surfaced VALUE rides in keyVals as the [16]byte (round-tripped
		// via the continuation's UUID tag), so the extractor/materializer are
		// unaffected. (This is the one spot where the [16]byte→tuple.UUID discipline
		// is intentionally not applied — the sink here is a JSON key, not the wire.)
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
		if c.probeErr != nil {
			return c.probeErr
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
	// RFC-173: build the OUTPUT ORDINAL ROW alongside the name-keyed map. Order + naming MUST
	// match streamingAggOutputNames (the plan's authoritative output schema the planner baked
	// downstream ordinals against): grouping keys in order (aggKeyName), then aggregates in order
	// (ALIAS-preferring, else aggResultName). A downstream ref baked to this schema reads by
	// Get(ord); an unbaked one resolves by GetByName against these exact names. This is a real
	// producer Positional from the known output columns, not a name-map wrapper.
	posNames := make([]string, 0, len(c.groupingKeys)+len(c.aggregates))
	posSlots := make([]any, 0, len(c.groupingKeys)+len(c.aggregates))
	for i, k := range c.groupingKeys {
		name := aggKeyName(k)
		result[name] = gs.keyVals[i]
		posNames = append(posNames, name)
		posSlots = append(posSlots, gs.keyVals[i])
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
				val = gs.sums[i] / float64(gs.counts[i])
			} else {
				val = nil
			}
		}
		result[name] = val
		// Alias-preferring output name (matches streamingAggOutputNames), so a downstream ref
		// over an ALIASED aggregate (SUM(a*b) AS foo, a derived-table projection) resolves.
		posName := name
		if agg.Alias != "" {
			posName = strings.ToUpper(agg.Alias)
		}
		posNames = append(posNames, posName)
		posSlots = append(posSlots, val)
		if agg.Alias != "" && agg.Alias != name {
			result[agg.Alias] = val
		}
		if agg.OperandName != "" {
			spaced := strings.ToUpper(agg.Function.String() + "(" + agg.OperandName + ")")
			if spaced != name {
				result[spaced] = val
			}
		}
	}
	qr := QueryResult{Datum: result, Complete: true}
	if !DisablePositionalEmission { // §4 birth-site obligation: gate every new Positional emission
		qr.Positional = &PositionalRow{Type: positionalTypeFromNames(posNames), Slots: posSlots}
	}
	return qr
}

func (c *aggregateCursor) emptyScalarResult() QueryResult {
	result := make(map[string]any)
	// RFC-173: emit the authoritative ordinal row alongside the name-keyed map,
	// SAME order + naming as finalizeGroup's aggregate arm (there are no grouping
	// keys on the empty-scalar path — an empty GROUP BY yields zero rows, not this
	// single all-groups row). Without it the empty-table scalar aggregate row had
	// no Positional and every read fell back to the name-keyed Datum.
	posNames := make([]string, 0, len(c.aggregates))
	posSlots := make([]any, 0, len(c.aggregates))
	for _, agg := range c.aggregates {
		name := aggResultName(agg)
		var val any
		if agg.Function == expressions.AggCount {
			val = int64(0)
		}
		result[name] = val
		posName := name
		if agg.Alias != "" {
			posName = strings.ToUpper(agg.Alias)
		}
		posNames = append(posNames, posName)
		posSlots = append(posSlots, val)
		if agg.Alias != "" && agg.Alias != name {
			result[agg.Alias] = val
		}
	}
	qr := QueryResult{Datum: result, Complete: true}
	if !DisablePositionalEmission {
		qr.Positional = &PositionalRow{Type: positionalTypeFromNames(posNames), Slots: posSlots}
	}
	return qr
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
	maxBuf  int                       // 0 = use DefaultMaxSortBufferRows
	st      *recordlayer.ExecuteState // RFC-130 statement memory budget
}

func newMemorySortCursor(
	inner recordlayer.RecordCursor[QueryResult],
	keys []string,
	dirs []bool,
	st *recordlayer.ExecuteState,
) *memorySortCursor {
	return &memorySortCursor{
		inner: inner,
		keys:  keys,
		dirs:  dirs,
		st:    st,
	}
}

func (c *memorySortCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.closed {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
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
				sortByKeys(c.buf, c.keys, c.dirs)
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
		// RFC-130: the in-memory sort buffer is a cardinality-growing buffer —
		// charge each row's bytes against the statement memory budget before
		// keeping it.
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

	// birth is the RFC-173 Slice 2 ordinal-BIRTH state, probed ONCE at
	// construction from the plan's result value (nil = name-model join,
	// today's path bit-identically). When enabled, every emitted row
	// additionally carries the positional merged row evaluated from the RC
	// with per-leg bindings — DUAL emission: the name-model Datum
	// (mergeRows/qualifyOuterRow) stays byte-identical — gated per emission
	// on the §5 DisablePositionalEmission oracle.
	birth *ordinalJoinBirth
	// birthActive = birth enabled AND the §5 oracle off, read once at
	// construction (a cursor's lifetime never straddles an oracle phase —
	// tests own whole phases). Gates ALL ordinal work below.
	birthActive bool
	// innerAdapted is the FIXED inner-rows slice adapted once at construction
	// (parallel to innerRows); outerAdapted is the current outer row adapted
	// once per outer-row advance. Together they make the per-candidate-pair
	// cost one small twoLegBinder — no re-adaptation, no map (review W3a-2
	// structural-perf catch: the name model pays no per-pair adapter work, so
	// neither may the window).
	innerAdapted []values.OrdinalRow
	outerAdapted values.OrdinalRow
	// outerCorr/innerCorr are the leg correlation identifiers, resolved once.
	outerCorr, innerCorr values.CorrelationIdentifier
	// mergeRC is set iff the result value is the S3 positional-merge RC: the
	// emitted Datum is then the MERGE SHAPE (slot `_i` = leg i's own Datum,
	// mergeShapeDatum) on BOTH the live and §5-oracle sides — the partition
	// rule rebases every upper reference through the merge quantifier, so
	// mergeRows' flat keys would silently NULL them all (0-row joins on the
	// oracle side, where no positional row backs the reads). NOT gated on
	// birthActive: the oracle side is exactly where the flat Datum bites.
	mergeRC *values.RecordConstructorValue
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
	birth, err := newOrdinalJoinBirth(resultValue, preds)
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
		birth:      birth,
	}
	c.outerCorr = values.NamedCorrelationIdentifier(outerAlias)
	c.innerCorr = values.NamedCorrelationIdentifier(innerAlias)
	if rc, isRC := resultValue.(*values.RecordConstructorValue); isRC && values.IsPositionalMergeRC(rc) {
		c.mergeRC = rc
	}
	if birth.enabled() && !DisablePositionalEmission {
		c.birthActive = true
		// Adapt the FIXED inner side once (review W3a-2: never per pair).
		innerType := birth.legType(c.innerCorr)
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

// recoverOracleDatumSpans recovers the DATUM spans for a TRANSLATED
// (fused-reference) NLJ top from the leg subplans' result values — the NLJ
// twin of newFlatMapCursor's legRV recovery, feeding ONLY the §5 oracle's
// qualified keys (oracleFusedDatum → oracleNameDatum). The adapter-side
// Spans/WindowsOK stay untouched: the live NLJ never consults DatumSpans
// (its Datum is mergeRows/mergeShapeDatum), and flipping WindowsOK would
// re-route the leg adapter's legType lookups (the Q6-pinned dimension).
// Spliced per the DatumSpans contract (a box leg's window opens to the leaf
// aliases dotted reads actually name).
func (c *nljCursor) recoverOracleDatumSpans(outerPlan, innerPlan plans.RecordQueryPlan) {
	if !c.birth.enabled() || c.birthActive {
		return
	}
	legRVs := make(map[values.CorrelationIdentifier]values.Value)
	addJoinLegRV(legRVs, c.outerCorr, outerPlan)
	addJoinLegRV(legRVs, c.innerCorr, innerPlan)
	if len(c.birth.DatumSpans) == 0 {
		if spans, _, ok := ordinalJoinSpansOf(c.birth.RC, legRVs); ok {
			c.birth.DatumSpans = spans
		}
	}
	// Splice even when the birth arrived with PRISTINE seed spans — the exact
	// FlatMap ordering (flat_map_cursor.go runs the splice as a separate step
	// after recovery): a seed whose leg is a gated-join BOX carries a span
	// named after the box alias covering the whole concat, and oracleNameDatum
	// would qualify every column under that one alias instead of the leaf
	// aliases dotted reads actually name (review finding: the early
	// return skipped the splice for already-windowed births).
	if len(c.birth.DatumSpans) > 0 && len(legRVs) > 0 {
		c.birth.DatumSpans = spliceLegSpans(c.birth.DatumSpans, legRVs)
	}
}

// oracleFusedDatum is the §5 NAME-MODEL ORACLE's emitted row for a
// TRANSLATED ordinal-RC NLJ top (fused post-merge references — the gathered
// N-way join/unnest star): mergeRows' flat keys never carry the RC's OUTPUT
// names (a merge leg's columns live inside its nested `_i` map, under the
// merge alias's original case), so every bare or dotted downstream read —
// the projection's lazy `EL.EL`, a sort key — silently NULLed oracle-side
// (the dualwindow differential's first W5/3-way catch). Reconstruct the
// output row exactly like the FlatMap's oracle arm: evaluate the RC per
// field over the leg bindings (values.OracleBakedNameFallback bridges the
// baked reads), bare names plus ALIAS.COL keys from the recovered
// DatumSpans. A nil leg Datum is the LEFT/FULL null leg — its references
// evaluate NULL (contract ruling #3). TEST-ONLY (oracle emissions); dies
// with the oracle in Slice 4.
func (c *nljCursor) oracleFusedDatum(outerDatum, innerDatum any) (map[string]any, error) {
	od, _ := outerDatum.(map[string]any)
	rowCtx := c.evalCtx.
		WithBinding(c.outerCorr, outerDatum).
		WithBinding(c.innerCorr, innerDatum).
		RowContext(od)
	return c.birth.oracleNameDatum(rowCtx)
}

// oracleSwapFusedDatum applies the emission-time Datum swap for a fused top
// on the oracle side (mirroring the mergeRC swap): predicates already ran
// over the mergeRows keys; only the EMITTED row carries the reconstructed
// output-shaped Datum consumers read.
func (c *nljCursor) oracleSwapFusedDatum(combined *QueryResult, outerDatum, innerDatum any) error {
	if c.birthActive || !c.birth.enabled() || c.mergeRC != nil {
		return nil
	}
	m, err := c.oracleFusedDatum(outerDatum, innerDatum)
	if err != nil {
		return err
	}
	combined.Datum = m
	return nil
}

// pairBinder builds the per-candidate-pair leg binder from PRE-adapted leg
// rows. A nil OrdinalRow is the deliberately-NULL leg (LEFT/FULL padding —
// the binder returns (nil, true), contract ruling #3). Only called when
// birthActive.
func (c *nljCursor) pairBinder(outer, inner values.OrdinalRow) *twoLegBinder {
	return &twoLegBinder{
		outerID: c.outerCorr, innerID: c.innerCorr,
		outer: outer, inner: inner,
		base: correlationBase(c.evalCtx),
	}
}

// adaptOuter adapts a just-advanced outer row ONCE (shared by every candidate
// pair and the unmatched-outer emission for this outer row). No-op for
// name-model cursors.
func (c *nljCursor) adaptOuter(outerRow QueryResult) error {
	if !c.birthActive {
		return nil
	}
	row, err := adaptLegPositional(outerRow, c.birth.legType(c.outerCorr))
	if err != nil {
		return err
	}
	c.outerAdapted = row
	return nil
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
	c.hashJoinCol = innerCol
	c.outerJoinCol = outerCol
}

// nljHashEntryBytes is the per-inner-row cost charged for the NLJ hash index:
// the int slot in the bucket slice plus amortized map/slice-header overhead.
const nljHashEntryBytes int64 = 24

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
		// A FUSED multi-accessor path (the S3 merge rebase: `m._i.col`) has NO
		// name-keyed hash key: its display Field is the LEAF column, but the
		// quantifier's rows are merge-shaped (`_i` slots) — keying the hash
		// index on "M.col" probes a key the rows never carry, so the index
		// comes up empty and the ≥100-row fast path silently drops every
		// match. Decline; the linear path evaluates the fused reference
		// correctly.
		if fv.Resolved != nil && len(fv.Resolved.Accessors) > 1 {
			return ""
		}
		// A FieldValue qualified via a QuantifiedObjectValue child
		// (QOV(alias).col — the form re-enumerated join predicates use)
		// carries its table alias in the child's correlation, not in the
		// bare Field. Return the qualified "ALIAS.COL" so extractEquijoinColumns
		// can tell the outer side from the inner side. Without this, the bare
		// field has an empty qualifier, matchesAlias("",x) is always true, and
		// the equijoin column extraction picks outer/inner backwards — the hash
		// index is then keyed on the wrong column and the join returns 0 rows
		// (only on the ≥100-row hash-join path, so the bug is data-dependent).
		// The legacy flat form (Field already "ALIAS.COL", no child) is
		// returned unchanged.
		if qov, ok := fv.Child.(*values.QuantifiedObjectValue); ok {
			alias := qov.Correlation.Name()
			if alias != "" {
				return alias + "." + fv.Field
			}
		}
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

// matchesAlias reports whether a field's table qualifier `table` refers to
// quantifier `alias`. An empty `table` (an unqualified field, e.g. the legacy
// flat "COL" form with no dot) matches ANY alias — the permissive fallback for
// fields that carry no qualifier. Callers that must distinguish sides (e.g.
// extractEquijoinColumns) therefore depend on fieldName() returning a QUALIFIED
// name for QOV-child FieldValues; otherwise both sides match the first branch
// and outer/inner are picked backwards (see equijoin_columns_test.go).
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

// fabricateNullLeg writes the NULL-supplied leg's declared columns into a null-padded merge
// Datum as present-nil, so a name-keyed read of that leg (e.g. O.ID over an unmatched LEFT
// JOIN row) resolves to SQL NULL instead of tripping the NameMissLoud guard (task #38). Per
// the design ruling, the three exactness safeguards: (1) fabricate nil from the null leg's
// OWN schema (legType) — NEVER read the present leg's row (that is the RFC-077 wrong-source
// clobber); (2) ADD-IF-ABSENT — it can only write into keys the present leg did not claim, so
// it can never overwrite a real value (a shared bare column, a dup alias); (3) the qualified
// "ALIAS.col" cannot collide (the present leg qualifies under its OWN alias), the bare "col"
// is defensive for a uniquely-named null-leg column read bare. It reads nothing from the
// present row. The Datum then agrees column-for-column with the Positional null-pad, which is
// built from the same legType.
func fabricateNullLeg(datum map[string]any, nullType *values.RecordType, nullAlias string) {
	if datum == nil || nullType == nil {
		return
	}
	up := strings.ToUpper(nullAlias)
	for _, f := range nullType.Fields {
		col := strings.ToUpper(f.Name)
		if up != "" {
			if qk := up + "." + col; !mapHasKeyLocal(datum, qk) {
				datum[qk] = nil
			}
		}
		if !mapHasKeyLocal(datum, col) {
			datum[col] = nil
		}
	}
}

func mapHasKeyLocal(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
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
						// qualifyOuterRow is repurposed here for an INNER
						// row: it qualifies the inner row's columns under
						// innerAlias and leaves the outer columns absent, so
						// downstream qualified refs to the outer side resolve
						// to NULL.
						qr := qualifyOuterRow(c.innerRows[i], c.innerAlias)
						// The merge-shape swap on the FULL drain too (the OUTER leg is
						// the empty-map NULL leg) — unreachable until LEFT/FULL gate in
						// W4 (the merge arm's IsNullOnEmpty tripwire), handled so all
						// three emission paths agree.
						if c.mergeRC != nil {
							qr.Datum = mergeShapeDatum(c.mergeRC, c.outerCorr, c.innerCorr, nil, c.innerRows[i].Datum)
						}
						if serr := c.oracleSwapFusedDatum(&qr, nil, c.innerRows[i].Datum); serr != nil {
							return recordlayer.RecordCursorResult[QueryResult]{}, serr
						}
						// RFC-173 S2: ordinal birth of the drain row — the
						// OUTER leg is the NULL leg (nil row), symmetric to
						// the unmatched-outer emission below. The inner was
						// adapted once at construction.
						// task #38: fabricate the NULL leg (OUTER) columns present-nil so a
						// name-keyed read of them resolves to SQL NULL, not a loud unresolved
						// reference. Uses the outer leg's own schema; reads nothing from the inner row.
						if c.birth != nil {
							if dm, ok := qr.Datum.(map[string]any); ok {
								fabricateNullLeg(dm, c.birth.legType(c.outerCorr), c.outerAlias)
							}
						}
						if c.birthActive {
							pos, berr := c.birth.evaluateBound(c.pairBinder(nil, c.innerAdapted[i]))
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
			// RFC-173 S2: adapt the new outer leg ONCE for all its candidate
			// pairs (no-op for name-model cursors).
			if err := c.adaptOuter(outerRow); err != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, err
			}

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
				if err := ctx.Err(); err != nil {
					return recordlayer.RecordCursorResult[QueryResult]{}, err
				}
				idx := c.innerMatches[c.innerIdx]
				innerRow := c.innerRows[idx]
				c.innerIdx++
				combined := mergeRows(*c.currentOuter, innerRow, c.outerAlias, c.innerAlias)
				// RFC-173 S2: for an ordinal-birth cursor the predicate row
				// context carries the per-leg bindings from the PRE-adapted
				// legs (nil binder = today's path bit-identically); the same
				// binder then births the positional row on a pass.
				var pair *twoLegBinder
				if c.birthActive {
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
					if c.mergeRC != nil {
						// EMISSION-time swap, after the predicates ran: the
						// cursor's own (leg-baked) predicates still read the
						// mergeRows keys on the oracle side; only the EMITTED
						// merge row carries the merge-shape Datum consumers read.
						combined.Datum = mergeShapeDatum(c.mergeRC, c.outerCorr, c.innerCorr, c.currentOuter.Datum, innerRow.Datum)
					}
					if serr := c.oracleSwapFusedDatum(&combined, c.currentOuter.Datum, innerRow.Datum); serr != nil {
						return recordlayer.RecordCursorResult[QueryResult]{}, serr
					}
					if pair != nil {
						pos, berr := c.birth.evaluateBound(pair)
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
				// RFC-173 S2: same ordinal-birth dual emission as the hash
				// path above (nil binder = today's path bit-identically).
				var pair *twoLegBinder
				if c.birthActive {
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
					if c.mergeRC != nil {
						// EMISSION-time swap, after the predicates ran: the
						// cursor's own (leg-baked) predicates still read the
						// mergeRows keys on the oracle side; only the EMITTED
						// merge row carries the merge-shape Datum consumers read.
						combined.Datum = mergeShapeDatum(c.mergeRC, c.outerCorr, c.innerCorr, c.currentOuter.Datum, innerRow.Datum)
					}
					if serr := c.oracleSwapFusedDatum(&combined, c.currentOuter.Datum, innerRow.Datum); serr != nil {
						return recordlayer.RecordCursorResult[QueryResult]{}, serr
					}
					if pair != nil {
						pos, berr := c.birth.evaluateBound(pair)
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
				qr := qualifyOuterRow(outerRow, c.outerAlias)
				// A merge-RC cursor's null extension carries the merge SHAPE
				// too — the NULL leg is the empty map, exactly the name
				// model's reconstructed empty inner row (unreachable today:
				// the merge arm's IsNullOnEmpty tripwire keeps LEFT/FULL
				// selects anchored until W4; handled rather than half-covered
				// so the shape is ready when LEFT gates).
				if c.mergeRC != nil {
					qr.Datum = mergeShapeDatum(c.mergeRC, c.outerCorr, c.innerCorr, outerRow.Datum, nil)
				}
				if serr := c.oracleSwapFusedDatum(&qr, outerRow.Datum, nil); serr != nil {
					return recordlayer.RecordCursorResult[QueryResult]{}, serr
				}
				// RFC-173 S2: ordinal birth of the null-padded row — the
				// INNER leg is the NULL leg (nil row): the RC evaluates with
				// QOV(inner)→nil and the inner slots fall out NULL (contract
				// ruling #3's appendNullLeg equivalence). The outer was
				// adapted once at its advance.
				// task #38: fabricate the NULL leg (INNER) columns present-nil so a name-keyed
				// read resolves to SQL NULL. Uses the inner leg's own schema; reads nothing
				// from the outer row.
				if c.birth != nil {
					if dm, ok := qr.Datum.(map[string]any); ok {
						fabricateNullLeg(dm, c.birth.legType(c.innerCorr), c.innerAlias)
					}
				}
				if c.birthActive {
					pos, berr := c.birth.evaluateBound(c.pairBinder(c.outerAdapted, nil))
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
	_ recordlayer.RecordCursor[QueryResult] = (*memorySortCursor)(nil)
	_ recordlayer.RecordCursor[QueryResult] = (*nljCursor)(nil)
	_ recordlayer.RecordCursor[QueryResult] = (*customSortCursor)(nil)
)
