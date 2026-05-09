package values

import "testing"

// FuzzRebaseValue_NoPanic verifies that RebaseValue never panics
// regardless of the alias map contents. Exercises all Value types
// with random alias pairings.
func FuzzRebaseValue_NoPanic(f *testing.F) {
	f.Add("src", "tgt", byte(0))
	f.Add("a", "b", byte(1))
	f.Add("x", "y", byte(2))
	f.Add("old", "new", byte(3))

	f.Fuzz(func(t *testing.T, srcName, tgtName string, typeIdx byte) {
		src := NamedCorrelationIdentifier(srcName)
		tgt := NamedCorrelationIdentifier(tgtName)
		aliases := AliasMap{src: tgt}

		var v Value
		switch typeIdx % 13 {
		case 0:
			v = &QuantifiedObjectValue{Correlation: src, Typ: UnknownType}
		case 1:
			v = &FieldValue{Field: "col", Typ: UnknownType}
		case 2:
			v = &ConstantValue{Value: int64(42)}
		case 3:
			v = &NullValue{}
		case 4:
			v = &BooleanValue{}
		case 5:
			v = &ArithmeticValue{
				Op:    OpAdd,
				Left:  &QuantifiedObjectValue{Correlation: src},
				Right: &ConstantValue{Value: int64(1)},
			}
		case 6:
			v = NewCastValue(&QuantifiedObjectValue{Correlation: src}, UnknownType)
		case 7:
			v = &PromoteValue{
				Child:  &QuantifiedObjectValue{Correlation: src},
				Target: UnknownType,
			}
		case 8:
			v = &ScalarFunctionValue{
				FuncName: "COALESCE",
				Args:     []Value{&QuantifiedObjectValue{Correlation: src}},
				Typ:      UnknownType,
			}
		case 9:
			v = &RecordConstructorValue{
				Fields: []RecordConstructorField{
					{Name: "f", Value: &QuantifiedObjectValue{Correlation: src}},
				},
			}
		case 10:
			v = &NotValue{Child: &QuantifiedObjectValue{Correlation: src}}
		case 11:
			v = NewAggregateValue(AggSum, &QuantifiedObjectValue{Correlation: src})
		case 12:
			v = NewAggregateValue(AggCountStar, nil)
		}

		result := RebaseValue(v, aliases)
		if result == nil && v != nil {
			t.Fatal("RebaseValue returned nil for non-nil input")
		}
	})
}
