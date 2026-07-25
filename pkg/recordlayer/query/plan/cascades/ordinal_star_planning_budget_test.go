package cascades

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// buildOrdinalStar builds an ORDINAL-seeded STAR: hub H + n IDENTICAL spokes
// S1..Sn, each joined to the hub (H.id = Si.hid), every spoke live via the
// projection → the ≥2-live merge with n structurally-identical legs. Identical
// legs maximise the alias-bijection ambiguity the interning tier
// (InternsAliasAware → MemoEqual, permutation-aware matchChildrenInMemo) must
// resolve — the per-Insert cost the task-count baseline is BLIND to. Ordinal
// seed (raw RC), so the positional arm is authoritative (MergeArmHits stays 0).
func buildOrdinalStar(n int) *expressions.SelectExpression {
	var quants []expressions.Quantifier
	var aliases []string
	var preds []predicates.QueryPredicate
	var fields []values.RecordConstructorField

	hubType := values.NewRecordType("H", false, []values.Field{{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0}})
	quants = append(quants, scanQuantifier("H"))
	aliases = append(aliases, "H")
	hubQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("H"), hubType)
	hubFV, err := values.NewFieldValueOfOrdinal(hubQOV, 0)
	if err != nil {
		panic("buildOrdinalStar hub: " + err.Error())
	}
	fields = append(fields, values.RecordConstructorField{Name: hubFV.Field, Value: hubFV})

	for i := 1; i <= n; i++ {
		a := "S" + strconv.Itoa(i) // robust for n>9 (spoke aliases S1..Sn)
		quants = append(quants, scanQuantifier(a))
		aliases = append(aliases, a)
		spokeType := values.NewRecordType(a, false, []values.Field{
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "HID", FieldType: values.NotNullLong, Ordinal: 1},
		})
		qov := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier(a), spokeType)
		for col := range spokeType.Fields {
			fv, ferr := values.NewFieldValueOfOrdinal(qov, col)
			if ferr != nil {
				panic("buildOrdinalStar spoke: " + ferr.Error())
			}
			fields = append(fields, values.RecordConstructorField{Name: fv.Field, Value: fv})
		}
		preds = append(preds, chainEqPred("H", "ID", a, "HID"))
	}
	seed := values.NewRawRecordConstructorValue(fields...)
	values.AssertOrdinalJoinSeed(seed)
	return expressions.NewSelectExpressionWithAliases(seed, quants, preds, aliases)
}

// starWallClockCeiling is a DELIBERATELY GENEROUS bound (observed ~90ms; ~55×
// headroom) — it is a catastrophe detector, not a micro-benchmark. Its job is
// to catch a per-Insert bijection-enumeration blowup (e.g. a MemoEqual that
// regresses to trying all N! alias permutations per comparison) that leaves the
// task COUNT flat but explodes wall-clock into seconds. A tight bound would
// flake under CI load / -race; this one does not, and an O(N!) regression on a
// 51k-task plan overruns it by orders of magnitude.
const starWallClockCeiling = 5 * time.Second

// TestOrdinalStarPlanningBudget is the STAR-topology planning-budget gate.
// The task-count baseline (chain topology) is BLIND to per-Insert bijection
// cost: the count can stay flat while MemoEqual's alias-permutation work
// explodes. This pins a fixed many-identical-legs STAR (hub + 3 identical
// spokes, 4-way, every spoke live) four ways:
//
//   - CONVERGES (no MaxTasks cap) — a count-level interning regression that
//     re-explodes shared sub-products blows past the 100k budget (measured at
//     HEAD: hub+4 is the widest all-live star that still converges, at ~67k
//     tasks; hub+5 exhausts the budget — see TestOrdinalStarRightDeepBudget).
//   - task-count == 42788 ±2% — the STAR-topology interning sentinel,
//     complementing the CHAIN baseline (1554/6706): a different topology
//     stresses sub-product sharing differently. IsOrdinalJoinRV admitting bare
//     TYPED QOV fields pulls the post-translation MIXED upper RVs
//     (ofOrdinal-over-merge alongside bare leg QOVs) into alias-aware
//     interning — MORE sub-product sharing, the win direction.
//   - MergeArmHits == 0 — the ordinal star routes wholly through the
//     positional merge arm (the only merge arm that exists).
//   - wall-clock < ceiling — the per-Insert bijection-cost catch the count pin
//     cannot provide (min of several runs, generous ceiling; see the const).
func TestOrdinalStarPlanningBudget(t *testing.T) {
	t.Parallel()
	const spokes = 3
	// 42788→43959 (+2.7%) with the OptimizeInputs identity guard (dead parent
	// expressions no longer prune child groups — see the CHAIN baseline note).
	// 43959→6509 (-85%) with the root-operator rule index (never-matching
	// rules no longer get transform tasks — see the CHAIN baseline note).
	// 6509→6219 (-4.5%) with the final-survival OptimizeInputs guard
	// (pruned dual-inserted losers no longer drive child optimization).
	// 6219→11740 (+89%) with the RFC-181 WS-P stage (b) convergence flip
	// (finals-only physical yields, epoch-driven rounds re-exploring ALL
	// members per round like Java's ExploreGroup — see the CHAIN baseline
	// note): idempotent re-exploration tasks, not re-enumerated
	// sub-products. 11740→9189 with the sentinel gate (PLANNING pure
	// LOGICAL finals are fail-to-plan sentinels, no longer rule-explored
	// per round); determinism and the wall-clock ceiling still hold.
	// 9189→9481 (+3.2%) with the RFC-184 W2 predicates_filter wrapper collapse:
	// plain-filter emitters keep the LIVE shared-group inner edge (as the wrapper's
	// innerQuant did) so a pushed filter stays reachable from a parent that captures
	// the leg; the star topology's extra star-spoke re-exploration of the bare
	// interned filter members is the residual delta. The corpus stays plan-identical
	// (the chain baselines are unchanged from pre-collapse) except the redundant-sort
	// elisions the wrapper's relink limitation previously suppressed.
	const wantTasks = 9481
	tol := wantTasks / 50 // ±2%

	best := time.Hour
	var firstTasks int
	for i := 0; i < 4; i++ {
		p := fullChainPlanner()
		ref := expressions.InitialOf(buildOrdinalStar(spokes))
		start := time.Now()
		_, tasks, err := p.Plan(ref)
		el := time.Since(start)
		if err != nil {
			t.Fatalf("ordinal %d-spoke star did NOT converge: %v (tasks=%d) — an interning "+
				"regression re-exploding shared sub-products blew the 100k budget", spokes, err, tasks)
		}
		if i == 0 {
			firstTasks = tasks
		} else if tasks != firstTasks {
			t.Fatalf("ordinal %d-spoke star: NON-DETERMINISTIC task count %d vs %d — an alias "+
				"or interning bug", spokes, tasks, firstTasks)
		}
		if el < best {
			best = el
		}
	}

	if firstTasks < wantTasks-tol || firstTasks > wantTasks+tol {
		t.Errorf("ordinal %d-spoke star tasksRun=%d, want %d ±2%% ([%d,%d]) — STAR-topology "+
			"interning changed", spokes, firstTasks, wantTasks, wantTasks-tol, wantTasks+tol)
	}
	if best > starWallClockCeiling {
		t.Errorf("ordinal %d-spoke star planning wall-clock (best of 4) = %s > %s — the task count "+
			"is flat but per-Insert bijection-enumeration cost exploded",
			spokes, best, starWallClockCeiling)
	}
	t.Logf("ordinal %d-spoke star: tasks=%d, best wall-clock=%s (ceiling %s)", spokes, firstTasks, best, starWallClockCeiling)
}

// rightDeepPlanContext carries a PlannerConfiguration into the planner.
// PartitionSelectRule reads ShouldJoinRightDeep off PlanContext and has no
// other route to it, so the flag can only be exercised through a context.
type rightDeepPlanContext struct{ cfg PlannerConfiguration }

func (c rightDeepPlanContext) GetPlannerConfiguration() PlannerConfiguration { return c.cfg }
func (c rightDeepPlanContext) GetMatchCandidates() []MatchCandidate          { return nil }
func (c rightDeepPlanContext) GetPrimaryKeyColumns(string) []string          { return nil }

// planStarWithRightDeep plans an n-spoke all-live ordinal star with
// ShouldJoinRightDeep set to rightDeep, returning the winning expression, the
// task count, and the planner error.
func planStarWithRightDeep(n int, rightDeep bool) (expressions.RelationalExpression, int, error) {
	cfg := DefaultPlannerConfiguration()
	cfg.ShouldJoinRightDeep = rightDeep
	p := NewPlanner(DefaultExpressionRules(), rightDeepPlanContext{cfg: cfg}).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	return p.Plan(expressions.InitialOf(buildOrdinalStar(n)))
}

// TestOrdinalStarRightDeepBudget pins what ShouldJoinRightDeep is FOR: it is
// the lever that brings a wide all-live star inside the 100,000-task planning
// budget, and it is Java's own opt-in lever
// (RecordQueryPlannerConfiguration.setJoinRightDeep, reached from SQL through
// the PLAN_RIGHT_DEEP option).
//
// It is deliberately NOT a default change. Java's default is right-deep OFF
// (JOIN_RIGHT_DEEP_MASK unset ⇒ shouldJoinRightDeep() false) and Go's matches,
// so a star this wide exhausts the budget in BOTH engines at default settings.
// Turning it on excludes bushy join trees, which can be the cheaper shape —
// that trade is the caller's to make, not the planner's.
//
// Two halves, because "converges" alone would also pass if the flag merely
// broke planning into a different shape of the same size:
//
//   - hub+5 (six-way): the default CAPS and right-deep CONVERGES. This is the
//     shape CQ-9 named; it is the narrowest all-live star that exceeds the
//     budget at HEAD (hub+4 converges — see TestOrdinalStarPlanningBudget).
//   - hub+3 (four-way): BOTH converge, and right-deep uses STRICTLY FEWER
//     tasks. That is the direct evidence the flag prunes the search space
//     rather than merely reshaping it.
func TestOrdinalStarRightDeepBudget(t *testing.T) {
	t.Parallel()

	t.Run("wide_star_caps_by_default_converges_right_deep", func(t *testing.T) {
		t.Parallel()
		const spokes = 5

		best, tasks, err := planStarWithRightDeep(spokes, false)
		if !errors.Is(err, ErrPlannerCapHit) {
			t.Fatalf("hub+%d star at DEFAULT settings: err=%v (tasks=%d), want ErrPlannerCapHit — "+
				"Java caps on this shape too (right-deep defaults off in both engines); if the "+
				"budget now covers it, widen the star rather than deleting this half",
				spokes, err, tasks)
		}
		if best != nil {
			t.Fatalf("a capped planning run must yield no plan, got %T", best)
		}

		rdBest, rdTasks, rdErr := planStarWithRightDeep(spokes, true)
		if rdErr != nil {
			t.Fatalf("hub+%d star with ShouldJoinRightDeep: %v (tasks=%d) — the option must bring "+
				"the star inside the budget", spokes, rdErr, rdTasks)
		}
		if rdBest == nil {
			t.Fatal("right-deep planning converged but produced no winner")
		}
		if rdTasks >= 100_000 {
			t.Fatalf("right-deep tasks=%d is not under the 100,000 cap", rdTasks)
		}
		// Determinism: a right-deep run must be reproducible, or the count
		// above is a coin flip rather than a budget.
		if _, again, err2 := planStarWithRightDeep(spokes, true); err2 != nil || again != rdTasks {
			t.Fatalf("right-deep task count is non-deterministic: %d then %d (err=%v)", rdTasks, again, err2)
		}
		t.Logf("hub+%d star: default CAPS at 100000 tasks; right-deep converges in %d", spokes, rdTasks)
	})

	t.Run("right_deep_prunes_the_search_space", func(t *testing.T) {
		t.Parallel()
		const spokes = 3

		_, baseTasks, baseErr := planStarWithRightDeep(spokes, false)
		if baseErr != nil {
			t.Fatalf("hub+%d star at default settings: %v", spokes, baseErr)
		}
		rdBest, rdTasks, rdErr := planStarWithRightDeep(spokes, true)
		if rdErr != nil {
			t.Fatalf("hub+%d star with ShouldJoinRightDeep: %v", spokes, rdErr)
		}
		if rdBest == nil {
			t.Fatal("right-deep planning converged but produced no winner")
		}
		if rdTasks >= baseTasks {
			t.Fatalf("right-deep tasks=%d >= default tasks=%d — the flag must EXCLUDE bushy "+
				"partitions, not merely reorder the same enumeration", rdTasks, baseTasks)
		}
		t.Logf("hub+%d star: default %d tasks, right-deep %d tasks", spokes, baseTasks, rdTasks)
	})
}
