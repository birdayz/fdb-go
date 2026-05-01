// Package functions holds SQL-value operations that don't need
// connection/session state: checked integer arithmetic, numeric +
// bitwise operators with SQL semantics, type-coercion helpers used
// by scalar-function arguments, and (in future PRs) the scalar
// function dispatcher + protoreflect <-> driver.Value marshaling.
//
// Mirrors Java's fdb-relational-core/recordlayer/query/functions/
// package plus the arithmetic helpers in ArithmeticValue.
// PhysicalOperator. All functions here are pure — no *Session, no
// *EmbeddedConnection — so the naive planner, Cascades planner, and
// any future frontend can call them uniformly.
//
// This is the first move of RFC-021 Phase 1c1. Future commits add
// castValue, convertToProtoValue, protoValueToDriver and the scalar
// function core.
package functions

import (
	"math"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// AddInt64Checked returns a+b and a success flag. Overflow iff
// the signs of a and b are the same and the result's sign flips.
// Matches Java's Math.addExact semantics.
func AddInt64Checked(a, b int64) (int64, bool) {
	s := a + b
	if (a^b) < 0 || (a^s) >= 0 {
		return s, true
	}
	return 0, false
}

// SubInt64Checked returns a-b and a success flag. Overflow iff the
// signs of a and b differ and the sign of the result flips against a.
func SubInt64Checked(a, b int64) (int64, bool) {
	d := a - b
	if (a^b) >= 0 || (a^d) >= 0 {
		return d, true
	}
	return 0, false
}

// MulInt64Checked returns a*b and a success flag. Mirrors Java's
// Math.multiplyExact. Uses the textbook "divide back" check: overflow
// iff (a*b)/b != a. The first special case (a == MinInt64 && b == -1)
// is REQUIRED: p/b would compute MinInt64 / -1, which traps with
// SIGFPE on amd64 — we must detect and bail before the divide. The
// second symmetric case is redundant (divide-back would flag it
// without a hardware trap, because the divisor is MinInt64 not -1)
// but kept for parallelism with the first so the intent is obvious.
func MulInt64Checked(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a == math.MinInt64 && b == -1 {
		return 0, false
	}
	if b == math.MinInt64 && a == -1 {
		return 0, false
	}
	p := a * b
	if p/b != a {
		return 0, false
	}
	return p, true
}

// ApplyMathOp evaluates one of +, -, *, /, % on two driver-level
// values with SQL semantics: NULL propagates, int64×int64 stays
// int64 and is overflow-checked (matching Java's ArithmeticValue.
// PhysicalOperator.*_LL), mixed int/float widens to float64.
// Division by zero errors with ErrCodeDivisionByZero; overflow
// errors with ErrCodeNumericValueOutOfRange.
func ApplyMathOp(left, right any, op string) (any, error) {
	// NULL propagates through arithmetic per SQL 3-valued logic.
	if left == nil || right == nil {
		return nil, nil
	}
	// Integer / integer stays integer and is overflow-checked —
	// matches Java's ArithmeticValue.PhysicalOperator.ADD_LL/SUB_LL/
	// MUL_LL/DIV_LL/MOD_LL which are Math.addExact/subtractExact/
	// multiplyExact on longs (throwing ArithmeticException on
	// overflow) and literal long / long / long % long (truncation
	// toward zero). Going through float first would turn 10 / 3 into
	// 3.333 instead of 3, and unchecked ops would silently wrap
	// MAX_INT + 1 to MIN_INT.
	// String + string → concat (Java alignment). Java's
	// `+` is overloaded for strings; fdb-relational evaluates
	// 'foo' + 'bar' = 'foobar'. Other operators (- * / %) on strings
	// remain unsupported.
	if op == "+" {
		if ls, lstr := left.(string); lstr {
			if rs, rstr := right.(string); rstr {
				return ls + rs, nil
			}
		}
	}
	li, lok := left.(int64)
	ri, rok := right.(int64)
	if lok && rok {
		switch op {
		case "+":
			r, ok := AddInt64Checked(li, ri)
			if !ok {
				return nil, api.NewErrorf(api.ErrCodeNumericValueOutOfRange, "long overflow")
			}
			return r, nil
		case "-":
			r, ok := SubInt64Checked(li, ri)
			if !ok {
				return nil, api.NewErrorf(api.ErrCodeNumericValueOutOfRange, "long overflow")
			}
			return r, nil
		case "*":
			r, ok := MulInt64Checked(li, ri)
			if !ok {
				return nil, api.NewErrorf(api.ErrCodeNumericValueOutOfRange, "long overflow")
			}
			return r, nil
		case "/":
			if ri == 0 {
				return nil, api.NewErrorf(api.ErrCodeDivisionByZero, "/ by zero")
			}
			// MinInt64 / -1 overflows (abs value doesn't fit in int64).
			if li == math.MinInt64 && ri == -1 {
				return nil, api.NewErrorf(api.ErrCodeNumericValueOutOfRange, "long overflow")
			}
			return li / ri, nil
		case "%":
			if ri == 0 {
				return nil, api.NewErrorf(api.ErrCodeDivisionByZero, "/ by zero")
			}
			return li % ri, nil
		default:
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported math operator %q", op)
		}
	}
	lf, lok := ToFloat64(left)
	rf, rok := ToFloat64(right)
	if !lok || !rok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"arithmetic operator %q requires numeric operands, got %T and %T", op, left, right)
	}
	var result float64
	switch op {
	case "+":
		result = lf + rf
	case "-":
		result = lf - rf
	case "*":
		result = lf * rf
	case "/":
		// Java IEEE-754 semantics for double division: x / 0.0 = ±Infinity,
		// 0.0 / 0.0 = NaN. fdb-relational does NOT throw — only integer
		// division throws ArithmeticException "/ by zero". Aligned
		// A previous broad-stroke change incorrectly threw
		// for both int and float).
		result = lf / rf
	case "%":
		// Java's `%` over double follows Math.IEEEremainder-like — for
		// rf=0 returns NaN, never throws. Mirror via math.Mod.
		result = math.Mod(lf, rf)
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported math operator %q", op)
	}
	return result, nil
}

// ApplyBitOp evaluates a bitwise operator. SQL standard + Java both
// require integer operands; float / string operands are an error (not
// a silent cast). The grammar exposes bitOperator tokens as
// concatenated text, so `<<` comes through as "<<" and `>>` as ">>".
//
// Bit-shift operators `<<` / `>>` are intentionally NOT registered —
// matching fdb-relational 4.11.1.0's behaviour. Java tokenizes the
// operators but has no entry in the function registry, so its planner
// returns `RelationalException: Unsupported operator <<`. The Go
// embedded engine mirrors this by NOT having `<<` / `>>` cases here,
// so they fall through to the default ErrCodeUnsupportedOperation
// arm. Same architectural reason in both engines: no evaluator
// registered for shift operators. Per CLAUDE.md "Java↔Go conformance
// gotchas": doesn't work in Java → doesn't work in Go.
func ApplyBitOp(left, right any, op string) (any, error) {
	if left == nil || right == nil {
		return nil, nil // NULL propagates
	}
	li, lok := left.(int64)
	ri, rok := right.(int64)
	if !lok || !rok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"bitwise operator %q requires integer operands, got %T and %T", op, left, right)
	}
	switch op {
	case "&":
		return li & ri, nil
	case "|":
		return li | ri, nil
	case "^":
		return li ^ ri, nil
	}
	// Java parity: fdb-relational raises `RelationalException:
	// Unsupported operator <op>` from the function-registry lookup
	// (CLAUDE.md gotcha: "Bit-shift operators `<<` / `>>`"). Match
	// the exact phrasing so cross-engine ExpectErrorContains can pin
	// identical substrings.
	return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "Unsupported operator %s", op)
}

// ToFloat64 coerces int64 / float64 to float64 for mixed-type
// arithmetic. Returns false for any other input type — callers error
// out with "requires numeric operands" on the failure path.
func ToFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

// ToIntegerArg coerces v to int64 for integer-typed function arguments
// (position, length, count). Whole-value floats are accepted as a
// convenience (`LEFT('hi', 2.0)` works); fractional floats and
// non-numeric types error rather than silently truncating to 0.
func ToIntegerArg(v any, funcName, argName string) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, api.NewErrorf(api.ErrCodeInvalidParameter,
				"%s: %s must be an integer, got %v", funcName, argName, n)
		}
		i := int64(n)
		if float64(i) != n {
			return 0, api.NewErrorf(api.ErrCodeInvalidParameter,
				"%s: %s must be an integer, got %v", funcName, argName, n)
		}
		return i, nil
	default:
		return 0, api.NewErrorf(api.ErrCodeInvalidParameter,
			"%s: %s must be an integer, got %T", funcName, argName, v)
	}
}
