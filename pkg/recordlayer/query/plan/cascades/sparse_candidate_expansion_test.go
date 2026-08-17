package cascades

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"google.golang.org/protobuf/proto"
)

func sparseTestRowType() *values.RecordType {
	return values.NewRecordType("SPARSE_T1", false, []values.Field{
		{Name: "COL1", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 1},
	})
}

func sparseTestCandidate(t *testing.T, pred *gen.Predicate) *ValueIndexScanMatchCandidate {
	t.Helper()
	nonFanOut := false
	cand := NewValueIndexScanMatchCandidateWithFunctions(
		"I1", []string{"T1"}, []string{"COL1"}, nil,
		[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
		sparseTestRowType(), false, []string{"ID"}, &nonFanOut,
	).WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong})
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

// TestUnprovablePredicateNeverMatchesAsAFullIndex is the END-TO-END form of the
// normalizer's invariant: a stored predicate that cannot be PROVEN to accept
// every record must leave the index unable to match as complete.
//
// The observable has to be the candidate TRAVERSAL, not the stored proto. A
// non-nil predicateProto only means the four `predicateProto != nil` gates see
// something; it says nothing about whether the flat expansion actually attached
// a candidate-side predicate. A candidate whose select carries ONLY the column
// placeholder — no predicate — is exactly a full-index candidate, and the
// matcher will serve the whole table from it. Asserting on the stored proto
// would pass while that happened.
func TestUnprovablePredicateNeverMatchesAsAFullIndex(t *testing.T) {
	t.Parallel()

	rowWindow := func() *gen.Predicate {
		return &gen.Predicate{RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
			Size: proto.Int32(100),
		}}
	}
	trueArm := func() *gen.Predicate {
		return &gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{
			Value: gen.ConstantPredicate_TRUE.Enum(),
		}}
	}

	for _, tc := range []struct {
		name string
		why  string
		pred *gen.Predicate
	}{
		{
			name: "bare row-number window",
			why: "a top-N index holds only the qualifying rows; nothing here is an AND, " +
				"so no unwrap is involved and the conversion is the only thing that can refuse it",
			pred: rowWindow(),
		},
		{
			name: "AND(rowWindow, TRUE)",
			why: "the TRUE conjunct folds away and the row-window one survives; " +
				"whether it survives wrapped or bare, the index is still partial",
			pred: &gen.Predicate{AndPredicate: &gen.AndPredicate{Children: []*gen.Predicate{
				rowWindow(), trueArm(),
			}}},
		},
		{
			name: "AND(nil)",
			why:  "a malformed singleton must not normalize to nil and read as a full index",
			pred: &gen.Predicate{AndPredicate: &gen.AndPredicate{Children: []*gen.Predicate{nil}}},
		},
		{
			name: "OR(nil)",
			why:  "same, through the disjunctive singleton collapse",
			pred: &gen.Predicate{OrPredicate: &gen.OrPredicate{Children: []*gen.Predicate{nil}}},
		},
		{
			name: "AND(TRUE, nil)",
			why:  "folding the provable conjunct must not leave the unprovable one erasable",
			pred: &gen.Predicate{AndPredicate: &gen.AndPredicate{Children: []*gen.Predicate{
				trueArm(), nil,
			}}},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cand := sparseTestCandidate(t, tc.pred)

			if cand.GetPredicateProto() == nil {
				t.Fatalf("the candidate boundary ERASED an unprovable predicate (%s); "+
					"every `predicateProto != nil` gate now reads this partial index as full", tc.why)
			}
			if tr := cand.GetTraversal(); tr != nil {
				var placeholders, others int
				for _, p := range candidateSelectPredicates(t, cand) {
					if _, isPlaceholder := p.(*predicates.Placeholder); isPlaceholder {
						placeholders++
						continue
					}
					others++
				}
				if others == 0 {
					t.Fatalf("candidate expanded with %d placeholders and NO candidate-side "+
						"predicate, which is precisely a FULL-index candidate (%s) — "+
						"the matcher will serve the whole table from a partial index",
						placeholders, tc.why)
				}
			}
		})
	}
}

// TestProvableTautologyStillMatchesAsAFullIndex is the contrast that keeps the
// test above from passing vacuously: the fail-closed rule must not have become
// "refuse everything". A predicate that IS provably total still erases, and the
// index still expands as the complete index it is.
func TestProvableTautologyStillMatchesAsAFullIndex(t *testing.T) {
	t.Parallel()
	cand := sparseTestCandidate(t, &gen.Predicate{
		ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()},
	})
	if cand.GetPredicateProto() != nil {
		t.Fatal("a WHERE TRUE predicate must still be erased — it provably accepts every record")
	}
	if cand.GetTraversal() == nil {
		t.Fatal("a WHERE TRUE index must still produce a candidate")
	}
	for _, p := range candidateSelectPredicates(t, cand) {
		if _, isPlaceholder := p.(*predicates.Placeholder); !isPlaceholder {
			t.Fatalf("a complete index carries only column placeholders, got %T", p)
		}
	}
}
