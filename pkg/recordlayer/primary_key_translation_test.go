package recordlayer

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestTranslatePrimaryKeyToValues pins RFC-189 B3's correctness core: a common
// primary-key KeyExpression translates to structure-encoding Values so PK
// identity compares by STRUCTURE, not bare column names. The load-bearing case
// is that Field("ID") and Concat(RecordTypeKey(), Field("ID")) — which share the
// flat name list ["ID"] — are NOT structurally equal (conflating them lets
// ImplementDistinctUnionRule dedup two legs that must both survive → dropped
// rows, the hazard the by-name M5 was reverted for).
func TestTranslatePrimaryKeyToValues(t *testing.T) {
	t.Parallel()

	flat := TranslatePrimaryKeyToValues(Field("ID"))
	if len(flat) != 1 {
		t.Fatalf("Field(ID) → %d values, want 1", len(flat))
	}
	prefixed := TranslatePrimaryKeyToValues(Concat(RecordTypeKey(), Field("ID")))
	if len(prefixed) != 2 {
		t.Fatalf("Concat(RecordTypeKey(), Field(ID)) → %d values, want 2", len(prefixed))
	}

	// THE ANTI-CONFLATION PROPERTY.
	if valuesSlicesStructurallyEqual(flat, prefixed) {
		t.Fatal("Field(ID) and Concat(RecordTypeKey(), Field(ID)) must NOT be structurally equal (M5 dropped-rows hazard)")
	}

	if !valuesSlicesStructurallyEqual(flat, TranslatePrimaryKeyToValues(Field("ID"))) {
		t.Fatal("identical PK expressions must translate to structurally-equal Values")
	}
	if !valuesSlicesStructurallyEqual(prefixed, TranslatePrimaryKeyToValues(Concat(RecordTypeKey(), Field("ID")))) {
		t.Fatal("identical record-type-prefixed PKs must be structurally equal")
	}

	nested := TranslatePrimaryKeyToValues(Nest("addr", Field("city")))
	if len(nested) != 1 {
		t.Fatalf("Nest → %d values, want 1", len(nested))
	}
	fv, ok := nested[0].(*values.FieldValue)
	if !ok || fv.Field != "city" || fv.Child == nil {
		t.Fatalf("Nest(addr, city) → %T %v, want FieldValue{city, Child:FieldValue{addr}}", nested[0], nested[0])
	}
	if valuesSlicesStructurallyEqual(nested, TranslatePrimaryKeyToValues(Nest("other", Field("city")))) {
		t.Fatal("Nest(addr, city) and Nest(other, city) must not be structurally equal (different nesting path)")
	}
	if valuesSlicesStructurallyEqual(nested, TranslatePrimaryKeyToValues(Field("city"))) {
		t.Fatal("Nest(addr, city) must not equal a flat Field(city)")
	}

	if TranslatePrimaryKeyToValues(FanOut("tags")) != nil {
		t.Fatal("a fan-out field PK must abstain (nil)")
	}
	if TranslatePrimaryKeyToValues(VersionKey()) != nil {
		t.Fatal("a version PK must abstain (nil)")
	}
	if TranslatePrimaryKeyToValues(nil) != nil {
		t.Fatal("nil PK must return nil")
	}
}

func valuesSlicesStructurallyEqual(a, b []values.Value) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !values.ValuesStructurallyEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}
