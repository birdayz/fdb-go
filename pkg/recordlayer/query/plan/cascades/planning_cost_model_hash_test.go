package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
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

func TestCostModel_ProjectionSchemaIdentityDoesNotPerturbTieBreak(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"SCORES"}, values.UnknownType, false)
	scanRef := expressions.InitialOf(scan)
	innerQ := expressions.ForEachQuantifier(scanRef)
	projected := []values.Value{
		values.NewFieldValueWithResolvedOrdinal("ID", 0, values.UnknownType),
	}

	logicalScore := expressions.NewLogicalProjectionExpressionWithAliases(
		projected, []string{"SCORE"}, innerQ)
	logicalPoints := expressions.NewLogicalProjectionExpressionWithAliases(
		projected, []string{"POINTS"}, innerQ)
	logicalMemo := NewMemo(nil)
	logicalMemo.RegisterReference(scanRef)
	if logicalMemo.MemoizeExpression(logicalScore) == logicalMemo.MemoizeExpression(logicalPoints) {
		t.Fatal("logical projections with different output schemas collapsed in memo identity")
	}
	if logicalScore.HashCodeWithoutChildren() == logicalPoints.HashCodeWithoutChildren() {
		t.Fatal("logical memo hashes must distinguish projection output schemas")
	}
	if tieBreakNodeHash(logicalScore) != tieBreakNodeHash(logicalPoints) {
		t.Fatal("logical projection output schema perturbed the schema-neutral tie-break node hash")
	}
	if deepHashCode(logicalScore) != deepHashCode(logicalPoints) {
		t.Fatal("logical projection output schema perturbed the planning cost tie-break")
	}
	if extractTieBreakHash(logicalScore, map[*expressions.Reference]bool{}) !=
		extractTieBreakHash(logicalPoints, map[*expressions.Reference]bool{}) {
		t.Fatal("logical projection output schema perturbed the extraction tie-break")
	}
	if newDesignationScope().deepHash(logicalScore, map[*expressions.Reference]bool{}) !=
		newDesignationScope().deepHash(logicalPoints, map[*expressions.Reference]bool{}) {
		t.Fatal("logical projection output schema perturbed the rewriting designation tie-break")
	}

	physicalScore := plans.NewRecordQueryProjectionPlanFromQuantifier(
		projected, []string{"SCORE"}, innerQ)
	physicalPoints := plans.NewRecordQueryProjectionPlanFromQuantifier(
		projected, []string{"POINTS"}, innerQ)
	physicalMemo := NewMemo(nil)
	physicalMemo.RegisterReference(scanRef)
	if physicalMemo.MemoizeExpression(physicalScore) == physicalMemo.MemoizeExpression(physicalPoints) {
		t.Fatal("physical projections with different output schemas collapsed in memo identity")
	}
	if physicalScore.EqualsPlanWithoutChildren(physicalPoints) {
		t.Fatal("physical memo equality must distinguish projection output schemas")
	}
	if physicalScore.HashCodeWithoutChildren() == physicalPoints.HashCodeWithoutChildren() {
		t.Fatal("physical memo hashes must distinguish projection output schemas")
	}
	if tieBreakNodeHash(physicalScore) != tieBreakNodeHash(physicalPoints) {
		t.Fatal("physical projection output schema perturbed the schema-neutral tie-break node hash")
	}
	if stablePlanHash(physicalScore) != stablePlanHash(physicalPoints) {
		t.Fatal("physical projection output schema perturbed the stable cost tie-break")
	}
}

func TestCostModel_ScanPlanExpressionProjectionSchemaDoesNotPerturbTieBreak(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"SCORES"}, values.UnknownType, false)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	projected := []values.Value{
		values.NewFieldValueWithResolvedOrdinal("ID", 0, values.UnknownType),
	}
	wrappedScore := &scanPlanExpression{plan: plans.NewRecordQueryProjectionPlanFromQuantifier(
		projected, []string{"SCORE"}, innerQ)}
	wrappedPoints := &scanPlanExpression{plan: plans.NewRecordQueryProjectionPlanFromQuantifier(
		projected, []string{"POINTS"}, innerQ)}

	memo := NewMemo(nil)
	if memo.MemoizeExpression(wrappedScore) == memo.MemoizeExpression(wrappedPoints) {
		t.Fatal("scanPlanExpression collapsed wrapped projections with different output schemas")
	}
	if wrappedScore.HashCodeWithoutChildren() == wrappedPoints.HashCodeWithoutChildren() {
		t.Fatal("scanPlanExpression memo hashes must preserve wrapped projection schemas")
	}
	if tieBreakNodeHash(wrappedScore) != tieBreakNodeHash(wrappedPoints) {
		t.Fatal("scanPlanExpression leaked wrapped projection schema into its tie-break node hash")
	}
	if deepHashCode(wrappedScore) != deepHashCode(wrappedPoints) {
		t.Fatal("wrapped projection schema perturbed the planning cost tie-break")
	}
	if extractTieBreakHash(wrappedScore, map[*expressions.Reference]bool{}) !=
		extractTieBreakHash(wrappedPoints, map[*expressions.Reference]bool{}) {
		t.Fatal("wrapped projection schema perturbed the extraction tie-break")
	}
	if newDesignationScope().deepHash(wrappedScore, map[*expressions.Reference]bool{}) !=
		newDesignationScope().deepHash(wrappedPoints, map[*expressions.Reference]bool{}) {
		t.Fatal("wrapped projection schema perturbed the rewriting designation tie-break")
	}
}

func TestCostModel_UnaliasedProjectionDisplayNamesAreMemoOnly(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"SCORES"}, values.UnknownType, false)
	scanRef := expressions.InitialOf(scan)
	innerQ := expressions.ForEachQuantifier(scanRef)
	readA := values.NewFieldValueWithResolvedOrdinal("A", 0, values.UnknownType)
	readB := values.NewFieldValueWithResolvedOrdinal("B", 0, values.UnknownType)
	if !values.SemanticEqualsUnderAliasMap(readA, readB, values.AliasMap{}) {
		t.Fatal("test requires same-ordinal baked reads to be semantically equal")
	}

	logicalA := expressions.NewLogicalProjectionExpression([]values.Value{readA}, innerQ)
	logicalB := expressions.NewLogicalProjectionExpression([]values.Value{readB}, innerQ)
	logicalMemo := NewMemo(nil)
	logicalMemo.RegisterReference(scanRef)
	if logicalMemo.MemoizeExpression(logicalA) == logicalMemo.MemoizeExpression(logicalB) {
		t.Fatal("unaliased logical projections with different derived names collapsed in memo identity")
	}
	if tieBreakNodeHash(logicalA) != tieBreakNodeHash(logicalB) {
		t.Fatal("derived display-only names perturbed the historical logical tie-break hash")
	}

	physicalA := plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{readA}, nil, innerQ)
	physicalB := plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{readB}, nil, innerQ)
	physicalMemo := NewMemo(nil)
	physicalMemo.RegisterReference(scanRef)
	if physicalMemo.MemoizeExpression(physicalA) == physicalMemo.MemoizeExpression(physicalB) {
		t.Fatal("unaliased physical projections with different derived names collapsed in memo identity")
	}
	if tieBreakNodeHash(physicalA) != tieBreakNodeHash(physicalB) {
		t.Fatal("derived display-only names perturbed the historical physical tie-break hash")
	}
}

func TestCostModel_ProjectionSemanticContentChangesTieBreakHash(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"SCORES"}, values.UnknownType, false)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	read0 := values.NewFieldValueWithResolvedOrdinal("ID", 0, values.UnknownType)
	read1 := values.NewFieldValueWithResolvedOrdinal("ID", 1, values.UnknownType)
	if values.SemanticEqualsUnderAliasMap(read0, read1, values.AliasMap{}) {
		t.Fatal("test requires different ordinals to be a genuine semantic Value change")
	}

	logical0 := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{read0}, []string{"SCORE"}, innerQ)
	logical1 := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{read1}, []string{"SCORE"}, innerQ)
	if tieBreakNodeHash(logical0) == tieBreakNodeHash(logical1) {
		t.Fatal("historical logical tie-break hash ignored a genuine projected-Value change")
	}

	physical0 := plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{read0}, []string{"SCORE"}, innerQ)
	physical1 := plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{read1}, []string{"SCORE"}, innerQ)
	if tieBreakNodeHash(physical0) == tieBreakNodeHash(physical1) {
		t.Fatal("historical physical tie-break hash ignored a genuine projected-Value change")
	}
}

func TestCostModel_ProjectionAliasVariantsRemainComparatorTies(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"SCORES"}, values.UnknownType, false)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	projected := []values.Value{
		values.NewFieldValueWithResolvedOrdinal("ID", 0, values.UnknownType),
	}

	testCases := []struct {
		name       string
		scorePlan  expressions.RelationalExpression
		pointsPlan expressions.RelationalExpression
	}{
		{
			name: "logical",
			scorePlan: expressions.NewLogicalProjectionExpressionWithAliases(
				projected, []string{"SCORE"}, innerQ),
			pointsPlan: expressions.NewLogicalProjectionExpressionWithAliases(
				projected, []string{"POINTS"}, innerQ),
		},
		{
			name: "physical",
			scorePlan: plans.NewRecordQueryProjectionPlanFromQuantifier(
				projected, []string{"SCORE"}, innerQ),
			pointsPlan: plans.NewRecordQueryProjectionPlanFromQuantifier(
				projected, []string{"POINTS"}, innerQ),
		},
	}
	comparators := []struct {
		name string
		less func(expressions.RelationalExpression, expressions.RelationalExpression) bool
	}{
		{name: "planning", less: PlanningCostModelLess},
		{name: "rewriting", less: RewritingCostModelLess},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, comparator := range comparators {
				t.Run(comparator.name, func(t *testing.T) {
					if comparator.less(tc.scorePlan, tc.pointsPlan) ||
						comparator.less(tc.pointsPlan, tc.scorePlan) {
						t.Fatal("alias-only projection variants must tie in both comparator directions")
					}
				})
			}
		})
	}
}
