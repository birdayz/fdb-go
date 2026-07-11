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
}
