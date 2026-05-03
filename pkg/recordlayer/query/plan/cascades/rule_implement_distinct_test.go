package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// TestImplementDistinctRule_FiresAfterScanImplemented pins the
// LogicalDistinctExpression → DistinctPlan implementation chain.
func TestImplementDistinctRule_FiresAfterScanImplemented(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)
	dist := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(innerRef),
	)
	topRef := expressions.InitialOf(dist)

	FireExpressionRule(NewPrimaryScanRule(), innerRef)

	yielded := FireExpressionRule(NewImplementDistinctRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementDistinctRule yielded %d, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*physicalDistinctWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalDistinctWrapper", yielded[0])
	}
	if _, ok := wrap.GetPlan().GetInner().(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("inner = %T, want *RecordQueryScanPlan", wrap.GetPlan().GetInner())
	}
}

// TestImplementDistinctRule_FiresAfterTypeFilterImplemented pins
// the 5-wrapper symmetry: ImplementDistinct also fires when the
// inner is a physical TypeFilter (not just Scan / Filter / Sort).
// Reviewer flagged this asymmetry mid-shift; pinning it as a test.
func TestImplementDistinctRule_FiresAfterTypeFilterImplemented(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	tf := expressions.NewLogicalTypeFilterExpression(
		[]string{"Order"},
		expressions.ForEachQuantifier(scanRef),
	)
	tfRef := expressions.InitialOf(tf)
	dist := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(tfRef),
	)
	topRef := expressions.InitialOf(dist)

	// Implement leaves up: scan, then typefilter.
	FireExpressionRule(NewPrimaryScanRule(), scanRef)
	FireExpressionRule(NewImplementTypeFilterRule(), tfRef)

	yielded := FireExpressionRule(NewImplementDistinctRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementDistinctRule yielded %d, want 1 (Distinct over physical TypeFilter)", len(yielded))
	}
	wrap := yielded[0].(*physicalDistinctWrapper)
	if _, ok := wrap.GetPlan().GetInner().(*plans.RecordQueryTypeFilterPlan); !ok {
		t.Fatalf("inner = %T, want *RecordQueryTypeFilterPlan", wrap.GetPlan().GetInner())
	}
}

// TestImplementDistinctRule_FiresOverPhysicalUnion pins the 7-wrapper
// symmetry fix: Distinct over a physically-implemented Union now
// fires correctly. This is the UNION DISTINCT pattern's lowering —
// LogicalDistinct(LogicalUnion(...)) → Distinct(Union(...)).
//
// Pre-fix, Distinct's inner-type switch lacked physicalUnionWrapper,
// so this shape silently couldn't physically implement.
func TestImplementDistinctRule_FiresOverPhysicalUnion(t *testing.T) {
	t.Parallel()
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	})
	unionRef := expressions.InitialOf(union)
	dist := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(unionRef),
	)
	topRef := expressions.InitialOf(dist)

	FireExpressionRule(NewPrimaryScanRule(), refA)
	FireExpressionRule(NewPrimaryScanRule(), refB)
	FireExpressionRule(NewImplementUnionRule(), unionRef)

	yielded := FireExpressionRule(NewImplementDistinctRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementDistinctRule yielded %d, want 1 (Distinct over physical Union)", len(yielded))
	}
	wrap := yielded[0].(*physicalDistinctWrapper)
	if _, ok := wrap.GetPlan().GetInner().(*plans.RecordQueryUnionPlan); !ok {
		t.Fatalf("inner = %T, want *RecordQueryUnionPlan", wrap.GetPlan().GetInner())
	}
}

// TestImplementDistinctRule_FiresOverPhysicalIntersection tests distinct
// over intersection (common SQL pattern: DISTINCT over INTERSECT).
func TestImplementDistinctRule_FiresOverPhysicalIntersection(t *testing.T) {
	t.Parallel()
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)
	intr := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(refA),
			expressions.ForEachQuantifier(refB),
		},
		nil,
	)
	intrRef := expressions.InitialOf(intr)
	dist := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(intrRef),
	)
	topRef := expressions.InitialOf(dist)

	FireExpressionRule(NewPrimaryScanRule(), refA)
	FireExpressionRule(NewPrimaryScanRule(), refB)
	FireExpressionRule(NewImplementIntersectionRule(), intrRef)

	yielded := FireExpressionRule(NewImplementDistinctRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementDistinctRule yielded %d, want 1", len(yielded))
	}
	wrap := yielded[0].(*physicalDistinctWrapper)
	if _, ok := wrap.GetPlan().GetInner().(*plans.RecordQueryIntersectionPlan); !ok {
		t.Fatalf("inner = %T, want *RecordQueryIntersectionPlan", wrap.GetPlan().GetInner())
	}
}

// TestImplementDistinctRule_NoFireWithoutPhysicalInner pins the
// gate.
func TestImplementDistinctRule_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	dist := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	)
	topRef := expressions.InitialOf(dist)

	yielded := FireExpressionRule(NewImplementDistinctRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementDistinctRule fired without physical inner; yielded %d", len(yielded))
	}
}

// TestImplementTypeFilterRule_FiresAfterScanImplemented pins the
// LogicalTypeFilterExpression → TypeFilterPlan implementation chain.
func TestImplementTypeFilterRule_FiresAfterScanImplemented(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)
	tf := expressions.NewLogicalTypeFilterExpression(
		[]string{"Order"},
		expressions.ForEachQuantifier(innerRef),
	)
	topRef := expressions.InitialOf(tf)

	FireExpressionRule(NewPrimaryScanRule(), innerRef)

	yielded := FireExpressionRule(NewImplementTypeFilterRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementTypeFilterRule yielded %d, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*physicalTypeFilterWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalTypeFilterWrapper", yielded[0])
	}
	plan := wrap.GetPlan()
	rts := plan.GetRecordTypes()
	if len(rts) != 1 || rts[0] != "Order" {
		t.Fatalf("record types = %v, want [Order]", rts)
	}
	if _, ok := plan.GetInner().(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("inner = %T, want *RecordQueryScanPlan", plan.GetInner())
	}
}

// TestImplementTypeFilterRule_NoFireWithoutPhysicalInner pins the
// gate.
func TestImplementTypeFilterRule_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	tf := expressions.NewLogicalTypeFilterExpression(
		[]string{"Order"},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	)
	topRef := expressions.InitialOf(tf)

	yielded := FireExpressionRule(NewImplementTypeFilterRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementTypeFilterRule fired without physical inner; yielded %d", len(yielded))
	}
}
