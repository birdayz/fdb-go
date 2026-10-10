package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustHashConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct cost-model hash fixture: " + err.Error())
	}
	return value
}

func hashRowType() values.Type {
	return values.NewRecordType("HashRow", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "GID", FieldType: values.NotNullLong, Ordinal: 1},
	})
}

func hashScan(recordType string) *plans.RecordQueryScanPlan {
	return mustHashConstruct(plans.NewRecordQueryScanPlan(
		[]string{recordType}, hashRowType(), false))
}

func hashField(q expressions.Quantifier, ordinal int) values.Value {
	flowedType := mustHashConstruct(q.GetFlowedObjectType())
	root := mustHashConstruct(values.NewQuantifiedObjectValue(q.GetAlias(), flowedType))
	return mustHashConstruct(values.ResolveFieldOrdinals(root, []int{ordinal}))
}

func hashResultValue() values.Value {
	return values.NewRawRecordConstructorValue(values.RecordConstructorField{
		Name:  "VALUE",
		Value: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
	})
}

// TestCostModel_PlanHashOrderSensitive pins the #17 tie-break's operand-order
// sensitivity: the two operand orders of a symmetric join MUST hash
// differently, or cost-tied swapped-operand alternatives compare equal and
// the winner follows task arrival order — a nondeterministic EXPLAIN (caught
// live by the flatten pins: the existential step-1 NLJ over
// two same-shaped scans flipped operand order across runs). A commutative
// child fold (`h ^= f(child)`) fails this; the fold must be positional.
func TestCostModel_PlanHashOrderSensitive(t *testing.T) {
	t.Parallel()
	scanA := hashScan("PA")
	scanB := hashScan("PB")

	ab := mustHashConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
		scanA, scanB, nil, plans.JoinInner,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), hashResultValue()))
	ba := mustHashConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
		scanB, scanA, nil, plans.JoinInner,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), hashResultValue()))

	if stablePlanHash(ab) == stablePlanHash(ba) {
		t.Fatal("stablePlanHash is operand-order-INSENSITIVE: NLJ(A,B) == NLJ(B,A) — the #17 tie-break cannot discriminate swapped-operand ties and the winner follows task arrival order")
	}

	// Same-child sanity: identical trees still hash identically.
	ab2 := mustHashConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
		scanA, scanB, nil, plans.JoinInner,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), hashResultValue()))
	if stablePlanHash(ab) != stablePlanHash(ab2) {
		t.Fatal("stablePlanHash is not structural: two identical trees hashed differently")
	}
}

func TestCostModel_ConstantPoolJoinOrderTies(t *testing.T) {
	t.Parallel()
	for _, operand := range []values.Value{
		&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
		values.NewConstantObjectValue(values.NamedCorrelationIdentifier("pool"), "0", values.NotNullLong),
	} {
		a, b := hashScan("GA"), hashScan("C")
		q := expressions.ForEachQuantifier(expressions.FinalOf(b))
		filter := mustHashConstruct(plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(q,
			[]predicates.QueryPredicate{predicates.NewComparisonPredicate(hashField(q, 1),
				predicates.Comparison{Type: predicates.ComparisonEquals, Operand: operand})}, q.GetAlias()))
		rangeResult := predicates.EmptyComparisonRange().Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: operand})
		if !rangeResult.Complete() {
			t.Fatal("fixture equality did not form a scan bound")
		}
		for _, other := range []plans.RecordQueryPlan{
			filter, b.WithScanComparisons([]*predicates.ComparisonRange{rangeResult.Range}),
		} {
			ab := mustHashConstruct(plans.NewRecordQueryNestedLoopJoinPlan(a, other, nil, plans.JoinInner,
				values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), hashResultValue()))
			ba := mustHashConstruct(plans.NewRecordQueryNestedLoopJoinPlan(other, a, nil, plans.JoinInner,
				values.NamedCorrelationIdentifier("B"), values.NamedCorrelationIdentifier("A"), hashResultValue()))
			ca := concretePlanCost(ab, properties.DefaultStatistics{}, nil)
			cb := concretePlanCost(ba, properties.DefaultStatistics{}, nil)
			if ca != cb {
				t.Fatalf("%T/%T materialized join orders have different costs: %+v / %+v", operand, other, ca, cb)
			}
			if got, want := PlanningCostModelLess(ab, ba), costExprHash(ab) < costExprHash(ba); got != want {
				t.Fatalf("%T/%T join order was not selected by the content hash", operand, other)
			}
		}
	}
}

// TestCostModel_PlanHashMintedAliasBlind pins the tie-break's alias
// blindness: two plans identical except for their MINTED correlation
// identifiers (fresh q$N per planning — the FlatMap outer/inner aliases,
// the alias-carrying PredicatesFilter) MUST hash identically, or two
// cost-tied candidates rank in a different order on every planning of the
// same query and the EXPLAIN'd winner flips run to run (the
// live catch: the existential step-1 NLJ operand order was nondeterministic
// because the tie-break hashed the translation-minted existential alias and
// the per-firing merged-outer correlation).
func TestCostModel_PlanHashMintedAliasBlind(t *testing.T) {
	t.Parallel()
	build := func(outerAlias, innerAlias, filterAlias string) plans.RecordQueryPlan {
		scanA := hashScan("PA")
		scanG := hashScan("PG")
		// The predicate REFERENCES the minted alias (a correlated comparison
		// over QOV(filterAlias)) — the shape the rebased existential
		// correlation predicates actually carry — so the PredicatesFilter
		// arm's alias-blindness is genuinely exercised, not just its (never
		// hashed) alias field.
		filterID := values.NamedCorrelationIdentifier(filterAlias)
		filterQ := expressions.NamedForEachQuantifier(filterID, expressions.FinalOf(scanG))
		pred := predicates.NewComparisonPredicate(hashField(filterQ, 1),
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))
		filtered := mustHashConstruct(plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
			filterQ, []predicates.QueryPredicate{pred}, filterID))
		return mustHashConstruct(plans.NewRecordQueryFlatMapPlan(
			scanA, filtered,
			values.NamedCorrelationIdentifier(outerAlias),
			values.NamedCorrelationIdentifier(innerAlias),
			hashResultValue(), false,
		))
	}
	p1 := build("q$100", "q$101", "q$102")
	p2 := build("q$900", "q$901", "q$902")
	if stablePlanHash(p1) != stablePlanHash(p2) {
		t.Fatal("stablePlanHash depends on minted correlation identifiers — cost-tied candidates rank differently on every planning and the EXPLAIN'd winner flips")
	}
	// Structural-identity sanity: the same aliases and content rebuilt is
	// the same hash.
	p3 := build("q$100", "q$101", "q$102")
	if stablePlanHash(p1) != stablePlanHash(p3) {
		t.Fatal("stablePlanHash is not structural: identical trees hashed differently")
	}
}

// TestCostModel_PlanHashContentSensitive pins the other half of alias
// blindness: a REAL content difference — a different literal inside an
// otherwise identical predicate tree — MUST change the hash. Alias-blind is
// not content-blind: the predicate folds through SemanticHashCode, which
// keeps literals (a content-blind hash would tie plans that filter
// differently and hand the winner to arrival order).
func TestCostModel_PlanHashContentSensitive(t *testing.T) {
	t.Parallel()
	build := func(lit int64) plans.RecordQueryPlan {
		scanG := hashScan("PG")
		alias := values.NamedCorrelationIdentifier("q$1")
		q := expressions.NamedForEachQuantifier(alias, expressions.FinalOf(scanG))
		pred := predicates.NewComparisonPredicate(hashField(q, 1),
			predicates.NewLiteralComparison(predicates.ComparisonEquals, lit))
		return mustHashConstruct(plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
			q, []predicates.QueryPredicate{pred}, alias))
	}
	if stablePlanHash(build(1)) == stablePlanHash(build(2)) {
		t.Fatal("stablePlanHash is content-blind: predicates differing only in their literal hashed equal — such ties fall to arrival order")
	}
}
