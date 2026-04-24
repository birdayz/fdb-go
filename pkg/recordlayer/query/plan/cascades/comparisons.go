package cascades

import "fmt"

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
)

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
	if left == nil || c.Operand == nil {
		return TriUnknown
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

// cmpAny is a tiny total-order comparator over the types the seed
// predicates exercise: int64, float64, string. Returns (cmp, ok);
// ok=false signals type mismatch — the caller degrades to UNKNOWN
// (SQL 3VL). The real port uses the embedded engine's
// `functions.CompareValues` which handles numeric promotion; seed
// stays minimal so the test surface is small.
func cmpAny(a, b any) (int, bool) {
	switch av := a.(type) {
	case int64:
		bv, ok := b.(int64)
		if !ok {
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
	case float64:
		bv, ok := b.(float64)
		if !ok {
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
	case string:
		bv, ok := b.(string)
		if !ok {
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

func (*ComparisonPredicate) Children() []QueryPredicate { return nil }

func (p *ComparisonPredicate) Eval(evalCtx any) TriBool {
	if p.Operand == nil {
		return TriUnknown
	}
	left := p.Operand.Evaluate(evalCtx)
	return p.Comparison.Eval(left)
}

func (p *ComparisonPredicate) Explain() string {
	operandName := "<unknown>"
	if p.Operand != nil {
		operandName = p.Operand.Name()
	}
	return fmt.Sprintf("%s %s %v", operandName, p.Comparison.Type.Symbol(), p.Comparison.Operand)
}
