package cascades

import (
	"fmt"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ---------------------------------------------------------------------------
// 1. RollUpPlanPartitions preserves expression pointer identity.
// ---------------------------------------------------------------------------

func TestEdge_RollUpPartitions_PreservesExpressionIdentity(t *testing.T) {
	t.Parallel()

	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
	wA := &physicalScanWrapper{plan: scanA}
	wB := &physicalScanWrapper{plan: scanB}

	p1 := NewPlanPartition(
		properties.PropertyMap{properties.PropDistinctRecords: true, properties.PropStoredRecord: true},
		map[expressions.RelationalExpression]properties.PropertyMap{wA: {}},
	)
	p2 := NewPlanPartition(
		properties.PropertyMap{properties.PropDistinctRecords: false, properties.PropStoredRecord: false},
		map[expressions.RelationalExpression]properties.PropertyMap{wB: {}},
	)

	// Roll up with no interesting properties => merge into one partition.
	rolled := RollUpPlanPartitions([]*PlanPartition{p1, p2})
	if len(rolled) != 1 {
		t.Fatalf("expected 1 merged partition, got %d", len(rolled))
	}

	exprs := rolled[0].GetExpressions()
	if len(exprs) != 2 {
		t.Fatalf("expected 2 expressions, got %d", len(exprs))
	}

	// Both original wrapper pointers must be present — not copies.
	foundA, foundB := false, false
	for _, e := range exprs {
		if e == wA {
			foundA = true
		}
		if e == wB {
			foundB = true
		}
	}
	if !foundA {
		t.Fatal("rolled-up partition lost pointer identity of wA")
	}
	if !foundB {
		t.Fatal("rolled-up partition lost pointer identity of wB")
	}
}

// ---------------------------------------------------------------------------
// 2. ToPlanPartitions fallback path when Reference has only exploratory
//    members (no FinalMembers).
// ---------------------------------------------------------------------------

func TestEdge_ToPlanPartitions_EmptyFinalMembers(t *testing.T) {
	t.Parallel()

	// Create a Reference with only exploratory members (via InitialOf).
	// No final members, no plan properties set.
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	wrapper := &physicalScanWrapper{plan: scan}
	ref := expressions.InitialOf(wrapper)

	// FinalMembers is empty, plan properties not set -> fallback path.
	partitions := ToPlanPartitions(ref)

	// The fallback should still produce partitions because AllMembers
	// includes the exploratory physical wrapper.
	if len(partitions) == 0 {
		t.Fatal("ToPlanPartitions should use fallback and produce partitions from exploratory members")
	}

	totalPlans := 0
	for _, p := range partitions {
		totalPlans += len(p.GetPlans())
	}
	if totalPlans != 1 {
		t.Fatalf("expected 1 plan via fallback, got %d", totalPlans)
	}
}

// ---------------------------------------------------------------------------
// 3. ComputeRefPlanProperties computes properties for multiple physical
//    wrappers (scan + index scan).
// ---------------------------------------------------------------------------

func TestEdge_ComputeRefPlanProperties_MultiplePlans(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	scanW := &physicalScanWrapper{plan: scan}

	idx := plans.NewRecordQueryIndexPlan("idx1", nil, []string{"T"}, values.UnknownType, false)
	idxW := &physicalIndexScanWrapper{plan: idx, unique: true}

	ref := expressions.NewFinalReference([]expressions.RelationalExpression{scanW, idxW})
	computeRefPlanProperties(ref)

	pm := GetRefPlanPropertiesMap(ref)
	if pm == nil {
		t.Fatal("PlanPropertiesMap should be set after computeRefPlanProperties")
	}

	// Both wrappers should have computed properties.
	scanProps := pm.GetProperties(scanW)
	if scanProps == nil {
		t.Fatal("no properties computed for scanW")
	}
	idxProps := pm.GetProperties(idxW)
	if idxProps == nil {
		t.Fatal("no properties computed for idxW")
	}

	// Scan is always distinct.
	if !scanProps.GetBool(properties.PropDistinctRecords) {
		t.Fatal("scan should have distinctRecords=true")
	}
	// Unique index scan is also distinct.
	if !idxProps.GetBool(properties.PropDistinctRecords) {
		t.Fatal("unique index scan should have distinctRecords=true")
	}
	// Both should be stored record.
	if !scanProps.GetBool(properties.PropStoredRecord) {
		t.Fatal("scan should have storedRecord=true")
	}
	if !idxProps.GetBool(properties.PropStoredRecord) {
		t.Fatal("index scan should have storedRecord=true")
	}
}

// ---------------------------------------------------------------------------
// 4. PlanPartition with zero expressions.
// ---------------------------------------------------------------------------

func TestEdge_PlanPartition_EmptyPartition(t *testing.T) {
	t.Parallel()

	pp := NewPlanPartition(
		properties.PropertyMap{properties.PropDistinctRecords: true},
		map[expressions.RelationalExpression]properties.PropertyMap{},
	)

	gotPlans := pp.GetPlans()
	if len(gotPlans) != 0 {
		t.Fatalf("GetPlans() on empty partition = %d, want 0", len(gotPlans))
	}

	gotExprs := pp.GetExpressions()
	if len(gotExprs) != 0 {
		t.Fatalf("GetExpressions() on empty partition = %d, want 0", len(gotExprs))
	}

	// Partition-level properties should still be accessible.
	if !pp.IsDistinct() {
		t.Fatal("IsDistinct() should return true even with no expressions")
	}
}

// ---------------------------------------------------------------------------
// 5. ImplementUniqueRule with Unique(Unique(Scan)).
//    Both Unique layers should be absorbed since the inner scan is distinct.
// ---------------------------------------------------------------------------

func TestEdge_ImplementUniqueRule_ChainedUnique(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	innerUnique := expressions.NewLogicalUniqueExpression(
		expressions.ForEachQuantifier(scanRef),
	)
	innerUniqueRef := expressions.InitialOf(innerUnique)

	outerUnique := expressions.NewLogicalUniqueExpression(
		expressions.ForEachQuantifier(innerUniqueRef),
	)
	rootRef := expressions.InitialOf(outerUnique)

	planWithImplRules(t, rootRef, DefaultImplementationRules())

	// After planning, the root should have a physicalScanWrapper in its
	// members — both Unique layers absorbed because scan is distinct.
	finals := rootRef.AllMembers()
	if len(finals) == 0 {
		t.Fatal("root Reference has no members — chained Unique not processed")
	}

	foundScan := false
	for _, f := range finals {
		if _, ok := f.(*physicalScanWrapper); ok {
			foundScan = true
			break
		}
	}
	if !foundScan {
		typs := make([]string, len(finals))
		for i, f := range finals {
			typs[i] = fmt.Sprintf("%T", f)
		}
		t.Fatalf("expected physicalScanWrapper in members (both Uniques absorbed), got: %v", typs)
	}
}

// ---------------------------------------------------------------------------
// 6. ImplementUnorderedUnionRule with 3 children.
// ---------------------------------------------------------------------------

func TestEdge_ImplementUnorderedUnionRule_ThreeChildren(t *testing.T) {
	t.Parallel()

	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
	scanC := plans.NewRecordQueryScanPlan([]string{"C"}, values.UnknownType, false)
	wA := &physicalScanWrapper{plan: scanA}
	wB := &physicalScanWrapper{plan: scanB}
	wC := &physicalScanWrapper{plan: scanC}

	refA := expressions.NewFinalReference([]expressions.RelationalExpression{wA})
	pmA := NewPlanPropertiesMap()
	pmA.Add(wA)
	refA.SetPlanProperties(pmA)

	refB := expressions.NewFinalReference([]expressions.RelationalExpression{wB})
	pmB := NewPlanPropertiesMap()
	pmB.Add(wB)
	refB.SetPlanProperties(pmB)

	refC := expressions.NewFinalReference([]expressions.RelationalExpression{wC})
	pmC := NewPlanPropertiesMap()
	pmC.Add(wC)
	refC.SetPlanProperties(pmC)

	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
		expressions.ForEachQuantifier(refC),
	})
	outerRef := expressions.InitialOf(union)

	results := FireImplementationRule(NewImplementUnorderedUnionRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("ImplementUnorderedUnionRule should yield expressions for 3 children")
	}

	foundWrapper := false
	for _, r := range results {
		if w, ok := r.(*physicalUnorderedUnionWrapper); ok {
			foundWrapper = true
			uup, ok := w.GetRecordQueryPlan().(*plans.RecordQueryUnorderedUnionPlan)
			if !ok {
				t.Fatalf("expected *RecordQueryUnorderedUnionPlan, got %T", w.GetRecordQueryPlan())
			}
			inners := uup.GetInners()
			if len(inners) != 3 {
				t.Fatalf("expected 3 inner plans, got %d", len(inners))
			}
		}
	}
	if !foundWrapper {
		t.Fatal("expected physicalUnorderedUnionWrapper in results")
	}
}

// ---------------------------------------------------------------------------
// 7. CrossProduct with 3 lists of 3 elements each = 27 combinations.
// ---------------------------------------------------------------------------

func TestEdge_CrossProduct_LargeInput(t *testing.T) {
	t.Parallel()

	lists := [][]int{
		{1, 2, 3},
		{4, 5, 6},
		{7, 8, 9},
	}
	result := CrossProduct(lists)
	if len(result) != 27 {
		t.Fatalf("expected 27 combinations (3*3*3), got %d", len(result))
	}

	// Verify each combination has exactly 3 elements.
	for i, combo := range result {
		if len(combo) != 3 {
			t.Fatalf("combo %d: expected length 3, got %d", i, len(combo))
		}
	}

	// Verify uniqueness: no two combinations should be identical.
	seen := make(map[[3]int]bool, 27)
	for _, combo := range result {
		key := [3]int{combo[0], combo[1], combo[2]}
		if seen[key] {
			t.Fatalf("duplicate combination: %v", combo)
		}
		seen[key] = true
	}

	// Verify all elements from each list appear the expected number of times.
	// Each element from a list of 3 should appear in 9 combos (3*3 from the other two lists).
	counts := make(map[int]int)
	for _, combo := range result {
		for _, v := range combo {
			counts[v]++
		}
	}
	for v := 1; v <= 9; v++ {
		if counts[v] != 9 {
			t.Fatalf("element %d appeared %d times, want 9", v, counts[v])
		}
	}
}

// ---------------------------------------------------------------------------
// 8. RichOrdering with 3 keys, request only the first 2.
// ---------------------------------------------------------------------------

func TestEdge_RichOrdering_MultiKeyOrdering(t *testing.T) {
	t.Parallel()

	a := fieldVal("a")
	b := fieldVal("b")
	c := fieldVal("c")

	o := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {SortedBinding(ProvidedSortOrderAscending)},
			b: {SortedBinding(ProvidedSortOrderDescending)},
			c: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{a, b, c},
		false,
	)

	// Request only first 2 keys — should be satisfied because the
	// provided ordering is a superset (covers the prefix).
	req := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: a, SortOrder: RequestedSortOrderAscending},
		{Value: b, SortOrder: RequestedSortOrderDescending},
	}, DistinctnessNotDistinct, false)

	if !o.Satisfies(req) {
		t.Fatal("ordering with 3 keys should satisfy a request for the first 2")
	}

	// Request the first 2 but with wrong direction on second key.
	reqBad := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: a, SortOrder: RequestedSortOrderAscending},
		{Value: b, SortOrder: RequestedSortOrderAscending}, // want asc, provided desc
	}, DistinctnessNotDistinct, false)

	if o.Satisfies(reqBad) {
		t.Fatal("ordering should NOT satisfy request when direction mismatches on second key")
	}
}

// ---------------------------------------------------------------------------
// 9. PropertyMap with nil value for a property key.
// ---------------------------------------------------------------------------

func TestEdge_PropertyMapNilValue(t *testing.T) {
	t.Parallel()

	m := properties.PropertyMap{
		properties.PropDistinctRecords: nil,
		properties.PropStoredRecord:    nil,
		properties.PropOrdering:        nil,
	}

	// GetBool with nil value should return false, not panic.
	if m.GetBool(properties.PropDistinctRecords) {
		t.Fatal("GetBool with nil value should return false")
	}
	if m.GetBool(properties.PropStoredRecord) {
		t.Fatal("GetBool with nil value should return false")
	}

	// GetOrdering with nil value should return zero Ordering, not panic.
	o := m.GetOrdering()
	if o.IsKnown {
		t.Fatal("GetOrdering with nil value should return zero Ordering")
	}
}

// ---------------------------------------------------------------------------
// 10. After PLANNING, extraction should prefer physical wrappers from
//     FinalMembers over logical expressions in Members.
// ---------------------------------------------------------------------------

func TestEdge_PlanExtraction_PrefersFinalOverExploratory(t *testing.T) {
	t.Parallel()

	// Build a Reference with a logical expression as an exploratory member.
	logicalScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	ref := expressions.InitialOf(logicalScan)

	// Simulate PLANNING phase: insert a physical wrapper as a final member.
	physScan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	wrapper := &physicalScanWrapper{plan: physScan}
	ref.InsertFinal(wrapper)

	// GetBest with a comparator that always prefers physical plans.
	best := ref.GetBest(func(a, b expressions.RelationalExpression) bool {
		_, aPhys := a.(physicalPlanExpression)
		_, bPhys := b.(physicalPlanExpression)
		// Prefer physical over logical.
		if aPhys && !bPhys {
			return true
		}
		return false
	})

	if best == nil {
		t.Fatal("GetBest should return a member")
	}

	// The best member should be the physical wrapper.
	if _, ok := best.(physicalPlanExpression); !ok {
		t.Fatalf("GetBest should prefer the physical wrapper, got %T", best)
	}
	if best != wrapper {
		t.Fatal("GetBest should return the exact physical wrapper pointer")
	}
}
