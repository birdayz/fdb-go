package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestProjectionPlan_AliasProvenanceExcludedFromIdentity pins the memo contract
// for the per-slot alias provenance: it records WHO named an output slot, not
// what the slot computes, so two projections differing only in it are the SAME
// memo member and must hash the same.
//
// This is the contract values.FieldPath.FrontierPinned already carries (values.go
// documents it: "an evaluation-contract marker, not a value distinction. Two
// references to the same column that arrived through different producers must
// still intern as one memo member"). Letting the marker into structuralKey would
// split a group in two and let extraction pick either half.
//
// The paired negative is deliberate: the alias STRING is still identity, so this
// test cannot pass by the whole output schema having dropped out of the key.
func TestProjectionPlan_AliasProvenanceExcludedFromIdentity(t *testing.T) {
	t.Parallel()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan(nil, exactTestRecordType(), false)
	})
	v := testFieldAt(t, "A.K", 0, values.NullableLong)

	minted := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases([]values.Value{v}, []string{"A.K"}, scan)
	}).
		WithAliasProvenance([]bool{true})
	minted = mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return minted.WithAliasSources([]values.ProjectionAliasSource{
			values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("A")),
		})
	})
	userWritten := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases([]values.Value{v}, []string{"A.K"}, scan)
	})

	if !minted.EqualsPlanWithoutChildren(userWritten) {
		t.Error("a projection differing only in alias provenance must be the same memo member")
	}
	if minted.HashCodeWithoutChildren() != userWritten.HashCodeWithoutChildren() {
		t.Error("equal plans must hash equal (memo invariant); provenance must not perturb the hash")
	}
	otherSource := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		base := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
			return NewRecordQueryProjectionPlanWithAliases([]values.Value{v}, []string{"A.K"}, scan)
		}).WithAliasProvenance([]bool{true})
		return base.WithAliasSources([]values.ProjectionAliasSource{
			values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("Z")),
		})
	})
	if !minted.EqualsPlanWithoutChildren(otherSource) ||
		minted.HashCodeWithoutChildren() != otherSource.HashCodeWithoutChildren() {
		t.Error("structured alias source is metadata provenance and must stay out of plan identity")
	}
	if _, err := userWritten.WithAliasSources([]values.ProjectionAliasSource{
		values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("A")),
	}); err == nil {
		t.Error("a user-named physical slot accepted a machinery alias source")
	}
	// Same in the other direction — an explicitly not-minted vector must not
	// differ from an absent one.
	notMinted := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases([]values.Value{v}, []string{"A.K"}, scan)
	}).
		WithAliasProvenance([]bool{false})
	if !notMinted.EqualsPlanWithoutChildren(userWritten) ||
		notMinted.HashCodeWithoutChildren() != userWritten.HashCodeWithoutChildren() {
		t.Error("an all-false provenance vector must be identical to an absent one")
	}

	// The output NAME is still identity — otherwise the assertions above would
	// hold vacuously.
	renamed := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases([]values.Value{v}, []string{"K"}, scan)
	})
	if minted.EqualsPlanWithoutChildren(renamed) {
		t.Error("the alias STRING is still a memo discriminator")
	}
}

func TestProjectionPlan_FrozenOutputNameDifferenceSplitsIdentity(t *testing.T) {
	t.Parallel()
	rowType := exactTestRecordType()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan(nil, rowType, false)
	})
	innerQ := QuantifierOverPlan(scan)
	input, err := innerQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	id, err := values.ResolveFieldOrdinals(input, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	makeProjection := func(name string) *RecordQueryProjectionPlan {
		t.Helper()
		return mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
			return NewRecordQueryProjectionPlanFromQuantifierWithOutputSchema(
				[]values.Value{id}, nil, nil, []string{name}, innerQ)
		})
	}

	bare := makeProjection("ID")
	qualified := makeProjection("S.ID")
	if bare.EqualsPlanWithoutChildren(qualified) {
		t.Fatal("same Value/aliases with different frozen output names compared equal")
	}
	if bare.HashCodeWithoutChildren() == qualified.HashCodeWithoutChildren() {
		t.Fatal("different frozen output names hashed equal")
	}

	same := makeProjection("ID")
	if !bare.EqualsPlanWithoutChildren(same) ||
		bare.HashCodeWithoutChildren() != same.HashCodeWithoutChildren() {
		t.Fatal("identical frozen output schemas must remain equal and hash-equal")
	}
}

// TestProjectionPlan_AliasProvenanceSurvivesChildRewrite pins the carry-across
// contract on the copy paths the memo drives. WithQuantifiers / WithChildren
// hand back "the same projection over a new child"; a provenance dropped there
// silently relabels every machinery datum key as a user alias, and the only
// visible symptom is a leaked qualifier in ResultSet metadata far downstream.
func TestProjectionPlan_AliasProvenanceSurvivesChildRewrite(t *testing.T) {
	t.Parallel()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan(nil, exactTestRecordType(), false)
	})
	other := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"U"}, exactTestRecordType(), false)
	})
	v := testFieldAt(t, "A.K", 0, values.NullableLong)

	p := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases([]values.Value{v}, []string{"A.K"}, scan)
	}).
		WithAliasProvenance([]bool{true})
	p = mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return p.WithAliasSources([]values.ProjectionAliasSource{
			values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("A")),
		})
	})

	rewired, err := p.WithQuantifiers([]expressions.Quantifier{QuantifierOverPlan(other)})
	if err != nil {
		t.Fatalf("WithQuantifiers: %v", err)
	}
	rp, ok := rewired.(*RecordQueryProjectionPlan)
	if !ok {
		t.Fatalf("WithQuantifiers returned %T, want *RecordQueryProjectionPlan", rewired)
	}
	if got := rp.GetAliasMinted(); len(got) != 1 || !got[0] {
		t.Errorf("WithQuantifiers dropped the alias provenance: got %v, want [true]", got)
	}
	if got := rp.GetAliasSources(); len(got) != 1 || !got[0].Present ||
		got[0].Source != values.NamedCorrelationIdentifier("A") {
		t.Errorf("WithQuantifiers dropped the structured alias source: %+v", got)
	}

	withChild, err := p.WithChildren([]expressions.Quantifier{QuantifierOverPlan(other)})
	if err != nil {
		t.Fatalf("WithChildren: %v", err)
	}
	cp, ok := withChild.(*RecordQueryProjectionPlan)
	if !ok {
		t.Fatalf("WithChildren returned %T, want *RecordQueryProjectionPlan", withChild)
	}
	if got := cp.GetAliasMinted(); len(got) != 1 || !got[0] {
		t.Errorf("WithChildren dropped the alias provenance: got %v, want [true]", got)
	}
	if got := cp.GetAliasSources(); len(got) != 1 || !got[0].Present ||
		got[0].Source != values.NamedCorrelationIdentifier("A") {
		t.Errorf("WithChildren dropped the structured alias source: %+v", got)
	}

	// The original is untouched — these are copies, not mutations.
	if got := p.GetAliasMinted(); len(got) != 1 || !got[0] {
		t.Errorf("the source plan's provenance was mutated: got %v, want [true]", got)
	}
	gotSources := p.GetAliasSources()
	gotSources[0] = values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("MUTATED"))
	if original := p.GetAliasSources(); original[0].Source != values.NamedCorrelationIdentifier("A") {
		t.Errorf("GetAliasSources exposed mutable storage: %+v", original)
	}
}

// TestProjectionPlan_ChildRewriteRebasesRetainedProgram pins the extraction
// boundary: extraction always relinks a physical plan through a freshly minted
// child quantifier. Once that edge selects a physical singleton, the projection
// program must follow the child's exact layout carrier; merely swapping innerQ
// leaves a valid-looking plan whose executor cannot bind the stale QOV.
func TestProjectionPlan_ChildRewriteRebasesRetainedProgram(t *testing.T) {
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
	request, err := values.FieldByNameAndOrdinal("V", 3)
	if err != nil {
		t.Fatalf("projected field request: %v", err)
	}
	field, err := values.ResolveFieldAccess(originalQOV, []values.FieldRequest{request})
	if err != nil {
		t.Fatalf("projected field: %v", err)
	}
	projection := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanFromQuantifierWithProvenance(
			[]values.Value{field}, []string{"V"}, []bool{true}, originalQ)
	})
	projection = mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return projection.WithAliasSources([]values.ProjectionAliasSource{
			values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("A")),
		})
	})
	projection = projection.WithDistinctProofIndexName("UNIQUE_V").(*RecordQueryProjectionPlan)

	replacementQ := QuantifierOverPlan(scan)
	if replacementQ.GetAlias() == originalQ.GetAlias() {
		t.Fatal("test requires a fresh replacement correlation")
	}
	rebuiltExpression, err := projection.WithChildren([]expressions.Quantifier{replacementQ})
	if err != nil {
		t.Fatalf("WithChildren: %v", err)
	}
	rebuilt := rebuiltExpression.(*RecordQueryProjectionPlan)
	scanLayout := requireProvidedLayout(t, scan)
	rebuiltField, ok := values.AsFieldValue(rebuilt.GetProjections()[0])
	if !ok || rebuiltField.ChildValue() != scanLayout.Carrier() {
		t.Fatalf("rebuilt projection root = %T/%v, want exact selected-child carrier %p",
			rebuilt.GetProjections()[0], rebuiltField, scanLayout.Carrier())
	}
	correlated := values.GetCorrelatedToOfValue(rebuilt.GetProjections()[0])
	if _, stale := correlated[originalQ.GetAlias()]; stale {
		t.Fatalf("rebuilt projection retained stale edge %s: %v", originalQ.GetAlias(), correlated)
	}
	if rebuilt.GetDistinctProofIndexName() != "UNIQUE_V" {
		t.Fatal("child rewrite dropped the distinct proof stamp")
	}
	if got := rebuilt.GetAliasMinted(); len(got) != 1 || !got[0] {
		t.Fatalf("child rewrite dropped alias provenance: %v", got)
	}
	if got := rebuilt.GetAliasSources(); len(got) != 1 || !got[0].Present ||
		got[0].Source != values.NamedCorrelationIdentifier("A") {
		t.Fatalf("child rewrite replaced authored source with physical current carrier: %+v", got)
	}
	if !rebuilt.GetResultType().Equals(projection.GetResultType()) {
		t.Fatalf("child rewrite changed result type: %s to %s", projection.GetResultType(), rebuilt.GetResultType())
	}
}

func TestProjectionPlan_ChildRewritePreservesDuplicateNamedFlatMapOwners(t *testing.T) {
	t.Parallel()

	empType := values.NewRecordType("EMP", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "FNAME", Ordinal: 1, FieldType: values.NullableString},
		{Name: "DEPT_ID", Ordinal: 2, FieldType: values.NullableLong},
	})
	deptType := values.NewRecordType("DEPT", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "NAME", Ordinal: 1, FieldType: values.NullableString},
	})

	type joinFixture struct {
		plan        *RecordQueryFlatMapPlan
		deptName    values.Value
		projectName values.Value
	}
	build := func(label string, projectNameType values.Type) joinFixture {
		t.Helper()
		projectType := values.NewRecordType("PROJECT", false, []values.Field{
			{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
			{Name: "NAME", Ordinal: 1, FieldType: projectNameType},
			{Name: "DSG", Ordinal: 2, FieldType: values.NullableString},
			{Name: "EMP_ID", Ordinal: 3, FieldType: values.NullableLong},
		})
		newLeg := func(alias values.CorrelationIdentifier, typ values.Type) expressions.Quantifier {
			t.Helper()
			scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
				return NewRecordQueryScanPlan([]string{alias.Name()}, typ, false)
			})
			return expressions.NamedPhysicalQuantifier(
				alias, expressions.FinalOfAtStage(scan, expressions.StageCanonical))
		}
		projectAlias := values.NamedCorrelationIdentifier("PROJECT")
		empAlias := values.NamedCorrelationIdentifier("EMP")
		deptAlias := values.NamedCorrelationIdentifier("DEPT")
		projectQ := newLeg(projectAlias, projectType)
		empQ := newLeg(empAlias, empType)
		deptQ := newLeg(deptAlias, deptType)
		projectRoot, err := projectQ.RequireFlowedObjectValue()
		if err != nil {
			t.Fatalf("%s project root: %v", label, err)
		}
		empRoot, err := empQ.RequireFlowedObjectValue()
		if err != nil {
			t.Fatalf("%s emp root: %v", label, err)
		}
		deptRoot, err := deptQ.RequireFlowedObjectValue()
		if err != nil {
			t.Fatalf("%s dept root: %v", label, err)
		}
		resolve := func(root values.QuantifiedObjectValue, path ...int) values.Value {
			t.Helper()
			field, resolveErr := values.ResolveFieldOrdinals(root, path)
			if resolveErr != nil {
				t.Fatalf("%s resolve %v: %v", label, path, resolveErr)
			}
			return field
		}

		// The lower join retains EMP and PROJECT as separate whole-record
		// windows. The upper join flattens those windows around DEPT, producing
		// two NAME leaves at ordinals 4 and 6.
		lower := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
			return NewRecordQueryFlatMapPlanFromQuantifiers(
				projectQ, empQ, projectAlias, empAlias,
				values.NewRawRecordConstructorValue(
					values.RecordConstructorField{Name: "_0", Value: empRoot},
					values.RecordConstructorField{Name: "_1", Value: projectRoot}),
				false)
		})
		mergedAlias := values.NamedCorrelationIdentifier("$m\"" + label)
		lowerQ := expressions.NamedPhysicalQuantifier(
			mergedAlias, expressions.FinalOfAtStage(lower, expressions.StageCanonical))
		lowerRoot, err := lowerQ.RequireFlowedObjectValue()
		if err != nil {
			t.Fatalf("%s lower root: %v", label, err)
		}
		upper := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
			return NewRecordQueryFlatMapPlanFromQuantifiers(
				lowerQ, deptQ, mergedAlias, deptAlias,
				values.NewRawRecordConstructorValue(
					values.RecordConstructorField{Name: "ID", Value: resolve(lowerRoot, 0, 0)},
					values.RecordConstructorField{Name: "FNAME", Value: resolve(lowerRoot, 0, 1)},
					values.RecordConstructorField{Name: "DEPT_ID", Value: resolve(lowerRoot, 0, 2)},
					values.RecordConstructorField{Name: "ID", Value: resolve(deptRoot, 0)},
					values.RecordConstructorField{Name: "NAME", Value: resolve(deptRoot, 1)},
					values.RecordConstructorField{Name: "ID", Value: resolve(lowerRoot, 1, 0)},
					values.RecordConstructorField{Name: "NAME", Value: resolve(lowerRoot, 1, 1)},
					values.RecordConstructorField{Name: "DSG", Value: resolve(lowerRoot, 1, 2)},
					values.RecordConstructorField{Name: "EMP_ID", Value: resolve(lowerRoot, 1, 3)}),
				false)
		})
		return joinFixture{
			plan:        upper,
			deptName:    resolve(deptRoot, 1),
			projectName: resolve(projectRoot, 1),
		}
	}

	originalFixture := build("old", values.NullableString)
	replacementFixture := build("new", values.NullableString)
	originalLayout := requireProvidedLayout(t, originalFixture.plan)
	replacementLayout := requireProvidedLayout(t, replacementFixture.plan)
	if originalLayout.Carrier() == replacementLayout.Carrier() {
		t.Fatal("fixture requires independently owned FlatMap output carriers")
	}
	projection := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithOutputSchema(
			[]values.Value{originalFixture.deptName, originalFixture.projectName},
			nil, nil, []string{"DEPT.NAME", "PROJECT.NAME"}, originalFixture.plan)
	})
	assertSlots := func(label string, plan *RecordQueryProjectionPlan, carrier values.QuantifiedObjectValue) {
		t.Helper()
		for i, wantOrdinal := range []int{4, 6} {
			field, ok := values.AsFieldValue(plan.GetProjections()[i])
			if !ok || field.ChildValue() != carrier {
				t.Fatalf("%s projection %d root = %T/%v, want exact carrier %p",
					label, i, plan.GetProjections()[i], field, carrier)
			}
			if got := field.Path().Ordinals(); len(got) != 1 || got[0] != wantOrdinal {
				t.Fatalf("%s projection %d path = %v, want [%d]", label, i, got, wantOrdinal)
			}
		}
	}
	assertSlots("original", projection, originalLayout.Carrier())

	rebuiltExpression, err := projection.WithChildren(
		[]expressions.Quantifier{QuantifierOverPlan(replacementFixture.plan)})
	if err != nil {
		t.Fatalf("WithChildren: %v", err)
	}
	rebuilt := rebuiltExpression.(*RecordQueryProjectionPlan)
	assertSlots("rebuilt", rebuilt, replacementLayout.Carrier())
	assertSlots("source after rebuild", projection, originalLayout.Carrier())
	if got := rebuilt.GetOutputNames(); len(got) != 2 ||
		got[0] != "DEPT.NAME" || got[1] != "PROJECT.NAME" {
		t.Fatalf("rebuilt output names = %v, want frozen owner-qualified schema", got)
	}

	// TranslatePhaseRoot is the relink's authority. An independently minted
	// same-shaped current row is not the old selected carrier and must therefore
	// remain byte-for-byte unchanged.
	independentLayout := mustChecked(t, func() (values.OrdinalLayout, error) {
		return values.NewOrdinalLayoutForCarrierType(
			originalFixture.plan.GetResultType(),
			[]values.OrdinalTileSpec{{Start: 0, Width: 9, Kind: values.OrdinalTileFlat}}, nil)
	})
	independentName, err := values.ResolveFieldOrdinals(independentLayout.Carrier(), []int{6})
	if err != nil {
		t.Fatalf("independent NAME: %v", err)
	}
	unchanged, err := values.TranslatePhaseRoot(
		independentName, originalLayout.Carrier(), replacementLayout.Carrier())
	if err != nil {
		t.Fatalf("independent phase translation: %v", err)
	}
	if unchanged != independentName {
		t.Fatal("relink accepted an independently minted same-shaped current carrier")
	}

	driftedFixture := build("drifted", values.NotNullString)
	if _, err := projection.WithChildren(
		[]expressions.Quantifier{QuantifierOverPlan(driftedFixture.plan)}); err == nil {
		t.Fatal("projection relink accepted duplicate-NAME leaf nullability drift")
	}
}

// A reshaping producer can happen to publish the same structural layout as
// its input while owning a distinct current carrier. In that case descendant
// materializer discovery must stop at the producer boundary: crossing an
// aggregate into a sort reanchors the parent program onto the sort's stale
// current handle, which the aggregate row cannot bind at execution.
func TestProjectionPlan_ReanchorStopsAtSameShapeAggregateCarrierBoundary(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("GROUP_INPUT", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "COL1", Ordinal: 1, FieldType: values.NullableLong},
	})
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	})
	scanLayout := requireProvidedLayout(t, scan)
	scanKey, err := values.ResolveFieldOrdinals(scanLayout.Carrier(), []int{1})
	if err != nil {
		t.Fatalf("scan key: %v", err)
	}
	innerProjection := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithOutputSchema(
			[]values.Value{scanKey}, nil, nil, []string{"COL1"}, scan)
	})
	innerLayout := requireProvidedLayout(t, innerProjection)
	projectedKey, err := values.ResolveFieldOrdinals(innerLayout.Carrier(), []int{0})
	if err != nil {
		t.Fatalf("projected key: %v", err)
	}
	sortPlan := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(innerProjection, []SortKey{{
			Field: "COL1", ValueExpr: projectedKey,
		}})
	})
	sortLayout := requireProvidedLayout(t, sortPlan)
	groupKey, err := values.ResolveFieldOrdinals(sortLayout.Carrier(), []int{0})
	if err != nil {
		t.Fatalf("group key: %v", err)
	}
	aggregate := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(sortPlan, []values.Value{groupKey}, nil)
	})
	aggregateLayout := requireProvidedLayout(t, aggregate)
	if !aggregateLayout.RawEqual(sortLayout) {
		t.Fatal("fixture requires structurally equal aggregate and sort layouts")
	}
	if aggregateLayout.Carrier() == sortLayout.Carrier() {
		t.Fatal("fixture requires distinct aggregate and sort carrier handles")
	}
	if _, found := descendantValueMaterializer(aggregate); found {
		t.Fatal("materializer discovery crossed a reshaping aggregate boundary")
	}

	projection := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan([]values.Value{groupKey}, aggregate)
	})
	field, ok := values.AsFieldValue(projection.GetProjections()[0])
	if !ok || field.ChildValue() != aggregateLayout.Carrier() {
		t.Fatalf("projection root = %T/%v, want aggregate carrier %p (sort carrier %p)",
			projection.GetProjections()[0], field, aggregateLayout.Carrier(), sortLayout.Carrier())
	}
}

// A real pass-through wrapper reuses the child's exact layout carrier. Keep
// walking through that boundary so a materializer below it remains available;
// the aggregate-boundary guard above must not collapse into a blanket stop at
// every unary plan.
func TestDescendantValueMaterializerTraversesExactCarrierPassThrough(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("LIMIT_INPUT", false, []values.Field{{
		Name: "COL1", Ordinal: 0, FieldType: values.NullableLong,
	}})
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	})
	sortPlan := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(scan, nil)
	})
	limit := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(sortPlan, 1, 0)
	})
	sortLayout := requireProvidedLayout(t, sortPlan)
	limitLayout := requireProvidedLayout(t, limit)
	if limitLayout.Carrier() != sortLayout.Carrier() || !limitLayout.RawEqual(sortLayout) {
		t.Fatal("fixture LIMIT did not preserve the sort's exact carrier layout")
	}
	materializer, found := descendantValueMaterializer(limit)
	if !found || materializer != sortPlan {
		t.Fatalf("descendant materializer = %T/%v, want exact sort %p", materializer, found, sortPlan)
	}
}

func TestProjectionPlan_ChildRewritePreservesComputedOutputSchema(t *testing.T) {
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
	field, err := values.ResolveFieldOrdinals(originalQOV, []int{3})
	if err != nil {
		t.Fatalf("field: %v", err)
	}
	computed := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  field,
		Right: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
	}
	projection := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanFromQuantifier([]values.Value{computed}, nil, originalQ)
	})
	wantNames := projection.GetOutputNames()
	if len(wantNames) != 1 || wantNames[0] == "" {
		t.Fatalf("source output names = %v, want one exact name", wantNames)
	}

	replacementQ := QuantifierOverPlan(scan)
	rebuiltExpression, err := projection.WithChildren([]expressions.Quantifier{replacementQ})
	if err != nil {
		t.Fatalf("WithChildren: %v", err)
	}
	rebuilt := rebuiltExpression.(*RecordQueryProjectionPlan)
	if got := rebuilt.GetOutputNames(); len(got) != 1 || got[0] != wantNames[0] {
		t.Fatalf("computed output schema changed across alpha-relink: %v to %v", wantNames, got)
	}
	if !rebuilt.GetResultType().Equals(projection.GetResultType()) {
		t.Fatalf("computed result type changed across alpha-relink: %s to %s", projection.GetResultType(), rebuilt.GetResultType())
	}
}

func TestProjectionPlan_NamedOutputSchemaIsIdentityAndSurvivesRewire(t *testing.T) {
	t.Parallel()
	collection := &values.ConstantValue{
		Typ:   &values.ArrayType{ElementType: values.NotNullInt},
		Value: []any{int32(1)},
	}
	explode := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(collection)
	})
	inner := QuantifierOverPlan(explode)
	scalar, err := inner.RequireFlowedObjectValue()
	if err != nil {
		t.Fatalf("scalar flowed object: %v", err)
	}
	build := func(name string) *RecordQueryProjectionPlan {
		return mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
			return NewRecordQueryProjectionPlanFromQuantifierWithOutputSchema(
				[]values.Value{scalar}, []string{name}, []bool{true}, []string{name}, inner)
		})
	}
	at, val := build("AT"), build("VAL")
	if at.EqualsPlanWithoutChildren(val) {
		t.Fatal("physical projections with different frozen output schemas reported equal")
	}
	if at.HashCodeWithoutChildren() == val.HashCodeWithoutChildren() {
		t.Fatal("physical projections with different frozen output schemas produced the same hash")
	}

	rebuiltExpr, err := at.WithChildren([]expressions.Quantifier{QuantifierOverPlan(explode)})
	if err != nil {
		t.Fatalf("WithChildren: %v", err)
	}
	if got := rebuiltExpr.(*RecordQueryProjectionPlan).GetOutputNames(); len(got) != 1 || got[0] != "AT" {
		t.Fatalf("WithChildren changed frozen output schema: %v", got)
	}
}

// A scalar child still needs a one-slot record projection. Removing that
// projection would widen the plan's result from RECORD<X> to X and make memo
// admission (correctly) reject the replacement. Only a whole-record QOV can
// therefore be a physical identity projection.
func TestProjectionPlan_ScalarQOVIsNotIdentity(t *testing.T) {
	t.Parallel()
	collection := &values.ConstantValue{
		Typ: &values.ArrayType{ElementType: values.NotNullLong},
		Value: []any{
			int64(1),
		},
	}
	explode := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(collection)
	})
	innerQ := QuantifierOverPlan(explode)
	scalarQOV, err := innerQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatalf("scalar flowed object: %v", err)
	}
	if scalarQOV.FlowedType().Code() == values.TypeCodeRecord {
		t.Fatalf("test requires a scalar child, got %s", scalarQOV.FlowedType())
	}
	projection := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanFromQuantifier([]values.Value{scalarQOV}, nil, innerQ)
	})
	if projection.IsIdentity() {
		t.Fatal("a scalar-QOV projection must not be removed as a whole-record identity")
	}
}
