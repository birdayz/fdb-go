// Portions derived from FoundationDB Record Layer (MessageHelpers.java,
// QueryResult.java, IndexOrphanBehavior.java, RecordCursor.java,
// UnionCursor.java, MergeCursorState.java, UnorderedUnionCursor.java,
// UnionCursorContinuation.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"math"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// scanBindContext returns the binder for evaluating scan-range comparands. It
// must be a *RowEvalContext (via RowContext), NOT the bare *EvaluationContext:
// an uncorrelated scalar subquery pushed as a scan bound is a ScalarSubqueryValue,
// and ScalarSubqueryValue.Evaluate only reads its pre-computed result from a
// *RowEvalContext's ScalarSubqueries map. Passing the bare *EvaluationContext made
// it resolve to nil → an `id = NULL` bound → an empty scan (e.g.
// `WHERE id = (SELECT MIN(id) FROM t)` returned 0 rows). RowContext still binds
// parameters (BindParameter) and correlations, so this is a strict superset.
// nil-safe (a nil evalCtx keeps the prior nil binder for the param-free unit path).
func scanBindContext(evalCtx *EvaluationContext) values.ParameterBinder {
	if evalCtx == nil {
		return nil
	}
	return evalCtx.RowContext()
}

// uuidToTupleElement converts a neutral 16-byte UUID comparand ([16]byte —
// the wire-agnostic representation a UUID carries through the value layer, see
// values.PromoteValue.Evaluate and predicates.cmpAny) into a tuple.UUID at the
// FDB wire boundary. Only a named tuple.UUID packs as the 0x30 UUID tuple
// element; a bare [16]byte would panic the tuple packer ("unencodable
// element"). Everything else (int64, string, float64, …) passes through
// unchanged. This is the sole place the value-layer [16]byte crosses into wire
// encoding on the scan-range path — symmetric with the index-entry write in
// recordlayer.scalarToInterface, so the equality probe seeks the exact 0x30
// bytes Java (and the maintainer) wrote.
func uuidToTupleElement(v any) any {
	if b, ok := v.([16]byte); ok {
		return tuple.UUID(b)
	}
	return v
}

// toFloat32Scalar narrows any admitted numeric runtime value to float32.
// Companion to toFloat64Scalar, used only at the tuple-packing boundary
// (coerceTupleElementForKey) where the resolved comparand type says FLOAT.
func toFloat32Scalar(v any) (float32, bool) {
	f, ok := toFloat64Scalar(v)
	if !ok {
		return 0, false
	}
	return float32(f), true
}

// coerceTupleElementForKey adapts an evaluated scan comparand to the physical
// tuple representation of its key component. physicalType is authoritative
// whenever it is known: comparison operands frequently carry UnknownType
// (parameters, correlated values, and IN legs), and using that erased RHS type
// can pack a FLOAT key as DOUBLE or a DOUBLE key as an integer. Only when the
// physical plan genuinely lacks key metadata do we fall back to the operand's
// declared type and finally to the evaluated Go value.
//
// This is deliberately a packing-boundary conversion, not a SQL cast. The
// planner has already established comparison compatibility; this function
// merely selects the tuple wire width that the indexed/primary key was written
// with. Two independent coercions land here:
//
//   - UUID: unchanged, delegates to uuidToTupleElement (kept as a single
//     choke point rather than duplicated at both call sites below).
//   - FLOAT (32-bit): downcast to a genuine Go float32. A FLOAT-indexed
//     column's entries are tuple-packed as 0x20 (single); everywhere else
//     in the value layer a FLOAT-typed result is deliberately carried as
//     float64 rounded to float32 precision (see tupleElementToRowValue's
//     doc comment — row-domain consistency between the base-record and
//     covering-index read paths, and values.PromoteValue's
//     coerceNumericResult, which returns that same float64-carrying
//     float32). That convention is right for materialized rows but WRONG
//     here: the tuple encoder dispatches on the Go RUNTIME type
//     (float32 → single, float64 → double — pkg/fdbgo/fdb/tuple), so a
//     float64 comparand would pack against a FLOAT column's index entries
//     under the wrong type code and match nothing (or, for an inequality
//     bound, degrade to all-or-nothing — the same class of bug
//     widenConstAgainstDoubleColumn fixed for int-vs-DOUBLE). This is the
//     mirror image of tupleElementToRowValue's float32→float64 upcast on
//     the READ side; this is the WRITE-a-probe-key side, downcasting back
//     down to the wire's actual type code. Covers both a bare re-typed
//     ConstantValue (expr.narrowConstAgainstFloatColumn: Evaluate already
//     returns a real float32, so this is a no-op type-assert-that-hits)
//     and a values.PromoteValue-wrapped correlated comparand
//     (expr.promoteColumnColumnNumeric, the column-vs-column cross-type
//     join case: Evaluate returns the row-domain float64, and THIS is
//     where it finally narrows to match the index's wire type).
func coerceTupleElementForKey(v any, physicalType, operandType values.Type) any {
	v = uuidToTupleElement(v)
	if v == nil {
		return nil
	}

	typ := physicalType
	if typ == nil || typ.Code() == values.TypeCodeUnknown {
		typ = operandType
	}
	if typ == nil {
		return v
	}

	switch typ.Code() {
	case values.TypeCodeInt, values.TypeCodeLong:
		if n, ok := toExactInt64Scalar(v); ok {
			return n
		}
	case values.TypeCodeFloat:
		if f, ok := toFloat32Scalar(v); ok {
			return f
		}
	case values.TypeCodeDouble:
		if f, ok := toFloat64Scalar(v); ok {
			return f
		}
	case values.TypeCodeVersion:
		// A row's __ROW_VERSION reads as its 12 serialized bytes; a version
		// index key holds it as a tuple versionstamp.
		if b, ok := v.([]byte); ok {
			if version, err := recordlayer.CompleteVersionFromBytes(b); err == nil {
				if versionstamp, err := version.ToVersionstamp(); err == nil {
					return versionstamp
				}
			}
		}
	}
	return v
}

// toExactInt64Scalar normalizes the integer representations admitted by the
// value layer to the tuple package's signed integer carrier. A floating value
// is accepted only when it is finite, integral, and exactly representable as
// int64; comparison type checking owns every lossy conversion.
func toExactInt64Scalar(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint:
		if uint64(n) <= math.MaxInt64 {
			return int64(n), true
		}
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		if n <= math.MaxInt64 {
			return int64(n), true
		}
	case float32:
		f := float64(n)
		if !math.IsNaN(f) && !math.IsInf(f, 0) && math.Trunc(f) == f &&
			f >= float64(math.MinInt64) && f < float64(math.MaxInt64) {
			return int64(f), true
		}
	case float64:
		if !math.IsNaN(n) && !math.IsInf(n, 0) && math.Trunc(n) == n &&
			n >= float64(math.MinInt64) && n < float64(math.MaxInt64) {
			return int64(n), true
		}
	}
	return 0, false
}

// tupleElementToRowValue is values.TupleElementToRowValue, applied at every
// index-entry → row boundary.
func tupleElementToRowValue(v any) any { return values.TupleElementToRowValue(v) }

// isZeroFloatBound reports whether a scan comparand v — already run through
// coerceTupleElementForKey, so it carries the wire Go type (float32 for FLOAT,
// float64 for DOUBLE) — is a zero of either sign. Only FLOAT/DOUBLE need the
// signed-zero range widening below: FDB tuple encoding preserves the IEEE
// sign bit (adjustFloatBytes, pkg/fdbgo/fdb/tuple/tuple.go), so +0.0 and
// -0.0 pack to two DISTINCT, adjacent index keys (-0.0 sorts immediately
// before +0.0, with no other representable value between them — the
// negative-float encoding inverts all bits, giving -0.0 the bit pattern
// just below +0.0's). cmpAny (predicates package, RFC-082) instead follows
// IEEE/SQL numeric equality, where -0.0 == +0.0. A single-key probe or a
// range endpoint pinned to the comparand's own sign therefore silently
// disagrees with the residual-filter semantics the planner promises for an
// unmatched predicate — see bindScanComparisonsToRangeSet's callers.
//
// Dispatches on v's OWN Go runtime type, not the Comparison operand's
// declared values.Type: an IN-list element reaches this comparand already
// narrowed to the right Go type by expr.ResolveIn (widenIntToDouble /
// narrowToFloat32, done once at plan time against the LHS column's type),
// but the per-iteration Operand the InJoin/Explode machinery binds it
// through is a QuantifiedObjectValue over an UnknownType-elemented array
// (rule_in_to_explode.go), so comp.Operand.Type() reports Unknown at this
// call site even though the runtime value is a genuine float32/float64. The
// coerced value's own type is authoritative for what the tuple packer will
// actually emit; the declared type is not.
func isZeroFloatBound(v any) bool {
	switch f := v.(type) {
	case float32:
		return f == 0
	case float64:
		return f == 0
	default:
		return false
	}
}

// negativeZeroLike and positiveZeroLike return the canonical negative/positive
// zero in the same Go runtime type as a coerced comparand v (float32 for a
// FLOAT column, float64 for DOUBLE) — the tuple encoder dispatches on the Go
// runtime type, so the returned value must pack under the SAME type code
// (single vs double) the comparand already carries, regardless of which sign
// the original literal or stored value happened to be.
func negativeZeroLike(v any) any {
	if _, ok := v.(float32); ok {
		return float32(math.Copysign(0, -1))
	}
	return math.Copysign(0, -1)
}

func positiveZeroLike(v any) any {
	if _, ok := v.(float32); ok {
		return float32(0)
	}
	return float64(0)
}

// canonicalizeZeroSignedBound rewrites a zero-valued FLOAT/DOUBLE
// ordered-inequality comparand to whichever sign makes its scan-range
// endpoint agree with IEEE/SQL numeric comparison (cmpAny), regardless of
// which sign the original literal or stored value happened to carry. -0.0
// and +0.0 are adjacent index keys (isZeroFloatBound), -0.0 sorting
// immediately below +0.0:
//   - `>`  is FALSE for both -0.0 and +0.0 (neither is strictly greater than
//     zero) -> pin to +0.0: an EXCLUSIVE low bound at +0.0 starts scanning
//     strictly after the +0.0 key, which — because -0.0 sorts below it — also
//     clears -0.0.
//   - `<=` is TRUE for both -0.0 and +0.0 -> pin to +0.0: an INCLUSIVE high
//     bound at +0.0 covers its own key and everything below, including -0.0.
//   - `>=` is TRUE for both -0.0 and +0.0 -> pin to -0.0: an INCLUSIVE low
//     bound at -0.0 starts scanning AT the lower of the two adjacent keys,
//     so both are included.
//   - `<`  is FALSE for both -0.0 and +0.0 -> pin to -0.0: an EXCLUSIVE high
//     bound at -0.0 stops before the lower of the two adjacent keys, so
//     neither is included.
func canonicalizeZeroSignedBound(comparand any, t predicates.ComparisonType) any {
	if !isZeroFloatBound(comparand) {
		return comparand
	}
	switch t {
	case predicates.ComparisonGreaterThan, predicates.ComparisonLessThanOrEq:
		return positiveZeroLike(comparand)
	case predicates.ComparisonGreaterThanEq, predicates.ComparisonLessThan:
		return negativeZeroLike(comparand)
	default:
		return comparand
	}
}

// anyLaterComparisonConstrains reports whether any comparison after index i
// narrows the key range. A candidate index contributes one ComparisonRange per
// indexed column, so unconstrained trailing columns appear as EMPTY ranges and
// must not count — otherwise a scan range that genuinely terminates at i looks
// non-terminal purely because the index has more columns than the query
// mentions.
func anyLaterComparisonConstrains(comparisons []*predicates.ComparisonRange, i int) bool {
	for _, cr := range comparisons[i+1:] {
		if cr != nil && !cr.IsEmpty() {
			return true
		}
	}
	return false
}

// widenInt32 normalizes an intersection/union merge comparison-key element so the
// FDB tuple layer can Pack it. The tuple layer has no int32 case — Pack panics on
// it — and the index key encoding already widens int32 columns to int64
// (key_expression.go's scalar field read), so widening here keeps the in-memory merge key
// byte-identical to the children's sort order (int32->int64 sign-extension is
// value-preserving and tuple integer encoding is monotonic). Matches Java, whose
// Tuple stores Long and never sees a 32-bit key element. Only int32 is handled:
// it's the unique order-preserving widening and the only confirmed-reachable
// unpackable comparison-key type (field reads pre-widen at query_result.go); a
// genuinely exotic type stays raw so compareKeys' Pack-error path catches it rather
// than risk a non-monotonic coercion. See RFC-092.
func widenInt32(v any) any {
	if i32, ok := v.(int32); ok {
		return int64(i32)
	}
	return v
}

// compareValues delegates to values.CompareOrdered, the single Java-faithful
// natural order this codebase uses for sort/merge/dedup ranking of runtime
// scalar values (see that function's doc for the exact semantics: NaN
// greatest, -0.0 < 0.0, FDB tuple byte order for []byte/UUID, nil least, a
// loud error on a cross-type pair). Kept as a thin package-local alias so the
// merge-sort cursor call sites and the differential fuzz test below don't
// need the values. qualifier at every use.
func compareValues(a, b any) (int, error) {
	return values.CompareOrdered(a, b)
}
