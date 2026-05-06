package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

func TestImplementProjectionRule_FiresAfterInnerImplemented(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)
	innerQ := expressions.ForEachQuantifier(innerRef)
	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{&values.FieldValue{Field: "PRICE"}},
		innerQ,
	)
	topRef := expressions.InitialOf(proj)

	FireExpressionRule(NewPrimaryScanRule(), innerRef)
	if got := len(innerRef.Members()); got != 2 {
		t.Fatalf("after PrimaryScanRule, innerRef has %d members, want 2", got)
	}

	yielded := FireExpressionRule(NewImplementProjectionRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementProjectionRule yielded %d, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*physicalProjectionWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalProjectionWrapper", yielded[0])
	}
	plan := wrap.GetPlan()
	if plan == nil {
		t.Fatal("wrapper has no plan")
	}
	if _, ok := plan.GetInner().(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("projection plan inner = %T, want *RecordQueryScanPlan", plan.GetInner())
	}
}

func TestImplementProjectionRule_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{&values.FieldValue{Field: "PRICE"}},
		innerQ,
	)
	topRef := expressions.InitialOf(proj)

	yielded := FireExpressionRule(NewImplementProjectionRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementProjectionRule fired without physical inner; yielded %d", len(yielded))
	}
}

func TestImplementValuesRule_Fires(t *testing.T) {
	t.Parallel()
	ve := expressions.NewLogicalValuesExpression([]values.Value{values.LiteralValue(int64(1))})
	ref := expressions.InitialOf(ve)

	yielded := FireExpressionRule(NewImplementValuesRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("ImplementValuesRule yielded %d, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*physicalValuesWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalValuesWrapper", yielded[0])
	}
	plan := wrap.GetPlan()
	if plan == nil {
		t.Fatal("wrapper has no plan")
	}
}

func TestImplementTempTableScanRule_Fires(t *testing.T) {
	t.Parallel()
	scan := expressions.NewTempTableScanExpression(values.NamedCorrelationIdentifier("tt_scan"))
	ref := expressions.InitialOf(scan)

	yielded := FireExpressionRule(NewImplementTempTableScanRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("ImplementTempTableScanRule yielded %d, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*physicalTempTableScanWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalTempTableScanWrapper", yielded[0])
	}
	if wrap.GetRecordQueryPlan() == nil {
		t.Fatal("wrapper has no plan")
	}
}

func TestImplementTempTableInsertRule_FiresAfterInnerImplemented(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)
	innerQ := expressions.ForEachQuantifier(innerRef)
	insert := expressions.NewTempTableInsertExpression(innerQ, values.NamedCorrelationIdentifier("tt_insert"), true)
	topRef := expressions.InitialOf(insert)

	FireExpressionRule(NewPrimaryScanRule(), innerRef)
	if got := len(innerRef.Members()); got != 2 {
		t.Fatalf("after PrimaryScanRule, innerRef has %d members, want 2", got)
	}

	yielded := FireExpressionRule(NewImplementTempTableInsertRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementTempTableInsertRule yielded %d, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*physicalTempTableInsertWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalTempTableInsertWrapper", yielded[0])
	}
	if wrap.GetRecordQueryPlan() == nil {
		t.Fatal("wrapper has no plan")
	}
}

func TestImplementTempTableInsertRule_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	insert := expressions.NewTempTableInsertExpression(innerQ, values.NamedCorrelationIdentifier("tt_insert"), true)
	topRef := expressions.InitialOf(insert)

	yielded := FireExpressionRule(NewImplementTempTableInsertRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementTempTableInsertRule fired without physical inner; yielded %d", len(yielded))
	}
}

func TestImplementRecursiveDfsJoinRule_FiresAfterInnerImplemented(t *testing.T) {
	t.Parallel()
	scanA := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)
	initialQ := expressions.ForEachQuantifier(refA)
	recursiveQ := expressions.ForEachQuantifier(refB)
	recUnion := expressions.NewRecursiveUnionExpression(
		initialQ, recursiveQ,
		values.NamedCorrelationIdentifier("scan_tt"),
		values.NamedCorrelationIdentifier("insert_tt"),
		expressions.TraversalPreorder,
	)
	topRef := expressions.InitialOf(recUnion)

	FireExpressionRule(NewPrimaryScanRule(), refA)
	FireExpressionRule(NewPrimaryScanRule(), refB)
	if got := len(refA.Members()); got != 2 {
		t.Fatalf("after PrimaryScanRule, refA has %d members, want 2", got)
	}
	if got := len(refB.Members()); got != 2 {
		t.Fatalf("after PrimaryScanRule, refB has %d members, want 2", got)
	}

	yielded := FireExpressionRule(NewImplementRecursiveDfsJoinRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementRecursiveDfsJoinRule yielded %d, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*physicalRecursiveDfsJoinWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalRecursiveDfsJoinWrapper", yielded[0])
	}
	if wrap.GetRecordQueryPlan() == nil {
		t.Fatal("wrapper has no plan")
	}
}

func TestImplementRecursiveDfsJoinRule_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()
	scanA := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	initialQ := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	recursiveQ := expressions.ForEachQuantifier(expressions.InitialOf(scanB))
	recUnion := expressions.NewRecursiveUnionExpression(
		initialQ, recursiveQ,
		values.NamedCorrelationIdentifier("scan_tt"),
		values.NamedCorrelationIdentifier("insert_tt"),
		expressions.TraversalPreorder,
	)
	topRef := expressions.InitialOf(recUnion)

	yielded := FireExpressionRule(NewImplementRecursiveDfsJoinRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementRecursiveDfsJoinRule fired without physical inner; yielded %d", len(yielded))
	}
}

func TestImplementRecursiveLevelUnionRule_FiresAfterInnerImplemented(t *testing.T) {
	t.Parallel()
	scanA := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)
	initialQ := expressions.ForEachQuantifier(refA)
	recursiveQ := expressions.ForEachQuantifier(refB)
	recUnion := expressions.NewRecursiveUnionExpression(
		initialQ, recursiveQ,
		values.NamedCorrelationIdentifier("scan_tt"),
		values.NamedCorrelationIdentifier("insert_tt"),
		expressions.TraversalLevel,
	)
	topRef := expressions.InitialOf(recUnion)

	FireExpressionRule(NewPrimaryScanRule(), refA)
	FireExpressionRule(NewPrimaryScanRule(), refB)
	if got := len(refA.Members()); got != 2 {
		t.Fatalf("after PrimaryScanRule, refA has %d members, want 2", got)
	}
	if got := len(refB.Members()); got != 2 {
		t.Fatalf("after PrimaryScanRule, refB has %d members, want 2", got)
	}

	yielded := FireExpressionRule(NewImplementRecursiveLevelUnionRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementRecursiveLevelUnionRule yielded %d, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*physicalRecursiveLevelUnionWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalRecursiveLevelUnionWrapper", yielded[0])
	}
	if wrap.GetRecordQueryPlan() == nil {
		t.Fatal("wrapper has no plan")
	}
}

func TestImplementRecursiveLevelUnionRule_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()
	scanA := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	initialQ := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	recursiveQ := expressions.ForEachQuantifier(expressions.InitialOf(scanB))
	recUnion := expressions.NewRecursiveUnionExpression(
		initialQ, recursiveQ,
		values.NamedCorrelationIdentifier("scan_tt"),
		values.NamedCorrelationIdentifier("insert_tt"),
		expressions.TraversalLevel,
	)
	topRef := expressions.InitialOf(recUnion)

	yielded := FireExpressionRule(NewImplementRecursiveLevelUnionRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementRecursiveLevelUnionRule fired without physical inner; yielded %d", len(yielded))
	}
}
