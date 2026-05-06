package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// TestFindPhysicalExpr_ReturnsWrapperFromReference pins the happy path:
// a Reference containing a physicalScanWrapper yields that wrapper.
func TestFindPhysicalExpr_ReturnsWrapperFromReference(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	wrapper := &physicalScanWrapper{plan: scan}
	ref := expressions.InitialOf(wrapper)
	got := findPhysicalExpr(ref)
	if got == nil {
		t.Fatal("findPhysicalExpr = nil, want non-nil")
	}
	if got != wrapper {
		t.Fatalf("findPhysicalExpr returned %p, want %p (same wrapper)", got, wrapper)
	}
}

// TestFindPhysicalExpr_NilReference returns nil on nil input.
func TestFindPhysicalExpr_NilReference(t *testing.T) {
	t.Parallel()
	if got := findPhysicalExpr(nil); got != nil {
		t.Fatalf("findPhysicalExpr(nil) = %v, want nil", got)
	}
}

// TestFindPhysicalExpr_LogicalOnlyReference returns nil when only
// logical expressions are present (no physical wrapper).
func TestFindPhysicalExpr_LogicalOnlyReference(t *testing.T) {
	t.Parallel()
	logical := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	ref := expressions.InitialOf(logical)
	if got := findPhysicalExpr(ref); got != nil {
		t.Fatalf("findPhysicalExpr(logical-only) = %v, want nil", got)
	}
}

// TestFindPhysicalExpr_MixedMembers finds the physical wrapper even
// when a logical expression was inserted first.
func TestFindPhysicalExpr_MixedMembers(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	wrapper := &physicalScanWrapper{plan: scan}
	logical := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	// Build ref with logical first, then insert physical.
	ref := expressions.InitialOf(logical)
	ref.Insert(wrapper)
	got := findPhysicalExpr(ref)
	if got == nil {
		t.Fatal("findPhysicalExpr(mixed) = nil, want non-nil")
	}
	if got != wrapper {
		t.Fatalf("findPhysicalExpr returned %p, want %p", got, wrapper)
	}
}

// TestFindPhysicalPlan_ReturnsUnderlyingPlan pins findPhysicalPlan:
// a Reference containing a physicalScanWrapper yields the scan plan.
func TestFindPhysicalPlan_ReturnsUnderlyingPlan(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	wrapper := &physicalScanWrapper{plan: scan}
	ref := expressions.InitialOf(wrapper)
	got := findPhysicalPlan(ref)
	if got == nil {
		t.Fatal("findPhysicalPlan = nil, want non-nil")
	}
	if got != scan {
		t.Fatalf("findPhysicalPlan returned wrong plan")
	}
}
