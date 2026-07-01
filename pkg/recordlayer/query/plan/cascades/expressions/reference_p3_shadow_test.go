package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestReference_P3InternShadow_RFC173 pins the P3 dark-shadow observation: two
// structurally-identical selects differing only in their quantifier alias are NOT
// deduped by the current alias-identity tiers (both get appended — behavior
// unchanged), but the InternShadowObserver reports that the GLOBAL alias-bijection
// tier (which P3 makes authoritative in Slice 3) WOULD dedup the second. This is
// the extra dedup P3 unlocks for non-opted-in expressions — today gated to
// InternsAliasAware merge-selects only.
func TestReference_P3InternShadow_RFC173(t *testing.T) {
	// Not t.Parallel(): mutates the package-level InternShadowObserver.
	var obs []bool
	InternShadowObserver = func(would bool) { obs = append(obs, would) }
	t.Cleanup(func() { InternShadowObserver = nil })

	newSelect := func(table string) RelationalExpression {
		scan := NewFullUnorderedScanExpression([]string{table}, values.UnknownType)
		q := ForEachQuantifier(InitialOf(scan))
		return NewSelectExpression(values.NewQuantifiedObjectValue(q.GetAlias()), []Quantifier{q}, nil)
	}

	ref := &Reference{plannerStage: StageCanonical}
	sel1 := newSelect("T")
	sel2 := newSelect("T") // same structure, DIFFERENT fresh quantifier alias

	if !ref.Insert(sel1) {
		t.Fatal("sel1 must be inserted into an empty reference")
	}
	if !ref.Insert(sel2) {
		t.Fatal("sel2 is appended today — the alias-identity tiers do not dedup a renamed-alias select")
	}

	// Behavior UNCHANGED by the dark shadow: both members present.
	if got := len(ref.AllMembers()); got != 2 {
		t.Fatalf("dark shadow must not change dedup: got %d members, want 2", got)
	}

	// The observer fired once per insert (both non-alias-aware, both missed the
	// identity tiers): [false (empty ref), true (bijection catches the renamed dup)].
	if len(obs) != 2 {
		t.Fatalf("observer should fire on both inserts, got %d: %v", len(obs), obs)
	}
	if obs[0] {
		t.Fatalf("first insert (empty ref) has nothing to dedup against, want would=false")
	}
	if !obs[1] {
		t.Fatalf("second insert: the global alias-bijection tier should dedup the renamed select (want would=true), got %v", obs)
	}
}
