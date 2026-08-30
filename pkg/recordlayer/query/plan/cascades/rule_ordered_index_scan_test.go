package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestOrderedIndexScan_SortMatchesIndex verifies that Sort(STATUS)
// over a bare scan yields an index scan when an index on STATUS exists.
func TestOrderedIndexScan_SortMatchesIndex(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := newKnownDistinctValueIndexCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		orderedScanTestRowType(),
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := mustOrderedScanFull(t, []string{"Order"})
	scanRef := mustOrderedScanInitial(t, scan)
	q := expressions.ForEachQuantifier(scanRef)
	sort := mustOrderedScanSort(t,
		[]expressions.SortKey{{Value: mustOrderedScanField(t, mustOrderedScanFlowed(t, q), "STATUS")}},
		q,
	)
	sortRef := mustOrderedScanInitial(t, sort)

	rule := NewOrderedIndexScanRule()
	results := mustFireExpressionRuleWithMemo(t, rule, sortRef, ctx, nil)

	if len(results) != 1 {
		t.Fatalf("expected 1 yield (ordered index scan), got %d", len(results))
	}
	if _, ok := results[0].(*plans.RecordQueryIndexPlan); !ok {
		t.Fatalf("expected *plans.RecordQueryIndexPlan, got %T", results[0])
	}
}

// TestOrderedIndexScan_MultiKeySortMatchesIndex verifies that
// Sort(STATUS, DATE) matches an index on (STATUS, DATE, ...).
func TestOrderedIndexScan_MultiKeySortMatchesIndex(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	a3 := values.UniqueCorrelationIdentifier()
	cand := newKnownDistinctValueIndexCandidate(
		"Order$status_date_amount",
		[]string{"Order"},
		[]string{"STATUS", "DATE", "AMOUNT"},
		[]values.CorrelationIdentifier{a1, a2, a3},
		orderedScanTestRowType(),
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := mustOrderedScanFull(t, []string{"Order"})
	scanRef := mustOrderedScanInitial(t, scan)
	q := expressions.ForEachQuantifier(scanRef)
	flowed := mustOrderedScanFlowed(t, q)
	sort := mustOrderedScanSort(t,
		[]expressions.SortKey{
			{Value: mustOrderedScanField(t, flowed, "STATUS")},
			{Value: mustOrderedScanField(t, flowed, "DATE")},
		},
		q,
	)
	sortRef := mustOrderedScanInitial(t, sort)

	rule := NewOrderedIndexScanRule()
	results := mustFireExpressionRuleWithMemo(t, rule, sortRef, ctx, nil)

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
	cand := newKnownDistinctValueIndexCandidate(
		"Order$status_date",
		[]string{"Order"},
		[]string{"STATUS", "DATE"},
		[]values.CorrelationIdentifier{a1, a2},
		orderedScanTestRowType(),
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := mustOrderedScanFull(t, []string{"Order"})
	scanRef := mustOrderedScanInitial(t, scan)
	q := expressions.ForEachQuantifier(scanRef)
	sort := mustOrderedScanSort(t,
		[]expressions.SortKey{{Value: mustOrderedScanField(t, mustOrderedScanFlowed(t, q), "AMOUNT")}},
		q,
	)
	sortRef := mustOrderedScanInitial(t, sort)

	rule := NewOrderedIndexScanRule()
	results := mustFireExpressionRuleWithMemo(t, rule, sortRef, ctx, nil)

	if len(results) != 0 {
		t.Fatalf("expected 0 yields (sort key doesn't match index), got %d", len(results))
	}
}

// TestOrderedIndexScan_DescSortProducesReverseIndexScan verifies that
// Sort(STATUS DESC) over an index on STATUS yields exactly one plan, and that
// it is a REVERSE index scan.
//
// The comment here used to say the opposite — that a DESC sort "does NOT match
// a forward index scan" — under a name from before reverse scans were
// implemented. It named a function that no longer exists and described a
// behaviour the body below contradicts, which is worse than no comment: a
// reader asking whether DESC is satisfiable would have got "no" from the prose
// and "yes, reversed" from the assertions.
func TestOrderedIndexScan_DescSortProducesReverseIndexScan(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := newKnownDistinctValueIndexCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		orderedScanTestRowType(),
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := mustOrderedScanFull(t, []string{"Order"})
	scanRef := mustOrderedScanInitial(t, scan)
	q := expressions.ForEachQuantifier(scanRef)
	sort := mustOrderedScanSort(t,
		[]expressions.SortKey{{Value: mustOrderedScanField(t, mustOrderedScanFlowed(t, q), "STATUS"), Reverse: true}},
		q,
	)
	sortRef := mustOrderedScanInitial(t, sort)

	rule := NewOrderedIndexScanRule()
	results := mustFireExpressionRuleWithMemo(t, rule, sortRef, ctx, nil)

	if len(results) != 1 {
		t.Fatalf("expected 1 yield (DESC sort → reverse index scan), got %d", len(results))
	}
	w, ok := results[0].(*plans.RecordQueryIndexPlan)
	if !ok {
		t.Fatalf("expected *plans.RecordQueryIndexPlan, got %T", results[0])
	}
	if !w.IsReverse() {
		t.Fatal("expected reverse index scan for DESC sort")
	}
}

// TestOrderedIndexScan_PlannerIntegration verifies the full pipeline:
// Sort(STATUS) over Scan with an index on STATUS → sort eliminated,
// index scan appears at the top.
func TestOrderedIndexScan_PlannerIntegration(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := newKnownDistinctValueIndexCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		orderedScanTestRowType(),
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := mustOrderedScanFull(t, []string{"Order"})
	scanRef := mustOrderedScanInitial(t, scan)
	q := expressions.ForEachQuantifier(scanRef)
	sort := mustOrderedScanSort(t,
		[]expressions.SortKey{{Value: mustOrderedScanField(t, mustOrderedScanFlowed(t, q), "STATUS")}},
		q,
	)
	ref := mustOrderedScanInitial(t, sort)

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

// TestOrderedIndexScan_RejectsFanOutCandidate pins the cardinality contract of
// the direct Sort(Scan) shortcut. A fan-out index orders its entries, but it can
// emit the same base record more than once (or no entry for an empty array), so
// replacing the base scan with that raw index scan changes the query's rows.
// The scalar candidate remains eligible for the same requested ordering.
func TestOrderedIndexScan_RejectsFanOutCandidate(t *testing.T) {
	t.Parallel()

	fanOut := true
	scalar := false
	newCandidate := func(name string, createsDuplicates *bool) MatchCandidate {
		return NewValueIndexScanMatchCandidateWithFunctions(
			name,
			[]string{"T"},
			[]string{"TAGS"},
			nil,
			[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
			orderedScanTestRowType(),
			false,
			nil,
			createsDuplicates,
			// A scalar tuple domain for the key coordinate. Sort elision now
			// depends on the physical key type, so leaving it Unknown would
			// make this test exercise the fail-closed path instead of the
			// fan-out cardinality contract it is named for.
		).WithKeyComponentTypes(syntheticIndexKeyTypes(1))
	}
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{
		newCandidate("T$tags_fanout", &fanOut),
		newCandidate("T$tags_scalar", &scalar),
	}}

	scan := mustOrderedScanFull(t, []string{"T"})
	quantifier := expressions.ForEachQuantifier(mustOrderedScanInitial(t, scan))
	sortExpr := mustOrderedScanSort(t,
		[]expressions.SortKey{{Value: mustOrderedScanField(t, mustOrderedScanFlowed(t, quantifier), "TAGS")}},
		quantifier,
	)
	results := mustFireExpressionRuleWithMemo(t,
		NewOrderedIndexScanRule(),
		mustOrderedScanInitial(t, sortExpr),
		ctx,
		nil,
	)

	if len(results) != 1 {
		t.Fatalf("expected only the scalar ordered-index alternative, got %d yields", len(results))
	}
	indexPlan, ok := results[0].(*plans.RecordQueryIndexPlan)
	if !ok {
		t.Fatalf("expected *plans.RecordQueryIndexPlan, got %T", results[0])
	}
	if got := indexPlan.GetIndexName(); got != "T$tags_scalar" {
		t.Fatalf("ordered shortcut selected %q, want the non-fan-out candidate", got)
	}
}
