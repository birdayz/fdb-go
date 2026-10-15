package wire

import (
	"slices"
	"testing"
)

func vtableOffsetPanics(tmpl *MessageTemplate, vt VTable) (panicked bool) {
	defer func() { panicked = recover() != nil }()
	tmpl.VTableOffset(vt)
	return false
}

func TestVTableOffsetMatchesContentsAtConstruction(t *testing.T) {
	t.Parallel()
	a := VTable{4, 8, 4}
	b := VTable{6, 12, 4, 8}
	dupOfA := VTable{4, 8, 4} // same contents, different backing array
	closure := []VTable{a, b, dupOfA}
	tmpl := NewMessageTemplate(1, a, 4, closure)

	set := NewVTableSetForTest()
	for _, vt := range closure {
		set.Add(vt)
	}
	set.Pack()
	wantA, wantB := set.GetOffset(a), set.GetOffset(b)

	if got := tmpl.VTableOffset(a); got != wantA {
		t.Fatalf("offset of a = %d, want %d", got, wantA)
	}
	if got := tmpl.VTableOffset(b); got != wantB {
		t.Fatalf("offset of b = %d, want %d", got, wantB)
	}
	if got := tmpl.VTableOffset(dupOfA); got != wantA {
		t.Fatalf("duplicate-content vtable resolved to %d, want a's %d", got, wantA)
	}
	if got := tmpl.VTableOffset(slices.Clone(b)); got != wantB {
		t.Fatalf("equal contents in another backing array resolved to %d, want %d", got, wantB)
	}

	// Mutating a closure vtable after construction changes neither the
	// packed bytes nor what the template matches.
	packed := slices.Clone(tmpl.PackedVTables())
	b[1] = 99
	if !slices.Equal(tmpl.PackedVTables(), packed) {
		t.Fatal("mutating a source vtable changed the packed vtables")
	}
	if got := tmpl.VTableOffset(VTable{6, 12, 4, 8}); got != wantB {
		t.Fatalf("construction-time contents of b resolved to %d, want %d", got, wantB)
	}
	if !vtableOffsetPanics(tmpl, b) {
		t.Fatal("a mutated vtable absent from the closure did not panic")
	}
}

func TestVTableOffsetAbsentOrEmptyPanics(t *testing.T) {
	t.Parallel()
	a := VTable{4, 8, 4}
	tmpl := NewMessageTemplate(1, a, 4, []VTable{a})
	for _, vt := range []VTable{{4, 8}, {4, 8, 4, 0}, {}, nil} {
		if !vtableOffsetPanics(tmpl, vt) {
			t.Fatalf("vtable %v absent from the closure did not panic", vt)
		}
	}
}
