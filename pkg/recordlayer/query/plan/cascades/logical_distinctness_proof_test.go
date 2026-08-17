package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func exactDistinctnessFieldType(physicalType values.Type) values.Type {
	if physicalType == nil || physicalType.Code() == values.TypeCodeUnknown {
		// Unknown here is the storage-metadata condition under test, not an
		// unknown flowed row. Keep the row itself exact so its PK address can be
		// resolved while the independently carried physical type remains unknown.
		return values.NullableLong
	}
	return physicalType
}

func typedDistinctnessScan(
	t testing.TB,
	recordTypes []string,
	physicalTypes ...values.Type,
) *plans.RecordQueryScanPlan {
	t.Helper()
	fields := make([]values.Field, len(physicalTypes))
	for i, typ := range physicalTypes {
		fields[i] = values.Field{
			Name:      fmt.Sprintf("PK%d", i),
			FieldType: exactDistinctnessFieldType(typ),
			Ordinal:   i,
		}
	}
	rowType := values.NewRecordType("DistinctnessScanRow", false, fields)
	scan, err := plans.NewRecordQueryScanPlan(recordTypes, rowType, false)
	scan = mustConstruct(t, scan, err)
	pk := make([]values.Value, len(physicalTypes))
	for i := range physicalTypes {
		resolved, resolveErr := values.ResolveFieldOrdinals(scan.GetResultValue(), []int{i})
		pk[i] = mustConstruct(t, resolved, resolveErr)
	}
	return scan.
		WithPrimaryKey(pk).
		WithKeyComponentTypes(physicalTypes)
}

func typedDistinctnessIndex(
	t testing.TB,
	indexTypes []values.Type,
	pkTypes []values.Type,
) *plans.RecordQueryIndexPlan {
	t.Helper()
	fields := make([]values.Field, 0, len(indexTypes)+len(pkTypes))
	indexColumns := make([]string, len(indexTypes))
	for i, typ := range indexTypes {
		indexColumns[i] = fmt.Sprintf("I%d", i)
		fields = append(fields, values.Field{
			Name:      indexColumns[i],
			FieldType: exactDistinctnessFieldType(typ),
			Ordinal:   len(fields),
		})
	}
	pkColumns := make([]string, len(pkTypes))
	for i, typ := range pkTypes {
		pkColumns[i] = fmt.Sprintf("PK%d", i)
		fields = append(fields, values.Field{
			Name:      pkColumns[i],
			FieldType: exactDistinctnessFieldType(typ),
			Ordinal:   len(fields),
		})
	}
	rowType := values.NewRecordType("DistinctnessIndexRow", false, fields)
	index, err := plans.NewRecordQueryIndexPlan("I", nil, []string{"T"}, rowType, false)
	return mustConstruct(t, index, err).
		WithIndexMetadata(indexColumns, pkColumns, false).
		WithKeyComponentTypes(indexTypes).
		WithPrimaryKeyComponentTypes(pkTypes).
		WithDistinctRecordsSignal(false)
}

func TestRecordIdentityMatchesLogicalEquality(t *testing.T) {
	t.Parallel()
	safeScan := typedDistinctnessScan(t, []string{"T"}, values.NullableLong)
	if !expressionRecordIdentityMatchesLogicalEquality(safeScan) {
		t.Fatal("nullable non-floating primary key should be logically congruent")
	}
	for name, plan := range map[string]plans.RecordQueryPlan{
		"FLOAT PK":       typedDistinctnessScan(t, []string{"T"}, values.NotNullFloat),
		"DOUBLE PK":      typedDistinctnessScan(t, []string{"T"}, values.NotNullDouble),
		"unknown PK":     typedDistinctnessScan(t, []string{"T"}, values.UnknownType),
		"multiple types": typedDistinctnessScan(t, []string{"A", "B"}, values.NotNullLong),
	} {
		if planRecordIdentityMatchesLogicalEquality(plan) {
			t.Errorf("%s unexpectedly proved logical row distinctness", name)
		}
	}

	// A floating INDEX key does not affect full-row DISTINCT: the base record's
	// safe PK is present in the row and still separates every record.
	floatIndexSafePK := typedDistinctnessIndex(t,
		[]values.Type{values.NotNullDouble}, []values.Type{values.NotNullLong},
	)
	if !planRecordIdentityMatchesLogicalEquality(floatIndexSafePK) {
		t.Fatal("safe base PK should prove row distinctness through a scalar index")
	}
	if planRecordIdentityMatchesLogicalEquality(typedDistinctnessIndex(t,
		[]values.Type{values.NotNullLong}, []values.Type{values.NotNullDouble},
	)) {
		t.Fatal("raw NaN base-PK variants must not prove row distinctness")
	}

	filter, err := plans.NewRecordQueryFilterPlan(nil, safeScan)
	filter = mustConstruct(t, filter, err)
	if !planRecordIdentityMatchesLogicalEquality(filter) {
		t.Fatal("1:1 filter should preserve the proof")
	}
	pkDistinct, err := plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(safeScan)
	pkDistinct = mustConstruct(t, pkDistinct, err)
	if planRecordIdentityMatchesLogicalEquality(pkDistinct) {
		t.Fatal("physical-PK dedup needs its own logical-value proof")
	}
}

func TestStorageOrderingCompletenessIsStrongerThanRecordIdentity(t *testing.T) {
	t.Parallel()
	safe := typedDistinctnessIndex(t,
		[]values.Type{values.NotNullLong, values.NullableString},
		[]values.Type{values.NotNullLong},
	)
	if !planStorageOrderingIsComplete(safe) {
		t.Fatal("fully authoritative non-floating index storage key should be complete")
	}

	floatIndexSafePK := typedDistinctnessIndex(t,
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
		"floating PK suffix": typedDistinctnessIndex(t,
			[]values.Type{values.NotNullLong}, []values.Type{values.NotNullFloat},
		),
		"unknown index key": typedDistinctnessIndex(t,
			[]values.Type{values.UnknownType}, []values.Type{values.NotNullLong},
		),
		"unknown PK suffix": typedDistinctnessIndex(t,
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
	safe := typedDistinctnessScan(t, []string{"T"}, values.NotNullLong)
	// Live alternatives in one memo reference must expose the same exact row
	// layout. Make the second member unsafe via its multi-type identity domain,
	// which both proofs reject, while keeping its PK address/type identical.
	unsafe := typedDistinctnessScan(t, []string{"T", "U"}, values.NotNullLong)
	childRef := expressions.InitialOf(safe)
	childRef.Insert(unsafe)
	childQ := expressions.ForEachQuantifier(childRef)

	filterFixture, err := plans.NewRecordQueryFilterPlan(nil, safe)
	filterFixture = mustConstruct(t, filterFixture, err)
	filterExpr := mustWithQuantifiers(t, filterFixture,
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
	singletonFilterFixture, err := plans.NewRecordQueryFilterPlan(nil, safe)
	singletonFilterFixture = mustConstruct(t, singletonFilterFixture, err)
	singletonFilterExpr := mustWithQuantifiers(t, singletonFilterFixture,
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

// TestPlanStorageOrderingIsComplete_ReadsTheOrderingNotTheKeyTypes pins WHICH
// question the sort rule's completeness proof asks.
//
// It must ask the ordering the plan actually advertises. Asking the key
// component types instead is a second property that has to agree with the first
// by hand, and on an expression-key index they do not agree: the types are
// ordinary LONGs while the advertised ordering is EMPTY, because the plan cannot
// synthesize ordering Values from physical column names. A "complete" answer
// there licenses a strictly-sorted claim over an ordering that has no
// coordinates at all.
func TestPlanStorageOrderingIsComplete_ReadsTheOrderingNotTheKeyTypes(t *testing.T) {
	t.Parallel()

	row := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "A", FieldType: values.NullableLong, Ordinal: 1},
	})
	build := func() *plans.RecordQueryIndexPlan {
		index, err := plans.NewRecordQueryIndexPlan(
			"IDX_A", []*predicates.ComparisonRange{},
			[]string{"T"}, row, false)
		return mustConstruct(t, index, err).
			WithKeyComponentTypes([]values.Type{values.NullableLong}).
			WithIndexMetadata([]string{"A"}, []string{"ID"}, false).
			WithPrimaryKeyComponentTypes([]values.Type{values.NullableLong})
	}

	if !planStorageOrderingIsComplete(build()) {
		t.Fatal("an index advertising its full key and PK suffix IS storage-complete")
	}
	if planStorageOrderingIsComplete(build().WithOrderingKeyNamesUnavailable()) {
		t.Fatal("an expression-key index advertises NO ordering, so its storage " +
			"ordering cannot be complete; answering from the key component types " +
			"alone says yes and licenses a strict-sort claim over nothing")
	}
}
