package embedded

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestMintDistinctUpper pins the collision-skip law: a minted correlation
// name must never equal a VISIBLE alias, even though a quoted SQL alias can
// legally spell `"Q$N"`. The counter is injected so the collision is
// deterministic — no dependence on the process-global mint state.
func TestMintDistinctUpper(t *testing.T) {
	t.Parallel()

	fake := func(names ...string) func() values.CorrelationIdentifier {
		i := 0
		return func() values.CorrelationIdentifier {
			n := names[i]
			i++
			return values.NamedCorrelationIdentifier(n)
		}
	}

	t.Run("no_collision_first_candidate", func(t *testing.T) {
		t.Parallel()
		got := mintDistinctUpper(map[string]struct{}{"MA": {}, "OT": {}}, fake("q$7"))
		if got != "Q$7" {
			t.Fatalf("mint = %q, want Q$7 (uppercased first candidate)", got)
		}
	})

	t.Run("skips_colliding_candidates", func(t *testing.T) {
		t.Parallel()
		// The user quoted an outer leg as "Q$44" and another as "Q$45"; the
		// counter happens to be at 44. Both collide and must be skipped.
		visible := map[string]struct{}{"Q$44": {}, "Q$45": {}}
		got := mintDistinctUpper(visible, fake("q$44", "q$45", "q$46"))
		if got != "Q$46" {
			t.Fatalf("mint = %q, want Q$46 (first non-colliding)", got)
		}
	})

	t.Run("case_insensitive_collision", func(t *testing.T) {
		t.Parallel()
		// visible is upper-cased by the caller; the candidate collides after
		// its own uppercasing.
		got := mintDistinctUpper(map[string]struct{}{"Q$9": {}}, fake("q$9", "q$10"))
		if got != "Q$10" {
			t.Fatalf("mint = %q, want Q$10", got)
		}
	})

	t.Run("identifier_form_preserves_case_and_skips", func(t *testing.T) {
		t.Parallel()
		// The subquery-alias mint (esq/scalar Alias) keeps the identifier's
		// own case while skipping candidates whose UPPER-cased name a
		// user-visible SQL name spells: a quoted "Q$3" unnest alias must
		// never be assigned as an esq.Alias.
		got := mintDistinctIdentifier(map[string]struct{}{"Q$3": {}}, fake("q$3", "q$4"))
		if got.Name() != "q$4" {
			t.Fatalf("mint = %q, want q$4 (case-preserved first non-colliding)", got.Name())
		}
	})

	t.Run("production_counter_postcondition", func(t *testing.T) {
		t.Parallel()
		// With the real counter, whatever comes out must not be in visible —
		// build visible from a handful of freshly-minted names to make the
		// postcondition non-trivial under any interleaving.
		visible := map[string]struct{}{}
		for i := 0; i < 5; i++ {
			visible[fmt.Sprintf("Q$%d", i)] = struct{}{}
		}
		got := mintDistinctUpper(visible, values.UniqueCorrelationIdentifier)
		if _, taken := visible[got]; taken {
			t.Fatalf("mint returned a visible name %q", got)
		}
	})
}
