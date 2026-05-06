package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestProjectionElimRule_FiresOnIdentity(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	// Projection's single Value is the inner Quantifier's flowed
	// object — identity projection.
	p := expressions.NewLogicalProjectionExpression(
		[]values.Value{q.GetFlowedObjectValue()},
		q,
	)
	ref := expressions.InitialOf(p)
	yielded := FireExpressionRule(NewProjectionElimRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}
	if _, ok := yielded[0].(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("yielded type=%T, want *FullUnorderedScanExpression", yielded[0])
	}
}

func TestProjectionElimRule_DeclinesOnMultipleColumns(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	p := expressions.NewLogicalProjectionExpression(
		[]values.Value{q.GetFlowedObjectValue(), values.NewBooleanValue(true)},
		q,
	)
	ref := expressions.InitialOf(p)
	yielded := FireExpressionRule(NewProjectionElimRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on multi-column projection — yielded %d, want 0", len(yielded))
	}
}

func TestProjectionElimRule_DeclinesOnComputedSingle(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	// Single Value, but it's NOT the flowed object (computed expression).
	p := expressions.NewLogicalProjectionExpression(
		[]values.Value{values.NewBooleanValue(true)},
		q,
	)
	ref := expressions.InitialOf(p)
	yielded := FireExpressionRule(NewProjectionElimRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on a computed projection — yielded %d, want 0", len(yielded))
	}
}

func TestProjectionElimRule_DeclinesOnDifferentAlias(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	otherAlias := values.NamedCorrelationIdentifier("OTHER")
	p := expressions.NewLogicalProjectionExpression(
		[]values.Value{values.NewQuantifiedObjectValue(otherAlias)},
		q,
	)
	ref := expressions.InitialOf(p)
	yielded := FireExpressionRule(NewProjectionElimRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on projection of different-alias QOV — yielded %d, want 0", len(yielded))
	}
}
