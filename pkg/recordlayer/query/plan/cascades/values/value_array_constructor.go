package values

// ArrayConstructorValue evaluates an N-element ARRAY[a, b, c, ...]
// SQL literal — gathers each child Value's evaluation into a `[]any`
// representing the array. Mirrors Java's
// `LightArrayConstructorValue` (the simple, non-protobuf-message
// variant of `AbstractArrayConstructorValue`).
//
// The explicit-type constructor, like Java's of(children, elementType),
// expects the planner to resolve and promote its children first. It does
// not validate them; Evaluate returns their values verbatim. Reconstruction
// validates the retained element type before injecting promotions, so a
// rewrite cannot silently attach incompatible children to that metadata.
//
// Result type: non-nullable Array(ElementType). Java's getResultType()
// returns `Type.Array(elementType)` (always non-nullable since the
// constructor produces a concrete array literal); Go
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
//
// The UNTYPED empty literal `[]` (element type NONE) is special: its
// result type is the bare NONE type, not Array(NONE) — matching
// Java's emptyArrayOfNone (AbstractArrayConstructorValue.java:304,
// getResultType() returns Type.noneType()). NONE is what the
// promotion lattice keys on (NONE_TO_ARRAY; Type.maximumType's NONE
// arms), so `arr = []` promotes the literal to the column's ARRAY
// type instead of failing an Array(NONE)-vs-Array(T) recursion.
func (v *ArrayConstructorValue) Type() Type {
	if v.ElementType != nil && v.ElementType.Code() == TypeCodeNone {
		return NoneType
	}
	return &ArrayType{Nullable: false, ElementType: v.ElementType}
}

// Evaluate gathers each child's evaluation into a `[]any`.
//
// Empty constructor returns an empty `[]any{}` (NOT nil) so callers
// can distinguish empty-array from NULL-array via `len(result) ==
// 0 && result != nil`.
//
// The raw Go constructor tolerates nil child Values as nil elements.
// Java rejects nil children when copying its constructor arguments.
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

// WithChildren retains the declared element type, rejecting incompatible
// replacement children with nil. Checked planners use WithChildrenChecked
// to retain the reconstruction diagnostic.
func (v *ArrayConstructorValue) WithChildren(newChildren []Value) *ArrayConstructorValue {
	rebuilt, _ := v.withChildrenChecked(newChildren)
	return rebuilt
}

func (v *ArrayConstructorValue) withChildrenChecked(newChildren []Value) (*ArrayConstructorValue, error) {
	if v == nil {
		return nil, resolutionError(RewriteNilReplacement, "array.rebuild", "cannot rebuild a nil array constructor")
	}
	// Java LightArrayConstructorValue.withChildren retains the original for
	// an empty replacement, including the untyped NONE literal.
	if len(newChildren) == 0 {
		return v, nil
	}
	if isNilBinding(v.ElementType) {
		return nil, resolutionError(TypeNil, "array.rebuild", "array element type is nil")
	}
	for _, child := range newChildren {
		if isNilBinding(child) {
			return nil, resolutionError(RewriteNilReplacement, "array.rebuild", "array child is nil")
		}
	}
	children := append([]Value(nil), newChildren...)
	if !IsAny(v.ElementType) {
		// Java resolves the common type BEFORE injecting promotions and requires
		// exact equality, including nullability and nested element/field types.
		var common Type
		for i, child := range children {
			childType := child.Type()
			if isNilBinding(childType) {
				return nil, resolutionError(TypeNil, "array.rebuild", "array child type is nil")
			}
			if i == 0 {
				common = childType
			} else {
				common = MaximumType(common, childType)
			}
			if common == nil {
				return nil, resolutionError(ReanchorResultTypeMismatch, "array.rebuild", "replacement children have no common element type")
			}
		}
		if !common.Equals(v.ElementType) {
			return nil, resolutionError(ReanchorResultTypeMismatch, "array.rebuild", "replacement children change the declared element type or nullability")
		}
		nullableElement := WithNullability(v.ElementType, true)
		for i, child := range children {
			if !WithNullability(child.Type(), true).Equals(nullableElement) {
				children[i] = NewPromoteValue(child, v.ElementType)
			}
		}
	}
	unchanged := len(children) == len(v.Elements)
	if unchanged {
		for i, child := range children {
			if child != v.Elements[i] {
				unchanged = false
				break
			}
		}
	}
	if unchanged {
		return v, nil
	}
	return &ArrayConstructorValue{ElementType: v.ElementType, Elements: children}, nil
}
