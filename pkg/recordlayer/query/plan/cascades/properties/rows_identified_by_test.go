package properties

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RowsIdentifiedBy is the soundness gate of a merge that dedups on a key: each
// claim must still hold AND sit inside the key. Every claim kind, both sides of
// the inside-the-key test, a projection that drops a claim coordinate, and the
// whole-row (coordinate-free) distinctness that no key may borrow.
func TestRichOrdering_RowsIdentifiedBy(t *testing.T) {
	t.Parallel()
	a, s, pk := fieldVal(t, "a"), fieldVal(t, "s"), fieldVal(t, "pk")
	sorted := []OrderingBinding{SortedBinding(ProvidedSortOrderAscending)}
	// a is the equality-bound prefix, as under an IN probe.
	ordering := func(claim CoordinateBoundClaim) *RichOrdering {
		return NewRichOrdering(map[values.Value][]OrderingBinding{a: {FixedBinding(nil)}, s: sorted, pk: sorted},
			[]values.Value{a, s, pk}, claim)
	}
	all := []values.Value{a, s, pk}

	cases := []struct {
		name string
		o    *RichOrdering
		keys []values.Value
		want bool
	}{
		{"no claim", ordering(NotDistinct()), all, false},
		{"distinct over the key", ordering(DistinctOverAllKeys()), all, true},
		{"distinct over coordinates the key omits", ordering(DistinctOverAllKeys()), []values.Value{s, pk}, false},
		{"storage key over the key", ordering(NotDistinct()).WithStorageKeyComplete(true), all, true},
		{"storage key the key omits", ordering(NotDistinct()).WithStorageKeyComplete(true), []values.Value{a, s}, false},
		{"record identity inside a narrower key", ordering(NotDistinct()).WithRecordIdentity([]values.Value{pk}), []values.Value{s, pk}, true},
		{"record identity the key omits", ordering(NotDistinct()).WithRecordIdentity([]values.Value{pk}), []values.Value{a, s}, false},
		{"record identity over a non-coordinate", ordering(NotDistinct()).WithRecordIdentity([]values.Value{fieldVal(t, "z")}), all, false},
		{"nil ordering", nil, all, false},
	}
	for _, c := range cases {
		if got := c.o.RowsIdentifiedBy(c.keys); got != c.want {
			t.Errorf("%s: RowsIdentifiedBy = %v, want %v", c.name, got, c.want)
		}
	}

	t.Run("a projection keeping the primary key keeps identity", func(t *testing.T) {
		t.Parallel()
		o := ordering(NotDistinct()).WithStorageKeyComplete(true).WithRecordIdentity([]values.Value{pk})
		s2, pk2 := fieldVal(t, "s2"), fieldVal(t, "pk2")
		pulled := o.PullUp(map[string]values.Value{values.ExplainValue(s): s2, values.ExplainValue(pk): pk2})
		if pulled.StorageKeyIsComplete() {
			t.Fatal("dropping a storage-key coordinate kept the completeness claim")
		}
		if !pulled.RowsIdentifiedBy([]values.Value{s2, pk2}) {
			t.Fatal("a projection that keeps every primary-key coordinate lost record identity")
		}
	})
	t.Run("a projection dropping the primary key loses every claim", func(t *testing.T) {
		t.Parallel()
		o := ordering(DistinctOverAllKeys()).WithStorageKeyComplete(true).WithRecordIdentity([]values.Value{pk})
		a2, s2 := fieldVal(t, "a2"), fieldVal(t, "s2")
		pulled := o.PullUp(map[string]values.Value{values.ExplainValue(a): a2, values.ExplainValue(s): s2})
		if pulled.RowsIdentifiedBy([]values.Value{a2, s2}) {
			t.Fatal("rows of a projection without the primary key were reported identified by the survivors")
		}
	})
	t.Run("whole-row distinctness identifies no key", func(t *testing.T) {
		t.Parallel()
		// A claim bound to no coordinates (a keyless inner's, carried by a
		// concatenation) says whole rows are distinct; equal keys may still
		// belong to different rows.
		inner := NewRichOrdering(nil, nil, DistinctOverAllKeys())
		o := NewRichOrdering(map[values.Value][]OrderingBinding{a: sorted}, []values.Value{a}, inner.DistinctnessClaim())
		if !o.IsDistinct() {
			t.Fatal("fixture: the coordinate-free claim should hold")
		}
		if o.RowsIdentifiedBy([]values.Value{a}) {
			t.Fatal("whole-row distinctness was read as distinctness over a key")
		}
	})
}
