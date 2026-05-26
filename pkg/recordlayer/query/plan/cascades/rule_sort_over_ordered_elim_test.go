package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// TestSortElim_IndexProvidesSortOrder verifies that Sort(col) over an
// index scan that provides col ordering is eliminated during PLANNING
// by ImplementSortRule (matching Java's RemoveSortRule).
func TestSortElim_IndexProvidesSortOrder(t *testing.T) {
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
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	filterQ := expressions.ForEachQuantifier(filterRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "DATE", Typ: values.UnknownType}}},
		filterQ,
	)
	sortRef := expressions.InitialOf(sort)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(sortRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if !IsPhysicalIndexScan(plan) && !IsPhysicalFilter(plan) && !IsPhysicalFetchFromPartialRecord(plan) {
		t.Fatalf("sort should be eliminated when index provides the ordering; got %T", plan)
	}
}

// TestSortElim_MultiKeySortMatchesIndex verifies that
// Sort(DATE, AMOUNT) is eliminated when the index on (STATUS, DATE, AMOUNT)
// with STATUS equality-bound provides (DATE, AMOUNT) ordering.
func TestSortElim_MultiKeySortMatchesIndex(t *testing.T) {
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
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	filterQ := expressions.ForEachQuantifier(filterRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "DATE", Typ: values.UnknownType}},
			{Value: &values.FieldValue{Field: "AMOUNT", Typ: values.UnknownType}},
		},
		filterQ,
	)
	sortRef := expressions.InitialOf(sort)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(sortRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if !IsPhysicalIndexScan(plan) && !IsPhysicalFilter(plan) && !IsPhysicalFetchFromPartialRecord(plan) {
		t.Fatalf("multi-key sort should be eliminated; got %T", plan)
	}
}

// TestSortElim_PartialSortKeyMatch verifies that Sort(DATE, AMOUNT)
// is NOT eliminated when the index only provides (DATE) ordering (prefix
// of sort keys is not sufficient — need ALL sort keys satisfied).
func TestSortElim_PartialSortKeyMatch(t *testing.T) {
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
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	filterQ := expressions.ForEachQuantifier(filterRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "DATE", Typ: values.UnknownType}},
			{Value: &values.FieldValue{Field: "AMOUNT", Typ: values.UnknownType}},
		},
		filterQ,
	)
	sortRef := expressions.InitialOf(sort)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(sortRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// Sort should NOT be eliminated — index provides (DATE) but sort
	// requires (DATE, AMOUNT). The top-level plan must be an in-memory sort.
	if IsPhysicalIndexScan(plan) || IsPhysicalFilter(plan) || IsPhysicalFetchFromPartialRecord(plan) {
		t.Fatal("sort should NOT be eliminated when index provides fewer ordering keys than sort requires")
	}
}

// TestSortElim_RangeScanProvidesSortOrder verifies that
// Sort(STATUS) over a range predicate (status > 'a') with index on (STATUS)
// eliminates the sort — the index scan produces rows in STATUS order even
// for inequality bounds.
func TestSortElim_RangeScanProvidesSortOrder(t *testing.T) {
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
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, "a"),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	filterQ := expressions.ForEachQuantifier(filterRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "STATUS", Typ: values.UnknownType}},
		},
		filterQ,
	)
	sortRef := expressions.InitialOf(sort)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(sortRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if !IsPhysicalIndexScan(plan) && !IsPhysicalFilter(plan) && !IsPhysicalFetchFromPartialRecord(plan) {
		t.Fatalf("sort should be eliminated when range-bound index scan provides the ordering; got %T", plan)
	}
}

// TestSortElim_SortKeyNotProvidedByIndex verifies that
// Sort(AMOUNT) is NOT eliminated when the index provides DATE ordering.
func TestSortElim_SortKeyNotProvidedByIndex(t *testing.T) {
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
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	// Sort by AMOUNT — index provides DATE ordering, not AMOUNT.
	filterQ := expressions.ForEachQuantifier(filterRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "AMOUNT", Typ: values.UnknownType}}},
		filterQ,
	)
	sortRef := expressions.InitialOf(sort)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(sortRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// The sort should NOT be eliminated — the index doesn't provide
	// AMOUNT ordering. The top-level plan must be an in-memory sort.
	if IsPhysicalIndexScan(plan) || IsPhysicalFilter(plan) || IsPhysicalFetchFromPartialRecord(plan) {
		t.Fatal("sort should NOT be eliminated when index doesn't provide the sort key")
	}
}

// TestSortElim_DescSortEliminated verifies that a DESC
// sort over an index scan IS eliminated — the planner produces a
// reverse index scan whose descending ordering matches the sort.
func TestSortElim_DescSortEliminated(t *testing.T) {
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

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(sortRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if !IsPhysicalIndexScan(plan) && !IsPhysicalFilter(plan) && !IsPhysicalFetchFromPartialRecord(plan) {
		t.Fatalf("DESC sort should be eliminated by a reverse index scan; got %T", plan)
	}
	if w, ok := plan.(*physicalIndexScanWrapper); ok {
		if !w.plan.IsReverse() {
			t.Fatal("DESC sort elimination should produce a reverse index scan")
		}
	} else if fw, ok := plan.(*physicalFetchFromPartialRecordWrapper); ok {
		if innerIdx := extractIndexPlan(fw.GetRecordQueryPlan()); innerIdx != nil {
			if !innerIdx.IsReverse() {
				t.Fatal("DESC sort elimination should produce a reverse index scan")
			}
		}
	}
}

// ---------------------------------------------------------------------------
// strictlyOrderedIfUnique unit tests
// ---------------------------------------------------------------------------

// TestStrictlySorted_UniqueIndexFullCoverage: unique index with numKeys
// covering all columns should be detected as strictly ordered.
func TestStrictlySorted_UniqueIndexFullCoverage(t *testing.T) {
	t.Parallel()

	idx := plans.NewRecordQueryIndexPlan("idx_u", nil, []string{"T"}, values.UnknownType, false)
	w := &physicalIndexScanWrapper{
		plan:        idx,
		columnNames: []string{"A", "B"},
		unique:      true,
	}

	// numKeys == len(columnNames): full coverage.
	if !strictlyOrderedIfUnique(w, 2) {
		t.Fatal("unique index with numKeys == len(columns) should be strictly ordered")
	}

	// numKeys > len(columnNames): still covers everything.
	if !strictlyOrderedIfUnique(w, 5) {
		t.Fatal("unique index with numKeys > len(columns) should be strictly ordered")
	}
}

// TestStrictlySorted_UniqueIndexPartialCoverage: unique index but numKeys
// less than the number of columns — not enough coverage.
func TestStrictlySorted_UniqueIndexPartialCoverage(t *testing.T) {
	t.Parallel()

	idx := plans.NewRecordQueryIndexPlan("idx_u", nil, []string{"T"}, values.UnknownType, false)
	w := &physicalIndexScanWrapper{
		plan:        idx,
		columnNames: []string{"A", "B", "C"},
		unique:      true,
	}

	// numKeys < len(columnNames): partial coverage.
	if strictlyOrderedIfUnique(w, 2) {
		t.Fatal("unique index with numKeys < len(columns) should NOT be strictly ordered")
	}

	if strictlyOrderedIfUnique(w, 0) {
		t.Fatal("unique index with numKeys=0 should NOT be strictly ordered")
	}
}

// TestStrictlySorted_NonUniqueIndex: non-unique index should never be
// strictly ordered, regardless of numKeys.
func TestStrictlySorted_NonUniqueIndex(t *testing.T) {
	t.Parallel()

	idx := plans.NewRecordQueryIndexPlan("idx_nu", nil, []string{"T"}, values.UnknownType, false)
	w := &physicalIndexScanWrapper{
		plan:        idx,
		columnNames: []string{"A"},
		unique:      false,
	}

	if strictlyOrderedIfUnique(w, 1) {
		t.Fatal("non-unique index should NOT be strictly ordered even with full coverage")
	}
	if strictlyOrderedIfUnique(w, 100) {
		t.Fatal("non-unique index should NOT be strictly ordered even with excess numKeys")
	}
}

// TestStrictlyOrderedIfUnique_NonIndexExpression: a non-index expression
// should never be strictly ordered.
func TestStrictlyOrderedIfUnique_NonIndexExpression(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	w := &physicalScanWrapper{plan: scan}

	if strictlyOrderedIfUnique(w, 100) {
		t.Fatal("non-index expression should never be strictly ordered")
	}
}

// ---------------------------------------------------------------------------
// makeStrictlySorted unit tests
// ---------------------------------------------------------------------------

// TestMakeStrictlySorted_IndexScan: makeStrictlySorted on a
// physicalIndexScanWrapper creates a new wrapper whose inner plan has
// strictlySorted=true.
func TestMakeStrictlySorted_IndexScan(t *testing.T) {
	t.Parallel()

	idx := plans.NewRecordQueryIndexPlan("idx_x", nil, []string{"T"}, values.UnknownType, false)
	orig := &physicalIndexScanWrapper{
		plan:        idx,
		columnNames: []string{"A", "B"},
		unique:      true,
	}

	result := makeStrictlySorted(orig)

	// Must return a new physicalIndexScanWrapper, not the same pointer.
	resultW, ok := result.(*physicalIndexScanWrapper)
	if !ok {
		t.Fatal("makeStrictlySorted should return a physicalIndexScanWrapper")
	}
	if resultW == orig {
		t.Fatal("makeStrictlySorted should return a new wrapper, not the original")
	}

	// The inner plan should be strictlySorted.
	if !resultW.plan.IsStrictlySorted() {
		t.Fatal("result plan should be strictlySorted")
	}

	// Original must be unmodified.
	if orig.plan.IsStrictlySorted() {
		t.Fatal("original plan should remain non-strictlySorted")
	}

	// Metadata preserved.
	if len(resultW.columnNames) != 2 || resultW.columnNames[0] != "A" || resultW.columnNames[1] != "B" {
		t.Fatalf("columnNames = %v, want [A B]", resultW.columnNames)
	}
	if !resultW.unique {
		t.Fatal("unique flag should be preserved")
	}
}

// TestMakeStrictlySorted_NonIndexScan: makeStrictlySorted on a
// non-index expression returns the expression unchanged.
func TestMakeStrictlySorted_NonIndexScan(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	w := &physicalScanWrapper{plan: scan}

	result := makeStrictlySorted(w)
	if result != w {
		t.Fatal("makeStrictlySorted on non-index expression should return the same pointer")
	}
}

// TestMakeStrictlySorted_Idempotent: calling makeStrictlySorted on an
// already-strictlySorted wrapper still produces a correct result.
func TestMakeStrictlySorted_Idempotent(t *testing.T) {
	t.Parallel()

	idx := plans.NewRecordQueryIndexPlan("idx_idem", nil, []string{"T"}, values.UnknownType, false)
	orig := &physicalIndexScanWrapper{
		plan:        idx.WithStrictlySorted(),
		columnNames: []string{"A"},
		unique:      true,
	}

	result := makeStrictlySorted(orig)
	resultW := result.(*physicalIndexScanWrapper)
	if !resultW.plan.IsStrictlySorted() {
		t.Fatal("double makeStrictlySorted should still be strictlySorted")
	}
}

// ---------------------------------------------------------------------------
// End-to-end planner tests for strictlySorted via ImplementSortRule
// ---------------------------------------------------------------------------

// TestPlanner_StrictlySorted_UniqueIndex verifies that ImplementSortRule
// marks a plan as strictlySorted when a unique index covers all sort keys.
//
// Setup: a LogicalSortExpression(DATE ASC) whose inner Reference contains
// a single physicalIndexScanWrapper for unique index (STATUS, DATE) with
// STATUS equality-bound. The inner Reference has pre-computed plan
// properties so ToPlanPartitions uses the PlanPropertiesMap path
// (as it would during a real Plan() call).
//
// ImplementSortRule sees partition.IsDistinct()=true, all ordering keys
// covered by sort + equality-bound keys, and yields makeStrictlySorted.
func TestPlanner_StrictlySorted_UniqueIndex(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status_date",
		[]string{"Order"},
		[]string{"STATUS", "DATE"},
		[]values.CorrelationIdentifier{a1, a2},
		values.UnknownType,
		true, // unique
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	// Build: Filter(STATUS = 'active') -> Scan(Order)
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	// Run ImplementIndexScanRule to produce the physicalIndexScanWrapper.
	indexRule := NewImplementIndexScanRule()
	idxResults := FireExpressionRuleWithMemo(indexRule, filterRef, ctx, nil)
	if len(idxResults) == 0 {
		t.Fatal("ImplementIndexScanRule should produce an index scan")
	}

	// Build a clean inner Reference with the index scan result (now
	// Fetch(IndexScan)), then compute plan properties (simulating
	// implementBottomUp).
	var idxExpr expressions.RelationalExpression
	for _, r := range idxResults {
		if _, ok := r.(*physicalIndexScanWrapper); ok {
			idxExpr = r
			break
		}
		if _, ok := r.(*physicalFetchFromPartialRecordWrapper); ok {
			idxExpr = r
			break
		}
	}
	if idxExpr == nil {
		t.Fatal("no index scan expression in ImplementIndexScanRule results")
	}

	innerRef := expressions.InitialOf(idxExpr)
	computeRefPlanProperties(innerRef)

	// Build Sort(DATE ASC) over the prepared inner Reference.
	sortQ := expressions.ForEachQuantifier(innerRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "DATE", Typ: values.UnknownType}}},
		sortQ,
	)
	sortRef := expressions.InitialOf(sort)

	// Fire ImplementSortRule directly on the sort reference.
	rule := NewImplementSortRule()
	yielded := FireImplementationRule(rule, sortRef)

	// Check that at least one yielded expression is strictlySorted.
	// With Fetch wrappers, look for an index plan at any level.
	var foundStrictly *plans.RecordQueryIndexPlan
	for _, e := range yielded {
		if w, ok := e.(*physicalIndexScanWrapper); ok && w.plan.IsStrictlySorted() {
			foundStrictly = w.plan
		}
		if fw, ok := e.(*physicalFetchFromPartialRecordWrapper); ok {
			if inner := extractIndexPlan(fw.GetRecordQueryPlan()); inner != nil && inner.IsStrictlySorted() {
				foundStrictly = inner
			}
		}
	}
	if foundStrictly == nil {
		t.Fatalf("ImplementSortRule should yield a strictlySorted plan for unique index; yielded %d expressions", len(yielded))
	}
}

// TestPlanner_StrictlySorted_NonUniqueIndex is the negative counterpart:
// same setup but with a NON-unique index. ImplementSortRule should still
// yield the plan (sort eliminated), but strictlySorted must be false.
func TestPlanner_StrictlySorted_NonUniqueIndex(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status_date",
		[]string{"Order"},
		[]string{"STATUS", "DATE"},
		[]values.CorrelationIdentifier{a1, a2},
		values.UnknownType,
		false, // non-unique
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	indexRule := NewImplementIndexScanRule()
	idxResults := FireExpressionRuleWithMemo(indexRule, filterRef, ctx, nil)
	if len(idxResults) == 0 {
		t.Fatal("ImplementIndexScanRule should produce an index scan")
	}

	var idxExpr expressions.RelationalExpression
	for _, r := range idxResults {
		if _, ok := r.(*physicalIndexScanWrapper); ok {
			idxExpr = r
			break
		}
		if _, ok := r.(*physicalFetchFromPartialRecordWrapper); ok {
			idxExpr = r
			break
		}
	}
	if idxExpr == nil {
		t.Fatal("no index scan expression in ImplementIndexScanRule results")
	}

	innerRef := expressions.InitialOf(idxExpr)
	computeRefPlanProperties(innerRef)

	sortQ := expressions.ForEachQuantifier(innerRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "DATE", Typ: values.UnknownType}}},
		sortQ,
	)
	sortRef := expressions.InitialOf(sort)

	rule := NewImplementSortRule()
	yielded := FireImplementationRule(rule, sortRef)

	// The rule should yield the plan (sort eliminated) but NOT strictlySorted.
	for _, e := range yielded {
		if w, ok := e.(*physicalIndexScanWrapper); ok && w.plan.IsStrictlySorted() {
			t.Fatalf("non-unique index should NOT produce a strictlySorted plan; got %s", w.plan.Explain())
		}
		if fw, ok := e.(*physicalFetchFromPartialRecordWrapper); ok {
			if inner := extractIndexPlan(fw.GetRecordQueryPlan()); inner != nil && inner.IsStrictlySorted() {
				t.Fatalf("non-unique index should NOT produce a strictlySorted plan; got %s", inner.Explain())
			}
		}
	}
	// Verify the rule DID yield something (sort was eliminated).
	if len(yielded) == 0 {
		t.Fatal("ImplementSortRule should yield at least one expression (sort eliminated)")
	}
}
