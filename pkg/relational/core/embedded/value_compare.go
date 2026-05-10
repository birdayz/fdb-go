package embedded

import (
	"database/sql/driver"
	"reflect"
	"time"
)

// SQL value-comparison helpers used across every predicate path.
//
//   valuesComparable  — "can `=` / `<` / `>` operate on these types?"
//                       SQL-style type-compatibility check. Numeric
//                       pairs (int64 / float64 mix) OK; same concrete
//                       type OK; anything else → 22000 when callers
//                       try to compare.
//   nullSafeEqual     — IS NOT DISTINCT FROM. Two NULLs equal, NULL
//                       vs non-NULL unequal, else valuesEqual.
//
// Pure helpers — no EmbeddedConnection state. Destined for
// pkg/relational/core/eval/value_compare.go per RFC 021 Phase 1c.

// valuesComparable reports whether two non-NULL driver values can be
// compared by SQL `=`/`<`/`>`/etc. without an explicit CAST. Mirrors
// Java's PromoteValue.isPromotionNeeded outcome: numeric↔numeric is
// always OK (auto-promote int→float); same concrete type is OK;
// everything else is incompatible. Both args must be non-nil.
func valuesComparable(a, b driver.Value) bool {
	_, aInt := a.(int64)
	_, aFloat := a.(float64)
	_, bInt := b.(int64)
	_, bFloat := b.(float64)
	if (aInt || aFloat) && (bInt || bFloat) {
		return true
	}
	_, aTime := a.(time.Time)
	_, bTime := b.(time.Time)
	if aTime || bTime {
		// time.Time is comparable with other time.Time and with strings
		// (ISO format from proto storage). CompareValues handles the
		// cross-type parsing.
		_, aStr := a.(string)
		_, bStr := b.(string)
		return (aTime && bTime) || (aTime && bStr) || (aStr && bTime)
	}
	return reflect.TypeOf(a) == reflect.TypeOf(b)
}

// nullSafeEqual is the underpinning of SQL's `IS NOT DISTINCT FROM`: two
// NULLs are equal, a NULL and a non-NULL are never equal, and two non-NULL
// values are compared by valuesEqual (same type-strict rules as `=`).
func nullSafeEqual(a, b driver.Value) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return valuesEqual(a, b)
}
