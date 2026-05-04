package query

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
)

func TestTranslateScan(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("orders", "")
	ref := TranslateToCascades(scan)
	if ref == nil {
		t.Fatal("expected non-nil reference")
	}
	members := ref.Members()
	if len(members) != 1 {
		t.Fatalf("expected 1 member, got %d", len(members))
	}
	if _, ok := members[0].(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("expected FullUnorderedScanExpression, got %T", members[0])
	}
}

func TestTranslateFilterOverScan(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("orders", "")
	filter := logical.NewFilter(scan, "price > 10")
	ref := TranslateToCascades(filter)
	if ref == nil {
		t.Fatal("expected non-nil reference")
	}
	members := ref.Members()
	if len(members) != 1 {
		t.Fatalf("expected 1 member, got %d", len(members))
	}
	if _, ok := members[0].(*expressions.LogicalFilterExpression); !ok {
		t.Fatalf("expected LogicalFilterExpression, got %T", members[0])
	}
}

func TestTranslateLimit(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("orders", "")
	limit := logical.NewLimit(scan, 10, 5)
	ref := TranslateToCascades(limit)
	if ref == nil {
		t.Fatal("expected non-nil reference")
	}
	members := ref.Members()
	if _, ok := members[0].(*expressions.LogicalLimitExpression); !ok {
		t.Fatalf("expected LogicalLimitExpression, got %T", members[0])
	}
}

func TestTranslateUnion(t *testing.T) {
	t.Parallel()
	scanA := logical.NewScan("A", "")
	scanB := logical.NewScan("B", "")
	union := logical.NewUnion([]logical.LogicalOperator{scanA, scanB}, false)
	ref := TranslateToCascades(union)
	if ref == nil {
		t.Fatal("expected non-nil reference")
	}
	if _, ok := ref.Members()[0].(*expressions.LogicalUnionExpression); !ok {
		t.Fatalf("expected LogicalUnionExpression, got %T", ref.Members()[0])
	}
}

func TestTranslateDistinctUnion(t *testing.T) {
	t.Parallel()
	scanA := logical.NewScan("A", "")
	scanB := logical.NewScan("B", "")
	union := logical.NewUnion([]logical.LogicalOperator{scanA, scanB}, true)
	ref := TranslateToCascades(union)
	if ref == nil {
		t.Fatal("expected non-nil reference for UNION DISTINCT")
	}
	if _, ok := ref.Members()[0].(*expressions.LogicalDistinctExpression); !ok {
		t.Fatalf("expected LogicalDistinctExpression wrapping union, got %T", ref.Members()[0])
	}
}

func TestTranslateSort(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("orders", "")
	sort := logical.NewSort(scan, []logical.SortKey{
		{Expr: "price", Dir: logical.SortAsc},
		{Expr: "id", Dir: logical.SortDesc},
	})
	ref := TranslateToCascades(sort)
	if ref == nil {
		t.Fatal("expected non-nil reference")
	}
	if _, ok := ref.Members()[0].(*expressions.LogicalSortExpression); !ok {
		t.Fatalf("expected LogicalSortExpression, got %T", ref.Members()[0])
	}
}

func TestTranslateProject(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("orders", "")
	proj := logical.NewProject(scan, []string{"id", "price"}, []string{"", "cost"})
	ref := TranslateToCascades(proj)
	if ref == nil {
		t.Fatal("expected non-nil reference")
	}
	if _, ok := ref.Members()[0].(*expressions.LogicalProjectionExpression); !ok {
		t.Fatalf("expected LogicalProjectionExpression, got %T", ref.Members()[0])
	}
}

func TestTranslateJoin(t *testing.T) {
	t.Parallel()
	left := logical.NewScan("orders", "")
	right := logical.NewScan("items", "")
	join := logical.NewJoin(left, right, logical.JoinInner, "orders.id = items.order_id")
	ref := TranslateToCascades(join)
	if ref == nil {
		t.Fatal("expected non-nil reference")
	}
	if _, ok := ref.Members()[0].(*expressions.SelectExpression); !ok {
		t.Fatalf("expected SelectExpression for join, got %T", ref.Members()[0])
	}
}

func TestTranslateNil(t *testing.T) {
	t.Parallel()
	ref := TranslateToCascades(nil)
	if ref != nil {
		t.Fatal("expected nil for nil input")
	}
}

func TestTranslateAggregate(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("orders", "")
	agg := logical.NewAggregate(scan, []string{"CATEGORY"}, []string{"SUM(PRICE)", "COUNT(*)"}, []string{"total", "cnt"}, "")
	ref := TranslateToCascades(agg)
	if ref == nil {
		t.Fatal("expected non-nil reference for aggregate")
	}
	gb, ok := ref.Members()[0].(*expressions.GroupByExpression)
	if !ok {
		t.Fatalf("expected GroupByExpression, got %T", ref.Members()[0])
	}
	if len(gb.GetGroupingKeys()) != 1 {
		t.Fatalf("expected 1 grouping key, got %d", len(gb.GetGroupingKeys()))
	}
	if len(gb.GetAggregates()) != 2 {
		t.Fatalf("expected 2 aggregates, got %d", len(gb.GetAggregates()))
	}
	if gb.GetAggregates()[0].Function != expressions.AggSum {
		t.Fatalf("expected AggSum, got %d", gb.GetAggregates()[0].Function)
	}
	if gb.GetAggregates()[1].Function != expressions.AggCount {
		t.Fatalf("expected AggCount, got %d", gb.GetAggregates()[1].Function)
	}
}

func TestTranslateAggregateNoGroup(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("orders", "")
	agg := logical.NewAggregate(scan, nil, []string{"COUNT(*)"}, []string{"cnt"}, "")
	ref := TranslateToCascades(agg)
	if ref == nil {
		t.Fatal("expected non-nil reference for scalar aggregate")
	}
	gb, ok := ref.Members()[0].(*expressions.GroupByExpression)
	if !ok {
		t.Fatalf("expected GroupByExpression, got %T", ref.Members()[0])
	}
	if len(gb.GetGroupingKeys()) != 0 {
		t.Fatalf("expected 0 grouping keys, got %d", len(gb.GetGroupingKeys()))
	}
}

func TestParseAggregateText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		fn    expressions.AggregateFunction
		ok    bool
	}{
		{"COUNT(*)", expressions.AggCount, true},
		{"SUM(PRICE)", expressions.AggSum, true},
		{"AVG(X)", expressions.AggAvg, true},
		{"MIN(Y)", expressions.AggMin, true},
		{"MAX(Z)", expressions.AggMax, true},
		{"count(*)", expressions.AggCount, true},
		{"UNKNOWN(X)", 0, false},
		{"noparen", 0, false},
	}
	for _, tc := range tests {
		spec, ok := parseAggregateText(tc.input)
		if ok != tc.ok {
			t.Errorf("parseAggregateText(%q): ok=%v, want %v", tc.input, ok, tc.ok)
			continue
		}
		if ok && spec.Function != tc.fn {
			t.Errorf("parseAggregateText(%q): fn=%d, want %d", tc.input, spec.Function, tc.fn)
		}
	}
}

func TestTranslateDistinct(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("orders", "")
	dist := logical.NewDistinct(scan)
	ref := TranslateToCascades(dist)
	if ref == nil {
		t.Fatal("expected non-nil reference for DISTINCT")
	}
	if _, ok := ref.Members()[0].(*expressions.LogicalDistinctExpression); !ok {
		t.Fatalf("expected LogicalDistinctExpression, got %T", ref.Members()[0])
	}
}

func TestTranslateNestedSortFilterScan(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("orders", "")
	filter := logical.NewFilter(scan, "active = true")
	sort := logical.NewSort(filter, []logical.SortKey{{Expr: "id", Dir: logical.SortAsc}})
	limit := logical.NewLimit(sort, 20, 0)
	ref := TranslateToCascades(limit)
	if ref == nil {
		t.Fatal("expected non-nil reference for nested tree")
	}
}

func TestTranslateCTEInlines(t *testing.T) {
	t.Parallel()
	body := logical.NewScan("Product", "")
	main := logical.NewScan("expensive", "")
	cte := logical.NewCTE("expensive", body, main, false)

	ref := TranslateToCascades(cte)
	if ref == nil {
		t.Fatal("expected non-nil reference for non-recursive CTE")
	}
	scan, ok := ref.Members()[0].(*expressions.FullUnorderedScanExpression)
	if !ok {
		t.Fatalf("expected inlined FullUnorderedScanExpression, got %T", ref.Members()[0])
	}
	if scan.GetRecordTypes()[0] != "Product" {
		t.Fatalf("expected scan of Product, got %s", scan.GetRecordTypes()[0])
	}
}

func TestTranslateCTEWithFilter(t *testing.T) {
	t.Parallel()
	body := logical.NewFilter(logical.NewScan("Product", ""), "price > 100")
	main := logical.NewProject(
		logical.NewScan("expensive", ""),
		[]string{"name"}, []string{""},
	)
	cte := logical.NewCTE("expensive", body, main, false)

	ref := TranslateToCascades(cte)
	if ref == nil {
		t.Fatal("expected non-nil reference")
	}
	proj, ok := ref.Members()[0].(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("expected LogicalProjectionExpression, got %T", ref.Members()[0])
	}
	innerRef := proj.GetQuantifiers()[0].GetRangesOver()
	inner := innerRef.Members()[0]
	if _, ok := inner.(*expressions.LogicalFilterExpression); !ok {
		t.Fatalf("expected inlined LogicalFilterExpression under projection, got %T", inner)
	}
}

func TestTranslateCTEChained(t *testing.T) {
	t.Parallel()
	bodyA := logical.NewScan("Product", "")
	mainA := logical.NewScan("B", "")
	bodyB := logical.NewScan("A", "")
	cteA := logical.NewCTE("A", bodyA, mainA, false)
	cteB := logical.NewCTE("B", bodyB, cteA, false)

	ref := TranslateToCascades(cteB)
	if ref == nil {
		t.Fatal("expected non-nil reference for chained CTEs")
	}
	scan, ok := ref.Members()[0].(*expressions.FullUnorderedScanExpression)
	if !ok {
		t.Fatalf("expected FullUnorderedScanExpression, got %T", ref.Members()[0])
	}
	if scan.GetRecordTypes()[0] != "Product" {
		t.Fatalf("expected scan of Product (A inlined into B's body), got %s", scan.GetRecordTypes()[0])
	}
}

func TestTranslateCTEOuterTextFilterBailsToNaive(t *testing.T) {
	t.Parallel()
	// Main query has a text-only filter on the CTE reference.
	// This must bail (return nil) so the planner falls back to naive
	// rather than silently dropping the filter.
	body := logical.NewScan("Product", "")
	main := logical.NewFilter(logical.NewScan("expensive", ""), "id > 5")
	cte := logical.NewCTE("expensive", body, main, false)

	ref := TranslateToCascades(cte)
	if ref != nil {
		t.Fatal("expected nil — text-only filter on CTE reference should bail to naive")
	}
}

func TestTranslateCTEShadowsTableName(t *testing.T) {
	t.Parallel()
	// CTE name = table name in body — must not infinite-recurse.
	body := logical.NewProject(logical.NewScan("T", ""), []string{"id"}, []string{""})
	main := logical.NewProject(logical.NewScan("T", ""), []string{"id"}, []string{""})
	cte := logical.NewCTE("T", body, main, false)

	ref := TranslateToCascades(cte)
	if ref == nil {
		t.Fatal("expected non-nil reference when CTE name shadows table name")
	}
	proj, ok := ref.Members()[0].(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("expected LogicalProjectionExpression, got %T", ref.Members()[0])
	}
	innerRef := proj.GetQuantifiers()[0].GetRangesOver()
	innerProj, ok := innerRef.Members()[0].(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("expected inlined projection from CTE body, got %T", innerRef.Members()[0])
	}
	innerScan := innerProj.GetQuantifiers()[0].GetRangesOver().Members()[0]
	if _, ok := innerScan.(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("expected FullUnorderedScanExpression at leaf, got %T", innerScan)
	}
}

func TestTranslateCTEMultipleReferences(t *testing.T) {
	t.Parallel()
	// CTE referenced twice in the main query (via join).
	body := logical.NewScan("Product", "")
	left := logical.NewScan("p", "")
	right := logical.NewScan("p", "")
	join := logical.NewJoin(left, right, logical.JoinInner, "")
	cte := logical.NewCTE("p", body, join, false)

	ref := TranslateToCascades(cte)
	if ref == nil {
		t.Fatal("expected non-nil reference for CTE with double reference")
	}
	sel, ok := ref.Members()[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected SelectExpression for join, got %T", ref.Members()[0])
	}
	quants := sel.GetQuantifiers()
	if len(quants) != 2 {
		t.Fatalf("expected 2 quantifiers, got %d", len(quants))
	}
}

func TestTranslateAggregateWithHavingReturnsNil(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("orders", "")
	agg := logical.NewAggregate(scan, []string{"REGION"}, []string{"SUM(PRICE)"}, []string{"total"}, "SUM(PRICE) > 100")
	ref := TranslateToCascades(agg)
	if ref != nil {
		t.Fatal("expected nil — aggregate with HAVING should bail to naive")
	}
}

func TestTranslateRecursiveCTEReturnsNil(t *testing.T) {
	t.Parallel()
	body := logical.NewScan("Product", "")
	main := logical.NewScan("recursive_cte", "")
	cte := logical.NewCTE("recursive_cte", body, main, true)

	ref := TranslateToCascades(cte)
	if ref != nil {
		t.Fatal("expected nil for recursive CTE (not yet supported)")
	}
}
