// Package values is the Value-tier of the Go Cascades planner port —
// scalar / row-context expressions that compose into predicates,
// projections, and join keys. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values` package.
//
// Contents:
//
//   - Value interface (Children, Type, Name, Evaluate) + ValueType
//     enum + concrete subtypes: Constant, Field, Arithmetic, Boolean,
//     Cast, Null, Aggregate, QuantifiedObject, Promote,
//     RecordConstructor, Parameter, ScalarFunction, Not.
//   - ExplainValue — SQL-ish renderer used by plan-cache keying and
//     EXPLAIN output.
//   - SimplifyValue — standalone constant-fold over a Value tree
//     (free function; the rule-driven equivalent lives in cascades's
//     `Simplify`).
//   - LiteralValue / ToInt64 / ToFloat64 — coercion helpers
//     promoted from comparisons.go (RFC-025 Phase 1) so both values/
//     and predicates/ can call them without a layering cycle.
//   - CorrelationIdentifier + Correlated — Quantifier-tracking
//     surface used by Values to declare which upstream Quantifier
//     they depend on; rewrite rules consult this when checking
//     correlation-shape preservation.
//   - ExpressionFolder + DefaultFolder — testable seam for plan-time
//     constant folding (RFC-025 §"Closing the leaks").
//   - Phase 4.0 Type hierarchy seed (`type.go`) — the rich
//     `Type` interface + `TypeCode` enum + concrete impls
//     (`PrimitiveType`, `RecordType`, `ArrayType`, `EnumType`),
//     `TypeRepository`, `WithNullability`, `Typed` interface,
//     and `ValueRichType` bridge mapping every Value impl to its
//     rich Type. Coexists with the legacy `ValueType` enum;
//     `FromValueType` / `ToValueType` adapters bridge the two
//     while migration proceeds. Phase 4.0 follow-on lifts more
//     impls (RelationType, UuidType) and ports the conversion
//     lattice; once the file exceeds ~1500 LOC it splits into a
//     dedicated `cascades/typing/` sub-package per RFC-025.
//
// Imports: nothing else from `pkg/recordlayer/query/plan/cascades/...`.
// `predicates/`, `matching/`, and root `cascades` all import this
// package; the dependency arrow points inward to keep cycles out.
package values

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ValueType is a stand-in for the full Cascades Type hierarchy. The
// production port adds `Type` / `TypeRepository` / `Typed`, at which
// point this enum goes away. See TODO.md Phase 4.0.
type ValueType int

const (
	TypeUnknown ValueType = iota
	TypeInt
	TypeString
	TypeBool
	TypeFloat
)

// Value is the root of the Phase 4.0 seed Value hierarchy.
// Concrete Values implement Children / Type / Name / Evaluate;
// matchers downcast via type switches / type assertions on the
// concrete Go type.
//
// Java equivalent: `Value extends Correlated<Value>, TreeLike<Value>,
// Typed, ...`. The initial port keeps Children + Type + Name + a
// simple Evaluate since those are the surfaces rules touch. The
// `Correlated.GetCorrelatedTo` contract is declared separately (see
// correlation.go) and implemented by those Values that reference a
// Quantifier; leaf values opt out.
type Value interface {
	// Children returns the immediate sub-Values of this node.
	// Leaf Values return an empty slice (never nil — keeps matcher
	// code free of nil checks).
	Children() []Value
	// Type is the result type of evaluating this Value.
	Type() ValueType
	// Name is a debug string for error messages + explain output.
	// Not part of the matcher DSL.
	Name() string
	// Evaluate produces the Go-native value this Value represents
	// against an eval context. Leaf ConstantValue ignores the
	// context; FieldValue looks up its column; ArithmeticValue
	// recurses. The context is opaque (`any`) so different
	// subsystems can pass their own row shape — seed uses
	// `map[string]any` in tests.
	Evaluate(evalCtx any) any
}

// --- Concrete values ------------------------------------------------

// ConstantValue is a literal. Evaluate returns Value verbatim.
type ConstantValue struct {
	Value any
	Typ   ValueType
}

func (c *ConstantValue) Children() []Value { return []Value{} }
func (c *ConstantValue) Type() ValueType   { return c.Typ }
func (c *ConstantValue) Name() string      { return "constant" }
func (c *ConstantValue) Evaluate(any) any  { return c.Value }

// FieldValue references a column by name. Evaluate expects a
// `map[string]any` eval context and returns the field's value
// (nil if absent — SQL NULL semantics).
//
// Field-name contract: callers constructing FieldValue via the SQL
// resolver (expr.ResolveIdentifier) receive the case-folded (upper-
// case) form, matching Identifier.Name(). Downstream row producers
// MUST normalise their map keys to the same form — a row with
// lowercase keys against an UPPER-case FieldValue silently returns
// nil for every lookup. This is intentional: SQL identifier
// resolution is case-insensitive by default, so there has to be a
// single canonical casing at the evaluation boundary.
type FieldValue struct {
	Field string
	Typ   ValueType
}

func (f *FieldValue) Children() []Value { return []Value{} }
func (f *FieldValue) Type() ValueType   { return f.Typ }
func (f *FieldValue) Name() string      { return "field" }

func (f *FieldValue) Evaluate(evalCtx any) any {
	if evalCtx == nil {
		return nil
	}
	row, ok := evalCtx.(map[string]any)
	if !ok {
		return nil
	}
	return row[f.Field]
}

// WalkValue applies visit to every node in v's subtree, pre-order.
// If visit returns false, descent into that node's children is
// skipped (siblings + ancestors continue). Rule authors use this
// for tree-wide searches — e.g. "does any sub-expression reference
// this correlation?" or "does this Value tree contain an aggregate?".
//
// Safe on nil: returns immediately. Mirrors WalkPredicate over the
// Value side of the hierarchy.
func WalkValue(v Value, visit func(Value) bool) {
	if v == nil {
		return
	}
	if !visit(v) {
		return
	}
	for _, c := range v.Children() {
		WalkValue(c, visit)
	}
}

// ValueSize returns the total node count in v (v + all
// descendants). Counterpart to PredicateSize for the Value tree.
// Rule authors use this to gate expensive rewrites that would
// otherwise explode tree size.
func ValueSize(v Value) int {
	if v == nil {
		return 0
	}
	n := 1
	for _, c := range v.Children() {
		n += ValueSize(c)
	}
	return n
}

// IsConstantValue reports whether v's Evaluate is row-context-
// independent — its value is known at plan time. True for
// ConstantValue, NullValue, BooleanValue, and any composite whose
// children are all constants (`1 + 2`, `CAST(5 AS STRING)`). False
// for FieldValue / QuantifiedObjectValue / AggregateValue and any
// composite containing them.
//
// Used by rule matchers that only fire on fully-foldable operands
// (e.g. ComparisonConstantSimplifyRule's whitelist).
func IsConstantValue(v Value) bool {
	if v == nil {
		return false
	}
	switch v.(type) {
	case *ConstantValue, *NullValue, *BooleanValue:
		return true
	case *FieldValue, *QuantifiedObjectValue, *AggregateValue, *ParameterValue:
		return false
	}
	// Composite: all children must be constant.
	children := v.Children()
	if len(children) == 0 {
		// Unknown leaf — conservatively not constant.
		return false
	}
	for _, c := range children {
		if !IsConstantValue(c) {
			return false
		}
	}
	return true
}

// EvaluateConstant attempts to fold v to a concrete literal at plan
// time. Returns (literal, true) when v is constant (per
// IsConstantValue); (nil, false) otherwise. Safe on nil (returns
// (nil, false)). Useful for rules that want to pre-compute a
// constant sub-expression without writing an `if isConstant { eval
// and wrap }` dance every time.
//
// Panics during Evaluate are caught and translated to (nil, false)
// — a constant-looking tree that panics (e.g. an AggregateValue
// buried inside a Cast — IsConstantValue should exclude it, but
// defence-in-depth) is better reported as "not foldable" than
// bubbling up.
func EvaluateConstant(v Value) (out any, ok bool) {
	if v == nil || !IsConstantValue(v) {
		return nil, false
	}
	defer func() {
		if r := recover(); r != nil {
			out = nil
			ok = false
		}
	}()
	return v.Evaluate(nil), true
}

// ContainsAggregate reports whether v has any AggregateValue in its
// subtree. Common gate for rules that only apply to scalar
// expressions — aggregates need the accumulator path, not per-row
// Evaluate.
func ContainsAggregate(v Value) bool {
	found := false
	WalkValue(v, func(n Value) bool {
		if _, ok := n.(*AggregateValue); ok {
			found = true
			return false // stop descent
		}
		return true
	})
	return found
}

// ExplainValue renders a Value as a readable expression string.
// Free function rather than a Value-interface method so existing
// third-party Value impls (once the port grows) don't have to
// track another method. Walks children recursively for composite
// values like ArithmeticValue / CastValue.
//
// Output style matches SQL-ish expression rendering:
//
//	ConstantValue     → the literal as %v
//	FieldValue        → the field name
//	ArithmeticValue   → (left OP right)
//	BooleanValue      → TRUE / FALSE / NULL
//	CastValue         → CAST(child AS TypeX)
//	NullValue         → NULL
func ExplainValue(v Value) string {
	if v == nil {
		return ""
	}
	switch cv := v.(type) {
	case *ConstantValue:
		if cv.Value == nil {
			return "NULL"
		}
		if s, ok := cv.Value.(string); ok {
			return "'" + s + "'"
		}
		return valueLiteralString(cv.Value)
	case *FieldValue:
		return cv.Field
	case *ArithmeticValue:
		return "(" + ExplainValue(cv.Left) + " " + cv.Op.symbol() + " " + ExplainValue(cv.Right) + ")"
	case *BooleanValue:
		if cv.Value == nil {
			return "NULL"
		}
		if *cv.Value {
			return "TRUE"
		}
		return "FALSE"
	case *CastValue:
		return "CAST(" + ExplainValue(cv.Child) + " AS " + cv.Target.String() + ")"
	case *PromoteValue:
		return "PROMOTE(" + ExplainValue(cv.Child) + " TO " + cv.Target.String() + ")"
	case *RecordConstructorValue:
		parts := make([]string, 0, len(cv.Fields))
		for _, f := range cv.Fields {
			parts = append(parts, f.Name+": "+ExplainValue(f.Value))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case *NullValue:
		return "NULL"
	case *AggregateValue:
		if cv.Op == AggCountStar {
			return "COUNT(*)"
		}
		return cv.Op.Symbol() + "(" + ExplainValue(cv.Operand) + ")"
	case *QuantifiedObjectValue:
		return cv.Correlation.Name()
	case *ScalarFunctionValue:
		parts := make([]string, len(cv.Args))
		for i, a := range cv.Args {
			parts[i] = ExplainValue(a)
		}
		return cv.FuncName + "(" + strings.Join(parts, ", ") + ")"
	case *ParameterValue:
		// Render with the same `?` sigil the grammar accepts:
		// `?` for plain positional, `?N` once an ordinal is assigned,
		// `?name` for the lexer's NAMED_PARAMETER form. Keeps Explain
		// round-trippable to recognisable SQL.
		switch {
		case cv.Ordinal > 0:
			return "?" + intToDec(int64(cv.Ordinal))
		case cv.ParamName != "":
			return "?" + cv.ParamName
		default:
			// Unnumbered positional `?` — the per-statement ordinal
			// counter isn't wired yet, so render the surface form.
			return "?"
		}
	}
	return v.Name()
}

// String renders ValueType as a human-readable name (used by
// ExplainValue for CAST rendering).
func (t ValueType) String() string {
	switch t {
	case TypeInt:
		return "INT"
	case TypeString:
		return "STRING"
	case TypeBool:
		return "BOOL"
	case TypeFloat:
		return "FLOAT"
	}
	return "UNKNOWN"
}

// Symbol returns the SQL-text form of the arithmetic operator.
// Exposed for callers that want to render the op without going
// through ExplainValue (e.g. error messages, plan diagnostics).
// Lower-case `symbol` continues to be the package-internal alias.
func (o ArithmeticOp) Symbol() string { return o.symbol() }

func (o ArithmeticOp) symbol() string {
	switch o {
	case OpAdd:
		return "+"
	case OpSub:
		return "-"
	case OpMul:
		return "*"
	case OpDiv:
		return "/"
	case OpMod:
		return "%"
	}
	return "?"
}

func valueLiteralString(v any) string {
	switch x := v.(type) {
	case int64:
		return intToDec(x)
	case int:
		return intToDec(int64(x))
	case int32:
		return intToDec(int64(x))
	case int16:
		return intToDec(int64(x))
	case int8:
		return intToDec(int64(x))
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32)
	case bool:
		if x {
			return "TRUE"
		}
		return "FALSE"
	case string:
		return "'" + x + "'"
	case []byte:
		// SQL hex-literal form — matches formatCompareOperand, so
		// the Explain and the RHS renderer agree. Also makes
		// ExplainValue-based equality injective over byte slices:
		// `X'0102'` ≠ `X'0103'`.
		const hex = "0123456789abcdef"
		buf := make([]byte, 0, 3+2*len(x))
		buf = append(buf, 'X', '\'')
		for _, b := range x {
			buf = append(buf, hex[b>>4], hex[b&0xf])
		}
		buf = append(buf, '\'')
		return string(buf)
	case []any:
		// Paren list so different element-counts / elements render
		// differently — required for structural equality via
		// ExplainValue. Matches formatCompareOperand's IN-list form.
		parts := make([]string, len(x))
		for i, e := range x {
			if e == nil {
				parts[i] = "NULL"
				continue
			}
			if s, ok := e.(string); ok {
				parts[i] = "'" + s + "'"
				continue
			}
			parts[i] = valueLiteralString(e)
		}
		return "(" + strings.Join(parts, ", ") + ")"
	}
	return "?"
}

func intToDec(n int64) string {
	// Defer to strconv.FormatInt — the previous hand-rolled
	// implementation negated `n` before walking the digits, which
	// overflows for n == math.MinInt64 (|MinInt64| > MaxInt64) and
	// produced "-" instead of "-9223372036854775808". valueLiteralString
	// feeds into ExplainValue, and ExplainValue is the plan-cache key
	// seam — a wrong literal rendering would collide cache keys
	// across distinct queries.
	return strconv.FormatInt(n, 10)
}

// NullValue is the SQL NULL literal — evaluates to nil regardless
// of context. Not collapsed into ConstantValue{Value: nil} because
// having a dedicated type lets rule matchers check for NULL
// specifically (without also matching `Value: nil` ConstantValues
// that happen to represent a NULL literal in a non-type-annotated
// way).
type NullValue struct {
	Typ ValueType // type NULL was cast to; TypeUnknown when unconstrained
}

// NewNullValue constructs a NullValue of the given type.
func NewNullValue(typ ValueType) *NullValue {
	return &NullValue{Typ: typ}
}

func (*NullValue) Children() []Value { return []Value{} }
func (n *NullValue) Type() ValueType { return n.Typ }
func (*NullValue) Name() string      { return "null" }
func (*NullValue) Evaluate(any) any  { return nil }

// ParameterValue is a placeholder for a prepared-statement parameter
// — `?` (positional, Ordinal>=1) or `:name` (named, Ordinal=0).
// Its concrete value is unknown at plan time, so Evaluate returns
// nil unless the eval context implements ParameterBinder. Treated
// as non-constant by IsConstantValue, so constant-fold rules
// decline to fire on `x = ?` / `x = :foo`.
//
// Plan-cache keying: ExplainValue renders a parameter as `?N` /
// `:name`, which means `WHERE x = ?` and `WHERE x = ?` for two
// different bind-values share the same Explain string — the seam a
// future plan cache will key on.
//
// Seed runtime evaluation is intentionally minimal: a richer
// EvalContext that threads parameter bindings through every
// Value.Evaluate is the next step. Until then ParameterValue
// degrades to NULL at exec time, which is harmless for the
// plan-time / explain-time work this type unblocks.
type ParameterValue struct {
	Ordinal   int       // 1-based positional index; 0 ⇒ named parameter
	ParamName string    // populated when Ordinal == 0
	Typ       ValueType // TypeUnknown until upstream type inference fills it
}

// NewParameterValue constructs a positional `?` parameter (1-based).
func NewParameterValue(ordinal int) *ParameterValue {
	return &ParameterValue{Ordinal: ordinal, Typ: TypeUnknown}
}

// NewNamedParameterValue constructs a named `:name` parameter.
func NewNamedParameterValue(name string) *ParameterValue {
	return &ParameterValue{ParamName: name, Typ: TypeUnknown}
}

// ParameterBinder is an optional eval-context capability: when
// ParameterValue.Evaluate is called with a context that implements
// this interface, the parameter is resolved to its bound value.
// Otherwise Evaluate returns nil (SQL UNKNOWN), which is the safe
// default for plan-time evaluation where no bindings exist.
type ParameterBinder interface {
	BindParameter(ordinal int, name string) (any, bool)
}

func (*ParameterValue) Children() []Value { return []Value{} }
func (p *ParameterValue) Type() ValueType { return p.Typ }
func (*ParameterValue) Name() string      { return "param" }

func (p *ParameterValue) Evaluate(evalCtx any) any {
	if evalCtx == nil {
		return nil
	}
	if b, ok := evalCtx.(ParameterBinder); ok {
		v, _ := b.BindParameter(p.Ordinal, p.ParamName)
		return v
	}
	return nil
}

// ScalarFunctionValue is a row-scalar function call — `UPPER(name)`,
// `LENGTH(str)`, etc. Args carries the evaluated sub-Values; Name is
// the canonical (UPPER-CASE) function identifier as it appears in the
// catalog. Children returns Args so IsConstantValue / WalkValue
// recurse normally — `UPPER('foo')` is a constant composite and folds
// via EvaluateConstant; `UPPER(name)` is non-constant because the
// FieldValue arg is non-constant.
//
// Seed function set is intentionally narrow: UPPER, LOWER,
// LENGTH/CHAR_LENGTH/CHARACTER_LENGTH (utf8 rune count),
// OCTET_LENGTH (byte count). The full function catalog port is a
// Phase 4.0 follow-up; the seam lives in evalScalarFunction so the
// production registry can replace this switch without touching the
// Value contract.
type ScalarFunctionValue struct {
	FuncName string
	Args     []Value
	Typ      ValueType
}

// NewScalarFunctionValue builds a ScalarFunctionValue. The function
// name is upper-cased so callers can pass case-insensitive identifiers.
func NewScalarFunctionValue(name string, typ ValueType, args ...Value) *ScalarFunctionValue {
	return &ScalarFunctionValue{FuncName: strings.ToUpper(name), Args: args, Typ: typ}
}

func (s *ScalarFunctionValue) Children() []Value {
	if len(s.Args) == 0 {
		return []Value{}
	}
	return s.Args
}
func (s *ScalarFunctionValue) Type() ValueType { return s.Typ }
func (*ScalarFunctionValue) Name() string      { return "scalarfn" }

func (s *ScalarFunctionValue) Evaluate(evalCtx any) any {
	args := make([]any, len(s.Args))
	for i, a := range s.Args {
		if a == nil {
			return nil
		}
		args[i] = a.Evaluate(evalCtx)
	}
	return evalScalarFunction(s.FuncName, args)
}

// evalScalarFunction dispatches the seed scalar function set. NULL
// argument propagates to NULL result (SQL standard). Unknown function,
// wrong arity, or wrong arg type returns nil — the seed errs on the
// side of declining rather than erroring, so the embedded executor's
// richer scalar_functions.go path remains the primary surface for now.
func evalScalarFunction(name string, args []any) any {
	switch name {
	case "UPPER":
		if len(args) != 1 || args[0] == nil {
			return nil
		}
		s, ok := args[0].(string)
		if !ok {
			return nil
		}
		return strings.ToUpper(s)
	case "LOWER":
		if len(args) != 1 || args[0] == nil {
			return nil
		}
		s, ok := args[0].(string)
		if !ok {
			return nil
		}
		return strings.ToLower(s)
	case "LENGTH", "LEN", "CHAR_LENGTH", "CHARACTER_LENGTH":
		// Rune count — matches embedded.scalar_functions.go's LENGTH
		// (utf8.RuneCountInString) so plan-time fold and runtime eval
		// agree. The seed coerces []byte the same way for symmetry
		// with OCTET_LENGTH (byte count there, rune count here).
		if len(args) != 1 || args[0] == nil {
			return nil
		}
		switch v := args[0].(type) {
		case string:
			return int64(utf8.RuneCountInString(v))
		case []byte:
			return int64(utf8.RuneCount(v))
		}
		return nil
	case "OCTET_LENGTH":
		if len(args) != 1 || args[0] == nil {
			return nil
		}
		switch v := args[0].(type) {
		case string:
			return int64(len(v))
		case []byte:
			return int64(len(v))
		}
		return nil
	case "ABS":
		if len(args) != 1 || args[0] == nil {
			return nil
		}
		switch n := args[0].(type) {
		case int64:
			// MinInt64 abs overflows; embedded errors and we can't
			// surface that from a fold path — decline (return nil)
			// so the runtime evaluator handles it and reports the
			// 22003 NUMERIC_VALUE_OUT_OF_RANGE.
			if n == math.MinInt64 {
				return nil
			}
			if n < 0 {
				return -n
			}
			return n
		case float64:
			return math.Abs(n)
		}
		return nil
	case "FLOOR", "CEIL", "CEILING", "ROUND":
		if len(args) < 1 || args[0] == nil {
			return nil
		}
		var f float64
		switch n := args[0].(type) {
		case int64:
			// Already an integer — short-circuit to mirror embedded.
			return n
		case float64:
			f = n
		default:
			return nil
		}
		var result float64
		switch name {
		case "FLOOR":
			result = math.Floor(f)
		case "CEIL", "CEILING":
			result = math.Ceil(f)
		case "ROUND":
			decimals := int64(0)
			if len(args) >= 2 {
				if args[1] == nil {
					return nil
				}
				d, ok := scalarFnInt64Arg(args[1])
				if !ok {
					return nil
				}
				decimals = d
			}
			if decimals == 0 {
				result = math.Round(f)
			} else {
				factor := math.Pow(10, float64(decimals))
				result = math.Round(f*factor) / factor
			}
		}
		// Match embedded's "return int64 if no fractional part" rule.
		if result == math.Trunc(result) && result >= math.MinInt64 && result <= math.MaxInt64 {
			return int64(result)
		}
		return result
	case "PI":
		// Zero-arg constant. Mirrors embedded.scalar_functions.go's PI.
		if len(args) != 0 {
			return nil
		}
		return math.Pi
	case "SQRT":
		if len(args) != 1 || args[0] == nil {
			return nil
		}
		f, _, ok := ToFloat64(args[0])
		if !ok {
			return nil
		}
		if f < 0 {
			return nil
		}
		return math.Sqrt(f)
	case "POWER", "POW":
		if len(args) != 2 || args[0] == nil || args[1] == nil {
			return nil
		}
		base, _, bok := ToFloat64(args[0])
		exp, _, eok := ToFloat64(args[1])
		if !bok || !eok {
			return nil
		}
		result := math.Pow(base, exp)
		if math.IsNaN(result) || math.IsInf(result, 0) {
			return nil
		}
		if result == math.Trunc(result) && result >= math.MinInt64 && result <= math.MaxInt64 {
			return int64(result)
		}
		return result
	case "COALESCE":
		// First non-nil argument wins; all nil → nil. Empty argument
		// list also folds to nil so a degenerate `COALESCE()` doesn't
		// error at plan time (the parser rejects zero-arg COALESCE
		// anyway, so this is just a defensive default).
		for _, a := range args {
			if a != nil {
				return a
			}
		}
		return nil
	case "NULLIF":
		// NULLIF(a, b) → NULL when a == b; otherwise a. Compare via
		// nullifEqual so int/float promotion mirrors embedded.
		if len(args) != 2 {
			return nil
		}
		if args[0] == nil {
			return nil
		}
		if args[1] != nil && nullifEqual(args[0], args[1]) {
			return nil
		}
		return args[0]
	case "TRIM":
		if len(args) != 1 || args[0] == nil {
			return nil
		}
		s, ok := args[0].(string)
		if !ok {
			return nil
		}
		return strings.TrimSpace(s)
	case "LTRIM":
		if len(args) != 1 || args[0] == nil {
			return nil
		}
		s, ok := args[0].(string)
		if !ok {
			return nil
		}
		return strings.TrimLeft(s, " \t\n\r")
	case "RTRIM":
		if len(args) != 1 || args[0] == nil {
			return nil
		}
		s, ok := args[0].(string)
		if !ok {
			return nil
		}
		return strings.TrimRight(s, " \t\n\r")
	case "CONCAT":
		// MySQL/Postgres semantics — NULL skips, doesn't poison.
		// Pinned by trim_concat.yaml; the embedded path uses the
		// same rule.
		var b strings.Builder
		for _, a := range args {
			if a == nil {
				continue
			}
			b.WriteString(fmt.Sprintf("%v", a))
		}
		return b.String()
	case "CONCAT_WS":
		// CONCAT With Separator — MySQL semantics: first arg is the
		// separator (NULL → result is NULL); remaining args are
		// concatenated with the separator between non-NULL values.
		// NULL elements are skipped (different from CONCAT in
		// Postgres, which poisons; matches embedded.scalar_functions.go).
		if len(args) < 1 || args[0] == nil {
			return nil
		}
		sep, ok := args[0].(string)
		if !ok {
			return nil
		}
		var b strings.Builder
		first := true
		for _, a := range args[1:] {
			if a == nil {
				continue
			}
			if !first {
				b.WriteString(sep)
			}
			b.WriteString(fmt.Sprintf("%v", a))
			first = false
		}
		return b.String()
	case "SUBSTRING", "SUBSTR":
		// SUBSTRING(s, pos[, len]) — 1-based position per SQL standard.
		// pos < 1 normalises to 1 (matches embedded, MySQL).
		if len(args) < 2 || args[0] == nil || args[1] == nil {
			return nil
		}
		s := fmt.Sprintf("%v", args[0])
		pos, ok := scalarFnInt64Arg(args[1])
		if !ok {
			return nil
		}
		if pos < 1 {
			pos = 1
		}
		runes := []rune(s)
		start := int(pos) - 1
		if start >= len(runes) {
			return ""
		}
		if len(args) >= 3 {
			if args[2] == nil {
				return nil
			}
			n, ok := scalarFnInt64Arg(args[2])
			if !ok {
				return nil
			}
			end := start + int(n)
			if end > len(runes) {
				end = len(runes)
			}
			if end < start {
				return ""
			}
			return string(runes[start:end])
		}
		return string(runes[start:])
	case "REPLACE":
		// REPLACE(s, from, to). NULL `to` is treated as empty (matches
		// embedded). Pure-string semantics — non-string args coerce
		// via fmt.Sprintf("%v", v) for parity with the embedded path.
		if len(args) != 3 || args[0] == nil || args[1] == nil {
			return nil
		}
		toStr := ""
		if args[2] != nil {
			toStr = fmt.Sprintf("%v", args[2])
		}
		return strings.ReplaceAll(fmt.Sprintf("%v", args[0]), fmt.Sprintf("%v", args[1]), toStr)
	case "SIGN":
		// SIGN(numeric) — -1 / 0 / 1 in the input's numeric type. Mirrors
		// embedded.scalar_functions.go's SIGN: int64 input → int64 sign,
		// float64 input → float64 sign. Non-numeric input declines so
		// the runtime evaluator surfaces 22018.
		if len(args) != 1 || args[0] == nil {
			return nil
		}
		switch n := args[0].(type) {
		case int64:
			switch {
			case n > 0:
				return int64(1)
			case n < 0:
				return int64(-1)
			}
			return int64(0)
		case float64:
			switch {
			case n > 0:
				return float64(1)
			case n < 0:
				return float64(-1)
			}
			return float64(0)
		}
		return nil
	case "MOD":
		// MOD(a, b) — int64%int64 stays int64, mixed promotes to float64
		// via math.Mod. Division-by-zero declines (runtime errors with
		// 22012 DIVISION_BY_ZERO). Mirrors embedded's MOD semantics.
		if len(args) != 2 || args[0] == nil || args[1] == nil {
			return nil
		}
		ai, aIsInt := args[0].(int64)
		bi, bIsInt := args[1].(int64)
		if aIsInt && bIsInt {
			if bi == 0 {
				return nil
			}
			return ai % bi
		}
		af, _, aok := ToFloat64(args[0])
		bf, _, bok := ToFloat64(args[1])
		if !aok || !bok {
			return nil
		}
		if bf == 0 {
			return nil
		}
		return math.Mod(af, bf)
	case "IFNULL":
		// IFNULL(a, b) — `a` if non-null, else `b`. 2-arg COALESCE alias
		// (MySQL/SQLite spelling). Type-uniform like embedded.
		if len(args) != 2 {
			return nil
		}
		if args[0] != nil {
			return args[0]
		}
		return args[1]
	case "IF", "IIF":
		// IF(cond, then, else) — evaluates condition first; returns
		// `then` if truthy, `else` otherwise. Truthy: non-zero numeric,
		// non-empty string, true bool. Mirrors embedded's IF.
		if len(args) != 3 {
			return nil
		}
		switch v := args[0].(type) {
		case bool:
			if v {
				return args[1]
			}
			return args[2]
		case int64:
			if v != 0 {
				return args[1]
			}
			return args[2]
		case float64:
			if v != 0 {
				return args[1]
			}
			return args[2]
		case string:
			if v != "" {
				return args[1]
			}
			return args[2]
		case nil:
			// SQL §6.30: IF(NULL, …) returns the else branch (NULL is
			// not truthy). embedded matches this.
			return args[2]
		}
		// Unsupported condition type — decline so runtime can error.
		return nil
	case "GREATEST", "LEAST":
		// GREATEST/LEAST — Java conformance: any NULL arg → NULL result
		// (Postgres skips, Oracle propagates; Java propagates). Mirror
		// Java per embedded's behaviour. Cross-type comparisons decline
		// at the fold path so the runtime can surface 22000
		// CANNOT_CONVERT_TYPE.
		if len(args) == 0 {
			return nil
		}
		isGreatest := name == "GREATEST"
		best := args[0]
		if best == nil {
			return nil
		}
		for _, a := range args[1:] {
			if a == nil {
				return nil
			}
			cmp, ok := compareScalar(best, a)
			if !ok {
				return nil // cross-type — runtime reports the error
			}
			if (isGreatest && cmp < 0) || (!isGreatest && cmp > 0) {
				best = a
			}
		}
		return best
	case "EXP":
		if len(args) != 1 || args[0] == nil {
			return nil
		}
		f, _, ok := ToFloat64(args[0])
		if !ok {
			return nil
		}
		return math.Exp(f)
	case "LN":
		// Natural log. Domain: x > 0. Out-of-domain (≤ 0) declines so
		// the runtime evaluator can surface 22003 NUMERIC_VALUE_OUT_OF_RANGE.
		if len(args) != 1 || args[0] == nil {
			return nil
		}
		f, _, ok := ToFloat64(args[0])
		if !ok || f <= 0 {
			return nil
		}
		return math.Log(f)
	case "LOG":
		// 1-arg LOG(x) = log10(x). 2-arg LOG(base, x) = ln(x)/ln(base).
		// Mirrors embedded; out-of-domain declines.
		switch len(args) {
		case 1:
			if args[0] == nil {
				return nil
			}
			f, _, ok := ToFloat64(args[0])
			if !ok || f <= 0 {
				return nil
			}
			return math.Log10(f)
		case 2:
			if args[0] == nil || args[1] == nil {
				return nil
			}
			base, _, baseOK := ToFloat64(args[0])
			x, _, xOK := ToFloat64(args[1])
			if !baseOK || !xOK || base <= 0 || base == 1 || x <= 0 {
				return nil
			}
			return math.Log(x) / math.Log(base)
		}
		return nil
	case "REVERSE":
		// String reverse — rune-aware so multibyte UTF-8 stays valid.
		if len(args) != 1 || args[0] == nil {
			return nil
		}
		s := fmt.Sprintf("%v", args[0])
		runes := []rune(s)
		for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
			runes[i], runes[j] = runes[j], runes[i]
		}
		return string(runes)
	case "POSITION":
		// POSITION(substr, str) — 1-based rune index of first match,
		// 0 if not found. Mirrors embedded POSITION (note: not the
		// `POSITION(substr IN str)` SQL-standard grammar shape).
		if len(args) != 2 || args[0] == nil || args[1] == nil {
			return nil
		}
		needle := fmt.Sprintf("%v", args[0])
		haystack := fmt.Sprintf("%v", args[1])
		byteIdx := strings.Index(haystack, needle)
		if byteIdx < 0 {
			return int64(0)
		}
		return int64(utf8.RuneCountInString(haystack[:byteIdx]) + 1)
	case "LEFT":
		// LEFT(str, n) — first n runes; whole string if n ≥ length.
		if len(args) != 2 || args[0] == nil || args[1] == nil {
			return nil
		}
		s := fmt.Sprintf("%v", args[0])
		n, ok := scalarFnInt64Arg(args[1])
		if !ok {
			return nil
		}
		if n < 0 {
			n = 0
		}
		runes := []rune(s)
		if int(n) >= len(runes) {
			return s
		}
		return string(runes[:n])
	case "RIGHT":
		// RIGHT(str, n) — last n runes; whole string if n ≥ length.
		if len(args) != 2 || args[0] == nil || args[1] == nil {
			return nil
		}
		s := fmt.Sprintf("%v", args[0])
		n, ok := scalarFnInt64Arg(args[1])
		if !ok {
			return nil
		}
		if n < 0 {
			n = 0
		}
		runes := []rune(s)
		if int(n) >= len(runes) {
			return s
		}
		return string(runes[len(runes)-int(n):])
	}
	return nil
}

// compareScalar returns -1 / 0 / 1 for a < b / a == b / a > b under the
// seed's numeric/string/bool comparison rules. Returns ok=false on
// cross-type pairs the seed can't compare (the runtime reports the
// CANNOT_CONVERT_TYPE error per Java alignment).
func compareScalar(a, b any) (int, bool) {
	switch av := a.(type) {
	case int64:
		switch bv := b.(type) {
		case int64:
			switch {
			case av < bv:
				return -1, true
			case av > bv:
				return 1, true
			}
			return 0, true
		case float64:
			af := float64(av)
			switch {
			case af < bv:
				return -1, true
			case af > bv:
				return 1, true
			}
			return 0, true
		}
	case float64:
		switch bv := b.(type) {
		case int64:
			bf := float64(bv)
			switch {
			case av < bf:
				return -1, true
			case av > bf:
				return 1, true
			}
			return 0, true
		case float64:
			switch {
			case av < bv:
				return -1, true
			case av > bv:
				return 1, true
			}
			return 0, true
		}
	case string:
		bv, ok := b.(string)
		if !ok {
			return 0, false
		}
		return strings.Compare(av, bv), true
	case bool:
		bv, ok := b.(bool)
		if !ok {
			return 0, false
		}
		switch {
		case !av && bv:
			return -1, true
		case av && !bv:
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// scalarFnInt64Arg coerces a numeric scalar-fn argument to int64.
// Float coercion only succeeds for whole-valued floats — non-integer
// floats decline so the fold path returns nil and the runtime
// evaluator (which can surface 22018 INVALID_CHARACTER_VALUE) handles
// the conversion error. Mirrors the strictness of
// embedded.functions.ToIntegerArg.
func scalarFnInt64Arg(v any) (int64, bool) {
	if i, ok := ToInt64(v); ok {
		return i, true
	}
	if f, _, ok := ToFloat64(v); ok && f == math.Trunc(f) &&
		f >= math.MinInt64 && f <= math.MaxInt64 {
		return int64(f), true
	}
	return 0, false
}

// nullifEqual is the equality test used by NULLIF's plan-time fold.
// Mirrors embedded.functions.CompareValues for the int/float promotion
// case while staying conservative (declines on mixed-type comparisons
// the seed Type hierarchy can't model).
func nullifEqual(a, b any) bool {
	switch av := a.(type) {
	case int64:
		switch bv := b.(type) {
		case int64:
			return av == bv
		case float64:
			return float64(av) == bv
		}
	case float64:
		switch bv := b.(type) {
		case int64:
			return av == float64(bv)
		case float64:
			return av == bv
		}
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	}
	return false
}

// ArithmeticOp is a subset of SQL arithmetic — enough to build a
// non-trivial matcher.
type ArithmeticOp int

const (
	OpAdd ArithmeticOp = iota
	OpSub
	OpMul
	OpDiv
	OpMod
)

// ArithmeticValue is a binary arithmetic over two child Values.
// Evaluate recurses left + right, coerces to int64, and applies
// the op. NULL on either side propagates (SQL semantics). Division
// by zero returns nil (UNKNOWN).
type ArithmeticValue struct {
	Op    ArithmeticOp
	Left  Value
	Right Value
	// Result type: int for the seed impls; full type inference lands
}

func (a *ArithmeticValue) Children() []Value { return []Value{a.Left, a.Right} }
func (a *ArithmeticValue) Type() ValueType   { return TypeInt }
func (a *ArithmeticValue) Name() string      { return "arith" }

func (a *ArithmeticValue) Evaluate(evalCtx any) any {
	l := a.Left.Evaluate(evalCtx)
	r := a.Right.Evaluate(evalCtx)
	if l == nil || r == nil {
		return nil
	}
	li, lok := l.(int64)
	ri, rok := r.(int64)
	if !lok || !rok {
		return nil
	}
	switch a.Op {
	case OpAdd:
		// Overflow-checked: matches Java ArithmeticValue.AddFn /
		// embedded.functions.AddInt64Checked. Cascades returns nil
		// (UNKNOWN) on overflow so the runtime executor surfaces
		// the 22003 NUMERIC_VALUE_OUT_OF_RANGE error rather than the
		// fold silently producing a wrapped value.
		out, ok := addInt64Checked(li, ri)
		if !ok {
			return nil
		}
		return out
	case OpSub:
		out, ok := subInt64Checked(li, ri)
		if !ok {
			return nil
		}
		return out
	case OpMul:
		out, ok := mulInt64Checked(li, ri)
		if !ok {
			return nil
		}
		return out
	case OpDiv:
		if ri == 0 {
			return nil
		}
		// MinInt64 / -1 overflows (abs value doesn't fit in int64).
		if li == math.MinInt64 && ri == -1 {
			return nil
		}
		return li / ri
	case OpMod:
		// SQL: `a MOD 0` is undefined / NULL. Match Div's nil-on-zero
		// guard. Sign of result matches Go's `%` (truncated toward
		// zero) — matches MySQL / PostgreSQL semantics.
		//
		// MinInt64 % -1 is SAFE — unlike division, Go's `%` produces
		// 0 for this combination (the mathematical result is 0,
		// representable in int64). No special-case overflow guard
		// needed. Pinned in TestArithmeticValue_OverflowBoundaries.
		if ri == 0 {
			return nil
		}
		return li % ri
	}
	return nil
}

// addInt64Checked / subInt64Checked / mulInt64Checked mirror
// embedded.functions.{Add,Sub,Mul}Int64Checked. Re-implemented in
// cascades to keep the value-layer arithmetic free of cross-package
// imports (the package-structure goal in RFC-025).
//
// Add/Sub overflow: signed-overflow detection via the standard
// "different sign" check (well-defined under int64 wrap semantics).
// Mul: defer to math/bits to avoid the full multiword arithmetic
// inline.
func addInt64Checked(a, b int64) (int64, bool) {
	r := a + b
	if (a > 0 && b > 0 && r < a) || (a < 0 && b < 0 && r > a) {
		return 0, false
	}
	return r, true
}

func subInt64Checked(a, b int64) (int64, bool) {
	r := a - b
	if (b > 0 && r > a) || (b < 0 && r < a) {
		return 0, false
	}
	return r, true
}

func mulInt64Checked(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	r := a * b
	// Reverse-divide to detect overflow. The MinInt64 * -1 case is
	// the one a/b == 1 wouldn't catch — handle explicitly.
	if a == math.MinInt64 && b == -1 || b == math.MinInt64 && a == -1 {
		return 0, false
	}
	if r/b != a {
		return 0, false
	}
	return r, true
}

// --- BooleanValue + CastValue -------------------------------------

// BooleanValue is a literal true / false (and NULL when Value is
// nil — SQL UNKNOWN at the Value layer).
type BooleanValue struct {
	Value *bool // nil = UNKNOWN
}

// NewBooleanValue wraps a Go bool.
func NewBooleanValue(v bool) *BooleanValue {
	b := v
	return &BooleanValue{Value: &b}
}

func (*BooleanValue) Children() []Value { return []Value{} }
func (*BooleanValue) Type() ValueType   { return TypeBool }
func (*BooleanValue) Name() string      { return "bool" }

func (b *BooleanValue) Evaluate(any) any {
	if b.Value == nil {
		return nil
	}
	return *b.Value
}

// CastValue converts a child Value's result to a target ValueType.
// Seed handles the trivial conversions our existing corpus needs:
// int ↔ string (via strconv-free formatting for the seed), bool ↔
// int (false=0, true=1). Unknown conversions return nil (UNKNOWN).
// Full type tower lands with the Type hierarchy.
type CastValue struct {
	Child  Value
	Target ValueType
}

// NewCastValue constructs a CastValue.
func NewCastValue(child Value, target ValueType) *CastValue {
	return &CastValue{Child: child, Target: target}
}

func (c *CastValue) Children() []Value { return []Value{c.Child} }
func (c *CastValue) Type() ValueType   { return c.Target }
func (c *CastValue) Name() string      { return "cast" }

func (c *CastValue) Evaluate(evalCtx any) any {
	v := c.Child.Evaluate(evalCtx)
	if v == nil {
		return nil
	}
	switch c.Target {
	case TypeInt:
		switch val := v.(type) {
		case int64:
			return val
		case bool:
			if val {
				return int64(1)
			}
			return int64(0)
		case float64:
			// SQL CAST AS INT truncates toward zero. NaN / ±Inf
			// fall through to nil (UNKNOWN-at-Value-layer).
			if val != val || val > 1<<62 || val < -(1<<62) {
				return nil
			}
			return int64(val)
		}
	case TypeBool:
		switch val := v.(type) {
		case bool:
			return val
		case int64:
			return val != 0
		case float64:
			return val != 0
		}
	case TypeString:
		if s, ok := v.(string); ok {
			return s
		}
		if i, ok := v.(int64); ok {
			// strconv.FormatInt handles signed values correctly —
			// uitoa(uint64(i)) would reinterpret negative int64 as
			// the corresponding huge positive number (CAST(-5 AS
			// STRING) → "18446744073709551611").
			return strconv.FormatInt(i, 10)
		}
		if f, ok := v.(float64); ok {
			return strconv.FormatFloat(f, 'g', -1, 64)
		}
		if b, ok := v.(bool); ok {
			// Match runtime functions.CastValue: lowercase
			// "true"/"false" (Java's CastValue.BOOLEAN_TO_STRING).
			// Without this arm, fold-time `CAST(TRUE AS STRING)`
			// returned nil while the runtime returned "true" — fold
			// vs runtime mismatch on a constant input.
			if b {
				return "true"
			}
			return "false"
		}
	case TypeFloat:
		// CAST … AS FLOAT — accept float64/float32 verbatim; promote
		// integral types to float64. Without this case, the walker's
		// shiny new CastValue{TypeFloat} path silently returns nil
		// from Evaluate and constant-fold of `CAST(5 AS FLOAT) = 3.14`
		// gets UNKNOWN instead of FALSE.
		switch val := v.(type) {
		case float64:
			return val
		case float32:
			return float64(val)
		case int64:
			return float64(val)
		case bool:
			// Java doesn't define CAST(BOOLEAN AS FLOAT) directly —
			// but the runtime path goes via CAST(b AS INT) AS FLOAT,
			// folding to 1.0/0.0. Mirror that one-step here so a
			// fold-time literal `CAST(TRUE AS FLOAT)` resolves cleanly.
			if val {
				return float64(1)
			}
			return float64(0)
		}
	}
	return nil
}

// --- RecordConstructorValue ----------------------------------------

// RecordConstructorField pairs a field name with the Value that
// computes its contents. Named so the output has a struct shape
// downstream consumers (projections, aggregations) can address by
// name.
type RecordConstructorField struct {
	Name  string
	Value Value
}

// RecordConstructorValue constructs a record (struct) from named
// children. Used by the analyzer for SELECT projection output
// (`SELECT a, b+1 AS c` → Record{a: a, c: b+1}) and anywhere a
// tuple-of-values is needed (ORDER BY key groups, aggregate keys).
//
// Mirrors Java's `RecordConstructorValue`.
type RecordConstructorValue struct {
	Fields []RecordConstructorField
}

// NewRecordConstructorValue constructs a RecordConstructorValue.
// Panics on duplicate field names — callers should rename via AS
// before constructing.
func NewRecordConstructorValue(fields ...RecordConstructorField) *RecordConstructorValue {
	seen := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		if _, dup := seen[f.Name]; dup {
			panic("NewRecordConstructorValue: duplicate field name: " + f.Name)
		}
		seen[f.Name] = struct{}{}
	}
	// Defensive copy so the caller can't mutate.
	out := make([]RecordConstructorField, len(fields))
	copy(out, fields)
	return &RecordConstructorValue{Fields: out}
}

// Children returns each field's Value as a flat list, in field
// declaration order. Lets WalkValue traverse the whole tree.
func (r *RecordConstructorValue) Children() []Value {
	out := make([]Value, len(r.Fields))
	for i, f := range r.Fields {
		out[i] = f.Value
	}
	return out
}

// Type returns TypeUnknown — record type is a struct shape which
// the seed's flat ValueType enum can't express. The Type hierarchy
// port replaces this with a real StructType.
func (*RecordConstructorValue) Type() ValueType { return TypeUnknown }

// Name returns the debug-print kind.
func (*RecordConstructorValue) Name() string { return "record" }

// Evaluate produces a map[string]any with each field evaluated.
// Downstream consumers (projections, field-access) index into this
// map by field name.
func (r *RecordConstructorValue) Evaluate(evalCtx any) any {
	out := make(map[string]any, len(r.Fields))
	for _, f := range r.Fields {
		out[f.Name] = f.Value.Evaluate(evalCtx)
	}
	return out
}

// --- PromoteValue --------------------------------------------------

// PromoteValue wraps a child Value to coerce it to a target SQL type
// when the analyzer inserts an implicit conversion. E.g.
// `int_col = 5.0` rewrites to `PromoteValue(int_col, FLOAT) = 5.0`
// so the comparison sees two FLOATs.
//
// Distinct from CastValue: Cast is an explicit `CAST(x AS T)` that
// the user wrote; Promote is machine-inserted and cost-modelled
// separately. Mirrors Java's `PromoteValue`.
//
// Seed Evaluate currently delegates to Child.Evaluate — the seed's
// cmpAny already promotes numerics at runtime, so an explicit
// Promote in the tree is a no-op evaluation-wise. The value is in
// having the coercion visible at plan time so rule matchers can
// simplify `Promote(x, x.Type)` → `x`.
type PromoteValue struct {
	Child  Value
	Target ValueType
}

// NewPromoteValue constructs a PromoteValue. Rejects nil child and
// zero-value Target — both are programmer errors.
func NewPromoteValue(child Value, target ValueType) *PromoteValue {
	if child == nil {
		panic("NewPromoteValue: child is nil")
	}
	if target == TypeUnknown {
		panic("NewPromoteValue: target is TypeUnknown; use CastValue if target is genuinely unknown")
	}
	return &PromoteValue{Child: child, Target: target}
}

// Children returns the single child as a one-element slice.
func (p *PromoteValue) Children() []Value { return []Value{p.Child} }

// Type returns the promotion target.
func (p *PromoteValue) Type() ValueType { return p.Target }

// Name returns the debug-print kind.
func (*PromoteValue) Name() string { return "promote" }

// Evaluate delegates to the child — the seed treats Promote as a
// no-op at runtime since cmpAny already handles cross-width
// promotion. Plan-time inspection (explain, rewrite rules) is where
// Promote earns its keep.
func (p *PromoteValue) Evaluate(evalCtx any) any {
	return p.Child.Evaluate(evalCtx)
}

// --- QuantifiedObjectValue -----------------------------------------

// QuantifiedObjectValue represents "the current row of the
// quantifier identified by Correlation". Emitted by the analyzer
// for references like `t` in `SELECT t.col FROM tbl AS t` — the
// parent expression (`t.col`) then projects a FieldValue with
// operand = QuantifiedObjectValue{Correlation: t}.
//
// Mirrors Java's `QuantifiedObjectValue`. The seed Evaluate reads
// the row directly out of the eval context when it's a
// `map[CorrelationIdentifier]map[string]any` (the multi-source
// shape); for the single-source `map[string]any` shape it returns
// the map verbatim so downstream FieldValue lookups can index into
// it.
type QuantifiedObjectValue struct {
	Correlation CorrelationIdentifier
	// Typ is the row type (struct shape) this quantifier produces.
	// Seed keeps it as TypeUnknown until the Type hierarchy port
	// lands — the test surface doesn't need real struct types yet.
	Typ ValueType
}

// NewQuantifiedObjectValue constructs a QuantifiedObjectValue. Zero
// correlation is rejected — a quantifier without an identifier is a
// design error, not something the analyzer should allow.
func NewQuantifiedObjectValue(corr CorrelationIdentifier) *QuantifiedObjectValue {
	if corr.IsZero() {
		panic("NewQuantifiedObjectValue: correlation is zero-value; use NamedCorrelationIdentifier or UniqueCorrelationIdentifier")
	}
	return &QuantifiedObjectValue{Correlation: corr, Typ: TypeUnknown}
}

// Children returns an empty slice — the quantifier is a leaf in
// the Value tree, with its correlation link being external metadata
// (not a child Value).
func (*QuantifiedObjectValue) Children() []Value { return []Value{} }

// Type returns the seed's placeholder TypeUnknown for the row type.
func (q *QuantifiedObjectValue) Type() ValueType { return q.Typ }

// Name returns the debug-print kind.
func (*QuantifiedObjectValue) Name() string { return "quantifier" }

// Evaluate extracts the row bound to this quantifier's correlation.
// Eval context shapes this impl handles:
//
//   - map[CorrelationIdentifier]map[string]any — multi-source shape,
//     returns the nested map for this correlation (nil if missing).
//   - map[string]any — single-source compat shim: IGNORES q.Correlation
//     and returns the whole map. Safe only when there's one
//     QuantifiedObjectValue in play; multi-source callers MUST use
//     the per-correlation shape or two quantifiers with different
//     correlations silently evaluate to the same row.
//   - anything else — nil.
//
// The single-source shim exists so existing single-table tests /
// callers that feed a bare row map keep working while the eval
// path migrates. New callers MUST NOT rely on it — thread the
// per-correlation shape end-to-end. The shim is scheduled for
// removal once no caller needs it.
//
// Downstream FieldValue / nested-field resolvers then index into the
// returned map to pick a specific column.
func (q *QuantifiedObjectValue) Evaluate(evalCtx any) any {
	if evalCtx == nil {
		return nil
	}
	switch ctx := evalCtx.(type) {
	case map[CorrelationIdentifier]map[string]any:
		return ctx[q.Correlation]
	case map[string]any:
		return ctx
	}
	return nil
}

// GetCorrelatedTo implements the Correlated interface — returns
// a set containing this quantifier's correlation.
func (q *QuantifiedObjectValue) GetCorrelatedTo() map[CorrelationIdentifier]struct{} {
	return map[CorrelationIdentifier]struct{}{q.Correlation: {}}
}

// --- AggregateValue -----------------------------------------------

// AggregateOp identifies an aggregate function. Mirrors the subset
// of Java's `AggregateValue` that the embedded engine currently
// lowers to a Record Layer aggregate-index query.
type AggregateOp int

// Enum of aggregate operators the seed supports. Ordered to match
// Java's bi-map so serialised plans round-trip.
const (
	AggInvalid   AggregateOp = iota // unassigned — rejects if ever evaluated
	AggCount                        // COUNT(expr)
	AggCountStar                    // COUNT(*)
	AggSum                          // SUM(expr)
	AggMin                          // MIN(expr)
	AggMax                          // MAX(expr)
	AggAvg                          // AVG(expr) — seed: rejects at Evaluate, no streaming impl
)

// Symbol returns the canonical SQL function name.
func (op AggregateOp) Symbol() string {
	switch op {
	case AggCount:
		return "COUNT"
	case AggCountStar:
		return "COUNT(*)"
	case AggSum:
		return "SUM"
	case AggMin:
		return "MIN"
	case AggMax:
		return "MAX"
	case AggAvg:
		return "AVG"
	default:
		return "?AGG?"
	}
}

// AggregateValue represents an aggregate function application —
// `COUNT(*)`, `SUM(col)`, `MIN(expr)`, etc. The Operand is the
// argument (nil for COUNT(*)); the Op identifies which aggregate.
//
// AggregateValue does NOT implement per-row Evaluate — aggregates
// span rows and need an accumulator. Evaluate returns nil to make
// the ignore-of-row-context explicit; rule code identifies
// AggregateValues by type-assertion and routes them to the aggregate
// operator (hash-agg, streaming-agg, index-backed agg) at build
// time.
type AggregateValue struct {
	Op      AggregateOp
	Operand Value // nil iff Op == AggCountStar
}

// NewAggregateValue constructs an AggregateValue. Panics on
// inconsistent op/operand combos (AggCountStar with operand,
// non-CountStar without operand) — these are static programmer
// errors, not runtime data problems.
func NewAggregateValue(op AggregateOp, operand Value) *AggregateValue {
	if op == AggCountStar && operand != nil {
		panic("NewAggregateValue: COUNT(*) takes no operand")
	}
	if op != AggCountStar && op != AggInvalid && operand == nil {
		panic("NewAggregateValue: aggregate requires an operand (use COUNT(*) for star)")
	}
	return &AggregateValue{Op: op, Operand: operand}
}

// Children returns the operand as a single child (empty for
// COUNT(*)). Lets WalkValue traverse aggregate arguments.
func (a *AggregateValue) Children() []Value {
	if a.Operand == nil {
		return []Value{}
	}
	return []Value{a.Operand}
}

// Type returns the SQL type the aggregate produces. COUNT / COUNT(*)
// always produce int; SUM / MIN / MAX inherit from the operand
// (seed assumes int — grows with the Type hierarchy port); AVG
// stays int in the seed even though true SQL AVG returns a rational.
func (a *AggregateValue) Type() ValueType {
	switch a.Op {
	case AggCount, AggCountStar:
		return TypeInt
	case AggSum, AggMin, AggMax, AggAvg:
		if a.Operand != nil {
			return a.Operand.Type()
		}
		return TypeInt
	}
	return TypeUnknown
}

// Name returns the debug-print kind.
func (*AggregateValue) Name() string { return "agg" }

// Evaluate panics — aggregates are multi-row and don't have a
// single-row Evaluate semantics. Rule / plan code type-asserts
// AggregateValue and routes it to an accumulator instead of calling
// Evaluate. The panic message is loud so nobody silently returns nil
// and debugs for an hour.
func (a *AggregateValue) Evaluate(any) any {
	panic("AggregateValue.Evaluate: aggregate must be evaluated over rows by the aggregator, not per-row")
}
