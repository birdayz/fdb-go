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

// b2FilteredBoxShape builds `FROM T4 LEFT JOIN T ON <none>, T4.SARR AS X WHERE
// T4.<col> = 10` over the chained-spine fixture's catalog (T4 carries the SARR
// array; T is the other leg).
func b2FilteredBoxShape(col string) *logical.LogicalFilter {
	box := logical.NewJoin(scan("T4", "T4"), scan("T", "T"), logical.JoinLeft, "")
	u := &logical.LogicalUnnest{Segments: []string{"T4", "SARR"}, Alias: "X"}
	j := logical.NewJoin(box, u, logical.JoinInner, "")
	pred := predicates.NewComparisonPredicate(
		values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("T4")), col, values.UnknownType),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(10)},
		},
	)
	return &logical.LogicalFilter{Input: j, Predicate: pred}
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
}
