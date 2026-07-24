package values

import (
	"testing"
)

func requireCandidateAnchoredField(
	t *testing.T,
	value Value,
	candidateAlias CorrelationIdentifier,
) *FieldValue {
	t.Helper()
	field, ok := value.(*FieldValue)
	if !ok {
		t.Fatalf("pulled-up value = %T, want *FieldValue", value)
	}
	qov, ok := field.Child.(*QuantifiedObjectValue)
	if !ok {
		t.Fatalf("pulled-up field child = %T, want *QuantifiedObjectValue", field.Child)
	}
	if qov.Correlation != candidateAlias {
		t.Fatalf(
			"pulled-up field correlation = %v, want candidate alias %v",
			qov.Correlation,
			candidateAlias,
		)
	}
	correlated := GetCorrelatedToOfValue(field)
	if len(correlated) != 1 {
		t.Fatalf(
			"pulled-up field correlations = %v, want only %v",
			correlated,
			candidateAlias,
		)
	}
	if _, ok := correlated[candidateAlias]; !ok {
		t.Fatalf(
			"pulled-up field correlations = %v, missing %v",
			correlated,
			candidateAlias,
		)
	}
	return field
}

func TestPullUpValue_ExactMatch(t *testing.T) {
	t.Parallel()
	// v equals resultValue → QuantifiedObjectValue(alias)
	alias := NamedCorrelationIdentifier("q1")
	v := &FieldValue{Field: "x", Typ: NullableString}
	result := &FieldValue{Field: "x", Typ: NullableString}

	pulled := PullUpValue(v, result, alias)
	if pulled == nil {
		t.Fatal("expected non-nil result")
	}
	qov, ok := pulled.(*QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected QuantifiedObjectValue, got %T", pulled)
	}
	if qov.Correlation != alias {
		t.Fatalf("expected alias %v, got %v", alias, qov.Correlation)
	}
}

func TestPullUpValue_ThroughRecordConstructor(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	// resultValue = RecordConstructor(a=FV("x"), b=FV("y"))
	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &FieldValue{Field: "x", Typ: NullableLong}},
		RecordConstructorField{Name: "b", Value: &FieldValue{Field: "y", Typ: NullableString}},
	)

	// PullUp FV("x") → FV(QOV(q1), "a")
	pulled := PullUpValue(&FieldValue{Field: "x", Typ: NullableLong}, resultValue, alias)
	if pulled == nil {
		t.Fatal("expected non-nil result for FV(x)")
	}
	fv, ok := pulled.(*FieldValue)
	if !ok {
		t.Fatalf("expected FieldValue, got %T", pulled)
	}
	if fv.Field != "a" {
		t.Fatalf("expected field 'a', got %q", fv.Field)
	}

	// PullUp FV("y") → FV(QOV(q1), "b")
	pulled = PullUpValue(&FieldValue{Field: "y", Typ: NullableString}, resultValue, alias)
	if pulled == nil {
		t.Fatal("expected non-nil result for FV(y)")
	}
	fv, ok = pulled.(*FieldValue)
	if !ok {
		t.Fatalf("expected FieldValue, got %T", pulled)
	}
	if fv.Field != "b" {
		t.Fatalf("expected field 'b', got %q", fv.Field)
	}
}

func TestPullUpValue_SourceLocalOrderingKeyThroughOwnedJoinField(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("C")
	qualifiedName := NewCorrelatedFieldValueWithResolvedOrdinal(
		NewQuantifiedObjectValue(alias), "NAME", 1, UnknownType)
	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "NAME", Value: qualifiedName},
	)

	pulled := PullUpValue(
		NewFlatFieldValue("name", UnknownType),
		resultValue,
		alias,
	)
	field := requireCandidateAnchoredField(t, pulled, alias)
	if field.Field != "NAME" {
		t.Fatalf("pulled field = %q, want NAME", field.Field)
	}
}

func TestPullUpValue_ThroughRecordConstructor_NotFound(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &FieldValue{Field: "x", Typ: NullableLong}},
	)

	// FV("z") is not in the constructor.
	pulled := PullUpValue(&FieldValue{Field: "z", Typ: NullableLong}, resultValue, alias)
	if pulled != nil {
		t.Fatalf("expected nil for unmapped field, got %v", pulled)
	}
}

func TestPullUpValue_ThroughRecordConstructor_ArithmeticChild(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	// resultValue = RecordConstructor(sum = (x + y))
	arith := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &FieldValue{Field: "x", Typ: NullableLong},
		Right: &FieldValue{Field: "y", Typ: NullableLong},
	}
	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "sum", Value: arith},
	)

	// PullUp (x + y) → FV(QOV(q1), "sum")
	vArith := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &FieldValue{Field: "x", Typ: NullableLong},
		Right: &FieldValue{Field: "y", Typ: NullableLong},
	}
	pulled := PullUpValue(vArith, resultValue, alias)
	if pulled == nil {
		t.Fatal("expected non-nil result for arithmetic match")
	}
	fv, ok := pulled.(*FieldValue)
	if !ok {
		t.Fatalf("expected FieldValue, got %T", pulled)
	}
	if fv.Field != "sum" {
		t.Fatalf("expected field 'sum', got %q", fv.Field)
	}
}

func TestPullUpValue_ThroughQOV(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q_out")
	innerAlias := NamedCorrelationIdentifier("q_in")

	// resultValue = QOV(q_in) — passthrough
	resultValue := &QuantifiedObjectValue{Correlation: innerAlias, Typ: UnknownType}

	// PullUp FV("col") through passthrough → FV(QOV(q_out), "col")
	pulled := PullUpValue(&FieldValue{Field: "col", Typ: NullableLong}, resultValue, alias)
	if pulled == nil {
		t.Fatal("expected non-nil result")
	}
	fv, ok := pulled.(*FieldValue)
	if !ok {
		t.Fatalf("expected FieldValue, got %T", pulled)
	}
	if fv.Field != "col" {
		t.Fatalf("expected field 'col', got %q", fv.Field)
	}
}

func TestPullUpValue_RecordConstructorAnchorsDuplicateFieldsOnCandidate(t *testing.T) {
	t.Parallel()

	leftAlias := NamedCorrelationIdentifier("left")
	rightAlias := NamedCorrelationIdentifier("right")
	candidateAlias := NamedCorrelationIdentifier("candidate")
	sourceType := NewRecordType("", false, []Field{{
		Name:      "ID",
		FieldType: NotNullLong,
		Ordinal:   0,
	}})
	leftID := NewCorrelatedFieldValueWithResolvedOrdinal(
		NewQuantifiedObjectValueOfType(leftAlias, sourceType),
		"ID",
		0,
		NotNullLong,
	)
	rightID := NewCorrelatedFieldValueWithResolvedOrdinal(
		NewQuantifiedObjectValueOfType(rightAlias, sourceType),
		"ID",
		0,
		NotNullLong,
	)
	resultValue := NewRawRecordConstructorValue(
		RecordConstructorField{Name: "ID", Value: leftID},
		RecordConstructorField{Name: "ID", Value: rightID},
	)

	pulledLeft := requireCandidateAnchoredField(
		t,
		PullUpValue(leftID, resultValue, candidateAlias),
		candidateAlias,
	)
	pulledRight := requireCandidateAnchoredField(
		t,
		PullUpValue(rightID, resultValue, candidateAlias),
		candidateAlias,
	)
	if pulledLeft.Resolved == nil || pulledLeft.Resolved.Root().Ordinal != 0 {
		t.Fatalf(
			"left duplicate path = %+v, want output ordinal 0",
			pulledLeft.Resolved,
		)
	}
	if pulledRight.Resolved == nil || pulledRight.Resolved.Root().Ordinal != 1 {
		t.Fatalf(
			"right duplicate path = %+v, want output ordinal 1",
			pulledRight.Resolved,
		)
	}
	if ValuesStructurallyEqual(pulledLeft, pulledRight) {
		t.Fatal("duplicate output fields with different ordinals conflated")
	}
	for _, sourceAlias := range []CorrelationIdentifier{leftAlias, rightAlias} {
		if _, leaked := GetCorrelatedToOfValue(pulledLeft)[sourceAlias]; leaked {
			t.Fatalf("left pulled-up field retained source alias %v", sourceAlias)
		}
		if _, leaked := GetCorrelatedToOfValue(pulledRight)[sourceAlias]; leaked {
			t.Fatalf("right pulled-up field retained source alias %v", sourceAlias)
		}
	}
}

func TestPullUpValue_PassthroughAnchorsSameNamedFieldsPerCandidate(t *testing.T) {
	t.Parallel()

	sourceType := NewRecordType("", false, []Field{{
		Name:      "ID",
		FieldType: NotNullLong,
		Ordinal:   0,
	}})
	leftSourceAlias := NamedCorrelationIdentifier("left_source")
	rightSourceAlias := NamedCorrelationIdentifier("right_source")
	leftCandidateAlias := NamedCorrelationIdentifier("left_candidate")
	rightCandidateAlias := NamedCorrelationIdentifier("right_candidate")
	leftSource := NewQuantifiedObjectValueOfType(leftSourceAlias, sourceType)
	rightSource := NewObjectValue(rightSourceAlias, sourceType)
	leftID := NewCorrelatedFieldValueWithResolvedOrdinal(
		leftSource,
		"ID",
		0,
		NotNullLong,
	)
	rightID := NewCorrelatedFieldValueWithResolvedOrdinal(
		rightSource,
		"ID",
		0,
		NotNullLong,
	)

	pulledLeft := requireCandidateAnchoredField(
		t,
		PullUpValue(leftID, leftSource, leftCandidateAlias),
		leftCandidateAlias,
	)
	pulledRight := requireCandidateAnchoredField(
		t,
		PullUpValue(rightID, rightSource, rightCandidateAlias),
		rightCandidateAlias,
	)
	if pulledLeft.Resolved != leftID.Resolved {
		t.Fatal("QOV passthrough did not preserve the resolved field path")
	}
	if pulledRight.Resolved != rightID.Resolved {
		t.Fatal("ObjectValue passthrough did not preserve the resolved field path")
	}
	if ValuesStructurallyEqual(pulledLeft, pulledRight) {
		t.Fatal("same-named fields from distinct candidate aliases conflated")
	}
}

func TestPullUpPushDown_PassthroughCorrelatedBakedPathRoundTrip(t *testing.T) {
	t.Parallel()

	sourceAlias := NamedCorrelationIdentifier("source")
	upperAlias := NamedCorrelationIdentifier("upper")
	source := NewQuantifiedObjectValue(sourceAlias)
	path := NewFieldPathOfSingle("NESTED", 0, true).
		WithSuffix(NewFieldPathOfSingle("ID", 1, false))
	original := &FieldValue{
		Field:    "ID",
		Typ:      NotNullLong,
		Child:    source,
		Resolved: path,
	}

	pulled := requireCandidateAnchoredField(
		t,
		PullUpValue(original, source, upperAlias),
		upperAlias,
	)
	if pulled.Resolved != path ||
		!pulled.Resolved.FrontierPinned ||
		len(pulled.Resolved.Accessors) != 2 {
		t.Fatalf(
			"pulled path = %+v, want original two-step pinned path",
			pulled.Resolved,
		)
	}

	pushed := PushDownValue(pulled, source, upperAlias)
	if !ValuesStructurallyEqual(pushed, original) {
		t.Fatalf(
			"passthrough round-trip = %s, want %s",
			ExplainValue(pushed),
			ExplainValue(original),
		)
	}
	pushedField := pushed.(*FieldValue)
	if pushedField.Child != source || pushedField.Resolved != path {
		t.Fatal("pushdown did not restore the source child and baked path")
	}
}

func TestPullUpPushDown_PassthroughRejectsForeignAlias(t *testing.T) {
	t.Parallel()

	sourceAlias := NamedCorrelationIdentifier("source")
	foreignAlias := NamedCorrelationIdentifier("foreign")
	upperAlias := NamedCorrelationIdentifier("upper")
	foreignUpperAlias := NamedCorrelationIdentifier("foreign_upper")
	source := NewQuantifiedObjectValue(sourceAlias)

	foreignSourceField := NewFieldValue(
		NewQuantifiedObjectValue(foreignAlias),
		"ID",
		NotNullLong,
	)
	if pulled := PullUpValue(
		foreignSourceField,
		source,
		upperAlias,
	); pulled != nil {
		t.Fatalf(
			"foreign-source pull-up = %s, want nil",
			ExplainValue(pulled),
		)
	}

	foreignUpperField := NewFieldValue(
		NewQuantifiedObjectValue(foreignUpperAlias),
		"ID",
		NotNullLong,
	)
	if pushed := PushDownValue(
		foreignUpperField,
		source,
		upperAlias,
	); pushed != nil {
		t.Fatalf(
			"foreign-upper pushdown = %s, want nil",
			ExplainValue(pushed),
		)
	}
}

func TestPullUpPushDown_PassthroughRejectsLegacyChainedNestedPath(
	t *testing.T,
) {
	t.Parallel()

	sourceAlias := NamedCorrelationIdentifier("source")
	upperAlias := NamedCorrelationIdentifier("upper")
	source := NewQuantifiedObjectValue(sourceAlias)
	sourceNested := NewFieldValue(
		NewFieldValue(source, "NESTED", UnknownType),
		"ID",
		NotNullLong,
	)
	if pulled := PullUpValue(
		sourceNested,
		source,
		upperAlias,
	); pulled != nil {
		t.Fatalf(
			"legacy chained pull-up = %s, want nil rather than a dropped inner path",
			ExplainValue(pulled),
		)
	}

	upperNested := NewFieldValue(
		NewFieldValue(
			NewQuantifiedObjectValue(upperAlias),
			"NESTED",
			UnknownType,
		),
		"ID",
		NotNullLong,
	)
	if pushed := PushDownValue(
		upperNested,
		source,
		upperAlias,
	); pushed != nil {
		t.Fatalf(
			"legacy chained pushdown = %s, want nil rather than a dropped inner path",
			ExplainValue(pushed),
		)
	}
}

func TestPullUpValue_WholeResultStillMapsToCandidateQOV(t *testing.T) {
	t.Parallel()

	candidateAlias := NamedCorrelationIdentifier("candidate")
	sourceAlias := NamedCorrelationIdentifier("source")
	results := []Value{
		NewRecordConstructorValue(RecordConstructorField{
			Name:  "ID",
			Value: NewFlatFieldValue("ID", NotNullLong),
		}),
		NewQuantifiedObjectValue(sourceAlias),
		NewObjectValue(sourceAlias, UnknownType),
	}
	for _, resultValue := range results {
		resultValue := resultValue
		t.Run(resultValue.Name(), func(t *testing.T) {
			pulled := PullUpValue(resultValue, resultValue, candidateAlias)
			qov, ok := pulled.(*QuantifiedObjectValue)
			if !ok {
				t.Fatalf("whole-result pull-up = %T, want *QuantifiedObjectValue", pulled)
			}
			if qov.Correlation != candidateAlias {
				t.Fatalf(
					"whole-result correlation = %v, want %v",
					qov.Correlation,
					candidateAlias,
				)
			}
		})
	}
}

func TestPullUpValue_Nil(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")
	if PullUpValue(nil, &FieldValue{Field: "x"}, alias) != nil {
		t.Fatal("expected nil for nil v")
	}
	if PullUpValue(&FieldValue{Field: "x"}, nil, alias) != nil {
		t.Fatal("expected nil for nil resultValue")
	}
}

func TestPushDownValue_ThroughRecordConstructor(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	// resultValue = RecordConstructor(a=FV("x"), b=FV("y"))
	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &FieldValue{Field: "x", Typ: NullableLong}},
		RecordConstructorField{Name: "b", Value: &FieldValue{Field: "y", Typ: NullableString}},
	)

	// PushDown FV("a") → FV("x")
	pushed := PushDownValue(&FieldValue{Field: "a", Typ: NullableLong}, resultValue, alias)
	if pushed == nil {
		t.Fatal("expected non-nil result for FV(a)")
	}
	fv, ok := pushed.(*FieldValue)
	if !ok {
		t.Fatalf("expected FieldValue, got %T", pushed)
	}
	if fv.Field != "x" {
		t.Fatalf("expected field 'x', got %q", fv.Field)
	}

	// PushDown FV("b") → FV("y")
	pushed = PushDownValue(&FieldValue{Field: "b", Typ: NullableString}, resultValue, alias)
	if pushed == nil {
		t.Fatal("expected non-nil result for FV(b)")
	}
	fv, ok = pushed.(*FieldValue)
	if !ok {
		t.Fatalf("expected FieldValue, got %T", pushed)
	}
	if fv.Field != "y" {
		t.Fatalf("expected field 'y', got %q", fv.Field)
	}
}

func TestPushDownValue_QOVReplacedByResultValue(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &FieldValue{Field: "x", Typ: NullableLong}},
	)

	// PushDown QOV(q1) → resultValue itself
	v := &QuantifiedObjectValue{Correlation: alias, Typ: UnknownType}
	pushed := PushDownValue(v, resultValue, alias)
	if pushed == nil {
		t.Fatal("expected non-nil result")
	}
	rc, ok := pushed.(*RecordConstructorValue)
	if !ok {
		t.Fatalf("expected RecordConstructorValue, got %T", pushed)
	}
	if len(rc.Fields) != 1 || rc.Fields[0].Name != "a" {
		t.Fatalf("expected 1-field record constructor with field 'a', got %v", rc.Fields)
	}
}

func TestPushDownValue_ThroughRecordConstructor_NotFound(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &FieldValue{Field: "x", Typ: NullableLong}},
	)

	// FV("z") not in constructor.
	pushed := PushDownValue(&FieldValue{Field: "z"}, resultValue, alias)
	if pushed != nil {
		t.Fatalf("expected nil for unmapped field, got %v", pushed)
	}
}

func TestPushDownValue_ThroughQOV(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q_out")
	innerAlias := NamedCorrelationIdentifier("q_in")

	// Passthrough result
	resultValue := &QuantifiedObjectValue{Correlation: innerAlias, Typ: UnknownType}

	// PushDown FV("col") through passthrough → FV("col")
	pushed := PushDownValue(&FieldValue{Field: "col", Typ: NullableLong}, resultValue, alias)
	if pushed == nil {
		t.Fatal("expected non-nil result")
	}
	fv, ok := pushed.(*FieldValue)
	if !ok {
		t.Fatalf("expected FieldValue, got %T", pushed)
	}
	if fv.Field != "col" {
		t.Fatalf("expected field 'col', got %q", fv.Field)
	}
	if fv.Child != nil {
		t.Fatalf(
			"legacy flat pushdown child = %T, want nil",
			fv.Child,
		)
	}
}

func TestPushDownValue_Nil(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")
	if PushDownValue(nil, &FieldValue{Field: "x"}, alias) != nil {
		t.Fatal("expected nil for nil v")
	}
	if PushDownValue(&FieldValue{Field: "x"}, nil, alias) != nil {
		t.Fatal("expected nil for nil resultValue")
	}
}

func TestPullUpPushDown_RoundTrip(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	// resultValue = RecordConstructor(a=FV("x"), b=FV("y"))
	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &FieldValue{Field: "x", Typ: NullableLong}},
		RecordConstructorField{Name: "b", Value: &FieldValue{Field: "y", Typ: NullableString}},
	)

	original := &FieldValue{Field: "x", Typ: NullableLong}

	// PullUp: FV("x") → FV(QOV(q1), "a")
	pulled := PullUpValue(original, resultValue, alias)
	if pulled == nil {
		t.Fatal("pullUp failed")
	}

	// PushDown: FV("a") → FV("x")
	pushed := PushDownValue(pulled, resultValue, alias)
	if pushed == nil {
		t.Fatal("pushDown failed")
	}

	if ExplainValue(pushed) != ExplainValue(original) {
		t.Fatalf("round-trip failed: got %q, want %q",
			ExplainValue(pushed), ExplainValue(original))
	}
}

func TestPullUpValues_Batch(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &FieldValue{Field: "x", Typ: NullableLong}},
		RecordConstructorField{Name: "b", Value: &FieldValue{Field: "y", Typ: NullableString}},
	)

	vs := []Value{
		&FieldValue{Field: "x", Typ: NullableLong},
		&FieldValue{Field: "y", Typ: NullableString},
		&FieldValue{Field: "z", Typ: NullableLong}, // not in constructor
	}

	result := PullUpValues(vs, resultValue, alias)
	if len(result) != 2 {
		t.Fatalf("expected 2 mapped values, got %d", len(result))
	}
}

func TestPushDownValues_Batch(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &FieldValue{Field: "x", Typ: NullableLong}},
		RecordConstructorField{Name: "b", Value: &FieldValue{Field: "y", Typ: NullableString}},
	)

	vs := []Value{
		&FieldValue{Field: "a", Typ: NullableLong},
		&FieldValue{Field: "b", Typ: NullableString},
		&FieldValue{Field: "z", Typ: NullableLong}, // not in constructor
	}

	result := PushDownValues(vs, resultValue, alias)
	if len(result) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(result))
	}
	if result[0] == nil || ExplainValue(result[0]) != "x" {
		t.Fatalf("expected FV(x), got %v", result[0])
	}
	if result[1] == nil || ExplainValue(result[1]) != "y" {
		t.Fatalf("expected FV(y), got %v", result[1])
	}
	if result[2] != nil {
		t.Fatalf("expected nil for unmapped field, got %v", result[2])
	}
}

func TestSemanticEqual(t *testing.T) {
	t.Parallel()
	a := &FieldValue{Field: "x", Typ: NullableLong}
	b := &FieldValue{Field: "x", Typ: NullableLong}
	c := &FieldValue{Field: "y", Typ: NullableLong}

	if !semanticEqual(a, b) {
		t.Fatal("expected a == b")
	}
	if semanticEqual(a, c) {
		t.Fatal("expected a != c")
	}
	if semanticEqual(nil, a) {
		t.Fatal("expected nil != a")
	}
	if semanticEqual(a, nil) {
		t.Fatal("expected a != nil")
	}
}
