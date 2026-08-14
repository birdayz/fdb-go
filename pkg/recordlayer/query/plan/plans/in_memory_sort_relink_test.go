package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestInMemorySortPlan_ChildRewriteRebasesRetainedKeys pins the same extraction
// boundary as projection relinking: a freshly selected child has an exact
// physical layout, so every retained sort key must be translated to that
// layout's carrier atomically.
func TestInMemorySortPlan_ChildRewriteRebasesRetainedKeys(t *testing.T) {
	t.Parallel()
	rowType := exactTestRecordType()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	})
	originalQ := QuantifierOverPlan(scan)
	originalQOV, err := originalQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatalf("original flowed object: %v", err)
	}
	key, err := values.ResolveFieldOrdinals(originalQOV, []int{3})
	if err != nil {
		t.Fatalf("sort key: %v", err)
	}
	sortPlan := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlanFromQuantifier(originalQ, []SortKey{{
			Field: "V", ValueExpr: key,
		}})
	})

	replacementQ := QuantifierOverPlan(scan)
	if replacementQ.GetAlias() == originalQ.GetAlias() {
		t.Fatal("test requires a fresh replacement correlation")
	}
	rebuiltExpression, err := sortPlan.WithChildren([]expressions.Quantifier{replacementQ})
	if err != nil {
		t.Fatalf("WithChildren: %v", err)
	}
	rebuilt := rebuiltExpression.(*RecordQueryInMemorySortPlan)
	scanLayout := requireProvidedLayout(t, scan)
	rebuiltKey, ok := values.AsFieldValue(rebuilt.GetSortKeys()[0].ValueExpr)
	if !ok || rebuiltKey.ChildValue() != scanLayout.Carrier() {
		t.Fatalf("rebuilt key root = %T/%v, want selected child carrier %p",
			rebuilt.GetSortKeys()[0].ValueExpr, rebuiltKey, scanLayout.Carrier())
	}
	correlated := values.GetCorrelatedToOfValue(rebuilt.GetSortKeys()[0].ValueExpr)
	if _, stale := correlated[originalQ.GetAlias()]; stale {
		t.Fatalf("rebuilt key retained stale edge %s: %v", originalQ.GetAlias(), correlated)
	}
	originalKey, ok := values.AsFieldValue(sortPlan.GetSortKeys()[0].ValueExpr)
	if !ok || originalKey.ChildValue() != scanLayout.Carrier() {
		t.Fatalf("source sort key was mutated away from selected child carrier: %T/%v",
			sortPlan.GetSortKeys()[0].ValueExpr, originalKey)
	}
}

func TestInMemorySortPlan_SelectedFlatMapOutputRelinkKeepsExactOutputOrdinal(t *testing.T) {
	t.Parallel()

	buildFlatMap := func(label string) *RecordQueryFlatMapPlan {
		t.Helper()
		outerType := values.NewRecordType("T", false, []values.Field{
			{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
			{Name: "COL1", Ordinal: 1, FieldType: values.NullableLong},
		})
		innerType := values.NewRecordType("U", false, []values.Field{{
			Name: "PRESENT", Ordinal: 0, FieldType: values.NotNullBoolean,
		}})
		outerAlias := values.NamedCorrelationIdentifier("T_" + label)
		innerAlias := values.NamedCorrelationIdentifier("U_" + label)
		outerScan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
			return NewRecordQueryScanPlan([]string{"T"}, outerType, false)
		})
		innerScan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
			return NewRecordQueryScanPlan([]string{"U"}, innerType, false)
		})
		outerQ := expressions.NamedPhysicalQuantifier(
			outerAlias, expressions.FinalOfAtStage(outerScan, expressions.StageCanonical))
		innerQ := expressions.NamedPhysicalQuantifier(
			innerAlias, expressions.FinalOfAtStage(innerScan, expressions.StageCanonical))
		outerRoot, err := outerQ.RequireFlowedObjectValue()
		if err != nil {
			t.Fatalf("%s outer root: %v", label, err)
		}
		id, err := values.ResolveFieldOrdinals(outerRoot, []int{0})
		if err != nil {
			t.Fatalf("%s ID: %v", label, err)
		}
		col1, err := values.ResolveFieldOrdinals(outerRoot, []int{1})
		if err != nil {
			t.Fatalf("%s COL1: %v", label, err)
		}
		return mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
			// The output alias ID deliberately contains source COL1 while X
			// contains source ID. Re-running producer matching on output ID#0
			// therefore changes the semantic key to X#1.
			return NewRecordQueryFlatMapPlanFromQuantifiers(
				outerQ, innerQ, outerAlias, innerAlias,
				values.NewRawRecordConstructorValue(
					values.RecordConstructorField{Name: "ID", Value: col1},
					values.RecordConstructorField{Name: "X", Value: id}),
				false)
		})
	}

	originalChild := buildFlatMap("old")
	replacementChild := buildFlatMap("new")
	liveRef := expressions.InitialOf(originalChild)
	originalQ := expressions.NewPhysicalQuantifier(liveRef)
	originalEdge, err := originalQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatalf("original edge: %v", err)
	}
	outputID, err := values.ResolveFieldOrdinals(originalEdge, []int{0})
	if err != nil {
		t.Fatalf("output ID: %v", err)
	}
	sortPlan := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		// The live group has no winner yet. This preserves the output-bound key
		// on its declared edge, matching implementation-rule construction.
		return NewRecordQueryInMemorySortPlanFromQuantifier(originalQ, []SortKey{{
			Field: "ID", ValueExpr: outputID,
		}})
	})
	originalStoredKey := sortPlan.GetSortKeys()[0].ValueExpr
	if originalStoredKey != outputID {
		t.Fatal("exploratory sort construction unexpectedly normalized its edge-bound key")
	}

	// Optimization selects the live group's physical child before extraction
	// replaces it with a fresh singleton edge.
	liveRef.SetWinner(originalChild)
	replacementQ := expressions.NewPhysicalQuantifier(
		expressions.FinalOfAtStage(replacementChild, expressions.StageCanonical))
	rebuiltExpression, err := sortPlan.WithQuantifiers([]expressions.Quantifier{replacementQ})
	if err != nil {
		t.Fatalf("WithQuantifiers: %v", err)
	}
	rebuilt := rebuiltExpression.(*RecordQueryInMemorySortPlan)
	replacementLayout := requireProvidedLayout(t, replacementChild)
	rebuiltField, ok := values.AsFieldValue(rebuilt.GetSortKeys()[0].ValueExpr)
	if !ok || rebuiltField.ChildValue() != replacementLayout.Carrier() {
		t.Fatalf("rebuilt key root = %T/%v, want exact replacement carrier %p",
			rebuilt.GetSortKeys()[0].ValueExpr, rebuiltField, replacementLayout.Carrier())
	}
	if got := rebuiltField.Path().Ordinals(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("rebuilt output-alias key path = %v, want [0]; [1] reads source ID through output X", got)
	}
	if sortPlan.GetSortKeys()[0].ValueExpr != originalStoredKey {
		t.Fatal("sort relink mutated the source plan's key")
	}
}

func TestTranslateSortKeyAcrossSelectedOutputRequiresExactOwnedPhase(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("SORT_OUT", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "X", Ordinal: 1, FieldType: values.NullableLong},
	})
	oldLayout := mustChecked(t, func() (values.OrdinalLayout, error) {
		return values.NewOrdinalLayoutForCarrierType(
			rowType, []values.OrdinalTileSpec{{Start: 0, Width: 2, Kind: values.OrdinalTileFlat}}, nil)
	})
	newLayout := mustChecked(t, func() (values.OrdinalLayout, error) {
		return values.NewOrdinalLayoutForCarrierType(
			rowType, []values.OrdinalTileSpec{{Start: 0, Width: 2, Kind: values.OrdinalTileFlat}}, nil)
	})
	edge, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("sort_edge"), rowType)
	if err != nil {
		t.Fatalf("edge: %v", err)
	}

	oldOutputX, err := values.ResolveFieldOrdinals(oldLayout.Carrier(), []int{1})
	if err != nil {
		t.Fatalf("old output X: %v", err)
	}
	translated, exact, err := translateSortKeyAcrossSelectedOutput(
		oldOutputX, edge, oldLayout, newLayout)
	if err != nil {
		t.Fatalf("exact phase translation: %v", err)
	}
	translatedField, ok := values.AsFieldValue(translated)
	if !exact || !ok || translatedField.ChildValue() != newLayout.Carrier() {
		t.Fatalf("exact phase translation = (%T/%v, %v), want replacement carrier",
			translated, translatedField, exact)
	}
	if got := translatedField.Path().Ordinals(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("exact phase translation changed path to %v, want [1]", got)
	}
	if field, ok := values.AsFieldValue(oldOutputX); !ok || field.ChildValue() != oldLayout.Carrier() {
		t.Fatal("phase translation mutated its source Value")
	}

	assertDeclinedUnchanged := func(label string, value values.Value) {
		t.Helper()
		got, admitted, translateErr := translateSortKeyAcrossSelectedOutput(
			value, edge, oldLayout, newLayout)
		if translateErr != nil {
			t.Fatalf("%s: %v", label, translateErr)
		}
		if admitted {
			t.Fatalf("%s was admitted as an exact selected-output program", label)
		}
		if got != value {
			t.Fatalf("%s was rewritten despite lacking exact phase authority", label)
		}
	}

	independentLayout := mustChecked(t, func() (values.OrdinalLayout, error) {
		return values.NewOrdinalLayoutForCarrierType(
			rowType, []values.OrdinalTileSpec{{Start: 0, Width: 2, Kind: values.OrdinalTileFlat}}, nil)
	})
	independentID, err := values.ResolveFieldOrdinals(independentLayout.Carrier(), []int{0})
	if err != nil {
		t.Fatalf("independent ID: %v", err)
	}
	assertDeclinedUnchanged("independent same-shaped current", independentID)

	foreignRoot, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("foreign"), rowType)
	if err != nil {
		t.Fatalf("foreign root: %v", err)
	}
	foreignID, err := values.ResolveFieldOrdinals(foreignRoot, []int{0})
	if err != nil {
		t.Fatalf("foreign ID: %v", err)
	}
	assertDeclinedUnchanged("foreign named owner", foreignID)

	driftedType := values.NewRecordType("SORT_OUT", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "X", Ordinal: 1, FieldType: values.NullableLong},
	})
	driftedRoot, err := values.NewQuantifiedObjectValue(edge.Correlation(), driftedType)
	if err != nil {
		t.Fatalf("drifted root: %v", err)
	}
	driftedID, err := values.ResolveFieldOrdinals(driftedRoot, []int{0})
	if err != nil {
		t.Fatalf("drifted ID: %v", err)
	}
	assertDeclinedUnchanged("same edge with leaf nullability drift", driftedID)

	// A mixed program must not claim that producer normalization is complete.
	// Its exact old-output leaf moves to the replacement phase, but the foreign
	// leaf remains and therefore keeps the ordinary relink path armed.
	mixed := &values.ArithmeticValue{Op: values.OpAdd, Left: oldOutputX, Right: foreignID}
	mixedResult, admitted, err := translateSortKeyAcrossSelectedOutput(
		mixed, edge, oldLayout, newLayout)
	if err != nil {
		t.Fatalf("mixed program: %v", err)
	}
	if admitted {
		t.Fatal("mixed exact-output/foreign program took the selected-output bypass")
	}
	if mixedResult == mixed {
		t.Fatal("mixed fixture did not exercise copy-on-write phase translation")
	}
	if mixed.Left != oldOutputX || mixed.Right != foreignID {
		t.Fatal("mixed phase translation mutated its source expression")
	}
}

func TestInMemorySortPlan_ProducerBoundaryReanchorsLegFieldsBeforeWindowsDisappear(t *testing.T) {
	t.Parallel()
	outerType := values.NewRecordType("CUST", false, []values.Field{
		{Name: "CID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "REGION", Ordinal: 1, FieldType: values.NotNullLong},
	})
	innerType := values.NewRecordType("ORD", false, []values.Field{
		{Name: "OID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "CID", Ordinal: 1, FieldType: values.NotNullLong},
	})
	fixture := newRetainedJoinFixture(
		t, "sort_producer", values.NamedCorrelationIdentifier("C"),
		values.NamedCorrelationIdentifier("O"), outerType, innerType)
	join := mustChecked(t, func() (*RecordQueryNestedLoopJoinPlan, error) {
		return NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
			fixture.outerQ, fixture.innerQ, nil, JoinInner,
			fixture.outerAlias, fixture.innerAlias, fixture.result)
	})
	sortPlan := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(join, nil)
	})

	// C retains its logical owner alias but not the storage record name. OID
	// carries an extraction edge alias instead of the logical O alias. The
	// producer RC is authoritative for both: owner+name selects C.CID, while
	// globally unique name selects O.OID.
	logicalOuter := mustOrdinalLayoutQOV(t, fixture.outerAlias,
		values.NewRecordType("", false, outerType.Fields))
	logicalCID, err := values.ResolveFieldOrdinals(logicalOuter, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	physicalInner := mustOrdinalLayoutQOV(t,
		values.NamedCorrelationIdentifier("physical_projection_edge"), innerType)
	physicalOID, err := values.ResolveFieldOrdinals(physicalInner, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	projection := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan([]values.Value{logicalCID, physicalOID}, sortPlan)
	})
	sortLayout := requireProvidedLayout(t, sortPlan)
	for i, wantPath := range [][]int{{0}, {2}} {
		field, ok := values.AsFieldValue(projection.GetProjections()[i])
		if !ok || field.ChildValue() != sortLayout.Carrier() {
			t.Fatalf("projection %d root = %T/%v, want exact sort carrier %p",
				i, projection.GetProjections()[i], field, sortLayout.Carrier())
		}
		gotPath := field.Path().Ordinals()
		if len(gotPath) != 1 || gotPath[0] != wantPath[0] {
			t.Fatalf("projection %d path = %v, want %v", i, gotPath, wantPath)
		}
	}

	// CID exists on both producer legs. A foreign owner cannot select either
	// slot merely because its display path and scalar type match.
	ambiguousCID, err := values.ResolveFieldOrdinals(physicalInner, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := sortPlan.reanchorInputValueToOutput(ambiguousCID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != ambiguousCID {
		t.Fatal("ambiguous foreign CID was guessed into a producer output slot")
	}
}

func TestInMemorySortPlan_ProducerBoundaryReanchorsNestedNominalJoinSource(t *testing.T) {
	t.Parallel()
	nestedType := values.NewRecordType("NST", true, []values.Field{
		{Name: "SK", FieldType: values.NullableLong},
		{Name: "CO", FieldType: values.NullableLong},
	})
	t1Physical := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong},
		{Name: "SK", FieldType: values.NullableLong},
		{Name: "N", FieldType: nestedType},
	})
	t2Physical := values.NewRecordType("", false, []values.Field{
		{Name: "ID2", FieldType: values.NullableLong},
		{Name: "V", FieldType: values.NullableLong},
	})
	t1Alias := values.NamedCorrelationIdentifier("T1")
	t2Alias := values.NamedCorrelationIdentifier("T2")
	t1 := mustOrdinalLayoutQOV(t, t1Alias, t1Physical)
	t2 := mustOrdinalLayoutQOV(t, t2Alias, t2Physical)
	resultFields := make([]values.RecordConstructorField, 0, 5)
	for i, name := range []string{"ID", "SK", "N"} {
		field, err := values.ResolveOrdinalSeedField(t1, i)
		if err != nil {
			t.Fatal(err)
		}
		resultFields = append(resultFields, values.RecordConstructorField{Name: name, Value: field})
	}
	for i, name := range []string{"ID2", "V"} {
		field, err := values.ResolveOrdinalSeedField(t2, i)
		if err != nil {
			t.Fatal(err)
		}
		resultFields = append(resultFields, values.RecordConstructorField{Name: name, Value: field})
	}
	t1Scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T1"}, t1Physical, false)
	})
	t2Scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T2"}, t2Physical, false)
	})
	join := mustChecked(t, func() (*RecordQueryNestedLoopJoinPlan, error) {
		return NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
			expressions.NamedPhysicalQuantifier(t1Alias, expressions.FinalOfAtStage(t1Scan, expressions.StageCanonical)),
			expressions.NamedPhysicalQuantifier(t2Alias, expressions.FinalOfAtStage(t2Scan, expressions.StageCanonical)),
			nil, JoinInner, t1Alias, t2Alias,
			values.NewRawRecordConstructorValue(resultFields...))
	})
	sortPlan := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(join, nil)
	})
	logicalT1 := mustOrdinalLayoutQOV(t, t1Alias,
		values.NewRecordType("T1", false, t1Physical.Fields))
	nestedSK, err := values.ResolveFieldOrdinals(logicalT1, []int{2, 0})
	if err != nil {
		t.Fatal(err)
	}
	projection := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan([]values.Value{nestedSK}, sortPlan)
	})
	projected, ok := values.AsFieldValue(projection.GetProjections()[0])
	sortLayout := requireProvidedLayout(t, sortPlan)
	if !ok || projected.ChildValue() != sortLayout.Carrier() {
		t.Fatalf("nested projection root = %T/%v, want exact sort carrier %p",
			projection.GetProjections()[0], projected, sortLayout.Carrier())
	}
	if path := projected.Path().Ordinals(); len(path) != 2 || path[0] != 2 || path[1] != 0 {
		t.Fatalf("nested projection path = %v, want [2 0]", path)
	}
	if original, ok := values.AsFieldValue(nestedSK); !ok || original.ChildValue() != logicalT1 {
		t.Fatal("nested source normalization mutated the logical request")
	}
	foreign := mustOrdinalLayoutQOV(t, values.NamedCorrelationIdentifier("FOREIGN"),
		values.NewRecordType("T1", false, t1Physical.Fields))
	foreignSK, err := values.ResolveFieldOrdinals(foreign, []int{2, 0})
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := sortPlan.reanchorInputValueToOutput(foreignSK)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != foreignSK {
		t.Fatal("foreign same-shaped nested source crossed the join producer")
	}
}

func TestInMemorySortPlan_ProducerBoundaryTraversesNestedFlatMaps(t *testing.T) {
	t.Parallel()
	legType := values.NewRecordType("LEG", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "NAME", Ordinal: 1, FieldType: values.NullableString},
	})
	newLeg := func(name string) (values.CorrelationIdentifier, *RecordQueryScanPlan, expressions.Quantifier, values.QuantifiedObjectValue) {
		t.Helper()
		alias := values.NamedCorrelationIdentifier(name)
		scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
			return NewRecordQueryScanPlan([]string{name}, legType, false)
		})
		q := expressions.NamedPhysicalQuantifier(
			alias, expressions.FinalOfAtStage(scan, expressions.StageCanonical))
		root, err := q.RequireFlowedObjectValue()
		if err != nil {
			t.Fatalf("%s flowed object: %v", name, err)
		}
		return alias, scan, q, root
	}
	aAlias, _, aQ, a := newLeg("A")
	bAlias, _, bQ, b := newLeg("B")
	cAlias, _, cQ, c := newLeg("C")
	dAlias, _, dQ, d := newLeg("D")

	ab := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			aQ, bQ, aAlias, bAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "_0", Value: a},
				values.RecordConstructorField{Name: "_1", Value: b}), false)
	})
	abAlias := values.NamedCorrelationIdentifier("AB")
	abQ := expressions.NamedPhysicalQuantifier(
		abAlias, expressions.FinalOfAtStage(ab, expressions.StageCanonical))
	abRoot, err := abQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	abA, err := values.ResolveFieldOrdinals(abRoot, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	abB, err := values.ResolveFieldOrdinals(abRoot, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	abc := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			abQ, cQ, abAlias, cAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "_0", Value: c},
				values.RecordConstructorField{Name: "_1", Value: abA},
				values.RecordConstructorField{Name: "_2", Value: abB}), false)
	})
	abcAlias := values.NamedCorrelationIdentifier("ABC")
	abcQ := expressions.NamedPhysicalQuantifier(
		abcAlias, expressions.FinalOfAtStage(abc, expressions.StageCanonical))
	abcRoot, err := abcQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	abcC, err := values.ResolveFieldOrdinals(abcRoot, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	abcA, err := values.ResolveFieldOrdinals(abcRoot, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	abcB, err := values.ResolveFieldOrdinals(abcRoot, []int{2})
	if err != nil {
		t.Fatal(err)
	}
	abcd := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			abcQ, dQ, abcAlias, dAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "_0", Value: d},
				values.RecordConstructorField{Name: "_1", Value: abcC},
				values.RecordConstructorField{Name: "_2", Value: abcA},
				values.RecordConstructorField{Name: "_3", Value: abcB}), false)
	})

	sortPlan := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(abcd, nil)
	})
	nameOf := func(root values.QuantifiedObjectValue) values.Value {
		t.Helper()
		name, resolveErr := values.ResolveFieldOrdinals(root, []int{1})
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		return name
	}
	requested := []values.Value{nameOf(d), nameOf(c), nameOf(a), nameOf(b)}
	projection := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan(requested, sortPlan)
	})
	sortLayout := requireProvidedLayout(t, sortPlan)
	wantPaths := [][]int{{0, 1}, {1, 1}, {2, 1}, {3, 1}}
	for i, want := range wantPaths {
		field, ok := values.AsFieldValue(projection.GetProjections()[i])
		if !ok || field.ChildValue() != sortLayout.Carrier() {
			t.Fatalf("projection %d root = %T/%v, want exact sort carrier %p",
				i, projection.GetProjections()[i], field, sortLayout.Carrier())
		}
		got := field.Path().Ordinals()
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("projection %d nested path = %v, want %v", i, got, want)
		}
		original := values.GetCorrelatedToOfValue(requested[i])
		if _, current := original[values.CurrentCorrelation()]; current {
			t.Fatalf("projection construction mutated request %d onto current", i)
		}
	}
}

func TestInMemorySortPlan_ProducerBoundaryTraversesInnerNestedFlatMap(t *testing.T) {
	t.Parallel()
	legType := func(name string, fields ...values.Field) *values.RecordType {
		t.Helper()
		return values.NewRecordType(name, false, fields)
	}
	sType := legType("S", values.Field{Name: "S_ID", FieldType: values.NotNullLong})
	iType := legType("I",
		values.Field{Name: "I_ID", FieldType: values.NotNullLong},
		values.Field{Name: "QTY", FieldType: values.NullableLong})
	pType := legType("P",
		values.Field{Name: "P_ID", FieldType: values.NotNullLong},
		values.Field{Name: "PRICE", FieldType: values.NullableLong})
	newLeg := func(alias string, typ values.Type) (values.CorrelationIdentifier, expressions.Quantifier, values.QuantifiedObjectValue) {
		t.Helper()
		correlation := values.NamedCorrelationIdentifier(alias)
		scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
			return NewRecordQueryScanPlan([]string{alias}, typ, false)
		})
		quantifier := expressions.NamedPhysicalQuantifier(
			correlation, expressions.FinalOfAtStage(scan, expressions.StageCanonical))
		root, err := quantifier.RequireFlowedObjectValue()
		if err != nil {
			t.Fatalf("%s flowed object: %v", alias, err)
		}
		return correlation, quantifier, root
	}
	sAlias, sQ, sRoot := newLeg("S", sType)
	iAlias, iQ, iRoot := newLeg("I", iType)
	pAlias, pQ, pRoot := newLeg("P", pType)

	inner := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			iQ, pQ, iAlias, pAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "_0", Value: iRoot},
				values.RecordConstructorField{Name: "_1", Value: pRoot}), false)
	})
	innerAlias := values.NamedCorrelationIdentifier("IP")
	innerQ := expressions.NamedPhysicalQuantifier(
		innerAlias, expressions.FinalOfAtStage(inner, expressions.StageCanonical))
	innerRoot, err := innerQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	iID, err := values.ResolveFieldOrdinals(innerRoot, []int{0, 0})
	if err != nil {
		t.Fatal(err)
	}
	iQty, err := values.ResolveFieldOrdinals(innerRoot, []int{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	pID, err := values.ResolveFieldOrdinals(innerRoot, []int{1, 0})
	if err != nil {
		t.Fatal(err)
	}
	pPrice, err := values.ResolveFieldOrdinals(innerRoot, []int{1, 1})
	if err != nil {
		t.Fatal(err)
	}
	sID, err := values.ResolveFieldOrdinals(sRoot, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	upper := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			sQ, innerQ, sAlias, innerAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "S_ID", Value: sID},
				values.RecordConstructorField{Name: "I_ID", Value: iID},
				values.RecordConstructorField{Name: "QTY", Value: iQty},
				values.RecordConstructorField{Name: "P_ID", Value: pID},
				values.RecordConstructorField{Name: "PRICE", Value: pPrice}), false)
	})
	sortPlan := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(upper, nil)
	})
	requestedQty, err := values.ResolveFieldOrdinals(iRoot, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	requestedPrice, err := values.ResolveFieldOrdinals(pRoot, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	product := &values.ArithmeticValue{
		Op: values.OpMul, Left: requestedPrice, Right: requestedQty,
	}
	reanchored, err := sortPlan.reanchorInputValueToOutput(product)
	if err != nil {
		t.Fatal(err)
	}
	sortLayout := requireProvidedLayout(t, sortPlan)
	wantPaths := map[int]struct{}{2: {}, 4: {}}
	seen := make(map[int]struct{})
	values.WalkValue(reanchored, func(node values.Value) bool {
		field, ok := values.AsFieldValue(node)
		if !ok {
			return true
		}
		if field.ChildValue() != sortLayout.Carrier() {
			t.Fatalf("reanchored operand root = %v, want exact sort carrier %p",
				field.ChildValue(), sortLayout.Carrier())
		}
		path := field.Path().Ordinals()
		if len(path) != 1 {
			t.Fatalf("reanchored operand path = %v, want one output ordinal", path)
		}
		if _, ok := wantPaths[path[0]]; !ok {
			t.Fatalf("reanchored operand path = %v, want QTY#2 or PRICE#4", path)
		}
		seen[path[0]] = struct{}{}
		return true
	})
	if len(seen) != len(wantPaths) {
		t.Fatalf("reanchored operand ordinals = %v, want %v", seen, wantPaths)
	}
	if _, current := values.GetCorrelatedToOfValue(product)[values.CurrentCorrelation()]; current {
		t.Fatal("source aggregate operand was mutated onto current")
	}
}

func TestInMemorySortPlan_IdentityFlatMapTraversesOuterJoinProducer(t *testing.T) {
	t.Parallel()
	outerType := values.NewRecordType("ORDERS", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong},
		{Name: "CUST_ID", FieldType: values.NullableLong},
	})
	innerType := values.NewRecordType("FLAGS", false, []values.Field{{
		Name: "K", FieldType: values.NullableLong,
	}, {
		Name: "V", FieldType: values.NullableLong,
	}})
	fixture := newRetainedJoinFixture(
		t, "identity_flatmap_outer_join",
		values.NamedCorrelationIdentifier("O"),
		values.NamedCorrelationIdentifier("F"),
		outerType, innerType)
	join := mustChecked(t, func() (*RecordQueryNestedLoopJoinPlan, error) {
		return NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
			fixture.outerQ, fixture.innerQ, nil, JoinInner,
			fixture.outerAlias, fixture.innerAlias, fixture.result)
	})
	joinAlias := values.NamedCorrelationIdentifier("join_identity")
	joinQ := expressions.NamedPhysicalQuantifier(
		joinAlias, expressions.FinalOfAtStage(join, expressions.StageCanonical))
	unitType := values.NewRecordType("UNIT", false, nil)
	unitScan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"UNIT"}, unitType, false)
	})
	unitAlias := values.NamedCorrelationIdentifier("unit")
	unitQ := expressions.NamedPhysicalQuantifier(
		unitAlias, expressions.FinalOfAtStage(unitScan, expressions.StageCanonical))
	joinRoot, err := joinQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	identity := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			joinQ, unitQ, joinAlias, unitAlias, joinRoot, true)
	})
	sortPlan := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(identity, nil)
	})

	logicalOuterType := values.NewRecordType("", false, outerType.Fields)
	logicalOuter := mustOrdinalLayoutQOV(t, fixture.outerAlias, logicalOuterType)
	logicalID, err := values.ResolveFieldOrdinals(logicalOuter, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	projection := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan([]values.Value{logicalID}, sortPlan)
	})
	sortLayout := requireProvidedLayout(t, sortPlan)
	projected, ok := values.AsFieldValue(projection.GetProjections()[0])
	if !ok || projected.ChildValue() != sortLayout.Carrier() {
		t.Fatalf("projected O.ID root = %T/%v, want exact sort carrier %p",
			projection.GetProjections()[0], projected, sortLayout.Carrier())
	}
	if path := projected.Path().Ordinals(); len(path) != 1 || path[0] != 0 {
		t.Fatalf("projected O.ID path = %v, want [0]", path)
	}

	foreignType := values.NewRecordType("", false, []values.Field{
		{Name: "OTHER", FieldType: values.NullableLong},
	})
	foreign := mustOrdinalLayoutQOV(
		t, values.NamedCorrelationIdentifier("foreign"), foreignType)
	foreignID, err := values.ResolveFieldOrdinals(foreign, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := sortPlan.reanchorInputValueToOutput(foreignID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != foreignID {
		t.Fatal("foreign same-shaped ID was guessed into the outer join producer")
	}
}

func TestInMemorySortPlan_FlatMapRoutesDirectOwnersBeforeUniqueProducerFallback(t *testing.T) {
	t.Parallel()
	empType := values.NewRecordType("EMP", false, []values.Field{
		{Name: "NAME", FieldType: values.NullableString},
		{Name: "DEPT_ID", FieldType: values.NullableLong},
	})
	deptType := values.NewRecordType("DEPT", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong},
		{Name: "NAME", FieldType: values.NullableString},
	})
	empScan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"EMP"}, empType, false)
	})
	empQ := expressions.NamedPhysicalQuantifier(
		values.NamedCorrelationIdentifier("E"),
		expressions.FinalOfAtStage(empScan, expressions.StageCanonical))
	empRoot, err := empQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	empName, err := values.ResolveFieldOrdinals(empRoot, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	empDeptID, err := values.ResolveFieldOrdinals(empRoot, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	empProjection := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanFromQuantifier(
			[]values.Value{empName, empDeptID}, []string{"ENAME", "DEPT_ID"}, empQ)
	})
	outerAlias := values.NamedCorrelationIdentifier("DEPT_EMP")
	outerQ := expressions.NamedPhysicalQuantifier(
		outerAlias, expressions.FinalOfAtStage(empProjection, expressions.StageCanonical))
	outerRoot, err := outerQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	outerEName, err := values.ResolveFieldOrdinals(outerRoot, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	outerDeptID, err := values.ResolveFieldOrdinals(outerRoot, []int{1})
	if err != nil {
		t.Fatal(err)
	}

	deptScan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"DEPT"}, deptType, false)
	})
	innerAlias := values.NamedCorrelationIdentifier("D")
	innerQ := expressions.NamedPhysicalQuantifier(
		innerAlias, expressions.FinalOfAtStage(deptScan, expressions.StageCanonical))
	innerRoot, err := innerQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	innerID, err := values.ResolveFieldOrdinals(innerRoot, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	innerName, err := values.ResolveFieldOrdinals(innerRoot, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	flatMap := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			outerQ, innerQ, outerAlias, innerAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "D_ID", Value: innerID},
				values.RecordConstructorField{Name: "D_NAME", Value: innerName},
				values.RecordConstructorField{Name: "ENAME", Value: outerEName},
				values.RecordConstructorField{Name: "DEPT_ID", Value: outerDeptID}),
			false)
	})
	sortPlan := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(flatMap, nil)
	})
	projection := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan([]values.Value{innerName, outerEName}, sortPlan)
	})
	sortLayout := requireProvidedLayout(t, sortPlan)
	for i, want := range []int{1, 2} {
		field, ok := values.AsFieldValue(projection.GetProjections()[i])
		if !ok || field.ChildValue() != sortLayout.Carrier() {
			t.Fatalf("projection %d root = %T/%v, want exact sort carrier %p",
				i, projection.GetProjections()[i], field, sortLayout.Carrier())
		}
		path := field.Path().Ordinals()
		if len(path) != 1 || path[0] != want {
			t.Fatalf("projection %d path = %v, want [%d]", i, path, want)
		}
	}
	originalInner, innerOK := values.AsFieldValue(innerName)
	originalOuter, outerOK := values.AsFieldValue(outerEName)
	if !innerOK || !outerOK || originalInner.ChildValue() != innerRoot ||
		originalOuter.ChildValue() != outerRoot {
		t.Fatal("FlatMap owner routing mutated its input Values")
	}
}

func TestInMemorySortPlan_MaterializationNormalizesChildLayout(t *testing.T) {
	t.Parallel()
	outerType, innerType := retainedJoinTypes()
	fixture := newRetainedJoinFixture(
		t,
		"sort_materialization",
		values.NamedCorrelationIdentifier("sort_outer"),
		values.NamedCorrelationIdentifier("sort_inner"),
		outerType,
		innerType,
	)
	join := mustChecked(t, func() (*RecordQueryNestedLoopJoinPlan, error) {
		return NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
			fixture.outerQ, fixture.innerQ, nil, JoinInner,
			fixture.outerAlias, fixture.innerAlias, fixture.result)
	})
	childLayout := requireProvidedLayout(t, join)
	canonicalChild, err := values.IsCanonicalCurrentOnlyOrdinalLayout(childLayout)
	if err != nil {
		t.Fatalf("child layout canonical check: %v", err)
	}
	if canonicalChild {
		t.Fatal("fixture join unexpectedly has a current-only layout")
	}

	inputAlias := values.NamedCorrelationIdentifier("sort_join_input")
	inputQ := expressions.NamedPhysicalQuantifier(
		inputAlias, expressions.FinalOfAtStage(join, expressions.StageCanonical))
	inputQOV, err := inputQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatalf("sort input QOV: %v", err)
	}
	key, err := values.ResolveFieldOrdinals(inputQOV, []int{0})
	if err != nil {
		t.Fatalf("sort key: %v", err)
	}
	sortPlan := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlanFromQuantifier(inputQ, []SortKey{{
			Field: "OUTER_FIRST", ValueExpr: key,
		}})
	})
	outputLayout := requireProvidedLayout(t, sortPlan)
	canonicalOutput, err := values.IsCanonicalCurrentOnlyOrdinalLayout(outputLayout)
	if err != nil {
		t.Fatalf("sort layout canonical check: %v", err)
	}
	if !canonicalOutput {
		t.Fatal("materializing sort forwarded the join's source-window layout")
	}
	if outputLayout.RawEqual(childLayout) {
		t.Fatal("materializing sort output layout equals its source-window child layout")
	}
	properties, err := sortPlan.OrdinalPhysicalProperties()
	if err != nil {
		t.Fatalf("sort properties: %v", err)
	}
	requirements := properties.RequiredInputLayouts()
	if len(requirements) != 1 {
		t.Fatalf("sort input requirements = %d, want 1", len(requirements))
	}
	if satisfied, satisfyErr := requirements[0].SatisfiedBy(childLayout); satisfyErr != nil || !satisfied {
		t.Fatalf("child layout satisfies requirement = (%v, %v), want true,nil", satisfied, satisfyErr)
	}
	if satisfied, satisfyErr := requirements[0].SatisfiedBy(outputLayout); satisfyErr != nil || satisfied {
		t.Fatalf("normalized output satisfies child requirement = (%v, %v), want false,nil", satisfied, satisfyErr)
	}
}

func TestInMemorySortPlan_RelinkRejectsChildTypeDrift(t *testing.T) {
	t.Parallel()
	rowType := exactTestRecordType()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	})
	oldQ := expressions.NamedPhysicalQuantifier(
		values.NamedCorrelationIdentifier("sort_type_old"),
		expressions.FinalOfAtStage(scan, expressions.StageCanonical))
	sortPlan := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlanFromQuantifier(oldQ, nil)
	})
	otherType := values.NewRecordType("other_sort_row", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	otherScan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"OTHER"}, otherType, false)
	})
	newQ := expressions.NamedPhysicalQuantifier(
		values.NamedCorrelationIdentifier("sort_type_new"),
		expressions.FinalOfAtStage(otherScan, expressions.StageCanonical))
	if _, err := sortPlan.WithQuantifiers([]expressions.Quantifier{newQ}); err == nil {
		t.Fatal("WithQuantifiers accepted a child with a different exact type")
	}
}
