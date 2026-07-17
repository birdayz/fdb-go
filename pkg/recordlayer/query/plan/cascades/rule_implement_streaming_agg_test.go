package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestImplementStreamingAgg_UnorderedScanFires(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "customer_id", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	FireExpressionRule(NewPrimaryScanRule(), scanRef)

	// Streaming agg is the only aggregation implementation, so it fires
	// regardless of input ordering.
	results := FireExpressionRule(NewImplementStreamingAggregationRule(), gbRef)
	if len(results) == 0 {
		t.Fatal("streaming agg should fire over unordered scan (only agg implementation)")
	}
}

func TestImplementStreamingAgg_UnorderedInput_Fires(t *testing.T) {
	t.Parallel()

	// GroupBy over a scan with no sort — streaming agg fires regardless
	// since it is the only aggregation implementation.
	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "amount", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	// Physicalize the scan only (no sort).
	FireExpressionRule(NewPrimaryScanRule(), scanRef)

	results := FireExpressionRule(NewImplementStreamingAggregationRule(), gbRef)
	if len(results) == 0 {
		t.Fatal("ImplementStreamingAggregationRule should fire with unordered input (only agg implementation)")
	}
}

func TestImplementStreamingAgg_IndexOrderedInput(t *testing.T) {
	t.Parallel()

	// Sort(customer_id) over Scan, with an index on (customer_id).
	// OrderedIndexScanRule produces an index scan ordered by customer_id.
	// GroupBy(customer_id) should then get a streaming aggregation.
	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"idx_orders_cid",
		[]string{"Orders"},
		[]string{"customer_id"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	sortExpr := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "customer_id", Typ: values.UnknownType}},
		}, scanQ)
	sortRef := expressions.InitialOf(sortExpr)
	sortQ := expressions.ForEachQuantifier(sortRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "customer_id", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		sortQ,
	)
	gbRef := expressions.InitialOf(gb)

	// OrderedIndexScanRule replaces Sort(Scan) with an index scan.
	FireExpressionRuleWithMemo(NewOrderedIndexScanRule(), sortRef, ctx, nil)

	// Now fire streaming agg — the inner (sortRef) has an index scan
	// member with ordering on customer_id.
	results := FireExpressionRule(NewImplementStreamingAggregationRule(), gbRef)
	if len(results) == 0 {
		t.Fatal("ImplementStreamingAggregationRule didn't fire with index-ordered input")
	}

	wrapper := results[0].(*physicalStreamingAggWrapper)
	explain := wrapper.GetPlan().Explain()
	if explain == "" {
		t.Fatal("empty explain string")
	}
}

func TestImplementStreamingAgg_EmptyGroupingKeys(t *testing.T) {
	t.Parallel()

	// No grouping keys — global aggregate (COUNT(*) with no GROUP BY).
	// Should fire unconditionally (no ordering requirement).
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		nil,
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "x", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	// Physicalize the scan.
	FireExpressionRule(NewPrimaryScanRule(), scanRef)

	results := FireExpressionRule(NewImplementStreamingAggregationRule(), gbRef)
	if len(results) == 0 {
		t.Fatal("ImplementStreamingAggregationRule should fire with empty grouping keys (global aggregate)")
	}
}

func TestStreamingAggPlan_Explain(t *testing.T) {
	t.Parallel()

	inner := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	plan := plans.NewRecordQueryStreamingAggregationPlan(
		inner,
		[]values.Value{
			&values.FieldValue{Field: "a", Typ: values.UnknownType},
			&values.FieldValue{Field: "b", Typ: values.UnknownType},
		},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "x", Typ: values.UnknownType}},
		},
	)

	got := plan.Explain()
	want := "StreamingAgg(keys=[a, b], Scan(T))"
	if got != want {
		t.Fatalf("Explain() = %q, want %q", got, want)
	}
}

func TestStreamingAggPlan_EqualityAndHash(t *testing.T) {
	t.Parallel()

	inner := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	p1 := plans.NewRecordQueryStreamingAggregationPlan(
		inner,
		[]values.Value{&values.FieldValue{Field: "a", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "x", Typ: values.UnknownType}},
		},
	)
	p2 := plans.NewRecordQueryStreamingAggregationPlan(
		inner,
		[]values.Value{&values.FieldValue{Field: "a", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "x", Typ: values.UnknownType}},
		},
	)
	p3 := plans.NewRecordQueryStreamingAggregationPlan(
		inner,
		[]values.Value{&values.FieldValue{Field: "b", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "y", Typ: values.UnknownType}},
		},
	)

	if !p1.EqualsWithoutChildren(p2) {
		t.Fatal("identical plans should be equal")
	}
	if p1.EqualsWithoutChildren(p3) {
		t.Fatal("different plans should not be equal")
	}
	if p1.HashCodeWithoutChildren() != p2.HashCodeWithoutChildren() {
		t.Fatal("identical plans should have same hash")
	}
}

// TestStreamingAgg_OrderedChildPinned pins RFC-181 P0.2: the ordered
// alternative's child must be a PINNED singleton, never the SHARED inner
// group — the agg wrapper's WithChildren relinks at extraction to whatever
// leaf-replaceable plan its child group resolves to, and the shared
// multi-member group can resolve to a cheaper UNORDERED member, silently
// splitting groups (grouping-key order is a correctness precondition of
// streaming aggregation). Driven through the REAL planner: the test-driver
// shortcut (FireExpressionRule) has no memo, so MemoizeExpression already
// degrades to a fresh singleton there and cannot exhibit the shared-group
// leak.
func TestStreamingAgg_OrderedChildPinned(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"T$g", []string{"T"}, []string{"G"},
		[]values.CorrelationIdentifier{a1}, values.UnknownType, false, nil)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)
	groupKey := &values.FieldValue{Field: "G", Typ: values.UnknownType}
	gb := expressions.NewGroupByExpression(
		[]values.Value{groupKey}, nil,
		expressions.ForEachQuantifier(innerRef),
	)
	topRef := expressions.InitialOf(gb)

	p := NewPlanner(DefaultExpressionRules(), ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(topRef); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// Find the ordered streaming-agg alternative: its plan child is the
	// index scan (not the in-memory sort).
	var orderedAgg *physicalStreamingAggWrapper
	for _, m := range topRef.AllMembers() {
		w, ok := m.(*physicalStreamingAggWrapper)
		if !ok {
			continue
		}
		if _, isIdx := w.plan.GetInner().(*plans.RecordQueryIndexPlan); isIdx {
			orderedAgg = w
			break
		}
	}
	if orderedAgg == nil {
		t.Skipf("no ordered streaming-agg alternative in the memo for this shape")
	}

	childRef := orderedAgg.innerQuant.GetRangesOver()
	members := childRef.AllMembers()
	if len(members) != 1 {
		t.Fatalf("the ordered agg's child must be a PINNED singleton; got %d members — the shared inner group leaks the unordered winner into the extraction relink", len(members))
	}
	if got := findPhysicalPlan(childRef); got != orderedAgg.plan.GetInner() {
		t.Fatalf("findPhysicalPlan over the pinned child = %T, want the ordered index plan — extraction would relink the agg onto an unordered child", got)
	}
}
