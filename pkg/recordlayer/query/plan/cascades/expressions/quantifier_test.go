package expressions

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

type stubExpr struct{ name string }

func (s *stubExpr) GetResultValue() values.Value    { return values.NewNullValue(values.UnknownType) }
func (s *stubExpr) GetQuantifiers() []Quantifier    { return nil }
func (s *stubExpr) CanCorrelate() bool              { return false }
func (s *stubExpr) ChildrenAsSet() bool             { return false }
func (s *stubExpr) HashCodeWithoutChildren() uint64 { return 0 }
func (s *stubExpr) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return nil
}

func (s *stubExpr) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*stubExpr)
	return ok && o.name == s.name
}

func (s *stubExpr) WithQuantifiers(_ []Quantifier) RelationalExpression { return s }

func TestForEachQuantifier_FreshAlias(t *testing.T) {
	t.Parallel()
	ref := InitialOf(&stubExpr{name: "T"})
	q1 := ForEachQuantifier(ref)
	q2 := ForEachQuantifier(ref)
	if q1.GetAlias() == q2.GetAlias() {
		t.Fatal("two ForEachQuantifier calls returned the same alias — should be unique")
	}
	if q1.GetRangesOver() != ref {
		t.Fatal("RangesOver pointer changed")
	}
	if q1.Kind() != QuantifierForEach {
		t.Fatalf("kind=%v, want ForEach", q1.Kind())
	}
}

func TestNamedForEachQuantifier_PreservesAlias(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("salesrow")
	ref := InitialOf(&stubExpr{name: "Sales"})
	q := NamedForEachQuantifier(alias, ref)
	if q.GetAlias() != alias {
		t.Fatalf("alias=%v, want %v", q.GetAlias(), alias)
	}
	if q.GetRangesOver() != ref {
		t.Fatal("RangesOver pointer changed")
	}
}

func TestQuantifier_FlowedObjectValue_CarriesAlias(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("rec")
	q := NamedForEachQuantifier(alias, InitialOf(&stubExpr{name: "T"}))
	v := q.GetFlowedObjectValue()
	corrSet := v.(*values.QuantifiedObjectValue).GetCorrelatedTo()
	if _, ok := corrSet[alias]; !ok {
		t.Fatalf("flowed object doesn't carry the quantifier's alias %v in its correlation set %v", alias, corrSet)
	}
}

func TestExistentialQuantifier_KindAndAlias(t *testing.T) {
	t.Parallel()
	ref := InitialOf(&stubExpr{name: "X"})
	q1 := ExistentialQuantifier(ref)
	if q1.Kind() != QuantifierExistential {
		t.Fatalf("kind=%v, want Existential", q1.Kind())
	}
	q2 := ExistentialQuantifier(ref)
	if q1.GetAlias() == q2.GetAlias() {
		t.Fatal("two ExistentialQuantifier calls returned the same alias — should be unique")
	}
	if q1.GetRangesOver() != ref {
		t.Fatal("RangesOver pointer changed")
	}
}

func TestNamedExistentialQuantifier_PreservesAlias(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("exists_subquery")
	ref := InitialOf(&stubExpr{name: "Sub"})
	q := NamedExistentialQuantifier(alias, ref)
	if q.Kind() != QuantifierExistential {
		t.Fatalf("kind=%v, want Existential", q.Kind())
	}
	if q.GetAlias() != alias {
		t.Fatalf("alias=%v, want %v", q.GetAlias(), alias)
	}
}

func TestQuantifier_ZeroValueHasNoRangesOver(t *testing.T) {
	t.Parallel()

	var quantifier Quantifier
	if got := quantifier.GetRangesOver(); got != nil {
		t.Fatalf("zero-value ranges-over = %v, want nil", got)
	}
	if correlatedTo := quantifier.GetCorrelatedTo(); len(correlatedTo) != 0 {
		t.Fatalf(
			"zero-value correlations = %v, want empty",
			correlatedTo,
		)
	}
}

// TestQuantifierKind_DoesNotAffectFlowedObjectValue pins that
// GetFlowedObjectValue returns a QuantifiedObjectValue regardless of
// kind. The seed treats ForEach and Existential identically here —
// future MaxMatchMap work will introduce kind-aware semantics, but
// the seed contract is clear: the alias is what matters.
func TestQuantifierKind_DoesNotAffectFlowedObjectValue(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("q1")
	ref := InitialOf(&stubExpr{name: "T"})
	forEach := NamedForEachQuantifier(alias, ref)
	existential := NamedExistentialQuantifier(alias, ref)
	if forEach.GetFlowedObjectValue().(*values.QuantifiedObjectValue).Correlation != alias {
		t.Fatal("ForEach flowed-object alias mismatch")
	}
	if existential.GetFlowedObjectValue().(*values.QuantifiedObjectValue).Correlation != alias {
		t.Fatal("Existential flowed-object alias mismatch")
	}
}

// typedStubExpr is a stub whose result value carries a chosen ROW type, so a
// Reference can be given members that AGREE or DISAGREE on the row they flow.
type typedStubExpr struct {
	name string
	typ  *values.RecordType
}

func (s *typedStubExpr) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier(s.name), s.typ)
}
func (s *typedStubExpr) GetQuantifiers() []Quantifier    { return nil }
func (s *typedStubExpr) CanCorrelate() bool              { return false }
func (s *typedStubExpr) ChildrenAsSet() bool             { return false }
func (s *typedStubExpr) HashCodeWithoutChildren() uint64 { return 0 }
func (s *typedStubExpr) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return nil
}

func (s *typedStubExpr) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*typedStubExpr)
	return ok && o.name == s.name
}

func (s *typedStubExpr) WithQuantifiers(_ []Quantifier) RelationalExpression { return s }

func rowOf(names ...string) *values.RecordType {
	fields := make([]values.Field, len(names))
	for i, n := range names {
		fields[i] = values.Field{Name: n, FieldType: values.NotNullLong, Ordinal: i}
	}
	// Nullable, because a QuantifiedObjectValue reports its row type as nullable
	// (rows pass through as nullable -- a LEFT JOIN's right side). Building the
	// expectation the same way keeps this test about member AGREEMENT rather than
	// about that policy.
	return &values.RecordType{Nullable: true, Fields: fields}
}

// TestGetFlowedObjectType_VerifiesMemberAgreement pins the ported half of Java's
// Reference.getResultType(): the row type is resolved by REDUCING over every
// member with `Verify.verify(left.equals(right))` (Reference.java:504-513), so
// "any member is authoritative" is a conclusion the verification earns rather
// than an assumption.
//
// Go used to read members[0] and cite that verification without performing it. On
// a memo where two members of one equivalence class flow different row shapes,
// that picks a row shape by INSERTION ORDER — and the shape feeds the positional
// merge's slot types, where a wrong row is a wrong-slot read with no error.
func TestGetFlowedObjectType_VerifiesMemberAgreement(t *testing.T) {
	t.Parallel()
	ab := rowOf("A", "B")

	// AGREEING members: the type resolves, and it resolves to the agreed row.
	agree := InitialOf(&typedStubExpr{name: "m1", typ: ab})
	agree.Insert(&typedStubExpr{name: "m2", typ: rowOf("A", "B")})
	if len(agree.AllMembers()) < 2 {
		t.Fatalf("fixture: reference holds %d members, need 2 for the agreement to be "+
			"tested at all", len(agree.AllMembers()))
	}
	q := NamedForEachQuantifier(values.NamedCorrelationIdentifier("Q"), agree)
	got, err := q.GetFlowedObjectType()
	if err != nil {
		t.Fatalf("agreeing members returned error %v, want the agreed row type", err)
	}
	if got == nil || !got.Equals(ab) {
		t.Fatalf("agreeing members resolved %v, want %v", got, ab)
	}

	// DISAGREEING members: an explicit error, never members[0].
	disagree := InitialOf(&typedStubExpr{name: "d1", typ: ab})
	disagree.Insert(&typedStubExpr{name: "d2", typ: rowOf("A", "B", "C")})
	if len(disagree.AllMembers()) < 2 {
		t.Fatalf("fixture: disagreeing reference holds %d members, need 2",
			len(disagree.AllMembers()))
	}
	qd := NamedForEachQuantifier(values.NamedCorrelationIdentifier("QD"), disagree)
	got, err = qd.GetFlowedObjectType()
	if got != nil {
		t.Errorf("disagreeing members resolved %v — a row shape chosen by memo "+
			"insertion order is exactly what the verification forbids", got)
	}
	var de *MemberResultTypeDisagreementError
	if !errors.As(err, &de) {
		t.Fatalf("disagreeing members returned err=%v, want a "+
			"*MemberResultTypeDisagreementError naming the quantifier and both types", err)
	}
	if de.Alias.Name() != "QD" {
		t.Errorf("error names alias %q, want QD", de.Alias.Name())
	}

	// GetFlowedObjectValueTyped must propagate it rather than degrade to the
	// untyped QOV: the untyped fallback is the "no type yet" case, and collapsing
	// the two would restore the exact silent path the typed accessor exists to close.
	v, err := qd.GetFlowedObjectValueTyped()
	if err == nil {
		t.Errorf("GetFlowedObjectValueTyped returned %v with no error on disagreeing "+
			"members — a disagreement must not read as 'type unavailable'", v)
	}

	// And the ordinary no-type case still reports (nil, nil): an untyped member
	// contradicts nothing, so the reporting gap stays a gap and not an error.
	untyped := NamedForEachQuantifier(values.NamedCorrelationIdentifier("QU"),
		InitialOf(&stubExpr{name: "T"}))
	if got, err := untyped.GetFlowedObjectType(); got != nil || err != nil {
		t.Errorf("untyped member gave (%v, %v), want (nil, nil)", got, err)
	}
}

// TestGetFlowedObjectType_MixedTypedAndUntypedMembers pins the dimension the
// member scan CHANGED: which member the row type comes from when the reference
// holds both untyped and typed members.
//
// The resolution used to read ref.Get() — the CANONICAL member, members[0]. On a
// reference whose canonical member is untyped and whose LATER member is typed,
// that reported "type unavailable" and the caller fell to its own fallback (the
// positional merge scavenges the select's baked references for legRowTypes). The
// scan over AllMembers reports the LATER member's type instead, so the merge slot
// is typed from the memo rather than from the fallback.
//
// That is the intended direction — every member of one equivalence class flows the
// same row, so a typed member is the authority and an untyped one reports nothing
// (Java has no untyped member at all; the gap is Go's) — but it was unpinned, and
// an unpinned change of which member is authoritative is exactly the
// wrong-slot-by-insertion-order hazard the surrounding test exists to close. Both
// orders are checked: a typed member must win whether it precedes or follows the
// untyped one, because "first TYPED member" must not degrade into "first member".
//
// And the agreement verification must still run over the TYPED members only: an
// untyped member sitting between two disagreeing typed ones must not swallow the
// disagreement.
func TestGetFlowedObjectType_MixedTypedAndUntypedMembers(t *testing.T) {
	t.Parallel()
	ab := rowOf("A", "B")

	// UNTYPED canonical, TYPED later member — the changed case. members[0] is the
	// untyped stub, so the retired ref.Get() reading gave (nil, nil) here.
	mixed := InitialOf(&stubExpr{name: "u1"})
	if !mixed.Insert(&typedStubExpr{name: "t1", typ: ab}) {
		t.Fatal("fixture: the typed member was not inserted, so there is no mixed " +
			"reference to measure")
	}
	if canonical := mixed.Get(); canonical == nil || rowTypeOf(canonical.GetResultValue().Type()) != nil {
		t.Fatalf("fixture: the canonical member is %T and it is TYPED — the whole point "+
			"of this case is a reference whose canonical member reports no row type", canonical)
	}
	qm := NamedForEachQuantifier(values.NamedCorrelationIdentifier("QM"), mixed)
	got, err := qm.GetFlowedObjectType()
	if err != nil {
		t.Fatalf("mixed typed+untyped members returned error %v — an untyped member "+
			"contradicts nothing and must not read as a disagreement", err)
	}
	if got == nil || !got.Equals(ab) {
		t.Fatalf("mixed members resolved %v, want the TYPED member's row %v.\n"+
			"  Reading the canonical member instead reports 'type unavailable' here and\n"+
			"  the caller falls to its own fallback for a type the memo already carries.",
			got, ab)
	}
	// And the typed value follows: this is the accessor the positional merge calls.
	v, err := qm.GetFlowedObjectValueTyped()
	if err != nil {
		t.Fatalf("GetFlowedObjectValueTyped on mixed members: %v", err)
	}
	if _, typed := v.Type().(*values.RecordType); !typed {
		t.Errorf("GetFlowedObjectValueTyped returned an UNTYPED value (%v) on a reference "+
			"holding a typed member — the merge slot then strips the leg types and a "+
			"source-relative operand pushed into a leg scan reads NULL", v.Type())
	}

	// TYPED canonical, UNTYPED later member — the direction that already worked.
	// Checked so "first TYPED member" cannot silently become "last typed member" or
	// "first member".
	mixedRev := InitialOf(&typedStubExpr{name: "t2", typ: ab})
	if !mixedRev.Insert(&stubExpr{name: "u2"}) {
		t.Fatal("fixture: the untyped member was not inserted")
	}
	qr := NamedForEachQuantifier(values.NamedCorrelationIdentifier("QR"), mixedRev)
	if got, err := qr.GetFlowedObjectType(); err != nil || got == nil || !got.Equals(ab) {
		t.Errorf("typed-then-untyped members gave (%v, %v), want %v and no error — a "+
			"trailing untyped member must not erase a resolved type", got, err, ab)
	}

	// Agreement is still verified among the TYPED members, with an untyped member
	// interleaved between them. If the scan stopped at the first typed member, or
	// treated the untyped one as a reset, this disagreement would go unreported and
	// the merge slot would take a row shape chosen by insertion order.
	interleaved := InitialOf(&typedStubExpr{name: "t3", typ: ab})
	if !interleaved.Insert(&stubExpr{name: "u3"}) {
		t.Fatal("fixture: the interleaved untyped member was not inserted")
	}
	if !interleaved.Insert(&typedStubExpr{name: "t4", typ: rowOf("A", "B", "C")}) {
		t.Fatal("fixture: the second typed member was not inserted")
	}
	if n := len(interleaved.AllMembers()); n != 3 {
		t.Fatalf("fixture: interleaved reference holds %d members, need 3 (typed, "+
			"untyped, typed) for the untyped member to sit BETWEEN the disagreeing pair", n)
	}
	qi := NamedForEachQuantifier(values.NamedCorrelationIdentifier("QI"), interleaved)
	gotI, errI := qi.GetFlowedObjectType()
	if gotI != nil {
		t.Errorf("interleaved members resolved %v — a disagreement between two typed "+
			"members must not be resolved just because an untyped member sits between "+
			"them", gotI)
	}
	var de *MemberResultTypeDisagreementError
	if !errors.As(errI, &de) {
		t.Fatalf("interleaved members returned err=%v, want a "+
			"*MemberResultTypeDisagreementError — the verification must span every TYPED "+
			"member, not just adjacent ones", errI)
	}
	if de.Alias.Name() != "QI" {
		t.Errorf("error names alias %q, want QI", de.Alias.Name())
	}
}
