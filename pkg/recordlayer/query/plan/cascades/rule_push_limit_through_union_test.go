package cascades_test

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func pushLimitUnionRowType() *values.RecordType {
	return values.NewRecordType("PUSH_LIMIT_UNION_ROW", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
}

func mustPushLimitUnionConstruct[T any](t testing.TB, value T, err error) T {
	t.Helper()
	if err != nil {
		t.Fatalf("construct push-limit-through-union fixture: %v", err)
	}
	return value
}

func pushLimitUnionScan(t testing.TB, recordType string) *expressions.FullUnorderedScanExpression {
	t.Helper()
	scan, err := expressions.NewFullUnorderedScanExpression(
		[]string{recordType}, pushLimitUnionRowType())
	return mustPushLimitUnionConstruct(t, scan, err)
}

func pushLimitUnion(
	t testing.TB,
	quantifiers ...expressions.Quantifier,
) *expressions.LogicalUnionExpression {
	t.Helper()
	union, err := expressions.NewLogicalUnionExpression(quantifiers)
	return mustPushLimitUnionConstruct(t, union, err)
}

func pushLimitUnionLimit(
	t testing.TB,
	limit, offset int64,
	inner expressions.Quantifier,
) *expressions.LogicalLimitExpression {
	t.Helper()
	limitExpr, err := expressions.NewLogicalLimitExpression(limit, offset, inner)
	return mustPushLimitUnionConstruct(t, limitExpr, err)
}

func TestPushLimitThroughUnion(t *testing.T) {
	t.Parallel()

	scanA := pushLimitUnionScan(t, "A")
	scanB := pushLimitUnionScan(t, "B")
	qA := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	qB := expressions.ForEachQuantifier(expressions.InitialOf(scanB))

	union := pushLimitUnion(t, qA, qB)
	unionRef := expressions.InitialOf(union)
	unionQ := expressions.ForEachQuantifier(unionRef)

	limit := pushLimitUnionLimit(t, 10, 5, unionQ)
	ref := expressions.InitialOf(limit)

	rule := cascades.NewPushLimitThroughUnionRule()
	results, err := cascades.FireExpressionRule(rule, ref)
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	outerLimit, ok := results[0].(*expressions.LogicalLimitExpression)
	if !ok {
		t.Fatalf("result is %T, want *LogicalLimitExpression", results[0])
	}
	if outerLimit.GetLimit() != 10 || outerLimit.GetOffset() != 5 {
		t.Fatalf("outer limit = %d/%d, want 10/5", outerLimit.GetLimit(), outerLimit.GetOffset())
	}

	innerExpr := outerLimit.GetInner().GetRangesOver().Get()
	innerUnion, ok := innerExpr.(*expressions.LogicalUnionExpression)
	if !ok {
		t.Fatalf("inner is %T, want *LogicalUnionExpression", innerExpr)
	}

	for i, q := range innerUnion.GetQuantifiers() {
		branchExpr := q.GetRangesOver().Get()
		branchLimit, ok := branchExpr.(*expressions.LogicalLimitExpression)
		if !ok {
			t.Fatalf("branch %d is %T, want *LogicalLimitExpression", i, branchExpr)
		}
		if branchLimit.GetLimit() != 15 {
			t.Fatalf("branch %d limit = %d, want 15 (10+5)", i, branchLimit.GetLimit())
		}
		if branchLimit.GetOffset() != 0 {
			t.Fatalf("branch %d offset = %d, want 0", i, branchLimit.GetOffset())
		}
	}
}

func TestPushLimitThroughUnion_NoOffset(t *testing.T) {
	t.Parallel()

	scanA := pushLimitUnionScan(t, "A")
	scanB := pushLimitUnionScan(t, "B")
	qA := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	qB := expressions.ForEachQuantifier(expressions.InitialOf(scanB))

	union := pushLimitUnion(t, qA, qB)
	unionRef := expressions.InitialOf(union)
	unionQ := expressions.ForEachQuantifier(unionRef)

	limit := pushLimitUnionLimit(t, 10, 0, unionQ)
	ref := expressions.InitialOf(limit)

	rule := cascades.NewPushLimitThroughUnionRule()
	results, err := cascades.FireExpressionRule(rule, ref)
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	outerLimit := results[0].(*expressions.LogicalLimitExpression)
	innerUnion := outerLimit.GetInner().GetRangesOver().Get().(*expressions.LogicalUnionExpression)
	for i, q := range innerUnion.GetQuantifiers() {
		branchLimit := q.GetRangesOver().Get().(*expressions.LogicalLimitExpression)
		if branchLimit.GetLimit() != 10 {
			t.Fatalf("branch %d limit = %d, want 10", i, branchLimit.GetLimit())
		}
	}
}

func TestPushLimitThroughUnion_NotUnion(t *testing.T) {
	t.Parallel()

	scan := pushLimitUnionScan(t, "T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	limit := pushLimitUnionLimit(t, 10, 0, scanQ)
	ref := expressions.InitialOf(limit)

	rule := cascades.NewPushLimitThroughUnionRule()
	results, err := cascades.FireExpressionRule(rule, ref)
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for non-union, got %d", len(results))
	}
}
