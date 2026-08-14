package expr_test

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustExprField(t testing.TB, value values.Value) values.FieldValue {
	t.Helper()
	field, ok := values.AsFieldValue(value)
	if !ok {
		t.Fatalf("got %T, want an admitted exact FieldValue", value)
	}
	return field
}

func mustExprQOV(t testing.TB, value values.Value) values.QuantifiedObjectValue {
	t.Helper()
	qov, ok := values.AsQuantifiedObjectValue(value)
	if !ok {
		t.Fatalf("got %T, want an admitted exact QuantifiedObjectValue", value)
	}
	return qov
}

func requireExprType(t testing.TB, got, want values.Type) {
	t.Helper()
	if got == nil || want == nil || !got.Equals(want) {
		t.Fatalf("type = %v, want exact %v", got, want)
	}
}

func exprAccessorName(t testing.TB, path values.FieldPathView, index int) string {
	t.Helper()
	accessor, ok := path.Accessor(index)
	if !ok {
		t.Fatalf("accessor %d absent from path %v", index, path.Ordinals())
	}
	name, ok := accessor.DisplayName()
	if !ok {
		t.Fatalf("accessor %d has no display name", index)
	}
	return name
}
