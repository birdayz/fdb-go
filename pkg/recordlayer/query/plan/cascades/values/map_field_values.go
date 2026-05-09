package values

// MapFieldValues recursively walks a Value tree and applies transform to
// every FieldValue encountered at any depth. Non-FieldValue leaf nodes
// are returned unchanged. Composite nodes are rebuilt with transformed
// children when at least one child changed.
//
// This handles all common composite Value types by type-switching and
// manually reconstructing the node with updated children. Types with a
// WithChildren method use that; types without one are reconstructed by
// field. Unknown composite types fall back to returning v unchanged
// (conservative: won't corrupt, won't transform FieldValues nested
// inside an unrecognized composite).
//
// The full type-switch ensures that nested expressions like
// UPPER(A.NAME), A.X + B.Y, CAST(A.COL AS INT), CASE WHEN A.X > 0 ...
// have their FieldValues properly transformed at any depth, unlike the
// old childReplacer-interface approach which only handled ~10 types.
//
// Used by rule_push_filter_below_join.go and
// rule_implement_nested_loop_join.go to strip alias prefixes from
// FieldValues at arbitrary nesting depth.
func MapFieldValues(v Value, transform func(*FieldValue) Value) Value {
	if v == nil {
		return nil
	}

	// FieldValue: apply the transform directly.
	if fv, ok := v.(*FieldValue); ok {
		return transform(fv)
	}

	// Leaf values with no children — return unchanged.
	children := v.Children()
	if len(children) == 0 {
		return v
	}

	// Composite values — recurse into children, rebuild if anything changed.
	changed := false
	newChildren := make([]Value, len(children))
	for i, c := range children {
		nc := MapFieldValues(c, transform)
		if nc != c {
			changed = true
		}
		newChildren[i] = nc
	}
	if !changed {
		return v
	}

	// Rebuild the value with new children. Type-switch on known
	// composite types to preserve metadata (op codes, type annotations,
	// etc.) that Children() alone doesn't carry.
	switch cv := v.(type) {
	case *ArithmeticValue:
		return &ArithmeticValue{Op: cv.Op, Left: newChildren[0], Right: newChildren[1]}
	case *CastValue:
		return &CastValue{Child: newChildren[0], Target: cv.Target}
	case *PromoteValue:
		return &PromoteValue{Child: newChildren[0], Target: cv.Target}
	case *NotValue:
		return &NotValue{Child: newChildren[0]}
	case *ScalarFunctionValue:
		return &ScalarFunctionValue{FuncName: cv.FuncName, Args: newChildren, Typ: cv.Typ}
	case *AggregateValue:
		var operand Value
		if len(newChildren) > 0 {
			operand = newChildren[0]
		}
		return &AggregateValue{Op: cv.Op, Operand: operand}
	case *RecordConstructorValue:
		fields := make([]RecordConstructorField, len(cv.Fields))
		for i, f := range cv.Fields {
			fields[i] = RecordConstructorField{Name: f.Name, Value: newChildren[i]}
		}
		return &RecordConstructorValue{Fields: fields}
	case *LikeOperatorValue:
		return &LikeOperatorValue{Probe: newChildren[0], Pattern: newChildren[1]}
	case *InOpValue:
		return &InOpValue{Probe: newChildren[0], List: newChildren[1]}
	case *DistanceValue:
		return &DistanceValue{Operator: cv.Operator, LeftChild: newChildren[0], RightChild: newChildren[1]}
	case *ArrayDistinctValue:
		return &ArrayDistinctValue{Child: newChildren[0], Typ: cv.Typ}
	case *CardinalityValue:
		return &CardinalityValue{Child: newChildren[0]}
	case *EvaluatesToValue:
		return &EvaluatesToValue{Child: newChildren[0], Eval: cv.Eval}
	case *OfTypeValue:
		return &OfTypeValue{Child: newChildren[0], ExpectedType: cv.ExpectedType}
	case *DerivedValue:
		return &DerivedValue{ChildrenList: newChildren, ResultType: cv.ResultType}
	case *SubscriptValue:
		return &SubscriptValue{Source: newChildren[0], Index: newChildren[1], Typ: cv.Typ}

	// Types with WithChildren — delegate to preserve internal metadata.
	case *AndOrValue:
		return cv.WithChildren(newChildren)
	case *ArrayConstructorValue:
		return cv.WithChildren(newChildren)
	case *CollateValue:
		return cv.WithChildren(newChildren)
	case *ConditionSelectorValue:
		return cv.WithChildren(newChildren)
	case *IndexOnlyAggregateValue:
		return cv.WithChildren(newChildren)
	case *RankValue:
		return cv.WithChildren(newChildren)
	case *RowNumberValue:
		return cv.WithChildren(newChildren)
	case *UdfValue:
		return cv.WithChildren(newChildren)
	case *FirstOrDefaultStreamingValue:
		return cv.WithChildren(newChildren)
	case *DistanceRowNumberValue:
		return cv.WithChildren(newChildren)

	default:
		// Unknown composite type — return unchanged. Won't corrupt, but
		// FieldValues nested inside this type won't be transformed. Add
		// new cases above when new composite types are introduced.
		return v
	}
}

// ValuesStructurallyEqual reports whether two Values are structurally
// equal: same concrete Go type, same metadata, and recursively equal
// children. Stronger than ExplainValue comparison which could
// theoretically collide on structurally different values that render
// the same string.
func ValuesStructurallyEqual(a, b Value) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	switch av := a.(type) {
	case *FieldValue:
		bv, ok := b.(*FieldValue)
		return ok && av.Field == bv.Field
	case *ConstantValue:
		bv, ok := b.(*ConstantValue)
		if !ok {
			return false
		}
		return constantValuesEqual(av.Value, bv.Value)
	case *NullValue:
		_, ok := b.(*NullValue)
		return ok
	case *BooleanValue:
		bv, ok := b.(*BooleanValue)
		if !ok {
			return false
		}
		if av.Value == nil && bv.Value == nil {
			return true
		}
		if av.Value == nil || bv.Value == nil {
			return false
		}
		return *av.Value == *bv.Value
	case *ParameterValue:
		bv, ok := b.(*ParameterValue)
		if !ok {
			return false
		}
		return av.Ordinal == bv.Ordinal && av.ParamName == bv.ParamName
	case *QuantifiedObjectValue:
		bv, ok := b.(*QuantifiedObjectValue)
		return ok && av.Correlation == bv.Correlation
	case *ObjectValue:
		bv, ok := b.(*ObjectValue)
		return ok && av.Alias == bv.Alias
	case *ArithmeticValue:
		bv, ok := b.(*ArithmeticValue)
		if !ok || av.Op != bv.Op {
			return false
		}
		return ValuesStructurallyEqual(av.Left, bv.Left) &&
			ValuesStructurallyEqual(av.Right, bv.Right)
	case *CastValue:
		bv, ok := b.(*CastValue)
		if !ok {
			return false
		}
		return typesEqual(av.Target, bv.Target) &&
			ValuesStructurallyEqual(av.Child, bv.Child)
	case *PromoteValue:
		bv, ok := b.(*PromoteValue)
		if !ok {
			return false
		}
		return typesEqual(av.Target, bv.Target) &&
			ValuesStructurallyEqual(av.Child, bv.Child)
	case *ScalarFunctionValue:
		bv, ok := b.(*ScalarFunctionValue)
		if !ok || av.FuncName != bv.FuncName || len(av.Args) != len(bv.Args) {
			return false
		}
		for i := range av.Args {
			if !ValuesStructurallyEqual(av.Args[i], bv.Args[i]) {
				return false
			}
		}
		return true
	case *AggregateValue:
		bv, ok := b.(*AggregateValue)
		if !ok || av.Op != bv.Op {
			return false
		}
		return ValuesStructurallyEqual(av.Operand, bv.Operand)
	case *RecordConstructorValue:
		bv, ok := b.(*RecordConstructorValue)
		if !ok || len(av.Fields) != len(bv.Fields) {
			return false
		}
		for i := range av.Fields {
			if av.Fields[i].Name != bv.Fields[i].Name {
				return false
			}
			if !ValuesStructurallyEqual(av.Fields[i].Value, bv.Fields[i].Value) {
				return false
			}
		}
		return true
	case *NotValue:
		bv, ok := b.(*NotValue)
		return ok && ValuesStructurallyEqual(av.Child, bv.Child)
	default:
		// For types not explicitly handled, fall back to ExplainValue
		// comparison. Preserves prior behavior for rarely-encountered
		// types while the common types above get proper structural
		// equality.
		return ExplainValue(a) == ExplainValue(b)
	}
}

// constantValuesEqual compares two any values for equality, handling
// the []byte case (slices aren't comparable with ==).
func constantValuesEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	// Handle []byte specially since Go's == panics on slices.
	if ab, ok := a.([]byte); ok {
		bb, ok := b.([]byte)
		if !ok {
			return false
		}
		if len(ab) != len(bb) {
			return false
		}
		for i := range ab {
			if ab[i] != bb[i] {
				return false
			}
		}
		return true
	}
	// Handle []any for IN-list comparisons.
	if al, ok := a.([]any); ok {
		bl, ok := b.([]any)
		if !ok || len(al) != len(bl) {
			return false
		}
		for i := range al {
			if !constantValuesEqual(al[i], bl[i]) {
				return false
			}
		}
		return true
	}
	return a == b
}

// typesEqual checks if two Types are structurally equal by code and
// nullability. Used by ValuesStructurallyEqual for CastValue/PromoteValue.
func typesEqual(a, b Type) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Code() == b.Code() && a.IsNullable() == b.IsNullable()
}
