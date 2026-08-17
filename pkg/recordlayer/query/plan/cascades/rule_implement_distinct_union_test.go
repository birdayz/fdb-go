package cascades

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustDistinctUnionConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct distinct-union fixture: " + err.Error())
	}
	return value
}

func distinctUnionScanRowType() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{
		{Name: "id", FieldType: values.NullableLong},
		{Name: "name", FieldType: values.NullableString},
	})
}

func distinctUnionField(root values.Value, name string) values.Value {
	request := mustDistinctUnionConstruct(values.FieldByName(name))
	return mustDistinctUnionConstruct(values.ResolveFieldAccess(
		root, []values.FieldRequest{request}))
}

func distinctUnionNamedFields(names ...string) []values.Value {
	fields := make([]values.Field, len(names))
	for i, name := range names {
		fields[i] = values.Field{Name: name, FieldType: values.NullableLong}
	}
	root := mustDistinctUnionConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("distinct_union_fields"),
		values.NewRecordType("", false, fields)))
	result := make([]values.Value, len(names))
	for i := range names {
		result[i] = mustDistinctUnionConstruct(values.ResolveFieldOrdinals(root, []int{i}))
	}
	return result
}

func distinctUnionPrimaryKeyFields(names ...string) []values.Value {
	root := mustDistinctUnionConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("distinct_union_primary_key"),
		distinctUnionScanRowType()))
	result := make([]values.Value, len(names))
	for i, name := range names {
		result[i] = distinctUnionField(root, name)
	}
	return result
}

func distinctUnionScan(recordType string, rowType values.Type) *plans.RecordQueryScanPlan {
	return mustDistinctUnionConstruct(plans.NewRecordQueryScanPlan(
		[]string{recordType}, rowType, false))
}

func TestImplementDistinctUnionRule_MatchesLogicalDistinct(t *testing.T) {
	t.Parallel()
	rule := NewImplementDistinctUnionRule()
	scanRef := expressions.InitialOf(mustDistinctUnionConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, distinctUnionScanRowType())))
	distinct := mustDistinctUnionConstruct(expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(scanRef)))
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), distinct)
	if len(bindings) == 0 {
		t.Fatal("should match LogicalDistinctExpression")
	}
}

func TestImplementDistinctUnionRule_SkipsNonDistinct(t *testing.T) {
	t.Parallel()
	rule := NewImplementDistinctUnionRule()
	scanRef := expressions.InitialOf(mustDistinctUnionConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, distinctUnionScanRowType())))
	filter := mustDistinctUnionConstruct(expressions.NewLogicalFilterExpression(nil, expressions.ForEachQuantifier(scanRef)))
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), filter)
	if len(bindings) != 0 {
		t.Fatal("should NOT match LogicalFilterExpression")
	}
}

func TestImplementDistinctUnionRule_RequiresUnionChild(t *testing.T) {
	t.Parallel()
	scan := distinctUnionScan("T", distinctUnionScanRowType())
	sw := scan

	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	distinct := mustDistinctUnionConstruct(expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(innerRef)))
	outerRef := expressions.InitialOf(distinct)

	results := mustFireImplementationRule(t, NewImplementDistinctUnionRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should not fire when child is not a Union, got %d", len(results))
	}
}

func makeScanWithPK(recordType string, pkCols ...string) (*plans.RecordQueryScanPlan, *expressions.Reference) {
	rowType := distinctUnionScanRowType()
	scan := distinctUnionScan(recordType, rowType)
	pkVals := distinctUnionPrimaryKeyFields(pkCols...)
	keyTypes := make([]values.Type, len(pkCols))
	for i := range pkCols {
		keyTypes[i] = pkVals[i].Type()
	}
	scan = scan.WithKeyComponentTypes(keyTypes).
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

	union := mustDistinctUnionConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	}))

	unionRef := expressions.InitialOf(union)

	distinct := mustDistinctUnionConstruct(expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(unionRef)))
	outerRef := expressions.InitialOf(distinct)

	results := mustFireImplementationRule(t, NewImplementDistinctUnionRule(), outerRef)
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
	scan := distinctUnionScan("T", distinctUnionScanRowType())
	sw := scan
	refA := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	refA.SetPlanProperties(pm)

	scan2 := distinctUnionScan("T", distinctUnionScanRowType())
	sw2 := scan2
	refB := expressions.InitialOf(sw2)
	pm2 := NewPlanPropertiesMap()
	pm2.Add(sw2)
	refB.SetPlanProperties(pm2)

	union := mustDistinctUnionConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	}))

	unionRef := expressions.InitialOf(union)

	distinct := mustDistinctUnionConstruct(expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(unionRef)))
	outerRef := expressions.InitialOf(distinct)

	results := mustFireImplementationRule(t, NewImplementDistinctUnionRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should not fire without PK, got %d", len(results))
	}
}

func TestImplementDistinctUnionRule_IncompatiblePK(t *testing.T) {
	t.Parallel()
	_, refA := makeScanWithPK("T", "id")
	_, refB := makeScanWithPK("T", "name")

	union := mustDistinctUnionConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	}))

	unionRef := expressions.InitialOf(union)

	distinct := mustDistinctUnionConstruct(expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(unionRef)))
	outerRef := expressions.InitialOf(distinct)

	results := mustFireImplementationRule(t, NewImplementDistinctUnionRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should not fire with incompatible PKs, got %d", len(results))
	}
}

func TestGetCommonPK_AllSame(t *testing.T) {
	t.Parallel()
	pk := distinctUnionNamedFields("id")
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
	pk := distinctUnionNamedFields("id")
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
	keys := distinctUnionNamedFields("a", "b")
	keyA, keyB := keys[0], keys[1]
	o1 := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{keyA: {properties.FixedBinding(nil)}},
		[]values.Value{keyA}, properties.NotDistinct())
	o2 := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{keyB: {properties.FixedBinding(nil)}},
		[]values.Value{keyB}, properties.NotDistinct())
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
	keys := distinctUnionNamedFields("a", "b")
	keyA, keyB := keys[0], keys[1]
	o1 := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			keyA: {properties.FixedBinding(nil)},
			keyB: {properties.SortedBinding(properties.ProvidedSortOrderAscending)},
		},
		[]values.Value{keyA, keyB}, properties.NotDistinct())
	o2 := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			keyA: {properties.FixedBinding(nil)},
			keyB: {properties.SortedBinding(properties.ProvidedSortOrderDescending)},
		},
		[]values.Value{keyA, keyB}, properties.NotDistinct())
	result := removeCommonEqualityBoundParts([]*properties.RichOrdering{o1, o2})
	if len(result) != 2 {
		t.Fatalf("expected 2 orderings, got %d", len(result))
	}
	if len(result[0].GetKeys()) != 1 {
		t.Fatalf("expected 1 key after removal, got %d", len(result[0].GetKeys()))
	}
	field, ok := values.AsFieldValue(result[0].GetKeys()[0])
	if !ok || field.DisplayName() != "b" {
		t.Fatalf("expected key 'b', got %q", values.ExplainValue(result[0].GetKeys()[0]))
	}
}

func TestRemoveCommonEqualityBoundParts_SingleOrdering(t *testing.T) {
	t.Parallel()
	keyA := distinctUnionNamedFields("a")[0]
	o := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{keyA: {properties.FixedBinding(nil)}},
		[]values.Value{keyA}, properties.NotDistinct())
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
	orderedScan := distinctUnionScan("T", distinctUnionScanRowType())
	pkVals := distinctUnionPrimaryKeyFields("id")
	orderedScan = orderedScan.
		WithKeyComponentTypes([]values.Type{values.NullableLong}).
		WithPrimaryKey(pkVals)
	orderedSW := orderedScan
	srcRef := expressions.InitialOf(orderedSW)
	pmSrc := NewPlanPropertiesMap()
	pmSrc.Add(orderedSW)
	srcRef.SetPlanProperties(pmSrc)

	staleScan := distinctUnionScan("T", distinctUnionScanRowType())
	stalePK := distinctUnionPrimaryKeyFields("id")
	staleScan = staleScan.
		WithKeyComponentTypes([]values.Type{values.NullableLong}).
		WithPrimaryKey(stalePK)
	filterWrap := mustWithQuantifiers(t, mustDistinctUnionConstruct(plans.NewRecordQueryPredicatesFilterPlan(
		staleScan,
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
	)), []expressions.Quantifier{expressions.ForEachQuantifier(srcRef)}).(*plans.RecordQueryPredicatesFilterPlan)
	refB := expressions.InitialOf(filterWrap)
	pmB := NewPlanPropertiesMap()
	pmB.Add(filterWrap)
	refB.SetPlanProperties(pmB)

	union := mustDistinctUnionConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	}))

	unionRef := expressions.InitialOf(union)
	distinct := mustDistinctUnionConstruct(expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(unionRef)))
	outerRef := expressions.InitialOf(distinct)

	results := mustFireImplementationRule(t, NewImplementDistinctUnionRule(), outerRef)
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

func distinctUnionCompositeFields(recordType string) []values.Value {
	root := mustDistinctUnionConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("distinct_union_composite_key"),
		distinctUnionCompositeRowType(recordType)))
	return []values.Value{
		mustDistinctUnionConstruct(values.ResolveFieldOrdinals(root, []int{0})),
		mustDistinctUnionConstruct(values.ResolveFieldOrdinals(root, []int{1})),
	}
}

func distinctUnionCompositeScanRef(recordType string) *expressions.Reference {
	pk := distinctUnionCompositeFields(recordType)
	scan := distinctUnionScan(recordType, distinctUnionCompositeRowType(recordType)).
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
	union := mustDistinctUnionConstruct(expressions.NewLogicalUnionExpression(quantifiers))
	distinct := mustDistinctUnionConstruct(expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(expressions.InitialOf(union))))

	return expressions.InitialOf(distinct)
}

// TestImplementDistinctUnionRule_ExactFreePrimaryKeySuffixIsPreserved pins the
// RFC-232 replacement for the old unresolved-key refusal. Every admitted field
// is now exact, so a proper-prefix request must retain the unrequested B suffix
// as a full dedup tiebreak instead of silently shortening the comparison tuple.
func TestImplementDistinctUnionRule_ExactFreePrimaryKeySuffixIsPreserved(t *testing.T) {
	t.Parallel()

	limitLeg := func() *expressions.Reference {
		scanRef := distinctUnionCompositeScanRef("T")
		limit := mustDistinctUnionConstruct(plans.NewRecordQueryLimitPlanFromQuantifier(
			expressions.ForEachQuantifier(scanRef), 100, 0, nil))

		limitRef := expressions.FinalOf(limit)
		computeRefPlanProperties(limitRef)
		return limitRef
	}
	distinctRef := distinctUnionOverLegs(limitLeg(), limitLeg())
	pk := distinctUnionCompositeFields("T")
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     pk[0],
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

	found := false
	for _, result := range mustFireImplementationRule(t,
		NewImplementDistinctUnionRule(), distinctRef, constraints,
	) {
		if merge, ok := result.(*plans.RecordQueryMergeSortUnionPlan); ok {
			found = true
			if got := len(merge.GetComparisonKeys()); got != 2 {
				t.Fatalf("merge comparison-key count = %d, want full (A,B) tiebreak", got)
			}
		}
	}
	if !found {
		t.Fatal("exact composite-PK legs did not yield a merge-sort DISTINCT")
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
	pk := distinctUnionCompositeFields("T")
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     pk[0],
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessPreserveDistinctness,
		false,
	)
	keys := bakeMergeComparisonKeys(pk, requested, rowType)
	if len(keys) != 2 {
		t.Fatalf("comparison keys = %#v, want baked (A,B)", keys)
	}
	for i, key := range keys {
		field, ok := values.AsFieldValue(key)
		if !ok || field.Path() == nil || field.Path().Len() == 0 {
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
	pk := mustDistinctUnionConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("distinct_union_projected_pk"), rowType))
	pkID := mustDistinctUnionConstruct(values.ResolveFieldOrdinals(pk, []int{0}))
	scan := distinctUnionScan("T", rowType).
		WithPrimaryKey([]values.Value{pkID}).
		WithKeyComponentTypes([]values.Type{values.NullableLong})
	scanRef := expressions.FinalOf(scan)
	computeRefPlanProperties(scanRef)
	projectionQ := expressions.ForEachQuantifier(scanRef)
	projectionRoot := mustDistinctUnionConstruct(projectionQ.RequireFlowedObjectValue())
	id := mustDistinctUnionConstruct(values.ResolveFieldOrdinals(projectionRoot, []int{0}))
	projection := mustDistinctUnionConstruct(plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{id, &values.ConstantValue{Value: constant, Typ: values.NotNullLong}},
		[]string{"ID", "V"},
		projectionQ))

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
	for _, result := range mustFireImplementationRule(t,
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
		projection := projectionRef.FinalMembers()[0].(*plans.RecordQueryProjectionPlan)
		resultType := projection.GetResultType()
		fetch := mustDistinctUnionConstruct(plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
			expressions.ForEachQuantifier(projectionRef),
			nil,
			resultType,
			plans.FetchIndexRecordsPrimaryKey))

		fetchRef := expressions.FinalOf(fetch)
		computeRefPlanProperties(fetchRef)
		return fetchRef
	}

	distinctRef := distinctUnionOverLegs(fetchLeg(1), fetchLeg(2))
	for _, result := range mustFireImplementationRule(t,
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

	rowType := values.NewRecordType("", false, []values.Field{{Name: "ID", FieldType: values.NullableLong}})
	pk := distinctUnionNamedFields("ID")
	scan := distinctUnionScan("T", rowType).
		WithPrimaryKey(pk).
		WithKeyComponentTypes([]values.Type{values.NullableLong})
	scanRef := expressions.FinalOf(scan)
	computeRefPlanProperties(scanRef)

	projectionQ := expressions.ForEachQuantifier(scanRef)
	projectionRoot := mustDistinctUnionConstruct(projectionQ.RequireFlowedObjectValue())
	if projection, err := plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{projectionRoot}, nil, projectionQ,
	); !errors.Is(err, values.ErrWholeRowProjection) || projection != nil {
		t.Fatalf("whole-row identity Projection = (%#v, %v), want constructor rejection", projection, err)
	}

	mapQ := expressions.ForEachQuantifier(scanRef)
	mapRoot := mustDistinctUnionConstruct(mapQ.RequireFlowedObjectValue())
	mapPlan := mustDistinctUnionConstruct(plans.NewRecordQueryMapPlanFromQuantifier(
		mapQ,
		mapRoot))

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
	for _, result := range mustFireImplementationRule(t,
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
	index := mustDistinctUnionConstruct(plans.NewRecordQueryIndexPlan(
		"IDX_TAG", nil, []string{"T"}, rowType, false))
	pkRoot := mustDistinctUnionConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("distinct_union_index_pk"), rowType))
	pk := []values.Value{mustDistinctUnionConstruct(values.ResolveFieldOrdinals(pkRoot, []int{1}))}
	index = index.
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
	for _, result := range mustFireImplementationRule(t,
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

	rowType := values.NewRecordType("", false, []values.Field{{Name: "ID", FieldType: values.NullableLong}})
	pk := distinctUnionNamedFields("ID")
	newScan := func(recordType string) *plans.RecordQueryScanPlan {
		return distinctUnionScan(recordType, rowType).
			WithPrimaryKey(pk).
			WithKeyComponentTypes([]values.Type{values.NullableLong})
	}

	mixed := expressions.FinalOf(newScan("T"))
	mixed.InsertFinal(newScan("U"))
	computeRefPlanProperties(mixed)
	limit := mustDistinctUnionConstruct(plans.NewRecordQueryLimitPlanFromQuantifier(
		expressions.ForEachQuantifier(mixed), 10, 0, nil))

	if recordType, ok := mergeDistinctStoredRecordIdentity(limit, pk); ok {
		t.Fatalf("mixed live child record types proved one identity %q", recordType)
	}

	unanimous := expressions.FinalOf(newScan("T"))
	unanimous.InsertFinal(newScan("T"))
	computeRefPlanProperties(unanimous)
	limit = mustDistinctUnionConstruct(plans.NewRecordQueryLimitPlanFromQuantifier(
		expressions.ForEachQuantifier(unanimous), 10, 0, nil))

	if recordType, ok := mergeDistinctStoredRecordIdentity(limit, pk); !ok || recordType != "T" {
		t.Fatalf("unanimous live children = (%q,%v), want (T,true)", recordType, ok)
	}
}
