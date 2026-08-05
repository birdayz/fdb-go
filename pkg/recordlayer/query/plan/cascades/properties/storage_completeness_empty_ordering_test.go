package properties

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The EMPTY coordinate set is the one shape on which CoordinateBoundClaim's
// central check inverts, and it inverts SILENTLY.
//
// holdsOver is a FOR-ALL over the coordinates a claim was proved on: "every one
// of them is still present". Over an empty proof set that quantifier is
// vacuously true, so a claim bound to no coordinates holds over ANY ordering —
// the strongest possible answer on the weakest possible evidence, from the type
// whose entire purpose is to make "carry a claim forward unexamined"
// inexpressible.
//
// The two properties sharing this type do NOT agree about whether that is a
// defect, which is why the guard is at the stamping site and not in the claim:
//
//   - COMPLETENESS — "these coordinates are the whole storage key" — is a
//     statement about a specific non-empty coordinate list. Over none of them
//     there is nothing to be complete about, so the vacuous yes is simply wrong.
//   - DISTINCTNESS — "the rows flowed are distinct" — is Java's plain boolean
//     and is legitimately coordinate-free. Requiring an ordering before it can
//     be asserted would break the nested-loop join, whose output distinctness
//     comes from its inner leg whether or not that leg advertises any order.
//
// This file pins both halves. Pinning only the first would leave the second
// looking like an oversight for someone to "fix" by moving the guard down into
// CoordinateBoundClaim, which is the change that was tried and reverted here:
// it turns TestConcatOrderings_DistinctnessComesFromInner red.
func TestStorageCompleteness_EmptyOrderingCannotBeComplete(t *testing.T) {
	t.Parallel()

	a := bindingTranslationField("a")

	t.Run("stamping complete on a zero-key ordering is refused", func(t *testing.T) {
		t.Parallel()
		empty := EmptyOrdering().WithStorageKeyComplete(true)
		if empty.StorageKeyIsComplete() {
			t.Fatal("an ordering with NO coordinates reported itself as the WHOLE " +
				"storage key. holdsOver cannot refute a claim proved over nothing, " +
				"so this claim is unfalsifiable from here on: it survives every " +
				"rename and every reduction, and the consumer that ANDs " +
				"completeness with record distinctness marks the plan strictly " +
				"sorted on the strength of an ordering that does not exist")
		}
		if empty.StorageKeyIsComplete() != EmptyOrdering().StorageKeyIsComplete() {
			t.Fatal("stamping complete=true on a zero-key ordering must leave it " +
				"indistinguishable from never having been stamped")
		}
	})

	t.Run("a real ordering is still stampable", func(t *testing.T) {
		t.Parallel()
		// The control. Without it the arm above is satisfied by a guard that
		// refuses every stamp, which would silently disable the property.
		real := NewRichOrdering(
			map[values.Value][]OrderingBinding{a: {SortedBinding(ProvidedSortOrderAscending)}},
			[]values.Value{a}, NotDistinct()).
			WithStorageKeyComplete(true)
		if !real.StorageKeyIsComplete() {
			t.Fatal("a one-coordinate ordering stamped complete does not report " +
				"complete; the empty-set guard has over-reached")
		}
	})

	t.Run("complete=false on a zero-key ordering is unchanged", func(t *testing.T) {
		t.Parallel()
		if EmptyOrdering().WithStorageKeyComplete(false).StorageKeyIsComplete() {
			t.Fatal("complete=false produced a holding claim")
		}
	})

	// The NEGATIVE RESULT, and the reason the guard is not in
	// CoordinateBoundClaim where it would look more general.
	//
	// A zero-coordinate DISTINCTNESS claim is load-bearing: MergeOrderingsForIntersection
	// mints one from the intersection contract, and ConcatOrderings takes the
	// inner leg's claim into a join whose own coordinates come from the outer.
	// Both are Java's semantics. If this arm ever fails, the empty-set refusal
	// has been generalised into the shared type and the join's distinctness has
	// gone with it.
	t.Run("distinctness is DELIBERATELY coordinate-free", func(t *testing.T) {
		t.Parallel()
		coordinateFree := NewRichOrdering(nil, nil, DistinctOverAllKeys())
		if !coordinateFree.IsDistinct() {
			t.Fatal("a zero-coordinate ordering minted with DistinctOverAllKeys is " +
				"no longer distinct. This is NOT the same vacuity the completeness " +
				"guard refuses: distinctness is a claim about ROWS and does not " +
				"need an ordering to be true. Removing it breaks ConcatOrderings, " +
				"whose whole contract is that a nested-loop join inherits its " +
				"inner leg's distinctness")
		}
		if coordinateFree.StorageKeyIsComplete() {
			t.Fatal("the coordinate-free DISTINCTNESS claim leaked into the " +
				"COMPLETENESS answer; the two claims must stay separate fields")
		}
	})
}
