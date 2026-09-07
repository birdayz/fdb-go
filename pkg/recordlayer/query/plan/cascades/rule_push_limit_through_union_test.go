package cascades_test

import (
	"fmt"
	"math"
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

// TestPushLimitThroughUnion_Unbounded never turns a skip-only operator into a
// finite branch cap. A negative limit is a sentinel, not a summand.
func TestPushLimitThroughUnion_Unbounded(t *testing.T) {
	t.Parallel()
	for _, offset := range []int64{0, 1, 2, 5, math.MaxInt64} {
		t.Run(fmt.Sprint(offset), func(t *testing.T) {
			t.Parallel()
			qA := expressions.ForEachQuantifier(expressions.InitialOf(pushLimitUnionScan(t, "A")))
			qB := expressions.ForEachQuantifier(expressions.InitialOf(pushLimitUnionScan(t, "B")))
			unionQ := expressions.ForEachQuantifier(expressions.InitialOf(pushLimitUnion(t, qA, qB)))
			limit := pushLimitUnionLimit(t, -1, offset, unionQ)
			results, err := cascades.FireExpressionRule(cascades.NewPushLimitThroughUnionRule(), expressions.InitialOf(limit))
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 0 {
				t.Fatalf("unbounded LIMIT with OFFSET %d must not cap union branches, got %d rewrites", offset, len(results))
			}
		})
	}
}

func TestPushLimitThroughUnion_Boundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		limit, offset int64
		wantBranch    int64 // negative means no rewrite
	}{
		{name: "maximum", limit: math.MaxInt64 - 1, offset: 1, wantBranch: math.MaxInt64},
		{name: "overflow", limit: math.MaxInt64, offset: 1, wantBranch: -1},
		{name: "zero", limit: 0, offset: 0, wantBranch: -1},
		{name: "zero_with_skip", limit: 0, offset: 2, wantBranch: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			qA := expressions.ForEachQuantifier(expressions.InitialOf(pushLimitUnionScan(t, "A")))
			qB := expressions.ForEachQuantifier(expressions.InitialOf(pushLimitUnionScan(t, "B")))
			unionQ := expressions.ForEachQuantifier(expressions.InitialOf(pushLimitUnion(t, qA, qB)))
			limit := pushLimitUnionLimit(t, tc.limit, tc.offset, unionQ)
			results, err := cascades.FireExpressionRule(cascades.NewPushLimitThroughUnionRule(), expressions.InitialOf(limit))
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantBranch < 0 {
				if len(results) != 0 {
					t.Fatalf("unrepresentable or empty branch cap yielded %d rewrites", len(results))
				}
				return
			}
			if len(results) != 1 {
				t.Fatalf("finite branch cap should yield one rewrite, got %d", len(results))
			}
			outer := results[0].(*expressions.LogicalLimitExpression)
			if outer.GetLimit() != tc.limit || outer.GetOffset() != tc.offset {
				t.Fatalf("outer window changed to %d/%d", outer.GetLimit(), outer.GetOffset())
			}
			union := outer.GetInner().GetRangesOver().Get().(*expressions.LogicalUnionExpression)
			for _, q := range union.GetQuantifiers() {
				branch := q.GetRangesOver().Get().(*expressions.LogicalLimitExpression)
				if branch.GetLimit() != tc.wantBranch || branch.GetOffset() != 0 {
					t.Fatalf("branch window = %d/%d, want %d/0", branch.GetLimit(), branch.GetOffset(), tc.wantBranch)
				}
			}
		})
	}
}

func TestPushLimitThroughUnion_RuntimeCap(t *testing.T) {
	t.Parallel()
	qA := expressions.ForEachQuantifier(expressions.InitialOf(pushLimitUnionScan(t, "A")))
	qB := expressions.ForEachQuantifier(expressions.InitialOf(pushLimitUnionScan(t, "B")))
	unionQ := expressions.ForEachQuantifier(expressions.InitialOf(pushLimitUnion(t, qA, qB)))
	cap := &values.ConstantValue{Value: int64(4), Typ: values.NotNullLong}
	limit, err := expressions.NewRuntimeLogicalLimitExpression(cap, 2, unionQ)
	if err != nil {
		t.Fatal(err)
	}
	results, err := cascades.FireExpressionRule(cascades.NewPushLimitThroughUnionRule(), expressions.InitialOf(limit))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("runtime cap must not be replaced by a static branch limit: %d rewrites", len(results))
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
