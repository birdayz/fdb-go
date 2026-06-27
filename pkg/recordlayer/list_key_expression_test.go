package recordlayer

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// assertTupleElem asserts that elem is a tuple.Tuple with the given expected values.
func assertTupleElem(t *testing.T, label string, elem any, expected ...any) {
	t.Helper()
	var nested tuple.Tuple
	switch v := elem.(type) {
	case tuple.Tuple:
		nested = v
	default:
		t.Fatalf("%s: expected tuple.Tuple, got %T", label, elem)
		return
	}
	if len(nested) != len(expected) {
		t.Fatalf("%s: expected %d elements, got %d: %v", label, len(expected), len(nested), nested)
	}
	for i, want := range expected {
		if nested[i] != want {
			t.Fatalf("%s[%d]: expected %v (%T), got %v (%T)", label, i, want, want, nested[i], nested[i])
		}
	}
}

func TestListKeyExpressionBasic(t *testing.T) {
	t.Parallel()

	// List(Field("order_id"), Field("price")) where order_id=1, price=100
	// should produce [tuple(1), tuple(100)] — each child's value wrapped in a nested tuple.
	expr := ListExpr(Field("order_id"), Field("price"))

	msg := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
	results, err := expr.Evaluate(nil, msg)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if len(results[0]) != 2 {
		t.Fatalf("expected 2 elements per result, got %d", len(results[0]))
	}

	assertTupleElem(t, "element 0", results[0][0], int64(1))
	assertTupleElem(t, "element 1", results[0][1], int64(100))
}

func TestListKeyExpressionSingleChild(t *testing.T) {
	t.Parallel()

	expr := ListExpr(Field("order_id"))
	msg := &gen.Order{OrderId: proto.Int64(42)}
	results, err := expr.Evaluate(nil, msg)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if len(results[0]) != 1 {
		t.Fatalf("expected 1 element, got %d", len(results[0]))
	}

	assertTupleElem(t, "element 0", results[0][0], int64(42))
}

func TestListKeyExpressionNoChildren(t *testing.T) {
	t.Parallel()

	expr := ListExpr()
	msg := &gen.Order{OrderId: proto.Int64(1)}
	results, err := expr.Evaluate(nil, msg)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if len(results[0]) != 0 {
		t.Fatalf("expected 0 elements, got %d", len(results[0]))
	}
}

func TestListKeyExpressionFanOutCrossProduct(t *testing.T) {
	t.Parallel()

	expr := ListExpr(FanOut("tags"), Field("order_id"))
	msg := &gen.Order{
		Tags:    []string{"alpha", "beta"},
		OrderId: proto.Int64(99),
	}
	results, err := expr.Evaluate(nil, msg)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results (cross-product), got %d", len(results))
	}

	assertTupleElem(t, "result[0][0]", results[0][0], "alpha")
	assertTupleElem(t, "result[0][1]", results[0][1], int64(99))
	assertTupleElem(t, "result[1][0]", results[1][0], "beta")
	assertTupleElem(t, "result[1][1]", results[1][1], int64(99))
}

func TestListKeyExpressionFanOutBothChildren(t *testing.T) {
	t.Parallel()

	expr := ListExpr(FanOut("tags"), FanOut("tags"))
	msg := &gen.Order{
		Tags: []string{"x", "y"},
	}
	results, err := expr.Evaluate(nil, msg)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("expected 4 results (2x2 cross-product), got %d", len(results))
	}
}

func TestListKeyExpressionEmptyFanOut(t *testing.T) {
	t.Parallel()

	expr := ListExpr(FanOut("tags"), Field("order_id"))
	msg := &gen.Order{
		Tags:    nil,
		OrderId: proto.Int64(1),
	}
	results, err := expr.Evaluate(nil, msg)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results (empty fanout), got %d", len(results))
	}
}

func TestListKeyExpressionMultiColumnChild(t *testing.T) {
	t.Parallel()

	expr := ListExpr(
		Concat(Field("order_id"), Field("price")),
		Field("order_id"),
	)
	msg := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
	results, err := expr.Evaluate(nil, msg)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if len(results[0]) != 2 {
		t.Fatalf("expected 2 elements, got %d", len(results[0]))
	}

	assertTupleElem(t, "element 0", results[0][0], int64(1), int64(100))
	assertTupleElem(t, "element 1", results[0][1], int64(1))
}

func TestListKeyExpressionProtoRoundtrip(t *testing.T) {
	t.Parallel()

	original := ListExpr(Field("order_id"), Field("price"))
	protoExpr := original.ToKeyExpression()

	if protoExpr.List == nil {
		t.Fatal("expected List proto field to be set")
	}
	if len(protoExpr.List.Child) != 2 {
		t.Fatalf("expected 2 proto children, got %d", len(protoExpr.List.Child))
	}

	reconstructed, err := KeyExpressionFromProto(protoExpr)
	if err != nil {
		t.Fatalf("KeyExpressionFromProto: %v", err)
	}

	listExpr, ok := reconstructed.(*ListKeyExpression)
	if !ok {
		t.Fatalf("expected *ListKeyExpression, got %T", reconstructed)
	}
	if len(listExpr.children) != 2 {
		t.Fatalf("expected 2 children, got %d", len(listExpr.children))
	}

	if !keyExpressionEquals(original, reconstructed) {
		t.Fatal("proto roundtrip produced structurally different expression")
	}
}

func TestListKeyExpressionColumnSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		expr     *ListKeyExpression
		expected int
	}{
		{"two children", ListExpr(Field("a"), Field("b")), 2},
		{"three children", ListExpr(Field("a"), Field("b"), Field("c")), 3},
		{"single child", ListExpr(Field("a")), 1},
		{"no children", ListExpr(), 0},
		{"composite child", ListExpr(Concat(Field("a"), Field("b")), Field("c")), 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.expr.ColumnSize()
			if got != tc.expected {
				t.Fatalf("keyExpressionColumnSize: expected %d, got %d", tc.expected, got)
			}
		})
	}
}

func TestListKeyExpressionFieldNames(t *testing.T) {
	t.Parallel()

	expr := ListExpr(Field("order_id"), Field("price"))
	names := expr.FieldNames()
	if len(names) != 2 || names[0] != "order_id" || names[1] != "price" {
		t.Fatalf("expected [order_id, price], got %v", names)
	}
}

func TestListKeyExpressionCreatesDuplicates(t *testing.T) {
	t.Parallel()

	if createsDuplicates(ListExpr(Field("order_id"), Field("price"))) {
		t.Fatal("expected no duplicates without fan-out")
	}

	if !createsDuplicates(ListExpr(FanOut("tags"), Field("order_id"))) {
		t.Fatal("expected duplicates with fan-out child")
	}
}

func TestListKeyExpressionEquals(t *testing.T) {
	t.Parallel()

	a := ListExpr(Field("order_id"), Field("price"))
	b := ListExpr(Field("order_id"), Field("price"))
	c := ListExpr(Field("order_id"))
	d := ListExpr(Field("order_id"), Field("order_id"))

	if !keyExpressionEquals(a, b) {
		t.Fatal("identical lists should be equal")
	}
	if keyExpressionEquals(a, c) {
		t.Fatal("different-length lists should not be equal")
	}
	if keyExpressionEquals(a, d) {
		t.Fatal("different children should not be equal")
	}
	if keyExpressionEquals(a, Field("order_id")) {
		t.Fatal("list should not equal field")
	}
}

func TestListKeyExpressionNormalizeKeyForPositions(t *testing.T) {
	t.Parallel()

	single := ListExpr(Field("a"))
	norm := normalizeKeyForPositions(single)
	if len(norm) != 1 {
		t.Fatalf("single child: expected 1 position, got %d", len(norm))
	}
	if _, ok := norm[0].(*ListKeyExpression); !ok {
		t.Fatalf("single child: expected ListKeyExpression, got %T", norm[0])
	}

	multi := ListExpr(Field("a"), Field("b"), Field("c"))
	norm = normalizeKeyForPositions(multi)
	if len(norm) != 3 {
		t.Fatalf("multi child: expected 3 positions, got %d", len(norm))
	}
	for i, n := range norm {
		listN, ok := n.(*ListKeyExpression)
		if !ok {
			t.Fatalf("position %d: expected *ListKeyExpression, got %T", i, n)
		}
		if len(listN.children) != 1 {
			t.Fatalf("position %d: expected 1 child, got %d", i, len(listN.children))
		}
	}

	empty := ListExpr()
	norm = normalizeKeyForPositions(empty)
	if len(norm) != 0 {
		t.Fatalf("empty: expected 0 positions, got %d", len(norm))
	}
}

func TestListKeyExpressionNestedTuplesPack(t *testing.T) {
	t.Parallel()

	expr := ListExpr(Field("order_id"), Field("price"))
	msg := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
	results, err := expr.Evaluate(nil, msg)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	// Build a tuple.Tuple from the result and pack it.
	// This will panic if nested values are not tuple.Tuple.
	var tup tuple.Tuple
	for _, v := range results[0] {
		tup = append(tup, v)
	}
	packed := tup.Pack()
	if len(packed) == 0 {
		t.Fatal("packed tuple should not be empty")
	}

	// Unpack and verify nested tuples survive roundtrip
	unpacked, err := tuple.Unpack(packed)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if len(unpacked) != 2 {
		t.Fatalf("expected 2 elements after unpack, got %d", len(unpacked))
	}

	assertTupleElem(t, "unpacked[0]", unpacked[0], int64(1))
	assertTupleElem(t, "unpacked[1]", unpacked[1], int64(100))
}

func TestListKeyExpressionNilMessage(t *testing.T) {
	t.Parallel()

	expr := ListExpr(Field("order_id"), Field("price"))
	results, err := expr.Evaluate(nil, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if len(results[0]) != 2 {
		t.Fatalf("expected 2 elements, got %d", len(results[0]))
	}

	// Each should be a tuple.Tuple containing nil
	for i, elem := range results[0] {
		nested, ok := elem.(tuple.Tuple)
		if !ok {
			t.Fatalf("element %d: expected tuple.Tuple, got %T", i, elem)
		}
		if len(nested) != 1 || nested[0] != nil {
			t.Fatalf("element %d: expected tuple(nil), got %v", i, nested)
		}
	}
}

func TestListKeyExpressionGetChildren(t *testing.T) {
	t.Parallel()

	children := []KeyExpression{Field("a"), Field("b"), Field("c")}
	expr := ListExpr(children...)
	got := expr.GetChildren()
	if len(got) != 3 {
		t.Fatalf("expected 3 children, got %d", len(got))
	}
}
