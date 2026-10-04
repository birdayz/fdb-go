package cascades

import (
	"slices"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func propertyReuseScan(reverse bool) *plans.RecordQueryScanPlan {
	scan := mustPropertiesConstruct(plans.NewRecordQueryScanPlan([]string{"T"}, planPropertiesRowType(), reverse))
	key := mustPropertiesConstruct(values.ResolveFieldOrdinals(scan.GetResultValue(), []int{0}))
	return scan.WithPrimaryKey([]values.Value{key}).WithKeyComponentTypes([]values.Type{values.NotNullLong})
}

func propertyReuseFilter(ref *expressions.Reference) *plans.RecordQueryPredicatesFilterPlan {
	return mustPropertiesConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(
		expressions.NamedPhysicalQuantifier(values.UniqueCorrelationIdentifier(), ref), nil,
	))
}

func TestPlanPropertiesReuseUnchangedInputs(t *testing.T) {
	t.Parallel()
	scan := propertyReuseScan(false)
	ref := expressions.FinalOfAtStage(scan, expressions.StagePlanned)
	for range 3 {
		ref = expressions.FinalOfAtStage(propertyReuseFilter(ref), expressions.StagePlanned)
	}
	member := ref.FinalMembers()[0]
	computeRefPlanProperties(ref)
	originalMap := GetRefPlanPropertiesMap(ref)
	original := originalMap.GetProperties(member)
	if ordering := original.GetOrdering(); !ordering.IsKnown || len(ordering.Keys) != 1 || ordering.DescendingAt(0) {
		t.Fatalf("initial ordering = %#v, want ascending primary key", ordering)
	}
	for range 5 {
		computeRefPlanProperties(ref)
		currentMap := GetRefPlanPropertiesMap(ref)
		current := currentMap.GetProperties(member)
		if currentMap != originalMap || current[properties.PropRichOrdering] != original[properties.PropRichOrdering] ||
			current.GetDerivations() != original.GetDerivations() {
			t.Fatal("unchanged reference rebuilt its property derivations")
		}
	}
}

func TestPlanPropertiesReuseInvalidatesDescendants(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"exploratory", "new-final", "prune", "clear-finals", "forward", "winner"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			forward, reverse := propertyReuseScan(false), propertyReuseScan(true)
			leaf := expressions.InitialOf(forward)
			if name == "prune" || name == "winner" {
				leaf.InsertFinal(forward)
				leaf.InsertFinal(reverse)
				leaf.SetWinner(forward)
			}
			if name == "clear-finals" {
				leaf.InsertFinal(reverse)
			}
			middle := expressions.FinalOfAtStage(propertyReuseFilter(leaf), expressions.StagePlanned)
			root := propertyReuseFilter(middle)
			pm := NewPlanPropertiesMap()
			pm.Add(root)
			original := pm.GetProperties(root)
			initial := original.GetOrdering()
			if !initial.IsKnown || len(initial.Keys) != 1 || initial.DescendingAt(0) != (name == "clear-finals") {
				t.Fatalf("initial ordering = %#v", initial)
			}
			descending := false
			switch name {
			case "exploratory":
				leaf.Insert(reverse)
			case "new-final":
				leaf.InsertFinal(reverse)
				descending = true
			case "prune":
				leaf.PruneWith(reverse)
				descending = true
			case "clear-finals":
				leaf.ClearFinalMembers()
			case "forward":
				survivor := expressions.FinalOfAtStage(reverse, expressions.StagePlanned)
				survivor.Absorb(leaf)
				descending = true
			case "winner":
				leaf.SetWinner(reverse)
			}
			pm.Add(root)
			current := pm.GetProperties(root)
			if current[properties.PropRichOrdering] == original[properties.PropRichOrdering] {
				t.Fatal("descendant change left the old property snapshot installed")
			}
			ordering := current.GetOrdering()
			if !ordering.IsKnown || len(ordering.Keys) != 1 || ordering.DescendingAt(0) != descending {
				t.Fatalf("ordering after mutation = %#v, want descending=%t", ordering, descending)
			}
			pm.Add(root)
			if pm.GetProperties(root)[properties.PropRichOrdering] != current[properties.PropRichOrdering] {
				t.Fatal("updated inputs did not settle after recomputation")
			}
		})
	}
}

func TestPlanPropertiesReuseTracksWinnerDescendants(t *testing.T) {
	t.Parallel()
	scan := propertyReuseScan(false)
	winnerInput := expressions.FinalOfAtStage(scan, expressions.StagePlanned)
	child := expressions.FinalOfAtStage(scan, expressions.StagePlanned)
	// Property derivation follows the stamped winner even before extraction
	// rejects a winner outside the final population.
	child.SetWinner(propertyReuseFilter(winnerInput))
	root := propertyReuseFilter(child)
	pm := NewPlanPropertiesMap()
	pm.Add(root)
	newKey := mustPropertiesConstruct(values.ResolveFieldOrdinals(scan.GetResultValue(), []int{2}))
	winnerInput.PruneWith(scan.WithPrimaryKey([]values.Value{newKey}))
	pm.Add(root)
	got := pm.GetProperties(root)[properties.PropPrimaryKey].([]values.Value)
	if len(got) != 1 || !values.ValuesStructurallyEqual(got[0], newKey) {
		t.Fatalf("winner's updated primary key was not propagated: %v", got)
	}
	assertFreshPlanProperties(t, pm.GetProperties(root), computeWrapperProperties(root))
}

func TestPlanPropertiesReuseTracksChildPropertyUpdates(t *testing.T) {
	t.Parallel()
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-map", true: "replacement-map"}[replace], func(t *testing.T) {
			t.Parallel()
			scan := propertyReuseScan(false)
			child := expressions.FinalOfAtStage(scan, expressions.StagePlanned)
			childProperties := NewPlanPropertiesMap()
			childProperties.Set(scan, properties.PropertyMap{properties.PropDistinctRecords: false})
			child.SetPlanProperties(childProperties)
			root := propertyReuseFilter(child)
			pm := NewPlanPropertiesMap()
			pm.Add(root)
			if pm.GetProperties(root).GetBool(properties.PropDistinctRecords) {
				t.Fatal("filter ignored its child's non-distinct property")
			}
			if replace {
				childProperties = NewPlanPropertiesMap()
				child.SetPlanProperties(childProperties)
			}
			childProperties.Set(scan, properties.PropertyMap{properties.PropDistinctRecords: true})
			pm.Add(root)
			if !pm.GetProperties(root).GetBool(properties.PropDistinctRecords) {
				t.Fatal("filter retained a stale child-property result")
			}
			settled := pm.GetProperties(root)[properties.PropRichOrdering]
			pm.Add(root)
			if pm.GetProperties(root)[properties.PropRichOrdering] != settled {
				t.Fatal("updated child properties did not settle")
			}
		})
	}
}

func TestPlanPropertiesReuseRetainsOnlyCurrentMembers(t *testing.T) {
	t.Parallel()
	first, second := propertyReuseScan(false), propertyReuseScan(true)
	ref := expressions.FinalOfAtStage(first, expressions.StagePlanned)
	ref.InsertFinal(second)
	computeRefPlanProperties(ref)
	pm := GetRefPlanPropertiesMap(ref)
	firstOrdering := pm.GetProperties(first)[properties.PropRichOrdering]
	ref.PruneWith(first)
	computeRefPlanProperties(ref)
	if GetRefPlanPropertiesMap(ref) != pm || pm.GetProperties(first)[properties.PropRichOrdering] != firstOrdering {
		t.Fatal("pruning rebuilt the surviving member's properties")
	}
	if len(pm.Expressions()) != 1 || pm.Expressions()[0] != first || pm.GetProperties(second) != nil {
		t.Fatal("pruned member's properties remain visible")
	}
	ref.InsertFinal(second)
	computeRefPlanProperties(ref)
	if len(pm.Expressions()) != 2 || pm.Expressions()[0] != first || pm.Expressions()[1] != second || pm.GetProperties(second) == nil {
		t.Fatal("late member lost its properties or insertion order")
	}
}

func FuzzPlanPropertiesReuse(f *testing.F) {
	for _, shared := range []bool{false, true} {
		f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}, shared)
		f.Add([]byte{13, 1, 16, 5, 17, 8, 19, 6, 22, 11, 9}, shared)
	}
	f.Fuzz(func(t *testing.T, steps []byte, shared bool) {
		t.Parallel()
		scans := []*plans.RecordQueryScanPlan{propertyReuseScan(false), propertyReuseScan(true)}
		leaf := expressions.InitialOf(scans[0])
		middlePlan := propertyReuseFilter(leaf)
		middle := expressions.FinalOfAtStage(middlePlan, expressions.StagePlanned)
		var root physicalPlanExpression = propertyReuseFilter(middle)
		if shared {
			other := expressions.FinalOfAtStage(propertyReuseFilter(leaf), expressions.StagePlanned)
			root = mustPropertiesConstruct(plans.NewRecordQueryUnorderedUnionPlanFromQuantifiers([]expressions.Quantifier{
				expressions.NamedPhysicalQuantifier(values.UniqueCorrelationIdentifier(), middle),
				expressions.NamedPhysicalQuantifier(values.UniqueCorrelationIdentifier(), other),
			}))
		}
		pm := NewPlanPropertiesMap()
		pm.Add(root)
		assertFreshPlanProperties(t, pm.GetProperties(root), computeWrapperProperties(root))
		for _, step := range steps[:min(len(steps), 48)] {
			scan := scans[(step/12)%2]
			switch step % 12 {
			case 0:
				leaf.Insert(scan)
			case 1:
				leaf.InsertFinal(scan)
			case 2:
				leaf.PruneWith(scan)
			case 3:
				leaf.ClearFinalMembers()
			case 4:
				members := leaf.AllMembers()
				leaf.SetWinner(members[int(step/12)%len(members)])
			case 5:
				survivor := expressions.InitialOf(scan)
				survivor.InsertFinal(scan)
				survivor.Absorb(leaf)
				leaf = survivor
			case 6, 7:
				childProperties := GetRefPlanPropertiesMap(leaf)
				if childProperties == nil || step%12 == 7 {
					childProperties = NewPlanPropertiesMap()
					leaf.SetPlanProperties(childProperties)
				}
				childProperties.Set(leaf.AllMembers()[0], properties.PropertyMap{
					properties.PropDistinctRecords: step/12%2 == 0,
				})
			case 8:
				computeRefPlanProperties(leaf)
			case 9:
				pm.Set(root, properties.PropertyMap{properties.PropDistinctRecords: false})
			case 10:
				childProperties := NewPlanPropertiesMap()
				childProperties.Set(middlePlan, properties.PropertyMap{
					properties.PropDistinctRecords: step/12%2 == 0,
				})
				middle.SetPlanProperties(childProperties)
			case 11:
				middle.SetPlanProperties(nil)
			}
			if !leaf.HasWinner() && len(leaf.FinalMembers()) > 1 {
				leaf.SetWinner(leaf.FinalMembers()[0])
			}
			pm.Add(root)
			current := pm.GetProperties(root)
			assertFreshPlanProperties(t, current, computeWrapperProperties(root))
			pm.Add(root)
			if pm.GetProperties(root).GetDerivations() != current.GetDerivations() {
				t.Fatal("unchanged inputs did not reuse the refreshed derivations")
			}
		}
	})
}

func assertFreshPlanProperties(t *testing.T, got, want properties.PropertyMap) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("property count = %d, fresh derivation has %d", len(got), len(want))
	}
	for property, expected := range want {
		actual, present := got[property]
		equal := false
		switch expected := expected.(type) {
		case *properties.Derivations:
			actual, ok := actual.(*properties.Derivations)
			equal = ok && slices.EqualFunc(actual.ResultValues, expected.ResultValues, values.ValuesStructurallyEqual) &&
				slices.EqualFunc(actual.LocalValues, expected.LocalValues, values.ValuesStructurallyEqual)
		case []values.Value:
			actual, ok := actual.([]values.Value)
			equal = ok && slices.EqualFunc(actual, expected, values.ValuesStructurallyEqual)
		case properties.Cardinalities:
			actual, ok := actual.(properties.Cardinalities)
			equal = ok && actual.Equal(expected)
		default:
			equal = partitionPropValueEqual(actual, expected)
		}
		if !present || !equal {
			t.Fatalf("cached %s = %#v, fresh derivation = %#v", property, actual, expected)
		}
	}
}

func BenchmarkRefPlanPropertiesUnchanged(b *testing.B) {
	ref := expressions.FinalOfAtStage(propertyReuseScan(false), expressions.StagePlanned)
	for range 3 {
		ref = expressions.FinalOfAtStage(propertyReuseFilter(ref), expressions.StagePlanned)
	}
	computeRefPlanProperties(ref)
	b.ReportAllocs()
	for b.Loop() {
		computeRefPlanProperties(ref)
	}
}
