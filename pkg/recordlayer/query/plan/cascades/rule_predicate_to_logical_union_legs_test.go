package cascades

import (
	"slices"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// The nine factors are the CNF of (p ∧ q ∧ r) ∨ ((s ∨ t) ∧ u ∧ v): every fixed
// subset Java enumerates multiplies out to terms that repeat across subsets.
func TestPredicateUnionLegsAreCanonicalAndShared(t *testing.T) {
	t.Parallel()
	leaves := simplificationContractLeaves(t)
	p, q, r, s, x, u, v := leaves[0], leaves[1], leaves[2], leaves[3], leaves[4], leaves[5], leaves[6]
	or := predicates.NewOr
	clauses := []predicates.QueryPredicate{
		or(p, s, x), or(q, s, x), or(r, s, x),
		or(p, u), or(q, u), or(r, u),
		or(p, v), or(q, v), or(r, v),
	}
	aliases := func(predicate predicates.QueryPredicate) []string {
		var names []string
		for alias := range predicates.GetCorrelatedToOfPredicate(predicate) {
			names = append(names, alias.Name())
		}
		slices.Sort(names)
		return names
	}
	var clauseAliases [][]string
	for _, clause := range clauses {
		clauseAliases = append(clauseAliases, aliases(clause))
	}
	_, ref := makeSelectWithOrPredicates(clauses)
	unions := mustExplorePredicateUnion(t, ref)
	if len(unions) != 511 {
		t.Fatalf("union choices=%d, want Java's 511", len(unions))
	}
	legsByTerm := make(map[string]*expressions.Reference)
	unionLegs := 0
	for _, expression := range unions {
		union := expression.(*expressions.LogicalUniqueExpression).GetInner().GetRangesOver().Get().(*expressions.LogicalUnionExpression)
		for _, quantifier := range union.GetQuantifiers() {
			unionLegs++
			leg := quantifier.GetRangesOver()
			sel := leg.Get().(*expressions.LogicalUniqueExpression).GetInner().GetRangesOver().Get().(*expressions.SelectExpression)
			var term, retained []string
			for _, predicate := range sel.GetPredicates() {
				if predicates.IsAtomic(predicate) {
					retained = append(retained, strings.Join(aliases(predicate), "|"))
				} else {
					term = append(term, aliases(predicate)...)
				}
			}
			slices.Sort(term)
			var unimplied []string
			for _, clause := range clauseAliases {
				if !slices.ContainsFunc(clause, func(alias string) bool { return slices.Contains(term, alias) }) {
					unimplied = append(unimplied, strings.Join(clause, "|"))
				}
			}
			if !slices.Equal(retained, unimplied) {
				t.Fatalf("term %v retains fixed factors %v, want exactly the factors it does not imply %v", term, retained, unimplied)
			}
			key := strings.Join(term, ",")
			if previous, seen := legsByTerm[key]; seen && previous != leg {
				t.Fatalf("term %s is planned as two leg groups", key)
			}
			legsByTerm[key] = leg
		}
	}
	if unionLegs != 2898 {
		t.Fatalf("union legs=%d, want Java's 2898 across all 511 choices", unionLegs)
	}
	// Over these 511 unions: the population the three-valued evaluator in
	// TestPredicateUnionNineFactorSemanticPopulation separates.
	if len(legsByTerm) != 72 {
		t.Fatalf("distinct leg groups=%d, want the 72 distinct terms", len(legsByTerm))
	}
}
