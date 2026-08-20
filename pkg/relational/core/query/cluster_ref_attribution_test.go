package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

func TestClusterBakeAttributesOnlyTheExactOwner(t *testing.T) {
	t.Parallel()

	tr := newGateTranslator(t)
	pu := tr.buildClusterPullUp(inner(scan("Order", "o"), scan("Customer", "c")))
	if pu == nil {
		t.Fatal("build cluster pull-up")
	}
	orderLeg, ok := pu.legByBinding["O"]
	if !ok {
		t.Fatal("fixture has no O leg")
	}
	// The leg's flowed type names its slots from the DESCRIPTOR, so the lookup
	// is spelled the descriptor's way; FieldIndexUnique is exact.
	orderID, ok := orderLeg.typ.FieldIndexUnique("order_id")
	if !ok {
		t.Fatal("fixture O leg has no order_id")
	}
	direct := exactTestField(t, exactTestQOV(t, "o", orderLeg.typ), orderID)

	baked := pu.bake(direct)
	field := exactTestFieldView(t, baked)
	owner, ok := values.AsQuantifiedObjectValue(field.ChildValue())
	if !ok || owner.Correlation() != pu.outerCorr {
		t.Fatalf("baked owner = %T, want exact cluster output QOV %s", field.ChildValue(), pu.outerCorr)
	}
	want := orderLeg.start + orderID
	if got := field.Path().Ordinals(); len(got) != 1 || got[0] != want {
		t.Fatalf("baked O.ORDER_ID path = %v, want [%d]", got, want)
	}

	// A literal dotted column on a different exact owner has no attribution to
	// O; display text cannot manufacture a correlation.
	literalType := &values.RecordType{Fields: []values.Field{
		{Name: "O.ORDER_ID", Ordinal: 0, FieldType: values.NotNullLong},
	}}
	literal := exactTestField(t, exactTestQOV(t, "INNER", literalType), 0)
	if got := pu.bake(literal); got != literal || pu.missed {
		t.Fatalf("literal dotted field was treated as an outer leg: got %T missed=%v", got, pu.missed)
	}

	// Mutation control: the correlation still identifies O, but a root type
	// outside O's exact window must be declined instead of mis-ordinalized.
	foreignType := &values.RecordType{Fields: []values.Field{
		{Name: "NOT_ORDER_ID", Ordinal: 0, FieldType: values.NotNullLong},
	}}
	foreign := exactTestField(t, exactTestQOV(t, "o", foreignType), 0)
	pu.missed = false
	if got := pu.bake(foreign); got != foreign || !pu.missed {
		t.Fatalf("out-of-window O field was not declined: got %T missed=%v", got, pu.missed)
	}
}

func TestCollectClusterOuterRefsUsesExactCorrelationNotDisplayName(t *testing.T) {
	t.Parallel()

	outer := map[string]struct{}{"C": {}, "E": {}}
	skip := map[string]struct{}{"SQ": {}}
	literalType := &values.RecordType{Fields: []values.Field{
		{Name: "C.CUSTOMER_ID", Ordinal: 0, FieldType: values.NotNullLong},
	}}
	literal := exactTestField(t, exactTestQOV(t, "SQ", literalType), 0)
	literalOp := logical.NewFilterWithPredicate(scan("Order", "SQ"),
		predicates.NewComparisonPredicate(literal, predicates.Comparison{
			Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1)},
		}), "")
	refs, exhaustive := collectClusterOuterRefs(literalOp, outer, skip)
	if !exhaustive || len(refs) != 0 {
		t.Fatalf("literal dotted name attributed as outer ref: refs=%v exhaustive=%v", refs, exhaustive)
	}

	outerField := exactTestNamedField(t, "c", "CUSTOMER_ID", values.NotNullLong)
	outerOp := logical.NewFilterWithPredicate(scan("Order", "SQ"),
		predicates.NewComparisonPredicate(outerField, predicates.Comparison{
			Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1)},
		}), "")
	refs, exhaustive = collectClusterOuterRefs(outerOp, outer, skip)
	if !exhaustive {
		t.Fatal("exact filter carrier was not exhaustively walked")
	}
	if _, hit := refs["C"]; !hit || len(refs) != 1 {
		t.Fatalf("exact QOV(C) reference attribution = %v, want only C", refs)
	}
}

func TestLegRefReadsTheExactOwner(t *testing.T) {
	t.Parallel()

	literalType := &values.RecordType{Fields: []values.Field{
		{Name: "A.ID", Ordinal: 0, FieldType: values.NotNullLong},
	}}
	merged := exactTestField(t, exactTestQOV(t, "S", literalType), 0)
	if key, ok := legRef(merged); !ok || key != "S" {
		t.Fatalf("legRef(exact QOV(S).A.ID) = (%q,%v), want (S,true)", key, ok)
	}
	direct := exactTestNamedField(t, "A", "ID", values.NotNullLong)
	if key, ok := legRef(direct); !ok || key != "A" {
		t.Fatalf("legRef(exact QOV(A).ID) = (%q,%v), want (A,true)", key, ok)
	}
	if key, ok := legRef(&values.ConstantValue{Value: int64(1)}); ok || key != "" {
		t.Fatalf("legRef(non-field) = (%q,%v), want empty false", key, ok)
	}
}
