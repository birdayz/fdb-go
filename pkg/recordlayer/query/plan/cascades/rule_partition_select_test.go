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

func mustPartitionConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct partition-select fixture: " + err.Error())
	}
	return value
}

func partitionSelectRowType(name string) *values.RecordType {
	return values.NewRecordType(name, false, []values.Field{
		{Name: "col", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "NEXT_ID", FieldType: values.NotNullLong, Ordinal: 2},
	})
}

func partitionField(alias, name string) values.Value {
	rowType := partitionSelectRowType(alias)
	root := mustPartitionConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier(alias), rowType))
	request := mustPartitionConstruct(values.FieldByName(name))
	return mustPartitionConstruct(values.ResolveFieldAccess(
		root, []values.FieldRequest{request}))
}

// joinPred builds an equi-predicate `a.col = b.col` whose
// GetCorrelatedToOfPredicate is {a, b} — the shape PartitionSelectRule
// classifies. Each side is a FieldValue over a QuantifiedObjectValue(alias).
func joinPred(a, b string) predicates.QueryPredicate {
	return predicates.NewComparisonPredicate(
		partitionField(a, "col"),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: partitionField(b, "col"),
		},
	)
}

// scanQuantifier builds a named ForEach quantifier over a fresh base scan,
// standing in for a SQL table source aliased `name`.
func scanQuantifier(name string) expressions.Quantifier {
	return typedPartitionScanQuantifier(name, partitionSelectRowType(name))
}

func typedPartitionScanQuantifier(name string, rowType values.Type) expressions.Quantifier {
	scan := mustPartitionConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{strings.ToUpper(name)}, rowType))
	tf := mustPartitionConstruct(expressions.NewLogicalTypeFilterExpression(
		[]string{strings.ToUpper(name)}, expressions.ForEachQuantifier(expressions.InitialOf(scan))))
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
	result := mustPartitionConstruct(a.RequireFlowedObjectValue())
	sel := mustPartitionConstruct(expressions.NewSelectExpressionWithAliases(
		result,
		[]expressions.Quantifier{a, b, c},
		[]predicates.QueryPredicate{joinPred("A", "B"), joinPred("B", "C")},
		[]string{"A", "B", "C"},
	))

	yielded := mustFireExpressionRule(t, NewPartitionSelectRule(), expressions.InitialOf(sel))
	if len(yielded) != 0 {
		t.Fatalf("N-way strict-single select yielded %d partition(s), want fail-closed zero", len(yielded))
	}
}

// TestPartitionSelect_OuterJoinFailsClosed pins the ChildrenAsSet() guard: a
// >=3-quantifier select carrying JoinLeftOuter must never be bipartitioned.
// Both halves NewSelectExpressionWithAliases builds default to JoinInner
// (it has no joinType parameter), so partitioning an OUTER select would
// silently erase its null-extension semantics — the same shape this select
// would happily partition on (a two-link chain A-B-C) if it were JoinInner.
func TestPartitionSelect_OuterJoinFailsClosed(t *testing.T) {
	t.Parallel()

	a := scanQuantifier("A")
	b := scanQuantifier("B")
	c := scanQuantifier("C")
	result := mustPartitionConstruct(a.RequireFlowedObjectValue())
	sel := mustPartitionConstruct(expressions.NewSelectExpressionWithJoinType(
		result,
		[]expressions.Quantifier{a, b, c},
		[]predicates.QueryPredicate{joinPred("A", "B"), joinPred("B", "C")},
		[]string{"A", "B", "C"},
		expressions.JoinLeftOuter,
	))

	yielded := mustFireExpressionRule(t, NewPartitionSelectRule(), expressions.InitialOf(sel))
	if len(yielded) != 0 {
		t.Fatalf("LEFT OUTER N-way select yielded %d partition(s), want fail-closed zero", len(yielded))
	}
}

// chainEqPred builds the join predicate `a.aCol = b.bCol` as QOV-rooted
// FieldValues, so GetCorrelatedToOfPredicate = {a, b} — the spanning shape
// PartitionSelectRule routes to the upper level.
func chainEqPred(a, aCol, b, bCol string) predicates.QueryPredicate {
	return predicates.NewComparisonPredicate(
		partitionField(a, aCol),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: partitionField(b, bCol),
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

// TestAliasesConnectedByCorrelation pins the correlation-edge reading of the
// same check, one arm per spelling it admits and per one it does not. The
// shared-outside arm is what admits {d, e} under {w} for two lateral legs that
// both read w — the only bipartition that block has — while a star whose legs
// are joined to a hub only by PREDICATES stays disconnected (the predicate
// arms above), so plain joins keep their pruning.
func TestAliasesConnectedByCorrelation(t *testing.T) {
	t.Parallel()
	deps := func(pairs ...string) map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{} {
		out := map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{}{}
		for i := 0; i+1 < len(pairs); i += 2 {
			from := values.NamedCorrelationIdentifier(pairs[i])
			if out[from] == nil {
				out[from] = map[values.CorrelationIdentifier]struct{}{}
			}
			out[from][values.NamedCorrelationIdentifier(pairs[i+1])] = struct{}{}
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		aliases map[values.CorrelationIdentifier]struct{}
		order   map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{}
		want    bool
	}{
		{"one depends on the other", aliasSet("S", "X"), deps("X", "S"), true},
		{"both depend on one alias outside", aliasSet("D", "E"), deps("D", "W", "E", "W"), true},
		{"three all depend on one alias outside", aliasSet("D", "E", "F"), deps("D", "W", "E", "W", "F", "W"), true},
		{"each depends on a different alias outside", aliasSet("D", "E"), deps("D", "W", "E", "H"), false},
		{"only one depends on anything", aliasSet("D", "E"), deps("D", "W"), false},
		{"no correlation at all", aliasSet("D", "E"), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := aliasesConnectedByPredicatesOrCorrelation(tc.aliases, nil, tc.order); got != tc.want {
				t.Errorf("aliasesConnectedByPredicatesOrCorrelation = %v, want %v", got, tc.want)
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
	correlatedScan := mustPartitionConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"X"}, partitionSelectRowType("X")))
	correlated := mustPartitionConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{joinPred("PB", "X")},
		expressions.ForEachQuantifier(expressions.InitialOf(correlatedScan)),
	))
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

	b2 := scanQuantifier("B2")
	c := scanQuantifier("C")
	innerResult := mustPartitionConstruct(b2.RequireFlowedObjectValue())
	inner := mustPartitionConstruct(expressions.NewSelectExpressionWithAliases(
		innerResult,
		[]expressions.Quantifier{b2, c},
		nil, nil))
	q := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("Q"), expressions.InitialOf(inner))
	outerResult := mustPartitionConstruct(q.RequireFlowedObjectValue())
	outer := mustPartitionConstruct(expressions.NewSelectExpressionWithAliases(
		outerResult,
		[]expressions.Quantifier{q},
		nil, nil))

	got := boundAliasesOfReference(expressions.InitialOf(outer))
	for _, want := range []string{"Q", "B2", "C"} {
		if _, ok := got[values.NamedCorrelationIdentifier(want)]; !ok {
			t.Fatalf("bound aliases missing %s (got %v)", want, got)
		}
	}
}

// TestPartitionSelect_NullOnEmptyMergesWithItsPartner: a null-on-empty leg B,
// correlated to its preserved partner A, may be collapsed into a positional
// lower together with A — the lower is then exactly the binary outer-join
// shape the NLJ rule implements with DefaultOnEmpty. A lower that strands the
// partner in the upper ({B,C}, connected by the B–C predicate) is declined.
// `SELECT * FROM b RIGHT JOIN a ON … WHERE EXISTS (…)` reaches the {A,B}
// shape once SelectMergeRule folds the rewritten outer join into its parent,
// and has no other plan.
func TestPartitionSelect_NullOnEmptyMergesWithItsPartner(t *testing.T) {
	t.Parallel()
	a, bBase, c := scanQuantifier("A"), scanQuantifier("B"), scanQuantifier("C")
	correlated := mustPartitionConstruct(expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{joinPred("A", "B")}, bBase))
	b := expressions.NamedForEachNullOnEmptyQuantifier(bBase.GetAlias(), expressions.InitialOf(correlated))
	rv := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: partitionField("A", "col")},
		values.RecordConstructorField{Name: "b", Value: partitionField("B", "col")},
		values.RecordConstructorField{Name: "c", Value: partitionField("C", "col")},
	)
	sel := mustPartitionConstruct(expressions.NewSelectExpression(rv, []expressions.Quantifier{a, b, c}, []predicates.QueryPredicate{joinPred("B", "C")}))
	lowers := map[string]bool{}
	for _, y := range mustFirePartitionExpressionRule(t, NewPartitionSelectRule(), expressions.InitialOf(sel)) {
		for _, lower := range nestedLowerAliasSets(y) {
			lowers[sortedAliasNames(lower)] = true
		}
	}
	if !lowers["{A,B}"] {
		t.Fatalf("no lower {A,B}: the null-on-empty leg must merge with its partner; lowers=%v", lowers)
	}
	if lowers["{B,C}"] {
		t.Fatalf("lower {B,C} strands the null-on-empty leg's partner A in the upper; lowers=%v", lowers)
	}
}
