package expressions

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

type stubExpr struct{ name string }

func (s *stubExpr) GetResultValue() values.Value    { return values.NewNullValue(values.UnknownType) }
func (s *stubExpr) GetQuantifiers() []Quantifier    { return nil }
func (s *stubExpr) CanCorrelate() bool              { return false }
func (s *stubExpr) ChildrenAsSet() bool             { return false }
func (s *stubExpr) HashCodeWithoutChildren() uint64 { return 0 }
func (s *stubExpr) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return nil
}

func (s *stubExpr) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*stubExpr)
	return ok && o.name == s.name
}

func (s *stubExpr) WithQuantifiers(_ []Quantifier) RelationalExpression { return s }

func TestForEachQuantifier_FreshAlias(t *testing.T) {
	t.Parallel()
	ref := InitialOf(&stubExpr{name: "T"})
	q1 := ForEachQuantifier(ref)
	q2 := ForEachQuantifier(ref)
	if q1.GetAlias() == q2.GetAlias() {
		t.Fatal("two ForEachQuantifier calls returned the same alias — should be unique")
	}
	if q1.GetRangesOver() != ref {
		t.Fatal("RangesOver pointer changed")
	}
	if q1.Kind() != QuantifierForEach {
		t.Fatalf("kind=%v, want ForEach", q1.Kind())
	}
}

func TestNamedForEachQuantifier_PreservesAlias(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("salesrow")
	ref := InitialOf(&stubExpr{name: "Sales"})
	q := NamedForEachQuantifier(alias, ref)
	if q.GetAlias() != alias {
		t.Fatalf("alias=%v, want %v", q.GetAlias(), alias)
	}
	if q.GetRangesOver() != ref {
		t.Fatal("RangesOver pointer changed")
	}
}

func TestQuantifier_FlowedObjectValue_CarriesAlias(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("rec")
	q := NamedForEachQuantifier(alias, InitialOf(&stubExpr{name: "T"}))
	v := q.GetFlowedObjectValue()
	corrSet := v.(*values.QuantifiedObjectValue).GetCorrelatedTo()
	if _, ok := corrSet[alias]; !ok {
		t.Fatalf("flowed object doesn't carry the quantifier's alias %v in its correlation set %v", alias, corrSet)
	}
}

func TestExistentialQuantifier_KindAndAlias(t *testing.T) {
	t.Parallel()
	ref := InitialOf(&stubExpr{name: "X"})
	q1 := ExistentialQuantifier(ref)
	if q1.Kind() != QuantifierExistential {
		t.Fatalf("kind=%v, want Existential", q1.Kind())
	}
	q2 := ExistentialQuantifier(ref)
	if q1.GetAlias() == q2.GetAlias() {
		t.Fatal("two ExistentialQuantifier calls returned the same alias — should be unique")
	}
	if q1.GetRangesOver() != ref {
		t.Fatal("RangesOver pointer changed")
	}
}

func TestNamedExistentialQuantifier_PreservesAlias(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("exists_subquery")
	ref := InitialOf(&stubExpr{name: "Sub"})
	q := NamedExistentialQuantifier(alias, ref)
	if q.Kind() != QuantifierExistential {
		t.Fatalf("kind=%v, want Existential", q.Kind())
	}
	if q.GetAlias() != alias {
		t.Fatalf("alias=%v, want %v", q.GetAlias(), alias)
	}
}

// TestQuantifierKind_DoesNotAffectFlowedObjectValue pins that
// GetFlowedObjectValue returns a QuantifiedObjectValue regardless of
// kind. The seed treats ForEach and Existential identically here —
// future MaxMatchMap work will introduce kind-aware semantics, but
// the seed contract is clear: the alias is what matters.
func TestQuantifierKind_DoesNotAffectFlowedObjectValue(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("q1")
	ref := InitialOf(&stubExpr{name: "T"})
	forEach := NamedForEachQuantifier(alias, ref)
	existential := NamedExistentialQuantifier(alias, ref)
	if forEach.GetFlowedObjectValue().(*values.QuantifiedObjectValue).Correlation != alias {
		t.Fatal("ForEach flowed-object alias mismatch")
	}
	if existential.GetFlowedObjectValue().(*values.QuantifiedObjectValue).Correlation != alias {
		t.Fatal("Existential flowed-object alias mismatch")
	}
}
