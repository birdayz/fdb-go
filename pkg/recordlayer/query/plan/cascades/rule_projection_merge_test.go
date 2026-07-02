package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// stackedProjections builds Project([outerVals]) over Project([innerVals]) over Scan("T").
func stackedProjections(outerVals, innerVals []values.Value) *expressions.LogicalProjectionExpression {
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerProj := expressions.NewLogicalProjectionExpression(innerVals, innerQ)
	outerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerProj))
	return expressions.NewLogicalProjectionExpression(outerVals, outerQ)
}

func TestProjectionMergeRule_Fires(t *testing.T) {
	t.Parallel()
	outerVals := []values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType},
	}
	innerVals := []values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType},
		&values.FieldValue{Field: "name", Typ: values.UnknownType},
		&values.FieldValue{Field: "age", Typ: values.UnknownType},
	}
	stacked := stackedProjections(outerVals, innerVals)
	ref := expressions.InitialOf(stacked)
	rule := NewProjectionMergeRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 1 {
		t.Fatalf("rule yielded %d expressions, want 1", len(yielded))
	}
	flat, ok := yielded[0].(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalProjectionExpression", yielded[0])
	}
	// Outer projection list preserved — exactly one entry, FieldValue("id").
	pv := flat.GetProjectedValues()
	if len(pv) != 1 {
		t.Fatalf("flat projected values len=%d, want 1", len(pv))
	}
	fv, ok := pv[0].(*values.FieldValue)
	if !ok || fv.Field != "id" {
		t.Fatalf("flat projected[0] = %v, want FieldValue(id)", pv[0])
	}
	// Inner of the flat projection is the Scan, not the inner projection.
	if _, ok := flat.GetInner().GetRangesOver().Get().(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("flat inner = %T, want *FullUnorderedScanExpression", flat.GetInner().GetRangesOver().Get())
	}
}

func TestProjectionMergeRule_DeclinesOnNonProjectionInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}},
		q,
	)
	ref := expressions.InitialOf(proj)
	rule := NewProjectionMergeRule()
	yielded := FireExpressionRule(rule, ref)
	if got := len(yielded); got != 0 {
		t.Fatalf("rule yielded %d on non-projection inner, want 0", got)
	}
}

func TestProjectionMergeRule_TriplyNested_FlattensInTwoFires(t *testing.T) {
	t.Parallel()
	// Project([id]) over Project([id, name]) over Project([id, name, age]) over Scan
	deepest := []values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType},
		&values.FieldValue{Field: "name", Typ: values.UnknownType},
		&values.FieldValue{Field: "age", Typ: values.UnknownType},
	}
	middle := []values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType},
		&values.FieldValue{Field: "name", Typ: values.UnknownType},
	}
	top := []values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType},
	}

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	deepProj := expressions.NewLogicalProjectionExpression(deepest, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	midProj := expressions.NewLogicalProjectionExpression(middle, expressions.ForEachQuantifier(expressions.InitialOf(deepProj)))
	topProj := expressions.NewLogicalProjectionExpression(top, expressions.ForEachQuantifier(expressions.InitialOf(midProj)))

	// Drive through the unified exploration driver so the rule
	// re-fires on its own yields until stable.
	ref := expressions.InitialOf(topProj)
	if _, conv := exploreRewriting(NewPlanner([]ExpressionRule{NewProjectionMergeRule()}, nil), ref); !conv {
		t.Fatal("exploration did not converge")
	}
	// After the rule applies twice, we should have a 1-deep projection
	// over the scan. The Reference's last yielded member is the flattest.
	members := ref.Members()
	flatFound := false
	for _, m := range members {
		p, ok := m.(*expressions.LogicalProjectionExpression)
		if !ok {
			continue
		}
		// Look at the immediate inner: is it the scan?
		if _, ok := p.GetInner().GetRangesOver().Get().(*expressions.FullUnorderedScanExpression); ok && len(p.GetProjectedValues()) == 1 {
			flatFound = true
			break
		}
	}
	if !flatFound {
		t.Fatalf("exploration did not produce a 1-deep projection over Scan; members=%d", len(members))
	}
}
