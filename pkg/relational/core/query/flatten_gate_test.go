package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// The flatten gate arm: a WHERE-EXISTS filter whose input is a maximal 2-way
// INNER join. These pins prove the ordinal seed FIRES at translation for the
// gated 2+1 flatten (the EXPLAIN summary renders the gated and anchored
// shapes identically, so the seed's presence is asserted here, at the layer
// that builds it): the join seeds the BAKED ordinal RC (FrontierPinned refs),
// the combined predicates bake, and every narrowing declines LOUDLY
// (nested-cluster leg, duplicate existential alias, enclosure) — there is no
// name-model fallback left for a decline to fall to.

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

// c3AssertDeclines asserts the flatten DECLINED loudly: the translation is nil
// and a translate error names the cause (there is no name-model anchored
// fallback left for the decline to fall to).
func c3AssertDeclines(t *testing.T, tr *cascadesTranslator, f *logical.LogicalFilter) {
	t.Helper()
	if ref := tr.translateRef(f); ref != nil {
		t.Fatalf("declined flatten shape translated (%T) — must decline loudly", ref.Members()[0])
	}
	if tr.translateErr == nil {
		t.Fatal("declined flatten must set a LOUD translate error, got nil")
	}
}

func TestFlattenGateArm(t *testing.T) {
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
		c3AssertDeclines(t, tr, c3ExistsFilter(j, "q$e"))
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
				c3AssertDeclines(t, tr, mk())
			})
		}
	})

	t.Run("enclosed flatten declines", func(t *testing.T) {
		t.Parallel()
		tr := newGateTranslator(t)
		tr.inInnerCluster = true
		j := inner(scan("Order", "o"), scan("Customer", "c"))
		c3AssertDeclines(t, tr, c3ExistsFilter(j, "q$e"))
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

// TestEnclosedUnnestDeclines pins that an ENCLOSED lateral unnest — the
// binary name-model residual path, forced here with inInnerCluster=true —
// DECLINES LOUDLY: its FlatMap return value (buildUnnestResultValue) was
// deleted along with the name-model producer, and no production translation
// reaches the enclosed binary path anymore (the gathered/rotated/
// star-normalized paths own every reachable shape). The un-enclosed LIVE
// paths' collection forms are pinned by the unnest seed tests
// (unnestBakedRootCollection, translateGatheredUnnestCluster).
func TestEnclosedUnnestDeclines(t *testing.T) {
	t.Parallel()

	shapes := map[string]func() *logical.LogicalJoin{
		"multi-source outer": func() *logical.LogicalJoin {
			outer := inner(scan("Customer", "c"), scan("Order", "o"))
			return logical.NewJoin(outer, &logical.LogicalUnnest{Segments: []string{"o", "TAGS"}, Alias: "x"}, logical.JoinInner, "")
		},
		"full-outer box outer": func() *logical.LogicalJoin {
			outer := logical.NewJoin(scan("Customer", "c"), scan("Order", "o"), logical.JoinFull, "")
			return logical.NewJoin(outer, &logical.LogicalUnnest{Segments: []string{"o", "TAGS"}, Alias: "x"}, logical.JoinInner, "")
		},
		"single-source outer": func() *logical.LogicalJoin {
			return logical.NewJoin(scan("Order", "o"), &logical.LogicalUnnest{Segments: []string{"o", "TAGS"}, Alias: "x"}, logical.JoinInner, "")
		},
	}
	for name, mk := range shapes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tr := newGateTranslator(t)
			tr.inInnerCluster = true
			if ref := tr.translateRef(mk()); ref != nil {
				t.Fatalf("enclosed unnest translated (%T) — the deleted name-model residual must decline", ref.Members()[0])
			}
			if tr.translateErr == nil {
				t.Fatal("enclosed unnest decline must be LOUD (a translate error), got nil")
			}
		})
	}
}
