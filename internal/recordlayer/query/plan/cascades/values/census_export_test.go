package values

import "testing"

// The external census tests drive the legacy-private lazy FieldValue, which no
// exported constructor builds; these are their only way in.

func LazyFieldValueForTest(field string, child Value) Value {
	return &fieldValue{Field: field, Child: child}
}

func BakedFieldValueForTest(field string, ordinal int, child Value) Value {
	return &fieldValue{
		Field:    field,
		Child:    child,
		Resolved: &fieldPath{Accessors: []resolvedAccessor{{Field: field, Ordinal: ordinal}}},
	}
}

func QOVForTest(t testing.TB, correlation CorrelationIdentifier) Value {
	t.Helper()
	return mustQOV(t, correlation)
}
