package cascades

import (
	"fmt"
	"math/rand"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ============================================================================
// WHY THIS FILE EXISTS
//
// Reference.GetBest (expressions/reference.go:300) elects the winning plan
// with a single pairwise fold: `best := all[0]; for m in all[1:] { if
// less(m, best) { best = m } }`. Its own doc comment states the
// precondition: "the comparator must be a total order on Cost". A criterion
// that is not a total preorder makes the elected plan depend on the order
// members happen to arrive in — identical SQL, different plan, silently.
//
// Three criteria in planning_cost_model.go were found non-transitive this
// way already (see comparePrimaryScanVsIndexScan's and compareInPlan's doc
// comments, and TestCriterion7_RankIsATotalPreorder in
// primary_vs_index_transitivity_test.go). This file makes the property
// self-checking instead of relying on the next one being caught by a
// reviewer: it runs reflexivity / antisymmetry / transitivity /
// indifference-transitivity / fold-stability over generated candidate sets
// for every criterion function this package exposes, and composes them for
// the full comparator too.
//
// compareJoinOrdering (CQ-24) and compareRecursiveCTE (CQ-23) used to be a
// SELF-CLEANING EXCLUSION LIST here: both were known to violate these
// properties (assertKnownViolation proved the documented counterexample
// still reproduced, forcing removal from the list the moment either was
// actually fixed). Both are fixed now — compareJoinOrdering ranks every pair
// by a single Cost.Less metric once FlatMapCost.Cardinality stopped being an
// outer-only proxy blind to the inner side (cost_formulas.go), and
// compareRecursiveCTE ranks by an intrinsic per-plan rank
// (recursiveCTERank) instead of a pair-dependent branch — so both are
// covered the same way every other criterion in this file is: standalone
// assertTotalPreorder + assertFoldStable sections below.
// ============================================================================

// preorderCandidate names an expression for readable violation messages.
type preorderCandidate struct {
	name string
	expr expressions.RelationalExpression
}

// preorderCompareFn is the shape shared by every criterion under test here:
// negative means a is preferred, positive means b is preferred, 0 is a tie —
// exactly planningCostModelCompareWith's own convention.
type preorderCompareFn func(a, b expressions.RelationalExpression) int

// preorderViolations runs reflexivity, antisymmetry, transitivity and
// indifference-transitivity over every pair/triple in corpus and returns one
// human-readable line per violation found. An empty result means compare is
// a total preorder ON THIS CORPUS — absence of a violation over a finite
// corpus is evidence, not proof, which is exactly why fold-stability
// (assertFoldStable, below) is the property that matters operationally: it
// is the direct analogue of what GetBest does in production.
func preorderViolations(compare preorderCompareFn, corpus []preorderCandidate) []string {
	n := len(corpus)
	cmp := make([][]int, n)
	for i := range cmp {
		cmp[i] = make([]int, n)
		for j := range cmp[i] {
			cmp[i][j] = compare(corpus[i].expr, corpus[j].expr)
		}
	}
	var violations []string
	for i := 0; i < n; i++ {
		if cmp[i][i] != 0 {
			violations = append(violations, fmt.Sprintf(
				"REFLEXIVITY: compare(%s,%s)=%d, want 0", corpus[i].name, corpus[i].name, cmp[i][i]))
		}
		for j := 0; j < n; j++ {
			if cmp[i][j] != -cmp[j][i] {
				violations = append(violations, fmt.Sprintf(
					"ANTISYMMETRY: compare(%s,%s)=%d but compare(%s,%s)=%d",
					corpus[i].name, corpus[j].name, cmp[i][j], corpus[j].name, corpus[i].name, cmp[j][i]))
			}
		}
	}
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i == j {
				continue
			}
			for k := 0; k < n; k++ {
				if j == k || i == k {
					continue
				}
				if cmp[i][j] < 0 && cmp[j][k] < 0 && cmp[i][k] >= 0 {
					violations = append(violations, fmt.Sprintf(
						"TRANSITIVITY: %s < %s < %s but compare(%s,%s)=%d",
						corpus[i].name, corpus[j].name, corpus[k].name, corpus[i].name, corpus[k].name, cmp[i][k]))
				}
				if cmp[i][j] == 0 && cmp[j][k] == 0 && cmp[i][k] != 0 {
					violations = append(violations, fmt.Sprintf(
						"INDIFFERENCE-TRANSITIVITY: %s ~ %s ~ %s but compare(%s,%s)=%d",
						corpus[i].name, corpus[j].name, corpus[k].name, corpus[i].name, corpus[k].name, cmp[i][k]))
				}
			}
		}
	}
	return violations
}

// assertTotalPreorder fails the test with every violation found (capped, so
// a badly-broken criterion doesn't flood the log).
func assertTotalPreorder(t *testing.T, label string, compare preorderCompareFn, corpus []preorderCandidate) {
	t.Helper()
	violations := preorderViolations(compare, corpus)
	const cap = 20
	for i, v := range violations {
		if i >= cap {
			t.Errorf("%s: ... and %d more violations", label, len(violations)-cap)
			break
		}
		t.Errorf("%s: %s", label, v)
	}
}

// foldGetBest replicates Reference.GetBest's exact pairwise fold
// (expressions/reference.go:300) over named candidates.
func foldGetBest(members []preorderCandidate, less func(a, b expressions.RelationalExpression) bool) preorderCandidate {
	best := members[0]
	for _, m := range members[1:] {
		if less(m.expr, best.expr) {
			best = m
		}
	}
	return best
}

// assertFoldStable is the property users actually observe: GetBest must
// elect an EQUALLY GOOD member regardless of the order candidates happen to
// arrive in. "Equally good" — compare(base winner, this winner) == 0 — is
// deliberate rather than "the identical named member": many rungs have
// legitimate multi-member tie classes (e.g. every corpus item with
// typeFilterCount==0 ties on that ONE rung, by design — the tie defers to
// the NEXT rung in the lexicographic chain, exactly as it should). Fold
// order legitimately picks an arbitrary representative of a genuine tie
// class; that is not the hazard this file exists to catch. What IS the
// hazard: a comparator that is not a total preorder can make the fold land
// in a DIFFERENT, non-equivalent class depending on arrival order (that is
// exactly what CQ-24's 3-cycle does — no two rotations even agree on
// whether their picks tie). Requiring compare(base,got)==0 catches that
// while tolerating harmless within-tie-class nondeterminism.
//
// Exhaustive over every permutation for small corpora (n<=8, 40320 folds); a
// fixed-seed random sample of 300 shuffles otherwise, so the check stays
// deterministic across runs.
func assertFoldStable(t *testing.T, label string, compare preorderCompareFn, members []preorderCandidate) {
	t.Helper()
	if len(members) < 2 {
		return
	}
	less := func(a, b expressions.RelationalExpression) bool { return compare(a, b) < 0 }
	base := foldGetBest(members, less)

	check := func(order []preorderCandidate) {
		got := foldGetBest(order, less)
		if cmp := compare(base.expr, got.expr); cmp != 0 {
			names := make([]string, len(order))
			for i, m := range order {
				names[i] = m.name
			}
			t.Errorf("%s: FOLD INSTABILITY: order %v elects %q, which is NOT equally-good "+
				"(compare(base=%q, got)=%d) as the base arrival order's %q",
				label, names, got.name, base.name, cmp, base.name)
		}
	}

	if len(members) <= 8 {
		permute(members, check)
		return
	}
	rng := rand.New(rand.NewSource(12345))
	for i := 0; i < 300; i++ {
		shuffled := append([]preorderCandidate(nil), members...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		check(shuffled)
	}
}

// permute calls visit once per permutation of items (Heap's algorithm).
func permute(items []preorderCandidate, visit func([]preorderCandidate)) {
	buf := append([]preorderCandidate(nil), items...)
	var generate func(k int)
	generate = func(k int) {
		if k == 1 {
			visit(append([]preorderCandidate(nil), buf...))
			return
		}
		for i := 0; i < k; i++ {
			generate(k - 1)
			if k%2 == 0 {
				buf[i], buf[k-1] = buf[k-1], buf[i]
			} else {
				buf[0], buf[k-1] = buf[k-1], buf[0]
			}
		}
	}
	generate(len(buf))
}

// ============================================================================
// Criterion #6 — compareInPlan (rank-based since the IN-plan-penalty fix).
// ============================================================================

func inPlanCorpus(t *testing.T) []preorderCandidate {
	t.Helper()
	bindingName := "in_value"
	sargedRange := []*predicates.ComparisonRange{
		rungEqualityRange(t, values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(bindingName))),
	}
	indexSarged := plans.NewRecordQueryIndexPlan("idx_pre_in_sarged", sargedRange, []string{"T"}, values.UnknownType, false)
	indexUnsarged := plans.NewRecordQueryIndexPlan("idx_pre_in_unsarged", nil, []string{"T"}, values.UnknownType, false)
	plainIndex := plans.NewRecordQueryIndexPlan("idx_pre_in_plain", nil, []string{"T"}, values.UnknownType, false)
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)

	return []preorderCandidate{
		{"inJoin_sarged", plans.NewRecordQueryInJoinPlan(indexSarged, bindingName, false, false)},
		{"inJoin_unsarged", plans.NewRecordQueryInJoinPlan(indexUnsarged, bindingName, false, false)},
		{"inUnion_sarged", plans.NewRecordQueryInUnionPlan(indexSarged, []string{bindingName}, nil, false)},
		{"inUnion_unsarged", plans.NewRecordQueryInUnionPlan(indexUnsarged, []string{bindingName}, nil, false)},
		{"plainIndex", plainIndex},
		{"plainScan", scan},
	}
}

func TestCostModel_CompareInPlan_TotalPreorderAndFoldStable(t *testing.T) {
	t.Parallel()
	compare := func(a, b expressions.RelationalExpression) int {
		return compareInPlan(a, b, expressionCounts{}, expressionCounts{})
	}
	corpus := inPlanCorpus(t)
	assertTotalPreorder(t, "compareInPlan", compare, corpus)
	assertFoldStable(t, "compareInPlan", compare, corpus)
}

// ============================================================================
// Criterion #7 — comparePrimaryScanVsIndexScan (rank-based since 6142f0939).
// TestCriterion7_RankIsATotalPreorder (primary_vs_index_transitivity_test.go)
// already covers reflexivity/antisymmetry/transitivity/indifference-
// transitivity in depth; this adds the fold-stability axis that test suite
// does not check, over the same style of corpus.
// ============================================================================

func primaryVsIndexFoldCorpus(t *testing.T) []preorderCandidate {
	t.Helper()
	one := rungEqualityRange(t, values.LiteralValue(int64(1)))
	two := rungEqualityRange(t, values.LiteralValue(int64(2)))
	covering := plans.NewRecordQueryIndexPlan("idx_pre_fold_covering",
		[]*predicates.ComparisonRange{one}, []string{"T"}, values.UnknownType, false).
		WithCovering([]string{"K"})

	return []preorderCandidate{
		{"primary", plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)},
		{"primary+tf", makeCriterion7Primary(t)},
		{"primary+tf+1sarg", makeCriterion7Primary(t, one)},
		{"primary+tf+2sargs", makeCriterion7Primary(t, one, two)},
		{"index+1sarg", makeCriterion7Index(t, "idx_pre_fold_1", 1, one)},
		{"index+2sargs", makeCriterion7Index(t, "idx_pre_fold_2", 1, one, two)},
		{"coveringNoFetch", covering},
		{"sortedPrimary", plans.NewRecordQueryInMemorySortPlan(
			plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false), nil)},
	}
}

func TestCostModel_ComparePrimaryScanVsIndexScan_FoldStable(t *testing.T) {
	t.Parallel()
	for _, pref := range []IndexScanPreference{PreferScan, PreferIndex, PreferPrimaryKeyIndex} {
		pref := pref
		t.Run(fmt.Sprintf("pref=%v", pref), func(t *testing.T) {
			t.Parallel()
			corpus := primaryVsIndexFoldCorpus(t)
			compare := func(a, b expressions.RelationalExpression) int {
				return comparePrimaryScanVsIndexScan(
					a, b,
					concretePlanCounts(a.(plans.RecordQueryPlan), nil),
					concretePlanCounts(b.(plans.RecordQueryPlan), nil),
					pref)
			}
			assertFoldStable(t, "comparePrimaryScanVsIndexScan", compare, corpus)
		})
	}
}

// ============================================================================
// The physical-vs-logical gate (leading criterion in
// planningCostModelCompareWith, not factored into a named function). A pure
// per-plan predicate (isPhysical), so it is provably a total preorder by
// construction; covered anyway as a REGRESSION GUARD, since the earlier
// history of this file is "an obviously-safe-looking rung turned out to be
// pair-dependent" three times over.
// ============================================================================

func physicalVsLogicalCompare(a, b expressions.RelationalExpression) int {
	aPhys, bPhys := isPhysical(a), isPhysical(b)
	switch {
	case aPhys && !bPhys:
		return -1
	case !aPhys && bPhys:
		return 1
	default:
		return 0
	}
}

func physicalVsLogicalCorpus() []preorderCandidate {
	scanRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	quantifier := expressions.ForEachQuantifier(scanRef)
	logicalSelect := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(quantifier.GetAlias()),
		[]expressions.Quantifier{quantifier}, nil)

	return []preorderCandidate{
		{"physicalScan", plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)},
		{"physicalIndex", plans.NewRecordQueryIndexPlan("idx_phys_gate", nil, []string{"T"}, values.UnknownType, false)},
		{"logicalSelect", logicalSelect},
	}
}

func TestCostModel_PhysicalVsLogicalGate_TotalPreorderAndFoldStable(t *testing.T) {
	t.Parallel()
	corpus := physicalVsLogicalCorpus()
	assertTotalPreorder(t, "physical-vs-logical gate", physicalVsLogicalCompare, corpus)
	assertFoldStable(t, "physical-vs-logical gate", physicalVsLogicalCompare, corpus)
}

// ============================================================================
// The structural rungs below comparePrimaryScanVsIndexScan split into two
// shapes:
//
//  1. COUNT rungs (inMemorySortCount, typeFilterCount, the index-fetch
//     block's total/explicit-count, unmatchedFieldCount, inJoinCount,
//     mapFilter count, nljPredicateCount, numDefaultOnEmpty) are EACH
//     `intCompare(perPlanFunction(a), perPlanFunction(b))` over a
//     non-negative integer — a total preorder by construction, since
//     ordering by a real-valued per-item function can never cycle. Tested
//     directly below; the exhaustive per-triple check is a regression guard
//     rather than a discovery tool for this shape (the guard is: nobody
//     quietly turns one of them pair-dependent later, which is exactly how
//     compareInPlan, comparePrimaryScanVsIndexScan and compareJoinOrdering
//     broke).
//
//  2. DEPTH rungs (typeFilterDepth, fetchDepth, distinctDepth) use a -1
//     "not present anywhere in this branch" sentinel (costExprDepth /
//     concretePlanDepth) and ABSTAIN (tie) whenever either side is -1 — NOT
//     "actually tied", just inapplicable. Tested here IN ISOLATION, this
//     abstention breaks indifference-transitivity exactly like CQ-23's
//     compareRecursiveCTE: e.g. typeFilterOne ~ bareScan (bareScan has no
//     type filter, depth=-1, abstain) and bareScan ~ typeFilterDeep
//     (abstain) — but typeFilterOne and typeFilterDeep both DO have a type
//     filter, so the rung fires between them and prefers the deeper one
//     strictly, breaking the transitivity of the two abstentions.
//
//     This does NOT reach the composed comparator as a live bug, and is
//     deliberately NOT added to the CQ-24/CQ-23 exclusion list: each depth
//     rung is reached ONLY after its corresponding COUNT rung already tied
//     (typeFilterDepth sits directly below typeFilterCount; fetchDepth sits
//     inside the same `indexScanCount+coveringIndexCount>0` block as
//     fetchTotal; distinctDepth is reached after unmatchedFieldCount, which
//     is itself reached after every count rung above it). Two plans that tie
//     on the corresponding COUNT can only BOTH be -1 (both have zero of the
//     operator, hence genuinely N/A on both sides — a real tie) or BOTH be
//     >=0 (both have at least one, hence comparable) — the mixed case
//     (bareScan, depth=-1, count=0) vs (typeFilterOne, depth=0, count=1)
//     that produces the false tie above is exactly the case the preceding
//     COUNT rung already resolves (0 != 1) before the depth rung is ever
//     consulted. TestCostModel_ComposedComparator_TotalPreorder exercises
//     this same corpus through the FULL chain (count rung still gating the
//     depth rung) and finds no violation — the isolated per-rung test below
//     is not a fair test of these three because it skips the precondition
//     their position in the chain provides for free. Covered only via the
//     composed corpus; not given a standalone sub-test.
// ============================================================================

type structuralCriterion struct {
	name    string
	compare preorderCompareFn
}

func structuralCriteria() []structuralCriterion {
	countOf := func(f func(expressionCounts) int) preorderCompareFn {
		return func(a, b expressions.RelationalExpression) int {
			return intCompare(f(findExpressionsByType(a, nil, nil)), f(findExpressionsByType(b, nil, nil)))
		}
	}
	return []structuralCriterion{
		{"inMemorySortCount", countOf(func(c expressionCounts) int { return c.inMemorySortCount })},
		{"typeFilterCount", countOf(func(c expressionCounts) int { return c.typeFilterCount })},
		{"fetchTotal (indexScan+fetch)", countOf(func(c expressionCounts) int { return c.indexScanCount + c.fetchCount })},
		{"fetchCount (explicit nodes)", countOf(func(c expressionCounts) int { return c.fetchCount })},
		{"unmatchedFieldCount", countOf(func(c expressionCounts) int { return c.unmatchedFieldCount })},
		{"inJoinCount (more wins)", countOf(func(c expressionCounts) int { return -c.inJoinCount })},
		{"mapFilterCount", countOf(func(c expressionCounts) int { return c.mapCount + c.predicatesFilterCount })},
		{"nljPredicateCount (more wins)", countOf(func(c expressionCounts) int { return -c.nljPredicateCount })},
		{"numDefaultOnEmpty", countOf(func(c expressionCounts) int { return c.numDefaultOnEmpty })},
	}
}

// TestCostModel_DepthCriteria_AbstentionIsPositionallyGuarded pins the
// isolated-rung behaviour documented above: it demonstrates the abstention
// break (so a future reader can see the exact mechanism) and then proves the
// guard that neutralises it — the corresponding COUNT rung already
// separates any pair where one side is genuinely N/A (-1) and the other
// is not, so the depth rung the false tie routes through is never reached
// on a live comparison. Contrast with the FORMER compareJoinOrdering defect
// (CQ-24, now fixed): that one had no such guard — its pair-dependent
// joinShapesDiffer branch was consulted with no preceding rung that already
// equalised the precondition, so the break reached production instead of
// being absorbed the way this one is.
func TestCostModel_DepthCriteria_AbstentionIsPositionallyGuarded(t *testing.T) {
	t.Parallel()
	corpus := structuralCorpus(t)
	byName := map[string]expressions.RelationalExpression{}
	for _, c := range corpus {
		byName[c.name] = c.expr
	}
	countTied := func(a, b expressions.RelationalExpression, count func(expressionCounts) int) bool {
		return count(findExpressionsByType(a, nil, nil)) == count(findExpressionsByType(b, nil, nil))
	}

	// The isolated abstention break: typeFilterOne ~ bareScan ~ typeFilterDeep
	// but typeFilterOne strictly beats typeFilterDeep once both sides have a
	// type filter. Demonstrates the mechanism; not itself the guard proof.
	one, bare, deep := byName["typeFilterOne"], byName["bareScan"], byName["typeFilterDeep"]
	depthCmp := func(a, b expressions.RelationalExpression) int {
		da, db := costExprDepth(a, matchTypeFilter), costExprDepth(b, matchTypeFilter)
		if da < 0 || db < 0 || da == db {
			return 0
		}
		return intCompare(db, da) // deeper wins
	}
	if !(depthCmp(one, bare) == 0 && depthCmp(bare, deep) == 0 && depthCmp(one, deep) != 0) {
		t.Fatalf("expected the documented isolated-rung abstention break to reproduce, got "+
			"cmp(one,bare)=%d cmp(bare,deep)=%d cmp(one,deep)=%d", depthCmp(one, bare), depthCmp(bare, deep), depthCmp(one, deep))
	}

	// The guard: typeFilterOne and bareScan do NOT tie on typeFilterCount (1
	// vs 0), so the preceding count rung already separates them before the
	// depth rung would ever be reached on a live comparison — the false tie
	// above is unreachable in the real chain.
	typeFilterCount := func(c expressionCounts) int { return c.typeFilterCount }
	if countTied(one, bare, typeFilterCount) {
		t.Fatal("guard precondition broken: typeFilterOne and bareScan must NOT tie on typeFilterCount")
	}
	if countTied(bare, deep, typeFilterCount) {
		t.Fatal("guard precondition broken: bareScan and typeFilterDeep must NOT tie on typeFilterCount")
	}

	// And directly: the FULL comparator (which runs typeFilterCount before
	// typeFilterDepth) never abstains on the (one,bare)/(bare,deep) pairs the
	// way the isolated depth rung does — it decides them via typeFilterCount
	// instead, so the false tie never happens in production.
	full := func(a, b expressions.RelationalExpression) int { return planningCostModelCompareWith(a, b, nil, nil) }
	if full(one, bare) == 0 || full(bare, deep) == 0 {
		t.Fatalf("expected the full comparator to decide (not abstain on) both pairs via typeFilterCount, "+
			"got full(one,bare)=%d full(bare,deep)=%d", full(one, bare), full(bare, deep))
	}
}

// structuralCorpus is deliberately diverse across ALL eleven dimensions at
// once: sort presence, type-filter count/depth, fetch shape, distinct
// presence/depth, unmatched fields, IN-join nesting, map/filter nodes,
// DefaultOnEmpty. No join or recursive-CTE plan appears anywhere (those are
// exercised only by the excluded-criteria tests below).
func structuralCorpus(t *testing.T) []preorderCandidate {
	t.Helper()
	one := rungEqualityRange(t, values.LiteralValue(int64(1)))
	two := rungEqualityRange(t, values.LiteralValue(int64(2)))

	scan := func() *plans.RecordQueryScanPlan {
		return plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	}
	idx := func(name string, fetches int, ranges ...*predicates.ComparisonRange) plans.RecordQueryPlan {
		var cur plans.RecordQueryPlan = plans.NewRecordQueryIndexPlan(name, ranges, []string{"T"}, values.UnknownType, false)
		for i := 0; i < fetches; i++ {
			cur = plans.NewRecordQueryFetchFromPartialRecordPlan(cur, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey)
		}
		return cur
	}

	nestedInJoins := plans.NewRecordQueryMapPlan(
		plans.NewRecordQueryInJoinPlan(
			plans.NewRecordQueryInJoinPlan(idx("idx_struct_nested_in", 0), "inner_in", false, false),
			"outer_in", false, false),
		nil)
	singleInJoin := plans.NewRecordQueryMapPlan(
		plans.NewRecordQueryLimitPlan(
			plans.NewRecordQueryInJoinPlan(idx("idx_struct_single_in", 0), "single_in", false, false), 1, 0),
		nil)

	return []preorderCandidate{
		{"bareScan", scan()},
		{"bareIndex", idx("idx_struct_bare", 0)},
		{"indexSarged", idx("idx_struct_sarged", 0, one)},
		{"indexWithFetch", idx("idx_struct_fetch1", 1)},
		{"indexWithTwoFetches", idx("idx_struct_fetch2", 2)},
		{"indexSargedWithFetch", idx("idx_struct_sarged_fetch", 1, one, two)},
		{"typeFilterOne", plans.NewRecordQueryTypeFilterPlan([]string{"T1"}, scan())},
		{"typeFilterThree", plans.NewRecordQueryTypeFilterPlan([]string{"T1", "T2", "T3"}, scan())},
		{"typeFilterDeep", plans.NewRecordQueryLimitPlan(
			plans.NewRecordQueryTypeFilterPlan([]string{"T1", "T2"}, scan()), 10, 0)},
		{"sortedScan", plans.NewRecordQueryInMemorySortPlan(scan(), nil)},
		{"sortedIndex", plans.NewRecordQueryInMemorySortPlan(idx("idx_struct_sorted", 1, one), nil)},
		{"distinctScan", plans.NewRecordQueryDistinctPlan(scan())},
		{"distinctIndexDeep", plans.NewRecordQueryLimitPlan(
			plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(idx("idx_struct_distinct_deep", 0)), 10, 0)},
		{"pfilterOnePred", plans.NewRecordQueryPredicatesFilterPlan(scan(), []predicates.QueryPredicate{rungPredicate("A")})},
		{"pfilterTwoPred", plans.NewRecordQueryPredicatesFilterPlan(scan(),
			[]predicates.QueryPredicate{rungPredicate("A"), rungPredicate("B")})},
		{"mapWrapped", plans.NewRecordQueryMapPlan(scan(), nil)},
		{"nestedInJoins", nestedInJoins},
		{"singleInJoin", singleInJoin},
		{"defaultOnEmpty", plans.NewRecordQueryDefaultOnEmptyPlanFromQuantifier(
			expressions.NewPhysicalQuantifier(expressions.InitialOf(scan())),
			values.NewNullValue(values.UnknownType))},
		{"multiIntersection", plans.NewRecordQueryIntersectionPlan([]plans.RecordQueryPlan{
			idx("idx_struct_int_a", 0, one), idx("idx_struct_int_b", 0, two),
		}, nil)},
	}
}

func TestCostModel_StructuralCriteria_TotalPreorderAndFoldStable(t *testing.T) {
	t.Parallel()
	for _, sc := range structuralCriteria() {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			corpus := structuralCorpus(t)
			assertTotalPreorder(t, sc.name, sc.compare, corpus)
			assertFoldStable(t, sc.name, sc.compare, corpus)
		})
	}
}

// ============================================================================
// The COMPOSED comparator (planningCostModelCompareWith / PlanningCostModelLess),
// the property users actually depend on: does the FULL lexicographic chain,
// not just one rung, elect a stable winner? Corpus is the union of every
// corpus above (IN-plan, primary-vs-index, structural, physical-vs-logical)
// plus a small set of provable/unbounded-cardinality plans that exercise
// criterion #2 (see the doc comment on cardinalityBoundaryCorpus for why
// that criterion has no standalone function to test directly).
//
// Join-containing plans (FlatMap / NestedLoopJoin) and recursive-CTE plans
// (RecursiveDfsJoin / RecursiveLevelUnion) get their own dedicated corpus and
// tests below (compareJoinOrdering / compareRecursiveCTE) rather than joining
// this union — each needs its own StatisticsProvider/table-cardinality setup
// to exercise the shape it is testing, which the rest of this corpus does not
// carry.
// ============================================================================

// cardinalityBoundaryCorpus exercises criterion #2 (max-of-max provable data-
// access cardinality), which is inline logic in planningCostModelCompareWith
// rather than a standalone function — there is nothing to call directly the
// way compareInPlan or comparePrimaryScanVsIndexScan can be called. Its outer
// guard (wholePlanMaxCardinalityKnown(a) || wholePlanMaxCardinalityKnown(b))
// is pair-dependent (an OR of two per-side booleans) — the same shape of gate
// the FORMER compareJoinOrdering defect used to pick its metric from (CQ-24,
// fixed by making the metric a single per-plan-pair Cost value with no
// shape-conditioned branch) — AND that guard is blind to PlanContext (it always
// resolves PK arity from plan-stamped metadata only — see
// wholePlanMaxCardinalityKnown -> computeCardinalities -> scanProvableMaxCard,
// which hardcodes ctx=nil) while the criterion's own rank
// (opsX.maxDataAccessCardinality) IS ctx-aware. That divergence was probed
// directly with a 3-plan construction (a ctx-only-provable point lookup, a
// plan-stamped point lookup, and a genuinely unbounded scan): the guard does
// abstain where it provably should not (ctx-only-known vs unbounded ties at
// this rung instead of preferring the bounded side), but no case was found
// where that abstention flips the FULL composed comparator's verdict — a
// later criterion (principally comparePrimaryScanVsIndexScan or the scalar
// cost fallback) independently reaches the same relative order. Folded into
// this corpus, rather than excluded, so the composed-comparator's
// transitivity/fold-stability checks exercise the axis on every run; if a
// real cycle is lurking here it will fail THIS test, not go unnoticed.
func cardinalityBoundaryCorpus() []preorderCandidate {
	pk := []values.Value{&values.FieldValue{Field: "ID", Typ: values.NullableLong}}
	return []preorderCandidate{
		{"pointLookupStamped", plans.NewRecordQueryScanPlan([]string{"P2"}, values.UnknownType, false).
			WithPrimaryKey(pk).
			WithScanComparisons([]*predicates.ComparisonRange{
				rungEqualityRangeUnchecked(values.LiteralValue(int64(1))),
			})},
		{"pointLookupCtxOnly", plans.NewRecordQueryScanPlan([]string{"P"}, values.UnknownType, false).
			WithScanComparisons([]*predicates.ComparisonRange{
				rungEqualityRangeUnchecked(values.LiteralValue(int64(1))),
			})},
		{"unboundedScan", plans.NewRecordQueryScanPlan([]string{"Q"}, values.UnknownType, false)},
		{"uniqueIndexPointLookup", plans.NewRecordQueryIndexPlan(
			"idx_card_unique", []*predicates.ComparisonRange{rungEqualityRangeUnchecked(values.LiteralValue(int64(1)))},
			[]string{"T"}, values.UnknownType, false)},
		{"nonUniqueIndexEquality", plans.NewRecordQueryIndexPlan(
			"idx_card_nonunique", []*predicates.ComparisonRange{rungEqualityRangeUnchecked(values.LiteralValue(int64(1)))},
			[]string{"T"}, values.UnknownType, false)},
	}
}

// rungEqualityRangeUnchecked is rungEqualityRange without a *testing.T, for
// package-level corpus builders that construct fixtures outside a test
// function body.
func rungEqualityRangeUnchecked(operand values.Value) *predicates.ComparisonRange {
	comparison := &predicates.Comparison{Type: predicates.ComparisonEquals, Operand: operand}
	merged := predicates.EmptyComparisonRange().Merge(comparison)
	if !merged.Ok {
		panic("failed to create equality comparison range")
	}
	return merged.Range
}

// composedComparatorPlanContext supplies the PK metadata cardinalityBoundaryCorpus
// needs (ctx-provable point lookups) plus match-candidate uniqueness for the
// index items, fixed across the whole composed-comparator corpus exactly as
// production planning uses one PlanContext per comparison.
type composedComparatorPlanContext struct {
	indexTestPlanContext
}

func (composedComparatorPlanContext) GetPrimaryKeyColumns(rt string) []string {
	if rt == "P" || rt == "P2" {
		return []string{"ID"}
	}
	return nil
}

func newComposedComparatorPlanContext() PlanContext {
	return &composedComparatorPlanContext{
		indexTestPlanContext: indexTestPlanContext{
			candidates: []MatchCandidate{
				newKnownDistinctValueIndexCandidate(
					"idx_card_unique", []string{"T"}, []string{"K"}, nil, values.UnknownType, true, nil),
				newKnownDistinctValueIndexCandidate(
					"idx_card_nonunique", []string{"T"}, []string{"K"}, nil, values.UnknownType, false, nil),
			},
		},
	}
}

func composedCorpus(t *testing.T) []preorderCandidate {
	t.Helper()
	var corpus []preorderCandidate
	corpus = append(corpus, inPlanCorpus(t)...)
	corpus = append(corpus, primaryVsIndexFoldCorpus(t)...)
	corpus = append(corpus, physicalVsLogicalCorpus()...)
	corpus = append(corpus, structuralCorpus(t)...)
	corpus = append(corpus, cardinalityBoundaryCorpus()...)
	// Dedupe-proof the names (several sub-corpora reuse "primary"/"bareScan"
	// style names): make every entry's name unique so violation messages stay
	// legible, without changing which expressions are compared.
	seen := map[string]int{}
	for i := range corpus {
		seen[corpus[i].name]++
		if n := seen[corpus[i].name]; n > 1 {
			corpus[i].name = fmt.Sprintf("%s#%d", corpus[i].name, n)
		}
	}
	return corpus
}

func TestCostModel_ComposedComparator_TotalPreorder(t *testing.T) {
	t.Parallel()
	ctx := newComposedComparatorPlanContext()
	corpus := composedCorpus(t)
	compare := func(a, b expressions.RelationalExpression) int {
		return planningCostModelCompareWith(a, b, nil, ctx)
	}
	assertTotalPreorder(t, "composed PlanningCostModel", compare, corpus)
}

func TestCostModel_ComposedComparator_FoldStable(t *testing.T) {
	t.Parallel()
	ctx := newComposedComparatorPlanContext()
	corpus := composedCorpus(t)
	compare := func(a, b expressions.RelationalExpression) int {
		return planningCostModelCompareWith(a, b, nil, ctx)
	}
	assertFoldStable(t, "composed PlanningCostModel", compare, corpus)
}

// ============================================================================
// compareJoinOrdering (CQ-24, fixed). Used to pick its metric from
// joinShapesDiffer(a,b), a property of the PAIR: shapes differ -> raw
// Cost.CPU, shapes same -> Cost.Less (Cardinality+CPU). Two independent
// unequal-metric choices for what is nominally one lexicographic rung broke
// transitivity outright. The fix made FlatMapCost.Cardinality a true
// outerCard*innerCard join-cardinality (cost_formulas.go) instead of an
// outer-only proxy blind to the inner side, so it agrees with
// NestedLoopJoinCost's cardinality on the SAME logical join and a single
// Cost.Less metric decides every pair — no shape-conditioned branch left.
//
// The corpus below carries BOTH 3-plan cycles TODO.md's CQ-24 entry
// documents: the minimal one found by a targeted search over FlatMap/NLJ
// pairs across a spread of table cardinalities (T0=1, T1=2, T2=3, T4=8), and
// the second, independent cycle a permutation sweep alongside it found
// (flatMap(outer=1,inner=1000), flatMap(outer=100,inner=1),
// nlj(outer=1,inner=220)). Neither cycles any more; assertTotalPreorder
// covers both plus every other pair the corpus can form.
// ============================================================================

func joinOrderingCorpus() ([]preorderCandidate, properties.StatisticsProvider) {
	stats := properties.MapStatistics{PerType: map[string]float64{
		"T0": 1, "T1": 2, "T2": 3, "T4": 8,
		"U_o1": 1, "U_i1000": 1000, "U_o100": 100, "U_i1": 1, "U_i220": 220,
	}}
	mkFlatMap := func(outerT, innerT string, suffix int) *plans.RecordQueryFlatMapPlan {
		outer := plans.NewRecordQueryScanPlan([]string{outerT}, values.UnknownType, false)
		inner := plans.NewRecordQueryScanPlan([]string{innerT}, values.UnknownType, false)
		return plans.NewRecordQueryFlatMapPlan(
			outer, inner,
			values.NamedCorrelationIdentifier(fmt.Sprintf("o%d", suffix)),
			values.NamedCorrelationIdentifier(fmt.Sprintf("i%d", suffix)),
			values.LiteralValue(int64(1)), false)
	}
	mkNLJ := func(outerT, innerT string, suffix int) *plans.RecordQueryNestedLoopJoinPlan {
		outer := plans.NewRecordQueryScanPlan([]string{outerT}, values.UnknownType, false)
		inner := plans.NewRecordQueryScanPlan([]string{innerT}, values.UnknownType, false)
		pred := predicates.NewComparisonPredicate(
			&values.FieldValue{Field: "K", Typ: values.NullableLong},
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))
		return plans.NewRecordQueryNestedLoopJoinPlan(
			outer, inner, []predicates.QueryPredicate{pred}, plans.JoinInner,
			fmt.Sprintf("no%d", suffix), fmt.Sprintf("ni%d", suffix), nil)
	}

	return []preorderCandidate{
		// Cycle 1 (the minimal reproducer).
		{"NLJ(T0,T2)", mkNLJ("T0", "T2", 1)},
		{"FM(T0,T4)", mkFlatMap("T0", "T4", 2)},
		{"FM(T1,T0)", mkFlatMap("T1", "T0", 3)},
		// Cycle 2 (the independent second cycle from the permutation sweep).
		{"FM(U_o1,U_i1000)", mkFlatMap("U_o1", "U_i1000", 4)},
		{"FM(U_o100,U_i1)", mkFlatMap("U_o100", "U_i1", 5)},
		{"NLJ(U_o1,U_i220)", mkNLJ("U_o1", "U_i220", 6)},
	}, stats
}

func TestCostModel_CompareJoinOrdering_TotalPreorderAndFoldStable(t *testing.T) {
	t.Parallel()
	corpus, stats := joinOrderingCorpus()
	compare := func(a, b expressions.RelationalExpression) int {
		return compareJoinOrdering(a, b, stats, nil)
	}
	assertTotalPreorder(t, "compareJoinOrdering", compare, corpus)
	assertFoldStable(t, "compareJoinOrdering", compare, corpus)

	// Pin the exact two documented cycles are gone, independent of the
	// generic harness, so a change to the harness itself can't silently stop
	// exercising them.
	ab := compare(corpus[0].expr, corpus[1].expr)
	bc := compare(corpus[1].expr, corpus[2].expr)
	ac := compare(corpus[0].expr, corpus[2].expr)
	if ab < 0 && bc < 0 && ac >= 0 {
		t.Fatalf("CQ-24 cycle 1 reproduced: cmp(NLJ(T0,T2),FM(T0,T4))=%d "+
			"cmp(FM(T0,T4),FM(T1,T0))=%d cmp(NLJ(T0,T2),FM(T1,T0))=%d", ab, bc, ac)
	}
	de := compare(corpus[3].expr, corpus[4].expr)
	ef := compare(corpus[4].expr, corpus[5].expr)
	df := compare(corpus[3].expr, corpus[5].expr)
	if de < 0 && ef < 0 && df >= 0 {
		t.Fatalf("CQ-24 cycle 2 reproduced: cmp(FM(U_o1,U_i1000),FM(U_o100,U_i1))=%d "+
			"cmp(FM(U_o100,U_i1),NLJ(U_o1,U_i220))=%d cmp(FM(U_o1,U_i1000),NLJ(U_o1,U_i220))=%d", de, ef, df)
	}
}

// ============================================================================
// compareRecursiveCTE (CQ-23, fixed). Used to rank via aDFS&&bLevel /
// aLevel&&bDFS, a property of the PAIR: DFS < Level strictly, but an
// unclassified candidate (any non-recursive-CTE plan — the overwhelmingly
// common case, since this runs on every pair unconditionally) tied with
// BOTH: DFS ~ Unclassified ~ Level, an indifference-transitivity violation.
// The fix ranks by recursiveCTERank, an intrinsic per-plan rank in {0,1,2}
// (DFS, Unclassified, Level) — ordering by intCompare on a per-item rank can
// never cycle or produce an intransitive tie.
// ============================================================================

func recursiveCTECorpus() []preorderCandidate {
	dfs := plans.NewRecordQueryRecursiveDfsJoinPlanFromQuantifiers(
		expressions.NewPhysicalQuantifier(expressions.InitialOf(
			plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false))),
		expressions.NewPhysicalQuantifier(expressions.InitialOf(
			plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false))),
		values.NamedCorrelationIdentifier("prior"), plans.DfsPreorder, false)
	level := plans.NewRecordQueryRecursiveLevelUnionPlanFromQuantifiers(
		expressions.NewPhysicalQuantifier(expressions.InitialOf(
			plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false))),
		expressions.NewPhysicalQuantifier(expressions.InitialOf(
			plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false))),
		values.NamedCorrelationIdentifier("scan"), values.NamedCorrelationIdentifier("insert"), false)
	unclassified := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)

	return []preorderCandidate{
		{"DFS", dfs},
		{"Level", level},
		{"Unclassified", unclassified},
	}
}

func TestCostModel_CompareRecursiveCTE_TotalPreorderAndFoldStable(t *testing.T) {
	t.Parallel()
	corpus := recursiveCTECorpus()
	compare := compareRecursiveCTE
	assertTotalPreorder(t, "compareRecursiveCTE", compare, corpus)
	assertFoldStable(t, "compareRecursiveCTE", compare, corpus)

	// Pin the documented relative order directly: DFS still strictly beats
	// Level (the load-bearing preference this criterion exists for), and
	// Unclassified no longer ties with both simultaneously.
	dl := compare(corpus[0].expr, corpus[1].expr) // DFS vs Level
	du := compare(corpus[0].expr, corpus[2].expr) // DFS vs Unclassified
	ul := compare(corpus[2].expr, corpus[1].expr) // Unclassified vs Level
	if !(dl < 0 && du < 0 && ul < 0) {
		t.Fatalf("expected DFS < Unclassified < Level strictly, got "+
			"compare(DFS,Level)=%d compare(DFS,Unclassified)=%d compare(Unclassified,Level)=%d", dl, du, ul)
	}
}
