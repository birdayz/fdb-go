package predicates

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TypeMismatchError is panicked when a comparison encounters incompatible
// types (e.g., int64 vs string). The executor recovers it and surfaces
// SQLSTATE 22000 (CANNOT_CONVERT_TYPE), matching Java's SemanticException.
type TypeMismatchError struct {
	Left  any
	Right any
}

func (e *TypeMismatchError) Error() string {
	return fmt.Sprintf("cannot convert types for comparison: %T vs %T", e.Left, e.Right)
}

func isNumericStringMismatch(a, b any) bool {
	aNum := isNumericType(a)
	bNum := isNumericType(b)
	_, aStr := a.(string)
	_, bStr := b.(string)
	return (aNum && bStr) || (bNum && aStr)
}

func isNumericType(v any) bool {
	switch v.(type) {
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return true
	}
	return false
}

// Comparisons — seed.
//
// Ports Java's
// `com.apple.foundationdb.record.query.expressions.Comparisons.Type`
// enum + `ComparisonPredicate` / equivalent wrapper in cascades.
// A ComparisonPredicate carries an operand Value (left-hand side)
// and a Comparison (operator + literal right-hand side value).
//
// Seed operators: =, <>, <, <=, >, >=, IS NULL, IS NOT NULL,
// STARTS_WITH, IN, IS DISTINCT FROM, IS NOT DISTINCT FROM. Constant
// RHS only. Follow-up shifts add: parameter-bound Comparison (RHS
// supplied at plan-cache lookup time), LIKE pattern comparator,
// TEXT_CONTAINS_*, the `ComparisonRange` aggregator.

// ComparisonType is the operator carried by a Comparison. Enum
// values match Java's
// `com.apple.foundationdb.record.query.expressions.Comparisons.Type`
// ordering so serialised plans round-trip (once we have plan
// serialisation).
type ComparisonType int

const (
	ComparisonEquals          ComparisonType = iota // =
	ComparisonNotEquals                             // !=, <>
	ComparisonLessThan                              // <
	ComparisonLessThanOrEq                          // <=
	ComparisonGreaterThan                           // >
	ComparisonGreaterThanEq                         // >=
	ComparisonIsNull                                // IS NULL (unary, LHS-only)
	ComparisonIsNotNull                             // IS NOT NULL (unary, LHS-only)
	ComparisonStartsWith                            // STARTS_WITH (string LHS, string RHS prefix)
	ComparisonIn                                    // IN (LHS any, RHS is a []any membership list)
	ComparisonIsDistinctFrom                        // IS DISTINCT FROM (null-safe !=)
	ComparisonNotDistinctFrom                       // IS NOT DISTINCT FROM (null-safe =)
	ComparisonLike                                  // LIKE (string LHS, SQL pattern RHS: % / _)
)

// IsUnary reports whether the comparison takes no RHS operand
// (IS NULL / IS NOT NULL). Callers use this to skip Operand-based
// folding / plumbing for unary predicates.
func (c ComparisonType) IsUnary() bool {
	return c == ComparisonIsNull || c == ComparisonIsNotNull
}

// IsEquality reports whether the comparison semantically tests for
// (null-safe or null-aware) equality. Mirrors Java's
// `Comparisons.Type.isEquality()` — useful for index-pushdown
// decisions (equality predicates can use point-lookups; inequality
// needs ranges).
func (c ComparisonType) IsEquality() bool {
	switch c {
	case ComparisonEquals, ComparisonIn, ComparisonIsNull, ComparisonNotDistinctFrom:
		return true
	}
	return false
}

// Negate returns the comparison type whose truth table is the logical
// negation of this one, plus a flag indicating whether a negation is
// known. `!(a = b)` → `a <> b`, `!(a IS NULL)` → `a IS NOT NULL`,
// etc. Used by the NOT-over-Comparison rewrite rules when pushing
// NOTs down past a leaf comparison.
//
// IN / STARTS_WITH have no direct negation operator — the caller
// should wrap in NotPredicate.
func (c ComparisonType) Negate() (ComparisonType, bool) {
	switch c {
	case ComparisonEquals:
		return ComparisonNotEquals, true
	case ComparisonNotEquals:
		return ComparisonEquals, true
	case ComparisonLessThan:
		return ComparisonGreaterThanEq, true
	case ComparisonLessThanOrEq:
		return ComparisonGreaterThan, true
	case ComparisonGreaterThan:
		return ComparisonLessThanOrEq, true
	case ComparisonGreaterThanEq:
		return ComparisonLessThan, true
	case ComparisonIsNull:
		return ComparisonIsNotNull, true
	case ComparisonIsNotNull:
		return ComparisonIsNull, true
	case ComparisonIsDistinctFrom:
		return ComparisonNotDistinctFrom, true
	case ComparisonNotDistinctFrom:
		return ComparisonIsDistinctFrom, true
	}
	return c, false
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
	case ComparisonIn:
		return "IN"
	case ComparisonIsDistinctFrom:
		return "IS DISTINCT FROM"
	case ComparisonNotDistinctFrom:
		return "IS NOT DISTINCT FROM"
	case ComparisonLike:
		return "LIKE"
	default:
		return "?"
	}
}

// Comparison pairs a ComparisonType with a right-hand side `Value`.
// The LHS lives on the parent ComparisonPredicate; each Comparison
// carries its own (Type, RHS-Value) pair. Unary comparisons
// (IS [NOT] NULL) leave Operand nil — Eval ignores it.
//
// The RHS is a Value (not a raw literal) so non-constant RHS shapes
// — `a = b`, `a < b + 1`, `a = CAST(col AS INT)` — compose
// uniformly with the LHS. Constant-RHS callers wrap via
// NewLiteralComparison / LiteralValue. IN-list RHS is carried as a
// ConstantValue whose Value is a `[]any` of evaluated literals.
//
// Escape is the LIKE-pattern escape rune (`LIKE 'a\%b' ESCAPE '\'`).
// Zero (the default) means "no escape" — `%` and `_` retain their
// SQL wildcard meaning everywhere in the pattern. Non-zero values
// flip the next character after the escape from wildcard to
// literal. Only Type==ComparisonLike consults Escape; other types
// ignore it.
type Comparison struct {
	Type    ComparisonType
	Operand values.Value
	Escape  rune
}

// GetCorrelatedTo returns the set of correlation identifiers referenced
// by this comparison's RHS operand. Used by ordering-aware rules to
// match comparison bindings to explode aliases.
func (c Comparison) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	if c.Operand == nil {
		return nil
	}
	return values.GetCorrelatedToOfValue(c.Operand)
}

// NewLiteralComparison is the common-case constructor for a binary
// Comparison whose RHS is a plan-time literal. Wraps lit in the
// appropriate Value subtype (NullValue for nil, BooleanValue for
// bool, ConstantValue otherwise). For unary types callers should
// set Operand to nil directly: `Comparison{Type: ComparisonIsNull}`.
func NewLiteralComparison(typ ComparisonType, lit any) Comparison {
	return Comparison{Type: typ, Operand: values.LiteralValue(lit)}
}

// Eval compares left against c's (plan-time-evaluated) RHS per c's
// ComparisonType. The RHS is produced by c.Operand.Evaluate(nil) —
// i.e. RHS evaluation without a row context. Constant-RHS callers
// (the common case, and the only shape that can fold at plan time)
// get their literal back. Non-constant RHS — a FieldValue or an
// ArithmeticValue over row columns — evaluates to nil here and
// degrades to UNKNOWN, which is the right answer when no row is in
// scope. For row-aware evaluation use ComparisonPredicate.Eval,
// which evaluates both sides against the given eval context.
//
// NULL (nil) on either side returns UNKNOWN per SQL 3VL for binary
// comparators; unary (IS [NOT] NULL) and null-safe
// (IS [NOT] DISTINCT FROM) types resolve even on NULL. Numeric
// operands promote via cmpAny so mixed-width int/float pairs don't
// degrade to UNKNOWN.
func (c Comparison) Eval(left any) TriBool {
	var right any
	if c.Operand != nil && !c.Type.IsUnary() {
		right = c.Operand.Evaluate(nil)
	}
	return c.EvalAgainst(left, right)
}

// EvalAgainst is the pure dispatch: given already-evaluated LHS and
// RHS Go-natives, return the Kleene truth value. ComparisonPredicate
// evaluates both sides against the row's eval context and calls
// EvalAgainst — separating eval from dispatch is what lets a
// non-constant RHS (`a = b + 1`) work row-by-row.
func (c Comparison) EvalAgainst(left, right any) TriBool {
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
	// IS [NOT] DISTINCT FROM: SQL null-safe (in)equality — always
	// resolves to TRUE/FALSE, even with NULL on either side. Two
	// NULLs are NOT DISTINCT. One NULL + one non-NULL is DISTINCT.
	switch c.Type {
	case ComparisonIsDistinctFrom, ComparisonNotDistinctFrom:
		bothNull := left == nil && right == nil
		distinct := true
		if bothNull {
			distinct = false
		} else if left != nil && right != nil {
			cmp, ok := cmpAny(left, right)
			if ok && cmp == 0 {
				distinct = false
			}
			// Type mismatch keeps distinct=true (they're not equal).
		}
		if c.Type == ComparisonIsDistinctFrom {
			if distinct {
				return TriTrue
			}
			return TriFalse
		}
		if distinct {
			return TriFalse
		}
		return TriTrue
	}
	// IN accepts a list RHS; NULL LHS still degrades to UNKNOWN per
	// SQL 3VL. Empty list never matches. One NULL element + no other
	// match returns UNKNOWN (SQL: `x IN (1, NULL)` → UNKNOWN when
	// x != 1) — covers the common "membership-checked against
	// possibly-NULL-containing set" case.
	if c.Type == ComparisonIn {
		if left == nil {
			return TriUnknown
		}
		list, ok := right.([]any)
		if !ok {
			return TriUnknown
		}
		sawNull := false
		for _, elem := range list {
			if elem == nil {
				sawNull = true
				continue
			}
			cmp, ok := cmpAny(left, elem)
			if !ok {
				if isNumericStringMismatch(left, elem) {
					panic(&TypeMismatchError{Left: left, Right: elem})
				}
				continue
			}
			if cmp == 0 {
				return TriTrue
			}
		}
		if sawNull {
			return TriUnknown
		}
		return TriFalse
	}
	if left == nil || right == nil {
		return TriUnknown
	}
	// STARTS_WITH needs string LHS + string RHS; typed-mismatch
	// degrades to UNKNOWN per SQL 3VL like the numeric comparators.
	if c.Type == ComparisonStartsWith {
		ls, lok := left.(string)
		rs, rok := right.(string)
		if !lok || !rok {
			return TriUnknown
		}
		if strings.HasPrefix(ls, rs) {
			return TriTrue
		}
		return TriFalse
	}
	// LIKE: SQL pattern with `%` (zero-or-more chars) and `_` (exactly
	// one char). When c.Escape is non-zero, the rune preceding `%`
	// or `_` makes the next character literal (`LIKE 'a\%b' ESCAPE '\'`
	// matches `a%b`). Escape == 0 disables escape handling.
	if c.Type == ComparisonLike {
		ls, lok := left.(string)
		ps, rok := right.(string)
		if !lok || !rok {
			return TriUnknown
		}
		if likeMatch(ps, ls, c.Escape) {
			return TriTrue
		}
		return TriFalse
	}
	cmp, ok := cmpAny(left, right)
	if !ok {
		if isNumericStringMismatch(left, right) {
			panic(&TypeMismatchError{Left: left, Right: right})
		}
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

// likeMatch implements SQL LIKE pattern matching against `s`:
//   - `%` matches zero or more characters (runes)
//   - `_` matches exactly one character (rune)
//   - every other character matches itself
//
// When `escape` is non-zero, the rune in the pattern equal to
// `escape` consumes the next pattern character and matches it
// literally — e.g. with escape='\\', the pattern `'a\%b'` matches
// the 3-character string `a%b` (the `%` is literal, not a
// wildcard). Escape preceding any character — meta or not —
// produces a literal match of that next character; the escape
// itself is consumed and not part of the match. SQL standard
// leaves escape-preceding-non-meta as implementation-defined; this
// behavior is the "always-consume-next" interpretation.
// A trailing escape (escape rune at the end of the pattern, no
// following char) is treated as a malformed pattern — never
// matches. escape == 0 disables escape handling entirely (matches
// the pre-LIKE+ESCAPE behaviour).
//
// Character-level — matches SQL standard semantics (PostgreSQL /
// MySQL / Java Record Layer). Multi-byte UTF-8 runes count as one
// character.
//
// Greedy backtrack; O(|pattern| * |s|) worst case. Returns true
// iff the pattern matches the whole string (SQL LIKE is anchored
// on both ends).
//
// Delegates to values.LikeMatch — the canonical LIKE matcher
// shared between the QueryPredicate-layer ComparisonLike and the
// Value-layer LikeOperatorValue. Conformance contract: this and
// Java's `Comparisons.likeMatcher` must produce identical results.
// Pinned by FuzzLikeMatch / FuzzLikeMatchEscape.
func likeMatch(pattern, s string, escape rune) bool {
	return values.LikeMatch(pattern, s, escape)
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
		if bv, ok2 := b.(string); ok2 {
			switch {
			case av < bv:
				return -1, true
			case av > bv:
				return 1, true
			default:
				return 0, true
			}
		}
		if bt, ok2 := b.(time.Time); ok2 {
			if at, pOK := functions.ParseTimestamp(av); pOK {
				switch {
				case at.Before(bt):
					return -1, true
				case at.After(bt):
					return 1, true
				}
				return 0, true
			}
		}
		return 0, false
	}
	// Bool equality: FALSE < TRUE (following SQL's TRUE > FALSE
	// convention). Used by `x = TRUE` / `x = FALSE` from the
	// expression resolver's IS TRUE / IS FALSE desugar.
	if av, ok := a.(bool); ok {
		bv, ok2 := b.(bool)
		if !ok2 {
			return 0, false
		}
		switch {
		case av == bv:
			return 0, true
		case !av && bv: // false < true
			return -1, true
		default: // av && !bv: true > false
			return 1, true
		}
	}
	// time.Time comparison (DATE/TIMESTAMP values from CAST or CURRENT_TIMESTAMP).
	// Also handles time.Time vs string cross-type (stored dates are strings).
	if at, ok := a.(time.Time); ok {
		switch bv := b.(type) {
		case time.Time:
			switch {
			case at.Before(bv):
				return -1, true
			case at.After(bv):
				return 1, true
			}
			return 0, true
		case string:
			if bt, pOK := functions.ParseTimestamp(bv); pOK {
				switch {
				case at.Before(bt):
					return -1, true
				case at.After(bt):
					return 1, true
				}
				return 0, true
			}
		}
		return 0, false
	}
	if at, ok := b.(time.Time); ok {
		if as, ok2 := a.(string); ok2 {
			if parsed, pOK := functions.ParseTimestamp(as); pOK {
				switch {
				case parsed.Before(at):
					return -1, true
				case parsed.After(at):
					return 1, true
				}
				return 0, true
			}
		}
		return 0, false
	}
	// Bytes comparison is lexicographic — matches SQL's BINARY / VARBINARY
	// collation and proto `bytes` semantics. Mixed bytes/string degrades
	// to UNKNOWN (type mismatch) on purpose: "abc" (STRING) and
	// []byte{0x61,0x62,0x63} (BYTES) are not interchangeable in SQL.
	if av, ok := a.([]byte); ok {
		bv, ok2 := b.([]byte)
		if !ok2 {
			return 0, false
		}
		return bytes.Compare(av, bv), true
	}
	return 0, false
}

// promoteInt returns (a,b) as int64 when both are integral. Signed
// int types only — unsigned promotion needs overflow rules we'll add
// when a concrete use case calls for it.
func promoteInt(a, b any) (int64, int64, bool) {
	ai, ok := values.ToInt64(a)
	if !ok {
		return 0, 0, false
	}
	bi, ok := values.ToInt64(b)
	if !ok {
		return 0, 0, false
	}
	return ai, bi, true
}

// promoteFloat returns (a,b) as float64 when at least one side is
// floating and the other is floating or integral. Pure-integral
// pairs return ok=false so the caller prefers the exact int path.
func promoteFloat(a, b any) (float64, float64, bool) {
	af, aFloat, aNum := values.ToFloat64(a)
	if !aNum {
		return 0, 0, false
	}
	bf, bFloat, bNum := values.ToFloat64(b)
	if !bNum {
		return 0, 0, false
	}
	if !aFloat && !bFloat {
		return 0, 0, false
	}
	return af, bf, true
}

// ComparisonPredicate applies a Comparison to an operand `Value`.
// The operand is evaluated against a row (the eval context) via
// Value.Evaluate to produce the left-hand side; the comparison's
// literal is the right-hand side. Returns UNKNOWN when either side
// is NULL (SQL 3VL).
type ComparisonPredicate struct {
	Operand    values.Value
	Comparison Comparison
}

// NewComparisonPredicate builds a ComparisonPredicate.
func NewComparisonPredicate(operand values.Value, cmp Comparison) *ComparisonPredicate {
	return &ComparisonPredicate{Operand: operand, Comparison: cmp}
}

func (*ComparisonPredicate) Children() []QueryPredicate { return []QueryPredicate{} }

func (p *ComparisonPredicate) Eval(evalCtx any) TriBool {
	if p.Operand == nil {
		return TriUnknown
	}
	left := p.Operand.Evaluate(evalCtx)
	var right any
	if p.Comparison.Operand != nil && !p.Comparison.Type.IsUnary() {
		// Evaluate RHS against the same row context. For constant
		// RHS this reduces to the literal; for a FieldValue or
		// arithmetic over row columns this reads the current row.
		right = p.Comparison.Operand.Evaluate(evalCtx)
	}
	return p.Comparison.EvalAgainst(left, right)
}

func (p *ComparisonPredicate) Explain() string {
	operandText := "<unknown>"
	if p.Operand != nil {
		// Use the tree-walking ExplainValue for readable output —
		// `age` / `(a + b)` / `CAST(1 AS STRING)` instead of the
		// bare Value.Name() which returns "field" / "arith" / "cast".
		operandText = values.ExplainValue(p.Operand)
	}
	if p.Comparison.Type.IsUnary() {
		return fmt.Sprintf("%s %s", operandText, p.Comparison.Type.Symbol())
	}
	// LIKE with escape: append the ESCAPE clause so Explain output
	// round-trips back to recognisable SQL. Plain LIKE elides the
	// (default-zero) escape. The escape rune is rendered SQL-escaped
	// — single quote becomes '' inside the literal so `ESCAPE ''''`
	// stays valid SQL, not `ESCAPE '''` (broken).
	rhs := formatComparisonRHS(p.Comparison.Operand)
	if p.Comparison.Type == ComparisonLike && p.Comparison.Escape != 0 {
		escLit := string(p.Comparison.Escape)
		if p.Comparison.Escape == '\'' {
			escLit = "''"
		}
		return fmt.Sprintf("%s %s %s ESCAPE '%s'", operandText, p.Comparison.Type.Symbol(), rhs, escLit)
	}
	return fmt.Sprintf("%s %s %s", operandText, p.Comparison.Type.Symbol(), rhs)
}

// formatComparisonRHS renders the RHS of a binary comparison.
//
// Only LEAF constants (ConstantValue / NullValue / BooleanValue)
// unwrap to a Go-native literal and route through
// formatCompareOperand for the SQL-ish literal form (quoted
// strings, X'…' for bytes, paren-list for IN). Composite values —
// `CAST(5 AS INT)`, `1 + 2`, `CAST(name AS STRING)` — render via
// ExplainValue so the user-written shape survives in Explain even
// when IsConstantValue would say it's foldable. Folding happens at
// the simplifier level, not in the rendering layer.
//
// The nil case handles the IS [NOT] NULL / IS [NOT] DISTINCT FROM
// NULL shape where Operand is genuinely missing — callers only reach
// here from the binary-comparison branch so "NULL" is the right
// text for a nil RHS Value.
func formatComparisonRHS(v values.Value) string {
	if v == nil {
		return "NULL"
	}
	switch v.(type) {
	case *values.ConstantValue, *values.NullValue, *values.BooleanValue:
		if lit, ok := values.EvaluateConstant(v); ok {
			return formatCompareOperand(lit)
		}
	}
	return values.ExplainValue(v)
}

// formatCompareOperand renders a Go-native RHS literal in a
// form consistent with ExplainValue (strings quoted, []any rendered
// as a paren list for IN). Falls back to fmt.Sprintf("%v", …) for
// unfamiliar types so Explain never blows up on a surprise.
func formatCompareOperand(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case bool:
		if x {
			return "TRUE"
		}
		return "FALSE"
	case string:
		return "'" + x + "'"
	case []byte:
		// SQL hex-literal: `X'0102'` — matches BINARY/VARBINARY.
		// Mirrors cmpAny's []byte branch (added this PR) so Explain
		// is consistent with the comparator dispatch.
		const hex = "0123456789abcdef"
		buf := make([]byte, 0, 3+2*len(x))
		buf = append(buf, 'X', '\'')
		for _, b := range x {
			buf = append(buf, hex[b>>4], hex[b&0xf])
		}
		buf = append(buf, '\'')
		return string(buf)
	case []any:
		// IN-list: `(e1, e2, e3)` — same rendering style as SQL.
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = formatCompareOperand(e)
		}
		return "(" + strings.Join(parts, ", ") + ")"
	default:
		return fmt.Sprintf("%v", v)
	}
}
