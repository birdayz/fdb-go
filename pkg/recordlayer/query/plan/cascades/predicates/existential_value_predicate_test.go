package predicates

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestExistentialValuePredicate_LeafShape(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("subq1")
	p := NewExistentialAlias(alias)
	if got := len(p.Children()); got != 0 {
		t.Fatalf("ExistentialValuePredicate children = %d, want 0", got)
	}
	if p.GetExistentialAlias().Name() != "subq1" {
		t.Fatalf("alias = %q, want subq1", p.GetExistentialAlias().Name())
	}
}

func TestExistentialValuePredicate_EvalIsUnknown(t *testing.T) {
	t.Parallel()
	p := NewExistentialAlias(values.NamedCorrelationIdentifier("x"))
	if got, _ := p.Eval(nil); got != TriUnknown {
		t.Fatalf("Eval = %v, want TriUnknown (per-row eval not supported for EXISTS)", got)
	}
}

func TestExistentialValuePredicate_ExplainRenders(t *testing.T) {
	t.Parallel()
	p := NewExistentialAlias(values.NamedCorrelationIdentifier("subq"))
	if got := p.Explain(); got != "EXISTS(subq)" {
		t.Fatalf("Explain = %q, want EXISTS(subq)", got)
	}
}

func TestExistentialValuePredicate_CorrelatedTo(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("subq")
	p := NewExistentialAlias(alias)
	cs := p.GetCorrelatedTo()
	if len(cs) != 1 {
		t.Fatalf("CorrelatedTo size = %d, want 1", len(cs))
	}
	if _, ok := cs[alias]; !ok {
		t.Fatalf("CorrelatedTo missing alias %v", alias)
	}
}

func TestExistentialValuePredicate_ComparisonIsNotNull(t *testing.T) {
	t.Parallel()
	p := NewExistentialAlias(values.NamedCorrelationIdentifier("x"))
	if p.Comparison.Type != ComparisonIsNotNull {
		t.Fatalf("comparison = %v, want ComparisonIsNotNull", p.Comparison.Type)
	}
}

func TestNewExistentialValuePredicate_RejectsNonQOV(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("constructing with a non-QuantifiedObjectValue should panic")
		}
	}()
	_ = NewExistentialValuePredicate(values.NewFieldValue(nil, "f", nil), Comparison{Type: ComparisonIsNotNull})
}

func TestExistentialValuePredicate_SatisfiesInterface(t *testing.T) {
	t.Parallel()
	var _ QueryPredicate = (*ExistentialValuePredicate)(nil)
}

func TestIsExistentialPredicate(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("e")

	// The ExistentialValuePredicate itself.
	if got, ok := IsExistentialPredicate(NewExistentialAlias(alias)); !ok || got != alias {
		t.Fatalf("IsExistentialPredicate(EVP) = (%v, %v), want (%v, true)", got, ok, alias)
	}

	// An ExistentialValuePredicate constructed with a non-NOT_NULL comparison must NOT be
	// classified as a positive EXISTS (codex): the exported constructor permits any comparison,
	// and only "IS NOT NULL over the QOV" is the existential semi-join shape.
	wrongComp := NewExistentialValuePredicate(values.NewQuantifiedObjectValue(alias), Comparison{Type: ComparisonIsNull})
	if _, ok := IsExistentialPredicate(wrongComp); ok {
		t.Fatal("an ExistentialValuePredicate with IS NULL must NOT be classified as existential")
	}

	// The residual ComparisonPredicate shape (QOV operand + NOT_NULL).
	residual := NewComparisonPredicate(values.NewQuantifiedObjectValue(alias), Comparison{Type: ComparisonIsNotNull})
	if got, ok := IsExistentialPredicate(residual); !ok || got != alias {
		t.Fatalf("IsExistentialPredicate(residual) = (%v, %v), want (%v, true)", got, ok, alias)
	}

	// A non-existential ComparisonPredicate (wrong comparison type).
	notIt := NewComparisonPredicate(values.NewQuantifiedObjectValue(alias), Comparison{Type: ComparisonIsNull})
	if _, ok := IsExistentialPredicate(notIt); ok {
		t.Fatal("IS NULL over a QOV must NOT be classified as existential")
	}

	// A plain value predicate.
	if _, ok := IsExistentialPredicate(NewConstantPredicate(TriTrue)); ok {
		t.Fatal("ConstantPredicate must NOT be classified as existential")
	}
}

func TestIsNotExistentialPredicate(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("e")

	notExists := NewNot(NewExistentialAlias(alias))
	if got, ok := IsNotExistentialPredicate(notExists); !ok || got != alias {
		t.Fatalf("IsNotExistentialPredicate(NOT EVP) = (%v, %v), want (%v, true)", got, ok, alias)
	}

	// A bare EXISTS is not a NOT-EXISTS.
	if _, ok := IsNotExistentialPredicate(NewExistentialAlias(alias)); ok {
		t.Fatal("bare existential must NOT be classified as NOT-existential")
	}

	// NOT over something non-existential.
	if _, ok := IsNotExistentialPredicate(NewNot(NewConstantPredicate(TriTrue))); ok {
		t.Fatal("NOT(constant) must NOT be classified as NOT-existential")
	}
}

func TestExistsValueToQueryPredicate(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("subq")
	ev := values.NewExistsValue(alias)
	p := ExistsValueToQueryPredicate(ev)
	evp, ok := p.(*ExistentialValuePredicate)
	if !ok {
		t.Fatalf("ExistsValueToQueryPredicate = %T, want *ExistentialValuePredicate", p)
	}
	if evp.GetExistentialAlias() != alias {
		t.Fatalf("alias = %v, want %v", evp.GetExistentialAlias(), alias)
	}
	if evp.Comparison.Type != ComparisonIsNotNull {
		t.Fatalf("comparison = %v, want ComparisonIsNotNull", evp.Comparison.Type)
	}
}
