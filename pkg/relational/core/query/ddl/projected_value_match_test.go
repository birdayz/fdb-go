package ddl

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func exactDDLField(t *testing.T, correlation, fieldName string, fieldType values.Type, ordinal int, extra ...values.Field) values.Value {
	t.Helper()
	fields := append([]values.Field{{Name: fieldName, FieldType: fieldType}}, extra...)
	root, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier(correlation),
		values.NewRecordType(correlation+"_ROW", false, fields),
	)
	if err != nil {
		t.Fatalf("construct exact QOV: %v", err)
	}
	field, err := values.ResolveFieldOrdinals(root, []int{ordinal})
	if err != nil {
		t.Fatalf("resolve field ordinal %d: %v", ordinal, err)
	}
	return field
}

func TestProjectedValueMatchesUsesExactPathAndType(t *testing.T) {
	t.Parallel()

	// These producer-local roots deliberately expose different display names.
	// DDL has already proved this is a single source, so ordinal path + exact
	// leaf type is the identity that survives the two independently built QOVs.
	ordering := exactDDLField(t, "ORDERING", "ORDER_NAME", values.NotNullLong, 0)
	projected := exactDDLField(t, "PROJECTION", "PROJECTED_NAME", values.NotNullLong, 0)
	if !projectedValueMatches(ordering, projected) {
		t.Fatal("same exact accessor path and leaf type did not match across producer-local QOV roots")
	}

	foreignPath := exactDDLField(t, "FOREIGN_PATH", "FIRST", values.NotNullLong, 1,
		values.Field{Name: "SECOND", FieldType: values.NotNullLong})
	if projectedValueMatches(ordering, foreignPath) {
		t.Fatal("different accessor ordinal matched merely because the leaf type was equal")
	}

	foreignType := exactDDLField(t, "FOREIGN_TYPE", "ORDER_NAME", values.NotNullString, 0)
	if projectedValueMatches(ordering, foreignType) {
		t.Fatal("same accessor ordinal matched despite a different exact leaf type")
	}
}
