package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// scanFixture builds a leaf RelationalExpression for merge tests.
func scanFixture(name string) RelationalExpression {
	return NewFullUnorderedScanExpression([]string{name}, values.UnknownType)
}

func TestReference_CanonicalIdentity(t *testing.T) {
	t.Parallel()
	r := InitialOf(scanFixture("T"))
	if r.IsForwarded() {
		t.Fatal("fresh Reference must not be forwarded")
	}
	if r.Canonical() != r {
		t.Fatal("canonical of a live Reference must be itself")
	}
	if (*Reference)(nil).Canonical() != nil {
		t.Fatal("Canonical(nil) must be nil")
	}
}

func TestReference_AbsorbForwardsAndFolds(t *testing.T) {
	t.Parallel()
	survivor := InitialOf(scanFixture("T"))
	loserMember := scanFixture("U")
	loser := InitialOf(loserMember)

	survivor.Absorb(loser)

	if !loser.IsForwarded() {
		t.Fatal("absorbed loser must be forwarded")
	}
	if loser.Canonical() != survivor {
		t.Fatal("loser.Canonical() must resolve to survivor")
	}
	// Loser now transparently delegates: its accessors see survivor state.
	if loser.Get() != survivor.Get() {
		t.Fatal("forwarded Reference must delegate Get() to survivor")
	}
	// Survivor absorbed the loser's distinct member.
	if got := len(survivor.Members()); got != 2 {
		t.Fatalf("survivor has %d members after absorb, want 2", got)
	}
	// Pointer preserved: the exact loser member object is in survivor.
	found := false
	for _, m := range survivor.Members() {
		if m == loserMember {
			found = true
		}
	}
	if !found {
		t.Fatal("survivor must hold the loser's member by the same pointer")
	}
}

func TestReference_AbsorbDedupesEqualMembers(t *testing.T) {
	t.Parallel()
	survivor := InitialOf(scanFixture("T"))
	loser := InitialOf(scanFixture("T")) // structurally equal member

	survivor.Absorb(loser)

	if got := len(survivor.Members()); got != 1 {
		t.Fatalf("equal members must dedup on absorb: survivor has %d, want 1", got)
	}
}

func TestReference_CanonicalPathCompression(t *testing.T) {
	t.Parallel()
	// Build a forwarding chain c -> b -> a by absorbing in sequence on
	// raw objects (Absorb does not canonicalize its receiver).
	a := InitialOf(scanFixture("A"))
	b := InitialOf(scanFixture("B"))
	c := InitialOf(scanFixture("C"))
	a.Absorb(b) // b -> a
	b.Absorb(c) // c -> b (b already forwards to a)

	if c.Canonical() != a {
		t.Fatalf("Canonical must resolve the whole chain to a")
	}
	// Path compression: after Canonical, c points straight at a.
	if c.forwardedTo != a {
		t.Fatal("Canonical must compress the path so c.forwardedTo == a")
	}
	if b.forwardedTo != a {
		t.Fatal("intermediate node must also point straight at a after compression")
	}
}

func TestReference_AbsorbReArmsExploration(t *testing.T) {
	t.Parallel()
	survivor := InitialOf(scanFixture("T"))
	survivor.StartExploration()
	survivor.CommitExploration() // explorationDone
	if survivor.NeedsExploration() {
		t.Fatal("precondition: survivor should be done exploring")
	}
	loser := InitialOf(scanFixture("U")) // distinct member → survivor grows
	survivor.Absorb(loser)
	if !survivor.NeedsExploration() {
		t.Fatal("absorbing a new member must re-arm exploration on the survivor")
	}
}

func TestReference_IDAssignmentIdempotent(t *testing.T) {
	t.Parallel()
	r := InitialOf(scanFixture("T"))
	if r.ID() != 0 {
		t.Fatal("unregistered Reference must have id 0")
	}
	r.AssignMemoID(7)
	r.AssignMemoID(99) // must not overwrite
	if r.ID() != 7 {
		t.Fatalf("AssignMemoID must be set-once: got %d, want 7", r.ID())
	}
}
