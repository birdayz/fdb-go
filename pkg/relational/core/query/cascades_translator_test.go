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
