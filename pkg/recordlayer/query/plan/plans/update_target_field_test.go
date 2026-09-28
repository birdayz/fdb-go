package plans

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestUpdateTargetFieldType drives every arm of the field-path walk the
// promotion check and the executor read the assigned field's type through:
// a column, a struct field, and each refusal, which must be loud (a transform
// resolved against another type never assigns whatever field its ordinal
// happens to name).
func TestUpdateTargetFieldType(t *testing.T) {
	t.Parallel()
	inner := &values.RecordType{Fields: []values.Field{
		{Name: "F", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "G", Ordinal: 1, FieldType: values.NullableString},
	}}
	target := &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "S", Ordinal: 1, FieldType: inner},
	}}
	tr := func(names []string, ordinals ...int) expressions.UpdateTransform {
		return expressions.UpdateTransform{FieldNames: names, FieldOrdinals: ordinals, NewValue: values.LiteralValue(int64(1))}
	}
	if got, err := UpdateTargetFieldType(target, tr([]string{"ID"}, 0)); err != nil || !got.Equals(values.NotNullLong) {
		t.Errorf("a column: %v, %v", got, err)
	}
	if got, err := UpdateTargetFieldType(target, tr([]string{"S", "G"}, 1, 1)); err != nil || !got.Equals(values.NullableString) {
		t.Errorf("a struct field: %v, %v", got, err)
	}
	for _, c := range []struct {
		name string
		tr   expressions.UpdateTransform
		want string
	}{
		{"no path", tr(nil), "0 ordinals for 0 names"},
		{"names and ordinals apart", tr([]string{"S"}, 1, 0), "2 ordinals for 1 names"},
		{"an ordinal outside", tr([]string{"X"}, 5), "ordinal 5 outside"},
		{"another field at the ordinal", tr([]string{"S"}, 0), `at ordinal 0 is field "ID"`},
		{"a field of a scalar", tr([]string{"ID", "F"}, 0, 0), "is not a record"},
		{"another field of the struct", tr([]string{"S", "F"}, 1, 1), `at ordinal 1 is field "G"`},
	} {
		if _, err := UpdateTargetFieldType(target, c.tr); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error containing %q", c.name, err, c.want)
		}
	}

	// A path that is a prefix of another (or equal to it) is Java's
	// UPDATE_TRANSFORM_AMBIGUOUS, whatever the order the SET list gave them.
	for _, pair := range [][2]expressions.UpdateTransform{
		{tr([]string{"S", "F"}, 1, 0), tr([]string{"S"}, 1)},
		{tr([]string{"S"}, 1), tr([]string{"S", "F"}, 1, 0)},
	} {
		var ambiguous *expressions.UpdateTransformAmbiguousError
		if err := checkUpdatePromotions(target, pair[:]); !errors.As(err, &ambiguous) || ambiguous.Prefix != "S" || ambiguous.Path != "S.F" {
			t.Errorf("%s with %s: %v, want S a prefix of S.F", pair[0].FieldPath(), pair[1].FieldPath(), err)
		}
	}
	// Two fields of one struct are not ambiguous: G takes a LONG, so
	// the promotion check refuses it, which it reaches only past the
	// ambiguity check.
	var incompatible *values.IncompatibleTypeError
	long := &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}
	f, g := tr([]string{"S", "F"}, 1, 0), tr([]string{"S", "G"}, 1, 1)
	f.NewValue, g.NewValue = long, long
	if err := checkUpdatePromotions(target, []expressions.UpdateTransform{f, g}); !errors.As(err, &incompatible) {
		t.Errorf("two fields of one struct: %v, want the promotion check's refusal, not an ambiguity", err)
	}
}
