package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestAliasMap_Empty(t *testing.T) {
	t.Parallel()
	m := EmptyAliasMap()
	if !m.IsEmpty() {
		t.Fatal("EmptyAliasMap not empty")
	}
	if m.Size() != 0 {
		t.Fatalf("size=%d, want 0", m.Size())
	}
	a := values.NamedCorrelationIdentifier("x")
	if _, ok := m.GetTarget(a); ok {
		t.Fatal("empty map returned target")
	}
	if _, ok := m.GetSource(a); ok {
		t.Fatal("empty map returned source")
	}
}

func TestAliasMap_Of(t *testing.T) {
	t.Parallel()
	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	c := values.NamedCorrelationIdentifier("c")
	d := values.NamedCorrelationIdentifier("d")
	m := AliasMapOf(a, b, c, d)
	if m.IsEmpty() {
		t.Fatal("non-empty map reports empty")
	}
	if m.Size() != 2 {
		t.Fatalf("size=%d, want 2", m.Size())
	}
	if got, ok := m.GetTarget(a); !ok || got != b {
		t.Fatalf("target(a)=%v, ok=%v, want %v, true", got, ok, b)
	}
	if got, ok := m.GetSource(d); !ok || got != c {
		t.Fatalf("source(d)=%v, ok=%v, want %v, true", got, ok, c)
	}
	if !m.ContainsSource(a) || !m.ContainsTarget(b) {
		t.Fatal("ContainsSource/Target wrong")
	}
	if m.ContainsSource(b) || m.ContainsTarget(a) {
		t.Fatal("source/target inverted")
	}
}

func TestAliasMap_Of_OddArgs(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on odd args")
		}
	}()
	// Build the slice via a runtime-shaped length so staticcheck (SA5012)
	// can't conclude the variadic call has an even/odd literal count.
	args := makeIdentRange(1, "a")
	_ = AliasMapOf(args...)
}

// makeIdentRange returns N identifiers named base+index, used to
// construct slices whose length is opaque to staticcheck.
func makeIdentRange(n int, base string) []values.CorrelationIdentifier {
	out := make([]values.CorrelationIdentifier, n)
	for i := 0; i < n; i++ {
		out[i] = values.NamedCorrelationIdentifier(base)
	}
	return out
}

func TestAliasMap_Of_DuplicateSource(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate source")
		}
	}()
	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	c := values.NamedCorrelationIdentifier("c")
	_ = AliasMapOf(a, b, a, c)
}

func TestAliasMap_Of_DuplicateTarget(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate target")
		}
	}()
	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	c := values.NamedCorrelationIdentifier("c")
	_ = AliasMapOf(a, b, c, b)
}

func TestAliasMap_Compose_Empty(t *testing.T) {
	t.Parallel()
	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	m := AliasMapOf(a, b)
	composed := m.Compose(EmptyAliasMap())
	if !composed.Equals(m) {
		t.Fatal("compose with empty changed map")
	}
	composed2 := EmptyAliasMap().Compose(m)
	if !composed2.Equals(m) {
		t.Fatal("compose onto empty wrong")
	}
}

func TestAliasMap_Compose_Disjoint(t *testing.T) {
	t.Parallel()
	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	c := values.NamedCorrelationIdentifier("c")
	d := values.NamedCorrelationIdentifier("d")
	m1 := AliasMapOf(a, b)
	m2 := AliasMapOf(c, d)
	composed := m1.Compose(m2)
	if composed.Size() != 2 {
		t.Fatalf("size=%d, want 2", composed.Size())
	}
	if got, _ := composed.GetTarget(a); got != b {
		t.Fatalf("a→%v, want %v", got, b)
	}
	if got, _ := composed.GetTarget(c); got != d {
		t.Fatalf("c→%v, want %v", got, d)
	}
}

func TestAliasMap_Compose_Conflict(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on conflicting Compose")
		}
	}()
	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	c := values.NamedCorrelationIdentifier("c")
	m1 := AliasMapOf(a, b)
	m2 := AliasMapOf(a, c)
	_ = m1.Compose(m2)
}

func TestAliasMap_Equals(t *testing.T) {
	t.Parallel()
	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	c := values.NamedCorrelationIdentifier("c")
	d := values.NamedCorrelationIdentifier("d")
	m1 := AliasMapOf(a, b, c, d)
	m2 := AliasMapOf(c, d, a, b) // same bindings, built in different order
	m3 := AliasMapOf(a, b)       // subset
	if !m1.Equals(m2) {
		t.Fatal("equal maps reported unequal")
	}
	if m1.Equals(m3) {
		t.Fatal("size-mismatched maps reported equal")
	}
	if !EmptyAliasMap().Equals(EmptyAliasMap()) {
		t.Fatal("empty maps not equal")
	}
}

// ---------------------------------------------------------------------------
// Tests ported from Java AliasMapTest.java
// Java uses builder().put().build() -- Go equivalent is AliasMapOf().
// Java combine() -- Go equivalent is Compose() (merge with conflict panic).
// Java compose() (mathematical function composition) -- not yet in Go.
// Java findCompleteMatches() -- not yet in Go.
// ---------------------------------------------------------------------------

// TestAliasMap_Combine ports Java testCombine.
// Java's combine() merges two disjoint maps; Go's Compose() does the same.
func TestAliasMap_Combine(t *testing.T) {
	t.Parallel()

	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	c := values.NamedCorrelationIdentifier("c")
	d := values.NamedCorrelationIdentifier("d")
	u := values.NamedCorrelationIdentifier("u")
	v := values.NamedCorrelationIdentifier("v")
	w := values.NamedCorrelationIdentifier("w")
	x := values.NamedCorrelationIdentifier("x")

	left := AliasMapOf(a, c, b, d)
	right := AliasMapOf(u, w, v, x)
	combined := left.Compose(right)

	if combined.Size() != 4 {
		t.Fatalf("size=%d, want 4", combined.Size())
	}

	for _, tc := range []struct {
		src, tgt values.CorrelationIdentifier
	}{
		{a, c}, {b, d}, {u, w}, {v, x},
	} {
		if !combined.ContainsSource(tc.src) {
			t.Fatalf("missing source %v", tc.src)
		}
		if !combined.ContainsTarget(tc.tgt) {
			t.Fatalf("missing target %v", tc.tgt)
		}
		if got, ok := combined.GetTarget(tc.src); !ok || got != tc.tgt {
			t.Fatalf("target(%v)=%v, ok=%v, want %v", tc.src, got, ok, tc.tgt)
		}
	}
}

// TestAliasMap_CombineIncompatible_SourceConflict ports Java testCombineIncompatible1.
// Both maps bind source "a" to different targets — Compose must panic.
func TestAliasMap_CombineIncompatible_SourceConflict(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on source conflict in Compose")
		}
	}()

	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	c := values.NamedCorrelationIdentifier("c")
	d := values.NamedCorrelationIdentifier("d")
	v := values.NamedCorrelationIdentifier("v")
	w := values.NamedCorrelationIdentifier("w")
	x := values.NamedCorrelationIdentifier("x")

	left := AliasMapOf(a, c, b, d)
	right := AliasMapOf(a, w, v, x) // "a" conflicts
	_ = left.Compose(right)
}

// TestAliasMap_CombineIncompatible_TargetConflict ports Java testCombineIncompatible2.
// Both maps bind target "c" from different sources — Compose must panic.
func TestAliasMap_CombineIncompatible_TargetConflict(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on target conflict in Compose")
		}
	}()

	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	c := values.NamedCorrelationIdentifier("c")
	d := values.NamedCorrelationIdentifier("d")
	u := values.NamedCorrelationIdentifier("u")
	v := values.NamedCorrelationIdentifier("v")
	x := values.NamedCorrelationIdentifier("x")

	left := AliasMapOf(a, c, b, d)
	right := AliasMapOf(u, c, v, x) // target "c" conflicts
	_ = left.Compose(right)
}

// TestAliasMap_Compose_ConflictTarget ports Java testComposeIncompatibleMaps2.
// Maps {a→i} and {b→i} share target "i" from different sources — panic.
func TestAliasMap_Compose_ConflictTarget(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on target conflict in Compose")
		}
	}()

	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	i := values.NamedCorrelationIdentifier("i")

	m1 := AliasMapOf(a, i)
	m2 := AliasMapOf(b, i) // target "i" conflicts
	_ = m1.Compose(m2)
}

// ---------------------------------------------------------------------------
// Java tests that require methods not yet ported to Go.
// Each is listed with the Java method it needs so the gap is visible.
// ---------------------------------------------------------------------------

// TestAliasMap_Derived_NotYetPortable documents Java testDerived.
// Needs: AliasMap.ToBuilder() / AliasMapBuilder — not yet in Go.

// TestAliasMap_Zip_NotYetPortable documents Java testZip.
// Needs: AliasMap.Zip([]CorrelationIdentifier, []CorrelationIdentifier) — not yet in Go.

// TestAliasMap_MathCompose_NotYetPortable documents Java testCompose.
// Needs: AliasMap.Compose as mathematical function composition
// (a→i, i→u → a→u). Go's Compose is a merge (= Java combine), not
// mathematical composition. Java tests: testCompose,
// testComposeDanglingTargets, testComposeDanglingSources.

// TestAliasMap_FindCompleteMatches_NotYetPortable documents Java
// testMatchNoCorrelations, testMatchNoCorrelations1,
// testMatchSomeCorrelations1, testMatchSomeCorrelations2,
// testMatchFullCorrelations, testMatchIncompatibleCorrelations,
// testMatchEmpty, testMatchExternalCorrelations1,
// testMatchExternalCorrelations2, testMatchExternalCorrelations3.
// Needs: AliasMap.FindCompleteMatches() — not yet in Go.
