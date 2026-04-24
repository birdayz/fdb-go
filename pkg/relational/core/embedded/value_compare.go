package embedded

import (
	"database/sql/driver"
	"reflect"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
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
//   matchSubqueryIN   — SQL §8.4 `fieldVal [NOT] IN (subrows)`.
//                       Kleene-aware: returns triNull when no concrete
//                       match and the subquery contributed a NULL —
//                       the expansion to AND/OR of equalities collapses
//                       to UNKNOWN in that case.
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

// matchSubqueryIN evaluates `fieldVal [NOT] IN (subRows)` per SQL §8.4.
// Returns triTrue/triFalse if a concrete match/non-match can be decided,
// or triNull when no concrete match is found and at least one subquery row
// contributed a NULL (the expansion into an AND/OR chain of equalities
// collapses to UNKNOWN in that case). WHERE callers collapse triNull to
// false; NOT IN sees an UNKNOWN that must not flip to TRUE.
func matchSubqueryIN(fieldVal driver.Value, subRows [][]driver.Value, negated bool) (triBool, error) {
	var hadNull bool
	for _, row := range subRows {
		if len(row) == 0 {
			continue
		}
		v := row[0]
		if v == nil {
			// NULL in subquery result contributes UNKNOWN to the expansion.
			hadNull = true
			continue
		}
		// Cross-type comparison is 22000 per Java alignment (matches the
		// IN-list path's valuesComparable check at evalInPredicateTri).
		// fieldVal != nil is guaranteed by callers — evalInPredicateTri
		// returns triNull early on NULL fieldVal.
		if fieldVal != nil && !valuesComparable(fieldVal, v) {
			return triFalse, api.NewErrorf(api.ErrCodeCannotConvertType,
				"subquery IN: cannot compare %T and %T", fieldVal, v)
		}
		if valuesEqual(fieldVal, v) {
			if negated {
				return triFalse, nil
			}
			return triTrue, nil
		}
	}
	if hadNull {
		return triNull, nil
	}
	if negated {
		return triTrue, nil
	}
	return triFalse, nil
}
