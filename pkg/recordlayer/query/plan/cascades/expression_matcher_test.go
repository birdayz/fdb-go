package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
	scan := mustFullUnorderedScan(t, []string{"T"}, values.NotNullLong)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	fValue, err := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	f := mustConstruct(t, fValue, err)
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
	scan := mustFullUnorderedScan(t, []string{"T"}, values.NotNullLong)
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

func TestExpressionMatcher_RootPredicatesComposeWithoutMutatingTheMatcher(t *testing.T) {
	t.Parallel()
	base := NewExpressionMatcher[*expressions.SelectExpression]("select")
	multiple := base.WithRootPredicate(func(sel *expressions.SelectExpression) bool {
		return len(sel.GetQuantifiers()) >= 2
	})
	binary := multiple.WithRootPredicate(func(sel *expressions.SelectExpression) bool {
		return len(sel.GetQuantifiers()) <= 2
	})
	for arity := 1; arity <= 3; arity++ {
		var quantifiers []expressions.Quantifier
		for range arity {
			quantifiers = append(quantifiers, expressions.ForEachQuantifier(expressions.InitialOf(
				mustFullUnorderedScan(t, []string{"T"}, values.NotNullLong),
			)))
		}
		sel, err := expressions.NewSelectExpression(mustOrderedScanFlowed(t, quantifiers[0]), quantifiers, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			matcher *ExpressionMatcher[*expressions.SelectExpression]
			want    bool
		}{
			{base, true},
			{base.WithRootPredicate(nil), true},
			{multiple, arity >= 2},
			{binary, arity == 2},
		} {
			if got := tc.matcher.MatchesRoot(sel); got != tc.want {
				t.Fatalf("arity %d: root admission=%t, want %t", arity, got, tc.want)
			}
			outer := matching.NewBindings()
			bindings := tc.matcher.BindMatches(outer, sel)
			if (len(bindings) == 1) != tc.want {
				t.Fatalf("arity %d: bindings=%d, want admitted=%t", arity, len(bindings), tc.want)
			}
			if len(outer.GetAll(tc.matcher)) != 0 {
				t.Fatal("matching mutated the caller's bindings")
			}
			if tc.want && matching.Get[*expressions.SelectExpression](bindings[0], tc.matcher) != sel {
				t.Fatal("restricted matcher lost its own binding identity")
			}
		}
	}
	for _, input := range []any{nil, "not an expression", mustFullUnorderedScan(t, []string{"T"}, values.NotNullLong)} {
		if binary.MatchesRoot(input) || len(binary.BindMatches(matching.NewBindings(), input)) != 0 {
			t.Fatalf("restricted matcher admitted %T", input)
		}
	}
}

func TestExpressionMatcher_InputPredicatesRemainLiveAndCompose(t *testing.T) {
	t.Parallel()
	scan := mustFullUnorderedScan(t, []string{"T"}, values.NotNullLong)
	child := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(child)
	parent := mustOrderedScanFilter(t, nil, q)
	base := NewExpressionMatcher[*expressions.LogicalFilterExpression]("filter")
	input := base.WithInputPredicate(func(filter *expressions.LogicalFilterExpression) bool {
		return len(filter.GetInner().GetRangesOver().AllMembers()) > 1
	})
	rootPredicate := func(filter *expressions.LogicalFilterExpression) bool { return len(filter.GetPredicates()) != 0 }
	rootThenInput := base.WithRootPredicate(rootPredicate).WithInputPredicate(func(filter *expressions.LogicalFilterExpression) bool {
		return input.MatchesInputs(filter)
	})
	inputThenRoot := input.WithRootPredicate(rootPredicate)
	rejectInput := input.WithInputPredicate(func(*expressions.LogicalFilterExpression) bool { return false })
	for _, grown := range []bool{false, true} {
		if grown {
			child.Insert(mustFullUnorderedScan(t, []string{"OTHER"}, values.NotNullLong))
		}
		for _, tc := range []struct {
			matcher *ExpressionMatcher[*expressions.LogicalFilterExpression]
			want    bool
		}{
			{base, true},
			{input, grown},
			{input.WithRootPredicate(nil), grown},
			{input.WithInputPredicate(nil), grown},
			{rootThenInput, false},
			{inputThenRoot, false},
			{rejectInput, false},
		} {
			bindings := tc.matcher.BindMatches(matching.NewBindings(), parent)
			if (len(bindings) == 1) != tc.want {
				t.Fatalf("grown=%t: bindings=%d, want admitted=%t", grown, len(bindings), tc.want)
			}
			if tc.want && bindings[0].Get(tc.matcher) != parent {
				t.Fatal("composed matcher lost binding identity")
			}
		}
		if !input.MatchesRoot(parent) {
			t.Fatal("mutable input predicate leaked into immutable root admission")
		}
	}
	for _, wrong := range []any{nil, "not an expression", scan} {
		if input.MatchesInputs(wrong) || len(input.BindMatches(matching.NewBindings(), wrong)) != 0 {
			t.Fatalf("input matcher admitted %T", wrong)
		}
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
