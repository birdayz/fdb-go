package properties

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/combinatorics"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func fieldVal(name string) values.Value {
	return &values.FieldValue{Field: name, Typ: values.UnknownType}
}

// outputSlot is a reference to a record constructor's OUTPUT SLOT — the shape a
// resolved reference to a projection output carries. Push-down selects the
// member by that ordinal (RFC-197 item 3); fieldVal above is the LAZY carrier,
// which pushes down to nothing.
func outputSlot(name string, ordinal int) values.Value {
	return values.NewFieldValueWithResolvedOrdinal(name, ordinal, values.UnknownType)
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

func TestConcatOrderings_Basic(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	b := fieldVal("b")
	c := fieldVal("c")

	outer := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{a},
		true,
	)
	inner := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			b: {SortedBinding(ProvidedSortOrderDescending)},
			c: {FixedBinding("eq")},
		},
		[]values.Value{b, c},
		false,
	)

	result := ConcatOrderings(outer, inner)
	if len(result.GetKeys()) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(result.GetKeys()))
	}
	if !valuesEqual(result.GetKeys()[0], a) {
		t.Fatal("first key should be 'a'")
	}
	if !valuesEqual(result.GetKeys()[1], b) {
		t.Fatal("second key should be 'b'")
	}
}

func TestConcatOrderings_SkipsDuplicates(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")

	outer := NewRichOrdering(
		map[values.Value][]OrderingBinding{a: {SortedBinding(ProvidedSortOrderAscending)}},
		[]values.Value{a},
		true,
	)
	inner := NewRichOrdering(
		map[values.Value][]OrderingBinding{a: {SortedBinding(ProvidedSortOrderDescending)}},
		[]values.Value{a},
		false,
	)

	result := ConcatOrderings(outer, inner)
	if len(result.GetKeys()) != 1 {
		t.Fatalf("expected 1 key (no duplicate), got %d", len(result.GetKeys()))
	}
}

func TestConcatOrderings_RejectsNonDistinctOuter(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	outer := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{a},
		false,
	)

	defer func() {
		if recover() == nil {
			t.Fatal("expected concat to reject a non-distinct outer ordering")
		}
	}()
	ConcatOrderings(outer, EmptyOrdering())
}

func TestMergeOrderings_CompatibleDirections(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	b := fieldVal("b")

	o1 := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {SortedBinding(ProvidedSortOrderAscending)},
			b: {SortedBinding(ProvidedSortOrderDescending)},
		},
		[]values.Value{a, b},
		false,
	)
	o2 := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {SortedBinding(ProvidedSortOrderAscending)},
			b: {SortedBinding(ProvidedSortOrderDescending)},
		},
		[]values.Value{a, b},
		false,
	)

	merged := MergeOrderings(o1, o2)
	if len(merged.GetKeys()) != 2 {
		t.Fatalf("expected 2 keys in merged, got %d", len(merged.GetKeys()))
	}
}

func TestMergeOrderings_IncompatibleDirections(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")

	o1 := NewRichOrdering(
		map[values.Value][]OrderingBinding{a: {SortedBinding(ProvidedSortOrderAscending)}},
		[]values.Value{a},
		false,
	)
	o2 := NewRichOrdering(
		map[values.Value][]OrderingBinding{a: {SortedBinding(ProvidedSortOrderDescending)}},
		[]values.Value{a},
		false,
	)

	merged := MergeOrderings(o1, o2)
	if len(merged.GetKeys()) != 0 {
		t.Fatalf("expected 0 keys in merged (directions incompatible), got %d", len(merged.GetKeys()))
	}
}

func TestEnumerateSatisfyingKeys_SimpleMatch(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	b := fieldVal("b")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {SortedBinding(ProvidedSortOrderAscending)},
			b: {SortedBinding(ProvidedSortOrderDescending)},
		},
		[]values.Value{a, b},
		false,
	)
	req := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: a, SortOrder: RequestedSortOrderAscending},
	}, DistinctnessNotDistinct, false)

	result := o.EnumerateSatisfyingComparisonKeyValues(req)
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	if len(result[0]) != 2 {
		t.Fatalf("expected 2 keys in result, got %d", len(result[0]))
	}
}

func TestEnumerateSatisfyingKeys_DirectionMismatch(t *testing.T) {
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

	result := o.EnumerateSatisfyingComparisonKeyValues(req)
	if result != nil {
		t.Fatal("should return nil on direction mismatch")
	}
}

func TestEnumerateSatisfyingKeys_PreserveReturnsAllKeys(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{a},
		false,
	)
	result := o.EnumerateSatisfyingComparisonKeyValues(PreserveOrdering())
	if len(result) != 1 || len(result[0]) != 1 {
		t.Fatal("preserve should return all keys")
	}
}

func TestSatisfies_FixedKeyReorderableInPartialOrder(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	b := fieldVal("b")
	c := fieldVal("c")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {FixedBinding("eq-3")},
			b: {SortedBinding(ProvidedSortOrderAscending)},
			c: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{a, b, c},
		false,
	)

	// b,c is valid because a is fixed (independent in partial order)
	req1 := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: b, SortOrder: RequestedSortOrderAscending},
		{Value: c, SortOrder: RequestedSortOrderAscending},
	}, DistinctnessNotDistinct, false)
	if !o.Satisfies(req1) {
		t.Fatal("should satisfy b,c (a is fixed, freely reorderable)")
	}

	// a,b,c is also valid
	req2 := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: a, SortOrder: RequestedSortOrderAny},
		{Value: b, SortOrder: RequestedSortOrderAscending},
		{Value: c, SortOrder: RequestedSortOrderAscending},
	}, DistinctnessNotDistinct, false)
	if !o.Satisfies(req2) {
		t.Fatal("should satisfy a,b,c")
	}
}

func TestEnumerateSatisfyingKeys_MultiplePermsWithFixedKeys(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	b := fieldVal("b")
	c := fieldVal("c")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {FixedBinding("eq")},
			b: {SortedBinding(ProvidedSortOrderAscending)},
			c: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{a, b, c},
		false,
	)
	req := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: b, SortOrder: RequestedSortOrderAscending},
	}, DistinctnessNotDistinct, false)

	results := o.EnumerateSatisfyingComparisonKeyValues(req)
	if len(results) == 0 {
		t.Fatal("should find at least one ordering")
	}
	// With a fixed, valid orderings include both [a,b,c] and [b,a,c]
	// since a can float freely
	if len(results) < 2 {
		t.Logf("found %d orderings (expected >=2 since 'a' is freely reorderable)", len(results))
	}
}

func TestDirectionalOrderingParts_Basic(t *testing.T) {
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
	req := NewRequestedOrdering(nil, DistinctnessNotDistinct, false)
	parts := o.DirectionalOrderingParts([]values.Value{a, b}, req, ProvidedSortOrderFixed)
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	if parts[0].SortOrder != ProvidedSortOrderAscending {
		t.Fatal("first part should be ascending (from binding)")
	}
	if parts[1].SortOrder != ProvidedSortOrderFixed {
		t.Fatal("second part should be fixed (from default)")
	}
}

func TestNaturalComparisonKeyValues_OnlyRawTupleDirections(t *testing.T) {
	t.Parallel()

	key := fieldVal("key")
	tests := []struct {
		name    string
		order   ProvidedSortOrder
		reverse bool
		ok      bool
	}{
		{name: "forward ascending", order: ProvidedSortOrderAscending, ok: true},
		{name: "reverse descending", order: ProvidedSortOrderDescending, reverse: true, ok: true},
		{name: "forward descending needs ordered bytes", order: ProvidedSortOrderDescending},
		{name: "reverse ascending needs ordered bytes", order: ProvidedSortOrderAscending, reverse: true},
		{name: "ascending counterflow needs ordered bytes", order: ProvidedSortOrderAscendingNullsLast},
		{name: "descending counterflow needs ordered bytes", order: ProvidedSortOrderDescendingNullsFirst, reverse: true},
		{name: "fixed is not a physical comparison key", order: ProvidedSortOrderFixed},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, ok := NaturalComparisonKeyValues(
				[]ProvidedOrderingPart{{Value: key, SortOrder: test.order}},
				test.reverse,
			)
			if ok != test.ok {
				t.Fatalf("NaturalComparisonKeyValues() ok = %v, want %v", ok, test.ok)
			}
			if test.ok {
				if len(got) != 1 || !values.ValuesStructurallyEqual(got[0], key) {
					t.Fatalf("NaturalComparisonKeyValues() = %#v, want the raw key", got)
				}
			} else if got != nil {
				t.Fatalf("unsupported direction returned comparison keys: %#v", got)
			}
		})
	}
}

func TestIntersectionOrdering_ExcludesFixedKeysAndPreservesDescendingRequest(t *testing.T) {
	t.Parallel()

	leftFixed := fieldVal("a")
	rightFixed := fieldVal("b")
	sortKey := fieldVal("sort_key")
	id := fieldVal("id")

	left := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			leftFixed: {FixedBinding("a = ?")},
			sortKey:   {SortedBinding(ProvidedSortOrderDescending)},
			id:        {SortedBinding(ProvidedSortOrderDescending)},
		},
		[]values.Value{leftFixed, sortKey, id},
		false,
	)
	right := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			rightFixed: {FixedBinding("b = ?")},
			sortKey:    {SortedBinding(ProvidedSortOrderDescending)},
			id:         {SortedBinding(ProvidedSortOrderDescending)},
		},
		[]values.Value{rightFixed, sortKey, id},
		false,
	)

	merged := MergeOrderingsForIntersection(left, right)
	if !merged.IsDistinct() {
		t.Fatal("intersection ordering must be distinct")
	}
	counts := make(map[string]int)
	for _, key := range merged.GetKeys() {
		counts[values.ColumnNameValue(key)]++
	}
	for _, name := range []string{"a", "b", "sort_key", "id"} {
		if counts[name] != 1 {
			t.Fatalf("merged key %q occurs %d times, want exactly once; all counts = %v", name, counts[name], counts)
		}
	}

	// The request is deliberately rebuilt with a different case and Value
	// identity. Direction lookup must use semantic/normalized key identity.
	requestedSort := fieldVal("SORT_KEY")
	requested := NewRequestedOrdering(
		[]RequestedOrderingPart{{
			Value:     requestedSort,
			SortOrder: RequestedSortOrderDescending,
		}},
		DistinctnessNotDistinct,
		false,
	)
	keys := merged.EnumerateSatisfyingIntersectionComparisonKeyValues(requested)
	if len(keys) != 1 || len(keys[0]) != 2 {
		t.Fatalf("intersection comparison keys = %#v, want exactly [sort_key, id]", keys)
	}
	if !values.ValuesStructurallyEqual(keys[0][0], sortKey) ||
		!values.ValuesStructurallyEqual(keys[0][1], id) {
		t.Fatalf("intersection comparison keys = [%s, %s], want [sort_key, id]",
			values.ExplainValue(keys[0][0]), values.ExplainValue(keys[0][1]))
	}

	parts := merged.DirectionalOrderingParts(keys[0], requested, ProvidedSortOrderFixed)
	if len(parts) != 2 ||
		parts[0].SortOrder != ProvidedSortOrderDescending ||
		parts[1].SortOrder != ProvidedSortOrderDescending {
		t.Fatalf("directional parts = %#v, want two descending parts", parts)
	}
}

func TestIntersectionOrdering_FixedPrefixFreesPrimaryKey(t *testing.T) {
	t.Parallel()

	prefix := fieldVal("a")
	primaryKey := fieldVal("pk")
	left := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			prefix:     {SortedBinding(ProvidedSortOrderAscending)},
			primaryKey: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{prefix, primaryKey},
		false,
	)
	right := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			prefix:     {FixedBinding("a = ?")},
			primaryKey: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{prefix, primaryKey},
		false,
	)

	merged := MergeOrderingsForIntersection(left, right)
	prefixKey := values.ExplainValue(prefix)
	primaryKeyKey := values.ExplainValue(primaryKey)
	if merged.OrderingSet().DependencyMap().Contains(primaryKeyKey, prefixKey) {
		t.Fatalf("fixed prefix retained stale dependency %s <- %s", primaryKeyKey, prefixKey)
	}

	requested := NewRequestedOrdering(
		[]RequestedOrderingPart{{
			Value:     primaryKey,
			SortOrder: RequestedSortOrderAscending,
		}},
		DistinctnessNotDistinct,
		false,
	)
	keys := merged.EnumerateSatisfyingIntersectionComparisonKeyValues(requested)
	if len(keys) != 1 || len(keys[0]) != 1 ||
		!values.ValuesStructurallyEqual(keys[0][0], primaryKey) {
		t.Fatalf("comparison keys = %#v, want exactly [pk]", keys)
	}
}

func TestRichOrdering_DoesNotCollapseDifferentBakedOrdinals(t *testing.T) {
	t.Parallel()

	provided := values.NewFieldValueWithResolvedOrdinal(
		"DUP",
		0,
		values.UnknownType,
	)
	requestedOtherSlot := values.NewFieldValueWithResolvedOrdinal(
		"DUP",
		1,
		values.UnknownType,
	)
	ordering := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			provided: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{provided},
		false,
	)
	requested := NewRequestedOrdering(
		[]RequestedOrderingPart{{
			Value:     requestedOtherSlot,
			SortOrder: RequestedSortOrderAscending,
		}},
		DistinctnessNotDistinct,
		false,
	)

	if ordering.Satisfies(requested) {
		t.Fatal("ordering on ordinal 0 must not satisfy the same display name at ordinal 1")
	}
	if got := ordering.EnumerateSatisfyingIntersectionComparisonKeyValues(requested); got != nil {
		t.Fatalf("different baked ordinal produced intersection comparison keys: %#v", got)
	}

	lazyRequest := NewRequestedOrdering(
		[]RequestedOrderingPart{{
			Value:     fieldVal("dup"),
			SortOrder: RequestedSortOrderAscending,
		}},
		DistinctnessNotDistinct,
		false,
	)
	if !ordering.Satisfies(lazyRequest) {
		t.Fatal("a lazy flat request should still bridge to its uniquely baked provider")
	}
}

func TestConcatOrderings_DistinctnessComesFromInner(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	outer := NewRichOrdering(
		map[values.Value][]OrderingBinding{a: {SortedBinding(ProvidedSortOrderAscending)}},
		[]values.Value{a}, true,
	)
	inner := EmptyOrdering()
	result := ConcatOrderings(outer, inner)
	if result.IsDistinct() {
		t.Fatal("concat must not propagate outer distinctness to duplicate inner rows")
	}

	innerDistinct := NewRichOrdering(nil, nil, true)
	if !ConcatOrderings(outer, innerDistinct).IsDistinct() {
		t.Fatal("concat should inherit distinctness from the inner ordering")
	}
}

func TestRichOrdering_Satisfies_DistinctRequest(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	requested := NewRequestedOrdering(
		[]RequestedOrderingPart{{
			Value:     a,
			SortOrder: RequestedSortOrderAscending,
		}},
		DistinctnessDistinct,
		false,
	)
	nonDistinct := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{a},
		false,
	)
	if nonDistinct.Satisfies(requested) {
		t.Fatal("a non-distinct provided ordering must not satisfy a distinct request")
	}
	distinct := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{a},
		true,
	)
	if !distinct.Satisfies(requested) {
		t.Fatal("a matching distinct provided ordering should satisfy the request")
	}
}

func TestCreateUnionOrdering_DeepCopy(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{a: {SortedBinding(ProvidedSortOrderAscending)}},
		[]values.Value{a}, true,
	)
	u := CreateUnionOrdering(o)
	if !u.IsDistinct() {
		t.Fatal("union copy should preserve distinct")
	}
	if len(u.GetKeys()) != 1 {
		t.Fatal("union copy should preserve keys")
	}
}

func TestMergeOrderings_DisjointKeys(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	b := fieldVal("b")
	o1 := NewRichOrdering(
		map[values.Value][]OrderingBinding{a: {SortedBinding(ProvidedSortOrderAscending)}},
		[]values.Value{a}, false,
	)
	o2 := NewRichOrdering(
		map[values.Value][]OrderingBinding{b: {SortedBinding(ProvidedSortOrderAscending)}},
		[]values.Value{b}, false,
	)
	merged := MergeOrderings(o1, o2)
	if len(merged.GetKeys()) != 0 {
		t.Fatalf("disjoint keys should produce empty merge, got %d keys", len(merged.GetKeys()))
	}
}

func TestEnumerateCompatibleRequestedOrderings_Basic(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	b := fieldVal("b")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {SortedBinding(ProvidedSortOrderAscending)},
			b: {SortedBinding(ProvidedSortOrderDescending)},
		},
		[]values.Value{a, b},
		false,
	)
	req := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: a, SortOrder: RequestedSortOrderAscending},
	}, DistinctnessNotDistinct, false)

	results := o.EnumerateCompatibleRequestedOrderings(req)
	if len(results) == 0 {
		t.Fatal("expected at least one compatible ordering")
	}
	if len(results[0]) != 2 {
		t.Fatalf("expected full-length ordering (2 keys), got %d", len(results[0]))
	}
	if results[0][0].SortOrder != RequestedSortOrderAscending {
		t.Fatal("first part should be ascending")
	}
	if results[0][1].SortOrder != RequestedSortOrderDescending {
		t.Fatal("second part should be descending")
	}
}

func TestEnumerateCompatibleRequestedOrderings_IncompatibleDirection(t *testing.T) {
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

	results := o.EnumerateCompatibleRequestedOrderings(req)
	if results != nil {
		t.Fatal("should return nil for incompatible direction")
	}
}

func TestSatisfiesGroupingValues_Basic(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	b := fieldVal("b")
	c := fieldVal("c")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {SortedBinding(ProvidedSortOrderAscending)},
			b: {SortedBinding(ProvidedSortOrderAscending)},
			c: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{a, b, c},
		false,
	)

	gv := map[string]struct{}{
		values.ExplainValue(a): {},
		values.ExplainValue(b): {},
	}
	if !o.SatisfiesGroupingValues(gv) {
		t.Fatal("a,b should be a valid prefix")
	}
}

func TestSatisfiesGroupingValues_Empty(t *testing.T) {
	t.Parallel()
	o := EmptyOrdering()
	if !o.SatisfiesGroupingValues(map[string]struct{}{}) {
		t.Fatal("empty grouping values should always satisfy")
	}
}

func TestSatisfiesGroupingValues_MissingValue(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{a},
		false,
	)
	gv := map[string]struct{}{
		values.ExplainValue(fieldVal("z")): {},
	}
	if o.SatisfiesGroupingValues(gv) {
		t.Fatal("should not satisfy with missing value")
	}
}

func TestSatisfiesGroupingValues_FixedKeysSkippable(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	b := fieldVal("b")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {FixedBinding("eq")},
			b: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{a, b},
		false,
	)
	gv := map[string]struct{}{
		values.ExplainValue(b): {},
	}
	if !o.SatisfiesGroupingValues(gv) {
		t.Fatal("should satisfy: fixed 'a' is independent, 'b' forms valid prefix")
	}
}

func TestMergeOrderings_MergesFixedBindings(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")

	o1 := NewRichOrdering(
		map[values.Value][]OrderingBinding{a: {FixedBinding("eq-5")}},
		[]values.Value{a},
		false,
	)
	o2 := NewRichOrdering(
		map[values.Value][]OrderingBinding{a: {FixedBinding("eq-5")}},
		[]values.Value{a},
		false,
	)

	merged := MergeOrderings(o1, o2)
	if len(merged.GetKeys()) != 1 {
		t.Fatalf("expected 1 key in merged (both fixed), got %d", len(merged.GetKeys()))
	}
}

func TestRichOrdering_PullUp(t *testing.T) {
	t.Parallel()
	keyA := fieldVal("a")
	keyB := fieldVal("b")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			keyA: {SortedBinding(ProvidedSortOrderAscending)},
			keyB: {SortedBinding(ProvidedSortOrderDescending)},
		},
		[]values.Value{keyA, keyB}, false,
	)

	renamed := fieldVal("x")
	mapping := map[string]values.Value{"a": renamed}
	pulled := o.PullUp(mapping)

	if len(pulled.GetKeys()) != 1 {
		t.Fatalf("expected 1 key after pullup, got %d", len(pulled.GetKeys()))
	}
	if values.ExplainValue(pulled.GetKeys()[0]) != "x" {
		t.Fatalf("expected key 'x', got %q", values.ExplainValue(pulled.GetKeys()[0]))
	}
	bindings := pulled.GetBindingMap()[renamed]
	if len(bindings) != 1 || SortOrderOf(bindings) != ProvidedSortOrderAscending {
		t.Fatal("expected ascending binding preserved")
	}
}

func TestRichOrdering_PullUp_AllMapped(t *testing.T) {
	t.Parallel()
	keyA := fieldVal("a")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			keyA: {FixedBinding(nil)},
		},
		[]values.Value{keyA}, true,
	)

	mapped := fieldVal("b")
	pulled := o.PullUp(map[string]values.Value{"a": mapped})
	if len(pulled.GetKeys()) != 1 {
		t.Fatalf("expected 1 key, got %d", len(pulled.GetKeys()))
	}
	if !pulled.IsDistinct() {
		t.Fatal("expected distinct flag preserved")
	}
}

func TestRichOrdering_PullUp_NoMatch(t *testing.T) {
	t.Parallel()
	keyA := fieldVal("a")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{keyA: {SortedBinding(ProvidedSortOrderAscending)}},
		[]values.Value{keyA}, false,
	)
	pulled := o.PullUp(map[string]values.Value{"z": fieldVal("w")})
	if len(pulled.GetKeys()) != 0 {
		t.Fatalf("expected 0 keys when no mapping matches, got %d", len(pulled.GetKeys()))
	}
}

func TestRichOrdering_PullUpThroughValue_RecordConstructor(t *testing.T) {
	t.Parallel()
	// Ordering: keys [FV("x"), FV("y")], x ASC, y DESC
	keyX := fieldVal("x")
	keyY := fieldVal("y")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			keyX: {SortedBinding(ProvidedSortOrderAscending)},
			keyY: {SortedBinding(ProvidedSortOrderDescending)},
		},
		[]values.Value{keyX, keyY}, false,
	)

	alias := values.NamedCorrelationIdentifier("q1")
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: &values.FieldValue{Field: "x", Typ: values.NullableLong}},
		values.RecordConstructorField{Name: "b", Value: &values.FieldValue{Field: "y", Typ: values.NullableString}},
	)

	pulled := o.PullUpThroughValue(resultValue, alias)

	if len(pulled.GetKeys()) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(pulled.GetKeys()))
	}
	if values.ExplainValue(pulled.GetKeys()[0]) != "q1.a" {
		t.Fatalf("expected first key 'q1.a', got %q", values.ExplainValue(pulled.GetKeys()[0]))
	}
	if values.ExplainValue(pulled.GetKeys()[1]) != "q1.b" {
		t.Fatalf("expected second key 'q1.b', got %q", values.ExplainValue(pulled.GetKeys()[1]))
	}

	// Pull-up has crossed into q1's output scope, so a request in that scope
	// must carry the same anchor. A flat "a" key no longer has enough identity
	// to distinguish same-named columns from different candidate legs.
	requested := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{
				Value: values.NewFieldValue(
					values.NewQuantifiedObjectValue(alias),
					"a",
					values.NullableLong,
				),
				SortOrder: RequestedSortOrderAscending,
			},
			{
				Value: values.NewFieldValue(
					values.NewQuantifiedObjectValue(alias),
					"b",
					values.NullableString,
				),
				SortOrder: RequestedSortOrderDescending,
			},
		},
		DistinctnessNotDistinct,
		false,
	)
	if !pulled.Satisfies(requested) {
		t.Fatal("pulled ordering does not satisfy its candidate-anchored request")
	}
}

func TestRichOrdering_PullUpThroughValue_PartialMatch(t *testing.T) {
	t.Parallel()
	// Only some keys match the result value.
	keyX := fieldVal("x")
	keyZ := fieldVal("z")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			keyX: {SortedBinding(ProvidedSortOrderAscending)},
			keyZ: {SortedBinding(ProvidedSortOrderDescending)},
		},
		[]values.Value{keyX, keyZ}, false,
	)

	alias := values.NamedCorrelationIdentifier("q1")
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: &values.FieldValue{Field: "x", Typ: values.NullableLong}},
	)

	pulled := o.PullUpThroughValue(resultValue, alias)

	if len(pulled.GetKeys()) != 1 {
		t.Fatalf("expected 1 key (z dropped), got %d", len(pulled.GetKeys()))
	}
	if values.ExplainValue(pulled.GetKeys()[0]) != "q1.a" {
		t.Fatalf("expected key 'q1.a', got %q", values.ExplainValue(pulled.GetKeys()[0]))
	}
}

func TestRichOrdering_PullUpThroughValue_PreservesIndependentKeys(t *testing.T) {
	t.Parallel()
	keyX := fieldVal("x")
	keyY := fieldVal("y")
	ordering := NewRichOrderingWithDeps(
		map[values.Value][]OrderingBinding{
			keyX: {SortedBinding(ProvidedSortOrderAscending)},
			keyY: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{keyX, keyY},
		combinatorics.NewSetMultimap[string](),
		false,
	)
	alias := values.NamedCorrelationIdentifier("out")
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: keyX},
		values.RecordConstructorField{Name: "b", Value: keyY},
	)

	pulled := ordering.PullUpThroughValue(resultValue, alias)
	if got := pulled.OrderingSet().DependencyMap().Size(); got != 0 {
		t.Fatalf("pull-up invented %d dependencies between independent keys", got)
	}
	reversed := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{
				Value: values.NewFieldValue(
					values.NewQuantifiedObjectValue(alias),
					"b",
					values.UnknownType,
				),
				SortOrder: RequestedSortOrderAscending,
			},
			{
				Value: values.NewFieldValue(
					values.NewQuantifiedObjectValue(alias),
					"a",
					values.UnknownType,
				),
				SortOrder: RequestedSortOrderAscending,
			},
		},
		DistinctnessNotDistinct,
		false,
	)
	if !pulled.Satisfies(reversed) {
		t.Fatal("pull-up totalized independent keys")
	}
}

func TestRichOrdering_PullUpThroughValue_DropsDependentWithMissingPrerequisite(t *testing.T) {
	t.Parallel()
	keyA := fieldVal("a")
	keyB := fieldVal("b")
	keyC := fieldVal("c")
	deps := combinatorics.NewSetMultimap[string]()
	deps.Put(values.ExplainValue(keyB), values.ExplainValue(keyA))
	deps.Put(values.ExplainValue(keyC), values.ExplainValue(keyB))
	ordering := NewRichOrderingWithDeps(
		map[values.Value][]OrderingBinding{
			keyA: {SortedBinding(ProvidedSortOrderAscending)},
			keyB: {SortedBinding(ProvidedSortOrderAscending)},
			keyC: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{keyA, keyB, keyC},
		deps,
		false,
	)
	alias := values.NamedCorrelationIdentifier("out")
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "first", Value: keyA},
		values.RecordConstructorField{Name: "last", Value: keyC},
	)

	pulled := ordering.PullUpThroughValue(resultValue, alias)
	if got := len(pulled.GetKeys()); got != 1 {
		t.Fatalf("expected only the independent prefix after dropping b, got %d keys", got)
	}
	if got := values.ExplainValue(pulled.GetKeys()[0]); got != "out.first" {
		t.Fatalf("surviving key = %q, want out.first", got)
	}
}

func TestRichOrdering_PullUpThroughValue_PreservesBindings(t *testing.T) {
	t.Parallel()
	keyX := fieldVal("x")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			keyX: {FixedBinding(nil)},
		},
		[]values.Value{keyX}, true,
	)

	alias := values.NamedCorrelationIdentifier("q1")
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "renamed", Value: &values.FieldValue{Field: "x", Typ: values.NullableLong}},
	)

	pulled := o.PullUpThroughValue(resultValue, alias)
	if !pulled.IsDistinct() {
		t.Fatal("expected distinct flag preserved")
	}
	bindings := pulled.GetBindingMap()[pulled.GetKeys()[0]]
	if len(bindings) != 1 || !bindings[0].IsFixed() {
		t.Fatal("expected fixed binding preserved")
	}
}

func TestRichOrdering_PushDownThroughValue_RecordConstructor(t *testing.T) {
	t.Parallel()
	// Ordering in output space: keys [FV("a"), FV("b")], a ASC, b DESC
	keyA := outputSlot("a", 0)
	keyB := outputSlot("b", 1)
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			keyA: {SortedBinding(ProvidedSortOrderAscending)},
			keyB: {SortedBinding(ProvidedSortOrderDescending)},
		},
		[]values.Value{keyA, keyB}, false,
	)

	upperAlias := values.NamedCorrelationIdentifier("q1")
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: &values.FieldValue{Field: "x", Typ: values.NullableLong}},
		values.RecordConstructorField{Name: "b", Value: &values.FieldValue{Field: "y", Typ: values.NullableString}},
	)

	pushed := o.PushDownThroughValue(resultValue, upperAlias)

	if len(pushed.GetKeys()) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(pushed.GetKeys()))
	}
	if values.ExplainValue(pushed.GetKeys()[0]) != "x" {
		t.Fatalf("expected first key 'x', got %q", values.ExplainValue(pushed.GetKeys()[0]))
	}
	if values.ExplainValue(pushed.GetKeys()[1]) != "y" {
		t.Fatalf("expected second key 'y', got %q", values.ExplainValue(pushed.GetKeys()[1]))
	}
}

func TestRichOrdering_PullUpPushDown_RoundTrip(t *testing.T) {
	t.Parallel()
	// The constructor's INPUTS are baked source-relative reads, which is what the
	// translator produces and what makes pull-up carry the ordinal through — the
	// push-down back down selects the member by that ordinal, never by a name.
	keyX := outputSlot("x", 0)
	keyY := outputSlot("y", 1)
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			keyX: {SortedBinding(ProvidedSortOrderAscending)},
			keyY: {SortedBinding(ProvidedSortOrderDescending)},
		},
		[]values.Value{keyX, keyY}, false,
	)

	alias := values.NamedCorrelationIdentifier("q1")
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: keyX},
		values.RecordConstructorField{Name: "b", Value: keyY},
	)

	// PullUp: x→a, y→b
	pulled := o.PullUpThroughValue(resultValue, alias)
	// PushDown back: a→x, b→y
	restored := pulled.PushDownThroughValue(resultValue, alias)

	if len(restored.GetKeys()) != 2 {
		t.Fatalf("expected 2 keys after round-trip, got %d", len(restored.GetKeys()))
	}
	if restored.GetKeys()[0] != keyX {
		t.Fatalf("expected the round trip to restore the constructor's first INPUT, got %q", values.ExplainValue(restored.GetKeys()[0]))
	}
	if restored.GetKeys()[1] != keyY {
		t.Fatalf("expected the round trip to restore the constructor's second INPUT, got %q", values.ExplainValue(restored.GetKeys()[1]))
	}
}

func TestRichOrdering_PullUpThroughValue_NilOrdering(t *testing.T) {
	t.Parallel()
	var o *RichOrdering
	alias := values.NamedCorrelationIdentifier("q1")
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: &values.FieldValue{Field: "x", Typ: values.NullableLong}},
	)
	if o.PullUpThroughValue(resultValue, alias) != nil {
		t.Fatal("expected nil for nil ordering")
	}
}

func TestRequestedOrdering_PushDownThroughValue(t *testing.T) {
	t.Parallel()
	req := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: outputSlot("a", 0), SortOrder: RequestedSortOrderAscending},
			{Value: outputSlot("b", 1), SortOrder: RequestedSortOrderDescending},
		},
		DistinctnessNotDistinct,
		false,
	)

	upperAlias := values.NamedCorrelationIdentifier("q1")
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: &values.FieldValue{Field: "x", Typ: values.NullableLong}},
		values.RecordConstructorField{Name: "b", Value: &values.FieldValue{Field: "y", Typ: values.NullableString}},
	)

	pushed := req.PushDownThroughValue(resultValue, upperAlias)

	parts := pushed.GetParts()
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	if values.ExplainValue(parts[0].Value) != "x" {
		t.Fatalf("expected first value 'x', got %q", values.ExplainValue(parts[0].Value))
	}
	if parts[0].SortOrder != RequestedSortOrderAscending {
		t.Fatalf("expected ascending, got %v", parts[0].SortOrder)
	}
	if values.ExplainValue(parts[1].Value) != "y" {
		t.Fatalf("expected second value 'y', got %q", values.ExplainValue(parts[1].Value))
	}
	if parts[1].SortOrder != RequestedSortOrderDescending {
		t.Fatalf("expected descending, got %v", parts[1].SortOrder)
	}
}

func TestRequestedOrdering_PushDownThroughValue_Preserve(t *testing.T) {
	t.Parallel()
	req := PreserveOrdering()
	alias := values.NamedCorrelationIdentifier("q1")
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: &values.FieldValue{Field: "x", Typ: values.NullableLong}},
	)
	pushed := req.PushDownThroughValue(resultValue, alias)
	if !pushed.IsPreserve() {
		t.Fatal("expected preserve ordering for preserve input")
	}
}

func TestRequestedOrdering_PushDownThroughValue_PartialDrop(t *testing.T) {
	t.Parallel()
	req := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: outputSlot("a", 0), SortOrder: RequestedSortOrderAscending},
			{Value: outputSlot("z", 7), SortOrder: RequestedSortOrderDescending}, // not in result
		},
		DistinctnessNotDistinct,
		false,
	)

	upperAlias := values.NamedCorrelationIdentifier("q1")
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: &values.FieldValue{Field: "x", Typ: values.NullableLong}},
	)

	pushed := req.PushDownThroughValue(resultValue, upperAlias)
	parts := pushed.GetParts()
	if len(parts) != 1 {
		t.Fatalf("expected 1 part (z dropped), got %d", len(parts))
	}
	if values.ExplainValue(parts[0].Value) != "x" {
		t.Fatalf("expected value 'x', got %q", values.ExplainValue(parts[0].Value))
	}
}

func TestRequestedOrdering_PushDownThroughValue_AllDropped(t *testing.T) {
	t.Parallel()
	req := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: fieldVal("z"), SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct,
		false,
	)

	upperAlias := values.NamedCorrelationIdentifier("q1")
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: &values.FieldValue{Field: "x", Typ: values.NullableLong}},
	)

	pushed := req.PushDownThroughValue(resultValue, upperAlias)
	if !pushed.IsPreserve() {
		t.Fatal("expected preserve ordering when all parts are dropped")
	}
}

func TestRichOrdering_GetEqualityBoundValues(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	b := fieldVal("b")
	c := fieldVal("c")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {FixedBinding(nil)},
			b: {SortedBinding(ProvidedSortOrderAscending)},
			c: {FixedBinding(nil), FixedBinding(nil)},
		},
		[]values.Value{a, b, c},
		false,
	)
	eq := o.GetEqualityBoundValues()
	if _, ok := eq[a]; !ok {
		t.Fatal("a should be equality-bound")
	}
	if _, ok := eq[b]; ok {
		t.Fatal("b should NOT be equality-bound (sorted)")
	}
	if _, ok := eq[c]; !ok {
		t.Fatal("c should be equality-bound (multiple fixed)")
	}
	if len(eq) != 2 {
		t.Fatalf("expected 2 equality-bound values, got %d", len(eq))
	}
}

func TestRichOrdering_GetEqualityBoundValues_MixedBindings(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {FixedBinding(nil), SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{a},
		false,
	)
	eq := o.GetEqualityBoundValues()
	if _, ok := eq[a]; !ok {
		t.Fatal("a should be equality-bound (has at least one fixed binding, matching Java's filterValues(isFixed))")
	}
}

func TestRichOrdering_GetEqualityBoundValues_Empty(t *testing.T) {
	t.Parallel()
	o := EmptyOrdering()
	eq := o.GetEqualityBoundValues()
	if len(eq) != 0 {
		t.Fatalf("empty ordering should have no equality-bound values, got %d", len(eq))
	}
}

func TestRichOrdering_GetOrderingKeys(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	b := fieldVal("b")
	c := fieldVal("c")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {FixedBinding(nil)},
			b: {SortedBinding(ProvidedSortOrderAscending)},
			c: {SortedBinding(ProvidedSortOrderDescending)},
		},
		[]values.Value{a, b, c},
		false,
	)
	keys := o.GetOrderingKeys()
	if len(keys) != 2 {
		t.Fatalf("expected 2 ordering keys (b, c), got %d", len(keys))
	}
	if keys[0] != b || keys[1] != c {
		t.Fatal("ordering keys should be [b, c]")
	}
}

func TestRichOrdering_GetOrderingKeys_AllFixed(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {FixedBinding(nil)},
		},
		[]values.Value{a},
		false,
	)
	keys := o.GetOrderingKeys()
	if len(keys) != 0 {
		t.Fatalf("all-fixed ordering should have no ordering keys, got %d", len(keys))
	}
}
