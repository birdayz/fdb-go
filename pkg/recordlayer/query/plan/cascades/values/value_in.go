package values

import (
	"bytes"
	"reflect"
)

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
// possible to express IN against dynamic lists. Go accepts a
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
// handles the byte-slice case.
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
	// Same-family integers compare EXACTLY — never round-trip two int64 through
	// float64, whose promotion ties adjacent values above 2^53 (so
	// 9007199254740993 IN (9007199254740992) would wrongly match). This mirrors
	// the predicate-layer IN (comparisons.cmpAny → CompareExactInts); the
	// value-layer IN previously promoted every numeric pair to float64.
	if cmp, ok := CompareExactInts(a, b); ok {
		return cmp == 0
	}
	// A genuine float on at least one side (not both integers): coerce to a
	// common numeric type, matching Java's Comparisons.evalComparison(EQUALS).
	if af, bf, ok := promoteNumeric(a, b); ok {
		return af == bf
	}
	return comparableEqual(a, b)
}

// comparableEqual reports value equality without panicking on non-comparable
// dynamic types. Go's `==` on an `any` holding a slice/map/func panics; for
// those (and for differing dynamic types) it falls back to reflect.DeepEqual,
// which is panic-safe and gives structural equality for nested array/map
// elements. Comparable same-type values use fast `==`.
func comparableEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ta := reflect.TypeOf(a)
	if ta == reflect.TypeOf(b) && ta.Comparable() {
		return a == b
	}
	return reflect.DeepEqual(a, b)
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
func (v *InOpValue) Evaluate(evalCtx any) (any, error) {
	if v.Probe == nil || v.List == nil {
		return nil, nil
	}
	probe, err := v.Probe.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	if probe == nil {
		return nil, nil // NULL IN anything = UNKNOWN
	}
	list, err := v.List.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	listAny, ok := list.([]any)
	if !ok {
		return nil, nil // type-degraded — list isn't a slice
	}
	sawNull := false
	for _, elem := range listAny {
		if elem == nil {
			sawNull = true
			continue
		}
		if equalsAny(probe, elem) {
			return true, nil
		}
	}
	if sawNull {
		// Probe didn't match any non-NULL element; an unknown
		// element might match. Result is UNKNOWN.
		return nil, nil
	}
	return false, nil
}
