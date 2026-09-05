package cascades

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustReferenceWinnerConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct reference-winner fixture: " + err.Error())
	}
	return value
}

func referenceWinnerRowType(recordType string, columns ...string) *values.RecordType {
	fields := make([]values.Field, len(columns))
	for ordinal, column := range columns {
		fieldType := values.NullableLong
		if column == "STATUS" {
			fieldType = values.NullableString
		}
		fields[ordinal] = values.Field{Name: column, FieldType: fieldType, Ordinal: ordinal}
	}
	return values.NewRecordType(recordType, false, fields)
}

func referenceWinnerFullScan(
	t testing.TB,
	recordType string,
	columns ...string,
) *expressions.FullUnorderedScanExpression {
	t.Helper()
	return mustReferenceWinnerConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{recordType}, referenceWinnerRowType(recordType, columns...)))
}

func referenceWinnerPlanScan(
	t testing.TB,
	recordType string,
	columns ...string,
) *plans.RecordQueryScanPlan {
	t.Helper()
	return mustReferenceWinnerConstruct(plans.NewRecordQueryScanPlan(
		[]string{recordType}, referenceWinnerRowType(recordType, columns...), false))
}

func referenceWinnerField(
	t testing.TB,
	owner values.Value,
	ordinal int,
) values.Value {
	t.Helper()
	return mustReferenceWinnerConstruct(values.ResolveFieldOrdinals(owner, []int{ordinal}))
}

func referenceWinnerQuantifiedField(
	t testing.TB,
	quantifier expressions.Quantifier,
	ordinal int,
) values.Value {
	t.Helper()
	root := mustReferenceWinnerConstruct(quantifier.RequireFlowedObjectValue())
	return referenceWinnerField(t, root, ordinal)
}

func referenceWinnerStatusCandidate(rowType values.Type) *ValueIndexScanMatchCandidate {
	createsDuplicates := false
	return NewValueIndexScanMatchCandidateWithFunctions(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		nil,
		[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
		rowType,
		false,
		nil,
		&createsDuplicates,
	).WithKeyComponentTypes([]values.Type{values.NullableString})
}

type referenceWinnerPlanContext struct {
	candidates []MatchCandidate
}

func (*referenceWinnerPlanContext) GetPlannerConfiguration() PlannerConfiguration {
	configuration := DefaultPlannerConfiguration()
	configuration.SingleReadVersion = true
	return configuration
}

func (c *referenceWinnerPlanContext) GetMatchCandidates() []MatchCandidate {
	return c.candidates
}

func (*referenceWinnerPlanContext) GetPrimaryKeyColumns(string) []string { return nil }

func referenceWinnerExploreRewriting(p *Planner, rootRef *expressions.Reference) (int, bool) {
	if rootRef == nil {
		return 0, true
	}
	if p.memo == nil {
		p.memo = NewMemo(rootRef)
	}
	if p.constraintMap == nil {
		p.constraintMap = NewConstraintMap()
	}
	if p.dataAccessConsumed == nil {
		p.dataAccessConsumed = make(map[*expressions.Reference]int)
	}
	p.push(&OptimizeGroupTask{Phase: PhaseRewriting, Ref: rootRef})
	p.push(&ExploreGroupTask{Phase: PhaseRewriting, Ref: rootRef})
	for len(p.stack) > 0 {
		if p.tasksRun >= p.MaxTasks {
			return p.tasksRun, false
		}
		p.pop().Run(context.Background(), p)
		p.tasksRun++
	}
	return p.tasksRun, true
}

func mustReferenceWinnerFireExpressionRule(
	t testing.TB,
	rule ExpressionRule,
	ref *expressions.Reference,
) {
	t.Helper()
	if _, err := FireExpressionRule(rule, ref); err != nil {
		t.Fatalf("FireExpressionRule() unexpected error: %v", err)
	}
}

func mustReferenceWinnerWithQuantifiers(
	t testing.TB,
	expression expressions.RelationalExpression,
	quantifiers []expressions.Quantifier,
) expressions.RelationalExpression {
	t.Helper()
	return mustReferenceWinnerConstruct(expression.WithQuantifiers(quantifiers))
}

func referenceWinnerSortedMemberOn(
	t testing.TB,
	fields ...string,
) expressions.RelationalExpression {
	t.Helper()
	rowFields := fields
	if len(fields) == 1 && (fields[0] == "A" || fields[0] == "B") {
		// The semantic-equality fixture needs both children to flow the SAME
		// exact row shape; only the ordering coordinate may differ.
		rowFields = []string{"A", "B"}
	}
	inner := referenceWinnerPlanScan(t, "T", rowFields...)
	keys := make([]plans.SortKey, len(fields))
	for keyOrdinal, field := range fields {
		fieldOrdinal := keyOrdinal
		if len(rowFields) == 2 && len(fields) == 1 && field == "B" {
			fieldOrdinal = 1
		}
		keys[keyOrdinal] = plans.SortKey{
			Field:      field,
			ValueExpr:  referenceWinnerField(t, inner.GetResultValue(), fieldOrdinal),
			NullsFirst: true,
		}
	}
	return mustReferenceWinnerConstruct(plans.NewRecordQueryInMemorySortPlan(inner, keys))
}

func TestReference_Winner_NoWinner(t *testing.T) {
	t.Parallel()
	scan := referenceWinnerFullScan(t, "T", "ID")
	ref := expressions.InitialOf(scan)
	if ref.Winner() != nil {
		t.Fatal("expected nil winner")
	}
	if ref.HasWinner() {
		t.Fatal("expected no winner")
	}
}

func TestReference_Winner_SetAndGet(t *testing.T) {
	t.Parallel()
	scan := referenceWinnerFullScan(t, "T", "ID")
	ref := expressions.InitialOf(scan)
	ref.SetWinner(scan)
	if ref.Winner() != scan {
		t.Fatal("expected scan as winner")
	}
	if !ref.HasWinner() {
		t.Fatal("expected winner present")
	}
}

func TestReference_Winner_Overwrites(t *testing.T) {
	t.Parallel()
	scan1 := referenceWinnerFullScan(t, "A", "ID")
	scan2 := referenceWinnerFullScan(t, "B", "ID")
	ref := expressions.InitialOf(scan1)
	ref.Insert(scan2)

	ref.SetWinner(scan1)
	ref.SetWinner(scan2)
	if ref.Winner() != scan2 {
		t.Fatal("second SetWinner should overwrite first")
	}
}

func TestReference_WinnerInvalidatedByMemberGrowth(t *testing.T) {
	t.Parallel()

	first := referenceWinnerFullScan(t, "A", "ID")
	second := referenceWinnerFullScan(t, "B", "ID")
	third := referenceWinnerFullScan(t, "C", "ID")
	ref := expressions.InitialOf(first)

	ref.SetWinner(first)
	if !ref.Insert(second) {
		t.Fatal("expected exploratory member growth")
	}
	if ref.HasWinner() {
		t.Fatal("exploratory member growth retained a winner stamped over the old member set")
	}

	ref.SetWinner(second)
	if !ref.InsertFinal(third) {
		t.Fatal("expected final member growth")
	}
	if ref.HasWinner() {
		t.Fatal("final member growth retained a winner stamped over the old physical candidate set")
	}

	ref.SetWinner(third)
	if ref.InsertFinal(third) {
		t.Fatal("duplicate final insertion unexpectedly mutated the member set")
	}
	if ref.Winner() != third {
		t.Fatal("duplicate insertion invalidated an otherwise-current winner")
	}
}

func TestSortElimination_ViaChildOrderedMember(t *testing.T) {
	t.Parallel()

	// Set up: Sort(STATUS ASC) → Scan. Insert an ordered index scan as a
	// MEMBER of the scan Reference — no winner stamping. Extraction must
	// elide the sort by scanning the child's members' derived rich
	// orderings (Planner.OrderedChildWinner).
	rowType := referenceWinnerRowType("Order", "STATUS")
	cand := referenceWinnerStatusCandidate(rowType)
	ctx := &referenceWinnerPlanContext{candidates: []MatchCandidate{cand}}

	scan := mustReferenceWinnerConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"Order"}, rowType))
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	sort := mustReferenceWinnerConstruct(expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: referenceWinnerQuantifiedField(t, q, 0)},
		},
		q,
	))
	sortRef := expressions.InitialOf(sort)

	// Explore so the planner's Memo and exploration state are populated.
	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules())
	referenceWinnerExploreRewriting(p, sortRef)

	emptyPrefix := map[values.CorrelationIdentifier]*predicates.ComparisonRange{}
	scanPlan := cand.ToScanPlan(emptyPrefix, false)
	idxPlan := extractIndexPlan(scanPlan)
	if idxPlan == nil {
		t.Fatal("could not extract index plan from candidate")
	}
	orderedScan := idxPlan.WithIndexMetadata([]string{"STATUS"}, nil, false)
	scanRef.Insert(orderedScan)

	plan, err := ExtractBestPlanFromSelector(sortRef, p, properties.DefaultStatistics{})
	if err != nil {
		t.Fatalf("ExtractBestPlanFromSelector: %v", err)
	}
	if plan == nil {
		t.Fatal("plan is nil")
	}
	// The extracted plan should be the index scan (sort eliminated),
	// not a LogicalSortExpression or InMemorySort.
	if !IsPhysicalIndexScan(plan) && !IsPhysicalFetchFromPartialRecord(plan) {
		t.Fatalf("sort should be eliminated via the child's ordered member; got %T", plan)
	}
}

// TestSortElimination_CounterflowNullsNotElidedAtExtraction pins the RFC-180
// D2 fix at the extraction seam: an `ORDER BY status ASC NULLS LAST` sort
// must NOT be elided against a child member that provides natural-order
// ASC (nulls first). The retired name+direction winner key dropped NULL
// placement, so the counterflow requirement map-hit the natural-order
// winner and elided the sort with the wrong null order.
func TestSortElimination_CounterflowNullsNotElidedAtExtraction(t *testing.T) {
	t.Parallel()

	rowType := referenceWinnerRowType("Order", "STATUS")
	cand := referenceWinnerStatusCandidate(rowType)
	ctx := &referenceWinnerPlanContext{candidates: []MatchCandidate{cand}}

	scan := mustReferenceWinnerConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"Order"}, rowType))
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	nullsLast := false
	sort := mustReferenceWinnerConstruct(expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: referenceWinnerQuantifiedField(t, q, 0), NullsFirst: &nullsLast},
		},
		q,
	))
	sortRef := expressions.InitialOf(sort)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules())
	referenceWinnerExploreRewriting(p, sortRef)

	emptyPrefix := map[values.CorrelationIdentifier]*predicates.ComparisonRange{}
	scanPlan := cand.ToScanPlan(emptyPrefix, false)
	idxPlan := extractIndexPlan(scanPlan)
	if idxPlan == nil {
		t.Fatal("could not extract index plan from candidate")
	}
	orderedScan := idxPlan.WithIndexMetadata([]string{"STATUS"}, nil, false)
	scanRef.Insert(orderedScan)

	// The natural-ASC index scan does NOT satisfy ASC NULLS LAST: the
	// elision hook must decline.
	if w := p.OrderedChildWinnerForSort(sort, scanRef); w != nil {
		t.Fatalf("ASC NULLS LAST must not elide against a natural-ASC index scan; got %T", w)
	}
}

// TestSortElimination_PinsOrderedSpineThroughWrapper reproduces the
// transparent-wrapper relink hole: Sort over a group whose satisfying
// member is a FILTER WRAPPER (order-preserving delegator), where the
// wrapper's child group holds BOTH an ordered index scan and a cheaper
// unordered scan stamped as the group's overall winner. Eliding the sort
// and then rebuilding generically resolves the child group to its overall
// winner — the UNORDERED scan — producing unordered output with the sort
// already gone. The elision path must pin the spine: the rebuilt tree
// must contain the ordered index scan.
func TestSortElimination_PinsOrderedSpineThroughWrapper(t *testing.T) {
	t.Parallel()

	rowType := referenceWinnerRowType("Order", "STATUS")
	cand := referenceWinnerStatusCandidate(rowType)
	emptyPrefix := map[values.CorrelationIdentifier]*predicates.ComparisonRange{}
	idxPlan := extractIndexPlan(cand.ToScanPlan(emptyPrefix, false))
	if idxPlan == nil {
		t.Fatal("could not extract index plan from candidate")
	}
	orderedScan := idxPlan.WithIndexMetadata([]string{"STATUS"}, nil, false)

	// The wrapper's child group: cheap unordered scan (stamped overall
	// winner) + the ordered index scan.
	scanExpr := mustReferenceWinnerConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"Order"}, rowType))
	innerRef := expressions.InitialOf(scanExpr)
	mustReferenceWinnerFireExpressionRule(t, NewPrimaryScanRule(), innerRef)
	cheap := findPhysicalExpr(innerRef)
	if cheap == nil {
		t.Fatal("no physical scan")
	}
	innerRef.InsertFinal(cheap)
	innerRef.Insert(orderedScan)
	innerRef.InsertFinal(orderedScan)
	innerRef.SetWinner(cheap) // generic extraction would relink to THIS

	// The order-preserving filter wrapper over that group.
	filterBase := mustReferenceWinnerConstruct(plans.NewRecordQueryPredicatesFilterPlan(
		mustReferenceWinnerConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, rowType, false)),
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
	))
	filterWrap := mustReferenceWinnerWithQuantifiers(t, filterBase,
		[]expressions.Quantifier{expressions.ForEachQuantifier(innerRef)})
	filterRef := expressions.InitialOf(filterWrap)

	filterQ := expressions.ForEachQuantifier(filterRef)
	sort := mustReferenceWinnerConstruct(expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: referenceWinnerQuantifiedField(t, filterQ, 0)},
		},
		filterQ,
	))
	sortRef := expressions.InitialOf(sort)
	// LogicalSort is the one non-physical selector result extraction may
	// consume: it is only a probe for immediate, physical sort elision.
	sortRef.SetWinner(sort)

	p := NewPlanner(DefaultExpressionRules(), nil)
	plan, err := ExtractBestPlanFromSelector(sortRef, p, properties.DefaultStatistics{})
	if err != nil {
		t.Fatalf("ExtractBestPlanFromSelector: %v", err)
	}
	if plan == nil {
		t.Fatal("plan is nil")
	}
	// The sort must be elided (the filter's source group HAS a satisfying
	// member) — and the pinned spine must carry the ORDERED index scan,
	// not the group's cheap overall winner.
	if _, isSort := plan.(*expressions.LogicalSortExpression); isSort {
		t.Fatalf("sort should be elided through the order-preserving filter wrapper; got %T", plan)
	}
	if !subtreeContainsIndexScan(plan) {
		t.Fatalf("elided-sort spine was relinked to the unordered winner — the ordered index scan is gone (plan root %T)", plan)
	}
}

// subtreeContainsIndexScan walks an extracted (singleton-ref) tree for a
// physical index scan wrapper.
func subtreeContainsIndexScan(e expressions.RelationalExpression) bool {
	if e == nil {
		return false
	}
	if IsPhysicalIndexScan(e) {
		return true
	}
	for _, q := range e.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		for _, m := range ref.AllMembers() {
			if subtreeContainsIndexScan(m) {
				return true
			}
		}
	}
	return false
}

// TestSortElimination_DeclinesWhenSpineUnpinnable pins the conservative
// arm: when the order-preserving wrapper's source group has NO satisfying
// member, elision must decline and the sort stays — an elided sort over
// an unpinnable spine is exactly the unordered-output hole.
func TestSortElimination_DeclinesWhenSpineUnpinnable(t *testing.T) {
	t.Parallel()

	rowType := referenceWinnerRowType("Order", "STATUS")
	scanExpr := mustReferenceWinnerConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"Order"}, rowType))
	innerRef := expressions.InitialOf(scanExpr)
	mustReferenceWinnerFireExpressionRule(t, NewPrimaryScanRule(), innerRef)
	cheap := findPhysicalExpr(innerRef)
	if cheap == nil {
		t.Fatal("no physical scan")
	}
	innerRef.InsertFinal(cheap)
	innerRef.SetWinner(cheap)

	filterBase := mustReferenceWinnerConstruct(plans.NewRecordQueryPredicatesFilterPlan(
		mustReferenceWinnerConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, rowType, false)),
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
	))
	filterWrap := mustReferenceWinnerWithQuantifiers(t, filterBase,
		[]expressions.Quantifier{expressions.ForEachQuantifier(innerRef)})
	filterRef := expressions.InitialOf(filterWrap)

	filterQ := expressions.ForEachQuantifier(filterRef)
	sort := mustReferenceWinnerConstruct(expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: referenceWinnerQuantifiedField(t, filterQ, 0)},
		},
		filterQ,
	))
	p := NewPlanner(DefaultExpressionRules(), nil)
	if w := p.OrderedChildWinnerForSort(sort, filterRef); w != nil {
		t.Fatalf("a delegating wrapper over an orderless group must not satisfy; got %T", w)
	}
}

func TestSortElimination_ViaDataAccessOrderingWinner(t *testing.T) {
	t.Parallel()

	// Sort(STATUS ASC) → Filter(STATUS > 'a') → Scan with index on STATUS.
	// The filter creates PartialMatches via matching rules, data access
	// produces an ordered index scan, and ImplementSortRule eliminates
	// the sort when it finds the ordered scan in the filter Reference.
	rowType := referenceWinnerRowType("Order", "STATUS")
	cand := referenceWinnerStatusCandidate(rowType)
	ctx := &referenceWinnerPlanContext{candidates: []MatchCandidate{cand}}

	scan := mustReferenceWinnerConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"Order"}, rowType))
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := mustReferenceWinnerConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				referenceWinnerQuantifiedField(t, q, 0),
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, "a"),
			),
		},
		q,
	))
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)
	sort := mustReferenceWinnerConstruct(expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: referenceWinnerQuantifiedField(t, filterQ, 0)},
		},
		filterQ,
	))
	sortRef := expressions.InitialOf(sort)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.PlanWithContext(context.Background(), sortRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("plan is nil")
	}

	// The plan should not be an in-memory sort — sort should be eliminated
	// via the ImplementSortRule + data access path.
	if !IsPhysicalIndexScan(plan) && !IsPhysicalFilter(plan) && !IsPhysicalFetchFromPartialRecord(plan) {
		t.Fatalf("sort should be eliminated via data access; got %T", plan)
	}
}

func TestPlan_OrderedMemberSelectable(t *testing.T) {
	t.Parallel()

	rowType := referenceWinnerRowType("Order", "STATUS")
	cand := referenceWinnerStatusCandidate(rowType)
	ctx := &referenceWinnerPlanContext{candidates: []MatchCandidate{cand}}

	scan := mustReferenceWinnerConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"Order"}, rowType))
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	sortKey := referenceWinnerQuantifiedField(t, q, 0)
	sort := mustReferenceWinnerConstruct(expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: sortKey},
		},
		q,
	))
	sortRef := expressions.InitialOf(sort)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.PlanWithContext(context.Background(), sortRef); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// OrderedIndexScanRule produces an ordered index scan at the sort
	// level; bestSatisfyingMember must find it for a STATUS ASC request.
	reqOrd := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     sortKey,
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessPreserveDistinctness, false)
	winner := bestSatisfyingMember(sortRef, reqOrd, nil)
	if winner == nil {
		t.Fatal("expected an ordering-satisfying member for STATUS ASC")
	}
	if !IsPhysicalIndexScan(winner) && !IsPhysicalFetchFromPartialRecord(winner) {
		t.Fatalf("expected *plans.RecordQueryIndexPlan or *plans.RecordQueryFetchFromPartialRecordPlan, got %T", winner)
	}
}

// TestFinalOfChildrenVisibleToSemanticEquality pins the FinalOf identity
// fix: a finals-only pinned singleton must be visible through
// Reference.Get — otherwise two otherwise-identical wrappers over
// DIFFERENT pinned children both expose nil children, SemanticEquals
// collapses them, and InsertFinal deduplicates a distinct ordered
// alternative away.
func TestFinalOfChildrenVisibleToSemanticEquality(t *testing.T) {
	t.Parallel()

	childA := referenceWinnerSortedMemberOn(t, "A")
	childB := referenceWinnerSortedMemberOn(t, "B")

	mk := func(child expressions.RelationalExpression) expressions.RelationalExpression {
		base := mustReferenceWinnerConstruct(plans.NewRecordQueryPredicatesFilterPlan(
			mustReferenceWinnerConstruct(plans.NewRecordQueryScanPlan(
				[]string{"T"}, child.GetResultValue().Type(), false)),
			[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		))
		return mustReferenceWinnerWithQuantifiers(t, base,
			[]expressions.Quantifier{expressions.ForEachQuantifier(expressions.FinalOf(child))})
	}
	w1, w2 := mk(childA), mk(childB)

	ref := expressions.InitialOf(referenceWinnerFullScan(t, "T", "A", "B"))
	ref.InsertFinal(w1)
	ref.InsertFinal(w2)
	if got := len(ref.FinalMembers()); got != 2 {
		t.Fatalf("wrappers over DIFFERENT pinned children must not deduplicate: finals = %d, want 2", got)
	}
	if expressions.FinalOf(childA).Get() != childA {
		t.Fatal("Get() must expose the pinned final of a finals-only Reference")
	}
}

// TestSortElimination_FiresThroughCollapsedDistinct pins the extraction
// twin of the rule-time executable-plan verification, from the OTHER side:
// the elision only fires when rebuildOrderedSpine's WithChildren relink
// REACHES the executable plan. Before RFC-184 W2 the distinct kept a physical
// wrapper whose WithChildren gated on isLeafReplaceable and so DECLINED to
// relink onto a non-leaf-replaceable (projection) pinned inner — keeping a
// redundant sort Java's RemoveSortRule elides. The collapsed bare distinct
// plan's WithChildren is an unconditional quantifier swap that re-resolves
// through GetInner, so the pin reaches the ordered projection and the sort is
// correctly dropped — a parity gain, matching the predicates-filter collapse.
// The pin still BAKES the ordered projection as the distinct's concrete inner,
// so dropping the sort is order-correct.
func TestSortElimination_FiresThroughCollapsedDistinct(t *testing.T) {
	t.Parallel()

	// Ordered member that is NOT leaf-replaceable: a projection wrapper
	// delegating over an in-memory sort on STATUS.
	sorted := referenceWinnerSortedMemberOn(t, "STATUS")
	sortedRef := expressions.InitialOf(sorted)
	projectionQ := expressions.ForEachQuantifier(sortedRef)
	orderedProjection := mustReferenceWinnerConstruct(plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{referenceWinnerQuantifiedField(t, projectionQ, 0)},
		nil,
		projectionQ,
	))

	// The distinct's source group: cheap unordered scan (winner) + the
	// non-leaf-replaceable ordered projection. Generic extraction would relink
	// the distinct's inner to the WINNER (unordered) and need the sort; the
	// ordered-spine pin relinks it to the projection instead.
	// The projection's output schema is the exact row shape that the shared
	// source group must flow; using the underlying record's named type would
	// make the two members ineligible for one Reference under RFC-232.
	rowType := orderedProjection.GetResultValue().Type()
	scanExpr := mustReferenceWinnerConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"Order"}, rowType))
	innerRef := expressions.InitialOf(scanExpr)
	mustReferenceWinnerFireExpressionRule(t, NewPrimaryScanRule(), innerRef)
	cheap := findPhysicalExpr(innerRef)
	if cheap == nil {
		t.Fatal("no physical scan")
	}
	innerRef.InsertFinal(cheap)
	innerRef.Insert(orderedProjection)
	innerRef.InsertFinal(orderedProjection)
	innerRef.SetWinner(cheap)

	// The distinct is its own cascades expression now (RFC-184 W2, no
	// physicalDistinctWrapper) ranging over the source group.
	distinctBase := mustReferenceWinnerConstruct(plans.NewRecordQueryDistinctPlan(
		mustReferenceWinnerConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, rowType, false))))
	distinctWrap := mustReferenceWinnerWithQuantifiers(t, distinctBase,
		[]expressions.Quantifier{expressions.ForEachQuantifier(innerRef)})
	distinctRef := expressions.InitialOf(distinctWrap)

	distinctQ := expressions.ForEachQuantifier(distinctRef)
	sort := mustReferenceWinnerConstruct(expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: referenceWinnerQuantifiedField(t, distinctQ, 0)},
		},
		distinctQ,
	))
	sortRef := expressions.InitialOf(sort)
	// LogicalSort is the one non-physical selector result extraction may
	// consume: it is only a probe for immediate, physical sort elision.
	sortRef.SetWinner(sort)

	p := NewPlanner(DefaultExpressionRules(), nil)
	plan, err := ExtractBestPlanFromSelector(sortRef, p, properties.DefaultStatistics{})
	if err != nil {
		t.Fatalf("ExtractBestPlanFromSelector: %v", err)
	}
	if plan == nil {
		t.Fatal("plan is nil")
	}
	if _, isSort := plan.(*expressions.LogicalSortExpression); isSort {
		t.Fatalf("the sort must be ELIDED: the collapsed distinct's WithChildren relink reaches the executable plan and bakes the ordered projection, so dropping the sort is order-correct; got %T with the sort still present", plan)
	}
	dp, ok := plan.(*plans.RecordQueryDistinctPlan)
	if !ok {
		t.Fatalf("expected the elided root to be *plans.RecordQueryDistinctPlan, got %T", plan)
	}
	if _, ok := dp.GetInner().(*plans.RecordQueryProjectionPlan); !ok {
		t.Fatalf("the pin must relink the distinct's inner to the ORDERED projection (proving the relink reached the executable plan, not the unordered winner); got inner %T", dp.GetInner())
	}
}
