package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func composeAlias(name string) values.CorrelationIdentifier {
	return values.NamedCorrelationIdentifier(name)
}

func composeMapOf(pairs ...string) *AliasMap {
	b := NewAliasMapBuilder()
	for i := 0; i+1 < len(pairs); i += 2 {
		b.Put(composeAlias(pairs[i]), composeAlias(pairs[i+1]))
	}
	return b.Build()
}

// TestAliasMapCompose_IsNotAssociative pins a property of this package's
// AliasMap.Compose that its signature invites you to assume and that does not
// hold.
//
// Compose is function composition, and the result's domain is the LEFT map's
// domain while lookups fall back to identity. Those two together break
// associativity whenever an intermediate alias is outside the middle map's
// domain but inside the right map's:
//
//	m={A→B} n={} p={B→E}
//	(m∘n)∘p = {A→E}   m∘n is {A→B}; p then rewrites B
//	m∘(n∘p) = {A→B}   n∘p is {} — it kept n's domain and dropped p entirely
//
// Unreachable today: no production code calls either AliasMap.Compose. This
// exists so that the first caller to chain three maps finds the property stated
// rather than discovering it from a wrong alias downstream, where it would
// present as a stale correlation rather than as an algebra problem.
func TestAliasMapCompose_IsNotAssociative(t *testing.T) {
	t.Parallel()

	m := composeMapOf("A", "B")
	n := composeMapOf()
	p := composeMapOf("B", "E")

	left := m.Compose(n).Compose(p)
	right := m.Compose(n.Compose(p))

	if left.GetTarget(composeAlias("A")) == right.GetTarget(composeAlias("A")) {
		t.Fatal("AliasMap.Compose has become associative on the shape that broke it. " +
			"If Compose now carries the right operand's mappings for sources outside the " +
			"left map's domain, it is the identity-extended composition and this test " +
			"should be replaced by an associativity law over a corpus of maps.")
	}
	if got := left.GetTarget(composeAlias("A")); got.Name() != "E" {
		t.Errorf("(m∘n)∘p maps A to %s, want E", got.Name())
	}
	if got := right.GetTarget(composeAlias("A")); got.Name() != "B" {
		t.Errorf("m∘(n∘p) maps A to %s, want B", got.Name())
	}

	// The control: when the middle map covers the intermediate, the two
	// groupings agree. Without it, "not associative" could be read as "Compose
	// is broken for everything", which it is not.
	covering := composeMapOf("B", "B")
	if a, b := m.Compose(covering).Compose(p).GetTarget(composeAlias("A")),
		m.Compose(covering.Compose(p)).GetTarget(composeAlias("A")); a != b {
		t.Errorf("with the middle map covering the intermediate the groupings must agree, "+
			"got %s vs %s", a.Name(), b.Name())
	}
}

// TestAliasMapCompose_MeansSomethingElseInTheExpressionsPackage pins the naming
// hazard the two doc comments now describe: two types called AliasMap, each with
// a Compose method, doing opposite things.
//
// This package's is FUNCTION COMPOSITION — A→B composed with B→C is A→C, one
// binding. expressions.AliasMap.Compose is a MERGE (Java's combine()) — the same
// two inputs yield BOTH bindings, and a conflicting one panics. A caller who
// reaches for the wrong type gets no compile error and different behaviour.
func TestAliasMapCompose_MeansSomethingElseInTheExpressionsPackage(t *testing.T) {
	t.Parallel()

	composed := composeMapOf("A", "B").Compose(composeMapOf("B", "C"))
	if composed.Size() != 1 {
		t.Fatalf("this package's Compose produced %d bindings, want 1 — it is function "+
			"composition, so A→B and B→C collapse to a single A→C", composed.Size())
	}
	if got := composed.GetTarget(composeAlias("A")); got.Name() != "C" {
		t.Errorf("A maps to %s, want C", got.Name())
	}
	if _, mapped := composed.GetTargetOrEmpty(composeAlias("B")); mapped {
		t.Error("B is still bound in the result; function composition keeps only the LEFT " +
			"map's domain, and a B binding is what the expressions package's MERGE would " +
			"produce instead")
	}
}
