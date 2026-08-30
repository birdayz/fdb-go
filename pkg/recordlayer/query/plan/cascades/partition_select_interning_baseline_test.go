package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func tName(i int) string {
	if i < 10 {
		return "T" + string(rune('0'+i))
	}
	return "T" + string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// fullChainPlanner returns a planner configured EXACTLY as the SQL pipeline
// configures it (plan_harness.go / cascades_generator.go): the REWRITING
// normalization rules, plus the PLANNING-phase exploration rules
// (PlanningExplorationRules — incl. PartitionSelectRule — prepended by
// WithPlanningExpressionRules) and the implementation rules. PartitionSelectRule
// is PLANNING-only, so a bare NewPlanner(DefaultExpressionRules()) NEVER fires
// the merge re-enumeration this gate measures.
func fullChainPlanner() *Planner {
	return NewPlanner(DefaultExpressionRules(), nil).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
}

// planChainTasks plans an n-table chain through the full pipeline and returns the
// deterministic total task count. tasksRun is the metric the interning
// sub-product sharing shows up in — plandiff is blind to it (byte-identical
// plans, more tasks).
func planChainTasks(t *testing.T, n int) int {
	t.Helper()
	ref := expressions.InitialOf(buildOrdinalChainSelect(t, n))
	_, tasks, err := fullChainPlanner().Plan(ref)
	if err != nil {
		t.Fatalf("%d-table chain Plan: %v (tasks=%d)", n, err, tasks)
	}
	return tasks
}

func interningBaselineQuantifier(
	t testing.TB,
	name string,
	rowType values.Type,
) expressions.Quantifier {
	t.Helper()
	scan := mustFullUnorderedScan(t, []string{name}, rowType)
	return expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(name), expressions.InitialOf(scan))
}

// TestSelectExpression_InternsAliasAware_GatedToMergeSelects pins the gate that
// confines alias-aware Reference.Insert/InsertFinal dedup to merge re-enumeration
// selects (RFC-077 7.5). A first cut made the alias-aware tier UNCONDITIONAL,
// which over-deduped CTE column-rename selects (whose quantifier aliases external
// consumers resolve by identity — Go has not unified namespaces, TODO 7.1) and
// silently read a renamed column as NULL (TestFDB_CTEChainedColumnAliases /
// TestFDB_CascadesCTEColumnAliases). Only a MERGE select — one whose result value
// is one of the two ordinal merge markers (IsPositionalMergeRC / IsOrdinalJoinRV),
// whose merge quantifier is planner-internal with NO external consumer — may
// intern alias-aware. Un-gating (returning true for non-merge selects) reopens
// the silent-NULL regression.
func TestSelectExpression_InternsAliasAware_GatedToMergeSelects(t *testing.T) {
	t.Parallel()

	t1Type := values.NewRecordType("T1", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	t2Type := values.NewRecordType("T2", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	t1 := interningBaselineQuantifier(t, "T1", t1Type)
	t2 := interningBaselineQuantifier(t, "T2", t2Type)
	t1RootValue, t1RootErr := t1.RequireFlowedObjectValue()
	t1Root := mustConstruct(t, t1RootValue, t1RootErr)
	t2RootValue, t2RootErr := t2.RequireFlowedObjectValue()
	t2Root := mustConstruct(t, t2RootValue, t2RootErr)

	// (b) The positional merge row (IsPositionalMergeRC: unnamed `_i` columns
	// over bare QOVs — PartitionSelectRule's positionalMergeCase shape). Same
	// rationale: the collapsed lower's quantifiers are planner-internal, so
	// alias-identity dedup would re-explode shared sub-products per
	// bipartition.
	posMergeValue, posMergeErr := expressions.NewSelectExpressionWithAliases(
		values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "_0", Value: t1Root},
			values.RecordConstructorField{Name: "_1", Value: t2Root},
		),
		[]expressions.Quantifier{t1, t2}, nil, []string{"T1", "T2"},
	)
	posMergeSel := mustConstruct(t, posMergeValue, posMergeErr)
	if !posMergeSel.InternsAliasAware() {
		t.Error("a positional merge row (IsPositionalMergeRC) must intern alias-aware")
	}

	// (c) The ordinal JOIN-SEED result value (IsOrdinalJoinRV: every field a
	// pinned baked leg reference over ≥2 quantifiers).
	if !buildOrdinalChainSelect(t, 2).InternsAliasAware() {
		t.Error("an ordinal join-seed RC (IsOrdinalJoinRV) must intern alias-aware")
	}

	// (c') A MIXED unnest seed: baked outer run + a bare TYPED non-record QOV
	// element — a whole-leg reference is as position-determined as a pinned
	// bake, so the gathered unnest select interns alias-aware too (it
	// participates in re-enumeration; identity dedup would re-explode its
	// sub-products). RFC-232 makes the old untyped-QOV negative fixture
	// unrepresentable; the ordinary projection below is the fail-closed control.
	srcType := values.NewRecordType("SRC", false, []values.Field{
		{Name: "SID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	srcQOVValue, srcQOVErr := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("S"), srcType)
	srcQOV := mustConstruct(t, srcQOVValue, srcQOVErr)
	srcFV := mustOrdinalSeedField(t, srcQOV, 0)
	elementQOVValue, elementQOVErr := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("EL"), values.NotNullLong)
	elementQOV := mustConstruct(t, elementQOVValue, elementQOVErr)
	mixedRC := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "SID", Value: srcFV},
		values.RecordConstructorField{Name: "EL", Value: elementQOV},
	)
	if !values.IsOrdinalJoinRV(mixedRC) {
		t.Error("a mixed unnest seed (baked run + bare TYPED element QOV) must classify IsOrdinalJoinRV")
	}
	// A plain projection select (e.g. a CTE column rename's body) must NOT opt in:
	// its quantifier aliases are externally resolved by identity, so alias-aware
	// dedup would pick a survivor whose columns the consumer reads as NULL.
	projValue, projErr := expressions.NewSelectExpressionWithAliases(
		t1Root,
		[]expressions.Quantifier{t1}, nil, []string{"T1"},
	)
	projSel := mustConstruct(t, projValue, projErr)
	if projSel.InternsAliasAware() {
		t.Error("a non-merge projection select must NOT intern alias-aware (reopens the CTE silent-NULL regression)")
	}
}

// TestMemo_NextMergeAlias pins two properties of the merge alias
// (RFC-077 7.5). (1) Collision-PROOF: the alias contains a double-quote, the one
// character no parsed SQL identifier can contain (lexer DOUBLE_QUOTE_ID:
// '"' ~'"'+ '"'), so no user alias — quoted or not — can ever equal a merge
// quantifier alias (a `AS "$m1"` collision would corrupt alias-keyed binding in a
// multi-way join). (2) Deterministic + per-occurrence-unique: two fresh Memos mint
// the SAME sequence (so the same query has a stable plan hash), and each call
// returns a DISTINCT alias (so equivalent sub-products differ and are interned by
// the alias-aware Reference.Insert tier, not a stable string).
func TestMemo_NextMergeAlias(t *testing.T) {
	t.Parallel()

	m1 := NewMemo(nil)
	a, b := m1.NextMergeAlias(), m1.NextMergeAlias()
	if !strings.Contains(a.Name(), `"`) {
		t.Errorf("merge alias %q must contain a double-quote to be uncollidable with any SQL identifier", a.Name())
	}
	if a == b {
		t.Errorf("consecutive merge aliases must differ, got %q twice", a.Name())
	}

	// A second fresh Memo mints the identical sequence (per-plan determinism).
	m2 := NewMemo(nil)
	if c := m2.NextMergeAlias(); c != a {
		t.Errorf("merge alias not deterministic across fresh Memos: %q vs %q", c.Name(), a.Name())
	}
}

// TestPartitionSelect_MergeAliasPlanHashStable pins that the merge
// quantifier alias must NOT make the plan hash depend on process history. The
// alias flows into RecordQueryNestedLoopJoinPlan.HashCodeWithoutChildren (raw
// source aliases) → plans.PlanHash (plan-log identity) + the cost-model tiebreak.
// A process-global UniqueCorrelationIdentifier made the SAME query hash
// differently once the global counter had advanced (a long-lived process that
// planned other queries first); the per-Memo deterministic Memo.NextMergeAlias
// makes the same query mint the same alias sequence regardless of global-counter
// state. This test plans the same chain twice with the global counter advanced in
// between and asserts the plan hash is identical — it FAILS with a process-global
// merge alias and PASSES with the per-Memo counter.
func TestPartitionSelect_MergeAliasPlanHashStable(t *testing.T) {
	t.Parallel()

	type planGetter interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	planOnce := func() plans.RecordQueryPlan {
		e, _, err := fullChainPlanner().Plan(expressions.InitialOf(buildOrdinalChainSelect(t, 3)))
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		pg, ok := e.(planGetter)
		if !ok {
			t.Fatalf("planned expr %T is not a plan getter", e)
		}
		return pg.GetRecordQueryPlan()
	}

	h1 := plans.PlanHash(planOnce())
	// Advance the process-global correlation counter, simulating a long-lived
	// process that planned other queries between the two plannings of this one.
	for i := 0; i < 41; i++ {
		_ = values.UniqueCorrelationIdentifier()
	}
	h2 := plans.PlanHash(planOnce())

	if h1 != h2 {
		t.Errorf("plan hash NOT stable across plannings (merge alias leaked process-global "+
			"counter state): h1=%d h2=%d", h1, h2)
	}
}

// TestPartitionSelect_ChainInterningBaseline is the RFC-077 7.5 task-count
// gate over the ORDINAL-seeded chain (the sole production path since the
// name-model producer was deleted). It pins the join-re-enumeration task
// count for 3- and 4-table chains so any memo-admission or interning touch is
// held to a tight tolerance. Historically alias-aware interning prevented the
// 29915 → 60044 per-occurrence-alias blow-up; after RFC-232, prepared admission
// drops deduped proposals before scheduling, so disabling tier 3 is an exact
// no-op on this corpus. TestAliasAwareInterningShadowDelta pins that handoff,
// while the gate and prepared-equality unit tests retain direct tier coverage.
func TestPartitionSelect_ChainInterningBaseline(t *testing.T) {
	t.Parallel()

	cases := []struct {
		tables   int
		expected int // pinned post-change baseline
	}{
		// Bumped for RFC-150 Phase-2b Piece-2: PartitionBinarySelectRule's
		// idempotency guard was narrowed from "any predicate-free binary in the
		// group blocks" to "only the predicate-free partition over THIS select's
		// own quantifier alias set blocks". The broad guard let the FIRST sibling
		// bipartition's predicate-free result block EVERY other bipartition from
		// being partitioned, so the merge-quantifier uppers ({$m(t1⋈t2), t3} etc.)
		// were never pushed into correlated sub-Selects — the correlated index-probe
		// FlatMap chain for ≥3-way joins was never enumerated and the inner table
		// materialized as a full-scan NLJ (the gap the Go-only tryFlatMapPlan papered
		// over, now RETIRED). Narrowing the guard enumerates those siblings, which is
		// the extra work: 9095→11122 (3-table, +22%) / 31210→46483 (4-table, +49%).
		// Bounded — the round-trip cycle (PartitionBinary↔SelectMerge) is still broken
		// (the same alias-set partition can't be re-created), interning still collapses
		// shared sub-products, and the 4-table count stays well under the 100k task
		// budget. This is the cost of producing the cost-optimal index-nested-loop chain
		// via the single data-access path instead of the hand-rolled tryFlatMapPlan.
		//
		// RFC-152 (cost-model materialization for the LEFT-OUTER rewrite) nudged the
		// 4-table count 46483→45306 (-2.5%): nestedLoopJoinCost now charges its inner
		// scanned ONCE and compareJoinOrdering ranks same-Reference join candidates by
		// WORK, so the NLJ-vs-FlatMap decision resolves slightly differently and the
		// search prunes marginally earlier (FEWER tasks — a strict improvement, well
		// inside the interning/correlation sentinels this baseline guards). 3-table is
		// unaffected.
		//
		// The OptimizeInputs identity guard (Java CascadesPlanner.OptimizeInputs's
		// containsExactly port) nudged 11122→11446 (3-table, +2.9%) / 45306→47088
		// (4-table, +3.9%): a parent expression pruned OUT of its group no longer
		// drives child-group pruning, so child groups keep more members through
		// re-exploration. That is the point — pruning on behalf of a LOSER parent
		// could fix a child winner chosen for a plan that no longer exists. The
		// extra members are re-explored, not re-enumerated: interning still
		// collapses shared sub-products, which is what this baseline guards.
		//
		// The root-operator rule index (Java AbstractRuleSet.getRootOperator
		// bucketing) dropped 11446→1554 (3-table, -86%) / 47088→6998
		// (4-table, -85%): ExploreExprTask no longer pushes transform tasks
		// for rules whose matcher root cannot match the expression's type —
		// those tasks previously popped, type-asserted, failed, and counted.
		// The surviving counts are the REAL rule-firing workload, which is
		// what the interning sentinels actually guard.
		//
		// The final-survival OptimizeInputs guard (ContainsFinal — a pruned
		// dual-inserted loser no longer drives child optimization) plus the
		// delegation-truthful ordering satisfaction trimmed 1554→1500
		// (3-table, -3.5%) / 6998→6706 (4-table, -4.2%).
		//
		// The RFC-181 WS-P stage (b) convergence flip (physical yields land
		// ONLY in FinalMembers, dual insertion retired; group re-exploration
		// is EPOCH-driven and every round re-explores ALL members like Java's
		// ExploreGroup, instead of the member-count model's only-new-members
		// slice) moved 1500→2786 (3-table, +86%) / 6706→13721 (4-table,
		// +105%): the extra tasks are idempotent re-explorations of
		// already-interned members, not re-enumerated sub-products —
		// interning still collapses shared sub-products (the alias-aware
		// shadow moved in lockstep, see TestAliasAwareInterningShadowDelta),
		// which is what this baseline guards. The sentinel gate then
		// trimmed 2786→2468 / 13721→10255: PLANNING pure LOGICAL finals
		// are fail-to-plan sentinels and no longer rule-explored each
		// round (the member loop owns every implementable form).
		//
		// RFC-232 prepared whole-batch admission initially dropped 2468→1380
		// (3-table, -44%) / 10255→8627 (4-table, -16%): the commit reports
		// exactly which ExpressionRule proposals became new memo members, and
		// only those insertions schedule ExploreExpr/OptimizeInputs work. A
		// proposal absorbed by memo equality is not a member and has no new
		// subtree to explore; the former downstream tasks were phantom work.
		// Removing those descendants also moves this corpus's alias-aware shadow
		// to zero: enabling/disabling tier 3 now produces identical members/tasks.
		// The ordering-aware pending-exploration fix then moved those counts to
		// 1524/11237: a child task already pending below parent transforms is moved
		// ahead of that parent rather than being treated as completed. The extra
		// work is the previously skipped, correctness-required child exploration
		// pinned by FuzzPlanner_PlanFullPipeline/1c96bcee4188ef3e.
		//
		// Exact selected-inner predicate normalization then moved 1524→1484 and
		// 11237→10965. A correlated PredicatesFilter can own an outer-row Value
		// whose logical root retains a nominal record name while the selected
		// FlatMap binds the same alias with an anonymous physical carrier. Once
		// that predicate is rebuilt against the selected carrier, the memo uses the
		// corrected plan-backed inner instead of continuing to explore the stale
		// nominal alternative. A controlled mutation that disables only this
		// predicate rebuild restores 1524/11237 exactly and reproduces the three
		// executor.layout type-mismatch witnesses; the enabled path is deterministic
		// and removes only that invalid work.
		{3, 1484},
		{4, 10965},
	}
	for _, tc := range cases {
		got := planChainTasks(t, tc.tables)
		tol := tc.expected / 50 // ±2%
		if got < tc.expected-tol || got > tc.expected+tol {
			t.Errorf("%d-table chain tasksRun=%d, want %d ±2%% ([%d,%d]) — join re-enumeration interning changed",
				tc.tables, got, tc.expected, tc.expected-tol, tc.expected+tol)
		}
	}
}
