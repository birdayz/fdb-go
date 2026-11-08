package values

import (
	"errors"
	"math"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/internal/partition"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func TestPartitionValueEvaluationAndErrors(t *testing.T) {
	t.Parallel()
	if typ := (&PartitionValue{}).Type(); !typ.Equals(NotNullLong) {
		t.Fatalf("zero-value result type = %v, want fixed NOT NULL BIGINT", typ)
	}
	for _, tc := range []struct{ source, width, want int64 }{
		{-11, 10, -2},
		{-10, 10, -1},
		{-1, 10, -1},
		{0, 10, 0},
		{10, 10, 1},
		{math.MinInt64, 1, math.MinInt64},
		{math.MaxInt64, 1, math.MaxInt64},
		{math.MinInt64, 10, -922337203685477581},
		{math.MaxInt64, 10, 922337203685477580},
	} {
		v, err := NewPartitionValue(&ConstantValue{Value: tc.source, Typ: NotNullLong}, tc.width)
		if err != nil {
			t.Fatal(err)
		}
		got, err := v.Evaluate(nil)
		if err != nil || got != tc.want || !v.Type().Equals(NotNullLong) {
			t.Fatalf("PartitionValue(%d,%d) = %v,%v; want %d", tc.source, tc.width, got, err, tc.want)
		}
	}
	for _, source := range []Value{nil, &ConstantValue{Value: int32(1), Typ: NotNullInt}, &ConstantValue{Value: "1", Typ: NotNullString}} {
		_, err := NewPartitionValue(source, 10)
		var typed *PartitionValueError
		if !errors.As(err, &typed) {
			t.Fatalf("invalid typed source = %v", err)
		}
	}
	for _, width := range []int64{0, -1} {
		if _, err := NewPartitionValue(&ConstantValue{Value: int64(1), Typ: NotNullLong}, width); err == nil {
			t.Fatal("invalid width accepted")
		}
	}
	v, err := NewPartitionValue(&ConstantValue{Value: nil, Typ: NullableLong}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Type().Equals(NotNullLong) {
		t.Fatalf("nullable-source result type = %v, want fixed NOT NULL BIGINT", v.Type())
	}
	_, err = v.Evaluate(nil)
	var routing *partition.RoutingError
	if !errors.As(err, &routing) {
		t.Fatalf("NULL routing = %v", err)
	}
	if _, err := WithChildrenChecked(v, nil); err == nil {
		t.Fatal("invalid rebuild arity accepted")
	}
	if _, err := WithChildrenChecked(v, []Value{LiteralValue("bad")}); err == nil {
		t.Fatal("invalid rebuilt type accepted")
	}
}

func TestPartitionValueUncheckedReplacement(t *testing.T) {
	t.Parallel()
	source := &ConstantValue{Value: int64(31), Typ: NotNullLong}
	v, err := NewPartitionValue(source, 10)
	if err != nil {
		t.Fatal(err)
	}
	replacement := &ConstantValue{Value: int64(-11), Typ: NotNullLong}
	hit := false
	replaced := Replace(v, func(node Value) Value {
		if node == source {
			hit = true
			return replacement
		}
		return node
	})
	if !hit {
		t.Fatal("replacement did not visit the source")
	}
	for label, result := range map[string]Value{"Replace": replaced, "WithChildren": WithChildren(v, []Value{replacement})} {
		got, ok := result.(*PartitionValue)
		if !ok || got == v || got.Source != replacement || got.Width != 10 {
			t.Fatalf("%s lost replacement or width: %#v", label, result)
		}
		if coordinate, err := got.Evaluate(nil); err != nil || coordinate != int64(-2) {
			t.Fatalf("%s coordinate = %v,%v", label, coordinate, err)
		}
	}
	bad := &ConstantValue{Value: "bad", Typ: NotNullString}
	if result := Replace(v, func(node Value) Value {
		if node == source {
			return bad
		}
		return node
	}); result != nil {
		t.Fatalf("invalid Replace retained a value: %#v", result)
	}
	for _, children := range [][]Value{nil, {bad}, {nil}, {replacement, replacement}} {
		if result := WithChildren(v, children); result != nil {
			t.Fatalf("invalid WithChildren(%v) retained a value: %#v", children, result)
		}
	}
	if v.Source != source || v.Width != 10 {
		t.Fatal("replacement mutated the original")
	}
}

func TestPartitionValueAccessorIdentityIncludesSource(t *testing.T) {
	t.Parallel()
	row := NewRecordType("T", false, []Field{{Name: "SOURCE", FieldType: NotNullLong}, {Name: "OTHER", Ordinal: 1, FieldType: NotNullLong}})
	root, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("row"), row)
	if err != nil {
		t.Fatal(err)
	}
	build := func(ordinal int) *PartitionValue {
		source, err := ResolveFieldOrdinals(root, []int{ordinal})
		if err != nil {
			t.Fatal(err)
		}
		v, err := NewPartitionValue(source, 10)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	a, same, other := build(0), build(0), build(1)
	if a.Source == same.Source {
		t.Fatal("positive control reused the source object")
	}
	for _, pair := range [][2]Value{{a, same}, {same, a}} {
		if !ColumnNamePathsEqual(pair[0], pair[1]) {
			t.Fatal("independently built same-source coordinates did not match")
		}
	}
	for _, pair := range [][2]Value{{a, other}, {other, a}} {
		if ColumnNamePathsEqual(pair[0], pair[1]) {
			t.Fatal("equal-width coordinates over different columns matched")
		}
	}
}

func TestPartitionValueTypedDecoding(t *testing.T) {
	t.Parallel()
	long := func(x int64) Value { return &ConstantValue{Value: x, Typ: NotNullLong} }
	nonliteral, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("width"), NotNullLong)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		source, width Value
		message       string
		routing       bool
	}{
		{"source INT", &ConstantValue{Value: int32(1), Typ: NotNullInt}, long(10), "source must have BIGINT type", false},
		{"source STRING", &ConstantValue{Value: "1", Typ: NotNullString}, long(10), "source must have BIGINT type", false},
		{"nonliteral width", long(1), nonliteral, "serialized interval must be a BIGINT literal", false},
		{"NULL width node", long(1), NewNullValue(NullableLong), "serialized interval must be a BIGINT literal", false},
		{"NULL width literal", long(1), &ConstantValue{Value: nil, Typ: NullableLong}, "serialized interval must be a BIGINT literal", false},
		{"INT width", long(1), &ConstantValue{Value: int32(10), Typ: NotNullInt}, "serialized interval must be a BIGINT literal", false},
		{"STRING width", long(1), &ConstantValue{Value: "10", Typ: NotNullString}, "serialized interval must be a BIGINT literal", false},
		{"zero width", long(1), long(0), "interval width must be positive", true},
		{"negative width", long(1), long(-1), "interval width must be positive", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := NewSerializationContext()
			source, err := c.ValueToProto(tc.source)
			if err != nil {
				t.Fatal(err)
			}
			var width *gen.PValue
			if literal, ok := tc.width.(*ConstantValue); ok && literal.Value == nil {
				resultType, err := c.TypeToProto(literal.Type())
				if err != nil {
					t.Fatal(err)
				}
				// An empty primitive object carries NULL; result_type retains LONG.
				width = &gen.PValue{SpecificValue: &gen.PValue_LiteralValue{LiteralValue: &gen.PLiteralValue{
					ResultType: resultType,
					Value:      &gen.PComparableObject{SpecificObject: &gen.PComparableObject_PrimitiveObject{PrimitiveObject: &gen.Value{}}},
				}}}
				decoded, err := NewSerializationContext().ValueFromProto(width)
				constant, ok := decoded.(*ConstantValue)
				if err != nil || !ok || constant.Value != nil || !constant.Type().Equals(NullableLong) {
					t.Fatalf("NULL width fixture did not decode as a nullable-LONG literal: %#v, %v", decoded, err)
				}
			} else {
				width, err = c.ValueToProto(tc.width)
				if err != nil {
					t.Fatal(err)
				}
			}
			payload, err := proto.Marshal(&gen.PDerivedValue{Children: []*gen.PValue{source, width}})
			if err != nil {
				t.Fatal(err)
			}
			wire := &gen.PValue{SpecificValue: &gen.PValue_AdditionalValues{AdditionalValues: &anypb.Any{TypeUrl: partitionValueTypeURL, Value: payload}}}
			_, err = NewSerializationContext().ValueFromProto(wire)
			if tc.routing {
				var typed *partition.RoutingError
				if !errors.As(err, &typed) || typed.Reason != tc.message {
					t.Fatalf("typed width rejection = %v", err)
				}
			} else {
				var typed *PartitionValueError
				if !errors.As(err, &typed) || typed.Message != tc.message {
					t.Fatalf("typed decode rejection = %v", err)
				}
			}
		})
	}
}

func TestPartitionValueRebaseEqualityHashAndSerialization(t *testing.T) {
	t.Parallel()
	oldAlias, newAlias := NamedCorrelationIdentifier("old"), NamedCorrelationIdentifier("new")
	rowType := NewRecordType("T", false, []Field{{Name: "SOURCE", FieldType: NullableLong}})
	root, err := NewQuantifiedObjectValue(oldAlias, rowType)
	if err != nil {
		t.Fatal(err)
	}
	source, err := ResolveFieldOrdinals(root, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewPartitionValue(source, 10)
	if err != nil {
		t.Fatal(err)
	}
	aliases := mustAliasMap(t, AliasPair{Source: oldAlias, Target: newAlias})
	rebased, err := RebaseValueChecked(v, aliases)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := GetCorrelatedToOfValue(rebased)[newAlias]; !ok || len(GetCorrelatedToOfValue(rebased)) != 1 {
		t.Fatalf("lost rebased correlation: %v", GetCorrelatedToOfValue(rebased))
	}
	if _, old := GetCorrelatedToOfValue(rebased)[oldAlias]; old {
		t.Fatal("retained old correlation")
	}
	if !SemanticEqualsUnderAliasMap(v, rebased, aliases) || SemanticHashCode(v) != SemanticHashCode(rebased) {
		t.Fatal("alias-invariant partition identity broken")
	}
	other, err := NewPartitionValue(source, 11)
	if err != nil {
		t.Fatal(err)
	}
	if !ColumnNamePathsEqual(v, v) || ColumnNamePathsEqual(v, other) || ColumnNamePathsEqual(v, source) || ColumnNamePathsEqual(source, v) {
		t.Fatal("partition accessor identity lost the wrapper or interval")
	}
	if SemanticEqualsUnderAliasMap(v, other, nil) || SemanticHashCode(v) == SemanticHashCode(other) {
		t.Fatal("interval width missing from value identity")
	}
	mapped := MapFieldValues(v, func(*fieldValue) Value { return &ConstantValue{Value: int64(-11), Typ: NotNullLong} })
	if mapped == nil {
		t.Fatal("field mapping dropped partition value")
	}
	if got, err := mapped.Evaluate(nil); err != nil || got != int64(-2) {
		t.Fatalf("mapped partition = %v,%v", got, err)
	}
	if !strings.Contains(ExplainValue(v), "fdb_partition_v1(") || !strings.Contains(ExplainValue(v), ", 10)") {
		t.Fatalf("explain dropped source or interval: %s", ExplainValue(v))
	}
	encoded, err := NewSerializationContext().ValueToProto(v)
	if err != nil {
		t.Fatal(err)
	}
	back, err := NewSerializationContext().ValueFromProto(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !SemanticEqualsUnderAliasMap(v, back, nil) || SemanticHashCode(v) != SemanticHashCode(back) {
		t.Fatal("serialized partition value lost identity")
	}
	again, err := NewSerializationContext().ValueToProto(back)
	if err != nil || !proto.Equal(encoded, again) {
		t.Fatalf("serialization changed: %v", err)
	}
	for _, payload := range [][]byte{nil, {0xff}} {
		_, err := NewSerializationContext().ValueFromProto(&gen.PValue{SpecificValue: &gen.PValue_AdditionalValues{AdditionalValues: &anypb.Any{TypeUrl: partitionValueTypeURL, Value: payload}}})
		var typed *PartitionValueError
		if !errors.As(err, &typed) {
			t.Fatalf("malformed serialized partition value = %v", err)
		}
		if len(payload) != 0 && (typed.Cause == nil || !errors.Is(err, proto.Error)) {
			t.Fatalf("malformed protobuf cause was discarded: %v", err)
		}
	}
}
