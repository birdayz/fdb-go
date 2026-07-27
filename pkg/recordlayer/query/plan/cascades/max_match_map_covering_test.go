package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestTranslateQueryValueMaybe_CoveringIndexMultiColumnProjection pins RFC-189
// A3 (finding 9): TranslateQueryValueMaybe pulled each candidate PART up against
// ITSELF (PullUpValue(part, part, alias)), so case-1 self-equality always fired
// and every part collapsed to QOV(alias). For a multi-field covering-index
// RecordConstructor result value that mis-projects every column to the whole
// record. The fix pulls each part up against the ROOT candidate value (Java
// MaxMatchMap.translateQueryValueMaybe), so each column projects its own field.
//
// Query projects two columns (a, b); the candidate index stores the same two
// field values under DIFFERENT column names (col0, col1), so the top-level RCVs
// do not match as a whole and the matcher maps each field as a separate entry —
// the multi-part case that detonates the self-pull-up bug.
func TestTranslateQueryValueMaybe_CoveringIndexMultiColumnProjection(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("candidate")
	fx := &values.FieldValue{Field: "X", Typ: values.NullableLong}
	fy := &values.FieldValue{Field: "Y", Typ: values.NullableLong}

	qv := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: fx},
		values.RecordConstructorField{Name: "b", Value: fy},
	)
	cv := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "col0", Value: fx},
		values.RecordConstructorField{Name: "col1", Value: fy},
	)

	mmm := ComputeMaxMatchMap(qv, cv, nil)
	if mmm.Size() != 2 {
		t.Fatalf("expected 2 per-field mapping entries (matcher descends into the RCV), got %d", mmm.Size())
	}

	result := mmm.TranslateQueryValueMaybe(alias)
	if result == nil {
		t.Fatal("TranslateQueryValueMaybe returned nil for a covering-index projection")
	}
	rc, ok := result.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("expected a RecordConstructorValue result, got %T", result)
	}
	if len(rc.Fields) != 2 {
		t.Fatalf("expected 2 result fields (query structure preserved), got %d", len(rc.Fields))
	}

	// Each column must project its OWN candidate column, not collapse both to
	// QOV(alias). Field "a" → col0, field "b" → col1.
	fv0 := assertFieldRef(t, rc.Fields[0], "a")
	fv1 := assertFieldRef(t, rc.Fields[1], "b")
	if fv0.Field != "col0" {
		t.Fatalf("query column a must project candidate column col0, got %q", fv0.Field)
	}
	if fv1.Field != "col1" {
		t.Fatalf("query column b must project candidate column col1, got %q", fv1.Field)
	}
	// Cross-check that survives any future pull-up shape change: the two
	// projected values must DIFFER (the self-pull-up bug makes both QOV(alias),
	// which are structurally equal).
	if values.ValuesStructurallyEqual(rc.Fields[0].Value, rc.Fields[1].Value) {
		t.Fatal("the two projected columns must not be identical (self-pull-up collapse)")
	}
}

// assertFieldRef fails unless field.Name == wantName and field.Value is a
// *FieldValue (a per-column projection), returning that FieldValue. The
// self-pull-up bug produces a *QuantifiedObjectValue here instead.
func assertFieldRef(t *testing.T, field values.RecordConstructorField, wantName string) *values.FieldValue {
	t.Helper()
	if field.Name != wantName {
		t.Fatalf("expected result field %q, got %q", wantName, field.Name)
	}
	fv, ok := field.Value.(*values.FieldValue)
	if !ok {
		t.Fatalf("field %q must project a column FieldValue, got %T (self-pull-up collapse to QOV)", wantName, field.Value)
	}
	return fv
}
