package values

// ArrayConstructorValue evaluates an N-element ARRAY[a, b, c, ...]
// SQL literal — gathers each child Value's evaluation into a `[]any`
// representing the array. Mirrors Java's
// `LightArrayConstructorValue` (the simple, non-protobuf-message
// variant of `AbstractArrayConstructorValue`).
//
// All children must produce values compatible with the declared
// `ElementType`. The seed does NOT enforce per-element type
// validation at construction — Java's `injectPromotions` chain
// handles type-coercion via `PromoteValue` wrappers; the planner is
// expected to pre-resolve children to compatible types before
// reaching this constructor. Mismatched child types surface at
// evaluation as nil-typed elements in the produced slice.
//
// Result type: nullable Array(ElementType). Java's getResultType()
// returns `Type.Array(elementType)` (always non-nullable since the
// constructor produces a concrete array literal); the Go seed
// matches by emitting `&ArrayType{Nullable: false, ElementType: ...}`.
//
// Empty-array case: an array constructor with zero children
// produces an empty slice (NOT nil) — Java's eval likewise returns
// `ImmutableList.of()`. This distinguishes "empty array" from
// "NULL array" — important for SQL CARDINALITY / ARRAY_LENGTH
// operations where empty has length 0 and NULL has length NULL.
type ArrayConstructorValue struct {
	ElementType Type
	Elements    []Value
}

// NewArrayConstructorValue constructs an array literal from N
// element Values, declaring the array's element type. ElementType
// can be UnknownType when the planner hasn't yet resolved child
// types — eval still works, child evaluations flow through.
func NewArrayConstructorValue(elementType Type, elements []Value) *ArrayConstructorValue {
	if elementType == nil {
		elementType = UnknownType
	}
	cp := make([]Value, len(elements))
	copy(cp, elements)
	return &ArrayConstructorValue{
		ElementType: elementType,
		Elements:    cp,
	}
}

// Children returns the element Values.
func (v *ArrayConstructorValue) Children() []Value { return v.Elements }

// Name returns the SQL function name.
func (*ArrayConstructorValue) Name() string { return "array" }

// Type returns Array(ElementType), non-nullable. Even an empty
// array constructor produces a non-nullable empty array — NULL
// arrays come from elsewhere (a column value of NULL, etc.), not
// from the constructor.
func (v *ArrayConstructorValue) Type() Type {
	return &ArrayType{Nullable: false, ElementType: v.ElementType}
}

// Evaluate is the error-returning twin (RFC-091).
func (v *ArrayConstructorValue) Evaluate(evalCtx any) (any, error) {
	out := make([]any, len(v.Elements))
	for i, child := range v.Elements {
		if child != nil {
			cv, err := child.Evaluate(evalCtx)
			if err != nil {
				return nil, err
			}
			out[i] = cv
		}
	}
	return out, nil
}

// WithChildren returns a fresh ArrayConstructorValue with new
// elements. Element type carries through unchanged — caller is
// responsible for ensuring new children's types are compatible.
func (v *ArrayConstructorValue) WithChildren(newChildren []Value) *ArrayConstructorValue {
	return NewArrayConstructorValue(v.ElementType, newChildren)
}
