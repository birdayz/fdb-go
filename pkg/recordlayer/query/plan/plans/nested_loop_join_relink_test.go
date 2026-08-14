package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestNestedLoopJoinPlan_ReanchorsBuriedFlatMapSources(t *testing.T) {
	t.Parallel()
	arrayType := values.NewArrayType(true, values.NotNullInt)
	paType := values.NewRecordType("PA", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "ARR", FieldType: arrayType},
	})
	pbType := values.NewRecordType("PB", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "ARR", FieldType: arrayType},
	})
	paAlias := values.NamedCorrelationIdentifier("PA")
	pbAlias := values.NamedCorrelationIdentifier("PB")
	xAlias := values.NamedCorrelationIdentifier("X")
	paScan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"PA"}, paType, false)
	})
	pbScan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"PB"}, pbType, false)
	})
	paQ := expressions.NamedPhysicalQuantifier(
		paAlias, expressions.FinalOfAtStage(paScan, expressions.StageCanonical))
	pbQ := expressions.NamedPhysicalQuantifier(
		pbAlias, expressions.FinalOfAtStage(pbScan, expressions.StageCanonical))
	paRoot, err := paQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	pbRoot, err := pbQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	paArray, err := values.ResolveFieldOrdinals(paRoot, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	explode := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(paArray)
	})
	xQ := expressions.NamedPhysicalQuantifier(
		xAlias, expressions.FinalOfAtStage(explode, expressions.StageCanonical))
	xRoot, err := xQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	flat := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			paQ, xQ, paAlias, xAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "_0", Value: paRoot},
				values.RecordConstructorField{Name: "_1", Value: xRoot}), false)
	})
	flatAlias := values.NamedCorrelationIdentifier("PA_X")
	flatQ := expressions.NamedPhysicalQuantifier(
		flatAlias, expressions.FinalOfAtStage(flat, expressions.StageCanonical))
	flatRoot, err := flatQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(root values.Value, path ...int) values.Value {
		t.Helper()
		field, resolveErr := values.ResolveFieldOrdinals(root, path)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		return field
	}
	join := mustChecked(t, func() (*RecordQueryNestedLoopJoinPlan, error) {
		return NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
			pbQ, flatQ, nil, JoinInner, pbAlias, flatAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "PA_ID", Value: resolve(flatRoot, 0, 0)},
				values.RecordConstructorField{Name: "PA_ARR", Value: resolve(flatRoot, 0, 1)},
				values.RecordConstructorField{Name: "PB_ID", Value: resolve(pbRoot, 0)},
				values.RecordConstructorField{Name: "PB_ARR", Value: resolve(pbRoot, 1)},
				values.RecordConstructorField{Name: "X", Value: resolve(flatRoot, 1)}))
	})
	joinLayout := requireProvidedLayout(t, join)
	if provided, provideErr := values.LayoutProvides(joinLayout, xRoot); provideErr != nil || !provided {
		t.Fatalf("join scalar source window = (%t, %v), want exact propagated X", provided, provideErr)
	}
	windowSources := joinLayout.WindowSources()
	foundScalar := false
	for _, source := range windowSources {
		if source == xRoot {
			foundScalar = true
		}
	}
	if !foundScalar {
		t.Fatalf("join WindowSources = %v, want original exact X source", windowSources)
	}
	assertCarrierPath := func(name string, requested values.Value, want int) {
		t.Helper()
		reanchored, reanchorErr := join.reanchorInputValueToOutput(requested)
		if reanchorErr != nil {
			t.Fatalf("%s: %v", name, reanchorErr)
		}
		field, ok := values.AsFieldValue(reanchored)
		if !ok || field.ChildValue() != joinLayout.Carrier() {
			t.Fatalf("%s root = %T/%v, want exact join carrier %p",
				name, reanchored, field, joinLayout.Carrier())
		}
		path := field.Path().Ordinals()
		if len(path) != 1 || path[0] != want {
			t.Fatalf("%s path = %v, want [%d]", name, path, want)
		}
	}

	// X is a bare scalar QOV inside the retained FlatMap. PA.ID is a named
	// source one level below that same child. Both must cross the child producer
	// before the join's flattened result program can select their output slots.
	assertCarrierPath("bare explode element", xRoot, 4)
	assertCarrierPath("buried table field", resolve(paRoot, 0), 0)

	foreign, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("FOREIGN_X"), values.NotNullInt)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := join.reanchorInputValueToOutput(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != foreign {
		t.Fatal("foreign same-typed scalar was guessed into the retained FlatMap")
	}
	foreignPA, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("FOREIGN_PA"), paType)
	if err != nil {
		t.Fatal(err)
	}
	foreignPAID := resolve(foreignPA, 0)
	unchanged, err = join.reanchorInputValueToOutput(foreignPAID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != foreignPAID {
		t.Fatalf("foreign same-shaped record field was guessed into the retained FlatMap: %s",
			values.ExplainValue(unchanged))
	}
	wrongType, err := values.NewQuantifiedObjectValue(xAlias, values.NotNullLong)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err = join.reanchorInputValueToOutput(wrongType)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != wrongType {
		t.Fatal("same-alias scalar with a different exact type crossed child lineage")
	}

	// If both selected children expose the same buried source, neither is a
	// unique lineage authority. Reusing the FlatMap on both sides makes that
	// mutation explicit without relying on field display names.
	leftAlias := values.NamedCorrelationIdentifier("LEFT_FLAT")
	rightAlias := values.NamedCorrelationIdentifier("RIGHT_FLAT")
	leftQ := expressions.NamedPhysicalQuantifier(
		leftAlias, expressions.FinalOfAtStage(flat, expressions.StageCanonical))
	rightQ := expressions.NamedPhysicalQuantifier(
		rightAlias, expressions.FinalOfAtStage(flat, expressions.StageCanonical))
	leftRoot, err := leftQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	rightRoot, err := rightQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	ambiguous := mustChecked(t, func() (*RecordQueryNestedLoopJoinPlan, error) {
		return NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
			leftQ, rightQ, nil, JoinInner, leftAlias, rightAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "LEFT", Value: leftRoot},
				values.RecordConstructorField{Name: "RIGHT", Value: rightRoot}))
	})
	unchanged, err = ambiguous.reanchorInputValueToOutput(xRoot)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != xRoot {
		t.Fatal("two retained children claimed the same buried scalar; join guessed an owner")
	}
}

func TestFlatMapPlan_PropagatesRetainedScalarFromSelectedOuterMaterializer(t *testing.T) {
	t.Parallel()
	arrayType := values.NewArrayType(true, values.NotNullInt)
	baseType := values.NewRecordType("BASE", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "ARR", FieldType: arrayType},
	})
	probeType := values.NewRecordType("PROBE", false, []values.Field{
		{Name: "VID", FieldType: values.NotNullLong},
	})
	baseAlias := values.NamedCorrelationIdentifier("BASE")
	xAlias := values.NamedCorrelationIdentifier("X")
	mergedAlias := values.NamedCorrelationIdentifier("BASE_X")
	probeAlias := values.NamedCorrelationIdentifier("V")
	baseScan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"BASE"}, baseType, false)
	})
	probeScan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"PROBE"}, probeType, false)
	})
	baseQ := expressions.NamedPhysicalQuantifier(
		baseAlias, expressions.FinalOfAtStage(baseScan, expressions.StageCanonical))
	baseRoot, err := baseQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	arrayValue, err := values.ResolveFieldOrdinals(baseRoot, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	explode := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(arrayValue)
	})
	xQ := expressions.NamedPhysicalQuantifier(
		xAlias, expressions.FinalOfAtStage(explode, expressions.StageCanonical))
	xRoot, err := xQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	lower := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			baseQ, xQ, baseAlias, xAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "_0", Value: baseRoot},
				values.RecordConstructorField{Name: "_1", Value: xRoot}), false)
	})
	lowerQ := expressions.NamedPhysicalQuantifier(
		mergedAlias, expressions.FinalOfAtStage(lower, expressions.StageCanonical))
	lowerRoot, err := lowerQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	probeQ := expressions.NamedPhysicalQuantifier(
		probeAlias, expressions.FinalOfAtStage(probeScan, expressions.StageCanonical))
	probeRoot, err := probeQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(root values.Value, path ...int) values.Value {
		t.Helper()
		field, resolveErr := values.ResolveFieldOrdinals(root, path)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		return field
	}
	upper := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			lowerQ, probeQ, mergedAlias, probeAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "ID", Value: resolve(lowerRoot, 0, 0)},
				values.RecordConstructorField{Name: "X", Value: resolve(lowerRoot, 1)},
				values.RecordConstructorField{Name: "VID", Value: resolve(probeRoot, 0)}), false)
	})
	upperLayout := requireProvidedLayout(t, upper)
	provided, provideErr := values.LayoutProvides(upperLayout, xRoot)
	if provideErr != nil || !provided {
		t.Fatalf("upper FlatMap scalar source = (%t, %v), windows=%v, want exact retained X",
			provided, provideErr, upperLayout.WindowSources())
	}
	foundScalar := false
	for _, source := range upperLayout.WindowSources() {
		if source == xRoot {
			foundScalar = true
			break
		}
	}
	if !foundScalar {
		t.Fatalf("upper FlatMap windows = %v, want pointer-exact retained X", upperLayout.WindowSources())
	}

	// An existential identity wrapper can deliberately reuse X for its complete
	// outer row while the selected child still publishes the earlier scalar X.
	// The two declarations are distinguished by their exact types. A bare QOV
	// result has no producer fields for generic reanchoring to inspect, so this
	// pins the explicit declared-edge translation used by the FlatMap
	// constructor.
	identityOuterQ := expressions.NamedPhysicalQuantifier(
		xAlias, expressions.FinalOfAtStage(upper, expressions.StageCanonical))
	identityOuterRoot, err := identityOuterQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	identityInnerAlias := values.NamedCorrelationIdentifier("EXISTS_PROBE")
	identityInnerQ := expressions.NamedPhysicalQuantifier(
		identityInnerAlias, expressions.FinalOfAtStage(probeScan, expressions.StageCanonical))
	identity := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			identityOuterQ, identityInnerQ, xAlias, identityInnerAlias,
			identityOuterRoot, false)
	})
	identityLayout := requireProvidedLayout(t, identity)
	provided, provideErr = values.LayoutProvides(identityLayout, xRoot)
	if provideErr != nil || !provided {
		t.Fatalf("identity FlatMap scalar source = (%t, %v), windows=%v, want retained X",
			provided, provideErr, identityLayout.WindowSources())
	}
	foundScalar = false
	for _, source := range identityLayout.WindowSources() {
		if source == xRoot {
			foundScalar = true
			break
		}
	}
	if !foundScalar {
		t.Fatalf("identity FlatMap windows = %v, want pointer-exact scalar X beside whole-row X",
			identityLayout.WindowSources())
	}
	wrongX, err := values.NewQuantifiedObjectValue(xAlias, values.NotNullLong)
	if err != nil {
		t.Fatal(err)
	}
	if wrongProvided, wrongErr := values.LayoutProvides(identityLayout, wrongX); wrongErr == nil || wrongProvided {
		t.Fatalf("same-alias wrong-type source = (%t, %v), want loud exact-type rejection",
			wrongProvided, wrongErr)
	}

	foreign, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("FOREIGN"), xRoot.FlowedType())
	if err != nil {
		t.Fatal(err)
	}
	if foreignProvided, foreignErr := values.LayoutProvides(upperLayout, foreign); foreignErr == nil || foreignProvided {
		t.Fatalf("foreign scalar source = (%t, %v), want loud missing-source rejection", foreignProvided, foreignErr)
	}
	if sourcePath := resolve(lowerRoot, 1); sourcePath == nil || xRoot.Correlation() != xAlias {
		t.Fatal("retained-source proof mutated its source values")
	}
}

func TestFlatMapPlan_PropagatesRetainedRecordFromSelectedOuterMaterializer(t *testing.T) {
	t.Parallel()
	elementType := values.NewRecordType("ELEM", false, []values.Field{
		{Name: "EK", FieldType: values.NullableLong},
		{Name: "PAYLOAD", FieldType: values.NullableString},
	})
	baseType := values.NewRecordType("BASE_RECORD", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "ARR", FieldType: values.NewArrayType(true, elementType)},
	})
	probeType := values.NewRecordType("PROBE_RECORD", false, []values.Field{
		{Name: "VID", FieldType: values.NotNullLong},
	})
	baseAlias := values.NamedCorrelationIdentifier("BASE_RECORD")
	elementAlias := values.NamedCorrelationIdentifier("X")
	mergedAlias := values.NamedCorrelationIdentifier("BASE_X_RECORD")
	probeAlias := values.NamedCorrelationIdentifier("V_RECORD")
	baseScan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"BASE_RECORD"}, baseType, false)
	})
	probeScan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"PROBE_RECORD"}, probeType, false)
	})
	baseQ := expressions.NamedPhysicalQuantifier(
		baseAlias, expressions.FinalOfAtStage(baseScan, expressions.StageCanonical))
	baseRoot, err := baseQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	arrayValue, err := values.ResolveFieldOrdinals(baseRoot, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	explode := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(arrayValue)
	})
	elementQ := expressions.NamedPhysicalQuantifier(
		elementAlias, expressions.FinalOfAtStage(explode, expressions.StageCanonical))
	elementRoot, err := elementQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(root values.Value, path ...int) values.Value {
		t.Helper()
		field, resolveErr := values.ResolveFieldOrdinals(root, path)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		return field
	}
	pinnedBaseID, err := values.ResolveOrdinalSeedAccess(baseRoot, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	lower := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			baseQ, elementQ, baseAlias, elementAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "ID", Value: pinnedBaseID},
				values.RecordConstructorField{Name: "X", Value: elementRoot}), false)
	})
	lowerLayout := requireProvidedLayout(t, lower)
	if provided, provideErr := values.LayoutProvides(lowerLayout, elementRoot); provideErr != nil || !provided {
		t.Fatalf("lower FlatMap record source = (%t, %v), windows=%v, want exact X",
			provided, provideErr, lowerLayout.WindowSources())
	}
	lowerQ := expressions.NamedPhysicalQuantifier(
		mergedAlias, expressions.FinalOfAtStage(lower, expressions.StageCanonical))
	lowerRoot, err := lowerQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	probeQ := expressions.NamedPhysicalQuantifier(
		probeAlias, expressions.FinalOfAtStage(probeScan, expressions.StageCanonical))
	probeRoot, err := probeQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	retainedElement := resolve(lowerRoot, 1)
	upper := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			lowerQ, probeQ, mergedAlias, probeAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "ID", Value: resolve(lowerRoot, 0)},
				values.RecordConstructorField{Name: "VID", Value: resolve(probeRoot, 0)},
				values.RecordConstructorField{Name: "X", Value: retainedElement}), false)
	})
	upperLayout := requireProvidedLayout(t, upper)
	if provided, provideErr := values.LayoutProvides(upperLayout, elementRoot); provideErr != nil || !provided {
		t.Fatalf("upper FlatMap record source = (%t, %v), windows=%v, want exact retained X",
			provided, provideErr, upperLayout.WindowSources())
	}
	if nullSupplying, nullErr := values.LayoutWindowNullSupplying(upperLayout, elementRoot); nullErr != nil || nullSupplying {
		t.Fatalf("upper FlatMap record source null-supplying = (%t, %v), want false",
			nullSupplying, nullErr)
	}

	// The existential wrapper deliberately reuses X for its complete three-field
	// outer row. The earlier X element record must remain a second exact binding
	// under that text, reached through the unique output object slot rather than
	// being shadowed by the whole row.
	identityOuterQ := expressions.NamedPhysicalQuantifier(
		elementAlias, expressions.FinalOfAtStage(upper, expressions.StageCanonical))
	identityOuterRoot, err := identityOuterQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	identityInnerAlias := values.NamedCorrelationIdentifier("EXISTS_RECORD_PROBE")
	identityInnerQ := expressions.NamedPhysicalQuantifier(
		identityInnerAlias, expressions.FinalOfAtStage(probeScan, expressions.StageCanonical))
	identity := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			identityOuterQ, identityInnerQ, elementAlias, identityInnerAlias,
			identityOuterRoot, false)
	})
	identityLayout := requireProvidedLayout(t, identity)
	if provided, provideErr := values.LayoutProvides(identityLayout, elementRoot); provideErr != nil || !provided {
		t.Fatalf("identity FlatMap record source = (%t, %v), windows=%v, want retained X",
			provided, provideErr, identityLayout.WindowSources())
	}
	foundRecord := false
	for _, source := range identityLayout.WindowSources() {
		if source == elementRoot {
			foundRecord = true
			break
		}
	}
	if !foundRecord {
		t.Fatalf("identity FlatMap windows = %v, want pointer-exact record X beside whole-row X",
			identityLayout.WindowSources())
	}
	wrongType := values.NewRecordType("OTHER_ELEM", false, []values.Field{
		{Name: "EK", FieldType: values.NullableLong},
		{Name: "PAYLOAD", FieldType: values.NullableString},
	})
	wrongX, err := values.NewQuantifiedObjectValue(elementAlias, wrongType)
	if err != nil {
		t.Fatal(err)
	}
	if provided, provideErr := values.LayoutProvides(identityLayout, wrongX); provideErr == nil || provided {
		t.Fatalf("same-alias wrong-record source = (%t, %v), want loud exact-type rejection",
			provided, provideErr)
	}
	foreign, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("FOREIGN_RECORD"), elementRoot.FlowedType())
	if err != nil {
		t.Fatal(err)
	}
	if provided, provideErr := values.LayoutProvides(identityLayout, foreign); provideErr == nil || provided {
		t.Fatalf("foreign record source = (%t, %v), want loud missing-source rejection",
			provided, provideErr)
	}
	if original, ok := values.AsFieldValue(retainedElement); !ok || original.ChildValue() != lowerRoot {
		t.Fatal("retained record proof mutated its selected child source path")
	}
}

func TestNestedLoopJoinPlan_DirectLegOwnerWinsOverRetainedChildDuplicateName(t *testing.T) {
	t.Parallel()

	empType := values.NewRecordType("EMP", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong},
		{Name: "FNAME", FieldType: values.NullableString},
		{Name: "DEPT_ID", FieldType: values.NullableLong},
	})
	deptType := values.NewRecordType("DEPT", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong},
		{Name: "NAME", FieldType: values.NullableString},
	})
	projectType := values.NewRecordType("PROJECT", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong},
		{Name: "NAME", FieldType: values.NullableString},
		{Name: "EMP_ID", FieldType: values.NullableLong},
	})
	empAlias := values.NamedCorrelationIdentifier("E")
	deptAlias := values.NamedCorrelationIdentifier("D")
	projectAlias := values.NamedCorrelationIdentifier("P")
	boxAlias := values.NamedCorrelationIdentifier("D$BOX")

	newLeg := func(alias values.CorrelationIdentifier, typ values.Type) (expressions.Quantifier, values.QuantifiedObjectValue) {
		t.Helper()
		scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
			return NewRecordQueryScanPlan([]string{alias.Name()}, typ, false)
		})
		q := expressions.NamedPhysicalQuantifier(
			alias, expressions.FinalOfAtStage(scan, expressions.StageCanonical))
		root, err := q.RequireFlowedObjectValue()
		if err != nil {
			t.Fatal(err)
		}
		return q, root
	}
	resolve := func(root values.Value, path ...int) values.Value {
		t.Helper()
		field, err := values.ResolveFieldOrdinals(root, path)
		if err != nil {
			t.Fatal(err)
		}
		return field
	}

	empQ, empRoot := newLeg(empAlias, empType)
	deptQ, deptRoot := newLeg(deptAlias, deptType)
	projectQ, projectRoot := newLeg(projectAlias, projectType)
	lower := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			empQ, deptQ, empAlias, deptAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "ID", Value: resolve(empRoot, 0)},
				values.RecordConstructorField{Name: "FNAME", Value: resolve(empRoot, 1)},
				values.RecordConstructorField{Name: "DEPT_ID", Value: resolve(empRoot, 2)},
				values.RecordConstructorField{Name: "ID", Value: resolve(deptRoot, 0)},
				values.RecordConstructorField{Name: "NAME", Value: resolve(deptRoot, 1)}), false)
	})
	lowerQ := expressions.NamedPhysicalQuantifier(
		boxAlias, expressions.FinalOfAtStage(lower, expressions.StageCanonical))
	lowerRoot, err := lowerQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	join := mustChecked(t, func() (*RecordQueryNestedLoopJoinPlan, error) {
		return NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
			lowerQ, projectQ, nil, JoinLeftOuter, boxAlias, projectAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "ID", Value: resolve(lowerRoot, 0)},
				values.RecordConstructorField{Name: "FNAME", Value: resolve(lowerRoot, 1)},
				values.RecordConstructorField{Name: "DEPT_ID", Value: resolve(lowerRoot, 2)},
				values.RecordConstructorField{Name: "ID", Value: resolve(lowerRoot, 3)},
				values.RecordConstructorField{Name: "NAME", Value: resolve(lowerRoot, 4)},
				values.RecordConstructorField{Name: "ID", Value: resolve(projectRoot, 0)},
				values.RecordConstructorField{Name: "NAME", Value: resolve(projectRoot, 1)},
				values.RecordConstructorField{Name: "EMP_ID", Value: resolve(projectRoot, 2)}))
	})
	joinLayout := requireProvidedLayout(t, join)
	empFName := resolve(empRoot, 1)
	deptName := resolve(deptRoot, 1)
	projectName := resolve(projectRoot, 1)
	projection := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithOutputSchema(
			[]values.Value{empFName, deptName, projectName},
			nil, nil, []string{"E.FNAME", "D.NAME", "P.NAME"}, join)
	})
	for i, want := range []int{1, 4, 6} {
		field, ok := values.AsFieldValue(projection.GetProjections()[i])
		if !ok || field.ChildValue() != joinLayout.Carrier() {
			t.Fatalf("projection %d root = %T/%v, want exact join carrier %p",
				i, projection.GetProjections()[i], field, joinLayout.Carrier())
		}
		if path := field.Path().Ordinals(); len(path) != 1 || path[0] != want {
			t.Fatalf("projection %d path = %v, want [%d]", i, path, want)
		}
	}
	for i, original := range []values.Value{empFName, deptName, projectName} {
		field, ok := values.AsFieldValue(original)
		wantRoot := []values.QuantifiedObjectValue{empRoot, deptRoot, projectRoot}[i]
		if !ok || field.ChildValue() != wantRoot {
			t.Fatalf("projection construction mutated source field %d", i)
		}
	}

	foreignRoot, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("FOREIGN_PROJECT"), projectType)
	if err != nil {
		t.Fatal(err)
	}
	foreignName := resolve(foreignRoot, 1)
	unchanged, err := join.reanchorInputValueToOutput(foreignName)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != foreignName {
		t.Fatal("foreign duplicate NAME was claimed by a retained child")
	}

	driftedProjectType := values.NewRecordType("PROJECT", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong},
		{Name: "NAME", FieldType: values.NotNullString},
		{Name: "EMP_ID", FieldType: values.NullableLong},
	})
	driftedRoot, err := values.NewQuantifiedObjectValue(projectAlias, driftedProjectType)
	if err != nil {
		t.Fatal(err)
	}
	driftedName := resolve(driftedRoot, 1)
	unchanged, err = join.reanchorInputValueToOutput(driftedName)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != driftedName {
		t.Fatal("same-alias duplicate NAME with nullability drift crossed join lineage")
	}
}

func TestNestedLoopJoinPlan_ExactOutputRelinkIsIdempotentWithDuplicateNames(t *testing.T) {
	t.Parallel()

	arrayType := values.NewArrayType(false, values.NotNullInt)
	pType := values.NewRecordType("P", false, []values.Field{
		{Name: "PID", FieldType: values.NotNullLong},
		{Name: "K", FieldType: values.NullableLong},
		{Name: "ARR", FieldType: arrayType},
	})
	qType := values.NewRecordType("Q", false, []values.Field{
		{Name: "QID", FieldType: values.NotNullLong},
		{Name: "K", FieldType: values.NullableLong},
	})
	pAlias := values.NamedCorrelationIdentifier("P")
	qAlias := values.NamedCorrelationIdentifier("Q")
	elAlias := values.NamedCorrelationIdentifier("EL")
	boxAlias := values.NamedCorrelationIdentifier("P$BOX")

	newScanLeg := func(alias values.CorrelationIdentifier, typ values.Type) (expressions.Quantifier, values.QuantifiedObjectValue) {
		t.Helper()
		scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
			return NewRecordQueryScanPlan([]string{alias.Name()}, typ, false)
		})
		q := expressions.NamedPhysicalQuantifier(
			alias, expressions.FinalOfAtStage(scan, expressions.StageCanonical))
		root, err := q.RequireFlowedObjectValue()
		if err != nil {
			t.Fatal(err)
		}
		return q, root
	}
	resolve := func(root values.Value, path ...int) values.Value {
		t.Helper()
		field, err := values.ResolveFieldOrdinals(root, path)
		if err != nil {
			t.Fatal(err)
		}
		return field
	}

	pQ, pRoot := newScanLeg(pAlias, pType)
	qQ, qRoot := newScanLeg(qAlias, qType)
	explode := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(resolve(pRoot, 2))
	})
	elQ := expressions.NamedPhysicalQuantifier(
		elAlias, expressions.FinalOfAtStage(explode, expressions.StageCanonical))
	elRoot, err := elQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	flat := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			pQ, elQ, pAlias, elAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "_0", Value: pRoot},
				values.RecordConstructorField{Name: "_1", Value: elRoot}), false)
	})
	flatQ := expressions.NamedPhysicalQuantifier(
		boxAlias, expressions.FinalOfAtStage(flat, expressions.StageCanonical))
	flatRoot, err := flatQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	join := mustChecked(t, func() (*RecordQueryNestedLoopJoinPlan, error) {
		return NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
			flatQ, qQ, nil, JoinInner, boxAlias, qAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "PID", Value: resolve(flatRoot, 0, 0)},
				values.RecordConstructorField{Name: "K", Value: resolve(flatRoot, 0, 1)},
				values.RecordConstructorField{Name: "ARR", Value: resolve(flatRoot, 0, 2)},
				values.RecordConstructorField{Name: "QID", Value: resolve(qRoot, 0)},
				values.RecordConstructorField{Name: "K", Value: resolve(qRoot, 1)},
				values.RecordConstructorField{Name: "EL", Value: resolve(flatRoot, 1)}))
	})
	joinLayout := requireProvidedLayout(t, join)
	assertCarrierPath := func(name string, value values.Value, want int) values.Value {
		t.Helper()
		field, ok := values.AsFieldValue(value)
		if !ok || field.ChildValue() != joinLayout.Carrier() {
			t.Fatalf("%s root = %T/%v, want exact join carrier %p",
				name, value, field, joinLayout.Carrier())
		}
		path := field.Path().Ordinals()
		if len(path) != 1 || path[0] != want {
			t.Fatalf("%s path = %v, want [%d]", name, path, want)
		}
		return value
	}

	// The first pass crosses P's selected FlatMap child and produces output K#1.
	// The second pass models extraction relinking the same InMemorySort. It must
	// not reinterpret that final ordinal through the NLJ result RC and select the
	// other one-step K at Q.K#4.
	pK := resolve(pRoot, 1)
	first, err := join.reanchorInputValueToOutput(pK)
	if err != nil {
		t.Fatal(err)
	}
	assertCarrierPath("first P.K relink", first, 1)
	second, err := join.reanchorInputValueToOutput(first)
	if err != nil {
		t.Fatal(err)
	}
	assertCarrierPath("idempotent P.K relink", second, 1)
	if second != first {
		t.Fatal("exact output relink rebuilt an already admitted field")
	}
	if original, ok := values.AsFieldValue(pK); !ok || original.ChildValue() != pRoot ||
		original.Path().Ordinals()[0] != 1 {
		t.Fatal("relink mutated the source P.K field")
	}

	// The idempotence gate is pointer-exact. A separately minted current carrier
	// with the same row type, a named foreign root, a type-drifted current root,
	// and a mixed-owner program all remain outside it.
	freshCurrent := func(typ values.Type) values.QuantifiedObjectValue {
		t.Helper()
		layout, layoutErr := newIdentityOutputLayout(typ)
		if layoutErr != nil {
			t.Fatal(layoutErr)
		}
		return layout.Carrier()
	}
	independentCurrent := freshCurrent(join.GetResultType())
	independentK := resolve(independentCurrent, 1)
	if valueReferencesOnlyExactQOV(independentK, joinLayout.Carrier()) {
		t.Fatal("independently minted same-shaped current carrier passed exact-output admission")
	}
	foreign := mustOrdinalLayoutQOV(
		t, values.NamedCorrelationIdentifier("FOREIGN"), join.GetResultType())
	foreignK := resolve(foreign, 1)
	if valueReferencesOnlyExactQOV(foreignK, joinLayout.Carrier()) {
		t.Fatal("foreign named carrier passed exact-output admission")
	}
	unchanged, err := join.reanchorInputValueToOutput(foreignK)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != foreignK {
		t.Fatal("foreign named K was claimed by duplicate NLJ output names")
	}
	driftedType := values.NewRecordType("", false, []values.Field{
		{Name: "PID", FieldType: values.NotNullLong},
		{Name: "K", FieldType: values.NotNullLong},
		{Name: "ARR", FieldType: arrayType},
		{Name: "QID", FieldType: values.NotNullLong},
		{Name: "QK", FieldType: values.NullableLong},
		{Name: "EL", FieldType: values.NotNullInt},
	})
	driftedCurrent := freshCurrent(driftedType)
	driftedK := resolve(driftedCurrent, 1)
	if valueReferencesOnlyExactQOV(driftedK, joinLayout.Carrier()) {
		t.Fatal("type-drifted current carrier passed exact-output admission")
	}
	nestedForeignType := values.NewRecordType("FOREIGN_NESTED", false, []values.Field{{
		Name: "N", FieldType: values.NewRecordType("N", false, []values.Field{{
			Name: "K", FieldType: values.NullableLong,
		}}),
	}})
	nestedForeign := mustOrdinalLayoutQOV(
		t, values.NamedCorrelationIdentifier("FOREIGN_NESTED"), nestedForeignType)
	nestedForeignK := resolve(nestedForeign, 0, 0)
	unchanged, err = join.reanchorInputValueToOutput(nestedForeignK)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != nestedForeignK {
		t.Fatal("foreign nested K path was guessed into a flat duplicate output")
	}
	mixed := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "OWN", Value: first},
		values.RecordConstructorField{Name: "FOREIGN", Value: foreignK})
	if valueReferencesOnlyExactQOV(mixed, joinLayout.Carrier()) {
		t.Fatal("mixed exact and foreign owners passed exact-output admission")
	}
}

func TestFlatMapPlan_ReanchorsSourceThroughNestedLoopJoinChildBinding(t *testing.T) {
	t.Parallel()
	leftType := values.NewRecordType("LEFT_SOURCE", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
	})
	rightType := values.NewRecordType("RIGHT_SOURCE", false, []values.Field{
		{Name: "RID", FieldType: values.NotNullLong},
	})
	innerType := values.NewRecordType("INNER_SOURCE", false, []values.Field{
		{Name: "K", FieldType: values.NotNullLong},
	})
	leftAlias := values.NamedCorrelationIdentifier("LEFT_SOURCE")
	rightAlias := values.NamedCorrelationIdentifier("RIGHT_SOURCE")
	innerAlias := values.NamedCorrelationIdentifier("INNER_SOURCE")
	boxAlias := values.NamedCorrelationIdentifier("BOX")
	left := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"LEFT_SOURCE"}, leftType, false)
	})
	right := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"RIGHT_SOURCE"}, rightType, false)
	})
	inner := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"INNER_SOURCE"}, innerType, false)
	})
	leftQ := expressions.NamedPhysicalQuantifier(
		leftAlias, expressions.FinalOfAtStage(left, expressions.StageCanonical))
	rightQ := expressions.NamedPhysicalQuantifier(
		rightAlias, expressions.FinalOfAtStage(right, expressions.StageCanonical))
	innerQ := expressions.NamedPhysicalQuantifier(
		innerAlias, expressions.FinalOfAtStage(inner, expressions.StageCanonical))
	leftRoot, err := leftQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	rightRoot, err := rightQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	innerRoot, err := innerQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	box := mustChecked(t, func() (*RecordQueryNestedLoopJoinPlan, error) {
		return NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
			leftQ, rightQ, nil, JoinLeftOuter, leftAlias, rightAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "LEFT", Value: leftRoot},
				values.RecordConstructorField{Name: "RIGHT", Value: rightRoot}))
	})
	boxQ := expressions.NamedPhysicalQuantifier(
		boxAlias, expressions.FinalOfAtStage(box, expressions.StageCanonical))
	boxRoot, err := boxQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	flat := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			boxQ, innerQ, boxAlias, innerAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "BOX", Value: boxRoot},
				values.RecordConstructorField{Name: "INNER", Value: innerRoot}), false)
	})
	leftID, err := values.ResolveFieldOrdinals(leftRoot, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	reanchored, err := flat.reanchorInputValueToOutput(leftID)
	if err != nil {
		t.Fatal(err)
	}
	field, ok := values.AsFieldValue(reanchored)
	flatLayout := requireProvidedLayout(t, flat)
	if !ok || field.ChildValue() != flatLayout.Carrier() {
		t.Fatalf("buried LEFT_SOURCE.ID root = %T, want exact FlatMap carrier", reanchored)
	}
	if path := field.Path().Ordinals(); len(path) != 3 ||
		path[0] != 0 || path[1] != 0 || path[2] != 0 {
		t.Fatalf("buried LEFT_SOURCE.ID path = %v, want [0 0 0]", path)
	}
	if original, ok := values.AsFieldValue(leftID); !ok || original.ChildValue() != leftRoot {
		t.Fatal("source field was mutated")
	}

	// A separately minted current row with the same exact shape is not the
	// selected box carrier. The child-to-binding bridge is pointer-exact and
	// must leave it foreign.
	boxClone := mustChecked(t, func() (*RecordQueryNestedLoopJoinPlan, error) {
		return NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
			leftQ, rightQ, nil, JoinLeftOuter, leftAlias, rightAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "LEFT", Value: leftRoot},
				values.RecordConstructorField{Name: "RIGHT", Value: rightRoot}))
	})
	foreignCurrent := requireProvidedLayout(t, boxClone).Carrier()
	foreignField, err := values.ResolveFieldOrdinals(foreignCurrent, []int{0, 0})
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := translateFlatMapChildOutputToBinding(foreignField, box, boxAlias)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != foreignField {
		t.Fatal("same-shaped foreign current row crossed the FlatMap child binding")
	}
}

func TestFlatMapPlan_ReanchorsSelectedOrdinalityBindingNames(t *testing.T) {
	t.Parallel()
	outerType := values.NewRecordType("OUTER", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
	})
	outerAlias := values.NamedCorrelationIdentifier("OUTER")
	innerAlias := values.NamedCorrelationIdentifier("X")
	outer := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"OUTER"}, outerType, false)
	})
	outerQ := expressions.NamedPhysicalQuantifier(
		outerAlias, expressions.FinalOfAtStage(outer, expressions.StageCanonical))
	outerRoot, err := outerQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	array := values.NewArrayConstructorValue(values.NullableLong, []values.Value{
		values.LiteralValue(int64(1)),
	})
	explode := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlanWithOrdinality(array, true)
	})
	filter := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(explode, nil)
	})
	logicalType := values.NewRecordType("", false, []values.Field{
		{Name: "X", FieldType: values.NullableLong},
		{Name: "O", FieldType: values.NotNullInt},
	})
	logicalRoot, err := values.NewQuantifiedObjectValue(innerAlias, logicalType)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name  string
		inner RecordQueryPlan
	}{
		{name: "direct explode", inner: explode},
		{name: "transparent filter", inner: filter},
	} {
		t.Run(test.name, func(t *testing.T) {
			innerQ := expressions.NamedPhysicalQuantifier(
				innerAlias, expressions.FinalOfAtStage(test.inner, expressions.StageCanonical))
			physicalRoot, requireErr := innerQ.RequireFlowedObjectValue()
			if requireErr != nil {
				t.Fatal(requireErr)
			}
			flat := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
				return NewRecordQueryFlatMapPlanFromQuantifiers(
					outerQ, innerQ, outerAlias, innerAlias,
					values.NewRawRecordConstructorValue(
						values.RecordConstructorField{Name: "_0", Value: outerRoot},
						values.RecordConstructorField{Name: "_1", Value: physicalRoot}), false)
			})
			layout := requireProvidedLayout(t, flat)
			for ordinal, name := range []string{"X", "O"} {
				requested, resolveErr := values.ResolveFieldOrdinals(logicalRoot, []int{ordinal})
				if resolveErr != nil {
					t.Fatal(resolveErr)
				}
				reanchored, reanchorErr := flat.reanchorInputValueToOutput(requested)
				if reanchorErr != nil {
					t.Fatalf("%s: %v", name, reanchorErr)
				}
				field, ok := values.AsFieldValue(reanchored)
				if !ok || field.ChildValue() != layout.Carrier() {
					t.Fatalf("%s root = %T, want exact FlatMap carrier", name, reanchored)
				}
				path := field.Path().Ordinals()
				if len(path) != 2 || path[0] != 1 || path[1] != ordinal {
					t.Fatalf("%s path = %v, want [1 %d]", name, path, ordinal)
				}
				original, ok := values.AsFieldValue(requested)
				if !ok || original.ChildValue() != logicalRoot {
					t.Fatalf("%s source Value was mutated", name)
				}
			}
		})
	}

	// Authored AS/AT names remain the primary lineage authority even when one
	// spells the other physical Explode slot. The physical carrier is [_0,_1],
	// but AS "_1" AT "O" must map element/ordinal to distinct FlatMap output
	// ordinals. Normalizing O to physical _1 before consulting the retained
	// result program would collapse both onto the element slot.
	collisionType := values.NewRecordType("", false, []values.Field{
		{Name: "_1", FieldType: values.NullableLong},
		{Name: "O", FieldType: values.NotNullInt},
	})
	collisionRoot, err := values.NewQuantifiedObjectValue(innerAlias, collisionType)
	if err != nil {
		t.Fatal(err)
	}
	resolveOrdinal := func(root values.Value, ordinal int) values.Value {
		t.Helper()
		resolved, resolveErr := values.ResolveFieldOrdinals(root, []int{ordinal})
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		return resolved
	}
	collisionInnerQ := expressions.NamedPhysicalQuantifier(
		innerAlias, expressions.FinalOfAtStage(explode, expressions.StageCanonical))
	collisionFlat := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			outerQ, collisionInnerQ, outerAlias, innerAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "ID", Value: outerRoot},
				values.RecordConstructorField{Name: "_1", Value: resolveOrdinal(collisionRoot, 0)},
				values.RecordConstructorField{Name: "O", Value: resolveOrdinal(collisionRoot, 1)}), false)
	})
	collisionLayout := requireProvidedLayout(t, collisionFlat)
	for ordinal, want := range []int{1, 2} {
		requested := resolveOrdinal(collisionRoot, ordinal)
		got, reanchorErr := collisionFlat.reanchorInputValueToOutput(requested)
		if reanchorErr != nil {
			t.Fatal(reanchorErr)
		}
		field, ok := values.AsFieldValue(got)
		if !ok || field.ChildValue() != collisionLayout.Carrier() {
			t.Fatalf("collision slot %d root = %T, want exact FlatMap carrier", ordinal, got)
		}
		if path := field.Path().Ordinals(); len(path) != 1 || path[0] != want {
			t.Fatalf("collision slot %d path = %v, want [%d]", ordinal, path, want)
		}
	}

	// A later extraction pass has already normalized the logical AS/AT row to
	// the selected Explode's physical type. The projection can therefore carry
	// either a field on that row or the bare scalar element QOV retained by the
	// FlatMap result program. Both must still cross the producer boundary.
	physicalInnerQ := expressions.NamedPhysicalQuantifier(
		innerAlias, expressions.FinalOfAtStage(explode, expressions.StageCanonical))
	scalarElement, err := values.NewQuantifiedObjectValue(innerAlias, values.NullableLong)
	if err != nil {
		t.Fatal(err)
	}
	flatWithScalar := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			outerQ, physicalInnerQ, outerAlias, innerAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "_0", Value: outerRoot},
				values.RecordConstructorField{Name: "_1", Value: scalarElement}), false)
	})
	flatWithScalarLayout := requireProvidedLayout(t, flatWithScalar)
	reanchoredScalar, err := flatWithScalar.reanchorInputValueToOutput(scalarElement)
	if err != nil {
		t.Fatal(err)
	}
	scalarField, ok := values.AsFieldValue(reanchoredScalar)
	if !ok || scalarField.ChildValue() != flatWithScalarLayout.Carrier() {
		t.Fatalf("bare scalar root = %T, want exact FlatMap carrier", reanchoredScalar)
	}
	if path := scalarField.Path().Ordinals(); len(path) != 1 || path[0] != 1 {
		t.Fatalf("bare scalar path = %v, want [1]", path)
	}

	// A projection built while its memo edge was exploratory can retain the
	// physical Explode binding rather than the FlatMap's output carrier. At
	// extraction both old and replacement edges are selected. WithChildren must
	// cross the replacement producer exactly once; otherwise EL._0 survives
	// above a later Sort as an unbound EL QOV.
	physicalBinding, err := values.NewQuantifiedObjectValue(
		innerAlias, requireProvidedLayout(t, explode).Carrier().FlowedType())
	if err != nil {
		t.Fatal(err)
	}
	physicalElement, err := values.ResolveFieldOrdinals(physicalBinding, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	buildRecordFlat := func() *RecordQueryFlatMapPlan {
		t.Helper()
		return mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
			return NewRecordQueryFlatMapPlanFromQuantifiers(
				outerQ, physicalInnerQ, outerAlias, innerAlias,
				values.NewRawRecordConstructorValue(
					values.RecordConstructorField{Name: "_0", Value: outerRoot},
					values.RecordConstructorField{Name: "_1", Value: physicalBinding}), false)
		})
	}
	oldFlat := buildRecordFlat()
	newFlat := buildRecordFlat()
	staleProjection := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return newRecordQueryProjectionPlanFromBoundValues(
			[]values.Value{physicalElement}, nil, nil, []string{"X"}, QuantifierOverPlan(oldFlat))
	})
	relinkedExpression, err := staleProjection.WithChildren(
		[]expressions.Quantifier{QuantifierOverPlan(newFlat)})
	if err != nil {
		t.Fatal(err)
	}
	relinked := relinkedExpression.(*RecordQueryProjectionPlan)
	relinkedField, ok := values.AsFieldValue(relinked.GetProjections()[0])
	newFlatLayout := requireProvidedLayout(t, newFlat)
	if !ok || relinkedField.ChildValue() != newFlatLayout.Carrier() {
		t.Fatalf("relinked stale ordinality root = %T, want exact replacement FlatMap carrier",
			relinked.GetProjections()[0])
	}
	if path := relinkedField.Path().Ordinals(); len(path) != 2 || path[0] != 1 || path[1] != 0 {
		t.Fatalf("relinked stale ordinality path = %v, want [1 0]", path)
	}
	staleField, ok := values.AsFieldValue(staleProjection.GetProjections()[0])
	if !ok || staleField.ChildValue() != physicalBinding {
		t.Fatal("projection relink mutated the stale source program")
	}

	foreignRoot, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("FOREIGN_X"), logicalType)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := values.ResolveFieldOrdinals(foreignRoot, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	translated, err := values.TranslateProjectionInputNameNormalizationToCorrelation(
		foreign, innerAlias, requireProvidedLayout(t, explode).Carrier().FlowedType())
	if err != nil {
		t.Fatal(err)
	}
	if translated != foreign {
		t.Fatal("foreign alias crossed the ordinality binding-name bridge")
	}

	driftedType := values.NewRecordType("", false, []values.Field{
		{Name: "X", FieldType: values.NotNullLong},
		{Name: "O", FieldType: values.NotNullInt},
	})
	driftedRoot, err := values.NewQuantifiedObjectValue(innerAlias, driftedType)
	if err != nil {
		t.Fatal(err)
	}
	drifted, err := values.ResolveFieldOrdinals(driftedRoot, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	translated, err = values.TranslateProjectionInputNameNormalizationToCorrelation(
		drifted, innerAlias, requireProvidedLayout(t, explode).Carrier().FlowedType())
	if err != nil {
		t.Fatal(err)
	}
	if translated != drifted {
		t.Fatal("leaf-type drift crossed the ordinality binding-name bridge")
	}

	plain := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(array)
	})
	if selectedOrdinalityExplode(plain) {
		t.Fatal("plain Explode admitted the WITH ORDINALITY lineage bridge")
	}
}
