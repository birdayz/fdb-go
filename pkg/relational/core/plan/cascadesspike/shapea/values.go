// Package shapea is a RFC-022 §4.-0.5 spike realisation of the
// Cascades Value + BindingMatcher hierarchy using non-generic
// interfaces + `any`. Matched values flow through `any`; rule
// bodies downcast via type assertion.
package shapea

// ValueType is a stand-in for the full Cascades Type hierarchy —
// for the spike we only need to distinguish by kind to show how
// matchers plumb types through bindings.
type ValueType int

const (
	TypeUnknown ValueType = iota
	TypeInt
	TypeString
	TypeBool
)

// Value is the root of the (spike) expression value hierarchy.
// Concrete Values implement Children / Type; matchers downcast via
// type switches / type assertions on the concrete Go type.
//
// Java equivalent: `Value extends Correlated<Value>, TreeLike<Value>,
// Typed, ...`. The spike only keeps Children + Type since those are
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
	// Result type: int for these ops in the spike.
}

func (a *ArithmeticValue) Children() []Value { return []Value{a.Left, a.Right} }
func (a *ArithmeticValue) Type() ValueType   { return TypeInt }
func (a *ArithmeticValue) Name() string      { return "arith" }
