package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestFinalizeExpressionsRule_PromotesMatchedExpression(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	ref := expressions.InitialOf(scan)

	yielded := FireImplementationRule(NewFinalizeExpressionsRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("expected one promoted expression, got %d", len(yielded))
	}
	if yielded[0] != scan {
		t.Fatalf("promoted %T, want the exact matched scan", yielded[0])
	}
	finals := ref.FinalMembers()
	if len(finals) != 1 || finals[0] != scan {
		t.Fatalf("final members = %#v, want the exact matched scan", finals)
	}
}
