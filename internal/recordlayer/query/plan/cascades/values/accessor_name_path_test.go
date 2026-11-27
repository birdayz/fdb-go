package values

import (
	"reflect"
	"testing"
)

// Test fixtures for the three plan-time column-reference representations
// (RFC-187 §2): (a) nested Child chain, (b) baked Resolved, (c) flat-dotted.
func anpQOV(t testing.TB) *quantifiedObjectValue {
	return mustQOV(t, NamedCorrelationIdentifier("Q"))
}

// lazyFlat: a top-level column `city` — form none-nested, leaf Field="city".
func lazyFlat(t testing.TB, field string) *fieldValue {
	return newFieldValue(anpQOV(t), field, UnknownType)
}

// lazyNested: `parent.child` as a nested Child chain (form a) — leaf Field="child".
func lazyNested(t testing.TB, parent, child string) *fieldValue {
	return newFieldValue(newFieldValue(anpQOV(t), parent, UnknownType), child, UnknownType)
}

// bakedSingle: a resolver/source-relative single-accessor bake (form b) — carries
// the accessor name, the shape resolveQualifiedBaked yields for `t.city`.
func bakedSingle(t testing.TB, field string, ord int) *fieldValue {
	return newCorrelatedFieldValueWithResolvedOrdinal(anpQOV(t), field, ord, UnknownType)
}

// bakedFused: `parent.child` fused into one baked node (form b) — Field is the
// leaf, Resolved carries the whole path.
func bakedFused(t testing.TB, parent, child string) *fieldValue {
	return &fieldValue{
		Field: child, Typ: UnknownType, Child: anpQOV(t),
		Resolved: &fieldPath{Accessors: []resolvedAccessor{{Field: parent, Ordinal: 0}, {Field: child, Ordinal: 1}}},
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
		{"lazy flat", lazyFlat(t, "city"), []string{"CITY"}, true},
		{"lazy flat lowercased", lazyFlat(t, "City"), []string{"CITY"}, true},
		{"lazy nested addr.city", lazyNested(t, "addr", "city"), []string{"ADDR", "CITY"}, true},
		{"baked single city", bakedSingle(t, "city", 3), []string{"CITY"}, true},
		{"baked fused addr.city", bakedFused(t, "addr", "city"), []string{"ADDR", "CITY"}, true},
		{"flat-dotted (form c) not split", lazyFlat(t, "addr.city"), nil, false},
		{"qualified-dotted (form c) not split", lazyFlat(t, "T.city"), nil, false},
		{"pure-ordinal accessor (lazy empty Field)", newFieldValue(anpQOV(t), "", UnknownType), nil, false},
		{"pure-ordinal accessor (baked empty Field)", &fieldValue{
			Child: anpQOV(t), Resolved: &fieldPath{Accessors: []resolvedAccessor{{Field: "", Ordinal: 2}}},
		}, nil, false},
		{"bare QOV is not a column", anpQOV(t), nil, false},
		{"legacy flat FieldValue no child", &fieldValue{Field: "city"}, []string{"CITY"}, true},
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
		{"nested vs top-level same leaf → NOT equal (form a)", lazyNested(t, "addr", "city"), lazyFlat(t, "city"), false},
		{"fused nested vs top-level same leaf → NOT equal (form b)", bakedFused(t, "addr", "city"), lazyFlat(t, "city"), false},
		// Bake-tolerance: a baked qualified ref still binds a lazy candidate over the same column.
		{"baked single vs lazy flat same name → equal", bakedSingle(t, "city", 3), lazyFlat(t, "city"), true},
		{"lazy flat vs baked single same name → equal", lazyFlat(t, "city"), bakedSingle(t, "city", 7), true},
		{"lazy flat vs lazy flat same name → equal", lazyFlat(t, "city"), lazyFlat(t, "CITY"), true},
		{"fused nested vs fused nested same path → equal", bakedFused(t, "addr", "city"), bakedFused(t, "ADDR", "City"), true},
		{"nested vs nested different parent same leaf → NOT equal", lazyNested(t, "addr", "city"), lazyNested(t, "billing", "city"), false},
		{"different leaf → NOT equal", lazyFlat(t, "name"), lazyFlat(t, "city"), false},
		// Ambiguous form (c) and pure-ordinal are conservative misses, never wrong binds.
		{"form-c dotted → conservative miss", lazyFlat(t, "addr.city"), lazyFlat(t, "city"), false},
		{"pure-ordinal → conservative miss", newFieldValue(anpQOV(t), "", UnknownType), lazyFlat(t, "city"), false},
		// Cardinality wrapper transparency.
		{"CARDINALITY(arr) vs CARDINALITY(arr) → equal", NewCardinalityValue(lazyFlat(t, "arr")), NewCardinalityValue(lazyFlat(t, "arr")), true},
		{"CARDINALITY(arr) vs arr → NOT equal (mismatched wrappers)", NewCardinalityValue(lazyFlat(t, "arr")), lazyFlat(t, "arr"), false},
		{"arr vs CARDINALITY(arr) → NOT equal (mismatched wrappers)", lazyFlat(t, "arr"), NewCardinalityValue(lazyFlat(t, "arr")), false},
		{"CARDINALITY(addr.arr) vs CARDINALITY(arr) → NOT equal", NewCardinalityValue(lazyNested(t, "addr", "arr")), NewCardinalityValue(lazyFlat(t, "arr")), false},
		{"nil operands → false", nil, lazyFlat(t, "city"), false},
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

func TestAccessorNamePathKey(t *testing.T) {
	t.Parallel()
	// Nested and top-level same-leaf keys must produce DIFFERENT keys (so a
	// path-keyed set distinguishes them), while equal columns produce the same
	// key — and the key agrees with ColumnNamePathsEqual.
	nestedK, ok := AccessorNamePathKey(lazyNested(t, "addr", "city"))
	if !ok {
		t.Fatal("nested addr.city produced no path key")
	}
	flatK, ok := AccessorNamePathKey(lazyFlat(t, "city"))
	if !ok {
		t.Fatal("flat city produced no path key")
	}
	if nestedK == flatK {
		t.Fatal("nested addr.city and top-level city produced the same path key")
	}
	// Equal ⟹ same key (baked and lazy over the same column agree).
	bakedK, _ := AccessorNamePathKey(bakedSingle(t, "city", 4))
	if bakedK != flatK {
		t.Fatalf("baked and lazy city produced different keys (%q vs %q)", bakedK, flatK)
	}
	// Ambiguous form (c) and pure-ordinal yield no key.
	if _, ok := AccessorNamePathKey(lazyFlat(t, "addr.city")); ok {
		t.Fatal("flat-dotted form c produced a path key (should be ok=false)")
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
		{"flat city vs [city] → equal", lazyFlat(t, "city"), []string{"city"}, true},
		{"baked city vs [CITY] → equal", bakedSingle(t, "city", 2), []string{"CITY"}, true},
		{"nested addr.city vs [city] → NOT equal (the aggregate/PK wrong-rows fix)", lazyNested(t, "addr", "city"), []string{"city"}, false},
		{"nested addr.city vs [addr,city] → equal", lazyNested(t, "addr", "city"), []string{"addr", "city"}, true},
		{"fused addr.city vs [addr,city] → equal", bakedFused(t, "addr", "city"), []string{"addr", "city"}, true},
		{"flat city vs [addr,city] → NOT equal", lazyFlat(t, "city"), []string{"addr", "city"}, false},
		{"form-c dotted → conservative miss", lazyFlat(t, "addr.city"), []string{"addr", "city"}, false},
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
