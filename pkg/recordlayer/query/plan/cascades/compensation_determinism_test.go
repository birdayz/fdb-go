package cascades

import (
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
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
	// Every compensation shares one ForEach base quantifier: an appliable
	// compensation rebuilds itself on exactly one, and sharing it keeps the
	// composed result at exactly one too. The ordered pool stays existential.
	qBase := namedForEachQuantifier("qBase")
	build := func(subset []string, pm *PredicateCompensationMap) *ForMatchCompensation {
		qs := make([]expressions.Quantifier, 0, len(subset)+1)
		qs = append(qs, qBase)
		for _, n := range subset {
			qs = append(qs, namedExistentialQuantifier(n))
		}
		return NewForMatchCompensation(
			false, NoCompensation, pm,
			qs, nil, aliasesOf(qs...),
			NoResultCompensation(), EmptyGroupByMappings(),
		)
	}
	// Exercise the REAL call sites (Intersect and Union), not the helper:
	// the pre-fix map+range lived inside those methods, so only an
	// end-to-end order assertion can catch a regression there. Repetition
	// makes a map-order regression overwhelmingly likely to surface: Go
	// randomizes map iteration per range, so ten runs landing on the one
	// correct order of six is vanishingly rare.
	want := []string{"qBase", "q1", "q2", "q3", "q4", "q5", "q6"}
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
		// Intersect requires identical compensated-alias responsibility on
		// both sides. Feed the same aliases in different orders and assert
		// that the left side's insertion order wins deterministically.
		a := build(names, sharedMap)
		b := build([]string{"q3", "q4", "q5", "q6", "q1", "q2"}, sharedMap)

		intersected, ok := a.Intersect(b).(*ForMatchCompensation)
		if !ok {
			t.Fatal("Intersect must produce a ForMatchCompensation for this shape")
		}
		assertOrder("Intersect", intersected.GetMatchedQuantifiers())

		// Union is allowed to grow the responsibility set, so retain the
		// overlapping-subset shape that exercises ordered append.
		ua := build(names[:4], freshMap())
		ub := build(names[2:], freshMap())
		unioned, ok := ua.Union(ub).(*ForMatchCompensation)
		if !ok {
			t.Fatal("Union must produce a ForMatchCompensation for this shape")
		}
		assertOrder("Union", unioned.GetMatchedQuantifiers())
	}
}

// TestBestSatisfyingMember_WideOrderingsExact pins RFC-180 D1+D2: ordering
// satisfaction has no fixed column capacity and is exact per part. The
// retired 8-column winner key could not represent a 9th column at all —
// truncating silently would have let a (C0..C7, I) provider serve a
// (C0..C7, J) requirement, and the interim overflow guard "fixed" that by
// making 9+-column requirements categorically unsatisfiable (sort always
// materialised). The rich representation does neither: the provider
// satisfies exactly its own 9-column ordering and nothing else.
func TestBestSatisfyingMember_WideOrderingsExact(t *testing.T) {
	t.Parallel()
	inner := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	mk := func(ninth string) *plans.RecordQueryInMemorySortPlan {
		keys := make([]plans.SortKey, 9)
		for i := 0; i < 8; i++ {
			f := fmt.Sprintf("C%d", i)
			keys[i] = plans.SortKey{Field: f, ValueExpr: values.NewFlatFieldValue(f, values.UnknownType), NullsFirst: true}
		}
		keys[8] = plans.SortKey{Field: ninth, ValueExpr: values.NewFlatFieldValue(ninth, values.UnknownType), NullsFirst: true}
		return plans.NewRecordQueryInMemorySortPlan(inner, keys)
	}

	provider := mk("I")
	ref := expressions.InitialOf(provider)

	req := func(ninth string) *properties.RequestedOrdering {
		parts := make([]properties.RequestedOrderingPart, 9)
		for i := 0; i < 8; i++ {
			f := fmt.Sprintf("C%d", i)
			parts[i] = properties.RequestedOrderingPart{Value: values.NewFlatFieldValue(f, values.UnknownType), SortOrder: properties.RequestedSortOrderAscending}
		}
		parts[8] = properties.RequestedOrderingPart{Value: values.NewFlatFieldValue(ninth, values.UnknownType), SortOrder: properties.RequestedSortOrderAscending}
		return properties.NewRequestedOrdering(parts, properties.DistinctnessPreserveDistinctness, false)
	}

	// Same first-8 prefix, DIFFERENT 9th column: never satisfied.
	if w := bestSatisfyingMember(ref, req("J"), func(a, b expressions.RelationalExpression) bool { return true }); w != nil {
		t.Fatalf("a provider ordered (C0..C7, I) must never satisfy a (C0..C7, J) requirement: got %T", w)
	}
	// The provider's OWN 9-column ordering IS satisfied — no width cap.
	if w := bestSatisfyingMember(ref, req("I"), func(a, b expressions.RelationalExpression) bool { return true }); w != provider {
		t.Fatalf("a 9-column requirement matching the provider exactly must be satisfied; got %T", w)
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

// TestRollUpPlanPartitions_MergesByPropertyEquality pins RFC-180 D3: two
// partitions whose interesting property values are SEMANTICALLY equal but
// pointer-distinct (fresh ordering Values) must merge — the retired %v
// string key rendered interface pointers as addresses, so equal orderings
// never merged (RED pre-fix).
func TestRollUpPlanPartitions_MergesByPropertyEquality(t *testing.T) {
	t.Parallel()
	mkPart := func() *PlanPartition {
		scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
		ord := properties.Ordering{
			IsKnown:    true,
			Keys:       []values.Value{values.NewFlatFieldValue("A", values.UnknownType)},
			Descending: []bool{false},
		}
		return &PlanPartition{
			partitionProps: properties.PropertyMap{properties.PropOrdering: ord},
			exprProps: map[expressions.RelationalExpression]properties.PropertyMap{
				scan: {properties.PropOrdering: ord},
			},
		}
	}
	merged := RollUpPlanPartitions([]*PlanPartition{mkPart(), mkPart()}, properties.PropOrdering)
	if len(merged) != 1 {
		t.Fatalf("semantically-equal ordering partitions must merge into one, got %d", len(merged))
	}
	if len(merged[0].GetExpressions()) != 2 {
		t.Fatalf("merged partition must carry both expressions, got %d", len(merged[0].GetExpressions()))
	}
}

// TestOrderingsEqual_NullsFirstSemantics pins the semantic null-placement contract: absent
// NullsFirst is NATURAL placement (ASC → nulls-first), so absent vs an
// explicit counterflow [false] on ASC are DIFFERENT orderings (must not
// merge), while absent vs explicit [true] on ASC are the SAME (must merge).
func TestOrderingsEqual_NullsFirstSemantics(t *testing.T) {
	t.Parallel()
	key := func() values.Value { return values.NewFlatFieldValue("A", values.UnknownType) }
	natural := properties.Ordering{IsKnown: true, Keys: []values.Value{key()}, Descending: []bool{false}}
	explicitTrue := properties.Ordering{IsKnown: true, Keys: []values.Value{key()}, Descending: []bool{false}, NullsFirst: []bool{true}}
	counterflow := properties.Ordering{IsKnown: true, Keys: []values.Value{key()}, Descending: []bool{false}, NullsFirst: []bool{false}}

	if !orderingsEqual(natural, explicitTrue) {
		t.Fatal("absent NullsFirst on ASC IS nulls-first: must equal the explicit [true] form")
	}
	if orderingsEqual(natural, counterflow) {
		t.Fatal("absent NullsFirst on ASC must NOT equal the counterflow explicit [false] form")
	}

	// DESC mirror: natural placement on DESC is nulls-LAST.
	descNatural := properties.Ordering{IsKnown: true, Keys: []values.Value{key()}, Descending: []bool{true}}
	descExplicitLast := properties.Ordering{IsKnown: true, Keys: []values.Value{key()}, Descending: []bool{true}, NullsFirst: []bool{false}}
	descExplicitFirst := properties.Ordering{IsKnown: true, Keys: []values.Value{key()}, Descending: []bool{true}, NullsFirst: []bool{true}}
	if !orderingsEqual(descNatural, descExplicitLast) {
		t.Fatal("absent NullsFirst on DESC IS nulls-last: must equal the explicit [false] form")
	}
	if orderingsEqual(descNatural, descExplicitFirst) {
		t.Fatal("absent NullsFirst on DESC must NOT equal the counterflow explicit [true] form")
	}
}

// tripStubMatcher matches any expression twice — two bindings per call.
type tripStubMatcher struct{}

func (*tripStubMatcher) RootType() string { return "any" }
func (*tripStubMatcher) BindMatches(outer *matching.PlannerBindings, in any) []*matching.PlannerBindings {
	return []*matching.PlannerBindings{outer, outer}
}

type tripStubRule struct{}

func (*tripStubRule) Matcher() matching.BindingMatcher { return &tripStubMatcher{} }
func (*tripStubRule) OnMatch(call *ExpressionRuleCall) {}

// TestPlannerCapTrips gives both new complexity-guard error paths a POSITIVE
// trip pin — a reverted guard stays red, not silently green.
func TestPlannerCapTrips(t *testing.T) {
	t.Parallel()

	t.Run("rule_match_cap_trips", func(t *testing.T) {
		t.Parallel()
		scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
		ref := expressions.InitialOf(scan)
		p := NewPlanner(nil, nil)
		p.MaxNumMatchesPerRuleCall = 1
		task := &TransformExprTask{Phase: PhaseRewriting, Ref: ref, Expr: scan, Rule: &tripStubRule{}}
		task.Run(plannerTestContext(), p)
		if !errors.Is(p.capErr, ErrPlannerRuleMatchCapHit) {
			t.Fatalf("two bindings over cap 1 must trip ErrPlannerRuleMatchCapHit, got %v", p.capErr)
		}
	})

	t.Run("match_cap_cumulative_across_swapped_bind", func(t *testing.T) {
		t.Parallel()
		// Java counts ONE numMatches stream per rule call, with quantifier
		// permutations inside it. Neither leg alone exceeds the cap here
		// (2 primary, 2 swapped, cap 3); only the cumulative count trips.
		qA := expressions.ForEachQuantifier(expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)))
		qB := expressions.ForEachQuantifier(expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"U"}, nil)))
		sel := expressions.NewSelectExpression(qA.GetFlowedObjectValue(), []expressions.Quantifier{qA, qB}, nil)
		ref := expressions.InitialOf(sel)
		p := NewPlanner(nil, nil)
		p.MaxNumMatchesPerRuleCall = 3
		task := &TransformExprTask{Phase: PhasePlanning, Ref: ref, Expr: sel, Rule: &tripStubRule{}}
		task.Run(plannerTestContext(), p)
		if !errors.Is(p.capErr, ErrPlannerRuleMatchCapHit) {
			t.Fatalf("2 primary + 2 swapped bindings over cap 3 must trip cumulatively, got %v", p.capErr)
		}
	})

	t.Run("round_cap_trips", func(t *testing.T) {
		t.Parallel()
		scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
		ref := expressions.InitialOf(scan)
		// Epoch-model divergence fixture (RFC-181 WS-P stage (b)): member
		// growth no longer re-rounds a group, so the old shape — Insert a
		// new member after 10 rounds — can no longer arm NeedsExploration.
		// What re-rounds a group now is a constraint TICK. Model a rule
		// cycle that never converges by re-arming the epoch inside every
		// round: StartExploration bumps the round counter and records the
		// goal watermark, ReArm pushes a fresh tick past it (the mid-round
		// constraint push), and CommitExploration lands behind the current
		// epoch — so the group still NeedsExploration at the tripwire
		// (100 since WS-P stage (d): the load-level cap is obsolete
		// under epoch convergence; the bound remains only as a loud
		// divergence tripwire for a tick leak / always-growing combine).
		for i := 0; i < 100; i++ {
			ref.StartExploration()
			ref.ConstraintsMap().ReArm()
			ref.CommitExploration()
		}
		if !ref.NeedsExploration() {
			t.Fatal("fixture must still need exploration")
		}
		p := NewPlanner(nil, nil)
		task := &ExploreGroupTask{Phase: PhaseRewriting, Ref: ref}
		task.Run(plannerTestContext(), p)
		if !errors.Is(p.capErr, ErrPlannerRoundCapHit) {
			t.Fatalf("100 epoch-re-armed rounds must trip ErrPlannerRoundCapHit, got %v", p.capErr)
		}
	})
}
