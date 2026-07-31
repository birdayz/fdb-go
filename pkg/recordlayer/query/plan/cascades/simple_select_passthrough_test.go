package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// passthroughRow builds a row type for the passthrough fixtures.
func passthroughRow(nullable bool, names ...string) *values.RecordType {
	fields := make([]values.Field, len(names))
	for i, n := range names {
		fields[i] = values.Field{Name: n, FieldType: values.NotNullLong, Ordinal: i}
	}
	return &values.RecordType{Nullable: nullable, Fields: fields}
}

// typedScanQuantifier returns a quantifier whose reference flows `rt`, i.e. one
// whose GetFlowedObjectType answers with a real row rather than the reporting gap.
func typedScanQuantifier(rt values.Type) expressions.Quantifier {
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, rt)
	return expressions.ForEachQuantifier(expressions.InitialOf(scan))
}

// TestIsSimplePassthroughOf_ComparesTheQuantifiersOwnFlowedType pins the operand
// this gate compares against, which is the whole gate.
//
// Java (ImplementSimpleSelectRule.java:128-130) asks whether the result value is a
// QuantifiedObjectValue over the inner alias AND whether its result type equals
// `innerQuantifier.getFlowedObjectType()`. Go asked the quantifier's UNTYPED flowed
// VALUE for its type instead — an accessor that answers UNKNOWN for every
// quantifier there is. That turns the whole comparison into "is the result value
// ALSO untyped?", which agrees with Java only for as long as nothing carries a
// type and inverts the moment something does: an EXACT passthrough of a typed row
// stops being recognised as one, an identity Map is wrapped around a plan that
// needs none, and the rows read through that Map land on the wrong slots.
//
// The four cases below are the four directions this can be wrong in, and each is
// a different wrong answer rather than four spellings of one:
//
//	exact    — the direction the untyped accessor breaks
//	widened  — the direction a "types agree, or either is a row" relaxation breaks
//	gap      — the direction a bare type comparison breaks
//	alias    — the passthrough test proper, which is prior to any of it
func TestIsSimplePassthroughOf_ComparesTheQuantifiersOwnFlowedType(t *testing.T) {
	t.Parallel()

	rt := passthroughRow(false, "A", "B")
	q := typedScanQuantifier(rt)

	// The fixture has to actually state a type, or every case below passes for the
	// uninteresting reason.
	flowed, err := q.GetFlowedObjectType()
	if err != nil || flowed == nil {
		t.Fatalf("fixture: the quantifier states no flowed row type (got %v, %v) — with "+
			"nothing typed this test cannot tell the two operands apart", flowed, err)
	}

	// EXACT passthrough: the value the quantifier itself flows. Java's
	// `QuantifiedObjectValue.of(alias, getFlowedObjectType())`.
	exact, err := q.GetFlowedObjectValueTyped()
	if err != nil {
		t.Fatalf("fixture: GetFlowedObjectValueTyped: %v", err)
	}
	if _, typed := exact.(*values.QuantifiedObjectValue).Typ.(*values.RecordType); !typed {
		t.Fatalf("fixture: the typed accessor returned an untyped value %v", exact)
	}
	if !isSimplePassthroughOf(exact, q) {
		t.Errorf("the quantifier's OWN flowed value is not recognised as a passthrough of it.\n" +
			"  Comparing against the untyped accessor's type answers UNKNOWN here, so an\n" +
			"  exact passthrough of a typed row reads as a projection and gets wrapped in an\n" +
			"  identity Map — the plan the golden calls Map(PredicatesFilter(...), q$N).")
	}

	// WIDENED passthrough: same alias, nullability widened. Java keeps the MAP so
	// the widening is expressed at runtime, and its sanity check asserts this is the
	// only shape a non-simple passthrough can take.
	widened := values.NewQuantifiedObjectValueOfType(q.GetAlias(), passthroughRow(true, "A", "B"))
	if isSimplePassthroughOf(widened, q) {
		t.Errorf("a nullability-WIDENED passthrough is treated as exact, so the Map that " +
			"expresses the widening is dropped. Java keeps it (ImplementSimpleSelectRule.java:131-136 " +
			"verifies the widened shape rather than skipping it).")
	}

	// DIFFERENT ROW: same alias, different row shape. Not a passthrough of this
	// quantifier at all.
	otherShape := values.NewQuantifiedObjectValueOfType(q.GetAlias(), passthroughRow(false, "A", "B", "C"))
	if isSimplePassthroughOf(otherShape, q) {
		t.Errorf("a QOV over the right alias but a DIFFERENT row shape is treated as an " +
			"exact passthrough")
	}

	// THE REPORTING GAP, both halves. An untyped QOV states no type; a quantifier
	// whose reference carries no row type states no type. Either way there is
	// nothing to compare and alias identity is the whole answer — which is what this
	// site gave every value before any of them carried a type. A comparison that
	// treats "absent" as "mismatched" turns the gap into a spurious Map on every
	// still-untyped shape in the planner.
	untypedQ := typedScanQuantifier(values.UnknownType)
	if got, err := untypedQ.GetFlowedObjectType(); got != nil || err != nil {
		t.Fatalf("fixture: the untyped quantifier resolved (%v, %v), want the gap", got, err)
	}
	if !isSimplePassthroughOf(values.NewQuantifiedObjectValue(untypedQ.GetAlias()), untypedQ) {
		t.Errorf("untyped value over an untyped quantifier is not a passthrough — the " +
			"reporting gap must stay a gap, not become a mismatch")
	}
	if !isSimplePassthroughOf(values.NewQuantifiedObjectValue(q.GetAlias()), q) {
		t.Errorf("untyped value over a TYPED quantifier is not a passthrough — half a gap " +
			"is still a gap; there is no type on the value to contradict the quantifier's")
	}
	if !isSimplePassthroughOf(exactOverUntypedQuantifier(untypedQ, rt), untypedQ) {
		t.Errorf("typed value over an UNTYPED quantifier is not a passthrough — the other " +
			"half of the same gap")
	}

	// ALIAS: prior to every type question. A QOV over some other quantifier is not a
	// passthrough of this one no matter how the types line up.
	foreign := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("ELSEWHERE"), rt)
	if isSimplePassthroughOf(foreign, q) {
		t.Errorf("a QOV over a FOREIGN alias is treated as a passthrough — the Map that " +
			"reads that correlation would be dropped")
	}
	if isSimplePassthroughOf(values.NewBooleanValue(true), q) {
		t.Errorf("a non-QOV result value is treated as a passthrough")
	}
}

// exactOverUntypedQuantifier builds a TYPED QOV over q's alias, for the half of
// the reporting gap where the value knows its row and the quantifier does not.
func exactOverUntypedQuantifier(q expressions.Quantifier, rt values.Type) values.Value {
	return values.NewQuantifiedObjectValueOfType(q.GetAlias(), rt)
}

// TestIsSimplePassthroughOf_DeclinesOnMemberDisagreement pins the arm that has no
// Java counterpart because Java cannot reach it.
//
// Java resolves the flowed type by REDUCING over the reference's members with
// Verify.verify (Reference.java:504-513), so a disagreement is a crash and no
// caller ever sees one. Go returns it, and this gate must not read it as "no type,
// therefore a passthrough": there is no single flowed row to be an exact
// passthrough OF, so the Map stays. That is the conservative side — a redundant
// projection, never a dropped one.
func TestIsSimplePassthroughOf_DeclinesOnMemberDisagreement(t *testing.T) {
	t.Parallel()

	ab := passthroughRow(false, "A", "B")
	ref := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, ab))
	ref.Insert(expressions.NewFullUnorderedScanExpression([]string{"U"}, passthroughRow(false, "A", "B", "C")))
	if len(ref.AllMembers()) < 2 {
		t.Fatalf("fixture: reference holds %d members, need 2 disagreeing ones",
			len(ref.AllMembers()))
	}
	q := expressions.ForEachQuantifier(ref)
	if _, err := q.GetFlowedObjectType(); err == nil {
		t.Fatalf("fixture: the members do not disagree, so the decline arm is unreachable " +
			"and this test proves nothing")
	}

	v := values.NewQuantifiedObjectValueOfType(q.GetAlias(), ab)
	if isSimplePassthroughOf(v, q) {
		t.Errorf("a member DISAGREEMENT is read as an exact passthrough. The Map is then " +
			"skipped on a reference whose members flow different rows, which picks a row " +
			"shape by memo insertion order — the choice the agreement verification exists " +
			"to refuse.")
	}
}
