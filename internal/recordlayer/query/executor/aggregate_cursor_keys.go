// Portions derived from FoundationDB Record Layer (AggregateCursor.java,
// StreamGrouping.java, FlatMapPipelinedCursor.java,
// NumericAggregationValue.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"bytes"
	"fmt"
	"math"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// aggregateEvalArg is the eval argument for a GROUP-BY key / aggregate operand.
//
// The streaming aggregate reads its keys / operands off the inner row exactly
// the way executeFilter / executeMap read their predicate / projected
// values — the ONE frontier dispatch, so the aggregate input resolves
// positionally on the same shapes those do.
//
//   - A BARE SCALAR input (scalarInputLayout: a non-ordinal Explode's element,
//     the correlated array a block's first FROM item unnests) binds the
//     unwrapped element to the input QOV, which is what a key or operand over
//     it reads (`SUM(x)` is SUM over QOV(x)).
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
//     executeMap / executeFilter resolve theirs over the same merge.
//     Unconditional on a windowed input: even with no param/subquery/outer binding,
//     the leg windows are required (the bare merged row misreads leg-relative
//     ordinals — a wrong-slot hazard).
//   - A row with NO Positional yields NULL: every live input flows an
//     ordinal PositionalRow; there is no name-map to read.
func (c *aggregateCursor) aggregateEvalArg(v values.Value, row QueryResult) (any, error) {
	if row.Positional == nil {
		return nil, nil
	}
	if c.scalarInputLayout != nil {
		scalar, err := isBareScalarRow(row.Positional)
		if err != nil {
			return nil, err
		}
		if scalar {
			return scalarLayoutRowContext(c.scalarInputLayout, row.Positional, c.evalCtx, c.inputEdges...)
		}
	}
	if valueReadsBakedOrdinal(v) {
		// A baked ordinal operand reads plan-time-resolved slots directly off the
		// positional row (evaluateOrdinal), so pass it as the bare ordinal row —
		// with the statement clock in reach for a CURRENT_TIMESTAMP-family
		// subtree of the operand.
		if row.Positional.Layout != nil {
			return frontierRowContext(row.Positional, c.evalCtx, true, c.inputEdges...)
		}
		rc := &values.RowEvalContext{Positional: row.Positional, Clock: c.evalCtx}
		if c.evalCtx != nil {
			rc.Binder = c.evalCtx
		}
		return rc, nil
	}
	if c.flatFrontierInput {
		return frontierRowContext(row.Positional, c.evalCtx, c.needsRowCtx, c.inputEdges...)
	}
	if c.joinWindowsOK {
		// A RAW 2-way ordinal JOIN merge: a qualified group-key / operand
		// QOV(leg).col resolves LEG-LOCALLY through its window.
		return legWindowRowContext(row.Positional, c.evalCtx, c.joinLegSpans), nil
	}
	// The general positional-row consumer: a flat output-named positional
	// (projecting derived source, recursive-CTE / bare-projected-join row, or a
	// constant operand) resolves by its baked plan-time ordinal — authoritative.
	return frontierRowContext(row.Positional, c.evalCtx, c.needsRowCtx, c.inputEdges...)
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
	if fv, ok := values.AsFieldValue(v); ok && fv.Path().IsFrontierPinned() {
		return true
	}
	for _, c := range v.Children() {
		if valueReadsBakedOrdinal(c) {
			return true
		}
	}
	return false
}

// canonicalizeGroupKeyFloat applies the GROUPING identity to a float before it
// is packed into the group key: every NaN payload becomes one value, and the
// two signed zeros stay distinct.
//
// That is java.lang.Double.equals — doubleToLongBits — which is what Java's
// streaming aggregation compares (StreamGrouping.java:186 decides a group break
// with `!currentGroup.equals(nextGroup)` over a protobuf message, and protobuf
// compares a double field with Double.equals). It is deliberately NOT IEEE
// `==`, under which the two zeros would merge and no two NaNs would ever match.
//
// THERE IS NO float32 COMPANION, and its absence is a measured fact rather than
// an oversight. A 32-bit FLOAT column arrives at this layer already WIDENED to
// float64 — the SQL row path hands back float64 for both carriers — so a
// float32 never reaches computeGroupKey from a query. MEASURED: a panic at the
// top of a float32 arm here passed the entire SQL-level gate untouched, and a
// `GROUP BY` on a FLOAT column carrying the payloads 0x7fc00000 and 0xffc00000
// is served correctly by the float64 arm above
// (TestFDB_FloatGroupByNaNAuthority_Float32 pins exactly that, and goes red
// with this canonicalization removed).
//
// A float32 that somehow did arrive is still handled: the switch has no
// float32 case, so it falls to appendDistinctValue, which canonicalizes NaN at
// every nesting level. Correct by fall-through beats a specialized arm no
// caller reaches — a dead arm cannot be mutation-tested, so it is a claim the
// suite can never check.
func canonicalizeGroupKeyFloat(v float64) float64 {
	if math.IsNaN(v) {
		return math.Float64frombits(distinctCanonicalNaN64Bits)
	}
	return v
}

func (c *aggregateCursor) computeGroupKey(row QueryResult) (string, []any, error) {
	if c.scalarMode {
		return "", nil, nil
	}
	keyParts := make([]any, len(c.groupingKeys))
	for i, k := range c.groupingKeys {
		arg, err := c.aggregateEvalArg(k, row)
		if err != nil {
			return "", nil, err
		}
		v, err := k.Evaluate(arg)
		if err != nil {
			return "", nil, recordlayer.TranslateTextCmpError(err)
		}
		keyParts[i] = v
	}
	packed, err := packGroupKey(keyParts)
	if err != nil {
		return "", nil, err
	}
	return packed, keyParts, nil
}

// packGroupKey turns evaluated grouping-key VALUES into the packed group key.
//
// It is separate from computeGroupKey because the resume path has values but no
// row to evaluate: withPartialState re-derives the key from the continuation's
// decoded keyVals rather than trusting the bytes the continuation carries. Both
// callers must use the same encoder or a resume compares keys from two
// different ones.
func packGroupKey(keyParts []any) (string, error) {
	t := make(tuple.Tuple, len(keyParts))
	for i, v := range keyParts {
		// tuple.Pack handles nil, int64, float64, string, []byte, bool natively.
		// Any other slot type (UUID [16]byte, composite ARRAY/STRUCT values)
		// encodes LOSSLESSLY via the continuation codec as one []byte tuple
		// slot — the retired %T:%v rendering collided on composites
		// ([]any{"a b"} vs []any{"a","b"}) and merged distinct groups
		// (RFC-180 C3).
		//
		// On resume withPartialState re-derives the key from the continuation's
		// typed keyVals through this same function, so a token minted under an
		// older encoding still compares against keys built by this one.
		//
		// FLOAT/DOUBLE grouping identity is java.lang.Double.equals, and it says
		// two different things that must BOTH be honoured. Java's streaming
		// aggregation compares grouping values with
		// `!currentGroup.equals(nextGroup)` (StreamGrouping.java:186) over the
		// protobuf DynamicMessage that RecordConstructorValue.eval builds
		// (RecordConstructorValue.java:113-140), and protobuf message equality
		// compares a double field with Double.equals — doubleToLongBits:
		//
		//   every NaN payload is ONE value (the bits are canonicalized)
		//   -0.0 and +0.0 are TWO values   (the bits differ — the OPPOSITE of
		//                                   IEEE `==`)
		//
		// So NaN is canonicalized here and zero is packed VERBATIM. Packing
		// both verbatim — which is what this did — takes the correct half of
		// that rule and applies it to the case it does not cover.
		//
		// The zero half must stay: when the same GROUP BY is served by a
		// maintained aggregate index the grouping prefix IS the index key, so
		// the two signed zeros are two physical entries Java reads the same
		// way, and merging them would change what is on the wire.
		//
		// The NaN half does NOT have that constraint in the direction that
		// matters. An aggregate index splits NaN payloads in Java too
		// (AtomicMutationIndexMaintainer.java:141,158 packs the prefix, and
		// tuple encoding preserves the payload), so Java is itself
		// plan-dependent on NaN — one group through the streaming plan, two
		// through the index. Reproducing that split is conformance; inventing a
		// THIRD answer, where Go's streaming path splits and its own DISTINCT
		// (executor.packedDedupKey) merges, is not.
		//
		// Composites go through appendDistinctValue rather than appendContValue
		// so a NaN inside an ARRAY or STRUCT grouping key obeys the same rule
		// at every nesting level, exactly as protobuf message equality does.
		// (appendContValue remains the reversible continuation codec and must
		// keep every raw bit; the two encoders differ on purpose.)
		//
		// The canonical NaN payloads are the ones packedDedupKey uses, so
		// GROUP BY and DISTINCT agree on the DOUBLE carrier — which is the
		// carrier both actually see. Neither sees a float32 from a query (see
		// canonicalizeGroupKeyFloat), so that is a shared CONSTANT, not a
		// shared exercised path, and it is not claimed as one.
		switch tv := v.(type) {
		case float64:
			t[i] = canonicalizeGroupKeyFloat(tv)
		case nil, int64, string, []byte, bool:
			t[i] = tv
		default:
			b, cerr := appendDistinctValue(nil, v)
			if cerr != nil {
				return "", fmt.Errorf("group key slot %d: %w", i, cerr)
			}
			t[i] = b
		}
	}
	return string(t.Pack()), nil
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
		arrays:  make([][]any, len(c.aggregates)),
		bitmaps: make([][]byte, len(c.aggregates)),
	}
}

func (c *aggregateCursor) finalizeGroup() QueryResult {
	gs := c.current
	// Build the OUTPUT ORDINAL ROW. Order + naming MUST match
	// expressions.GroupByOutputColumnNames (the plan's authoritative output schema the planner
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
		case expressions.AggBitmapConstructAgg:
			if gs.counts[i] > 0 {
				val = bytes.Clone(gs.bitmaps[i])
			}
		case expressions.AggMin:
			val = gs.mins[i]
		case expressions.AggMax:
			val = gs.maxs[i]
		case expressions.AggArrayAgg:
			// A group that saw rows yields an array, empty if every row was
			// dropped.
			val = append([]any{}, gs.arrays[i]...)
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
		// Alias-preferring output name (matches expressions.GroupByOutputColumnNames), so a downstream ref
		// over an ALIASED aggregate (SUM(a*b) AS foo, a derived-table projection) resolves.
		//
		// VERBATIM, because "matches GroupByOutputColumnNames" is a claim this
		// line has to keep: that function
		// publishes an alias unfolded. This is
		// the second of two copies of one naming rule, and two copies that
		// disagree is the shape that produced the recursive-UNION-DISTINCT
		// wrong answer. Note honestly that no constructible query drives it —
		// a plan-supplied c.outputType wins whenever it is non-nil, and the
		// derived-table arm written to reach it resolves by ordinal instead —
		// so this is kept in step with its partner rather than pinned by a
		// test of its own.
		posName := name
		if agg.Alias != "" {
			posName = agg.Alias
		}
		posNames = append(posNames, posName)
		posSlots = append(posSlots, val)
	}
	outputType := c.outputType
	if outputType == nil {
		outputType = positionalTypeFromNames(posNames)
	}
	return QueryResult{Positional: &PositionalRow{Type: outputType, Slots: posSlots}}
}

// emptyScalarAggregateRow is the scalar-aggregation-on-empty-input default row
// (COUNT(*)=0, SUM/MIN/MAX/AVG=NULL): the authoritative ordinal row, SAME order
// + naming as finalizeGroup's aggregate arm (there are no grouping keys on the
// empty-scalar path — an empty GROUP BY yields zero rows, not this single
// all-groups row). It is executeAggregation's OrElse ALTERNATIVE (Java: the
// DefaultOnEmpty plan's onEmptyResultValue riding RecordCursor.orElse), never
// emitted by the aggregate cursor itself (Java AggregateCursor.isNoRecords
// returns exhausted on empty input).
func emptyScalarAggregateRow(aggregates []expressions.AggregateSpec, outputType *values.RecordType) QueryResult {
	posNames := make([]string, 0, len(aggregates))
	posSlots := make([]any, 0, len(aggregates))
	for _, agg := range aggregates {
		name := aggResultName(agg)
		var val any
		if agg.Function == expressions.AggCount {
			val = int64(0)
		}
		posName := name
		if agg.Alias != "" {
			posName = agg.Alias
		}
		posNames = append(posNames, posName)
		posSlots = append(posSlots, val)
	}
	if outputType == nil {
		outputType = positionalTypeFromNames(posNames)
	}
	return QueryResult{Positional: &PositionalRow{Type: outputType, Slots: posSlots}}
}
