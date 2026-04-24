package cascades

import (
	"fmt"
	"strings"
)

// Comparisons — seed.
//
// Ports Java's
// `com.apple.foundationdb.record.query.expressions.Comparisons.Type`
// enum + `ComparisonPredicate` / equivalent wrapper in cascades.
// A ComparisonPredicate carries an operand Value (left-hand side)
// and a Comparison (operator + literal right-hand side value).
//
// Seed is intentionally narrow: just the six common comparison
// operators, constant RHS only. Follow-up shifts add: parameter-
// bound Comparison (RHS supplied at plan-cache lookup time),
// `LIKE` / `STARTS_WITH`, the `ComparisonRange` aggregator.

// ComparisonType is the operator carried by a Comparison. Enum
// values match Java's
// `com.apple.foundationdb.record.query.expressions.Comparisons.Type`
// ordering so serialised plans round-trip (once we have plan
// serialisation).
type ComparisonType int

const (
	ComparisonEquals        ComparisonType = iota // =
	ComparisonNotEquals                           // !=, <>
	ComparisonLessThan                            // <
	ComparisonLessThanOrEq                        // <=
	ComparisonGreaterThan                         // >
	ComparisonGreaterThanEq                       // >=
	ComparisonIsNull                              // IS NULL (unary, LHS-only)
	ComparisonIsNotNull                           // IS NOT NULL (unary, LHS-only)
	ComparisonStartsWith                          // STARTS_WITH (string LHS, string RHS prefix)
)

// IsUnary reports whether the comparison takes no RHS operand
// (IS NULL / IS NOT NULL). Callers use this to skip Operand-based
// folding / plumbing for unary predicates.
func (c ComparisonType) IsUnary() bool {
	return c == ComparisonIsNull || c == ComparisonIsNotNull
}

// Symbol returns the SQL-text form of the operator.
func (c ComparisonType) Symbol() string {
	switch c {
	case ComparisonEquals:
		return "="
	case ComparisonNotEquals:
		return "<>"
	case ComparisonLessThan:
		return "<"
	case ComparisonLessThanOrEq:
		return "<="
	case ComparisonGreaterThan:
		return ">"
	case ComparisonGreaterThanEq:
		return ">="
	case ComparisonIsNull:
		return "IS NULL"
	case ComparisonIsNotNull:
		return "IS NOT NULL"
	case ComparisonStartsWith:
		return "STARTS_WITH"
	default:
		return "?"
	}
}

// Comparison pairs a ComparisonType with a literal right-hand side
// value. The operand (left-hand side) lives on the parent
// ComparisonPredicate so `a = 1 AND a > 0` shares the operand
// while each Comparison carries its own (Type, Value) pair.
type Comparison struct {
	Type    ComparisonType
	Operand any // seed: Go-native literal; real port wraps in a Value
}

// Eval compares left against c's operand per c's ComparisonType.
// NULL (nil) on either side returns UNKNOWN per SQL 3VL. The
// operand is compared via Go's comparable semantics — seed assumes
// matching types; follow-up shifts add `CompareValues`-style
// numeric promotion.
func (c Comparison) Eval(left any) TriBool {
	// IS NULL / IS NOT NULL are SQL 2VL: they resolve definitively
	// even when the LHS is NULL, and ignore Operand entirely.
	switch c.Type {
	case ComparisonIsNull:
		if left == nil {
			return TriTrue
		}
		return TriFalse
	case ComparisonIsNotNull:
		if left == nil {
			return TriFalse
		}
		return TriTrue
	}
	if left == nil || c.Operand == nil {
		return TriUnknown
	}
	// STARTS_WITH needs string LHS + string RHS; typed-mismatch
	// degrades to UNKNOWN per SQL 3VL like the numeric comparators.
	if c.Type == ComparisonStartsWith {
		ls, lok := left.(string)
		rs, rok := c.Operand.(string)
		if !lok || !rok {
			return TriUnknown
		}
		if strings.HasPrefix(ls, rs) {
			return TriTrue
		}
		return TriFalse
	}
	cmp, ok := cmpAny(left, c.Operand)
	if !ok {
		return TriUnknown
	}
	var matches bool
	switch c.Type {
	case ComparisonEquals:
		matches = cmp == 0
	case ComparisonNotEquals:
		matches = cmp != 0
	case ComparisonLessThan:
		matches = cmp < 0
	case ComparisonLessThanOrEq:
		matches = cmp <= 0
	case ComparisonGreaterThan:
		matches = cmp > 0
	case ComparisonGreaterThanEq:
		matches = cmp >= 0
	default:
		return TriUnknown
	}
	if matches {
		return TriTrue
	}
	return TriFalse
}

// cmpAny is a total-order comparator over the primitive types the
// seed predicates exercise: signed-int{8,16,32,64}, int, float{32,64},
// string. Returns (cmp, ok); ok=false signals a genuine type
// mismatch (int vs string, bool, etc.) — the caller degrades to
// UNKNOWN per SQL 3VL.
//
// Numeric promotion matches Java's `functions.CompareValues`: any
// two numeric operands compare by promoting both to int64 (when all
// integral) or float64 (when either side is floating). Keeps the
// common WHERE `int32_col > 18` case from degrading to UNKNOWN just
// because the literal arrived as int64.
func cmpAny(a, b any) (int, bool) {
	if af, bf, ok := promoteFloat(a, b); ok {
		switch {
		case af < bf:
			return -1, true
		case af > bf:
			return 1, true
		default:
			return 0, true
		}
	}
	if ai, bi, ok := promoteInt(a, b); ok {
		switch {
		case ai < bi:
			return -1, true
		case ai > bi:
			return 1, true
		default:
			return 0, true
		}
	}
	if av, ok := a.(string); ok {
		bv, ok2 := b.(string)
		if !ok2 {
			return 0, false
		}
		switch {
		case av < bv:
			return -1, true
		case av > bv:
			return 1, true
		default:
			return 0, true
		}
	}
	return 0, false
}

// promoteInt returns (a,b) as int64 when both are integral. Signed
// int types only — unsigned promotion needs overflow rules we'll add
// when a concrete use case calls for it.
func promoteInt(a, b any) (int64, int64, bool) {
	ai, ok := toInt64(a)
	if !ok {
		return 0, 0, false
	}
	bi, ok := toInt64(b)
	if !ok {
		return 0, 0, false
	}
	return ai, bi, true
}

// promoteFloat returns (a,b) as float64 when at least one side is
// floating and the other is floating or integral. Pure-integral
// pairs return ok=false so the caller prefers the exact int path.
func promoteFloat(a, b any) (float64, float64, bool) {
	af, aFloat, aNum := toFloat64(a)
	if !aNum {
		return 0, 0, false
	}
	bf, bFloat, bNum := toFloat64(b)
	if !bNum {
		return 0, 0, false
	}
	if !aFloat && !bFloat {
		return 0, 0, false
	}
	return af, bf, true
}

// toInt64 reports whether v is an integral type; returns the int64
// promotion when so.
func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case int16:
		return int64(x), true
	case int8:
		return int64(x), true
	}
	return 0, false
}

// toFloat64 reports whether v is numeric (int-like or float) and
// returns its float64 promotion. isFloat distinguishes native-float
// inputs from integral ones promoted here — promoteFloat uses it to
// prefer the int path when both sides are integral.
func toFloat64(v any) (f float64, isFloat, numeric bool) {
	switch x := v.(type) {
	case float64:
		return x, true, true
	case float32:
		return float64(x), true, true
	case int64:
		return float64(x), false, true
	case int:
		return float64(x), false, true
	case int32:
		return float64(x), false, true
	case int16:
		return float64(x), false, true
	case int8:
		return float64(x), false, true
	}
	return 0, false, false
}

// ComparisonPredicate applies a Comparison to an operand `Value`.
// The operand is evaluated against a row (the eval context) via
// Value.Evaluate to produce the left-hand side; the comparison's
// literal is the right-hand side. Returns UNKNOWN when either side
// is NULL (SQL 3VL).
type ComparisonPredicate struct {
	Operand    Value
	Comparison Comparison
}

// NewComparisonPredicate builds a ComparisonPredicate.
func NewComparisonPredicate(operand Value, cmp Comparison) *ComparisonPredicate {
	return &ComparisonPredicate{Operand: operand, Comparison: cmp}
}

func (*ComparisonPredicate) Children() []QueryPredicate { return []QueryPredicate{} }

func (p *ComparisonPredicate) Eval(evalCtx any) TriBool {
	if p.Operand == nil {
		return TriUnknown
	}
	left := p.Operand.Evaluate(evalCtx)
	return p.Comparison.Eval(left)
}

func (p *ComparisonPredicate) Explain() string {
	operandText := "<unknown>"
	if p.Operand != nil {
		// Use the tree-walking ExplainValue for readable output —
		// `age` / `(a + b)` / `CAST(1 AS STRING)` instead of the
		// bare Value.Name() which returns "field" / "arith" / "cast".
		operandText = ExplainValue(p.Operand)
	}
	if p.Comparison.Type.IsUnary() {
		return fmt.Sprintf("%s %s", operandText, p.Comparison.Type.Symbol())
	}
	return fmt.Sprintf("%s %s %v", operandText, p.Comparison.Type.Symbol(), p.Comparison.Operand)
}
