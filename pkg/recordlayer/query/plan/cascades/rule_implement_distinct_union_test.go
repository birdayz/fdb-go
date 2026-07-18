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
	sw := &physicalScanWrapper{plan: scan}

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

func makeScanWithPK(recordType string, pkCols ...string) (*physicalScanWrapper, *expressions.Reference) {
	pkVals := make([]values.Value, len(pkCols))
	for i, col := range pkCols {
		pkVals[i] = &values.FieldValue{Field: col, Typ: values.UnknownType}
	}
	scan := plans.NewRecordQueryScanPlan([]string{recordType}, values.UnknownType, false).WithPrimaryKey(pkVals)
	sw := &physicalScanWrapper{plan: scan}
	ref := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	ref.SetPlanProperties(pm)
	return sw, ref
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
		if _, ok := r.(*physicalMergeSortUnionWrapper); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("should yield physicalMergeSortUnionWrapper")
	}
}

func TestImplementDistinctUnionRule_NoFireWithoutPK(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}
	refA := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	refA.SetPlanProperties(pm)

	scan2 := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw2 := &physicalScanWrapper{plan: scan2}
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
	o1 := NewRichOrdering(
		map[values.Value][]OrderingBinding{keyA: {FixedBinding(nil)}},
		[]values.Value{keyA}, false,
	)
	o2 := NewRichOrdering(
		map[values.Value][]OrderingBinding{keyB: {FixedBinding(nil)}},
		[]values.Value{keyB}, false,
	)
	result := removeCommonEqualityBoundParts([]*RichOrdering{o1, o2})
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
	o1 := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			keyA: {FixedBinding(nil)},
			keyB: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{keyA, keyB}, false,
	)
	o2 := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			keyA: {FixedBinding(nil)},
			keyB: {SortedBinding(ProvidedSortOrderDescending)},
		},
		[]values.Value{keyA, keyB}, false,
	)
	result := removeCommonEqualityBoundParts([]*RichOrdering{o1, o2})
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
	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{keyA: {FixedBinding(nil)}},
		[]values.Value{keyA}, false,
	)
	result := removeCommonEqualityBoundParts([]*RichOrdering{o})
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
	orderedScan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).WithPrimaryKey(pkVals)
	orderedSW := &physicalScanWrapper{plan: orderedScan}
	srcRef := expressions.InitialOf(orderedSW)
	pmSrc := NewPlanPropertiesMap()
	pmSrc.Add(orderedSW)
	srcRef.SetPlanProperties(pmSrc)

	staleScan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).WithPrimaryKey(pkVals)
	filterWrap := &physicalPredicatesFilterWrapper{
		plan: plans.NewRecordQueryPredicatesFilterPlan(
			staleScan,
			[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		),
		innerQuant: expressions.ForEachQuantifier(srcRef),
	}
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
	var msu *physicalMergeSortUnionWrapper
	for _, r := range results {
		if w, ok := r.(*physicalMergeSortUnionWrapper); ok {
			msu = w
			break
		}
	}
	if msu == nil {
		t.Fatal("expected a merge-sort union yield (the pinned path must not over-decline this shape)")
	}
	for _, child := range msu.plan.GetChildren() {
		if child == staleScan {
			t.Fatal("the union baked the delegator's STALE plan child — the leg was not spine-pinned and the merge dedup runs over an order the leg does not produce")
		}
		if fp, ok := child.(*plans.RecordQueryPredicatesFilterPlan); ok && fp.GetInner() == staleScan {
			t.Fatal("the union baked the filter over its STALE child — pinOrderedSpine must relink the executable plan to the ordered member")
		}
	}
}
