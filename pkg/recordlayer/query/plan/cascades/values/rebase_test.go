package values

import "testing"

func TestRebaseValue_QOV(t *testing.T) {
	t.Parallel()
	oldAlias := NamedCorrelationIdentifier("old")
	newAlias := NamedCorrelationIdentifier("new")
	v := mustQOV(t, oldAlias)
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: oldAlias, Target: newAlias}))
	qov, ok := result.(*quantifiedObjectValue)
	if !ok {
		t.Fatalf("expected *quantifiedObjectValue, got %T", result)
	}
	if qov.Correlation() != newAlias {
		t.Fatalf("expected correlation %v, got %v", newAlias, qov.Correlation())
	}
}

func TestRebaseValue_QOV_NoMatch(t *testing.T) {
	t.Parallel()
	v := mustQOV(t, NamedCorrelationIdentifier("other"))
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: NamedCorrelationIdentifier("old"), Target: NamedCorrelationIdentifier("new")}))
	if result != v {
		t.Fatal("non-matching QOV should return same pointer")
	}
}

func TestRebaseValue_Field(t *testing.T) {
	t.Parallel()
	v := &fieldValue{Field: "x", Typ: UnknownType}
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: NamedCorrelationIdentifier("old"), Target: NamedCorrelationIdentifier("new")}))
	if result != v {
		t.Fatal("FieldValue should return same pointer (no correlation)")
	}
}

func TestRebaseValue_Constant(t *testing.T) {
	t.Parallel()
	v := &ConstantValue{Value: 42}
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: NamedCorrelationIdentifier("old"), Target: NamedCorrelationIdentifier("new")}))
	if result != v {
		t.Fatal("ConstantValue should return same pointer")
	}
}

func TestRebaseValue_Nil(t *testing.T) {
	t.Parallel()
	result := RebaseValue(nil, mustAliasMap(t))
	if result != nil {
		t.Fatal("nil value should return nil")
	}
}

func TestRebaseValue_EmptyAliasMap(t *testing.T) {
	t.Parallel()
	v := mustQOV(t, NamedCorrelationIdentifier("x"))
	result := RebaseValue(v, nil)
	if result != v {
		t.Fatal("nil alias map should return same pointer")
	}
}

func TestRebaseValue_ArithmeticRecursion(t *testing.T) {
	t.Parallel()
	oldAlias := NamedCorrelationIdentifier("old")
	newAlias := NamedCorrelationIdentifier("new")
	v := &ArithmeticValue{
		Op:    OpAdd,
		Left:  mustQOV(t, oldAlias),
		Right: &ConstantValue{Value: 1},
	}
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: oldAlias, Target: newAlias}))
	arith, ok := result.(*ArithmeticValue)
	if !ok {
		t.Fatalf("expected *ArithmeticValue, got %T", result)
	}
	qov, ok := arith.Left.(*quantifiedObjectValue)
	if !ok {
		t.Fatalf("expected left to be *quantifiedObjectValue, got %T", arith.Left)
	}
	if qov.Correlation() != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation())
	}
	if arith.Right != v.Right {
		t.Fatal("right side (constant) should be preserved")
	}
}

func TestRebaseValue_ArithmeticNoChange(t *testing.T) {
	t.Parallel()
	v := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: 1},
		Right: &ConstantValue{Value: 2},
	}
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: NamedCorrelationIdentifier("old"), Target: NamedCorrelationIdentifier("new")}))
	if result != v {
		t.Fatal("arithmetic with no matching aliases should return same pointer")
	}
}

func TestRebaseValue_CastRecursion(t *testing.T) {
	t.Parallel()
	oldAlias := NamedCorrelationIdentifier("old")
	newAlias := NamedCorrelationIdentifier("new")
	v := NewCastValue(mustQOV(t, oldAlias), NullableLong)
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: oldAlias, Target: newAlias}))
	cast, ok := result.(*CastValue)
	if !ok {
		t.Fatalf("expected *CastValue, got %T", result)
	}
	qov, ok := cast.Child.(*quantifiedObjectValue)
	if !ok {
		t.Fatalf("expected child to be *quantifiedObjectValue, got %T", cast.Child)
	}
	if qov.Correlation() != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation())
	}
}

func TestRebaseValue_CastNoChange(t *testing.T) {
	t.Parallel()
	v := NewCastValue(&ConstantValue{Value: 42}, NullableLong)
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: NamedCorrelationIdentifier("old"), Target: NamedCorrelationIdentifier("new")}))
	if result != v {
		t.Fatal("cast with no matching aliases should return same pointer")
	}
}

func TestRebaseValue_ScalarFunction(t *testing.T) {
	t.Parallel()
	oldAlias := NamedCorrelationIdentifier("old")
	newAlias := NamedCorrelationIdentifier("new")
	v := &ScalarFunctionValue{
		FuncName: "COALESCE",
		Args: []Value{
			mustQOV(t, oldAlias),
			&ConstantValue{Value: 0},
		},
		Typ: UnknownType,
	}
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: oldAlias, Target: newAlias}))
	fn, ok := result.(*ScalarFunctionValue)
	if !ok {
		t.Fatalf("expected *ScalarFunctionValue, got %T", result)
	}
	if fn.FuncName != "COALESCE" {
		t.Fatalf("function name = %q, want COALESCE", fn.FuncName)
	}
	qov, ok := fn.Args[0].(*quantifiedObjectValue)
	if !ok {
		t.Fatalf("expected arg[0] to be *quantifiedObjectValue, got %T", fn.Args[0])
	}
	if qov.Correlation() != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation())
	}
}

func TestRebaseValue_RecordConstructor(t *testing.T) {
	t.Parallel()
	oldAlias := NamedCorrelationIdentifier("old")
	newAlias := NamedCorrelationIdentifier("new")
	v := &RecordConstructorValue{
		Fields: []RecordConstructorField{
			{Name: "a", Value: mustQOV(t, oldAlias)},
			{Name: "b", Value: &ConstantValue{Value: 42}},
		},
	}
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: oldAlias, Target: newAlias}))
	rc, ok := result.(*RecordConstructorValue)
	if !ok {
		t.Fatalf("expected *RecordConstructorValue, got %T", result)
	}
	if len(rc.Fields) != 2 {
		t.Fatalf("fields count = %d, want 2", len(rc.Fields))
	}
	qov, ok := rc.Fields[0].Value.(*quantifiedObjectValue)
	if !ok {
		t.Fatalf("expected field[0].Value to be *quantifiedObjectValue, got %T", rc.Fields[0].Value)
	}
	if qov.Correlation() != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation())
	}
}

func TestRebaseValue_NotValue_Rebases(t *testing.T) {
	t.Parallel()
	oldAlias := NamedCorrelationIdentifier("old")
	newAlias := NamedCorrelationIdentifier("new")
	v := &NotValue{Child: mustQOV(t, oldAlias)}
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: oldAlias, Target: newAlias}))
	nv, ok := result.(*NotValue)
	if !ok {
		t.Fatalf("expected *NotValue, got %T", result)
	}
	qov, ok := nv.Child.(*quantifiedObjectValue)
	if !ok {
		t.Fatalf("expected child to be *quantifiedObjectValue, got %T", nv.Child)
	}
	if qov.Correlation() != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation())
	}
}

func TestRebaseValue_NotValue_NoChange(t *testing.T) {
	t.Parallel()
	v := &NotValue{Child: &ConstantValue{Value: true}}
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: NamedCorrelationIdentifier("old"), Target: NamedCorrelationIdentifier("new")}))
	if result != v {
		t.Fatal("NOT with no matching aliases should return same pointer")
	}
}

func TestRebaseValue_AggregateValue_Rebases(t *testing.T) {
	t.Parallel()
	oldAlias := NamedCorrelationIdentifier("old")
	newAlias := NamedCorrelationIdentifier("new")
	v := NewAggregateValue(AggSum, mustQOV(t, oldAlias))
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: oldAlias, Target: newAlias}))
	agg, ok := result.(*AggregateValue)
	if !ok {
		t.Fatalf("expected *AggregateValue, got %T", result)
	}
	if agg.Op != AggSum {
		t.Fatalf("op = %v, want AggSum", agg.Op)
	}
	qov, ok := agg.Operand.(*quantifiedObjectValue)
	if !ok {
		t.Fatalf("expected operand to be *quantifiedObjectValue, got %T", agg.Operand)
	}
	if qov.Correlation() != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation())
	}
}

func TestRebaseValue_AggregateValue_NoChange(t *testing.T) {
	t.Parallel()
	v := NewAggregateValue(AggSum, &ConstantValue{Value: 42})
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: NamedCorrelationIdentifier("old"), Target: NamedCorrelationIdentifier("new")}))
	if result != v {
		t.Fatal("aggregate with no matching aliases should return same pointer")
	}
}

func TestRebaseValue_AggregateValue_CountStar(t *testing.T) {
	t.Parallel()
	v := NewAggregateValue(AggCountStar, nil)
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: NamedCorrelationIdentifier("old"), Target: NamedCorrelationIdentifier("new")}))
	if result != v {
		t.Fatal("COUNT(*) (nil operand) should return same pointer")
	}
}

func TestRebaseValue_QuantifiedRecordValue(t *testing.T) {
	t.Parallel()
	oldAlias := NamedCorrelationIdentifier("old")
	newAlias := NamedCorrelationIdentifier("new")
	v := &QuantifiedRecordValue{Alias: oldAlias, ResultType: NullableLong}
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: oldAlias, Target: newAlias}))
	qrv, ok := result.(*QuantifiedRecordValue)
	if !ok {
		t.Fatalf("expected *QuantifiedRecordValue, got %T", result)
	}
	if qrv.Alias != newAlias {
		t.Fatalf("expected alias %v, got %v", newAlias, qrv.Alias)
	}
	if qrv.ResultType != NullableLong {
		t.Fatal("ResultType should be preserved")
	}
}

func TestRebaseValue_ExistsValue(t *testing.T) {
	t.Parallel()
	oldAlias := NamedCorrelationIdentifier("old")
	newAlias := NamedCorrelationIdentifier("new")
	v := mustExistsValue(t, oldAlias)
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: oldAlias, Target: newAlias}))
	ev, ok := result.(*ExistsValue)
	if !ok {
		t.Fatalf("expected *ExistsValue, got %T", result)
	}
	// RFC-141: the alias lives on the child QuantifiedObjectValue now.
	qov, ok := ev.GetChild().(*quantifiedObjectValue)
	if !ok {
		t.Fatalf("child should be *quantifiedObjectValue, got %T", ev.GetChild())
	}
	if qov.Correlation() != newAlias {
		t.Fatalf("expected alias %v, got %v", newAlias, qov.Correlation())
	}
}

func TestRebaseValue_ScalarSubqueryValue(t *testing.T) {
	t.Parallel()
	oldAlias := NamedCorrelationIdentifier("old")
	newAlias := NamedCorrelationIdentifier("new")
	v := &ScalarSubqueryValue{Alias: oldAlias}
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: oldAlias, Target: newAlias}))
	ssv, ok := result.(*ScalarSubqueryValue)
	if !ok {
		t.Fatalf("expected *ScalarSubqueryValue, got %T", result)
	}
	if ssv.Alias != newAlias {
		t.Fatalf("expected alias %v, got %v", newAlias, ssv.Alias)
	}
}

func TestRebaseValue_ObjectValue(t *testing.T) {
	t.Parallel()
	oldAlias := NamedCorrelationIdentifier("old")
	newAlias := NamedCorrelationIdentifier("new")
	v := &ObjectValue{Alias: oldAlias, ResultType: TypeString}
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: oldAlias, Target: newAlias}))
	ov, ok := result.(*ObjectValue)
	if !ok {
		t.Fatalf("expected *ObjectValue, got %T", result)
	}
	if ov.Alias != newAlias {
		t.Fatalf("expected alias %v, got %v", newAlias, ov.Alias)
	}
	if ov.ResultType != TypeString {
		t.Fatal("ResultType should be preserved")
	}
}

func TestRebaseValue_AndOrValue_Generic(t *testing.T) {
	t.Parallel()
	oldAlias := NamedCorrelationIdentifier("old")
	newAlias := NamedCorrelationIdentifier("new")
	v := NewAndOrValue(AndOrAnd, mustQOV(t, oldAlias), &ConstantValue{Value: true})
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: oldAlias, Target: newAlias}))
	aov, ok := result.(*AndOrValue)
	if !ok {
		t.Fatalf("expected *AndOrValue, got %T", result)
	}
	if aov.Op != AndOrAnd {
		t.Fatalf("op should be AND, got %v", aov.Op)
	}
	qov, ok := aov.Left.(*quantifiedObjectValue)
	if !ok {
		t.Fatalf("expected Left to be *quantifiedObjectValue, got %T", aov.Left)
	}
	if qov.Correlation() != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation())
	}
}

func TestRebaseValue_LikeOperatorValue_Generic(t *testing.T) {
	t.Parallel()
	oldAlias := NamedCorrelationIdentifier("old")
	newAlias := NamedCorrelationIdentifier("new")
	v := &LikeOperatorValue{
		Probe:   mustQOV(t, oldAlias),
		Pattern: &ConstantValue{Value: "%test%"},
	}
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: oldAlias, Target: newAlias}))
	lv, ok := result.(*LikeOperatorValue)
	if !ok {
		t.Fatalf("expected *LikeOperatorValue, got %T", result)
	}
	qov, ok := lv.Probe.(*quantifiedObjectValue)
	if !ok {
		t.Fatalf("expected Probe to be *quantifiedObjectValue, got %T", lv.Probe)
	}
	if qov.Correlation() != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation())
	}
}

func TestRebaseValue_PickValue_Generic(t *testing.T) {
	t.Parallel()
	oldAlias := NamedCorrelationIdentifier("old")
	newAlias := NamedCorrelationIdentifier("new")
	v := &PickValue{
		Selector:     &ConstantValue{Value: 0},
		Alternatives: []Value{mustQOV(t, oldAlias), &ConstantValue{Value: 42}},
		Typ:          UnknownType,
	}
	result := RebaseValue(v, mustAliasMap(t, AliasPair{Source: oldAlias, Target: newAlias}))
	pv, ok := result.(*PickValue)
	if !ok {
		t.Fatalf("expected *PickValue, got %T", result)
	}
	qov, ok := pv.Alternatives[0].(*quantifiedObjectValue)
	if !ok {
		t.Fatalf("expected Alternatives[0] to be *quantifiedObjectValue, got %T", pv.Alternatives[0])
	}
	if qov.Correlation() != newAlias {
		t.Fatalf("expected rebased correlation %v, got %v", newAlias, qov.Correlation())
	}
}

func TestRebaseValue_CorrelationRoundTrip(t *testing.T) {
	t.Parallel()
	oldA := NamedCorrelationIdentifier("a")
	oldB := NamedCorrelationIdentifier("b")
	newA := NamedCorrelationIdentifier("a_prime")
	newB := NamedCorrelationIdentifier("b_prime")

	tree := &ArithmeticValue{
		Op:    OpAdd,
		Left:  mustQOV(t, oldA),
		Right: &ScalarFunctionValue{FuncName: "F", Args: []Value{mustQOV(t, oldB)}, Typ: UnknownType},
	}

	before := GetCorrelatedToOfValue(tree)
	if _, ok := before[oldA]; !ok {
		t.Fatal("before: missing oldA")
	}
	if _, ok := before[oldB]; !ok {
		t.Fatal("before: missing oldB")
	}

	rebased := RebaseValue(tree, mustAliasMap(t,
		AliasPair{Source: oldA, Target: newA},
		AliasPair{Source: oldB, Target: newB},
	))
	after := GetCorrelatedToOfValue(rebased)
	if _, ok := after[newA]; !ok {
		t.Fatal("after: missing newA")
	}
	if _, ok := after[newB]; !ok {
		t.Fatal("after: missing newB")
	}
	if _, ok := after[oldA]; ok {
		t.Fatal("after: old alias A should not be present")
	}
	if _, ok := after[oldB]; ok {
		t.Fatal("after: old alias B should not be present")
	}
}

func TestRebaseValue_LeafNoChange(t *testing.T) {
	t.Parallel()
	aliases := mustAliasMap(t, AliasPair{Source: NamedCorrelationIdentifier("old"), Target: NamedCorrelationIdentifier("new")})
	leaves := []Value{
		&fieldValue{Field: "x"},
		&ConstantValue{Value: 42},
		&NullValue{},
		&BooleanValue{},
		&ParameterValue{Ordinal: 1},
		&EmptyValue{},
		&IncarnationValue{},
	}
	for _, v := range leaves {
		result := RebaseValue(v, aliases)
		if result != v {
			t.Fatalf("%T should return same pointer (no correlation)", v)
		}
	}
}

func TestUnmatchedAggregateValue_FullIntegration(t *testing.T) {
	t.Parallel()
	id := UniqueUnmatchedID()
	v := NewUnmatchedAggregateValue(id)

	// Name and Type
	if v.Name() != "unmatched_aggregate" {
		t.Fatalf("Name() = %q, want unmatched_aggregate", v.Name())
	}
	if v.Type() != UnknownType {
		t.Fatal("Type() should be UnknownType")
	}

	// Children (leaf)
	if len(v.Children()) != 0 {
		t.Fatal("should have no children")
	}

	// GetCorrelatedTo
	corr := GetCorrelatedToOfValue(v)
	if _, ok := corr[id]; !ok {
		t.Fatal("UnmatchedID should be in correlation set")
	}

	// IsConstantValue
	if IsConstantValue(v) {
		t.Fatal("should not be constant")
	}

	// RebaseValue
	newID := NamedCorrelationIdentifier("new_id")
	rebased := RebaseValue(v, mustAliasMap(t, AliasPair{Source: id, Target: newID}))
	uav, ok := rebased.(*UnmatchedAggregateValue)
	if !ok {
		t.Fatalf("rebased should be *UnmatchedAggregateValue, got %T", rebased)
	}
	if uav.UnmatchedID != newID {
		t.Fatalf("rebased ID = %v, want %v", uav.UnmatchedID, newID)
	}

	// ValuesStructurallyEqual
	v2 := NewUnmatchedAggregateValue(id)
	if !ValuesStructurallyEqual(v, v2) {
		t.Fatal("two UAVs with same ID should be structurally equal")
	}
	v3 := NewUnmatchedAggregateValue(newID)
	if ValuesStructurallyEqual(v, v3) {
		t.Fatal("two UAVs with different IDs should not be equal")
	}

	// ExplainValue
	explain := ExplainValue(v)
	if explain == "" {
		t.Fatal("ExplainValue should return non-empty")
	}
}

func TestValuesStructurallyEqual_Nil(t *testing.T) {
	t.Parallel()
	if !ValuesStructurallyEqual(nil, nil) {
		t.Fatal("nil == nil should be true")
	}
	if ValuesStructurallyEqual(nil, &fieldValue{Field: "x"}) {
		t.Fatal("nil != non-nil should be false")
	}
}

func TestValuesStructurallyEqual_SamePointer(t *testing.T) {
	t.Parallel()
	v := &fieldValue{Field: "x"}
	if !ValuesStructurallyEqual(v, v) {
		t.Fatal("same pointer should be equal")
	}
}

func TestValuesStructurallyEqual_Leaf(t *testing.T) {
	t.Parallel()
	a := &fieldValue{Field: "COL"}
	b := &fieldValue{Field: "COL"}
	if !ValuesStructurallyEqual(a, b) {
		t.Fatal("identical FieldValues should be equal")
	}
	c := &fieldValue{Field: "OTHER"}
	if ValuesStructurallyEqual(a, c) {
		t.Fatal("different FieldValues should not be equal")
	}
}

func TestValuesStructurallyEqual_Composite(t *testing.T) {
	t.Parallel()
	a := &ArithmeticValue{Op: OpAdd, Left: &ConstantValue{Value: int64(1)}, Right: &ConstantValue{Value: int64(2)}}
	b := &ArithmeticValue{Op: OpAdd, Left: &ConstantValue{Value: int64(1)}, Right: &ConstantValue{Value: int64(2)}}
	if !ValuesStructurallyEqual(a, b) {
		t.Fatal("identical ArithmeticValues should be equal")
	}
	c := &ArithmeticValue{Op: OpSub, Left: &ConstantValue{Value: int64(1)}, Right: &ConstantValue{Value: int64(2)}}
	if ValuesStructurallyEqual(a, c) {
		t.Fatal("different ops should not be equal")
	}
}

func TestValuesStructurallyEqual_DifferentTypes(t *testing.T) {
	t.Parallel()
	a := &fieldValue{Field: "x"}
	b := &ConstantValue{Value: "x"}
	if ValuesStructurallyEqual(a, b) {
		t.Fatal("different types should not be equal")
	}
}

func TestValuesStructurallyEqual_AndOrValue(t *testing.T) {
	t.Parallel()
	a := NewAndOrValue(AndOrAnd, &ConstantValue{Value: true}, &ConstantValue{Value: false})
	b := NewAndOrValue(AndOrAnd, &ConstantValue{Value: true}, &ConstantValue{Value: false})
	if !ValuesStructurallyEqual(a, b) {
		t.Fatal("identical AndOrValues should be equal")
	}
	c := NewAndOrValue(AndOrOr, &ConstantValue{Value: true}, &ConstantValue{Value: false})
	if ValuesStructurallyEqual(a, c) {
		t.Fatal("different ops should not be equal")
	}
}

func TestValuesStructurallyEqual_RecordConstructor(t *testing.T) {
	t.Parallel()
	a := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "a", Value: &ConstantValue{Value: int64(1)}},
		{Name: "b", Value: &ConstantValue{Value: int64(2)}},
	}}
	b := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "a", Value: &ConstantValue{Value: int64(1)}},
		{Name: "b", Value: &ConstantValue{Value: int64(2)}},
	}}
	if !ValuesStructurallyEqual(a, b) {
		t.Fatal("identical RecordConstructorValues should be equal")
	}
	c := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "a", Value: &ConstantValue{Value: int64(1)}},
		{Name: "DIFFERENT", Value: &ConstantValue{Value: int64(2)}},
	}}
	if ValuesStructurallyEqual(a, c) {
		t.Fatal("different field names should not be equal")
	}
}
