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
		// The rebuild FUSES a baked node over a new BAKED
		// FieldValue child into one multi-accessor node — Java's architecture,
		// where fuse is a property of the rebuild itself (FieldValue.withNewChild
		// = ofFieldsAndFuseIfPossible, FieldValue.java:278-280): a TranslationMap
		// replacing a QOV leaf with ofOrdinalNumber(QOV(upper), i) composes with
		// the enclosing reference automatically, no map composition. Gated
		// both-baked — the DEFINITION of fusibility (a lazy node has no path to
		// concatenate; in Java the condition is vacuously always true) — so lazy
		// chains keep their shape. Must produce the IDENTICAL node
		// to composeFieldOverField (pinned by the rebuild≡compose property test).
		// inner.Child != nil mirrors compose's gate exactly: a CHILDLESS baked
		// inner (the recursive-CTE wrap shape) stays chained through BOTH
		// mechanisms — there is no base to re-anchor the fused path onto.
		if vt.Resolved != nil {
			// COLLAPSE a baked ordinal path over an
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
				// An UNPINNED baked node's ROOT ordinal is relative to the
				// reference's OWN source row, NOT this seed RC.
				// Re-base the root into the seed by the reference's LEG
				// (vt.Child's correlation), matching the seed field whose value is
				// a FieldValue over that SAME leg. A bare-name match would pick the
				// first colliding occurrence (dept.id before emp.id) — the wrong
				// leg, the raw-RC duplicate-name conflation; and the raw source
				// ordinal would pick the seed's slot-0 (e.g. the outer scan's PK),
				// fabricating a wrong comparand that then mis-SARGs. Falls back to
				// the plan-time name authority when no leg-tagged field bears the
				// reference's correlation. Keyed on the ROOT's leg-relativity
				// (RootIsLegRelativeUnpinned), NOT the accessor count: a FUSED
				// unpinned path (WithSuffix inherits the inner's unpinned leg-relative
				// root) is ALSO source-relative and MUST be rebased — collapsing it by
				// the RAW root would fuse the suffix onto the WRONG leg's seed field
				// (leg-0 by accident for a first-leg reference, a foreign leg
				// otherwise). Only FrontierPinned bakes keep the raw collapse — their
				// ordinal IS seed-relative by construction.
				if vt.RootIsLegRelativeUnpinned() && len(accs) >= 1 {
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
		// Preserve the baked-ordinal marker: dropping Resolved would
		// silently degrade a BAKED node to lazy — a conflation hazard for
		// duplicate same-named columns. Covers Replace/RebaseValue and
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
// the raw-RC duplicate-name conflation. Disambiguate by the reference's OWN leg
// (vt.Child's correlation): pick the seed field whose value is a FieldValue over
// that SAME correlation, matched by leg-relative ORDINAL.
//
// Two name arms used to sit under that, and both are gone (RFC-197 item 3):
//
//   - a within-leg name TIEBREAK, for a seed field carrying no baked ordinal.
//     "A single source has unique column names" is the argument it rested on,
//     and it is an argument for the ordinal, not against it: where the names are
//     unique the ordinal answers identically, and where they are not the name is
//     wrong. A seed field with no ordinal states nothing, so it is skipped.
//   - a cross-leg name FALLBACK, for a seed whose leg field is a bare QOV rather
//     than a per-column FieldValue (a lateral unnest). It resolved a UNIQUE name
//     and poisoned a duplicate — the guard being the admission that the key was
//     wrong, since two bare-QOV legs exposing the same column reach here
//     indistinguishable. Every such seed now poisons.
//
// Poison (-1) is the fail-closed answer for both: the caller leaves the
// reference UN-COLLAPSED rather than collapsing it onto a slot chosen by a
// display name, which is loud on a positional row and never a wrong slot. It is
// the same ambiguity-poison discipline bakeMergeComparisonKeys and rich_ordering
// apply.
//
// Deliberately NOT falling back to fallbackOrd: that is the reference's RAW
// leg-relative ordinal, and applying it to the box's concatenated RC picks the
// FIRST leg's slot — the wrong-leg bug this function exists to prevent (it once
// turned `e.id IS NULL` into `d.id IS NULL` and made a LEFT-join anti-join
// return zero rows). fallbackOrd is used only where a leg-relative rebase was
// never needed.
func LegAwareRootOrdinal(vt *FieldValue, srcOrd int, rc *RecordConstructorValue, fallbackOrd int) int {
	childCorr, ok := ownCorrelationOfLeaf(vt.Child)
	if !ok {
		// The reference names no leg at all, so there is nothing to rebase
		// against and its own ordinal stands.
		return fallbackOrd
	}
	for i, f := range rc.Fields {
		fv, isFV := f.Value.(*FieldValue)
		if !isFV {
			continue
		}
		fc, ok := ownCorrelationOfLeaf(fv.Child)
		if !ok || fc != childCorr {
			continue
		}
		if fieldValueLegOrdinal(fv) == srcOrd {
			return i
		}
	}
	return -1
}

// fieldValueLegOrdinal returns fv's root source-relative ordinal (its leg-local
// slot), or -1 when fv carries no baked ordinal.
func fieldValueLegOrdinal(fv *FieldValue) int {
	if fv.Resolved != nil && len(fv.Resolved.Accessors) > 0 {
		return fv.Resolved.Accessors[0].Ordinal
	}
	return -1
}
