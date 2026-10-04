package predicates

import (
	"maps"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

type correlationOnlyTestPredicate struct {
	correlations map[values.CorrelationIdentifier]struct{}
}

func (*correlationOnlyTestPredicate) Children() []QueryPredicate { return nil }
func (*correlationOnlyTestPredicate) Eval(any) (TriBool, error)  { return TriTrue, nil }
func (*correlationOnlyTestPredicate) Explain() string            { return "CORRELATION_ONLY" }
func (p *correlationOnlyTestPredicate) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	return p.correlations
}

// TestComparisonPredicate_GetCorrelatedTo_IncludesQueryVector pins that a
// ComparisonPredicate reports every correlation its comparison carries, not
// just the ones reachable through Operand.
//
// A DistanceRank comparison holds a query vector in addition to its operand.
// Reading Operand directly dropped any correlation living only in that vector,
// so a vector-search predicate correlated to an outer quantifier looked
// uncorrelated — and callers deciding which quantifiers a compensation or join
// still needs would drop the one supplying the vector.
func TestComparisonPredicate_GetCorrelatedTo_IncludesQueryVector(t *testing.T) {
	t.Parallel()

	vecAlias := values.NamedCorrelationIdentifier("qVec")
	lhsAlias := values.NamedCorrelationIdentifier("qLhs")
	rhsAlias := values.NamedCorrelationIdentifier("qRhs")

	cmp, ok := NewDistanceRankComparison(
		ComparisonDistanceRankLessThanOrEq,
		mustQOV(t, vecAlias), // query vector: the ONLY mention
		mustQOV(t, rhsAlias),
		nil, nil,
	)
	if !ok {
		t.Fatal("setup: could not build a DistanceRank comparison")
	}
	if _, viaComparison := cmp.GetCorrelatedTo()[vecAlias]; !viaComparison {
		t.Fatal("setup: the comparison itself does not report its query-vector correlation")
	}

	pred := NewComparisonPredicate(mustQOV(t, lhsAlias), cmp)

	correlations := pred.GetCorrelatedTo()
	if _, ok := correlations[lhsAlias]; !ok {
		t.Fatal("predicate must report its LHS operand correlation")
	}
	if _, ok := correlations[rhsAlias]; !ok {
		t.Fatal("predicate must report its comparison-operand correlation")
	}
	if _, ok := correlations[vecAlias]; !ok {
		t.Fatal("predicate must report the query-vector correlation carried by its comparison")
	}
	if len(correlations) != 3 {
		t.Fatalf("correlations = %v, want LHS, RHS, and query-vector aliases", correlations)
	}

	// The shared helper is what the planner calls; it must agree.
	if _, ok := GetCorrelatedToOfPredicate(pred)[vecAlias]; !ok {
		t.Fatal("GetCorrelatedToOfPredicate misses the query-vector correlation")
	}
}

func BenchmarkCompoundPredicateCorrelations(b *testing.B) {
	left := mustQOV(b, values.NamedCorrelationIdentifier("left"))
	right := mustQOV(b, values.NamedCorrelationIdentifier("right"))
	comparison := NewComparisonPredicate(left, Comparison{Type: ComparisonEquals, Operand: right})
	var predicate QueryPredicate = comparison
	for range 9 {
		predicate = NewOr(NewNot(predicate), comparison)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := GetCorrelatedToOfPredicate(predicate); len(got) != 2 {
			b.Fatalf("correlations=%v, want left and right", got)
		}
	}
}

func rangeCorrelationPredicate(t testing.TB) *PredicateWithValueAndRanges {
	t.Helper()
	return NewPredicateWithValueAndRanges(mustQOV(t, values.NamedCorrelationIdentifier("left")), []*RangeConstraints{
		NewRangeConstraints([]Comparison{{Type: ComparisonGreaterThan, Operand: values.LiteralValue(int64(0))}},
			[]Comparison{{
				Type:        ComparisonDistanceRankLessThan,
				Operand:     mustQOV(t, values.NamedCorrelationIdentifier("right")),
				QueryVector: mustQOV(t, values.NamedCorrelationIdentifier("vector")),
			}}),
		EmptyRangeConstraints(),
	})
}

func TestRangePredicateCorrelationsPreserveRangesAndFreshResults(t *testing.T) {
	t.Parallel()
	ranged := rangeCorrelationPredicate(t)
	root := NewAnd(NewNot(ranged), ranged)
	want := map[values.CorrelationIdentifier]struct{}{
		values.NamedCorrelationIdentifier("left"): {}, values.NamedCorrelationIdentifier("right"): {},
		values.NamedCorrelationIdentifier("vector"): {},
	}
	for _, read := range []func() map[values.CorrelationIdentifier]struct{}{
		ranged.GetCorrelatedTo,
		func() map[values.CorrelationIdentifier]struct{} { return GetCorrelatedToOfPredicate(root) },
	} {
		for range 2 {
			got := read()
			if !maps.Equal(got, want) {
				t.Fatalf("correlations = %v, want %v", got, want)
			}
			clear(got)
		}
	}
	ranged.value = values.NewBooleanValue(true)
	delete(want, values.NamedCorrelationIdentifier("left"))
	if got := GetCorrelatedToOfPredicate(root); !maps.Equal(got, want) {
		t.Fatalf("correlations after value replacement = %v, want %v", got, want)
	}
}

type overridingRangeCorrelationPredicate struct {
	*PredicateWithValueAndRanges
	correlations map[values.CorrelationIdentifier]struct{}
}

func (p *overridingRangeCorrelationPredicate) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	return p.correlations
}

func TestCollectCorrelatedToOfPredicate(t *testing.T) {
	t.Parallel()
	out := CollectCorrelatedToOfPredicate(nil, nil)
	if out == nil || len(out) != 0 {
		t.Fatalf("nil input = %v, want a writable empty set", out)
	}
	outer := values.NamedCorrelationIdentifier("outer")
	overridden := values.NamedCorrelationIdentifier("overridden")
	out[outer] = struct{}{}
	ranged := rangeCorrelationPredicate(t)
	borrowed := map[values.CorrelationIdentifier]struct{}{overridden: {}}
	custom := &overridingRangeCorrelationPredicate{PredicateWithValueAndRanges: ranged, correlations: borrowed}
	want := map[values.CorrelationIdentifier]struct{}{outer: {}, overridden: {}}
	if got := CollectCorrelatedToOfPredicate(NewNot(custom), out); !maps.Equal(got, want) {
		t.Fatalf("custom range override = %v, want %v", got, want)
	}
	for _, alias := range []string{"left", "right", "vector"} {
		want[values.NamedCorrelationIdentifier(alias)] = struct{}{}
	}
	CollectCorrelatedToOfPredicate(ranged, out)
	CollectCorrelatedToOfPredicate(nil, out)
	if !maps.Equal(out, want) {
		t.Fatalf("shared result = %v, want %v", out, want)
	}
	if !maps.Equal(borrowed, map[values.CorrelationIdentifier]struct{}{overridden: {}}) {
		t.Fatal("collection changed an unknown predicate's borrowed correlations")
	}
}

func BenchmarkRangePredicateCorrelations(b *testing.B) {
	ranged := rangeCorrelationPredicate(b)
	var root QueryPredicate = ranged
	for range 9 {
		root = NewOr(NewNot(root), ranged)
	}
	GetCorrelatedToOfPredicate(root)
	b.ReportAllocs()
	for b.Loop() {
		if got := GetCorrelatedToOfPredicate(root); len(got) != 3 {
			b.Fatalf("correlations = %v, want left, right and vector", got)
		}
	}
}

func predicateCorrelationLookup(p QueryPredicate, alias values.CorrelationIdentifier) bool {
	_, found := GetCorrelatedToOfPredicate(p)[alias]
	return found
}

func BenchmarkRangePredicateCorrelationLookup(b *testing.B) {
	ranged := rangeCorrelationPredicate(b)
	root := NewAnd(NewNot(ranged), ranged)
	alias := values.NamedCorrelationIdentifier("vector")
	predicateCorrelationLookup(root, alias)
	b.ReportAllocs()
	for b.Loop() {
		if !predicateCorrelationLookup(root, alias) {
			b.Fatal("missing query-vector correlation")
		}
	}
}

type overridingCorrelationPredicate struct {
	*ComparisonPredicate
	correlations map[values.CorrelationIdentifier]struct{}
}

func (p *overridingCorrelationPredicate) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	return p.correlations
}

func TestCompoundPredicateCorrelationsRespectOverridesAndMutation(t *testing.T) {
	t.Parallel()
	left := values.NamedCorrelationIdentifier("left")
	right := values.NamedCorrelationIdentifier("right")
	vector := values.NamedCorrelationIdentifier("vector")
	overridden := values.NamedCorrelationIdentifier("overridden")
	borrowed := map[values.CorrelationIdentifier]struct{}{overridden: {}}
	comparison := NewComparisonPredicate(mustQOV(t, left), Comparison{
		Type: ComparisonDistanceRankLessThan, Operand: mustQOV(t, right), QueryVector: mustQOV(t, vector),
	})
	custom := &overridingCorrelationPredicate{ComparisonPredicate: comparison, correlations: borrowed}
	root := NewAnd(NewNot(NewOr(nil, custom)), comparison)
	want := map[values.CorrelationIdentifier]struct{}{left: {}, right: {}, vector: {}, overridden: {}}
	for _, read := range []func() map[values.CorrelationIdentifier]struct{}{
		root.GetCorrelatedTo,
		func() map[values.CorrelationIdentifier]struct{} { return GetCorrelatedToOfPredicate(root) },
	} {
		for range 2 {
			got := read()
			if !maps.Equal(got, want) {
				t.Fatalf("correlations=%v, want %v", got, want)
			}
			clear(got)
		}
	}
	if !maps.Equal(borrowed, map[values.CorrelationIdentifier]struct{}{overridden: {}}) {
		t.Fatal("collecting correlations mutated an unfamiliar predicate's borrowed map")
	}
	comparison.Operand = values.NewBooleanValue(true)
	comparison.Comparison.QueryVector = nil
	delete(want, left)
	delete(want, vector)
	if got := GetCorrelatedToOfPredicate(root); !maps.Equal(got, want) {
		t.Fatalf("correlations after mutation=%v, want %v", got, want)
	}
}

func TestGetCorrelatedToOfPredicate_DelegatesAndReturnsFreshMap(t *testing.T) {
	t.Parallel()

	carried := values.NamedCorrelationIdentifier("custom")
	injected := values.NamedCorrelationIdentifier("injected")
	predicate := &correlationOnlyTestPredicate{
		correlations: map[values.CorrelationIdentifier]struct{}{carried: {}},
	}

	first := GetCorrelatedToOfPredicate(predicate)
	if _, ok := first[carried]; !ok {
		t.Fatal("helper missed a correlation reported by an otherwise unknown predicate implementation")
	}

	delete(first, carried)
	first[injected] = struct{}{}
	if _, ok := predicate.correlations[carried]; !ok {
		t.Fatal("mutating the helper result mutated the predicate's own correlation set")
	}
	if _, ok := predicate.correlations[injected]; ok {
		t.Fatal("helper returned the predicate's map instead of a fresh copy")
	}

	second := GetCorrelatedToOfPredicate(predicate)
	if _, ok := second[carried]; !ok {
		t.Fatal("a later helper call did not return the predicate's original correlation")
	}
	if _, ok := second[injected]; ok {
		t.Fatal("mutation of one helper result leaked into a later call")
	}
}
