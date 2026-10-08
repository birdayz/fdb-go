package cascades

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/indexpredicate"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"google.golang.org/protobuf/proto"
)

// TestRowNumberWindowPredicateIsNeverFoldedAsATautology pins a DELIBERATE
// divergence from Java's conversion, and the reason it is deliberate.
//
// Java's RowNumberWindowPredicate.toPredicate returns ConstantPredicate.TRUE
// (IndexPredicate.java:770-772). Read literally, that makes a row-window arm a
// tautological conjunct which AndPredicate.and would drop. Go's own candidate
// converter deliberately does NOT mirror it — indexPredicateToQueryPredicate
// refuses the arm rather than returning TRUE — and this test guards the other
// half of that decision, in the normalizer.
//
// It is not one. The TRUE says the constraint is not expressible as a
// QueryPredicate over a single record — NOT that it accepts every record. It
// rejects nearly all of them: `QualifyRowNumber(score, DESC) <= 100` "keeps the
// 100 records with the highest score values in the index"
// (IndexPredicate.java:608-619). Folding it would classify a top-100 index as
// COMPLETE, drop the candidate's sparseness at every gate, and let a scan serve
// it as the whole table — a wrong-rows bug, the exact class this normalizer was
// introduced to close.
//
// So the arm is left unfolded and the index stays filtering everywhere. If a
// future change makes Go's conversion the authority for folding, this test is
// what fails.
func TestRowNumberWindowPredicateIsNeverFoldedAsATautology(t *testing.T) {
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

	if indexpredicate.IsTautology(rowWindow()) {
		t.Fatal("a row-number window predicate was classified as a tautology; it keeps " +
			"only the top-N records, so the index it guards is the OPPOSITE of complete")
	}

	// The conjunctive case: AND(rowWindow, TRUE). The TRUE
	// conjunct folds away; the row-window one must survive and keep the whole
	// conjunction filtering. Java restricts row-window arms to AND-only paths
	// (IndexPredicate.java:235-243), so this is the shape that actually occurs.
	andWithRowWindow := &gen.Predicate{AndPredicate: &gen.AndPredicate{Children: []*gen.Predicate{
		rowWindow(), trueArm(),
	}}}
	if indexpredicate.IsTautology(andWithRowWindow) {
		t.Fatal("AND(rowWindow, TRUE) was classified as a tautology — the surviving " +
			"row-window conjunct keeps the index partial")
	}
	normalized := indexpredicate.Normalize(andWithRowWindow)
	if normalized.GetRowNumberWindowPredicate() == nil {
		t.Fatalf("AND(rowWindow, TRUE) normalized to %v, want the row-window conjunct "+
			"to survive as the lone child", normalized)
	}

	// The candidate boundary must agree: sparseness is retained.
	duplicates := false
	cand := NewValueIndexScanMatchCandidateWithFunctions(
		"idx_topn", []string{"Item"}, []string{"SCORE"}, nil,
		[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
		values.UnknownType, false, nil, &duplicates,
	).WithPredicateProto(andWithRowWindow)
	if cand.GetPredicateProto() == nil {
		t.Fatal("the candidate boundary dropped a row-window predicate; a top-N index " +
			"would then be matched as if it held every record")
	}
}
