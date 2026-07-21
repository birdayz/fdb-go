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

	kA := []values.Value{values.NewFlatFieldValue("A", values.UnknownType)}
	kB := []values.Value{values.NewFlatFieldValue("B", values.UnknownType)}

	// The InUnion is its own cascades expression over the live scanRef edge now
	// (RFC-184 W2) — no wrapper snapshot.
	planA := plans.NewRecordQueryInUnionPlanFromQuantifier(
		expressions.NewPhysicalQuantifier(scanRef), []string{"b1"}, kA, false, 0)
	planB := plans.NewRecordQueryInUnionPlanFromQuantifier(
		expressions.NewPhysicalQuantifier(scanRef), []string{"b1"}, kB, false, 0)

	m := NewMemo(nil)
	m.RegisterReference(scanRef)

	refA := m.MemoizeExpression(planA)
	refB := m.MemoizeExpression(planB)
	if refA == refB {
		t.Fatal("in-union alternatives differing only in comparison keys must NOT collapse into one memo Reference")
	}

	// Sanity: a re-memoized identical in-union DOES intern into refA.
	planA2 := plans.NewRecordQueryInUnionPlanFromQuantifier(
		expressions.NewPhysicalQuantifier(scanRef), []string{"b1"},
		[]values.Value{values.NewFlatFieldValue("A", values.UnknownType)}, false, 0)
	if m.MemoizeExpression(planA2) != refA {
		t.Fatal("identical in-union alternative must intern into the existing Reference")
	}
}
