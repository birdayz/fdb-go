package values

import "bytes"

// InOpValue is the Value-layer SQL `IN` operator: tests whether a
// probe value matches any element of a list of candidate values.
// Mirrors Java's `com.apple.foundationdb.record.query.plan.cascades.
// values.InOpValue`.
//
//	probe IN (a, b, c)  ↔  InOpValue{Probe: probe, List: [a, b, c]}
//
// Why a Value-layer IN in addition to the predicate-layer
// `ComparisonPredicate{Type: ComparisonIn}`: rules that operate at
// the Value tree (e.g. fold a constant probe against a constant
// list) need a Value-shaped node. The predicate-side path is
// reserved for evaluation; the Value-side path is for plan rewrites.
//
// Java's InOpValue carries an `inListValue` Value (typically
// LiteralValue wrapping a list, or a ListValue node), making it
// possible to express IN against dynamic lists. The seed accepts a
// generic List Value field that can be a literal []any (for static
// IN-lists) or any other Value that evaluates to a slice at runtime.
//
// Evaluate semantics — Kleene 3VL:
//   - probe IN (NULL, ...) where probe is non-NULL: TRUE if any non-NULL
//     element matches, otherwise UNKNOWN (NULL propagation).
//   - NULL IN (anything): UNKNOWN.
//   - Empty list: FALSE (no match possible).
//
// Type is always nullable boolean — IN can produce NULL via Kleene
// propagation.
type InOpValue struct {
	Probe Value
	List  Value // expected to evaluate to []any at runtime
}

// equalsAny compares two `any` values without panicking on
// non-comparable types. Go's `==` on `any` would panic if both
// sides were []byte (slices aren't comparable). bytes.Equal
// handles the byte-slice case; everything else falls through to
// `==`.
func equalsAny(a, b any) bool {
	if ab, ok := a.([]byte); ok {
		if bb, ok := b.([]byte); ok {
			return bytes.Equal(ab, bb)
		}
		return false
	}
	if _, ok := b.([]byte); ok {
		return false
	}
	if af, bf, ok := promoteNumeric(a, b); ok {
		return af == bf
	}
	return a == b
}

// promoteNumeric promotes mixed-type numeric pairs for comparison.
// Matches Java's Comparisons.evalComparison(EQUALS) which coerces all
// numerics to a common type. Only triggers when both are numeric —
// same-type pairs already work via Go's == operator, but cross-type
// (int32 vs int64, int64 vs float64) need promotion.
func promoteNumeric(a, b any) (float64, float64, bool) {
	af, aok := toFloat64(a)
	bf, bok := toFloat64(b)
	if !aok || !bok {
		return 0, 0, false
	}
	return af, bf, true
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case int:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	default:
		return 0, false
	}
}

// NewInOpValue constructs an InOpValue.
//
// Either Probe or List nil produces a Value that always evaluates to
// nil (UNKNOWN). Defensive — callers should construct with both
// operands set.
func NewInOpValue(probe, list Value) *InOpValue {
	return &InOpValue{Probe: probe, List: list}
}

// Children returns probe + list. Lets WalkValue traverse both
// operands as a standard 2-child Value.
func (v *InOpValue) Children() []Value {
	out := make([]Value, 0, 2)
	if v.Probe != nil {
		out = append(out, v.Probe)
	}
	if v.List != nil {
		out = append(out, v.List)
	}
	return out
}

// Name returns the debug-print kind.
func (*InOpValue) Name() string { return "in" }

// Type is always nullable boolean (NULL propagation through Kleene
// 3VL forces nullable).
func (*InOpValue) Type() Type { return NullableBoolean }

// Evaluate computes probe IN list with SQL three-valued semantics.
//
// Returns:
//   - true if probe matches any non-NULL element of the list.
//   - false if probe doesn't match any list element AND the list
//     contains no NULLs.
//   - nil (UNKNOWN) if probe is NULL, OR probe doesn't match a non-
//     NULL element AND the list contains a NULL (NULL propagation).
//   - nil if probe or list is nil-Value, or list doesn't evaluate
//     to a slice.
//
// equalsAny performs numeric coercion for mixed int/float
// comparisons, matching Java's Comparisons.evalComparison(EQUALS).
// See D-10 in CASCADES_DIVERGENCE.md.
func (v *InOpValue) Evaluate(evalCtx any) any {
	if v.Probe == nil || v.List == nil {
		return nil
	}
	probe := v.Probe.Evaluate(evalCtx)
	if probe == nil {
		return nil // NULL IN anything = UNKNOWN
	}
	list := v.List.Evaluate(evalCtx)
	listAny, ok := list.([]any)
	if !ok {
		return nil // type-degraded — list isn't a slice
	}
	sawNull := false
	for _, elem := range listAny {
		if elem == nil {
			sawNull = true
			continue
		}
		if equalsAny(probe, elem) {
			return true
		}
	}
	if sawNull {
		// Probe didn't match any non-NULL element; an unknown
		// element might match. Result is UNKNOWN.
		return nil
	}
	return false
}
