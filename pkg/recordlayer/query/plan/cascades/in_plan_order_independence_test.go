package cascades

import (
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// IN-plan comparison order-independence.
//
// The cost model's IN-plan rung (criterion #6) used to be evaluated for the
// LEFT argument only, mirroring Java's compareInOperator/flipFlop pair. That
// made it non-antisymmetric on two shapes:
//
//   - two IN-plans whose bindings never became search arguments: BOTH
//     orientations answered "+1, the first argument is worse", so `less` was
//     false both ways and every later rung — including the full cost model —
//     was skipped in favour of a plan-hash coin flip (or, in the group fold
//     that compares without a tie-break, member insertion order);
//   - a SARGed IN-plan against an unSARGed one: one orientation abstained and
//     kept walking the chain while the swapped orientation short-circuited to
//     "+1", so the two directions could disagree about which plan wins.
//
// A comparator built by lexicographic composition of criteria is transitive
// only if every criterion is itself a total preorder. This rung now ranks BOTH
// sides on the penalty, which makes it one — see compareInPlan.
//
// The corpus below is deliberately IN-plan-rooted: compareInPlan inspects the
// ROOT expression, so wrapping an IN-plan in a Map (as the RFC-190 sort-gate
// corpus does) keeps the rung from ever firing. That is exactly the dimension
// the pre-existing transitivity corpus left unprobed.

// inCorpusSpec describes one candidate. Every candidate's data access is a
// single RecordQueryIndexPlan leaf, so criterion #4 (data-access count) ties
// across the whole corpus and criterion #2 (max cardinality) is unknown for
// every candidate — no equality binds a full unique key anywhere. Differences
// between two candidates therefore route through the IN rung and the
// structural rungs below it.
type inCorpusSpec struct {
	name string

	// kind selects the ROOT shape:
	//
	//	inJoin      — RecordQueryInJoinPlan (single binding)
	//	inUnion     — RecordQueryInUnionPlan (one or two bindings)
	//	wrappedIn   — Map over an InJoin: an IN plan the ROOT-only rung does
	//	              NOT see, so it is a "non-IN plan" as far as criterion #6
	//	              is concerned while still carrying inJoinCount
	//	plain       — no IN operator anywhere
	//	primaryScan — a primary RecordQueryScanPlan leaf instead of an index
	//	              leaf, so the primary-vs-index rung directly below the IN
	//	              rung also gets exercised against IN-plan candidates
	kind string

	// sargedBindings is the number of leading bindings whose name appears in
	// an equality comparison of the inner index scan. compareInOperator
	// penalises an IN-plan only when NONE of its bindings is SARGed, so 0
	// means penalty 1 and anything else means penalty 0.
	sargedBindings int

	// bindings is the number of IN bindings (inUnion only; inJoin is always 1).
	bindings int

	// fetches stacks RecordQueryFetchFromPartialRecordPlan layers over the
	// leaf, feeding the fetch-count/fetch-depth rungs.
	fetches int

	// typeFilter inserts a single-record-type filter over the leaf.
	typeFilter bool

	// extraMaps stacks RecordQueryMapPlan layers between the leaf chain and
	// the IN operator, feeding the map/filter node-count rung.
	extraMaps int
}

func (s inCorpusSpec) indexName() string { return "idx_" + s.name }

func (s inCorpusSpec) bindingName(i int) string { return fmt.Sprintf("%s_bind%d", s.name, i) }

// build materialises the spec. The inner chain is shared by every kind so the
// only structural difference between an IN-rooted and a plain candidate of the
// same spec is the IN operator itself.
func (s inCorpusSpec) build(t *testing.T) plans.RecordQueryPlan {
	t.Helper()

	numBindings := s.bindings
	if numBindings == 0 {
		numBindings = 1
	}

	var ranges []*predicates.ComparisonRange
	for i := 0; i < s.sargedBindings; i++ {
		ranges = append(ranges, rungEqualityRange(
			t,
			values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(s.bindingName(i))),
		))
	}

	var cur plans.RecordQueryPlan
	if s.kind == "primaryScan" {
		// The scan carries the same comparisons an index leaf would, so the
		// primary-vs-index rung's search-argument component is probed too.
		cur = plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
			WithScanComparisons(ranges)
	} else {
		cur = plans.NewRecordQueryIndexPlan(s.indexName(), ranges, []string{"T"}, values.UnknownType, false)
	}
	for i := 0; i < s.fetches; i++ {
		cur = plans.NewRecordQueryFetchFromPartialRecordPlan(cur, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey)
	}
	if s.typeFilter {
		cur = plans.NewRecordQueryTypeFilterPlan([]string{"T"}, cur)
	}
	for i := 0; i < s.extraMaps; i++ {
		cur = plans.NewRecordQueryMapPlan(cur, nil)
	}

	switch s.kind {
	case "inJoin":
		return plans.NewRecordQueryInJoinPlan(cur, s.bindingName(0), false, false)
	case "inUnion":
		names := make([]string, numBindings)
		for i := range names {
			names[i] = s.bindingName(i)
		}
		return plans.NewRecordQueryInUnionPlan(cur, names, nil, false)
	case "wrappedIn":
		return plans.NewRecordQueryMapPlan(
			plans.NewRecordQueryInJoinPlan(cur, s.bindingName(0), false, false), nil)
	case "plain", "primaryScan":
		return cur
	default:
		t.Fatalf("unknown corpus kind %q", s.kind)
		return nil
	}
}

// buildInPlanCorpus returns the candidate set the property sweep scans. Every
// pair shape from the antisymmetry table is present, in both the InJoin and the
// InUnion flavour:
//
//	SARGed IN   vs unSARGed IN     — the pair whose orientations used to disagree
//	unSARGed IN vs unSARGed IN     — the pair both orientations used to call "worse"
//	SARGed IN   vs SARGed IN
//	IN          vs non-IN (plain, Map-wrapped IN, primary scan)
func buildInPlanCorpus(t *testing.T) []namedPlan {
	t.Helper()

	specs := []inCorpusSpec{
		// --- unSARGed IN-joins: penalty 1, and structurally distinguishable
		// from each other so the LATER rungs have a real verdict to give. ---
		{name: "unsargedInJoin_lean", kind: "inJoin", sargedBindings: 0},
		{name: "unsargedInJoin_fetch1", kind: "inJoin", sargedBindings: 0, fetches: 1},
		{name: "unsargedInJoin_fetch3", kind: "inJoin", sargedBindings: 0, fetches: 3},
		{name: "unsargedInJoin_tf", kind: "inJoin", sargedBindings: 0, typeFilter: true},
		{name: "unsargedInJoin_maps2", kind: "inJoin", sargedBindings: 0, extraMaps: 2},

		// --- SARGed IN-joins: penalty 0, so criterion #6 abstains and the
		// chain continues, which is the whole point of the SARGed path. ---
		{name: "sargedInJoin_lean", kind: "inJoin", sargedBindings: 1},
		{name: "sargedInJoin_fetch1", kind: "inJoin", sargedBindings: 1, fetches: 1},
		{name: "sargedInJoin_fetch3", kind: "inJoin", sargedBindings: 1, fetches: 3},
		{name: "sargedInJoin_tf", kind: "inJoin", sargedBindings: 1, typeFilter: true},
		{name: "sargedInJoin_maps2", kind: "inJoin", sargedBindings: 1, extraMaps: 2},

		// --- IN-unions, single and multi binding. A multi-binding union with
		// ONE SARGed binding is SARGed (compareInOperator is an any-match). ---
		{name: "unsargedInUnion1_lean", kind: "inUnion", bindings: 1, sargedBindings: 0},
		{name: "unsargedInUnion1_fetch2", kind: "inUnion", bindings: 1, sargedBindings: 0, fetches: 2},
		{name: "unsargedInUnion2_lean", kind: "inUnion", bindings: 2, sargedBindings: 0},
		{name: "unsargedInUnion2_tf", kind: "inUnion", bindings: 2, sargedBindings: 0, typeFilter: true},
		{name: "sargedInUnion1_lean", kind: "inUnion", bindings: 1, sargedBindings: 1},
		{name: "sargedInUnion2_partial", kind: "inUnion", bindings: 2, sargedBindings: 1},
		{name: "sargedInUnion2_full", kind: "inUnion", bindings: 2, sargedBindings: 2},
		{name: "sargedInUnion2_fetch2", kind: "inUnion", bindings: 2, sargedBindings: 2, fetches: 2},

		// --- Non-IN roots: the "Y" column of the pair table. ---
		{name: "plain_lean", kind: "plain"},
		{name: "plain_fetch1", kind: "plain", fetches: 1},
		{name: "plain_fetch3", kind: "plain", fetches: 3},
		{name: "plain_tf", kind: "plain", typeFilter: true},
		{name: "plain_maps2", kind: "plain", extraMaps: 2},

		// --- An IN operator the ROOT-only rung cannot see. Non-IN for
		// criterion #6, IN-bearing for the inJoinCount rung further down. ---
		{name: "wrappedIn_lean", kind: "wrappedIn", sargedBindings: 0},
		{name: "wrappedIn_sarged", kind: "wrappedIn", sargedBindings: 1},
		{name: "wrappedIn_fetch2", kind: "wrappedIn", sargedBindings: 0, fetches: 2},

		// --- Primary scans, which additionally drive the primary-vs-index
		// rung sitting immediately below the IN rung. Both the unencumbered
		// shape and the type-filtered one appear, with and without search
		// arguments, so the rung's contested band is exercised in every
		// combination against the index-rooted candidates above. ---
		{name: "primaryScan_lean", kind: "primaryScan"},
		{name: "primaryScan_tf", kind: "primaryScan", typeFilter: true},
		{name: "primaryScan_sarged", kind: "primaryScan", sargedBindings: 1},
		{name: "primaryScan_tf_sarged", kind: "primaryScan", sargedBindings: 1, typeFilter: true},
		{name: "primaryScan_tf_sarged2", kind: "primaryScan", sargedBindings: 2, typeFilter: true},
		{name: "primaryScan_tf_fetch1", kind: "primaryScan", typeFilter: true, fetches: 1},
	}

	out := make([]namedPlan, 0, len(specs))
	for _, s := range specs {
		out = append(out, namedPlan{name: s.name, plan: s.build(t)})
	}
	return out
}

// inPlanProfile renders the IN-rung inputs plus the structural counts the
// rungs below it consume, for failure reporting.
func inPlanProfile(p plans.RecordQueryPlan) string {
	penalty, applicable := compareInOperator(p)
	c := concretePlanCounts(p, nil)
	return fmt.Sprintf(
		"inRung=(penalty=%d applicable=%v) idxScan=%d scan=%d fetch=%d typeFilter=%d map=%d inJoin=%d inUnion=%d",
		penalty, applicable, c.indexScanCount, c.scanCount, c.fetchCount,
		c.typeFilterCount, c.mapCount, c.inJoinCount, c.inUnionCount)
}

// TestCostModel_InPlanComparisonIsOrderIndependent is the RED/GREEN property
// test for the IN rung. It brute-forces every ordered pair and every ordered
// triple over the IN-rooted corpus against the FULL comparator chain — not the
// rung in isolation — because the property CQ-10 actually needs is that the
// WINNER is a function of the memo's contents, and only the whole chain
// decides a winner.
//
// Against the pre-fix comparator this reports antisymmetry violations on every
// unSARGed/unSARGed and SARGed/unSARGed IN pair, plus the permutation
// violations they cause.
//
// SCOPE. Every property — irreflexivity, antisymmetry, totality, transitivity
// and permutation-independence — is asserted over the WHOLE corpus, primary
// scans included.
//
// The transitivity and permutation sweeps used to be scoped to the index-rooted
// subset, because criterion #7 (comparePrimaryScanVsIndexScan) was a second,
// independent source of intransitivity: it was pair-restricted by construction,
// firing only for (lone primary scan) versus (singular index-scan-with-fetch)
// and abstaining for index-versus-index, so it could not be a total preorder.
// Over this corpus that produced 69 transitivity violations, every one of them
// involving a primary scan, of the form
//
//	sargedIndex+3fetches < primaryScan+typeFilter   (criterion #7's SARG sub-case:
//	                                                 the index SARGs strictly more
//	                                                 and pays no type filter)
//	primaryScan+typeFilter < plainIndex+1fetch      (criterion #7's config branch:
//	                                                 PREFER_SCAN, the default)
//	plainIndex+1fetch      < sargedIndex+3fetches   (criterion #7 abstains for an
//	                                                 index/index pair; the fetch
//	                                                 rung decides, 2 < 4)
//
// which contains no IN operator at all. Criterion #7 now ranks both sides
// instead (primaryVsIndexRankOf), so the scoping is gone and the widened sweep
// is the proof: it fails on the pre-fix rung and passes on the ranked one.
func TestCostModel_InPlanComparisonIsOrderIndependent(t *testing.T) {
	t.Parallel()

	corpus := buildInPlanCorpus(t)
	n := len(corpus)

	// The corpus is only meaningful if it actually contains all three
	// classes the rung distinguishes. Guard against silent drift.
	var numSarged, numUnsarged, numNonIn int
	for _, c := range corpus {
		penalty, applicable := compareInOperator(c.plan)
		switch {
		case !applicable:
			numNonIn++
		case penalty == 0:
			numSarged++
		default:
			numUnsarged++
		}
	}
	if numSarged < 2 || numUnsarged < 2 || numNonIn < 2 {
		t.Fatalf("corpus lost a class: sarged=%d unsarged=%d nonIn=%d, want >=2 of each",
			numSarged, numUnsarged, numNonIn)
	}

	cmp := make([][]int, n)
	for i := range cmp {
		cmp[i] = make([]int, n)
		for j := range cmp[i] {
			cmp[i][j] = planningCostModelCompareWith(corpus[i].plan, corpus[j].plan, nil, nil)
		}
	}
	less := func(i, j int) bool { return cmp[i][j] < 0 }

	// --- Property 0: irreflexivity. ---
	for i := 0; i < n; i++ {
		if cmp[i][i] != 0 {
			t.Errorf("IRREFLEXIVITY VIOLATION: compare(%s,%s)=%d, want 0",
				corpus[i].name, corpus[i].name, cmp[i][i])
		}
	}

	// --- Property 1: antisymmetry over every unordered pair. ---
	antisymViolations := 0
	pairs := 0
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			pairs++
			if cmp[i][j] != -cmp[j][i] {
				antisymViolations++
				if antisymViolations <= 20 {
					t.Errorf("ANTISYMMETRY VIOLATION: compare(%s,%s)=%d but compare(%s,%s)=%d (want %d)\n  %s: %s\n  %s: %s",
						corpus[i].name, corpus[j].name, cmp[i][j],
						corpus[j].name, corpus[i].name, cmp[j][i], -cmp[i][j],
						corpus[i].name, inPlanProfile(corpus[i].plan),
						corpus[j].name, inPlanProfile(corpus[j].plan))
				}
			}
		}
	}

	// Transitivity and permutation-independence run over the WHOLE corpus,
	// primary scans included — see the SCOPE paragraph on this test. The corpus
	// must still contain primary scans for that widening to mean anything.
	all := make([]int, n)
	numPrimaryScans := 0
	for i, c := range corpus {
		all[i] = i
		if strings.HasPrefix(c.name, "primaryScan") {
			numPrimaryScans++
		}
	}
	if numPrimaryScans < 2 {
		t.Fatalf("corpus has %d primary-scan candidates, want >= 2: the criterion-#7 "+
			"dimension this sweep exists to cover is no longer probed", numPrimaryScans)
	}

	// --- Property 2: strict-order transitivity over every ordered triple. ---
	transitivityViolations := 0
	triples := 0
	for _, i := range all {
		for _, j := range all {
			if i == j {
				continue
			}
			for _, k := range all {
				if k == i || k == j {
					continue
				}
				triples++
				if less(i, j) && less(j, k) && !less(i, k) {
					transitivityViolations++
					if transitivityViolations <= 20 {
						t.Errorf("TRANSITIVITY VIOLATION: %s < %s < %s but compare(%s,%s)=%d\n  %s: %s\n  %s: %s\n  %s: %s",
							corpus[i].name, corpus[j].name, corpus[k].name,
							corpus[i].name, corpus[k].name, cmp[i][k],
							corpus[i].name, inPlanProfile(corpus[i].plan),
							corpus[j].name, inPlanProfile(corpus[j].plan),
							corpus[k].name, inPlanProfile(corpus[k].plan))
					}
				}
			}
		}
	}

	// --- Property 3: totality. Distinct candidates must not tie; the chain
	// ends in a plan-hash tie-break, so a tie means a rung ABOVE it returned
	// the same non-zero sign in both orientations and swallowed the chain. ---
	totalityViolations := 0
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if cmp[i][j] == 0 && cmp[j][i] == 0 {
				totalityViolations++
				if totalityViolations <= 20 {
					t.Errorf("TOTALITY VIOLATION: %s and %s tie despite distinct plans\n  %s: %s\n  %s: %s",
						corpus[i].name, corpus[j].name,
						corpus[i].name, inPlanProfile(corpus[i].plan),
						corpus[j].name, inPlanProfile(corpus[j].plan))
				}
			}
		}
	}

	// --- Property 4: permutation-independent minimum selection, over the
	// same linear fold OptimizeGroup runs. ---
	minimum := func(order []int) int {
		best := order[0]
		for _, candidate := range order[1:] {
			if less(candidate, best) {
				best = candidate
			}
		}
		return best
	}
	m := len(all)
	baseline := minimum(all)
	permutationViolations := 0
	permutations := 0
	for reverse := 0; reverse < 2; reverse++ {
		for shift := 0; shift < m; shift++ {
			order := make([]int, m)
			for position := 0; position < m; position++ {
				offset := position
				if reverse == 1 {
					offset = m - 1 - position
				}
				order[position] = all[(shift+offset)%m]
			}
			permutations++
			if winner := minimum(order); winner != baseline {
				permutationViolations++
				if permutationViolations <= 20 {
					t.Errorf("PERMUTATION VIOLATION: rotation=%d reverse=%v chose %s, baseline chose %s",
						shift, reverse == 1, corpus[winner].name, corpus[baseline].name)
				}
			}
		}
	}

	if antisymViolations == 0 && transitivityViolations == 0 &&
		totalityViolations == 0 && permutationViolations == 0 {
		t.Logf("order properties hold over all %d plans (%d of them primary-scan-rooted): "+
			"%d unordered pairs (irreflexivity, antisymmetry, totality); %d ordered triples "+
			"and %d permutations (transitivity, permutation-independence); winner=%s",
			n, numPrimaryScans, pairs, triples, permutations, corpus[baseline].name)
	}
}

// TestCostModel_UnsargedInPlansRankedByRealRungs pins the concrete symptom.
//
// Two IN-joins, neither of whose bindings became a search argument, differing
// only in how much work they do below the IN operator: one reads the index and
// fetches three times, the other reads and fetches once. That is a genuine
// fetch-count difference the chain's fetch rung is there to decide.
//
// Under the pre-fix rung both orientations returned +1 (each call said its own
// LEFT argument was worse), so `less` was false in both directions and the
// fetch rung — along with every other rung, including the full cost model —
// never ran. The winner was whatever costExprHash happened to order first,
// which is unrelated to how expensive the plans are.
func TestCostModel_UnsargedInPlansRankedByRealRungs(t *testing.T) {
	t.Parallel()

	cheap := plans.NewRecordQueryInJoinPlan(
		plans.NewRecordQueryFetchFromPartialRecordPlan(
			plans.NewRecordQueryIndexPlan("idx_unsarged_cheap", nil, []string{"T"}, values.UnknownType, false),
			nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey),
		"bind_cheap", false, false)

	expensive := plans.NewRecordQueryInJoinPlan(
		plans.NewRecordQueryFetchFromPartialRecordPlan(
			plans.NewRecordQueryFetchFromPartialRecordPlan(
				plans.NewRecordQueryFetchFromPartialRecordPlan(
					plans.NewRecordQueryIndexPlan("idx_unsarged_expensive", nil, []string{"T"}, values.UnknownType, false),
					nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey),
				nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey),
			nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey),
		"bind_expensive", false, false)

	// Preconditions: both are unSARGed IN-plans (penalty 1), and the fetch
	// rung genuinely separates them.
	for _, c := range []struct {
		name string
		plan plans.RecordQueryPlan
	}{{"cheap", cheap}, {"expensive", expensive}} {
		penalty, applicable := compareInOperator(c.plan)
		if !applicable || penalty != 1 {
			t.Fatalf("%s: compareInOperator = (%d,%v), want (1,true)", c.name, penalty, applicable)
		}
	}
	cheapOps := concretePlanCounts(cheap, nil)
	expensiveOps := concretePlanCounts(expensive, nil)
	if cheapOps.fetchCount != 1 || expensiveOps.fetchCount != 3 {
		t.Fatalf("fetch precondition = (%d,%d), want (1,3)", cheapOps.fetchCount, expensiveOps.fetchCount)
	}

	// The rung itself must TIE two equally-penalised IN-plans.
	if got := compareInPlan(cheap, expensive, cheapOps, expensiveOps); got != 0 {
		t.Fatalf("compareInPlan(unsarged, unsarged) = %d, want 0 so the fetch rung can decide", got)
	}

	assertStrictPlanningPreference(t, PlanningCostModelLess, cheap, expensive)

	// And the verdict must be the fetch rung's, not the hash's: assert the
	// hash would have ordered them the OTHER way, so a hash-decided outcome
	// is distinguishable from a rung-decided one.
	if costExprHash(cheap) < costExprHash(expensive) {
		t.Fatalf("adversary missing: the hash tie-break already prefers the cheap plan "+
			"(hash %d < %d), so this test cannot tell a rung verdict from a coin flip",
			costExprHash(cheap), costExprHash(expensive))
	}
}

// TestCostModel_SargedInPlanBeatsUnsargedThroughFullChain pins the second
// broken pair shape end to end through the whole comparator, with the later
// rungs deliberately pointing the OTHER way.
//
// The SARGed IN-plan carries an extra fetch, so every structural rung below
// criterion #6 prefers the unSARGed candidate. Under the pre-fix rung
// compare(sarged, unsarged) abstained and let those rungs hand the win to the
// unSARGed plan, while compare(unsarged, sarged) short-circuited to +1 —
// neither direction was `less`, and the winner fell to the hash. The IN
// penalty is supposed to be decisive here: an IN-transformation that produced
// a real search argument beats one that did not.
func TestCostModel_SargedInPlanBeatsUnsargedThroughFullChain(t *testing.T) {
	t.Parallel()

	bindingName := "in_bind"
	sarged := plans.NewRecordQueryInJoinPlan(
		plans.NewRecordQueryFetchFromPartialRecordPlan(
			plans.NewRecordQueryIndexPlan(
				"idx_chain_sarged",
				[]*predicates.ComparisonRange{rungEqualityRange(
					t, values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(bindingName)))},
				[]string{"T"}, values.UnknownType, false),
			nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey),
		bindingName, false, false)

	unsarged := plans.NewRecordQueryInJoinPlan(
		plans.NewRecordQueryIndexPlan("idx_chain_unsarged", nil, []string{"T"}, values.UnknownType, false),
		"other_bind", false, false)

	if penalty, applicable := compareInOperator(sarged); !applicable || penalty != 0 {
		t.Fatalf("sarged compareInOperator = (%d,%v), want (0,true)", penalty, applicable)
	}
	if penalty, applicable := compareInOperator(unsarged); !applicable || penalty != 1 {
		t.Fatalf("unsarged compareInOperator = (%d,%v), want (1,true)", penalty, applicable)
	}
	// Adversary: the fetch rung below criterion #6 prefers the unSARGed plan.
	sargedOps := concretePlanCounts(sarged, nil)
	unsargedOps := concretePlanCounts(unsarged, nil)
	if sargedOps.fetchCount <= unsargedOps.fetchCount {
		t.Fatalf("adversary missing: sarged fetches=%d must exceed unsarged fetches=%d",
			sargedOps.fetchCount, unsargedOps.fetchCount)
	}

	assertStrictPlanningPreference(t, PlanningCostModelLess, sarged, unsarged)
}

// TestOptimizeGroup_InPlanWinnerIsInsertionOrderIndependent is the
// user-visible property CQ-10 names, exercised through the real group
// optimizer rather than the comparator alone: the same memo contents inserted
// in any order must elect the same winner.
//
// OptimizeGroup's fold is `if bestFinal == nil || costModel(m, bestFinal)`
// over FinalMembers — a plain linear minimum with NO tie-break. A comparator
// that answers "not less" in both directions therefore hands the decision
// straight to arrival order, which is why this test drives the real task
// instead of asserting on the comparator.
func TestOptimizeGroup_InPlanWinnerIsInsertionOrderIndependent(t *testing.T) {
	t.Parallel()

	// Four IN-rooted candidates: one SARGed and cheap (the intended winner),
	// one SARGed and costlier, and two unSARGed ones — the pair whose mutual
	// comparison used to be decided by nothing at all.
	build := func() []expressions.RelationalExpression {
		sargedRange := func(binding string) []*predicates.ComparisonRange {
			return []*predicates.ComparisonRange{rungEqualityRange(
				t, values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(binding)))}
		}
		sargedCheap := plans.NewRecordQueryInJoinPlan(
			plans.NewRecordQueryIndexPlan("idx_group_sarged_cheap", sargedRange("g_sc"),
				[]string{"T"}, values.UnknownType, false),
			"g_sc", false, false)
		sargedCostly := plans.NewRecordQueryInJoinPlan(
			plans.NewRecordQueryFetchFromPartialRecordPlan(
				plans.NewRecordQueryIndexPlan("idx_group_sarged_costly", sargedRange("g_sk"),
					[]string{"T"}, values.UnknownType, false),
				nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey),
			"g_sk", false, false)
		unsargedA := plans.NewRecordQueryInJoinPlan(
			plans.NewRecordQueryIndexPlan("idx_group_unsarged_a", nil,
				[]string{"T"}, values.UnknownType, false),
			"g_ua", false, false)
		unsargedB := plans.NewRecordQueryInJoinPlan(
			plans.NewRecordQueryFetchFromPartialRecordPlan(
				plans.NewRecordQueryIndexPlan("idx_group_unsarged_b", nil,
					[]string{"T"}, values.UnknownType, false),
				nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey),
			"g_ub", false, false)
		return []expressions.RelationalExpression{sargedCheap, sargedCostly, unsargedA, unsargedB}
	}

	// Every permutation of four members.
	var permute func(prefix, rest []int) [][]int
	permute = func(prefix, rest []int) [][]int {
		if len(rest) == 0 {
			out := make([]int, len(prefix))
			copy(out, prefix)
			return [][]int{out}
		}
		var all [][]int
		for i := range rest {
			next := make([]int, 0, len(rest)-1)
			next = append(next, rest[:i]...)
			next = append(next, rest[i+1:]...)
			all = append(all, permute(append(prefix, rest[i]), next)...)
		}
		return all
	}
	orders := permute(nil, []int{0, 1, 2, 3})
	if len(orders) != 24 {
		t.Fatalf("expected 24 permutations, got %d", len(orders))
	}

	winnerFor := func(order []int) string {
		members := build()
		ref := expressions.InitialOf(
			expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
		for _, idx := range order {
			ref.InsertFinal(members[idx])
		}
		p := NewPlanner(nil, nil)
		p.constraintMap = NewConstraintMap()
		(&OptimizeGroupTask{Phase: PhasePlanning, Ref: ref}).Run(plannerTestContext(), p)
		w := ref.Winner()
		if w == nil {
			t.Fatalf("no winner elected for insertion order %v", order)
		}
		plan, ok := w.(plans.RecordQueryPlan)
		if !ok {
			t.Fatalf("winner %T is not a RecordQueryPlan", w)
		}
		return plan.Explain()
	}

	baseline := winnerFor(orders[0])
	for _, order := range orders[1:] {
		if got := winnerFor(order); got != baseline {
			t.Fatalf("INSERTION-ORDER DEPENDENCE: order %v elected\n  %s\nbut order %v elected\n  %s",
				order, got, orders[0], baseline)
		}
	}

	// And the elected winner must be the SARGed, fetch-free candidate — not
	// merely stable, but stably RIGHT.
	if want := "idx_group_sarged_cheap"; !strings.Contains(baseline, want) {
		t.Fatalf("winner %q does not scan %s; the IN penalty did not decide", baseline, want)
	}
}
