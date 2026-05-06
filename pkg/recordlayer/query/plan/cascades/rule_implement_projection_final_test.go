package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestImplementProjectionFinalRule_MatchesProjection(t *testing.T) {
	t.Parallel()
	rule := NewImplementProjectionFinalRule()

	proj := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{&values.FieldValue{Field: "ID", Typ: values.UnknownType}},
		nil,
		expressions.ForEachQuantifier(expressions.InitialOf(
			expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
		)),
	)

	bindings := rule.Matcher().BindMatches(matching.NewBindings(), proj)
	if len(bindings) == 0 {
		t.Fatal("rule should match LogicalProjectionExpression")
	}
}

func TestImplementProjectionFinalRule_DoesNotMatchScan(t *testing.T) {
	t.Parallel()
	rule := NewImplementProjectionFinalRule()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), scan)
	if len(bindings) != 0 {
		t.Fatal("rule should NOT match non-projection expressions")
	}
}
