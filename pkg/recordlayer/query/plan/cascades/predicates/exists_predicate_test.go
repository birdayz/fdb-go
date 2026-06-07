package predicates

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestExistsPredicate_LeafShape(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("subq1")
	p := NewExistsPredicate(alias)
	if got := len(p.Children()); got != 0 {
		t.Fatalf("ExistsPredicate children = %d, want 0", got)
	}
	if p.GetExistentialAlias().Name() != "subq1" {
		t.Fatalf("alias = %q, want subq1", p.GetExistentialAlias().Name())
	}
}

func TestExistsPredicate_EvalIsUnknown(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("x")
	p := NewExistsPredicate(alias)
	if got := mustEval(p, nil); got != TriUnknown {
		t.Fatalf("Eval = %v, want TriUnknown (per-row eval not supported for EXISTS)", got)
	}
}

func TestExistsPredicate_ExplainRenders(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("subq")
	p := NewExistsPredicate(alias)
	if got := p.Explain(); got != "EXISTS(subq)" {
		t.Fatalf("Explain = %q, want EXISTS(subq)", got)
	}
}

func TestExistsPredicate_HashStable(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("subq")
	p1 := NewExistsPredicate(alias)
	p2 := NewExistsPredicate(alias)
	if p1.HashCodeWithoutChildren() != p2.HashCodeWithoutChildren() {
		t.Fatal("equal ExistsPredicates should hash equal")
	}
	other := NewExistsPredicate(values.NamedCorrelationIdentifier("different"))
	if p1.HashCodeWithoutChildren() == other.HashCodeWithoutChildren() {
		t.Fatal("predicates with different aliases should hash different")
	}
}

func TestExistsPredicate_SatisfiesQueryPredicateInterface(t *testing.T) {
	t.Parallel()
	var _ QueryPredicate = (*ExistsPredicate)(nil) // compile-time check
}
