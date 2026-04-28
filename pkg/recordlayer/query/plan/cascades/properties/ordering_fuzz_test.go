package properties_test

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// FuzzEstimateOrdering_NoPanic pins that EstimateOrdering doesn't
// panic on random expression-tree shapes. The walker has to handle
// every operator type in the seed; missing-case bugs would surface
// here as a panic from a nil dereference.
//
// Properties pinned:
//
//  1. No panic on any byte input.
//  2. EstimateOrdering(scan)         → IsKnown=false (always)
//  3. EstimateOrdering(sort(...))    → IsKnown=true (always)
//  4. EstimateOrdering(filter(sort)) → IsKnown=true (preserves)
//  5. EstimateOrdering(filter(scan)) → IsKnown=false (preserves)
//
// The last two properties pin "Filter inherits ordering from inner".
func FuzzEstimateOrdering_NoPanic(f *testing.F) {
	f.Add(true, true)   // sort + filter
	f.Add(false, true)  // scan + filter
	f.Add(true, false)  // sort no filter
	f.Add(false, false) // scan only

	f.Fuzz(func(t *testing.T, hasSort, hasFilter bool) {
		scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)

		var inner expressions.RelationalExpression = scan
		if hasSort {
			keys := []expressions.SortKey{
				{Value: &values.FieldValue{Field: "id", Typ: values.NotNullLong}},
			}
			inner = expressions.NewLogicalSortExpression(keys, expressions.ForEachQuantifier(expressions.InitialOf(inner)))
		}
		if hasFilter {
			pred := predicates.NewConstantPredicate(predicates.TriTrue)
			inner = expressions.NewLogicalFilterExpression(
				[]predicates.QueryPredicate{pred},
				expressions.ForEachQuantifier(expressions.InitialOf(inner)),
			)
		}

		// Property 1: no panic.
		o := properties.EstimateOrdering(inner)

		// Property 2-5: filter inherits ordering from inner; no-sort
		// stack is unknown, sort stack is known.
		// (Filter / Sort always preserve the inner's ordering;
		// Sort introduces ordering even over an unsorted scan.)
		expectedOrdered := hasSort
		if o.IsKnown != expectedOrdered {
			t.Fatalf("hasSort=%v hasFilter=%v: IsKnown = %v, want %v",
				hasSort, hasFilter, o.IsKnown, expectedOrdered)
		}
	})
}
