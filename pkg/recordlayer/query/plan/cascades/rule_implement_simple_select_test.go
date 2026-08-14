package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustSimpleSelectConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct simple-select fixture: " + err.Error())
	}
	return value
}

func mustFireSimpleSelectRule(
	t testing.TB,
	rule ImplementationRule,
	ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	yielded, err := FireImplementationRule(rule, ref)
	if err != nil {
		t.Fatalf("FireImplementationRule() unexpected error: %v", err)
	}
	return yielded
}

func simpleSelectRowType() *values.RecordType {
	return values.NewRecordType("simple_select_row", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "x", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "projected_col", FieldType: values.NotNullLong, Ordinal: 2},
	})
}

func simpleSelectLogicalScan(recordType string) *expressions.FullUnorderedScanExpression {
	return mustSimpleSelectConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{recordType}, simpleSelectRowType()))
}

func simpleSelectPlanScan(recordType string) *plans.RecordQueryScanPlan {
	return mustSimpleSelectConstruct(plans.NewRecordQueryScanPlan(
		[]string{recordType}, simpleSelectRowType(), false))
}

func simpleSelectFlowedObject(q expressions.Quantifier) values.QuantifiedObjectValue {
	return mustSimpleSelectConstruct(q.RequireFlowedObjectValue())
}

func simpleSelectQOV(
	alias values.CorrelationIdentifier,
	flowedType values.Type,
) values.QuantifiedObjectValue {
	return mustSimpleSelectConstruct(values.NewQuantifiedObjectValue(alias, flowedType))
}

func simpleSelectField(q expressions.Quantifier, ordinal int) values.Value {
	return mustSimpleSelectConstruct(values.ResolveFieldOrdinals(
		simpleSelectFlowedObject(q), []int{ordinal}))
}

func simpleSelectExpression(
	result values.Value,
	quantifiers []expressions.Quantifier,
	preds []predicates.QueryPredicate,
) *expressions.SelectExpression {
	return mustSimpleSelectConstruct(expressions.NewSelectExpression(
		result, quantifiers, preds))
}

func TestImplementSimpleSelectRule_MatchesSelectExpression(t *testing.T) {
	t.Parallel()
	rule := NewImplementSimpleSelectRule()
	scanRef := expressions.InitialOf(simpleSelectLogicalScan("T"))
	q := expressions.ForEachQuantifier(scanRef)
	sel := simpleSelectExpression(
		simpleSelectFlowedObject(q),
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
	scanRef := expressions.InitialOf(simpleSelectLogicalScan("T"))
	filter := mustSimpleSelectConstruct(expressions.NewLogicalFilterExpression(
		nil, expressions.ForEachQuantifier(scanRef)))
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), filter)
	if len(bindings) != 0 {
		t.Fatal("should NOT match LogicalFilterExpression")
	}
}

func TestImplementSimpleSelectRule_SkipsMultiQuantifier(t *testing.T) {
	t.Parallel()
	scanA := expressions.InitialOf(simpleSelectLogicalScan("A"))
	scanB := expressions.InitialOf(simpleSelectLogicalScan("B"))
	qA := expressions.ForEachQuantifier(scanA)
	qB := expressions.ForEachQuantifier(scanB)
	sel := simpleSelectExpression(
		simpleSelectFlowedObject(qA),
		[]expressions.Quantifier{qA, qB},
		nil,
	)

	scan := simpleSelectPlanScan("A")
	sw := scan
	scanA.Insert(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	scanA.SetPlanProperties(pm)

	outerRef := expressions.InitialOf(sel)
	results := mustFireSimpleSelectRule(t, NewImplementSimpleSelectRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should not fire on multi-quantifier SELECT, got %d results", len(results))
	}
}

func TestImplementSimpleSelectRule_NoPredicatesSimpleResult(t *testing.T) {
	t.Parallel()
	scan := simpleSelectPlanScan("T")
	sw := scan

	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ForEachQuantifier(innerRef)
	sel := simpleSelectExpression(
		simpleSelectFlowedObject(q),
		[]expressions.Quantifier{q},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := mustFireSimpleSelectRule(t, NewImplementSimpleSelectRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should yield when no predicates and simple result (pass-through)")
	}
	if results[0] != sw {
		t.Fatalf("should yield inner scan wrapper directly, got %T", results[0])
	}
}

func TestImplementSimpleSelectRule_StrictSingleFailsClosed(t *testing.T) {
	t.Parallel()

	scan := simpleSelectPlanScan("T")
	innerRef := expressions.InitialOf(scan)
	pm := NewPlanPropertiesMap()
	pm.Add(scan)
	innerRef.SetPlanProperties(pm)
	alias := values.NamedCorrelationIdentifier("STRICT")
	q := expressions.NamedForEachStrictSingleQuantifier(alias, innerRef)
	sel := simpleSelectExpression(
		simpleSelectFlowedObject(q),
		[]expressions.Quantifier{q},
		nil,
	)

	results := mustFireSimpleSelectRule(t,
		NewImplementSimpleSelectRule(), expressions.InitialOf(sel))
	if len(results) != 0 {
		t.Fatalf("standalone strict-single select yielded %d implementation(s), want zero", len(results))
	}
}

func TestImplementSimpleSelectRule_WithPredicates(t *testing.T) {
	t.Parallel()
	scan := simpleSelectPlanScan("T")
	sw := scan

	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ForEachQuantifier(innerRef)
	pred := predicates.NewComparisonPredicate(
		simpleSelectField(q, 1),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
	)
	sel := simpleSelectExpression(
		simpleSelectFlowedObject(q),
		[]expressions.Quantifier{q},
		[]predicates.QueryPredicate{pred},
	)

	outerRef := expressions.InitialOf(sel)
	results := mustFireSimpleSelectRule(t, NewImplementSimpleSelectRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should yield a filter wrapper")
	}
	if _, ok := results[0].(*plans.RecordQueryPredicatesFilterPlan); !ok {
		t.Fatalf("expected *plans.RecordQueryPredicatesFilterPlan, got %T", results[0])
	}
}

func TestImplementSimpleSelectRule_WithProjection(t *testing.T) {
	t.Parallel()
	scan := simpleSelectPlanScan("T")
	sw := scan

	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ForEachQuantifier(innerRef)
	resultValue := simpleSelectField(q, 2)
	sel := simpleSelectExpression(
		resultValue,
		[]expressions.Quantifier{q},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := mustFireSimpleSelectRule(t, NewImplementSimpleSelectRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should yield a map wrapper for non-trivial result")
	}
	if _, ok := results[0].(*plans.RecordQueryMapPlan); !ok {
		t.Fatalf("expected *plans.RecordQueryMapPlan for projection, got %T", results[0])
	}
}

func TestImplementSimpleSelectRule_NilInnerRef(t *testing.T) {
	t.Parallel()
	q := expressions.ForEachQuantifier(nil)
	sel := simpleSelectExpression(
		simpleSelectQOV(q.GetAlias(), simpleSelectRowType()),
		[]expressions.Quantifier{q},
		nil,
	)
	outerRef := expressions.InitialOf(sel)
	results := mustFireSimpleSelectRule(t, NewImplementSimpleSelectRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should yield nothing for nil inner ref, got %d", len(results))
	}
}

func TestImplementSimpleSelectRule_ExistentialQuantifier(t *testing.T) {
	t.Parallel()
	scan := simpleSelectPlanScan("T")
	sw := scan

	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ExistentialQuantifier(innerRef)
	sel := simpleSelectExpression(
		simpleSelectFlowedObject(q),
		[]expressions.Quantifier{q},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := mustFireSimpleSelectRule(t, NewImplementSimpleSelectRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should yield for Existential quantifier")
	}

	found := false
	for _, r := range results {
		// The FirstOrDefault is its own cascades expression now (RFC-184 W2) — no
		// physicalFirstOrDefaultWrapper.
		if _, ok := r.(*plans.RecordQueryFirstOrDefaultPlan); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("Existential quantifier should produce FirstOrDefault plan")
	}
}

func TestImplementSimpleSelectRule_NullOnEmptyQuantifier(t *testing.T) {
	t.Parallel()
	scan := simpleSelectPlanScan("T")
	sw := scan

	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ForEachNullOnEmptyQuantifier(innerRef)
	sel := simpleSelectExpression(
		simpleSelectFlowedObject(q),
		[]expressions.Quantifier{q},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := mustFireSimpleSelectRule(t, NewImplementSimpleSelectRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should yield for nullOnEmpty quantifier")
	}

	found := false
	for _, r := range results {
		if _, ok := r.(*plans.RecordQueryDefaultOnEmptyPlan); ok {
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
	scan := simpleSelectPlanScan("T")
	sw := scan

	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ForEachQuantifier(innerRef)
	tautology := predicates.NewConstantPredicate(predicates.TriTrue)
	sel := simpleSelectExpression(
		simpleSelectFlowedObject(q),
		[]expressions.Quantifier{q},
		[]predicates.QueryPredicate{tautology},
	)

	outerRef := expressions.InitialOf(sel)
	results := mustFireSimpleSelectRule(t, NewImplementSimpleSelectRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should yield when tautology is the only predicate")
	}
	if _, ok := results[0].(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("tautology-only predicate should pass through to scan, got %T", results[0])
	}
}
