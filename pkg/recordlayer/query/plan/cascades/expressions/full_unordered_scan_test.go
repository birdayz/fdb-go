package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestFullUnorderedScanStoresExactTypeAndCanonicalNames(t *testing.T) {
	t.Parallel()
	// Built inline rather than via rowOfTypes: this test MUTATES it below, and a
	// graph from a helper carries the helper's provenance. See the same note in
	// quantifier_test.go.
	row := &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "V", Ordinal: 1, FieldType: values.NullableString},
	}}
	scan := mustExpression(NewFullUnorderedScanExpression([]string{"Order", "Customer", "Order"}, row))
	if got := scan.GetRecordTypes(); len(got) != 2 || got[0] != "Customer" || got[1] != "Order" {
		t.Fatalf("record types = %v, want sorted and deduplicated names", got)
	}
	if !scan.GetFlowedType().Equals(row) || !scan.GetResultValue().Type().Equals(row) {
		t.Fatalf("scan types = flowed %v result %v, want %v", scan.GetFlowedType(), scan.GetResultValue().Type(), row)
	}
	if len(values.GetCorrelatedToOfValue(scan.GetResultValue())) != 0 {
		t.Fatal("source scan result is unexpectedly correlation-bearing")
	}
	// GetFlowedType hands back the SHARED graph — mutating it here would write
	// through to an interned handle and corrupt unrelated parallel tests. The
	// sharing is asserted instead, and isolation is asserted where it still
	// holds: against the caller's own graph. See RFC-234.
	if a, b := scan.GetFlowedType(), scan.GetFlowedType(); a != b {
		t.Fatalf("GetFlowedType returned two graphs (%p, %p); the defensive copy is back", a, b)
	}
	row.Fields[0].Name = "CALLER_MUTATED"
	if got := scan.GetFlowedType().(*values.RecordType).Fields[0].Name; got != "ID" {
		t.Fatalf("a caller's later edit reached the stored snapshot: %q", got)
	}
}

func TestFullUnorderedScanRejectsUnresolvedTypeWithoutObject(t *testing.T) {
	t.Parallel()
	for _, typ := range []values.Type{nil, values.UnknownType, values.AnyType} {
		scan, err := NewFullUnorderedScanExpression([]string{"T"}, typ)
		if err == nil || scan != nil {
			t.Fatalf("type %v returned (%v, %v), want nil scan and error", typ, scan, err)
		}
	}
}

func TestFullUnorderedScanEqualityUsesNamesAndExactType(t *testing.T) {
	t.Parallel()
	a := mustExpression(NewFullUnorderedScanExpression([]string{"B", "A"}, testRecordType()))
	b := mustExpression(NewFullUnorderedScanExpression([]string{"A", "B"}, testRecordType()))
	otherName := mustExpression(NewFullUnorderedScanExpression([]string{"A"}, testRecordType()))
	otherType := mustExpression(NewFullUnorderedScanExpression([]string{"A", "B"}, rowOfTypes("ID", values.NotNullString)))
	if !a.EqualsWithoutChildren(b, EmptyAliasMap()) || a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("equivalent exact scans did not compare and hash equally")
	}
	if a.EqualsWithoutChildren(otherName, EmptyAliasMap()) || a.EqualsWithoutChildren(otherType, EmptyAliasMap()) {
		t.Fatal("scan equality ignored a record-name or exact-type difference")
	}
}

func TestFullUnorderedScanResultIsStable(t *testing.T) {
	t.Parallel()
	scan := mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType()))
	first := scan.GetResultValue()
	second := scan.GetResultValue()
	if !values.SemanticEqualsUnderAliasMap(first, second, values.EmptyAliasMap()) {
		t.Fatalf("repeated scan results differ: %v vs %v", first, second)
	}
}
