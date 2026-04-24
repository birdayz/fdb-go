// Package shapeb is a RFC-022 §4.-0.5 spike realisation of the
// Cascades Value + BindingMatcher hierarchy using generic structs +
// constraint interfaces. Matched values keep their concrete Go type
// at the binding retrieval site.
package shapeb

// Mirror of shapea's Value hierarchy — identical so the two shapes
// match on example-data surface.

type ValueType int

const (
	TypeUnknown ValueType = iota
	TypeInt
	TypeString
	TypeBool
)

// Value is the constraint every concrete value type satisfies. In
// shape (b) we also rely on Value being the upper bound of matcher
// type parameters.
type Value interface {
	Children() []Value
	Type() ValueType
	Name() string
}

type ConstantValue struct {
	Value any
	Typ   ValueType
}

func (c *ConstantValue) Children() []Value { return nil }
func (c *ConstantValue) Type() ValueType   { return c.Typ }
func (c *ConstantValue) Name() string      { return "constant" }

type FieldValue struct {
	Field string
	Typ   ValueType
}

func (f *FieldValue) Children() []Value { return nil }
func (f *FieldValue) Type() ValueType   { return f.Typ }
func (f *FieldValue) Name() string      { return "field" }

type ArithmeticOp int

const (
	OpAdd ArithmeticOp = iota
	OpSub
	OpMul
	OpDiv
)

type ArithmeticValue struct {
	Op    ArithmeticOp
	Left  Value
	Right Value
}

func (a *ArithmeticValue) Children() []Value { return []Value{a.Left, a.Right} }
func (a *ArithmeticValue) Type() ValueType   { return TypeInt }
func (a *ArithmeticValue) Name() string      { return "arith" }
