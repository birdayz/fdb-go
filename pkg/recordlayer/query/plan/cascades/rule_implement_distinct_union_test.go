package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestImplementDistinctUnionRule_MatchesLogicalDistinct(t *testing.T) {
	t.Parallel()
	rule := NewImplementDistinctUnionRule()
	scanRef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, nil))
	distinct := expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(scanRef))
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), distinct)
	if len(bindings) == 0 {
		t.Fatal("should match LogicalDistinctExpression")
	}
}

func TestImplementDistinctUnionRule_SkipsNonDistinct(t *testing.T) {
	t.Parallel()
	rule := NewImplementDistinctUnionRule()
	scanRef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, nil))
	filter := expressions.NewLogicalFilterExpression(nil, expressions.ForEachQuantifier(scanRef))
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), filter)
	if len(bindings) != 0 {
		t.Fatal("should NOT match LogicalFilterExpression")
	}
}

func TestImplementDistinctUnionRule_RequiresUnionChild(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := scan

	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	distinct := expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(innerRef))
	outerRef := expressions.InitialOf(distinct)

	results := FireImplementationRule(NewImplementDistinctUnionRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should not fire when child is not a Union, got %d", len(results))
	}
}

func makeScanWithPK(recordType string, pkCols ...string) (*plans.RecordQueryScanPlan, *expressions.Reference) {
	pkVals := make([]values.Value, len(pkCols))
	keyTypes := make([]values.Type, len(pkCols))
	for i, col := range pkCols {
		pkVals[i] = &values.FieldValue{Field: col, Typ: values.UnknownType}
		keyTypes[i] = values.NullableLong
	}
	scan := plans.NewRecordQueryScanPlan([]string{recordType}, values.UnknownType, false).
		WithKeyComponentTypes(keyTypes).
		WithPrimaryKey(pkVals)
	ref := expressions.InitialOf(scan)
	pm := NewPlanPropertiesMap()
	pm.Add(scan)
	ref.SetPlanProperties(pm)
	return scan, ref
}

func TestImplementDistinctUnionRule_FiresWithPKAndStoredRecord(t *testing.T) {
	t.Parallel()
	_, refA := makeScanWithPK("T", "id")
	_, refB := makeScanWithPK("T", "id")

	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	})
	unionRef := expressions.InitialOf(union)

	distinct := expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(unionRef))
	outerRef := expressions.InitialOf(distinct)

	results := FireImplementationRule(NewImplementDistinctUnionRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should fire when union legs have PK and stored records")
	}

	found := false
	for _, r := range results {
		if _, ok := r.(*plans.RecordQueryMergeSortUnionPlan); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("should yield *plans.RecordQueryMergeSortUnionPlan")
	}
}

func TestImplementDistinctUnionRule_NoFireWithoutPK(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := scan
	refA := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	refA.SetPlanProperties(pm)

	scan2 := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw2 := scan2
	refB := expressions.InitialOf(sw2)
	pm2 := NewPlanPropertiesMap()
	pm2.Add(sw2)
	refB.SetPlanProperties(pm2)

	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	})
	unionRef := expressions.InitialOf(union)

	distinct := expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(unionRef))
	outerRef := expressions.InitialOf(distinct)

	results := FireImplementationRule(NewImplementDistinctUnionRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should not fire without PK, got %d", len(results))
	}
}

func TestImplementDistinctUnionRule_IncompatiblePK(t *testing.T) {
	t.Parallel()
	_, refA := makeScanWithPK("T", "id")
	_, refB := makeScanWithPK("T", "name")

	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	})
	unionRef := expressions.InitialOf(union)

	distinct := expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(unionRef))
	outerRef := expressions.InitialOf(distinct)

	results := FireImplementationRule(NewImplementDistinctUnionRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should not fire with incompatible PKs, got %d", len(results))
	}
}

func TestGetCommonPK_AllSame(t *testing.T) {
	t.Parallel()
	pk := []values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}}
	p1 := &PlanPartition{
		partitionProps: properties.PropertyMap{properties.PropPrimaryKey: pk},
	}
	p2 := &PlanPartition{
		partitionProps: properties.PropertyMap{properties.PropPrimaryKey: pk},
	}
	result := getCommonPK([]*PlanPartition{p1, p2})
	if result == nil {
		t.Fatal("same PK should return non-nil")
	}
}

func TestGetCommonPK_OneMissing(t *testing.T) {
	t.Parallel()
	pk := []values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}}
	p1 := &PlanPartition{
		partitionProps: properties.PropertyMap{properties.PropPrimaryKey: pk},
	}
	p2 := &PlanPartition{
		partitionProps: properties.PropertyMap{properties.PropPrimaryKey: nil},
	}
	result := getCommonPK([]*PlanPartition{p1, p2})
	if result != nil {
		t.Fatal("missing PK should return nil")
	}
}

func TestRemoveCommonEqualityBoundParts_NoCommon(t *testing.T) {
	t.Parallel()
	keyA := &values.FieldValue{Field: "a"}
	keyB := &values.FieldValue{Field: "b"}
	o1 := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{keyA: {properties.FixedBinding(nil)}},
		[]values.Value{keyA}, false,
	)
	o2 := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{keyB: {properties.FixedBinding(nil)}},
		[]values.Value{keyB}, false,
	)
	result := removeCommonEqualityBoundParts([]*properties.RichOrdering{o1, o2})
	if len(result) != 2 {
		t.Fatalf("expected 2 orderings, got %d", len(result))
	}
	if len(result[0].GetKeys()) != 1 || len(result[1].GetKeys()) != 1 {
		t.Fatal("no keys should be removed")
	}
}

func TestRemoveCommonEqualityBoundParts_CommonRemoved(t *testing.T) {
	t.Parallel()
	keyA := &values.FieldValue{Field: "a"}
	keyB := &values.FieldValue{Field: "b"}
	o1 := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			keyA: {properties.FixedBinding(nil)},
			keyB: {properties.SortedBinding(properties.ProvidedSortOrderAscending)},
		},
		[]values.Value{keyA, keyB}, false,
	)
	o2 := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			keyA: {properties.FixedBinding(nil)},
			keyB: {properties.SortedBinding(properties.ProvidedSortOrderDescending)},
		},
		[]values.Value{keyA, keyB}, false,
	)
	result := removeCommonEqualityBoundParts([]*properties.RichOrdering{o1, o2})
	if len(result) != 2 {
		t.Fatalf("expected 2 orderings, got %d", len(result))
	}
	if len(result[0].GetKeys()) != 1 {
		t.Fatalf("expected 1 key after removal, got %d", len(result[0].GetKeys()))
	}
	if values.ExplainValue(result[0].GetKeys()[0]) != "b" {
		t.Fatalf("expected key 'b', got %q", values.ExplainValue(result[0].GetKeys()[0]))
	}
}

func TestRemoveCommonEqualityBoundParts_SingleOrdering(t *testing.T) {
	t.Parallel()
	keyA := &values.FieldValue{Field: "a"}
	o := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{keyA: {properties.FixedBinding(nil)}},
		[]values.Value{keyA}, false,
	)
	result := removeCommonEqualityBoundParts([]*properties.RichOrdering{o})
	if len(result) != 1 || len(result[0].GetKeys()) != 1 {
		t.Fatal("single ordering should not be modified")
	}
}

// TestImplementDistinctUnionRule_LyingDelegatorLegPinned pins RFC-181 P0.3:
// a leg whose member is an order-PRESERVING wrapper derives its ordering
// from its SOURCE GROUP, but its BAKED plan child was frozen when the
// wrapper's own rule fired — possibly before ordered variants entered the
// group. The old shape trusted the group estimate and baked the wrapper's
// stale plan: comparison keys describing an order the executed leg does not
// produce → merge-front dedup misses → duplicates through UNION (distinct).
// The leg must be spine-PINNED: the yielded merge union's executable child
// for that leg is the ORDERED member's plan, never the stale one.
func TestImplementDistinctUnionRule_LyingDelegatorLegPinned(t *testing.T) {
	t.Parallel()

	// Leg A: plain pk-ordered scan.
	_, refA := makeScanWithPK("T", "id")

	// Leg B: a filter wrapper (delegator) whose SOURCE group holds a
	// pk-ordered scan, but whose BAKED plan child is a DIFFERENT (stale)
	// scan object — the estimate/executable divergence.
	pkVals := []values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}}
	orderedScan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithKeyComponentTypes([]values.Type{values.NullableLong}).
		WithPrimaryKey(pkVals)
	orderedSW := orderedScan
	srcRef := expressions.InitialOf(orderedSW)
	pmSrc := NewPlanPropertiesMap()
	pmSrc.Add(orderedSW)
	srcRef.SetPlanProperties(pmSrc)

	staleScan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithKeyComponentTypes([]values.Type{values.NullableLong}).
		WithPrimaryKey(pkVals)
	filterWrap := plans.NewRecordQueryPredicatesFilterPlan(
		staleScan,
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
	).WithQuantifiers([]expressions.Quantifier{expressions.ForEachQuantifier(srcRef)}).(*plans.RecordQueryPredicatesFilterPlan)
	refB := expressions.InitialOf(filterWrap)
	pmB := NewPlanPropertiesMap()
	pmB.Add(filterWrap)
	refB.SetPlanProperties(pmB)

	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	})
	unionRef := expressions.InitialOf(union)
	distinct := expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(unionRef))
	outerRef := expressions.InitialOf(distinct)

	results := FireImplementationRule(NewImplementDistinctUnionRule(), outerRef)
	var msu *plans.RecordQueryMergeSortUnionPlan
	for _, r := range results {
		if w, ok := r.(*plans.RecordQueryMergeSortUnionPlan); ok {
			msu = w
			break
		}
	}
	if msu == nil {
		t.Fatal("expected a merge-sort union yield (the pinned path must not over-decline this shape)")
	}
	for _, child := range msu.GetChildren() {
		if child == staleScan {
			t.Fatal("the union baked the delegator's STALE plan child — the leg was not spine-pinned and the merge dedup runs over an order the leg does not produce")
		}
		if fp, ok := child.(*plans.RecordQueryPredicatesFilterPlan); ok && fp.GetInner() == staleScan {
			t.Fatal("the union baked the filter over its STALE child — pinOrderedSpine must relink the executable plan to the ordered member")
		}
	}
}

func distinctUnionCompositeRowType(recordType string) *values.RecordType {
	return values.NewRecordType(recordType, false, []values.Field{
		{Name: "A", FieldType: values.NullableLong},
		{Name: "B", FieldType: values.NullableLong},
	})
}

func distinctUnionCompositeScanRef(
	recordType string,
	flowedType values.Type,
) *expressions.Reference {
	pk := []values.Value{
		values.NewFlatFieldValue("A", values.NullableLong),
		values.NewFlatFieldValue("B", values.NullableLong),
	}
	scan := plans.NewRecordQueryScanPlan([]string{recordType}, flowedType, false).
		WithPrimaryKey(pk).
		WithKeyComponentTypes([]values.Type{values.NullableLong, values.NullableLong})
	ref := expressions.FinalOf(scan)
	computeRefPlanProperties(ref)
	return ref
}

func distinctUnionOverLegs(legs ...*expressions.Reference) *expressions.Reference {
	quantifiers := make([]expressions.Quantifier, len(legs))
	for i, leg := range legs {
		quantifiers[i] = expressions.ForEachQuantifier(leg)
	}
	union := expressions.NewLogicalUnionExpression(quantifiers)
	distinct := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(expressions.InitialOf(union)),
	)
	return expressions.InitialOf(distinct)
}

// TestImplementDistinctUnionRule_UnresolvedFreePrimaryKeySuffixDeclines pins
// the nil contract of bakeMergeComparisonKeys on the real rule path. The
// requested A prefix is addressable at the record boundary, but Limit erases
// the flowed RecordType and the unrequested/free B tiebreak remains lazy. The
// merge front also deduplicates by its comparison tuple, so treating nil as an
// empty key would collapse every row in both legs.
func TestImplementDistinctUnionRule_UnresolvedFreePrimaryKeySuffixDeclines(t *testing.T) {
	t.Parallel()

	limitLeg := func() *expressions.Reference {
		scanRef := distinctUnionCompositeScanRef("T", values.UnknownType)
		limit := plans.NewRecordQueryLimitPlanFromQuantifier(
			expressions.ForEachQuantifier(scanRef), 100, 0, nil,
		)
		limitRef := expressions.FinalOf(limit)
		computeRefPlanProperties(limitRef)
		return limitRef
	}
	distinctRef := distinctUnionOverLegs(limitLeg(), limitLeg())
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     values.NewFlatFieldValue("A", values.NullableLong),
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessPreserveDistinctness,
		false,
	)
	constraints := NewConstraintMap()
	Set(
		constraints,
		distinctRef,
		RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{requested},
	)

	for _, result := range FireImplementationRule(
		NewImplementDistinctUnionRule(), distinctRef, constraints,
	) {
		if merge, ok := result.(*plans.RecordQueryMergeSortUnionPlan); ok {
			t.Fatalf(
				"unresolved free PK suffix yielded merge-sort DISTINCT with keys %#v",
				merge.GetComparisonKeys(),
			)
		}
	}
}

// TestBakeMergeComparisonKeys_ResolvableFreePrimaryKeySuffix is the positive
// twin of the rule-level refusal above: when the row type authoritatively maps
// A and B to ordinals, the same proper-prefix request retains and bakes B as
// the full dedup tiebreak. The existing full-record rule positive control then
// proves that a sound candidate is still yielded end-to-end.
func TestBakeMergeComparisonKeys_ResolvableFreePrimaryKeySuffix(t *testing.T) {
	t.Parallel()

	rowType := distinctUnionCompositeRowType("T")
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     values.NewFlatFieldValue("A", values.NullableLong),
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessPreserveDistinctness,
		false,
	)
	keys := bakeMergeComparisonKeys(
		[]values.Value{
			values.NewFlatFieldValue("A", values.NullableLong),
			values.NewFlatFieldValue("B", values.NullableLong),
		},
		requested,
		rowType,
	)
	if len(keys) != 2 {
		t.Fatalf("comparison keys = %#v, want baked (A,B)", keys)
	}
	for i, key := range keys {
		field, ok := key.(*values.FieldValue)
		if !ok || field.Resolved == nil {
			t.Fatalf("comparison key %d = %#v, want resolved FieldValue", i, key)
		}
	}
}

func distinctUnionProjectedLeg(constant int64) *expressions.Reference {
	rowType := values.NewRecordType("T", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong},
		{Name: "V", FieldType: values.NullableLong},
	})
	// The resolved ordinal is load-bearing for the regression: the old plan's
	// merge key ID#0 evaluated successfully against the projected row and
	// silently over-collapsed it; this is not an unresolved-value loud failure.
	id := values.NewFieldValueWithResolvedOrdinal("ID", 0, values.NullableLong)
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, rowType, false).
		WithPrimaryKey([]values.Value{id}).
		WithKeyComponentTypes([]values.Type{values.NullableLong})
	scanRef := expressions.FinalOf(scan)
	computeRefPlanProperties(scanRef)
	projection := plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{id, values.LiteralValue(constant)},
		[]string{"ID", "V"},
		expressions.ForEachQuantifier(scanRef),
	)
	projectionRef := expressions.FinalOf(projection)
	computeRefPlanProperties(projectionRef)
	return projectionRef
}

// TestImplementDistinctUnionRule_RejectsPrimaryKeyThroughReshapingProjection
// distinguishes record identity from SQL row identity. Both projections carry
// StoredRecord+PrimaryKey from their scan, and both are ordered by the valid
// resolved key ID#0, but (ID,1) and (ID,2) are distinct output rows. A PK-front
// merge would consume them together and emit only one.
func TestImplementDistinctUnionRule_RejectsPrimaryKeyThroughReshapingProjection(t *testing.T) {
	t.Parallel()

	left := distinctUnionProjectedLeg(1)
	right := distinctUnionProjectedLeg(2)
	for _, ref := range []*expressions.Reference{left, right} {
		partitions := ToPlanPartitions(ref)
		if len(partitions) == 0 || !partitions[0].IsStoredRecord() || !partitions[0].HasPrimaryKey() {
			t.Fatal("fixture must carry StoredRecord and PrimaryKey through Projection")
		}
	}

	distinctRef := distinctUnionOverLegs(left, right)
	for _, result := range FireImplementationRule(
		NewImplementDistinctUnionRule(), distinctRef,
	) {
		if merge, ok := result.(*plans.RecordQueryMergeSortUnionPlan); ok {
			t.Fatalf(
				"reshaped rows with the same PK yielded merge-sort DISTINCT keys %#v",
				merge.GetComparisonKeys(),
			)
		}
	}
}

// TestImplementDistinctUnionRule_FetchDoesNotRestoreProjectedRows pins the Go
// executor contract (which differs from the Java plan model): Fetch currently
// executes its child unchanged because index scans already return record
// payloads. It therefore cannot turn (ID,constant) back into the full stored
// row, and must not make a row-shaping projection eligible for PK dedup.
func TestImplementDistinctUnionRule_FetchDoesNotRestoreProjectedRows(t *testing.T) {
	t.Parallel()

	fetchLeg := func(constant int64) *expressions.Reference {
		projectionRef := distinctUnionProjectedLeg(constant)
		fetch := plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
			expressions.ForEachQuantifier(projectionRef),
			nil,
			values.UnknownType,
			plans.FetchIndexRecordsPrimaryKey,
		)
		fetchRef := expressions.FinalOf(fetch)
		computeRefPlanProperties(fetchRef)
		return fetchRef
	}

	distinctRef := distinctUnionOverLegs(fetchLeg(1), fetchLeg(2))
	for _, result := range FireImplementationRule(
		NewImplementDistinctUnionRule(), distinctRef,
	) {
		if merge, ok := result.(*plans.RecordQueryMergeSortUnionPlan); ok {
			t.Fatalf(
				"Fetch-wrapped projected rows yielded merge-sort DISTINCT keys %#v",
				merge.GetComparisonKeys(),
			)
		}
	}
}

// TestMergeDistinctStoredRecordIdentity_RejectsPlannerIdentityRowWrappers
// protects against treating planner-level identity as runtime row identity.
// Projection and Map both allocate a one-slot PositionalRow containing the
// evaluated input row; neither is byte-for-byte/pass-through at execution.
func TestMergeDistinctStoredRecordIdentity_RejectsPlannerIdentityRowWrappers(t *testing.T) {
	t.Parallel()

	pk := []values.Value{values.NewFlatFieldValue("ID", values.NullableLong)}
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithPrimaryKey(pk).
		WithKeyComponentTypes([]values.Type{values.NullableLong})
	scanRef := expressions.FinalOf(scan)
	computeRefPlanProperties(scanRef)

	projectionQ := expressions.ForEachQuantifier(scanRef)
	projection := plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{values.NewQuantifiedObjectValue(projectionQ.GetAlias())},
		nil,
		projectionQ,
	)
	if !projection.IsIdentity() {
		t.Fatal("fixture must be planner-classified as an identity projection")
	}
	if recordType, ok := mergeDistinctStoredRecordIdentity(projection, pk); ok {
		t.Fatalf("identity Projection unexpectedly proved stored-row identity %q", recordType)
	}

	mapQ := expressions.ForEachQuantifier(scanRef)
	mapPlan := plans.NewRecordQueryMapPlanFromQuantifier(
		mapQ,
		values.NewQuantifiedObjectValue(mapQ.GetAlias()),
	)
	if recordType, ok := mergeDistinctStoredRecordIdentity(mapPlan, pk); ok {
		t.Fatalf("QOV-identity Map unexpectedly proved stored-row identity %q", recordType)
	}
}

// TestImplementDistinctUnionRule_RejectsSameVisiblePrimaryKeyAcrossRecordTypes
// pins the second half of the output-identity proof: primary keys are scoped to
// a record type. T/1 and U/1 are different stored records even when both scan
// plans expose the same structural Field(ID) property.
func TestImplementDistinctUnionRule_RejectsSameVisiblePrimaryKeyAcrossRecordTypes(t *testing.T) {
	t.Parallel()

	_, left := makeScanWithPK("T", "id")
	_, right := makeScanWithPK("U", "id")
	distinctRef := distinctUnionOverLegs(left, right)
	for _, result := range FireImplementationRule(
		NewImplementDistinctUnionRule(), distinctRef,
	) {
		if merge, ok := result.(*plans.RecordQueryMergeSortUnionPlan); ok {
			t.Fatalf(
				"different record types with the same visible PK yielded merge keys %#v",
				merge.GetComparisonKeys(),
			)
		}
	}
}

func distinctUnionIndexLeg(
	distinctSignal *bool,
) (*plans.RecordQueryIndexPlan, *expressions.Reference) {
	rowType := values.NewRecordType("T", false, []values.Field{
		{Name: "TAG", FieldType: values.NullableString},
		{Name: "ID", FieldType: values.NullableLong},
	})
	pk := []values.Value{values.NewFlatFieldValue("ID", values.NullableLong)}
	index := plans.NewRecordQueryIndexPlan(
		"IDX_TAG", nil, []string{"T"}, rowType, false,
	).
		WithIndexMetadata([]string{"TAG"}, []string{"ID"}, false).
		WithKeyComponentTypes([]values.Type{values.NullableString}).
		WithPrimaryKeyComponentTypes([]values.Type{values.NullableLong}).
		WithCommonPrimaryKey(pk)
	if distinctSignal != nil {
		index = index.WithDistinctRecordsSignal(*distinctSignal)
	}
	ref := expressions.FinalOf(index)
	computeRefPlanProperties(ref)
	return index, ref
}

// TestImplementDistinctUnionRule_RequiresEachLegToBeInternallyDistinct pins a
// merge-cursor constraint that is easy to miss: it can advance all DIFFERENT
// legs whose current heads tie, but it cannot see a second equal row from one
// leg until the next round. An index with no candidate distinctness proof may
// repeat a base record (fan-out is the production example), so PK merge-dedup
// must decline even though the index carries a safe base PK and full ordering.
func TestImplementDistinctUnionRule_RequiresEachLegToBeInternallyDistinct(t *testing.T) {
	t.Parallel()

	_, left := distinctUnionIndexLeg(nil) // no !createsDuplicates proof
	_, right := distinctUnionIndexLeg(nil)
	distinctRef := distinctUnionOverLegs(left, right)
	for _, result := range FireImplementationRule(
		NewImplementDistinctUnionRule(), distinctRef,
	) {
		if merge, ok := result.(*plans.RecordQueryMergeSortUnionPlan); ok {
			t.Fatalf(
				"internally non-distinct index legs yielded merge-sort DISTINCT keys %#v",
				merge.GetComparisonKeys(),
			)
		}
	}
}

func TestMergeDistinctLegProducesDistinctRecords_IndexSignal(t *testing.T) {
	t.Parallel()

	createsDuplicates := true
	fanOut, _ := distinctUnionIndexLeg(&createsDuplicates)
	if mergeDistinctLegProducesDistinctRecords(fanOut) {
		t.Fatal("fan-out index leg must not prove internal record distinctness")
	}

	createsDuplicates = false
	scalar, _ := distinctUnionIndexLeg(&createsDuplicates)
	if !mergeDistinctLegProducesDistinctRecords(scalar) {
		t.Fatal("scalar index with an explicit !createsDuplicates signal should prove distinctness")
	}
}

func TestMergeDistinctStoredRecordIdentity_QuantifiesEveryLiveChild(t *testing.T) {
	t.Parallel()

	pk := []values.Value{values.NewFlatFieldValue("ID", values.NullableLong)}
	newScan := func(recordType string) *plans.RecordQueryScanPlan {
		return plans.NewRecordQueryScanPlan([]string{recordType}, values.UnknownType, false).
			WithPrimaryKey(pk).
			WithKeyComponentTypes([]values.Type{values.NullableLong})
	}

	mixed := expressions.FinalOf(newScan("T"))
	mixed.InsertFinal(newScan("U"))
	computeRefPlanProperties(mixed)
	limit := plans.NewRecordQueryLimitPlanFromQuantifier(
		expressions.ForEachQuantifier(mixed), 10, 0, nil,
	)
	if recordType, ok := mergeDistinctStoredRecordIdentity(limit, pk); ok {
		t.Fatalf("mixed live child record types proved one identity %q", recordType)
	}

	unanimous := expressions.FinalOf(newScan("T"))
	unanimous.InsertFinal(newScan("T"))
	computeRefPlanProperties(unanimous)
	limit = plans.NewRecordQueryLimitPlanFromQuantifier(
		expressions.ForEachQuantifier(unanimous), 10, 0, nil,
	)
	if recordType, ok := mergeDistinctStoredRecordIdentity(limit, pk); !ok || recordType != "T" {
		t.Fatalf("unanimous live children = (%q,%v), want (T,true)", recordType, ok)
	}
}
