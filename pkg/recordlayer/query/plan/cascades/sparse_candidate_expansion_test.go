package cascades

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func sparseTestCandidate(t *testing.T, pred *gen.Predicate) *ValueIndexScanMatchCandidate {
	t.Helper()
	nonFanOut := false
	cand := NewValueIndexScanMatchCandidateWithFunctions(
		"I1", []string{"T1"}, []string{"COL1"}, nil,
		[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
		values.UnknownType, false, []string{"ID"}, &nonFanOut,
	)
	if pred != nil {
		cand.WithPredicateProto(pred)
	}
	return cand
}

func candidateSelectPredicates(t *testing.T, cand *ValueIndexScanMatchCandidate) []predicates.QueryPredicate {
	t.Helper()
	tr := cand.GetTraversal()
	if tr == nil {
		t.Fatal("nil traversal")
	}
	var found []predicates.QueryPredicate
	seen := map[*expressions.Reference]bool{}
	var walk func(ref *expressions.Reference)
	walk = func(ref *expressions.Reference) {
		if ref == nil || seen[ref] {
			return
		}
		seen[ref] = true
		for _, m := range ref.AllMembers() {
			if sel, ok := m.(*expressions.SelectExpression); ok {
				found = append(found, sel.GetPredicates()...)
			}
			for _, q := range m.GetQuantifiers() {
				walk(q.GetRangesOver())
			}
		}
	}
	walk(tr.GetRootReference())
	return found
}

// TestSparseCandidateExpansionCarriesPredicate pins the candidate-graph shape
// of a SPARSE index (ValueIndexExpansionVisitor.java:138-162): the stored
// predicate converts into a candidate-side QueryPredicate alongside the
// column placeholders, which is what makes the matcher refuse to treat the
// filtered index as full.
func TestSparseCandidateExpansionCarriesPredicate(t *testing.T) {
	t.Parallel()
	lt := gen.ComparisonType_LESS_THAN
	n := int32(200)
	cand := sparseTestCandidate(t, &gen.Predicate{ValuePredicate: &gen.ValuePredicate{
		Value: []string{"COL1"},
		Comparison: &gen.Comparison{SimpleComparison: &gen.SimpleComparison{
			Type: &lt, Operand: &gen.Value{IntValue: &n},
		}},
	}})
	preds := candidateSelectPredicates(t, cand)
	var comparisons, placeholders int
	for _, p := range preds {
		switch p.(type) {
		case *predicates.ComparisonPredicate:
			comparisons++
		case *predicates.Placeholder:
			placeholders++
		}
	}
	if placeholders != 1 || comparisons != 1 {
		t.Fatalf("candidate select carries %d placeholders and %d comparison predicates, want 1 and 1 — "+
			"the sparse predicate must ride the candidate graph", placeholders, comparisons)
	}
}

// TestSparseCandidateExpansionSkipsTautology pins the :141 gate: a stored
// `WHERE TRUE` predicate is a tautology and is NOT attached — the candidate
// stays an ordinary full value index.
func TestSparseCandidateExpansionSkipsTautology(t *testing.T) {
	t.Parallel()
	v := gen.ConstantPredicate_TRUE
	cand := sparseTestCandidate(t, &gen.Predicate{
		ConstantPredicate: &gen.ConstantPredicate{Value: &v},
	})
	for _, p := range candidateSelectPredicates(t, cand) {
		if _, isPh := p.(*predicates.Placeholder); !isPh {
			t.Fatalf("tautological predicate attached to the candidate: %T %s", p, p.Explain())
		}
	}
}
