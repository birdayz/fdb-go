package fuzzfloor

import (
	"flag"
	"testing"
)

// TestSuppressedFor drives every arm from explicit state rather than from the
// process's real -test.fuzz flag, because the corpus reading exercises only the
// arm this run happens to take: under plain `go test` the flag is empty and the
// three matching arms below never execute at all.
//
// The table is written around the bug this function exists for. Fuzzing ONE
// target must not suppress the floor of any OTHER target, which is what a
// non-empty check does and what the "other target" rows pin.
func TestSuppressedFor(t *testing.T) {
	t.Parallel()

	f := flag.Lookup("test.fuzz")
	if f == nil {
		t.Fatal("the testing package no longer registers -test.fuzz — SuppressedFor can " +
			"never suppress anything and every floor is now enforced under -fuzz too, " +
			"which reports zero comparisons on a healthy run")
	}
	original := f.Value.String()
	t.Cleanup(func() { _ = f.Value.Set(original) })

	const target = "FuzzNormalForm_PreservesSemantics"
	for _, tc := range []struct {
		name    string
		pattern string
		want    bool
	}{
		{"no pattern enforces the floor", "", false},
		{"this target, anchored", "FuzzNormalForm_PreservesSemantics$", true},
		{"this target, bare", "FuzzNormalForm_PreservesSemantics", true},
		{"this target, prefix", "FuzzNormalForm", true},
		{"unanchored substring still matches", "NormalForm_Preserves", true},
		// The bug. A neighbour being fuzzed must leave this target's floor ON.
		{"another target in the same package", "FuzzSimplifyPredicate_PreservesSemantics$", false},
		{"another target, prefix", "FuzzSimplifyValue", false},
		{"match-nothing pattern", "FuzzNoSuchTarget$", false},
		// Only the first '/' element names the target.
		{"subtest path on this target", "FuzzNormalForm_PreservesSemantics/seed#0", true},
		{"subtest path on another target", "FuzzSimplifyValue/seed#0", false},
		// Fails closed: an uncompilable pattern enforces rather than silences.
		{"uncompilable pattern enforces", "Fuzz[", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := f.Value.Set(tc.pattern); err != nil {
				t.Fatalf("setting -test.fuzz=%q: %v", tc.pattern, err)
			}
			if got := SuppressedFor(target); got != tc.want {
				t.Errorf("SuppressedFor(%q) with -test.fuzz=%q = %v, want %v",
					target, tc.pattern, got, tc.want)
			}
		})
	}
}
