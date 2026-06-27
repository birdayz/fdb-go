package recordlayer

import (
	"testing"

	gen "fdb.dev/gen"
	"google.golang.org/protobuf/proto"
)

// --------------------------------------------------------------------------
// 1. ConstantPredicate
// --------------------------------------------------------------------------

func TestConstantPredicateTrue(t *testing.T) {
	t.Parallel()
	pred := &gen.Predicate{
		ConstantPredicate: &gen.ConstantPredicate{
			Value: gen.ConstantPredicate_TRUE.Enum(),
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
	if !fn(order) {
		t.Fatal("TRUE predicate should return true for any message")
	}
	if !fn(&gen.Order{}) {
		t.Fatal("TRUE predicate should return true for zero-value message")
	}
}

func TestConstantPredicateFalse(t *testing.T) {
	t.Parallel()
	pred := &gen.Predicate{
		ConstantPredicate: &gen.ConstantPredicate{
			Value: gen.ConstantPredicate_FALSE.Enum(),
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	if fn(&gen.Order{OrderId: proto.Int64(1)}) {
		t.Fatal("FALSE predicate should return false")
	}
}

func TestConstantPredicateNull(t *testing.T) {
	t.Parallel()
	pred := &gen.Predicate{
		ConstantPredicate: &gen.ConstantPredicate{
			Value: gen.ConstantPredicate_NULL.Enum(),
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	if fn(&gen.Order{OrderId: proto.Int64(1)}) {
		t.Fatal("NULL predicate should return false")
	}
}

// --------------------------------------------------------------------------
// 2. ValuePredicate with SimpleComparison
// --------------------------------------------------------------------------

func TestValuePredicateEquals(t *testing.T) {
	t.Parallel()
	pred := &gen.Predicate{
		ValuePredicate: &gen.ValuePredicate{
			Value: []string{"price"},
			Comparison: &gen.Comparison{
				SimpleComparison: &gen.SimpleComparison{
					Type:    gen.ComparisonType_EQUALS.Enum(),
					Operand: &gen.Value{IntValue: proto.Int32(100)},
				},
			},
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	// Match
	if !fn(&gen.Order{Price: proto.Int32(100)}) {
		t.Fatal("EQUALS should match price=100")
	}
	// Non-match
	if fn(&gen.Order{Price: proto.Int32(200)}) {
		t.Fatal("EQUALS should not match price=200")
	}
}

func TestValuePredicateNotEquals(t *testing.T) {
	t.Parallel()
	pred := &gen.Predicate{
		ValuePredicate: &gen.ValuePredicate{
			Value: []string{"price"},
			Comparison: &gen.Comparison{
				SimpleComparison: &gen.SimpleComparison{
					Type:    gen.ComparisonType_NOT_EQUALS.Enum(),
					Operand: &gen.Value{IntValue: proto.Int32(100)},
				},
			},
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	if fn(&gen.Order{Price: proto.Int32(100)}) {
		t.Fatal("NOT_EQUALS should not match price=100")
	}
	if !fn(&gen.Order{Price: proto.Int32(200)}) {
		t.Fatal("NOT_EQUALS should match price=200")
	}
}

func TestValuePredicateLessThan(t *testing.T) {
	t.Parallel()
	pred := &gen.Predicate{
		ValuePredicate: &gen.ValuePredicate{
			Value: []string{"price"},
			Comparison: &gen.Comparison{
				SimpleComparison: &gen.SimpleComparison{
					Type:    gen.ComparisonType_LESS_THAN.Enum(),
					Operand: &gen.Value{IntValue: proto.Int32(500)},
				},
			},
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	if !fn(&gen.Order{Price: proto.Int32(100)}) {
		t.Fatal("LESS_THAN 500: price=100 should match")
	}
	if fn(&gen.Order{Price: proto.Int32(500)}) {
		t.Fatal("LESS_THAN 500: price=500 should not match (not strictly less)")
	}
	if fn(&gen.Order{Price: proto.Int32(999)}) {
		t.Fatal("LESS_THAN 500: price=999 should not match")
	}
}

func TestValuePredicateGreaterThan(t *testing.T) {
	t.Parallel()
	pred := &gen.Predicate{
		ValuePredicate: &gen.ValuePredicate{
			Value: []string{"price"},
			Comparison: &gen.Comparison{
				SimpleComparison: &gen.SimpleComparison{
					Type:    gen.ComparisonType_GREATER_THAN.Enum(),
					Operand: &gen.Value{IntValue: proto.Int32(500)},
				},
			},
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	if fn(&gen.Order{Price: proto.Int32(500)}) {
		t.Fatal("GREATER_THAN 500: price=500 should not match (not strictly greater)")
	}
	if !fn(&gen.Order{Price: proto.Int32(501)}) {
		t.Fatal("GREATER_THAN 500: price=501 should match")
	}
	if fn(&gen.Order{Price: proto.Int32(100)}) {
		t.Fatal("GREATER_THAN 500: price=100 should not match")
	}
}

func TestValuePredicateLessThanOrEquals(t *testing.T) {
	t.Parallel()
	pred := &gen.Predicate{
		ValuePredicate: &gen.ValuePredicate{
			Value: []string{"price"},
			Comparison: &gen.Comparison{
				SimpleComparison: &gen.SimpleComparison{
					Type:    gen.ComparisonType_LESS_THAN_OR_EQUALS.Enum(),
					Operand: &gen.Value{IntValue: proto.Int32(500)},
				},
			},
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	if !fn(&gen.Order{Price: proto.Int32(500)}) {
		t.Fatal("LTE 500: price=500 should match")
	}
	if !fn(&gen.Order{Price: proto.Int32(100)}) {
		t.Fatal("LTE 500: price=100 should match")
	}
	if fn(&gen.Order{Price: proto.Int32(501)}) {
		t.Fatal("LTE 500: price=501 should not match")
	}
}

func TestValuePredicateGreaterThanOrEquals(t *testing.T) {
	t.Parallel()
	pred := &gen.Predicate{
		ValuePredicate: &gen.ValuePredicate{
			Value: []string{"price"},
			Comparison: &gen.Comparison{
				SimpleComparison: &gen.SimpleComparison{
					Type:    gen.ComparisonType_GREATER_THAN_OR_EQUALS.Enum(),
					Operand: &gen.Value{IntValue: proto.Int32(500)},
				},
			},
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	if !fn(&gen.Order{Price: proto.Int32(500)}) {
		t.Fatal("GTE 500: price=500 should match")
	}
	if !fn(&gen.Order{Price: proto.Int32(999)}) {
		t.Fatal("GTE 500: price=999 should match")
	}
	if fn(&gen.Order{Price: proto.Int32(499)}) {
		t.Fatal("GTE 500: price=499 should not match")
	}
}

func TestValuePredicateIsNull(t *testing.T) {
	t.Parallel()
	pred := &gen.Predicate{
		ValuePredicate: &gen.ValuePredicate{
			Value: []string{"price"},
			Comparison: &gen.Comparison{
				SimpleComparison: &gen.SimpleComparison{
					Type: gen.ComparisonType_IS_NULL.Enum(),
				},
			},
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	// Price not set
	if !fn(&gen.Order{OrderId: proto.Int64(1)}) {
		t.Fatal("IS_NULL should match when price is unset")
	}
	// Price set
	if fn(&gen.Order{Price: proto.Int32(100)}) {
		t.Fatal("IS_NULL should not match when price is set")
	}
}

func TestValuePredicateNotNull(t *testing.T) {
	t.Parallel()
	pred := &gen.Predicate{
		ValuePredicate: &gen.ValuePredicate{
			Value: []string{"price"},
			Comparison: &gen.Comparison{
				SimpleComparison: &gen.SimpleComparison{
					Type: gen.ComparisonType_NOT_NULL.Enum(),
				},
			},
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	if !fn(&gen.Order{Price: proto.Int32(100)}) {
		t.Fatal("NOT_NULL should match when price is set")
	}
	if fn(&gen.Order{OrderId: proto.Int64(1)}) {
		t.Fatal("NOT_NULL should not match when price is unset")
	}
}

func TestValuePredicateStartsWith(t *testing.T) {
	t.Parallel()
	// Test on flower.type which is a string field
	pred := &gen.Predicate{
		ValuePredicate: &gen.ValuePredicate{
			Value: []string{"flower", "type"},
			Comparison: &gen.Comparison{
				SimpleComparison: &gen.SimpleComparison{
					Type:    gen.ComparisonType_STARTS_WITH.Enum(),
					Operand: &gen.Value{StringValue: proto.String("ros")},
				},
			},
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	if !fn(&gen.Order{Flower: &gen.Flower{Type: proto.String("rose")}}) {
		t.Fatal("STARTS_WITH 'ros' should match 'rose'")
	}
	if fn(&gen.Order{Flower: &gen.Flower{Type: proto.String("tulip")}}) {
		t.Fatal("STARTS_WITH 'ros' should not match 'tulip'")
	}
	if !fn(&gen.Order{Flower: &gen.Flower{Type: proto.String("ros")}}) {
		t.Fatal("STARTS_WITH 'ros' should match exact 'ros'")
	}
}

// --------------------------------------------------------------------------
// 3. Nested field path
// --------------------------------------------------------------------------

func TestValuePredicateNestedFieldPath(t *testing.T) {
	t.Parallel()
	// flower.type == "rose"
	pred := &gen.Predicate{
		ValuePredicate: &gen.ValuePredicate{
			Value: []string{"flower", "type"},
			Comparison: &gen.Comparison{
				SimpleComparison: &gen.SimpleComparison{
					Type:    gen.ComparisonType_EQUALS.Enum(),
					Operand: &gen.Value{StringValue: proto.String("rose")},
				},
			},
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	if !fn(&gen.Order{Flower: &gen.Flower{Type: proto.String("rose")}}) {
		t.Fatal("nested path should match flower.type=rose")
	}
	if fn(&gen.Order{Flower: &gen.Flower{Type: proto.String("tulip")}}) {
		t.Fatal("nested path should not match flower.type=tulip")
	}
	// flower not set at all
	if fn(&gen.Order{OrderId: proto.Int64(1)}) {
		t.Fatal("nested path should return false when intermediate message is nil")
	}
}

// --------------------------------------------------------------------------
// 4. AndPredicate
// --------------------------------------------------------------------------

func TestAndPredicate(t *testing.T) {
	t.Parallel()
	// price > 100 AND price < 1000
	pred := &gen.Predicate{
		AndPredicate: &gen.AndPredicate{
			Children: []*gen.Predicate{
				{
					ValuePredicate: &gen.ValuePredicate{
						Value: []string{"price"},
						Comparison: &gen.Comparison{
							SimpleComparison: &gen.SimpleComparison{
								Type:    gen.ComparisonType_GREATER_THAN.Enum(),
								Operand: &gen.Value{IntValue: proto.Int32(100)},
							},
						},
					},
				},
				{
					ValuePredicate: &gen.ValuePredicate{
						Value: []string{"price"},
						Comparison: &gen.Comparison{
							SimpleComparison: &gen.SimpleComparison{
								Type:    gen.ComparisonType_LESS_THAN.Enum(),
								Operand: &gen.Value{IntValue: proto.Int32(1000)},
							},
						},
					},
				},
			},
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	if !fn(&gen.Order{Price: proto.Int32(500)}) {
		t.Fatal("AND: price=500 should match (100 < 500 < 1000)")
	}
	if fn(&gen.Order{Price: proto.Int32(50)}) {
		t.Fatal("AND: price=50 should not match (not > 100)")
	}
	if fn(&gen.Order{Price: proto.Int32(1000)}) {
		t.Fatal("AND: price=1000 should not match (not < 1000)")
	}
}

// --------------------------------------------------------------------------
// 5. OrPredicate
// --------------------------------------------------------------------------

func TestOrPredicate(t *testing.T) {
	t.Parallel()
	// price == 100 OR price == 200
	pred := &gen.Predicate{
		OrPredicate: &gen.OrPredicate{
			Children: []*gen.Predicate{
				{
					ValuePredicate: &gen.ValuePredicate{
						Value: []string{"price"},
						Comparison: &gen.Comparison{
							SimpleComparison: &gen.SimpleComparison{
								Type:    gen.ComparisonType_EQUALS.Enum(),
								Operand: &gen.Value{IntValue: proto.Int32(100)},
							},
						},
					},
				},
				{
					ValuePredicate: &gen.ValuePredicate{
						Value: []string{"price"},
						Comparison: &gen.Comparison{
							SimpleComparison: &gen.SimpleComparison{
								Type:    gen.ComparisonType_EQUALS.Enum(),
								Operand: &gen.Value{IntValue: proto.Int32(200)},
							},
						},
					},
				},
			},
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	if !fn(&gen.Order{Price: proto.Int32(100)}) {
		t.Fatal("OR: price=100 should match")
	}
	if !fn(&gen.Order{Price: proto.Int32(200)}) {
		t.Fatal("OR: price=200 should match")
	}
	if fn(&gen.Order{Price: proto.Int32(300)}) {
		t.Fatal("OR: price=300 should not match")
	}
}

// --------------------------------------------------------------------------
// 6. NotPredicate
// --------------------------------------------------------------------------

func TestNotPredicate(t *testing.T) {
	t.Parallel()
	// NOT (price == 100)
	pred := &gen.Predicate{
		NotPredicate: &gen.NotPredicate{
			Child: &gen.Predicate{
				ValuePredicate: &gen.ValuePredicate{
					Value: []string{"price"},
					Comparison: &gen.Comparison{
						SimpleComparison: &gen.SimpleComparison{
							Type:    gen.ComparisonType_EQUALS.Enum(),
							Operand: &gen.Value{IntValue: proto.Int32(100)},
						},
					},
				},
			},
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	if fn(&gen.Order{Price: proto.Int32(100)}) {
		t.Fatal("NOT(price==100): price=100 should not match")
	}
	if !fn(&gen.Order{Price: proto.Int32(200)}) {
		t.Fatal("NOT(price==100): price=200 should match")
	}
}

// --------------------------------------------------------------------------
// 7. Complex nested: AND(price > 100, OR(quantity > 5, NOT(IS_NULL flower.type)))
// --------------------------------------------------------------------------

func TestComplexNestedPredicate(t *testing.T) {
	t.Parallel()
	pred := &gen.Predicate{
		AndPredicate: &gen.AndPredicate{
			Children: []*gen.Predicate{
				// price > 100
				{
					ValuePredicate: &gen.ValuePredicate{
						Value: []string{"price"},
						Comparison: &gen.Comparison{
							SimpleComparison: &gen.SimpleComparison{
								Type:    gen.ComparisonType_GREATER_THAN.Enum(),
								Operand: &gen.Value{IntValue: proto.Int32(100)},
							},
						},
					},
				},
				// OR(quantity > 5, NOT(IS_NULL flower.type))
				{
					OrPredicate: &gen.OrPredicate{
						Children: []*gen.Predicate{
							// quantity > 5
							{
								ValuePredicate: &gen.ValuePredicate{
									Value: []string{"quantity"},
									Comparison: &gen.Comparison{
										SimpleComparison: &gen.SimpleComparison{
											Type:    gen.ComparisonType_GREATER_THAN.Enum(),
											Operand: &gen.Value{IntValue: proto.Int32(5)},
										},
									},
								},
							},
							// NOT(IS_NULL flower.type)
							{
								NotPredicate: &gen.NotPredicate{
									Child: &gen.Predicate{
										ValuePredicate: &gen.ValuePredicate{
											Value: []string{"flower", "type"},
											Comparison: &gen.Comparison{
												SimpleComparison: &gen.SimpleComparison{
													Type: gen.ComparisonType_IS_NULL.Enum(),
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}

	// price=200, quantity=10 -> both AND arms satisfied
	if !fn(&gen.Order{Price: proto.Int32(200), Quantity: proto.Int32(10)}) {
		t.Fatal("price=200, qty=10 should match")
	}
	// price=200, flower.type set -> second OR arm (NOT IS_NULL) satisfied
	if !fn(&gen.Order{Price: proto.Int32(200), Flower: &gen.Flower{Type: proto.String("rose")}}) {
		t.Fatal("price=200, flower.type=rose should match")
	}
	// price=50 -> first AND arm fails regardless of other fields
	if fn(&gen.Order{Price: proto.Int32(50), Quantity: proto.Int32(100)}) {
		t.Fatal("price=50 should not match (first AND arm fails)")
	}
	// price=200, quantity=1, no flower -> OR fails (qty not > 5, flower.type IS_NULL)
	if fn(&gen.Order{Price: proto.Int32(200), Quantity: proto.Int32(1)}) {
		t.Fatal("price=200, qty=1, no flower should not match")
	}
}

// --------------------------------------------------------------------------
// 8. Index.SetPredicateProto
// --------------------------------------------------------------------------

func TestIndexSetPredicateProto(t *testing.T) {
	t.Parallel()
	idx := NewIndex("test_idx", Field("price"))
	pred := &gen.Predicate{
		ConstantPredicate: &gen.ConstantPredicate{
			Value: gen.ConstantPredicate_TRUE.Enum(),
		},
	}
	if err := idx.SetPredicateProto(pred); err != nil {
		t.Fatalf("SetPredicateProto: %v", err)
	}
	if idx.GetPredicateProto() == nil {
		t.Fatal("predicateProto should be stored")
	}
	if !proto.Equal(idx.GetPredicateProto(), pred) {
		t.Fatal("stored predicateProto should match input")
	}
	if idx.Predicate == nil {
		t.Fatal("Predicate function should be set")
	}
	if !idx.Predicate(&gen.Order{Price: proto.Int32(1)}) {
		t.Fatal("TRUE predicate function should return true")
	}
}

func TestIndexSetPredicateProtoNil(t *testing.T) {
	t.Parallel()
	idx := NewIndex("test_idx", Field("price"))
	// First set a predicate, then clear it
	pred := &gen.Predicate{
		ConstantPredicate: &gen.ConstantPredicate{
			Value: gen.ConstantPredicate_TRUE.Enum(),
		},
	}
	if err := idx.SetPredicateProto(pred); err != nil {
		t.Fatalf("SetPredicateProto: %v", err)
	}
	if err := idx.SetPredicateProto(nil); err != nil {
		t.Fatalf("SetPredicateProto(nil): %v", err)
	}
	if idx.GetPredicateProto() != nil {
		t.Fatal("predicateProto should be nil after clearing")
	}
	if idx.Predicate != nil {
		t.Fatal("Predicate function should be nil after clearing")
	}
}

// --------------------------------------------------------------------------
// 9. Proto round-trip via indexToProto/indexFromProto
// --------------------------------------------------------------------------

func TestIndexPredicateProtoRoundTrip(t *testing.T) {
	t.Parallel()
	idx := NewIndex("price_idx", Field("price"))
	pred := &gen.Predicate{
		ValuePredicate: &gen.ValuePredicate{
			Value: []string{"price"},
			Comparison: &gen.Comparison{
				SimpleComparison: &gen.SimpleComparison{
					Type:    gen.ComparisonType_GREATER_THAN.Enum(),
					Operand: &gen.Value{IntValue: proto.Int32(0)},
				},
			},
		},
	}
	if err := idx.SetPredicateProto(pred); err != nil {
		t.Fatalf("SetPredicateProto: %v", err)
	}

	// Serialize
	p, err := indexToProto(idx)
	if err != nil {
		t.Fatalf("indexToProto: %v", err)
	}
	if p.Predicate == nil {
		t.Fatal("serialized proto should have Predicate field")
	}
	if !proto.Equal(p.Predicate, pred) {
		t.Fatal("serialized predicate should match original")
	}

	// Deserialize
	idx2, err := indexFromProto(p)
	if err != nil {
		t.Fatalf("indexFromProto: %v", err)
	}
	if idx2.GetPredicateProto() == nil {
		t.Fatal("deserialized index should have predicateProto")
	}
	if !proto.Equal(idx2.GetPredicateProto(), pred) {
		t.Fatal("deserialized predicateProto should match original")
	}
	if idx2.Predicate == nil {
		t.Fatal("deserialized index should have Predicate function")
	}

	// Verify the evaluator works
	if !idx2.Predicate(&gen.Order{Price: proto.Int32(100)}) {
		t.Fatal("deserialized predicate should match price=100 (> 0)")
	}
	if idx2.Predicate(&gen.Order{Price: proto.Int32(-5)}) {
		t.Fatal("deserialized predicate should not match price=-5 (not > 0)")
	}
}

// --------------------------------------------------------------------------
// 10. Nil predicate round-trip
// --------------------------------------------------------------------------

func TestIndexNilPredicateRoundTrip(t *testing.T) {
	t.Parallel()
	idx := NewIndex("plain_idx", Field("price"))
	// No predicate set

	p, err := indexToProto(idx)
	if err != nil {
		t.Fatalf("indexToProto: %v", err)
	}
	if p.Predicate != nil {
		t.Fatal("serialized proto should NOT have Predicate field when none set")
	}

	idx2, err := indexFromProto(p)
	if err != nil {
		t.Fatalf("indexFromProto: %v", err)
	}
	if idx2.GetPredicateProto() != nil {
		t.Fatal("deserialized index should not have predicateProto")
	}
	if idx2.Predicate != nil {
		t.Fatal("deserialized index should not have Predicate function")
	}
}

// --------------------------------------------------------------------------
// 11. resolveFieldPath edge cases
// --------------------------------------------------------------------------

func TestResolveFieldPathNilMessage(t *testing.T) {
	t.Parallel()
	val, has := resolveFieldPath(nil, []string{"price"})
	if has {
		t.Fatal("nil message should return has=false")
	}
	if val != nil {
		t.Fatal("nil message should return val=nil")
	}
}

func TestResolveFieldPathUnknownField(t *testing.T) {
	t.Parallel()
	val, has := resolveFieldPath(&gen.Order{Price: proto.Int32(100)}, []string{"nonexistent"})
	if has {
		t.Fatal("unknown field should return has=false")
	}
	if val != nil {
		t.Fatal("unknown field should return val=nil")
	}
}

func TestResolveFieldPathNonMessageIntermediate(t *testing.T) {
	t.Parallel()
	// Try to navigate through price (an int32) as if it were a message
	val, has := resolveFieldPath(&gen.Order{Price: proto.Int32(100)}, []string{"price", "sub"})
	if has {
		t.Fatal("non-message intermediate should return has=false")
	}
	if val != nil {
		t.Fatal("non-message intermediate should return val=nil")
	}
}

func TestResolveFieldPathUnsetIntermediate(t *testing.T) {
	t.Parallel()
	// flower is nil, try to navigate flower.type
	val, has := resolveFieldPath(&gen.Order{}, []string{"flower", "type"})
	if has {
		t.Fatal("unset intermediate message should return has=false")
	}
	if val != nil {
		t.Fatal("unset intermediate message should return val=nil")
	}
}

func TestResolveFieldPathUnknownIntermediateField(t *testing.T) {
	t.Parallel()
	// "bogus" doesn't exist as a field on Order
	val, has := resolveFieldPath(&gen.Order{}, []string{"bogus", "type"})
	if has {
		t.Fatal("unknown intermediate field should return has=false")
	}
	if val != nil {
		t.Fatal("unknown intermediate field should return val=nil")
	}
}

// --------------------------------------------------------------------------
// 12. compareValues edge cases
// --------------------------------------------------------------------------

func TestCompareValuesNilVsNil(t *testing.T) {
	t.Parallel()
	if compareValues(nil, nil) != 0 {
		t.Fatal("nil vs nil should be 0")
	}
}

func TestCompareValuesNilVsValue(t *testing.T) {
	t.Parallel()
	if compareValues(nil, int64(1)) >= 0 {
		t.Fatal("nil vs value should be < 0")
	}
	if compareValues(int64(1), nil) <= 0 {
		t.Fatal("value vs nil should be > 0")
	}
}

func TestCompareValuesInt64VsFloat64(t *testing.T) {
	t.Parallel()
	// int64(100) vs float64(100.0) should be equal
	if compareValues(int64(100), float64(100.0)) != 0 {
		t.Fatal("int64(100) vs float64(100.0) should be 0")
	}
	// float64(99.5) vs int64(100) should be < 0
	if compareValues(float64(99.5), int64(100)) >= 0 {
		t.Fatal("float64(99.5) vs int64(100) should be < 0")
	}
	// int64(101) vs float64(100.5) should be > 0
	if compareValues(int64(101), float64(100.5)) <= 0 {
		t.Fatal("int64(101) vs float64(100.5) should be > 0")
	}
}

func TestCompareValuesStrings(t *testing.T) {
	t.Parallel()
	if compareValues("abc", "abc") != 0 {
		t.Fatal("equal strings should be 0")
	}
	if compareValues("abc", "abd") >= 0 {
		t.Fatal("abc < abd")
	}
	if compareValues("abd", "abc") <= 0 {
		t.Fatal("abd > abc")
	}
}

func TestCompareValuesBool(t *testing.T) {
	t.Parallel()
	if compareValues(true, true) != 0 {
		t.Fatal("true vs true should be 0")
	}
	if compareValues(false, false) != 0 {
		t.Fatal("false vs false should be 0")
	}
	if compareValues(false, true) >= 0 {
		t.Fatal("false < true")
	}
	if compareValues(true, false) <= 0 {
		t.Fatal("true > false")
	}
}

func TestCompareValuesBytes(t *testing.T) {
	t.Parallel()
	if compareValues([]byte{1, 2, 3}, []byte{1, 2, 3}) != 0 {
		t.Fatal("equal bytes should be 0")
	}
	if compareValues([]byte{1, 2}, []byte{1, 3}) >= 0 {
		t.Fatal("[1,2] < [1,3]")
	}
}

func TestCompareValuesCrossTypeReturnsZero(t *testing.T) {
	t.Parallel()
	// Comparing incompatible types (string vs int64) returns 0
	if compareValues("abc", int64(1)) != 0 {
		t.Fatal("incompatible types should return 0")
	}
}

func TestCompareValuesInt32Normalization(t *testing.T) {
	t.Parallel()
	// int32 should normalize to int64 for comparison
	if compareValues(int32(42), int64(42)) != 0 {
		t.Fatal("int32(42) vs int64(42) should be equal after normalization")
	}
	if compareValues(int32(10), int64(20)) >= 0 {
		t.Fatal("int32(10) vs int64(20) should be < 0")
	}
}

func TestCompareValuesFloat32Normalization(t *testing.T) {
	t.Parallel()
	// float32 should normalize to float64
	if compareValues(float32(1.5), float64(1.5)) != 0 {
		t.Fatal("float32(1.5) vs float64(1.5) should be equal after normalization")
	}
}

// --------------------------------------------------------------------------
// 13. extractValueOperand
// --------------------------------------------------------------------------

func TestExtractValueOperandNil(t *testing.T) {
	t.Parallel()
	if extractValueOperand(nil) != nil {
		t.Fatal("nil Value should return nil")
	}
}

func TestExtractValueOperandLong(t *testing.T) {
	t.Parallel()
	v := extractValueOperand(&gen.Value{LongValue: proto.Int64(42)})
	if v != int64(42) {
		t.Fatalf("LongValue: got %v (%T), want int64(42)", v, v)
	}
}

func TestExtractValueOperandInt(t *testing.T) {
	t.Parallel()
	v := extractValueOperand(&gen.Value{IntValue: proto.Int32(7)})
	// IntValue is promoted to int64
	if v != int64(7) {
		t.Fatalf("IntValue: got %v (%T), want int64(7)", v, v)
	}
}

func TestExtractValueOperandDouble(t *testing.T) {
	t.Parallel()
	v := extractValueOperand(&gen.Value{DoubleValue: proto.Float64(3.14)})
	if v != float64(3.14) {
		t.Fatalf("DoubleValue: got %v (%T), want float64(3.14)", v, v)
	}
}

func TestExtractValueOperandFloat(t *testing.T) {
	t.Parallel()
	v := extractValueOperand(&gen.Value{FloatValue: proto.Float32(2.5)})
	// FloatValue is promoted to float64
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("FloatValue: got type %T, want float64", v)
	}
	if f != float64(float32(2.5)) {
		t.Fatalf("FloatValue: got %v, want %v", f, float64(float32(2.5)))
	}
}

func TestExtractValueOperandBool(t *testing.T) {
	t.Parallel()
	v := extractValueOperand(&gen.Value{BoolValue: proto.Bool(true)})
	if v != true {
		t.Fatalf("BoolValue: got %v (%T), want true", v, v)
	}
	v = extractValueOperand(&gen.Value{BoolValue: proto.Bool(false)})
	if v != false {
		t.Fatalf("BoolValue false: got %v (%T), want false", v, v)
	}
}

func TestExtractValueOperandString(t *testing.T) {
	t.Parallel()
	v := extractValueOperand(&gen.Value{StringValue: proto.String("hello")})
	if v != "hello" {
		t.Fatalf("StringValue: got %v (%T), want 'hello'", v, v)
	}
}

func TestExtractValueOperandBytes(t *testing.T) {
	t.Parallel()
	data := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	v := extractValueOperand(&gen.Value{BytesValue: data})
	bs, ok := v.([]byte)
	if !ok {
		t.Fatalf("BytesValue: got type %T, want []byte", v)
	}
	if len(bs) != 4 || bs[0] != 0xDE || bs[3] != 0xEF {
		t.Fatalf("BytesValue: got %v, want %v", bs, data)
	}
}

func TestExtractValueOperandEmpty(t *testing.T) {
	t.Parallel()
	// A Value with no fields set returns nil
	v := extractValueOperand(&gen.Value{})
	if v != nil {
		t.Fatalf("empty Value: got %v (%T), want nil", v, v)
	}
}

// --------------------------------------------------------------------------
// Additional: NullComparison via Comparison proto
// --------------------------------------------------------------------------

func TestNullComparisonIsNull(t *testing.T) {
	t.Parallel()
	pred := &gen.Predicate{
		ValuePredicate: &gen.ValuePredicate{
			Value: []string{"price"},
			Comparison: &gen.Comparison{
				NullComparison: &gen.NullComparison{
					IsNull: proto.Bool(true),
				},
			},
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	// price unset
	if !fn(&gen.Order{OrderId: proto.Int64(1)}) {
		t.Fatal("NullComparison isNull=true should match when field is unset")
	}
	// price set
	if fn(&gen.Order{Price: proto.Int32(100)}) {
		t.Fatal("NullComparison isNull=true should not match when field is set")
	}
}

func TestNullComparisonNotNull(t *testing.T) {
	t.Parallel()
	pred := &gen.Predicate{
		ValuePredicate: &gen.ValuePredicate{
			Value: []string{"price"},
			Comparison: &gen.Comparison{
				NullComparison: &gen.NullComparison{
					IsNull: proto.Bool(false),
				},
			},
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	// price set
	if !fn(&gen.Order{Price: proto.Int32(100)}) {
		t.Fatal("NullComparison isNull=false should match when field is set")
	}
	// price unset
	if fn(&gen.Order{OrderId: proto.Int64(1)}) {
		t.Fatal("NullComparison isNull=false should not match when field is unset")
	}
}

// --------------------------------------------------------------------------
// Additional: error paths
// --------------------------------------------------------------------------

func TestPredicateFromProtoNilReturnsNil(t *testing.T) {
	t.Parallel()
	fn, err := predicateFromProto(nil)
	if err != nil {
		t.Fatalf("nil proto should not error: %v", err)
	}
	if fn != nil {
		t.Fatal("nil proto should return nil function")
	}
}

func TestPredicateFromProtoEmptyErrors(t *testing.T) {
	t.Parallel()
	_, err := predicateFromProto(&gen.Predicate{})
	if err == nil {
		t.Fatal("empty predicate message should return error")
	}
}

func TestValuePredicateEmptyFieldPathErrors(t *testing.T) {
	t.Parallel()
	_, err := predicateFromProto(&gen.Predicate{
		ValuePredicate: &gen.ValuePredicate{
			Value: []string{},
			Comparison: &gen.Comparison{
				SimpleComparison: &gen.SimpleComparison{
					Type: gen.ComparisonType_EQUALS.Enum(),
				},
			},
		},
	})
	if err == nil {
		t.Fatal("empty field path should return error")
	}
}

func TestValuePredicateNoComparisonErrors(t *testing.T) {
	t.Parallel()
	_, err := predicateFromProto(&gen.Predicate{
		ValuePredicate: &gen.ValuePredicate{
			Value: []string{"price"},
		},
	})
	if err == nil {
		t.Fatal("missing comparison should return error")
	}
}

func TestComparisonNoTypeErrors(t *testing.T) {
	t.Parallel()
	_, err := predicateFromProto(&gen.Predicate{
		ValuePredicate: &gen.ValuePredicate{
			Value:      []string{"price"},
			Comparison: &gen.Comparison{
				// neither simple nor null
			},
		},
	})
	if err == nil {
		t.Fatal("comparison with neither simple nor null should error")
	}
}

// --------------------------------------------------------------------------
// Additional: predicate on unset field returns false for value comparisons
// --------------------------------------------------------------------------

func TestValuePredicateUnsetFieldReturnsFalse(t *testing.T) {
	t.Parallel()
	pred := &gen.Predicate{
		ValuePredicate: &gen.ValuePredicate{
			Value: []string{"price"},
			Comparison: &gen.Comparison{
				SimpleComparison: &gen.SimpleComparison{
					Type:    gen.ComparisonType_EQUALS.Enum(),
					Operand: &gen.Value{IntValue: proto.Int32(100)},
				},
			},
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	// price not set
	if fn(&gen.Order{OrderId: proto.Int64(1)}) {
		t.Fatal("EQUALS on unset field should return false")
	}
}

// --------------------------------------------------------------------------
// Additional: LongValue operand with int64 comparison
// --------------------------------------------------------------------------

func TestValuePredicateLongValueOperand(t *testing.T) {
	t.Parallel()
	// Use LongValue (int64) to compare with order_id (int64 field)
	pred := &gen.Predicate{
		ValuePredicate: &gen.ValuePredicate{
			Value: []string{"order_id"},
			Comparison: &gen.Comparison{
				SimpleComparison: &gen.SimpleComparison{
					Type:    gen.ComparisonType_EQUALS.Enum(),
					Operand: &gen.Value{LongValue: proto.Int64(42)},
				},
			},
		},
	}
	fn, err := predicateFromProto(pred)
	if err != nil {
		t.Fatalf("predicateFromProto: %v", err)
	}
	if !fn(&gen.Order{OrderId: proto.Int64(42)}) {
		t.Fatal("LongValue operand should match order_id=42")
	}
	if fn(&gen.Order{OrderId: proto.Int64(99)}) {
		t.Fatal("LongValue operand should not match order_id=99")
	}
}
