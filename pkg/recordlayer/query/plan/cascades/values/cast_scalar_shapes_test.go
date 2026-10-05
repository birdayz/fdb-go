package values

import (
	"math"
	"reflect"
	"testing"
)

// Scalar CAST shapes, carried over from the deleted map-path CAST
// (functions.CastValue), which agreed with this operator on every one of them.
func TestCastValue_ScalarShapes(t *testing.T) {
	t.Parallel()
	sourceType := func(v any) Type {
		switch v.(type) {
		case nil:
			return NullType
		case int64:
			return NotNullLong
		case float64:
			return NotNullDouble
		case string:
			return NotNullString
		case bool:
			return NotNullBoolean
		}
		t.Fatalf("no source type for %T", v)
		return nil
	}
	for _, c := range []struct {
		in      any
		target  Type
		want    any
		wantErr bool
	}{
		{float64(0.1), NullableDouble, float64(0.1), false},
		{"0.1", NullableFloat, float64(0.10000000149011612), false},
		{float64(0.1), NullableFloat, float64(0.10000000149011612), false},
		{"0", NullableBoolean, false, false},
		{"1", NullableBoolean, true, false},
		{"2024-07-04 15:30:45", NullableDate, "2024-07-04", false},
		{"2024-07-04 15:30:45", NullableTimestamp, "2024-07-04 15:30:45", false},
		{"2024-07-04", NullableDate, "2024-07-04", false},
		{"2024-07-04", NullableTimestamp, "2024-07-04 00:00:00", false},
		{"3.14", NullableFloat, float64(3.140000104904175), false},
		{"42", NullableLong, int64(42), false},
		{"abc", NullableLong, nil, true},
		{"false", NullableBoolean, false, false},
		{false, NullableInt, int64(0), false},
		{false, NullableString, "false", false},
		{float64(1), NullableString, "1.0", false},
		{float64(2.4), NullableInt, int64(2), false},
		{float64(2.5), NullableInt, int64(3), false},
		{float64(3.14), NullableDouble, float64(3.14), false},
		{float64(3.14), NullableString, "3.14", false},
		{float64(42.7), NullableInt, int64(43), false},
		{float64(3.4028234663852886e+38), NullableFloat, float64(3.4028234663852886e+38), false},
		{"hello", NullableString, "hello", false},
		{int64(1), NullableBytes, nil, true},
		{int64(1), NullableBoolean, nil, true},
		{int64(1), NullableUuid, nil, true},
		{int64(42), NullableInt, int64(42), false},
		{int64(42), NullableString, "42", false},
		{int64(7), NullableFloat, float64(7), false},
		{int64(2147483648), NullableInt, nil, true},
		{int64(2147483647), NullableInt, int64(2147483647), false},
		{int64(-2147483649), NullableInt, nil, true},
		{math.Inf(1), NullableLong, nil, true},
		{math.NaN(), NullableInt, nil, true},
		{"not-a-date", NullableDate, nil, true},
		{"notanumber", NullableFloat, nil, true},
		{"not-a-timestamp", NullableTimestamp, nil, true},
		{"not-a-uuid", NullableUuid, nil, true},
		{"true", NullableBoolean, true, false},
		{true, NullableBoolean, true, false},
		{true, NullableInt, int64(1), false},
		{true, NullableString, "true", false},
		{"yes", NullableBoolean, nil, true},
		{int64(16777217), NullableFloat, float64(1.6777216e+07), false},
		{float64(1.5), NullableFloat, float64(1.5), false},
		{float64(2.5), NullableFloat, float64(2.5), false},
		{float64(0), NullableFloat, float64(0), false},
		{float64(-0.5), NullableFloat, float64(-0.5), false},
		{float64(1.6777216e+07), NullableFloat, float64(1.6777216e+07), false},
		{math.NaN(), NullableFloat, nil, true},
		{math.Inf(1), NullableFloat, nil, true},
		{math.Inf(-1), NullableFloat, nil, true},
		{float64(1e+39), NullableFloat, nil, true},
		{float64(-1e+39), NullableFloat, nil, true},
		{nil, NullableInt, nil, false},
		{nil, NullableLong, nil, false},
		{nil, NullableFloat, nil, false},
		{nil, NullableDouble, nil, false},
		{nil, NullableString, nil, false},
		{nil, NullableBoolean, nil, false},
		{nil, NullableUuid, nil, false},
	} {
		got, err := NewCastValue(&ConstantValue{Value: c.in, Typ: sourceType(c.in)}, c.target).Evaluate(nil)
		if (err != nil) != c.wantErr || !reflect.DeepEqual(got, c.want) {
			t.Errorf("CAST(%#v AS %v) = %#v, %v; want %#v, error %t", c.in, c.target, got, err, c.want, c.wantErr)
		}
	}
}

// Every scalar CAST answers a value or an error, never both and never a panic;
// CAST(NULL AS T) is NULL; and a LONG fits an INT cast exactly when it is in
// int32 range.
func FuzzCastValue_ScalarInvariants(f *testing.F) {
	targets := []Type{NullableInt, NullableLong, NullableFloat, NullableDouble, NullableString, NullableBoolean, NullableUuid, NullableBytes, NullableDate, NullableTimestamp}
	for i := range targets {
		f.Add(uint8(i), int64(0), float64(0), "")
		f.Add(uint8(i), int64(-1), math.NaN(), "not-a-value")
	}
	f.Add(uint8(0), int64(math.MaxInt32)+1, math.Inf(1), "2147483648")
	f.Add(uint8(6), int64(0), float64(0), "00000000-0000-0000-0000-000000000000")
	f.Add(uint8(9), int64(253402300800000), float64(0), "9999-12-31T23:00:00-05:00")
	f.Fuzz(func(t *testing.T, target uint8, i int64, fl float64, s string) {
		to := targets[int(target)%len(targets)]
		for _, src := range []struct {
			v   any
			typ Type
		}{{nil, NullType}, {i, NotNullLong}, {fl, NotNullDouble}, {s, NotNullString}, {true, NotNullBoolean}, {false, NotNullBoolean}} {
			r, err := NewCastValue(&ConstantValue{Value: src.v, Typ: src.typ}, to).Evaluate(nil)
			if src.v == nil && (r != nil || err != nil) {
				t.Fatalf("CAST(NULL AS %v) = %v, %v", to, r, err)
			}
			if err != nil && r != nil {
				t.Fatalf("CAST(%#v AS %v) = %#v alongside error %v", src.v, to, r, err)
			}
		}
		if to == NullableInt {
			_, err := NewCastValue(&ConstantValue{Value: i, Typ: NotNullLong}, to).Evaluate(nil)
			if fits := i >= math.MinInt32 && i <= math.MaxInt32; fits != (err == nil) {
				t.Fatalf("CAST(%d AS INT): fits int32 %t, error %v", i, fits, err)
			}
		}
	})
}
