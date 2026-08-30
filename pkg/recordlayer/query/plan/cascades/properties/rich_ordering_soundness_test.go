package properties

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The soundness law for RichOrdering.Satisfies, stated over concrete rows
// rather than over the ordering algebra:
//
//	if o.Satisfies(req) then a row stream ordered by o is ordered by req.
//
// That is the only thing Satisfies is FOR. A false positive there is not a lost
// plan — it is the planner accepting a scan whose rows arrive in the wrong
// order and eliding the sort that would have fixed them, so the user gets
// wrongly-ordered rows with no error anywhere.
//
// Nothing checked it. FuzzRichOrdering_Satisfies calls Satisfies and DISCARDS
// the result — it is a no-panic fuzz. It also drives every key from one
// bindKind and every requested part from one reqSort, so it cannot generate a
// MIXED ordering (a ascending, b descending), which is where the interesting
// disagreements live. The point tests beside it each pin one hand-built shape.
//
// NULLS PLACEMENT is in scope deliberately, because it is the dimension the
// implementation itself names as the hazard: IsCompatibleWithRequestedSortOrder
// documents that its counterflow-nulls check is "the gate that keeps a forward
// (ASC_NULLS_FIRST) scan from satisfying an ORDER BY x ASC NULLS LAST request,
// so the sort is not wrongly elided". A model with no NULLs in it cannot
// exercise that gate at all, and the first version of this test had none.
//
// So the rows carry NULLs, and the four directional orders are modelled with
// their documented placement (Ascending -> ASC_NULLS_FIRST, Descending ->
// DESC_NULLS_LAST, and the two counterflow spellings that invert it).
//
// That the NULLs are load-bearing is measured, not assumed. Deleting the
// counterflow-nulls check from IsCompatibleWithRequestedSortOrder makes this
// test report 3416 violations. The NULL-free version of this same test — three
// bindings, three requested orders, rows of plain ints — reports ZERO and
// passes GREEN against that identical break, because without a NULL in the data
// ASC_NULLS_FIRST and ASC_NULLS_LAST order rows identically and the gate has
// nothing to gate. A test can enumerate 1053 shapes and still be blind to a
// whole dimension.

type soundnessBinding int

const (
	soundAsc            soundnessBinding = iota // ASC_NULLS_FIRST
	soundDesc                                   // DESC_NULLS_LAST
	soundAscNullsLast                           // ASC_NULLS_LAST (counterflow)
	soundDescNullsFirst                         // DESC_NULLS_FIRST (counterflow)
	soundFixed
)

// soundNull is the modelled NULL. A distinguished sentinel rather than a
// pointer keeps the comparator below readable; nothing else uses this value.
const soundNull = -1

func (b soundnessBinding) ascending() bool {
	return b == soundAsc || b == soundAscNullsLast
}

// nullsFirst reports where this binding puts NULLs, per the documented
// convention on ProvidedSortOrder.
func (b soundnessBinding) nullsFirst() bool {
	return b == soundAsc || b == soundDescNullsFirst
}

func (b soundnessBinding) provided() ProvidedSortOrder {
	switch b {
	case soundAsc:
		return ProvidedSortOrderAscending
	case soundDesc:
		return ProvidedSortOrderDescending
	case soundAscNullsLast:
		return ProvidedSortOrderAscendingNullsLast
	case soundDescNullsFirst:
		return ProvidedSortOrderDescendingNullsFirst
	}
	return ProvidedSortOrderFixed
}

// soundnessRow is one modelled row: a value per ordering key, where soundNull
// stands for SQL NULL.
type soundnessRow [3]int

// buildSoundnessRows produces rows spanning {NULL, 0, 1} per key, EXCEPT for
// keys bound FIXED — an equality-bound key is constant across the stream, which
// is exactly why Satisfies is allowed to skip it in the request.
func buildSoundnessRows(bindings [3]soundnessBinding) []soundnessRow {
	domain := []int{soundNull, 0, 1}
	var rows []soundnessRow
	for _, a := range domain {
		for _, b := range domain {
			for _, c := range domain {
				row := soundnessRow{a, b, c}
				for i, bind := range bindings {
					if bind == soundFixed {
						row[i] = 7 // one constant, shared by every row
					}
				}
				rows = append(rows, row)
			}
		}
	}
	return rows
}

// keyLess compares two values on one key under a direction and a NULL
// placement. Returns (less, decided).
func keyLess(x, y int, ascending, nullsFirst bool) (bool, bool) {
	if x == y {
		return false, false
	}
	if x == soundNull {
		return nullsFirst, true
	}
	if y == soundNull {
		return !nullsFirst, true
	}
	if ascending {
		return x < y, true
	}
	return x > y, true
}

// sortByOrdering arranges rows the way a stream with this ordering arrives:
// lexicographic over the ordering's key sequence, each key in its own direction
// and NULL placement. A FIXED key contributes nothing because every row shares
// its value.
func sortByOrdering(rows []soundnessRow, bindings [3]soundnessBinding) []soundnessRow {
	out := append([]soundnessRow(nil), rows...)
	less := func(x, y soundnessRow) bool {
		for i, bind := range bindings {
			if bind == soundFixed {
				continue
			}
			if lt, decided := keyLess(x[i], y[i], bind.ascending(), bind.nullsFirst()); decided {
				return lt
			}
		}
		return false
	}
	// Insertion sort: the corpus is 27 rows and a hand-rolled sort keeps the
	// comparator visible next to the law it encodes.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && less(out[j], out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// requestedShape is the (ascending, nullsFirst) pair a requested order asks
// for. RequestedSortOrderAny asks for nothing, so it is tried both ways.
func requestedShape(o RequestedSortOrder) (ascending, nullsFirst, concrete bool) {
	switch o {
	case RequestedSortOrderAscending:
		return true, true, true
	case RequestedSortOrderDescending:
		return false, false, true
	case RequestedSortOrderAscendingNullsLast:
		return true, false, true
	case RequestedSortOrderDescendingNullsFirst:
		return false, true, true
	}
	return false, false, false
}

// requestMet reports whether the row sequence honours the request. A part with
// RequestedSortOrderAny accepts any of the four shapes, but the SAME one for
// the whole stream, so each is tried and any may succeed.
func requestMet(rows []soundnessRow, parts []int, orders []RequestedSortOrder) bool {
	var check func(idx, lo, hi int) bool
	check = func(idx, lo, hi int) bool {
		if idx >= len(parts) || hi-lo <= 1 {
			return true
		}
		key := parts[idx]
		try := func(ascending, nullsFirst bool) bool {
			for i := lo + 1; i < hi; i++ {
				if lt, decided := keyLess(rows[i][key], rows[i-1][key], ascending, nullsFirst); decided && lt {
					return false // strictly out of order
				}
			}
			runStart := lo
			for i := lo + 1; i <= hi; i++ {
				if i == hi || rows[i][key] != rows[runStart][key] {
					if !check(idx+1, runStart, i) {
						return false
					}
					runStart = i
				}
			}
			return true
		}
		if ascending, nullsFirst, concrete := requestedShape(orders[idx]); concrete {
			return try(ascending, nullsFirst)
		}
		return try(true, true) || try(true, false) || try(false, true) || try(false, false)
	}
	return check(0, 0, len(rows))
}

func TestRichOrdering_SatisfiesIsSoundOverConcreteRows(t *testing.T) {
	t.Parallel()

	keyNames := []string{"a", "b", "c"}
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

	accepted, checked, violations := 0, 0, 0
	requests := enumerateRequests(keys, allOrders)
	for _, ba := range allBindings {
		for _, bb := range allBindings {
			for _, bc := range allBindings {
				bindings := [3]soundnessBinding{ba, bb, bc}
				bm := map[values.Value][]OrderingBinding{}
				for i, key := range keys {
					if bindings[i] == soundFixed {
						bm[key] = []OrderingBinding{FixedBinding("eq")}
						continue
					}
					bm[key] = []OrderingBinding{SortedBinding(bindings[i].provided())}
				}
				ordering := NewRichOrdering(bm, keys, NotDistinct())
				rows := sortByOrdering(buildSoundnessRows(bindings), bindings)

				for _, req := range requests {
					checked++
					requested := NewRequestedOrdering(req.parts, DistinctnessNotDistinct, false)
					if !ordering.Satisfies(requested) {
						continue
					}
					accepted++
					if !requestMet(rows, req.keyIdx, req.orders) {
						violations++
						t.Errorf("Satisfies accepted an ordering that does not meet the request.\n"+
							"    ordering  : %v\n    request   : keys=%v orders=%v\n"+
							"    A plan with this ordering would have its sort elided and return "+
							"rows in the wrong order, with no error anywhere.",
							bindings, req.keyIdx, req.orders)
					}
				}
			}
		}
	}

	// Both populations, because either collapsing makes the verdict noise.
	// 125 orderings (5^3) x 155 requests (5 + 25 + 125 for lengths 1..3).
	if checked != 125*155 {
		t.Fatalf("enumerated %d (ordering, request) pairs, want %d — the enumeration "+
			"changed shape and the law above was checked over something else", checked, 125*155)
	}
	// The one that matters: if Satisfies rejected everything, every row check was
	// skipped and this test passes while proving nothing. Floored well under the
	// measured acceptance so ordinary tightening does not trip it.
	if accepted < 1000 {
		t.Fatalf("Satisfies accepted only %d of %d pairs — too few to exercise the law; "+
			"it has become far stricter and this test is now close to vacuous",
			accepted, checked)
	}
	t.Logf("ordering soundness: %d pairs, %d accepted by Satisfies, %d violations",
		checked, accepted, violations)
}

type soundnessRequest struct {
	parts  []RequestedOrderingPart
	keyIdx []int
	orders []RequestedSortOrder
}

// enumerateRequests yields every request over key prefixes of length 1..3 with
// every direction assignment. Prefixes rather than arbitrary subsets: a request
// names the columns an operator needs sorted, in order, and Satisfies is asked
// about that sequence.
func enumerateRequests(keys []values.Value, orders []RequestedSortOrder) []soundnessRequest {
	var out []soundnessRequest
	var build func(idx []int, ord []RequestedSortOrder)
	build = func(idx []int, ord []RequestedSortOrder) {
		if len(idx) > 0 {
			parts := make([]RequestedOrderingPart, len(idx))
			for i, k := range idx {
				parts[i] = RequestedOrderingPart{Value: keys[k], SortOrder: ord[i]}
			}
			out = append(out, soundnessRequest{
				parts:  parts,
				keyIdx: append([]int(nil), idx...),
				orders: append([]RequestedSortOrder(nil), ord...),
			})
		}
		if len(idx) == len(keys) {
			return
		}
		for _, o := range orders {
			build(append(idx, len(idx)), append(ord, o))
		}
	}
	build(nil, nil)
	return out
}
