package values

import "testing"

func TestRecordTypeValue_ExtractsFromMap(t *testing.T) {
	t.Parallel()
	child := &constMapValue{m: map[string]any{
		"_recordType": "Order",
		"id":          int64(42),
	}}
	v := NewRecordTypeValue(child)
	if got := v.Evaluate(nil); got != "Order" {
		t.Fatalf("RecordTypeValue.Evaluate = %v, want 'Order'", got)
	}
}

func TestRecordTypeValue_NilChild(t *testing.T) {
	t.Parallel()
	v := NewRecordTypeValue(nil)
	if got := v.Evaluate(nil); got != nil {
		t.Fatalf("nil child = %v, want nil", got)
	}
}

func TestRecordTypeValue_MissingDiscriminator(t *testing.T) {
	t.Parallel()
	child := &constMapValue{m: map[string]any{"id": int64(1)}}
	v := NewRecordTypeValue(child)
	if got := v.Evaluate(nil); got != nil {
		t.Fatalf("missing _recordType = %v, want nil", got)
	}
}

func TestRecordTypeValue_TypeIsNotNullLong(t *testing.T) {
	t.Parallel()
	v := NewRecordTypeValue(LiteralValue(nil))
	if !v.Type().Equals(NotNullLong) {
		t.Fatalf("Type=%v, want NotNullLong", v.Type())
	}
}
