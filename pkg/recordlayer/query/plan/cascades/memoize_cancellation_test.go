package cascades

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// After a cancel, each memoize entry point leaves the memo alone: four calls,
// cancelled after the second, add two references.
func TestMemoizeAfterCancellationLeavesTheMemoAlone(t *testing.T) {
	t.Parallel()

	scanNamed := func(i int) expressions.RelationalExpression {
		return mustFullUnorderedScan(t, []string{fmt.Sprintf("T%d", i)}, compensationRowType("ID"))
	}
	physicalNamed := func(i int) expressions.RelationalExpression {
		plan, err := plans.NewRecordQueryScanPlan([]string{fmt.Sprintf("T%d", i)}, compensationRowType("ID"), false)
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	type memoizeFunc func(ctx context.Context, memo *Memo, root *expressions.Reference, i int)
	for _, tc := range []struct {
		name    string
		memoize memoizeFunc
	}{
		{
			name: "expression call MemoizeExpression",
			memoize: func(ctx context.Context, memo *Memo, root *expressions.Reference, i int) {
				(&ExpressionRuleCall{Reference: root, RunContext: ctx, memo: memo}).MemoizeExpression(scanNamed(i))
			},
		},
		{
			name: "expression call MemoizeExpressions",
			memoize: func(ctx context.Context, memo *Memo, root *expressions.Reference, i int) {
				(&ExpressionRuleCall{Reference: root, RunContext: ctx, memo: memo}).MemoizeExpressions(
					[]expressions.RelationalExpression{scanNamed(i), scanNamed(100 + i)},
				)
			},
		},
		{
			name: "expression call MemoizeUnknownExpression, physical",
			memoize: func(ctx context.Context, memo *Memo, root *expressions.Reference, i int) {
				(&ExpressionRuleCall{Reference: root, RunContext: ctx, memo: memo}).MemoizeUnknownExpression(physicalNamed(i))
			},
		},
		{
			name: "implementation call MemoizeUnknownExpression",
			memoize: func(ctx context.Context, memo *Memo, root *expressions.Reference, i int) {
				(&ImplementationRuleCall{Reference: root, RunContext: ctx, memo: memo}).MemoizeUnknownExpression(scanNamed(i))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := expressions.InitialOf(scanNamed(0))
			memo := NewMemo(root)
			ctx, cancel := context.WithCancel(context.Background())
			before := len(memo.References())
			for i := 1; i <= 4; i++ {
				if i == 3 {
					cancel()
				}
				tc.memoize(ctx, memo, root, i)
			}
			cancel()
			if got := len(memo.References()) - before; got != 2 {
				t.Fatalf("memo gained %d references, want 2 (the memoize calls before the cancel)", got)
			}
		})
	}
}
