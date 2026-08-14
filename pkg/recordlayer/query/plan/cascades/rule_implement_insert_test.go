package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func dmlRuleRowType() *values.RecordType {
	return values.NewRecordType("Order", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "qty", FieldType: values.NullableLong},
	})
}

func mustDMLRuleConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct DML-rule fixture: " + err.Error())
	}
	return value
}

func dmlRuleScan() *expressions.FullUnorderedScanExpression {
	return mustDMLRuleConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"Order"}, dmlRuleRowType()))
}

func fireDMLRule(
	t testing.TB, rule ExpressionRule, ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireExpressionRule(rule, ref)
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	return result
}

// TestImplementInsertRule_FiresAfterScanImplemented pins that
// InsertExpression over a scan with a physical-implemented inner
// yields a RecordQueryInsertPlan wrapping the inner ScanPlan.
func TestImplementInsertRule_FiresAfterScanImplemented(t *testing.T) {
	t.Parallel()
	scan := dmlRuleScan()
	innerRef := expressions.InitialOf(scan)
	ins := mustDMLRuleConstruct(expressions.NewInsertExpression(
		expressions.ForEachQuantifier(innerRef),
		"Order",
		dmlRuleRowType(),
	))
	topRef := expressions.InitialOf(ins)

	fireDMLRule(t, NewPrimaryScanRule(), innerRef)

	yielded := fireDMLRule(t, NewImplementInsertRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementInsertRule yielded %d, want 1", len(yielded))
	}
	plan, ok := yielded[0].(*plans.RecordQueryInsertPlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryInsertPlan", yielded[0])
	}
	if got := plan.GetTargetRecordType(); got != "Order" {
		t.Fatalf("target = %q, want %q", got, "Order")
	}
	if _, ok := plan.GetInner().(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("inner = %T, want *RecordQueryScanPlan", plan.GetInner())
	}
}

// TestImplementInsertRule_NoFireWithoutPhysicalInner pins that the
// rule waits if the inner Reference has no physical member.
func TestImplementInsertRule_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()
	scan := dmlRuleScan()
	innerRef := expressions.InitialOf(scan)
	ins := mustDMLRuleConstruct(expressions.NewInsertExpression(
		expressions.ForEachQuantifier(innerRef),
		"Order",
		dmlRuleRowType(),
	))
	topRef := expressions.InitialOf(ins)

	yielded := fireDMLRule(t, NewImplementInsertRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementInsertRule fired without physical inner; yielded %d", len(yielded))
	}
}

// TestImplementDeleteRule_FiresAfterScanImplemented pins the
// DeleteExpression → DeletePlan implementation chain.
func TestImplementDeleteRule_FiresAfterScanImplemented(t *testing.T) {
	t.Parallel()
	scan := dmlRuleScan()
	innerRef := expressions.InitialOf(scan)
	del := mustDMLRuleConstruct(expressions.NewDeleteExpression(
		expressions.ForEachQuantifier(innerRef),
		"Order",
	))
	topRef := expressions.InitialOf(del)

	fireDMLRule(t, NewPrimaryScanRule(), innerRef)

	yielded := fireDMLRule(t, NewImplementDeleteRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementDeleteRule yielded %d, want 1", len(yielded))
	}
	plan, ok := yielded[0].(*plans.RecordQueryDeletePlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryDeletePlan", yielded[0])
	}
	if got := plan.GetTargetRecordType(); got != "Order" {
		t.Fatalf("target = %q, want %q", got, "Order")
	}
	if _, ok := plan.GetInner().(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("inner = %T, want *RecordQueryScanPlan", plan.GetInner())
	}
}

// TestImplementDeleteRule_NoFireWithoutPhysicalInner pins the gate.
func TestImplementDeleteRule_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()
	scan := dmlRuleScan()
	del := mustDMLRuleConstruct(expressions.NewDeleteExpression(
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
		"Order",
	))
	topRef := expressions.InitialOf(del)

	yielded := fireDMLRule(t, NewImplementDeleteRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementDeleteRule fired without physical inner; yielded %d", len(yielded))
	}
}

// TestImplementUpdateRule_FiresAfterScanImplemented pins the
// UpdateExpression → UpdatePlan chain. The transforms list passes
// through unchanged — UpdatePlan applies them per-row at execution.
func TestImplementUpdateRule_FiresAfterScanImplemented(t *testing.T) {
	t.Parallel()
	scan := dmlRuleScan()
	innerRef := expressions.InitialOf(scan)
	transforms := []expressions.UpdateTransform{
		{FieldPath: "qty", NewValue: &values.ConstantValue{Value: int64(0), Typ: values.NotNullLong}},
	}
	upd := mustDMLRuleConstruct(expressions.NewUpdateExpression(
		expressions.ForEachQuantifier(innerRef),
		"Order",
		dmlRuleRowType(),
		transforms,
	))
	topRef := expressions.InitialOf(upd)

	fireDMLRule(t, NewPrimaryScanRule(), innerRef)

	yielded := fireDMLRule(t, NewImplementUpdateRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementUpdateRule yielded %d, want 1", len(yielded))
	}
	plan, ok := yielded[0].(*plans.RecordQueryUpdatePlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryUpdatePlan", yielded[0])
	}
	if got := plan.GetTargetRecordType(); got != "Order" {
		t.Fatalf("target = %q, want %q", got, "Order")
	}
	if got := len(plan.GetTransforms()); got != 1 {
		t.Fatalf("transforms = %d, want 1", got)
	}
	// Java's ImplementUpdateRule interposes a primary-key dedup between the
	// access path and the mutation UNCONDITIONALLY (ImplementUpdateRule.java:
	// 79-80) — unlike ImplementDeleteRule it does not consult
	// DistinctRecordsProperty, so even a plain primary scan (which IS distinct)
	// gets one. A bare Scan directly under the UpdatePlan is the pre-port
	// shape, not a simplification of this one.
	dedup, ok := plan.GetInner().(*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan)
	if !ok {
		t.Fatalf("inner = %T, want *RecordQueryUnorderedPrimaryKeyDistinctPlan", plan.GetInner())
	}
	if _, ok := dedup.GetInner().(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("dedup inner = %T, want *RecordQueryScanPlan", dedup.GetInner())
	}
}

// TestImplementUpdateRule_NoFireWithoutPhysicalInner pins the gate.
func TestImplementUpdateRule_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()
	scan := dmlRuleScan()
	upd := mustDMLRuleConstruct(expressions.NewUpdateExpression(
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
		"Order",
		dmlRuleRowType(),
		nil,
	))
	topRef := expressions.InitialOf(upd)

	yielded := fireDMLRule(t, NewImplementUpdateRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementUpdateRule fired without physical inner; yielded %d", len(yielded))
	}
}
