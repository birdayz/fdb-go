package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// pkPlanContext provides PK column info for testing distinct
// elimination in the PLANNING phase.
type pkPlanContext struct {
	pk map[string][]string // record type → PK column names
}

func (c *pkPlanContext) GetPlannerConfiguration() PlannerConfiguration {
	return DefaultPlannerConfiguration()
}

func (c *pkPlanContext) GetMatchCandidates() []MatchCandidate { return nil }

func (c *pkPlanContext) GetPrimaryKeyColumns(recordType string) []string {
	if c.pk == nil {
		return nil
	}
	return c.pk[recordType]
}

// makeFakePlanWrapper creates a trivial physical plan that can be
// inserted as a FinalMember of a Reference. Used to simulate what
// the planner's bottom-up implementation phase would produce.
func makeFakePlanWrapper(recType string) *plans.RecordQueryScanPlan {
	return plans.NewRecordQueryScanPlan([]string{recType}, values.UnknownType, false)
}

// buildDistinctOverProjection creates:
//
//	Distinct(Projection([projected...], Scan([recType])))
//
// and returns the Distinct Reference with a physical FinalMember
// in the inner (projection) Reference.
func buildDistinctOverProjection(
	recType string,
	projected []values.Value,
) *expressions.Reference {
	scan := expressions.NewFullUnorderedScanExpression([]string{recType}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	proj := expressions.NewLogicalProjectionExpression(projected, scanQ)
	projRef := expressions.InitialOf(proj)
	projRef.Insert(makeFakePlanWrapper(recType))
	projQ := expressions.ForEachQuantifier(projRef)

	distinct := expressions.NewLogicalDistinctExpression(projQ)
	return expressions.InitialOf(distinct)
}

// buildDistinctOverScan creates:
//
//	Distinct(Scan([recType]))
//
// and returns the Distinct Reference with a physical FinalMember
// in the inner (scan) Reference.
func buildDistinctOverScan(recType string) *expressions.Reference {
	scan := expressions.NewFullUnorderedScanExpression([]string{recType}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanRef.Insert(makeFakePlanWrapper(recType))
	scanQ := expressions.ForEachQuantifier(scanRef)

	distinct := expressions.NewLogicalDistinctExpression(scanQ)
	return expressions.InitialOf(distinct)
}

// TestDistinctFinal_PKProjected_Eliminates verifies DISTINCT elimination
// during PLANNING when the projection includes all PK columns.
func TestDistinctFinal_PKProjected_Eliminates(t *testing.T) {
	t.Parallel()
	distinctRef := buildDistinctOverProjection("USERS", []values.Value{
		&values.FieldValue{Field: "ID", Typ: values.UnknownType},
		&values.FieldValue{Field: "NAME", Typ: values.UnknownType},
	})
	ctx := &pkPlanContext{pk: map[string][]string{"USERS": {"ID"}}}
	results := FireImplementationRuleWithContext(NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("ImplementDistinctFinalRule should fire and eliminate DISTINCT when PK is projected")
	}
	for _, r := range results {
		if _, ok := r.(*physicalDistinctWrapper); ok {
			t.Fatal("expected elimination (no DistinctWrapper), but got DistinctWrapper")
		}
	}
}

// TestDistinctFinal_NonPKProjected_Wraps verifies DISTINCT is kept
// when the projection does NOT include the PK.
func TestDistinctFinal_NonPKProjected_Wraps(t *testing.T) {
	t.Parallel()
	distinctRef := buildDistinctOverProjection("USERS", []values.Value{
		&values.FieldValue{Field: "NAME", Typ: values.UnknownType},
	})
	ctx := &pkPlanContext{pk: map[string][]string{"USERS": {"ID"}}}
	results := FireImplementationRuleWithContext(NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("ImplementDistinctFinalRule should fire")
	}
	foundDistinct := false
	for _, r := range results {
		if _, ok := r.(*physicalDistinctWrapper); ok {
			foundDistinct = true
		}
	}
	if !foundDistinct {
		t.Fatal("expected DistinctWrapper when PK is not projected")
	}
}

// TestDistinctFinal_FullScan_Eliminates verifies DISTINCT elimination
// on a full table scan (no projection). Every column is available,
// so the PK is always covered.
func TestDistinctFinal_FullScan_Eliminates(t *testing.T) {
	t.Parallel()
	distinctRef := buildDistinctOverScan("USERS")
	ctx := &pkPlanContext{pk: map[string][]string{"USERS": {"ID"}}}
	results := FireImplementationRuleWithContext(NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("ImplementDistinctFinalRule should fire on full scan with PK")
	}
	for _, r := range results {
		if _, ok := r.(*physicalDistinctWrapper); ok {
			t.Fatal("expected elimination (no DistinctWrapper), but got DistinctWrapper")
		}
	}
}

// TestDistinctFinal_NoPlanContext_Wraps verifies DISTINCT is kept
// when no PlanContext provides PK info.
func TestDistinctFinal_NoPlanContext_Wraps(t *testing.T) {
	t.Parallel()
	distinctRef := buildDistinctOverScan("USERS")
	results := FireImplementationRule(NewImplementDistinctFinalRule(), distinctRef)
	if len(results) == 0 {
		t.Fatal("ImplementDistinctFinalRule should fire")
	}
	foundDistinct := false
	for _, r := range results {
		if _, ok := r.(*physicalDistinctWrapper); ok {
			foundDistinct = true
		}
	}
	if !foundDistinct {
		t.Fatal("expected DistinctWrapper when no PK info available")
	}
}

// TestDistinctFinal_CompositePK_Eliminates verifies DISTINCT elimination
// when all columns of a composite PK are projected.
func TestDistinctFinal_CompositePK_Eliminates(t *testing.T) {
	t.Parallel()
	distinctRef := buildDistinctOverProjection("ORDER_ITEMS", []values.Value{
		&values.FieldValue{Field: "ORDER_ID", Typ: values.UnknownType},
		&values.FieldValue{Field: "ITEM_ID", Typ: values.UnknownType},
		&values.FieldValue{Field: "QTY", Typ: values.UnknownType},
	})
	ctx := &pkPlanContext{pk: map[string][]string{"ORDER_ITEMS": {"ORDER_ID", "ITEM_ID"}}}
	results := FireImplementationRuleWithContext(NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("ImplementDistinctFinalRule should eliminate when all composite PK cols projected")
	}
	for _, r := range results {
		if _, ok := r.(*physicalDistinctWrapper); ok {
			t.Fatal("expected elimination (no DistinctWrapper), but got DistinctWrapper")
		}
	}
}

// TestDistinctFinal_CompositePKPartial_Wraps verifies DISTINCT is
// kept when only some columns of a composite PK are projected.
func TestDistinctFinal_CompositePKPartial_Wraps(t *testing.T) {
	t.Parallel()
	distinctRef := buildDistinctOverProjection("ORDER_ITEMS", []values.Value{
		&values.FieldValue{Field: "ORDER_ID", Typ: values.UnknownType},
		&values.FieldValue{Field: "QTY", Typ: values.UnknownType},
	})
	ctx := &pkPlanContext{pk: map[string][]string{"ORDER_ITEMS": {"ORDER_ID", "ITEM_ID"}}}
	results := FireImplementationRuleWithContext(NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("ImplementDistinctFinalRule should fire")
	}
	foundDistinct := false
	for _, r := range results {
		if _, ok := r.(*physicalDistinctWrapper); ok {
			foundDistinct = true
		}
	}
	if !foundDistinct {
		t.Fatal("expected DistinctWrapper when composite PK is partial")
	}
}

// TestDistinctFinal_ComputedPKExpr_Wraps verifies that a projection whose only
// reference to the PK is BURIED inside an expression (id/3) does NOT elide the
// DISTINCT: f(pk) is many-to-one, so eliding dedup would emit duplicates.
// Regression for the buried-FieldValue coverage bug (SELECT DISTINCT id/3 over
// ids 1..6 returned 6 rows instead of 3).
func TestDistinctFinal_ComputedPKExpr_Wraps(t *testing.T) {
	t.Parallel()
	// id / 3 — the PK column ID appears only as an ArithmeticValue operand.
	idOver3 := &values.ArithmeticValue{
		Op:    values.OpDiv,
		Left:  &values.FieldValue{Field: "ID", Typ: values.UnknownType},
		Right: &values.ConstantValue{Value: int64(3), Typ: values.UnknownType},
	}
	distinctRef := buildDistinctOverProjection("USERS", []values.Value{idOver3})
	ctx := &pkPlanContext{pk: map[string][]string{"USERS": {"ID"}}}
	results := FireImplementationRuleWithContext(NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("ImplementDistinctFinalRule should fire")
	}
	foundDistinct := false
	for _, r := range results {
		if _, ok := r.(*physicalDistinctWrapper); ok {
			foundDistinct = true
		}
	}
	if !foundDistinct {
		t.Fatal("expected DistinctWrapper: id/3 is not injective over the PK, so DISTINCT must NOT be elided")
	}
}

// TestCollectProjectedFieldNames_BuriedFieldNotCredited is a white-box pin on
// the coverage helper: a bare column reference IS credited; the same column
// buried inside an arithmetic expression is NOT.
func TestCollectProjectedFieldNames_BuriedFieldNotCredited(t *testing.T) {
	t.Parallel()

	bareID := &values.FieldValue{Field: "ID", Typ: values.UnknownType}
	buildProj := func(v values.Value) *expressions.LogicalProjectionExpression {
		scan := expressions.NewFullUnorderedScanExpression([]string{"USERS"}, values.UnknownType)
		scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
		return expressions.NewLogicalProjectionExpression([]values.Value{v}, scanQ)
	}

	// Bare FieldValue → credited.
	if cols := collectProjectedFieldNames(buildProj(bareID)); len(cols) != 1 {
		t.Fatalf("bare id: expected 1 credited column, got %v", cols)
	} else if _, ok := cols["ID"]; !ok {
		t.Fatalf("bare id: expected ID credited, got %v", cols)
	}

	// id / 3 → the buried ID must NOT be credited (empty covering set).
	idOver3 := &values.ArithmeticValue{
		Op:    values.OpDiv,
		Left:  bareID,
		Right: &values.ConstantValue{Value: int64(3), Typ: values.UnknownType},
	}
	if cols := collectProjectedFieldNames(buildProj(idOver3)); len(cols) != 0 {
		t.Fatalf("id/3: expected NO credited columns (buried FieldValue), got %v", cols)
	}
}

// TestDistinctFinal_CaseInsensitive verifies case-insensitive
// matching between PK column names and projected field names.
func TestDistinctFinal_CaseInsensitive(t *testing.T) {
	t.Parallel()
	distinctRef := buildDistinctOverProjection("USERS", []values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType},
	})
	ctx := &pkPlanContext{pk: map[string][]string{"USERS": {"ID"}}}
	results := FireImplementationRuleWithContext(NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("ImplementDistinctFinalRule should fire with case-insensitive PK match")
	}
	for _, r := range results {
		if _, ok := r.(*physicalDistinctWrapper); ok {
			t.Fatal("expected elimination (no DistinctWrapper), but got DistinctWrapper")
		}
	}
}

// TestDistinctFinal_ThroughFilter verifies DISTINCT elimination
// when a filter sits between the projection and the scan.
func TestDistinctFinal_ThroughFilter(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"USERS"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	filter := expressions.NewLogicalFilterExpression(nil, scanQ)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{
			&values.FieldValue{Field: "ID", Typ: values.UnknownType},
		},
		filterQ,
	)
	projRef := expressions.InitialOf(proj)
	projRef.Insert(makeFakePlanWrapper("USERS"))
	projQ := expressions.ForEachQuantifier(projRef)

	distinct := expressions.NewLogicalDistinctExpression(projQ)
	distinctRef := expressions.InitialOf(distinct)

	ctx := &pkPlanContext{pk: map[string][]string{"USERS": {"ID"}}}
	results := FireImplementationRuleWithContext(NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("ImplementDistinctFinalRule should fire through filter")
	}
	for _, r := range results {
		if _, ok := r.(*physicalDistinctWrapper); ok {
			t.Fatal("expected elimination (no DistinctWrapper), but got DistinctWrapper")
		}
	}
}

// TestDistinctFinal_WrapsAllMembers verifies the wrapping path
// yields a DistinctWrapper for EVERY physical member, not just
// the first. Regression test for the early-return bug.
func TestDistinctFinal_WrapsAllMembers(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"ITEMS"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	// Insert TWO physical members to simulate multiple candidates.
	scanRef.Insert(makeFakePlanWrapper("ITEMS"))
	fwd := plans.NewRecordQueryScanPlan([]string{"ITEMS"}, values.UnknownType, false)
	rev := plans.NewRecordQueryScanPlan([]string{"ITEMS"}, values.UnknownType, true)
	scanRef.Insert(fwd)
	scanRef.Insert(rev)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Project a non-PK column so elimination does NOT fire.
	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{
			&values.FieldValue{Field: "NAME", Typ: values.UnknownType},
		},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)
	// Copy members to projRef so the rule has plans to wrap.
	for _, m := range scanRef.Members() {
		projRef.Insert(m)
	}
	projQ := expressions.ForEachQuantifier(projRef)

	distinct := expressions.NewLogicalDistinctExpression(projQ)
	distinctRef := expressions.InitialOf(distinct)

	// PK is "ID" but projection only has "NAME" → no elimination.
	ctx := &pkPlanContext{pk: map[string][]string{"ITEMS": {"ID"}}}
	results := FireImplementationRuleWithContext(NewImplementDistinctFinalRule(), distinctRef, ctx, nil)

	wrapCount := 0
	for _, r := range results {
		if _, ok := r.(*physicalDistinctWrapper); ok {
			wrapCount++
		}
	}
	if wrapCount < 2 {
		t.Fatalf("expected at least 2 DistinctWrappers (one per FinalMember), got %d", wrapCount)
	}
}
