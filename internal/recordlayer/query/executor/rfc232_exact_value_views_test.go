package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// These embedded views deliberately satisfy the sealed interfaces through
// method promotion. They are not values-owned concrete nodes, so every
// executor shape probe must reject them. This makes the tests sensitive to a
// regression from an exact recognizer back to a broad interface assertion.
type rfc232EmbeddedQOV struct {
	values.QuantifiedObjectValue
}

type rfc232EmbeddedFieldValue struct {
	values.FieldValue
}

type rfc232PinnedPathImpostor struct {
	values.FieldPathView
}

func (rfc232PinnedPathImpostor) IsFrontierPinned() bool { return true }

type rfc232PinnedFieldValueImpostor struct {
	values.FieldValue
}

func (f *rfc232PinnedFieldValueImpostor) Path() values.FieldPathView {
	return rfc232PinnedPathImpostor{FieldPathView: f.FieldValue.Path()}
}

func mustRFC232ExecutorConstruct[T any](t testing.TB, value T, err error) T {
	t.Helper()
	if err != nil {
		t.Fatalf("construct exact executor fixture: %v", err)
	}
	return value
}

func rfc232ExecutorRowType(names ...string) *values.RecordType {
	fields := make([]values.Field, len(names))
	for i, name := range names {
		fields[i] = values.Field{Name: name, FieldType: values.NotNullLong, Ordinal: i}
	}
	return &values.RecordType{Fields: fields}
}

func mustRFC232ExecutorQOV(
	t testing.TB,
	alias values.CorrelationIdentifier,
	typ values.Type,
) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(alias, typ)
	return mustRFC232ExecutorConstruct(t, qov, err)
}

func mustRFC232ExecutorField(
	t testing.TB,
	child values.Value,
	ordinals ...int,
) values.FieldValue {
	t.Helper()
	resolved, err := values.ResolveFieldOrdinals(child, ordinals)
	resolved = mustRFC232ExecutorConstruct(t, resolved, err)
	field, ok := values.AsFieldValue(resolved)
	if !ok {
		t.Fatalf("resolved fixture %T was not an exact FieldValue", resolved)
	}
	return field
}

func TestRFC232FlatMapIdentityUsesExactQOVRecognition(t *testing.T) {
	t.Parallel()

	outerAlias := values.NamedCorrelationIdentifier("outer")
	innerAlias := values.NamedCorrelationIdentifier("inner")
	outer := mustRFC232ExecutorQOV(t, outerAlias, rfc232ExecutorRowType("ID"))
	inner := mustRFC232ExecutorQOV(t, innerAlias, rfc232ExecutorRowType("ID"))

	if !isIdentityOuterRV(outer, outerAlias) {
		t.Fatal("exact outer QOV was not recognized as the outer identity")
	}
	if !isIdentityInnerRV(inner, innerAlias) {
		t.Fatal("exact inner QOV was not recognized as the inner identity")
	}
	if isIdentityOuterRV(inner, outerAlias) || isIdentityInnerRV(outer, innerAlias) {
		t.Fatal("a QOV for the other leg was accepted as an identity")
	}

	foreign := &rfc232EmbeddedQOV{QuantifiedObjectValue: outer}
	if isIdentityOuterRV(foreign, outerAlias) || isIdentityInnerRV(foreign, outerAlias) {
		t.Fatal("an embedded foreign QOV view was accepted as an exact identity")
	}
}

func TestRFC232AggregateFlatFrontierUnwrapUsesExactQOVRecognition(t *testing.T) {
	t.Parallel()

	rowType := rfc232ExecutorRowType("ID")
	outerPlan, err := plans.NewRecordQueryScanPlan(nil, rowType, false)
	outerPlan = mustRFC232ExecutorConstruct(t, outerPlan, err)
	innerPlan, err := plans.NewRecordQueryScanPlan(nil, rowType, false)
	innerPlan = mustRFC232ExecutorConstruct(t, innerPlan, err)
	outerAlias := values.NamedCorrelationIdentifier("outer")
	innerAlias := values.NamedCorrelationIdentifier("inner")
	outer := mustRFC232ExecutorQOV(t, outerAlias, rowType)

	exact, err := plans.NewRecordQueryFlatMapPlan(
		outerPlan, innerPlan, outerAlias, innerAlias, outer, false,
	)
	exact = mustRFC232ExecutorConstruct(t, exact, err)
	if !aggregateInputIsFlatFrontier(exact) {
		t.Fatal("an exact outer-identity FlatMap did not unwrap to its flat scan")
	}

	foreign := &rfc232EmbeddedQOV{QuantifiedObjectValue: outer}
	impostor, err := plans.NewRecordQueryFlatMapPlan(
		outerPlan, innerPlan, outerAlias, innerAlias, foreign, false,
	)
	impostor = mustRFC232ExecutorConstruct(t, impostor, err)
	if aggregateInputIsFlatFrontier(impostor) {
		t.Fatal("a FlatMap carrying an embedded foreign QOV was unwrapped as an exact identity")
	}
}

func TestRFC232BakedLegOperandUsesExactFieldAndQOVViews(t *testing.T) {
	t.Parallel()

	outerAlias := values.NamedCorrelationIdentifier("outer")
	innerAlias := values.NamedCorrelationIdentifier("inner")
	outerQOV := mustRFC232ExecutorQOV(t, outerAlias, rfc232ExecutorRowType("ID", "V"))
	innerQOV := mustRFC232ExecutorQOV(t, innerAlias, rfc232ExecutorRowType("ID", "V"))
	outerField := mustRFC232ExecutorField(t, outerQOV, 0)
	innerField := mustRFC232ExecutorField(t, innerQOV, 1)

	if isOuter, ok := bakedLegOperand(outerField, outerAlias, innerAlias); !ok || !isOuter {
		t.Fatalf("outer field classification = (isOuter=%t, ok=%t), want (true, true)", isOuter, ok)
	}
	if isOuter, ok := bakedLegOperand(innerField, outerAlias, innerAlias); !ok || isOuter {
		t.Fatalf("inner field classification = (isOuter=%t, ok=%t), want (false, true)", isOuter, ok)
	}

	foreign := &rfc232EmbeddedFieldValue{FieldValue: outerField}
	if _, ok := bakedLegOperand(foreign, outerAlias, innerAlias); ok {
		t.Fatal("an embedded foreign FieldValue was admitted as a baked leg operand")
	}

	nestedType := &values.RecordType{Fields: []values.Field{{
		Name:      "NESTED",
		FieldType: rfc232ExecutorRowType("ID"),
		Ordinal:   0,
	}}}
	nestedQOV := mustRFC232ExecutorQOV(t, outerAlias, nestedType)
	fused := mustRFC232ExecutorField(t, nestedQOV, 0, 0)
	if _, ok := bakedLegOperand(fused, outerAlias, innerAlias); ok {
		t.Fatal("a multi-accessor FieldValue was admitted as a leg-local operand")
	}
}

func TestRFC232BakedOrdinalProbeRejectsPinnedFieldViewImpostor(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("input")
	qov := mustRFC232ExecutorQOV(t, alias, rfc232ExecutorRowType("ID"))
	field := mustRFC232ExecutorField(t, qov, 0)
	if valueReadsBakedOrdinal(field) {
		t.Fatal("an ordinary resolved field invented a machinery-owned frontier pin")
	}

	foreign := &rfc232PinnedFieldValueImpostor{FieldValue: field}
	if !foreign.Path().IsFrontierPinned() {
		t.Fatal("test fixture does not advertise the hostile frontier pin")
	}
	if valueReadsBakedOrdinal(foreign) {
		t.Fatal("an embedded foreign FieldValue forged a baked-ordinal contract")
	}
}
