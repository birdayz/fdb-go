package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func projectionMergeRowType() *values.RecordType {
	return values.NewRecordType("ProjectionMergeRow", false, []values.Field{
		{Name: "id", FieldType: values.NotNullLong},
		{Name: "name", FieldType: values.NullableString},
		{Name: "age", FieldType: values.NullableLong},
		{Name: "LEFT_SOURCE", FieldType: values.NullableLong},
		{Name: "RIGHT_SOURCE", FieldType: values.NullableLong},
		{Name: "K_LEFT", FieldType: values.NullableLong},
		{Name: "K_RIGHT", FieldType: values.NullableLong},
	})
}

func mustProjectionMergeConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct projection-merge fixture: " + err.Error())
	}
	return value
}

func projectionMergeScanQ() (*expressions.FullUnorderedScanExpression, expressions.Quantifier) {
	scan := mustProjectionMergeConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, projectionMergeRowType()))
	return scan, expressions.ForEachQuantifier(expressions.InitialOf(scan))
}

func projectionMergeField(q expressions.Quantifier, ordinal int) values.Value {
	root := mustProjectionMergeConstruct(q.RequireFlowedObjectValue())
	return mustProjectionMergeConstruct(values.ResolveFieldOrdinals(root, []int{ordinal}))
}

func projectionMergeProjection(
	projected []values.Value, aliases []string, inner expressions.Quantifier,
) *expressions.LogicalProjectionExpression {
	return mustProjectionMergeConstruct(
		expressions.NewLogicalProjectionExpressionWithAliases(projected, aliases, inner))
}

func fireProjectionMergeRule(
	t testing.TB, projection *expressions.LogicalProjectionExpression,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireExpressionRule(
		NewProjectionMergeRule(), expressions.InitialOf(projection))
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	return result
}

func TestProjectionMergeRule_Fires(t *testing.T) {
	t.Parallel()
	scan, scanQ := projectionMergeScanQ()
	inner := projectionMergeProjection([]values.Value{
		projectionMergeField(scanQ, 0),
		projectionMergeField(scanQ, 1),
		projectionMergeField(scanQ, 2),
	}, nil, scanQ)
	outerQ := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	outer := projectionMergeProjection(
		[]values.Value{projectionMergeField(outerQ, 0)}, nil, outerQ)

	yielded := fireProjectionMergeRule(t, outer)
	if len(yielded) != 1 {
		t.Fatalf("rule yielded %d expressions, want 1", len(yielded))
	}
	flat, ok := yielded[0].(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalProjectionExpression", yielded[0])
	}
	projected := flat.GetProjectedValues()
	if len(projected) != 1 {
		t.Fatalf("flat projected values len=%d, want 1", len(projected))
	}
	fv, ok := values.AsFieldValue(projected[0])
	if !ok || fv.DisplayName() != "id" {
		t.Fatalf("flat projected[0] = %v, want exact FieldValue(id)", projected[0])
	}
	if flat.GetInner().GetRangesOver().Get() != scan {
		t.Fatalf("flat inner = %T, want original scan", flat.GetInner().GetRangesOver().Get())
	}
}

func TestProjectionMergeRule_DeclinesOnNonProjectionInner(t *testing.T) {
	t.Parallel()
	_, q := projectionMergeScanQ()
	projection := projectionMergeProjection(
		[]values.Value{projectionMergeField(q, 0)}, nil, q)
	if yielded := fireProjectionMergeRule(t, projection); len(yielded) != 0 {
		t.Fatalf("rule yielded %d on non-projection inner, want 0", len(yielded))
	}
}

// RFC-232 seals FieldValue construction: an admitted QOV-rooted field over an
// exact record is resolved to an ordinal immediately. The pre-RFC lazy/name-only
// population therefore cannot reach ProjectionMergeRule anymore.
func TestProjectionMergeRule_LazyOuterReadRejectedAtAdmission(t *testing.T) {
	t.Parallel()
	_, q := projectionMergeScanQ()
	root := mustProjectionMergeConstruct(q.RequireFlowedObjectValue())
	resolved := projectionMergeField(q, 0)
	fv, ok := values.AsFieldValue(resolved)
	if !ok || len(fv.Path().Ordinals()) != 1 {
		t.Fatalf("exact field did not carry its resolved ordinal: %v", resolved)
	}
	missing, err := values.ResolveFieldAccess(root, []values.FieldRequest{
		mustProjectionMergeConstruct(values.FieldByName("does_not_exist")),
	})
	if err == nil || missing != nil {
		t.Fatalf("unresolved name-only read = %v, %v; want atomic admission rejection", missing, err)
	}
}

func TestProjectionMergeCensus_CountsTheOrdinalArm(t *testing.T) {
	before := ProjectionMergeCensusSnapshot()
	_, scanQ := projectionMergeScanQ()
	inner := projectionMergeProjection([]values.Value{
		projectionMergeField(scanQ, 0), projectionMergeField(scanQ, 1),
	}, nil, scanQ)
	outerQ := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	outer := projectionMergeProjection(
		[]values.Value{projectionMergeField(outerQ, 0)}, nil, outerQ)
	fireProjectionMergeRule(t, outer)
	after := ProjectionMergeCensusSnapshot()
	if after.BakedSingleAccessor <= before.BakedSingleAccessor {
		t.Fatalf("BakedSingleAccessor did not move (%d -> %d)",
			before.BakedSingleAccessor, after.BakedSingleAccessor)
	}
}

func TestProjectionMergeRule_DuplicateInnerSlotNames_OrdinalPicksTheRightSlot(t *testing.T) {
	t.Parallel()
	_, scanQ := projectionMergeScanQ()
	inner := projectionMergeProjection([]values.Value{
		projectionMergeField(scanQ, 3), projectionMergeField(scanQ, 4),
	}, []string{"K", "K"}, scanQ)
	outerQ := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	outer := projectionMergeProjection(
		[]values.Value{projectionMergeField(outerQ, 1)}, nil, outerQ)

	yielded := fireProjectionMergeRule(t, outer)
	if len(yielded) != 1 {
		t.Fatalf("rule yielded %d on duplicate names, want 1", len(yielded))
	}
	projected := yielded[0].(*expressions.LogicalProjectionExpression).GetProjectedValues()
	fv, ok := values.AsFieldValue(projected[0])
	if !ok || fv.DisplayName() != "RIGHT_SOURCE" {
		t.Fatalf("ordinal 1 composed to %v, want RIGHT_SOURCE", projected[0])
	}
}

func TestProjectionMergeRule_TriplyNested_FlattensInTwoFires(t *testing.T) {
	t.Parallel()
	scan, scanQ := projectionMergeScanQ()
	deep := projectionMergeProjection([]values.Value{
		projectionMergeField(scanQ, 0),
		projectionMergeField(scanQ, 1),
		projectionMergeField(scanQ, 2),
	}, nil, scanQ)
	midQ := expressions.ForEachQuantifier(expressions.InitialOf(deep))
	mid := projectionMergeProjection([]values.Value{
		projectionMergeField(midQ, 0), projectionMergeField(midQ, 1),
	}, nil, midQ)
	topQ := expressions.ForEachQuantifier(expressions.InitialOf(mid))
	top := projectionMergeProjection(
		[]values.Value{projectionMergeField(topQ, 0)}, nil, topQ)

	first := fireProjectionMergeRule(t, top)
	if len(first) != 1 {
		t.Fatalf("first merge yielded %d, want 1", len(first))
	}
	second := fireProjectionMergeRule(t, first[0].(*expressions.LogicalProjectionExpression))
	if len(second) != 1 {
		t.Fatalf("second merge yielded %d, want 1", len(second))
	}
	flat := second[0].(*expressions.LogicalProjectionExpression)
	if flat.GetInner().GetRangesOver().Get() != scan || len(flat.GetProjectedValues()) != 1 {
		t.Fatalf("two fires did not produce one projection over the scan")
	}
}

func TestProjectionMergeRule_PinsOuterEffectiveNames(t *testing.T) {
	t.Parallel()
	_, scanQ := projectionMergeScanQ()
	// The inner aliases both independently-resolved source slots; only the exact
	// output ordinal can distinguish the two reads after the duplicate spelling.
	inner := projectionMergeProjection([]values.Value{
		projectionMergeField(scanQ, 5), projectionMergeField(scanQ, 6),
	}, []string{"AK", "BK"}, scanQ)
	outerQ := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	outer := projectionMergeProjection([]values.Value{
		projectionMergeField(outerQ, 0), projectionMergeField(outerQ, 1),
	}, nil, outerQ)

	yielded := fireProjectionMergeRule(t, outer)
	if len(yielded) != 1 {
		t.Fatalf("rule yielded %d expressions, want 1", len(yielded))
	}
	flat := yielded[0].(*expressions.LogicalProjectionExpression)
	got := flat.GetOutputNames()
	if len(got) != 2 || got[0] != "AK" || got[1] != "BK" {
		t.Fatalf("merged output names = %v, want [AK BK]", got)
	}
}
