package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func fieldVal(name string) values.Value {
	return &values.FieldValue{Field: name, Typ: values.UnknownType}
}

func TestRichOrdering_EmptyOrdering(t *testing.T) {
	t.Parallel()
	o := EmptyOrdering()
	if len(o.GetKeys()) != 0 {
		t.Fatal("empty ordering should have no keys")
	}
	if o.IsDistinct() {
		t.Fatal("empty ordering should not be distinct")
	}
}

func TestRichOrdering_Satisfies_EmptyRequest(t *testing.T) {
	t.Parallel()
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			fieldVal("a"): {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{fieldVal("a")},
		false,
	)
	req := PreserveOrdering()
	if !o.Satisfies(req) {
		t.Fatal("any ordering should satisfy preserve")
	}
}

func TestRichOrdering_Satisfies_SingleKey(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{a},
		false,
	)
	req := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: a, SortOrder: RequestedSortOrderAscending},
	}, DistinctnessNotDistinct, false)

	if !o.Satisfies(req) {
		t.Fatal("ascending ordering should satisfy ascending request")
	}
}

func TestRichOrdering_Satisfies_DirectionMismatch(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{a},
		false,
	)
	req := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: a, SortOrder: RequestedSortOrderDescending},
	}, DistinctnessNotDistinct, false)

	if o.Satisfies(req) {
		t.Fatal("ascending ordering should NOT satisfy descending request")
	}
}

func TestRichOrdering_Satisfies_AnyDirection(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {SortedBinding(ProvidedSortOrderDescending)},
		},
		[]values.Value{a},
		false,
	)
	req := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: a, SortOrder: RequestedSortOrderAny},
	}, DistinctnessNotDistinct, false)

	if !o.Satisfies(req) {
		t.Fatal("descending ordering should satisfy ANY direction request")
	}
}

func TestRichOrdering_Satisfies_SkipsFixedKeys(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	b := fieldVal("b")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {FixedBinding("eq-5")},
			b: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{a, b},
		false,
	)
	req := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: b, SortOrder: RequestedSortOrderAscending},
	}, DistinctnessNotDistinct, false)

	if !o.Satisfies(req) {
		t.Fatal("should skip fixed key 'a' and satisfy request on 'b'")
	}
}

func TestRichOrdering_Satisfies_WrongKey(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	b := fieldVal("b")
	c := fieldVal("c")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {SortedBinding(ProvidedSortOrderAscending)},
			b: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{a, b},
		false,
	)
	req := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: c, SortOrder: RequestedSortOrderAscending},
	}, DistinctnessNotDistinct, false)

	if o.Satisfies(req) {
		t.Fatal("should NOT satisfy request for key not in ordering")
	}
}

func TestSortOrderOf_AllSorted(t *testing.T) {
	t.Parallel()
	bindings := []OrderingBinding{
		SortedBinding(ProvidedSortOrderAscending),
		SortedBinding(ProvidedSortOrderAscending),
	}
	if SortOrderOf(bindings) != ProvidedSortOrderAscending {
		t.Fatal("all ascending should return ascending")
	}
}

func TestSortOrderOf_MixedSorted(t *testing.T) {
	t.Parallel()
	bindings := []OrderingBinding{
		SortedBinding(ProvidedSortOrderAscending),
		SortedBinding(ProvidedSortOrderDescending),
	}
	if SortOrderOf(bindings) != ProvidedSortOrderFixed {
		t.Fatal("mixed sorted should return fixed")
	}
}

func TestSortOrderOf_AllFixed(t *testing.T) {
	t.Parallel()
	bindings := []OrderingBinding{
		FixedBinding("eq"),
	}
	if SortOrderOf(bindings) != ProvidedSortOrderFixed {
		t.Fatal("all fixed should return fixed")
	}
}

func TestAreAllBindingsFixed(t *testing.T) {
	t.Parallel()
	if !AreAllBindingsFixed([]OrderingBinding{FixedBinding("a"), FixedBinding("b")}) {
		t.Fatal("all fixed should return true")
	}
	if AreAllBindingsFixed([]OrderingBinding{FixedBinding("a"), SortedBinding(ProvidedSortOrderAscending)}) {
		t.Fatal("mixed should return false")
	}
	if !AreAllBindingsFixed(nil) {
		t.Fatal("empty should return true")
	}
}

func TestRichOrdering_IsSingularNonFixedValue(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	b := fieldVal("b")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {SortedBinding(ProvidedSortOrderAscending)},
			b: {FixedBinding("eq")},
		},
		[]values.Value{a, b},
		false,
	)
	if !o.IsSingularNonFixedValue(a) {
		t.Fatal("a should be singular non-fixed")
	}
	if o.IsSingularNonFixedValue(b) {
		t.Fatal("b is fixed, should not be singular non-fixed")
	}
}
