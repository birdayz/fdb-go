package rowdiff

import (
	"fmt"
	"os"
	"strconv"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/core/embedded"
)

// eqRange builds a single-column equality ComparisonRange `= lit`.
func eqRange(t *testing.T, lit any) *predicates.ComparisonRange {
	t.Helper()
	res := predicates.EmptyComparisonRange().Merge(&predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: values.LiteralValue(lit),
	})
	if !res.Ok {
		t.Fatalf("EmptyComparisonRange().Merge(= %v) failed", lit)
	}
	return res.Range
}

// indexScan builds a synthetic RecordQueryIndexPlan carrying the metadata the
// ordering derivation reads: index key columns, primary-key columns, direction,
// and per-column scan comparisons.
func indexScan(cols, pk []string, reverse bool, comps ...*predicates.ComparisonRange) *plans.RecordQueryIndexPlan {
	p := plans.NewRecordQueryIndexPlan("IDX", comps, nil, nil, reverse)
	return p.WithIndexMetadata(cols, pk, false)
}

// The arms of the ordering derivation, as the sweep's vacuity floors name them.
// "other" is every node kind that ends the derivation with no claim.
const (
	orderingArmPrimaryKey = "primary-key-scan"
	orderingArmIndex      = "index-scan"
	orderingArmOther      = "other"
)

// orderingLeafArm classifies which arm of providedOrdering's type switch a sort's
// input reaches, by asking the derivation's OWN descent (orderingLeaf) rather
// than re-walking the tree — a census that walked separately could report an arm
// as reached that the derivation does not read.
//
// The bare and covering index plans share ONE arm because they share one
// derivation: the covering wrapper delegates every fact to the scan it wraps.
// Counting them apart would let the floor stay green on bare-index plans while
// the shape the access path actually emits went unread.
func orderingLeafArm(p plans.RecordQueryPlan) string {
	switch orderingLeaf(p).(type) {
	case *plans.RecordQueryScanPlan:
		return orderingArmPrimaryKey
	case *plans.RecordQueryIndexPlan, *plans.RecordQueryCoveringIndexPlan:
		return orderingArmIndex
	default:
		return orderingArmOther
	}
}

// TestOrderingLeafArm_EveryArm drives every arm of the sweep's vacuity census
// explicitly. The corpus reaches only the arms it happens to produce today, so
// an arm that is rare now — or that a pending planner change is about to make
// live — would otherwise ship untested and its first real firing would read as a
// finding rather than as an untested branch.
func TestOrderingLeafArm_EveryArm(t *testing.T) {
	t.Parallel()
	idx := indexScan([]string{"B"}, []string{"ID"}, false, predicates.EmptyComparisonRange())
	cov := plans.NewRecordQueryCoveringIndexPlan(idx)
	scan := plans.NewRecordQueryScanPlan(nil, nil, false)
	fetch := func(inner plans.RecordQueryPlan) plans.RecordQueryPlan {
		return plans.NewRecordQueryFetchFromPartialRecordPlan(inner, nil, nil, plans.FetchIndexRecordsPrimaryKey)
	}
	cases := []struct {
		name string
		plan plans.RecordQueryPlan
		want string
	}{
		{"bare primary-key scan", scan, orderingArmPrimaryKey},
		{"bare index scan", idx, orderingArmIndex},
		{"bare covering scan", cov, orderingArmIndex},
		{"fetch over index scan", fetch(idx), orderingArmIndex},
		{"fetch over covering scan", fetch(cov), orderingArmIndex},
		{"nested pass-throughs over a pk scan", fetch(fetch(scan)), orderingArmPrimaryKey},
		{"a sort ends the derivation", plans.NewRecordQueryInMemorySortPlan(scan, []plans.SortKey{{Field: "ID"}}), orderingArmOther},
		{"nil plan", nil, orderingArmOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := orderingLeafArm(tc.plan); got != tc.want {
				t.Fatalf("orderingLeafArm(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestCheckPlanOrdering_RedundantSort proves the redundant-sort invariant both
// FIRES on a sort over an already-ordered input and stays quiet on every sort
// that is genuinely required — the sensitivity net for the ORDERING check
// itself (RFC-184 W1), mirroring TestCheckPlanCost_SpuriousSort for the COST
// axis. Each CLEAN case is a shape that DOES appear in the corpus (a scan that
// does not provide the requested order, a mixed direction, a NULLS-placement
// difference), so a false positive here would be a nightly-net false alarm.
func TestCheckPlanOrdering_RedundantSort(t *testing.T) {
	t.Parallel()

	fwdScan := plans.NewRecordQueryScanPlan(nil, nil, false)
	revScan := plans.NewRecordQueryScanPlan(nil, nil, true)

	sortOver := func(inner plans.RecordQueryPlan, keys ...plans.SortKey) *plans.RecordQueryInMemorySortPlan {
		return plans.NewRecordQueryInMemorySortPlan(inner, keys)
	}
	idKey := plans.SortKey{Field: "ID"}
	idDescKey := plans.SortKey{Field: "ID", Desc: true}
	bKey := plans.SortKey{Field: "B"}
	aKey := plans.SortKey{Field: "A"}
	aDescKey := plans.SortKey{Field: "A", Desc: true}

	orderByID := Query{OrderBy: []OrderKey{{Col: "ID"}}}
	orderByIDDesc := Query{OrderBy: []OrderKey{{Col: "ID", Desc: true}}}
	orderByBID := Query{OrderBy: []OrderKey{{Col: "B"}, {Col: "ID"}}}

	// --- FIRES: the sort is provably redundant. ---

	// ORDER BY id over a forward PK scan — scan already emits ID ascending.
	if got := checkPlanOrdering(sortOver(fwdScan, idKey), orderByID); len(got) == 0 {
		t.Fatal("redundant sort over a forward PK scan (ORDER BY id) not flagged")
	}
	// ORDER BY id DESC over a reverse PK scan.
	if got := checkPlanOrdering(sortOver(revScan, idDescKey), orderByIDDesc); len(got) == 0 {
		t.Fatal("redundant sort over a reverse PK scan (ORDER BY id DESC) not flagged")
	}
	// ORDER BY id over an ALL-equality-bound index scan (index [A], A = 5) —
	// entries are (5, id), so the stream is already ID-ordered.
	allEqIdxA := indexScan([]string{"A"}, []string{"ID"}, false, eqRange(t, int64(5)))
	if got := checkPlanOrdering(sortOver(allEqIdxA, idKey), orderByID); len(got) == 0 {
		t.Fatal("redundant sort over an all-equality index scan (ORDER BY id) not flagged")
	}
	// ORDER BY b, id over a forward index [B] scan — provides (b, id).
	fwdIdxB := indexScan([]string{"B"}, []string{"ID"}, false, predicates.EmptyComparisonRange())
	if got := checkPlanOrdering(sortOver(fwdIdxB, bKey, idKey), orderByBID); len(got) == 0 {
		t.Fatal("redundant sort over a forward index [B] scan (ORDER BY b, id) not flagged")
	}
	// Same, but through an order-preserving FETCH pass-through above the scan
	// (the non-covering index shape the real planner produces).
	fetchIdxB := plans.NewRecordQueryFetchFromPartialRecordPlan(fwdIdxB, nil, nil, plans.FetchIndexRecordsPrimaryKey)
	if got := checkPlanOrdering(sortOver(fetchIdxB, bKey, idKey), orderByBID); len(got) == 0 {
		t.Fatal("redundant sort over Fetch(index [B] scan) not flagged (pass-through not walked)")
	}
	// Fetch(Covering(index [B] scan)) — the shape the access path ACTUALLY emits
	// for every index-backed access. The covering plan holds its index scan as a
	// FIELD, so nothing about the bare-index arm carries over to it: if the
	// derivation does not name the covering type, it answers "order unprovable"
	// and this detector goes silent on every index shape in the corpus — a clean
	// report produced by not running, which is the failure mode a nightly net
	// cannot afford.
	covIdxB := plans.NewRecordQueryCoveringIndexPlan(fwdIdxB)
	if got := checkPlanOrdering(sortOver(covIdxB, bKey, idKey), orderByBID); len(got) == 0 {
		t.Fatal("redundant sort over Covering(index [B] scan) not flagged — the ordering derivation does not reach the covering shape, so it reports clean by never running")
	}
	fetchCovIdxB := plans.NewRecordQueryFetchFromPartialRecordPlan(covIdxB, nil, nil, plans.FetchIndexRecordsPrimaryKey)
	if got := checkPlanOrdering(sortOver(fetchCovIdxB, bKey, idKey), orderByBID); len(got) == 0 {
		t.Fatal("redundant sort over Fetch(Covering(index [B] scan)) not flagged — this is the exact shape the access path emits")
	}
	// The covering wrapper must not merely be REACHED, it must derive the SAME
	// ordering as the scan it wraps: it reads the same physical range and emits
	// one row per entry. A covering arm that reached the code but read different
	// facts would flag and clear on different shapes than the bare arm.
	bare, cov := providedOrdering(fwdIdxB), providedOrdering(covIdxB)
	if len(bare) == 0 {
		t.Fatal("providedOrdering(bare index [B] scan) = nil — the reference arm of the comparison is itself dead")
	}
	if len(cov) != len(bare) {
		t.Fatalf("providedOrdering(Covering(idx)) = %v, want the wrapped scan's %v", cov, bare)
	}
	for i := range bare {
		if cov[i] != bare[i] {
			t.Fatalf("providedOrdering(Covering(idx))[%d] = %+v, want the wrapped scan's %+v", i, cov[i], bare[i])
		}
	}

	// --- The covering arm inherits the bare arm's CONSERVATISM too. ---
	//
	// Reaching the covering shape must not turn the detector into something that
	// knows more than the planner: a covering scan over a REVERSE index with a
	// mixed-direction ORDER BY stays clean, exactly as the bare shape does.
	covRevIdxA := plans.NewRecordQueryCoveringIndexPlan(
		indexScan([]string{"A"}, []string{"ID"}, true, predicates.EmptyComparisonRange()))
	orderByADescIDCov := Query{OrderBy: []OrderKey{{Col: "A", Desc: true}, {Col: "ID"}}}
	if got := checkPlanOrdering(sortOver(covRevIdxA, aDescKey, idKey), orderByADescIDCov); len(got) != 0 {
		t.Fatalf("covering scan with a mixed direction (a DESC, id ASC) flagged: %v", got)
	}

	// --- CLEAN: the sort is genuinely required (or out of scope). ---

	// ORDER BY b, id over a plain PK scan — the scan provides only [id], not
	// [b, id]. This is the exact corpus shape (full-scan + sort when no index
	// on b is chosen); flagging it would be a false positive.
	if got := checkPlanOrdering(sortOver(fwdScan, bKey, idKey), orderByBID); len(got) != 0 {
		t.Fatalf("sort over a PK scan that does NOT provide [b, id] flagged: %v", got)
	}
	// ORDER BY a DESC, id over a REVERSE index [A] scan — reverse gives
	// (a desc, id DESC) but the query wants id ASC: mixed direction, sort
	// needed.
	revIdxA := indexScan([]string{"A"}, []string{"ID"}, true, predicates.EmptyComparisonRange())
	orderByADescID := Query{OrderBy: []OrderKey{{Col: "A", Desc: true}, {Col: "ID"}}}
	if got := checkPlanOrdering(sortOver(revIdxA, aDescKey, idKey), orderByADescID); len(got) != 0 {
		t.Fatalf("sort with a mixed direction (a DESC, id ASC) flagged: %v", got)
	}
	// ORDER BY a NULLS LAST, id over a forward index [A] scan — forward emits
	// NULLs FIRST, so a NULLS LAST request genuinely needs the sort.
	fwdIdxA := indexScan([]string{"A"}, []string{"ID"}, false, predicates.EmptyComparisonRange())
	orderByANullsLast := Query{OrderBy: []OrderKey{{Col: "A", Nulls: NullsLast}, {Col: "ID"}}}
	if got := checkPlanOrdering(sortOver(fwdIdxA, aKey, idKey), orderByANullsLast); len(got) != 0 {
		t.Fatalf("sort over a nullable column with a NULLS-placement mismatch flagged: %v", got)
	}
	// No ORDER BY: nothing to be redundant against.
	if got := checkPlanOrdering(sortOver(fwdScan, bKey), Query{}); len(got) != 0 {
		t.Fatalf("sort under a no-ORDER-BY query flagged: %v", got)
	}
	// Guard: a sort whose keys do NOT line up with the ORDER BY is not treated
	// as the ORDER BY sort (skip, never a false flag).
	if got := checkPlanOrdering(sortOver(fwdScan, bKey), orderByID); len(got) != 0 {
		t.Fatalf("sort whose keys mismatch the ORDER BY flagged: %v", got)
	}
	// Out of scope: a join query's ordering is not derived from the physical
	// tree here, so even a sort over a forward PK scan is left alone.
	joinQ := Query{Join: &JoinSpec{}, OrderBy: []OrderKey{{Col: "ID"}}}
	if got := checkPlanOrdering(sortOver(fwdScan, idKey), joinQ); len(got) != 0 {
		t.Fatalf("join query flagged by the single-table ordering invariant: %v", got)
	}
}

// TestOrderingInvariant_PurePlannerSweep validates the redundant-sort invariant
// against the REAL planner over the generative corpus, with no FDB: every
// generated query is planned and checked, and a correct engine (whose
// sort-elimination path drops every redundant sort) must produce ZERO ordering
// violations. This is the permanent net — if a planner change ever starts
// keeping a sort over an already-ordered scan, this test goes red without a
// container. Observe-only telemetry (how many plans carried an in-memory sort,
// and how many of those had an unprovable input order) is logged so a shrinking
// reach is noticed.
func TestOrderingInvariant_PurePlannerSweep(t *testing.T) {
	t.Parallel()
	seeds := uint64(costSweepDefaultSeeds)
	if s := os.Getenv("ROWDIFF_COST_SWEEP_SEEDS"); s != "" {
		if n, err := strconv.ParseUint(s, 10, 64); err == nil && n > 0 {
			seeds = n
		}
	}
	var checked, violations int
	var sorts, sortsUnprovable int
	// Per-ARM census of what the derivation actually read. A total-only floor
	// cannot separate a live index arm from one that stopped matching: when the
	// access path started wrapping every index scan in a covering plan, the
	// index arm went dark and the totals barely moved, because primary-key scans
	// kept the "provable" count healthy on their own.
	leafArms := map[string]int{}
	// Observe-only: the CONCRETE leaf types behind those arms. The index arm is
	// fed by two plan shapes and the split between them is what a change to the
	// access path moves, so recording it is what makes a future shift in reach
	// legible rather than a silent one. Deliberately NOT floored: either of the
	// two shapes dropping to zero is a legitimate access-path outcome, so a floor
	// on one of them would be a build break rather than an alarm. What each shape
	// needs is a UNIT pin, and TestCheckPlanOrdering_RedundantSort has one per
	// shape.
	leafTypes := map[string]int{}
	var samples []string
	for seed := uint64(1); seed <= seeds; seed++ {
		c := Generate(seed)
		ddl := c.DDL()
		for _, q := range c.Queries {
			for _, proj := range c.ProjectionsFor(q) {
				sqlText := c.SQL(q, proj)
				plan, err := embedded.PlanPhysicalForTest(sqlText, ddl, nil)
				if err != nil {
					continue // plan errors are the row harness's concern, not this check's
				}
				checked++
				// Observe-only reach telemetry: count in-memory sorts and how
				// many sat over an input whose order the harness could not prove.
				if singleTablePlain(q) && len(q.OrderBy) > 0 {
					forEachInMemorySort(plan, func(s *plans.RecordQueryInMemorySortPlan) {
						sorts++
						if providedOrdering(s.GetInner()) == nil {
							sortsUnprovable++
						}
						leafArms[orderingLeafArm(s.GetInner())]++
						leafTypes[fmt.Sprintf("%T", orderingLeaf(s.GetInner()))]++
					})
				}
				for _, v := range checkPlanOrdering(plan, q) {
					violations++
					if len(samples) < 10 {
						samples = append(samples, fmt.Sprintf("seed %d: %s [%s]", seed, sqlText, v))
					}
				}
			}
		}
	}
	t.Logf("ordering-invariant sweep: %d plans checked across %d seeds; in-memory sorts observed=%d (input order unprovable=%d); leaf arms=%v; leaf types=%v",
		checked, seeds, sorts, sortsUnprovable, leafArms, leafTypes)
	if checked == 0 {
		t.Fatal("planned zero queries — the sweep is not exercising the planner")
	}
	// Vacuity floors. The alarm direction here is COLLAPSE: this axis reports a
	// clean nightly by emitting no findings, so an arm that stops being reached
	// is indistinguishable from an engine that stopped producing the defect. The
	// index arm gets its own floor because it is the one that went dark once the
	// access path started wrapping index scans in a covering plan, while the
	// primary-key arm kept the totals looking healthy.
	if sorts == 0 {
		t.Fatal("the corpus produced ZERO in-memory sorts — the redundant-sort detector examined nothing, so its clean report means nothing (alarm direction: COLLAPSE)")
	}
	if leafArms[orderingArmIndex] == 0 {
		t.Fatalf("ZERO of the %d observed sorts sat over an INDEX-backed input (arms=%v) — the index arm of the ordering derivation is unreachable and the axis is silent on every index shape (alarm direction: COLLAPSE)", sorts, leafArms)
	}
	if leafArms[orderingArmPrimaryKey] == 0 {
		t.Fatalf("ZERO of the %d observed sorts sat over a primary-key scan (arms=%v) — the PK arm of the ordering derivation is unreachable (alarm direction: COLLAPSE)", sorts, leafArms)
	}
	if violations != 0 {
		for _, s := range samples {
			t.Errorf("ordering violation: %s", s)
		}
		t.Fatalf("%d ordering violations across %d plans — a correct engine must produce none", violations, checked)
	}
}
