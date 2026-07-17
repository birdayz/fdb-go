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
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	scanRef := expressions.InitialOf(scan)

	inner := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	kA := []values.Value{values.NewFlatFieldValue("A", values.UnknownType)}
	kB := []values.Value{values.NewFlatFieldValue("B", values.UnknownType)}

	planA := plans.NewRecordQueryInUnionPlan(inner, []string{"b1"}, kA, false)
	planB := plans.NewRecordQueryInUnionPlan(inner, []string{"b1"}, kB, false)

	wrapA := NewPhysicalInUnionWrapper(planA, expressions.NewPhysicalQuantifier(scanRef))
	wrapB := NewPhysicalInUnionWrapper(planB, expressions.NewPhysicalQuantifier(scanRef))

	m := NewMemo(nil)
	m.RegisterReference(scanRef)

	refA := m.MemoizeExpression(wrapA)
	refB := m.MemoizeExpression(wrapB)
	if refA == refB {
		t.Fatal("in-union alternatives differing only in comparison keys must NOT collapse into one memo Reference")
	}

	// Sanity: a re-memoized identical wrapper DOES intern into refA.
	planA2 := plans.NewRecordQueryInUnionPlan(inner, []string{"b1"},
		[]values.Value{values.NewFlatFieldValue("A", values.UnknownType)}, false)
	wrapA2 := NewPhysicalInUnionWrapper(planA2, expressions.NewPhysicalQuantifier(scanRef))
	if m.MemoizeExpression(wrapA2) != refA {
		t.Fatal("identical in-union alternative must intern into the existing Reference")
	}
}
