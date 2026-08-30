package properties

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestMergeOrderingsForUnion_NeverOutClaimsItsLegs is the soundness law for the
// union merge, and it composes with the row-level law in
// rich_ordering_soundness_test.go:
//
//	if merge(a,b).Satisfies(req) then a.Satisfies(req) AND b.Satisfies(req).
//
// A union's output stream is an interleaving of its legs, so the strongest
// ordering it can claim is one BOTH legs already provide. If the merge claims
// more, the union advertises an order one of its inputs does not deliver, a
// downstream sort is elided on that claim, and rows come back misordered — the
// same silent failure the Satisfies law guards, reached through the merge
// instead.
//
// FuzzMergeOrderings_NoPanic asserts only that the result is non-nil. It also
// offers each leg just two binding kinds (ascending or fixed) and drives every
// key of a leg from the same one, so it never builds a leg with mixed
// directions or any NULLS-placement variant.
//
// Scoped to UNION deliberately. MergeOrderingsForIntersection is NOT bound by
// this law: rows surviving every leg are distinct under the comparison contract,
// so it may legitimately claim DistinctOverAllKeys() — strictly more than either
// input advertises. Asserting the same law there would be wrong, not stricter.
func TestMergeOrderingsForUnion_NeverOutClaimsItsLegs(t *testing.T) {
	t.Parallel()

	keyNames := []string{"a", "b"}
	keys := make([]values.Value, len(keyNames))
	for i, name := range keyNames {
		keys[i] = propertyField(t, name, values.NullableLong)
	}

	allBindings := []soundnessBinding{
		soundAsc, soundDesc, soundAscNullsLast, soundDescNullsFirst, soundFixed,
	}
	allOrders := []RequestedSortOrder{
		RequestedSortOrderAscending, RequestedSortOrderDescending,
		RequestedSortOrderAscendingNullsLast, RequestedSortOrderDescendingNullsFirst,
		RequestedSortOrderAny,
	}
	requests := enumerateRequests(keys, allOrders)

	build := func(bindings [2]soundnessBinding) *RichOrdering {
		bm := map[values.Value][]OrderingBinding{}
		for i, key := range keys {
			if bindings[i] == soundFixed {
				bm[key] = []OrderingBinding{FixedBinding("eq")}
				continue
			}
			bm[key] = []OrderingBinding{SortedBinding(bindings[i].provided())}
		}
		return NewRichOrdering(bm, keys, NotDistinct())
	}

	var legShapes [][2]soundnessBinding
	for _, x := range allBindings {
		for _, y := range allBindings {
			legShapes = append(legShapes, [2]soundnessBinding{x, y})
		}
	}

	checked, mergedAccepts, overClaims := 0, 0, 0
	for _, sa := range legShapes {
		a := build(sa)
		for _, sb := range legShapes {
			b := build(sb)
			merged := MergeOrderingsForUnion(a, b)
			if merged == nil {
				t.Fatalf("MergeOrderingsForUnion(%v, %v) returned nil", sa, sb)
			}
			for _, req := range requests {
				checked++
				requested := NewRequestedOrdering(req.parts, DistinctnessNotDistinct, false)
				if !merged.Satisfies(requested) {
					continue
				}
				mergedAccepts++
				if !a.Satisfies(requested) || !b.Satisfies(requested) {
					overClaims++
					t.Errorf("union merge out-claims a leg.\n"+
						"    leg A     : %v (satisfies=%v)\n"+
						"    leg B     : %v (satisfies=%v)\n"+
						"    request   : keys=%v orders=%v\n"+
						"    The merged ordering accepts a request a leg does not, so a union "+
						"over these legs advertises an order one input never delivers.",
						sa, a.Satisfies(requested), sb, b.Satisfies(requested),
						req.keyIdx, req.orders)
				}
			}
		}
	}

	// 25 leg shapes squared x 30 requests (5 + 25 for lengths 1..2).
	if checked != 25*25*30 {
		t.Fatalf("enumerated %d (legA, legB, request) triples, want %d — the enumeration "+
			"changed shape and the law was checked over something else", checked, 25*25*30)
	}
	// The vacuity guard. Every comparison sits behind a `continue` on the merged
	// ordering rejecting the request; a merge that claimed nothing would skip all
	// of them and pass clean. Measured: 1566 of 18750.
	if mergedAccepts < 500 {
		t.Fatalf("the merged ordering accepted only %d of %d requests — too few to exercise "+
			"the law, which is now close to vacuous", mergedAccepts, checked)
	}
	t.Logf("union merge law: %d triples, %d merged-accepts, %d over-claims",
		checked, mergedAccepts, overClaims)
}
