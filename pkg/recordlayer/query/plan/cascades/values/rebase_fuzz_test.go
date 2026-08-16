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
		aliases := mustAliasMap(t, AliasPair{Source: src, Target: tgt})

		var v Value
		switch typeIdx % 13 {
		case 0:
			v = mustQOV(t, src)
		case 1:
			v = &fieldValue{Field: "col", Typ: UnknownType}
		case 2:
			v = &ConstantValue{Value: int64(42)}
		case 3:
			v = &NullValue{}
		case 4:
			v = &BooleanValue{}
		case 5:
			v = &ArithmeticValue{
				Op:    OpAdd,
				Left:  mustQOV(t, src),
				Right: &ConstantValue{Value: int64(1)},
			}
		case 6:
			v = NewCastValue(mustQOV(t, src), UnknownType)
		case 7:
			v = &PromoteValue{
				Child:  mustQOV(t, src),
				Target: UnknownType,
			}
		case 8:
			v = &ScalarFunctionValue{
				FuncName: "COALESCE",
				Args:     []Value{mustQOV(t, src)},
				Typ:      UnknownType,
			}
		case 9:
			v = &RecordConstructorValue{
				Fields: []RecordConstructorField{
					{Name: "f", Value: mustQOV(t, src)},
				},
			}
		case 10:
			v = &NotValue{Child: mustQOV(t, src)}
		case 11:
			v = NewAggregateValue(AggSum, mustQOV(t, src))
		case 12:
			v = NewAggregateValue(AggCountStar, nil)
		}

		result, err := RebaseValueChecked(v, aliases)
		if err != nil {
			t.Fatalf("RebaseValueChecked failed on a well-formed tree: %v", err)
		}
		if result == nil && v != nil {
			t.Fatal("RebaseValueChecked returned nil for non-nil input without an error")
		}
	})
}
