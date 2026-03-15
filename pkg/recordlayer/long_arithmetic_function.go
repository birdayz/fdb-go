package recordlayer

import (
	"fmt"
	"math"

	"google.golang.org/protobuf/proto"
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
		RegisterFunction(f.name, makeBinaryEvaluator(f.name, op))
	}

	// "both" functions: unary (1 arg) OR binary (2 args).
	// Java registers "sub" as bothFunction(x -> -x, Math::subtractExact)
	// and "subtract" as bothFunction("sub", Math::negateExact, Math::subtractExact).
	RegisterFunction("sub", makeBothEvaluator("sub", longNegate, longSubtractExact))
	RegisterFunction("subtract", makeBothEvaluator("subtract", longNegateExact, longSubtractExact))

	// Aliases: "multiply" → same as "mul", "divide" → same as "div"
	RegisterFunction("multiply", makeBinaryEvaluator("multiply", longMultiplyExact))
	RegisterFunction("divide", makeBinaryEvaluator("divide", longDiv))

	// Unary: bitnot (1 argument, bitwise complement)
	RegisterFunction("bitnot", makeUnaryEvaluator("bitnot", func(x int64) (int64, error) {
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
	if left == nil || right == nil {
		return nil, nil
	}
	l, ok := left.(int64)
	if !ok {
		return nil, fmt.Errorf("function %s: left argument must be int64, got %T", name, left)
	}
	r, ok := right.(int64)
	if !ok {
		return nil, fmt.Errorf("function %s: right argument must be int64, got %T", name, right)
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
	if arg == nil {
		return nil, nil
	}
	x, ok := arg.(int64)
	if !ok {
		return nil, fmt.Errorf("function %s: argument must be int64, got %T", name, arg)
	}
	result, err := op(x)
	if err != nil {
		return nil, fmt.Errorf("function %s: %w", name, err)
	}
	return result, nil
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
