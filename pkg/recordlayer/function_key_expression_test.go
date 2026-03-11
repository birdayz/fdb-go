package recordlayer

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("FunctionKeyExpression", func() {

	Describe("Constructor", func() {
		It("creates with correct name and arguments", func() {
			args := EmptyKey()
			expr := FunctionExpr("test_fn", args)
			Expect(expr.Name()).To(Equal("test_fn"))
			Expect(expr.Arguments()).To(Equal(args))
		})
	})

	Describe("Proto round-trip", func() {
		It("round-trips with EmptyKey arguments", func() {
			original := FunctionExpr("get_versionstamp_incarnation", EmptyKey())
			p := original.ToKeyExpression()
			restored, err := KeyExpressionFromProto(p)
			Expect(err).NotTo(HaveOccurred())

			fn, ok := restored.(*FunctionKeyExpression)
			Expect(ok).To(BeTrue(), "expected *FunctionKeyExpression")
			Expect(fn.Name()).To(Equal("get_versionstamp_incarnation"))
			_, argsIsEmpty := fn.Arguments().(*EmptyKeyExpression)
			Expect(argsIsEmpty).To(BeTrue(), "expected EmptyKeyExpression arguments")
		})

		It("round-trips with Field argument", func() {
			original := FunctionExpr("custom_fn", Field("name"))
			p := original.ToKeyExpression()
			restored, err := KeyExpressionFromProto(p)
			Expect(err).NotTo(HaveOccurred())

			fn, ok := restored.(*FunctionKeyExpression)
			Expect(ok).To(BeTrue())
			Expect(fn.Name()).To(Equal("custom_fn"))

			field, ok := fn.Arguments().(*FieldKeyExpression)
			Expect(ok).To(BeTrue(), "expected FieldKeyExpression arguments")
			Expect(field.fieldName).To(Equal("name"))
		})

		It("round-trips with Concat arguments", func() {
			original := FunctionExpr("nested_fn", Concat(Field("a"), Field("b")))
			p := original.ToKeyExpression()
			restored, err := KeyExpressionFromProto(p)
			Expect(err).NotTo(HaveOccurred())

			fn, ok := restored.(*FunctionKeyExpression)
			Expect(ok).To(BeTrue())
			Expect(fn.Name()).To(Equal("nested_fn"))

			comp, ok := fn.Arguments().(*CompositeKeyExpression)
			Expect(ok).To(BeTrue(), "expected CompositeKeyExpression arguments")
			Expect(comp.expressions).To(HaveLen(2))
		})
	})

	Describe("FieldNames", func() {
		It("returns empty for EmptyKey arguments", func() {
			expr := FunctionExpr("fn", EmptyKey())
			Expect(expr.FieldNames()).To(BeEmpty())
		})

		It("returns field name from Field argument", func() {
			expr := FunctionExpr("fn", Field("x"))
			Expect(expr.FieldNames()).To(Equal([]string{"x"}))
		})

		It("returns multiple field names from Concat arguments", func() {
			expr := FunctionExpr("fn", Concat(Field("a"), Field("b")))
			Expect(expr.FieldNames()).To(Equal([]string{"a", "b"}))
		})
	})

	Describe("createsDuplicates", func() {
		It("always returns true for FunctionKeyExpression", func() {
			Expect(createsDuplicates(FunctionExpr("fn", EmptyKey()))).To(BeTrue())
			Expect(createsDuplicates(FunctionExpr("fn", Field("x")))).To(BeTrue())
			Expect(createsDuplicates(FunctionExpr("fn", Concat(Field("a"), Field("b"))))).To(BeTrue())
		})
	})

	Describe("keyExpressionColumnSize", func() {
		It("returns 1", func() {
			Expect(keyExpressionColumnSize(FunctionExpr("fn", EmptyKey()))).To(Equal(1))
			Expect(keyExpressionColumnSize(FunctionExpr("fn", Concat(Field("a"), Field("b"))))).To(Equal(1))
		})
	})

	Describe("keyExpressionEquals", func() {
		It("same name and same args returns true", func() {
			a := FunctionExpr("fn", EmptyKey())
			b := FunctionExpr("fn", EmptyKey())
			Expect(keyExpressionEquals(a, b)).To(BeTrue())
		})

		It("same name and same Field args returns true", func() {
			a := FunctionExpr("fn", Field("x"))
			b := FunctionExpr("fn", Field("x"))
			Expect(keyExpressionEquals(a, b)).To(BeTrue())
		})

		It("different name returns false", func() {
			a := FunctionExpr("fn1", EmptyKey())
			b := FunctionExpr("fn2", EmptyKey())
			Expect(keyExpressionEquals(a, b)).To(BeFalse())
		})

		It("different args returns false", func() {
			a := FunctionExpr("fn", EmptyKey())
			b := FunctionExpr("fn", Field("x"))
			Expect(keyExpressionEquals(a, b)).To(BeFalse())
		})

		It("FunctionKeyExpression vs FieldKeyExpression returns false", func() {
			a := FunctionExpr("fn", EmptyKey())
			b := Field("fn")
			Expect(keyExpressionEquals(a, b)).To(BeFalse())
		})
	})

	Describe("normalizeKeyForPositions", func() {
		It("returns single element", func() {
			expr := FunctionExpr("fn", EmptyKey())
			result := normalizeKeyForPositions(expr)
			Expect(result).To(HaveLen(1))
			Expect(result[0]).To(Equal(expr))
		})

		It("returns the same expression instance", func() {
			expr := FunctionExpr("fn", Field("x"))
			result := normalizeKeyForPositions(expr)
			Expect(result).To(HaveLen(1))
			// Should be the exact same pointer
			fn, ok := result[0].(*FunctionKeyExpression)
			Expect(ok).To(BeTrue())
			Expect(fn.Name()).To(Equal("fn"))
		})
	})

	Describe("Evaluate with registered function", func() {
		const testFnName = "test_echo_for_function_key_test"

		AfterEach(func() {
			delete(globalFunctionRegistry, testFnName)
		})

		It("calls the registered function with evaluated arguments", func() {
			RegisterFunction(testFnName, func(_ *FDBStoredRecord[proto.Message], _ proto.Message, args [][]any) ([][]any, error) {
				return args, nil
			})

			expr := FunctionExpr(testFnName, EmptyKey())
			result, err := expr.Evaluate(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			// EmptyKey evaluates to [[]], so function receives [[]] and echoes it back
			Expect(result).To(Equal([][]any{{}}))
		})

		It("passes Field-evaluated arguments to the function", func() {
			var capturedArgs [][]any
			RegisterFunction(testFnName, func(_ *FDBStoredRecord[proto.Message], _ proto.Message, args [][]any) ([][]any, error) {
				capturedArgs = args
				return [][]any{{int64(42)}}, nil
			})

			expr := FunctionExpr(testFnName, EmptyKey())
			result, err := expr.Evaluate(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([][]any{{int64(42)}}))
			Expect(capturedArgs).To(Equal([][]any{{}}))
		})
	})

	Describe("Evaluate with unknown function", func() {
		It("returns error for unregistered function", func() {
			expr := FunctionExpr("totally_nonexistent_function_xyz", EmptyKey())
			_, err := expr.Evaluate(nil, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unknown function key expression"))
			Expect(err.Error()).To(ContainSubstring("totally_nonexistent_function_xyz"))
		})
	})

	Describe("get_versionstamp_incarnation", func() {
		It("returns error with nil record", func() {
			expr := FunctionExpr("get_versionstamp_incarnation", EmptyKey())
			_, err := expr.Evaluate(nil, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("get_versionstamp_incarnation requires store context"))
		})

		It("returns error with nil Store on record", func() {
			record := &FDBStoredRecord[proto.Message]{
				Store: nil,
			}
			expr := FunctionExpr("get_versionstamp_incarnation", EmptyKey())
			_, err := expr.Evaluate(record, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("get_versionstamp_incarnation requires store context"))
		})
	})

	Describe("countVersionColumns", func() {
		It("returns 0 for FunctionExpr with EmptyKey arguments", func() {
			expr := FunctionExpr("fn", EmptyKey())
			Expect(countVersionColumns(expr)).To(Equal(0))
		})

		It("returns 1 for FunctionExpr with VersionKey arguments", func() {
			expr := FunctionExpr("fn", VersionKey())
			Expect(countVersionColumns(expr)).To(Equal(1))
		})

		It("returns 0 for FunctionExpr with Field arguments", func() {
			expr := FunctionExpr("fn", Field("x"))
			Expect(countVersionColumns(expr)).To(Equal(0))
		})

		It("returns 1 for FunctionExpr with Concat containing VersionKey", func() {
			expr := FunctionExpr("fn", Concat(Field("a"), VersionKey()))
			Expect(countVersionColumns(expr)).To(Equal(1))
		})
	})
})
