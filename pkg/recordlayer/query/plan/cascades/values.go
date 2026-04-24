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
// Concrete Values implement Children / Type; matchers downcast via
// type switches / type assertions on the concrete Go type.
//
// Java equivalent: `Value extends Correlated<Value>, TreeLike<Value>,
// Typed, ...`. The initial port keeps just Children + Type since those are
// the surfaces the matcher DSL actually touches.
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
}

// --- Concrete values ------------------------------------------------

// ConstantValue is a literal.
type ConstantValue struct {
	Value any
	Typ   ValueType
}

func (c *ConstantValue) Children() []Value { return nil }
func (c *ConstantValue) Type() ValueType   { return c.Typ }
func (c *ConstantValue) Name() string      { return "constant" }

// FieldValue references a column by name.
type FieldValue struct {
	Field string
	Typ   ValueType
}

func (f *FieldValue) Children() []Value { return nil }
func (f *FieldValue) Type() ValueType   { return f.Typ }
func (f *FieldValue) Name() string      { return "field" }

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
type ArithmeticValue struct {
	Op    ArithmeticOp
	Left  Value
	Right Value
	// Result type: int for the seed impls; full type inference lands
}

func (a *ArithmeticValue) Children() []Value { return []Value{a.Left, a.Right} }
func (a *ArithmeticValue) Type() ValueType   { return TypeInt }
func (a *ArithmeticValue) Name() string      { return "arith" }
