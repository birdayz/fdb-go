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
	ref := expressions.InitialOf(buildOrdinalChainSelect(n))
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
// TestFDB_CascadesCTEColumnAliases). Only a MERGE select — one whose result value
// is one of the two ordinal merge markers (IsPositionalMergeRC / IsOrdinalJoinRV),
// whose merge quantifier is planner-internal with NO external consumer — may
// intern alias-aware. Un-gating (returning true for non-merge selects) reopens
// the silent-NULL regression.
func TestSelectExpression_InternsAliasAware_GatedToMergeSelects(t *testing.T) {
	t.Parallel()

	t1 := scanQuantifier("T1")
	t2 := scanQuantifier("T2")

	// (b) The positional merge row (IsPositionalMergeRC: unnamed `_i` columns
	// over bare QOVs — PartitionSelectRule's positionalMergeCase shape). Same
	// rationale: the collapsed lower's quantifiers are planner-internal, so
	// alias-identity dedup would re-explode shared sub-products per
	// bipartition.
	posMergeSel := expressions.NewSelectExpressionWithAliases(
		values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "_0", Value: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("T1"))},
			values.RecordConstructorField{Name: "_1", Value: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("T2"))},
		),
		[]expressions.Quantifier{t1, t2}, nil, []string{"T1", "T2"},
	)
	if !posMergeSel.InternsAliasAware() {
		t.Error("a positional merge row (IsPositionalMergeRC) must intern alias-aware")
	}

	// (c) The ordinal JOIN-SEED result value (IsOrdinalJoinRV: every field a
	// pinned baked leg reference over ≥2 quantifiers).
	if !buildOrdinalChainSelect(2).InternsAliasAware() {
		t.Error("an ordinal join-seed RC (IsOrdinalJoinRV) must intern alias-aware")
	}

	// (c') A MIXED unnest seed: baked outer run + a bare TYPED non-record QOV
	// element — a whole-leg reference is as position-determined as a pinned
	// bake, so the gathered unnest select interns alias-aware too (it
	// participates in re-enumeration; identity dedup would re-explode its
	// sub-products). An UNTYPED bare QOV keeps declining — the CTE-rename/lazy
	// rationale is untouched.
	srcType := values.NewRecordType("SRC", false, []values.Field{
		{Name: "SID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	srcQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("S"), srcType)
	srcFV, sErr := values.NewFieldValueOfOrdinal(srcQOV, 0)
	if sErr != nil {
		t.Fatalf("bake SRC#0: %v", sErr)
	}
	mixedRC := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "SID", Value: srcFV},
		values.RecordConstructorField{Name: "EL", Value: values.NewQuantifiedObjectValueOfType(
			values.NamedCorrelationIdentifier("EL"), values.NotNullLong)},
	)
	if !values.IsOrdinalJoinRV(mixedRC) {
		t.Error("a mixed unnest seed (baked run + bare TYPED element QOV) must classify IsOrdinalJoinRV")
	}
	untypedRC := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "SID", Value: srcFV},
		values.RecordConstructorField{Name: "EL", Value: values.NewQuantifiedObjectValue(
			values.NamedCorrelationIdentifier("EL"))},
	)
	if values.IsOrdinalJoinRV(untypedRC) {
		t.Error("an UNTYPED bare QOV field must keep declining IsOrdinalJoinRV (no leg contract)")
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
		e, _, err := fullChainPlanner().Plan(expressions.InitialOf(buildOrdinalChainSelect(3)))
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
// count for 3- and 4-table chains so any
// memo-interning touch is held to a tight tolerance: if alias-aware
// Reference.Insert/InsertFinal interning regresses, the count doubles
// (29915 → 60044 with a naive per-occurrence alias). The pinned numbers are
// the only thing that catches it — a bare "must not regress" is a vibe.
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
		{3, 11122},
		{4, 45306},
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
