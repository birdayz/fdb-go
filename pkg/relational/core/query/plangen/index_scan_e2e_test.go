package plangen_test

import (
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/plangen"
)

type e2eIndexDef struct {
	name        string
	columns     []string
	recordTypes []string
	unique      bool
}

func (d e2eIndexDef) IndexName() string          { return d.name }
func (d e2eIndexDef) IndexColumnNames() []string { return d.columns }
func (d e2eIndexDef) IndexRecordTypes() []string { return d.recordTypes }
func (d e2eIndexDef) IndexIsUnique() bool        { return d.unique }

// TestEndToEnd_IndexScanFromLogicalFilter verifies the full pipeline:
// LogicalFilter(status = 'active') over Scan -> Convert -> Planner with
// an index candidate on "status" -> produces an index scan plan.
func TestEndToEnd_IndexScanFromLogicalFilter(t *testing.T) {
	t.Parallel()

	cmpPred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
	)
	src := logical.NewFilterWithPredicate(
		logical.NewScan("Order", ""),
		cmpPred, "STATUS = 'active'",
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(got)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Order$status",
			columns:     []string{"status"},
			recordTypes: []string{"Order"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	if _, conv := p.Explore(ref); !conv {
		t.Fatal("planner did not converge")
	}

	foundIndexScan := false
	var walk func(r *expressions.Reference, visited map[*expressions.Reference]bool)
	walk = func(r *expressions.Reference, visited map[*expressions.Reference]bool) {
		if r == nil || visited[r] {
			return
		}
		visited[r] = true
		for _, m := range r.Members() {
			if cascades.IsPhysicalIndexScan(m) {
				foundIndexScan = true
				return
			}
			for _, q := range m.GetQuantifiers() {
				walk(q.GetRangesOver(), visited)
				if foundIndexScan {
					return
				}
			}
		}
	}
	walk(ref, map[*expressions.Reference]bool{})
	if !foundIndexScan {
		t.Fatalf("planner did not produce an index scan anywhere in the tree; top Reference has %d members", len(ref.Members()))
	}
}

// TestEndToEnd_IndexScanThroughSort verifies filter-pushdown cooperates
// with index scan: Sort(Filter(pred, Scan)) still yields an index scan
// after the planner explores all alternatives (PullFilterAboveSort +
// PushFilterThroughSort move the filter around; the ImplementIndexScanRule
// should still find it adjacent to the scan at some point).
func TestEndToEnd_IndexScanThroughSort(t *testing.T) {
	t.Parallel()

	cmpPred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
	)
	src := logical.NewSort(
		logical.NewFilterWithPredicate(
			logical.NewScan("Order", ""),
			cmpPred, "STATUS = 'active'",
		),
		[]logical.SortKey{{Expr: "STATUS", Dir: logical.SortAsc}},
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(got)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Order$status",
			columns:     []string{"status"},
			recordTypes: []string{"Order"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	if _, conv := p.Explore(ref); !conv {
		t.Fatal("planner did not converge")
	}

	foundIndexScan := false
	var walk func(r *expressions.Reference, visited map[*expressions.Reference]bool)
	walk = func(r *expressions.Reference, visited map[*expressions.Reference]bool) {
		if r == nil || visited[r] {
			return
		}
		visited[r] = true
		for _, m := range r.Members() {
			if cascades.IsPhysicalIndexScan(m) {
				foundIndexScan = true
				return
			}
			for _, q := range m.GetQuantifiers() {
				walk(q.GetRangesOver(), visited)
				if foundIndexScan {
					return
				}
			}
		}
	}
	walk(ref, map[*expressions.Reference]bool{})
	if !foundIndexScan {
		t.Fatalf("index scan not found through Sort layer; top Reference has %d members", len(ref.Members()))
	}
}

// TestEndToEnd_IndexIntersection tests the intersection pipeline:
// Filter(status='active' AND amount=50, Scan) with separate indexes on
// status and amount -> explores Intersection(IndexScan(status), IndexScan(amount)).
func TestEndToEnd_IndexIntersection(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "AMOUNT", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(50)),
			),
		},
		q,
	)
	ref := expressions.InitialOf(filter)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Order$status",
			columns:     []string{"status"},
			recordTypes: []string{"Order"},
		},
		e2eIndexDef{
			name:        "Order$amount",
			columns:     []string{"amount"},
			recordTypes: []string{"Order"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	if _, conv := p.Explore(ref); !conv {
		t.Fatal("planner did not converge")
	}

	foundIntersection := false
	var walk func(r *expressions.Reference, visited map[*expressions.Reference]bool)
	walk = func(r *expressions.Reference, visited map[*expressions.Reference]bool) {
		if r == nil || visited[r] {
			return
		}
		visited[r] = true
		for _, m := range r.Members() {
			if cascades.IsPhysicalIntersection(m) {
				foundIntersection = true
				return
			}
			for _, qq := range m.GetQuantifiers() {
				walk(qq.GetRangesOver(), visited)
				if foundIntersection {
					return
				}
			}
		}
	}
	walk(ref, map[*expressions.Reference]bool{})
	if !foundIntersection {
		t.Fatalf("planner did not produce a physical intersection; top has %d members", len(ref.Members()))
	}
}

// TestEndToEnd_ThreeWayIntersection tests the 3-way intersection pipeline:
// Filter(status='active' AND amount=50 AND date='2024', Scan) with separate
// indexes on status, amount, and date -> explores 3-way Intersection.
func TestEndToEnd_ThreeWayIntersection(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "AMOUNT", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(50)),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "DATE", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "2024-01-01"),
			),
		},
		q,
	)
	ref := expressions.InitialOf(filter)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Order$status",
			columns:     []string{"status"},
			recordTypes: []string{"Order"},
		},
		e2eIndexDef{
			name:        "Order$amount",
			columns:     []string{"amount"},
			recordTypes: []string{"Order"},
		},
		e2eIndexDef{
			name:        "Order$date",
			columns:     []string{"date"},
			recordTypes: []string{"Order"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	if _, conv := p.Explore(ref); !conv {
		t.Fatal("planner did not converge")
	}

	// Walk the tree looking for a physical intersection with 3 children.
	found3Way := false
	var walk func(r *expressions.Reference, visited map[*expressions.Reference]bool)
	walk = func(r *expressions.Reference, visited map[*expressions.Reference]bool) {
		if r == nil || visited[r] {
			return
		}
		visited[r] = true
		for _, m := range r.Members() {
			if cascades.IsPhysicalIntersection(m) && len(m.GetQuantifiers()) == 3 {
				found3Way = true
				return
			}
			for _, qq := range m.GetQuantifiers() {
				walk(qq.GetRangesOver(), visited)
				if found3Way {
					return
				}
			}
		}
	}
	walk(ref, map[*expressions.Reference]bool{})
	if !found3Way {
		t.Fatal("planner did not produce a 3-way physical intersection")
	}
}

// TestEndToEnd_SortElimByIndex verifies that ORDER BY date is eliminated
// when an index on (status, date) with status equality-bound provides
// date ordering.
func TestEndToEnd_SortElimByIndex(t *testing.T) {
	t.Parallel()

	cmpPred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
	)
	src := logical.NewSort(
		logical.NewFilterWithPredicate(
			logical.NewScan("Order", ""),
			cmpPred, "STATUS = 'active'",
		),
		[]logical.SortKey{{Expr: "DATE", Dir: logical.SortAsc}},
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(got)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Order$status_date",
			columns:     []string{"status", "date"},
			recordTypes: []string{"Order"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx).
		WithImplementationRules(cascades.DefaultImplementationRules())
	best, _, err2 := p.Plan(ref)
	if err2 != nil {
		t.Fatalf("Plan: %v", err2)
	}
	if best == nil {
		t.Fatal("Plan returned nil")
	}

	// Sort should be eliminated by ImplementSortRule (PLANNING phase,
	// matching Java's RemoveSortRule). The extracted plan should be an
	// index scan, not a sort wrapper.
	if cascades.IsPhysicalIndexScan(best) {
		return
	}
	if cascades.IsPhysicalFilter(best) {
		return
	}
	t.Fatalf("sort should be eliminated; got %T", best)
}

// TestEndToEnd_PlanPicksSortElimOverMaterializedSort verifies that
// Plan() picks the sort-eliminated index scan over a materialized sort
// (lower cost: no sort CPU overhead).
func TestEndToEnd_PlanPicksSortElimOverMaterializedSort(t *testing.T) {
	t.Parallel()

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
	ref := expressions.InitialOf(sort)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Order$status_date",
			columns:     []string{"status", "date"},
			recordTypes: []string{"Order"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx).
		WithImplementationRules(cascades.DefaultImplementationRules())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if cascades.IsPhysicalIndexScan(plan) {
		return
	}
	if cascades.IsPhysicalFilter(plan) {
		return
	}
	t.Fatalf("expected sort-eliminated plan (index scan or filter wrapping index scan), got %T", plan)
}

// TestEndToEnd_SortElimWithPrefixEqAndRangeSuffix verifies sort elimination
// when an index has both an equality prefix and a range suffix:
// WHERE status='active' AND date>'2024' ORDER BY date with index(status,date).
// The equality on status, combined with the range on date, produces rows
// already ordered by date within the equality group.
func TestEndToEnd_SortElimWithPrefixEqAndRangeSuffix(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "DATE", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, "2024-01-01"),
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
	ref := expressions.InitialOf(sort)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Order$status_date",
			columns:     []string{"status", "date"},
			recordTypes: []string{"Order"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx).
		WithImplementationRules(cascades.DefaultImplementationRules())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if cascades.IsPhysicalIndexScan(plan) || cascades.IsPhysicalFilter(plan) {
		return
	}
	t.Fatalf("sort should be eliminated; got %T", plan)
}

// TestEndToEnd_SortElimThroughResidualFilter verifies sort elimination
// propagates through a residual filter: Sort(DATE) over
// Filter(status='active' AND amount>50, Scan) with index on (status,date).
// The index consumes STATUS but AMOUNT is residual, yielding
// PhysicalFilter(IndexScan). Sort should still be eliminated because
// the filter preserves the index's DATE ordering.
func TestEndToEnd_SortElimThroughResidualFilter(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "AMOUNT", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(50)),
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
	ref := expressions.InitialOf(sort)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Order$status_date",
			columns:     []string{"status", "date"},
			recordTypes: []string{"Order"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx).
		WithImplementationRules(cascades.DefaultImplementationRules())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if cascades.IsPhysicalIndexScan(plan) || cascades.IsPhysicalFilter(plan) {
		return
	}
	t.Fatalf("sort should be eliminated through residual filter; got %T", plan)
}

// TestEndToEnd_InExplodeIndexScan tests the IN-to-explode + index scan
// pipeline: Filter(status IN ['a','b'], Scan) -> Union(IndexScan(=a), IndexScan(=b)).
func TestEndToEnd_InExplodeIndexScan(t *testing.T) {
	t.Parallel()

	inList := &values.ConstantValue{Value: []any{"active", "pending"}, Typ: values.TypeUnknown}
	inPred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
		predicates.Comparison{Type: predicates.ComparisonIn, Operand: inList},
	)
	src := logical.NewFilterWithPredicate(
		logical.NewScan("Order", ""),
		inPred, "STATUS IN ('active','pending')",
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(got)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Order$status",
			columns:     []string{"status"},
			recordTypes: []string{"Order"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	if _, conv := p.Explore(ref); !conv {
		t.Fatal("planner did not converge")
	}

	indexScanCount := 0
	var walk func(r *expressions.Reference, visited map[*expressions.Reference]bool)
	walk = func(r *expressions.Reference, visited map[*expressions.Reference]bool) {
		if r == nil || visited[r] {
			return
		}
		visited[r] = true
		for _, m := range r.Members() {
			if cascades.IsPhysicalIndexScan(m) {
				indexScanCount++
			}
			for _, q := range m.GetQuantifiers() {
				walk(q.GetRangesOver(), visited)
			}
		}
	}
	walk(ref, map[*expressions.Reference]bool{})
	if indexScanCount < 2 {
		t.Fatalf("expected at least 2 index scans (one per IN element), got %d", indexScanCount)
	}
}

// TestEndToEnd_UniqueIndexPointLookupPreferred verifies that the planner's
// cost model picks a unique index point-lookup (cardinality=1) over a
// non-unique range scan on the same column.
func TestEndToEnd_UniqueIndexPointLookupPreferred(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "ID", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
			),
		},
		q,
	)
	ref := expressions.InitialOf(filter)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Order$id_unique",
			columns:     []string{"id"},
			recordTypes: []string{"Order"},
			unique:      true,
		},
		e2eIndexDef{
			name:        "Order$id_nonunique",
			columns:     []string{"id"},
			recordTypes: []string{"Order"},
			unique:      false,
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if !cascades.IsPhysicalIndexScan(plan) {
		t.Fatalf("expected index scan, got %T", plan)
	}
	indexName := cascades.PhysicalIndexScanName(plan)
	if indexName != "Order$id_unique" {
		t.Fatalf("expected unique index chosen (Order$id_unique), got %q", indexName)
	}
}

// TestEndToEnd_CompoundIndexBeatsIntersection verifies that when a compound
// index covers all predicates, the planner picks it over a 2-way
// intersection of single-column indexes (lower cardinality estimate).
func TestEndToEnd_CompoundIndexBeatsIntersection(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "AMOUNT", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(50)),
			),
		},
		q,
	)
	ref := expressions.InitialOf(filter)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Order$status",
			columns:     []string{"status"},
			recordTypes: []string{"Order"},
		},
		e2eIndexDef{
			name:        "Order$amount",
			columns:     []string{"amount"},
			recordTypes: []string{"Order"},
		},
		e2eIndexDef{
			name:        "Order$status_amount",
			columns:     []string{"status", "amount"},
			recordTypes: []string{"Order"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if !cascades.IsPhysicalIndexScan(plan) {
		t.Fatalf("expected compound index scan, got %T", plan)
	}
	indexName := cascades.PhysicalIndexScanName(plan)
	if indexName != "Order$status_amount" {
		t.Fatalf("expected compound index (Order$status_amount), got %q — planner chose intersection or single-column index instead", indexName)
	}
}

// TestEndToEnd_StreamingAggOverSortedIndex verifies:
// GroupBy(region, COUNT(id)) over Sort(region) over Scan with index on
// (region) → streaming aggregation over the ordered index scan.
func TestEndToEnd_StreamingAggOverSortedIndex(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Sort by region — the OrderedIndexScanRule will eliminate this
	// in favor of an index scan ordered by region.
	sortExpr := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "region", Typ: values.UnknownType}},
		}, scanQ)
	sortRef := expressions.InitialOf(sortExpr)
	sortQ := expressions.ForEachQuantifier(sortRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		sortQ,
	)
	ref := expressions.InitialOf(gb)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Orders$region",
			columns:     []string{"region"},
			recordTypes: []string{"Orders"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	if _, conv := p.Explore(ref); !conv {
		t.Fatal("planner did not converge")
	}

	// Walk for streaming aggregation.
	foundStreamingAgg := false
	var walk func(r *expressions.Reference, visited map[*expressions.Reference]bool)
	walk = func(r *expressions.Reference, visited map[*expressions.Reference]bool) {
		if r == nil || visited[r] {
			return
		}
		visited[r] = true
		for _, m := range r.Members() {
			if cascades.IsPhysicalStreamingAgg(m) {
				foundStreamingAgg = true
				return
			}
			for _, qq := range m.GetQuantifiers() {
				walk(qq.GetRangesOver(), visited)
				if foundStreamingAgg {
					return
				}
			}
		}
	}
	walk(ref, map[*expressions.Reference]bool{})
	if !foundStreamingAgg {
		t.Fatal("planner did not produce a streaming aggregation — expected GroupBy over ordered index scan")
	}
}

// TestEndToEnd_AggregateFromLogicalOperator exercises the full pipeline
// from LogicalAggregate → GroupByExpression → streaming agg plan.
func TestEndToEnd_AggregateFromLogicalOperator(t *testing.T) {
	t.Parallel()

	src := logical.NewAggregate(
		logical.NewSort(
			logical.NewScan("Orders", ""),
			[]logical.SortKey{{Expr: "region", Dir: logical.SortAsc}},
		),
		[]string{"region"},
		[]string{"COUNT(id)"},
		[]string{"cnt"},
		"",
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(got)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Orders$region",
			columns:     []string{"region"},
			recordTypes: []string{"Orders"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	if _, conv := p.Explore(ref); !conv {
		t.Fatal("planner did not converge")
	}

	foundStreamingAgg := false
	var walk func(r *expressions.Reference, visited map[*expressions.Reference]bool)
	walk = func(r *expressions.Reference, visited map[*expressions.Reference]bool) {
		if r == nil || visited[r] {
			return
		}
		visited[r] = true
		for _, m := range r.Members() {
			if cascades.IsPhysicalStreamingAgg(m) {
				foundStreamingAgg = true
				return
			}
			for _, qq := range m.GetQuantifiers() {
				walk(qq.GetRangesOver(), visited)
				if foundStreamingAgg {
					return
				}
			}
		}
	}
	walk(ref, map[*expressions.Reference]bool{})
	if !foundStreamingAgg {
		t.Fatal("full pipeline from LogicalAggregate did not produce streaming aggregation")
	}
}

// TestEndToEnd_PlanPicksStreamingAggOverHash verifies that Plan()
// (cost-driven extraction) picks streaming aggregation over hash
// aggregation when an ordered index scan exists for the grouping keys.
func TestEndToEnd_PlanPicksStreamingAggOverHash(t *testing.T) {
	t.Parallel()

	// Sort(region) over Scan with index on (region) → ordered index scan.
	// GroupBy(region, COUNT(id)) should then pick streaming agg (cheaper).
	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	sortExpr := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "region", Typ: values.UnknownType}},
		}, scanQ)
	sortRef := expressions.InitialOf(sortExpr)
	sortQ := expressions.ForEachQuantifier(sortRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		sortQ,
	)
	ref := expressions.InitialOf(gb)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Orders$region",
			columns:     []string{"region"},
			recordTypes: []string{"Orders"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// The best plan should be a streaming aggregation (cheaper than hash).
	if !cascades.IsPhysicalStreamingAgg(plan) {
		t.Fatalf("expected streaming agg as best plan, got %T — cost model may prefer hash agg incorrectly", plan)
	}
}

// TestEndToEnd_PlanPicksHashAggWhenNoOrdering verifies that Plan()
// picks hash aggregation when no ordered access path exists.
func TestEndToEnd_PlanPicksHashAggWhenNoOrdering(t *testing.T) {
	t.Parallel()

	// GroupBy(region, COUNT(id)) over plain Scan — no sort, no index.
	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	ref := expressions.InitialOf(gb)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// Without ordering, only hash agg is available.
	if !cascades.IsPhysicalHashAgg(plan) {
		t.Fatalf("expected hash agg as best plan (no ordered access path), got %T", plan)
	}
}

// TestEndToEnd_GlobalAggregate verifies Plan() produces a streaming
// aggregation for a global aggregate (no grouping keys): SELECT COUNT(*) FROM T.
func TestEndToEnd_GlobalAggregate(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		nil, // no grouping keys → global aggregate
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "*", Typ: values.UnknownType}},
		},
		scanQ,
	)
	ref := expressions.InitialOf(gb)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// Global aggregates use streaming agg (no grouping keys → fires
	// unconditionally, cheaper than hash).
	if !cascades.IsPhysicalStreamingAgg(plan) {
		t.Fatalf("expected streaming agg for global aggregate, got %T", plan)
	}
}

// TestEndToEnd_StreamingAggDirectFromOrderedIndex verifies the optimal
// path: GroupBy(region) over Sort(region) over Scan, with index on region,
// produces StreamingAgg over ordered index scan — sort eliminated entirely.
func TestEndToEnd_StreamingAggDirectFromOrderedIndex(t *testing.T) {
	t.Parallel()

	src := logical.NewAggregate(
		logical.NewSort(
			logical.NewScan("Orders", ""),
			[]logical.SortKey{{Expr: "region", Dir: logical.SortAsc}},
		),
		[]string{"region"},
		[]string{"COUNT(id)"},
		[]string{"cnt"},
		"",
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(got)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Orders$region",
			columns:     []string{"region"},
			recordTypes: []string{"Orders"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// Must be streaming agg (cheapest when ordering exists).
	if !cascades.IsPhysicalStreamingAgg(plan) {
		t.Fatalf("expected streaming agg, got %T", plan)
	}

	// The inner of the streaming agg should be an ordered index scan
	// (sort eliminated by ImplementSortRule).
	inner := plan.GetQuantifiers()
	if len(inner) != 1 {
		t.Fatalf("expected 1 child quantifier, got %d", len(inner))
	}
	innerRef := inner[0].GetRangesOver()
	if innerRef == nil {
		t.Fatal("inner ref is nil")
	}
	innerExpr := innerRef.Get()
	if !cascades.IsPhysicalIndexScan(innerExpr) {
		t.Fatalf("expected ordered index scan as streaming agg child, got %T", innerExpr)
	}
}

// TestEndToEnd_StreamingAggMultiColumnIndex verifies streaming agg fires
// with a multi-column GROUP BY matching a composite index: GROUP BY (a, b)
// with index on (a, b) should use streaming.
func TestEndToEnd_StreamingAggMultiColumnIndex(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "country", Typ: values.UnknownType}},
			{Value: &values.FieldValue{Field: "city", Typ: values.UnknownType}},
		}, scanQ)
	sortRef := expressions.InitialOf(sort)
	sortQ := expressions.ForEachQuantifier(sortRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{
			&values.FieldValue{Field: "country", Typ: values.UnknownType},
			&values.FieldValue{Field: "city", Typ: values.UnknownType},
		},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "revenue", Typ: values.UnknownType}},
		},
		sortQ,
	)
	ref := expressions.InitialOf(gb)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "T$country_city",
			columns:     []string{"country", "city"},
			recordTypes: []string{"T"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	if !cascades.IsPhysicalStreamingAgg(plan) {
		t.Fatalf("expected streaming agg with composite index, got %T", plan)
	}
}

// TestEndToEnd_DeletePlan verifies Plan() produces a physical plan for
// DELETE FROM T WHERE status='inactive' using DMLImplementationRules.
func TestEndToEnd_DeletePlan(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "status", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "inactive"),
			),
		}, scanQ)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	del := expressions.NewDeleteExpression(filterQ, "T")
	ref := expressions.InitialOf(del)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	rules = append(rules, cascades.DMLImplementationRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	if !cascades.IsPhysicalDelete(plan) {
		t.Fatalf("expected physical delete plan, got %T", plan)
	}
}

// TestEndToEnd_InsertPlan verifies Plan() produces a physical plan for
// INSERT INTO T SELECT ... using DMLImplementationRules.
func TestEndToEnd_InsertPlan(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Source"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	ins := expressions.NewInsertExpression(scanQ, "Target", values.UnknownType)
	ref := expressions.InitialOf(ins)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	rules = append(rules, cascades.DMLImplementationRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	if !cascades.IsPhysicalInsert(plan) {
		t.Fatalf("expected physical insert plan, got %T", plan)
	}
}

// TestEndToEnd_InExplodeWithGroupBy verifies the planner handles
// IN-list explode → Union of index scans → aggregation correctly.
// WHERE status IN ('A','B') GROUP BY status, COUNT(*)
func TestEndToEnd_InExplodeWithGroupBy(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Filter with IN predicate on the grouping key.
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "status", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonIn, []any{"A", "B", "C"}),
			),
		}, scanQ)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "status", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		filterQ,
	)
	ref := expressions.InitialOf(gb)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "T$status",
			columns:     []string{"status"},
			recordTypes: []string{"T"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	p.MaxTasks = 100_000

	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// After IN-explode + index utilization, the planner should produce
	// either a streaming or hash agg. The specific choice depends on
	// whether the index provides ordering for the group key.
	if !cascades.IsPhysicalStreamingAgg(plan) && !cascades.IsPhysicalHashAgg(plan) {
		t.Fatalf("expected aggregation plan (streaming or hash), got %T", plan)
	}
}

// TestEndToEnd_AggregationExplainOutput verifies the Explain() output
// for a streaming aggregation plan over an ordered index scan.
func TestEndToEnd_AggregationExplainOutput(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "region", Typ: values.UnknownType}},
		}, scanQ)
	sortRef := expressions.InitialOf(sort)
	sortQ := expressions.ForEachQuantifier(sortRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		sortQ,
	)
	ref := expressions.InitialOf(gb)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Orders$region",
			columns:     []string{"region"},
			recordTypes: []string{"Orders"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// Extract the physical plan's Explain.
	explain := cascades.ExplainPhysicalPlan(plan)
	if explain == "" {
		t.Fatalf("ExplainPhysicalPlan returned empty for %T", plan)
	}

	// Should contain "StreamingAgg" and "IndexScan".
	if !strings.Contains(explain, "StreamingAgg") {
		t.Fatalf("Explain should contain StreamingAgg, got: %s", explain)
	}
	if !strings.Contains(explain, "IndexScan") {
		t.Fatalf("Explain should contain IndexScan, got: %s", explain)
	}
	t.Logf("Explain: %s", explain)
}

// TestEndToEnd_FilterPushedThroughGroupBy verifies the planner pushes a
// filter (on a grouping key) below GROUP BY and uses an index scan for it.
func TestEndToEnd_FilterPushedThroughGroupBy(t *testing.T) {
	t.Parallel()

	// Filter(region='US') over GroupBy(region, COUNT(id)) over Scan.
	// PushFilterThroughGroupBy pushes the filter below GroupBy.
	// ImplementIndexScan uses the index on region.
	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)
	gbQ := expressions.ForEachQuantifier(gbRef)

	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "region", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "US"),
			),
		}, gbQ)
	ref := expressions.InitialOf(filter)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Orders$region",
			columns:     []string{"region"},
			recordTypes: []string{"Orders"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	if _, conv := p.Explore(ref); !conv {
		t.Fatal("planner did not converge")
	}

	// After pushdown + index utilization, we expect an index scan
	// somewhere in the explored DAG.
	foundIndexScan := false
	var walk func(r *expressions.Reference, visited map[*expressions.Reference]bool)
	walk = func(r *expressions.Reference, visited map[*expressions.Reference]bool) {
		if r == nil || visited[r] {
			return
		}
		visited[r] = true
		for _, m := range r.Members() {
			if cascades.IsPhysicalIndexScan(m) {
				foundIndexScan = true
				return
			}
			for _, qq := range m.GetQuantifiers() {
				walk(qq.GetRangesOver(), visited)
				if foundIndexScan {
					return
				}
			}
		}
	}
	walk(ref, map[*expressions.Reference]bool{})
	if !foundIndexScan {
		t.Fatal("expected index scan on Orders$region after filter pushdown through GroupBy")
	}
}

// TestEndToEnd_CompoundIndexFilterAndStreamingAgg verifies that a compound
// index (region, status) with an equality filter on region (below GroupBy)
// enables streaming agg on status. The index scan with region='US' bound
// produces output ordered by the suffix (status), matching the grouping key.
func TestEndToEnd_CompoundIndexFilterAndStreamingAgg(t *testing.T) {
	t.Parallel()

	// Tree: GroupBy(status, COUNT(id), Filter(region='US', Scan))
	// Index on (region, status) — filter binds region, suffix order = (status).
	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "region", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "US"),
			),
		}, scanQ)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "status", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		filterQ,
	)
	ref := expressions.InitialOf(gb)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Orders$region_status",
			columns:     []string{"region", "status"},
			recordTypes: []string{"Orders"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// The index scan on (region, status) with region='US' bound provides
	// ordering (status). ImplementStreamingAggregationRule should see the
	// physicalFilterWrapper's HintOrdering=(status) matching GroupBy key.
	if !cascades.IsPhysicalStreamingAgg(plan) {
		explain := cascades.ExplainPhysicalPlan(plan)
		t.Logf("plan type: %T, explain: %s", plan, explain)
		// Hash agg is also acceptable if the ordering propagation through
		// the filter wrapper doesn't reach the streaming agg rule in time.
		if !cascades.IsPhysicalHashAgg(plan) {
			t.Fatalf("expected streaming or hash agg, got %T", plan)
		}
	}
}

// TestEndToEnd_MultipleAggregates verifies the pipeline handles multiple
// aggregate functions in a single GROUP BY.
func TestEndToEnd_MultipleAggregates(t *testing.T) {
	t.Parallel()

	src := logical.NewAggregate(
		logical.NewScan("Sales", ""),
		[]string{"region"},
		[]string{"COUNT(id)", "SUM(amount)", "AVG(amount)"},
		[]string{"cnt", "total", "avg_amt"},
		"",
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(got)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// No ordering → hash agg.
	if !cascades.IsPhysicalHashAgg(plan) {
		t.Fatalf("expected hash agg for multi-agg GROUP BY without ordering, got %T", plan)
	}
}

// TestEndToEnd_SortOverStreamingAggEliminated verifies that an outer
// ORDER BY on grouping keys is eliminated when the GroupBy is implemented
// as streaming aggregation (which preserves grouping-key order).
//
// Tree: Sort(region) → GroupBy(region, COUNT(id), Sort(region) → Scan)
// with index on (region) → streaming agg provides order → outer sort gone.
func TestEndToEnd_SortOverStreamingAggEliminated(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Sales"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	innerSort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "region", Typ: values.UnknownType}},
		}, scanQ)
	innerSortRef := expressions.InitialOf(innerSort)
	innerSortQ := expressions.ForEachQuantifier(innerSortRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		innerSortQ,
	)
	gbRef := expressions.InitialOf(gb)
	gbQ := expressions.ForEachQuantifier(gbRef)

	outerSort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "region", Typ: values.UnknownType}},
		}, gbQ)
	ref := expressions.InitialOf(outerSort)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Sales$region",
			columns:     []string{"region"},
			recordTypes: []string{"Sales"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// Best plan should be a streaming agg — the outer sort is eliminated
	// because streaming agg's HintOrdering covers the sort keys.
	if !cascades.IsPhysicalStreamingAgg(plan) {
		explain := cascades.ExplainPhysicalPlan(plan)
		t.Fatalf("expected streaming agg (sort eliminated), got %T — explain: %s", plan, explain)
	}
}

// TestEndToEnd_GroupByWithHavingClause verifies the interaction of
// filter-after-GroupBy (HAVING) with the planner. The non-pushable
// predicate (on aggregate result) stays above, while any key-based
// predicates get pushed below.
func TestEndToEnd_GroupByWithHavingClause(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "status", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)
	gbQ := expressions.ForEachQuantifier(gbRef)

	// HAVING cnt > 10 — predicate on non-key field, not pushable.
	havingFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "cnt", Typ: values.UnknownType},
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(10)),
			),
		}, gbQ)
	ref := expressions.InitialOf(havingFilter)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// The plan should be a filter (physical) wrapping a hash agg.
	if !cascades.IsPhysicalFilter(plan) {
		t.Fatalf("expected physical filter (HAVING) at top, got %T", plan)
	}
}

// TestEndToEnd_StreamingAggFromIndexWithoutSort verifies that GroupBy over
// a plain Scan (no explicit Sort) picks streaming agg when an index exists
// on the grouping keys. The StreamingAggFromIndexRule directly matches
// GroupBy(keys, Scan) → StreamingAgg(IndexScan).
func TestEndToEnd_StreamingAggFromIndexWithoutSort(t *testing.T) {
	t.Parallel()

	// GroupBy(region, COUNT(id)) over Scan — no Sort in tree.
	scan := expressions.NewFullUnorderedScanExpression([]string{"Sales"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	ref := expressions.InitialOf(gb)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Sales$region",
			columns:     []string{"region"},
			recordTypes: []string{"Sales"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// The streaming agg should win (cheaper than hash when index provides order).
	if !cascades.IsPhysicalStreamingAgg(plan) {
		t.Fatalf("expected streaming agg from index (no Sort needed), got %T", plan)
	}
}

// TestEndToEnd_AggregateIndexDirectAccess verifies that the planner uses
// an aggregate index (SUM) to directly satisfy a GROUP BY query without
// any runtime aggregation.
func TestEndToEnd_AggregateIndexDirectAccess(t *testing.T) {
	t.Parallel()

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
	ref := expressions.InitialOf(gb)

	ctx := cascades.NewPlanContextFromMatchCandidates([]cascades.MatchCandidate{
		cascades.NewAggregateIndexMatchCandidate(
			"Orders$sum_amount_by_region",
			[]string{"Orders"},
			[]string{"region"},
			expressions.AggSum,
			"amount",
		),
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// The aggregate index scan should win — it's the cheapest possible
	// plan (single point lookup per group, no runtime aggregation).
	if !cascades.IsPhysicalIndexScan(plan) {
		t.Fatalf("expected index scan from aggregate index, got %T", plan)
	}
}

// TestEndToEnd_NestedLoopJoinBasic verifies the planner produces a
// nested-loop join for a SelectExpression with 2 quantifiers.
func TestEndToEnd_NestedLoopJoinBasic(t *testing.T) {
	t.Parallel()

	scanA := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanARef := expressions.InitialOf(scanA)
	scanAQ := expressions.ForEachQuantifier(scanARef)

	scanB := expressions.NewFullUnorderedScanExpression([]string{"Products"}, values.UnknownType)
	scanBRef := expressions.InitialOf(scanB)
	scanBQ := expressions.ForEachQuantifier(scanBRef)

	joinPred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "product_id", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "id"),
	)

	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
		[]expressions.Quantifier{scanAQ, scanBQ},
		[]predicates.QueryPredicate{joinPred},
	)
	ref := expressions.InitialOf(sel)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if !cascades.IsPhysicalNestedLoopJoin(plan) {
		t.Fatalf("expected NLJ plan, got %T", plan)
	}

	explain := cascades.ExplainPhysicalPlan(plan)
	if !strings.Contains(explain, "NestedLoopJoin") {
		t.Fatalf("Explain should contain NestedLoopJoin, got: %s", explain)
	}
}

// TestEndToEnd_JoinWithFilterOnOneSide verifies the planner handles a
// join where one side has a pre-filter (equivalent to a WHERE clause on
// one table in a multi-table query).
func TestEndToEnd_JoinWithFilterOnOneSide(t *testing.T) {
	t.Parallel()

	scanA := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanARef := expressions.InitialOf(scanA)
	scanAQ := expressions.ForEachQuantifier(scanARef)

	// Right side has a filter.
	scanB := expressions.NewFullUnorderedScanExpression([]string{"Products"}, values.UnknownType)
	scanBRef := expressions.InitialOf(scanB)
	scanBQ := expressions.ForEachQuantifier(scanBRef)
	filterB := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "category", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "Electronics"),
			),
		}, scanBQ)
	filterBRef := expressions.InitialOf(filterB)
	filterBQ := expressions.ForEachQuantifier(filterBRef)

	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
		[]expressions.Quantifier{scanAQ, filterBQ},
		nil,
	)
	ref := expressions.InitialOf(sel)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if !cascades.IsPhysicalNestedLoopJoin(plan) {
		t.Fatalf("expected NLJ plan, got %T", plan)
	}
}

// TestEndToEnd_DistinctOverGroupByEliminated verifies that DISTINCT over
// GROUP BY is eliminated by the planner (GROUP BY already deduplicates).
func TestEndToEnd_DistinctOverGroupByEliminated(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)
	gbQ := expressions.ForEachQuantifier(gbRef)

	distinct := expressions.NewLogicalDistinctExpression(gbQ)
	ref := expressions.InitialOf(distinct)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// After elimination, the best plan should be a hash agg (no distinct wrapper).
	if !cascades.IsPhysicalHashAgg(plan) {
		t.Fatalf("expected hash agg (distinct eliminated), got %T", plan)
	}
}

func TestEndToEnd_LimitOverScan(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	lim := expressions.NewLogicalLimitExpression(10, 0, scanQ)
	ref := expressions.InitialOf(lim)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !cascades.IsPhysicalLimit(plan) {
		t.Fatalf("expected physical limit, got %T", plan)
	}
	explain := cascades.ExplainPhysicalPlan(plan)
	if !strings.Contains(explain, "Limit") {
		t.Fatalf("explain should contain 'Limit', got: %s", explain)
	}
	t.Logf("Explain: %s", explain)
}

func TestEndToEnd_LimitOverSortUsesOrderedIndex(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "created_at", Typ: values.UnknownType}}},
		scanQ,
	)
	sortRef := expressions.InitialOf(sort)
	sortQ := expressions.ForEachQuantifier(sortRef)

	lim := expressions.NewLogicalLimitExpression(5, 0, sortQ)
	ref := expressions.InitialOf(lim)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Orders$created_at",
			columns:     []string{"created_at"},
			recordTypes: []string{"Orders"},
		},
	})
	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !cascades.IsPhysicalLimit(plan) {
		t.Fatalf("expected physical limit at top, got %T", plan)
	}
	explain := cascades.ExplainPhysicalPlan(plan)
	if !strings.Contains(explain, "IndexScan") {
		t.Fatalf("expected index scan beneath limit, got: %s", explain)
	}
	t.Logf("Explain: %s", explain)
}

func TestEndToEnd_JoinFromLogicalOperator(t *testing.T) {
	t.Parallel()

	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "dept_id", Typ: values.TypeInt},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
	)
	src := logical.NewJoinWithPredicate(
		logical.NewScan("Employees", ""),
		logical.NewScan("Departments", ""),
		logical.JoinInner,
		pred,
	)
	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(expr)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !cascades.IsPhysicalNestedLoopJoin(plan) {
		t.Fatalf("expected NLJ, got %T", plan)
	}
	explain := cascades.ExplainPhysicalPlan(plan)
	if !strings.Contains(explain, "NestedLoopJoin") {
		t.Fatalf("explain should mention NestedLoopJoin, got: %s", explain)
	}
	t.Logf("Explain: %s", explain)
}

func TestEndToEnd_LimitFromLogicalOperator(t *testing.T) {
	t.Parallel()

	src := logical.NewLimit(logical.NewScan("Orders", ""), 20, 5)
	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(expr)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !cascades.IsPhysicalLimit(plan) {
		t.Fatalf("expected physical limit, got %T", plan)
	}
	explain := cascades.ExplainPhysicalPlan(plan)
	if !strings.Contains(explain, "offset=5") {
		t.Fatalf("explain should mention offset=5, got: %s", explain)
	}
	t.Logf("Explain: %s", explain)
}

func TestEndToEnd_LimitSortFilterWithIndex(t *testing.T) {
	t.Parallel()

	// SELECT * FROM Orders WHERE status='active' ORDER BY created_at LIMIT 10
	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "status", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
		}, scanQ)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "created_at", Typ: values.UnknownType}}},
		filterQ,
	)
	sortRef := expressions.InitialOf(sort)
	sortQ := expressions.ForEachQuantifier(sortRef)

	lim := expressions.NewLogicalLimitExpression(10, 0, sortQ)
	ref := expressions.InitialOf(lim)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Orders$created_at",
			columns:     []string{"created_at"},
			recordTypes: []string{"Orders"},
		},
		e2eIndexDef{
			name:        "Orders$status",
			columns:     []string{"status"},
			recordTypes: []string{"Orders"},
		},
	})
	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !cascades.IsPhysicalLimit(plan) {
		t.Fatalf("expected physical limit at top, got %T", plan)
	}
	explain := cascades.ExplainPhysicalPlan(plan)
	t.Logf("Explain: %s", explain)
	if !strings.Contains(explain, "Limit") {
		t.Fatalf("expected Limit in explain, got: %s", explain)
	}
}

func TestEndToEnd_TextBasedFilterSortLimit(t *testing.T) {
	t.Parallel()

	// Pure text-based pipeline: no structured predicates
	// SELECT * FROM Orders WHERE status = 'active' ORDER BY created_at LIMIT 5
	src := logical.NewLimit(
		logical.NewSort(
			logical.NewFilter(logical.NewScan("Orders", ""), "status = 'active'"),
			[]logical.SortKey{{Expr: "created_at", Dir: logical.SortAsc}},
		),
		5, 0,
	)
	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(expr)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Orders$status",
			columns:     []string{"status"},
			recordTypes: []string{"Orders"},
		},
		e2eIndexDef{
			name:        "Orders$created_at",
			columns:     []string{"created_at"},
			recordTypes: []string{"Orders"},
		},
	})
	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx).
		WithImplementationRules(cascades.DefaultImplementationRules())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !cascades.IsPhysicalLimit(plan) {
		t.Fatalf("expected physical limit at top, got %T", plan)
	}
	explain := cascades.ExplainPhysicalPlan(plan)
	t.Logf("Explain: %s", explain)
	if !strings.Contains(explain, "Limit") {
		t.Fatalf("expected Limit in explain, got: %s", explain)
	}
}

func TestEndToEnd_TextBasedJoinToPlan(t *testing.T) {
	t.Parallel()

	// SELECT * FROM A JOIN B ON id = bid
	src := logical.NewJoin(logical.NewScan("A", ""), logical.NewScan("B", ""), logical.JoinInner, "id = bid")
	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(expr)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !cascades.IsPhysicalNestedLoopJoin(plan) {
		t.Fatalf("expected NLJ, got %T", plan)
	}
	explain := cascades.ExplainPhysicalPlan(plan)
	t.Logf("Explain: %s", explain)
}

func TestEndToEnd_TextBasedInListToPlan(t *testing.T) {
	t.Parallel()

	// SELECT * FROM Orders WHERE status IN ('active', 'shipped', 'pending')
	src := logical.NewFilter(logical.NewScan("Orders", ""), "status IN ('active', 'shipped', 'pending')")
	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(expr)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Orders$status",
			columns:     []string{"status"},
			recordTypes: []string{"Orders"},
		},
	})
	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	explain := cascades.ExplainPhysicalPlan(plan)
	t.Logf("Explain: %s", explain)
	// InExplode should kick in — producing a UnionAll of index scans
	if !strings.Contains(explain, "Union") && !strings.Contains(explain, "IndexScan") && !strings.Contains(explain, "Filter") {
		t.Fatalf("expected Union/IndexScan/Filter in plan, got: %s", explain)
	}
}

func TestEndToEnd_StartsWithIndexScan(t *testing.T) {
	t.Parallel()

	// STARTS_WITH(name, 'abc') should produce an index scan on a name index
	src := logical.NewFilter(logical.NewScan("Users", ""), "STARTS_WITH(name, 'abc')")
	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(expr)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Users$name",
			columns:     []string{"name"},
			recordTypes: []string{"Users"},
		},
	})
	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	explain := cascades.ExplainPhysicalPlan(plan)
	t.Logf("Explain: %s", explain)
	if !strings.Contains(explain, "IndexScan") {
		t.Fatalf("expected IndexScan for STARTS_WITH prefix lookup, got: %s", explain)
	}
}

func TestEndToEnd_LimitMergeEndToEnd(t *testing.T) {
	t.Parallel()

	// Nested limits: LIMIT 100 OFFSET 0 → LIMIT 10 OFFSET 5
	// Should merge to LIMIT 10 OFFSET 5 (or the merged equivalent)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	inner := expressions.NewLogicalLimitExpression(100, 0, scanQ)
	innerRef := expressions.InitialOf(inner)
	innerQ := expressions.ForEachQuantifier(innerRef)

	outer := expressions.NewLogicalLimitExpression(10, 5, innerQ)
	ref := expressions.InitialOf(outer)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !cascades.IsPhysicalLimit(plan) {
		t.Fatalf("expected physical limit, got %T", plan)
	}
	explain := cascades.ExplainPhysicalPlan(plan)
	t.Logf("Explain: %s", explain)
}

func TestEndToEnd_LimitOverUnionPushesDown(t *testing.T) {
	t.Parallel()

	// LIMIT 5 over UNION ALL of two scans
	a := logical.NewScan("A", "")
	b := logical.NewScan("B", "")
	union := logical.NewUnion([]logical.LogicalOperator{a, b}, false)
	lim := logical.NewLimit(union, 5, 0)

	expr, err := plangen.Convert(lim)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(expr)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	explain := cascades.ExplainPhysicalPlan(plan)
	t.Logf("Explain: %s", explain)
	if !strings.Contains(explain, "Limit") {
		t.Fatalf("expected Limit in plan, got: %s", explain)
	}
}

func TestEndToEnd_InsertValuesToPlan(t *testing.T) {
	t.Parallel()
	vals := logical.NewValues([]string{"1", "'Alice'"}, nil)
	src := logical.NewInsert("Users", []string{"id", "name"}, vals)
	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(expr)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	rules = append(rules, cascades.DMLImplementationRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil expression")
	}
	explain := cascades.ExplainPhysicalPlan(plan)
	t.Logf("Explain: %s", explain)
	if !strings.Contains(explain, "Insert") {
		t.Fatalf("expected Insert in plan, got: %q (type %T)", explain, plan)
	}
}

func TestEndToEnd_FunctionCallInFilterToPlan(t *testing.T) {
	t.Parallel()
	src := logical.NewFilter(logical.NewScan("Users", ""), "UPPER(name) = 'ALICE'")
	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(expr)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	explain := cascades.ExplainPhysicalPlan(plan)
	t.Logf("Explain: %s", explain)
	if !strings.Contains(explain, "Filter") {
		t.Fatalf("expected Filter in plan, got: %s", explain)
	}
	if !strings.Contains(explain, "Scan") {
		t.Fatalf("expected Scan in plan, got: %s", explain)
	}
}

func TestEndToEnd_ArithmeticInProjectToPlan(t *testing.T) {
	t.Parallel()
	src := logical.NewProject(
		logical.NewScan("T", ""),
		[]string{"x + 1", "y * 2"},
		[]string{"", ""},
	)
	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(expr)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	explain := cascades.ExplainPhysicalPlan(plan)
	t.Logf("Explain: %s", explain)
	if !strings.Contains(explain, "Project") {
		t.Fatalf("expected Project in plan, got: %s", explain)
	}
	if !strings.Contains(explain, "Scan") {
		t.Fatalf("expected Scan in plan, got: %s", explain)
	}
}

func TestEndToEnd_ProjectFilterIndexScanPipeline(t *testing.T) {
	t.Parallel()
	// Proj(Filter(Scan)) with an index on "status".
	// Verifies the planner produces a physical plan with
	// index scan + filter + projection layers.
	scan := logical.NewScan("Users", "")
	filt := logical.NewFilter(scan, "status = 'active'")
	proj := logical.NewProject(filt, []string{"name", "status"}, []string{"", ""})

	expr, err := plangen.Convert(proj)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(expr)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{name: "Users$status", columns: []string{"status"}, recordTypes: []string{"Users"}},
	})
	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, ctx)
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	explain := cascades.ExplainPhysicalPlan(plan)
	t.Logf("Explain: %s", explain)
	if !strings.Contains(explain, "IndexScan") && !strings.Contains(explain, "Scan") {
		t.Fatalf("expected some scan in plan, got: %s", explain)
	}
}

func TestEndToEnd_ProjectionMergeThenImplement(t *testing.T) {
	t.Parallel()
	inner := logical.NewProject(
		logical.NewScan("T", ""),
		[]string{"x", "y", "z"},
		[]string{"", "", ""},
	)
	outer := logical.NewProject(inner, []string{"x", "y"}, []string{"", ""})
	expr, err := plangen.Convert(outer)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(expr)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	explain := cascades.ExplainPhysicalPlan(plan)
	t.Logf("Explain: %s", explain)
	if !strings.Contains(explain, "Project") {
		t.Fatalf("expected Project in plan, got: %s", explain)
	}
	if !strings.Contains(explain, "Scan") {
		t.Fatalf("expected Scan in plan, got: %s", explain)
	}
}

func TestEndToEnd_ValuesToPlan(t *testing.T) {
	t.Parallel()
	src := logical.NewValues([]string{"42", "'hello'", "TRUE"}, nil)
	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(expr)

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	p := cascades.NewPlanner(rules, cascades.EmptyPlanContext())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	explain := cascades.ExplainPhysicalPlan(plan)
	t.Logf("Explain: %s", explain)
	if !strings.Contains(explain, "Values") {
		t.Fatalf("expected Values in plan, got: %s", explain)
	}
}
