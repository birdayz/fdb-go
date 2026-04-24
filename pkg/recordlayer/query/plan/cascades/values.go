// Package cascades is the Go port of Java's
// `com.apple.foundationdb.record.query.plan.cascades` (the Cascades
// query planner). This is the Phase 4.0 seed — committed shape per
// RFC-023 (non-generic BindingMatcher interface + `any` + generic
// `Get[T]` retrieval helper). Follow-up shifts add the full Value
// and Type hierarchies, the BindingMatcher combinators (AllOf, AnyOf,
// TypedWithDownstream, etc.), PlannerBindings with proper identity
// keying, CascadesRule / CascadesRuleCall, the memo, cost model, and
// the planner driver.
//
// Dayshift-46 seed contents:
//
//   - values.go          — Value interface (Children, Type, Name,
//     Evaluate) + ValueType enum + 6 concrete
//     values (Constant, Field, Arithmetic, Boolean,
//     Cast, Null) + ExplainValue SQL-ish renderer.
//   - matcher.go         — BindingMatcher interface +
//     PlannerBindings + MergedWith + AnyValue /
//     Instance / ArithmeticMatcher + the generic
//     Get[T] retrieval helper.
//   - combinators.go     — AllOf / AnyOf matcher combinators —
//     primary building blocks for real rule
//     patterns.
//   - predicates.go      — QueryPredicate interface + TriBool
//     (Kleene 3VL) + Constant / And / Or / Not /
//     ValuePredicate.
//   - comparisons.go     — ComparisonType enum (6 operators) +
//     Comparison value-pair + ComparisonPredicate
//     wrapping a Value operand with SQL-3VL on NULL
//     and type mismatch.
//   - correlation.go     — CorrelationIdentifier value-type +
//     Named / Unique factories + Correlated
//     interface signature (Quantifier-tracking
//     surface for rewrite rules).
//   - rule.go            — CascadesRule interface + RuleCall
//     (Yield / Yielded) + FireRule testing driver.
//     Example addConstantFoldRule folds
//     `Const + Const` → `Const`.
//   - rule_simplify.go   — Eleven Phase 4.5 Batch A-style rules:
//     AndFlatten / OrFlatten (associative
//     normalisation), AndConstantSimplify /
//     OrConstantSimplify / NotConstantSimplify /
//     ComparisonConstantSimplify (Kleene folds),
//     AndDedup / OrDedup (structural dedup),
//     AndAbsorbOr / OrAbsorbAnd (absorption),
//     NotComparisonRewrite (NOT-past-comparison).
//   - simplifier.go      — Fixed-point Simplify driver + the
//     DefaultSimplifyRules rule set. Pre-4.6 seed;
//     real planner task-stack replaces this later.
//   - benchmark_test.go  — 7 micro-benchmarks covering Value
//     evaluation, predicate evaluation, and
//     matcher dispatch.
//
// Establishes the shape RFC-023 committed to; real Value
// subtypes (77 in Java), the rest of the matcher combinator
// catalogue (~15 shapes), full QueryPredicate + Comparisons, and
// the CascadesRule / CascadesRuleCall / memo / cost / planner
// driver land in subsequent Phase 4.0 / 4.2-4.6 shifts.
//
// **Zero-size struct gotcha.** Go's spec allows two distinct
// zero-size variables to share an address. `&AnyValue{}` +
// `&AnyValue{}` collapse to the same pointer, which breaks
// PlannerBindings' matcher-identity keying. Every matcher struct
// carries a `uint64` nonce; factory constructors (NewAnyValue,
// NewConstantMatcher, NewFieldMatcher, …) increment an atomic
// counter. Rule authors MUST use the factories, never bare struct
// literals. Java doesn't hit this — `new Object()` always allocates.
//
// Mapping to Java:
//
//	pkg/recordlayer/query/plan/cascades/values.go
//	  ↔ com.apple.foundationdb.record.query.plan.cascades.values.Value
//	     (trimmed — full Java hierarchy has 77 Value subtypes)
//	pkg/recordlayer/query/plan/cascades/matcher.go
//	  ↔ com.apple.foundationdb.record.query.plan.cascades.matching.structure
//	     (trimmed — full Java hierarchy has ~15 matcher shapes)
//
// Future shifts will likely split this into `cascades/values/` +
// `cascades/matching/structure/` subpackages to mirror Java more
// closely, once enough types exist to justify the split.
package cascades

// ValueType is a stand-in for the full Cascades Type hierarchy. The
// production port adds `Type` / `TypeRepository` / `Typed`, at which
// point this enum goes away. See TODO.md Phase 4.0.
type ValueType int

const (
	TypeUnknown ValueType = iota
	TypeInt
	TypeString
	TypeBool
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
	case *NullValue:
		return "NULL"
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
	}
	return "UNKNOWN"
}

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
	}
	return "?"
}

func valueLiteralString(v any) string {
	switch x := v.(type) {
	case int64:
		return intToDec(x)
	case bool:
		if x {
			return "TRUE"
		}
		return "FALSE"
	case string:
		return "'" + x + "'"
	}
	return "?"
}

func intToDec(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
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

// ArithmeticOp is a subset of SQL arithmetic — enough to build a
// non-trivial matcher.
type ArithmeticOp int

const (
	OpAdd ArithmeticOp = iota
	OpSub
	OpMul
	OpDiv
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
		return li + ri
	case OpSub:
		return li - ri
	case OpMul:
		return li * ri
	case OpDiv:
		if ri == 0 {
			return nil
		}
		return li / ri
	}
	return nil
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
		}
	case TypeBool:
		switch val := v.(type) {
		case bool:
			return val
		case int64:
			return val != 0
		}
	case TypeString:
		if s, ok := v.(string); ok {
			return s
		}
		if i, ok := v.(int64); ok {
			return uitoa(uint64(i))
		}
	}
	return nil
}
