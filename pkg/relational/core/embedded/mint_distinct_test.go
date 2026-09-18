package embedded

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The production query-local allocator must skip names visible in any of its
// shared lexical frames. Its deterministic sequence makes collisions explicit,
// without a fake counter or dependence on process-global planning history.
func TestBindingAllocatorSkipsVisibleNames(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		visible  []string
		wantName string
	}{
		{"no_collision_first_candidate", []string{"MA", "OT"}, "q$bound1"},
		{"skips_colliding_candidates", []string{"Q$BOUND1", "Q$BOUND2"}, "q$bound3"},
		{"case_insensitive_collision", []string{"q$BoUnD1"}, "q$bound2"},
		{"identifier_form_preserves_case_and_kind", []string{"Q$BOUND1"}, "q$bound2"},
		{"production_counter_postcondition", []string{"Q$BOUND1", "Q$BOUND2", "Q$BOUND3", "Q$BOUND4", "Q$BOUND5"}, "q$bound6"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var allocator bindingAllocator
			for _, name := range test.visible {
				allocator.reserve(name)
			}
			got := allocator.mint()
			if got.Name() != test.wantName {
				t.Fatalf("mint = %q, want %q", got.Name(), test.wantName)
			}
			if got == values.NamedCorrelationIdentifier(got.Name()) {
				t.Fatal("private identity lost its kind")
			}
			for _, name := range test.visible {
				if strings.EqualFold(got.Name(), name) {
					t.Fatalf("mint returned a visible name %q", got.Name())
				}
			}
			if _, retained := allocator.reserved[strings.ToUpper(got.Name())]; !retained {
				t.Fatal("mint did not reserve its own name for sibling/child scopes")
			}
			if next := allocator.mint(); strings.EqualFold(next.Name(), got.Name()) {
				t.Fatal("same query owner reused a private identity")
			}
		})
	}
}
