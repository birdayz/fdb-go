package query

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// RFC-173 QP-REF-BIND item 2, commit 3 (+ the P2 N-way generalization) —
// the flatten gate arm. These pins prove the ordinal seed FIRES at
// translation for the gated N+M flatten (the e2e EXPLAIN summary renders
// the gated and anchored shapes identically, so the seed's presence is
// asserted here, at the layer that builds it): a maximal INNER cluster
// under a WHERE-EXISTS filter seeds the BAKED ordinal RC (FrontierPinned
// refs, never the anchored RC) — 2-way AND gathered N-way alike (the
// former arity-exactly-2 narrowing retired with the P2 slice) — the
// combined predicates bake, and the remaining narrowings decline back to
// the anchored name model (duplicate existential alias, enclosure).

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

	t.Run("nested-cluster flatten gates N-way (arity 3)", func(t *testing.T) {
		t.Parallel()
		// The P2 chartered flip: this shape DECLINED to the anchored name
		// model while the flatten narrowed to arity exactly 2; it now rides
		// the gathered-cluster seed — one flat select over the three
		// FROM-order legs plus the trailing existential, the shape the
		// executor's implementNWayJoinWithExistential consumes.
		tr := newGateTranslator(t)
		nested := inner(scan("Order", "o"), scan("Customer", "c"))
		j := inner(nested, scan("TypedRecord", "tr"))
		sel := c3TranslateFlatten(t, tr, c3ExistsFilter(j, "q$e"))
		c3AssertOrdinalSeed(t, sel)
		if got := len(sel.GetQuantifiers()); got != 4 {
			t.Fatalf("N-way flatten select has %d quantifiers, want 4 (3 ForEach + existential)", got)
		}
		// Trailing-existential invariant (the executor dispatch requires it).
		quants := sel.GetQuantifiers()
		for i, q := range quants[:3] {
			if q.Kind() != expressions.QuantifierForEach {
				t.Fatalf("quantifier %d kind = %v, want ForEach before the trailing existential", i, q.Kind())
			}
		}
		if quants[3].Kind() != expressions.QuantifierExistential {
			t.Fatalf("last quantifier kind = %v, want Existential", quants[3].Kind())
		}
		// The RECORD matches the seed built (P2 H6): gated, post-flattening
		// arity 3.
		if d, ok := tr.wedgeGate[j]; !ok || !d.Gated || d.Arity != 3 {
			t.Fatalf("N-way flatten's record = %+v (ok=%v), want gated arity 3", d, ok)
		}
	})

	t.Run("duplicate existential alias declines", func(t *testing.T) {
		t.Parallel()
		// All three collision axes the `seen` map guards: EXISTS-vs-right-leg,
		// EXISTS-vs-left-leg, and EXISTS-vs-EXISTS.
		for name, mk := range map[string]func() *logical.LogicalFilter{
			"right leg": func() *logical.LogicalFilter {
				return c3ExistsFilter(inner(scan("Order", "o"), scan("Customer", "c")), "c")
			},
			"left leg": func() *logical.LogicalFilter {
				return c3ExistsFilter(inner(scan("Order", "o"), scan("Customer", "c")), "o")
			},
			"exists vs exists": func() *logical.LogicalFilter {
				f := c3ExistsFilter(inner(scan("Order", "o"), scan("Customer", "c")), "q$e")
				f.ExistsSubqueries = append(f.ExistsSubqueries, logical.ExistsSubquery{
					Alias: values.NamedCorrelationIdentifier("q$e"),
					Plan:  scan("TypedRecord", "g2"),
				})
				return f
			},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				tr := newGateTranslator(t)
				f := mk()
				j := f.Input.(*logical.LogicalJoin) //nolint:errcheck // fixture shape
				sel := c3TranslateFlatten(t, tr, f)
				c3AssertAnchoredSeed(t, sel)
				if d, ok := tr.wedgeGate[j]; !ok || d.Gated {
					t.Fatalf("dup-alias flatten's record = %+v (ok=%v), want recorded NOT gated (record must match the anchored seed)", d, ok)
				}
			})
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

// c3FindExplode walks a translated reference for the ExplodeExpression.
func c3FindExplode(ref *expressions.Reference, seen map[*expressions.Reference]bool) *expressions.ExplodeExpression {
	if ref == nil || seen[ref] {
		return nil
	}
	seen[ref] = true
	for _, m := range ref.AllMembers() {
		if ex, ok := m.(*expressions.ExplodeExpression); ok {
			return ex
		}
		for _, q := range m.GetQuantifiers() {
			if ex := c3FindExplode(q.GetRangesOver(), seen); ex != nil {
				return ex
			}
		}
	}
	return nil
}

// TestRFC173Item2C3_UnnestQualifiedReadRightmostSource pins the
// order-INDEPENDENCE of the binary unnest path's array read at the layer
// that builds it: for a MULTI-SOURCE outer the Explode's collection
// reference reads the QUALIFIED SEG0.FIELD key even when the unnest source
// is the RIGHTMOST FROM leg. The bare read was last-leg-wins over the
// merged row — correct only while the join's execution operand order
// cooperated; the deterministic tie-break drove the swapped order and the
// Explode returned the OTHER leg's array (silent wrong rows, caught by the
// unnest matrix). A translation-level pin holds regardless of which operand
// order the cost model picks — the e2e matrix coverage of the swapped order
// is contingent on cost-model internals.
func TestRFC173Item2C3_UnnestQualifiedReadRightmostSource(t *testing.T) {
	t.Parallel()

	t.Run("multi-source outer reads qualified (rightmost source)", func(t *testing.T) {
		t.Parallel()
		tr := newGateTranslator(t)
		// Enclosed: routes the BINARY unnest path (the W5 gathered path owns
		// un-enclosed multi-source shapes with its own baked correlation).
		tr.inInnerCluster = true
		outer := inner(scan("Customer", "c"), scan("Order", "o"))
		j := logical.NewJoin(outer, &logical.LogicalUnnest{Segments: []string{"o", "TAGS"}, Alias: "x"}, logical.JoinInner, "")
		ref := tr.translateRef(j)
		if ref == nil {
			t.Fatalf("translation failed: %v", tr.translateErr)
		}
		ex := c3FindExplode(ref, map[*expressions.Reference]bool{})
		if ex == nil {
			t.Fatal("no ExplodeExpression in the translated unnest join")
		}
		fv, isFV := ex.GetCollectionValue().(*values.FieldValue)
		if !isFV {
			t.Fatalf("Explode collection = %T, want a FieldValue over the outer QOV", ex.GetCollectionValue())
		}
		if !strings.EqualFold(fv.Field, "O.TAGS") {
			t.Fatalf("Explode reads %q, want the QUALIFIED %q — a bare read is last-leg-wins over the merged row and follows the join's execution operand order", fv.Field, "O.TAGS")
		}
	})

	t.Run("full-outer box outer reads qualified", func(t *testing.T) {
		t.Parallel()
		// A FULL OUTER box is merge-OPAQUE (clusterArity counts it as ONE
		// post-flattening quantifier) yet its output row is MERGED — bare
		// keys are last-leg-wins across both legs. clusterArity is therefore
		// the WRONG proxy for row shape here: the authority is the outer
		// row's visible namespace count (outerBoundAliases). A bare read
		// over `FROM a FULL JOIN b, a.arr AS x` explodes whichever leg's
		// array merged last.
		tr := newGateTranslator(t)
		tr.inInnerCluster = true
		outer := logical.NewJoin(scan("Customer", "c"), scan("Order", "o"), logical.JoinFull, "")
		j := logical.NewJoin(outer, &logical.LogicalUnnest{Segments: []string{"o", "TAGS"}, Alias: "x"}, logical.JoinInner, "")
		ref := tr.translateRef(j)
		if ref == nil {
			t.Fatalf("translation failed: %v", tr.translateErr)
		}
		ex := c3FindExplode(ref, map[*expressions.Reference]bool{})
		if ex == nil {
			t.Fatal("no ExplodeExpression in the translated unnest join")
		}
		fv, isFV := ex.GetCollectionValue().(*values.FieldValue)
		if !isFV {
			t.Fatalf("Explode collection = %T, want a FieldValue", ex.GetCollectionValue())
		}
		if !strings.EqualFold(fv.Field, "O.TAGS") {
			t.Fatalf("FULL-box Explode reads %q, want the QUALIFIED %q — the box is merge-opaque (arity 1) but its ROW is merged (bare keys last-leg-wins)", fv.Field, "O.TAGS")
		}
	})

	t.Run("single-source outer keeps the bare read", func(t *testing.T) {
		t.Parallel()
		tr := newGateTranslator(t)
		tr.inInnerCluster = true
		j := logical.NewJoin(scan("Order", "o"), &logical.LogicalUnnest{Segments: []string{"o", "TAGS"}, Alias: "x"}, logical.JoinInner, "")
		ref := tr.translateRef(j)
		if ref == nil {
			t.Fatalf("translation failed: %v", tr.translateErr)
		}
		ex := c3FindExplode(ref, map[*expressions.Reference]bool{})
		if ex == nil {
			t.Fatal("no ExplodeExpression in the translated unnest join")
		}
		fv, isFV := ex.GetCollectionValue().(*values.FieldValue)
		if !isFV {
			t.Fatalf("Explode collection = %T, want a FieldValue", ex.GetCollectionValue())
		}
		if strings.Contains(fv.Field, ".") {
			t.Fatalf("single-source Explode reads %q, want the BARE field (scan rows carry bare keys only)", fv.Field)
		}
	})
}
