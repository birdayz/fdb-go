package properties

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The record-type horizon mark (RFC-257 WS-F 4.3 item 2) names the keys an
// index scan reaches only past the record-type coordinate of its primary key.
// It changes no answer of the ordering itself; the in-union rule alone asks
// RequestReachesPastRecordTypeHorizon. A renaming carries it, a merge of legs
// starts without it.
func TestRichOrdering_RecordTypeHorizon(t *testing.T) {
	t.Parallel()
	a, pk, z := fieldVal(t, "a"), fieldVal(t, "pk"), fieldVal(t, "z")
	sorted := []OrderingBinding{SortedBinding(ProvidedSortOrderAscending)}
	o := NewRichOrdering(map[values.Value][]OrderingBinding{a: {FixedBinding(nil)}, pk: sorted},
		[]values.Value{a, pk}, NotDistinct()).
		WithPastRecordTypeHorizon([]values.Value{pk, z})
	request := func(parts ...values.Value) *RequestedOrdering {
		requested := make([]RequestedOrderingPart, len(parts))
		for i, part := range parts {
			requested[i] = RequestedOrderingPart{Value: part, SortOrder: RequestedSortOrderAscending}
		}
		return NewRequestedOrdering(requested, DistinctnessPreserveDistinctness, false)
	}

	if o.IsPastRecordTypeHorizon(a) || !o.IsPastRecordTypeHorizon(pk) || o.IsPastRecordTypeHorizon(z) {
		t.Fatalf("marks: a=%v pk=%v z=%v, want only pk (z is no key of the ordering)",
			o.IsPastRecordTypeHorizon(a), o.IsPastRecordTypeHorizon(pk), o.IsPastRecordTypeHorizon(z))
	}
	for _, c := range []struct {
		name  string
		parts []values.Value
		want  bool
	}{
		{"the marked key", []values.Value{pk}, true},
		{"the fixed prefix then the marked key", []values.Value{a, pk}, true},
		{"the fixed prefix alone", []values.Value{a}, false},
		{"no key of the ordering", []values.Value{z}, false},
	} {
		if got := o.RequestReachesPastRecordTypeHorizon(request(c.parts...)); got != c.want {
			t.Errorf("%s: RequestReachesPastRecordTypeHorizon = %v, want %v", c.name, got, c.want)
		}
	}
	if !o.Satisfies(request(pk)) || !o.Satisfies(request(a, pk)) {
		t.Fatal("the mark must not change what the ordering satisfies")
	}
	if NewRichOrdering(map[values.Value][]OrderingBinding{pk: sorted}, []values.Value{pk}, NotDistinct()).
		RequestReachesPastRecordTypeHorizon(request(pk)) {
		t.Fatal("an unmarked ordering reaches no horizon")
	}

	t.Run("a renaming carries the mark", func(t *testing.T) {
		t.Parallel()
		a2, pk2 := fieldVal(t, "a2"), fieldVal(t, "pk2")
		pulled := o.PullUp(map[string]values.Value{values.ExplainValue(a): a2, values.ExplainValue(pk): pk2})
		if pulled.IsPastRecordTypeHorizon(a2) || !pulled.IsPastRecordTypeHorizon(pk2) {
			t.Fatalf("pulled marks: a2=%v pk2=%v, want only pk2",
				pulled.IsPastRecordTypeHorizon(a2), pulled.IsPastRecordTypeHorizon(pk2))
		}
		if !o.WithoutKeys(map[string]struct{}{values.ExplainValue(a): {}}).IsPastRecordTypeHorizon(pk) {
			t.Fatal("dropping another key must keep the mark")
		}
	})

	t.Run("a concatenation keeps each leg's marks", func(t *testing.T) {
		t.Parallel()
		b := fieldVal(t, "b")
		outer := NewRichOrdering(map[values.Value][]OrderingBinding{b: sorted}, []values.Value{b}, DistinctOverAllKeys())
		concat := ConcatOrderings(outer, o)
		if concat.IsPastRecordTypeHorizon(b) || !concat.IsPastRecordTypeHorizon(pk) {
			t.Fatalf("concatenated marks: b=%v pk=%v, want only pk",
				concat.IsPastRecordTypeHorizon(b), concat.IsPastRecordTypeHorizon(pk))
		}
	})

	t.Run("a merge of legs starts without marks", func(t *testing.T) {
		t.Parallel()
		if MergeOrderingsForIntersection(o, o).IsPastRecordTypeHorizon(pk) {
			t.Fatal("an intersection ordering inherited a leg's horizon mark")
		}
	})
}
