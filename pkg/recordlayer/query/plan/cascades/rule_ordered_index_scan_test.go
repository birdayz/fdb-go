package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestOrderedIndexScan_SortMatchesIndex verifies that Sort(STATUS)
// over a bare scan yields an index scan when an index on STATUS exists.
func TestOrderedIndexScan_SortMatchesIndex(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "STATUS", Typ: values.UnknownType}}},
		q,
	)
	sortRef := expressions.InitialOf(sort)

	rule := NewOrderedIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, sortRef, ctx, nil)

	if len(results) != 1 {
		t.Fatalf("expected 1 yield (ordered index scan), got %d", len(results))
	}
	if _, ok := results[0].(*physicalIndexScanWrapper); !ok {
		t.Fatalf("expected physicalIndexScanWrapper, got %T", results[0])
	}
}

// TestOrderedIndexScan_MultiKeySortMatchesIndex verifies that
// Sort(STATUS, DATE) matches an index on (STATUS, DATE, ...).
func TestOrderedIndexScan_MultiKeySortMatchesIndex(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	a3 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status_date_amount",
		[]string{"Order"},
		[]string{"STATUS", "DATE", "AMOUNT"},
		[]values.CorrelationIdentifier{a1, a2, a3},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "STATUS", Typ: values.UnknownType}},
			{Value: &values.FieldValue{Field: "DATE", Typ: values.UnknownType}},
		},
		q,
	)
	sortRef := expressions.InitialOf(sort)

	rule := NewOrderedIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, sortRef, ctx, nil)

	if len(results) != 1 {
		t.Fatalf("expected 1 yield (ordered index scan), got %d", len(results))
	}
}

// TestOrderedIndexScan_SortKeyMismatch verifies that Sort(AMOUNT)
// does NOT match an index on (STATUS, DATE).
func TestOrderedIndexScan_SortKeyMismatch(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status_date",
		[]string{"Order"},
		[]string{"STATUS", "DATE"},
		[]values.CorrelationIdentifier{a1, a2},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "AMOUNT", Typ: values.UnknownType}}},
		q,
	)
	sortRef := expressions.InitialOf(sort)

	rule := NewOrderedIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, sortRef, ctx, nil)

	if len(results) != 0 {
		t.Fatalf("expected 0 yields (sort key doesn't match index), got %d", len(results))
	}
}

// TestOrderedIndexScan_DescSortNotSatisfied verifies that Sort(STATUS DESC)
// does NOT match a forward index scan on STATUS.
func TestOrderedIndexScan_DescSortProducesReverseIndexScan(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "STATUS", Typ: values.UnknownType}, Reverse: true}},
		q,
	)
	sortRef := expressions.InitialOf(sort)

	rule := NewOrderedIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, sortRef, ctx, nil)

	if len(results) != 1 {
		t.Fatalf("expected 1 yield (DESC sort → reverse index scan), got %d", len(results))
	}
	w, ok := results[0].(*physicalIndexScanWrapper)
	if !ok {
		t.Fatalf("expected physicalIndexScanWrapper, got %T", results[0])
	}
	if !w.plan.IsReverse() {
		t.Fatal("expected reverse index scan for DESC sort")
	}
}

// TestOrderedIndexScan_PlannerIntegration verifies the full pipeline:
// Sort(STATUS) over Scan with an index on STATUS → sort eliminated,
// index scan appears at the top.
func TestOrderedIndexScan_PlannerIntegration(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "STATUS", Typ: values.UnknownType}}},
		q,
	)
	ref := expressions.InitialOf(sort)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(ref); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	foundIndexScanAtTop := false
	for _, m := range ref.AllMembers() {
		if IsPhysicalIndexScan(m) || IsPhysicalFetchFromPartialRecord(m) {
			foundIndexScanAtTop = true
			break
		}
	}
	if !foundIndexScanAtTop {
		t.Fatal("planner should produce index scan at top (sort eliminated by ordered index scan)")
	}
}
