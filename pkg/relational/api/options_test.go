package api

import (
	"math"
	"testing"
)

func TestNoOptionsSingleton(t *testing.T) {
	t.Parallel()
	// Assign to locals — the linter (SA4000) rejects a literal
	// NoOptions() != NoOptions() comparison.
	a, b := NoOptions(), NoOptions()
	if a != b {
		t.Fatal("NoOptions should be a singleton")
	}
	if len(a.Entries()) != 0 {
		t.Fatal("NoOptions should have no values")
	}
}

func TestDefaultValues(t *testing.T) {
	t.Parallel()
	// Java sets MAX_ROWS = Integer.MAX_VALUE (= 1<<31 - 1). Check exact
	// match: this is part of the wire contract.
	m := DefaultOptionValues()
	if m[OptMaxRows] != math.MaxInt32 {
		t.Errorf("MAX_ROWS default = %v", m[OptMaxRows])
	}
	if m[OptIndexFetchMethod] != IndexFetchUseRemoteFetchWithFallback {
		t.Errorf("INDEX_FETCH_METHOD default = %v", m[OptIndexFetchMethod])
	}
	if m[OptCompressWhenSerializing] != true {
		t.Errorf("COMPRESS default = %v", m[OptCompressWhenSerializing])
	}
	// Mutating the returned map must not affect future lookups.
	m[OptMaxRows] = 999
	if DefaultOptionValues()[OptMaxRows] != math.MaxInt32 {
		t.Errorf("DefaultOptionValues leaked internal map")
	}
}

func TestGetFallsBackToDefault(t *testing.T) {
	t.Parallel()
	opts := NoOptions()
	if got := opts.Get(OptMaxRows); got != math.MaxInt32 {
		t.Errorf("Get(MAX_ROWS) = %v, want default", got)
	}
	if got := opts.Get(OptContinuation); got != nil {
		t.Errorf("Get(CONTINUATION) = %v, want nil (no default)", got)
	}
}

func TestWithOverridesValue(t *testing.T) {
	t.Parallel()
	base := NoOptions()
	a := base.With(OptMaxRows, 42)
	if a.Get(OptMaxRows) != 42 {
		t.Errorf("Get(MAX_ROWS) after With = %v", a.Get(OptMaxRows))
	}
	// Original untouched.
	if base.Get(OptMaxRows) != math.MaxInt32 {
		t.Errorf("With mutated original: base.MAX_ROWS = %v", base.Get(OptMaxRows))
	}
}

func TestWithNilMasksDefault(t *testing.T) {
	t.Parallel()
	// Explicit nil should shadow the default value.
	a := NoOptions().With(OptMaxRows, nil)
	if a.Get(OptMaxRows) != nil {
		t.Errorf("Get after With(nil) = %v, want nil", a.Get(OptMaxRows))
	}
}

func TestWithChild(t *testing.T) {
	t.Parallel()
	parent := NewOptionsBuilder().
		Set(OptMaxRows, 100).
		Set(OptLogQuery, true).
		Build()
	child := NewOptionsBuilder().Set(OptMaxRows, 50).Build()
	combined, err := parent.WithChild(child)
	if err != nil {
		t.Fatalf("WithChild: %v", err)
	}
	// Child overrides parent for MAX_ROWS.
	if v := combined.Get(OptMaxRows); v != 50 {
		t.Errorf("child value missing: %v", v)
	}
	// Parent values surface when child doesn't override.
	if v := combined.Get(OptLogQuery); v != true {
		t.Errorf("parent value missing: %v", v)
	}
}

func TestWithChildRejectsNestedParent(t *testing.T) {
	t.Parallel()
	p1 := NewOptionsBuilder().Set(OptMaxRows, 1).Build()
	p2 := NewOptionsBuilder().Set(OptMaxRows, 2).Build()
	c, err := p1.WithChild(p2)
	if err != nil {
		t.Fatalf("first WithChild: %v", err)
	}
	_, err = p1.WithChild(c)
	if err == nil {
		t.Fatal("expected error for child with parent")
	}
	e := AsError(err)
	if e == nil || e.Code != ErrCodeInternalError {
		t.Errorf("expected InternalError, got %v", err)
	}
}

func TestWithChildSameReturnsChild(t *testing.T) {
	t.Parallel()
	o := NewOptionsBuilder().Set(OptMaxRows, 1).Build()
	got, err := o.WithChild(o)
	if err != nil {
		t.Fatalf("self-WithChild: %v", err)
	}
	if got != o {
		t.Error("combining with self should return self")
	}
}

func TestBuilderFrom(t *testing.T) {
	t.Parallel()
	orig := NewOptionsBuilder().Set(OptMaxRows, 7).Set(OptLogQuery, true).Build()
	copied := NewOptionsBuilder().From(orig).Set(OptDryRun, true).Build()
	if copied.Get(OptMaxRows) != 7 {
		t.Errorf("copied MAX_ROWS: %v", copied.Get(OptMaxRows))
	}
	if copied.Get(OptLogQuery) != true {
		t.Errorf("copied LOG_QUERY: %v", copied.Get(OptLogQuery))
	}
	if copied.Get(OptDryRun) != true {
		t.Errorf("copied DRY_RUN: %v", copied.Get(OptDryRun))
	}
	// Original unaffected.
	if orig.Get(OptDryRun) != false {
		t.Errorf("orig DRY_RUN mutated: %v", orig.Get(OptDryRun))
	}
}

func TestBuilderReuse(t *testing.T) {
	t.Parallel()
	b := NewOptionsBuilder().Set(OptMaxRows, 1)
	a := b.Build()
	b.Set(OptMaxRows, 2)
	// a must not observe the post-Build mutation.
	if a.Get(OptMaxRows) != 1 {
		t.Errorf("Build() returned aliased values: %v", a.Get(OptMaxRows))
	}
}

func TestEntriesIsCopy(t *testing.T) {
	t.Parallel()
	o := NewOptionsBuilder().Set(OptMaxRows, 42).Build()
	entries := o.Entries()
	entries[OptMaxRows] = 99
	if o.Get(OptMaxRows) != 42 {
		t.Errorf("Entries() returned internal map")
	}
}

func TestAllEntriesIncludesParent(t *testing.T) {
	t.Parallel()
	parent := NewOptionsBuilder().Set(OptLogQuery, true).Build()
	child := NewOptionsBuilder().Set(OptMaxRows, 50).Build()
	combined, _ := parent.WithChild(child)
	all := combined.AllEntries()
	if all[OptLogQuery] != true {
		t.Errorf("AllEntries missing parent LOG_QUERY: %v", all)
	}
	if all[OptMaxRows] != 50 {
		t.Errorf("AllEntries missing child MAX_ROWS: %v", all)
	}
}

func TestEntriesNilSentinel(t *testing.T) {
	t.Parallel()
	o := NoOptions().With(OptMaxRows, nil)
	entries := o.Entries()
	v, ok := entries[OptMaxRows]
	if !ok || v != nil {
		t.Errorf("explicit-nil option missing: got (%v, %v)", v, ok)
	}
}

func TestEqual(t *testing.T) {
	t.Parallel()
	a := NewOptionsBuilder().Set(OptMaxRows, 1).Set(OptLogQuery, true).Build()
	b := NewOptionsBuilder().Set(OptMaxRows, 1).Set(OptLogQuery, true).Build()
	c := NewOptionsBuilder().Set(OptMaxRows, 2).Build()
	if !a.Equal(b) {
		t.Error("equal options not equal")
	}
	if a.Equal(c) {
		t.Error("different options considered equal")
	}
	if !a.Equal(a) {
		t.Error("identity should be equal")
	}
	// nil-safety.
	if a.Equal(nil) {
		t.Error("Equal(nil) should be false")
	}
}

func TestEqualWithSliceValues(t *testing.T) {
	t.Parallel()
	a := NewOptionsBuilder().Set(OptDisabledPlannerRules, []string{"r1", "r2"}).Build()
	b := NewOptionsBuilder().Set(OptDisabledPlannerRules, []string{"r1", "r2"}).Build()
	c := NewOptionsBuilder().Set(OptDisabledPlannerRules, []string{"r1"}).Build()
	if !a.Equal(b) {
		t.Error("same slice values should be equal")
	}
	if a.Equal(c) {
		t.Error("different slice values considered equal")
	}
}

func TestEqualStructuralParent(t *testing.T) {
	t.Parallel()
	// Bug previously: Equal compared parents by pointer. Java uses
	// Objects.equals (recursive). Verify that two Options with
	// distinct-pointer-but-structurally-equal parents are Equal.
	parentA := NewOptionsBuilder().Set(OptMaxRows, 10).Build()
	parentB := NewOptionsBuilder().Set(OptMaxRows, 10).Build()
	if parentA == parentB {
		t.Skip("builders returned same pointer — test setup invalid")
	}
	if !parentA.Equal(parentB) {
		t.Fatal("structurally-equal parents should be Equal")
	}
	childSelf := NewOptionsBuilder().Set(OptLogQuery, true).Build()
	childA, _ := parentA.WithChild(childSelf)
	childB, _ := parentB.WithChild(NewOptionsBuilder().Set(OptLogQuery, true).Build())
	if !childA.Equal(childB) {
		t.Error("Options with structurally-equal parents should be Equal")
	}
	// And Options with structurally-different parents should not.
	parentC := NewOptionsBuilder().Set(OptMaxRows, 999).Build()
	childC, _ := parentC.WithChild(NewOptionsBuilder().Set(OptLogQuery, true).Build())
	if childA.Equal(childC) {
		t.Error("Options with different parents should not Equal")
	}
	// One has parent, the other doesn't.
	naked := NewOptionsBuilder().Set(OptLogQuery, true).Build()
	if childA.Equal(naked) {
		t.Error("Options with parent shouldn't Equal parentless Options")
	}
}

func TestEqualWithUncomparableValues(t *testing.T) {
	t.Parallel()
	// A future option value type that's neither comparable with == nor
	// []string must not panic. Simulate with a nested map.
	type customOptName OptionName
	name := OptionName("custom-map-option")
	a := NewOptionsBuilder().Set(name, map[string]int{"a": 1}).Build()
	b := NewOptionsBuilder().Set(name, map[string]int{"a": 1}).Build()
	c := NewOptionsBuilder().Set(name, map[string]int{"a": 2}).Build()
	if !a.Equal(b) {
		t.Error("equal maps should compare equal")
	}
	if a.Equal(c) {
		t.Error("different maps considered equal")
	}
	// Byte slices — another common uncomparable.
	a2 := NewOptionsBuilder().Set(name, []byte{1, 2, 3}).Build()
	b2 := NewOptionsBuilder().Set(name, []byte{1, 2, 3}).Build()
	c2 := NewOptionsBuilder().Set(name, []byte{9, 9, 9}).Build()
	if !a2.Equal(b2) {
		t.Error("equal byte slices should compare equal")
	}
	if a2.Equal(c2) {
		t.Error("different byte slices considered equal")
	}
}

func TestIndexFetchMethodString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		m    IndexFetchMethod
		want string
	}{
		{IndexFetchScanAndFetch, "SCAN_AND_FETCH"},
		{IndexFetchUseRemoteFetch, "USE_REMOTE_FETCH"},
		{IndexFetchUseRemoteFetchWithFallback, "USE_REMOTE_FETCH_WITH_FALLBACK"},
	}
	for _, c := range cases {
		if got := c.m.String(); got != c.want {
			t.Errorf("%v.String() = %q, want %q", c.m, got, c.want)
		}
	}
}
