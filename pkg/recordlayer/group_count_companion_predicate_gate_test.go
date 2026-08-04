package recordlayer

import (
	"testing"

	"google.golang.org/protobuf/proto"

	gen "fdb.dev/gen"
)

// newGroupedSumForCompanionTest builds the minimal owner NewGroupCountCompanion
// accepts: a grouped SUM whose grouping half splits exactly.
func newGroupedSumForCompanionTest(name string) *Index {
	idx := NewIndex(name, GroupBy(Field("v"), Field("g")))
	idx.Type = IndexTypeSum
	return idx
}

// TestNewGroupCountCompanion_TautologyIsNotAFilter pins the exact line the
// companion emission gate is drawn on.
//
// The gate exists because a FILTERED aggregate index is not buildable as a
// match candidate, so its companion could only ever be written, never read.
// The question this test settles is where "filtered" stops: at "carries a
// predicate at all", or at "carries a predicate that can reject a record".
//
// It must be the latter. A predicate that provably rejects nothing leaves an
// entry per record, so the owner is a perfectly ordinary dense aggregate index,
// is candidate-buildable, and needs its companion exactly as much as an index
// with no predicate at all. Drawing the gate at HasPredicate would withdraw the
// companion from those indexes only, and phantom groups would come back for
// them — a wrong answer confined to a shape nobody would think to test.
//
// This lives here, not in the DDL tests, because the case has no DDL spelling:
// generatePredicate only serializes scan-prefix comparison ranges, so a
// constant-TRUE predicate never survives that path. The DDL test's unfiltered
// arm cannot distinguish the two gates (an unfiltered index has no predicate
// proto, and both predicates read false on it), which is why the mutation to
// HasPredicate passed there and reddens here.
func TestNewGroupCountCompanion_TautologyIsNotAFilter(t *testing.T) {
	t.Parallel()

	owner := newGroupedSumForCompanionTest("SUM_G")
	tautology := &gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{
		Value: gen.ConstantPredicate_TRUE.Enum(),
	}}
	if err := owner.SetPredicateProto(tautology); err != nil {
		t.Fatalf("SetPredicateProto: %v", err)
	}

	// Fixture guards. The whole point is an index that HAS a predicate but is
	// not FILTERED by it; if either half stops holding, the test is vacuous.
	if !owner.HasPredicate() {
		t.Fatal("fixture carries no predicate, so it cannot distinguish the two gates")
	}
	if owner.HasFilteringPredicate() {
		t.Fatal("fixture's predicate is being read as filtering; the tautology was not " +
			"recognised, so this test cannot cover the non-filtering-predicate case")
	}

	companion, ok := NewGroupCountCompanion(owner)
	if !ok || companion == nil {
		t.Fatal("no companion for an owner whose predicate rejects nothing. The emission " +
			"gate is drawn at 'has a predicate' instead of 'has a FILTERING predicate': " +
			"such an owner holds an entry per record, is candidate-buildable, and needs " +
			"the companion exactly as much as an owner with no predicate at all. Without " +
			"it the vacated-group phantoms return for these indexes only.")
	}
	if companion.Type != IndexTypeCount {
		t.Errorf("companion type = %q, want %q", companion.Type, IndexTypeCount)
	}
	// The predicate must be carried across even though it filters nothing:
	// create-if-absent matches companions by signature, and a companion that
	// dropped the tautology would not be recognised as already present, so every
	// rebuild would try to emit a second one.
	if !SamePredicateSignature(PredicateSignature(owner), PredicateSignature(companion)) {
		t.Errorf("companion's predicate signature does not match its owner's:\n"+
			"  owner:     %x\n  companion: %x",
			PredicateSignature(owner), PredicateSignature(companion))
	}
}

// TestNewGroupCountCompanion_FilteringOwnerDeclined is the unit-level twin of
// the DDL gate test: a predicate that CAN reject a record makes the owner
// unplannable as an aggregate candidate, so no companion may be built.
func TestNewGroupCountCompanion_FilteringOwnerDeclined(t *testing.T) {
	t.Parallel()

	owner := newGroupedSumForCompanionTest("SUM_G")
	if _, ok := NewGroupCountCompanion(owner); !ok {
		t.Fatal("baseline: an unfiltered grouped SUM must get a companion, otherwise " +
			"the assertion below cannot tell the gate from a blanket refusal")
	}

	// A programmatic Go predicate is an opaque closure: it cannot be proved
	// tautological and cannot be serialized onto a companion, so it fails closed.
	owner.Predicate = func(proto.Message) bool { return true }
	if !owner.HasFilteringPredicate() {
		t.Fatal("a programmatic predicate must read as filtering (it is opaque and " +
			"unprovable); the fixture cannot cover the fail-closed case otherwise")
	}
	if companion, ok := NewGroupCountCompanion(owner); ok {
		t.Fatalf("built companion %q for a FILTERED owner. It can never be read — "+
			"buildMatchCandidates declines aggregate candidates for filtered indexes — "+
			"so it is pure write cost on every insert, update and delete.", companion.Name)
	}
}
