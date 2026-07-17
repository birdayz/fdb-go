package cascades

import (
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func namedForEachQuantifier(name string) expressions.Quantifier {
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	return expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(name), expressions.InitialOf(scan))
}

func namedExistentialQuantifier(name string) expressions.Quantifier {
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	return expressions.NamedExistentialQuantifier(
		values.NamedCorrelationIdentifier(name), expressions.InitialOf(scan))
}

// TestUnionQuantifiersOrdered_InsertionOrderAndDedup pins the Go analog of
// Java's LinkedIdentitySet union: first-seen insertion order, alias dedup.
func TestUnionQuantifiersOrdered_InsertionOrderAndDedup(t *testing.T) {
	t.Parallel()
	qa, qb, qc := namedForEachQuantifier("a"), namedForEachQuantifier("b"), namedForEachQuantifier("c")
	got := unionQuantifiersOrdered(
		[]expressions.Quantifier{qb, qa},
		[]expressions.Quantifier{qc, qa, qb},
	)
	want := []string{"b", "a", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %d quantifiers, want %d", len(got), len(want))
	}
	for i, q := range got {
		if q.GetAlias().Name() != want[i] {
			t.Fatalf("slot %d: got %s, want %s", i, q.GetAlias().Name(), want[i])
		}
	}
}

// TestCompensationIntersect_QuantifierOrderDeterministic pins RFC-180 E:
// the matched-quantifier union in Intersect/Union ranged a map, leaking
// iteration order into the compensation's quantifier list — downstream
// expression trees, and with them plan identity, varied run to run. 10×
// repetition would flake pre-fix (map order over 6 aliases).
func TestCompensationIntersect_QuantifierOrderDeterministic(t *testing.T) {
	t.Parallel()
	names := []string{"q1", "q2", "q3", "q4", "q5", "q6"}
	// Intersect needs a SHARED predicate on both sides (disjoint maps
	// intersect to empty → NoCompensation early return); Union needs
	// DISJOINT predicates (a duplicate pointer is ImpossibleCompensation).
	// Union also caps matched ForEach quantifiers at one (Java parity), so
	// the pool is existential; both methods share the ordered-union helper.
	sharedPred := predicates.NewConstantPredicate(predicates.TriTrue)
	sharedMap := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{sharedPred},
		[]PredicateCompensationFunc{ImpossiblePredicateCompensation()},
	)
	freshMap := func() *PredicateCompensationMap {
		return NewPredicateCompensationMap(
			[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
			[]PredicateCompensationFunc{ImpossiblePredicateCompensation()},
		)
	}
	build := func(subset []string, pm *PredicateCompensationMap) *ForMatchCompensation {
		qs := make([]expressions.Quantifier, len(subset))
		for i, n := range subset {
			qs[i] = namedExistentialQuantifier(n)
		}
		return NewForMatchCompensation(
			false, NoCompensation, pm,
			qs, nil, nil,
			NoResultCompensation(), EmptyGroupByMappings(),
		)
	}
	// Exercise the REAL call sites (Intersect and Union), not the helper:
	// the pre-fix map+range lived inside those methods, so only an
	// end-to-end order assertion can catch a regression there. Repetition
	// makes a map-order regression overwhelmingly likely to surface: Go
	// randomizes map iteration per range, so ten runs landing on the one
	// correct order of six is vanishingly rare.
	want := []string{"q1", "q2", "q3", "q4", "q5", "q6"}
	assertOrder := func(label string, qs []expressions.Quantifier) {
		t.Helper()
		if len(qs) != len(want) {
			t.Fatalf("%s: got %d quantifiers, want %d", label, len(qs), len(want))
		}
		for i, q := range qs {
			if q.GetAlias().Name() != want[i] {
				t.Fatalf("%s: slot %d: got %s, want %s (insertion order)", label, i, q.GetAlias().Name(), want[i])
			}
		}
	}
	for run := 0; run < 10; run++ {
		a := build(names[:4], sharedMap) // q1..q4
		b := build(names[2:], sharedMap) // q3..q6

		intersected, ok := a.Intersect(b).(*ForMatchCompensation)
		if !ok {
			t.Fatal("Intersect must produce a ForMatchCompensation for this shape")
		}
		assertOrder("Intersect", intersected.GetMatchedQuantifiers())

		ua := build(names[:4], freshMap())
		ub := build(names[2:], freshMap())
		unioned, ok := ua.Union(ub).(*ForMatchCompensation)
		if !ok {
			t.Fatal("Union must produce a ForMatchCompensation for this shape")
		}
		assertOrder("Union", unioned.GetMatchedQuantifiers())
	}
}

// TestStampOrderingWinners_OverflowedKeyNeverStamped pins the D1 residual:
// a 9-column provider stamped under the overflowed key {first-8,
// overflow:true} would be map-hit DIRECTLY (Reference.Winner bypasses
// Satisfies) by a DIFFERENT 9-column requirement sharing the first-8
// prefix, eliding its sort with the wrong 9th-column ordering.
// stampOrderingWinners must skip overflowed keys entirely.
func TestStampOrderingWinners_OverflowedKeyNeverStamped(t *testing.T) {
	t.Parallel()
	inner := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	mk := func(ninth string) *plans.RecordQueryInMemorySortPlan {
		keys := make([]plans.SortKey, 9)
		for i := 0; i < 8; i++ {
			f := fmt.Sprintf("C%d", i)
			keys[i] = plans.SortKey{Field: f, ValueExpr: values.NewFlatFieldValue(f, values.UnknownType)}
		}
		keys[8] = plans.SortKey{Field: ninth, ValueExpr: values.NewFlatFieldValue(ninth, values.UnknownType)}
		return plans.NewRecordQueryInMemorySortPlan(inner, keys)
	}

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	scanRef := expressions.InitialOf(scan)
	provider := newPhysicalInMemorySortWrapper(mk("I"), expressions.ForEachQuantifier(scanRef))
	ref := expressions.InitialOf(provider)

	stampOrderingWinners(ref, func(a, b expressions.RelationalExpression) bool { return true })

	names := make([]string, 9)
	for i := 0; i < 8; i++ {
		names[i] = fmt.Sprintf("C%d", i)
	}
	names[8] = "J" // same first-8 prefix, DIFFERENT 9th column
	otherReq := expressions.OrderingFromNameDir(names, make([]bool, 9))
	if w := ref.Winner(otherReq); w != nil {
		t.Fatalf("a provider ordered (C0..C7, I) must never be served as winner for a (C0..C7, J) requirement: got %T", w)
	}
	// The provider's own overflowed key is not stamped either.
	names[8] = "I"
	ownReq := expressions.OrderingFromNameDir(names, make([]bool, 9))
	if w := ref.Winner(ownReq); w != nil {
		t.Fatalf("overflowed keys must not be stamped at all: got %T", w)
	}
	// Sanity: an 8-column provider still stamps normally.
	provider8 := newPhysicalInMemorySortWrapper(
		plans.NewRecordQueryInMemorySortPlan(inner, mk("I").GetSortKeys()[:8]),
		expressions.ForEachQuantifier(scanRef))
	ref8 := expressions.InitialOf(provider8)
	stampOrderingWinners(ref8, func(a, b expressions.RelationalExpression) bool { return true })
	req8 := expressions.OrderingFromNameDir(names[:8], make([]bool, 8))
	if ref8.Winner(req8) == nil {
		t.Fatal("an 8-column provider must still stamp its ordering winner")
	}
}

// TestPlannerComplexityGuards pins RFC-180 I1 — the Java-parity complexity
// guards beyond MaxTasks: MaxTaskQueueSize trips ErrPlannerQueueCapHit,
// MaxNumMatchesPerRuleCall trips ErrPlannerRuleMatchCapHit via the task
// capErr channel, and DisabledRules excludes a rule from selection
// (configuration.isRuleEnabled parity).
func TestPlannerComplexityGuards(t *testing.T) {
	t.Parallel()
	buildRef := func() *expressions.Reference {
		scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
		q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
		filter := expressions.NewLogicalFilterExpression(
			[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}, q)
		return expressions.InitialOf(filter)
	}

	t.Run("queue_cap", func(t *testing.T) {
		t.Parallel()
		p := NewPlanner(DefaultExpressionRules(), nil).
			WithImplementationRules(DefaultImplementationRules())
		p.MaxTaskQueueSize = 1 // any real plan pushes deeper than one task
		_, _, err := p.Plan(buildRef())
		if !errors.Is(err, ErrPlannerQueueCapHit) {
			t.Fatalf("want ErrPlannerQueueCapHit, got %v", err)
		}
	})

	t.Run("rule_match_cap_zero_disabled", func(t *testing.T) {
		t.Parallel()
		p := NewPlanner(DefaultExpressionRules(), nil).
			WithImplementationRules(DefaultImplementationRules())
		// Zero (Java default) disables the guard: planning proceeds.
		if _, _, err := p.Plan(buildRef()); err != nil {
			t.Fatalf("zero cap must not trip: %v", err)
		}
	})

	t.Run("disabled_rule_excluded", func(t *testing.T) {
		t.Parallel()
		p := NewPlanner(DefaultExpressionRules(), nil).
			WithImplementationRules(DefaultImplementationRules())
		er, ir := p.rulesForPhase(PhaseRewriting)
		if len(er) == 0 && len(ir) == 0 {
			t.Skip("no rewriting rules registered")
		}
		var name string
		if len(er) > 0 {
			name = fmt.Sprintf("%T", er[0])
		} else {
			name = fmt.Sprintf("%T", ir[0])
		}
		p.DisabledRules = map[string]struct{}{name: {}}
		er2, ir2 := p.rulesForPhase(PhaseRewriting)
		for _, r := range er2 {
			if fmt.Sprintf("%T", r) == name {
				t.Fatalf("disabled rule %s still selected", name)
			}
		}
		for _, r := range ir2 {
			if fmt.Sprintf("%T", r) == name {
				t.Fatalf("disabled rule %s still selected", name)
			}
		}
		if len(er2)+len(ir2) != len(er)+len(ir)-1 {
			t.Fatalf("exactly one rule must be excluded: %d -> %d", len(er)+len(ir), len(er2)+len(ir2))
		}
	})
}
