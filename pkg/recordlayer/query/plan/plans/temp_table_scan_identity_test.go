package plans

// Two temp-table scans that share an alias but flow different rows are
// different memo expressions.
//
// The alias alone was a sufficient key only while the constructor could not
// produce two differently-typed scans of one alias. It can, and the failure is
// silent in the worst way: equal-and-same-hash means MemoizeExpression interns
// the second into the first and returns a reference whose result QOV has the
// other expression's type, so the wrong schema propagates rather than an error
// being raised.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func tempTableScanRow(t *testing.T, fieldName string, fieldType values.Type) values.Type {
	t.Helper()
	return &values.RecordType{Fields: []values.Field{
		{Name: fieldName, Ordinal: 0, FieldType: fieldType},
	}}
}

func TestTempTableScanIdentityFoldsTheFlowedRow(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("TT")

	same := func(t *testing.T, a, b *RecordQueryTempTableScanPlan) (bool, bool) {
		t.Helper()
		return a.EqualsPlanWithoutChildren(b), a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren()
	}

	t.Run("same_alias_same_row_is_one_expression", func(t *testing.T) {
		t.Parallel()
		a, err := NewRecordQueryTempTableScanPlan(alias, tempTableScanRow(t, "A", values.NotNullLong))
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		b, err := NewRecordQueryTempTableScanPlan(alias, tempTableScanRow(t, "A", values.NotNullLong))
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		equal, hashEqual := same(t, a, b)
		if !equal {
			t.Error("two identical temp-table scans compared unequal — folding the row" +
				" must not make an expression differ from itself")
		}
		if !hashEqual {
			t.Error("two identical temp-table scans hashed differently — equal" +
				" expressions must hash equal or memo dedup breaks")
		}
	})

	t.Run("same_alias_different_column_type", func(t *testing.T) {
		t.Parallel()
		a, err := NewRecordQueryTempTableScanPlan(alias, tempTableScanRow(t, "A", values.NotNullLong))
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		b, err := NewRecordQueryTempTableScanPlan(alias, tempTableScanRow(t, "A", values.NotNullString))
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		if equal, _ := same(t, a, b); equal {
			t.Error("temp-table scans over LONG and STRING rows compared equal —" +
				" the memo would intern one into the other and hand back the wrong schema")
		}
	})

	t.Run("same_alias_different_column_name", func(t *testing.T) {
		t.Parallel()
		a, err := NewRecordQueryTempTableScanPlan(alias, tempTableScanRow(t, "A", values.NotNullLong))
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		b, err := NewRecordQueryTempTableScanPlan(alias, tempTableScanRow(t, "B", values.NotNullLong))
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		if equal, _ := same(t, a, b); equal {
			t.Error("temp-table scans whose one column is named differently compared equal")
		}
	})

	t.Run("same_alias_different_width", func(t *testing.T) {
		t.Parallel()
		a, err := NewRecordQueryTempTableScanPlan(alias, tempTableScanRow(t, "A", values.NotNullLong))
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		wide := &values.RecordType{Fields: []values.Field{
			{Name: "A", Ordinal: 0, FieldType: values.NotNullLong},
			{Name: "B", Ordinal: 1, FieldType: values.NotNullLong},
		}}
		b, err := NewRecordQueryTempTableScanPlan(alias, wide)
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		if equal, _ := same(t, a, b); equal {
			t.Error("temp-table scans of different widths compared equal")
		}
	})

	t.Run("different_alias_same_row", func(t *testing.T) {
		t.Parallel()
		a, err := NewRecordQueryTempTableScanPlan(alias, tempTableScanRow(t, "A", values.NotNullLong))
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		b, err := NewRecordQueryTempTableScanPlan(
			values.NamedCorrelationIdentifier("OTHER"), tempTableScanRow(t, "A", values.NotNullLong))
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		if equal, _ := same(t, a, b); equal {
			t.Error("the alias stopped being load-bearing when the row was folded in")
		}
	})
}
