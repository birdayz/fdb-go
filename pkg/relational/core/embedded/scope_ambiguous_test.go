package embedded

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/semantic"
)

// TestScopeAmbiguousName pins the multi-source scope-ambiguity detector's SET
// SEMANTICS deterministically — reachability-independent (the e2e sentinels
// pin the reachable shapes; this pins the law the arm computes). The critical
// case is the BOUND-name discipline: a minted middle carries
// {Alias: MID, CorrelationName: Q$N} and only Q$N binds at runtime, so a
// predicate ref to MID over an inner leg also named MID must NOT count as a
// collision — a display-alias comparison (the reviewed over-fire) flips that
// case red.
func TestScopeAmbiguousName(t *testing.T) {
	t.Parallel()

	refPred := func(name, col string) predicates.QueryPredicate {
		return predicates.NewComparisonPredicate(
			values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(name)), col, values.UnknownType),
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))},
		)
	}
	src := func(alias, corr string) semantic.ScopeSource {
		return semantic.ScopeSource{Alias: semantic.NewUnquoted(alias), CorrelationName: corr}
	}
	inner := func(names ...string) map[string]struct{} {
		m := map[string]struct{}{}
		for _, n := range names {
			m[n] = struct{}{}
		}
		return m
	}

	t.Run("ordinary_leg_collides", func(t *testing.T) {
		t.Parallel()
		got := scopeAmbiguousName(refPred("ST", "C"), inner("ST", "OI"), []semantic.ScopeSource{src("ST", "ST"), src("OT", "OT")})
		if got != "ST" {
			t.Fatalf("scopeAmbiguousName = %q, want ST (ordinary leg: bound name IS the display name)", got)
		}
	})

	t.Run("minted_middle_display_not_bound", func(t *testing.T) {
		t.Parallel()
		// The over-fire class: outer MID is minted (bound as Q$7); a local
		// inner leg re-declaring MID cannot collide with a name that never
		// binds. A display-alias comparison returns "MID" here — red.
		got := scopeAmbiguousName(refPred("MID", "C"), inner("MID", "ST"), []semantic.ScopeSource{src("MID", "q$7"), src("OT", "OT")})
		if got != "" {
			t.Fatalf("scopeAmbiguousName = %q, want \"\" (minted middle's display alias never binds)", got)
		}
	})

	t.Run("empty_corrname_falls_back_to_alias", func(t *testing.T) {
		t.Parallel()
		got := scopeAmbiguousName(refPred("X", "C"), inner("X"), []semantic.ScopeSource{src("X", "")})
		if got != "X" {
			t.Fatalf("scopeAmbiguousName = %q, want X (Alias fallback when CorrelationName empty)", got)
		}
	})

	t.Run("non_inner_ref_never_counts", func(t *testing.T) {
		t.Parallel()
		got := scopeAmbiguousName(refPred("OT", "K"), inner("MI", "ST2"), []semantic.ScopeSource{src("OT", "OT")})
		if got != "" {
			t.Fatalf("scopeAmbiguousName = %q, want \"\" (a pure outer correlation is not ambiguous)", got)
		}
	})

	t.Run("foldable_ref_eliminated_before_check", func(t *testing.T) {
		t.Parallel()
		// The call-site composition: a colliding ref that constant folding
		// eliminates must not decline — COALESCE(1, ST.C) never reads ST.
		// The decline site checks SimplifyPredicateValues(pred); this pins
		// that composition.
		foldable := predicates.NewComparisonPredicate(
			values.NewScalarFunctionValue("COALESCE", values.UnknownType,
				values.LiteralValue(int64(1)),
				values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("ST")), "C", values.UnknownType),
			),
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))},
		)
		outer := []semantic.ScopeSource{src("ST", "ST")}
		if got := scopeAmbiguousName(predicates.SimplifyPredicateValues(foldable), inner("ST"), outer); got != "" {
			t.Fatalf("simplified check = %q, want \"\" (the fold eliminates the ref)", got)
		}
		// Teeth: the UNSIMPLIFIED predicate does carry the ref — proving the
		// simplify step, not the detector, is what admits the foldable shape.
		if got := scopeAmbiguousName(foldable, inner("ST"), outer); got != "ST" {
			t.Fatalf("unsimplified check = %q, want ST (the raw ref is present)", got)
		}
	})
}
