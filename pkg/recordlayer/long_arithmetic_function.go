package recordlayer

import (
	"fmt"
	"math"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Long arithmetic function key expressions.
// Matches Java's LongArithmethicFunctionKeyExpression (note: Java has a typo
// "Arithmethic"). These functions operate on int64 values extracted from key
// expression evaluation results.
//
// Registered function names match Java's FunctionNames constants:
//   add, subtract, sub, multiply, mul, divide, div, mod,
//   bitand, bitor, bitxor, bitnot,
//   bitmap_bit_position, bitmap_bucket_offset

func init() {
	registerArithmeticFunctions()
}

// longArithmeticFunctions names every function registerArithmeticFunctions registers:
// each reads its operands with nullableLong, so a literal argument's integer width and
// its float precision never reach a stored entry beyond the long it evaluates to.
var longArithmeticFunctions = map[string]bool{}

// registerLongArithmetic registers a LongArithmethicFunctionKeyExpression of
// minArgs..maxArgs arguments (Builder.unaryFunction 1..1, binaryFunction 2..2,
// bothFunction 1..2, LongArithmethicFunctionKeyExpression.java:162-235), one
// column (:108-111), whose null is a plain null (:98).
func registerLongArithmetic(name string, minArgs, maxArgs int, eval FunctionEvaluator) {
	longArithmeticFunctions[name] = true
	registerCoreFunction(name, FunctionSpec{Evaluator: eval, MinArguments: minArgs, MaxArguments: maxArgs, ColumnSize: 1})
}

func registerArithmeticFunctions() {
	// Binary functions: exactly 2 arguments.
	// Matches Java's LongArithmethicFunctionKeyExpressionFactory.BUILDERS.
	binaryFunctions := []struct {
		name string
		op   func(int64, int64) (int64, error)
	}{
		{"add", longAddExact},
		{"mul", longMultiplyExact},
		{"mod", longMod},
		{"div", longDiv},
		{"bitand", func(a, b int64) (int64, error) { return a & b, nil }},
		{"bitor", func(a, b int64) (int64, error) { return a | b, nil }},
		{"bitxor", func(a, b int64) (int64, error) { return a ^ b, nil }},
		{"bitmap_bit_position", longBitmapBitPosition},
		{"bitmap_bucket_offset", longBitmapBucketOffset},
	}

	for _, f := range binaryFunctions {
		op := f.op // capture
		registerLongArithmetic(f.name, 2, 2, makeBinaryEvaluator(f.name, op))
	}

	// "both" functions: unary (1 arg) OR binary (2 args).
	// Java registers "sub" as bothFunction(x -> -x, Math::subtractExact)
	// and "subtract" as bothFunction("sub", Math::negateExact, Math::subtractExact).
	registerLongArithmetic("sub", 1, 2, makeBothEvaluator("sub", longNegate, longSubtractExact))
	registerLongArithmetic("subtract", 1, 2, makeBothEvaluator("subtract", longNegateExact, longSubtractExact))

	// Aliases: "multiply" → same as "mul", "divide" → same as "div"
	registerLongArithmetic("multiply", 2, 2, makeBinaryEvaluator("multiply", longMultiplyExact))
	registerLongArithmetic("divide", 2, 2, makeBinaryEvaluator("divide", longDiv))

	// Unary: bitnot (1 argument, bitwise complement)
	registerLongArithmetic("bitnot", 1, 1, makeUnaryEvaluator("bitnot", func(x int64) (int64, error) {
		return ^x, nil
	}))
}

// makeBinaryEvaluator creates a FunctionEvaluator that applies a binary int64 operation
// to each argument tuple. Each tuple must have exactly 2 elements.
func makeBinaryEvaluator(name string, op func(int64, int64) (int64, error)) FunctionEvaluator {
	return func(_ *FDBStoredRecord[proto.Message], _ proto.Message, arguments [][]any) ([][]any, error) {
		result := make([][]any, len(arguments))
		for i, args := range arguments {
			if len(args) != 2 {
				return nil, fmt.Errorf("function %s requires exactly 2 arguments, got %d", name, len(args))
			}
			val, err := applyBinary(name, op, args[0], args[1])
			if err != nil {
				return nil, err
			}
			result[i] = []any{val}
		}
		return result, nil
	}
}

// makeUnaryEvaluator creates a FunctionEvaluator that applies a unary int64 operation
// to each argument tuple. Each tuple must have exactly 1 element.
func makeUnaryEvaluator(name string, op func(int64) (int64, error)) FunctionEvaluator {
	return func(_ *FDBStoredRecord[proto.Message], _ proto.Message, arguments [][]any) ([][]any, error) {
		result := make([][]any, len(arguments))
		for i, args := range arguments {
			if len(args) != 1 {
				return nil, fmt.Errorf("function %s requires exactly 1 argument, got %d", name, len(args))
			}
			val, err := applyUnary(name, op, args[0])
			if err != nil {
				return nil, err
			}
			result[i] = []any{val}
		}
		return result, nil
	}
}

// makeBothEvaluator creates a FunctionEvaluator that accepts either 1 or 2 arguments.
// With 1 argument, applies the unary operator. With 2, applies the binary operator.
// Matches Java's Builder.bothFunction().
func makeBothEvaluator(name string, unaryOp func(int64) (int64, error), binaryOp func(int64, int64) (int64, error)) FunctionEvaluator {
	return func(_ *FDBStoredRecord[proto.Message], _ proto.Message, arguments [][]any) ([][]any, error) {
		result := make([][]any, len(arguments))
		for i, args := range arguments {
			var val any
			var err error
			switch len(args) {
			case 1:
				val, err = applyUnary(name, unaryOp, args[0])
			case 2:
				val, err = applyBinary(name, binaryOp, args[0], args[1])
			default:
				return nil, fmt.Errorf("function %s requires 1 or 2 arguments, got %d", name, len(args))
			}
			if err != nil {
				return nil, err
			}
			result[i] = []any{val}
		}
		return result, nil
	}
}

// applyBinary extracts two int64 values from arguments and applies the binary operation.
// Returns nil if either argument is nil (matches Java's null propagation).
func applyBinary(name string, op func(int64, int64) (int64, error), left, right any) (any, error) {
	l, lok, err := nullableLong(name, 0, left)
	if err != nil {
		return nil, err
	}
	r, rok, err := nullableLong(name, 1, right)
	if err != nil {
		return nil, err
	}
	if !lok || !rok {
		return nil, nil
	}
	result, err := op(l, r)
	if err != nil {
		return nil, fmt.Errorf("function %s: %w", name, err)
	}
	return result, nil
}

// applyUnary extracts one int64 value from the argument and applies the unary operation.
// Returns nil if the argument is nil (matches Java's null propagation).
func applyUnary(name string, op func(int64) (int64, error), arg any) (any, error) {
	x, ok, err := nullableLong(name, 0, arg)
	if err != nil || !ok {
		return nil, err
	}
	result, err := op(x)
	if err != nil {
		return nil, fmt.Errorf("function %s: %w", name, err)
	}
	return result, nil
}

// KeyExpressionInvalidResultError is Java's KeyExpression.InvalidResultException
// as Key.Evaluated.getObject raises it for an argument of the wrong class. The
// fields are the exception's addLogInfo keys ("index", expected_type,
// actual_type).
type KeyExpressionInvalidResultError struct {
	Function     string
	Index        int
	ExpectedType string
	ActualType   string
}

func (e *KeyExpressionInvalidResultError) Error() string {
	return fmt.Sprintf("Invalid type in value: function %s argument %d expected %s, got %s",
		e.Function, e.Index, e.ExpectedType, e.ActualType)
}

// javaTupleValueClassName is the ACTUAL_TYPE Java logs for a value that is not a
// Number: result.getClass().getName() of the value after
// TupleTypeUtil.toTupleAppropriateValue (Key.java:553-561), so both fields of the
// error carry Java class names. A Go carrier with no Java counterpart keeps its Go
// type name, which no Java-produced value can be.
func javaTupleValueClassName(v any) string {
	switch x := v.(type) {
	case string:
		return "java.lang.String"
	case []byte:
		return "[B"
	case bool:
		return "java.lang.Boolean"
	case tuple.UUID, [16]byte:
		return "java.util.UUID"
	case tuple.Versionstamp:
		return "com.apple.foundationdb.tuple.Versionstamp"
	case tuple.Tuple:
		return "com.apple.foundationdb.tuple.Tuple"
	case []any:
		return "java.util.ArrayList"
	case proto.Message:
		return javaMessageClassName(x)
	}
	return fmt.Sprintf("%T", v)
}

// javaMessageClassName is getClass().getName() of the Java object that carries a
// message, where Go can know it: a message read through a descriptor loaded at
// run time (Go's dynamicpb, the relational layer's DynamicMessage) is
// com.google.protobuf.DynamicMessage. A GENERATED message's Java class depends
// on the protoc options of the Java build, which the Go-embedded descriptor does
// not carry faithfully (this repository's generator rewrites java_package,
// java_multiple_files and java_outer_classname in the Go copy: record_metadata.proto
// declares RecordMetaDataProto and the Go copy says RecordMetadataProto), so a
// generated message keeps its proto full name.
func javaMessageClassName(m proto.Message) string {
	if _, dynamic := m.(*dynamicpb.Message); dynamic {
		return "com.google.protobuf.DynamicMessage"
	}
	return string(m.ProtoReflect().Descriptor().FullName())
}

// nullableLong is Key.Evaluated.getNullableLong (Key.java:579-582): any
// java.lang.Number is accepted and converted with Number.longValue(); anything
// else is InvalidResultException. ok is false for a NULL argument. Field
// evaluation already widens every integral proto field to int64, so the other
// carriers come from literal arguments (an INT literal is int32, as Java's
// Integer is) and from float and double fields.
func nullableLong(name string, idx int, v any) (int64, bool, error) {
	switch x := v.(type) {
	case nil:
		return 0, false, nil
	case int64:
		return x, true, nil
	case int32:
		return int64(x), true, nil
	case int:
		return int64(x), true, nil
	case int16:
		return int64(x), true, nil
	case int8:
		return int64(x), true, nil
	case float64:
		return javaDoubleToLong(x), true, nil
	case float32:
		return javaDoubleToLong(float64(x)), true, nil
	default:
		return 0, false, &KeyExpressionInvalidResultError{
			Function: name, Index: idx, ExpectedType: "java.lang.Number", ActualType: javaTupleValueClassName(v),
		}
	}
}

// javaDoubleToLong is Java's narrowing primitive conversion from double (and,
// through the exact float-to-double widening, from float) to long (JLS 5.1.3):
// NaN becomes 0, a value beyond the long range saturates, and everything else
// rounds toward zero. Go's int64 conversion of an out-of-range float is
// implementation-defined, so the saturating arms are explicit.
func javaDoubleToLong(f float64) int64 {
	switch {
	case math.IsNaN(f):
		return 0
	case f >= 9223372036854775807.0:
		return math.MaxInt64
	case f <= -9223372036854775808.0:
		return math.MinInt64
	default:
		return int64(f)
	}
}

// Overflow-checked arithmetic operations matching Java's Math.*Exact methods.

// longAddExact returns a + b, or error on overflow. Matches Math.addExact.
func longAddExact(a, b int64) (int64, error) {
	result := a + b
	// Overflow occurs when both operands have the same sign but result has different sign.
	if (a^b) >= 0 && (a^result) < 0 {
		return 0, fmt.Errorf("long overflow: %d + %d", a, b)
	}
	return result, nil
}

// longSubtractExact returns a - b, or error on overflow. Matches Math.subtractExact.
func longSubtractExact(a, b int64) (int64, error) {
	result := a - b
	// Overflow occurs when operands have different signs and result sign differs from left.
	if (a^b) < 0 && (a^result) < 0 {
		return 0, fmt.Errorf("long overflow: %d - %d", a, b)
	}
	return result, nil
}

// longMultiplyExact returns a * b, or error on overflow. Matches Math.multiplyExact.
func longMultiplyExact(a, b int64) (int64, error) {
	result := a * b
	// Use uint64 for absolute values to avoid MinInt64 negation wrapping.
	// Matches Java's Math.multiplyExact which uses unsigned right shift (>>> 31).
	absA := uint64(a)
	if a < 0 {
		absA = uint64(-a)
	}
	absB := uint64(b)
	if b < 0 {
		absB = uint64(-b)
	}
	// Fast path: if both values fit in 31 bits, no overflow possible.
	if absA|absB <= math.MaxInt32 {
		return result, nil
	}
	// Check: if a != 0 and result/a != b, overflow occurred.
	// Special case: MinInt64 * -1 overflows but division check works.
	if a != 0 {
		if result/a != b {
			return 0, fmt.Errorf("long overflow: %d * %d", a, b)
		}
	}
	return result, nil
}

// longNegateExact returns -x, or error on overflow. Matches Math.negateExact.
// Only overflows for MinInt64.
func longNegateExact(x int64) (int64, error) {
	if x == math.MinInt64 {
		return 0, fmt.Errorf("long overflow: negate %d", x)
	}
	return -x, nil
}

// longNegate returns -x without overflow check.
// Used by Java's "sub" unary variant: Builder.bothFunction("sub", x -> -x, ...).
// Note: Java's lambda `x -> -x` does NOT use negateExact, so MinInt64 wraps silently.
func longNegate(x int64) (int64, error) {
	return -x, nil
}

// longDiv returns a / b (truncated toward zero, matching Java's / operator).
// Returns error on division by zero.
func longDiv(a, b int64) (int64, error) {
	if b == 0 {
		return 0, fmt.Errorf("division by zero: %d / %d", a, b)
	}
	return a / b, nil
}

// longMod returns a % b (matching Java's % operator — sign of result follows dividend).
// Returns error on division by zero.
func longMod(a, b int64) (int64, error) {
	if b == 0 {
		return 0, fmt.Errorf("division by zero: %d %% %d", a, b)
	}
	return a % b, nil
}

// floorDiv returns the largest (closest to positive infinity) int64 value that is less than
// or equal to the algebraic quotient. Matches Java's Math.floorDiv.
// For positive divisors with a non-negative dividend, this is the same as truncation.
// For negative dividends or divisors, it rounds toward negative infinity.
func floorDiv(a, b int64) (int64, error) {
	if b == 0 {
		return 0, fmt.Errorf("division by zero: floorDiv(%d, %d)", a, b)
	}
	q := a / b
	// If signs differ and there's a remainder, subtract 1 (floor).
	if (a^b) < 0 && q*b != a {
		q--
	}
	return q, nil
}

// longBitmapBitPosition computes the position of a value within its bitmap bucket.
// Java: Math.subtractExact(l, Math.multiplyExact(Math.floorDiv(l, r), r))
// Equivalent to a modulo that rounds toward negative infinity (floored modulo).
func longBitmapBitPosition(l, r int64) (int64, error) {
	fd, err := floorDiv(l, r)
	if err != nil {
		return 0, err
	}
	product, err := longMultiplyExact(fd, r)
	if err != nil {
		return 0, err
	}
	return longSubtractExact(l, product)
}

// longBitmapBucketOffset computes the bucket offset for a bitmap value.
// Java: Math.multiplyExact(Math.floorDiv(l, r), r)
func longBitmapBucketOffset(l, r int64) (int64, error) {
	fd, err := floorDiv(l, r)
	if err != nil {
		return 0, err
	}
	return longMultiplyExact(fd, r)
}
