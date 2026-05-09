package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// pkPlanContext provides PK column info for testing the
// DistinctOnUniqueElimRule.
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

// TestDistinctOnUniqueElim_PKProjected verifies DISTINCT elimination
// when the projection includes all PK columns.
func TestDistinctOnUniqueElim_PKProjected(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"USERS"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{
			&values.FieldValue{Field: "ID", Typ: values.UnknownType},
			&values.FieldValue{Field: "NAME", Typ: values.UnknownType},
		},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)
	projQ := expressions.ForEachQuantifier(projRef)

	distinct := expressions.NewLogicalDistinctExpression(projQ)
	distinctRef := expressions.InitialOf(distinct)

	ctx := &pkPlanContext{pk: map[string][]string{"USERS": {"ID"}}}
	results := FireExpressionRuleWithMemo(NewDistinctOnUniqueElimRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("DistinctOnUniqueElimRule should fire when PK is projected")
	}

	if _, ok := results[0].(*expressions.LogicalProjectionExpression); !ok {
		t.Fatalf("expected *LogicalProjectionExpression, got %T", results[0])
	}
}

// TestDistinctOnUniqueElim_NonPKProjected verifies DISTINCT is kept
// when the projection does NOT include the PK.
func TestDistinctOnUniqueElim_NonPKProjected(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"USERS"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Project only NAME (not the PK column ID).
	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{
			&values.FieldValue{Field: "NAME", Typ: values.UnknownType},
		},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)
	projQ := expressions.ForEachQuantifier(projRef)

	distinct := expressions.NewLogicalDistinctExpression(projQ)
	distinctRef := expressions.InitialOf(distinct)

	ctx := &pkPlanContext{pk: map[string][]string{"USERS": {"ID"}}}
	results := FireExpressionRuleWithMemo(NewDistinctOnUniqueElimRule(), distinctRef, ctx, nil)
	if len(results) != 0 {
		t.Fatal("DistinctOnUniqueElimRule should NOT fire when PK is not projected")
	}
}

// TestDistinctOnUniqueElim_FullScan verifies DISTINCT elimination
// on a full table scan (no projection). Every column is available,
// so the PK is always covered.
func TestDistinctOnUniqueElim_FullScan(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"USERS"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	distinct := expressions.NewLogicalDistinctExpression(scanQ)
	distinctRef := expressions.InitialOf(distinct)

	ctx := &pkPlanContext{pk: map[string][]string{"USERS": {"ID"}}}
	results := FireExpressionRuleWithMemo(NewDistinctOnUniqueElimRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("DistinctOnUniqueElimRule should fire on full scan with PK")
	}

	if _, ok := results[0].(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("expected *FullUnorderedScanExpression, got %T", results[0])
	}
}

// TestDistinctOnUniqueElim_NoPlanContext verifies DISTINCT is kept
// when no PlanContext is available (empty context, no PK info).
func TestDistinctOnUniqueElim_NoPlanContext(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"USERS"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	distinct := expressions.NewLogicalDistinctExpression(scanQ)
	distinctRef := expressions.InitialOf(distinct)

	// Use empty plan context (no PK info).
	results := FireExpressionRule(NewDistinctOnUniqueElimRule(), distinctRef)
	if len(results) != 0 {
		t.Fatal("DistinctOnUniqueElimRule should NOT fire without PK info")
	}
}

// TestDistinctOnUniqueElim_CompositePK verifies DISTINCT elimination
// when all columns of a composite PK are projected.
func TestDistinctOnUniqueElim_CompositePK(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"ORDER_ITEMS"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{
			&values.FieldValue{Field: "ORDER_ID", Typ: values.UnknownType},
			&values.FieldValue{Field: "ITEM_ID", Typ: values.UnknownType},
			&values.FieldValue{Field: "QTY", Typ: values.UnknownType},
		},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)
	projQ := expressions.ForEachQuantifier(projRef)

	distinct := expressions.NewLogicalDistinctExpression(projQ)
	distinctRef := expressions.InitialOf(distinct)

	ctx := &pkPlanContext{pk: map[string][]string{"ORDER_ITEMS": {"ORDER_ID", "ITEM_ID"}}}
	results := FireExpressionRuleWithMemo(NewDistinctOnUniqueElimRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("DistinctOnUniqueElimRule should fire when all composite PK cols projected")
	}
}

// TestDistinctOnUniqueElim_CompositePKPartial verifies DISTINCT is
// kept when only some columns of a composite PK are projected.
func TestDistinctOnUniqueElim_CompositePKPartial(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"ORDER_ITEMS"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Project only ORDER_ID (missing ITEM_ID from the composite PK).
	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{
			&values.FieldValue{Field: "ORDER_ID", Typ: values.UnknownType},
			&values.FieldValue{Field: "QTY", Typ: values.UnknownType},
		},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)
	projQ := expressions.ForEachQuantifier(projRef)

	distinct := expressions.NewLogicalDistinctExpression(projQ)
	distinctRef := expressions.InitialOf(distinct)

	ctx := &pkPlanContext{pk: map[string][]string{"ORDER_ITEMS": {"ORDER_ID", "ITEM_ID"}}}
	results := FireExpressionRuleWithMemo(NewDistinctOnUniqueElimRule(), distinctRef, ctx, nil)
	if len(results) != 0 {
		t.Fatal("DistinctOnUniqueElimRule should NOT fire when composite PK is partial")
	}
}

// TestDistinctOnUniqueElim_CaseInsensitive verifies case-insensitive
// matching between PK column names and projected field names.
func TestDistinctOnUniqueElim_CaseInsensitive(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"USERS"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Project "id" in lowercase, PK defined as "ID" uppercase.
	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{
			&values.FieldValue{Field: "id", Typ: values.UnknownType},
		},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)
	projQ := expressions.ForEachQuantifier(projRef)

	distinct := expressions.NewLogicalDistinctExpression(projQ)
	distinctRef := expressions.InitialOf(distinct)

	ctx := &pkPlanContext{pk: map[string][]string{"USERS": {"ID"}}}
	results := FireExpressionRuleWithMemo(NewDistinctOnUniqueElimRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("DistinctOnUniqueElimRule should fire with case-insensitive PK match")
	}
}

// TestDistinctOnUniqueElim_ThroughFilter verifies DISTINCT elimination
// when a filter sits between the projection and the scan.
func TestDistinctOnUniqueElim_ThroughFilter(t *testing.T) {
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
	projQ := expressions.ForEachQuantifier(projRef)

	distinct := expressions.NewLogicalDistinctExpression(projQ)
	distinctRef := expressions.InitialOf(distinct)

	ctx := &pkPlanContext{pk: map[string][]string{"USERS": {"ID"}}}
	results := FireExpressionRuleWithMemo(NewDistinctOnUniqueElimRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("DistinctOnUniqueElimRule should fire through filter")
	}
}
