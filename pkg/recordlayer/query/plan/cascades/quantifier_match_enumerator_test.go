package cascades

import (
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func enumeratorTestQuantifier(
	alias values.CorrelationIdentifier,
	dependencies ...values.CorrelationIdentifier,
) expressions.Quantifier {
	scan := expressions.NewFullUnorderedScanExpression(
		[]string{"T"},
		values.UnknownType,
	)
	inner := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	queryPredicates := make(
		[]predicates.QueryPredicate,
		0,
		len(dependencies),
	)
	for _, dependency := range dependencies {
		queryPredicates = append(
			queryPredicates,
			predicates.NewExistentialAlias(dependency),
		)
	}
	child := expressions.NewLogicalFilterExpression(queryPredicates, inner)
	return expressions.NamedForEachQuantifier(alias, expressions.InitialOf(child))
}

func enumeratorTestIndependentQuantifiers(
	prefix string,
	count int,
) []expressions.Quantifier {
	quantifiers := make([]expressions.Quantifier, count)
	for i := range quantifiers {
		quantifiers[i] = enumeratorTestQuantifier(
			values.NamedCorrelationIdentifier(fmt.Sprintf("%s%d", prefix, i)),
		)
	}
	return quantifiers
}

// enumeratorTestReverseChain returns quantifiers in ordinal order 0..count-1,
// but gives them the dependency order count-1..0.
func enumeratorTestReverseChain(
	prefix string,
	count int,
) []expressions.Quantifier {
	aliases := make([]values.CorrelationIdentifier, count)
	for i := range aliases {
		aliases[i] = values.NamedCorrelationIdentifier(
			fmt.Sprintf("%s%d", prefix, i),
		)
	}
	quantifiers := make([]expressions.Quantifier, count)
	for i := range quantifiers {
		if i+1 < count {
			quantifiers[i] = enumeratorTestQuantifier(aliases[i], aliases[i+1])
		} else {
			quantifiers[i] = enumeratorTestQuantifier(aliases[i])
		}
	}
	return quantifiers
}

func enumeratorTestMappingKey(mapping []quantifierMapping) string {
	parts := make([]string, len(mapping))
	for i, pair := range mapping {
		parts[i] = fmt.Sprintf(
			"q%d:c%d",
			pair.queryIndex,
			pair.candidateIndex,
		)
	}
	return strings.Join(parts, ",")
}

func TestQuantifierMatchEnumerator_DependencyConvexity(t *testing.T) {
	t.Parallel()

	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	c := values.NamedCorrelationIdentifier("c")
	order := buildQuantifierDependencyOrder([]expressions.Quantifier{
		enumeratorTestQuantifier(a),
		enumeratorTestQuantifier(b, a),
		enumeratorTestQuantifier(c, b),
	})
	if !order.ok {
		t.Fatal("valid dependency chain was rejected")
	}

	tests := []struct {
		name     string
		selected []int
		want     bool
	}{
		{
			name:     "omit user-side boundary",
			selected: []int{0, 1},
			want:     true,
		},
		{
			name:     "omit dependency-side boundary",
			selected: []int{1, 2},
			want:     true,
		},
		{
			name:     "omit middle and re-enter chain",
			selected: []int{0, 2},
			want:     false,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			if got := dependencyConvex(test.selected, order.transitive); got != test.want {
				t.Fatalf(
					"dependencyConvex(%v) = %v, want %v",
					test.selected,
					got,
					test.want,
				)
			}
		})
	}
}

func TestQuantifierMatchEnumerator_DependencyCompatibilityIsAsymmetric(t *testing.T) {
	t.Parallel()

	independent := buildQuantifierDependencyOrder(
		enumeratorTestIndependentQuantifiers("independent", 2),
	)
	chain := buildQuantifierDependencyOrder(
		enumeratorTestReverseChain("chain", 2),
	)
	if !independent.ok || !chain.ok {
		t.Fatal("valid dependency graph was rejected")
	}
	mapping := []quantifierMapping{
		{queryIndex: 0, candidateIndex: 0},
		{queryIndex: 1, candidateIndex: 1},
	}

	// Candidate-only dependencies are permitted here and are left to the
	// expression-specific subsumption gate.
	if !mappingDependenciesCompatible(
		mapping,
		independent.transitive,
		chain.transitive,
	) {
		t.Fatal("candidate-only dependency should not invalidate a mapping")
	}

	// The reverse is unsafe: a selected query dependency must exist under the
	// mapping on the candidate side.
	if mappingDependenciesCompatible(
		mapping,
		chain.transitive,
		independent.transitive,
	) {
		t.Fatal("missing candidate counterpart for a query dependency was accepted")
	}

	if !mappingDependenciesCompatible(
		mapping,
		chain.transitive,
		chain.transitive,
	) {
		t.Fatal("isomorphic dependency chains should be compatible")
	}
}

func TestQuantifierMatchEnumerator_MalformedGraphsFailClosed(t *testing.T) {
	t.Parallel()

	duplicateAlias := values.NamedCorrelationIdentifier("duplicate")
	duplicate := []expressions.Quantifier{
		enumeratorTestQuantifier(duplicateAlias),
		enumeratorTestQuantifier(duplicateAlias),
	}
	cycleA := values.NamedCorrelationIdentifier("cycle_a")
	cycleB := values.NamedCorrelationIdentifier("cycle_b")
	cycle := []expressions.Quantifier{
		enumeratorTestQuantifier(cycleA, cycleB),
		enumeratorTestQuantifier(cycleB, cycleA),
	}
	valid := enumeratorTestIndependentQuantifiers("valid", 2)

	if buildQuantifierDependencyOrder(duplicate).ok {
		t.Fatal("duplicate aliases must invalidate the dependency graph")
	}
	if buildQuantifierDependencyOrder(cycle).ok {
		t.Fatal("dependency cycles must invalidate the dependency graph")
	}

	tests := []struct {
		name      string
		query     []expressions.Quantifier
		candidate []expressions.Quantifier
	}{
		{name: "duplicate query", query: duplicate, candidate: valid},
		{name: "duplicate candidate", query: valid, candidate: duplicate},
		{name: "cyclic query", query: cycle, candidate: valid},
		{name: "cyclic candidate", query: valid, candidate: cycle},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			budget := &matchIntermediateSearchBudget{}
			visits := 0
			enumerateQuantifierMappings(
				test.query,
				test.candidate,
				false,
				budget,
				func([]quantifierMapping) bool {
					visits++
					return true
				},
			)
			if visits != 0 {
				t.Fatalf("malformed graph reached visitor %d times", visits)
			}
			if budget.visitedStates != 0 || budget.exhausted {
				t.Fatalf(
					"malformed graph consumed budget: visited=%d exhausted=%v",
					budget.visitedStates,
					budget.exhausted,
				)
			}
		})
	}
}

func TestQuantifierMatchEnumerator_KZeroNeverVisits(t *testing.T) {
	t.Parallel()

	one := enumeratorTestIndependentQuantifiers("one", 1)
	tests := []struct {
		name      string
		query     []expressions.Quantifier
		candidate []expressions.Quantifier
	}{
		{name: "both empty"},
		{name: "empty query", candidate: one},
		{name: "empty candidate", query: one},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			budget := &matchIntermediateSearchBudget{}
			visits := 0
			enumerateQuantifierMappings(
				test.query,
				test.candidate,
				false,
				budget,
				func(mapping []quantifierMapping) bool {
					if len(mapping) == 0 {
						t.Error("k=0 mapping reached the visitor")
					}
					visits++
					return true
				},
			)
			if visits != 0 {
				t.Fatalf("empty side reached visitor %d times", visits)
			}
			if budget.visitedStates != 0 {
				t.Fatalf("k=0 search consumed %d states", budget.visitedStates)
			}
		})
	}
}

func TestQuantifierMatchEnumerator_DeterministicFullFirstOrder(t *testing.T) {
	t.Parallel()

	query := enumeratorTestIndependentQuantifiers("q", 2)
	candidate := enumeratorTestIndependentQuantifiers("c", 2)
	want := []string{
		"q0:c0,q1:c1",
		"q1:c0,q0:c1",
		"q0:c0",
		"q1:c0",
		"q0:c1",
		"q1:c1",
	}

	for run := 0; run < 20; run++ {
		budget := &matchIntermediateSearchBudget{}
		var got []string
		enumerateQuantifierMappings(
			query,
			candidate,
			false,
			budget,
			func(mapping []quantifierMapping) bool {
				got = append(got, enumeratorTestMappingKey(mapping))
				return true
			},
		)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("run %d mapping order = %v, want %v", run, got, want)
		}
		if budget.exhausted {
			t.Fatalf("run %d unexpectedly exhausted its budget", run)
		}
	}
}

func TestQuantifierMatchEnumerator_EightIndependentLegsTruncateDeterministically(t *testing.T) {
	t.Parallel()

	const (
		quantifierCount = 8
		factorialEight  = 40_320
		repetitions     = 10
	)
	query := enumeratorTestIndependentQuantifiers("q_eight_budget", quantifierCount)
	candidate := enumeratorTestIndependentQuantifiers("c_eight_budget", quantifierCount)

	var firstRun []string
	for run := 0; run < repetitions; run++ {
		budget := &matchIntermediateSearchBudget{}
		mappingVisits := 0
		enumerateQuantifierMappings(
			query,
			candidate,
			true,
			budget,
			func(mapping []quantifierMapping) bool {
				key := enumeratorTestMappingKey(mapping)
				if run == 0 {
					firstRun = append(firstRun, key)
				} else {
					if mappingVisits >= len(firstRun) {
						t.Fatalf(
							"run %d emitted an unexpected mapping %d: %s",
							run,
							mappingVisits,
							key,
						)
					}
					if key != firstRun[mappingVisits] {
						t.Fatalf(
							"run %d mapping %d = %q, first run = %q",
							run,
							mappingVisits,
							key,
							firstRun[mappingVisits],
						)
					}
				}
				mappingVisits++
				return true
			},
		)

		if !budget.exhausted {
			t.Fatalf("run %d: eight-way prefix search should exhaust its strict budget", run)
		}
		if budget.visitedStates != matchIntermediateMaxVisitedStates {
			t.Fatalf(
				"run %d: visited states = %d, want strict cap %d",
				run,
				budget.visitedStates,
				matchIntermediateMaxVisitedStates,
			)
		}
		if mappingVisits == 0 || mappingVisits >= factorialEight {
			t.Fatalf(
				"run %d: completed mappings = %d, want deterministic truncation in [1, %d)",
				run,
				mappingVisits,
				factorialEight,
			)
		}
		if run > 0 && mappingVisits != len(firstRun) {
			t.Fatalf(
				"run %d: completed mappings = %d, first run = %d",
				run,
				mappingVisits,
				len(firstRun),
			)
		}
	}
}

func TestQuantifierMatchEnumerator_StrictSharedVisitBudget(t *testing.T) {
	t.Parallel()

	const quantifierCount = 9
	query := enumeratorTestIndependentQuantifiers("q_budget", quantifierCount)
	// Reverse the candidate's only topological order. The positional mapping
	// is therefore the final query permutation, well beyond the visit budget.
	candidate := enumeratorTestReverseChain("c_budget", quantifierCount)
	candidateOrder := buildQuantifierDependencyOrder(candidate)
	if !candidateOrder.ok {
		t.Fatal("valid reverse dependency chain was rejected")
	}
	for i, got := range candidateOrder.stableTopo {
		want := quantifierCount - 1 - i
		if got != want {
			t.Fatalf(
				"candidate topological order[%d] = %d, want %d",
				i,
				got,
				want,
			)
		}
	}

	budget := &matchIntermediateSearchBudget{}
	mappingVisits := 0
	visitedAfterExhaustion := 0
	sawPositionalMapping := false
	enumerateQuantifierMappings(
		query,
		candidate,
		true,
		budget,
		func(mapping []quantifierMapping) bool {
			mappingVisits++
			if budget.exhausted {
				visitedAfterExhaustion++
			}
			positional := true
			for _, pair := range mapping {
				if pair.queryIndex != pair.candidateIndex {
					positional = false
					break
				}
			}
			sawPositionalMapping = sawPositionalMapping || positional
			return true
		},
	)

	if !budget.exhausted {
		t.Fatal("nine-way search should exhaust the shared visit budget")
	}
	if budget.visitedStates != matchIntermediateMaxVisitedStates {
		t.Fatalf(
			"visited states = %d, want strict cap %d",
			budget.visitedStates,
			matchIntermediateMaxVisitedStates,
		)
	}
	if mappingVisits == 0 {
		t.Fatal("budgeted search produced no mappings before exhaustion")
	}
	if visitedAfterExhaustion != 0 {
		t.Fatalf(
			"visitor called %d times after exhaustion",
			visitedAfterExhaustion,
		)
	}
	if sawPositionalMapping {
		t.Fatal("search fell back to a positional mapping on exhaustion")
	}

	// The same exhausted budget must stop a later enumeration immediately;
	// budgets are shared across every branch of one MatchIntermediate attempt.
	visitsBeforeReuse := mappingVisits
	oneQuery := enumeratorTestIndependentQuantifiers("q_reuse", 1)
	oneCandidate := enumeratorTestIndependentQuantifiers("c_reuse", 1)
	enumerateQuantifierMappings(
		oneQuery,
		oneCandidate,
		true,
		budget,
		func([]quantifierMapping) bool {
			mappingVisits++
			return true
		},
	)
	if mappingVisits != visitsBeforeReuse {
		t.Fatal("exhausted shared budget allowed a later positional match")
	}
	if budget.visitedStates != matchIntermediateMaxVisitedStates {
		t.Fatalf(
			"budget reuse changed visited states to %d",
			budget.visitedStates,
		)
	}
}
