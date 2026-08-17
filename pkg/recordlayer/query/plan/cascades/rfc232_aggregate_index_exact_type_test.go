package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestRFC232AggregateIndexOutputTypeIsExactAndOwned(t *testing.T) {
	t.Parallel()

	base := &values.RecordType{Fields: []values.Field{
		{Name: "G", FieldType: values.NotNullString, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullInt, Ordinal: 1},
	}}
	candidate := NewAggregateIndexMatchCandidate(
		"sum_v_by_g", []string{"T"}, []string{"G"},
		expressions.AggSum, "V", base, []values.Type{values.NotNullString}, 1)

	// Mutating the caller-owned schema after candidate construction must not
	// change the physical row program the candidate can produce.
	base.Fields[0].Name = "MUTATED"
	base.Fields[1].FieldType = values.NullableDouble

	result, ok := aggregateIndexOutputType(candidate)
	if !ok {
		t.Fatal("exact aggregate candidate was declined")
	}
	if len(result.Fields) != 2 {
		t.Fatalf("aggregate output width = %d, want 2", len(result.Fields))
	}
	if got := result.Fields[0]; got.Name != "G" || !got.FieldType.Equals(values.NotNullString) {
		t.Fatalf("group field = %#v, want G STRING NOT NULL", got)
	}
	if got := result.Fields[1]; got.Name != "SUM(V)" || !got.FieldType.Equals(values.NullableInt) {
		t.Fatalf("aggregate field = %#v, want SUM(V) INT", got)
	}
	if _, err := values.SnapshotExactType(result); err != nil {
		t.Fatalf("aggregate output is not exact: %v", err)
	}
}

func TestRFC232AggregateIndexOutputTypeDeclinesUnknownAuthority(t *testing.T) {
	t.Parallel()

	base := &values.RecordType{Fields: []values.Field{
		{Name: "G", FieldType: values.NotNullString, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
	}}
	unknownGroup := NewAggregateIndexMatchCandidate(
		"sum_v_by_g", []string{"T"}, []string{"G"},
		expressions.AggSum, "V", base, []values.Type{values.UnknownType}, 1)
	if result, ok := aggregateIndexOutputType(unknownGroup); ok || result != nil {
		t.Fatalf("unknown grouping type produced executable output %#v", result)
	}

	missingOperand := NewAggregateIndexMatchCandidate(
		"sum_missing_by_g", []string{"T"}, []string{"G"},
		expressions.AggSum, "MISSING", base, []values.Type{values.NotNullString}, 1)
	if result, ok := aggregateIndexOutputType(missingOperand); ok || result != nil {
		t.Fatalf("missing aggregate operand produced executable output %#v", result)
	}
}
