package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

func TestImplementStreamingAgg_UnorderedScanDoesNotFire(t *testing.T) {
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

	results := FireExpressionRule(NewImplementStreamingAggregationRule(), gbRef)
	if len(results) != 0 {
		t.Fatal("streaming agg should NOT fire over unordered scan")
	}
}

func TestImplementStreamingAgg_UnorderedInput_DoesNotFire(t *testing.T) {
	t.Parallel()

	// GroupBy over a scan with no sort — the physical scan has no
	// ordering guarantee, so streaming agg should NOT fire.
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
	if len(results) != 0 {
		t.Fatal("ImplementStreamingAggregationRule should NOT fire with unordered input")
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
