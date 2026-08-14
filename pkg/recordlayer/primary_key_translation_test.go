package recordlayer

import (
	"strings"
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
	nestedType := values.NewRecordType("ADDRESS", false, []values.Field{
		{Name: "city", FieldType: values.NullableString, Ordinal: 0},
	})
	rowType := values.NewRecordType("ROW", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "addr", FieldType: nestedType, Ordinal: 1},
		{Name: "other", FieldType: nestedType, Ordinal: 2},
		{Name: "city", FieldType: values.NullableString, Ordinal: 3},
		{Name: "id", FieldType: values.NotNullLong, Ordinal: 4},
	})

	flat := TranslatePrimaryKeyToValues(Field("ID"), nil, rowType)
	if len(flat) != 1 {
		t.Fatalf("Field(ID) → %d values, want 1", len(flat))
	}
	prefixed := TranslatePrimaryKeyToValues(Concat(RecordTypeKey(), Field("ID")), nil, rowType)
	if len(prefixed) != 2 {
		t.Fatalf("Concat(RecordTypeKey(), Field(ID)) → %d values, want 2", len(prefixed))
	}

	// THE ANTI-CONFLATION PROPERTY.
	if valuesSlicesStructurallyEqual(flat, prefixed) {
		t.Fatal("Field(ID) and Concat(RecordTypeKey(), Field(ID)) must NOT be structurally equal (M5 dropped-rows hazard)")
	}

	if !valuesSlicesStructurallyEqual(flat, TranslatePrimaryKeyToValues(Field("ID"), nil, rowType)) {
		t.Fatal("identical PK expressions must translate to structurally-equal Values")
	}
	if !valuesSlicesStructurallyEqual(prefixed, TranslatePrimaryKeyToValues(Concat(RecordTypeKey(), Field("ID")), nil, rowType)) {
		t.Fatal("identical record-type-prefixed PKs must be structurally equal")
	}

	nested := TranslatePrimaryKeyToValues(Nest("addr", Field("city")), nil, rowType)
	if len(nested) != 1 {
		t.Fatalf("Nest → %d values, want 1", len(nested))
	}
	fv, ok := values.AsFieldValue(nested[0])
	if !ok || fv.DisplayName() != "city" || fv.ChildValue() == nil {
		t.Fatalf("Nest(addr, city) → %T %v, want FieldValue{city, Child:FieldValue{addr}}", nested[0], nested[0])
	}
	if got := fv.Path().Ordinals(); len(got) != 2 || got[0] != 1 || got[1] != 0 {
		t.Fatalf("Nest(addr, city) resolved path = %v, want [1 0]", got)
	}
	if valuesSlicesStructurallyEqual(nested, TranslatePrimaryKeyToValues(Nest("other", Field("city")), nil, rowType)) {
		t.Fatal("Nest(addr, city) and Nest(other, city) must not be structurally equal (different nesting path)")
	}
	if valuesSlicesStructurallyEqual(nested, TranslatePrimaryKeyToValues(Field("city"), nil, rowType)) {
		t.Fatal("Nest(addr, city) must not equal a flat Field(city)")
	}

	if TranslatePrimaryKeyToValues(FanOut("tags"), nil, rowType) != nil {
		t.Fatal("a fan-out field PK must abstain (nil)")
	}
	if TranslatePrimaryKeyToValues(VersionKey(), nil, rowType) != nil {
		t.Fatal("a version PK must abstain (nil)")
	}
	// A nested RecordTypeKey must abstain: translating to a bare
	// RecordTypeValue would drop the nested field, so RecordTypeKey().Nest(Field("x"))
	// and RecordTypeKey().Nest(Field("y")) would translate identically → wrong dedup.
	if TranslatePrimaryKeyToValues(RecordTypeKey().Nest(Field("x")), nil, rowType) != nil {
		t.Fatal("a nested RecordTypeKey PK must abstain (nil) — else structurally-different nested PKs conflate → dropped rows")
	}
	if TranslatePrimaryKeyToValues(nil, nil, rowType) != nil {
		t.Fatal("nil PK must return nil")
	}

	// Casing: with a name normalizer (the SQL layer's strings.ToUpper), lowercase
	// protobuf field names are lifted into the same namespace as the plan's
	// ordering values, so the structural PK can actually match (else B3 is inert).
	// identity vs ToUpper must differ for a lowercase field.
	lower := TranslatePrimaryKeyToValues(Field("id"), nil, rowType)
	upper := TranslatePrimaryKeyToValues(Field("id"), strings.ToUpper, rowType)
	if valuesSlicesStructurallyEqual(lower, upper) {
		t.Fatal("the name normalizer must lift 'id' → 'ID' so the structural PK matches the uppercased ordering namespace")
	}
	if uv, ok := values.AsFieldValue(upper[0]); !ok || uv.DisplayName() != "ID" {
		t.Fatalf("normalized field must be 'ID', got %v", upper[0])
	}
	if got := TranslatePrimaryKeyToValues(Nest("addr", Field("missing")), nil, rowType); got != nil {
		t.Fatalf("invalid nested suffix must decline, got %v", got)
	}
	if got := TranslatePrimaryKeyToValues(Field("ID"), nil, values.UnknownType); got != nil {
		t.Fatalf("unresolved candidate layout must decline, got %v", got)
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
