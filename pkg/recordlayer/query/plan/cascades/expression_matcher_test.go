package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestExpressionMatcher_RootType(t *testing.T) {
	t.Parallel()
	m := NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter")
	if got := m.RootType(); got != "logical_filter" {
		t.Fatalf("RootType=%q, want logical_filter", got)
	}
}

func TestExpressionMatcher_BindMatches_Hit(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	f := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	m := NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter")
	matches := m.BindMatches(matching.NewBindings(), f)
	if len(matches) != 1 {
		t.Fatalf("matches=%d, want 1", len(matches))
	}
	// Verify the binding maps the matcher to the expression.
	got := matching.Get[*expressions.LogicalFilterExpression](matches[0], m)
	if got != f {
		t.Fatalf("Get returned %v, want %v", got, f)
	}
}

func TestExpressionMatcher_BindMatches_Miss(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	// Matcher for LogicalFilter receiving a Scan — should miss.
	m := NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter")
	matches := m.BindMatches(matching.NewBindings(), scan)
	if len(matches) != 0 {
		t.Fatalf("matcher matched on wrong type — matches=%d, want 0", len(matches))
	}
}

func TestExpressionMatcher_DistinctInstances(t *testing.T) {
	t.Parallel()
	// Each constructor call returns a distinct allocation —
	// pointer-identity comparison stays distinct.
	m1 := NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter")
	m2 := NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter")
	if m1 == m2 {
		t.Fatal("two ExpressionMatcher constructions returned the same pointer — bindings would collide")
	}
}

func TestExpressionMatcher_BindMatches_NonExpression(t *testing.T) {
	t.Parallel()
	// Passing a non-RelationalExpression must not match.
	m := NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter")
	matches := m.BindMatches(matching.NewBindings(), "not an expression")
	if len(matches) != 0 {
		t.Fatalf("matched on non-expression input — matches=%d, want 0", len(matches))
	}
}
