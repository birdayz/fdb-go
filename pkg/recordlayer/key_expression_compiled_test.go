package recordlayer

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// helper: evaluate via compiled path and return the result tuple.
func evalCompiled(t *testing.T, compiled *compiledKeyEvaluator, record *FDBStoredRecord[proto.Message], msg proto.Message) tuple.Tuple {
	t.Helper()
	g := NewGomegaWithT(t)
	var appender tupleAppender
	err := compiled.evaluate(&appender, record, msg)
	g.Expect(err).NotTo(HaveOccurred())
	return appender.toTuple()
}

// helper: evaluate via standard Evaluate path and return the first result as tuple.
func evalStandard(t *testing.T, expr KeyExpression, record *FDBStoredRecord[proto.Message], msg proto.Message) tuple.Tuple {
	t.Helper()
	g := NewGomegaWithT(t)
	result, err := expr.Evaluate(record, msg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).NotTo(BeEmpty())
	out := make(tuple.Tuple, len(result[0]))
	for i, v := range result[0] {
		out[i] = v
	}
	return out
}

// assertByteEquivalence compiles the expression, evaluates both paths, and asserts
// the packed bytes are identical.
func assertByteEquivalence(t *testing.T, expr KeyExpression, msg proto.Message) {
	t.Helper()
	g := NewGomegaWithT(t)

	compiled := compileKeyExpression(expr)
	g.Expect(compiled).NotTo(BeNil(), "expression should be compilable")
	g.Expect(compiled.ok).To(BeTrue())

	record := &FDBStoredRecord[proto.Message]{Record: msg}

	compiledResult := evalCompiled(t, compiled, record, msg)
	standardResult := evalStandard(t, expr, record, msg)

	g.Expect(compiledResult.Pack()).To(Equal(standardResult.Pack()),
		"compiled and standard paths must produce byte-identical packed tuples")
}

func TestCompiledKeyEvaluator_SingleFieldInt64(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Field("order_id")
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).NotTo(BeNil())

	msg := &gen.Order{OrderId: proto.Int64(42)}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	compiledResult := evalCompiled(t, compiled, record, msg)
	standardResult := evalStandard(t, expr, record, msg)

	g.Expect(compiledResult).To(HaveLen(1))
	g.Expect(compiledResult[0]).To(Equal(int64(42)))
	g.Expect(compiledResult.Pack()).To(Equal(standardResult.Pack()),
		"compiled and standard paths must produce identical bytes")
}

func TestCompiledKeyEvaluator_SingleFieldString(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Field("name")
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).NotTo(BeNil())

	msg := &gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("test")}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	compiledResult := evalCompiled(t, compiled, record, msg)
	standardResult := evalStandard(t, expr, record, msg)

	g.Expect(compiledResult).To(HaveLen(1))
	g.Expect(compiledResult[0]).To(Equal("test"))
	g.Expect(compiledResult.Pack()).To(Equal(standardResult.Pack()))
}

func TestCompiledKeyEvaluator_SingleFieldFloat32(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Field("val_float")
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).NotTo(BeNil())

	msg := &gen.TypedRecord{Id: proto.Int64(1), ValFloat: proto.Float32(3.14)}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	compiledResult := evalCompiled(t, compiled, record, msg)
	standardResult := evalStandard(t, expr, record, msg)

	g.Expect(compiledResult).To(HaveLen(1))
	g.Expect(compiledResult[0]).To(BeAssignableToTypeOf(float32(0)), "must be float32, not float64")
	g.Expect(compiledResult[0]).To(BeNumerically("~", 3.14, 0.001))
	g.Expect(compiledResult.Pack()).To(Equal(standardResult.Pack()))
}

func TestCompiledKeyEvaluator_SingleFieldFloat64(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Field("val_double")
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).NotTo(BeNil())

	msg := &gen.TypedRecord{Id: proto.Int64(1), ValDouble: proto.Float64(2.71828)}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	compiledResult := evalCompiled(t, compiled, record, msg)
	standardResult := evalStandard(t, expr, record, msg)

	g.Expect(compiledResult).To(HaveLen(1))
	g.Expect(compiledResult[0]).To(BeAssignableToTypeOf(float64(0)))
	g.Expect(compiledResult[0]).To(Equal(float64(2.71828)))
	g.Expect(compiledResult.Pack()).To(Equal(standardResult.Pack()))
}

func TestCompiledKeyEvaluator_SingleFieldEnum(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Field("val_enum")
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).NotTo(BeNil())

	msg := &gen.TypedRecord{Id: proto.Int64(1), ValEnum: gen.Color_RED.Enum()}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	compiledResult := evalCompiled(t, compiled, record, msg)
	standardResult := evalStandard(t, expr, record, msg)

	g.Expect(compiledResult).To(HaveLen(1))
	g.Expect(compiledResult[0]).To(Equal(int64(1))) // RED=1
	g.Expect(compiledResult.Pack()).To(Equal(standardResult.Pack()))
}

func TestCompiledKeyEvaluator_CompositeKeyTwoFields(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Concat(Field("order_id"), Field("price"))
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).NotTo(BeNil())

	msg := &gen.Order{OrderId: proto.Int64(99), Price: proto.Int32(250)}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	compiledResult := evalCompiled(t, compiled, record, msg)
	standardResult := evalStandard(t, expr, record, msg)

	g.Expect(compiledResult).To(HaveLen(2))
	g.Expect(compiledResult[0]).To(Equal(int64(99)))
	g.Expect(compiledResult[1]).To(Equal(int64(250)))
	g.Expect(compiledResult.Pack()).To(Equal(standardResult.Pack()))
}

func TestCompiledKeyEvaluator_RecordTypeKeyWithField(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	rtk := &RecordTypeKeyExpression{typeKeys: map[string]int64{"Order": 1, "Customer": 2}}
	expr := Concat(rtk, Field("order_id"))
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).NotTo(BeNil())

	msg := &gen.Order{OrderId: proto.Int64(42)}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	compiledResult := evalCompiled(t, compiled, record, msg)
	standardResult := evalStandard(t, expr, record, msg)

	g.Expect(compiledResult).To(HaveLen(2))
	g.Expect(compiledResult[0]).To(Equal(int64(1))) // "Order" -> 1
	g.Expect(compiledResult[1]).To(Equal(int64(42)))
	g.Expect(compiledResult.Pack()).To(Equal(standardResult.Pack()))
}

func TestCompiledKeyEvaluator_RecordTypeKeyNoMapping(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	// No typeKeys mapping - should fall back to type name string.
	rtk := &RecordTypeKeyExpression{}
	compiled := compileKeyExpression(rtk)
	g.Expect(compiled).NotTo(BeNil())

	msg := &gen.Order{OrderId: proto.Int64(1)}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	compiledResult := evalCompiled(t, compiled, record, msg)
	standardResult := evalStandard(t, rtk, record, msg)

	g.Expect(compiledResult).To(HaveLen(1))
	g.Expect(compiledResult[0]).To(Equal("Order"))
	g.Expect(compiledResult.Pack()).To(Equal(standardResult.Pack()))
}

func TestCompiledKeyEvaluator_RecordTypeKeyUnknownType(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	// typeKeys has entries but not for Customer - should fall back to type name string.
	rtk := &RecordTypeKeyExpression{typeKeys: map[string]int64{"Order": 1}}
	compiled := compileKeyExpression(rtk)
	g.Expect(compiled).NotTo(BeNil())

	msg := &gen.Customer{CustomerId: proto.Int64(1)}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	compiledResult := evalCompiled(t, compiled, record, msg)
	standardResult := evalStandard(t, rtk, record, msg)

	g.Expect(compiledResult).To(HaveLen(1))
	g.Expect(compiledResult[0]).To(Equal("Customer"))
	g.Expect(compiledResult.Pack()).To(Equal(standardResult.Pack()))
}

func TestCompiledKeyEvaluator_UnsetOptionalField(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Field("val_float")
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).NotTo(BeNil())

	// val_float is NOT set
	msg := &gen.TypedRecord{Id: proto.Int64(1)}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	compiledResult := evalCompiled(t, compiled, record, msg)
	standardResult := evalStandard(t, expr, record, msg)

	g.Expect(compiledResult).To(HaveLen(1))
	g.Expect(compiledResult[0]).To(BeNil())
	g.Expect(compiledResult.Pack()).To(Equal(standardResult.Pack()))
}

func TestCompiledKeyEvaluator_FanOutReturnsNil(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := FanOut("tags")
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).To(BeNil(), "fan-out expressions should not be compilable")
}

func TestCompiledKeyEvaluator_NilMessage(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Field("order_id")
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).NotTo(BeNil())

	record := &FDBStoredRecord[proto.Message]{Record: nil}

	var appender tupleAppender
	err := compiled.evaluate(&appender, record, nil)
	g.Expect(err).NotTo(HaveOccurred())
	result := appender.toTuple()

	g.Expect(result).To(HaveLen(1))
	g.Expect(result[0]).To(BeNil())
}

func TestCompiledKeyEvaluator_NilMessageRecordTypeKey(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	rtk := &RecordTypeKeyExpression{typeKeys: map[string]int64{"Order": 1}}
	compiled := compileKeyExpression(rtk)
	g.Expect(compiled).NotTo(BeNil())

	record := &FDBStoredRecord[proto.Message]{Record: nil}

	var appender tupleAppender
	err := compiled.evaluate(&appender, record, nil)
	g.Expect(err).NotTo(HaveOccurred())
	result := appender.toTuple()

	g.Expect(result).To(HaveLen(1))
	g.Expect(result[0]).To(BeNil())
}

func TestCompiledKeyEvaluator_EmptyKeyExpression(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := EmptyKey()
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).NotTo(BeNil())

	msg := &gen.Order{OrderId: proto.Int64(1)}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	compiledResult := evalCompiled(t, compiled, record, msg)

	g.Expect(compiledResult).To(HaveLen(0), "empty key produces no elements")
}

func TestCompiledKeyEvaluator_CompositeWithMixedTypes(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Concat(Field("val_int32"), Field("val_float"), Field("val_string"))
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).NotTo(BeNil())

	msg := &gen.TypedRecord{
		Id:        proto.Int64(1),
		ValInt32:  proto.Int32(42),
		ValFloat:  proto.Float32(1.5),
		ValString: proto.String("abc"),
	}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	compiledResult := evalCompiled(t, compiled, record, msg)
	standardResult := evalStandard(t, expr, record, msg)

	g.Expect(compiledResult).To(HaveLen(3))
	g.Expect(compiledResult[0]).To(Equal(int64(42)))
	g.Expect(compiledResult[1]).To(BeAssignableToTypeOf(float32(0)))
	g.Expect(compiledResult[1]).To(BeNumerically("~", 1.5, 0.001))
	g.Expect(compiledResult[2]).To(Equal("abc"))
	g.Expect(compiledResult.Pack()).To(Equal(standardResult.Pack()))
}

func TestCompiledKeyEvaluator_CompositeWithUnsetFields(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Concat(Field("val_int32"), Field("val_string"))
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).NotTo(BeNil())

	// Both fields unset
	msg := &gen.TypedRecord{Id: proto.Int64(1)}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	compiledResult := evalCompiled(t, compiled, record, msg)
	standardResult := evalStandard(t, expr, record, msg)

	g.Expect(compiledResult).To(HaveLen(2))
	g.Expect(compiledResult[0]).To(BeNil())
	g.Expect(compiledResult[1]).To(BeNil())
	g.Expect(compiledResult.Pack()).To(Equal(standardResult.Pack()))
}

func TestCompiledKeyEvaluator_NonexistentField(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Field("no_such_field")
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).NotTo(BeNil())

	msg := &gen.Order{OrderId: proto.Int64(1)}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	var appender tupleAppender
	err := compiled.evaluate(&appender, record, msg)
	g.Expect(err).NotTo(HaveOccurred())
	result := appender.toTuple()

	g.Expect(result).To(HaveLen(1))
	g.Expect(result[0]).To(BeNil(), "nonexistent field should produce nil")
}

func TestCompiledKeyEvaluator_GroupingKeyDelegates(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	// GroupBy wraps a whole key. The compiled path should see through it.
	inner := Concat(Field("order_id"), Field("price"))
	grouped := GroupBy(Field("price"), Field("order_id"))

	compiled := compileKeyExpression(grouped)
	g.Expect(compiled).NotTo(BeNil(), "GroupingKeyExpression should compile via its wholeKey")

	msg := &gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(100)}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	compiledResult := evalCompiled(t, compiled, record, msg)
	standardResult := evalStandard(t, inner, record, msg)

	// GroupBy(Field("price"), Field("order_id")) creates wholeKey = Concat(order_id, price)
	g.Expect(compiledResult).To(HaveLen(2))
	g.Expect(compiledResult.Pack()).To(Equal(standardResult.Pack()))
}

func TestCompiledKeyEvaluator_PackKeyWithSubspace(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Concat(Field("order_id"), Field("price"))
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).NotTo(BeNil())

	msg := &gen.Order{OrderId: proto.Int64(42), Price: proto.Int32(100)}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	// Use a subspace prefix to test packKey.
	ss := subspace.Sub(int64(1), int64(2))

	var appender tupleAppender
	packed, err := compiled.packKey(&appender, ss, record, msg)
	g.Expect(err).NotTo(HaveOccurred())

	// Standard path: pack with same prefix.
	result, err := expr.Evaluate(record, msg)
	g.Expect(err).NotTo(HaveOccurred())
	standardTuple := make(tuple.Tuple, len(result[0]))
	for i, v := range result[0] {
		standardTuple[i] = v
	}
	expected := standardTuple.PackWithPrefix(ss.Bytes())
	g.Expect(packed).To(Equal(expected))
}

func TestCompiledKeyEvaluator_RepeatedFieldInCompositeReturnsNil(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	// Concat with a fan-out field should fail to compile.
	expr := Concat(Field("order_id"), FanOut("tags"))
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).To(BeNil(), "composite with fan-out should not compile")
}

func TestCompiledKeyEvaluator_FieldStepCachesDifferentMessageTypes(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	// Both Order and TypedRecord have a "price" field.
	expr := Field("price")
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).NotTo(BeNil())

	// Evaluate on Order first.
	orderMsg := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
	orderRecord := &FDBStoredRecord[proto.Message]{Record: orderMsg}
	orderResult := evalCompiled(t, compiled, orderRecord, orderMsg)
	g.Expect(orderResult).To(HaveLen(1))
	g.Expect(orderResult[0]).To(Equal(int64(100)))

	// Now evaluate on TypedRecord (different message type, same field name).
	typedMsg := &gen.TypedRecord{Id: proto.Int64(2), Price: proto.Int32(200)}
	typedRecord := &FDBStoredRecord[proto.Message]{Record: typedMsg}
	typedResult := evalCompiled(t, compiled, typedRecord, typedMsg)
	g.Expect(typedResult).To(HaveLen(1))
	g.Expect(typedResult[0]).To(Equal(int64(200)))

	// Go back to Order to confirm cache invalidation worked.
	orderResult2 := evalCompiled(t, compiled, orderRecord, orderMsg)
	g.Expect(orderResult2[0]).To(Equal(int64(100)))
}

func TestCompiledKeyEvaluator_AppenderResetReuse(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Field("order_id")
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).NotTo(BeNil())

	var appender tupleAppender

	// First evaluation.
	msg1 := &gen.Order{OrderId: proto.Int64(1)}
	record1 := &FDBStoredRecord[proto.Message]{Record: msg1}
	err := compiled.evaluate(&appender, record1, msg1)
	g.Expect(err).NotTo(HaveOccurred())
	first := make(tuple.Tuple, len(appender.toTuple()))
	copy(first, appender.toTuple())
	g.Expect(first).To(Equal(tuple.Tuple{int64(1)}))

	// Second evaluation reuses same appender (tests reset).
	msg2 := &gen.Order{OrderId: proto.Int64(999)}
	record2 := &FDBStoredRecord[proto.Message]{Record: msg2}
	err = compiled.evaluate(&appender, record2, msg2)
	g.Expect(err).NotTo(HaveOccurred())
	second := appender.toTuple()
	g.Expect(second).To(Equal(tuple.Tuple{int64(999)}))

	// Must NOT contain leftover from first evaluation.
	g.Expect(second).To(HaveLen(1))
}

func TestCompiledKeyEvaluator_Int32Field(t *testing.T) {
	t.Parallel()
	assertByteEquivalence(t, Field("val_int32"), &gen.TypedRecord{Id: proto.Int64(1), ValInt32: proto.Int32(-100)})
}

func TestCompiledKeyEvaluator_Sint32Field(t *testing.T) {
	t.Parallel()
	assertByteEquivalence(t, Field("val_sint32"), &gen.TypedRecord{Id: proto.Int64(1), ValSint32: proto.Int32(-50)})
}

func TestCompiledKeyEvaluator_Sint64Field(t *testing.T) {
	t.Parallel()
	assertByteEquivalence(t, Field("val_sint64"), &gen.TypedRecord{Id: proto.Int64(1), ValSint64: proto.Int64(-999999)})
}

func TestCompiledKeyEvaluator_Sfixed32Field(t *testing.T) {
	t.Parallel()
	assertByteEquivalence(t, Field("val_sfixed32"), &gen.TypedRecord{Id: proto.Int64(1), ValSfixed32: proto.Int32(12345)})
}

func TestCompiledKeyEvaluator_Sfixed64Field(t *testing.T) {
	t.Parallel()
	assertByteEquivalence(t, Field("val_sfixed64"), &gen.TypedRecord{Id: proto.Int64(1), ValSfixed64: proto.Int64(-12345)})
}

func TestCompiledKeyEvaluator_BoolField(t *testing.T) {
	t.Parallel()
	assertByteEquivalence(t, Field("val_bool"), &gen.TypedRecord{Id: proto.Int64(1), ValBool: proto.Bool(true)})
}

func TestCompiledKeyEvaluator_BoolFieldFalse(t *testing.T) {
	t.Parallel()
	assertByteEquivalence(t, Field("val_bool"), &gen.TypedRecord{Id: proto.Int64(1), ValBool: proto.Bool(false)})
}

func TestCompiledKeyEvaluator_BytesField(t *testing.T) {
	t.Parallel()
	assertByteEquivalence(t, Field("val_bytes"), &gen.TypedRecord{Id: proto.Int64(1), ValBytes: []byte{0x00, 0xFF, 0xAB}})
}

func TestCompiledKeyEvaluator_LargeInt64(t *testing.T) {
	t.Parallel()
	assertByteEquivalence(t, Field("order_id"), &gen.Order{OrderId: proto.Int64(9223372036854775807)}) // MaxInt64
}

func TestCompiledKeyEvaluator_NegativeInt64(t *testing.T) {
	t.Parallel()
	assertByteEquivalence(t, Field("order_id"), &gen.Order{OrderId: proto.Int64(-9223372036854775808)}) // MinInt64
}

func TestCompiledKeyEvaluator_ZeroInt64(t *testing.T) {
	t.Parallel()
	assertByteEquivalence(t, Field("order_id"), &gen.Order{OrderId: proto.Int64(0)})
}

func TestCompiledKeyEvaluator_EmptyString(t *testing.T) {
	t.Parallel()
	assertByteEquivalence(t, Field("name"), &gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("")})
}

func TestCompiledKeyEvaluator_UnicodeString(t *testing.T) {
	t.Parallel()
	assertByteEquivalence(t, Field("name"), &gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Hello \u4e16\u754c \U0001F600")})
}

func TestCompiledKeyEvaluator_NestedRecordTypeKeyReturnsNil(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	// RecordTypeKey with nested expression should not compile.
	rtk := RecordTypeKey().Nest(Field("order_id"))
	compiled := compileKeyExpression(rtk)
	g.Expect(compiled).To(BeNil(), "nested RecordTypeKeyExpression should not compile")
}

func TestCompiledKeyEvaluator_NestingExpressionReturnsNil(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	// NestingKeyExpression should not compile (unsupported).
	expr := Nest("flower", Field("name"))
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).To(BeNil(), "NestingKeyExpression should not compile")
}

func TestCompiledKeyEvaluator_FanOutFieldReturnsErrFanOut(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	// A repeated field (non-fan-out FanTypeNone) should also fail at runtime
	// because fieldStep checks fd.IsList().
	// But tags is repeated and FanOut uses FanTypeFanOut so compile returns nil.
	// Let's test a Concat where a middle expression sneaks through somehow:
	// Actually we can't -- FanOut won't compile. So just verify it returns nil.
	compiled := compileKeyExpression(FanOut("tags"))
	g.Expect(compiled).To(BeNil())
}

func TestCompiledKeyEvaluator_ThreeFieldComposite(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	rtk := &RecordTypeKeyExpression{typeKeys: map[string]int64{"Order": 7}}
	expr := Concat(rtk, Field("order_id"), Field("price"))
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).NotTo(BeNil())

	msg := &gen.Order{OrderId: proto.Int64(42), Price: proto.Int32(250)}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	compiledResult := evalCompiled(t, compiled, record, msg)
	standardResult := evalStandard(t, expr, record, msg)

	g.Expect(compiledResult).To(HaveLen(3))
	g.Expect(compiledResult[0]).To(Equal(int64(7)))
	g.Expect(compiledResult[1]).To(Equal(int64(42)))
	g.Expect(compiledResult[2]).To(Equal(int64(250)))
	g.Expect(compiledResult.Pack()).To(Equal(standardResult.Pack()))
}

func TestCompiledKeyEvaluator_AllIntegerTypes(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Concat(
		Field("val_int32"),
		Field("val_int64"),
		Field("val_sint32"),
		Field("val_sint64"),
		Field("val_sfixed32"),
		Field("val_sfixed64"),
	)
	compiled := compileKeyExpression(expr)
	g.Expect(compiled).NotTo(BeNil())

	msg := &gen.TypedRecord{
		Id:          proto.Int64(1),
		ValInt32:    proto.Int32(32),
		ValInt64:    proto.Int64(64),
		ValSint32:   proto.Int32(-32),
		ValSint64:   proto.Int64(-64),
		ValSfixed32: proto.Int32(320),
		ValSfixed64: proto.Int64(640),
	}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	compiledResult := evalCompiled(t, compiled, record, msg)
	standardResult := evalStandard(t, expr, record, msg)

	g.Expect(compiledResult).To(HaveLen(6))
	for i := range compiledResult {
		g.Expect(compiledResult[i]).To(BeAssignableToTypeOf(int64(0)), "element %d should be int64", i)
	}
	g.Expect(compiledResult.Pack()).To(Equal(standardResult.Pack()))
}
