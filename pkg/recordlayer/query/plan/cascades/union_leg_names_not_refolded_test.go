package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// A union's legs are aligned once, by the translator, and nothing below it may
// re-derive their names (RFC-242). The implementation rule used to read every
// leg's column names off its physical plan — through a case fold for a
// scan-terminal leg and exactly for a projection leg — and wrap a leg whose
// spelling differed in a rename Map that the physical union then rejected,
// because the legs' flowed types no longer agreed. The shape below is the one
// FuzzPlanner_WithBatchA_NoPanic minimized to (`[]byte{35, 4, 4, 33}`): one leg
// ends in a scan, the other in a per-field identity projection, both over a
// row whose field names are not upper-case. It planned with upper-case names
// only because the fold was a no-op there.

// lowerCaseScanExpr flows a row whose field names are NOT their own upper-case
// spelling. Every other fixture in this package upper-cases, which is exactly
// why the fold went unnoticed.
func lowerCaseScanExpr(t testing.TB) expressions.RelationalExpression {
	t.Helper()
	return mustFullUnorderedScan(t, []string{"T"}, values.NewRecordType("LowerRow", false, []values.Field{
		{Name: "K", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "g", Ordinal: 1, FieldType: values.NullableLong},
		{Name: "x", Ordinal: 2, FieldType: values.NullableLong},
	}))
}

// identityProjection projects every field of inner's row by ordinal, the way
// the planner fuzz fixtures do, so the leg tops out in a projection plan whose
// output names are the inner row's exact field names.
func identityProjection(t testing.TB, inner expressions.RelationalExpression) expressions.RelationalExpression {
	t.Helper()
	q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	flowed, err := q.GetFlowedObjectType()
	if err != nil {
		t.Fatalf("flowed type: %v", err)
	}
	root, err := values.NewQuantifiedObjectValue(q.GetAlias(), flowed)
	if err != nil {
		t.Fatalf("root QOV: %v", err)
	}
	record, ok := flowed.(*values.RecordType)
	if !ok {
		t.Fatalf("flowed type %T is not a record", flowed)
	}
	projected := make([]values.Value, len(record.Fields))
	for i := range record.Fields {
		field, resolveErr := values.ResolveFieldOrdinals(root, []int{i})
		projected[i] = mustConstruct(t, field, resolveErr)
	}
	projection, err := expressions.NewLogicalProjectionExpression(projected, q)
	return mustConstruct(t, projection, err)
}

func TestUnionLegNamesNotRefolded_ScanLegBesideProjectionLeg(t *testing.T) {
	t.Parallel()
	root := union(t,
		typeFilter(t, typeFilter(t, lowerCaseScanExpr(t))),
		typeFilter(t, identityProjection(t, lowerCaseScanExpr(t))),
	)
	plan := physicalPlanOf(t, planAndAssertWinner(t, root))
	switch plan.(type) {
	case *plans.RecordQueryUnionPlan, *plans.RecordQueryUnorderedUnionPlan:
	default:
		t.Fatalf("root plan is %T, want a physical union plan", plan)
	}
	children := plan.GetChildren()
	if len(children) != 2 {
		t.Fatalf("union has %d children, want 2", len(children))
	}
	// The legs entered the union with equal types and must leave it so: no
	// operator between the union and either leg may have restated the leg's
	// field names in another spelling.
	first := children[0].GetResultType()
	for i, child := range children[1:] {
		if !first.Equals(child.GetResultType()) {
			t.Fatalf("leg %d type %s disagrees with leg 0 type %s — a leg's names were re-derived below the translator",
				i+1, child.GetResultType(), first)
		}
	}
	for i, child := range children {
		if _, isMap := child.(*plans.RecordQueryMapPlan); isMap {
			t.Fatalf("leg %d is a rename Map — the rule re-aligned legs the translator already aligned", i)
		}
	}
}

// The mirror image: projection leg first. The old walker read the fold off
// whichever leg was scan-terminal, so the failure was symmetric — but its EXIT
// was not. With the projection leg first the rename targeted the projection's
// exact spelling, so the renamed scan leg's type WAS Equals to leg 0's and the
// union accepted it; the plan then carried a Map whose rename value read its
// input through a correlation nothing at runtime declares, and execution
// failed on the first row. A type-equality assertion alone therefore passes
// with the defect present; the pin on this exit is that no leg is a Map.
func TestUnionLegNamesNotRefolded_ProjectionLegBesideScanLeg(t *testing.T) {
	t.Parallel()
	root := union(t,
		typeFilter(t, identityProjection(t, lowerCaseScanExpr(t))),
		typeFilter(t, typeFilter(t, lowerCaseScanExpr(t))),
	)
	plan := physicalPlanOf(t, planAndAssertWinner(t, root))
	children := plan.GetChildren()
	if len(children) != 2 {
		t.Fatalf("union has %d children, want 2", len(children))
	}
	if !children[0].GetResultType().Equals(children[1].GetResultType()) {
		t.Fatalf("leg types disagree: %s vs %s", children[0].GetResultType(), children[1].GetResultType())
	}
	for i, child := range children {
		if _, isMap := child.(*plans.RecordQueryMapPlan); isMap {
			t.Fatalf("leg %d is a rename Map — the rule re-aligned legs the translator already aligned, "+
				"and on this exit the Map's type matched so only its presence betrays it", i)
		}
	}
}

// Upper-case control: the same shape over the package's usual upper-case row
// always planned, because folding an upper-case name is the identity. It is
// pinned so the two tests above are measured against the shape that never
// failed.
func TestUnionLegNamesNotRefolded_UpperCaseControl(t *testing.T) {
	t.Parallel()
	root := union(t,
		typeFilter(t, typeFilter(t, scanExpr(t))),
		typeFilter(t, identityProjection(t, scanExpr(t))),
	)
	plan := physicalPlanOf(t, planAndAssertWinner(t, root))
	if n := len(plan.GetChildren()); n != 2 {
		t.Fatalf("union has %d children, want 2", n)
	}
}
