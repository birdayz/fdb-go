package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestEmptyAliasMapIsAnUncorruptedSingleton guards both halves of making the
// empty map shared. The memo asks for it on every plan-level equality, so
// handing back three fresh allocations each time cost 2.5GB over the
// pure-planner sweep — but the builders use an empty map as their mutable
// accumulator, so the sharing is only safe while they start somewhere else.
//
// The corruption half is what needs a test: a builder that reached for
// EmptyAliasMap would still return the right answer to its own caller and
// would poison every reader in the process, silently and permanently.
func TestEmptyAliasMapIsAnUncorruptedSingleton(t *testing.T) {
	t.Parallel()

	if EmptyAliasMap() != EmptyAliasMap() {
		t.Error("EmptyAliasMap returns a fresh value per call; it is allocated on " +
			"every plan-level equality, so it must be shared")
	}

	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	c := values.NamedCorrelationIdentifier("c")

	// Every builder, driven from the empty map — the exact shape that would
	// write through to the singleton if one of them used it as its accumulator.
	pair := AliasMapOf(a, b)
	if _, ok := EmptyAliasMap().With(a, b); !ok {
		t.Fatal("With on the empty map failed")
	}
	if composed := EmptyAliasMap().Compose(pair); composed.Size() != 1 {
		t.Fatalf("Compose produced %d bindings, want 1", composed.Size())
	}
	if extended, ok := pair.With(c, c); !ok || extended.Size() != 2 {
		t.Fatalf("With on a populated map: ok=%v size=%d", ok, extended.Size())
	}

	if !EmptyAliasMap().IsEmpty() || EmptyAliasMap().Size() != 0 {
		t.Fatalf("a builder wrote through to the shared empty map: size=%d",
			EmptyAliasMap().Size())
	}
	if _, bound := EmptyAliasMap().GetTarget(a); bound {
		t.Error("the shared empty map reports a binding")
	}
}
