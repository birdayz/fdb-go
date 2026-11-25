// Portions derived from FoundationDB Record Layer (AggregateCursor.java,
// StreamGrouping.java, FlatMapPipelinedCursor.java,
// NumericAggregationValue.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"fmt"
	"math"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func (c *aggregateCursor) accumulateRow(row QueryResult) error {
	gs := c.current
	gs.count++

	for i, agg := range c.aggregates {
		// COUNT(*) and its planner-normalized constant equivalents consume the
		// row itself, not an operand. In particular, the canonical COUNT(*)
		// AggregateSpec has a nil Operand; trying to route it through the exact
		// value evaluator both invents a binding requirement and dereferences a
		// value that deliberately does not exist. finalizeGroup reads gs.count
		// for this class, so there is no per-aggregate state to update here.
		if expressions.IsCountStar(agg) {
			continue
		}
		if agg.Operand == nil {
			return fmt.Errorf("%s aggregate has no operand", agg.Function)
		}
		arg, err := c.aggregateEvalArg(agg.Operand, row)
		if err != nil {
			return err
		}
		val, err := agg.Operand.Evaluate(arg)
		if err != nil {
			return recordlayer.TranslateTextCmpError(err)
		}
		if agg.Function == expressions.AggArrayAgg {
			if err := accumulateArrayAgg(gs, i, agg, val); err != nil {
				return err
			}
			continue
		}
		if val == nil {
			continue
		}
		gs.counts[i]++
		switch agg.Function {
		case expressions.AggBitmapConstructAgg:
			if err := accumulateBitmapConstruct(gs, i, val); err != nil {
				return err
			}
		case expressions.AggSum, expressions.AggAvg:
			if err := accumulateSumAvg(gs, i, agg, val); err != nil {
				return err
			}
		case expressions.AggMin, expressions.AggMax:
			if !isNumeric(val) {
				return &AggregateTypeMismatchError{
					Message: "unable to encapsulate aggregate operation due to type mismatch(es)",
				}
			}
			gs.mins[i] = aggMinMax(gs.mins[i], val, true)
			gs.maxs[i] = aggMinMax(gs.maxs[i], val, false)
		}
	}
	return nil
}

func accumulateBitmapConstruct(gs *groupState, i int, val any) error {
	n, ok := asInt64(val)
	if !ok {
		return fmt.Errorf("bitmap aggregate requires an integer, got %T", val)
	}
	// Java LONG narrows via Long.intValue before setting the BitSet.
	position := int32(n)
	if position < 0 || position >= 250000 {
		return &BitmapAggregatePositionError{Position: position}
	}
	size := int(position)/8 + 1
	if size < 1250 {
		size = 1250
	}
	if len(gs.bitmaps[i]) < size {
		gs.bitmaps[i] = append(gs.bitmaps[i], make([]byte, size-len(gs.bitmaps[i]))...)
	}
	gs.bitmaps[i][position/8] |= 1 << (uint32(position) % 8)
	return nil
}

func accumulateSumAvg(gs *groupState, i int, agg expressions.AggregateSpec, val any) error {
	if !isNumeric(val) {
		return fmt.Errorf("cannot aggregate non-numeric value of type %T", val)
	}
	if agg.OperandIntType == values.TypeCodeFloat {
		// Java SUM_F / AVG_F add at the operand's static FLOAT width,
		// even though stored FLOAT values arrive as float64. Widen the
		// rounded partial exactly into the existing continuation slot;
		// AVG widens before division, not before accumulation.
		num, _ := asFloat32(val) // isNumeric admits precisely these carriers.
		if gs.counts[i] == 1 {
			gs.sums[i] = float64(num)
		} else {
			gs.sums[i] = float64(float32(gs.sums[i]) + num)
		}
		gs.allInt[i] = false
		return nil
	}
	num, ok := asFloat64(val)
	if !ok {
		// Unreachable after isNumeric; an error, never a NaN, if not.
		return fmt.Errorf("cannot aggregate non-numeric value of type %T", val)
	}
	// Java NumericAccumulator seeds from the first non-NULL partial.
	// Adding it to an implicit +0 would change SUM/AVG(-0) to +0.
	// The non-NULL count also preserves this distinction on resume.
	if gs.counts[i] == 1 {
		gs.sums[i] = num
	} else {
		gs.sums[i] += num
	}
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
				return &SumOverflowError{Int32: true}
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
	return nil
}

// accumulateArrayAgg is Java's ArrayAccumulator.accumulate: past the LIMIT a
// row is consumed but dropped, IGNORE NULLS drops a NULL, and RESPECT NULLS
// refuses one because an array element is never NULL.
func accumulateArrayAgg(gs *groupState, i int, agg expressions.AggregateSpec, val any) error {
	if agg.Limit != values.ArrayAggNoLimit && len(gs.arrays[i]) >= agg.Limit {
		return nil
	}
	if val == nil {
		if agg.IgnoreNulls {
			return nil
		}
		return &values.NullArrayElementError{}
	}
	gs.arrays[i] = append(gs.arrays[i], val)
	return nil
}

// aggMinMax folds val into the running MIN (isMin=true) or MAX extremum, matching
// Java's NumericAggregationValue MIN_*/MAX_* operators (NumericAggregationValue.java:679-687),
// which are Math.min / Math.max per numeric type. It is the SOLE MIN/MAX comparator;
// the accumulate loop's isNumeric gate guarantees both operands are one of
// int64/int32/int/float64/float32 — but NOT that acc and val share a type: an
// unpromoted CASE/PickValue operand (`MIN(CASE WHEN c THEN dbl ELSE 0 END)`) mixes
// int64 and float64 across rows. Java never sees that mix — its encapsulation
// wraps the operand in PromoteValue, so MIN_D receives doubles only. Until Go
// grows plan-time operand promotion, the same numeric promotion is applied here
// PER PAIR (widest float type present wins), which computes what Java computes
// over the promoted operand for two-type streams (promotion is monotone, so
// pairwise and whole-stream promotion select the same extremum; long→double
// beyond 2^53 and long→float beyond 2^24 round identically in both). Two
// precision corners remain where PAIRWISE differs from Java's STATIC promotion,
// both scoped to the RFC-083 PromoteValue follow-up: an all-int-branch group
// emits int64 where Java emits double (value-equal below 2^53, Go more precise
// above), and in a three-type stream an int > 2^24 paired against a float32
// BEFORE any float64 arrives rounds long→float where Java's static DOUBLE lane
// would round long→double. No-crash, no-drop corners only.
//
// For FLOAT/DOUBLE this MUST use Java Math.min/Math.max semantics, NOT a
// total-order comparator: NaN PROPAGATES into BOTH extremes
// (min(x,NaN)=max(x,NaN)=NaN) and -0.0 sorts below +0.0
// (min(-0.0,+0.0)=-0.0, max(-0.0,+0.0)=+0.0). Go's math.Min/math.Max are NOT
// identical to Java's on one corner: their special-case order resolves
// Min(-Inf,NaN)=-Inf and Max(+Inf,NaN)=+Inf where Java propagates NaN — the
// javaMinF64/javaMaxF64 wrappers below add the NaN-first guard (divergence
// surfaced by the G4 exotic-float pin). A total-order
// comparator (values.CompareFloat64, used by the sort/predicate path) makes NaN the
// GREATEST element — correct for MAX but WRONG for MIN, which must yield NaN, not the
// smallest finite. So float MIN/MAX is deliberately a DIFFERENT float semantic than
// the ordering authority, exactly mirroring Java's Math.min/max (MIN_D/MAX_D) vs
// Double.compare (tuple/index order) split. Integer types (int/int32/int64) have no
// NaN or signed zero, so an all-integer pair takes the ordinary int64 min/max
// (Java MIN_I/MIN_L / MAX_I/MAX_L).
func aggMinMax(acc, val any, isMin bool) any {
	if acc == nil {
		return val
	}
	_, aIsF64 := acc.(float64)
	_, vIsF64 := val.(float64)
	_, aIsF32 := acc.(float32)
	_, vIsF32 := val.(float32)
	switch {
	case aIsF64 || vIsF64:
		// DOUBLE lane (Java MIN_D/MAX_D over the double-promoted operand).
		a, aok := asFloat64(acc)
		v, vok := asFloat64(val)
		if !aok || !vok {
			return acc // non-numeric mix: contract guard (isNumeric-gated upstream)
		}
		if isMin {
			return javaMinF64(a, v)
		}
		return javaMaxF64(a, v)
	case aIsF32 || vIsF32:
		// FLOAT lane (Java MIN_F/MAX_F over the float-promoted operand). An int
		// side converts directly to float32 (Java's (float) promotion, one
		// rounding); the result is one of the two operands, bits included.
		a, aok := asFloat32(acc)
		v, vok := asFloat32(val)
		if !aok || !vok {
			return acc // contract guard
		}
		if isMin {
			return javaMinF32(a, v)
		}
		return javaMaxF32(a, v)
	}
	// Integer numerics (int64/int32/int). Row integers arrive int64 via the
	// tupleElementToRowValue canonicalization; int32/int are accepted for
	// robustness. Widen to int64 and take the ordinary min/max.
	ai, aok := asInt64(acc)
	vi, vok := asInt64(val)
	if aok && vok && ((isMin && vi < ai) || (!isMin && vi > ai)) {
		return val
	}
	return acc
}

// asInt64 widens an integer aggregate operand to int64. Only int64/int32/int reach
// aggMinMax's integer path (isNumeric gates the operands; floats are handled above),
// so the false return is a contract guard, not a live branch.
func asInt64(x any) (int64, bool) {
	switch n := x.(type) {
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case int:
		return int64(n), true
	}
	return 0, false
}

// asFloat64 promotes a numeric aggregate operand to float64 (Java's double
// promotion: float→double exact; long→double rounds beyond 2^53 identically).
func asFloat64(x any) (float64, bool) {
	switch n := x.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}

// asFloat32 converts a numeric aggregate operand to the FLOAT lane. Stored
// FLOATs arrive as float64; integers convert directly to avoid double rounding
// through float64. MIN/MAX choose their widest runtime lane before calling it.
func asFloat32(x any) (float32, bool) {
	switch n := x.(type) {
	case float64:
		return float32(n), true
	case float32:
		return n, true
	case int64:
		return float32(n), true
	case int32:
		return float32(n), true
	case int:
		return float32(n), true
	}
	return 0, false
}

// javaMinF64 / javaMaxF64 mirror Java Math.min/Math.max exactly: if either
// operand is NaN the result is NaN, unconditionally. Go's math.Min/math.Max
// check the infinity special cases FIRST, so Min(-Inf, NaN) = -Inf and
// Max(+Inf, NaN) = +Inf — an evaluated-aggregate divergence from Java
// whenever a set contains both an infinity and a NaN.
//
// They are Math.min(double, double) and Math.max line for line (JDK
// Math.java), so a NaN
// result is the NaN OPERAND with its own bits (Java's `if (a != a) return a`,
// and `a <= b` is false for a NaN b, which returns b): a MIN over the NaN of
// 0.0/0.0 stores 0xfff8000000000000 in both engines, never Go's
// math.NaN(). -0.0 is below +0.0.
func javaMinF64(a, b float64) float64 {
	if a != a {
		return a
	}
	if a == 0 && b == 0 && math.Float64bits(b) == 1<<63 {
		return b
	}
	if a <= b {
		return a
	}
	return b
}

func javaMaxF64(a, b float64) float64 {
	if a != a {
		return a
	}
	if a == 0 && b == 0 && math.Float64bits(a) == 1<<63 {
		return b
	}
	if a >= b {
		return a
	}
	return b
}

// javaMinF32 and javaMaxF32 are Math.min(float, float) and Math.max: the same
// algorithm in binary32, so a FLOAT NaN operand keeps its exact bits (a trip
// through float64 would quiet a signaling payload).
func javaMinF32(a, b float32) float32 {
	if a != a {
		return a
	}
	if a == 0 && b == 0 && math.Float32bits(b) == 1<<31 {
		return b
	}
	if a <= b {
		return a
	}
	return b
}

func javaMaxF32(a, b float32) float32 {
	if a != a {
		return a
	}
	if a == 0 && b == 0 && math.Float32bits(a) == 1<<31 {
		return b
	}
	if a >= b {
		return a
	}
	return b
}

// BitmapAggregatePositionError rejects a negative bit or a result exceeding
// Java BitmapValueIndexMaintainer.MAX_ENTRY_SIZE, before allocating that result.
type BitmapAggregatePositionError struct{ Position int32 }

func (e *BitmapAggregatePositionError) Error() string {
	if e.Position < 0 {
		return fmt.Sprintf("bitIndex < 0: %d", e.Position)
	}
	return "entry size option is too large"
}
