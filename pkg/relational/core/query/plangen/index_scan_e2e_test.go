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
