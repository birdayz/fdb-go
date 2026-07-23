package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// RFC-190 190.2 — cost-comparator transitivity regression.
//
// Before 190.2, planningCostModelCompareWith gated five structural rungs
// (typeFilterDepth, fetchDepth, distinctDepth, unmatchedFieldCount,
// mapFilter) on both candidates being sort-free, while its inMemorySortCount
// tiebreak ran after several ungated rungs. A gated rung could therefore
// decide a sort-free pair but be skipped when either side carried a sort,
// letting a different rung decide another pair in the same triple. That
// produced a non-transitive 3-cycle and made linear minimum selection
// insertion-order-dependent. The current comparator promotes sort count
// ahead of every affected structural rung and makes those rungs unconditional.

// planChainSpec builds a synthetic physical plan chain purely to exercise the
// comparator's structural rungs. The leaf is always a single non-covering,
// fully-unbound RecordQueryIndexPlan: indexScanCount=1, scanCount=0,
// coveringIndexCount=0 — so criterion #4 (data-access count) ties across the
// WHOLE set, and criterion #2 (max cardinality) is unknown for every plan
// (no equality binds anywhere), so it never discriminates either. Every
// difference between two chains traces to the structural rungs under test.
type planChainSpec struct {
	name string

	sorted bool // wrap the whole chain in a RecordQueryInMemorySortPlan

	topMaps int // RecordQueryMapPlan layers at the very top (feeds mapCount / the typeFilterDepth position)

	typeFilter bool // insert exactly ONE single-record-type TypeFilter below the top maps
	// (typeFilterCount is 0 or 1 for every spec in this file — ties whenever
	// both sides have one, so the DEPTH rung is what actually discriminates)

	fetches int // RecordQueryFetchFromPartialRecordPlan layers directly over the leaf

	distinct    bool // insert a RecordQueryDistinctPlan
	distinctTop bool // true: Distinct sits ABOVE the fetches (below TypeFilter); false: directly over the leaf

	// predFilterNodes stacks N zero-predicate RecordQueryPredicatesFilterPlan
	// layers directly above the TypeFilter — mapFilter (mapCount +
	// predicatesFilterCount) diversity. Zero predicates keeps CNF size (and
	// so criterion #3, residual predicates) tied at 0 regardless of N, so a
	// pair differing only in N isolates the mapFilter rung's NODE count from
	// the residual-predicate count — the same shape as a pushdown split that
	// turns one filter node into two without adding a conjunct.
	predFilterNodes int

	// indexColumns registers this spec's index name as a MatchCandidate with
	// N declared (fully-unbound) columns — unmatchedFieldCount diversity
	// (concretePlanCounts.unmatchedFieldCount = declared columns − bound
	// columns; every spec's ScanComparisons are nil, so it is exactly N).
	// Zero means "no candidate registered", i.e. unmatchedFieldCount stays 0
	// (indexMetadata's not-found fallback), matching every other spec in the
	// corpus.
	indexColumns int
}

func (s planChainSpec) indexName() string { return "idx_" + s.name }

func (s planChainSpec) build() plans.RecordQueryPlan {
	var cur plans.RecordQueryPlan = plans.NewRecordQueryIndexPlan(
		s.indexName(), nil, []string{"T"}, values.UnknownType, false)
	if s.distinct && !s.distinctTop {
		cur = plans.NewRecordQueryDistinctPlan(cur)
	}
	for i := 0; i < s.fetches; i++ {
		cur = plans.NewRecordQueryFetchFromPartialRecordPlan(cur, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey)
	}
	if s.distinct && s.distinctTop {
		cur = plans.NewRecordQueryDistinctPlan(cur)
	}
	if s.typeFilter {
		cur = plans.NewRecordQueryTypeFilterPlan([]string{"T"}, cur)
	}
	for i := 0; i < s.predFilterNodes; i++ {
		cur = plans.NewRecordQueryPredicatesFilterPlan(cur, nil)
	}
	for i := 0; i < s.topMaps; i++ {
		cur = plans.NewRecordQueryMapPlan(cur, nil)
	}
	if s.sorted {
		cur = plans.NewRecordQueryInMemorySortPlan(cur, nil)
	}
	return cur
}

// transitivityMatchCandidate is a minimal MatchCandidate carrying only a name
// and a declared column-name list — enough to drive
// concretePlanCounts/indexMetadata's unmatchedFieldCount computation. Every
// other MatchCandidate method is unused by the cost model's concrete-plan
// walk and returns a zero value.
type transitivityMatchCandidate struct {
	name    string
	columns []string
}

func (c transitivityMatchCandidate) CandidateName() string    { return c.name }
func (c transitivityMatchCandidate) GetColumnNames() []string { return c.columns }
func (c transitivityMatchCandidate) GetSargableAliases() []values.CorrelationIdentifier {
	return nil
}
func (c transitivityMatchCandidate) GetRecordTypes() []string { return nil }
func (c transitivityMatchCandidate) IsUnique() bool           { return false }
func (c transitivityMatchCandidate) GetTraversal() *Traversal { return nil }
func (c transitivityMatchCandidate) ComputeBoundParameterPrefixMap(
	_ map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	return nil
}

func (c transitivityMatchCandidate) ToScanPlan(
	_ map[values.CorrelationIdentifier]*predicates.ComparisonRange, _ bool,
) plans.RecordQueryPlan {
	return nil
}

// buildTransitivityCtx registers a MatchCandidate for every spec with a
// non-zero indexColumns, so concretePlanCounts resolves a real
// unmatchedFieldCount for those specs via indexMetadata. Specs that don't set
// indexColumns are simply absent from the candidate list, so indexMetadata's
// not-found fallback (nil, false) applies to them exactly as it does under a
// nil PlanContext — this ctx is a strict superset of "no metadata" and does
// not perturb any of the pre-existing corpus's comparisons.
func buildTransitivityCtx(specs []planChainSpec) PlanContext {
	var candidates []MatchCandidate
	for _, s := range specs {
		if s.indexColumns <= 0 {
			continue
		}
		cols := make([]string, s.indexColumns)
		for i := range cols {
			cols[i] = fmt.Sprintf("c%d", i)
		}
		candidates = append(candidates, transitivityMatchCandidate{name: s.indexName(), columns: cols})
	}
	return &indexTestPlanContext{candidates: candidates}
}

// nljChainSpec builds a two-leaf materialized nested-loop join, for
// nljPredicateCount / join-shape diversity (best-effort coverage — NLJ pairs
// route through compareJoinOrdering/residual-predicate-count first, so they
// are not depended on to produce the pinned cycle below, only to broaden the
// brute-force sweep).
func nljPlan(name string, numPredicates int) plans.RecordQueryPlan {
	outer := plans.NewRecordQueryIndexPlan("idx_"+name+"_outer", nil, []string{"T"}, values.UnknownType, false)
	inner := plans.NewRecordQueryIndexPlan("idx_"+name+"_inner", nil, []string{"T"}, values.UnknownType, false)
	preds := make([]predicates.QueryPredicate, numPredicates)
	for i := range preds {
		preds[i] = predicates.NewComparisonPredicate(
			&values.FieldValue{Field: fmt.Sprintf("f%d", i), Typ: values.TypeInt},
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(i)),
		)
	}
	return plans.NewRecordQueryNestedLoopJoinPlan(outer, inner, preds, plans.JoinInner, "o", "i", nil)
}

// namedPlan pairs a plan with a label for failure reporting.
type namedPlan struct {
	name string
	plan plans.RecordQueryPlan
}

// buildFetchDepthInJoinCycle returns a SECOND, independently-derived
// sort-bearing 3-cycle (RFC-190 190.2 Fix 3), distinct from the A/B/C
// pinned cycle above: A/B/C is keyed on typeFilterDepth (the gated decisive
// rung for the sort-free pair) with fetch-COUNT as the single fallback
// decider for both sort-bearing pairs. This second cycle is keyed on
// fetchDepth as the gated decisive rung, and — critically — uses TWO
// DIFFERENT fallback deciders for its two sort-bearing pairs (inJoinCount
// for one, the OLD low-position inMemorySortCount rung itself for the
// other), which is the sharper form of "two different rungs governing the
// same slot" this whole fix targets.
//
//	D2 = IndexScan (bare leaf)                    sort=0 inJoinCount=0 fetchDepth=0
//	E2 = Map(InJoin(IndexScan))                    sort=0 inJoinCount=1 fetchDepth=2
//	F2 = InMemorySort(Map(InJoin(IndexScan)))      sort=1 inJoinCount=1 fetchDepth=2 (irrelevant — gate skipped)
//
// Under the PRE-190.2 comparator (gated depth rungs, inMemorySortCount at
// its OLD low position, matching oldGatedCompare in the fix's derivation):
//
//	compare(D2,E2): both sort=0 -> fetchDepth GATE ACTIVE ("shallower wins"):
//	                depth(D2)=0 < depth(E2)=2 -> D2 wins -> compare(D2,E2) = -1  (D2<E2)
//	compare(E2,F2): F2.sort=1  -> fetchDepth GATE SKIPPED. fetch-COUNT ties (1=1, same
//	                underlying Map+IndexScan+InJoin core). inJoinCount and mapCount TIE too
//	                (F2 is literally E2 wrapped in a sort). The Map roots deliberately keep
//	                compareInPlan from intercepting this structural regression. Falls all
//	                the way to the OLD low-position inMemorySortCount rung: sort(E2)=0 <
//	                sort(F2)=1 -> E2 wins -> compare(E2,F2) = -1  (E2<F2)
//	compare(F2,D2): F2.sort=1  -> fetchDepth GATE SKIPPED. fetch-COUNT ties (1=1). This
//	                time inJoinCount DIFFERS (F2=1 vs D2=0, since D2 has no InJoin at all)
//	                -> the UNGATED inJoinCount rung decides FIRST, before ever reaching the
//	                sort-count rung: more inJoins wins -> F2 wins -> compare(F2,D2) = -1  (F2<D2)
//
//	D2<E2, E2<F2, F2<D2: a second 3-cycle, decided by THREE different rungs across
//	its three pairs (fetchDepth, inMemorySortCount, inJoinCount) — sharper evidence
//	that "which rung decides" is pair-dependent under the old gate, not just a
//	single alternate fallback as in A/B/C.
func buildFetchDepthInJoinCycle() []namedPlan {
	d2 := plans.NewRecordQueryIndexPlan("idx_D2", nil, []string{"T"}, values.UnknownType, false)
	e2 := plans.NewRecordQueryMapPlan(
		plans.NewRecordQueryInJoinPlan(
			plans.NewRecordQueryIndexPlan("idx_E2", nil, []string{"T"}, values.UnknownType, false),
			"b", false, false),
		nil)
	f2 := plans.NewRecordQueryInMemorySortPlan(
		plans.NewRecordQueryMapPlan(
			plans.NewRecordQueryInJoinPlan(
				plans.NewRecordQueryIndexPlan("idx_F2", nil, []string{"T"}, values.UnknownType, false),
				"b", false, false),
			nil),
		nil)
	return []namedPlan{
		{name: "D2_bare_leaf", plan: d2},
		{name: "E2_map_injoin_leaf", plan: e2},
		{name: "F2_sorted_map_injoin_leaf", plan: f2},
	}
}

// buildTransitivityCorpus returns the diverse plan set TestCostModel_SortGateCycleRegression
// scans. Plans "A", "B", "C" are the PINNED minimal 3-cycle (see the derivation in the
// comparator-order trace below); the rest are structural-diversity filler that broadens
// the brute-force pairwise/triple sweep without being load-bearing for the pinned cycle.
//
// Derivation of the A/B/C cycle (nil stats, nil ctx — PlanningCostModelLess):
//
//	A = Map(Map(Map(TypeFilter(Fetch(Fetch(IndexScan))))))   sort=0 typeFilterDepth=3 fetch=1+2=3
//	B = Map(TypeFilter(IndexScan))                            sort=0 typeFilterDepth=1 fetch=1+0=1
//	C = InMemorySort(TypeFilter(Fetch(IndexScan)))             sort=1 (depth irrelevant)  fetch=1+1=2
//
//	compare(A,B): both sort=0 -> typeFilterDepth GATE is ACTIVE (Java "deeper wins"):
//	              depth(A)=3 > depth(B)=1 -> A wins -> compare(A,B) = -1  (A<B)
//	compare(B,C): C.sort=1    -> typeFilterDepth GATE SKIPPED. Falls through to the
//	              UNGATED fetch-count rung: fetch(B)=1 < fetch(C)=2 -> B wins -> compare(B,C) = -1  (B<C)
//	compare(C,A): C.sort=1    -> typeFilterDepth GATE SKIPPED. UNGATED fetch-count rung:
//	              fetch(C)=2 < fetch(A)=3 -> C wins -> compare(C,A) = -1  (C<A)
//
//	A<B, B<C, C<A: a 3-cycle. No pairwise antisymmetry violation (each pair is
//	individually antisymmetric — intCompare and the gate condition are both
//	symmetric in (a,b)) — the bug is purely a TRANSITIVITY violation.
func buildTransitivityCorpus() ([]namedPlan, PlanContext) {
	specs := []planChainSpec{
		// --- The pinned minimal cycle ---
		{name: "A_sortfree_deepTF_fetch2", sorted: false, topMaps: 3, typeFilter: true, fetches: 2},
		{name: "B_sortfree_shallowTF_fetch0", sorted: false, topMaps: 1, typeFilter: true, fetches: 0},
		{name: "C_sorted_TF_fetch1", sorted: true, topMaps: 0, typeFilter: true, fetches: 1},

		// --- Structural-diversity filler (sort-free family) ---
		{name: "sf_tf0_f0", sorted: false, topMaps: 0, typeFilter: false, fetches: 0},
		{name: "sf_tf0_f1", sorted: false, topMaps: 0, typeFilter: false, fetches: 1},
		{name: "sf_tf0_f3", sorted: false, topMaps: 0, typeFilter: false, fetches: 3},
		{name: "sf_tf_depth0_f0", sorted: false, topMaps: 0, typeFilter: true, fetches: 0},
		{name: "sf_tf_depth2_f1", sorted: false, topMaps: 2, typeFilter: true, fetches: 1},
		{name: "sf_tf_depth2_f2", sorted: false, topMaps: 2, typeFilter: true, fetches: 2},
		{name: "sf_tf_depth4_f0", sorted: false, topMaps: 4, typeFilter: true, fetches: 0},
		{name: "sf_distinctBottom_f2", sorted: false, topMaps: 1, typeFilter: true, fetches: 2, distinct: true, distinctTop: false},
		{name: "sf_distinctTop_f2", sorted: false, topMaps: 1, typeFilter: true, fetches: 2, distinct: true, distinctTop: true},
		{name: "sf_distinct_f0", sorted: false, topMaps: 0, typeFilter: false, fetches: 0, distinct: true, distinctTop: false},

		// --- Structural-diversity filler (sorted family) ---
		{name: "sorted_tf0_f0", sorted: true, topMaps: 0, typeFilter: false, fetches: 0},
		{name: "sorted_tf0_f2", sorted: true, topMaps: 0, typeFilter: false, fetches: 2},
		{name: "sorted_tf_depth3_f0", sorted: true, topMaps: 3, typeFilter: true, fetches: 0},
		{name: "sorted_tf_depth1_f3", sorted: true, topMaps: 1, typeFilter: true, fetches: 3},
		{name: "sorted_distinctTop_f1", sorted: true, topMaps: 0, typeFilter: true, fetches: 1, distinct: true, distinctTop: true},
		{name: "sorted_distinctBottom_f0", sorted: true, topMaps: 2, typeFilter: false, fetches: 0, distinct: true, distinctTop: false},
		{name: "sorted_manyMaps_f1", sorted: true, topMaps: 5, typeFilter: true, fetches: 1},

		// --- unmatchedFieldCount diversity (RFC-190 190.2 Verify step 1): a
		// matched pair (same shape, only indexColumns differs) in both
		// sort-free and sorted flavors, so the now-ungated unmatchedFieldCount
		// rung (#12) gets exercised standalone and cross-sort. ---
		{name: "uf_zero_sf", sorted: false, topMaps: 1, typeFilter: true, fetches: 1, indexColumns: 0},
		{name: "uf_three_sf", sorted: false, topMaps: 1, typeFilter: true, fetches: 1, indexColumns: 3},
		{name: "uf_zero_sorted", sorted: true, topMaps: 1, typeFilter: true, fetches: 1, indexColumns: 0},
		{name: "uf_three_sorted", sorted: true, topMaps: 1, typeFilter: true, fetches: 1, indexColumns: 3},

		// --- mapFilter (mapCount+predicatesFilterCount) diversity (RFC-190
		// 190.2 Verify step 1): a pushdown-split proxy — one vs two
		// zero-predicate PredicatesFilter nodes (CNF size 0, so criterion #3
		// ties) — in both sort-free and sorted flavors, exercising the
		// now-ungated mapFilter rung (#14) plus its interaction with the
		// now-ungated depth rungs above it. ---
		{name: "split1_sortfree", sorted: false, typeFilter: true, predFilterNodes: 1},
		{name: "split2_sortfree", sorted: false, typeFilter: true, predFilterNodes: 2},
		{name: "split1_sorted", sorted: true, typeFilter: true, predFilterNodes: 1},
		{name: "split2_sorted", sorted: true, typeFilter: true, predFilterNodes: 2},
	}

	ctx := buildTransitivityCtx(specs)

	out := make([]namedPlan, 0, len(specs)+6)
	for _, s := range specs {
		out = append(out, namedPlan{name: s.name, plan: s.build()})
	}
	// Deliberate comparator-equivalent clone. The stable hash includes index
	// identity, so independently named candidates otherwise never tie and a
	// tie-consistency loop would be vacuous.
	out = append(out, namedPlan{
		name: "sf_tf0_f0_equivalent",
		plan: plans.NewRecordQueryIndexPlan(
			"idx_sf_tf0_f0", nil, []string{"T"}, values.UnknownType, false),
	})
	// NLJ diversity — nljPredicateCount / join-shape coverage.
	out = append(out, namedPlan{name: "nlj_1pred", plan: nljPlan("nlj1", 1)})
	out = append(out, namedPlan{name: "nlj_3pred", plan: nljPlan("nlj3", 3)})
	// Second pinned cycle (RFC-190 190.2 Fix 3) — see buildFetchDepthInJoinCycle's
	// doc comment for the exact rung-by-rung derivation.
	out = append(out, buildFetchDepthInJoinCycle()...)
	return out, ctx
}

// planProfile renders the operator-count profile the comparator's structural
// rungs consume, for failure reporting.
func planProfile(p plans.RecordQueryPlan, ctx PlanContext) string {
	c := concretePlanCounts(p, ctx)
	tfDepth := costExprDepth(p, matchTypeFilter)
	fetchDepth := costExprDepth(p, matchFetch)
	distDepth := costExprDepth(p, matchDistinct)
	return fmt.Sprintf(
		"sort=%d typeFilterCount=%d typeFilterDepth=%d idxScan=%d fetchNodes=%d fetchTotal=%d fetchDepth=%d "+
			"distinctDepth=%d mapCount=%d predFilterCount=%d nljPredCount=%d unmatchedField=%d",
		c.inMemorySortCount, c.typeFilterCount, tfDepth, c.indexScanCount, c.fetchCount,
		c.indexScanCount+c.fetchCount, fetchDepth, distDepth, c.mapCount, c.predicatesFilterCount,
		c.nljPredicateCount, c.unmatchedFieldCount)
}

// TestCostModel_SortGateCycleRegression is the RFC-190 190.2 RED/GREEN test.
// It brute-forces every pair and triple over a diverse corpus of physical plan
// chains, asserting antisymmetry, strict-order transitivity, and consistency
// of comparator ties. The corpus exercises the five formerly sort-gated
// structural rungs against the later rungs that exposed the old cycle. It is
// deliberately an all-index, sort-gate-scoped corpus; it does not claim a
// global total-order contract across Java's retained applicability gates.
//
// Before the RFC-190 190.2 fix (moving the sort-count tiebreak to just before
// the typeFilterCount rung, and dropping the now-moot per-rung sort gates),
// this test finds a genuine 3-cycle: plans A, B, C with compare(A,B)<0,
// compare(B,C)<0, compare(C,A)<0 — see buildTransitivityCorpus's doc comment
// for the exact rung-by-rung derivation.
func TestCostModel_SortGateCycleRegression(t *testing.T) {
	t.Parallel()

	corpus, ctx := buildTransitivityCorpus()

	// Pairwise compare matrix.
	n := len(corpus)
	cmp := make([][]int, n)
	for i := range cmp {
		cmp[i] = make([]int, n)
	}
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i == j {
				continue
			}
			cmp[i][j] = planningCostModelCompareWith(corpus[i].plan, corpus[j].plan, nil, ctx)
		}
	}
	less := func(i, j int) bool { return cmp[i][j] < 0 }

	// Pin the repaired orientation of both pre-190.2 cycles explicitly; the
	// property sweep below must not be allowed to pass after corpus drift has
	// accidentally removed either load-bearing triple.
	byName := make(map[string]int, n)
	for i, candidate := range corpus {
		byName[candidate.name] = i
	}
	assertLess := func(left, right string) {
		t.Helper()
		i, iOK := byName[left]
		j, jOK := byName[right]
		if !iOK || !jOK {
			t.Fatalf("missing pinned candidate(s): %q=%v %q=%v", left, iOK, right, jOK)
		}
		if !less(i, j) {
			t.Errorf("pinned order: compare(%s,%s)=%d, want < 0\n  %s: %s\n  %s: %s",
				left, right, cmp[i][j],
				left, planProfile(corpus[i].plan, ctx),
				right, planProfile(corpus[j].plan, ctx))
		}
	}
	assertLess("A_sortfree_deepTF_fetch2", "B_sortfree_shallowTF_fetch0")
	assertLess("B_sortfree_shallowTF_fetch0", "C_sorted_TF_fetch1")
	assertLess("A_sortfree_deepTF_fetch2", "C_sorted_TF_fetch1")
	assertLess("D2_bare_leaf", "E2_map_injoin_leaf")
	assertLess("E2_map_injoin_leaf", "F2_sorted_map_injoin_leaf")
	assertLess("D2_bare_leaf", "F2_sorted_map_injoin_leaf")

	// --- Property 1: antisymmetry. compare(a,b) == -compare(b,a). ---
	antisymViolations := 0
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if cmp[i][j] != -cmp[j][i] {
				antisymViolations++
				if antisymViolations <= 20 {
					t.Errorf("ANTISYMMETRY VIOLATION: compare(%s,%s)=%d but compare(%s,%s)=%d (want %d)\n  %s: %s\n  %s: %s",
						corpus[i].name, corpus[j].name, cmp[i][j],
						corpus[j].name, corpus[i].name, cmp[j][i], -cmp[i][j],
						corpus[i].name, planProfile(corpus[i].plan, ctx),
						corpus[j].name, planProfile(corpus[j].plan, ctx))
				}
			}
		}
	}

	// --- Property 2: strict-order transitivity. ---
	// If i < j and j < k, then i must be strictly less than k. Requiring a
	// strict result catches both a 3-cycle and the subtler i < j < k but
	// i ties k case that would still make minimum selection order-dependent.
	transitivityViolations := 0
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			for k := 0; k < n; k++ {
				if less(i, j) && less(j, k) && !less(i, k) {
					transitivityViolations++
					if transitivityViolations <= 20 {
						t.Errorf("TRANSITIVITY VIOLATION: %s < %s and %s < %s, but compare(%s,%s)=%d\n"+
							"  compare(%s,%s)=%d compare(%s,%s)=%d\n"+
							"  %s: %s\n  %s: %s\n  %s: %s",
							corpus[i].name, corpus[j].name, corpus[j].name, corpus[k].name,
							corpus[i].name, corpus[k].name, cmp[i][k],
							corpus[i].name, corpus[j].name, cmp[i][j],
							corpus[j].name, corpus[k].name, cmp[j][k],
							corpus[i].name, planProfile(corpus[i].plan, ctx),
							corpus[j].name, planProfile(corpus[j].plan, ctx),
							corpus[k].name, planProfile(corpus[k].plan, ctx))
					}
				}
			}
		}
	}

	// --- Property 3: tie consistency. ---
	// Comparator equality must be substitutable: if i ties j, every third
	// candidate k must relate to i and j in the same direction. This also
	// implies transitivity of the induced equivalence relation.
	tieViolations := 0
	tiesChecked := 0
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if less(i, j) || less(j, i) {
				continue
			}
			tiesChecked++
			for k := 0; k < n; k++ {
				if less(i, k) != less(j, k) || less(k, i) != less(k, j) {
					tieViolations++
					if tieViolations <= 20 {
						t.Errorf("TIE-CONSISTENCY VIOLATION: %s ~ %s, but their relation to %s differs\n"+
							"  compare(%s,%s)=%d compare(%s,%s)=%d\n"+
							"  compare(%s,%s)=%d compare(%s,%s)=%d",
							corpus[i].name, corpus[j].name, corpus[k].name,
							corpus[i].name, corpus[k].name, cmp[i][k],
							corpus[j].name, corpus[k].name, cmp[j][k],
							corpus[k].name, corpus[i].name, cmp[k][i],
							corpus[k].name, corpus[j].name, cmp[k][j])
					}
				}
			}
		}
	}
	if tiesChecked == 0 {
		t.Error("tie-consistency corpus is vacuous: no equivalent candidate pair")
	}

	if antisymViolations == 0 && transitivityViolations == 0 && tieViolations == 0 {
		t.Logf("strict weak ordering holds across the scoped %d-plan corpus (%d pairs, %d ordered triples, %d ties)",
			n, n*(n-1)/2, n*(n-1)*(n-2), tiesChecked)
	} else {
		t.Logf("found %d antisymmetry, %d transitivity, and %d tie-consistency violations across %d plans",
			antisymViolations, transitivityViolations, tieViolations, n)
	}
}
