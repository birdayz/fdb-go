package values

// The reserved-current forgery guard has to recognise the impostor in the
// spelling the SQL layer actually produces.
//
// `_current` is reserved: a value rooted at the real current handle gets a
// phase privilege on the ordering bridge that an ordinary named root must not
// have. The guard that denies an ordinary root wearing that name compared the
// text EXACTLY, in lowercase — but a user alias reaches the planner through the
// semantic scope, which upper-folds every correlation. So `AS _current` arrives
// as `_CURRENT`, missed the guard, and was handed the privilege it exists to
// deny. The lowercase spelling a Go test writes by hand is the one case that
// could never occur through SQL.
//
// The KIND check is what proves the real handle is real (correlationKind is
// private and NamedCorrelationIdentifier cannot forge it); this is what
// recognises the copy. Both are needed: neither alone denies a folded impostor.

import (
	"strings"
	"testing"
)

func TestCurrentForgeryGuardFoldsCase(t *testing.T) {
	t.Parallel()

	reserved := CurrentCorrelation().Name()
	if reserved != strings.ToLower(reserved) {
		t.Fatalf("the reserved spelling %q is not lowercase; this test's premise"+
			" (SQL folds UP, the constant is DOWN) no longer holds", reserved)
	}

	for _, spelling := range []string{
		reserved,                  // as written in Go
		strings.ToUpper(reserved), // as the SQL scope folds it — the live case
		"_Current",                // mixed, for completeness
	} {
		spelling := spelling
		t.Run(spelling, func(t *testing.T) {
			t.Parallel()
			impostor := mustQOV(t, NamedCorrelationIdentifier(spelling))
			if !isNamedCurrentForgery(impostor) {
				t.Errorf("a NAMED correlation spelled %q was not recognised as a"+
					" forgery of the reserved %q, so it can take the tagged-current"+
					" phase bridge", spelling, reserved)
			}
		})
	}

	t.Run("the_real_handle_is_not_a_forgery", func(t *testing.T) {
		t.Parallel()
		// The genuine handle carries the private current KIND. Folding the name
		// comparison must not make the real one look like an impostor.
		//
		// Built directly rather than through NewQuantifiedObjectValue, which
		// refuses the reserved correlation ("current is owner-scoped") — the
		// owner mints it internally, exactly as ordinal_layout.go does.
		genuine := &quantifiedObjectValue{correlation: CurrentCorrelation()}
		if !genuine.correlation.isCurrent() {
			t.Fatal("fixture did not produce the reserved current correlation")
		}
		if isNamedCurrentForgery(genuine) {
			t.Error("the reserved current handle was classified as a forgery of itself")
		}
	})

	t.Run("an_ordinary_alias_is_not_a_forgery", func(t *testing.T) {
		t.Parallel()
		// Case folding must not widen the guard onto unrelated names.
		// "" is excluded deliberately: the zero correlation is refused at
		// construction, so it cannot reach this guard.
		for _, name := range []string{"C", "CURRENT", "_CURRENTX", "X_CURRENT", "CURRENT_"} {
			if isNamedCurrentForgery(mustQOV(t, NamedCorrelationIdentifier(name))) {
				t.Errorf("ordinary alias %q was classified as a reserved-name forgery", name)
			}
		}
	})
}
