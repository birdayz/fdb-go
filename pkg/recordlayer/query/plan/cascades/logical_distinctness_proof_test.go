package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func typedDistinctnessScan(recordTypes []string, physicalTypes ...values.Type) *plans.RecordQueryScanPlan {
	pk := make([]values.Value, len(physicalTypes))
	for i, typ := range physicalTypes {
		pk[i] = values.NewFieldValueWithResolvedOrdinal("PK", i, typ)
	}
	return plans.NewRecordQueryScanPlan(recordTypes, values.UnknownType, false).
		WithPrimaryKey(pk).
		WithKeyComponentTypes(physicalTypes)
}

func typedDistinctnessIndex(
	indexTypes []values.Type,
	pkTypes []values.Type,
) *plans.RecordQueryIndexPlan {
	indexColumns := make([]string, len(indexTypes))
	for i := range indexColumns {
		indexColumns[i] = "I"
	}
	pkColumns := make([]string, len(pkTypes))
	for i := range pkColumns {
		pkColumns[i] = "PK"
	}
	return plans.NewRecordQueryIndexPlan("I", nil, []string{"T"}, values.UnknownType, false).
		WithIndexMetadata(indexColumns, pkColumns, false).
		WithKeyComponentTypes(indexTypes).
		WithPrimaryKeyComponentTypes(pkTypes).
		WithDistinctRecordsSignal(false)
}

func TestRecordIdentityMatchesLogicalEquality(t *testing.T) {
	t.Parallel()
	safeScan := typedDistinctnessScan([]string{"T"}, values.NullableLong)
	if !expressionRecordIdentityMatchesLogicalEquality(safeScan) {
		t.Fatal("nullable non-floating primary key should be logically congruent")
	}
	for name, plan := range map[string]plans.RecordQueryPlan{
		"FLOAT PK":       typedDistinctnessScan([]string{"T"}, values.NotNullFloat),
		"DOUBLE PK":      typedDistinctnessScan([]string{"T"}, values.NotNullDouble),
		"unknown PK":     typedDistinctnessScan([]string{"T"}, values.UnknownType),
		"multiple types": typedDistinctnessScan([]string{"A", "B"}, values.NotNullLong),
	} {
		if planRecordIdentityMatchesLogicalEquality(plan) {
			t.Errorf("%s unexpectedly proved logical row distinctness", name)
		}
	}

	// A floating INDEX key does not affect full-row DISTINCT: the base record's
	// safe PK is present in the row and still separates every record.
	floatIndexSafePK := typedDistinctnessIndex(
		[]values.Type{values.NotNullDouble}, []values.Type{values.NotNullLong},
	)
	if !planRecordIdentityMatchesLogicalEquality(floatIndexSafePK) {
		t.Fatal("safe base PK should prove row distinctness through a scalar index")
	}
	if planRecordIdentityMatchesLogicalEquality(typedDistinctnessIndex(
		[]values.Type{values.NotNullLong}, []values.Type{values.NotNullDouble},
	)) {
		t.Fatal("raw NaN base-PK variants must not prove row distinctness")
	}

	filter := plans.NewRecordQueryFilterPlan(nil, safeScan)
	if !planRecordIdentityMatchesLogicalEquality(filter) {
		t.Fatal("1:1 filter should preserve the proof")
	}
	if planRecordIdentityMatchesLogicalEquality(
		plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(safeScan),
	) {
		t.Fatal("physical-PK dedup needs its own logical-value proof")
	}
}

func TestStorageOrderingCompletenessIsStrongerThanRecordIdentity(t *testing.T) {
	t.Parallel()
	safe := typedDistinctnessIndex(
		[]values.Type{values.NotNullLong, values.NullableString},
		[]values.Type{values.NotNullLong},
	)
	if !planStorageOrderingIsComplete(safe) {
		t.Fatal("fully authoritative non-floating index storage key should be complete")
	}

	floatIndexSafePK := typedDistinctnessIndex(
		[]values.Type{values.NotNullLong, values.NotNullDouble},
		[]values.Type{values.NotNullLong},
	)
	if !planRecordIdentityMatchesLogicalEquality(floatIndexSafePK) {
		t.Fatal("test setup: base record identity should remain safe")
	}
	if planStorageOrderingIsComplete(floatIndexSafePK) {
		t.Fatal("NaN barrier in index key must prevent strict-prefix proof")
	}

	for name, plan := range map[string]plans.RecordQueryPlan{
		"floating PK suffix": typedDistinctnessIndex(
			[]values.Type{values.NotNullLong}, []values.Type{values.NotNullFloat},
		),
		"unknown index key": typedDistinctnessIndex(
			[]values.Type{values.UnknownType}, []values.Type{values.NotNullLong},
		),
		"unknown PK suffix": typedDistinctnessIndex(
			[]values.Type{values.NotNullLong}, []values.Type{values.UnknownType},
		),
	} {
		if planStorageOrderingIsComplete(plan) {
			t.Errorf("%s unexpectedly proved a complete unique ordering key", name)
		}
	}
}

func TestLogicalDistinctnessProofQuantifiesLiveChildAlternatives(t *testing.T) {
	t.Parallel()
	safe := typedDistinctnessScan([]string{"T"}, values.NotNullLong)
	unsafe := typedDistinctnessScan([]string{"T"}, values.NotNullDouble)
	childRef := expressions.InitialOf(safe)
	childRef.Insert(unsafe)
	childQ := expressions.ForEachQuantifier(childRef)

	filterExpr := plans.NewRecordQueryFilterPlan(nil, safe).WithQuantifiers(
		[]expressions.Quantifier{childQ},
	)
	filter, ok := filterExpr.(*plans.RecordQueryFilterPlan)
	if !ok {
		t.Fatalf("WithQuantifiers returned %T", filterExpr)
	}
	if planRecordIdentityMatchesLogicalEquality(filter) {
		t.Fatal("DISTINCT proof inspected only the safe first child alternative")
	}
	if planStorageOrderingIsComplete(filter) {
		t.Fatal("strict-order proof inspected only the safe first child alternative")
	}

	// A detached/singleton edge remains provable.
	singletonFilterExpr := plans.NewRecordQueryFilterPlan(nil, safe).WithQuantifiers(
		[]expressions.Quantifier{expressions.ForEachQuantifier(expressions.InitialOf(safe))},
	)
	singletonFilter := singletonFilterExpr.(*plans.RecordQueryFilterPlan)
	if !planRecordIdentityMatchesLogicalEquality(singletonFilter) {
		t.Fatal("singleton safe child should prove row distinctness")
	}
	if !planStorageOrderingIsComplete(singletonFilter) {
		t.Fatal("singleton safe child should prove complete storage ordering")
	}
}
