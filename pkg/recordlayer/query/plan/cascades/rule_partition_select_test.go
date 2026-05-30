package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// aliasSet builds a lowerAliases set from alias names.
func aliasSet(names ...string) map[values.CorrelationIdentifier]struct{} {
	s := make(map[values.CorrelationIdentifier]struct{}, len(names))
	for _, n := range names {
		s[values.NamedCorrelationIdentifier(n)] = struct{}{}
	}
	return s
}

// joinPred builds an equi-predicate `a.col = b.col` whose
// GetCorrelatedToOfPredicate is {a, b} — the shape PartitionSelectRule
// classifies. Each side is a FieldValue over a QuantifiedObjectValue(alias).
func joinPred(a, b string) predicates.QueryPredicate {
	left := values.NewFieldValue(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(a)),
		"col", values.UnknownType,
	)
	right := values.NewFieldValue(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(b)),
		"col", values.UnknownType,
	)
	return predicates.NewComparisonPredicate(
		left,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: right},
	)
}

// TestLowerAliasesConnected pins the union-find connectivity check that gates
// the degenerate cross-product skip in PartitionSelectRule. A disconnected
// lower (no predicate links its quantifiers, e.g. {A,C} for chain A—B—C or
// {XX,YY} for a star) flows a multi-alias RecordConstructorValue the executor
// cannot resolve, so it must be reported as NOT connected → skipped.
func TestLowerAliasesConnected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		aliases    map[values.CorrelationIdentifier]struct{}
		predicates []predicates.QueryPredicate
		want       bool
	}{
		{
			name:    "single alias is trivially connected",
			aliases: aliasSet("A"),
			want:    true,
		},
		{
			name:    "single alias ignores predicates",
			aliases: aliasSet("A"),
			predicates: []predicates.QueryPredicate{
				joinPred("A", "B"), // B not in lower; still connected (size 1)
			},
			want: true,
		},
		{
			name:       "two aliases, no predicate → disconnected",
			aliases:    aliasSet("A", "B"),
			predicates: nil,
			want:       false,
		},
		{
			name:    "two aliases linked by one predicate → connected",
			aliases: aliasSet("A", "B"),
			predicates: []predicates.QueryPredicate{
				joinPred("A", "B"),
			},
			want: true,
		},
		{
			name:    "two aliases, predicate touches only one (spans to upper) → disconnected",
			aliases: aliasSet("A", "C"),
			predicates: []predicates.QueryPredicate{
				// A—B and C—B: each intersects {A,C} in exactly one alias, so
				// neither links A to C. This is the chain A—B—C lower {A,C}.
				joinPred("A", "B"),
				joinPred("C", "B"),
			},
			want: false,
		},
		{
			name:    "three aliases in a chain → connected",
			aliases: aliasSet("A", "B", "C"),
			predicates: []predicates.QueryPredicate{
				joinPred("A", "B"),
				joinPred("B", "C"),
			},
			want: true,
		},
		{
			name:    "three aliases, one isolated → disconnected",
			aliases: aliasSet("A", "B", "C"),
			predicates: []predicates.QueryPredicate{
				joinPred("A", "B"), // C never linked
			},
			want: false,
		},
		{
			name:    "star lower {XX,YY} with hub in upper → disconnected",
			aliases: aliasSet("XX", "YY"),
			predicates: []predicates.QueryPredicate{
				joinPred("HUB", "XX"),
				joinPred("HUB", "YY"),
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := lowerAliasesConnected(tc.aliases, tc.predicates)
			if got != tc.want {
				t.Errorf("lowerAliasesConnected(%v) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
