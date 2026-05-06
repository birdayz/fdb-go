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
