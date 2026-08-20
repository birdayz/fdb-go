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

func TestIndexSetPredicateProtoOwnsPublishedMessage(t *testing.T) {
	t.Parallel()
	idx := NewIndex("test_idx", Field("price"))
	pred := &gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{
		Value: gen.ConstantPredicate_FALSE.Enum(),
	}}
	if err := idx.SetPredicateProto(pred); err != nil {
		t.Fatalf("SetPredicateProto: %v", err)
	}

	// Mutating the caller's input after Set must affect neither representation.
	pred.ConstantPredicate.Value = gen.ConstantPredicate_TRUE.Enum()
	if idx.Predicate(&gen.Order{}) {
		t.Fatal("caller mutation changed compiled FALSE evaluator")
	}
	if got := idx.GetPredicateProto(); got.GetConstantPredicate().GetValue() != gen.ConstantPredicate_FALSE {
		t.Fatalf("caller mutation changed stored proto to %s", got.GetConstantPredicate().GetValue())
	}

	// The getter is also an ownership boundary.
	got := idx.GetPredicateProto()
	got.ConstantPredicate.Value = gen.ConstantPredicate_TRUE.Enum()
	if idx.Predicate(&gen.Order{}) {
		t.Fatal("getter-result mutation changed compiled FALSE evaluator")
	}
	if again := idx.GetPredicateProto(); again.GetConstantPredicate().GetValue() != gen.ConstantPredicate_FALSE {
		t.Fatalf("getter-result mutation changed stored proto to %s", again.GetConstantPredicate().GetValue())
	}
}

func TestIndexPredicateMetadataProtoIsMutationIsolated(t *testing.T) {
	t.Parallel()
	idx := NewIndex("test_idx", Field("price"))
	if err := idx.SetPredicateProto(&gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{
		Value: gen.ConstantPredicate_FALSE.Enum(),
	}}); err != nil {
		t.Fatalf("SetPredicateProto: %v", err)
	}

	wire, err := indexToProto(idx)
	if err != nil {
		t.Fatalf("indexToProto: %v", err)
	}
	wire.Predicate.ConstantPredicate.Value = gen.ConstantPredicate_TRUE.Enum()
	if idx.Predicate(&gen.Order{}) {
		t.Fatal("serialized metadata mutation changed compiled FALSE evaluator")
	}
	again, err := indexToProto(idx)
	if err != nil {
		t.Fatalf("second indexToProto: %v", err)
	}
	if got := again.GetPredicate().GetConstantPredicate().GetValue(); got != gen.ConstantPredicate_FALSE {
		t.Fatalf("serialized metadata mutation changed internal proto to %s", got)
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

func TestIndexPredicateRepresentationsStayAtomic(t *testing.T) {
	t.Parallel()
	idx := NewIndex("test_idx", Field("price"))
	if idx.HasPredicate() {
		t.Fatal("new index unexpectedly reports a predicate")
	}
	valid := &gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{
		Value: gen.ConstantPredicate_FALSE.Enum(),
	}}
	if err := idx.SetPredicateProto(valid); err != nil {
		t.Fatalf("SetPredicateProto(valid): %v", err)
	}
	if !idx.HasPredicate() || idx.Predicate == nil || idx.Predicate(&gen.Order{}) {
		t.Fatal("valid FALSE proto was not published consistently")
	}

	// Compilation failure must leave both previously valid representations
	// intact, rather than publishing a proto that HasPredicate sees while the
	// evaluator still implements some other predicate.
	if err := idx.SetPredicateProto(&gen.Predicate{}); err == nil {
		t.Fatal("empty predicate proto unexpectedly compiled")
	}
	if !proto.Equal(idx.GetPredicateProto(), valid) || idx.Predicate == nil || idx.Predicate(&gen.Order{}) {
		t.Fatal("rejected proto partially mutated the previous predicate")
	}

	idx.SetPredicate(func(proto.Message) bool { return true })
	if !idx.HasPredicate() || idx.GetPredicateProto() != nil || !idx.Predicate(&gen.Order{}) {
		t.Fatal("programmatic predicate did not replace and clear serialized semantics")
	}
	idx.SetPredicate(nil)
	if idx.HasPredicate() || idx.Predicate != nil || idx.GetPredicateProto() != nil {
		t.Fatal("SetPredicate(nil) did not clear both predicate representations")
	}

	// Defensive representation check: metadata loaded by another path may have
	// only the serializable form. Planner admission must still classify it as
	// sparse and fail closed.
	idx.predicateProto = valid
	if !idx.HasPredicate() {
		t.Fatal("proto-only predicate representation was not classified as sparse")
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

// --------------------------------------------------------------------------
// Tautology proof (index completeness)
// --------------------------------------------------------------------------

// TestPredicateProtoTautologyProofFailsClosed pins the completeness proof that
// admits a filtered index to query execution. Every case that is NOT a proved
// tautology must answer false: an index whose predicate can reject a record is
// incomplete, and a scan over it silently omits rows.
//
// The nil / absent-arm cases are the load-bearing ones. ConstantPredicate.value
// is a proto2 field whose declared DEFAULT is TRUE, so gen's nil-safe
// GetConstantPredicate().GetValue() returns TRUE for a predicate that has no
// constant arm at all — the getters fail OPEN. A proof written on those getters
// declares EVERY predicate a tautology, which admits every sparse index.
func TestPredicateProtoTautologyProofFailsClosed(t *testing.T) {
	t.Parallel()

	valueArm := &gen.Predicate{ValuePredicate: &gen.ValuePredicate{
		Value: []string{"price"},
		Comparison: &gen.Comparison{
			SimpleComparison: &gen.SimpleComparison{
				Type:    gen.ComparisonType_GREATER_THAN.Enum(),
				Operand: &gen.Value{IntValue: proto.Int32(100)},
			},
		},
	}}

	for _, tc := range []struct {
		name string
		pred *gen.Predicate
		want bool
	}{
		{"nil predicate", nil, false},
		{"empty predicate message (no arm set)", &gen.Predicate{}, false},
		{
			"constant arm present but value unset — proto2 default is TRUE",
			&gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{}},
			true,
		},
		{
			"constant TRUE",
			&gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{
				Value: gen.ConstantPredicate_TRUE.Enum(),
			}},
			true,
		},
		{
			"constant FALSE",
			&gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{
				Value: gen.ConstantPredicate_FALSE.Enum(),
			}},
			false,
		},
		{
			"constant NULL",
			&gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{
				Value: gen.ConstantPredicate_NULL.Enum(),
			}},
			false,
		},
		{"value comparison", valueArm, false},
		{
			// AndPredicate.and drops tautological conjuncts and returns
			// ConstantPredicate.TRUE when none remain (AndPredicate.java:188-206),
			// so Java never holds an AndPredicate of TRUEs to classify — it holds
			// the constant. Normalizing the same way is what makes
			// `WHERE TRUE AND TRUE` a complete index here too.
			"AND of EXPLICIT tautologies folds to the constant",
			&gen.Predicate{AndPredicate: &gen.AndPredicate{Children: []*gen.Predicate{
				{ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()}},
				{ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()}},
			}}},
			true,
		},
		{
			// A child that carries the constant ARM with no value set folds like
			// any other TRUE, and must: proto2 declares TRUE as that field's
			// default, so constantPredicateFromProto compiles it to an
			// always-true evaluator and the index really does hold every record.
			// The classifier has to agree with the evaluator that built the
			// index, not be stricter than it.
			"AND of value-defaulted constant children folds like explicit TRUEs",
			&gen.Predicate{AndPredicate: &gen.AndPredicate{Children: []*gen.Predicate{
				{ConstantPredicate: &gen.ConstantPredicate{}},
				{ConstantPredicate: &gen.ConstantPredicate{}},
			}}},
			true,
		},
		{
			// THIS is the proto2 trap, and the fold must not re-open it: these
			// children carry NO constant arm at all, and the nil-safe getter
			// chain GetConstantPredicate().GetValue() would still answer TRUE for
			// them. Only a child whose constant arm is PRESENT may be folded out;
			// an armless child is unknowable and keeps the conjunction filtering.
			"AND of children with NO constant arm stays filtering",
			&gen.Predicate{AndPredicate: &gen.AndPredicate{Children: []*gen.Predicate{
				{}, {},
			}}},
			false,
		},
		{
			"AND mixing a real TRUE with an armless child stays filtering",
			&gen.Predicate{AndPredicate: &gen.AndPredicate{Children: []*gen.Predicate{
				{ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()}},
				{},
			}}},
			false,
		},
		{
			// A surviving conjunct keeps the conjunction filtering: TRUE AND x
			// folds to x, which is a real comparison.
			"AND of a tautology and a real comparison folds to the comparison",
			&gen.Predicate{AndPredicate: &gen.AndPredicate{Children: []*gen.Predicate{
				{ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()}},
				valueArm,
			}}},
			false,
		},
		{
			"AND of two real comparisons stays filtering",
			&gen.Predicate{AndPredicate: &gen.AndPredicate{Children: []*gen.Predicate{valueArm, valueArm}}},
			false,
		},
		{
			// Nested: AND(AND(TRUE, TRUE), TRUE) collapses all the way down,
			// because the fold is recursive exactly as construction is.
			"nested AND of tautologies folds recursively",
			&gen.Predicate{AndPredicate: &gen.AndPredicate{Children: []*gen.Predicate{
				{AndPredicate: &gen.AndPredicate{Children: []*gen.Predicate{
					{ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()}},
					{ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()}},
				}}},
				{ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()}},
			}}},
			true,
		},
		{
			// OR IS NOT THE DUAL. OrPredicate.or -> of (OrPredicate.java:417-445)
			// does no tautology folding, and OrPredicate never overrides
			// isTautology, so TRUE OR x is filtering to Java. Folding it would
			// make Go scan an index Java treats as incomplete.
			"OR containing a tautology is NOT a proved tautology",
			&gen.Predicate{OrPredicate: &gen.OrPredicate{Children: []*gen.Predicate{
				{ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()}},
				valueArm,
			}}},
			false,
		},
		{
			// A SINGLETON disjunction is different, and this is Java too: of()
			// collapses a one-element list to that element, which here is the
			// constant itself. The narrow test then answers about a constant,
			// not about an Or.
			"singleton OR collapses to its only child",
			&gen.Predicate{OrPredicate: &gen.OrPredicate{Children: []*gen.Predicate{
				{ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()}},
			}}},
			true,
		},
		{
			"singleton OR of a real comparison collapses and stays filtering",
			&gen.Predicate{OrPredicate: &gen.OrPredicate{Children: []*gen.Predicate{valueArm}}},
			false,
		},
		{
			"empty AND has no surviving conjunct and is vacuously complete",
			&gen.Predicate{AndPredicate: &gen.AndPredicate{}},
			true,
		},
		{
			// An empty disjunction is what Java's of() refuses outright
			// (Verify.verify(!disjuncts.isEmpty())). Unreconstructable, so it is
			// handed back untouched and fails closed.
			"empty OR fails closed",
			&gen.Predicate{OrPredicate: &gen.OrPredicate{}},
			false,
		},
		{
			// The evaluator (predicateFromProto) dispatches AND before the
			// constant arm, so a multi-arm message runs the AND. Reading the
			// constant arm out of turn would prove a tautology about a
			// predicate that is never evaluated.
			"multi-arm message answers for the arm the evaluator runs",
			&gen.Predicate{
				AndPredicate:      &gen.AndPredicate{Children: []*gen.Predicate{valueArm}},
				ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()},
			},
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := predicateProtoIsTautology(tc.pred); got != tc.want {
				t.Fatalf("predicateProtoIsTautology = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHasFilteringPredicateTreatsOpaqueGoPredicateAsFiltering pins the other
// half: an index's completeness can only be proved from the SERIALIZED
// predicate. A programmatic Go closure is opaque — this one is a tautology and
// no one can prove it — so it must count as filtering and keep the index out of
// query execution.
func TestHasFilteringPredicateTreatsOpaqueGoPredicateAsFiltering(t *testing.T) {
	t.Parallel()

	plain := NewIndex("plain", Field("price"))
	if plain.HasFilteringPredicate() {
		t.Fatal("index with no predicate at all reports a filtering predicate")
	}

	opaque := NewIndex("opaque", Field("price")).
		SetPredicate(func(proto.Message) bool { return true })
	if !opaque.HasFilteringPredicate() {
		t.Fatal("an opaque programmatic predicate must count as filtering — " +
			"its tautology is unprovable, so the index cannot be assumed complete")
	}

	serialized := NewIndex("serialized", Field("price"))
	if err := serialized.SetPredicateProto(&gen.Predicate{
		ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()},
	}); err != nil {
		t.Fatalf("SetPredicateProto: %v", err)
	}
	if !serialized.HasPredicate() {
		t.Fatal("a WHERE TRUE index still DECLARES a predicate")
	}
	if serialized.HasFilteringPredicate() {
		t.Fatal("a proved-tautology predicate rejects no record, so the index " +
			"is complete and must not be treated as filtering")
	}

	filtering := NewIndex("filtering", Field("price"))
	if err := filtering.SetPredicateProto(&gen.Predicate{
		ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_FALSE.Enum()},
	}); err != nil {
		t.Fatalf("SetPredicateProto: %v", err)
	}
	if !filtering.HasFilteringPredicate() {
		t.Fatal("WHERE FALSE indexes nothing — maximally filtering")
	}
}

// TestSetPredicateProtoAcceptsRowNumberWindow is the INVERTED form of the
// tripwire that used to stand here.
//
// That tripwire said: predicateFromProto has no row-window arm, so Go cannot
// carry the predicate at all, so the hazard is latent — and "the moment Go
// implements row-window index maintenance, this test fails, and that failure is
// the signal to check that every sparseness gate still treats the resulting
// index as filtering". Go now implements it (slidingWindowIndexMaintainer), so
// this is that check, kept at the site the tripwire named.
//
// Two claims, and both matter:
//   - a WELL-FORMED window declaration is now accepted and carried, so a
//     Java-authored windowed vector index no longer makes the whole metadata
//     unloadable;
//   - accepting it does NOT make the index look complete. The per-record
//     evaluator answers `true` for every record (Java's shouldIndexThisRecord),
//     which is exactly the reading that would be catastrophic if it leaked into
//     the completeness question — the index holds only the top N. So
//     HasFilteringPredicate must still say "filtering".
func TestSetPredicateProtoAcceptsRowNumberWindow(t *testing.T) {
	t.Parallel()

	idx := NewIndex("topn", Field("score"))
	if err := idx.SetPredicateProto(&gen.Predicate{
		RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
			OrderingField: []string{"score"},
			Size:          proto.Int32(100),
			Direction:     gen.RowNumberWindowPredicate_DESC.Enum(),
		},
	}); err != nil {
		t.Fatalf("SetPredicateProto refused a well-formed row-number window: %v", err)
	}
	if !idx.HasPredicate() {
		t.Fatal("an accepted predicate must leave the index predicated")
	}
	if idx.GetPredicateProto().GetRowNumberWindowPredicate() == nil {
		t.Fatal("the stored proto lost the row-window arm")
	}
	if !idx.HasRowNumberWindowPredicate() {
		t.Fatal("HasRowNumberWindowPredicate did not see the arm it decorates on")
	}

	// The evaluator accepts everything — that is Java's shouldIndexThisRecord,
	// and it is why the completeness question must NOT be answered from it.
	if !idx.Predicate(&gen.Order{}) {
		t.Fatal("the compiled row-window evaluator rejected a record; Java's " +
			"RowNumberWindowPredicate.shouldIndexThisRecord is `return true`")
	}

	// ...and this is the gate that keeps that `true` from meaning "complete".
	if !idx.HasFilteringPredicate() {
		t.Fatal("a top-N index was classified as NON-filtering — a scan of it would " +
			"serve the qualifying slice as the whole table (see " +
			"NormalizeIndexPredicateProto's row-window note)")
	}
}

// TestSetPredicateProtoRejectsMalformedRowNumberWindow pins the arms of
// validateRowNumberWindowSpec, each of which describes a window that cannot
// exist. They are checked when the metadata is LOADED rather than at the first
// record save, because a store whose window declaration is unusable should fail
// to open, not fail one write at a time.
//
// The non-positive size is the load-bearing one: with N <= 0 the window is empty
// forever, so the wrapped vector index would be maintained as permanently empty
// while the entry list grew without bound — an index that answers every search
// with nothing and never says why.
func TestSetPredicateProtoRejectsMalformedRowNumberWindow(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		pred *gen.RowNumberWindowPredicate
	}{
		{
			name: "no ordering field",
			pred: &gen.RowNumberWindowPredicate{
				Size:      proto.Int32(100),
				Direction: gen.RowNumberWindowPredicate_ASC.Enum(),
			},
		},
		{
			name: "empty ordering field name",
			pred: &gen.RowNumberWindowPredicate{
				OrderingField: []string{""},
				Size:          proto.Int32(100),
				Direction:     gen.RowNumberWindowPredicate_ASC.Enum(),
			},
		},
		{
			name: "zero size",
			pred: &gen.RowNumberWindowPredicate{
				OrderingField: []string{"score"},
				Size:          proto.Int32(0),
				Direction:     gen.RowNumberWindowPredicate_ASC.Enum(),
			},
		},
		{
			name: "negative size",
			pred: &gen.RowNumberWindowPredicate{
				OrderingField: []string{"score"},
				Size:          proto.Int32(-1),
				Direction:     gen.RowNumberWindowPredicate_ASC.Enum(),
			},
		},
		{
			name: "absent direction",
			pred: &gen.RowNumberWindowPredicate{
				OrderingField: []string{"score"},
				Size:          proto.Int32(100),
			},
		},
		{
			name: "empty partition path",
			pred: &gen.RowNumberWindowPredicate{
				OrderingField:   []string{"score"},
				Size:            proto.Int32(100),
				Direction:       gen.RowNumberWindowPredicate_ASC.Enum(),
				PartitionFields: []*gen.FieldPath{{}},
			},
		},
		{
			name: "empty partition field name",
			pred: &gen.RowNumberWindowPredicate{
				OrderingField:   []string{"score"},
				Size:            proto.Int32(100),
				Direction:       gen.RowNumberWindowPredicate_ASC.Enum(),
				PartitionFields: []*gen.FieldPath{{Field: []string{""}}},
			},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			idx := NewIndex("topn", Field("score"))
			if err := idx.SetPredicateProto(&gen.Predicate{RowNumberWindowPredicate: tc.pred}); err == nil {
				t.Fatal("SetPredicateProto accepted a window declaration that cannot describe a window")
			}
			if idx.HasPredicate() {
				t.Fatal("a rejected predicate must leave the index unpredicated")
			}
		})
	}
}
