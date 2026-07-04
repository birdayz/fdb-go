package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// RFC-173 QP-REF-BIND item 2, commit 3 — the flatten gate arm. These pins
// prove the ordinal seed FIRES at translation for the gated 2+1 flatten
// (the e2e EXPLAIN summary renders the gated and anchored shapes
// identically, so the seed's presence is asserted here, at the layer that
// builds it): a maximal 2-way INNER join under a WHERE-EXISTS filter seeds
// the BAKED ordinal RC (FrontierPinned refs, never the anchored RC), the
// combined predicates bake, and every narrowing declines back to the
// anchored name model (nested-cluster leg, duplicate existential alias,
// enclosure).

// c3ExistsFilter wraps j in a WHERE-EXISTS filter carrying one existential
// subquery over table pg (aliased by alias).
func c3ExistsFilter(j *logical.LogicalJoin, alias string) *logical.LogicalFilter {
	return &logical.LogicalFilter{
		Input: j,
		ExistsSubqueries: []logical.ExistsSubquery{{
			Alias: values.NamedCorrelationIdentifier(alias),
			Plan:  scan("TypedRecord", "g"),
		}},
	}
}

// c3TranslateFlatten translates the filter and returns the resulting select.
func c3TranslateFlatten(t *testing.T, tr *cascadesTranslator, f *logical.LogicalFilter) *expressions.SelectExpression {
	t.Helper()
	ref := tr.translateRef(f)
	if ref == nil {
		t.Fatalf("translation failed: %v", tr.translateErr)
	}
	for _, m := range ref.AllMembers() {
		if sel, ok := m.(*expressions.SelectExpression); ok {
			return sel
		}
	}
	t.Fatal("no SelectExpression member from the flatten translation")
	return nil
}

// c3AssertOrdinalSeed asserts the select's RV is the BAKED ordinal seed:
// a non-anchored RC whose every field is a FrontierPinned baked reference.
func c3AssertOrdinalSeed(t *testing.T, sel *expressions.SelectExpression) {
	t.Helper()
	rc, isRC := sel.GetResultValue().(*values.RecordConstructorValue)
	if !isRC {
		t.Fatalf("gated flatten RV = %T, want the ordinal seed RC", sel.GetResultValue())
	}
	if rc.AnchoredJoin {
		t.Fatal("gated flatten seeded the ANCHORED RC — the ordinal seed did not fire")
	}
	if len(rc.Fields) == 0 {
		t.Fatal("ordinal seed RC has no fields")
	}
	for i, f := range rc.Fields {
		fv, isFV := f.Value.(*values.FieldValue)
		if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			t.Fatalf("seed field %d (%s) is not a FrontierPinned baked reference: %T", i, f.Name, f.Value)
		}
	}
}

// c3AssertAnchoredSeed asserts the select's RV stayed the anchored name-model RC.
func c3AssertAnchoredSeed(t *testing.T, sel *expressions.SelectExpression) {
	t.Helper()
	rc, isRC := sel.GetResultValue().(*values.RecordConstructorValue)
	if !isRC {
		t.Fatalf("flatten RV = %T, want the anchored RC", sel.GetResultValue())
	}
	if !rc.AnchoredJoin {
		t.Fatal("declined flatten did NOT stay anchored — a narrowing failed open")
	}
}

func TestRFC173Item2C3_FlattenGateArm(t *testing.T) {
	t.Parallel()

	t.Run("gated 2-way flatten seeds ordinal", func(t *testing.T) {
		t.Parallel()
		tr := newGateTranslator(t)
		j := inner(scan("Order", "o"), scan("Customer", "c"))
		sel := c3TranslateFlatten(t, tr, c3ExistsFilter(j, "q$e"))
		d, ok := tr.wedgeGate[j]
		if !ok || !d.Gated || d.Arity != 2 {
			t.Fatalf("flatten gate decision = %+v (recorded=%v), want gated arity 2", d, ok)
		}
		c3AssertOrdinalSeed(t, sel)
		// The existential quantifier contributes NO seed columns (Java's
		// model): 3 quantifiers, RV covers only the two ForEach legs' columns.
		if got := len(sel.GetQuantifiers()); got != 3 {
			t.Fatalf("flatten select has %d quantifiers, want 3 (2 ForEach + existential)", got)
		}
	})

	t.Run("nested-cluster leg declines (arity > 2)", func(t *testing.T) {
		t.Parallel()
		tr := newGateTranslator(t)
		nested := inner(scan("Order", "o"), scan("Customer", "c"))
		j := inner(nested, scan("TypedRecord", "tr"))
		sel := c3TranslateFlatten(t, tr, c3ExistsFilter(j, "q$e"))
		c3AssertAnchoredSeed(t, sel)
		// The RECORD matches the seed built — a Gated record over an
		// anchored seed would misroute downstream consumers (the
		// WHERE-conjunct baking arm, commit 4's enclosure lift).
		if d, ok := tr.wedgeGate[j]; !ok || d.Gated {
			t.Fatalf("narrowed flatten's record = %+v (ok=%v), want recorded NOT gated", d, ok)
		}
	})

	t.Run("duplicate existential alias declines", func(t *testing.T) {
		t.Parallel()
		tr := newGateTranslator(t)
		j := inner(scan("Order", "o"), scan("Customer", "c"))
		// The EXISTS alias collides with the right leg's alias.
		sel := c3TranslateFlatten(t, tr, c3ExistsFilter(j, "c"))
		c3AssertAnchoredSeed(t, sel)
		if d, ok := tr.wedgeGate[j]; !ok || d.Gated {
			t.Fatalf("dup-alias flatten's record = %+v (ok=%v), want recorded NOT gated (record must match the anchored seed)", d, ok)
		}
	})

	t.Run("enclosed flatten declines", func(t *testing.T) {
		t.Parallel()
		tr := newGateTranslator(t)
		tr.inInnerCluster = true
		j := inner(scan("Order", "o"), scan("Customer", "c"))
		sel := c3TranslateFlatten(t, tr, c3ExistsFilter(j, "q$e"))
		c3AssertAnchoredSeed(t, sel)
	})
}
