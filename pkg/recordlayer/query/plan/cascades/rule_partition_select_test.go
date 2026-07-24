package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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

// scanQuantifier builds a named ForEach quantifier over a fresh base scan,
// standing in for a SQL table source aliased `name`.
func scanQuantifier(name string) expressions.Quantifier {
	scan := &expressions.FullUnorderedScanExpression{}
	tf := expressions.NewLogicalTypeFilterExpression([]string{strings.ToUpper(name)}, pbForEachOf(scan))
	return expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(name),
		expressions.InitialOf(tf),
	)
}

func TestPartitionSelect_StrictSingleFailsClosed(t *testing.T) {
	t.Parallel()

	a := scanQuantifier("A")
	bBase := scanQuantifier("B")
	b := expressions.NamedForEachStrictSingleQuantifier(
		bBase.GetAlias(), bBase.GetRangesOver())
	c := scanQuantifier("C")
	sel := expressions.NewSelectExpressionWithAliases(
		a.GetFlowedObjectValue(),
		[]expressions.Quantifier{a, b, c},
		[]predicates.QueryPredicate{joinPred("A", "B"), joinPred("B", "C")},
		[]string{"A", "B", "C"},
	)

	yielded := FireExpressionRule(NewPartitionSelectRule(), expressions.InitialOf(sel))
	if len(yielded) != 0 {
		t.Fatalf("N-way strict-single select yielded %d partition(s), want fail-closed zero", len(yielded))
	}
}

// chainEqPred builds the join predicate `a.aCol = b.bCol` as QOV-rooted
// FieldValues, so GetCorrelatedToOfPredicate = {a, b} — the spanning shape
// PartitionSelectRule routes to the upper level.
func chainEqPred(a, aCol, b, bCol string) predicates.QueryPredicate {
	return predicates.NewComparisonPredicate(
		values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(a)), aCol, values.UnknownType),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(b)), bCol, values.UnknownType),
		},
	)
}

// TestMergeQuantifierAlias_Injective REMOVED (RFC-077 7.5): the synthetic
// stable merge-alias encoding (mergeQuantifierAlias) it pinned is gone — merge
// quantifiers now carry a plain uniqueId and intern STRUCTURALLY via alias-aware
// Reference.Insert/InsertFinal (MemoEqual). The interning the alias encoding
// protected is now pinned by the chain task-count gate
// (TestPartitionSelect_ChainInterningBaseline) instead — a far stronger probe
// (it measures the actual exploration sharing, not a string-encoding property).

// orPred builds `a.col = b.col OR a.col = c.col` — one N-ary conjunct whose
// GetCorrelatedToOfPredicate is {a, b, c}.
func orPred(a, b, c string) predicates.QueryPredicate {
	return predicates.NewOr(joinPred(a, b), joinPred(a, c))
}

// TestAliasesConnectedByPredicates pins the union-find connectivity check that
// gates the disconnected-lower skip in PartitionSelectRule: judged over the
// select's FULL conjunct list, so a spanning N-ary predicate connects the
// aliases it touches (a lower-resident-only reading starved
// `WHERE a.x = b.y OR a.x = c.y` of every bipartition → 0AF00), while a
// genuinely predicate-less pair stays disconnected (the chain {A,C} /
// star {XX,YY} pruning that holds the task baseline).
func TestAliasesConnectedByPredicates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		aliases    map[values.CorrelationIdentifier]struct{}
		predicates []predicates.QueryPredicate
		want       bool
	}{
		{"single alias is trivially connected", aliasSet("A"), nil, true},
		{"two aliases, no predicate", aliasSet("A", "B"), nil, false},
		{
			"two aliases linked by one predicate", aliasSet("A", "B"),
			[]predicates.QueryPredicate{joinPred("A", "B")},
			true,
		},
		{
			"chain lower {A,C}: binary preds touch one each", aliasSet("A", "C"),
			[]predicates.QueryPredicate{joinPred("A", "B"), joinPred("C", "B")},
			false,
		},
		{
			"three aliases in a chain", aliasSet("A", "B", "C"),
			[]predicates.QueryPredicate{joinPred("A", "B"), joinPred("B", "C")},
			true,
		},
		{
			"three aliases, one isolated", aliasSet("A", "B", "C"),
			[]predicates.QueryPredicate{joinPred("A", "B")},
			false,
		},
		{
			"star lower {XX,YY} with hub upper", aliasSet("XX", "YY"),
			[]predicates.QueryPredicate{joinPred("HUB", "XX"), joinPred("HUB", "YY")},
			false,
		},
		{
			"spanning OR connects the pair it touches", aliasSet("B", "C"),
			[]predicates.QueryPredicate{orPred("A", "B", "C")},
			true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := aliasesConnectedByPredicates(tc.aliases, tc.predicates)
			if got != tc.want {
				t.Errorf("aliasesConnectedByPredicates(%v) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestTransitiveCorrelationOrder_RangesOverEdges pins the recovered
// quantifier→sibling correlation edges: Go's Quantifier.GetCorrelatedTo is
// empty (registered divergence), so computeTransitiveCorrelationOrder must
// read the rangesOver Reference's transitive correlation set — a lateral
// unnest's Explode correlates to its array source with NO predicate between
// them, and without this edge every bipartition check (components, cycle,
// lower-depends-on-upper) treats the pair as independent: the cross-product
// paths then tear them apart and the unnest's AS/AT columns silently NULL.
func TestTransitiveCorrelationOrder_RangesOverEdges(t *testing.T) {
	t.Parallel()
	src := scanQuantifier("PB")
	// A quantifier whose EXPRESSION references PB — the Explode shape (any
	// correlated member works; a filter carrying a QOV(PB) predicate is the
	// simplest constructible stand-in).
	correlated := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{joinPred("PB", "X")},
		pbForEachOf(&expressions.FullUnorderedScanExpression{}),
	)
	x := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("X"),
		expressions.InitialOf(correlated),
	)

	order := computeTransitiveCorrelationOrder([]expressions.Quantifier{src, x})
	if _, ok := order[x.GetAlias()][src.GetAlias()]; !ok {
		t.Fatalf("X's rangesOver correlation to PB missing from the correlation order: %v", order[x.GetAlias()])
	}
	if len(order[src.GetAlias()]) != 0 {
		t.Fatalf("PB must not depend on X: %v", order[src.GetAlias()])
	}
}

// TestBoundAliasesOfReference pins the buried-alias collector the partition
// classifier keys on: every quantifier alias bound anywhere inside the
// reference's subgraph is reported — including aliases
// nested one level down — so a predicate referencing a subquery-INTERNAL
// alias (an existential's hoisted join predicate, `B2.A_ID = A.ID`) is
// classified as correlated to the existential quantifier that owns it, never
// sunk into the outer's partition half where the alias can never bind.
func TestBoundAliasesOfReference(t *testing.T) {
	t.Parallel()

	inner := expressions.NewSelectExpressionWithAliases(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("B2")),
		[]expressions.Quantifier{
			expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("B2"),
				expressions.InitialOf(&expressions.FullUnorderedScanExpression{})),
			expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("C"),
				expressions.InitialOf(&expressions.FullUnorderedScanExpression{})),
		},
		nil, nil)
	outer := expressions.NewSelectExpressionWithAliases(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("Q")),
		[]expressions.Quantifier{
			expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("Q"),
				expressions.InitialOf(inner)),
		},
		nil, nil)

	got := boundAliasesOfReference(expressions.InitialOf(outer))
	for _, want := range []string{"Q", "B2", "C"} {
		if _, ok := got[values.NamedCorrelationIdentifier(want)]; !ok {
			t.Fatalf("bound aliases missing %s (got %v)", want, got)
		}
	}
}
