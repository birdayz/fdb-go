package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustSmallRewriteConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct small rewrite fixture: " + err.Error())
	}
	return value
}

func smallRewriteRowType() *values.RecordType {
	return values.NewRecordType("SmallRewriteRuleRow", false, []values.Field{{
		Name: "ID", FieldType: values.NotNullLong,
	}})
}

func smallRewriteScan(recordType string) *expressions.FullUnorderedScanExpression {
	return mustSmallRewriteConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{recordType}, smallRewriteRowType()))
}

func smallRewriteLimit(
	limit, offset int64, inner expressions.Quantifier,
) *expressions.LogicalLimitExpression {
	return mustSmallRewriteConstruct(expressions.NewLogicalLimitExpression(limit, offset, inner))
}

func fireSmallRewriteRule(
	t testing.TB, rule ExpressionRule, ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireExpressionRule(rule, ref)
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	return result
}

func TestNoOpLimitElimRule_Fires(t *testing.T) {
	t.Parallel()

	scan := smallRewriteScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	lim := smallRewriteLimit(-1, 0, scanQ)
	ref := expressions.InitialOf(lim)

	rule := NewNoOpLimitElimRule()
	results := fireSmallRewriteRule(t, rule, ref)
	if len(results) == 0 {
		t.Fatal("rule did not fire")
	}
	if _, ok := results[0].(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("expected scan (inner) to be yielded, got %T", results[0])
	}
}

func TestNoOpLimitElimRule_DoesNotFireWithLimit(t *testing.T) {
	t.Parallel()

	scan := smallRewriteScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	lim := smallRewriteLimit(10, 0, scanQ)
	ref := expressions.InitialOf(lim)

	rule := NewNoOpLimitElimRule()
	results := fireSmallRewriteRule(t, rule, ref)
	if len(results) != 0 {
		t.Fatalf("rule should not fire when limit is positive, got %d results", len(results))
	}
}

func TestNoOpLimitElimRule_DoesNotFireWithOffset(t *testing.T) {
	t.Parallel()

	scan := smallRewriteScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	lim := smallRewriteLimit(-1, 5, scanQ)
	ref := expressions.InitialOf(lim)

	rule := NewNoOpLimitElimRule()
	results := fireSmallRewriteRule(t, rule, ref)
	if len(results) != 0 {
		t.Fatalf("rule should not fire when offset>0, got %d results", len(results))
	}
}
