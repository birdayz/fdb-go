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

// posMergeSelect builds a POSITIONAL merge select (IsPositionalMergeRC result
// value: `_0`/`_1` over bare QOVs) — one of the shapes that opts into the
// alias-aware interning tier. Two calls with different aliases are MemoEqual
// (equal up to a consistent quantifier-alias renaming), so inserting the second
// into a Reference holding the first records an alias-aware dedup.
func posMergeSelect(a1, a2 string) *SelectExpression {
	q1 := NamedForEachQuantifier(values.NamedCorrelationIdentifier(a1), InitialOf(scanFixture("T")))
	q2 := NamedForEachQuantifier(values.NamedCorrelationIdentifier(a2), InitialOf(scanFixture("T")))
	rv := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "_0", Value: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(a1))},
		values.RecordConstructorField{Name: "_1", Value: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(a2))},
	)
	return NewSelectExpressionWithAliases(rv, []Quantifier{q1, q2}, nil, []string{a1, a2})
}

// TestReference_AbsorbFoldsAliasAwareDedups pins the shadow-delta metric under
// reference merges: Absorb reinserts only the loser's KEPT members, so the
// loser's HISTORICAL alias-aware dedups would be discarded when
// AliasAwareDedups canonicalizes to the survivor — undercounting the shadow.
// Absorb must FOLD the loser's counter into the survivor.
func TestReference_AbsorbFoldsAliasAwareDedups(t *testing.T) {
	t.Parallel()

	// Record a real alias-aware dedup on the loser: the second alias-variant
	// merge select is MemoEqual to the first, so it collapses.
	loser := InitialOf(posMergeSelect("qa", "qb"))
	if loser.Insert(posMergeSelect("qc", "qd")) {
		t.Fatal("precondition: the alias-variant merge select must dedup (MemoEqual) — test vacuous otherwise")
	}
	loserDedups := loser.AliasAwareDedups()
	if loserDedups == 0 {
		t.Fatal("precondition: the loser must have recorded an alias-aware dedup")
	}

	// Survivor holds a structurally DIFFERENT member (a plain scan), so absorbing
	// the loser's kept merge-select member adds no NEW dedup — isolating the fold.
	survivor := InitialOf(scanFixture("Z"))
	before := survivor.AliasAwareDedups()

	survivor.Absorb(loser)

	if got := survivor.AliasAwareDedups(); got != before+loserDedups {
		t.Errorf("Absorb dropped alias-aware dedups: survivor now %d, want %d (before %d + folded loser %d)",
			got, before+loserDedups, before, loserDedups)
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
