package values

// ToInt64 reports whether v is an integral type; returns the int64
// promotion when so.
func ToInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case int16:
		return int64(x), true
	case int8:
		return int64(x), true
	}
	return 0, false
}

// ToFloat64 reports whether v is numeric (int-like or float) and
// returns its float64 promotion. isFloat distinguishes native-float
// inputs from integral ones promoted here — comparison-time promotion
// uses it to prefer the int path when both sides are integral.
func ToFloat64(v any) (f float64, isFloat, numeric bool) {
	switch x := v.(type) {
	case float64:
		return x, true, true
	case float32:
		return float64(x), true, true
	case int64:
		return float64(x), false, true
	case int:
		return float64(x), false, true
	case int32:
		return float64(x), false, true
	case int16:
		return float64(x), false, true
	case int8:
		return float64(x), false, true
	}
	return 0, false, false
}

// LiteralValue wraps a Go-native literal in the matching Value
// subtype: nil → NullValue, bool → BooleanValue, otherwise a
// ConstantValue. Typ defaults to TypeUnknown; the simplifier does
// not depend on the type tag today — it inspects the wrapped Value
// subtype.
func LiteralValue(lit any) Value {
	if lit == nil {
		return &NullValue{Typ: TypeUnknown}
	}
	if b, ok := lit.(bool); ok {
		return NewBooleanValue(b)
	}
	return &ConstantValue{Value: lit, Typ: TypeUnknown}
}
