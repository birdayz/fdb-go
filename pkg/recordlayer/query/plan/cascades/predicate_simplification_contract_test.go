package cascades

import (
	"errors"
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func simplificationContractLeaves(t testing.TB) [8]predicates.QueryPredicate {
	t.Helper()
	var leaves [8]predicates.QueryPredicate
	for i, name := range []string{"p", "q", "r", "s", "t", "u", "v", "w"} {
		root, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(name), values.NotNullLong)
		if err != nil {
			t.Fatal(err)
		}
		leaves[i] = predicates.NewComparisonPredicate(root,
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))
	}
	return leaves
}

func assertSimplificationTree(t testing.TB, got, want predicates.QueryPredicate) {
	t.Helper()
	if !predicates.PredicateEquals(got, want) || predicates.IsAtomic(got) != predicates.IsAtomic(want) {
		t.Fatalf("got %s (atomic=%t), want %s (atomic=%t)", got.Explain(), predicates.IsAtomic(got), want.Explain(), predicates.IsAtomic(want))
	}
	gotChildren, wantChildren := got.Children(), want.Children()
	if len(gotChildren) != len(wantChildren) {
		t.Fatalf("children=%d, want %d", len(gotChildren), len(wantChildren))
	}
	for i, child := range gotChildren {
		assertSimplificationTree(t, child, wantChildren[i])
	}
}

// These ordered trees are also asserted by //conformance:predicate_simplification_test.
func TestPredicateUnionDNFSimplificationContract(t *testing.T) {
	t.Parallel()
	leaves := simplificationContractLeaves(t)
	p, q, r, s, u, v, w := leaves[0], leaves[1], leaves[2], leaves[3], leaves[5], leaves[6], leaves[7]
	and, or := predicates.NewAnd, predicates.NewOr
	for _, tc := range []struct {
		name    string
		factors []predicates.QueryPredicate
		want    []predicates.QueryPredicate
	}{
		{
			name: "children_before_distribution",
			factors: []predicates.QueryPredicate{
				or(predicates.WithAtomicity(and(p, q), true), and(p, q, r)), or(s, leaves[4]),
			},
			want: []predicates.QueryPredicate{and(p, q, s), and(p, q, leaves[4])},
		},
		{
			name: "redistribute_after_absorption",
			factors: []predicates.QueryPredicate{
				predicates.WithAtomicity(or(p, q), true), predicates.WithAtomicity(or(r, s), true), p,
			},
			want: []predicates.QueryPredicate{and(r, p), and(s, p)},
		},
		{
			name:    "last_duplicate_position",
			factors: []predicates.QueryPredicate{or(p, u), or(q, v), or(p, w)},
			want:    []predicates.QueryPredicate{and(q, p), and(v, p), and(u, q, w), and(u, v, w)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := predicateUnionDNFTerms(tc.factors)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("DNF terms=%d, want %d: %v", len(got), len(tc.want), got)
			}
			for i, term := range got {
				assertSimplificationTree(t, term, tc.want[i])
			}
		})
	}
}

func TestPredicateUnionNineFactorPopulationMatchesJava(t *testing.T) {
	t.Parallel()
	leaves := simplificationContractLeaves(t)
	p, q, r, s, u, v := leaves[0], leaves[1], leaves[2], leaves[3], leaves[5], leaves[6]
	or := predicates.NewOr
	factors := []predicates.QueryPredicate{
		or(p, s, leaves[4]), or(q, s, leaves[4]), or(r, s, leaves[4]),
		or(p, u), or(q, u), or(r, u),
		or(p, v), or(q, v), or(r, v),
	}
	_, ref := makeSelectWithOrPredicates(factors)
	yielded := mustExplorePredicateUnion(t, ref)
	if len(yielded) != 511 {
		t.Fatalf("union choices=%d, want Java's 511", len(yielded))
	}
	legs := 0
	for _, expression := range yielded {
		distinct := expression.(*expressions.LogicalUniqueExpression)
		union := distinct.GetInner().GetRangesOver().Get().(*expressions.LogicalUnionExpression)
		legs += len(union.GetQuantifiers())
	}
	if legs != 2898 {
		t.Fatalf("union legs=%d, want Java's 2898 across all 511 choices", legs)
	}
}

type simplificationContractBindings map[values.CorrelationIdentifier]any

func (b simplificationContractBindings) GetCorrelationBinding(alias values.CorrelationIdentifier) (any, bool) {
	value, ok := b[alias]
	return value, ok
}

func TestPredicateUnionNineFactorSemanticPopulation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		withOverflow bool
		assignments  int
		functions    int
		outcomes     int
	}{
		{"three_valued", false, 2187, 72, 3},
		// A leg evaluates only its term and the fixed factors the term does not
		// imply, so observing overflow separates no further legs.
		{"overflow_sensitive", true, 2916, 72, 4},
	} {
		withOverflow := tc.withOverflow
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			leaves := simplificationContractLeaves(t)
			if withOverflow {
				operand := leaves[6].(*predicates.ComparisonPredicate).Operand
				leaves[6] = predicates.NewComparisonPredicate(
					values.NewScalarFunctionValue("ABS", values.NullableLong, operand),
					predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))
			}
			p, q, r, s, u, v := leaves[0], leaves[1], leaves[2], leaves[3], leaves[5], leaves[6]
			or := predicates.NewOr
			_, ref := makeSelectWithOrPredicates([]predicates.QueryPredicate{
				or(p, s, leaves[4]), or(q, s, leaves[4]), or(r, s, leaves[4]),
				or(p, u), or(q, u), or(r, u),
				or(p, v), or(q, v), or(r, v),
			})
			unions := mustExplorePredicateUnion(t, ref)
			if len(unions) != 511 {
				t.Fatalf("union choices=%d, want 511", len(unions))
			}
			var legs []predicates.QueryPredicate
			for _, expression := range unions {
				distinct := expression.(*expressions.LogicalUniqueExpression)
				union := distinct.GetInner().GetRangesOver().Get().(*expressions.LogicalUnionExpression)
				for _, quantifier := range union.GetQuantifiers() {
					unique := quantifier.GetRangesOver().Get().(*expressions.LogicalUniqueExpression)
					leg := unique.GetInner().GetRangesOver().Get().(*expressions.SelectExpression)
					residuals, err := predicates.ToResidualPredicates(leg.GetPredicates())
					if err != nil {
						t.Fatal(err)
					}
					legs = append(legs, predicates.NewAnd(residuals...))
				}
			}
			if len(legs) != 2898 {
				t.Fatalf("union legs=%d, want 2898", len(legs))
			}

			var rows []simplificationContractBindings
			for ordinal := range 2187 {
				row := make(simplificationContractBindings)
				remaining := ordinal
				for _, name := range []string{"p", "q", "r", "s", "t", "u", "v"} {
					row[values.NamedCorrelationIdentifier(name)] = []any{int64(0), int64(1), nil}[remaining%3]
					remaining /= 3
				}
				rows = append(rows, row)
			}
			if withOverflow {
				for _, original := range rows[:729] {
					row := make(simplificationContractBindings)
					for alias, value := range original {
						row[alias] = value
					}
					row[values.NamedCorrelationIdentifier("v")] = int64(math.MinInt64)
					rows = append(rows, row)
				}
			}
			functions := make(map[string]struct{})
			outcomes := make(map[byte]int)
			for _, leg := range legs {
				signature := make([]byte, len(rows))
				for i, row := range rows {
					result, err := leg.Eval(row)
					if err != nil {
						var overflow *values.ArithmeticOverflowError
						if !withOverflow || !errors.As(err, &overflow) {
							t.Fatal(err)
						}
						signature[i] = 3
					} else if result == nil {
						signature[i] = 2
					} else if *result {
						signature[i] = 1
					}
					outcomes[signature[i]]++
				}
				functions[string(signature)] = struct{}{}
			}
			t.Logf("unions=%d ordered legs=%d assignments=%d evaluator signatures=%d outcomes=%v", len(unions), len(legs), len(rows), len(functions), outcomes)
			if len(rows) != tc.assignments || len(outcomes) != tc.outcomes {
				t.Fatalf("incomplete evaluator domain: assignments=%d outcomes=%v", len(rows), outcomes)
			}
			if len(functions) != tc.functions {
				t.Fatalf("evaluator signatures=%d, want %d", len(functions), tc.functions)
			}
		})
	}
}

func TestPredicateDNFOnlyNormalizesRoot(t *testing.T) {
	t.Parallel()
	p := simplificationContractLeaves(t)
	input := predicates.WithAtomicity(predicates.NewAnd(p[0], predicates.NewAnd(predicates.NewOr(p[1], p[2]), p[3])), true)
	rules := append([]CascadesRule{newPredicateDNFRule()}, queryPredicateSimplificationRules()...)
	got, err := Simplify(input, rules)
	if err != nil {
		t.Fatal(err)
	}
	assertSimplificationTree(t, got, input)
	if got != input {
		t.Fatal("root-only normalization rewrote an already-simplified atomic tree")
	}
}

func TestSimplificationAtomicContract(t *testing.T) {
	t.Parallel()
	leaves := simplificationContractLeaves(t)
	p, q := leaves[0], leaves[1]
	negate := func(p predicates.QueryPredicate) predicates.QueryPredicate {
		cp := p.(*predicates.ComparisonPredicate)
		return predicates.NewComparisonPredicate(cp.Operand,
			predicates.NewLiteralComparison(predicates.ComparisonNotEquals, int64(1)))
	}
	for _, tc := range []struct {
		name  string
		input predicates.QueryPredicate
		want  predicates.QueryPredicate
	}{
		{"simplify_atomic_root", predicates.WithAtomicity(predicates.NewAnd(p, p), true), p},
		{
			"preserve_atomicity_when_rebuilding_children",
			predicates.WithAtomicity(predicates.NewAnd(predicates.NewOr(p, predicates.NewConstantPredicate(predicates.TriFalse)), q), true),
			predicates.WithAtomicity(predicates.NewAnd(p, q), true),
		},
		{
			"demorgan_atomic_child",
			predicates.NewNot(predicates.WithAtomicity(predicates.NewAnd(p, q), true)),
			predicates.NewOr(negate(p), negate(q)),
		},
		{
			"default_demorgan",
			predicates.NewNot(predicates.NewOr(p, q)),
			predicates.NewAnd(negate(p), negate(q)),
		},
		{
			"default_nested_connective",
			predicates.NewAnd(p, predicates.NewAnd(q, leaves[2])),
			predicates.NewAnd(p, predicates.NewAnd(q, leaves[2])),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Simplify(tc.input, queryPredicateSimplificationRules())
			if err != nil {
				t.Fatal(err)
			}
			assertSimplificationTree(t, got, tc.want)
		})
	}
}
