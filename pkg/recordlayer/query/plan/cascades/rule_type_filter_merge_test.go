package cascades

import (
	"context"
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func typeRewriteRowType() *values.RecordType {
	return values.NewRecordType("TypeRewriteRow", false, []values.Field{{
		Name: "ID", FieldType: values.NotNullLong,
	}})
}

func mustTypeRewriteConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct type-rewrite fixture: " + err.Error())
	}
	return value
}

func typeRewriteScan(recordTypes ...string) *expressions.FullUnorderedScanExpression {
	return mustTypeRewriteConstruct(expressions.NewFullUnorderedScanExpression(
		recordTypes, typeRewriteRowType()))
}

func typeRewriteFilter(
	recordTypes []string, inner expressions.Quantifier,
) *expressions.LogicalTypeFilterExpression {
	return mustTypeRewriteConstruct(expressions.NewLogicalTypeFilterExpression(recordTypes, inner))
}

func fireTypeRewriteRule(
	t testing.TB, rule ExpressionRule, ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireExpressionRule(rule, ref)
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	return result
}

func exploreTypeRewriting(p *Planner, rootRef *expressions.Reference) (int, bool) {
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

func TestTypeFilterMergeRule_Intersects(t *testing.T) {
	t.Parallel()
	scan := typeRewriteScan()
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerTF := typeRewriteFilter([]string{"Order", "Customer", "Sale"}, scanQ)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerTF))
	outerTF := typeRewriteFilter([]string{"Order", "Sale"}, innerQ)
	ref := expressions.InitialOf(outerTF)

	rule := NewTypeFilterMergeRule()
	yielded := fireTypeRewriteRule(t, rule, ref)
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
	scan := typeRewriteScan()
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerTF := typeRewriteFilter([]string{"Order"}, scanQ)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerTF))
	outerTF := typeRewriteFilter([]string{"Customer"}, innerQ)
	ref := expressions.InitialOf(outerTF)
	rule := NewTypeFilterMergeRule()
	yielded := fireTypeRewriteRule(t, rule, ref)
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
	scan := typeRewriteScan()
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	tf := typeRewriteFilter([]string{"Order"}, scanQ)
	ref := expressions.InitialOf(tf)
	rule := NewTypeFilterMergeRule()
	yielded := fireTypeRewriteRule(t, rule, ref)
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
