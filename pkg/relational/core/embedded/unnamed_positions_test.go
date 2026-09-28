package embedded

import (
	"slices"
	"testing"

	"fdb.dev/pkg/relational/core/query/semantic"
)

// unnamedFromLegs and unnamedBeforeOn place Java's unnamed operators by FROM
// position (0 = the primary source). Each case names the query whose answer the
// target measured in conformance/ws_f_table_qualifier_conformance_test.go.
func TestUnnamedPositions(t *testing.T) {
	t.Parallel()
	inner, left, right := joinClause{joinType: joinTypeInner}, joinClause{joinType: joinTypeLeft}, joinClause{joinType: joinTypeRight}
	for _, tc := range []struct {
		name  string
		joins []joinClause
		after int   // positions unnamed for the clauses after the FROM
		on    []int // positions unnamed for each join's ON
	}{
		// FROM x JOIN h ON …: every clause sees named sources.
		{"inner", []joinClause{inner}, 0, []int{0}},
		// FROM x LEFT JOIN h ON x.f = h.id resolves (its ON sees x named);
		// SELECT x.f FROM x LEFT JOIN h ON … is 42702 (x and h unnamed after).
		{"left", []joinClause{left}, 2, []int{0}},
		{"right", []joinClause{right}, 2, []int{0}},
		// FROM x JOIN y ON … LEFT JOIN h ON x.f = …: 42702, the two sources
		// before the outer join are collapsed for its ON.
		{"inner then left", []joinClause{inner, left}, 3, []int{0, 2}},
		// FROM x LEFT JOIN y ON … LEFT JOIN h ON x.f = …: 42702.
		{"left then left", []joinClause{left, left}, 3, []int{0, 2}},
		// FROM x LEFT JOIN y ON … JOIN h ON x.f = …: 42702, the inner join's
		// ON sees the outer join's operator; h, joined after it, stays named.
		{"left then inner", []joinClause{left, inner}, 2, []int{0, 2}},
	} {
		if got := unnamedFromLegs(tc.joins); got != tc.after {
			t.Errorf("%s: unnamedFromLegs = %d, want %d", tc.name, got, tc.after)
		}
		for at, want := range tc.on {
			if got := unnamedBeforeOn(tc.joins, at); got != want {
				t.Errorf("%s: unnamedBeforeOn(%d) = %d, want %d", tc.name, at, got, want)
			}
		}
	}
}

// withUnnamedFromLegs marks a one-source-per-position scope and leaves any
// other scope as it was.
func TestWithUnnamedFromLegs(t *testing.T) {
	t.Parallel()
	build := func(n int) *semantic.Scope {
		scope := semantic.NewScope(nil)
		for i := 0; i < n; i++ {
			name := string(rune('A' + i))
			src := semantic.ScopeSource{
				Table:           &semantic.StaticTable{TableName: semantic.ParseQualifiedName(name, false)},
				Alias:           semantic.NewUnquoted(name),
				CorrelationName: name,
			}
			if err := scope.AddSource(src); err != nil {
				t.Fatal(err)
			}
		}
		return scope
	}
	unnamed := func(scope *semantic.Scope, ok bool) []bool {
		if !ok {
			return nil
		}
		out := []bool{}
		for _, src := range scope.Sources() {
			out = append(out, src.Unnamed)
		}
		return out
	}
	left, inner := joinClause{joinType: joinTypeLeft}, joinClause{joinType: joinTypeInner}
	if got := unnamed(withUnnamedFromLegs(build(3), []joinClause{left, inner})); !slices.Equal(got, []bool{true, true, false}) {
		t.Errorf("x LEFT JOIN y JOIN h: %v, want the outer join's two sources unnamed", got)
	}
	if got := unnamed(withUnnamedFromLegs(build(2), []joinClause{inner})); !slices.Equal(got, []bool{false, false}) {
		t.Errorf("x JOIN y: %v, want none unnamed", got)
	}
	// A scope that does not hold one source per position (a builder that
	// skipped or doubled one) is refused, not re-marked by guesswork.
	if got, ok := withUnnamedFromLegs(build(3), []joinClause{left}); ok || got != nil {
		t.Errorf("mismatched shape: %v, %v; want the bridge to refuse", got, ok)
	}
}
