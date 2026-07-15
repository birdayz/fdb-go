package values

import (
	"fmt"
	"strings"
)

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
		return &RecordConstructorValue{Fields: fields}
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
		// RFC-173 S3-W2: the rebuild FUSES a baked node over a new BAKED
		// FieldValue child into one multi-accessor node — Java's architecture,
		// where fuse is a property of the rebuild itself (FieldValue.withNewChild
		// = ofFieldsAndFuseIfPossible, FieldValue.java:278-280): a TranslationMap
		// replacing a QOV leaf with ofOrdinalNumber(QOV(upper), i) composes with
		// the enclosing reference automatically, no map composition. Gated
		// both-baked — the DEFINITION of fusibility (a lazy node has no path to
		// concatenate; in Java the condition is vacuously always true) — so lazy
		// chains keep their shape through the coexistence window and the gate
		// self-widens as W2/W3 bake everything. Must produce the IDENTICAL node
		// to composeFieldOverField (pinned by the rebuild≡compose property test).
		// inner.Child != nil mirrors compose's gate exactly: a CHILDLESS baked
		// inner (the recursive-CTE wrap shape) stays chained through BOTH
		// mechanisms — there is no base to re-anchor the fused path onto.
		if vt.Resolved != nil {
			// RFC-173 item 3: COLLAPSE a baked ordinal path over an
			// RC LITERAL child — the merge's TranslationMap replaces a box
			// quantifier with the box's ordinal RC, and the parent's window
			// ref then IS the RC field's own value (a planner-constructed
			// baked leaf ref, types and markers intact). A fused path over an
			// RC would strand data access (no sarg extraction), materializing
			// the correlated probe the rfc153 plan pins forbid.
			if rc, isRC := newChildren[0].(*RecordConstructorValue); isRC {
				accs := vt.Resolved.Accessors
				rootOrd := -1
				if len(accs) >= 1 {
					rootOrd = accs[0].Ordinal
				}
				// RFC-173 item C: a SOURCE-RELATIVE baked node's root ordinal is
				// relative to the reference's OWN source row, NOT this seed RC.
				// Re-base the root into the seed by the reference's LEG
				// (vt.Child's correlation), matching the seed field whose value is
				// a FieldValue over that SAME leg. A bare-name match would pick the
				// first colliding occurrence (dept.id before emp.id) — the wrong
				// leg, RFC-173's raw-RC duplicate conflation; and the raw source
				// ordinal would pick the seed's slot-0 (e.g. the outer scan's PK),
				// fabricating a wrong comparand that then mis-SARGs. Falls back to
				// the lazy-twin name authority when no leg-tagged field bears the
				// reference's correlation. Machinery-owned bakes (FrontierPinned /
				// multi-accessor) keep the ordinal collapse — their ordinal IS
				// seed-relative by construction.
				if vt.SourceRelativeBaked() && len(accs) >= 1 {
					rootOrd = LegAwareRootOrdinal(vt, accs[0].Ordinal, rc, rootOrd)
				}
				if len(accs) >= 1 && rootOrd >= 0 && rootOrd < len(rc.Fields) && rc.Fields[rootOrd].Value != nil {
					slot := rc.Fields[rootOrd].Value
					if len(accs) == 1 {
						return slot
					}
					// A MULTI-accessor path (fused by an earlier merge round)
					// collapses its ROOT through the RC and fuses the suffix
					// onto the slot's own baked reference — the identical
					// construction the FieldValue-over-FieldValue fuse below
					// produces. A whole fused path left over an RC literal
					// strands data access exactly like the single-accessor
					// case this arm exists for.
					if inner, isFV := slot.(*FieldValue); isFV && inner.Resolved != nil && inner.Child != nil {
						fused := inner.Resolved.WithSuffix(&FieldPath{Accessors: accs[1:]})
						return &FieldValue{Field: fused.Last().Field, Typ: vt.Typ, Child: inner.Child, Resolved: fused}
					}
				}
			}
			if inner, isFV := newChildren[0].(*FieldValue); isFV && inner.Resolved != nil && inner.Child != nil {
				fused := inner.Resolved.WithSuffix(vt.Resolved)
				return &FieldValue{Field: fused.Last().Field, Typ: vt.Typ, Child: inner.Child, Resolved: fused}
			}
		}
		// Preserve the RFC-173 baked-ordinal marker: dropping Resolved would
		// silently degrade a BAKED node to lazy — a conflation hazard for
		// duplicate same-named columns (§5 pin). Covers Replace/RebaseValue and
		// every simplifier rebuild that funnels through WithChildren.
		return &FieldValue{Field: vt.Field, Typ: vt.Typ, Child: newChildren[0], Resolved: vt.Resolved}

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

// LegAwareRootOrdinal resolves the seed-RC slot for a SourceRelativeBaked
// reference collapsed over a flat leg-concatenation seed (a merged box/join
// whose RC concatenates each leg's columns, every field a baked FieldValue over
// its OWN leg's QOV — NewRawRecordConstructorValue, values.go). The reference's
// baked ordinal is relative to its OWN leg, so applying it directly to the seed
// picks the wrong slot; and with columns colliding across legs (dept.id AND
// emp.id) a bare-name match picks the first occurrence — again the wrong leg,
// RFC-173's raw-RC duplicate conflation. Disambiguate by the reference's OWN leg
// (vt.Child's correlation): pick the seed field whose value is a FieldValue over
// that SAME correlation, matched by leg-relative ordinal (name as the within-leg
// tiebreak — a single source has unique column names). Falls back to the
// lazy-twin name authority (then fallbackOrd) when no seed field bears the
// reference's correlation — a differently-shaped seed (e.g. a lateral-unnest
// whose leg field is a bare QOV) resolves by name exactly as before.
func LegAwareRootOrdinal(vt *FieldValue, srcOrd int, rc *RecordConstructorValue, fallbackOrd int) int {
	if childCorr, ok := ownCorrelationOfLeaf(vt.Child); ok {
		for i, f := range rc.Fields {
			fv, isFV := f.Value.(*FieldValue)
			if !isFV {
				continue
			}
			fc, ok := ownCorrelationOfLeaf(fv.Child)
			if !ok || fc != childCorr {
				continue
			}
			if fieldValueLegOrdinal(fv) == srcOrd || strings.EqualFold(fv.Field, vt.Field) {
				return i
			}
		}
	}
	// No leg-tagged field for this correlation: the lazy-twin name authority.
	for i, f := range rc.Fields {
		if strings.EqualFold(f.Name, vt.Field) {
			return i
		}
	}
	return fallbackOrd
}

// fieldValueLegOrdinal returns fv's root source-relative ordinal (its leg-local
// slot), or -1 when fv carries no baked ordinal.
func fieldValueLegOrdinal(fv *FieldValue) int {
	if fv.Resolved != nil && len(fv.Resolved.Accessors) > 0 {
		return fv.Resolved.Accessors[0].Ordinal
	}
	return -1
}
