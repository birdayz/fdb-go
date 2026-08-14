package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
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

func TestCostModel_ProjectionSchemaIdentityDoesNotPerturbTieBreak(t *testing.T) {
	t.Parallel()
	scan := hashScan("SCORES")
	scanRef := expressions.InitialOf(scan)
	innerQ := expressions.ForEachQuantifier(scanRef)
	projected := []values.Value{hashField(innerQ, 0)}

	logicalScore := mustHashConstruct(expressions.NewLogicalProjectionExpressionWithAliases(
		projected, []string{"SCORE"}, innerQ))
	logicalPoints := mustHashConstruct(expressions.NewLogicalProjectionExpressionWithAliases(
		projected, []string{"POINTS"}, innerQ))
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

	physicalScore := mustHashConstruct(plans.NewRecordQueryProjectionPlanFromQuantifier(
		projected, []string{"SCORE"}, innerQ))
	physicalPoints := mustHashConstruct(plans.NewRecordQueryProjectionPlanFromQuantifier(
		projected, []string{"POINTS"}, innerQ))
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
	scan := hashScan("SCORES")
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	projected := []values.Value{hashField(innerQ, 0)}
	wrappedScore := &scanPlanExpression{plan: mustHashConstruct(plans.NewRecordQueryProjectionPlanFromQuantifier(
		projected, []string{"SCORE"}, innerQ))}
	wrappedPoints := &scanPlanExpression{plan: mustHashConstruct(plans.NewRecordQueryProjectionPlanFromQuantifier(
		projected, []string{"POINTS"}, innerQ))}

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

func TestCostModel_UnaliasedProjectionCanonicalFieldNamesAreTieNeutral(t *testing.T) {
	t.Parallel()
	scan := hashScan("SCORES")
	scanRef := expressions.InitialOf(scan)
	innerQ := expressions.ForEachQuantifier(scanRef)
	readByOrdinal := hashField(innerQ, 0)
	request := mustHashConstruct(values.FieldByName("ID"))
	flowedType := mustHashConstruct(innerQ.GetFlowedObjectType())
	root := mustHashConstruct(values.NewQuantifiedObjectValue(innerQ.GetAlias(), flowedType))
	readByName := mustHashConstruct(values.ResolveFieldAccess(
		root, []values.FieldRequest{request}))
	if !values.SemanticEqualsUnderAliasMap(readByOrdinal, readByName, values.EmptyAliasMap()) {
		t.Fatal("name and ordinal resolution of the same exact slot must be semantically equal")
	}

	logicalA := mustHashConstruct(expressions.NewLogicalProjectionExpression(
		[]values.Value{readByOrdinal}, innerQ))
	logicalB := mustHashConstruct(expressions.NewLogicalProjectionExpression(
		[]values.Value{readByName}, innerQ))
	logicalMemo := NewMemo(nil)
	logicalMemo.RegisterReference(scanRef)
	if logicalMemo.MemoizeExpression(logicalA) != logicalMemo.MemoizeExpression(logicalB) {
		t.Fatal("equivalent exact logical projections did not collapse in memo identity")
	}
	if tieBreakNodeHash(logicalA) != tieBreakNodeHash(logicalB) {
		t.Fatal("equivalent exact field requests perturbed the logical tie-break hash")
	}

	physicalA := mustHashConstruct(plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{readByOrdinal}, nil, innerQ))
	physicalB := mustHashConstruct(plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{readByName}, nil, innerQ))
	physicalMemo := NewMemo(nil)
	physicalMemo.RegisterReference(scanRef)
	if physicalMemo.MemoizeExpression(physicalA) != physicalMemo.MemoizeExpression(physicalB) {
		t.Fatal("equivalent exact physical projections did not collapse in memo identity")
	}
	if tieBreakNodeHash(physicalA) != tieBreakNodeHash(physicalB) {
		t.Fatal("equivalent exact field requests perturbed the physical tie-break hash")
	}
}

func TestCostModel_ProjectionSemanticContentChangesTieBreakHash(t *testing.T) {
	t.Parallel()
	scan := hashScan("SCORES")
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	read0 := hashField(innerQ, 0)
	read1 := hashField(innerQ, 1)
	if values.SemanticEqualsUnderAliasMap(read0, read1, values.EmptyAliasMap()) {
		t.Fatal("test requires different ordinals to be a genuine semantic Value change")
	}

	logical0 := mustHashConstruct(expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{read0}, []string{"SCORE"}, innerQ))
	logical1 := mustHashConstruct(expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{read1}, []string{"SCORE"}, innerQ))
	if tieBreakNodeHash(logical0) == tieBreakNodeHash(logical1) {
		t.Fatal("historical logical tie-break hash ignored a genuine projected-Value change")
	}

	physical0 := mustHashConstruct(plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{read0}, []string{"SCORE"}, innerQ))
	physical1 := mustHashConstruct(plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{read1}, []string{"SCORE"}, innerQ))
	if tieBreakNodeHash(physical0) == tieBreakNodeHash(physical1) {
		t.Fatal("historical physical tie-break hash ignored a genuine projected-Value change")
	}
}

func TestCostModel_ProjectionAliasVariantsRemainComparatorTies(t *testing.T) {
	t.Parallel()
	scan := hashScan("SCORES")
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	projected := []values.Value{hashField(innerQ, 0)}

	testCases := []struct {
		name       string
		scorePlan  expressions.RelationalExpression
		pointsPlan expressions.RelationalExpression
	}{
		{
			name: "logical",
			scorePlan: mustHashConstruct(expressions.NewLogicalProjectionExpressionWithAliases(
				projected, []string{"SCORE"}, innerQ)),
			pointsPlan: mustHashConstruct(expressions.NewLogicalProjectionExpressionWithAliases(
				projected, []string{"POINTS"}, innerQ)),
		},
		{
			name: "physical",
			scorePlan: mustHashConstruct(plans.NewRecordQueryProjectionPlanFromQuantifier(
				projected, []string{"SCORE"}, innerQ)),
			pointsPlan: mustHashConstruct(plans.NewRecordQueryProjectionPlanFromQuantifier(
				projected, []string{"POINTS"}, innerQ)),
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
