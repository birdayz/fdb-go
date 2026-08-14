package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustRFC190RecoveryConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct RFC-190 recovery fixture: " + err.Error())
	}
	return value
}

func rfc190RecoveryOuterType() *values.RecordType {
	return values.NewRecordType("RFC190RecoveryOuter", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "OUTER_NAME", FieldType: values.NullableString},
	})
}

func rfc190RecoveryInnerType() *values.RecordType {
	return values.NewRecordType("RFC190RecoveryInner", false, []values.Field{
		{Name: "INNER_ID", FieldType: values.NotNullLong},
		{Name: "INNER_NAME", FieldType: values.NullableString},
		{Name: "INNER_VALUE", FieldType: values.NullableLong},
	})
}

func rfc190RecoveryField(
	alias values.CorrelationIdentifier,
	rowType values.Type,
	ordinal int,
) values.Value {
	root := mustRFC190RecoveryConstruct(values.NewQuantifiedObjectValue(alias, rowType))
	return mustRFC190RecoveryConstruct(values.ResolveFieldOrdinals(root, []int{ordinal}))
}

func rfc190RecoveryFieldOf(child values.Value, ordinal int) values.Value {
	return mustRFC190RecoveryConstruct(values.ResolveFieldOrdinals(child, []int{ordinal}))
}

// Exact RFC-232 fields already carry their owner root. The ordering lens must
// retain that ownership instead of manufacturing a childless field and later
// guessing which join leg owns it.
func TestRFC190FlatMapOrderingLensPreservesExactChildOwnership(t *testing.T) {
	t.Parallel()

	outerAlias := values.NamedCorrelationIdentifier("outer")
	innerAlias := values.NamedCorrelationIdentifier("inner")
	outerID := rfc190RecoveryField(outerAlias, rfc190RecoveryOuterType(), 0)
	alreadyOuter := rfc190RecoveryField(outerAlias, rfc190RecoveryOuterType(), 1)
	innerField := rfc190RecoveryField(innerAlias, rfc190RecoveryInnerType(), 2)
	literal := &values.ConstantValue{Value: "constant", Typ: values.NotNullString}
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: outerID},
		values.RecordConstructorField{Name: "OUTER_NAME", Value: alreadyOuter},
		values.RecordConstructorField{Name: "INNER_VALUE", Value: innerField},
		values.RecordConstructorField{Name: "LITERAL", Value: literal},
	)
	outerPlan := mustRFC190RecoveryConstruct(plans.NewRecordQueryScanPlan(
		[]string{"OUTER"}, rfc190RecoveryOuterType(), false))
	innerPlan := mustRFC190RecoveryConstruct(plans.NewRecordQueryScanPlan(
		[]string{"INNER"}, rfc190RecoveryInnerType(), false))

	inheriting := mustRFC190RecoveryConstruct(plans.NewRecordQueryFlatMapPlan(
		outerPlan, innerPlan, outerAlias, innerAlias, resultValue, true))
	lens := flatMapOrderingResultForChild(inheriting, outerAlias, true)
	lensRecord, ok := lens.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("inherited ordering lens = %T, want RecordConstructorValue", lens)
	}

	qualifiedID, ok := values.AsFieldValue(lensRecord.Fields[0].Value)
	if !ok {
		t.Fatalf("qualified ID = %T, want FieldValue", lensRecord.Fields[0].Value)
	}
	idRoot, ok := values.AsQuantifiedObjectValue(qualifiedID.ChildValue())
	if !ok || idRoot.Correlation() != outerAlias {
		t.Fatalf("qualified ID root = %#v, want QOV(%s)", qualifiedID.ChildValue(), outerAlias.Name())
	}
	originalID, ok := values.AsFieldValue(outerID)
	if !ok || qualifiedID.DisplayName() != originalID.DisplayName() ||
		qualifiedID.Path() == nil || originalID.Path() == nil ||
		len(qualifiedID.Path().Ordinals()) != 1 ||
		qualifiedID.Path().Ordinals()[0] != originalID.Path().Ordinals()[0] {
		t.Fatal("the ordering lens changed the exact outer ID accessor identity")
	}
	if !values.ValuesStructurallyEqual(lensRecord.Fields[1].Value, alreadyOuter) {
		t.Fatal("the lens rewrote an already-qualified outer field")
	}
	if !values.ValuesStructurallyEqual(lensRecord.Fields[2].Value, innerField) {
		t.Fatal("the lens claimed an inner-correlated field for the outer")
	}
	if !values.ValuesStructurallyEqual(lensRecord.Fields[3].Value, literal) {
		t.Fatal("the lens rewrote a non-field result value")
	}
	if got := flatMapOrderingResultForChild(
		inheriting, innerAlias, false,
	); got != resultValue {
		t.Fatal("the inherited-record lens must apply only to the outer child")
	}

	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     outerID,
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessNotDistinct,
		false,
	)
	localAliases := map[values.CorrelationIdentifier]struct{}{
		outerAlias: {},
		innerAlias: {},
	}
	pushedInherited := pushRequestedOrderingToSelectChild(
		requested, lens, outerAlias, localAliases)
	if pushedInherited.IsPreserve() || len(pushedInherited.GetParts()) != 1 {
		t.Fatalf("inherited outer request = %#v, want one concrete part",
			pushedInherited.GetParts())
	}
	pushedRoot := values.GetCorrelatedToOfValue(
		pushedInherited.GetParts()[0].Value)
	if _, ok := pushedRoot[outerAlias]; !ok || len(pushedRoot) != 1 {
		t.Fatalf("inherited outer request correlations = %v, want only %s",
			pushedRoot, outerAlias.Name())
	}

	ordinary := mustRFC190RecoveryConstruct(plans.NewRecordQueryFlatMapPlan(
		outerPlan, innerPlan, outerAlias, innerAlias, resultValue, false))
	ordinaryLens := flatMapOrderingResultForChild(
		ordinary, outerAlias, true)
	if ordinaryLens != resultValue {
		t.Fatal("an ordinary FlatMap unexpectedly acquired inherited outer ownership")
	}
	pushedOrdinary := pushRequestedOrderingToSelectChild(
		requested, ordinaryLens, outerAlias, localAliases)
	if pushedOrdinary.IsPreserve() || len(pushedOrdinary.GetParts()) != 1 ||
		!values.ValuesStructurallyEqual(pushedOrdinary.GetParts()[0].Value, outerID) {
		t.Fatalf("exact ordinary FlatMap outer request = %#v, want the one owner-rooted ID part",
			pushedOrdinary.GetParts())
	}
}

func TestRFC190FlatMapOrderingCrossesPhysicalCarrierToRuntimeAlias(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "V", Ordinal: 1, FieldType: values.NullableLong},
	})
	index := mustRFC190RecoveryConstruct(plans.NewRecordQueryIndexPlan(
		"IDX_V", nil, []string{"T"}, rowType, false,
	)).WithKeyComponentTypes([]values.Type{values.NullableLong}).
		WithIndexMetadata([]string{"V"}, []string{"ID"}, false)
	physical := physicalPlanExpression(index)
	provided := computeWrapperRichOrdering(physical)
	if provided == nil || len(provided.GetKeys()) == 0 {
		t.Fatal("index fixture did not publish a physical ordering")
	}
	runtimeAlias := values.NamedCorrelationIdentifier("T")
	runtimeRow := mustRFC190RecoveryConstruct(values.NewQuantifiedObjectValue(runtimeAlias, rowType))
	pulled, err := pullChildOrderingThroughResult(
		provided, physical, runtimeRow, runtimeAlias)
	if err != nil {
		t.Fatal(err)
	}
	requestedV := rfc190RecoveryField(runtimeAlias, rowType, 1)
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value: requestedV, SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessPreserveDistinctness,
		false,
	)
	if pulled == nil || !pulled.Satisfies(requested) {
		t.Fatalf("pulled ordering = %v, want runtime T.V ordering", pulled)
	}
	keyCorrelations := values.GetCorrelatedToOfValue(pulled.GetKeys()[0])
	if _, ok := keyCorrelations[runtimeAlias]; !ok || len(keyCorrelations) != 1 {
		t.Fatalf("pulled key correlations = %v, want only runtime alias %s",
			keyCorrelations, runtimeAlias)
	}
	physicalResult, ok := values.AsQuantifiedObjectValue(index.GetResultValue())
	if !ok {
		t.Fatal("index result is not an exact QOV")
	}
	if physicalResult.Correlation() != runtimeAlias {
		if _, leaked := keyCorrelations[physicalResult.Correlation()]; leaked {
			t.Fatal("pulled key retained the selected plan's physical result alias")
		}
	}
}

func TestRFC190FlatMapOrderingCrossesNominalJoinResult(t *testing.T) {
	t.Parallel()

	runtimeAlias := values.NamedCorrelationIdentifier("T")
	innerAlias := values.NamedCorrelationIdentifier("I")
	physicalType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "V", Ordinal: 1, FieldType: values.NullableLong},
	})
	logicalType := values.NewRecordType("T", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "V", Ordinal: 1, FieldType: values.NullableLong},
	})
	innerType := values.NewRecordType("I", false, []values.Field{
		{Name: "X", Ordinal: 0, FieldType: values.NullableLong},
	})
	logicalID := rfc190RecoveryField(runtimeAlias, logicalType, 0)
	logicalV := rfc190RecoveryField(runtimeAlias, logicalType, 1)
	innerX := rfc190RecoveryField(innerAlias, innerType, 0)
	resultValue := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: logicalID},
		values.RecordConstructorField{Name: "V", Value: logicalV},
		values.RecordConstructorField{Name: "X", Value: innerX},
	)
	requestedV := rfc190RecoveryField(runtimeAlias, physicalType, 1)
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value: requestedV, SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessPreserveDistinctness,
		false,
	)
	localAliases := map[values.CorrelationIdentifier]struct{}{
		runtimeAlias: {},
		innerAlias:   {},
	}
	pushed := pushRequestedOrderingToSelectChild(
		requested, resultValue, runtimeAlias, localAliases)
	if pushed.IsPreserve() || len(pushed.GetParts()) != 1 ||
		!values.ValuesStructurallyEqual(pushed.GetParts()[0].Value, logicalV) {
		t.Fatalf("nominal join request pushed to %#v, want exact retained T.V",
			pushed.GetParts())
	}

	index := mustRFC190RecoveryConstruct(plans.NewRecordQueryIndexPlan(
		"IDX_V", nil, []string{"T"}, physicalType, false,
	)).WithKeyComponentTypes([]values.Type{values.NullableLong}).
		WithIndexMetadata([]string{"V"}, []string{"ID"}, false)
	physical := physicalPlanExpression(index)
	provided := computeWrapperRichOrdering(physical)
	pulled, err := pullChildOrderingThroughResult(
		provided, physical, resultValue, runtimeAlias)
	if err != nil {
		t.Fatal(err)
	}
	if pulled == nil || !pulled.Satisfies(requested) {
		t.Fatalf("nominal join result ordering = %v, want runtime T.V ordering",
			pulled)
	}

	foreignAlias := values.NamedCorrelationIdentifier("foreign")
	foreignRequested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     rfc190RecoveryField(foreignAlias, physicalType, 1),
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessPreserveDistinctness,
		false,
	)
	if got := pushRequestedOrderingToSelectChild(
		foreignRequested, resultValue, runtimeAlias, localAliases,
	); !got.IsPreserve() {
		t.Fatalf("foreign ordering request pushed into T as %#v", got.GetParts())
	}
	driftedType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "V", Ordinal: 1, FieldType: values.NullableString},
	})
	driftedRequested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     rfc190RecoveryField(runtimeAlias, driftedType, 1),
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessPreserveDistinctness,
		false,
	)
	if got := pushRequestedOrderingToSelectChild(
		driftedRequested, resultValue, runtimeAlias, localAliases,
	); !got.IsPreserve() {
		t.Fatalf("leaf-type drift pushed into T as %#v", got.GetParts())
	}
}

type rfc190PrimaryKeyPlanContext struct {
	indexTestPlanContext
	primaryKeyColumns []string
}

func (c *rfc190PrimaryKeyPlanContext) GetPrimaryKeyColumns(string) []string {
	return c.primaryKeyColumns
}

func TestRFC190OrderedFullScanAlternativesFinalPrimaryScanSafety(t *testing.T) {
	t.Parallel()

	ctx := &rfc190PrimaryKeyPlanContext{
		primaryKeyColumns: []string{"ID"},
	}
	// A REAL flowed type. Sort elision now depends on the physical type of each
	// primary-key coordinate, because a raw FLOAT/DOUBLE key is not in logical
	// order; a stubbed UnknownType would exercise that fail-closed path rather
	// than the reverse-scan recovery this test is named for.
	rowType := values.NewRecordType("T", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	requestedID := rfc190RecoveryField(
		values.NamedCorrelationIdentifier("rfc190_recovery_request"), rowType, 0)
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     requestedID,
			SortOrder: properties.RequestedSortOrderDescending,
		}},
		properties.DistinctnessNotDistinct,
		false,
	)

	t.Run("unbounded_forward_recovers_reverse", func(t *testing.T) {
		t.Parallel()

		forwardBase := mustRFC190RecoveryConstruct(plans.NewRecordQueryScanPlan(
			[]string{"T"}, rowType, false))
		forward := forwardBase.WithPrimaryKey([]values.Value{
			rfc190RecoveryFieldOf(forwardBase.GetResultValue(), 0),
		}).WithKeyComponentTypes([]values.Type{values.NotNullLong})
		ref := expressions.FinalOf(forward)

		alternatives, err := orderedFullScanAlternatives(ref, requested, ctx)
		if err != nil {
			t.Fatalf("ordered full-scan alternatives: %v", err)
		}
		if len(alternatives) != 1 {
			t.Fatalf("ordered alternatives = %d, want one reverse primary scan",
				len(alternatives))
		}
		reverse, ok := alternatives[0].(*plans.RecordQueryScanPlan)
		if !ok {
			t.Fatalf("ordered alternative = %T, want RecordQueryScanPlan",
				alternatives[0])
		}
		if !reverse.IsReverse() {
			t.Fatal("recovered primary scan is not reverse")
		}
		if len(reverse.GetScanComparisons()) != 0 {
			t.Fatal("recovered full scan unexpectedly acquired bounds")
		}
		if len(ref.Members()) != 0 ||
			len(ref.FinalMembers()) != 1 ||
			ref.FinalMembers()[0] != forward {
			t.Fatal("ordered full-scan recovery mutated the finals-only source group")
		}
	})

	t.Run("bounded_scan_declines", func(t *testing.T) {
		t.Parallel()

		comparison := predicates.NewLiteralComparison(
			predicates.ComparisonEquals, int64(7))
		merged := predicates.EmptyComparisonRange().Merge(&comparison)
		if !merged.Ok {
			t.Fatal("failed to construct bounded-scan comparison")
		}
		boundedBase := mustRFC190RecoveryConstruct(plans.NewRecordQueryScanPlan(
			[]string{"T"}, rowType, false))
		bounded := boundedBase.WithPrimaryKey([]values.Value{
			rfc190RecoveryFieldOf(boundedBase.GetResultValue(), 0),
		}).WithScanComparisons([]*predicates.ComparisonRange{merged.Range}).
			WithKeyComponentTypes([]values.Type{values.NotNullLong})
		ref := expressions.FinalOf(bounded)

		alternatives, err := orderedFullScanAlternatives(
			ref, requested, ctx,
		)
		if err != nil {
			t.Fatalf("ordered full-scan alternatives: %v", err)
		}
		if len(alternatives) != 0 {
			t.Fatalf("bounded scan produced %d ordered full-scan alternatives",
				len(alternatives))
		}
		if len(ref.Members()) != 0 ||
			len(ref.FinalMembers()) != 1 ||
			ref.FinalMembers()[0] != bounded {
			t.Fatal("bounded-scan decline mutated the finals-only source group")
		}
	})
}

// A projected EXISTS reaches ImplementSort with its request rooted at the
// Sort's input quantifier, not at either FlatMap runtime binding. The two named
// roots are intentionally not comparable: only the selected FlatMap output
// carrier can authorize crossing that boundary. This is the actual RFC-190.6
// result shape, including the ExistsValue that the simpler two-field ordering
// fixtures do not carry.
func TestRFC190ProjectedExistsSortRecoveryUsesExactOutputCarrier(t *testing.T) {
	t.Parallel()

	outerType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "COL1", Ordinal: 1, FieldType: values.NullableLong},
	})
	innerType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "T1_ID", Ordinal: 1, FieldType: values.NullableLong},
	})
	outerAlias := values.NamedCorrelationIdentifier("T1")
	innerAlias := values.UniqueCorrelationIdentifier()

	build := func(reverse bool, includeSortOnlyColumn bool) *plans.RecordQueryFlatMapPlan {
		outerBase := mustRFC190RecoveryConstruct(plans.NewRecordQueryScanPlan(
			[]string{"T1"}, outerType, reverse))
		outer := outerBase.WithPrimaryKey([]values.Value{
			rfc190RecoveryFieldOf(outerBase.GetResultValue(), 0),
		}).WithKeyComponentTypes([]values.Type{values.NullableLong})
		innerScan := mustRFC190RecoveryConstruct(plans.NewRecordQueryScanPlan(
			[]string{"T2"}, innerType, false))
		inner := mustRFC190RecoveryConstruct(
			plans.NewRecordQueryFirstOrDefaultPlan(innerScan, nil))
		outerQ := expressions.NamedPhysicalQuantifier(
			outerAlias, expressions.FinalOf(outer))
		innerQ := expressions.NamedPhysicalQuantifier(
			innerAlias, expressions.FinalOf(inner))
		outerObject := mustRFC190RecoveryConstruct(outerQ.RequireFlowedObjectValue())
		exists := mustRFC190RecoveryConstruct(values.NewExistsValue(
			innerAlias, inner.GetResultType()))
		fields := []values.RecordConstructorField{
			{Name: "ID", Value: rfc190RecoveryFieldOf(outerObject, 0)},
			{Name: "HAS_T2", Value: exists},
		}
		if includeSortOnlyColumn {
			fields = append(fields, values.RecordConstructorField{
				Name: "COL1", Value: rfc190RecoveryFieldOf(outerObject, 1),
			})
		}
		result := values.NewRecordConstructorValue(fields...)
		return mustRFC190RecoveryConstruct(
			plans.NewRecordQueryFlatMapPlanFromQuantifiers(
				outerQ, innerQ, outerAlias, innerAlias, result, true))
	}
	requestFor := func(
		root values.QuantifiedObjectValue,
		ordinal int,
		direction properties.RequestedSortOrder,
	) *properties.RequestedOrdering {
		return properties.NewRequestedOrdering(
			[]properties.RequestedOrderingPart{{
				Value: rfc190RecoveryFieldOf(root, ordinal), SortOrder: direction,
			}},
			properties.DistinctnessPreserveDistinctness,
			false,
		)
	}
	assertRecovered := func(t *testing.T, reverse bool) {
		t.Helper()
		flatMap := build(reverse, false)
		sortInput := mustRFC190RecoveryConstruct(values.NewQuantifiedObjectValue(
			values.UniqueCorrelationIdentifier(), flatMap.GetResultType()))
		direction := properties.RequestedSortOrderAscending
		if reverse {
			direction = properties.RequestedSortOrderDescending
		}
		original := requestFor(sortInput, 0, direction)
		normalized, admitted, err := requestedOrderingOnExactPlanOutput(
			original, sortInput, flatMap)
		if err != nil {
			t.Fatal(err)
		}
		if !admitted {
			t.Fatal("sort-edge request did not normalize onto the selected FlatMap output")
		}
		layout := mustRFC190RecoveryConstruct(flatMap.ProvidedOutputLayout())
		field, ok := values.AsFieldValue(normalized.GetParts()[0].Value)
		if !ok {
			t.Fatalf("normalized request = %T, want FieldValue", normalized.GetParts()[0].Value)
		}
		root, ok := values.AsQuantifiedObjectValue(field.ChildValue())
		if !ok || root != layout.Carrier() {
			t.Fatalf("normalized request root = %v/%p, want exact carrier %v/%p",
				root, root, layout.Carrier(), layout.Carrier())
		}
		originalField, _ := values.AsFieldValue(original.GetParts()[0].Value)
		originalRoot, _ := values.AsQuantifiedObjectValue(originalField.ChildValue())
		if originalRoot != sortInput {
			t.Fatal("normalization mutated the source request")
		}

		localAliases := map[values.CorrelationIdentifier]struct{}{
			outerAlias: {}, innerAlias: {},
		}
		outerRequest := pushRequestedOrderingToSelectChildThroughOutput(
			normalized, flatMap.GetResultValue(), values.CurrentCorrelation(),
			outerAlias, localAliases)
		if outerRequest.IsPreserve() || len(outerRequest.GetParts()) != 1 {
			t.Fatalf("outer request = %#v, want exact T1.ID", outerRequest.GetParts())
		}
		outerField, _ := values.AsFieldValue(outerRequest.GetParts()[0].Value)
		outerRoot, _ := values.AsQuantifiedObjectValue(outerField.ChildValue())
		if outerRoot == nil || outerRoot.Correlation() != outerAlias {
			t.Fatalf("outer request root = %v, want %s", outerRoot, outerAlias.Name())
		}
		innerRequest := pushRequestedOrderingToSelectChildThroughOutput(
			normalized, flatMap.GetResultValue(), values.CurrentCorrelation(),
			innerAlias, localAliases)
		if !innerRequest.IsPreserve() {
			t.Fatalf("outer-owned ID leaked into existential leg: %#v", innerRequest.GetParts())
		}
		provided := computeWrapperRichOrdering(flatMap)
		if provided == nil || !provided.Satisfies(normalized) {
			t.Fatalf("projected-EXISTS FlatMap ordering = %v, want selected ID %v",
				provided, direction)
		}
	}

	t.Run("ascending_outer_scan_elides_sort", func(t *testing.T) {
		assertRecovered(t, false)
	})
	t.Run("descending_outer_scan_elides_sort", func(t *testing.T) {
		assertRecovered(t, true)
	})

	t.Run("retained_outer_leg_key_elides_sort", func(t *testing.T) {
		flatMap := build(false, false)
		layout := mustRFC190RecoveryConstruct(flatMap.ProvidedOutputLayout())
		outerSource := mustRFC190RecoveryConstruct(values.NewQuantifiedObjectValue(
			outerAlias, outerType))
		sortInput := mustRFC190RecoveryConstruct(values.NewQuantifiedObjectValue(
			values.UniqueCorrelationIdentifier(), flatMap.GetResultType()))
		original := requestFor(
			outerSource, 0, properties.RequestedSortOrderAscending)
		normalized, admitted, err := requestedOrderingOnExactPlanOutput(
			original, sortInput, flatMap)
		if err != nil || !admitted {
			t.Fatalf("retained outer request normalization = %v/%v", admitted, err)
		}
		normalizedField, ok := values.AsFieldValue(normalized.GetParts()[0].Value)
		if !ok {
			t.Fatalf("normalized retained request = %T, want FieldValue", normalized.GetParts()[0].Value)
		}
		normalizedRoot, ok := values.AsQuantifiedObjectValue(normalizedField.ChildValue())
		if !ok || normalizedRoot != layout.Carrier() {
			t.Fatalf("normalized retained root = %v, want exact output carrier %v",
				normalizedRoot, layout.Carrier())
		}
		localAliases := map[values.CorrelationIdentifier]struct{}{
			outerAlias: {}, innerAlias: {},
		}
		outerRequest := pushRequestedOrderingToSelectChildThroughOutput(
			normalized, flatMap.GetResultValue(), values.CurrentCorrelation(),
			outerAlias, localAliases)
		if outerRequest.IsPreserve() {
			t.Fatal("retained outer request did not push into the outer child")
		}
		provided := computeWrapperRichOrdering(flatMap)
		if provided == nil || !provided.Satisfies(normalized) {
			t.Fatalf("FlatMap ordering = %v, want retained outer window ordering", provided)
		}
		originalField, _ := values.AsFieldValue(original.GetParts()[0].Value)
		originalRoot, _ := values.AsQuantifiedObjectValue(originalField.ChildValue())
		if originalRoot != outerSource {
			t.Fatal("retained source request was mutated")
		}

		foreignAlias := values.NamedCorrelationIdentifier("FOREIGN_T1")
		foreignSource := mustRFC190RecoveryConstruct(values.NewQuantifiedObjectValue(
			foreignAlias, outerType))
		if _, foreignAdmitted, foreignErr := requestedOrderingOnExactPlanOutput(
			requestFor(foreignSource, 0, properties.RequestedSortOrderAscending),
			sortInput, flatMap,
		); foreignErr != nil || foreignAdmitted {
			t.Fatalf("same-shaped foreign retained leg admission = %v/%v, want clean decline",
				foreignAdmitted, foreignErr)
		}
	})

	t.Run("unindexed_retained_column_still_needs_sort", func(t *testing.T) {
		flatMap := build(false, true)
		sortInput := mustRFC190RecoveryConstruct(values.NewQuantifiedObjectValue(
			values.UniqueCorrelationIdentifier(), flatMap.GetResultType()))
		request := requestFor(sortInput, 2, properties.RequestedSortOrderDescending)
		normalized, admitted, err := requestedOrderingOnExactPlanOutput(
			request, sortInput, flatMap)
		if err != nil || !admitted {
			t.Fatalf("sort-only retained column normalization = %v/%v", admitted, err)
		}
		if computeWrapperRichOrdering(flatMap).Satisfies(normalized) {
			t.Fatal("outer ID ordering falsely satisfied unindexed COL1 DESC")
		}
	})

	t.Run("foreign_named_root_declines", func(t *testing.T) {
		flatMap := build(false, false)
		declaration := mustRFC190RecoveryConstruct(values.NewQuantifiedObjectValue(
			values.UniqueCorrelationIdentifier(), flatMap.GetResultType()))
		foreign := mustRFC190RecoveryConstruct(values.NewQuantifiedObjectValue(
			values.UniqueCorrelationIdentifier(), flatMap.GetResultType()))
		if _, admitted, err := requestedOrderingOnExactPlanOutput(
			requestFor(foreign, 0, properties.RequestedSortOrderAscending),
			declaration, flatMap,
		); err != nil || admitted {
			t.Fatalf("foreign request admission = %v/%v, want clean decline", admitted, err)
		}
	})

	t.Run("different_exact_type_is_loud", func(t *testing.T) {
		flatMap := build(false, false)
		drifted := values.NewRecordType("", false, []values.Field{
			{Name: "ID", Ordinal: 0, FieldType: values.NullableString},
			{Name: "HAS_T2", Ordinal: 1, FieldType: values.NotNullBoolean},
		})
		declaration := mustRFC190RecoveryConstruct(values.NewQuantifiedObjectValue(
			values.UniqueCorrelationIdentifier(), drifted))
		if _, admitted, err := requestedOrderingOnExactPlanOutput(
			requestFor(declaration, 0, properties.RequestedSortOrderAscending),
			declaration, flatMap,
		); err == nil || admitted {
			t.Fatalf("type-drift admission = %v/%v, want loud rejection", admitted, err)
		}
	})

	t.Run("existential_output_path_is_not_outer_owned", func(t *testing.T) {
		flatMap := build(false, false)
		declaration := mustRFC190RecoveryConstruct(values.NewQuantifiedObjectValue(
			values.UniqueCorrelationIdentifier(), flatMap.GetResultType()))
		normalized, admitted, err := requestedOrderingOnExactPlanOutput(
			requestFor(declaration, 1, properties.RequestedSortOrderAscending),
			declaration, flatMap)
		if err != nil || !admitted {
			t.Fatalf("EXISTS output request normalization = %v/%v", admitted, err)
		}
		outerRequest := pushRequestedOrderingToSelectChildThroughOutput(
			normalized, flatMap.GetResultValue(), values.CurrentCorrelation(),
			outerAlias, map[values.CorrelationIdentifier]struct{}{
				outerAlias: {}, innerAlias: {},
			})
		if !outerRequest.IsPreserve() {
			t.Fatalf("EXISTS output path was claimed by outer leg: %#v", outerRequest.GetParts())
		}
	})

	t.Run("independent_current_handle_declines", func(t *testing.T) {
		flatMap := build(false, false)
		declaration := mustRFC190RecoveryConstruct(values.NewQuantifiedObjectValue(
			values.UniqueCorrelationIdentifier(), flatMap.GetResultType()))
		foreignLayout := mustRFC190RecoveryConstruct(values.NewOrdinalLayoutForCarrierType(
			flatMap.GetResultType(), []values.OrdinalTileSpec{{
				Start: 0, Width: 2, Kind: values.OrdinalTileFlat,
			}}, nil))
		if _, admitted, err := requestedOrderingOnExactPlanOutput(
			requestFor(foreignLayout.Carrier(), 0, properties.RequestedSortOrderAscending),
			declaration, flatMap,
		); err != nil || admitted {
			t.Fatalf("foreign current admission = %v/%v, want clean decline", admitted, err)
		}
	})
}
