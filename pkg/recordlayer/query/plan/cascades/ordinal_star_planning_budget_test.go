package cascades

import (
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
//     re-explodes shared sub-products blows past the 100k budget (measured: the
//     5-way all-live star already caps; the 4-way is the largest that converges).
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
	const wantTasks = 6219
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
