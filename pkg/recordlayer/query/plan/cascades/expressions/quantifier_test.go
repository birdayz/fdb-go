package expressions

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

type stubExpr struct{ name string }

func (s *stubExpr) GetResultValue() values.Value {
	return values.NewQueriedValue(nil, values.UnknownType)
}
func (s *stubExpr) GetQuantifiers() []Quantifier { return nil }
func (s *stubExpr) CanCorrelate() bool           { return false }
func (s *stubExpr) ChildrenAsSet() bool          { return false }
func (s *stubExpr) HashCodeWithoutChildren() uint64 {
	return 0
}

func (s *stubExpr) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return nil
}

func (s *stubExpr) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*stubExpr)
	return ok && o.name == s.name
}

func (s *stubExpr) WithQuantifiers(quantifiers []Quantifier) (RelationalExpression, error) {
	if err := requireQuantifierArity("stubExpr", len(quantifiers), 0); err != nil {
		return nil, err
	}
	return s, nil
}

type typedStubExpr struct {
	name string
	typ  values.Type
}

func (s *typedStubExpr) GetResultValue() values.Value { return values.NewQueriedValue(nil, s.typ) }
func (s *typedStubExpr) GetQuantifiers() []Quantifier { return nil }
func (s *typedStubExpr) CanCorrelate() bool           { return false }
func (s *typedStubExpr) ChildrenAsSet() bool          { return false }
func (s *typedStubExpr) HashCodeWithoutChildren() uint64 {
	return 0
}

func (s *typedStubExpr) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return nil
}

func (s *typedStubExpr) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*typedStubExpr)
	return ok && o.name == s.name
}

func (s *typedStubExpr) WithQuantifiers(quantifiers []Quantifier) (RelationalExpression, error) {
	if err := requireQuantifierArity("typedStubExpr", len(quantifiers), 0); err != nil {
		return nil, err
	}
	return s, nil
}

func TestForEachQuantifierFreshAlias(t *testing.T) {
	t.Parallel()
	ref := InitialOf(&stubExpr{name: "T"})
	q1 := ForEachQuantifier(ref)
	q2 := ForEachQuantifier(ref)
	if q1.GetAlias() == q2.GetAlias() {
		t.Fatal("two ForEach quantifiers reused an alias")
	}
	if q1.GetRangesOver() != ref || q1.Kind() != QuantifierForEach {
		t.Fatal("ForEach quantifier did not retain its reference and kind")
	}
}

func TestNamedQuantifiersPreserveAliasAndKind(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("row")
	ref := InitialOf(&stubExpr{name: "T"})
	forEach := NamedForEachQuantifier(alias, ref)
	existential := NamedExistentialQuantifier(alias, ref)
	if forEach.GetAlias() != alias || forEach.Kind() != QuantifierForEach {
		t.Fatal("named ForEach quantifier lost its alias or kind")
	}
	if existential.GetAlias() != alias || existential.Kind() != QuantifierExistential {
		t.Fatal("named existential quantifier lost its alias or kind")
	}
}

func TestRequireFlowedObjectValueReturnsExactView(t *testing.T) {
	t.Parallel()
	// Built inline rather than via rowOfTypes because this test MUTATES it below
	// to prove snapshot isolation, and a graph from a helper carries the helper's
	// provenance rather than this function's. rowOfTypes does allocate, but that
	// is a fact about another function's body — the typeimmutable gate cannot see
	// through a call and should not, since the invariant it protects is exactly
	// "the writer built it".
	row := &values.RecordType{Fields: []values.Field{
		{Name: "A", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "B", Ordinal: 1, FieldType: values.NullableString},
	}}
	alias := values.NamedCorrelationIdentifier("Q")
	q := NamedForEachQuantifier(alias, InitialOf(&typedStubExpr{name: "source", typ: row}))

	result, err := q.RequireFlowedObjectValue()
	if err != nil {
		t.Fatalf("RequireFlowedObjectValue: %v", err)
	}
	recognized, ok := values.AsQuantifiedObjectValue(result)
	if !ok {
		t.Fatalf("result %T is not a values-owned exact QOV", result)
	}
	if recognized.Correlation() != alias || !recognized.FlowedType().Equals(row) {
		t.Fatalf("QOV = (%v, %v), want (%v, %v)", recognized.Correlation(), recognized.FlowedType(), alias, row)
	}

	// FlowedType hands back the SHARED graph, so this no longer mutates it to
	// prove isolation — under sharing that mutation writes through to an INTERNED
	// handle and corrupts every other value flowing this shape, including in
	// tests running in parallel. What is asserted instead is the sharing itself,
	// because a reintroduced defensive copy passes every other assertion here and
	// costs 128.9M objects per planner sweep.
	if a, b := recognized.FlowedType(), recognized.FlowedType(); a != b {
		t.Fatalf("FlowedType returned two graphs (%p, %p); the defensive copy is "+
			"back — see RFC-234", a, b)
	}
	// Isolation from the CALLER's graph is the half that did not change, and it
	// matters more now: `row` is the caller's, and an edit to it must not reach a
	// snapshot the whole process reads.
	row.Fields[0].Name = "CALLER_MUTATED"
	if got := recognized.FlowedType().(*values.RecordType).Fields[0].Name; got != "A" {
		t.Fatalf("a caller's later edit reached the stored snapshot: %q", got)
	}
}

func TestRequireFlowedObjectValueRejectsUnavailableTypeWithoutObject(t *testing.T) {
	t.Parallel()
	q := NamedForEachQuantifier(values.NamedCorrelationIdentifier("Q"), InitialOf(&stubExpr{name: "unknown"}))
	result, err := q.RequireFlowedObjectValue()
	if err == nil || result != nil {
		t.Fatalf("unresolved member returned (%v, %v), want nil result and error", result, err)
	}
}

func TestGetFlowedObjectTypeVerifiesEveryMember(t *testing.T) {
	t.Parallel()
	ab := rowOfTypes("A", values.NotNullLong, "B", values.NotNullLong)
	ref := InitialOf(&typedStubExpr{name: "one", typ: ab})
	if !ref.Insert(&typedStubExpr{name: "two", typ: rowOfTypes("A", values.NotNullLong, "B", values.NotNullLong)}) {
		t.Fatal("fixture failed to retain the agreeing second member")
	}
	q := NamedForEachQuantifier(values.NamedCorrelationIdentifier("Q"), ref)
	got, err := q.GetFlowedObjectType()
	if err != nil || !got.Equals(ab) {
		t.Fatalf("agreeing members resolved to (%v, %v), want %v", got, err, ab)
	}

	disagree := InitialOf(&typedStubExpr{name: "left", typ: ab})
	if !disagree.Insert(&typedStubExpr{name: "right", typ: rowOfTypes("A", values.NotNullString)}) {
		t.Fatal("fixture failed to retain the disagreeing member")
	}
	qd := NamedForEachQuantifier(values.NamedCorrelationIdentifier("QD"), disagree)
	got, err = qd.GetFlowedObjectType()
	var disagreement *MemberResultTypeDisagreementError
	if got != nil || !errors.As(err, &disagreement) {
		t.Fatalf("disagreeing members returned (%v, %v), want nil and MemberResultTypeDisagreementError", got, err)
	}
	if result, qovErr := qd.RequireFlowedObjectValue(); qovErr == nil || result != nil {
		t.Fatalf("disagreeing members published QOV %v with error %v", result, qovErr)
	}
}

func TestNullOnEmptyWidensExactFlowedTypeOnce(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("Q")
	q := NamedForEachNullOnEmptyQuantifier(alias, InitialOf(&typedStubExpr{name: "source", typ: values.NotNullLong}))
	result, err := q.RequireFlowedObjectValue()
	if err != nil {
		t.Fatalf("RequireFlowedObjectValue: %v", err)
	}
	if !result.FlowedType().Equals(values.NullableLong) {
		t.Fatalf("NullOnEmpty type = %v, want %v", result.FlowedType(), values.NullableLong)
	}
	if !result.FlowedType().Equals(result.Type()) {
		t.Fatalf("QOV Type widened differently from FlowedType: %v vs %v", result.Type(), result.FlowedType())
	}
}

func TestQuantifierZeroValueHasNoRangesOver(t *testing.T) {
	t.Parallel()
	var quantifier Quantifier
	if quantifier.GetRangesOver() != nil || len(quantifier.GetCorrelatedTo()) != 0 {
		t.Fatal("zero-value quantifier unexpectedly has a reference or correlation")
	}
}
