package expressions

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestRelationalAliasCompleteness is the RFC-040 pre-PR-A completeness gate
// (Graefe check-in): for EVERY alias-bearing RelationalExpression, two instances
// that differ ONLY by the quantifier alias their node-info references must
//
//	(a) be EqualsWithoutChildren under the {q0↦q1} alias map (alias-aware), AND
//	(b) have equal HashCodeWithoutChildren (alias-invariant),
//
// and must NOT be equal under the empty map (non-vacuous: the mapping is what
// makes them equal). A new alias-bearing relational type added without wiring
// its EqualsWithoutChildren/HashCodeWithoutChildren to the alias-aware toolkit
// fails this test.
func TestRelationalAliasCompleteness(t *testing.T) {
	t.Parallel()
	q0 := values.NamedCorrelationIdentifier("q0")
	q1 := values.NamedCorrelationIdentifier("q1")
	scanRef := InitialOf(NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	inner := func() Quantifier { return ForEachQuantifier(scanRef) }
	qov := func(a values.CorrelationIdentifier) values.Value { return values.NewQuantifiedObjectValue(a) }
	cmp := func(a values.CorrelationIdentifier) predicates.QueryPredicate {
		return predicates.NewComparisonPredicate(qov(a),
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1)}})
	}

	cases := []struct {
		name  string
		build func(a values.CorrelationIdentifier) RelationalExpression
	}{
		{"LogicalFilter", func(a values.CorrelationIdentifier) RelationalExpression {
			return NewLogicalFilterExpression([]predicates.QueryPredicate{cmp(a)}, inner())
		}},
		{"Select", func(a values.CorrelationIdentifier) RelationalExpression {
			return NewSelectExpression(qov(a), []Quantifier{inner()}, []predicates.QueryPredicate{cmp(a)})
		}},
		{"LogicalSort", func(a values.CorrelationIdentifier) RelationalExpression {
			return NewLogicalSortExpression([]SortKey{{Value: qov(a)}}, inner())
		}},
		{"GroupBy", func(a values.CorrelationIdentifier) RelationalExpression {
			return NewGroupByExpression([]values.Value{qov(a)},
				[]AggregateSpec{{Function: AggCount, Operand: qov(a)}}, inner())
		}},
		{"LogicalProjection", func(a values.CorrelationIdentifier) RelationalExpression {
			return NewLogicalProjectionExpression([]values.Value{qov(a)}, inner())
		}},
		{"LogicalValues", func(a values.CorrelationIdentifier) RelationalExpression {
			return NewLogicalValuesExpression([]values.Value{qov(a)})
		}},
		{"LogicalIntersection", func(a values.CorrelationIdentifier) RelationalExpression {
			return NewLogicalIntersectionExpression([]Quantifier{inner(), inner()}, []values.Value{qov(a)})
		}},
		{"Update", func(a values.CorrelationIdentifier) RelationalExpression {
			return NewUpdateExpression(inner(), "T", []UpdateTransform{{FieldPath: "f", NewValue: qov(a)}})
		}},
	}

	mapping := AliasMapOf(q0, q1)
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, b := tc.build(q0), tc.build(q1)
			if !a.EqualsWithoutChildren(b, mapping) {
				t.Fatalf("%s: alias-variants must be EqualsWithoutChildren under {q0↦q1}", tc.name)
			}
			if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
				t.Fatalf("%s: alias-variants must hash equal (alias-invariant)", tc.name)
			}
			if a.EqualsWithoutChildren(b, EmptyAliasMap()) {
				t.Fatalf("%s: must NOT be equal under empty map — test is vacuous (node-info doesn't reference the alias)", tc.name)
			}
		})
	}
}
