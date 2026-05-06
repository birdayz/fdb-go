package functions

import (
	"bytes"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"
)

// IsTruthy returns true when v is a non-nil, non-zero boolean or
// non-zero numeric. Used by SELECT-projection boolean coercion and by
// the `IF(cond, a, b)` scalar function.
func IsTruthy(v any) bool {
	if v == nil {
		return false
	}
	switch n := v.(type) {
	case bool:
		return n
	case int64:
		return n != 0
	case float64:
		return n != 0
	case string:
		return n != ""
	}
	return true
}

// CompareValues returns -1/0/1 for a < b / a == b / a > b under SQL
// ordering semantics. NULL sorts before non-NULL (sort-site callers
// should honour NULLS FIRST / LAST before reaching here). Numeric
// promotion (int64 ↔ float64) mirrors ORDER BY rules; cross-type
// comparisons fall back to a stable type-name-based order so `=`
// correctly fails without a runtime panic.
func CompareValues(a, b driver.Value) int {
	// NULL ordering: NULL < non-NULL.
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}

	// Exact int64 compare when both are int64 avoids float64 precision loss
	// for values beyond ±2^53.
	if ai, ok1 := a.(int64); ok1 {
		if bi, ok2 := b.(int64); ok2 {
			switch {
			case ai < bi:
				return -1
			case ai > bi:
				return 1
			}
			return 0
		}
	}
	toFloat := func(v any) (float64, bool) {
		switch n := v.(type) {
		case int64:
			return float64(n), true
		case float64:
			return n, true
		}
		return 0, false
	}
	fa, aNum := toFloat(a)
	fb, bNum := toFloat(b)
	if aNum && bNum {
		switch {
		case fa < fb:
			return -1
		case fa > fb:
			return 1
		}
		return 0
	}
	// One numeric and one non-numeric → not equal. SQL rejects cross-type
	// comparison; we return a stable non-zero ordering so `=` fails.
	if aNum != bNum {
		return strings.Compare(reflect.TypeOf(a).String(), reflect.TypeOf(b).String())
	}

	// Same concrete type.
	if reflect.TypeOf(a) == reflect.TypeOf(b) {
		switch av := a.(type) {
		case string:
			return strings.Compare(av, b.(string))
		case bool:
			bv := b.(bool)
			if av == bv {
				return 0
			}
			if !av {
				return -1
			}
			return 1
		case []byte:
			return bytes.Compare(av, b.([]byte))
		}
		// Exotic driver types with equal concrete type: compare via fmt.
		return strings.Compare(fmt.Sprintf("%v", a), fmt.Sprintf("%v", b))
	}

	// Genuinely different types (e.g. string vs bool) — stable non-zero order.
	return strings.Compare(reflect.TypeOf(a).String(), reflect.TypeOf(b).String())
}
