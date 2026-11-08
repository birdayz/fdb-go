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
		if err == nil {
			t.Fatal("malformed serialized partition value accepted")
		}
	}
}
