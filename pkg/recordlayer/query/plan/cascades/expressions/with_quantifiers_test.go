package expressions

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustWithQuantifiers(
	t testing.TB,
	expression RelationalExpression,
	quantifiers []Quantifier,
) RelationalExpression {
	t.Helper()
	rebuilt, err := expression.WithQuantifiers(quantifiers)
	if err != nil {
		t.Fatalf("WithQuantifiers() unexpected error: %v", err)
	}
	if rebuilt == nil {
		t.Fatal("WithQuantifiers() returned a nil expression without an error")
	}
	return rebuilt
}

func TestWithQuantifiersRejectsArityMismatchWithoutObject(t *testing.T) {
	t.Parallel()

	leaf := &leafScan{name: "child"}
	q1 := ForEachQuantifier(InitialOf(leaf))
	q2 := ForEachQuantifier(InitialOf(leaf))
	selectExpression := mustExpression(NewSelectExpression(values.NewBooleanValue(true), []Quantifier{q1, q2}, nil))
	recursiveUnion := mustExpression(NewRecursiveUnionExpression(
		q1,
		q2,
		values.NamedCorrelationIdentifier("scan"),
		values.NamedCorrelationIdentifier("insert"),
		TraversalPreorder))

	tests := []struct {
		name        string
		expression  RelationalExpression
		quantifiers []Quantifier
	}{
		{
			name:        "full unordered scan",
			expression:  mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType())),
			quantifiers: []Quantifier{q1},
		},
		{
			name:        "logical values",
			expression:  mustExpression(NewLogicalValuesExpression(nil)),
			quantifiers: []Quantifier{q1},
		},
		{
			name: "explode",
			expression: mustExpression(NewExplodeExpression(&values.ConstantValue{
				Value: []any{},
				Typ:   &values.ArrayType{ElementType: values.NotNullLong},
			})),
			quantifiers: []Quantifier{q1},
		},
		{
			name:        "table function",
			expression:  mustExpression(NewTableFunctionExpression(values.NewBooleanValue(true))),
			quantifiers: []Quantifier{q1},
		},
		{
			name:        "temp table scan",
			expression:  mustExpression(NewTempTableScanExpression(values.NamedCorrelationIdentifier("temp"), values.NotNullLong)),
			quantifiers: []Quantifier{q1},
		},
		{
			name:        "group by",
			expression:  &GroupByExpression{inner: q1},
			quantifiers: nil,
		},
		{
			name:        "logical filter",
			expression:  &LogicalFilterExpression{inner: q1},
			quantifiers: nil,
		},
		{
			name:        "delete",
			expression:  &DeleteExpression{inner: q1},
			quantifiers: nil,
		},
		{
			name:        "logical limit",
			expression:  mustExpression(NewLogicalLimitExpression(10, 0, q1)),
			quantifiers: nil,
		},
		{
			name:        "logical distinct",
			expression:  &LogicalDistinctExpression{inner: q1},
			quantifiers: nil,
		},
		{
			name:        "temp table insert",
			expression:  &TempTableInsertExpression{inner: q1},
			quantifiers: nil,
		},
		{
			name:        "logical sort",
			expression:  &LogicalSortExpression{inner: q1},
			quantifiers: nil,
		},
		{
			name:        "update",
			expression:  &UpdateExpression{inner: q1},
			quantifiers: nil,
		},
		{
			name:        "logical projection",
			expression:  &LogicalProjectionExpression{inner: q1},
			quantifiers: nil,
		},
		{
			name:        "logical unique",
			expression:  &LogicalUniqueExpression{inner: q1},
			quantifiers: nil,
		},
		{
			name:        "insert",
			expression:  &InsertExpression{inner: q1},
			quantifiers: nil,
		},
		{
			name:        "logical type filter",
			expression:  &LogicalTypeFilterExpression{inner: q1},
			quantifiers: nil,
		},
		{
			name:        "matchable sort",
			expression:  &MatchableSortExpression{inner: q1},
			quantifiers: nil,
		},
		{
			name:        "recursive union",
			expression:  recursiveUnion,
			quantifiers: []Quantifier{q1},
		},
		{
			name:        "logical union",
			expression:  mustExpression(NewLogicalUnionExpression([]Quantifier{q1, q2})),
			quantifiers: []Quantifier{q1},
		},
		{
			name:        "logical intersection",
			expression:  mustExpression(NewLogicalIntersectionExpression([]Quantifier{q1, q2}, nil)),
			quantifiers: []Quantifier{q1},
		},
		{
			name:        "select",
			expression:  selectExpression,
			quantifiers: []Quantifier{q1},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			rebuilt, err := test.expression.WithQuantifiers(test.quantifiers)
			if !errors.Is(err, ErrQuantifierArity) {
				t.Fatalf("WithQuantifiers() error = %v, want errors.Is(ErrQuantifierArity)", err)
			}
			if rebuilt != nil {
				t.Fatalf("WithQuantifiers() result = %T, want nil on arity error", rebuilt)
			}
		})
	}
}

func TestWithQuantifiersExactArityRebuildsPositionally(t *testing.T) {
	t.Parallel()

	leaf := &leafScan{name: "child"}
	q1 := ForEachQuantifier(InitialOf(leaf))
	q2 := ForEachQuantifier(InitialOf(leaf))
	q3 := ForEachQuantifier(InitialOf(leaf))

	scan := mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType()))
	if rebuilt := mustWithQuantifiers(t, scan, nil); rebuilt != scan {
		t.Fatalf("leaf rebuild = %T at %p, want the unchanged leaf at %p", rebuilt, rebuilt, scan)
	}

	limit := mustExpression(NewLogicalLimitExpression(10, 3, q1))
	rebuiltLimit, ok := mustWithQuantifiers(t, limit, []Quantifier{q2}).(*LogicalLimitExpression)
	if !ok {
		t.Fatalf("unary rebuild type = %T, want *LogicalLimitExpression", rebuiltLimit)
	}
	if rebuiltLimit.GetInner() != q2 || rebuiltLimit.GetLimit() != 10 || rebuiltLimit.GetOffset() != 3 {
		t.Fatalf(
			"unary rebuild = {inner:%v limit:%d offset:%d}, want replacement inner with node data intact",
			rebuiltLimit.GetInner(),
			rebuiltLimit.GetLimit(),
			rebuiltLimit.GetOffset(),
		)
	}

	recursiveUnion := mustExpression(NewRecursiveUnionExpression(
		q1,
		q2,
		values.NamedCorrelationIdentifier("scan"),
		values.NamedCorrelationIdentifier("insert"),
		TraversalPostorder))

	rebuiltRecursive, ok := mustWithQuantifiers(t, recursiveUnion, []Quantifier{q2, q3}).(*RecursiveUnionExpression)
	if !ok {
		t.Fatalf("multi rebuild type = %T, want *RecursiveUnionExpression", rebuiltRecursive)
	}
	if rebuiltRecursive.GetInitialState() != q2 || rebuiltRecursive.GetRecursiveState() != q3 {
		t.Fatal("multi rebuild did not preserve the replacement quantifiers' positional order")
	}
	if rebuiltRecursive.GetTraversalStrategy() != TraversalPostorder {
		t.Fatal("multi rebuild dropped node information")
	}

	selectExpression := mustExpression(NewSelectExpressionWithJoinType(
		values.NewBooleanValue(true),
		[]Quantifier{q1, q2},
		nil,
		[]string{"left", "right"},
		JoinCross))

	rebuiltSelect, ok := mustWithQuantifiers(t, selectExpression, []Quantifier{q2, q3}).(*SelectExpression)
	if !ok {
		t.Fatalf("select rebuild type = %T, want *SelectExpression", rebuiltSelect)
	}
	gotSelectQuantifiers := rebuiltSelect.GetQuantifiers()
	if len(gotSelectQuantifiers) != 2 || gotSelectQuantifiers[0] != q2 || gotSelectQuantifiers[1] != q3 {
		t.Fatalf("select quantifiers = %v, want exact positional replacements", gotSelectQuantifiers)
	}
	if rebuiltSelect.GetJoinType() != JoinCross {
		t.Fatal("select rebuild dropped node information")
	}
}
