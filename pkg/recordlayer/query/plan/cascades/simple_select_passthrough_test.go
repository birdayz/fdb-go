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
func typedScanQuantifier(t testing.TB, rt values.Type) expressions.Quantifier {
	t.Helper()
	scan, err := expressions.NewFullUnorderedScanExpression([]string{"T"}, rt)
	scan = mustConstruct(t, scan, err)
	return expressions.ForEachQuantifier(expressions.InitialOf(scan))
}

func passthroughQOV(
	t testing.TB,
	alias values.CorrelationIdentifier,
	typ values.Type,
) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(alias, typ)
	return mustConstruct(t, qov, err)
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
// The five cases below are the five directions this can be wrong in, and each is
// a different wrong answer rather than five spellings of one:
//
//	exact       — the direction the old untyped accessor broke
//	widened     — the direction an assignability relaxation breaks
//	shape       — same alias does not erase a different exact row
//	unavailable — no authoritative quantifier type means no passthrough
//	alias       — the passthrough test proper, which is prior to any of it
func TestIsSimplePassthroughOf_ComparesTheQuantifiersOwnFlowedType(t *testing.T) {
	t.Parallel()

	rt := passthroughRow(false, "A", "B")
	q := typedScanQuantifier(t, rt)

	// The fixture has to actually state a type, or every case below passes for the
	// uninteresting reason.
	flowed, err := q.GetFlowedObjectType()
	if err != nil || flowed == nil {
		t.Fatalf("fixture: the quantifier states no flowed row type (got %v, %v) — with "+
			"nothing typed this test cannot tell the two operands apart", flowed, err)
	}

	// EXACT passthrough: the value the quantifier itself flows. Java's
	// `QuantifiedObjectValue.of(alias, getFlowedObjectType())`.
	exact, err := q.RequireFlowedObjectValue()
	exact = mustConstruct(t, exact, err)
	if _, typed := exact.FlowedType().(*values.RecordType); !typed {
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
	widened := passthroughQOV(t, q.GetAlias(), passthroughRow(true, "A", "B"))
	if isSimplePassthroughOf(widened, q) {
		t.Errorf("a nullability-WIDENED passthrough is treated as exact, so the Map that " +
			"expresses the widening is dropped. Java keeps it (ImplementSimpleSelectRule.java:131-136 " +
			"verifies the widened shape rather than skipping it).")
	}

	// DIFFERENT ROW: same alias, different row shape. Not a passthrough of this
	// quantifier at all.
	otherShape := passthroughQOV(t, q.GetAlias(), passthroughRow(false, "A", "B", "C"))
	if isSimplePassthroughOf(otherShape, q) {
		t.Errorf("a QOV over the right alias but a DIFFERENT row shape is treated as an " +
			"exact passthrough")
	}

	// UNAVAILABLE QUANTIFIER TYPE: the QOV is exact, but the quantifier has no
	// ranged-over Reference and therefore no authoritative flowed type. That is a
	// decline, never permission to drop the Map by alias alone.
	unavailableAlias := values.NamedCorrelationIdentifier("UNAVAILABLE")
	unavailableQ := expressions.NamedForEachQuantifier(unavailableAlias, nil)
	if _, err := unavailableQ.GetFlowedObjectType(); err == nil {
		t.Fatal("fixture: quantifier without a Reference unexpectedly reports a flowed type")
	}
	if isSimplePassthroughOf(passthroughQOV(t, unavailableAlias, rt), unavailableQ) {
		t.Errorf("an exact QOV over a quantifier with no authoritative flowed type is " +
			"treated as a passthrough — the Map must remain on an unavailable edge")
	}

	// ALIAS: prior to every type question. A QOV over some other quantifier is not a
	// passthrough of this one no matter how the types line up.
	foreign := passthroughQOV(t, values.NamedCorrelationIdentifier("ELSEWHERE"), rt)
	if isSimplePassthroughOf(foreign, q) {
		t.Errorf("a QOV over a FOREIGN alias is treated as a passthrough — the Map that " +
			"reads that correlation would be dropped")
	}
	if isSimplePassthroughOf(values.NewBooleanValue(true), q) {
		t.Errorf("a non-QOV result value is treated as a passthrough")
	}
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
	scanAB, err := expressions.NewFullUnorderedScanExpression([]string{"T"}, ab)
	scanAB = mustConstruct(t, scanAB, err)
	ref := expressions.InitialOf(scanAB)
	scanABC, err := expressions.NewFullUnorderedScanExpression(
		[]string{"U"}, passthroughRow(false, "A", "B", "C"))
	scanABC = mustConstruct(t, scanABC, err)
	ref.Insert(scanABC)
	if len(ref.AllMembers()) < 2 {
		t.Fatalf("fixture: reference holds %d members, need 2 disagreeing ones",
			len(ref.AllMembers()))
	}
	q := expressions.ForEachQuantifier(ref)
	if _, err := q.GetFlowedObjectType(); err == nil {
		t.Fatalf("fixture: the members do not disagree, so the decline arm is unreachable " +
			"and this test proves nothing")
	}

	v := passthroughQOV(t, q.GetAlias(), ab)
	if isSimplePassthroughOf(v, q) {
		t.Errorf("a member DISAGREEMENT is read as an exact passthrough. The Map is then " +
			"skipped on a reference whose members flow different rows, which picks a row " +
			"shape by memo insertion order — the choice the agreement verification exists " +
			"to refuse.")
	}
}
