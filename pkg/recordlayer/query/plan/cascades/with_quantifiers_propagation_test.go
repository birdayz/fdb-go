package cascades

import (
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

var errWithQuantifiersProbe = errors.New("with quantifiers probe")

type failingWithQuantifiersExpression struct{}

func (*failingWithQuantifiersExpression) GetResultValue() values.Value {
	return &values.ConstantValue{Value: int64(1)}
}

func (*failingWithQuantifiersExpression) GetQuantifiers() []expressions.Quantifier {
	return nil
}

func (*failingWithQuantifiersExpression) CanCorrelate() bool { return false }

func (*failingWithQuantifiersExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return nil
}

func (e *failingWithQuantifiersExpression) EqualsWithoutChildren(
	other expressions.RelationalExpression,
	_ *expressions.AliasMap,
) bool {
	return e == other
}

func (*failingWithQuantifiersExpression) HashCodeWithoutChildren() uint64 { return 1 }
func (*failingWithQuantifiersExpression) ChildrenAsSet() bool             { return false }

func (*failingWithQuantifiersExpression) WithQuantifiers(
	quantifiers []expressions.Quantifier,
) (expressions.RelationalExpression, error) {
	if len(quantifiers) != 0 {
		return nil, fmt.Errorf("%w: failingWithQuantifiersExpression requires 0, got %d",
			expressions.ErrQuantifierArity, len(quantifiers))
	}
	return nil, errWithQuantifiersProbe
}

func TestScanPlanExpressionWithQuantifiersRejectsChildren(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("T", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
	})
	plan, planErr := plans.NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	if planErr != nil {
		t.Fatalf("construct propagation scan: %v", planErr)
	}
	scan := &scanPlanExpression{plan: plan}
	logical, logicalErr := expressions.NewFullUnorderedScanExpression([]string{"T"}, rowType)
	if logicalErr != nil {
		t.Fatalf("construct propagation logical scan: %v", logicalErr)
	}
	got, err := scan.WithQuantifiers([]expressions.Quantifier{
		expressions.ForEachQuantifier(expressions.InitialOf(logical)),
	})
	if !errors.Is(err, expressions.ErrQuantifierArity) {
		t.Fatalf("WithQuantifiers error = %v, want %v", err, expressions.ErrQuantifierArity)
	}
	if got != nil {
		t.Fatalf("WithQuantifiers returned %#v after arity error, want nil", got)
	}

	got, err = scan.WithQuantifiers(nil)
	if err != nil {
		t.Fatalf("WithQuantifiers(nil): %v", err)
	}
	if got != scan {
		t.Fatalf("WithQuantifiers(nil) returned %p, want original %p", got, scan)
	}
}

func TestPushValuesDefaultPropagatesWithQuantifiersError(t *testing.T) {
	t.Parallel()

	got, err := pushValuesDefault(
		&failingWithQuantifiersExpression{},
		nil,
		EmptyTranslationMap(),
		NewExpressionRuleCall(nil, nil, EmptyPlanContext()),
	)
	if !errors.Is(err, errWithQuantifiersProbe) {
		t.Fatalf("pushValuesDefault error = %v, want %v", err, errWithQuantifiersProbe)
	}
	if got != nil {
		t.Fatalf("pushValuesDefault returned %#v after rebuild error, want nil", got)
	}
}

var _ expressions.RelationalExpression = (*failingWithQuantifiersExpression)(nil)
