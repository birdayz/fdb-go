package predicates

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestEffectiveConstant_JavaShapes pins effectiveConstant to Java's
// EffectiveConstant.from(Value) (ConstantPredicateFoldingUtil.java:282-301),
// shape by shape: a NullValue is NULL; a BOOLEAN literal is its value (NULL
// for a null one); anything else is NOT_NULL when its type is NOT NULL and
// UNKNOWN otherwise. A non-boolean literal is NOT special: its type decides,
// as the target's third arm does (WS-E design 5.4(f)). The Object overload
// (a SimpleComparison's literal comparand) has no Go planning caller: Go
// comparands are Values, and NewLiteralComparison serves only the rowdiff
// oracle's evaluation.
func TestEffectiveConstant_JavaShapes(t *testing.T) {
	t.Parallel()
	// A non-nil ConstantValue is typed NOT NULL whatever its declared
	// nullability (ConstantValue.Type), as a target literal is.
	notNullFive := &values.ConstantValue{Value: int64(5), Typ: values.NullableLong}
	nullLong := values.NewNullValue(values.NullableLong)
	for _, c := range []struct {
		name string
		v    values.Value
		want effectiveConstantKind
	}{
		{"nil", nil, ecNull},
		{"NullValue", values.NewNullValue(values.NullableLong), ecNull},
		{"BooleanValue TRUE", values.NewBooleanValue(true), ecTrue},
		{"BooleanValue FALSE", values.NewBooleanValue(false), ecFalse},
		{"BooleanValue NULL", &values.BooleanValue{}, ecNull},
		{"ConstantValue true", &values.ConstantValue{Value: true, Typ: values.NotNullBoolean}, ecTrue},
		{"ConstantValue false", &values.ConstantValue{Value: false, Typ: values.NotNullBoolean}, ecFalse},
		{"ConstantValue nil", &values.ConstantValue{Value: nil, Typ: values.NullableLong}, ecNull},
		{"NOT NULL literal", notNullFive, ecNotNull},
		{"untyped literal", &values.ConstantValue{Value: int64(5)}, ecUnknown},
		{"NOT NULL COALESCE", values.NewScalarFunctionValue("COALESCE", values.NotNullLong, nullLong, notNullFive), ecNotNull},
		{"nullable COALESCE", values.NewScalarFunctionValue("COALESCE", values.NullableLong, nullLong, nullLong), ecUnknown},
		{"nullable NOT over a literal", values.NewNotValue(values.NewBooleanValue(false)), ecUnknown},
	} {
		if got := effectiveConstant(c.v); got != c.want {
			t.Errorf("%s: effectiveConstant = %d, want %d", c.name, got, c.want)
		}
	}
}
