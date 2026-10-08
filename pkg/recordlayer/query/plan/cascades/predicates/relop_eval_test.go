package predicates

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// A RelOpValue evaluates as BinaryPhysicalOperator.eval: NULL in, NULL out,
// except for the null-safe comparisons, and otherwise as the comparison
// predicate it lifts to.
func TestRelOpValueEvaluates(t *testing.T) {
	t.Parallel()
	lng := func(v any) values.Value { return &values.ConstantValue{Value: v, Typ: values.NullableLong} }
	null := values.NewNullValue(values.NullableLong)
	for _, tc := range []struct {
		cmp         values.RelOpComparison
		left, right values.Value
		want        any
	}{
		{values.RelOpGreaterThan, lng(int64(7)), lng(int64(5)), true},
		{values.RelOpGreaterThan, lng(int64(5)), lng(int64(5)), false},
		{values.RelOpLessThanOrEquals, lng(int64(5)), &values.ConstantValue{Value: int64(5), Typ: values.NotNullInt}, true},
		{values.RelOpEquals, lng(int64(5)), null, nil},
		{values.RelOpNotEquals, null, lng(int64(5)), nil},
		{values.RelOpIsDistinctFrom, null, lng(int64(5)), true},
		{values.RelOpIsDistinctFrom, null, null, false},
		{values.RelOpNotDistinctFrom, null, null, true},
		{values.RelOpNotDistinctFrom, lng(int64(4)), lng(int64(5)), false},
	} {
		rel, err := values.NewBinaryRelOpValue(tc.cmp, tc.left, tc.right)
		if err != nil {
			t.Fatal(err)
		}
		got, err := rel.Evaluate(nil)
		if err != nil || fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s(%v) = %v, %v; want %v", rel.FunctionName, rel.Children(), got, err, tc.want)
		}
	}
	// ENUM against STRING promotes the string to the member (EQ_ES).
	mood := values.NewEnumType("MOOD", true, []values.EnumValue{{Name: "HAPPY", Number: 3}, {Name: "SAD", Number: 4}})
	for name, want := range map[string]bool{"HAPPY": true, "SAD": false} {
		rel, err := values.NewBinaryRelOpValue(values.RelOpEquals, &values.ConstantValue{Value: int64(3), Typ: mood},
			&values.ConstantValue{Value: name, Typ: values.NotNullString})
		if err != nil {
			t.Fatal(err)
		}
		if got, err := rel.Evaluate(nil); err != nil || got != want {
			t.Errorf("HAPPY = %q is %v, %v", name, got, err)
		}
	}
	for cmp, want := range map[values.RelOpComparison]bool{values.RelOpIsNull: true, values.RelOpNotNull: false} {
		un, err := values.NewUnaryRelOpValue(cmp, null)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := un.Evaluate(nil); err != nil || got != want {
			t.Errorf("%s(NULL) = %v, %v", un.FunctionName, got, err)
		}
	}
}
