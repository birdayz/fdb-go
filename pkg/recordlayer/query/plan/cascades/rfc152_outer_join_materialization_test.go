package cascades

// RFC-152 — cost-model materialization for the LEFT-OUTER rewrite.
//
// `SELECT a.id FROM a LEFT JOIN b ON a.flag = 1` (ON pred references ONLY the
// preserved leg). RewriteOuterJoinRule ALWAYS fires (Java-faithful, no cross-leg
// guard) and produces a FlatMap whose inner RE-SCANS b from FDB once per a-row.
// Go ALSO keeps a materialized RecordQueryNestedLoopJoinPlan that scans b ONCE.
// The fix is in the COST MODEL, not a rule guard:
//   (1) nestedLoopJoinCost charges the inner scanned ONCE (materialize-once), so a
//       materialized NLJ with a non-probe inner is strictly cheaper ON WORK than
//       the re-scan FlatMap, while a card-1 probe inner keeps the FlatMap cheapest.
//   (2) compareJoinOrdering ranks same-Reference join candidates by WORK (CPU) —
//       their true output cardinality is identical (a group property), so the
//       per-shape cardinality proxy is an unfair discriminator; work is the honest
//       one.
//
// Tests are TYPED: a FireExpressionRule structural assertion on the yielded
// SelectExpression + nullOnEmpty quantifier, and PlanningCostModelLess / direct
// compareJoinOrdering comparisons on the typed physical wrappers. NO EXPLAIN
// string matching.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// --- Rule-level: the rewrite still yields the nullOnEmpty form (unchanged) ---

// TestRFC152_RewriteOuterJoinYieldsNullOnEmpty asserts RewriteOuterJoinRule still
// rewrites a preserved-correlated LEFT OUTER SelectExpression into an INNER
// SelectExpression carrying a null-on-empty quantifier — the rule's behavior is
// UNCHANGED by RFC-152 (the fix is entirely in the cost model). Typed structural
// assertions on the yielded expression, not plan strings.
func TestRFC152_RewriteOuterJoinYieldsNullOnEmpty(t *testing.T) {
	t.Parallel()

	aliasA := values.NamedCorrelationIdentifier("A")
	aliasB := values.NamedCorrelationIdentifier("B")

	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	qA := expressions.NamedForEachQuantifier(aliasA, expressions.InitialOf(scanA))
	qB := expressions.NamedForEachQuantifier(aliasB, expressions.InitialOf(scanB))

	// ON A.flag = 1 — references ONLY the preserved leg A (preserved-correlated, so
	// the rule's `correlated` guard passes and it fires).
	flagField := values.NewFieldValue(values.NewQuantifiedObjectValue(aliasA), "flag", values.UnknownType)
	pred := predicates.NewComparisonPredicate(flagField, predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))

	sel := expressions.NewSelectExpressionWithJoinType(
		values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
		[]expressions.Quantifier{qA, qB},
		[]predicates.QueryPredicate{pred},
		[]string{"A", "B"},
		expressions.JoinLeftOuter,
	)
	ref := expressions.InitialOf(sel)

	yielded := FireExpressionRule(NewRewriteOuterJoinRule(), ref)

	var rewritten *expressions.SelectExpression
	for _, e := range yielded {
		s, ok := e.(*expressions.SelectExpression)
		if !ok {
			continue
		}
		if s.GetJoinType() != expressions.JoinInner {
			continue
		}
		rewritten = s
	}
	if rewritten == nil {
		t.Fatalf("RewriteOuterJoinRule yielded no INNER SelectExpression (got %d expressions)", len(yielded))
	}
	quants := rewritten.GetQuantifiers()
	if len(quants) != 2 {
		t.Fatalf("rewritten select: want 2 quantifiers, got %d", len(quants))
	}
	if len(rewritten.GetPredicates()) != 0 {
		t.Errorf("rewritten OUTER select must carry NO predicates (they live below the null-extension); got %d", len(rewritten.GetPredicates()))
	}
	nullOnEmpty := 0
	for _, q := range quants {
		if q.IsNullOnEmpty() {
			nullOnEmpty++
		}
	}
	if nullOnEmpty != 1 {
		t.Errorf("rewritten select: want exactly 1 nullOnEmpty quantifier (the LEFT-OUTER semantics carrier), got %d", nullOnEmpty)
	}
}

// TestRewriteOuterJoinDeclinesFullOuter pins that RewriteOuterJoinRule
// yields NOTHING for a FULL OUTER SelectExpression. The fixture is
// deliberately the EXACT shape the LEFT-OUTER test above fires on — 2
// ForEach quantifiers, an ON-predicate correlated to the preserved leg —
// with ONLY the join type flipped to FULL, so the decline is attributable
// to the join type alone.
//
// This pin is load-bearing for translation-time ordinalization: FULL OUTER
// boxes are ALWAYS translated into the ordinal (positional) form, on the
// premise that this rule never rewrites them, so they stay opaque boxes
// through every planner phase (unlike LEFT/RIGHT boxes, which this rule
// dissolves post-translation). Extending this rule to FULL would dissolve
// ordinalized FULL boxes into merge-eligible INNER selects and break that
// premise — this pin must fail BEFORE the ordinalization logic does.
func TestRewriteOuterJoinDeclinesFullOuter(t *testing.T) {
	t.Parallel()

	aliasA := values.NamedCorrelationIdentifier("A")
	aliasB := values.NamedCorrelationIdentifier("B")

	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	qA := expressions.NamedForEachQuantifier(aliasA, expressions.InitialOf(scanA))
	qB := expressions.NamedForEachQuantifier(aliasB, expressions.InitialOf(scanB))

	// Preserved-correlated ON-predicate — the same one the LEFT-OUTER fixture
	// fires on, so nothing but the join type can explain a decline.
	flagField := values.NewFieldValue(values.NewQuantifiedObjectValue(aliasA), "flag", values.UnknownType)
	pred := predicates.NewComparisonPredicate(flagField, predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))

	sel := expressions.NewSelectExpressionWithJoinType(
		values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
		[]expressions.Quantifier{qA, qB},
		[]predicates.QueryPredicate{pred},
		[]string{"A", "B"},
		expressions.JoinFullOuter,
	)
	ref := expressions.InitialOf(sel)

	yielded := FireExpressionRule(NewRewriteOuterJoinRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("RewriteOuterJoinRule yielded %d expression(s) for a FULL OUTER select, want 0 — "+
			"the premise that FULL boxes stay opaque through all planner phases (because this rule never rewrites them) is broken; "+
			"fix the rule or revisit FULL-outer ordinalization BEFORE extending the rewrite", len(yielded))
	}
}

// --- Cost-level: the materialized NLJ vs re-scan FlatMap ordering ---

// rfc152Plans builds the two competing physical plans for the preserved-only and
// the probe shapes, exactly as the planner would: a materialized LEFT-OUTER NLJ
// (inner = full Scan(B)) vs a re-scan FlatMap (inner = DefaultOnEmpty over a
// filtered full Scan(B)) for preserved-only; and a card-1 probe FlatMap (inner =
// DefaultOnEmpty over an equality-bound Scan(B)) vs the materialized NLJ.
func rfc152Plans() (preservedNLJ, preservedFlatMap, probeNLJ, probeFlatMap plans.RecordQueryPlan) {
	aliasA := values.NamedCorrelationIdentifier("A")
	aliasB := values.NamedCorrelationIdentifier("B")
	resVal := values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())

	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)

	flagField := values.NewFieldValue(values.NewQuantifiedObjectValue(aliasA), "flag", values.UnknownType)
	pred := predicates.NewComparisonPredicate(flagField, predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))

	// Preserved-only: materialized NLJ (inner = bare Scan(B), scanned once).
	preservedNLJ = plans.NewRecordQueryNestedLoopJoinPlan(scanA, scanB, []predicates.QueryPredicate{pred}, plans.JoinLeftOuter, "A", "B", resVal)
	// Preserved-only: re-scan FlatMap (inner re-scans full Scan(B) per outer row).
	// LEFT-OUTER semantics live in the DefaultOnEmpty wrapper, as in production.
	innerFilter := plans.NewRecordQueryPredicatesFilterPlan(scanB, []predicates.QueryPredicate{pred})
	innerDoE := plans.NewRecordQueryDefaultOnEmptyPlan(innerFilter, values.NewNullValue(values.UnknownType))
	preservedFlatMap = plans.NewRecordQueryFlatMapPlan(scanA, innerDoE, aliasA, aliasB, resVal, false)

	// Probe: card-1 equality-bound Scan(B) inner (a point probe, scanned per outer).
	eqProbe := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(7))
	eqRange := predicates.EmptyComparisonRange().Merge(&eqProbe).Range
	probeScanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false).
		WithPrimaryKey([]values.Value{&values.FieldValue{Field: "ID", Typ: values.UnknownType}}).
		WithScanComparisons([]*predicates.ComparisonRange{eqRange})
	probeDoE := plans.NewRecordQueryDefaultOnEmptyPlan(probeScanB, values.NewNullValue(values.UnknownType))
	probeFlatMap = plans.NewRecordQueryFlatMapPlan(scanA, probeDoE, aliasA, aliasB, resVal, false)
	probeNLJ = plans.NewRecordQueryNestedLoopJoinPlan(scanA, scanB, []predicates.QueryPredicate{pred}, plans.JoinLeftOuter, "A", "B", resVal)
	return
}

// wrapJoin wraps a join plan in its physical wrapper with quantifiers ranging over
// physical scan wrappers (the cost model reads the concrete embedded plan, so the
// quantifiers only need to be physical — no nil-inner Fetch templates here).
// Since RFC-184 W2 the join is its own cascades expression carrying its real
// (SARG'd) leg children directly, and the cost model reads GetRecordQueryPlan()
// — so the bare plan IS the cost-model-ready expression. The retired wrappers
// added dummy scan quantifiers only to satisfy the RelationalExpression
// interface; the bare plan already exposes its concrete children, so it is
// returned unchanged (a re-quantification over fresh plain scans would drop the
// probe SARG and mis-rank the probe FlatMap).
func wrapNLJ(p plans.RecordQueryPlan) *plans.RecordQueryNestedLoopJoinPlan {
	return p.(*plans.RecordQueryNestedLoopJoinPlan)
}

func wrapFlatMap(p plans.RecordQueryPlan) *plans.RecordQueryFlatMapPlan {
	return p.(*plans.RecordQueryFlatMapPlan)
}

// TestRFC152_CostModelPrefersMaterializedNLJ_PreservedOnly asserts the cost model
// orders the materialized NLJ STRICTLY BELOW the re-scan FlatMap for a preserved-
// only inner — via both the full multi-criteria comparator (PlanningCostModelLess)
// and the join-ordering criterion directly (compareJoinOrdering, which reaches the
// new CPU-work ranking for this pair). Typed wrapper comparison, no plan strings.
func TestRFC152_CostModelPrefersMaterializedNLJ_PreservedOnly(t *testing.T) {
	t.Parallel()
	nljPlan, fmPlan, _, _ := rfc152Plans()
	stats := properties.MapStatistics{PerType: map[string]float64{"A": 1_000_000, "B": 1_000_000}}
	less := NewPlanningCostModelLessWithContext(stats, nil)

	nlj := wrapNLJ(nljPlan)
	fm := wrapFlatMap(fmPlan)

	if !less(nlj, fm) {
		t.Errorf("preserved-only: want materialized NLJ < re-scan FlatMap (cost model must prefer scan-B-once)")
	}
	if less(fm, nlj) {
		t.Errorf("preserved-only: re-scan FlatMap must NOT be cheaper than materialized NLJ")
	}

	// Direct join-ordering criterion: NLJ wins on WORK (CPU), the RFC-152 ranking.
	if cmp := compareJoinOrdering(nlj, fm, stats, nil); cmp >= 0 {
		t.Errorf("compareJoinOrdering(NLJ, FlatMap) = %d, want < 0 (NLJ cheaper on work)", cmp)
	}
	if cmp := compareJoinOrdering(fm, nlj, stats, nil); cmp <= 0 {
		t.Errorf("compareJoinOrdering(FlatMap, NLJ) = %d, want > 0", cmp)
	}
}

// TestRFC152_CostModelPrefersProbeFlatMap asserts the materialization fix does NOT
// regress the cross-leg/probe case: a card-1 probe inner keeps the FlatMap strictly
// cheaper than the materialized NLJ (the probe FlatMap point-probes B once per
// outer; the materialized NLJ would scan all of B). Typed wrapper comparison.
func TestRFC152_CostModelPrefersProbeFlatMap(t *testing.T) {
	t.Parallel()
	_, _, nljPlan, fmPlan := rfc152Plans()
	stats := properties.MapStatistics{PerType: map[string]float64{"A": 1_000_000, "B": 1_000_000}}
	less := NewPlanningCostModelLessWithContext(stats, nil)

	nlj := wrapNLJ(nljPlan)
	fm := wrapFlatMap(fmPlan)

	if !less(fm, nlj) {
		t.Errorf("probe: want probe FlatMap < materialized NLJ (FlatMap point-probes once per outer)")
	}
	if less(nlj, fm) {
		t.Errorf("probe: materialized NLJ must NOT be cheaper than the card-1 probe FlatMap")
	}
}
