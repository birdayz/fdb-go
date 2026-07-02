package values

import (
	"fmt"
)

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
	case *StrictRankLimitValue:
		// Same rebuild-switch coverage the ArithmeticValue it replaced had: a
		// transform on the K child must not fall to the default (which discards
		// newChildren). Currently latent (K is always const/param) but symmetric.
		return cv.WithChildren(newChildren)
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
		// Preserve the AnchoredJoin marker (RFC-077 F2) — see replace.go.
		return &RecordConstructorValue{Fields: fields, AnchoredJoin: cv.AnchoredJoin}
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
	case *CosineDistanceRowNumberValue:
		return cv.WithChildren(newChildren)
	case *DotProductDistanceRowNumberValue:
		return cv.WithChildren(newChildren)
	case *EuclideanDistanceRowNumberValue:
		return cv.WithChildren(newChildren)
	case *EuclideanSquareDistanceRowNumberValue:
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
	if !EqualsWithoutChildren(a, b) {
		return false
	}
	ac := a.Children()
	bc := b.Children()
	if len(ac) != len(bc) {
		return false
	}
	for i := range ac {
		if !ValuesStructurallyEqual(ac[i], bc[i]) {
			return false
		}
	}
	return true
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

// ptrEqual compares optional-config pointer fields for value identity
// (RFC-176 §3): nil == nil, nil ≠ &v, otherwise pointee equality — Java's
// optional-config semantics (Objects.equals over @Nullable boxed fields).
// Shared by every arm with *T config so the nil pattern lives in one place.
func ptrEqual[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
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

// SelfEqualsWithoutChildren lets a Value implement its own structural
// node-equality — the analogue of Java's per-type Value.equalsWithoutChildren.
// EqualsWithoutChildren's type switch can only enumerate Value implementations
// defined in THIS package; a Value defined elsewhere (e.g. expr.predicateValue,
// which would create an import cycle if referenced here) MUST implement this so
// the Cascades matcher can compare it instead of hitting the unhandled-type
// panic. Children() is still walked by ValuesStructurallyEqual, so a leaf Value
// (Children() empty) returns its full node identity here.
type SelfEqualsWithoutChildren interface {
	EqualsWithoutChildrenValue(other Value) bool
}

// EqualsWithoutChildren checks whether two Values are the same type
// with the same non-child attributes, WITHOUT recursing into children.
// This is the Go equivalent of Java's Value.equalsWithoutChildren().
//
// For leaf values (no children) this is equivalent to
// ValuesStructurallyEqual. For composite values it checks the type
// and any type-specific attributes (operator, field names, etc.) but
// does NOT compare children.
//
// Returns true if a and b have the same concrete type and the same
// non-child attributes (e.g. same ArithmeticOp, same field names in
// RecordConstructorValue, same CastValue target type, etc.).
func EqualsWithoutChildren(a, b Value) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	switch av := a.(type) {
	case *FieldValue:
		bv, ok := b.(*FieldValue)
		if !ok {
			return false
		}
		// RFC-173 identity refinement (§4 ruling #2): BAKED nodes
		// (Resolved != nil) compare by (name, ordinal) — two same-named columns
		// at different ordinals are genuinely different values and must not
		// intern as one memo member (§5 duplicate-name pin); Java's
		// ofOrdinalNumber ordinals are distinct FieldPaths. Baked vs lazy is
		// UNEQUAL by contract: worst case a missed dedup, never a conflation.
		// Lazy vs lazy stays name-only (unchanged; Slice 3 owns the full flip).
		// FrontierPinned is deliberately NOT compared: it is an
		// evaluation-contract marker, not a value distinction (like Java
		// excluding name/type from ResolvedAccessor equality).
		if (av.Resolved != nil) != (bv.Resolved != nil) {
			return false
		}
		if av.Resolved != nil {
			// Path identity is element-wise (Field, Ordinal) list equality —
			// the display Field is NOT separately compared (it duplicates the
			// last accessor's name by construction; Java compares fieldPath
			// only, FieldValue.java:214-217).
			return av.Resolved.Equals(bv.Resolved)
		}
		return av.Field == bv.Field
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
		return ok && ptrEqual(av.Value, bv.Value)
	case *ParameterValue:
		bv, ok := b.(*ParameterValue)
		if !ok {
			return false
		}
		return av.Ordinal == bv.Ordinal && av.ParamName == bv.ParamName
	case *QuantifiedObjectValue:
		bv, ok := b.(*QuantifiedObjectValue)
		return ok && av.Correlation == bv.Correlation
	case *QuantifiedRecordValue:
		bv, ok := b.(*QuantifiedRecordValue)
		return ok && av.Alias == bv.Alias
	case *ObjectValue:
		bv, ok := b.(*ObjectValue)
		return ok && av.Alias == bv.Alias
	case *ArithmeticValue:
		bv, ok := b.(*ArithmeticValue)
		return ok && av.Op == bv.Op
	case *CastValue:
		bv, ok := b.(*CastValue)
		return ok && typesEqual(av.Target, bv.Target)
	case *PromoteValue:
		bv, ok := b.(*PromoteValue)
		return ok && typesEqual(av.Target, bv.Target)
	case *ScalarFunctionValue:
		bv, ok := b.(*ScalarFunctionValue)
		return ok && av.FuncName == bv.FuncName && len(av.Args) == len(bv.Args)
	case *AggregateValue:
		bv, ok := b.(*AggregateValue)
		return ok && av.Op == bv.Op
	case *RecordConstructorValue:
		bv, ok := b.(*RecordConstructorValue)
		if !ok || len(av.Fields) != len(bv.Fields) {
			return false
		}
		// A source-anchored join RC and a plain projection RC with the same field
		// names are NOT the same value: the anchored form HIDES its leg QOVs from
		// GetCorrelatedToOfValue (RFC-077 7.6 F2), the plain form reports them. If
		// they interned as one memo member, partition-time classification would
		// drop buried columns from the live set → 0-row joins.
		if av.AnchoredJoin != bv.AnchoredJoin {
			return false
		}
		for i := range av.Fields {
			if av.Fields[i].Name != bv.Fields[i].Name {
				return false
			}
		}
		return true
	case *NotValue:
		_, ok := b.(*NotValue)
		return ok
	case *AndOrValue:
		bv, ok := b.(*AndOrValue)
		return ok && av.Op == bv.Op
	case *ExistsValue:
		// Transparent composite (RFC-141): no own attributes — the child
		// QuantifiedObjectValue is compared separately by the children
		// recursion. Two ExistsValues are equal-without-children.
		_, ok := b.(*ExistsValue)
		return ok
	case *ScalarSubqueryValue:
		bv, ok := b.(*ScalarSubqueryValue)
		return ok && av.Alias == bv.Alias
	case *ThrowsValue:
		bv, ok := b.(*ThrowsValue)
		return ok && typesEqual(av.ResultType, bv.ResultType)
	case *IndexOnlyAggregateValue:
		bv, ok := b.(*IndexOnlyAggregateValue)
		return ok && av.Op == bv.Op
	case *IndexEntryObjectValue:
		bv, ok := b.(*IndexEntryObjectValue)
		// Source (KEY vs VALUE) is a semantic discriminator: Evaluate reads
		// PrimaryKey() for KEY and IndexValues() for VALUE, so KEY[p] and
		// VALUE[p] address different tuples and must NOT compare equal.
		if !ok || av.Source != bv.Source || len(av.OrdinalPath) != len(bv.OrdinalPath) {
			return false
		}
		for i := range av.OrdinalPath {
			if av.OrdinalPath[i] != bv.OrdinalPath[i] {
				return false
			}
		}
		return true
	case *ToOrderedBytesValue:
		bv, ok := b.(*ToOrderedBytesValue)
		return ok && av.Direction == bv.Direction
	case *FromOrderedBytesValue:
		bv, ok := b.(*FromOrderedBytesValue)
		return ok && av.Direction == bv.Direction && typesEqual(av.TargetType, bv.TargetType)
	case *EvaluatesToValue:
		bv, ok := b.(*EvaluatesToValue)
		return ok && av.Eval == bv.Eval
	case *OfTypeValue:
		bv, ok := b.(*OfTypeValue)
		return ok && typesEqual(av.ExpectedType, bv.ExpectedType)
	case *DistanceValue:
		bv, ok := b.(*DistanceValue)
		return ok && av.Operator == bv.Operator
	case *ConstantObjectValue:
		bv, ok := b.(*ConstantObjectValue)
		return ok && av.ConstantID == bv.ConstantID
	case *UdfValue:
		bv, ok := b.(*UdfValue)
		return ok && av.FunctionName == bv.FunctionName
	case *QueriedValue:
		bv, ok := b.(*QueriedValue)
		if !ok || len(av.RecordTypes) != len(bv.RecordTypes) {
			return false
		}
		for i := range av.RecordTypes {
			if av.RecordTypes[i] != bv.RecordTypes[i] {
				return false
			}
		}
		return true
	case *IndexedValue:
		bv, ok := b.(*IndexedValue)
		return ok && typesEqual(av.ResultType, bv.ResultType)

	// Types with no non-child attributes — type equality is sufficient.
	case *CollateValue:
		_, ok := b.(*CollateValue)
		return ok
	case *LikeOperatorValue:
		_, ok := b.(*LikeOperatorValue)
		return ok
	case *InOpValue:
		_, ok := b.(*InOpValue)
		return ok
	case *PickValue:
		_, ok := b.(*PickValue)
		return ok
	case *CardinalityValue:
		_, ok := b.(*CardinalityValue)
		return ok
	case *ArrayDistinctValue:
		_, ok := b.(*ArrayDistinctValue)
		return ok
	case *ArrayConstructorValue:
		_, ok := b.(*ArrayConstructorValue)
		return ok
	case *DerivedValue:
		_, ok := b.(*DerivedValue)
		return ok
	case *FirstOrDefaultValue:
		_, ok := b.(*FirstOrDefaultValue)
		return ok
	case *FirstOrDefaultStreamingValue:
		_, ok := b.(*FirstOrDefaultStreamingValue)
		return ok
	case *ConditionSelectorValue:
		_, ok := b.(*ConditionSelectorValue)
		return ok
	case *PatternForLikeValue:
		_, ok := b.(*PatternForLikeValue)
		return ok
	case *SubscriptValue:
		_, ok := b.(*SubscriptValue)
		return ok
	case *RangeValue:
		_, ok := b.(*RangeValue)
		return ok
	case *EmptyValue:
		_, ok := b.(*EmptyValue)
		return ok
	case *IncarnationValue:
		_, ok := b.(*IncarnationValue)
		return ok
	case *RecordTypeValue:
		_, ok := b.(*RecordTypeValue)
		return ok
	case *VersionValue:
		_, ok := b.(*VersionValue)
		return ok
	// RFC-176 §3: the Go-only vector config is a non-child attribute and
	// joins identity — Java's principle (every non-child attribute joins
	// equalsWithoutChildren) applied to a read-side extension Java has no
	// arm for. writeSemanticHash folds the same discriminator set.
	//
	// nil-EfSearch (RFC-176 §3): for HNSW (IndexTypeVector) nil means
	// default-200 at eval, so nil-vs-&200 splits two spellings that execute
	// identically — a benign refinement. For SPFresh indexes nil takes the
	// maintainer default while an explicit 200 overrides it, so there
	// nil-vs-&200 is a real semantic discriminator.
	case *DistanceRowNumberValue:
		bv, ok := b.(*DistanceRowNumberValue)
		return ok && av.Metric == bv.Metric &&
			ptrEqual(av.EfSearch, bv.EfSearch) &&
			ptrEqual(av.IsReturningVectors, bv.IsReturningVectors)
	// The four metric specialisations below stay type-only (RFC-176 §2): no
	// non-child attributes — partition/argument Values are children on the
	// embedded WindowedValue, and the class check IS Java's
	// WindowedValue.equalsWithoutChildren (class + name; the concrete Go type
	// is the name).
	case *CosineDistanceRowNumberValue:
		_, ok := b.(*CosineDistanceRowNumberValue)
		return ok
	case *DotProductDistanceRowNumberValue:
		_, ok := b.(*DotProductDistanceRowNumberValue)
		return ok
	case *EuclideanDistanceRowNumberValue:
		_, ok := b.(*EuclideanDistanceRowNumberValue)
		return ok
	case *EuclideanSquareDistanceRowNumberValue:
		_, ok := b.(*EuclideanSquareDistanceRowNumberValue)
		return ok
	case *RowNumberValue:
		// RFC-176 §3: HNSW config joins identity — see the
		// DistanceRowNumberValue arm for the nil-EfSearch semantics per index
		// type.
		bv, ok := b.(*RowNumberValue)
		return ok && ptrEqual(av.EfSearch, bv.EfSearch) &&
			ptrEqual(av.IsReturningVectors, bv.IsReturningVectors)
	case *RowNumberHighOrderValue:
		// RFC-176 §3: same discriminators as RowNumberValue — the curried form
		// must not unify across configs it bakes into the applied RowNumberValue.
		bv, ok := b.(*RowNumberHighOrderValue)
		return ok && ptrEqual(av.EfSearch, bv.EfSearch) &&
			ptrEqual(av.IsReturningVectors, bv.IsReturningVectors)
	case *RankValue:
		// Type-only (RFC-176 §2): no non-child attributes; WindowedValue
		// class+name identity.
		_, ok := b.(*RankValue)
		return ok
	case *UnmatchedAggregateValue:
		bv, ok := b.(*UnmatchedAggregateValue)
		return ok && av.UnmatchedID == bv.UnmatchedID
	default:
		// A Value implementation defined outside this package carries its own
		// structural node-equality (see SelfEqualsWithoutChildren) — this is how
		// e.g. expr.predicateValue is compared without an import cycle. A type
		// that is neither in the switch nor self-equatable is a genuine bug:
		// keep the assertion.
		if eq, ok := a.(SelfEqualsWithoutChildren); ok {
			return eq.EqualsWithoutChildrenValue(b)
		}
		panic(fmt.Sprintf("EqualsWithoutChildren: unhandled Value type %T", a))
	}
}
