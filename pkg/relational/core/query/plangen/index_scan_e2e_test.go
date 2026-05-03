package plangen_test

import (
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
	p := cascades.NewPlanner(rules, ctx)
	if _, conv := p.Explore(ref); !conv {
		t.Fatal("planner did not converge")
	}

	// The top Reference should contain the index scan directly (sort
	// eliminated because the index on (status, date) with status=eq
	// provides date ordering).
	foundIndexScanAtTop := false
	for _, m := range ref.Members() {
		if cascades.IsPhysicalIndexScan(m) {
			foundIndexScanAtTop = true
			break
		}
	}
	if !foundIndexScanAtTop {
		t.Fatal("sort should be eliminated; index scan should appear at top")
	}
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
	p := cascades.NewPlanner(rules, ctx)
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
	p := cascades.NewPlanner(rules, ctx)
	if _, conv := p.Explore(ref); !conv {
		t.Fatal("planner did not converge")
	}

	foundIndexScanAtTop := false
	for _, m := range ref.Members() {
		if cascades.IsPhysicalIndexScan(m) {
			foundIndexScanAtTop = true
			break
		}
	}
	if !foundIndexScanAtTop {
		t.Fatal("sort should be eliminated; index(status,date) with status=eq AND date>x provides date ordering")
	}
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
	p := cascades.NewPlanner(rules, ctx)
	if _, conv := p.Explore(ref); !conv {
		t.Fatal("planner did not converge")
	}

	// Sort should be eliminated — the physicalFilterWrapper wrapping
	// the index scan preserves DATE ordering.
	foundPhysicalAtTop := false
	for _, m := range ref.Members() {
		if cascades.IsPhysicalIndexScan(m) || cascades.IsPhysicalFilter(m) {
			foundPhysicalAtTop = true
			break
		}
	}
	if !foundPhysicalAtTop {
		t.Fatal("sort should be eliminated through residual filter; physical plan should appear at top")
	}
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
	// (sort eliminated by SortOverOrderedElimRule).
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
