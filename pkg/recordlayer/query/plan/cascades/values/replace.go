package values

import "fmt"

// Replace applies replacementFn to every node in the Value tree
// rooted at v, in pre-order (parent before children). If
// replacementFn returns nil, the entire subtree is removed (Replace
// returns nil). If replacementFn returns a different Value, that
// Value's children are then recursed. If replacementFn returns the
// same Value unchanged, children are recursed and the original node
// is kept unless a child was replaced.
//
// Copy-on-write: a node's children list is only allocated when at
// least one child was actually replaced — identical subtrees reuse
// the original pointers.
//
// Matches Java's `TreeLike.replace(UnaryOperator<T>)` semantics
// exactly: pre-order traversal, CoW, nil-propagation.
func Replace(v Value, replacementFn func(Value) Value) Value {
	if v == nil {
		return nil
	}

	// Step 1: apply fn to this node (pre-order).
	maybeReplaced := replacementFn(v)
	if maybeReplaced == nil {
		return nil
	}

	// Step 2: recurse into children of the (potentially replaced) node.
	children := maybeReplaced.Children()
	if len(children) == 0 {
		return maybeReplaced
	}

	// CoW: only allocate newChildren when a child actually changes.
	var newChildren []Value
	for i, child := range children {
		replacedChild := Replace(child, replacementFn)
		if replacedChild == nil {
			return nil
		}
		if replacedChild != child {
			if newChildren == nil {
				// Lazily allocate and copy preceding unchanged children.
				newChildren = make([]Value, len(children))
				copy(newChildren[:i], children[:i])
			}
			newChildren[i] = replacedChild
		} else if newChildren != nil {
			newChildren[i] = child
		}
	}

	if newChildren == nil {
		// No child changed — return the (potentially replaced) node as-is.
		return maybeReplaced
	}

	// Step 3: reconstruct the node with new children.
	return withChildren(maybeReplaced, newChildren)
}

// ReplaceLeavesMaybe applies replaceFn only to leaf nodes (Values
// with no children) in pre-order. Non-leaf nodes are traversed but
// not passed to replaceFn. Matches Java's
// `TreeLike.replaceLeavesMaybe(UnaryOperator<T>)`.
//
// Returns nil if replaceFn returns nil for any leaf.
func ReplaceLeavesMaybe(v Value, replaceFn func(Value) Value) Value {
	return Replace(v, func(node Value) Value {
		if len(node.Children()) == 0 {
			return replaceFn(node)
		}
		return node
	})
}

// ReplaceLeavesOnceMaybe is Java's TreeLike.replaceLeavesMaybe(op,
// visitNewLeaves=false): it applies replaceFn to leaf nodes, but does NOT
// re-apply it to leaves INTRODUCED by a replacement. After a leaf is replaced,
// every leaf of the replacement subtree is recorded (by pointer identity) and
// skipped on the subsequent re-descent.
//
// This is the correct semantics for SELF-REFERENTIAL substitutions — e.g.
// TranslationMap substituting alias B with a value that itself references B (the
// source-anchored join RC anchors its right-leg columns to QOV(B), while the
// parent quantifier over the join is ALSO aliased B). Plain Replace /
// ReplaceLeavesMaybe re-descend into the replacement, re-match B, and loop
// forever. Tracking new leaves breaks the cycle exactly as Java does.
//
// Returns nil if replaceFn returns nil for any (original) leaf.
func ReplaceLeavesOnceMaybe(v Value, replaceFn func(Value) Value) Value {
	newLeaves := map[Value]struct{}{}
	return Replace(v, func(node Value) Value {
		if len(node.Children()) != 0 {
			return node
		}
		if _, isNew := newLeaves[node]; isNew {
			return node
		}
		result := replaceFn(node)
		if result == nil {
			return nil
		}
		// Record every leaf of the replacement subtree so the re-descent does not
		// re-apply replaceFn to them (pointer identity, matching Java's
		// Sets.newIdentityHashSet()).
		WalkValue(result, func(n Value) bool {
			if len(n.Children()) == 0 {
				newLeaves[n] = struct{}{}
			}
			return true
		})
		return result
	})
}

// WithChildren is the exported entry point for reconstructing a Value
// with new children. Delegates to the unexported withChildren.
func WithChildren(v Value, newChildren []Value) Value {
	return withChildren(v, newChildren)
}

// withChildren reconstructs a Value with new children. Dispatches
// via type switch over all known concrete Value types in this
// package. Types that already have a WithChildren method are called
// directly; all other non-leaf types are handled inline.
//
// If the concrete type is unrecognised (e.g. a Value implementation
// from outside this package), the original node is returned with its
// old children — the caller's fn was still applied to the node
// itself in step 1.
func withChildren(v Value, newChildren []Value) Value {
	if v == nil {
		return nil
	}
	if len(newChildren) == 0 && len(v.Children()) == 0 {
		return v
	}
	switch vt := v.(type) {
	// --- Types with existing WithChildren methods ---
	case *AndOrValue:
		return vt.WithChildren(newChildren)
	case *CollateValue:
		return vt.WithChildren(newChildren)
	case *ConditionSelectorValue:
		return vt.WithChildren(newChildren)
	case *UdfValue:
		return vt.WithChildren(newChildren)
	case *ArrayConstructorValue:
		return vt.WithChildren(newChildren)
	case *IndexOnlyAggregateValue:
		return vt.WithChildren(newChildren)
	case *RankValue:
		return vt.WithChildren(newChildren)
	case *RowNumberValue:
		return vt.WithChildren(newChildren)
	case *DistanceRowNumberValue:
		return vt.WithChildren(newChildren)
	case *CosineDistanceRowNumberValue:
		return vt.WithChildren(newChildren)
	case *DotProductDistanceRowNumberValue:
		return vt.WithChildren(newChildren)
	case *EuclideanDistanceRowNumberValue:
		return vt.WithChildren(newChildren)
	case *EuclideanSquareDistanceRowNumberValue:
		return vt.WithChildren(newChildren)
	case *FirstOrDefaultStreamingValue:
		return vt.WithChildren(newChildren)
	case *StrictRankLimitValue:
		return vt.WithChildren(newChildren)

	// --- values.go types without WithChildren ---
	case *ArithmeticValue:
		if len(newChildren) != 2 {
			return v
		}
		return &ArithmeticValue{Op: vt.Op, Left: newChildren[0], Right: newChildren[1]}
	case *CastValue:
		if len(newChildren) != 1 {
			return v
		}
		return &CastValue{Child: newChildren[0], Target: vt.Target}
	case *PromoteValue:
		if len(newChildren) != 1 {
			return v
		}
		return &PromoteValue{Child: newChildren[0], Target: vt.Target}
	case *RecordConstructorValue:
		if len(newChildren) != len(vt.Fields) {
			return v
		}
		fields := make([]RecordConstructorField, len(vt.Fields))
		for i, f := range vt.Fields {
			fields[i] = RecordConstructorField{Name: f.Name, Value: newChildren[i]}
		}
		// Preserve the AnchoredJoin marker (RFC-077 F2) — it must survive the
		// flatten-time substitution SelectMergeRule does via values.Replace, or
		// the exploration-time correlation hiding silently lapses after the first
		// rebase and the ≥4-way STAR blows past the task budget.
		return &RecordConstructorValue{Fields: fields, AnchoredJoin: vt.AnchoredJoin}
	case *AggregateValue:
		if vt.Op == AggCountStar {
			return v // COUNT(*) has no operand children
		}
		if len(newChildren) != 1 {
			return v
		}
		return &AggregateValue{Op: vt.Op, Operand: newChildren[0]}
	case *ScalarFunctionValue:
		args := make([]Value, len(newChildren))
		copy(args, newChildren)
		return &ScalarFunctionValue{FuncName: vt.FuncName, Args: args, Typ: vt.Typ}
	case *NotValue:
		if len(newChildren) != 1 {
			return v
		}
		return &NotValue{Child: newChildren[0]}

	// --- separate value_*.go files without WithChildren ---
	case *LikeOperatorValue:
		// Children() filters nil Probe/Pattern; reconstruct matching
		// the same positional layout.
		idx := 0
		probe := vt.Probe
		if vt.Probe != nil && idx < len(newChildren) {
			probe = newChildren[idx]
			idx++
		}
		pattern := vt.Pattern
		if vt.Pattern != nil && idx < len(newChildren) {
			pattern = newChildren[idx]
		}
		return &LikeOperatorValue{Probe: probe, Pattern: pattern}
	case *InOpValue:
		// Children() filters nil Probe/List.
		idx := 0
		probe := vt.Probe
		if vt.Probe != nil && idx < len(newChildren) {
			probe = newChildren[idx]
			idx++
		}
		list := vt.List
		if vt.List != nil && idx < len(newChildren) {
			list = newChildren[idx]
		}
		return &InOpValue{Probe: probe, List: list}
	case *OfTypeValue:
		if len(newChildren) != 1 {
			return v
		}
		return &OfTypeValue{Child: newChildren[0], ExpectedType: vt.ExpectedType}
	case *EvaluatesToValue:
		if len(newChildren) != 1 {
			return v
		}
		return &EvaluatesToValue{Child: newChildren[0], Eval: vt.Eval}
	case *CardinalityValue:
		if len(newChildren) != 1 {
			return v
		}
		return &CardinalityValue{Child: newChildren[0]}
	case *ArrayDistinctValue:
		if len(newChildren) != 1 {
			return v
		}
		return &ArrayDistinctValue{Child: newChildren[0], Typ: vt.Typ}
	case *RecordTypeValue:
		if len(newChildren) != 1 {
			return v
		}
		return &RecordTypeValue{Child: newChildren[0]}
	case *VersionValue:
		if len(newChildren) != 1 {
			return v
		}
		return &VersionValue{Child: newChildren[0]}
	case *ToOrderedBytesValue:
		if len(newChildren) != 1 {
			return v
		}
		return &ToOrderedBytesValue{Child: newChildren[0], Direction: vt.Direction}
	case *FromOrderedBytesValue:
		if len(newChildren) != 1 {
			return v
		}
		return &FromOrderedBytesValue{Child: newChildren[0], Direction: vt.Direction, TargetType: vt.TargetType}
	case *DistanceValue:
		if len(newChildren) != 2 {
			return v
		}
		return &DistanceValue{Operator: vt.Operator, LeftChild: newChildren[0], RightChild: newChildren[1]}
	case *DerivedValue:
		cp := make([]Value, len(newChildren))
		copy(cp, newChildren)
		return &DerivedValue{ChildrenList: cp, ResultType: vt.ResultType}
	case *PickValue:
		// PickValue.Children() includes Selector (position 0) only when
		// non-nil, followed by Alternatives. Reconstruct with the same
		// shape.
		if vt.Selector != nil {
			if len(newChildren) < 1 {
				return v
			}
			alts := make([]Value, len(newChildren)-1)
			copy(alts, newChildren[1:])
			return &PickValue{Selector: newChildren[0], Alternatives: alts, Typ: vt.Typ}
		}
		// Selector was nil — newChildren are all alternatives.
		alts := make([]Value, len(newChildren))
		copy(alts, newChildren)
		return &PickValue{Selector: nil, Alternatives: alts, Typ: vt.Typ}
	case *SubscriptValue:
		// Children() filters nil Source/Index.
		idx := 0
		source := vt.Source
		if vt.Source != nil && idx < len(newChildren) {
			source = newChildren[idx]
			idx++
		}
		index := vt.Index
		if vt.Index != nil && idx < len(newChildren) {
			index = newChildren[idx]
		}
		return &SubscriptValue{Source: source, Index: index, Typ: vt.Typ}
	case *PatternForLikeValue:
		if len(newChildren) != 2 {
			return v
		}
		return &PatternForLikeValue{PatternChild: newChildren[0], EscapeChild: newChildren[1]}
	case *FirstOrDefaultValue:
		// Children() filters nil Array/Default.
		idx := 0
		array := vt.Array
		if vt.Array != nil && idx < len(newChildren) {
			array = newChildren[idx]
			idx++
		}
		def := vt.Default
		if vt.Default != nil && idx < len(newChildren) {
			def = newChildren[idx]
		}
		return &FirstOrDefaultValue{Array: array, Default: def, Typ: vt.Typ}
	case *RangeValue:
		if len(newChildren) != 3 {
			return v
		}
		return &RangeValue{BeginInclusive: newChildren[0], EndExclusive: newChildren[1], Step: newChildren[2]}

	case *FieldValue:
		if len(newChildren) != 1 {
			return v
		}
		return &FieldValue{Field: vt.Field, Typ: vt.Typ, Child: newChildren[0]}

	case *ExistsValue:
		// Transparent composite (RFC-141) over a single child
		// QuantifiedObjectValue.
		if len(newChildren) != 1 {
			return v
		}
		return &ExistsValue{Value: newChildren[0]}

	default:
		// A Value defined OUTSIDE this package (e.g. expr.predicateValue, which
		// would import-cycle if referenced here) reconstructs itself via
		// SelfWithChildren — the WithChildren analogue of SelfEqualsWithoutChildren
		// / SelfSemanticHash. Without this, exposing such a value's Children()
		// (e.g. a CASE WHEN condition's operand values) would hit the
		// unhandled-type panic the moment Replace/RebaseValue rebuilds it.
		if swc, ok := v.(SelfWithChildren); ok {
			return swc.WithChildrenValue(newChildren)
		}
		panic(fmt.Sprintf("withChildren: unhandled Value type %T", v))
	}
}

// SelfWithChildren lets a Value defined outside this package reconstruct itself
// with new children, so values.WithChildren (and Replace/RebaseValue, which build
// new trees bottom-up) can rewrite it without this package's type switch
// enumerating it. The WithChildren analogue of SelfEqualsWithoutChildren and
// SelfSemanticHash. The newChildren slice has the same length and order as the
// value's Children().
type SelfWithChildren interface {
	WithChildrenValue(newChildren []Value) Value
}
