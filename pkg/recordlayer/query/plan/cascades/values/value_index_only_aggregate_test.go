package values

import "testing"

func TestIndexOnlyAggregateOp_String(t *testing.T) {
	t.Parallel()
	cases := map[IndexOnlyAggregateOp]string{
		IndexOnlyMaxEverLong:     "MAX_EVER_LONG",
		IndexOnlyMinEverLong:     "MIN_EVER_LONG",
		IndexOnlyAggregateOp(99): "INVALID",
	}
	for op, want := range cases {
		if got := op.String(); got != want {
			t.Errorf("Op(%d).String() = %q, want %q", op, got, want)
		}
	}
}

func TestIndexOnlyAggregateValue_Type(t *testing.T) {
	t.Parallel()
	v := NewIndexOnlyAggregateValue(IndexOnlyMaxEverLong, &FieldValue{Field: "qty", Typ: NotNullLong})
	if !v.Type().Equals(NotNullLong) {
		t.Fatalf("Type = %v, want NotNullLong (carries from child)", v.Type())
	}
}

func TestIndexOnlyAggregateValue_NilChildFallsBackToUnknown(t *testing.T) {
	t.Parallel()
	v := NewIndexOnlyAggregateValue(IndexOnlyMaxEverLong, nil)
	if !v.Type().Equals(UnknownType) {
		t.Fatalf("Type = %v, want UnknownType (nil child fallback)", v.Type())
	}
}

func TestIndexOnlyAggregateValue_Name(t *testing.T) {
	t.Parallel()
	cases := map[IndexOnlyAggregateOp]string{
		IndexOnlyMaxEverLong: "MAX_EVER_LONG",
		IndexOnlyMinEverLong: "MIN_EVER_LONG",
	}
	for op, want := range cases {
		v := NewIndexOnlyAggregateValue(op, nil)
		if got := v.Name(); got != want {
			t.Errorf("Op=%v Name = %q, want %q", op, got, want)
		}
	}
}

func TestIndexOnlyAggregateValue_Children(t *testing.T) {
	t.Parallel()
	c := &FieldValue{Field: "x", Typ: NotNullLong}
	v := NewIndexOnlyAggregateValue(IndexOnlyMaxEverLong, c)
	cs := v.Children()
	if len(cs) != 1 || cs[0] != c {
		t.Fatalf("Children = %v, want [c]", cs)
	}
}

func TestIndexOnlyAggregateValue_NilChildEmptyChildren(t *testing.T) {
	t.Parallel()
	v := NewIndexOnlyAggregateValue(IndexOnlyMaxEverLong, nil)
	if got := v.Children(); len(got) != 0 {
		t.Fatalf("Children(nil) len = %d, want 0", len(got))
	}
}

func TestIndexOnlyAggregateValue_EvaluateIsPlaceholder(t *testing.T) {
	t.Parallel()
	v := NewIndexOnlyAggregateValue(IndexOnlyMaxEverLong, &FieldValue{Field: "x", Typ: NotNullLong})
	if got := mustEvalForTest(v, nil); got != nil {
		t.Fatalf("Evaluate = %v, want nil (compile-time-only)", got)
	}
}

func TestIndexOnlyAggregateValue_IsIndexOnly(t *testing.T) {
	t.Parallel()
	v := NewIndexOnlyAggregateValue(IndexOnlyMaxEverLong, nil)
	if !v.IsIndexOnly() {
		t.Fatal("IsIndexOnly = false, want true")
	}
}

func TestIndexOnlyAggregateValue_GetIndexTypeName(t *testing.T) {
	t.Parallel()
	cases := map[IndexOnlyAggregateOp]string{
		IndexOnlyMaxEverLong: "MAX_EVER_LONG",
		IndexOnlyMinEverLong: "MIN_EVER_LONG",
	}
	for op, want := range cases {
		v := NewIndexOnlyAggregateValue(op, nil)
		if got := v.GetIndexTypeName(); got != want {
			t.Errorf("Op=%v GetIndexTypeName = %q, want %q", op, got, want)
		}
	}
}

func TestIndexOnlyAggregateValue_ImplementsIndexableAggregate(t *testing.T) {
	t.Parallel()
	v := NewIndexOnlyAggregateValue(IndexOnlyMaxEverLong, nil)
	var _ IndexableAggregate = v
	iav, ok := Value(v).(IndexableAggregate)
	if !ok {
		t.Fatal("IndexOnlyAggregateValue should implement IndexableAggregate")
	}
	if iav.GetIndexTypeName() != "MAX_EVER_LONG" {
		t.Fatalf("via interface: GetIndexTypeName = %q", iav.GetIndexTypeName())
	}
}

func TestIndexOnlyAggregateValue_WithChildren(t *testing.T) {
	t.Parallel()
	original := NewIndexOnlyAggregateValue(IndexOnlyMaxEverLong,
		&FieldValue{Field: "x", Typ: NotNullLong})
	rebuilt := original.WithChildren([]Value{
		&FieldValue{Field: "y", Typ: NotNullLong},
	})
	if rebuilt.Op != IndexOnlyMaxEverLong {
		t.Fatalf("rebuilt.Op = %v, want MAX_EVER_LONG", rebuilt.Op)
	}
	if fv, ok := rebuilt.Child.(*FieldValue); !ok || fv.Field != "y" {
		t.Fatalf("rebuilt.Child = %v, want FieldValue(y)", rebuilt.Child)
	}
}
