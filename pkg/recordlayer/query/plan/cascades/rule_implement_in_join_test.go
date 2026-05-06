package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

func TestImplementInJoinRule_MatchesSelectExpression(t *testing.T) {
	t.Parallel()
	rule := NewImplementInJoinRule()
	scanRef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, nil))
	q := expressions.ForEachQuantifier(scanRef)
	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(q.GetAlias()),
		[]expressions.Quantifier{q},
		nil,
	)
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), sel)
	if len(bindings) == 0 {
		t.Fatal("should match SelectExpression")
	}
}

func TestImplementInJoinRule_SkipsWithPredicates(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}
	innerRef := expressions.NewFinalReference([]expressions.RelationalExpression{sw})
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ForEachQuantifier(innerRef)

	explodeRef := expressions.InitialOf(
		expressions.NewExplodeExpression(&values.ConstantValue{Value: []any{1, 2, 3}}),
	)
	eq := expressions.ForEachQuantifier(explodeRef)

	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "x", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, 42),
	)
	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(q.GetAlias()),
		[]expressions.Quantifier{eq, q},
		[]predicates.QueryPredicate{pred},
	)

	outerRef := expressions.InitialOf(sel)
	results := FireImplementationRule(NewImplementInJoinRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should not fire with predicates, got %d", len(results))
	}
}

func TestImplementInJoinRule_SkipsSingleQuantifier(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}
	innerRef := expressions.NewFinalReference([]expressions.RelationalExpression{sw})
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ForEachQuantifier(innerRef)
	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(q.GetAlias()),
		[]expressions.Quantifier{q},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := FireImplementationRule(NewImplementInJoinRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should not fire with single quantifier, got %d", len(results))
	}
}

func TestImplementInJoinRule_FiresWithExplodeAndInner(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}
	innerRef := expressions.NewFinalReference([]expressions.RelationalExpression{sw})
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	innerQ := expressions.ForEachQuantifier(innerRef)

	explodeRef := expressions.InitialOf(
		expressions.NewExplodeExpression(&values.ConstantValue{Value: []any{1, 2, 3}}),
	)
	explodeQ := expressions.ForEachQuantifier(explodeRef)

	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(innerQ.GetAlias()),
		[]expressions.Quantifier{explodeQ, innerQ},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := FireImplementationRule(NewImplementInJoinRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should fire with explode + inner quantifier")
	}

	found := false
	for _, r := range results {
		if _, ok := r.(*physicalInJoinWrapper); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("should yield physicalInJoinWrapper")
	}
}

func TestImplementInJoinRule_SkipsWhenResultNotQOV(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}
	innerRef := expressions.NewFinalReference([]expressions.RelationalExpression{sw})
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	innerQ := expressions.ForEachQuantifier(innerRef)

	explodeRef := expressions.InitialOf(
		expressions.NewExplodeExpression(&values.ConstantValue{Value: []any{1}}),
	)
	explodeQ := expressions.ForEachQuantifier(explodeRef)

	sel := expressions.NewSelectExpression(
		&values.FieldValue{Field: "computed", Typ: values.UnknownType},
		[]expressions.Quantifier{explodeQ, innerQ},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := FireImplementationRule(NewImplementInJoinRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should not fire when result is not QOV for inner, got %d", len(results))
	}
}

func TestIsExplodeExpression_True(t *testing.T) {
	t.Parallel()
	ref := expressions.InitialOf(
		expressions.NewExplodeExpression(&values.ConstantValue{Value: []any{1}}),
	)
	if getExplodeExpression(ref) == nil {
		t.Fatal("should detect ExplodeExpression")
	}
}

func TestIsExplodeExpression_False(t *testing.T) {
	t.Parallel()
	ref := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, nil),
	)
	if getExplodeExpression(ref) != nil {
		t.Fatal("should not detect scan as ExplodeExpression")
	}
}

func TestIsSupportedExplodeValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		val  values.Value
		ok   bool
	}{
		{"constant", &values.ConstantValue{Value: []any{1, 2}}, true},
		{"quantified", values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()), true},
		{"field", &values.FieldValue{Field: "x"}, false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		if got := isSupportedExplodeValue(tc.val); got != tc.ok {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.ok)
		}
	}
}

func TestImplementInJoinRule_MultipleExplodes(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}
	innerRef := expressions.NewFinalReference([]expressions.RelationalExpression{sw})
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	innerQ := expressions.ForEachQuantifier(innerRef)

	explodeRef1 := expressions.InitialOf(
		expressions.NewExplodeExpression(&values.ConstantValue{Value: []any{1, 2}}),
	)
	explodeRef2 := expressions.InitialOf(
		expressions.NewExplodeExpression(&values.ConstantValue{Value: []any{"a", "b"}}),
	)
	eq1 := expressions.ForEachQuantifier(explodeRef1)
	eq2 := expressions.ForEachQuantifier(explodeRef2)

	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(innerQ.GetAlias()),
		[]expressions.Quantifier{eq1, eq2, innerQ},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := FireImplementationRule(NewImplementInJoinRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should fire with multiple explodes + inner quantifier")
	}
}

func TestImplementInJoinRule_WithIndexScanInner(t *testing.T) {
	t.Parallel()
	eqRange := predicates.EmptyComparisonRange()
	eqComp := predicates.NewLiteralComparison(predicates.ComparisonEquals, 1)
	eqRange.Merge(&eqComp)
	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_a", []*predicates.ComparisonRange{eqRange},
		[]string{"T"}, values.UnknownType, false)
	iw := &physicalIndexScanWrapper{
		plan:        indexPlan,
		columnNames: []string{"a"},
		unique:      false,
	}
	innerRef := expressions.NewFinalReference([]expressions.RelationalExpression{iw})
	pm := NewPlanPropertiesMap()
	pm.Add(iw)
	innerRef.SetPlanProperties(pm)

	innerQ := expressions.ForEachQuantifier(innerRef)

	explodeRef := expressions.InitialOf(
		expressions.NewExplodeExpression(&values.ConstantValue{Value: []any{1, 2, 3}}),
	)
	explodeQ := expressions.ForEachQuantifier(explodeRef)

	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(innerQ.GetAlias()),
		[]expressions.Quantifier{explodeQ, innerQ},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := FireImplementationRule(NewImplementInJoinRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should fire with index scan inner")
	}

	found := false
	for _, r := range results {
		if _, ok := r.(*physicalInJoinWrapper); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("should yield physicalInJoinWrapper with index scan inner")
	}
}
