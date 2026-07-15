package values

import "testing"

func TestReplace_NilValue(t *testing.T) {
	t.Parallel()
	result := Replace(nil, func(v Value) Value { return v })
	if result != nil {
		t.Fatalf("Replace(nil, ...) should return nil, got %v", result)
	}
}

func TestReplace_LeafMatched(t *testing.T) {
	t.Parallel()
	original := &ConstantValue{Value: int64(1), Typ: NullableLong}
	replacement := &ConstantValue{Value: int64(42), Typ: NullableLong}

	result := Replace(original, func(v Value) Value {
		if c, ok := v.(*ConstantValue); ok && c.Value == int64(1) {
			return replacement
		}
		return v
	})

	c, ok := result.(*ConstantValue)
	if !ok {
		t.Fatalf("expected *ConstantValue, got %T", result)
	}
	if c.Value != int64(42) {
		t.Fatalf("expected 42, got %v", c.Value)
	}
}

func TestReplace_LeafUnmatched(t *testing.T) {
	t.Parallel()
	original := &ConstantValue{Value: int64(1), Typ: NullableLong}

	result := Replace(original, func(v Value) Value {
		return v // identity — no replacement
	})

	if result != original {
		t.Fatalf("identity fn should return the same pointer")
	}
}

func TestReplace_FnReturnsNil(t *testing.T) {
	t.Parallel()
	original := &ConstantValue{Value: int64(1), Typ: NullableLong}

	result := Replace(original, func(v Value) Value {
		return nil
	})

	if result != nil {
		t.Fatalf("fn returning nil should propagate nil, got %v", result)
	}
}

func TestReplace_ArithmeticReplaceOneChild(t *testing.T) {
	t.Parallel()
	left := &ConstantValue{Value: int64(1), Typ: NullableLong}
	right := &ConstantValue{Value: int64(2), Typ: NullableLong}
	arith := &ArithmeticValue{Op: OpAdd, Left: left, Right: right}

	result := Replace(arith, func(v Value) Value {
		if c, ok := v.(*ConstantValue); ok && c.Value == int64(1) {
			return &ConstantValue{Value: int64(10), Typ: NullableLong}
		}
		return v
	})

	a, ok := result.(*ArithmeticValue)
	if !ok {
		t.Fatalf("expected *ArithmeticValue, got %T", result)
	}
	lc, ok := a.Left.(*ConstantValue)
	if !ok {
		t.Fatalf("expected left to be *ConstantValue, got %T", a.Left)
	}
	if lc.Value != int64(10) {
		t.Fatalf("expected left = 10, got %v", lc.Value)
	}
	// Right should be unchanged (same pointer).
	if a.Right != right {
		t.Fatalf("right child should be the same pointer")
	}
	if a.Op != OpAdd {
		t.Fatalf("op should be preserved as OpAdd, got %v", a.Op)
	}
}

func TestReplace_DeepNestedReplace(t *testing.T) {
	t.Parallel()
	// (1 + (2 + 3)) — replace the inner 3 with 30
	inner := &ArithmeticValue{
		Op:    OpMul,
		Left:  &ConstantValue{Value: int64(2), Typ: NullableLong},
		Right: &ConstantValue{Value: int64(3), Typ: NullableLong},
	}
	outer := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(1), Typ: NullableLong},
		Right: inner,
	}

	result := Replace(outer, func(v Value) Value {
		if c, ok := v.(*ConstantValue); ok && c.Value == int64(3) {
			return &ConstantValue{Value: int64(30), Typ: NullableLong}
		}
		return v
	})

	a, ok := result.(*ArithmeticValue)
	if !ok {
		t.Fatalf("expected outer *ArithmeticValue, got %T", result)
	}
	innerResult, ok := a.Right.(*ArithmeticValue)
	if !ok {
		t.Fatalf("expected inner *ArithmeticValue, got %T", a.Right)
	}
	rc, ok := innerResult.Right.(*ConstantValue)
	if !ok {
		t.Fatalf("expected inner right *ConstantValue, got %T", innerResult.Right)
	}
	if rc.Value != int64(30) {
		t.Fatalf("expected inner right = 30, got %v", rc.Value)
	}
}

func TestReplace_IdentityPreservesPointers(t *testing.T) {
	t.Parallel()
	left := &ConstantValue{Value: int64(1), Typ: NullableLong}
	right := &ConstantValue{Value: int64(2), Typ: NullableLong}
	arith := &ArithmeticValue{Op: OpAdd, Left: left, Right: right}

	result := Replace(arith, func(v Value) Value {
		return v
	})

	// No child changed, so the result should be the same pointer.
	if result != arith {
		t.Fatalf("identity fn should preserve the root pointer")
	}
}

func TestReplace_NilChildPropagates(t *testing.T) {
	t.Parallel()
	left := &ConstantValue{Value: int64(1), Typ: NullableLong}
	right := &ConstantValue{Value: int64(2), Typ: NullableLong}
	arith := &ArithmeticValue{Op: OpAdd, Left: left, Right: right}

	// Return nil for the right child — whole tree should become nil.
	result := Replace(arith, func(v Value) Value {
		if c, ok := v.(*ConstantValue); ok && c.Value == int64(2) {
			return nil
		}
		return v
	})

	if result != nil {
		t.Fatalf("nil child should propagate to nil result, got %v", result)
	}
}

func TestReplace_CastValue(t *testing.T) {
	t.Parallel()
	child := &ConstantValue{Value: int64(5), Typ: NullableLong}
	cast := &CastValue{Child: child, Target: NullableString}

	result := Replace(cast, func(v Value) Value {
		if c, ok := v.(*ConstantValue); ok && c.Value == int64(5) {
			return &ConstantValue{Value: int64(99), Typ: NullableLong}
		}
		return v
	})

	c, ok := result.(*CastValue)
	if !ok {
		t.Fatalf("expected *CastValue, got %T", result)
	}
	inner, ok := c.Child.(*ConstantValue)
	if !ok {
		t.Fatalf("expected inner *ConstantValue, got %T", c.Child)
	}
	if inner.Value != int64(99) {
		t.Fatalf("expected 99, got %v", inner.Value)
	}
	if c.Target != NullableString {
		t.Fatalf("target type should be preserved")
	}
}

func TestReplace_RecordConstructorValue(t *testing.T) {
	t.Parallel()
	rcv := &RecordConstructorValue{
		Fields: []RecordConstructorField{
			{Name: "A", Value: &ConstantValue{Value: int64(1), Typ: NullableLong}},
			{Name: "B", Value: &ConstantValue{Value: int64(2), Typ: NullableLong}},
		},
	}

	result := Replace(rcv, func(v Value) Value {
		if c, ok := v.(*ConstantValue); ok && c.Value == int64(2) {
			return &ConstantValue{Value: int64(20), Typ: NullableLong}
		}
		return v
	})

	r, ok := result.(*RecordConstructorValue)
	if !ok {
		t.Fatalf("expected *RecordConstructorValue, got %T", result)
	}
	if len(r.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(r.Fields))
	}
	if r.Fields[0].Name != "A" || r.Fields[1].Name != "B" {
		t.Fatalf("field names should be preserved")
	}
	c1, _ := r.Fields[1].Value.(*ConstantValue)
	if c1.Value != int64(20) {
		t.Fatalf("expected field B = 20, got %v", c1.Value)
	}
}

func TestReplace_PreOrder(t *testing.T) {
	t.Parallel()
	// Pre-order means fn is applied to the parent BEFORE recursing
	// into children. If we replace the whole ArithmeticValue with a
	// leaf, children should NOT be visited.
	left := &ConstantValue{Value: int64(1), Typ: NullableLong}
	right := &ConstantValue{Value: int64(2), Typ: NullableLong}
	arith := &ArithmeticValue{Op: OpAdd, Left: left, Right: right}

	childVisited := false
	replacement := &ConstantValue{Value: int64(999), Typ: NullableLong}

	result := Replace(arith, func(v Value) Value {
		if _, ok := v.(*ArithmeticValue); ok {
			return replacement // replace entire subtree
		}
		childVisited = true
		return v
	})

	if childVisited {
		t.Fatalf("children should not be visited when parent is replaced with a leaf")
	}
	if result != replacement {
		t.Fatalf("should return the replacement leaf")
	}
}

func TestReplace_ScalarFunctionValue(t *testing.T) {
	t.Parallel()
	sfv := &ScalarFunctionValue{
		FuncName: "UPPER",
		Args: []Value{
			&FieldValue{Field: "name", Typ: NullableString},
		},
		Typ: NullableString,
	}

	result := Replace(sfv, func(v Value) Value {
		if f, ok := v.(*FieldValue); ok && f.Field == "name" {
			return &FieldValue{Field: "title", Typ: NullableString}
		}
		return v
	})

	s, ok := result.(*ScalarFunctionValue)
	if !ok {
		t.Fatalf("expected *ScalarFunctionValue, got %T", result)
	}
	if s.FuncName != "UPPER" {
		t.Fatalf("function name should be preserved")
	}
	f, ok := s.Args[0].(*FieldValue)
	if !ok {
		t.Fatalf("expected *FieldValue arg, got %T", s.Args[0])
	}
	if f.Field != "title" {
		t.Fatalf("expected field 'title', got %q", f.Field)
	}
}

func TestReplace_AndOrValue(t *testing.T) {
	t.Parallel()
	andor := NewAndOrValue(AndOrAnd,
		NewBooleanValue(true),
		NewBooleanValue(false),
	)

	result := Replace(andor, func(v Value) Value {
		if b, ok := v.(*BooleanValue); ok && b.Value != nil && *b.Value {
			return NewBooleanValue(false)
		}
		return v
	})

	a, ok := result.(*AndOrValue)
	if !ok {
		t.Fatalf("expected *AndOrValue, got %T", result)
	}
	lb, ok := a.Left.(*BooleanValue)
	if !ok || lb.Value == nil || *lb.Value {
		t.Fatalf("left should be false after replacement")
	}
}

func TestReplace_NotValue(t *testing.T) {
	t.Parallel()
	not := &NotValue{Child: NewBooleanValue(true)}

	result := Replace(not, func(v Value) Value {
		if b, ok := v.(*BooleanValue); ok && b.Value != nil && *b.Value {
			return NewBooleanValue(false)
		}
		return v
	})

	n, ok := result.(*NotValue)
	if !ok {
		t.Fatalf("expected *NotValue, got %T", result)
	}
	b, ok := n.Child.(*BooleanValue)
	if !ok || b.Value == nil || *b.Value {
		t.Fatalf("child should be false after replacement")
	}
}

func TestReplace_PromoteValue(t *testing.T) {
	t.Parallel()
	pv := &PromoteValue{
		Child:  &ConstantValue{Value: int64(5), Typ: NullableLong},
		Target: NullableDouble,
	}

	result := Replace(pv, func(v Value) Value {
		if c, ok := v.(*ConstantValue); ok && c.Value == int64(5) {
			return &ConstantValue{Value: int64(50), Typ: NullableLong}
		}
		return v
	})

	p, ok := result.(*PromoteValue)
	if !ok {
		t.Fatalf("expected *PromoteValue, got %T", result)
	}
	c, ok := p.Child.(*ConstantValue)
	if !ok {
		t.Fatalf("expected child *ConstantValue, got %T", p.Child)
	}
	if c.Value != int64(50) {
		t.Fatalf("expected 50, got %v", c.Value)
	}
	if p.Target != NullableDouble {
		t.Fatalf("target should be preserved")
	}
}

func TestReplace_AggregateValue(t *testing.T) {
	t.Parallel()
	agg := &AggregateValue{
		Op:      AggSum,
		Operand: &FieldValue{Field: "amount", Typ: NullableLong},
	}

	result := Replace(agg, func(v Value) Value {
		if f, ok := v.(*FieldValue); ok && f.Field == "amount" {
			return &FieldValue{Field: "total", Typ: NullableLong}
		}
		return v
	})

	a, ok := result.(*AggregateValue)
	if !ok {
		t.Fatalf("expected *AggregateValue, got %T", result)
	}
	f, ok := a.Operand.(*FieldValue)
	if !ok {
		t.Fatalf("expected operand *FieldValue, got %T", a.Operand)
	}
	if f.Field != "total" {
		t.Fatalf("expected field 'total', got %q", f.Field)
	}
	if a.Op != AggSum {
		t.Fatalf("op should be preserved")
	}
}

func TestReplace_AggregateCountStarUnchanged(t *testing.T) {
	t.Parallel()
	agg := &AggregateValue{Op: AggCountStar}

	result := Replace(agg, func(v Value) Value {
		return v
	})

	// COUNT(*) has no children, identity fn should preserve pointer.
	if result != agg {
		t.Fatalf("COUNT(*) with identity fn should return same pointer")
	}
}

func TestReplaceLeavesMaybe_OnlyLeavesTouched(t *testing.T) {
	t.Parallel()
	left := &ConstantValue{Value: int64(1), Typ: NullableLong}
	right := &ConstantValue{Value: int64(2), Typ: NullableLong}
	arith := &ArithmeticValue{Op: OpAdd, Left: left, Right: right}

	nonLeafTouched := false
	result := ReplaceLeavesMaybe(arith, func(v Value) Value {
		if _, ok := v.(*ArithmeticValue); ok {
			nonLeafTouched = true
		}
		if c, ok := v.(*ConstantValue); ok && c.Value == int64(1) {
			return &ConstantValue{Value: int64(100), Typ: NullableLong}
		}
		return v
	})

	if nonLeafTouched {
		t.Fatalf("ReplaceLeavesMaybe should not pass non-leaf nodes to fn")
	}
	a, ok := result.(*ArithmeticValue)
	if !ok {
		t.Fatalf("expected *ArithmeticValue, got %T", result)
	}
	lc, ok := a.Left.(*ConstantValue)
	if !ok {
		t.Fatalf("expected *ConstantValue, got %T", a.Left)
	}
	if lc.Value != int64(100) {
		t.Fatalf("expected 100, got %v", lc.Value)
	}
}

func TestReplaceLeavesMaybe_NilLeafPropagates(t *testing.T) {
	t.Parallel()
	arith := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(1), Typ: NullableLong},
		Right: &ConstantValue{Value: int64(2), Typ: NullableLong},
	}

	result := ReplaceLeavesMaybe(arith, func(v Value) Value {
		if c, ok := v.(*ConstantValue); ok && c.Value == int64(2) {
			return nil
		}
		return v
	})

	if result != nil {
		t.Fatalf("nil leaf replacement should propagate to nil, got %v", result)
	}
}

func TestReplace_PickValue(t *testing.T) {
	t.Parallel()
	pick := &PickValue{
		Selector: &ConstantValue{Value: int64(0), Typ: NullableLong},
		Alternatives: []Value{
			&ConstantValue{Value: int64(10), Typ: NullableLong},
			&ConstantValue{Value: int64(20), Typ: NullableLong},
		},
		Typ: NullableLong,
	}

	result := Replace(pick, func(v Value) Value {
		if c, ok := v.(*ConstantValue); ok && c.Value == int64(20) {
			return &ConstantValue{Value: int64(200), Typ: NullableLong}
		}
		return v
	})

	p, ok := result.(*PickValue)
	if !ok {
		t.Fatalf("expected *PickValue, got %T", result)
	}
	if len(p.Alternatives) != 2 {
		t.Fatalf("expected 2 alternatives, got %d", len(p.Alternatives))
	}
	c, ok := p.Alternatives[1].(*ConstantValue)
	if !ok {
		t.Fatalf("expected *ConstantValue in alt[1], got %T", p.Alternatives[1])
	}
	if c.Value != int64(200) {
		t.Fatalf("expected alt[1] = 200, got %v", c.Value)
	}
}

func TestReplace_DerivedValue(t *testing.T) {
	t.Parallel()
	dv := &DerivedValue{
		ChildrenList: []Value{
			&FieldValue{Field: "a", Typ: NullableLong},
			&FieldValue{Field: "b", Typ: NullableString},
		},
		ResultType: UnknownType,
	}

	result := Replace(dv, func(v Value) Value {
		if f, ok := v.(*FieldValue); ok && f.Field == "a" {
			return &FieldValue{Field: "x", Typ: NullableLong}
		}
		return v
	})

	d, ok := result.(*DerivedValue)
	if !ok {
		t.Fatalf("expected *DerivedValue, got %T", result)
	}
	f, ok := d.ChildrenList[0].(*FieldValue)
	if !ok {
		t.Fatalf("expected *FieldValue, got %T", d.ChildrenList[0])
	}
	if f.Field != "x" {
		t.Fatalf("expected field 'x', got %q", f.Field)
	}
}

func TestReplace_ReplaceRootWithDifferentType(t *testing.T) {
	t.Parallel()
	// Replace the root ArithmeticValue with a ConstantValue.
	arith := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(1), Typ: NullableLong},
		Right: &ConstantValue{Value: int64(2), Typ: NullableLong},
	}

	result := Replace(arith, func(v Value) Value {
		if _, ok := v.(*ArithmeticValue); ok {
			return &ConstantValue{Value: int64(3), Typ: NullableLong}
		}
		return v
	})

	c, ok := result.(*ConstantValue)
	if !ok {
		t.Fatalf("expected *ConstantValue after root replacement, got %T", result)
	}
	if c.Value != int64(3) {
		t.Fatalf("expected 3, got %v", c.Value)
	}
}

func TestReplace_CollateValue(t *testing.T) {
	t.Parallel()
	cv := NewCollateValue(
		&FieldValue{Field: "name", Typ: NullableString},
		&ConstantValue{Value: "en_US", Typ: NullableString},
		nil,
	)

	result := Replace(cv, func(v Value) Value {
		if f, ok := v.(*FieldValue); ok && f.Field == "name" {
			return &FieldValue{Field: "title", Typ: NullableString}
		}
		return v
	})

	c, ok := result.(*CollateValue)
	if !ok {
		t.Fatalf("expected *CollateValue, got %T", result)
	}
	f, ok := c.StringChild.(*FieldValue)
	if !ok {
		t.Fatalf("expected *FieldValue, got %T", c.StringChild)
	}
	if f.Field != "title" {
		t.Fatalf("expected 'title', got %q", f.Field)
	}
}

func TestReplace_ConditionSelectorValue(t *testing.T) {
	t.Parallel()
	csv := NewConditionSelectorValue([]Value{
		NewBooleanValue(false),
		NewBooleanValue(true),
	})

	result := Replace(csv, func(v Value) Value {
		if b, ok := v.(*BooleanValue); ok && b.Value != nil && !*b.Value {
			return NewBooleanValue(true)
		}
		return v
	})

	c, ok := result.(*ConditionSelectorValue)
	if !ok {
		t.Fatalf("expected *ConditionSelectorValue, got %T", result)
	}
	b, ok := c.Implications[0].(*BooleanValue)
	if !ok || b.Value == nil || !*b.Value {
		t.Fatalf("first implication should be true after replacement")
	}
}

// TestReplaceLeavesOnceMaybe_SelfReferentialTerminates pins the cycle-break
// (RFC-077 7.6): substituting a leaf with a value that itself
// contains a same-alias leaf must apply replaceFn EXACTLY ONCE and terminate.
// Plain ReplaceLeavesMaybe re-descends into the replacement, re-matches the
// same-alias leaf, and recurses forever (the real failure: the source-anchored
// join RC anchors right-leg columns to QOV(B) while the parent quantifier over
// the join is also aliased B, so a B->value-containing-B substitution loops).
func TestReplaceLeavesOnceMaybe_SelfReferentialTerminates(t *testing.T) {
	t.Parallel()
	b := NamedCorrelationIdentifier("B")
	orig := NewQuantifiedObjectValue(b) // a leaf

	calls := 0
	replaceFn := func(node Value) Value {
		qov, ok := node.(*QuantifiedObjectValue)
		if !ok || qov.Correlation != b {
			return node
		}
		calls++
		// The replacement CONTAINS a new same-alias (B) leaf — the self-reference.
		return NewFieldValue(NewQuantifiedObjectValue(b), "col", UnknownType)
	}

	got := ReplaceLeavesOnceMaybe(orig, replaceFn)
	// Applied exactly once: the original B leaf is replaced; the NEW B leaf inside
	// the replacement is recorded and skipped (not re-replaced).
	if calls != 1 {
		t.Fatalf("replaceFn applied %d times, want exactly 1 (new-leaf guard must break the self-reference)", calls)
	}
	fv, ok := got.(*FieldValue)
	if !ok {
		t.Fatalf("result = %T, want *FieldValue (the single replacement)", got)
	}
	if _, ok := fv.Child.(*QuantifiedObjectValue); !ok {
		t.Fatalf("replacement's child = %T, want the un-re-replaced *QuantifiedObjectValue(B)", fv.Child)
	}
}

// TestLegAwareRootOrdinal pins the RFC-173 item C leg-aware collapse: the
// tiebreak (ordinal-preferred over name) and the cross-leg name fallback are the
// conflation-sensitive paths and get a dedicated pin. A SourceRelativeBaked ref
// collapsed over a flat leg-concatenation seed must re-base by the reference's
// OWN leg (its QOV correlation), never by bare name — else colliding cross-leg
// columns (dept.id / emp.id) conflate to the first occurrence.
func TestLegAwareRootOrdinal(t *testing.T) {
	t.Parallel()
	qov := func(name string) *QuantifiedObjectValue {
		return NewQuantifiedObjectValue(NamedCorrelationIdentifier(name))
	}
	// ref builds a SourceRelativeBaked reference over leg `corr`.
	ref := func(corr, field string, ord int) *FieldValue {
		return NewCorrelatedFieldValueWithResolvedOrdinal(qov(corr), field, ord, UnknownType)
	}
	legField := func(name, corr string, ord int) RecordConstructorField {
		return RecordConstructorField{Name: name, Value: ref(corr, name, ord)}
	}

	// (1) Colliding cross-leg names: dept(ID,DNAME) ++ emp(ID,DEPT_ID,FNAME).
	// e.id (leg E, source ordinal 0) must pick emp.id at seed slot 2 — NOT the
	// first "ID" (dept.id, slot 0), the raw-RC duplicate conflation that produced
	// the I3 wrong-rows bug.
	i3seed := NewRawRecordConstructorValue(
		legField("ID", "D", 0), legField("DNAME", "D", 1),
		legField("ID", "E", 0), legField("DEPT_ID", "E", 1), legField("FNAME", "E", 2),
	)
	if got := LegAwareRootOrdinal(ref("E", "ID", 0), 0, i3seed, -1); got != 2 {
		t.Fatalf("colliding leg: e.id must resolve to seed slot 2 (emp.id), got %d", got)
	}
	if got := LegAwareRootOrdinal(ref("D", "ID", 0), 0, i3seed, -1); got != 0 {
		t.Fatalf("colliding leg: d.id must resolve to seed slot 0 (dept.id), got %d", got)
	}

	// (2) Ordinal-preferred tiebreak: when a reference's source ordinal and Field
	// disagree (an upstream construction inconsistency), the ORDINAL wins — the
	// baked slot is the authority (RFC-173's ordinal-not-name principle). Leg E
	// has ID at leg-ordinal 5 (slot 0) and FNAME at leg-ordinal 0 (slot 1); a ref
	// {Field:ID, srcOrd:0} must pick the ordinal-0 slot (1), NOT the name-"ID"
	// slot (0). Ordering ID FIRST is deliberate: the retired
	// `ordinal==srcOrd || name`-OR predecessor name-first-matches slot 0 here, so
	// this assertion FAILS on a revert to it (the pin catches the real regression,
	// not just a hypothetical strict-name scan).
	skewSeed := NewRawRecordConstructorValue(
		legField("ID", "E", 5), legField("FNAME", "E", 0),
	)
	if got := LegAwareRootOrdinal(ref("E", "ID", 0), 0, skewSeed, -1); got != 1 {
		t.Fatalf("ordinal-preferred: srcOrd 0 must win over name ID → slot 1 (FNAME@0); got %d (the retired OR-code gives 0)", got)
	}

	// (2b) The within-leg NAME tiebreak — the `return nameMatch` arm. When NO leg
	// field's ordinal matches srcOrd but a name does, resolve by name. Leg E has
	// FNAME@3 and ID@5, neither at ordinal 0; a ref {Field:ID, srcOrd:0} misses on
	// ordinal and resolves ID by name at slot 1. (Exercises the branch the ordinal
	// and cross-leg arms otherwise shadow.)
	nameTieSeed := NewRawRecordConstructorValue(
		legField("FNAME", "E", 3), legField("ID", "E", 5),
	)
	if got := LegAwareRootOrdinal(ref("E", "ID", 0), 0, nameTieSeed, -1); got != 1 {
		t.Fatalf("within-leg name tiebreak: e.id with no ordinal-0 field must resolve ID by name at slot 1, got %d", got)
	}

	// (3) Cross-leg name-fallback: no seed field bears the reference's correlation
	// (a differently-shaped seed). Falls back to the FIRST bare-name match — the
	// retired name-model authority. INVARIANT: reachable only when NO leg carries
	// the corr, so it cannot mis-pick a colliding sibling (that took arm (1)).
	noLegSeed := NewRawRecordConstructorValue(
		legField("ID", "D", 0), legField("DNAME", "D", 1),
	)
	if got := LegAwareRootOrdinal(ref("Z", "ID", 0), 0, noLegSeed, -99); got != 0 {
		t.Fatalf("name-fallback: unknown-corr ref must match first name 'ID' at slot 0, got %d", got)
	}
	if got := LegAwareRootOrdinal(ref("Z", "NOPE", 0), 0, noLegSeed, -99); got != -99 {
		t.Fatalf("name-fallback: no name match must return fallbackOrd -99, got %d", got)
	}
}
