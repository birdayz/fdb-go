package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// aggOrderingInnerIndexPlan builds the index scan that feeds a streaming
// aggregation in the tests below: an index on (K) over table T, in the given
// direction.
func aggOrderingInnerIndexPlan(reverse bool) *RecordQueryIndexPlan {
	return NewRecordQueryIndexPlan("IDX_K", nil, []string{"T"}, values.UnknownType, reverse).
		WithIndexMetadata([]string{"K"}, []string{"ID"}, false)
}

// aggOrderingFetchOverCovering builds Fetch(Covering(IndexScan)) — the shape
// the access path now builds unconditionally, and therefore the shape an
// ordered inner beneath a streaming aggregation normally has.
func aggOrderingFetchOverCovering(reverse bool) RecordQueryPlan {
	cov := NewRecordQueryCoveringIndexPlan(aggOrderingInnerIndexPlan(reverse))
	return NewRecordQueryFetchFromPartialRecordPlan(cov, nil, values.UnknownType, FetchIndexRecordsPrimaryKey)
}

// aggOverInner builds StreamingAggregation(GROUP BY K, COUNT(*)) over the
// given physical inner, carried as a frozen single-plan edge exactly as the
// implementation rule carries its pinned ordered inner.
func aggOverInner(inner RecordQueryPlan) *RecordQueryStreamingAggregationPlan {
	q := expressions.ForEachQuantifier(expressions.FinalOf(inner))
	return NewRecordQueryStreamingAggregationPlanFromQuantifier(
		q,
		[]values.Value{&values.FieldValue{Field: "K", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{{Function: expressions.AggCount}},
	)
}

// TestStreamingAggOrdering_ReverseDirectionSurvivesCoveringInner pins that the
// streaming aggregation's advertised ordering DIRECTION is read from the index
// scan that actually produces the rows, not from whichever wrapper happens to
// sit directly beneath the aggregate.
//
// The direction is not decoration. A consumer that believes the aggregate emits
// ascending groups will elide its own sort; if the scan underneath is reverse,
// the rows arrive descending and the query returns them in the wrong order with
// nothing failing. Reading the direction off a single concrete plan type means
// any equally-valid producer of the same rows — a covering scan being the one
// the access path now always builds — silently falls back to "ascending", which
// is a WRONG claim rather than an absent one.
//
// Both directions are asserted from the same shape so the test cannot pass by
// the ordering claim being uniformly ascending.
func TestStreamingAggOrdering_ReverseDirectionSurvivesCoveringInner(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		inner   func(reverse bool) RecordQueryPlan
		reverse bool
	}{
		{"bare_forward", func(r bool) RecordQueryPlan { return aggOrderingInnerIndexPlan(r) }, false},
		{"bare_reverse", func(r bool) RecordQueryPlan { return aggOrderingInnerIndexPlan(r) }, true},
		{"covering_forward", func(r bool) RecordQueryPlan {
			return NewRecordQueryCoveringIndexPlan(aggOrderingInnerIndexPlan(r))
		}, false},
		{"covering_reverse", func(r bool) RecordQueryPlan {
			return NewRecordQueryCoveringIndexPlan(aggOrderingInnerIndexPlan(r))
		}, true},
		{"fetch_over_covering_forward", func(r bool) RecordQueryPlan {
			return aggOrderingFetchOverCovering(r)
		}, false},
		{"fetch_over_covering_reverse", func(r bool) RecordQueryPlan {
			return aggOrderingFetchOverCovering(r)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			agg := aggOverInner(tc.inner(tc.reverse))
			o := agg.HintOrdering()
			if !o.IsKnown {
				t.Fatalf("ordering claim should be known for %s", tc.name)
			}
			if len(o.Keys) != 1 {
				t.Fatalf("expected 1 ordering key, got %d", len(o.Keys))
			}
			if len(o.Descending) != 1 {
				t.Fatalf("expected 1 direction flag, got %d", len(o.Descending))
			}
			if o.Descending[0] != tc.reverse {
				t.Fatalf("%s: ordering claims descending=%v, but the producing index scan is reverse=%v — "+
					"a consumer elides its sort on this claim, so a wrong direction returns wrongly-ordered rows silently",
					tc.name, o.Descending[0], tc.reverse)
			}
		})
	}
}
