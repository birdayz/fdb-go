package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestCostModel_PlanHashOrderSensitive pins the #17 tie-break's operand-order
// sensitivity: the two operand orders of a symmetric join MUST hash
// differently, or cost-tied swapped-operand alternatives compare equal and
// the winner follows task arrival order — a nondeterministic EXPLAIN (caught
// live by the flatten pins: the existential step-1 NLJ over
// two same-shaped scans flipped operand order across runs). A commutative
// child fold (`h ^= f(child)`) fails this; the fold must be positional.
func TestCostModel_PlanHashOrderSensitive(t *testing.T) {
	t.Parallel()
	scanA := plans.NewRecordQueryScanPlan([]string{"PA"}, nil, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"PB"}, nil, false)

	ab := plans.NewRecordQueryNestedLoopJoinPlan(scanA, scanB, nil, plans.JoinInner, "A", "B", nil)
	ba := plans.NewRecordQueryNestedLoopJoinPlan(scanB, scanA, nil, plans.JoinInner, "A", "B", nil)

	if stablePlanHash(ab) == stablePlanHash(ba) {
		t.Fatal("stablePlanHash is operand-order-INSENSITIVE: NLJ(A,B) == NLJ(B,A) — the #17 tie-break cannot discriminate swapped-operand ties and the winner follows task arrival order")
	}

	// Same-child sanity: identical trees still hash identically.
	ab2 := plans.NewRecordQueryNestedLoopJoinPlan(scanA, scanB, nil, plans.JoinInner, "A", "B", nil)
	if stablePlanHash(ab) != stablePlanHash(ab2) {
		t.Fatal("stablePlanHash is not structural: two identical trees hashed differently")
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
		scanA := plans.NewRecordQueryScanPlan([]string{"PA"}, nil, false)
		scanG := plans.NewRecordQueryScanPlan([]string{"PG"}, nil, false)
		// The predicate REFERENCES the minted alias (a correlated comparison
		// over QOV(filterAlias)) — the shape the rebased existential
		// correlation predicates actually carry — so the PredicatesFilter
		// arm's alias-blindness is genuinely exercised, not just its (never
		// hashed) alias field.
		pred := predicates.NewComparisonPredicate(
			values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(filterAlias)), "GID", values.NotNullLong),
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))},
		)
		filtered := plans.NewRecordQueryPredicatesFilterPlanWithAlias(scanG,
			[]predicates.QueryPredicate{pred}, values.NamedCorrelationIdentifier(filterAlias))
		return plans.NewRecordQueryFlatMapPlan(
			scanA, filtered,
			values.NamedCorrelationIdentifier(outerAlias),
			values.NamedCorrelationIdentifier(innerAlias),
			nil, false,
		)
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
		scanG := plans.NewRecordQueryScanPlan([]string{"PG"}, nil, false)
		pred := predicates.NewComparisonPredicate(
			values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("q$1")), "GID", values.NotNullLong),
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(lit)},
		)
		return plans.NewRecordQueryPredicatesFilterPlanWithAlias(scanG,
			[]predicates.QueryPredicate{pred}, values.NamedCorrelationIdentifier("q$1"))
	}
	if stablePlanHash(build(1)) == stablePlanHash(build(2)) {
		t.Fatal("stablePlanHash is content-blind: predicates differing only in their literal hashed equal — such ties fall to arrival order")
	}
}
