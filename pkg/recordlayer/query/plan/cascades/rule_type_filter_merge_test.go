package cascades

import (
	"reflect"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestTypeFilterMergeRule_Intersects(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression(nil, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerTF := expressions.NewLogicalTypeFilterExpression([]string{"Order", "Customer", "Sale"}, scanQ)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerTF))
	outerTF := expressions.NewLogicalTypeFilterExpression([]string{"Order", "Sale"}, innerQ)
	ref := expressions.InitialOf(outerTF)

	rule := NewTypeFilterMergeRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}
	merged := yielded[0].(*expressions.LogicalTypeFilterExpression)
	want := []string{"Order", "Sale"}
	if got := merged.GetRecordTypes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("merged types=%v, want %v", got, want)
	}
}

func TestTypeFilterMergeRule_EmptyIntersection(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression(nil, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerTF := expressions.NewLogicalTypeFilterExpression([]string{"Order"}, scanQ)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerTF))
	outerTF := expressions.NewLogicalTypeFilterExpression([]string{"Customer"}, innerQ)
	ref := expressions.InitialOf(outerTF)
	rule := NewTypeFilterMergeRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1 (empty-intersection still emits, just with no types)", len(yielded))
	}
	merged := yielded[0].(*expressions.LogicalTypeFilterExpression)
	if got := merged.GetRecordTypes(); len(got) != 0 {
		t.Fatalf("empty-intersection got %v, want empty", got)
	}
}

func TestTypeFilterMergeRule_DeclinesOnSingle(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression(nil, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	tf := expressions.NewLogicalTypeFilterExpression([]string{"Order"}, scanQ)
	ref := expressions.InitialOf(tf)
	rule := NewTypeFilterMergeRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on a single TypeFilter — yielded %d, want 0", len(yielded))
	}
}

func TestIntersectStringSlices(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		a, b, want []string
	}{
		{name: "disjoint", a: []string{"A", "B"}, b: []string{"C", "D"}, want: nil},
		{name: "subset", a: []string{"A", "B"}, b: []string{"A", "B", "C"}, want: []string{"A", "B"}},
		{name: "overlap", a: []string{"A", "B", "C"}, b: []string{"B", "C", "D"}, want: []string{"B", "C"}},
		{name: "identical", a: []string{"A", "B"}, b: []string{"A", "B"}, want: []string{"A", "B"}},
		{name: "empty-a", a: nil, b: []string{"A"}, want: nil},
		{name: "both-empty", a: nil, b: nil, want: nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := intersectStringSlices(tc.a, tc.b)
			if len(got) == 0 && len(tc.want) == 0 {
				return // both empty — equal
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
