package recordlayer

import (
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// newOrder constructs an Order with sensible defaults for key expression tests.
// Order has: order_id (int64), flower (Flower msg), price (int32), tags (repeated string), quantity (int32)
func newOrder(id int64, price int32, tags ...string) *gen.Order {
	return &gen.Order{
		OrderId: proto.Int64(id),
		Price:   proto.Int32(price),
		Tags:    tags,
	}
}

// newOrderWithFlower constructs an Order that has a Flower sub-message.
func newOrderWithFlower(id int64, flowerType string, color gen.Color) *gen.Order {
	return &gen.Order{
		OrderId: proto.Int64(id),
		Flower: &gen.Flower{
			Type:  proto.String(flowerType),
			Color: color.Enum(),
		},
	}
}

// asStored wraps a proto.Message into a minimal FDBStoredRecord for evaluation.
func asStored(msg proto.Message) *FDBStoredRecord[proto.Message] {
	return &FDBStoredRecord[proto.Message]{Record: msg}
}

var _ = Describe("KeyExpression unit tests", func() {
	// ---------------------------------------------------------------------------
	// FieldKeyExpression
	// ---------------------------------------------------------------------------

	Describe("FieldKeyExpression", func() {
		Describe("Evaluate scalar fields", func() {
			It("extracts int64 order_id", func() {
				expr := Field("order_id")
				order := newOrder(42, 100)
				result, err := expr.Evaluate(asStored(order), order)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal([][]any{{int64(42)}}))
			})

			It("extracts int32 price as int64", func() {
				expr := Field("price")
				order := newOrder(1, 999)
				result, err := expr.Evaluate(asStored(order), order)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal([][]any{{int64(999)}}))
			})

			It("extracts string field from Customer", func() {
				expr := Field("name")
				c := &gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Alice")}
				result, err := expr.Evaluate(asStored(c), c)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal([][]any{{"Alice"}}))
			})

			It("returns nil for unset proto2 optional scalar", func() {
				expr := Field("price")
				order := &gen.Order{OrderId: proto.Int64(1)} // price not set
				result, err := expr.Evaluate(asStored(order), order)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal([][]any{{nil}}))
			})

			It("returns nil for unset optional string field", func() {
				expr := Field("name")
				c := &gen.Customer{CustomerId: proto.Int64(1)} // name not set
				result, err := expr.Evaluate(asStored(c), c)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal([][]any{{nil}}))
			})
		})

		Describe("Evaluate with nil message", func() {
			It("FanTypeNone on nil message returns [[nil]]", func() {
				expr := Field("order_id") // FanTypeNone
				result, err := expr.Evaluate(nil, nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal([][]any{{nil}}))
			})

			It("FanTypeFanOut on nil message returns nil (no entries)", func() {
				expr := FanOut("tags")
				result, err := expr.Evaluate(nil, nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeNil())
			})

			It("FanTypeConcatenate on nil message returns [[null]] (tuple null, Java default)", func() {
				expr := &FieldKeyExpression{fieldName: "tags", fanType: FanTypeConcatenate}
				result, err := expr.Evaluate(nil, nil)
				Expect(err).NotTo(HaveOccurred())
				// ABSENT field / nil message -> Java getNullResult() default
				// NullStandin.NULL -> scalar(nullStandin) -> tuple null (0x00),
				// NOT an empty nested tuple (0x05 0x00). The empty-nested-tuple
				// form is the present-but-empty repeated case below (evaluateRepeated).
				Expect(result).To(Equal([][]any{{nil}}))
			})
		})

		Describe("Evaluate repeated fields", func() {
			It("FanOut returns one tuple per tag", func() {
				expr := FanOut("tags")
				order := newOrder(1, 10, "rose", "lily", "tulip")
				result, err := expr.Evaluate(asStored(order), order)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal([][]any{
					{"rose"},
					{"lily"},
					{"tulip"},
				}))
			})

			It("FanOut on empty repeated returns nil", func() {
				expr := FanOut("tags")
				order := newOrder(1, 10) // no tags
				result, err := expr.Evaluate(asStored(order), order)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeNil())
			})

			It("Concatenate packs all values into one nested-tuple element", func() {
				expr := &FieldKeyExpression{fieldName: "tags", fanType: FanTypeConcatenate}
				order := newOrder(1, 10, "a", "b", "c")
				result, err := expr.Evaluate(asStored(order), order)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(HaveLen(1))
				// A nested tuple.Tuple (Java's Tuple.addObject(List)), packable — not a
				// bare []any, which the FDB tuple packer panics on.
				packed, ok := result[0][0].(tuple.Tuple)
				Expect(ok).To(BeTrue())
				Expect(packed).To(Equal(tuple.Tuple{"a", "b", "c"}))
			})

			It("Concatenate on empty repeated returns [[empty nested tuple]]", func() {
				expr := &FieldKeyExpression{fieldName: "tags", fanType: FanTypeConcatenate}
				order := newOrder(1, 10) // no tags
				result, err := expr.Evaluate(asStored(order), order)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal([][]any{{tuple.Tuple{}}}))
			})

			It("FanTypeNone on repeated field returns KeyExpressionError", func() {
				expr := Field("tags") // FanTypeNone
				order := newOrder(1, 10, "x")
				_, err := expr.Evaluate(asStored(order), order)
				Expect(err).To(HaveOccurred())
				var ke *KeyExpressionError
				Expect(errors.As(err, &ke)).To(BeTrue())
			})
		})

		Describe("Evaluate missing field", func() {
			It("returns KeyExpressionError for non-existent field name", func() {
				expr := Field("does_not_exist")
				order := newOrder(1, 10)
				_, err := expr.Evaluate(asStored(order), order)
				Expect(err).To(HaveOccurred())
				var ke *KeyExpressionError
				Expect(errors.As(err, &ke)).To(BeTrue())
				Expect(ke.Message).To(ContainSubstring("does_not_exist"))
			})
		})

		Describe("FieldNames and ColumnSize", func() {
			It("FieldNames returns the field name", func() {
				Expect(Field("order_id").FieldNames()).To(Equal([]string{"order_id"}))
			})

			It("FanOut FieldNames returns the field name", func() {
				Expect(FanOut("tags").FieldNames()).To(Equal([]string{"tags"}))
			})

			It("ColumnSize is always 1", func() {
				Expect(Field("order_id").ColumnSize()).To(Equal(1))
				Expect(FanOut("tags").ColumnSize()).To(Equal(1))
				Expect((&FieldKeyExpression{fieldName: "tags", fanType: FanTypeConcatenate}).ColumnSize()).To(Equal(1))
			})
		})
	})

	// ---------------------------------------------------------------------------
	// RecordTypeKeyExpression
	// ---------------------------------------------------------------------------

	Describe("RecordTypeKeyExpression", func() {
		It("unbound returns type name string when typeKeys not set", func() {
			expr := RecordTypeKey()
			order := newOrder(1, 10)
			result, err := expr.Evaluate(asStored(order), order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{"Order"}}))
		})

		It("bound returns int64 type key when typeKeys populated", func() {
			expr := RecordTypeKey()
			expr.bindTypeKeys(map[string]int64{"Order": 5})
			order := newOrder(1, 10)
			result, err := expr.Evaluate(asStored(order), order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{int64(5)}}))
		})

		It("bound falls back to name when record type not in map", func() {
			expr := RecordTypeKey()
			expr.bindTypeKeys(map[string]int64{"Customer": 3})
			order := newOrder(1, 10)
			result, err := expr.Evaluate(asStored(order), order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{"Order"}}))
		})

		It("nil message returns [[nil]]", func() {
			expr := RecordTypeKey()
			result, err := expr.Evaluate(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{nil}}))
		})

		It("with nested expression prepends type key to nested values", func() {
			expr := RecordTypeKey().Nest(Field("order_id"))
			expr.(*RecordTypeKeyExpression).bindTypeKeys(map[string]int64{"Order": 7})
			order := newOrder(99, 10)
			result, err := expr.Evaluate(asStored(order), order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{int64(7), int64(99)}}))
		})

		It("FieldNames without nested returns empty slice", func() {
			expr := RecordTypeKey()
			Expect(expr.FieldNames()).To(BeEmpty())
		})

		It("FieldNames with nested delegates to nested", func() {
			expr := RecordTypeKey().Nest(Field("order_id"))
			Expect(expr.FieldNames()).To(Equal([]string{"order_id"}))
		})

		It("ColumnSize without nested is 1", func() {
			Expect(RecordTypeKey().ColumnSize()).To(Equal(1))
		})

		It("ColumnSize with nested is 1 + nested size", func() {
			expr := RecordTypeKey().Nest(Concat(Field("order_id"), Field("price")))
			Expect(expr.ColumnSize()).To(Equal(3))
		})
	})

	// ---------------------------------------------------------------------------
	// EmptyKeyExpression
	// ---------------------------------------------------------------------------

	Describe("EmptyKeyExpression", func() {
		It("Evaluate returns one empty tuple", func() {
			expr := EmptyKey()
			result, err := expr.Evaluate(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{}}))
		})

		It("Evaluate ignores message content", func() {
			expr := EmptyKey()
			order := newOrder(1, 10)
			result, err := expr.Evaluate(asStored(order), order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{}}))
		})

		It("FieldNames returns nil", func() {
			Expect(EmptyKey().FieldNames()).To(BeNil())
		})

		It("ColumnSize returns 0", func() {
			Expect(EmptyKey().ColumnSize()).To(Equal(0))
		})
	})

	// ---------------------------------------------------------------------------
	// CompositeKeyExpression (Concat)
	// ---------------------------------------------------------------------------

	Describe("CompositeKeyExpression", func() {
		It("single child passes through its result", func() {
			expr := Concat(Field("order_id"))
			order := newOrder(5, 10)
			result, err := expr.Evaluate(asStored(order), order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{int64(5)}}))
		})

		It("multiple scalar children concatenate into one tuple", func() {
			expr := Concat(Field("order_id"), Field("price"))
			order := newOrder(7, 200)
			result, err := expr.Evaluate(asStored(order), order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{int64(7), int64(200)}}))
		})

		It("three scalar children concatenate correctly", func() {
			expr := Concat(Field("order_id"), Field("price"), Field("quantity"))
			order := &gen.Order{
				OrderId:  proto.Int64(3),
				Price:    proto.Int32(50),
				Quantity: proto.Int32(10),
			}
			result, err := expr.Evaluate(asStored(order), order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{int64(3), int64(50), int64(10)}}))
		})

		It("Cartesian product with FanOut child produces cross-product", func() {
			// Field("order_id") → [[7]], FanOut("tags") → [[a],[b]]
			// Cross-product → [[7,a],[7,b]]
			expr := Concat(Field("order_id"), FanOut("tags"))
			order := newOrder(7, 0, "a", "b")
			result, err := expr.Evaluate(asStored(order), order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{
				{int64(7), "a"},
				{int64(7), "b"},
			}))
		})

		It("Cartesian product of two FanOut children", func() {
			// Two fan-out fields that each have 2 values → 4 combinations
			// Use Customer which has no repeated fields — use tags twice from Order
			expr := Concat(FanOut("tags"), FanOut("tags"))
			order := newOrder(1, 0, "x", "y")
			result, err := expr.Evaluate(asStored(order), order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(HaveLen(4))
			Expect(result).To(ContainElements(
				[]any{"x", "x"},
				[]any{"x", "y"},
				[]any{"y", "x"},
				[]any{"y", "y"},
			))
		})

		It("FanOut child with empty repeated produces no rows", func() {
			expr := Concat(Field("order_id"), FanOut("tags"))
			order := newOrder(1, 0) // no tags
			result, err := expr.Evaluate(asStored(order), order)
			Expect(err).NotTo(HaveOccurred())
			// empty FanOut → cross with empty → 0 rows
			Expect(result).To(BeEmpty())
		})

		It("FieldNames aggregates all child field names", func() {
			expr := Concat(Field("order_id"), Field("price"), FanOut("tags"))
			Expect(expr.FieldNames()).To(Equal([]string{"order_id", "price", "tags"}))
		})

		It("ColumnSize is sum of all children", func() {
			expr := Concat(Field("order_id"), Field("price"), FanOut("tags"))
			Expect(expr.ColumnSize()).To(Equal(3))
		})

		It("ColumnSize with nested child counts nested columns", func() {
			// Nest("flower", Field("type")) contributes 1 column
			expr := Concat(Field("order_id"), Nest("flower", Field("type")))
			Expect(expr.ColumnSize()).To(Equal(2))
		})
	})

	// ---------------------------------------------------------------------------
	// NestingKeyExpression
	// ---------------------------------------------------------------------------

	Describe("NestingKeyExpression", func() {
		It("Nest evaluates child on the sub-message", func() {
			expr := Nest("flower", Field("type"))
			order := newOrderWithFlower(1, "rose", gen.Color_RED)
			result, err := expr.Evaluate(asStored(order), order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{"rose"}}))
		})

		It("Nest evaluates enum field in sub-message as int64", func() {
			expr := Nest("flower", Field("color"))
			order := newOrderWithFlower(1, "rose", gen.Color_BLUE) // BLUE=2
			result, err := expr.Evaluate(asStored(order), order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{int64(2)}}))
		})

		It("Nest on unset message field delegates child to nil → returns nil for scalar child", func() {
			expr := Nest("flower", Field("type"))
			order := &gen.Order{OrderId: proto.Int64(1)} // flower not set
			result, err := expr.Evaluate(asStored(order), order)
			Expect(err).NotTo(HaveOccurred())
			// Child Field("type") on nil message → [[nil]]
			Expect(result).To(Equal([][]any{{nil}}))
		})

		It("Nest on nil message with non-FanOut evaluates child on nil", func() {
			expr := Nest("flower", Field("type"))
			result, err := expr.Evaluate(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			// non-FanOut → child evaluated on nil msg → [[nil]]
			Expect(result).To(Equal([][]any{{nil}}))
		})

		It("NestFanOut on nil message returns nil (no entries)", func() {
			// NestFanOut parent on nil message → empty (no repeated elements)
			expr := NestFanOut("flower", Field("type"))
			result, err := expr.Evaluate(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeNil())
		})

		It("returns KeyExpressionError for missing parent field", func() {
			expr := Nest("nonexistent_field", Field("type"))
			order := newOrder(1, 10)
			_, err := expr.Evaluate(asStored(order), order)
			Expect(err).To(HaveOccurred())
			var ke *KeyExpressionError
			Expect(errors.As(err, &ke)).To(BeTrue())
			Expect(ke.Message).To(ContainSubstring("nonexistent_field"))
		})

		It("returns KeyExpressionError when parent field is not a message type", func() {
			expr := Nest("price", Field("something")) // price is int32, not a message
			order := newOrder(1, 10)
			_, err := expr.Evaluate(asStored(order), order)
			Expect(err).To(HaveOccurred())
			var ke *KeyExpressionError
			Expect(errors.As(err, &ke)).To(BeTrue())
			Expect(ke.Message).To(ContainSubstring("not a message type"))
		})

		It("FieldNames includes parent and child field names", func() {
			expr := Nest("flower", Field("type"))
			Expect(expr.FieldNames()).To(Equal([]string{"flower", "type"}))
		})

		It("FieldNames includes parent and child field names for NestFanOut", func() {
			expr := NestFanOut("flower", Concat(Field("type"), Field("color")))
			Expect(expr.FieldNames()).To(Equal([]string{"flower", "type", "color"}))
		})

		It("ColumnSize delegates to child", func() {
			Expect(Nest("flower", Field("type")).ColumnSize()).To(Equal(1))
			Expect(Nest("flower", Concat(Field("type"), Field("color"))).ColumnSize()).To(Equal(2))
		})

		It("Nest with composite child concatenates sub-message fields", func() {
			expr := Nest("flower", Concat(Field("type"), Field("color")))
			order := newOrderWithFlower(1, "lily", gen.Color_YELLOW) // YELLOW=3
			result, err := expr.Evaluate(asStored(order), order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{"lily", int64(3)}}))
		})
	})

	// ---------------------------------------------------------------------------
	// GroupingKeyExpression
	// ---------------------------------------------------------------------------

	Describe("GroupingKeyExpression", func() {
		It("GroupBy produces correct column counts", func() {
			// GroupBy(Field("price"), Field("order_id")) →
			//   wholeKey = Concat(order_id, price), groupedCount = 1
			g := GroupBy(Field("price"), Field("order_id"))
			Expect(g.GetGroupedCount()).To(Equal(1))
			Expect(g.GetGroupingCount()).To(Equal(1))
			Expect(g.ColumnSize()).To(Equal(2))
		})

		It("GroupBy with multiple groupBy expressions", func() {
			// GroupBy(Field("price"), Field("order_id"), Field("quantity"))
			// groupedCount=1, total=3, groupingCount=2
			g := GroupBy(Field("price"), Field("order_id"), Field("quantity"))
			Expect(g.GetGroupedCount()).To(Equal(1))
			Expect(g.GetGroupingCount()).To(Equal(2))
		})

		It("Ungrouped puts all columns in grouped (aggregated)", func() {
			g := Ungrouped(EmptyKey())
			Expect(g.GetGroupedCount()).To(Equal(0)) // EmptyKey ColumnSize=0
			Expect(g.GetGroupingCount()).To(Equal(0))
		})

		It("Ungrouped on Field has groupedCount=1, groupingCount=0", func() {
			g := Ungrouped(Field("price"))
			Expect(g.GetGroupedCount()).To(Equal(1))
			Expect(g.GetGroupingCount()).To(Equal(0))
		})

		It("GroupAll puts all columns in grouping", func() {
			g := GroupAll(Field("price"))
			Expect(g.GetGroupedCount()).To(Equal(0))
			Expect(g.GetGroupingCount()).To(Equal(1))
		})

		It("GroupAll with Concat: all 2 columns are grouping, none grouped", func() {
			g := GroupAll(Concat(Field("order_id"), Field("price")))
			Expect(g.GetGroupedCount()).To(Equal(0))
			Expect(g.GetGroupingCount()).To(Equal(2))
		})

		It("Evaluate delegates to wholeKey", func() {
			g := GroupBy(Field("price"), Field("order_id"))
			order := newOrder(3, 150)
			result, err := g.Evaluate(asStored(order), order)
			Expect(err).NotTo(HaveOccurred())
			// wholeKey = Concat(order_id, price) → [[3, 150]]
			Expect(result).To(Equal([][]any{{int64(3), int64(150)}}))
		})

		It("FieldNames delegates to wholeKey", func() {
			g := GroupBy(Field("price"), Field("order_id"))
			Expect(g.FieldNames()).To(Equal([]string{"order_id", "price"}))
		})

		It("ColumnSize delegates to wholeKey", func() {
			g := GroupBy(Field("price"), Field("order_id"), Field("quantity"))
			Expect(g.ColumnSize()).To(Equal(3))
		})
	})

	// ---------------------------------------------------------------------------
	// LiteralKeyExpression
	// ---------------------------------------------------------------------------

	Describe("LiteralKeyExpression", func() {
		It("nil value evaluates to [[nil]]", func() {
			expr := Literal(nil)
			result, err := expr.Evaluate(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{nil}}))
		})

		It("int64 value is preserved", func() {
			expr := Literal(int64(42))
			result, err := expr.Evaluate(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{int64(42)}}))
		})

		It("string value is preserved", func() {
			expr := Literal("hello")
			result, err := expr.Evaluate(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{"hello"}}))
		})

		It("bool value is preserved", func() {
			expr := Literal(true)
			result, err := expr.Evaluate(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{true}}))
		})

		It("float64 value is preserved", func() {
			expr := Literal(float64(3.14))
			result, err := expr.Evaluate(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{float64(3.14)}}))
		})

		It("[]byte value is preserved", func() {
			b := []byte{0xDE, 0xAD, 0xBE, 0xEF}
			expr := Literal(b)
			result, err := expr.Evaluate(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{b}}))
		})

		It("Evaluate ignores the record and message entirely", func() {
			expr := Literal(int64(99))
			order := newOrder(1, 10)
			result, err := expr.Evaluate(asStored(order), order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{int64(99)}}))
		})

		It("FieldNames returns nil", func() {
			Expect(Literal("x").FieldNames()).To(BeNil())
		})

		It("ColumnSize returns 1", func() {
			Expect(Literal(42).ColumnSize()).To(Equal(1))
		})

		It("GetValue returns the stored value", func() {
			expr := Literal("test")
			Expect(expr.GetValue()).To(Equal("test"))
		})

		It("GetValue returns nil for nil literal", func() {
			expr := Literal(nil)
			Expect(expr.GetValue()).To(BeNil())
		})
	})

	// ---------------------------------------------------------------------------
	// KeyWithValueExpression
	// ---------------------------------------------------------------------------

	Describe("KeyWithValueExpression", func() {
		It("Evaluate delegates to inner key expression", func() {
			inner := Concat(Field("order_id"), Field("price"))
			expr := KeyWithValue(inner, 1)
			order := newOrder(5, 300)
			result, err := expr.Evaluate(asStored(order), order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{int64(5), int64(300)}}))
		})

		It("ColumnSize returns the splitPoint", func() {
			inner := Concat(Field("order_id"), Field("price"), Field("quantity"))
			expr := KeyWithValue(inner, 2)
			Expect(expr.ColumnSize()).To(Equal(2))
		})

		It("InnerKey returns the wrapped expression", func() {
			inner := Field("order_id")
			expr := KeyWithValue(inner, 1)
			Expect(expr.InnerKey()).To(Equal(inner))
		})

		It("SplitPoint returns the split point", func() {
			expr := KeyWithValue(Field("order_id"), 1)
			Expect(expr.SplitPoint()).To(Equal(1))
		})

		It("SplitEvaluatedKey splits at splitPoint", func() {
			expr := KeyWithValue(Concat(Field("order_id"), Field("price"), Field("quantity")), 2)
			full := []any{int64(1), int64(2), int64(3)}
			keyPart, valuePart := expr.SplitEvaluatedKey(full)
			Expect(keyPart).To(Equal([]any{int64(1), int64(2)}))
			Expect(valuePart).To(Equal([]any{int64(3)}))
		})

		It("SplitEvaluatedKey with splitPoint >= len returns all in key, nil in value", func() {
			expr := KeyWithValue(Concat(Field("order_id"), Field("price")), 5)
			full := []any{int64(1), int64(2)}
			keyPart, valuePart := expr.SplitEvaluatedKey(full)
			Expect(keyPart).To(Equal(full))
			Expect(valuePart).To(BeNil())
		})

		It("SplitEvaluatedKey with splitPoint=0 moves everything to value", func() {
			expr := KeyWithValue(Field("order_id"), 0)
			full := []any{int64(42)}
			keyPart, valuePart := expr.SplitEvaluatedKey(full)
			Expect(keyPart).To(BeEmpty())
			Expect(valuePart).To(Equal([]any{int64(42)}))
		})

		It("FieldNames delegates to inner", func() {
			inner := Concat(Field("order_id"), Field("price"))
			expr := KeyWithValue(inner, 1)
			Expect(expr.FieldNames()).To(Equal([]string{"order_id", "price"}))
		})
	})

	// ---------------------------------------------------------------------------
	// VersionKeyExpression
	// ---------------------------------------------------------------------------

	Describe("VersionKeyExpression", func() {
		It("nil record returns [[nil]]", func() {
			expr := VersionKey()
			result, err := expr.Evaluate(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{nil}}))
		})

		It("record with nil version returns [[nil]]", func() {
			expr := VersionKey()
			record := &FDBStoredRecord[proto.Message]{Version: nil}
			result, err := expr.Evaluate(record, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{nil}}))
		})

		It("complete version returns Versionstamp", func() {
			expr := VersionKey()
			globalBytes := make([]byte, GlobalVersionBytes)
			globalBytes[0] = 0x01 // non-zero so it passes completeness check
			ver, err := NewCompleteVersion(globalBytes, 42)
			Expect(err).NotTo(HaveOccurred())
			record := &FDBStoredRecord[proto.Message]{Version: ver}

			result, err := expr.Evaluate(record, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(HaveLen(1))
			Expect(result[0]).To(HaveLen(1))

			vs, ok := result[0][0].(tuple.Versionstamp)
			Expect(ok).To(BeTrue())
			Expect(vs.UserVersion).To(Equal(uint16(42)))
		})

		It("FieldNames returns nil", func() {
			Expect(VersionKey().FieldNames()).To(BeNil())
		})

		It("ColumnSize returns 1", func() {
			Expect(VersionKey().ColumnSize()).To(Equal(1))
		})
	})

	// ---------------------------------------------------------------------------
	// FunctionKeyExpression
	// ---------------------------------------------------------------------------

	Describe("FunctionKeyExpression", func() {
		It("unregistered function returns KeyExpressionError", func() {
			expr := FunctionExpr("unknown_fn_xyzzy", EmptyKey())
			order := newOrder(1, 10)
			_, err := expr.Evaluate(asStored(order), order)
			Expect(err).To(HaveOccurred())
			var ke *KeyExpressionError
			Expect(errors.As(err, &ke)).To(BeTrue())
			Expect(ke.Message).To(ContainSubstring("unknown_fn_xyzzy"))
		})

		It("registered function is called with evaluated arguments", func() {
			name := "test_fn_for_unit_test"
			RegisterFunction(name, func(_ *FDBStoredRecord[proto.Message], _ proto.Message, args [][]any) ([][]any, error) {
				// Echo back arg value + 1
				v := args[0][0].(int64)
				return [][]any{{v + 1}}, nil
			})

			expr := FunctionExpr(name, Field("order_id"))
			order := newOrder(10, 50)
			result, err := expr.Evaluate(asStored(order), order)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{int64(11)}}))
		})

		It("FieldNames delegates to arguments expression", func() {
			expr := FunctionExpr("get_versionstamp_incarnation", Field("order_id"))
			Expect(expr.FieldNames()).To(Equal([]string{"order_id"}))
		})

		It("FieldNames with EmptyKey arguments returns nil", func() {
			expr := FunctionExpr("get_versionstamp_incarnation", EmptyKey())
			Expect(expr.FieldNames()).To(BeNil())
		})

		It("ColumnSize returns 1", func() {
			expr := FunctionExpr("get_versionstamp_incarnation", EmptyKey())
			Expect(expr.ColumnSize()).To(Equal(1))
		})

		It("Name returns the function name", func() {
			expr := FunctionExpr("my_func", EmptyKey())
			Expect(expr.Name()).To(Equal("my_func"))
		})

		It("Arguments returns the arguments expression", func() {
			arg := Field("price")
			expr := FunctionExpr("my_func", arg)
			Expect(expr.Arguments()).To(Equal(arg))
		})
	})

	// ---------------------------------------------------------------------------
	// createsDuplicates
	// ---------------------------------------------------------------------------

	Describe("createsDuplicates", func() {
		It("Field with FanTypeNone returns false", func() {
			Expect(createsDuplicates(Field("order_id"))).To(BeFalse())
		})

		It("FanOut returns true", func() {
			Expect(createsDuplicates(FanOut("tags"))).To(BeTrue())
		})

		It("Concatenate field returns false", func() {
			Expect(createsDuplicates(&FieldKeyExpression{fieldName: "tags", fanType: FanTypeConcatenate})).To(BeFalse())
		})

		It("EmptyKey returns false", func() {
			Expect(createsDuplicates(EmptyKey())).To(BeFalse())
		})

		It("Concat with no FanOut child returns false", func() {
			Expect(createsDuplicates(Concat(Field("order_id"), Field("price")))).To(BeFalse())
		})

		It("Concat with FanOut child returns true", func() {
			Expect(createsDuplicates(Concat(Field("order_id"), FanOut("tags")))).To(BeTrue())
		})

		It("Nest with FanTypeNone and non-duplicating child returns false", func() {
			Expect(createsDuplicates(Nest("flower", Field("type")))).To(BeFalse())
		})

		It("NestFanOut returns true", func() {
			Expect(createsDuplicates(NestFanOut("flower", Field("type")))).To(BeTrue())
		})

		It("Nest with duplicating child returns true", func() {
			// Nest with FanTypeNone but child is FanOut
			n := &NestingKeyExpression{parentField: "flower", fanType: FanTypeNone, child: FanOut("tags")}
			Expect(createsDuplicates(n)).To(BeTrue())
		})

		It("RecordTypeKey without nested returns false", func() {
			Expect(createsDuplicates(RecordTypeKey())).To(BeFalse())
		})

		It("RecordTypeKey with non-duplicating nested returns false", func() {
			expr := RecordTypeKey().Nest(Field("order_id"))
			Expect(createsDuplicates(expr)).To(BeFalse())
		})

		It("RecordTypeKey with FanOut nested returns true", func() {
			expr := RecordTypeKey().Nest(FanOut("tags"))
			Expect(createsDuplicates(expr)).To(BeTrue())
		})

		It("VersionKey returns false", func() {
			Expect(createsDuplicates(VersionKey())).To(BeFalse())
		})

		It("FunctionKeyExpression always returns true", func() {
			Expect(createsDuplicates(FunctionExpr("get_versionstamp_incarnation", EmptyKey()))).To(BeTrue())
		})

		It("KeyWithValue delegates to inner key", func() {
			Expect(createsDuplicates(KeyWithValue(Field("order_id"), 1))).To(BeFalse())
			Expect(createsDuplicates(KeyWithValue(FanOut("tags"), 1))).To(BeTrue())
		})
	})

	// ---------------------------------------------------------------------------
	// normalizeKeyForPositions
	// ---------------------------------------------------------------------------

	Describe("normalizeKeyForPositions", func() {
		It("scalar Field returns itself as single-element list", func() {
			f := Field("order_id")
			result := normalizeKeyForPositions(f)
			Expect(result).To(HaveLen(1))
			Expect(result[0]).To(Equal(f))
		})

		It("Concat flattens into atomic components", func() {
			a := Field("order_id")
			b := Field("price")
			expr := Concat(a, b)
			result := normalizeKeyForPositions(expr)
			Expect(result).To(HaveLen(2))
			Expect(result[0]).To(Equal(a))
			Expect(result[1]).To(Equal(b))
		})

		It("nested Concat is flattened recursively", func() {
			a := Field("order_id")
			b := Field("price")
			c := Field("quantity")
			expr := Concat(Concat(a, b), c)
			result := normalizeKeyForPositions(expr)
			Expect(result).To(HaveLen(3))
			Expect(result[0]).To(Equal(a))
			Expect(result[1]).To(Equal(b))
			Expect(result[2]).To(Equal(c))
		})

		It("NestingKeyExpression re-wraps each child component", func() {
			inner := Concat(Field("type"), Field("color"))
			n := Nest("flower", inner)
			result := normalizeKeyForPositions(n)
			Expect(result).To(HaveLen(2))
			// Each element should be a NestingKeyExpression wrapping one child
			n0, ok := result[0].(*NestingKeyExpression)
			Expect(ok).To(BeTrue())
			Expect(n0.parentField).To(Equal("flower"))
			Expect(n0.child).To(Equal(Field("type")))

			n1, ok := result[1].(*NestingKeyExpression)
			Expect(ok).To(BeTrue())
			Expect(n1.parentField).To(Equal("flower"))
			Expect(n1.child).To(Equal(Field("color")))
		})

		It("GroupingKeyExpression delegates to wholeKey", func() {
			g := GroupBy(Field("price"), Field("order_id"))
			result := normalizeKeyForPositions(g)
			// wholeKey = Concat(order_id, price) → 2 components
			Expect(result).To(HaveLen(2))
		})

		It("KeyWithValueExpression delegates to inner key", func() {
			inner := Concat(Field("order_id"), Field("price"))
			expr := KeyWithValue(inner, 1)
			result := normalizeKeyForPositions(expr)
			Expect(result).To(HaveLen(2))
		})

		It("VersionKeyExpression returns itself", func() {
			v := VersionKey()
			result := normalizeKeyForPositions(v)
			Expect(result).To(HaveLen(1))
			Expect(result[0]).To(Equal(v))
		})

		It("FunctionKeyExpression returns itself", func() {
			f := FunctionExpr("get_versionstamp_incarnation", EmptyKey())
			result := normalizeKeyForPositions(f)
			Expect(result).To(HaveLen(1))
			Expect(result[0]).To(Equal(f))
		})
	})

	// ---------------------------------------------------------------------------
	// keyExpressionEquals
	// ---------------------------------------------------------------------------

	Describe("keyExpressionEquals", func() {
		It("same Field expressions are equal", func() {
			Expect(keyExpressionEquals(Field("order_id"), Field("order_id"))).To(BeTrue())
		})

		It("different field names are not equal", func() {
			Expect(keyExpressionEquals(Field("order_id"), Field("price"))).To(BeFalse())
		})

		It("same name, different FanType are not equal", func() {
			Expect(keyExpressionEquals(Field("tags"), FanOut("tags"))).To(BeFalse())
		})

		It("same FanOut expressions are equal", func() {
			Expect(keyExpressionEquals(FanOut("tags"), FanOut("tags"))).To(BeTrue())
		})

		It("different types are not equal", func() {
			Expect(keyExpressionEquals(Field("order_id"), EmptyKey())).To(BeFalse())
		})

		It("two EmptyKey expressions are equal", func() {
			Expect(keyExpressionEquals(EmptyKey(), EmptyKey())).To(BeTrue())
		})

		It("two RecordTypeKey expressions are equal (structural equality)", func() {
			Expect(keyExpressionEquals(RecordTypeKey(), RecordTypeKey())).To(BeTrue())
		})

		It("Concat with same children is equal", func() {
			a := Concat(Field("order_id"), Field("price"))
			b := Concat(Field("order_id"), Field("price"))
			Expect(keyExpressionEquals(a, b)).To(BeTrue())
		})

		It("Concat with different lengths is not equal", func() {
			a := Concat(Field("order_id"), Field("price"))
			b := Concat(Field("order_id"))
			Expect(keyExpressionEquals(a, b)).To(BeFalse())
		})

		It("Concat with different children is not equal", func() {
			a := Concat(Field("order_id"), Field("price"))
			b := Concat(Field("order_id"), Field("quantity"))
			Expect(keyExpressionEquals(a, b)).To(BeFalse())
		})

		It("Nesting expressions with same parent and child are equal", func() {
			a := Nest("flower", Field("type"))
			b := Nest("flower", Field("type"))
			Expect(keyExpressionEquals(a, b)).To(BeTrue())
		})

		It("Nesting expressions with different parent are not equal", func() {
			a := Nest("flower", Field("type"))
			b := Nest("other", Field("type"))
			Expect(keyExpressionEquals(a, b)).To(BeFalse())
		})

		It("Nesting expressions with different FanType are not equal", func() {
			a := Nest("flower", Field("type"))
			b := NestFanOut("flower", Field("type"))
			Expect(keyExpressionEquals(a, b)).To(BeFalse())
		})

		It("VersionKey expressions are equal", func() {
			Expect(keyExpressionEquals(VersionKey(), VersionKey())).To(BeTrue())
		})

		It("VersionKey is not equal to Field", func() {
			Expect(keyExpressionEquals(VersionKey(), Field("order_id"))).To(BeFalse())
		})

		It("same FunctionKeyExpression (name+args) is equal", func() {
			a := FunctionExpr("get_versionstamp_incarnation", EmptyKey())
			b := FunctionExpr("get_versionstamp_incarnation", EmptyKey())
			Expect(keyExpressionEquals(a, b)).To(BeTrue())
		})

		It("FunctionKeyExpression with different names is not equal", func() {
			a := FunctionExpr("fn_a", EmptyKey())
			b := FunctionExpr("fn_b", EmptyKey())
			Expect(keyExpressionEquals(a, b)).To(BeFalse())
		})

		It("KeyWithValue expressions with same inner and splitPoint are equal", func() {
			a := KeyWithValue(Concat(Field("order_id"), Field("price")), 1)
			b := KeyWithValue(Concat(Field("order_id"), Field("price")), 1)
			Expect(keyExpressionEquals(a, b)).To(BeTrue())
		})

		It("KeyWithValue with different splitPoint is not equal", func() {
			inner := Concat(Field("order_id"), Field("price"))
			a := KeyWithValue(inner, 1)
			b := KeyWithValue(inner, 2)
			Expect(keyExpressionEquals(a, b)).To(BeFalse())
		})

		It("GroupingKeyExpression with same wholeKey and groupedCount are equal", func() {
			a := GroupBy(Field("price"), Field("order_id"))
			b := GroupBy(Field("price"), Field("order_id"))
			Expect(keyExpressionEquals(a, b)).To(BeTrue())
		})

		It("GroupingKeyExpression with different groupedCount is not equal", func() {
			a := GroupBy(Field("price"), Field("order_id"))
			b := GroupAll(Concat(Field("order_id"), Field("price")))
			Expect(keyExpressionEquals(a, b)).To(BeFalse())
		})
	})

	// ---------------------------------------------------------------------------
	// keyExpressionsEqualNilSafe
	// ---------------------------------------------------------------------------

	Describe("keyExpressionsEqualNilSafe", func() {
		It("both nil returns true", func() {
			Expect(keyExpressionsEqualNilSafe(nil, nil)).To(BeTrue())
		})

		It("left nil returns false", func() {
			Expect(keyExpressionsEqualNilSafe(nil, Field("x"))).To(BeFalse())
		})

		It("right nil returns false", func() {
			Expect(keyExpressionsEqualNilSafe(Field("x"), nil)).To(BeFalse())
		})

		It("two equal non-nil returns true", func() {
			Expect(keyExpressionsEqualNilSafe(Field("x"), Field("x"))).To(BeTrue())
		})

		It("two different non-nil returns false", func() {
			Expect(keyExpressionsEqualNilSafe(Field("x"), Field("y"))).To(BeFalse())
		})
	})

	// ---------------------------------------------------------------------------
	// buildPrimaryKeyComponentPositions
	// ---------------------------------------------------------------------------

	Describe("buildPrimaryKeyComponentPositions", func() {
		It("returns nil when there is no overlap", func() {
			indexKey := Field("price")
			primaryKey := Field("order_id")
			result := buildPrimaryKeyComponentPositions(indexKey, primaryKey)
			Expect(result).To(BeNil())
		})

		It("returns positions when index key includes PK component", func() {
			// indexKey = Concat(price, order_id), pk = order_id
			// normalized index = [price, order_id]
			// pk component order_id is at position 1
			indexKey := Concat(Field("price"), Field("order_id"))
			primaryKey := Field("order_id")
			result := buildPrimaryKeyComponentPositions(indexKey, primaryKey)
			Expect(result).NotTo(BeNil())
			Expect(result).To(HaveLen(1))
			Expect(result[0]).To(Equal(1))
		})

		It("returns positions for each PK component found in index", func() {
			// indexKey = Concat(name, customer_id, email)
			// primaryKey = Concat(customer_id, name)
			// customer_id at index pos 1, name at index pos 0
			indexKey := Concat(Field("name"), Field("customer_id"), Field("email"))
			primaryKey := Concat(Field("customer_id"), Field("name"))
			result := buildPrimaryKeyComponentPositions(indexKey, primaryKey)
			Expect(result).NotTo(BeNil())
			Expect(result).To(HaveLen(2))
			Expect(result[0]).To(Equal(1)) // customer_id at pos 1
			Expect(result[1]).To(Equal(0)) // name at pos 0
		})

		It("returns positions with -1 for PK components not found in index", func() {
			// pk has two components, only one is in the index
			indexKey := Field("price")
			primaryKey := Concat(Field("price"), Field("order_id"))
			result := buildPrimaryKeyComponentPositions(indexKey, primaryKey)
			Expect(result).NotTo(BeNil())
			Expect(result[0]).To(Equal(0))  // price at pos 0
			Expect(result[1]).To(Equal(-1)) // order_id not in index
		})

		It("exact match returns position 0", func() {
			indexKey := Field("order_id")
			primaryKey := Field("order_id")
			result := buildPrimaryKeyComponentPositions(indexKey, primaryKey)
			Expect(result).NotTo(BeNil())
			Expect(result).To(Equal([]int{0}))
		})
	})
})
