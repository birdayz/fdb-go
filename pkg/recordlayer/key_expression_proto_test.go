package recordlayer

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"google.golang.org/protobuf/proto"
)

func TestFieldKeyExpressionRoundtrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		expr    KeyExpression
		fanType gen.Field_FanType
	}{
		{"scalar", Field("order_id"), gen.Field_SCALAR},
		{"fan_out", FanOut("tags"), gen.Field_FAN_OUT},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := tt.expr.ToKeyExpression()
			if p.Field == nil {
				t.Fatal("expected Field to be set")
			}
			if p.Field.GetFanType() != tt.fanType {
				t.Fatalf("fan type: got %v, want %v", p.Field.GetFanType(), tt.fanType)
			}

			restored, err := KeyExpressionFromProto(p)
			if err != nil {
				t.Fatal(err)
			}
			if !keyExpressionEquals(tt.expr, restored) {
				t.Fatalf("roundtrip mismatch")
			}
		})
	}
}

func TestCompositeKeyExpressionRoundtrip(t *testing.T) {
	t.Parallel()
	expr := Concat(Field("a"), Field("b"), FanOut("c"))
	p := expr.ToKeyExpression()
	if p.Then == nil {
		t.Fatal("expected Then to be set")
	}
	if len(p.Then.Child) != 3 {
		t.Fatalf("children: got %d, want 3", len(p.Then.Child))
	}

	restored, err := KeyExpressionFromProto(p)
	if err != nil {
		t.Fatal(err)
	}
	if !keyExpressionEquals(expr, restored) {
		t.Fatal("roundtrip mismatch")
	}
}

func TestNestingKeyExpressionRoundtrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		expr KeyExpression
	}{
		{"simple", Nest("flower", Field("type"))},
		{"fan_out", NestFanOut("items", Field("price"))},
		{"deep", Nest("flower", Concat(Field("type"), Field("color")))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := tt.expr.ToKeyExpression()
			if p.Nesting == nil {
				t.Fatal("expected Nesting to be set")
			}

			restored, err := KeyExpressionFromProto(p)
			if err != nil {
				t.Fatal(err)
			}
			if !keyExpressionEquals(tt.expr, restored) {
				t.Fatal("roundtrip mismatch")
			}
		})
	}
}

func TestEmptyKeyExpressionRoundtrip(t *testing.T) {
	t.Parallel()
	expr := EmptyKey()
	p := expr.ToKeyExpression()
	if p.Empty == nil {
		t.Fatal("expected Empty to be set")
	}

	restored, err := KeyExpressionFromProto(p)
	if err != nil {
		t.Fatal(err)
	}
	if !keyExpressionEquals(expr, restored) {
		t.Fatal("roundtrip mismatch")
	}
}

func TestRecordTypeKeyExpressionRoundtrip(t *testing.T) {
	t.Parallel()

	t.Run("bare", func(t *testing.T) {
		t.Parallel()
		expr := RecordTypeKey()
		p := expr.ToKeyExpression()
		if p.RecordTypeKey == nil {
			t.Fatal("expected RecordTypeKey to be set")
		}
		restored, err := KeyExpressionFromProto(p)
		if err != nil {
			t.Fatal(err)
		}
		if !keyExpressionEquals(expr, restored) {
			t.Fatal("roundtrip mismatch")
		}
	})

	t.Run("with_nested", func(t *testing.T) {
		t.Parallel()
		// RecordTypeKey with nested serializes as Then{RecordTypeKey, nested}
		rtk := RecordTypeKey()
		expr := rtk.Nest(Field("order_id"))
		p := expr.ToKeyExpression()
		if p.Then == nil {
			t.Fatal("expected Then to be set for nested RecordTypeKey")
		}
		if len(p.Then.Child) != 2 {
			t.Fatalf("children: got %d, want 2", len(p.Then.Child))
		}
		if p.Then.Child[0].RecordTypeKey == nil {
			t.Fatal("first child should be RecordTypeKey")
		}
		if p.Then.Child[1].Field == nil {
			t.Fatal("second child should be Field")
		}
	})
}

func TestKeyExpressionFromProtoErrors(t *testing.T) {
	t.Parallel()

	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		_, err := KeyExpressionFromProto(nil)
		if err == nil {
			t.Fatal("expected error for nil proto")
		}
	})

	t.Run("empty_proto", func(t *testing.T) {
		t.Parallel()
		_, err := KeyExpressionFromProto(&gen.KeyExpression{})
		if err == nil {
			t.Fatal("expected error for empty proto")
		}
	})

	t.Run("then_too_few_children", func(t *testing.T) {
		t.Parallel()
		ft := gen.Field_SCALAR
		_, err := KeyExpressionFromProto(&gen.KeyExpression{
			Then: &gen.Then{
				Child: []*gen.KeyExpression{
					{Field: &gen.Field{FieldName: proto.String("x"), FanType: &ft}},
				},
			},
		})
		if err == nil {
			t.Fatal("expected error for Then with 1 child")
		}
	})

	t.Run("nesting_no_parent", func(t *testing.T) {
		t.Parallel()
		_, err := KeyExpressionFromProto(&gen.KeyExpression{
			Nesting: &gen.Nesting{
				Child: &gen.KeyExpression{Empty: &gen.Empty{}},
			},
		})
		if err == nil {
			t.Fatal("expected error for Nesting without parent")
		}
	})

	t.Run("split_size_zero", func(t *testing.T) {
		t.Parallel()
		// Crafted proto with Split.split_size = 0 must error, not panic
		// (pre-swingshift-35 would have hit Split()'s invariant panic).
		zero := int32(0)
		_, err := KeyExpressionFromProto(&gen.KeyExpression{
			Split: &gen.Split{
				SplitSize: &zero,
				Joined:    &gen.KeyExpression{Empty: &gen.Empty{}},
			},
		})
		if err == nil {
			t.Fatal("expected error for split_size=0, got nil")
		}
	})

	t.Run("key_with_value_split_point_negative", func(t *testing.T) {
		t.Parallel()
		// Crafted proto with KeyWithValue.split_point < 0 must error
		// (pre-swingshift-35 would have propagated to downstream slicing).
		ft := gen.Field_SCALAR
		neg := int32(-1)
		_, err := KeyExpressionFromProto(&gen.KeyExpression{
			KeyWithValue: &gen.KeyWithValue{
				InnerKey:   &gen.KeyExpression{Field: &gen.Field{FieldName: proto.String("x"), FanType: &ft}},
				SplitPoint: &neg,
			},
		})
		if err == nil {
			t.Fatal("expected error for negative split_point, got nil")
		}
	})

	t.Run("key_with_value_split_point_too_large", func(t *testing.T) {
		t.Parallel()
		// Crafted proto with KeyWithValue.split_point > inner.ColumnSize()
		// must error. Inner is a single field → columnSize = 1; splitPoint=5 is out.
		ft := gen.Field_SCALAR
		big := int32(5)
		_, err := KeyExpressionFromProto(&gen.KeyExpression{
			KeyWithValue: &gen.KeyWithValue{
				InnerKey:   &gen.KeyExpression{Field: &gen.Field{FieldName: proto.String("x"), FanType: &ft}},
				SplitPoint: &big,
			},
		})
		if err == nil {
			t.Fatal("expected error for split_point beyond inner columnSize, got nil")
		}
	})

	t.Run("depth_limit", func(t *testing.T) {
		t.Parallel()
		// Build a Nesting chain deeper than maxKeyExpressionDepth. Each
		// layer wraps the inner KeyExpression in another Nesting; the
		// depth guard must trip before the goroutine stack blows.
		ft := gen.Field_SCALAR
		var current *gen.KeyExpression = &gen.KeyExpression{
			Field: &gen.Field{FieldName: proto.String("leaf"), FanType: &ft},
		}
		for i := 0; i < maxKeyExpressionDepth+10; i++ {
			current = &gen.KeyExpression{
				Nesting: &gen.Nesting{
					Parent: &gen.Field{FieldName: proto.String("p"), FanType: &ft},
					Child:  current,
				},
			}
		}
		_, err := KeyExpressionFromProto(current)
		if err == nil {
			t.Fatal("expected depth-limit error, got nil")
		}
	})
}

func TestKeyExpressionProtoWireRoundtrip(t *testing.T) {
	t.Parallel()
	// Test that serializing to bytes and back preserves the expression
	expr := Concat(
		Field("a"),
		Nest("b", FanOut("c")),
		EmptyKey(),
	)

	p := expr.ToKeyExpression()
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
	if !keyExpressionEquals(expr, result) {
		t.Fatal("wire roundtrip mismatch")
	}
}

func TestLiteralKeyExpressionRoundtrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value any
	}{
		{"nil", nil},
		{"int64", int64(42)},
		{"string", "hello"},
		{"bool", true},
		{"float64", 3.14},
		{"bytes", []byte{0xDE, 0xAD}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			expr := Literal(tt.value)
			p := expr.ToKeyExpression()
			if p.Value == nil {
				t.Fatal("expected Value to be set")
			}

			restored, err := KeyExpressionFromProto(p)
			if err != nil {
				t.Fatal(err)
			}
			if !keyExpressionEquals(expr, restored) {
				t.Fatalf("roundtrip mismatch for %v", tt.value)
			}
		})
	}
}

func TestLiteralKeyExpressionEvaluate(t *testing.T) {
	t.Parallel()

	t.Run("constant_value", func(t *testing.T) {
		t.Parallel()
		expr := Literal(int64(99))
		result, err := expr.Evaluate(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(result) != 1 || len(result[0]) != 1 {
			t.Fatalf("expected [[99]], got %v", result)
		}
		if result[0][0] != int64(99) {
			t.Fatalf("expected 99, got %v", result[0][0])
		}
	})

	t.Run("nil_value", func(t *testing.T) {
		t.Parallel()
		expr := Literal(nil)
		result, err := expr.Evaluate(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result[0][0] != nil {
			t.Fatalf("expected nil, got %v", result[0][0])
		}
	})

	t.Run("ignores_record", func(t *testing.T) {
		t.Parallel()
		// Literal should return the same value regardless of what message is passed
		expr := Literal("fixed")
		result, err := expr.Evaluate(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result[0][0] != "fixed" {
			t.Fatalf("expected 'fixed', got %v", result[0][0])
		}
	})

	t.Run("field_names_empty", func(t *testing.T) {
		t.Parallel()
		expr := Literal(int64(1))
		if len(expr.FieldNames()) != 0 {
			t.Fatalf("expected no field names, got %v", expr.FieldNames())
		}
	})

	t.Run("column_size", func(t *testing.T) {
		t.Parallel()
		if Literal(int64(1)).ColumnSize() != 1 {
			t.Fatal("expected column size 1")
		}
	})

	t.Run("in_composite", func(t *testing.T) {
		t.Parallel()
		// Literal works in Concat (composite key expression)
		expr := Concat(Literal("prefix"), Field("order_id"))
		if expr.ColumnSize() != 2 {
			t.Fatal("expected column size 2")
		}
	})
}
