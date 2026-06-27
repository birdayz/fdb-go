package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// filterOverIntersection builds Filter([P], Intersection(<scans>, keys=keys)).
func filterOverIntersection(p predicates.QueryPredicate, scanNames []string, keys []values.Value) *expressions.LogicalFilterExpression {
	qs := make([]expressions.Quantifier, 0, len(scanNames))
	for _, name := range scanNames {
		scan := expressions.NewFullUnorderedScanExpression([]string{name}, values.UnknownType)
		qs = append(qs, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	}
	x := expressions.NewLogicalIntersectionExpression(qs, keys)
	xQ := expressions.ForEachQuantifier(expressions.InitialOf(x))
	return expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{p}, xQ)
}

func TestPushFilterThroughIntersectionRule_Distributes(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	keys := []values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}}
	src := filterOverIntersection(pT, []string{"A", "B"}, keys)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPushFilterThroughIntersectionRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	newX, ok := yielded[0].(*expressions.LogicalIntersectionExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalIntersectionExpression", yielded[0])
	}
	if got := len(newX.GetQuantifiers()); got != 2 {
		t.Fatalf("intersection has %d children, want 2", got)
	}
	// Comparison keys preserved.
	if got := newX.GetComparisonKeyValues(); len(got) != 1 || got[0] != keys[0] {
		t.Fatalf("comparison keys not preserved: got %v, want %v", got, keys)
	}
	// Each child is a Filter.
	for i, q := range newX.GetQuantifiers() {
		if _, ok := q.GetRangesOver().Get().(*expressions.LogicalFilterExpression); !ok {
			t.Errorf("child %d = %T, want *LogicalFilterExpression", i, q.GetRangesOver().Get())
		}
	}
}

func TestPushFilterThroughIntersectionRule_DeclinesOnNonIntersectionInner(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	src := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, q)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPushFilterThroughIntersectionRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d, want 0", len(yielded))
	}
}

func TestPushFilterThroughIntersectionRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	keys := []values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}}
	src := filterOverIntersection(pT, []string{"A", "B"}, keys)
	ref := expressions.InitialOf(src)
	progress, converged := FixpointApply([]ExpressionRule{NewPushFilterThroughIntersectionRule()}, ref, 50)
	if !converged {
		t.Fatalf("FixpointApply did not converge — progress=%d, members=%d", progress, len(ref.Members()))
	}
}
