package query

import (
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
)

func TestUnnestCorrelationReject(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, alias, binding string
		reject               bool
	}{
		{"distinct", "X", "", false},
		{"same_label_distinct_identity", "T", "Q$DUP1", false},
		{"same_identity", "T", "", true},
		{"different_label_same_identity", "X", "T", true},
		{"canonical_identity_collision", "x", "t", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := unnestCorrelationReject(logical.NewScan("T", "T"), &logical.LogicalUnnest{Alias: tc.alias, Binding: tc.binding})
			if tc.reject {
				if err == nil || err.Code != api.ErrCodeDuplicateAlias {
					t.Fatalf("want duplicate correlation rejection, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	// A source inside a derived definition is not visible in this FROM scope.
	derived := logical.NewCTE("D", logical.NewScan("T", "X"), logical.NewScan("D", ""), false)
	if err := unnestCorrelationReject(derived, &logical.LogicalUnnest{Alias: "X"}); err != nil {
		t.Fatal(err)
	}
}

func TestUnnestOrdinalityPhysicalNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, alias, at string
		physical        []string
	}{
		{"distinct", "v", "o", []string{"v", "o"}},
		{"duplicate_labels", "v", "v", []string{"v", "v_2"}},
		{"quoted_case_distinct", "v", "V", []string{"v", "V"}},
		{"ordinal_only_collision", "", "_0", []string{"_0_2", "_0"}},
		{"ordinal_only_distinct", "", "_1", []string{"_0", "_1"}},
		{"explicit_frees_reserved_name", "v", "_0", []string{"v", "_0"}},
		{"element_default_name", "_0", "o", []string{"_0", "o"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u := &logical.LogicalUnnest{Alias: tc.alias, AtAlias: tc.at}
			fields, _, ok := unnestSeedInnerFields(values.NamedCorrelationIdentifier("INNER"), u, values.NotNullLong)
			if !ok || len(fields) == 0 {
				t.Fatalf("ordinal seed was not built: %v", fields)
			}
			for i, field := range fields {
				fv, ok := values.AsFieldValue(field.Value)
				if !ok {
					t.Fatalf("seed field %d is %T", i, field.Value)
				}
				wantOrdinal := i
				if tc.alias == "" {
					wantOrdinal = 1
				}
				if !reflect.DeepEqual(fv.Path().Ordinals(), []int{wantOrdinal}) {
					t.Fatalf("slot %d path %v", i, fv.Path().Ordinals())
				}
				row, ok := fv.ChildValue().Type().(*values.RecordType)
				if !ok || len(row.Fields) != 2 {
					t.Fatalf("inner row type %v", fv.ChildValue().Type())
				}
				names := []string{row.Fields[0].Name, row.Fields[1].Name}
				if !reflect.DeepEqual(names, tc.physical) {
					t.Fatalf("physical names %v, want %v", names, tc.physical)
				}
			}
		})
	}
}
