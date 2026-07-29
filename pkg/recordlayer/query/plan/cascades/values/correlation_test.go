package values

import (
	"strings"
	"sync"
	"testing"
)

func TestCorrelationIdentifier_Named(t *testing.T) {
	t.Parallel()
	a := NamedCorrelationIdentifier("emp")
	b := NamedCorrelationIdentifier("emp")
	if a != b {
		t.Fatal("same-name ids should compare equal")
	}
	if a.Name() != "emp" {
		t.Fatalf("Name: got %q", a.Name())
	}
	if a.String() != "emp" {
		t.Fatalf("String: got %q", a.String())
	}
	if a.IsZero() {
		t.Fatal("non-empty id should not be zero")
	}

	zero := CorrelationIdentifier{}
	if !zero.IsZero() {
		t.Fatal("zero-value id should be zero")
	}
}

func TestCorrelationIdentifier_Unique(t *testing.T) {
	t.Parallel()
	a := UniqueCorrelationIdentifier()
	b := UniqueCorrelationIdentifier()
	if a == b {
		t.Fatal("two unique() calls should never collide")
	}
	if !strings.HasPrefix(a.Name(), "q$") {
		t.Fatalf("unique id prefix: got %q", a.Name())
	}
	if !strings.HasPrefix(b.Name(), "q$") {
		t.Fatalf("unique id prefix: got %q", b.Name())
	}
}

// Parallel allocations of unique IDs don't collide.
func TestCorrelationIdentifier_UniqueRace(t *testing.T) {
	t.Parallel()
	const n = 50
	ids := make([]CorrelationIdentifier, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ids[i] = UniqueCorrelationIdentifier()
		}(i)
	}
	wg.Wait()
	seen := make(map[CorrelationIdentifier]bool, n)
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("unique() race: duplicate id %v", id)
		}
		seen[id] = true
	}
}

// CorrelationIdentifier is usable as a map key (the whole point of
// being a value-type with no pointers).
func TestCorrelationIdentifier_MapKey(t *testing.T) {
	t.Parallel()
	m := map[CorrelationIdentifier]string{}
	a := NamedCorrelationIdentifier("a")
	b := NamedCorrelationIdentifier("b")
	m[a] = "first"
	m[b] = "second"
	m[NamedCorrelationIdentifier("a")] = "overwritten"

	if len(m) != 2 {
		t.Fatalf("expected 2 entries after overwrite, got %d", len(m))
	}
	if m[a] != "overwritten" {
		t.Fatalf("same-name lookup: got %q", m[a])
	}
}

func TestUitoa(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{9, "9"},
		{10, "10"},
		{1234567890, "1234567890"},
		{18446744073709551615, "18446744073709551615"}, // max uint64
	}
	for _, tc := range cases {
		if got := uitoa(tc.in); got != tc.want {
			t.Fatalf("uitoa(%d): got %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSameLeg_IsExact_AndPreservesMintedNamespaceDisjointness pins the leg
// comparison's exactness against the protection that depends on it.
//
// semantic/scope.go upper-folds every USER correlation at its single
// registration chokepoint, while UniqueCorrelationIdentifier mints the machine
// counter in LOWERCASE. scope.go names what the resulting case-disjointness
// buys, verbatim: "the lowercase q$N machine-counter namespace stays
// case-DISJOINT from every upper-folded user correlation — quoted \"q$5\"
// cannot forge a planner-minted q$5."
//
// SameLeg is the one comparison every identity proof routes through, so a fold
// here erases that protection everywhere at once. Java has the property by
// construction: CorrelationIdentifier.equals is exact
// (CorrelationIdentifier.java:132).
//
// What re-arms if this changes: a quoted user alias could be accepted as the
// minted leg it upper-folds onto, and the proofs that consume SameLeg answer
// "which row does this value read" — a forged match is a wrong-rows plan or a
// fabricated cardinality, not a lost optimization.
func TestSameLeg_IsExact_AndPreservesMintedNamespaceDisjointness(t *testing.T) {
	t.Parallel()

	minted := UniqueCorrelationIdentifier()
	if got := minted.String(); got != strings.ToLower(got) {
		t.Fatalf("UniqueCorrelationIdentifier minted %q, which is not lowercase — the "+
			"case-disjointness scope.go relies on comes from this namespace being lower "+
			"while user correlations are upper-folded", got)
	}

	// The forgery: what a quoted user alias spelled the same as the minted one
	// becomes after scope.go's upper-fold.
	forged := NamedCorrelationIdentifier(strings.ToUpper(minted.String()))
	if SameLeg(minted, forged) {
		t.Fatalf("SameLeg(%q, %q) = true — an upper-folded user alias was accepted as the "+
			"planner-minted leg. semantic/scope.go keeps those namespaces case-DISJOINT so a "+
			"quoted identifier cannot forge a minted correlation; a case-folding comparison "+
			"here erases that for every identity proof at once", minted, forged)
	}

	// Same shape one level down, spelled out rather than derived, so the pin
	// survives a change to the minting format.
	if SameLeg(NamedCorrelationIdentifier("q$5"), NamedCorrelationIdentifier("Q$5")) {
		t.Fatal("SameLeg(q$5, Q$5) = true — scope.go's anti-forgery disjointness is erased")
	}

	// Accept direction, so exactness is not satisfied by refusing everything.
	if !SameLeg(minted, NamedCorrelationIdentifier(minted.String())) {
		t.Fatal("SameLeg declined a leg against its own spelling — exactness must still " +
			"recognize identical identifiers")
	}
	if !SameLeg(NamedCorrelationIdentifier("O"), NamedCorrelationIdentifier("O")) {
		t.Fatal("SameLeg declined two identical user aliases")
	}
	if SameLeg(NamedCorrelationIdentifier("O"), NamedCorrelationIdentifier("I")) {
		t.Fatal("SameLeg accepted two different legs")
	}
}
