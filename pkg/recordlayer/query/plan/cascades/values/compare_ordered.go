package values

import (
	"bytes"
	"fmt"
)

// CompareOrdered is a total order over the concrete scalar/binary Go types the
// query engine's row and IN-list values take (the int/uint/float variants,
// bool, string, []byte, [16]byte UUID, and nil). It is the single
// Java-faithful natural order this codebase uses wherever two runtime values
// must be ranked against each other outside of an indexed comparison:
// in-memory sort, merge-cursor ordering keys, and a SORTED IN-join's literal
// list (cascades.sortInJoinValues).
//
// Two edge cases differ from Go's native comparison operators, matching
// java.lang.Double.compare exactly (see CompareFloat64): NaN sorts greater
// than every non-NaN value and compares equal to NaN, and -0.0 sorts
// strictly before +0.0. Every other typed arm matches FDB's own tuple
// encoding order, so an in-memory ordering agrees with what an indexed scan
// of the same column would produce.
//
// nil sorts before every other value (a nil pair compares equal). A pair
// with no shared typed arm returns a loud error rather than silently
// picking a wrong order — the planner's type checking is expected to
// exclude cross-type comparisons before this is ever called on production
// data, so this arm is a defensive backstop, not a designed code path.
func CompareOrdered(a, b any) (int, error) {
	if a == nil && b == nil {
		return 0, nil
	}
	if a == nil {
		return -1, nil
	}
	if b == nil {
		return 1, nil
	}
	// Integer domain first, EXACTLY (CompareExactInts): tuple decoding admits
	// the full signed range as int64 AND positive values above math.MaxInt64
	// as uint64, so integer pairs compare by true numeric value without a
	// float round-trip. A mixed integer/float pair falls through to the
	// float arms' total order.
	if c, ok := CompareExactInts(a, b); ok {
		return c, nil
	}
	switch av := a.(type) {
	case int64, int32, int, uint64, uint:
		// Integer vs a floating operand (the integer/integer case returned
		// above): promote and use the Java-faithful float total order. A
		// non-numeric b is the loud cross-type arm below.
		if bf, _, ok := ToFloat64(b); ok {
			af, _, _ := ToFloat64(av)
			return CompareFloat64(af, bf), nil
		}
	case float64:
		if bf, _, ok := ToFloat64(b); ok {
			return CompareFloat64(av, bf), nil
		}
	case float32:
		if bf, _, ok := ToFloat64(b); ok {
			return CompareFloat64(float64(av), bf), nil
		}
	case bool:
		// false < true — FDB tuple order (0x26 < 0x27), same as Java's
		// Boolean.compare.
		if bv, ok := b.(bool); ok {
			switch {
			case av == bv:
				return 0, nil
			case !av:
				return -1, nil
			default:
				return 1, nil
			}
		}
	case string:
		if bv, ok := b.(string); ok {
			switch {
			case av < bv:
				return -1, nil
			case av > bv:
				return 1, nil
			default:
				return 0, nil
			}
		}
	case []byte:
		// BYTES sorts by unsigned lexicographic byte order — the same order
		// the FDB tuple byte-string encoding gives an indexed BYTES column.
		if bv, ok := b.([]byte); ok {
			return bytes.Compare(av, bv), nil
		}
	case [16]byte:
		// UUID sorts by unsigned big-endian bytes — the same order the
		// tuple.UUID wire encoding uses.
		if bv, ok := b.([16]byte); ok {
			return bytes.Compare(av[:], bv[:]), nil
		}
	}
	// A pair with no typed arm is a cross-type mismatch the planner's type
	// checking excludes (every row-domain type has an arm above; the float
	// arms totally order NaN) — correct-or-loud, never a silently misordered
	// result.
	return 0, fmt.Errorf("values: no ordering defined between %T and %T (cross-type comparison the planner should have excluded)", a, b)
}
