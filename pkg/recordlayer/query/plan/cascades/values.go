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
// The first two commits here (shipped in dayshift-46):
//
//   - values.go   — Value interface + 3 concrete values (Constant,
//     Field, Arithmetic) + ValueType enum. A seed that
//     establishes the shape RFC-023 committed to; real
//     Value subtypes land in subsequent Phase 4.0
//     shifts (77 in Java).
//   - matcher.go  — BindingMatcher interface + PlannerBindings +
//     AnyValue / Instance / ArithmeticMatcher + the
//     generic Get[T] retrieval helper.
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

func (c *ConstantValue) Children() []Value { return nil }
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

func (f *FieldValue) Children() []Value { return nil }
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

func (*BooleanValue) Children() []Value { return nil }
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
