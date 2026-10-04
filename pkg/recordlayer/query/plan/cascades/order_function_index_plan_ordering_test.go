package cascades

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"google.golang.org/protobuf/proto"
)

// TestStampIndexMetadata_OrderFunctionColumnOrdersByItsField: a scan of an
// index over (order_desc_nulls_last(A), B) claims an ordering by A descending
// then B ascending. Java simplifies ToOrderedBytesValue(A, DESC_NULLS_LAST) to
// the ordering part (A, DESC_NULLS_LAST); stamping the column names as
// unavailable instead left every consumer above the scan without an order.
func TestStampIndexMetadata_OrderFunctionColumnOrdersByItsField(t *testing.T) {
	t.Parallel()
	rowType := testRecordRowType("T", "A", "B", "ID")
	root := &gen.KeyExpression{Then: &gen.Then{Child: []*gen.KeyExpression{
		{Function: &gen.Function{
			Name:      proto.String(FunctionKindOrderDescNullsLast),
			Arguments: keyExpressionField("A", gen.Field_SCALAR),
		}},
		keyExpressionField("B", gen.Field_SCALAR),
	}}}
	noDuplicates := false
	cand := NewValueIndexScanMatchCandidateWithFunctions(
		"T$A_DESC_B",
		[]string{"T"},
		[]string{"A", "B"},
		[]string{FunctionKindOrderDescNullsLast, ""},
		rfc219Aliases(2),
		rowType,
		false,
		[]string{"ID"},
		&noDuplicates,
	).WithRootKeyExpression(root)

	for _, reverse := range []bool{false, true} {
		plan := stampIndexMetadata(cand, mustRFC219WidthConstruct(plans.NewRecordQueryIndexPlan(
			cand.CandidateName(), nil, cand.GetRecordTypes(), rowType, reverse)))
		got := plan.HintOrdering()
		if !got.IsKnown || len(got.Keys) < 2 {
			t.Fatalf("reverse=%v: HintOrdering = %#v, want [A DESC, B ASC, ...]", reverse, got)
		}
		for i, name := range []string{"A", "B"} {
			fv, ok := values.AsFieldValue(got.Keys[i])
			if !ok || fv.DisplayName() != name {
				t.Fatalf("reverse=%v: key %d = %s, want %s", reverse, i, values.ExplainValue(got.Keys[i]), name)
			}
		}
		if got.DescendingAt(0) == reverse || got.NullsFirstAt(0) != reverse {
			t.Errorf("reverse=%v: A desc=%v nullsFirst=%v, want the DESC NULLS LAST column flipped only by the scan",
				reverse, got.DescendingAt(0), got.NullsFirstAt(0))
		}
		if got.DescendingAt(1) != reverse {
			t.Errorf("reverse=%v: B desc=%v, want the scan direction", reverse, got.DescendingAt(1))
		}
	}
}
