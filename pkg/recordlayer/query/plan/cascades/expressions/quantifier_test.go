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
