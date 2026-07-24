package predicates

import (
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
		values.NewQuantifiedObjectValue(vecAlias), // query vector: the ONLY mention
		values.NewQuantifiedObjectValue(rhsAlias),
		nil, nil,
	)
	if !ok {
		t.Fatal("setup: could not build a DistanceRank comparison")
	}
	if _, viaComparison := cmp.GetCorrelatedTo()[vecAlias]; !viaComparison {
		t.Fatal("setup: the comparison itself does not report its query-vector correlation")
	}

	pred := NewComparisonPredicate(values.NewQuantifiedObjectValue(lhsAlias), cmp)

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
