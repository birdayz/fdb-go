package recordlayer

import (
	"testing"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
)

func TestSplitKeyExpressionEvaluateBasic(t *testing.T) {
	t.Parallel()

	// FanOut of 6 values, splitSize=2 -> 3 results of 2 elements each
	msg := &gen.Order{
		OrderId: proto.Int64(1),
		Tags:    []string{"a", "b", "c", "d", "e", "f"},
	}

	expr := Split(FanOut("tags"), 2)
	results, err := expr.Evaluate(nil, msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 batches, got %d", len(results))
	}
	for i, batch := range results {
		if len(batch) != 2 {
			t.Fatalf("batch %d: expected 2 elements, got %d", i, len(batch))
		}
	}
	if results[0][0] != "a" || results[0][1] != "b" {
		t.Fatalf("batch 0: expected [a, b], got %v", results[0])
	}
	if results[1][0] != "c" || results[1][1] != "d" {
		t.Fatalf("batch 1: expected [c, d], got %v", results[1])
	}
	if results[2][0] != "e" || results[2][1] != "f" {
		t.Fatalf("batch 2: expected [e, f], got %v", results[2])
	}
}

func TestSplitKeyExpressionEvaluateUnevenError(t *testing.T) {
	t.Parallel()

	// Java throws RecordCoreException if not evenly divisible
	msg := &gen.Order{
		OrderId: proto.Int64(1),
		Tags:    []string{"a", "b", "c", "d", "e", "f", "g"},
	}

	expr := Split(FanOut("tags"), 3)
	_, err := expr.Evaluate(nil, msg)
	if err == nil {
		t.Fatal("expected error for 7 values with splitSize=3")
	}
}

func TestSplitKeyExpressionEvaluateEmpty(t *testing.T) {
	t.Parallel()

	// FanOut of 0 values -> nil
	msg := &gen.Order{
		OrderId: proto.Int64(1),
		Tags:    []string{},
	}

	expr := Split(FanOut("tags"), 3)
	results, err := expr.Evaluate(nil, msg)
	if err != nil {
		t.Fatal(err)
	}
	if results != nil {
		t.Fatalf("expected nil for empty FanOut, got %v", results)
	}
}

func TestSplitKeyExpressionEvaluateSplitSizeOne(t *testing.T) {
	t.Parallel()

	// splitSize=1: each value becomes its own batch
	msg := &gen.Order{
		OrderId: proto.Int64(1),
		Tags:    []string{"x", "y", "z"},
	}

	expr := Split(FanOut("tags"), 1)
	results, err := expr.Evaluate(nil, msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 batches, got %d", len(results))
	}
	for i, batch := range results {
		if len(batch) != 1 {
			t.Fatalf("batch %d: expected 1 element, got %d", i, len(batch))
		}
	}
	if results[0][0] != "x" {
		t.Fatalf("batch 0: expected [x], got %v", results[0])
	}
	if results[1][0] != "y" {
		t.Fatalf("batch 1: expected [y], got %v", results[1])
	}
	if results[2][0] != "z" {
		t.Fatalf("batch 2: expected [z], got %v", results[2])
	}
}

func TestSplitKeyExpressionEvaluateLargerBatches(t *testing.T) {
	t.Parallel()

	// 12 values, splitSize=3 -> 4 batches
	msg := &gen.Order{
		OrderId: proto.Int64(1),
		Tags:    []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"},
	}

	expr := Split(FanOut("tags"), 3)
	results, err := expr.Evaluate(nil, msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 4 {
		t.Fatalf("expected 4 batches, got %d", len(results))
	}
	for i, batch := range results {
		if len(batch) != 3 {
			t.Fatalf("batch %d: expected 3 elements, got %d", i, len(batch))
		}
	}
	if results[3][0] != "j" || results[3][1] != "k" || results[3][2] != "l" {
		t.Fatalf("batch 3: expected [j, k, l], got %v", results[3])
	}
}

func TestSplitKeyExpressionProtoRoundtrip(t *testing.T) {
	t.Parallel()

	original := Split(FanOut("tags"), 3)
	p := original.ToKeyExpression()
	if p.Split == nil {
		t.Fatal("expected Split to be set")
	}
	if p.Split.GetSplitSize() != 3 {
		t.Fatalf("expected split_size 3, got %d", p.Split.GetSplitSize())
	}

	restored, err := KeyExpressionFromProto(p)
	if err != nil {
		t.Fatal(err)
	}
	if !keyExpressionEquals(original, restored) {
		t.Fatal("roundtrip mismatch")
	}

	splitExpr, ok := restored.(*SplitKeyExpression)
	if !ok {
		t.Fatalf("expected *SplitKeyExpression, got %T", restored)
	}
	if splitExpr.splitSize != 3 {
		t.Fatalf("expected splitSize 3, got %d", splitExpr.splitSize)
	}
}

func TestSplitKeyExpressionProtoWireRoundtrip(t *testing.T) {
	t.Parallel()

	original := Split(FanOut("tags"), 2)
	p := original.ToKeyExpression()

	data, err := proto.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}

	var restored gen.KeyExpression
	if err := proto.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}

	result, err := KeyExpressionFromProto(&restored)
	if err != nil {
		t.Fatal(err)
	}
	if !keyExpressionEquals(original, result) {
		t.Fatal("wire roundtrip mismatch")
	}
}

func TestSplitKeyExpressionColumnSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		splitSize int
		want      int
	}{
		{"size_1", 1, 1},
		{"size_2", 2, 2},
		{"size_3", 3, 3},
		{"size_5", 5, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			expr := Split(FanOut("tags"), tt.splitSize)
			got := expr.ColumnSize()
			if got != tt.want {
				t.Fatalf("expected column size %d, got %d", tt.want, got)
			}
		})
	}
}

func TestSplitKeyExpressionCreatesDuplicates(t *testing.T) {
	t.Parallel()

	expr := Split(FanOut("tags"), 3)
	if !createsDuplicates(expr) {
		t.Fatal("SplitKeyExpression should always createsDuplicates")
	}
}

func TestSplitKeyExpressionFieldNames(t *testing.T) {
	t.Parallel()

	expr := Split(FanOut("tags"), 3)
	names := expr.FieldNames()
	if len(names) != 1 || names[0] != "tags" {
		t.Fatalf("expected [tags], got %v", names)
	}
}

func TestSplitKeyExpressionEquals(t *testing.T) {
	t.Parallel()

	t.Run("equal", func(t *testing.T) {
		t.Parallel()
		a := Split(FanOut("tags"), 3)
		b := Split(FanOut("tags"), 3)
		if !keyExpressionEquals(a, b) {
			t.Fatal("structurally identical SplitKeyExpressions should be equal")
		}
	})

	t.Run("different_split_size", func(t *testing.T) {
		t.Parallel()
		a := Split(FanOut("tags"), 3)
		b := Split(FanOut("tags"), 2)
		if keyExpressionEquals(a, b) {
			t.Fatal("different splitSize should not be equal")
		}
	})

	t.Run("different_joined", func(t *testing.T) {
		t.Parallel()
		a := Split(FanOut("tags"), 3)
		b := Split(FanOut("names"), 3)
		if keyExpressionEquals(a, b) {
			t.Fatal("different joined expression should not be equal")
		}
	})

	t.Run("different_type", func(t *testing.T) {
		t.Parallel()
		a := Split(FanOut("tags"), 3)
		b := FanOut("tags")
		if keyExpressionEquals(a, b) {
			t.Fatal("Split should not equal Field")
		}
	})
}

func TestSplitKeyExpressionNormalizeKeyForPositions(t *testing.T) {
	t.Parallel()

	// Java: Collections.nCopies(splitSize, getJoined())
	joined := FanOut("tags")
	expr := Split(joined, 3)
	normalized := normalizeKeyForPositions(expr)
	if len(normalized) != 3 {
		t.Fatalf("expected 3 normalized positions, got %d", len(normalized))
	}
	for i, n := range normalized {
		if !keyExpressionEquals(n, joined) {
			t.Fatalf("position %d: expected joined expression, got different", i)
		}
	}
}

func TestSplitKeyExpressionNilMessage(t *testing.T) {
	t.Parallel()

	expr := Split(FanOut("tags"), 3)
	results, err := expr.Evaluate(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if results != nil {
		t.Fatalf("expected nil for nil message, got %v", results)
	}
}

func TestSplitKeyExpressionGetters(t *testing.T) {
	t.Parallel()

	joined := FanOut("tags")
	expr := Split(joined, 5)
	if expr.GetSplitSize() != 5 {
		t.Fatalf("expected splitSize 5, got %d", expr.GetSplitSize())
	}
	if !keyExpressionEquals(expr.GetJoined(), joined) {
		t.Fatal("GetJoined should return the joined expression")
	}
}
