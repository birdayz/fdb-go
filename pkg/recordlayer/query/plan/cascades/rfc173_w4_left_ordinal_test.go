package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestRFC173W4_SeedDeclinesClusteredLeg pins the seed builder's symmetric
// decline: an anchored RC that decomposes into more than the TWO expected legs
// (a clustered leg on EITHER side exposing sub-aliases — the null-side gap the
// preserved-only gate missed) yields no ordinal seed (nil → name-model
// fallback), never a wrong under-covered seed.
func TestRFC173W4_SeedDeclinesClusteredLeg(t *testing.T) {
	t.Parallel()
	a := values.NamedCorrelationIdentifier("A")
	b := values.NamedCorrelationIdentifier("B")
	c := values.NamedCorrelationIdentifier("C")

	// Clean 2-leg anchored RC (A preserved, C null) → ordinalizes.
	if ordinalSeedFromAnchoredLeft(leftAnchoredRC(t, a, c), a, c) == nil {
		t.Fatal("a clean 2-leg anchored RC must produce an ordinal seed")
	}

	// 3-leg anchored RC — a clustered null side (B JOIN C) exposes B and C as
	// sub-aliases. The seed builder must DECLINE (nil), not emit a seed that
	// under-covers the cluster's columns.
	clustered := values.NewAnchoredJoinRecord([]values.AnchoredJoinLeg{
		{Alias: a, Columns: []values.Field{{Name: "ID", FieldType: values.NotNullLong}}},
		{Alias: b, Columns: []values.Field{{Name: "ID", FieldType: values.NotNullLong}}},
		{Alias: c, Columns: []values.Field{{Name: "ID", FieldType: values.NotNullLong}}},
	})
	if ordinalSeedFromAnchoredLeft(clustered, a, c) != nil {
		t.Fatal("a 3-leg (clustered) anchored RC must DECLINE — an under-covered seed would drop the buried leg's columns")
	}
}

// TestRFC173W4_SeedPreservesDeclarationOrder pins the RIGHT-JOIN column-order
// fix: a RIGHT JOIN normalizes to LEFT with swapped children (preserved/null
// swapped), but the anchored RC stays in SQL DECLARATION order. The seed must
// follow the anchored order, nullable-wrapping only the null-supplying leg —
// not reorder to preserved-then-null (which would corrupt SELECT */positional
// output). Here the null-supplying leg A is declared FIRST, preserved C second.
func TestRFC173W4_SeedPreservesDeclarationOrder(t *testing.T) {
	t.Parallel()
	a := values.NamedCorrelationIdentifier("A") // null-supplying, declared first
	c := values.NamedCorrelationIdentifier("C") // preserved, declared second
	anchored := values.NewAnchoredJoinRecord([]values.AnchoredJoinLeg{
		{Alias: a, Columns: []values.Field{{Name: "AID", FieldType: values.NotNullLong}, {Name: "AV", FieldType: values.NotNullLong}}},
		{Alias: c, Columns: []values.Field{{Name: "CID", FieldType: values.NotNullLong}}},
	})
	// A is the NULL-supplying side even though it's declared first.
	rc, ok := ordinalSeedFromAnchoredLeft(anchored, c, a).(*values.RecordConstructorValue)
	if !ok || len(rc.Fields) != 3 {
		t.Fatalf("seed must be a 3-field RC in declaration order (A.AID, A.AV, C.CID), got %v", rc)
	}
	// A's two columns FIRST (nullable — null-supplying), then C's (non-nullable).
	wantNullable := []bool{true, true, false}
	for i, f := range rc.Fields {
		fv := f.Value.(*values.FieldValue)
		if fv.Typ.IsNullable() != wantNullable[i] {
			t.Fatalf("field %d (%s) nullable=%v, want %v (A declared first + null-supplying → nullable; C preserved → not)", i, f.Name, fv.Typ.IsNullable(), wantNullable[i])
		}
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
