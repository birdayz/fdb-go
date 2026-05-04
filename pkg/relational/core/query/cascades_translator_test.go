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
