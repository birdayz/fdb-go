package embedded

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

const unnestExistsOuterOnlyDDL = `
CREATE TABLE MA (ID BIGINT, ARR INTEGER ARRAY, C INTEGER, PRIMARY KEY (ID))
CREATE TABLE JU (ID BIGINT, K INTEGER, PRIMARY KEY (ID))`

// TestUnnestExistsOuterOnlyConjunctPlans pins the under-EXISTS placement of a
// conjunct that reads only the lateral unnest's outer table. Positive and
// negative polarity share the same buried predicate program: MA.ID must remain
// below the existential cardinality boundary, where it is evaluated once per
// unnested outer row. Hoisting it outside NOT EXISTS changes the truth table,
// while leaving the exact MA-rooted read unrebased makes translation decline.
func TestUnnestExistsOuterOnlyConjunctPlans(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{
			name: "exists",
			sql:  `SELECT "X" FROM MA, MA."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM JU WHERE MA."ID" = 1)`,
		},
		{
			name: "not_exists",
			sql:  `SELECT "X" FROM MA, MA."ARR" AS "X" WHERE NOT EXISTS (SELECT 1 FROM JU WHERE MA."ID" = 1)`,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanQueryForTest(tc.sql, unnestExistsOuterOnlyDDL, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			for _, want := range []string{"FlatMap(", "FirstOrDefault("} {
				if !strings.Contains(plan, want) {
					t.Fatalf("outer-only EXISTS lost its nested cardinality boundary %q: %s", want, plan)
				}
			}
			if strings.Contains(plan, "<nil>") {
				t.Fatalf("outer-only EXISTS retained a nil child: %s", plan)
			}
		})
	}
}

// TestCorrelatedExistsWithInnerLateralUnnestPlans pins the builder/type
// boundary for an EXISTS whose own FROM list contains a lateral unnest. That
// inner LogicalUnnest intentionally carries source syntax rather than a
// CorrelatedCollection; its enclosing LogicalJoin must still acquire an exact
// element row before the existential QOV is minted. AS+AT is the width-sensitive
// twin: both element and ordinal slots must survive exact typing.
func TestCorrelatedExistsWithInnerLateralUnnestPlans(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{
			name: "scalar element",
			sql:  `SELECT ID FROM JU WHERE EXISTS (SELECT 1 FROM MA, MA.ARR AS X WHERE X = JU.K)`,
		},
		{
			name: "element with ordinality",
			sql:  `SELECT ID FROM JU WHERE EXISTS (SELECT 1 FROM MA, MA.ARR AS X AT POS WHERE X = JU.K AND POS >= 1)`,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanQueryForTest(tc.sql, unnestExistsOuterOnlyDDL, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			for _, want := range []string{"FlatMap(", "FirstOrDefault(", "Explode("} {
				if !strings.Contains(plan, want) {
					t.Fatalf("correlated inner unnest lost %q: %s", want, plan)
				}
			}
			if strings.Contains(plan, "<nil>") || strings.Contains(plan, "Unknown") {
				t.Fatalf("correlated inner unnest retained an inexact carrier: %s", plan)
			}
		})
	}
}

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
		typ := &values.RecordType{Fields: []values.Field{{Name: "K", Ordinal: 0, FieldType: values.NotNullLong}}}
		left, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(a), typ)
		if err != nil {
			t.Fatalf("left QOV: %v", err)
		}
		right, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(b), typ)
		if err != nil {
			t.Fatalf("right QOV: %v", err)
		}
		return predicates.NewComparisonPredicate(
			left,
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: right},
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
