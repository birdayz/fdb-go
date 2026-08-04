package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestSortElim_IndexProvidesSortOrder verifies that Sort(col) over an
// index scan that provides col ordering is eliminated during PLANNING
// by ImplementSortRule (matching Java's RemoveSortRule).
func TestSortElim_IndexProvidesSortOrder(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	cand := newKnownDistinctValueIndexCandidate(
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
	cand := newKnownDistinctValueIndexCandidate(
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
	cand := newKnownDistinctValueIndexCandidate(
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
	cand := newKnownDistinctValueIndexCandidate(
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
	cand := newKnownDistinctValueIndexCandidate(
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
	cand := newKnownDistinctValueIndexCandidate(
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
	if w, ok := plan.(*plans.RecordQueryIndexPlan); ok {
		if !w.IsReverse() {
			t.Fatal("DESC sort elimination should produce a reverse index scan")
		}
	} else if fw, ok := plan.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
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
	w := idx.WithIndexMetadata([]string{"A", "B"}, nil, true).
		WithKeyComponentTypes([]values.Type{values.NotNullLong, values.NotNullLong})

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
	w := idx.WithIndexMetadata([]string{"A", "B", "C"}, nil, true).
		WithKeyComponentTypes([]values.Type{values.NotNullLong, values.NotNullLong, values.NotNullLong})

	// numKeys < len(columnNames): partial coverage.
	if strictlyOrderedIfUnique(w, 2) {
		t.Fatal("unique index with numKeys < len(columns) should NOT be strictly ordered")
	}

	if strictlyOrderedIfUnique(w, 0) {
		t.Fatal("unique index with numKeys=0 should NOT be strictly ordered")
	}
}

func TestStrictlySorted_UniqueStorageKeyMustMatchLogicalEquality(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		typ  values.Type
	}{
		{name: "nullable", typ: values.NullableLong},
		{name: "unknown", typ: values.UnknownType},
		{name: "FLOAT raw NaN encodings", typ: values.NotNullFloat},
		{name: "DOUBLE raw NaN encodings", typ: values.NotNullDouble},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			index := plans.NewRecordQueryIndexPlan(
				"idx_u", nil, []string{"T"}, values.UnknownType, false,
			).WithIndexMetadata([]string{"A"}, nil, true).
				WithKeyComponentTypes([]values.Type{test.typ})
			if strictlyOrderedIfUnique(index, 1) {
				t.Fatal("storage UNIQUE was treated as globally strict although raw tuple uniqueness is not congruent with logical equality")
			}
		})
	}
}

// TestStrictlySorted_NonUniqueIndex: non-unique index should never be
// strictly ordered, regardless of numKeys.
func TestStrictlySorted_NonUniqueIndex(t *testing.T) {
	t.Parallel()

	idx := plans.NewRecordQueryIndexPlan("idx_nu", nil, []string{"T"}, values.UnknownType, false)
	w := idx.WithIndexMetadata([]string{"A"}, nil, false)

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
	w := scan

	if strictlyOrderedIfUnique(w, 100) {
		t.Fatal("non-index expression should never be strictly ordered")
	}
}

// ---------------------------------------------------------------------------
// makeStrictlySorted unit tests
// ---------------------------------------------------------------------------

// TestMakeStrictlySorted_IndexScan: makeStrictlySorted on a
// *plans.RecordQueryIndexPlan creates a new plan whose strictlySorted
// flag is true.
func TestMakeStrictlySorted_IndexScan(t *testing.T) {
	t.Parallel()

	idx := plans.NewRecordQueryIndexPlan("idx_x", nil, []string{"T"}, values.UnknownType, false)
	orig := idx.WithIndexMetadata([]string{"A", "B"}, nil, true)

	result := makeStrictlySorted(orig)

	// Must return a new *plans.RecordQueryIndexPlan, not the same pointer.
	resultW, ok := result.(*plans.RecordQueryIndexPlan)
	if !ok {
		t.Fatal("makeStrictlySorted should return a *plans.RecordQueryIndexPlan")
	}
	if resultW == orig {
		t.Fatal("makeStrictlySorted should return a new plan, not the original")
	}

	// The result plan should be strictlySorted.
	if !resultW.IsStrictlySorted() {
		t.Fatal("result plan should be strictlySorted")
	}

	// Original must be unmodified.
	if orig.IsStrictlySorted() {
		t.Fatal("original plan should remain non-strictlySorted")
	}

	// Metadata preserved.
	if cols := resultW.GetColumnNames(); len(cols) != 2 || cols[0] != "A" || cols[1] != "B" {
		t.Fatalf("columnNames = %v, want [A B]", cols)
	}
	if !resultW.IsUnique() {
		t.Fatal("unique flag should be preserved")
	}
}

// TestMakeStrictlySorted_NonIndexScan: makeStrictlySorted on a
// non-index expression returns the expression unchanged.
func TestMakeStrictlySorted_NonIndexScan(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	w := scan

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
	orig := idx.WithStrictlySorted().WithIndexMetadata([]string{"A"}, nil, true)

	result := makeStrictlySorted(orig)
	resultW := result.(*plans.RecordQueryIndexPlan)
	if !resultW.IsStrictlySorted() {
		t.Fatal("double makeStrictlySorted should still be strictlySorted")
	}
}

// ---------------------------------------------------------------------------
// End-to-end planner tests for strictlySorted via ImplementSortRule
// ---------------------------------------------------------------------------

// buildStatusActiveIndexScan builds the physical index-scan expression the data-access
// path produces for `STATUS = 'active'` against a (STATUS, DATE) candidate — using the
// candidate's own ToScanPlan, the same primitive the match path uses. Replaces the
// retired ImplementIndexScanRule in these sort-elimination setups (RFC-076).
func buildStatusActiveIndexScan(t *testing.T, cand MatchCandidate) expressions.RelationalExpression {
	t.Helper()
	aliases := cand.GetSargableAliases()
	if len(aliases) == 0 {
		t.Fatal("candidate has no sargable aliases")
	}
	cmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, "active")
	res := predicates.EmptyComparisonRange().Merge(&cmp)
	if !res.Ok {
		t.Fatal("failed to build equality comparison range")
	}
	bindings := map[values.CorrelationIdentifier]*predicates.ComparisonRange{aliases[0]: res.Range}
	prefix := cand.ComputeBoundParameterPrefixMap(bindings)
	if len(prefix) == 0 {
		t.Fatal("candidate produced empty prefix for STATUS equality")
	}
	idxPlan := cand.ToScanPlan(prefix, false)
	if fetchPlan, ok := idxPlan.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
		innerIdx, ok := fetchPlan.GetInner().(*plans.RecordQueryIndexPlan)
		if !ok {
			t.Fatalf("expected RecordQueryIndexPlan inside Fetch, got %T", fetchPlan.GetInner())
		}
		idxWrapper := innerIdx.WithIndexMetadata(cand.GetColumnNames(), nil, cand.IsUnique())
		fetchQ := expressions.ForEachQuantifier(expressions.InitialOf(idxWrapper))
		return fetchPlan.WithQuantifiers([]expressions.Quantifier{fetchQ})
	}
	if ip := extractIndexPlan(idxPlan); ip != nil {
		return ip.WithIndexMetadata(cand.GetColumnNames(), nil, cand.IsUnique())
	}
	t.Fatalf("candidate ToScanPlan produced unexpected plan %T", idxPlan)
	return nil
}

// TestPlanner_StrictlySorted_UniqueIndex verifies that ImplementSortRule
// marks a plan as strictlySorted when a unique index covers all sort keys.
//
// Setup: a LogicalSortExpression(DATE ASC) whose inner Reference contains
// a single *plans.RecordQueryIndexPlan for unique index (STATUS, DATE) with
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
	cand := newKnownDistinctValueIndexCandidate(
		"Order$status_date",
		[]string{"Order"},
		[]string{"STATUS", "DATE"},
		[]values.CorrelationIdentifier{a1, a2},
		values.UnknownType,
		true, // unique
		nil,
	).WithKeyComponentTypes([]values.Type{values.NotNullLong, values.NotNullLong})
	// Build the index scan the data-access path produces for STATUS='active'
	// (the retired ImplementIndexScanRule's job, RFC-076), then compute plan
	// properties on a clean inner Reference (simulating implementBottomUp).
	idxExpr := buildStatusActiveIndexScan(t, cand)
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
		if w, ok := e.(*plans.RecordQueryIndexPlan); ok && w.IsStrictlySorted() {
			foundStrictly = w
		}
		if fw, ok := e.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
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
	cand := newKnownDistinctValueIndexCandidate(
		"Order$status_date",
		[]string{"Order"},
		[]string{"STATUS", "DATE"},
		[]values.CorrelationIdentifier{a1, a2},
		values.UnknownType,
		false, // non-unique
		nil,
	)
	// Build the index scan the data-access path produces for STATUS='active'
	// (the retired ImplementIndexScanRule's job, RFC-076).
	idxExpr := buildStatusActiveIndexScan(t, cand)
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
		if w, ok := e.(*plans.RecordQueryIndexPlan); ok && w.IsStrictlySorted() {
			t.Fatalf("non-unique index should NOT produce a strictlySorted plan; got %s", w.Explain())
		}
		if fw, ok := e.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
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

// distinctnessProbeLayout is the FLOWED row the sort keys resolve against.
// A real record type is load-bearing, not scaffolding: the ordering-truncation
// machinery resolves key names against the flowed layout, and values.UnknownType
// never terminates that resolution — so an untyped fixture silently advertises
// an UNtruncated ordering and never reaches the arm under test at all.
func distinctnessProbeLayout(bType values.Type) values.Type {
	return values.NewRecordType("T", false, []values.Field{
		{Name: "A", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "B", FieldType: bType, Ordinal: 1},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 2},
	})
}

// distinctnessProbeSortKey mints a sort key that can actually MATCH an
// advertised ordering key. A bare &values.FieldValue{Field: "A"} explains as
// "A" while the advertised key explains as "A#0", so the coverage loop compares
// two strings that never agree and the arm is skipped — which is how a test can
// look like it exercises this and exercise nothing.
func distinctnessProbeSortKey(layout values.Type, name string) values.Value {
	ident, ok := values.OrdinalOfNameIn(layout, name)
	if !ok {
		panic("distinctness probe: " + name + " is not in the probe layout")
	}
	return values.NewFieldValueWithResolvedOrdinalInDomain(
		name, ident.Ordinal, values.UnknownType, ident.Domain)
}

// ImplementSortRule's distinct-partition arm marks a plan STRICTLY sorted on
// the strength of record distinctness. That is sound only when the storage key
// whose order is being borrowed agrees with the logical comparator over every
// coordinate it advertises — index key and appended primary-key suffix both.
//
// The two arms are a FILTER test, not an on/off test, and both are required:
//
//   - FLOAT arm. B is DOUBLE, so raw NaN sign and payload survive to disk and
//     put two logically-equal keys on opposite sides of every finite value.
//     The advertised ordering is truncated to [A] at that barrier, which means
//     ORDER BY A covers every advertised key and the arm IS entered. The proof
//     is then the only thing standing between record distinctness and a
//     strictness claim the storage cannot honour — the PK suffix past the
//     barrier is not in comparator order.
//   - All-LONG control. Nothing truncates, so ORDER BY A, B, ID is needed to
//     cover the advertised keys, and the arm must still yield STRICT. Without
//     this the FLOAT arm is satisfied by a gate that refuses everything, which
//     is indistinguishable from deleting the optimization.
func TestPlanner_NaNBarrierPrefixIsOrderedButNotStrict(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		bType      values.Type
		sortKeys   []string
		wantStrict bool
	}{
		{
			name:     "DOUBLE barrier truncates the advertised ordering to [A]",
			bType:    values.NotNullDouble,
			sortKeys: []string{"A"},
		},
		{
			name:       "all-LONG key advertises A, B, ID and may be strict",
			bType:      values.NotNullLong,
			sortKeys:   []string{"A", "B", "ID"},
			wantStrict: true,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			layout := distinctnessProbeLayout(test.bType)
			idx := plans.NewRecordQueryIndexPlan(
				"T_AB", nil, []string{"T"}, layout, false,
			).WithIndexMetadata(
				[]string{"A", "B"}, []string{"ID"}, false,
			).WithKeyComponentTypes(
				[]values.Type{values.NotNullLong, test.bType},
			).WithPrimaryKeyComponentTypes(
				[]values.Type{values.NotNullLong},
			).WithDistinctRecordsSignal(false)

			innerRef := expressions.InitialOf(idx)
			computeRefPlanProperties(innerRef)
			keys := make([]expressions.SortKey, 0, len(test.sortKeys))
			for _, name := range test.sortKeys {
				keys = append(keys, expressions.SortKey{
					Value: distinctnessProbeSortKey(layout, name),
				})
			}
			sortExpr := expressions.NewLogicalSortExpression(
				keys, expressions.ForEachQuantifier(innerRef),
			)
			yielded := FireImplementationRule(
				NewImplementSortRule(), expressions.InitialOf(sortExpr),
			)
			if len(yielded) == 0 {
				t.Fatal("the physical key prefix should satisfy the requested ordering")
			}
			gotStrict := false
			for _, expr := range yielded {
				if plan, ok := expr.(*plans.RecordQueryIndexPlan); ok && plan.IsStrictlySorted() {
					gotStrict = true
				}
			}
			if gotStrict != test.wantStrict {
				if test.wantStrict {
					t.Fatal("a fully comparator-congruent storage key was refused a " +
						"strict ordering — the gate is an off switch, not a filter, " +
						"and the float arm above now proves nothing")
				}
				t.Fatal("record distinctness claimed a strict ordering across a raw " +
					"NaN barrier, which the storage key does not deliver")
			}
		})
	}
}
