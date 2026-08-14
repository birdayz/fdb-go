package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestMemo_InUnionDifferentComparisonKeys_NoCollapse pins RFC-180 B1 at the
// memo level: two physical in-union alternatives over the SAME child that
// differ ONLY in their merge comparison keys are DIFFERENT equivalence-class
// members — under the pre-fix count-only identity they interned into one
// Reference, and the surviving member did not produce the ordering the
// winner's property claimed.
func TestMemo_InUnionDifferentComparisonKeys_NoCollapse(t *testing.T) {
	t.Parallel()
	rowType := values.NewRecordType("T", false, []values.Field{
		{Name: "A", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "B", FieldType: values.NotNullLong, Ordinal: 1},
	})
	scan := mustFullUnorderedScan(t, []string{"T"}, rowType)
	scanRef := expressions.InitialOf(scan)
	innerAlias := values.NamedCorrelationIdentifier("in_union_inner")

	quantifierA := expressions.NamedPhysicalQuantifier(innerAlias, scanRef)
	rootA, rootAErr := quantifierA.RequireFlowedObjectValue()
	rootA = mustConstruct(t, rootA, rootAErr)
	keyA, keyAErr := values.ResolveFieldOrdinals(rootA, []int{0})
	keyA = mustConstruct(t, keyA, keyAErr)
	quantifierB := expressions.NamedPhysicalQuantifier(innerAlias, scanRef)
	rootB, rootBErr := quantifierB.RequireFlowedObjectValue()
	rootB = mustConstruct(t, rootB, rootBErr)
	keyB, keyBErr := values.ResolveFieldOrdinals(rootB, []int{1})
	keyB = mustConstruct(t, keyB, keyBErr)
	kA := []values.Value{keyA}
	kB := []values.Value{keyB}

	// The InUnion is its own cascades expression over the live scanRef edge now
	// (RFC-184 W2) — no wrapper snapshot.
	planA, planAErr := plans.NewRecordQueryInUnionPlanFromQuantifier(
		quantifierA, []string{"b1"}, kA, false, 0)
	planA = mustConstruct(t, planA, planAErr)
	planB, planBErr := plans.NewRecordQueryInUnionPlanFromQuantifier(
		quantifierB, []string{"b1"}, kB, false, 0)
	planB = mustConstruct(t, planB, planBErr)

	m := NewMemo(nil)
	m.RegisterReference(scanRef)

	refA := m.MemoizeExpression(planA)
	refB := m.MemoizeExpression(planB)
	if refA == refB {
		t.Fatal("in-union alternatives differing only in comparison keys must NOT collapse into one memo Reference")
	}

	// Sanity: a re-memoized identical in-union DOES intern into refA.
	quantifierA2 := expressions.NamedPhysicalQuantifier(innerAlias, scanRef)
	rootA2, rootA2Err := quantifierA2.RequireFlowedObjectValue()
	rootA2 = mustConstruct(t, rootA2, rootA2Err)
	keyA2, keyA2Err := values.ResolveFieldOrdinals(rootA2, []int{0})
	keyA2 = mustConstruct(t, keyA2, keyA2Err)
	planA2, planA2Err := plans.NewRecordQueryInUnionPlanFromQuantifier(
		quantifierA2, []string{"b1"},
		[]values.Value{keyA2}, false, 0)
	planA2 = mustConstruct(t, planA2, planA2Err)
	if m.MemoizeExpression(planA2) != refA {
		t.Fatal("identical in-union alternative must intern into the existing Reference")
	}
}
