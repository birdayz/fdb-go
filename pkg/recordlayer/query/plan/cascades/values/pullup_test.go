package values

import (
	"testing"
)

func requireCandidateAnchoredField(
	t *testing.T,
	value Value,
	candidateAlias CorrelationIdentifier,
) *fieldValue {
	t.Helper()
	field, ok := value.(*fieldValue)
	if !ok {
		t.Fatalf("pulled-up value = %T, want *fieldValue", value)
	}
	qov, ok := field.Child.(*quantifiedObjectValue)
	if !ok {
		t.Fatalf("pulled-up field child = %T, want *quantifiedObjectValue", field.Child)
	}
	if qov.Correlation() != candidateAlias {
		t.Fatalf(
			"pulled-up field correlation = %v, want candidate alias %v",
			qov.Correlation(),
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
	v := &fieldValue{Field: "x", Typ: NullableString}
	result := &fieldValue{Field: "x", Typ: NullableString}

	pulled := mustPullUpValue(t, v, result, alias)
	if pulled == nil {
		t.Fatal("expected non-nil result")
	}
	qov, ok := pulled.(*quantifiedObjectValue)
	if !ok {
		t.Fatalf("expected QuantifiedObjectValue, got %T", pulled)
	}
	if qov.Correlation() != alias {
		t.Fatalf("expected alias %v, got %v", alias, qov.Correlation())
	}
}

func TestPullUpValue_ThroughRecordConstructor(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	// resultValue = RecordConstructor(a=FV("x"), b=FV("y"))
	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &fieldValue{Field: "x", Typ: NullableLong}},
		RecordConstructorField{Name: "b", Value: &fieldValue{Field: "y", Typ: NullableString}},
	)

	// PullUp FV("x") → FV(QOV(q1), "a")
	pulled := mustPullUpValue(t, &fieldValue{Field: "x", Typ: NullableLong}, resultValue, alias)
	if pulled == nil {
		t.Fatal("expected non-nil result for FV(x)")
	}
	fv, ok := pulled.(*fieldValue)
	if !ok {
		t.Fatalf("expected FieldValue, got %T", pulled)
	}
	if fv.Field != "a" {
		t.Fatalf("expected field 'a', got %q", fv.Field)
	}

	// PullUp FV("y") → FV(QOV(q1), "b")
	pulled = mustPullUpValue(t, &fieldValue{Field: "y", Typ: NullableString}, resultValue, alias)
	if pulled == nil {
		t.Fatal("expected non-nil result for FV(y)")
	}
	fv, ok = pulled.(*fieldValue)
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
	qualifiedName := newCorrelatedFieldValueWithResolvedOrdinal(
		mustQOV(t, alias), "NAME", 1, NotNullString)
	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "NAME", Value: qualifiedName},
	)

	pulled := mustPullUpValue(t,
		newFlatFieldValue("name", NotNullString),
		resultValue,
		alias)

	field := requireCandidateAnchoredField(t, pulled, alias)
	if field.Field != "NAME" {
		t.Fatalf("pulled field = %q, want NAME", field.Field)
	}
}

func TestPullUpPushDownValue_RecordConstructorNormalizesLogicalSourceNameOnly(t *testing.T) {
	t.Parallel()

	alias := NamedCorrelationIdentifier("T")
	logicalType := NewRecordType("T", false, []Field{
		{Name: "ID", Ordinal: 0, FieldType: NotNullLong},
		{Name: "V", Ordinal: 1, FieldType: NullableLong},
	})
	physicalType := NewRecordType("", false, []Field{
		{Name: "ID", Ordinal: 0, FieldType: NotNullLong},
		{Name: "V", Ordinal: 1, FieldType: NullableLong},
	})
	logical := mustQOV(t, alias, logicalType)
	physical := mustQOV(t, alias, physicalType)
	logicalID, err := ResolveFieldOrdinals(logical, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	logicalV, err := ResolveFieldOrdinals(logical, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	physicalV, err := ResolveFieldOrdinals(physical, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	result := NewRawRecordConstructorValue(
		RecordConstructorField{Name: "ID", Value: logicalID},
		RecordConstructorField{Name: "V", Value: logicalV},
	)

	pulled := requireCandidateAnchoredField(
		t, mustPullUpValue(t, physicalV, result, alias), alias)
	if pulled.Resolved == nil || len(pulled.Resolved.Ordinals()) != 1 ||
		pulled.Resolved.Ordinals()[0] != 1 {
		t.Fatalf("pulled nominal source path = %v, want result ordinal [1]",
			pulled.Resolved)
	}
	pushed := PushDownValue(physicalV, result, alias)
	if !ValuesStructurallyEqual(pushed, logicalV) {
		t.Fatalf("pushed nominal source = %q, want exact retained source %q",
			ExplainValue(pushed), ExplainValue(logicalV))
	}

	foreign := mustQOV(t, NamedCorrelationIdentifier("foreign"), physicalType)
	foreignV, err := ResolveFieldOrdinals(foreign, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	narrow := mustQOV(t, alias, NewRecordType("", false, []Field{
		{Name: "V", Ordinal: 0, FieldType: NullableLong},
	}))
	narrowV, err := ResolveFieldOrdinals(narrow, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	drifted := mustQOV(t, alias, NewRecordType("", false, []Field{
		{Name: "ID", Ordinal: 0, FieldType: NotNullLong},
		{Name: "V", Ordinal: 1, FieldType: NullableString},
	}))
	driftedV, err := ResolveFieldOrdinals(drifted, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]Value{
		"foreign alias": foreignV,
		"narrow row":    narrowV,
		"leaf drift":    driftedV,
	} {
		value := value
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := mustPullUpValue(t, value, result, alias); got != nil {
				t.Fatalf("pull-up accepted %s as %q", name, ExplainValue(got))
			}
			if got := PushDownValue(value, result, alias); got != nil {
				t.Fatalf("push-down accepted %s as %q", name, ExplainValue(got))
			}
		})
	}
}

func TestPullUpValue_ThroughRecordConstructor_NotFound(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &fieldValue{Field: "x", Typ: NullableLong}},
	)

	// FV("z") is not in the constructor.
	pulled := mustPullUpValue(t, &fieldValue{Field: "z", Typ: NullableLong}, resultValue, alias)
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
		Left:  &fieldValue{Field: "x", Typ: NullableLong},
		Right: &fieldValue{Field: "y", Typ: NullableLong},
	}
	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "sum", Value: arith},
	)

	// PullUp (x + y) → FV(QOV(q1), "sum")
	vArith := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &fieldValue{Field: "x", Typ: NullableLong},
		Right: &fieldValue{Field: "y", Typ: NullableLong},
	}
	pulled := mustPullUpValue(t, vArith, resultValue, alias)
	if pulled == nil {
		t.Fatal("expected non-nil result for arithmetic match")
	}
	fv, ok := pulled.(*fieldValue)
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
	resultValue := mustQOV(t, innerAlias)

	// PullUp FV("col") through passthrough → FV(QOV(q_out), "col")
	pulled := mustPullUpValue(t, &fieldValue{Field: "col", Typ: NullableLong}, resultValue, alias)
	if pulled == nil {
		t.Fatal("expected non-nil result")
	}
	fv, ok := pulled.(*fieldValue)
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
	leftID := newCorrelatedFieldValueWithResolvedOrdinal(
		mustQOV(t, leftAlias, sourceType),
		"ID",
		0,
		NotNullLong,
	)
	rightID := newCorrelatedFieldValueWithResolvedOrdinal(
		mustQOV(t, rightAlias, sourceType),
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
		mustPullUpValue(t, leftID, resultValue, candidateAlias),
		candidateAlias,
	)
	pulledRight := requireCandidateAnchoredField(
		t,
		mustPullUpValue(t, rightID, resultValue, candidateAlias),
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
	leftSource := mustQOV(t, leftSourceAlias, sourceType)
	rightSource := NewObjectValue(rightSourceAlias, sourceType)
	leftID := newCorrelatedFieldValueWithResolvedOrdinal(
		leftSource,
		"ID",
		0,
		NotNullLong,
	)
	rightID := newCorrelatedFieldValueWithResolvedOrdinal(
		rightSource,
		"ID",
		0,
		NotNullLong,
	)

	pulledLeft := requireCandidateAnchoredField(
		t,
		mustPullUpValue(t, leftID, leftSource, leftCandidateAlias),
		leftCandidateAlias,
	)
	pulledRight := requireCandidateAnchoredField(
		t,
		mustPullUpValue(t, rightID, rightSource, rightCandidateAlias),
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
	source := mustQOV(t, sourceAlias)
	path := newFieldPathOfSingle("NESTED", 0, true).
		WithSuffix(newFieldPathOfSingle("ID", 1, false))
	original := &fieldValue{
		Field:    "ID",
		Typ:      NotNullLong,
		Child:    source,
		Resolved: path,
	}

	pulled := requireCandidateAnchoredField(
		t,
		mustPullUpValue(t, original, source, upperAlias),
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
	pushedField := pushed.(*fieldValue)
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
	source := mustQOV(t, sourceAlias)

	foreignSourceField := newFieldValue(
		mustQOV(t, foreignAlias),
		"ID",
		NotNullLong,
	)
	if pulled := mustPullUpValue(t,
		foreignSourceField,
		source,
		upperAlias); pulled != nil {
		t.Fatalf(
			"foreign-source pull-up = %s, want nil",
			ExplainValue(pulled),
		)
	}

	foreignUpperField := newFieldValue(
		mustQOV(t, foreignUpperAlias),
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
	source := mustQOV(t, sourceAlias)
	sourceNested := newFieldValue(
		newFieldValue(source, "NESTED", UnknownType),
		"ID",
		NotNullLong,
	)
	if pulled := mustPullUpValue(t,
		sourceNested,
		source,
		upperAlias); pulled != nil {
		t.Fatalf(
			"legacy chained pull-up = %s, want nil rather than a dropped inner path",
			ExplainValue(pulled),
		)
	}

	upperNested := newFieldValue(
		newFieldValue(
			mustQOV(t, upperAlias),
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
			Value: newFlatFieldValue("ID", NotNullLong),
		}),
		mustQOV(t, sourceAlias),
		NewObjectValue(sourceAlias, NotNullLong),
	}
	for _, resultValue := range results {
		resultValue := resultValue
		t.Run(resultValue.Name(), func(t *testing.T) {
			pulled := mustPullUpValue(t, resultValue, resultValue, candidateAlias)
			qov, ok := pulled.(*quantifiedObjectValue)
			if !ok {
				t.Fatalf("whole-result pull-up = %T, want *quantifiedObjectValue", pulled)
			}
			if qov.Correlation() != candidateAlias {
				t.Fatalf(
					"whole-result correlation = %v, want %v",
					qov.Correlation(),
					candidateAlias,
				)
			}
		})
	}
}

func TestPullUpValue_WholeResultMapsToOwnerScopedCurrentQOV(t *testing.T) {
	t.Parallel()

	resultType := NewRecordType("Row", false, []Field{
		{Name: "ID", FieldType: NotNullLong, Ordinal: 0},
	})
	resultValue := mustQOV(t, NamedCorrelationIdentifier("source"), resultType)
	pulled := mustPullUpValue(t, resultValue, resultValue, CurrentCorrelation())
	qov, ok := AsQuantifiedObjectValue(pulled)
	if !ok {
		t.Fatalf("current whole-result pull-up = %T, want exact QuantifiedObjectValue", pulled)
	}
	if qov.Correlation() != CurrentCorrelation() {
		t.Fatalf("whole-result correlation = %v, want reserved current", qov.Correlation())
	}
	if !qov.FlowedType().Equals(resultType) {
		t.Fatalf("whole-result type = %s, want %s", qov.FlowedType(), resultType)
	}
}

func TestPullUpValue_Nil(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")
	if mustPullUpValue(t, nil, &fieldValue{Field: "x"}, alias) != nil {
		t.Fatal("expected nil for nil v")
	}
	if mustPullUpValue(t, &fieldValue{Field: "x"}, nil, alias) != nil {
		t.Fatal("expected nil for nil resultValue")
	}
}

func TestPushDownValue_ThroughRecordConstructor(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	// resultValue = RecordConstructor(a=FV("x"), b=FV("y"))
	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &fieldValue{Field: "x", Typ: NullableLong}},
		RecordConstructorField{Name: "b", Value: &fieldValue{Field: "y", Typ: NullableString}},
	)

	// The pushed-down references address the constructor's OUTPUT SLOTS, which
	// is what a resolved reference to a projection output carries. The display
	// names are deliberately WRONG here — slot 0 is rendered "b" and slot 1 "a" —
	// so the test cannot pass by matching a name: only the ordinal selects.
	pushed := PushDownValue(newFieldValueWithResolvedOrdinal("b", 0, NullableLong), resultValue, alias)
	if pushed == nil {
		t.Fatal("expected non-nil result for output slot 0")
	}
	fv, ok := pushed.(*fieldValue)
	if !ok {
		t.Fatalf("expected FieldValue, got %T", pushed)
	}
	if fv.Field != "x" {
		t.Fatalf("output slot 0 pushed to %q, want the constructor's first input 'x'", fv.Field)
	}

	pushed = PushDownValue(newFieldValueWithResolvedOrdinal("a", 1, NullableString), resultValue, alias)
	if pushed == nil {
		t.Fatal("expected non-nil result for output slot 1")
	}
	fv, ok = pushed.(*fieldValue)
	if !ok {
		t.Fatalf("expected FieldValue, got %T", pushed)
	}
	if fv.Field != "y" {
		t.Fatalf("output slot 1 pushed to %q, want the constructor's second input 'y'", fv.Field)
	}

	// A LAZY reference has no ordinal, so it selects no member and declines —
	// resolution belongs upstream, at the one place a name is legitimate. This
	// is the arm RFC-197 item 3 removed; without this case the conversion is
	// unfalsifiable, since every other case here is baked.
	if got := PushDownValue(&fieldValue{Field: "a", Typ: NullableLong}, resultValue, alias); got != nil {
		t.Fatalf("a lazy reference matched a constructor member by NAME = %v, want DECLINE", got)
	}
}

func TestPushDownValue_QOVReplacedByResultValue(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &fieldValue{Field: "x", Typ: NullableLong}},
	)

	// PushDown QOV(q1) → resultValue itself
	v := mustQOV(t, alias)
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
		RecordConstructorField{Name: "a", Value: &fieldValue{Field: "x", Typ: NullableLong}},
	)

	// FV("z") not in constructor.
	pushed := PushDownValue(&fieldValue{Field: "z"}, resultValue, alias)
	if pushed != nil {
		t.Fatalf("expected nil for unmapped field, got %v", pushed)
	}
}

func TestPushDownValue_ThroughQOV(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q_out")
	innerAlias := NamedCorrelationIdentifier("q_in")

	// Passthrough result
	resultValue := mustQOV(t, innerAlias)

	// PushDown FV("col") through passthrough → FV("col")
	pushed := PushDownValue(&fieldValue{Field: "col", Typ: NullableLong}, resultValue, alias)
	if pushed == nil {
		t.Fatal("expected non-nil result")
	}
	fv, ok := pushed.(*fieldValue)
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
	if PushDownValue(nil, &fieldValue{Field: "x"}, alias) != nil {
		t.Fatal("expected nil for nil v")
	}
	if PushDownValue(&fieldValue{Field: "x"}, nil, alias) != nil {
		t.Fatal("expected nil for nil resultValue")
	}
}

func TestPullUpPushDown_RoundTrip(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	// The constructor's inputs are BAKED source-relative reads, which is what the
	// translator produces. Pull-up bakes a baked input (pullUpThroughRecordConstructor),
	// so the round trip closes on ORDINALS and never on a name.
	original := newFieldValueWithResolvedOrdinal("x", 0, NullableLong)
	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: original},
		RecordConstructorField{Name: "b", Value: newFieldValueWithResolvedOrdinal("y", 1, NullableString)},
	)

	// PullUp: FV("x")#0 → FV(QOV(q1), "a")#0
	pulled := mustPullUpValue(t, original, resultValue, alias)
	if pulled == nil {
		t.Fatal("pullUp failed")
	}
	if pfv, ok := pulled.(*fieldValue); !ok || pfv.Resolved == nil {
		t.Fatalf("pull-up through a record constructor must emit a BAKED reference to the "+
			"output slot; the push-down back down resolves by ordinal and a lazy node would "+
			"decline, got %v", pulled)
	}

	// PushDown: FV(QOV(q1), "a")#0 → FV("x")#0
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
		RecordConstructorField{Name: "a", Value: &fieldValue{Field: "x", Typ: NullableLong}},
		RecordConstructorField{Name: "b", Value: &fieldValue{Field: "y", Typ: NullableString}},
	)

	vs := []Value{
		&fieldValue{Field: "x", Typ: NullableLong},
		&fieldValue{Field: "y", Typ: NullableString},
		&fieldValue{Field: "z", Typ: NullableLong}, // not in constructor
	}

	result := mustPullUpValues(t, vs, resultValue, alias)
	if len(result) != 2 {
		t.Fatalf("expected 2 mapped values, got %d", len(result))
	}
}

func TestPushDownValues_Batch(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q1")

	resultValue := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &fieldValue{Field: "x", Typ: NullableLong}},
		RecordConstructorField{Name: "b", Value: &fieldValue{Field: "y", Typ: NullableString}},
	)

	vs := []Value{
		newFieldValueWithResolvedOrdinal("a", 0, NullableLong),
		newFieldValueWithResolvedOrdinal("b", 1, NullableString),
		newFieldValueWithResolvedOrdinal("z", 7, NullableLong), // no such output slot
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
	a := &fieldValue{Field: "x", Typ: NullableLong}
	b := &fieldValue{Field: "x", Typ: NullableLong}
	c := &fieldValue{Field: "y", Typ: NullableLong}

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

// TestPushDownValue_ThroughRecordConstructor_NestedOrdinalPath pins the
// two-domain translation used by UPDATE's exact {OLD,NEW} result: the root
// ordinal selects the computation column, then the suffix is resolved against
// that selected column's own exact row. Treating [0,0] as one flat constructor
// ordinal either declines OLD.ID or reads the wrong computation slot.
func TestPushDownValue_ThroughRecordConstructor_NestedOrdinalPath(t *testing.T) {
	t.Parallel()

	rowType := NewRecordType("update_row", false, []Field{
		{Name: "ID", FieldType: NotNullLong},
	})
	inputAlias := NamedCorrelationIdentifier("update_input")
	input := mustQOV(t, inputAlias, rowType)
	inputID, err := ResolveFieldOrdinals(input, []int{0})
	if err != nil {
		t.Fatalf("resolve input ID: %v", err)
	}
	computation := NewRawRecordConstructorValue(
		RecordConstructorField{Name: "OLD", Value: input},
		RecordConstructorField{
			Name:  "NEW",
			Value: NewObjectValue(NamedCorrelationIdentifier("updated_record"), rowType),
		},
	)
	// upperAlias is the correlation at which the computation row is visible;
	// it is not a second source identity. The nested suffix changes domains
	// after OLD is selected, while the root remains this upper boundary.
	output := mustQOV(t, inputAlias, computation.Type())
	oldID, err := ResolveFieldOrdinals(output, []int{0, 0})
	if err != nil {
		t.Fatalf("resolve OLD.ID: %v", err)
	}

	pushed := PushDownValue(oldID, computation, inputAlias)
	if !ValuesStructurallyEqual(pushed, inputID) {
		t.Fatalf("OLD.ID pushed to %q, want exact input ID %q",
			ExplainValue(pushed), ExplainValue(inputID))
	}

	newID, err := ResolveFieldOrdinals(output, []int{1, 0})
	if err != nil {
		t.Fatalf("resolve NEW.ID: %v", err)
	}
	if pushedNew := PushDownValue(newID, computation, inputAlias); pushedNew != nil {
		t.Fatalf("NEW.ID pushed to %q, want decline for post-mutation object",
			ExplainValue(pushedNew))
	}
}
