package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestRFC173W4_LeftOrdinalizable pins the buried-eligibility gate (RFC-173 W4 Q3),
// keyed on preserved-leg SOURCE COUNT (the alias-collision-safe reading): a
// dissolved LEFT ordinalizes iff its preserved leg is a SINGLE SOURCE
// (provides only its own top alias). A MULTI-source preserved cluster is never
// eligible — its synthetic quantifier alias collides with its rightmost leaf's,
// so an ON-pred touching that alias is really touching a BURIED source whose
// column lives in the erased ordinal concat (the RFC-153 hazard).
func TestRFC173W4_LeftOrdinalizable(t *testing.T) {
	t.Parallel()
	top := values.NamedCorrelationIdentifier("A")
	buried := values.NamedCorrelationIdentifier("B")

	// Single-source preserved: provides only {A}. Eligible.
	single := map[values.CorrelationIdentifier]struct{}{top: {}}
	if !leftOrdinalizable(nil, top, single) {
		t.Fatal("single-source preserved (provides only its top alias) must be ORDINALIZABLE")
	}

	// Multi-source preserved cluster: provides {A, B} — flattened (A JOIN B).
	// NOT eligible even though the ON-pred (elided; the gate is source-count
	// based) may touch the top-or-rightmost alias.
	cluster := map[values.CorrelationIdentifier]struct{}{top: {}, buried: {}}
	if leftOrdinalizable(nil, top, cluster) {
		t.Fatal("multi-source preserved cluster must NOT be ordinalizable — ordinalizing erases the buried names (RFC-153 hazard)")
	}

	// Defensive: the sole provided alias must be the top.
	wrong := map[values.CorrelationIdentifier]struct{}{buried: {}}
	if leftOrdinalizable(nil, top, wrong) {
		t.Fatal("a single provided alias that is NOT the top must decline (defensive)")
	}
}

// leftAnchoredRC builds the anchored RC the translator produces for a 2-leg
// LEFT box: leg A [ID, FLAG], leg C [ID, A_ID].
func leftAnchoredRC(t *testing.T, aliasA, aliasC values.CorrelationIdentifier) *values.RecordConstructorValue {
	t.Helper()
	legs := []values.AnchoredJoinLeg{
		{Alias: aliasA, Columns: []values.Field{{Name: "ID", FieldType: values.NotNullLong}, {Name: "FLAG", FieldType: values.NotNullLong}}},
		{Alias: aliasC, Columns: []values.Field{{Name: "ID", FieldType: values.NotNullLong}, {Name: "A_ID", FieldType: values.NotNullLong}}},
	}
	return values.NewAnchoredJoinRecord(legs)
}

// TestRFC173W4_RewriteYieldsOrdinalSeed_TopLevel pins the ordinalization: a
// TOP-LEVEL-correlated LEFT box (ON C.a_id = A.id, preserved A a single source)
// dissolves into an INNER select whose result value is the ORDINAL SEED (baked
// ofOrdinalNumber refs, NOT AnchoredJoin), with the null-supplying leg's
// ordinals nullable-wrapped and the null-on-empty quantifier carrying the
// LEFT semantics.
func TestRFC173W4_RewriteYieldsOrdinalSeed_TopLevel(t *testing.T) {
	t.Parallel()
	aliasA := values.NamedCorrelationIdentifier("A")
	aliasC := values.NamedCorrelationIdentifier("C")

	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanC := expressions.NewFullUnorderedScanExpression([]string{"C"}, values.UnknownType)
	qA := expressions.NamedForEachQuantifier(aliasA, expressions.InitialOf(scanA))
	qC := expressions.NamedForEachQuantifier(aliasC, expressions.InitialOf(scanC))

	// ON C.a_id = A.id — correlates to the TOP preserved alias A.
	pred := predicates.NewComparisonPredicate(
		values.NewFieldValue(values.NewQuantifiedObjectValue(aliasC), "A_ID", values.UnknownType),
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.NewFieldValue(values.NewQuantifiedObjectValue(aliasA), "ID", values.UnknownType)},
	)

	sel := expressions.NewSelectExpressionWithJoinType(
		leftAnchoredRC(t, aliasA, aliasC),
		[]expressions.Quantifier{qA, qC},
		[]predicates.QueryPredicate{pred},
		[]string{"A", "C"},
		expressions.JoinLeftOuter,
	)

	yielded := FireExpressionRule(NewRewriteOuterJoinRule(), expressions.InitialOf(sel))
	var inner *expressions.SelectExpression
	for _, e := range yielded {
		if s, ok := e.(*expressions.SelectExpression); ok && s.GetJoinType() == expressions.JoinInner {
			inner = s
		}
	}
	if inner == nil {
		t.Fatalf("rule yielded no INNER select (got %d)", len(yielded))
	}
	rc, ok := inner.GetResultValue().(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("result value = %T, want *RecordConstructorValue", inner.GetResultValue())
	}
	if rc.AnchoredJoin {
		t.Fatal("top-level dissolved LEFT must ordinalize — got the name-model anchored RC, not the ordinal seed")
	}
	values.AssertOrdinalJoinSeed(rc) // panics if not the ordinal seed shape
	// Two legs of 2 columns each = 4 baked fields; the null-supplying C leg
	// (fields 2,3) must be nullable-wrapped.
	if len(rc.Fields) != 4 {
		t.Fatalf("ordinal seed has %d fields, want 4 (A.ID,A.FLAG,C.ID,C.A_ID)", len(rc.Fields))
	}
	for i, f := range rc.Fields {
		fv, ok := f.Value.(*values.FieldValue)
		if !ok || fv.Resolved == nil {
			t.Fatalf("field %d is not a baked leg reference: %T", i, f.Value)
		}
		if i >= 2 && !fv.Typ.IsNullable() {
			t.Fatalf("null-supplying field %d (%s) must be NULLABLE-wrapped, got %s", i, f.Name, fv.Typ)
		}
		if i < 2 && fv.Typ.IsNullable() {
			t.Fatalf("preserved field %d (%s) must stay NON-nullable, got %s", i, f.Name, fv.Typ)
		}
	}
	// The null-on-empty quantifier still carries the LEFT semantics.
	nOnEmpty := 0
	for _, q := range inner.GetQuantifiers() {
		if q.IsNullOnEmpty() {
			nOnEmpty++
		}
	}
	if nOnEmpty != 1 {
		t.Fatalf("want 1 null-on-empty quantifier, got %d", nOnEmpty)
	}
}
