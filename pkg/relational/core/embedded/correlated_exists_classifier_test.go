package embedded

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestSplitConjunctsByOuterRef_Shadowing pins the ON-conjunct classifier that
// decides whether a correlated-EXISTS join's ON conjunct is a liftable outer
// correlation or an inner-only predicate. The alias-shadowing case is the
// regression: when the outer query and the inner FROM reuse the same alias, an
// inner reference to that alias binds INNER (inner shadows outer), so the
// conjunct must be classified inner-only — never treated as an outer correlation
// (which would over-decline a valid inner-only ON).
func TestSplitConjunctsByOuterRef_Shadowing(t *testing.T) {
	// eqOf builds `QOV(a) = QOV(b)` — GetCorrelatedToOfPredicate yields {a, b}.
	eqOf := func(a, b string) predicates.QueryPredicate {
		return predicates.NewComparisonPredicate(
			values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(a)),
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(b))},
		)
	}
	set := func(names ...string) map[string]struct{} {
		m := map[string]struct{}{}
		for _, n := range names {
			m[n] = struct{}{}
		}
		return m
	}

	cases := []struct {
		name      string
		pred      predicates.QueryPredicate
		outer     map[string]struct{}
		inner     map[string]struct{}
		wantOuter bool // refsOuter non-nil (i.e. lifted)
		wantInner bool // innerOnly non-nil (i.e. stays on node)
	}{
		{
			// q4: `E.eid = P.id`, P outer-only, E inner -> liftable correlation.
			name:      "unshadowed_outer_lifts",
			pred:      eqOf("E", "P"),
			outer:     set("P"),
			inner:     set("E"),
			wantOuter: true,
			wantInner: false,
		},
		{
			// Shadowing: `X.k = C.k`, C is in BOTH outer and inner (inner Order AS c
			// shadows outer Customer AS c), X inner -> inner-only, NOT lifted.
			name:      "shadowed_alias_stays_inner",
			pred:      eqOf("X", "C"),
			outer:     set("C", "P"),
			inner:     set("C", "X"),
			wantOuter: false,
			wantInner: true,
		},
		{
			// Pure inner-inner: `E.fid = F.fid`, neither in outer -> inner-only.
			name:      "pure_inner_stays_inner",
			pred:      eqOf("E", "F"),
			outer:     set("P"),
			inner:     set("E", "F"),
			wantOuter: false,
			wantInner: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			refsOuter, innerOnly := splitConjunctsByOuterRef(c.pred, c.outer, c.inner)
			if (refsOuter != nil) != c.wantOuter {
				t.Errorf("refsOuter non-nil = %v, want %v", refsOuter != nil, c.wantOuter)
			}
			if (innerOnly != nil) != c.wantInner {
				t.Errorf("innerOnly non-nil = %v, want %v", innerOnly != nil, c.wantInner)
			}
		})
	}
}

// TestWrapCorrelatedExistsWalkErr_PropagatesUnsupported pins that wrapping a
// walk failure PROPAGATES the Unsupported flag when the wrapped error is itself
// a deliberate Unsupported decline (a nested correlated EXISTS whose JOIN ON hit
// the RIGHT/FULL / nested-subquery decline). Without propagation the outer
// wrapper defaults Unsupported=false → mapPredicateWalkError reports 42703
// instead of the intended 0A000. Red-first: return a plain wrapper and
// unsupported_inner flips to false.
func TestWrapCorrelatedExistsWalkErr_PropagatesUnsupported(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   error
		want bool
	}{
		{"unsupported_inner", &CorrelatedExistsError{Message: "decline", Unsupported: true}, true},
		{"supported_inner", &CorrelatedExistsError{Message: "resolution failure", Unsupported: false}, false},
		{"non_corr_inner", errors.New("some other walk error"), false},
		// The OUTERMOST classification wins: a supported wrapper already decided
		// this is a resolution failure, so the flag stays false even over an
		// unsupported cause (errors.As matches the outer first). This shape does
		// not arise in practice — the wrap sites propagate consistently.
		{"supported_outer_wins", &CorrelatedExistsError{Message: "outer", Unsupported: false, Cause: &CorrelatedExistsError{Message: "inner decline", Unsupported: true}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := wrapCorrelatedExistsWalkErr("wrapped", c.in)
			if got.Unsupported != c.want {
				t.Errorf("wrapCorrelatedExistsWalkErr(%s).Unsupported = %v, want %v", c.name, got.Unsupported, c.want)
			}
		})
	}
}
