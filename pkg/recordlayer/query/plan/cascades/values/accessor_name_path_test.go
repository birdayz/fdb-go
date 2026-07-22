package values

import (
	"reflect"
	"testing"
)

// Test fixtures for the three plan-time column-reference representations
// (RFC-187 §2): (a) nested Child chain, (b) baked Resolved, (c) flat-dotted.
func anpQOV() *QuantifiedObjectValue {
	return NewQuantifiedObjectValue(NamedCorrelationIdentifier("Q"))
}

// lazyFlat: a top-level column `city` — form none-nested, leaf Field="city".
func lazyFlat(field string) *FieldValue { return NewFieldValue(anpQOV(), field, UnknownType) }

// lazyNested: `parent.child` as a nested Child chain (form a) — leaf Field="child".
func lazyNested(parent, child string) *FieldValue {
	return NewFieldValue(NewFieldValue(anpQOV(), parent, UnknownType), child, UnknownType)
}

// bakedSingle: a resolver/source-relative single-accessor bake (form b) — carries
// the accessor name, the shape resolveQualifiedBaked yields for `t.city`.
func bakedSingle(field string, ord int) *FieldValue {
	return NewCorrelatedFieldValueWithResolvedOrdinal(anpQOV(), field, ord, UnknownType)
}

// bakedFused: `parent.child` fused into one baked node (form b) — Field is the
// leaf, Resolved carries the whole path.
func bakedFused(parent, child string) *FieldValue {
	return &FieldValue{
		Field: child, Typ: UnknownType, Child: anpQOV(),
		Resolved: &FieldPath{Accessors: []ResolvedAccessor{{Field: parent, Ordinal: 0}, {Field: child, Ordinal: 1}}},
	}
}

func TestAccessorNamePath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    Value
		want []string
		ok   bool
	}{
		{"lazy flat", lazyFlat("city"), []string{"CITY"}, true},
		{"lazy flat lowercased", lazyFlat("City"), []string{"CITY"}, true},
		{"lazy nested addr.city", lazyNested("addr", "city"), []string{"ADDR", "CITY"}, true},
		{"baked single city", bakedSingle("city", 3), []string{"CITY"}, true},
		{"baked fused addr.city", bakedFused("addr", "city"), []string{"ADDR", "CITY"}, true},
		{"flat-dotted (form c) not split", lazyFlat("addr.city"), nil, false},
		{"qualified-dotted (form c) not split", lazyFlat("T.city"), nil, false},
		{"pure-ordinal accessor (lazy empty Field)", NewFieldValue(anpQOV(), "", UnknownType), nil, false},
		{"pure-ordinal accessor (baked empty Field)", &FieldValue{
			Child: anpQOV(), Resolved: &FieldPath{Accessors: []ResolvedAccessor{{Field: "", Ordinal: 2}}},
		}, nil, false},
		{"bare QOV is not a column", anpQOV(), nil, false},
		{"legacy flat FieldValue no child", &FieldValue{Field: "city"}, []string{"CITY"}, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, ok := AccessorNamePath(c.v)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (path=%v)", ok, c.ok, got)
			}
			if ok && !reflect.DeepEqual(got, c.want) {
				t.Fatalf("path = %v, want %v", got, c.want)
			}
		})
	}
}

func TestColumnNamePathsEqual(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b Value
		want bool
	}{
		// The core wrong-rows fix: nested addr.city must NOT match top-level city.
		{"nested vs top-level same leaf → NOT equal (form a)", lazyNested("addr", "city"), lazyFlat("city"), false},
		{"fused nested vs top-level same leaf → NOT equal (form b)", bakedFused("addr", "city"), lazyFlat("city"), false},
		// Bake-tolerance: a baked qualified ref still binds a lazy candidate over the same column.
		{"baked single vs lazy flat same name → equal", bakedSingle("city", 3), lazyFlat("city"), true},
		{"lazy flat vs baked single same name → equal", lazyFlat("city"), bakedSingle("city", 7), true},
		{"lazy flat vs lazy flat same name → equal", lazyFlat("city"), lazyFlat("CITY"), true},
		{"fused nested vs fused nested same path → equal", bakedFused("addr", "city"), bakedFused("ADDR", "City"), true},
		{"nested vs nested different parent same leaf → NOT equal", lazyNested("addr", "city"), lazyNested("billing", "city"), false},
		{"different leaf → NOT equal", lazyFlat("name"), lazyFlat("city"), false},
		// Ambiguous form (c) and pure-ordinal are conservative misses, never wrong binds.
		{"form-c dotted → conservative miss", lazyFlat("addr.city"), lazyFlat("city"), false},
		{"pure-ordinal → conservative miss", NewFieldValue(anpQOV(), "", UnknownType), lazyFlat("city"), false},
		// Cardinality wrapper transparency.
		{"CARDINALITY(arr) vs CARDINALITY(arr) → equal", NewCardinalityValue(lazyFlat("arr")), NewCardinalityValue(lazyFlat("arr")), true},
		{"CARDINALITY(arr) vs arr → NOT equal (mismatched wrappers)", NewCardinalityValue(lazyFlat("arr")), lazyFlat("arr"), false},
		{"arr vs CARDINALITY(arr) → NOT equal (mismatched wrappers)", lazyFlat("arr"), NewCardinalityValue(lazyFlat("arr")), false},
		{"CARDINALITY(addr.arr) vs CARDINALITY(arr) → NOT equal", NewCardinalityValue(lazyNested("addr", "arr")), NewCardinalityValue(lazyFlat("arr")), false},
		{"nil operands → false", nil, lazyFlat("city"), false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := ColumnNamePathsEqual(c.a, c.b); got != c.want {
				t.Fatalf("ColumnNamePathsEqual = %v, want %v", got, c.want)
			}
		})
	}
}

func TestAccessorNamePathMatchesNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		v         Value
		candidate []string
		want      bool
	}{
		{"flat city vs [city] → equal", lazyFlat("city"), []string{"city"}, true},
		{"baked city vs [CITY] → equal", bakedSingle("city", 2), []string{"CITY"}, true},
		{"nested addr.city vs [city] → NOT equal (the aggregate/PK wrong-rows fix)", lazyNested("addr", "city"), []string{"city"}, false},
		{"nested addr.city vs [addr,city] → equal", lazyNested("addr", "city"), []string{"addr", "city"}, true},
		{"fused addr.city vs [addr,city] → equal", bakedFused("addr", "city"), []string{"addr", "city"}, true},
		{"flat city vs [addr,city] → NOT equal", lazyFlat("city"), []string{"addr", "city"}, false},
		{"form-c dotted → conservative miss", lazyFlat("addr.city"), []string{"addr", "city"}, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := AccessorNamePathMatchesNames(c.v, c.candidate); got != c.want {
				t.Fatalf("AccessorNamePathMatchesNames = %v, want %v", got, c.want)
			}
		})
	}
}
