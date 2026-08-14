package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustProjectionFinalConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct projection-final fixture: " + err.Error())
	}
	return value
}

func projectionFinalRowType() *values.RecordType {
	return values.NewRecordType("projection_final_row", false, []values.Field{{
		Name:      "ID",
		FieldType: values.NotNullLong,
		Ordinal:   0,
	}})
}

func projectionFinalScan() *expressions.FullUnorderedScanExpression {
	return mustProjectionFinalConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, projectionFinalRowType()))
}

func TestImplementProjectionFinalRule_MatchesProjection(t *testing.T) {
	t.Parallel()
	rule := NewImplementProjectionFinalRule()

	inner := expressions.ForEachQuantifier(expressions.InitialOf(projectionFinalScan()))
	root := mustProjectionFinalConstruct(inner.RequireFlowedObjectValue())
	id := mustProjectionFinalConstruct(values.ResolveFieldOrdinals(root, []int{0}))
	proj := mustProjectionFinalConstruct(expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{id},
		nil,
		inner,
	))

	bindings := rule.Matcher().BindMatches(matching.NewBindings(), proj)
	if len(bindings) == 0 {
		t.Fatal("rule should match LogicalProjectionExpression")
	}
}

func TestImplementProjectionFinalRule_DoesNotMatchScan(t *testing.T) {
	t.Parallel()
	rule := NewImplementProjectionFinalRule()

	scan := projectionFinalScan()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), scan)
	if len(bindings) != 0 {
		t.Fatal("rule should NOT match non-projection expressions")
	}
}
