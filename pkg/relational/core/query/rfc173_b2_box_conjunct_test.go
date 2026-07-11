package query

// White-box census pins for RFC-173 B2 sub-slice A (the filtered box unnest).
// The ORDINAL-FIRED proof: a BAKEABLE box-leg conjunct rides the gathered path
// and fires ZERO name-model producers; an UNBAKEABLE one (here: a reference the
// buried window cannot resolve) declines to the name-model lowering and fires
// producers — the discriminating control proving the verdict routes, not just
// passes. Rows/plan correctness is pinned e2e by
// TestFDB_RFC173B2_FilteredBoxUnnest (sqldriver).

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// b2BoxShapeWithPred builds `FROM T4 LEFT JOIN T ON <none>, T4.SARR AS X WHERE
// <pred>` over the chained-spine fixture's catalog (T4 carries the SARR array;
// T is the other leg).
func b2BoxShapeWithPred(pred predicates.QueryPredicate) *logical.LogicalFilter {
	box := logical.NewJoin(scan("T4", "T4"), scan("T", "T"), logical.JoinLeft, "")
	u := &logical.LogicalUnnest{Segments: []string{"T4", "SARR"}, Alias: "X"}
	j := logical.NewJoin(box, u, logical.JoinInner, "")
	return &logical.LogicalFilter{Input: j, Predicate: pred}
}

// b2FilteredBoxShape is b2BoxShapeWithPred with `WHERE T4.<col> = 10`.
func b2FilteredBoxShape(col string) *logical.LogicalFilter {
	return b2BoxShapeWithPred(predicates.NewComparisonPredicate(
		values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("T4")), col, values.UnknownType),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(10)},
		},
	))
}

func TestRFC173B2_FilteredBoxUnnestCensus(t *testing.T) {
	countProducers := func(t *testing.T, f *logical.LogicalFilter) (int, bool) {
		t.Helper()
		var n int
		SetProducerCensusObserver(func(ProducerCensusRecord) { n++ })
		defer SetProducerCensusObserver(nil)
		tr := newChainedSpineTranslator(t)
		sel := tr.translateFilter(f)
		return n, sel != nil
	}

	// BAKEABLE: T4.ID resolves in the box seed's buried window → the gather
	// admits, the merge bakes over the recorded legTypes → ZERO producers.
	t.Run("bakeable_conjunct_ordinalizes", func(t *testing.T) {
		n, ok := countProducers(t, b2FilteredBoxShape("ID"))
		if !ok {
			t.Fatalf("filtered box unnest failed to translate")
		}
		if n != 0 {
			t.Fatalf("bakeable box-leg conjunct fired %d name-model producer(s), want 0 (the gather must admit it)", n)
		}
	})

	// UNBAKEABLE (the discriminating control): a reference the buried window
	// cannot resolve (no such column) → the verdict declines pre-translation →
	// the name-model lowering fires producers. Proves the Bakeable path above is
	// the verdict ROUTING, not a vacuously-quiet observer.
	t.Run("unresolvable_conjunct_declines_name_model", func(t *testing.T) {
		n, ok := countProducers(t, b2FilteredBoxShape("NO_SUCH_COL"))
		if !ok {
			t.Fatalf("unbakeable filtered box unnest failed to translate (must fall to name-model, not nil)")
		}
		if n == 0 {
			t.Fatalf("unbakeable box-leg conjunct fired 0 producers — the decline path is dead or the observer is not wired")
		}
	})

	// PER-ARM classifier pins: each Unbakeable arm of classifyBoxLegConjunct
	// exercised DIRECTLY (the verdict is metadata-only by contract, callable
	// without translating). The census pair above proves the verdict ROUTES;
	// these prove each decline ARM is live — the e2e shapes can't distinguish
	// them (e.g. a SQL scalar subquery may reach the classifier as the minted
	// alias's foreign QOV rather than a ScalarSubqueryValue node, leaving the
	// subquery arm otherwise unexercised).
	t.Run("classifier_unbakeable_arms", func(t *testing.T) {
		t4ID := func() values.Value {
			return values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("T4")), "ID", values.UnknownType)
		}
		eq := func(operand values.Value) predicates.Comparison {
			return predicates.Comparison{Type: predicates.ComparisonEquals, Operand: operand}
		}
		cases := []struct {
			name string
			pred predicates.QueryPredicate
		}{
			{"scalar_subquery_operand", predicates.NewComparisonPredicate(
				t4ID(), eq(values.NewScalarSubqueryValue(values.NamedCorrelationIdentifier("SQ"))))},
			{"exists_value_operand", predicates.NewComparisonPredicate(
				t4ID(), eq(&values.ExistsValue{Value: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("EQ"))}))},
			{"foreign_correlation", predicates.NewComparisonPredicate(
				values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("OUTERQ")), "K", values.UnknownType),
				eq(&values.ConstantValue{Value: int64(10)}))},
			{"dotted_frontier_read", predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "T4.ID", Typ: values.UnknownType},
				eq(&values.ConstantValue{Value: int64(10)}))},
		}
		classify := func(t *testing.T, f *logical.LogicalFilter) boxConjVerdict {
			t.Helper()
			j := f.Input.(*logical.LogicalJoin)
			return newChainedSpineTranslator(t).classifyBoxLegConjunct(
				j.Left.(*logical.LogicalJoin), j.Right.(*logical.LogicalUnnest), f.Predicate)
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := classify(t, b2BoxShapeWithPred(tc.pred)); got != boxConjUnbakeable {
					t.Fatalf("classifyBoxLegConjunct = %d, want Unbakeable(%d)", got, boxConjUnbakeable)
				}
			})
		}
		// BAKEABLE baseline over the same shape — proves the arms above are
		// discriminating (an always-Unbakeable classifier would pass them all).
		t.Run("bakeable_baseline", func(t *testing.T) {
			if got := classify(t, b2FilteredBoxShape("ID")); got != boxConjBakeable {
				t.Fatalf("classifyBoxLegConjunct = %d, want Bakeable(%d)", got, boxConjBakeable)
			}
		})
	})

	// SHAPE-driven classifier gates (vs the predicate-driven arms above). The
	// gate-first check delegates to gatesAsFreshCluster — the SAME wedge-gate
	// authority the gather itself runs — so classify and gather can never
	// diverge on shape admission; these pins nail the authority down on both
	// sides of the gate.
	t.Run("classifier_shape_gates", func(t *testing.T) {
		classify := func(t *testing.T, box *logical.LogicalJoin, legAlias string) boxConjVerdict {
			t.Helper()
			u := &logical.LogicalUnnest{Segments: []string{"T4", "SARR"}, Alias: "X"}
			pred := predicates.NewComparisonPredicate(
				values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(legAlias)), "ID", values.UnknownType),
				predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(10)}},
			)
			return newChainedSpineTranslator(t).classifyBoxLegConjunct(box, u, pred)
		}
		// DUP-ALIAS box (`T4 AS D LEFT T AS D` — two legs binding the SAME
		// correlation): the wedge gate poisons it (indistinguishable leg
		// correlations → name model), so classify must return Unbakeable from
		// the GATE-FIRST check. DISCRIMINATING for that check: without it, the
		// seed map derives, `D` finds a window, `ID` resolves → Bakeable → this
		// pin goes red. This is the pin for the classify-must-never-outrun-the-
		// gate hazard (a genuine name-model box reaching ordinalJoinSeedFields
		// is the panic class).
		t.Run("dup_alias_box_gate_first_declines", func(t *testing.T) {
			dup := logical.NewJoin(scan("T4", "D"), scan("T", "D"), logical.JoinLeft, "")
			if got := classify(t, dup, "D"); got != boxConjUnbakeable {
				t.Fatalf("dup-alias box classify = %d, want Unbakeable(%d) via the gate-first check", got, boxConjUnbakeable)
			}
		})
		// NESTED outer-box leg (`(T4 FULL T) FULL TB`): the wedge gate ADMITS
		// it (nested buried windows derive through the box-as-one-leg concat),
		// so the conjunct classifies BAKEABLE — pinned together with the zero-
		// producer census below and the e2e correct-rows pin
		// (TestFDB_RFC173B2_FilteredBoxUnnest/nested_box_conjunct). Note this
		// is the GATHER authority: boxGatesFresh (the BINARY-seed/birth gate)
		// still excludes nested box legs — the two gates differ by design.
		t.Run("nested_box_leg_classifies_bakeable", func(t *testing.T) {
			nested := logical.NewJoin(
				logical.NewJoin(scan("T4", "T4"), scan("T", "T"), logical.JoinFull, ""),
				scan("T", "TB"), logical.JoinFull, "")
			if got := classify(t, nested, "T4"); got != boxConjBakeable {
				t.Fatalf("nested-box-leg classify = %d, want Bakeable(%d) (the wedge gate admits nested boxes)", got, boxConjBakeable)
			}
			u := &logical.LogicalUnnest{Segments: []string{"T4", "SARR"}, Alias: "X"}
			j := logical.NewJoin(nested, u, logical.JoinInner, "")
			pred := predicates.NewComparisonPredicate(
				values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("T4")), "ID", values.UnknownType),
				predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(10)}},
			)
			n, ok := countProducers(t, &logical.LogicalFilter{Input: j, Predicate: pred})
			if !ok {
				t.Fatalf("nested filtered box unnest failed to translate")
			}
			if n != 0 {
				t.Fatalf("bakeable nested-box conjunct fired %d name-model producer(s), want 0", n)
			}
		})
	})

	// CLUSTERED-LEG box census arm: the box's LEFT leg is an INNER cluster
	// (T4 ⋈ T) under FULL, and the conjunct references the buried NON-OWNER
	// leaf (T.ID). Must classify Bakeable and ride the gathered ordinal path
	// (0 producers) — the e2e sibling (c5a's clustered-box-bakes-ordinal) has
	// path-independent rows, so THIS census arm is the routing discriminator
	// for the clustered dimension.
	t.Run("bakeable_clustered_conjunct_ordinalizes", func(t *testing.T) {
		innerCluster := inner(scan("T4", "T4"), scan("T", "T"))
		box := logical.NewJoin(innerCluster, scan("T", "TB"), logical.JoinFull, "")
		u := &logical.LogicalUnnest{Segments: []string{"T4", "SARR"}, Alias: "X"}
		j := logical.NewJoin(box, u, logical.JoinInner, "")
		pred := predicates.NewComparisonPredicate(
			values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("T")), "ID", values.UnknownType),
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(10)}},
		)
		n, ok := countProducers(t, &logical.LogicalFilter{Input: j, Predicate: pred})
		if !ok {
			t.Fatalf("clustered filtered box unnest failed to translate")
		}
		if n != 0 {
			t.Fatalf("bakeable clustered buried-leg conjunct fired %d name-model producer(s), want 0 (the gather must admit it)", n)
		}
	})
}
