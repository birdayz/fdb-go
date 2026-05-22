package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

func TestImplementSimpleSelectRule_MatchesSelectExpression(t *testing.T) {
	t.Parallel()
	rule := NewImplementSimpleSelectRule()
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

func TestImplementSimpleSelectRule_SkipsNonSelect(t *testing.T) {
	t.Parallel()
	rule := NewImplementSimpleSelectRule()
	scanRef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, nil))
	filter := expressions.NewLogicalFilterExpression(nil, expressions.ForEachQuantifier(scanRef))
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), filter)
	if len(bindings) != 0 {
		t.Fatal("should NOT match LogicalFilterExpression")
	}
}

func TestImplementSimpleSelectRule_SkipsMultiQuantifier(t *testing.T) {
	t.Parallel()
	scanA := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"A"}, nil))
	scanB := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"B"}, nil))
	qA := expressions.ForEachQuantifier(scanA)
	qB := expressions.ForEachQuantifier(scanB)
	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(qA.GetAlias()),
		[]expressions.Quantifier{qA, qB},
		nil,
	)

	scan := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}
	scanA.Insert(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	scanA.SetPlanProperties(pm)

	outerRef := expressions.InitialOf(sel)
	results := FireImplementationRule(NewImplementSimpleSelectRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should not fire on multi-quantifier SELECT, got %d results", len(results))
	}
}

func TestImplementSimpleSelectRule_NoPredicatesSimpleResult(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}

	innerRef := expressions.InitialOf(sw)
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
	results := FireImplementationRule(NewImplementSimpleSelectRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should yield when no predicates and simple result (pass-through)")
	}
	if results[0] != sw {
		t.Fatalf("should yield inner scan wrapper directly, got %T", results[0])
	}
}

func TestImplementSimpleSelectRule_WithPredicates(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}

	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ForEachQuantifier(innerRef)
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "x", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, 42),
	)
	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(q.GetAlias()),
		[]expressions.Quantifier{q},
		[]predicates.QueryPredicate{pred},
	)

	outerRef := expressions.InitialOf(sel)
	results := FireImplementationRule(NewImplementSimpleSelectRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should yield a filter wrapper")
	}
	if _, ok := results[0].(*physicalPredicatesFilterWrapper); !ok {
		t.Fatalf("expected physicalPredicatesFilterWrapper, got %T", results[0])
	}
}

func TestImplementSimpleSelectRule_WithProjection(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}

	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ForEachQuantifier(innerRef)
	resultValue := &values.FieldValue{Field: "projected_col", Typ: values.UnknownType}
	sel := expressions.NewSelectExpression(
		resultValue,
		[]expressions.Quantifier{q},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := FireImplementationRule(NewImplementSimpleSelectRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should yield a map wrapper for non-trivial result")
	}
	if _, ok := results[0].(*physicalMapWrapper); !ok {
		t.Fatalf("expected physicalMapWrapper for projection, got %T", results[0])
	}
}

func TestImplementSimpleSelectRule_NilInnerRef(t *testing.T) {
	t.Parallel()
	q := expressions.ForEachQuantifier(nil)
	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(q.GetAlias()),
		[]expressions.Quantifier{q},
		nil,
	)
	outerRef := expressions.InitialOf(sel)
	results := FireImplementationRule(NewImplementSimpleSelectRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should yield nothing for nil inner ref, got %d", len(results))
	}
}

func TestImplementSimpleSelectRule_ExistentialQuantifier(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}

	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ExistentialQuantifier(innerRef)
	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(q.GetAlias()),
		[]expressions.Quantifier{q},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := FireImplementationRule(NewImplementSimpleSelectRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should yield for Existential quantifier")
	}

	found := false
	for _, r := range results {
		if _, ok := r.(*physicalFirstOrDefaultWrapper); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("Existential quantifier should produce FirstOrDefault wrapper")
	}
}

func TestImplementSimpleSelectRule_NullOnEmptyQuantifier(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}

	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ForEachNullOnEmptyQuantifier(innerRef)
	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(q.GetAlias()),
		[]expressions.Quantifier{q},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := FireImplementationRule(NewImplementSimpleSelectRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should yield for nullOnEmpty quantifier")
	}

	found := false
	for _, r := range results {
		if _, ok := r.(*physicalDefaultOnEmptyWrapper); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("nullOnEmpty ForEach should produce DefaultOnEmpty wrapper")
	}
}

func TestImplementSimpleSelectRule_TautologyPredicatesFiltered(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}

	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ForEachQuantifier(innerRef)
	tautology := predicates.NewConstantPredicate(predicates.TriTrue)
	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(q.GetAlias()),
		[]expressions.Quantifier{q},
		[]predicates.QueryPredicate{tautology},
	)

	outerRef := expressions.InitialOf(sel)
	results := FireImplementationRule(NewImplementSimpleSelectRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should yield when tautology is the only predicate")
	}
	if _, ok := results[0].(*physicalScanWrapper); !ok {
		t.Fatalf("tautology-only predicate should pass through to scan, got %T", results[0])
	}
}
