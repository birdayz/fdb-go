package cascades

import (
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// buildChainSelect builds an n-table CHAIN join T1—T2—…—Tn, each link an
// equi-predicate Ti.NEXT_ID = T(i+1).ID. A chain is the topology that exercises
// PartitionSelectRule's MERGE re-enumeration (the path the merge quantifier's
// interning protects): a connected lower sub-chain of ≥2 tables both referenced
// by spanning upper predicates collapses into one merge quantifier, and the SAME
// sub-chain (e.g. T2⋈T3) is reachable from many bipartitions — so its sub-product
// must INTERN to one Reference or the task count explodes super-linearly with
// arity. (A pure STAR does NOT hit the merge branch: with all predicates through
// the hub, a connected ≥2-table lower always contains the hub and only the hub is
// upper-referenced → the single-live case, not the ≥2-live merge.) The result
// value is the translator SEED merge (Seed=true) naming the first two tables —
// exactly the flat seed — so the rule keeps every leg live and re-enumerates the
// full join order.
func buildChainSelect(n int) *expressions.SelectExpression {
	var quants []expressions.Quantifier
	var aliases []string
	var preds []predicates.QueryPredicate
	for i := 1; i <= n; i++ {
		quants = append(quants, scanQuantifier(tName(i)))
		aliases = append(aliases, tName(i))
	}
	for i := 1; i < n; i++ {
		preds = append(preds, chainEqPred(tName(i), "NEXT_ID", tName(i+1), "ID"))
	}
	seed := values.NewJoinMergeSeedValue(
		values.NamedCorrelationIdentifier(tName(1)),
		values.NamedCorrelationIdentifier(tName(2)),
	)
	return expressions.NewSelectExpressionWithAliases(seed, quants, preds, aliases)
}

// buildChainSelectAnchored is buildChainSelect but the result value is the
// SOURCE-ANCHORED join RESULT value (RFC-077 7.6) over the first two tables,
// instead of the opaque JoinMergeSeedValue. It exercises the F2 exploration-time
// HIDING path: GetCorrelatedToOfValue must NOT descend into the anchored RC (its
// leg QOVs are self-bound by the select's own quantifiers), and PartitionSelectRule
// must treat the anchored RC as a seed (keep all lowers live). If the hiding lapses
// the leg aliases inflate the correlation order and the ≥4-way task count blows up.
func buildChainSelectAnchored(n int) *expressions.SelectExpression {
	var quants []expressions.Quantifier
	var aliases []string
	var preds []predicates.QueryPredicate
	for i := 1; i <= n; i++ {
		quants = append(quants, scanQuantifier(tName(i)))
		aliases = append(aliases, tName(i))
	}
	for i := 1; i < n; i++ {
		preds = append(preds, chainEqPred(tName(i), "NEXT_ID", tName(i+1), "ID"))
	}
	seed := values.NewAnchoredJoinRecord([]values.AnchoredJoinLeg{
		{Alias: values.NamedCorrelationIdentifier(tName(1)), Columns: []values.Field{
			{Name: "ID"}, {Name: "NEXT_ID"},
		}},
		{Alias: values.NamedCorrelationIdentifier(tName(2)), Columns: []values.Field{
			{Name: "ID"}, {Name: "NEXT_ID"},
		}},
	})
	return expressions.NewSelectExpressionWithAliases(seed, quants, preds, aliases)
}

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
	ref := expressions.InitialOf(buildChainSelect(n))
	_, tasks, err := fullChainPlanner().Plan(ref)
	if err != nil {
		t.Fatalf("%d-table chain Plan: %v (tasks=%d)", n, err, tasks)
	}
	return tasks
}

// TestPartitionSelect_ChainInterningBaseline is the RFC-077 7.5 task-count gate.
// It pins the join-re-enumeration task count for 3- and 4-table chains so the
// retirement of the synthetic stable merge alias (now a per-Memo deterministic
// alias, Memo.NextMergeAlias, interned alias-aware by Reference.Insert/InsertFinal)
// — and any future memo-interning touch — is held to a tight tolerance. The old
// content-stable merge alias was load-bearing for interning: an alias that differs
// per merge occurrence WITHOUT alias-aware Insert/InsertFinal DOUBLES the 4-chain
// count (29915 → 60044) while plandiff stays byte-identical. These pinned numbers
// are the only thing that catches such a sub-product-sharing miss; a bare "must
// not regress" is a vibe.
//
// Baseline provenance (master, with the old mergeQuantifierAlias): 3-chain 8999,
// 4-chain 29915. Post-change (uniqueId + alias-aware Insert/InsertFinal): 3-chain
// 8999 (EXACT), 4-chain 30593 (+2.3%, identical merge-branch hit count 42) — the
// alias-aware interning reproduces the stable sub-product sharing; the small
// 4-way residual is bounded (NOT super-linear) and far from the +100% naive blowup.
// TestSelectExpression_InternsAliasAware_GatedToMergeSelects pins the gate that
// confines alias-aware Reference.Insert/InsertFinal dedup to merge re-enumeration
// selects (RFC-077 7.5). A first cut made the alias-aware tier UNCONDITIONAL,
// which over-deduped CTE column-rename selects (whose quantifier aliases external
// consumers resolve by identity — Go has not unified namespaces, TODO 7.1) and
// silently read a renamed column as NULL (TestFDB_CTEChainedColumnAliases /
// TestFDB_CascadesCTEColumnAliases). Only a select whose result value is a
// JoinMergeAllValue — the marker of a merge select, whose merge quantifier is
// planner-internal with NO external consumer — may intern alias-aware. Un-gating
// (returning true for non-merge selects) reopens the silent-NULL regression.
func TestSelectExpression_InternsAliasAware_GatedToMergeSelects(t *testing.T) {
	t.Parallel()

	t1 := scanQuantifier("T1")
	t2 := scanQuantifier("T2")

	// A merge re-enumeration select (result value is a JoinMergeAllValue) opts in.
	mergeSel := expressions.NewSelectExpressionWithAliases(
		values.NewJoinMergeAllValue(
			values.NamedCorrelationIdentifier("T1"),
			values.NamedCorrelationIdentifier("T2"),
		),
		[]expressions.Quantifier{t1, t2}, nil, []string{"T1", "T2"},
	)
	if !mergeSel.InternsAliasAware() {
		t.Error("a select with a JoinMergeAllValue result must intern alias-aware (merge re-enumeration)")
	}

	// A translator SEED merge also opts in (same value type) — harmless, the root
	// seed is inserted once so there is nothing to collapse.
	seedSel := expressions.NewSelectExpressionWithAliases(
		values.NewJoinMergeSeedValue(
			values.NamedCorrelationIdentifier("T1"),
			values.NamedCorrelationIdentifier("T2"),
		),
		[]expressions.Quantifier{t1, t2}, nil, []string{"T1", "T2"},
	)
	if !seedSel.InternsAliasAware() {
		t.Error("a seed merge select must intern alias-aware (same JoinMergeAllValue marker)")
	}

	// A plain projection select (e.g. a CTE column rename's body) must NOT opt in:
	// its quantifier aliases are externally resolved by identity, so alias-aware
	// dedup would pick a survivor whose columns the consumer reads as NULL.
	projSel := expressions.NewSelectExpressionWithAliases(
		t1.GetFlowedObjectValue(),
		[]expressions.Quantifier{t1}, nil, []string{"T1"},
	)
	if projSel.InternsAliasAware() {
		t.Error("a non-merge projection select must NOT intern alias-aware (reopens the CTE silent-NULL regression)")
	}
}

// TestMemo_NextMergeAlias pins two codex-P2 properties of the merge alias
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

// TestPartitionSelect_MergeAliasPlanHashStable pins the codex P2-1 fix: the merge
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
		e, _, err := fullChainPlanner().Plan(expressions.InitialOf(buildChainSelect(3)))
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

// planChainTasksAnchored plans an n-table chain whose seed is the source-anchored
// join RESULT value (RFC-077 7.6) and returns the deterministic task count.
func planChainTasksAnchored(t *testing.T, n int) int {
	t.Helper()
	ref := expressions.InitialOf(buildChainSelectAnchored(n))
	_, tasks, err := fullChainPlanner().Plan(ref)
	if err != nil {
		t.Fatalf("%d-table anchored chain Plan: %v (tasks=%d)", n, err, tasks)
	}
	return tasks
}

// TestPartitionSelect_AnchoredSeedExplorationBudget is the RFC-077 7.6 F2-HIDING
// gate. It confirms the source-anchored join RESULT value keeps the ≥4-way
// chain/STAR exploration within budget — i.e. GetCorrelatedToOfValue's "do not
// descend into an anchored-join RC" suppression actually fires, so the buried leg
// QOVs do NOT inflate the correlation order. The anchored-seed task count must be
// within ±5% of the opaque-seed baseline at every arity (the anchored RC is the
// structural successor of the Seed bit, so the budget must hold). If the hiding
// lapses (e.g. the AnchoredJoin marker is lost across a rebase, or
// GetCorrelatedToOfValue descends), the leg aliases surface and the 4-chain count
// jumps — the +~32% blowup the Seed bit's suppression was measured to prevent.
func TestPartitionSelect_AnchoredSeedExplorationBudget(t *testing.T) {
	t.Parallel()
	for _, n := range []int{3, 4} {
		opaque := planChainTasks(t, n)
		anchored := planChainTasksAnchored(t, n)
		tol := opaque/20 + 1 // ±5%
		if anchored < opaque-tol || anchored > opaque+tol {
			t.Errorf("%d-chain anchored-seed tasksRun=%d, opaque-seed baseline=%d ±5%% ([%d,%d]) — "+
				"F2 exploration-time hiding lapsed (the anchored RC's leg QOVs leaked into the correlation order)",
				n, anchored, opaque, opaque-tol, opaque+tol)
		}
	}
}

func TestPartitionSelect_ChainInterningBaseline(t *testing.T) {
	t.Parallel()

	cases := []struct {
		tables   int
		expected int // pinned post-change baseline
	}{
		{3, 8999},
		{4, 30593},
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
