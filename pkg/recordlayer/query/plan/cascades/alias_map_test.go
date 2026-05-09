package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestAliasMap_Empty(t *testing.T) {
	t.Parallel()
	m := EmptyAliasMap()
	if !m.IsEmpty() {
		t.Fatal("empty map should be empty")
	}
	if !m.IsIdentity() {
		t.Fatal("empty map should be identity")
	}
	if m.Size() != 0 {
		t.Fatalf("size = %d, want 0", m.Size())
	}
}

func TestAliasMap_SingleMapping(t *testing.T) {
	t.Parallel()
	a := values.UniqueCorrelationIdentifier()
	b := values.UniqueCorrelationIdentifier()
	m := AliasMapOfAliases(a, b)

	if m.IsEmpty() {
		t.Fatal("should not be empty")
	}
	if m.IsIdentity() {
		t.Fatal("a→b should not be identity")
	}
	if m.Size() != 1 {
		t.Fatalf("size = %d, want 1", m.Size())
	}
	if !m.ContainsSource(a) {
		t.Fatal("should contain source a")
	}
	if !m.ContainsTarget(b) {
		t.Fatal("should contain target b")
	}
	if got := m.GetTarget(a); got != b {
		t.Fatalf("GetTarget(a) = %v, want %v", got, b)
	}
	if got := m.GetSource(b); got != a {
		t.Fatalf("GetSource(b) = %v, want %v", got, a)
	}
}

func TestAliasMap_IdentityMapping(t *testing.T) {
	t.Parallel()
	a := values.UniqueCorrelationIdentifier()
	m := AliasMapOfAliases(a, a)

	if m.IsEmpty() {
		t.Fatal("should not be empty")
	}
	if !m.IsIdentity() {
		t.Fatal("a→a should be identity")
	}
}

func TestAliasMap_GetTargetFallback(t *testing.T) {
	t.Parallel()
	a := values.UniqueCorrelationIdentifier()
	unknown := values.UniqueCorrelationIdentifier()
	m := AliasMapOfAliases(a, a)

	if got := m.GetTarget(unknown); got != unknown {
		t.Fatalf("unmapped source should return itself, got %v", got)
	}
}

func TestAliasMap_Builder(t *testing.T) {
	t.Parallel()
	a := values.UniqueCorrelationIdentifier()
	b := values.UniqueCorrelationIdentifier()
	c := values.UniqueCorrelationIdentifier()
	d := values.UniqueCorrelationIdentifier()

	builder := NewAliasMapBuilder()
	if !builder.Put(a, b) {
		t.Fatal("first put should succeed")
	}
	if !builder.Put(c, d) {
		t.Fatal("second put should succeed")
	}
	if builder.Put(a, d) {
		t.Fatal("duplicate source should fail")
	}
	if builder.Put(c, b) {
		t.Fatal("duplicate target should fail")
	}

	m := builder.Build()
	if m.Size() != 2 {
		t.Fatalf("size = %d, want 2", m.Size())
	}
	if got := m.GetTarget(a); got != b {
		t.Fatalf("GetTarget(a) = %v, want %v", got, b)
	}
	if got := m.GetTarget(c); got != d {
		t.Fatalf("GetTarget(c) = %v, want %v", got, d)
	}
}

func TestAliasMap_Derived(t *testing.T) {
	t.Parallel()
	a := values.UniqueCorrelationIdentifier()
	b := values.UniqueCorrelationIdentifier()
	c := values.UniqueCorrelationIdentifier()
	d := values.UniqueCorrelationIdentifier()

	base := AliasMapOfAliases(a, b)
	ext := AliasMapOfAliases(c, d)
	derived := base.Derived(ext)

	if derived.Size() != 2 {
		t.Fatalf("derived size = %d, want 2", derived.Size())
	}
	if got := derived.GetTarget(a); got != b {
		t.Fatalf("derived.GetTarget(a) = %v, want %v", got, b)
	}
	if got := derived.GetTarget(c); got != d {
		t.Fatalf("derived.GetTarget(c) = %v, want %v", got, d)
	}
}

func TestAliasMap_Compose(t *testing.T) {
	t.Parallel()
	a := values.UniqueCorrelationIdentifier()
	b := values.UniqueCorrelationIdentifier()
	c := values.UniqueCorrelationIdentifier()

	first := AliasMapOfAliases(a, b)
	second := AliasMapOfAliases(b, c)
	composed := first.Compose(second)

	if composed.Size() != 1 {
		t.Fatalf("composed size = %d, want 1", composed.Size())
	}
	if got := composed.GetTarget(a); got != c {
		t.Fatalf("composed.GetTarget(a) = %v, want %v (should be c via a→b→c)", got, c)
	}
}
