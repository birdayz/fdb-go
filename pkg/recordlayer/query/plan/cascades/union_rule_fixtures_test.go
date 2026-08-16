package cascades

// Fixtures shared by the union implementation tests. They outlived
// ImplementUnionRule, whose file used to hold them: a bare `UNION ALL` is
// implemented by ImplementUnorderedUnionRule alone now (see
// BatchAExpressionRules for why the second rule went), and its tests still
// need the same scan/row shapes.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func unionRuleRowType() *values.RecordType {
	return values.NewRecordType("UnionRow", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "V", FieldType: values.NullableLong},
	})
}

func mustUnionRuleConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct union-rule fixture: " + err.Error())
	}
	return value
}

func mustUnionRuleQOV(value values.Value) values.QuantifiedObjectValue {
	qov, ok := values.AsQuantifiedObjectValue(value)
	if !ok {
		panic("union-rule fixture result is not an exact QOV")
	}
	return qov
}

func unionRuleFullScan(name string) *expressions.FullUnorderedScanExpression {
	return mustUnionRuleConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{name}, unionRuleRowType()))
}

func unionRulePlanScan(name string) *plans.RecordQueryScanPlan {
	return mustUnionRuleConstruct(plans.NewRecordQueryScanPlan(
		[]string{name}, unionRuleRowType(), false))
}

func fireUnionExpressionRule(
	t testing.TB, rule ExpressionRule, ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireExpressionRule(rule, ref)
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	return result
}

func fireUnionImplementationRule(
	t testing.TB, rule ImplementationRule, ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireImplementationRule(rule, ref)
	if err != nil {
		t.Fatalf("FireImplementationRule: %v", err)
	}
	return result
}
