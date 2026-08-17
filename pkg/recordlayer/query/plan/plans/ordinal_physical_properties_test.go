package plans

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustPhysicalPropertiesQOV(t testing.TB, name string, typ values.Type) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(name), typ)
	if err != nil {
		t.Fatalf("NewQuantifiedObjectValue: %v", err)
	}
	return qov
}

func TestOrdinalPhysicalPropertiesSourceAndExactRequirements(t *testing.T) {
	t.Parallel()
	sourceType := values.NewRecordType("", false, []values.Field{{Name: "S", FieldType: values.NotNullLong}})
	carrierType := values.NewRecordType("", false, []values.Field{{Name: "S", FieldType: values.NotNullLong}})
	source := mustPhysicalPropertiesQOV(t, "SOURCE", sourceType)
	layout, err := values.NewOrdinalLayoutForCarrierType(
		carrierType,
		[]values.OrdinalTileSpec{{Start: 0, Width: 1, Kind: values.OrdinalTileFlat}},
		[]values.OrdinalWindowSpec{{Source: source, FieldPaths: [][]int{{0}}}},
	)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	field, err := values.ResolveFieldOrdinals(source, []int{0})
	if err != nil {
		t.Fatalf("field: %v", err)
	}
	required, err := values.CollectRequiredBindings(layout.Carrier(), []values.Value{field}, nil, nil)
	if err != nil {
		t.Fatalf("required: %v", err)
	}
	sourceRequirement, err := RequireSources(required)
	if err != nil {
		t.Fatalf("RequireSources: %v", err)
	}
	if ok, satisfyErr := sourceRequirement.SatisfiedBy(layout); satisfyErr != nil || !ok {
		t.Fatalf("source SatisfiedBy = (%v,%v), want true,nil", ok, satisfyErr)
	}
	program, err := NewOrdinalEvaluationProgram(layout, required, OrdinalAddressingSourceBound)
	if err != nil {
		t.Fatalf("source program: %v", err)
	}
	properties, err := NewOrdinalPhysicalProperties(
		[]OrdinalLayoutRequirement{sourceRequirement},
		[]OrdinalEvaluationProgram{program}, layout)
	if err != nil {
		t.Fatalf("properties: %v", err)
	}
	exact, ok := AsOrdinalPhysicalProperties(properties)
	if !ok || exact.ProvidedOutputLayout() != layout {
		t.Fatalf("AsOrdinalPhysicalProperties = (%T,%v), want admitted exact view", exact, ok)
	}
	requirements := exact.RequiredInputLayouts()
	requirements[0] = nil
	if again := exact.RequiredInputLayouts(); len(again) != 1 || again[0] == nil {
		t.Fatal("mutating RequiredInputLayouts result changed properties")
	}
	programs := exact.EvaluationPrograms()
	programs[0] = nil
	if again := exact.EvaluationPrograms(); len(again) != 1 || again[0] == nil {
		t.Fatal("mutating EvaluationPrograms result changed properties")
	}

	exactRequirement, err := RequireExactLayout(layout)
	if err != nil {
		t.Fatalf("RequireExactLayout: %v", err)
	}
	if !OrdinalLayoutRequirementsEqual(sourceRequirement, sourceRequirement) {
		t.Fatal("one immutable source requirement must equal itself")
	}
	sourceRequirementTwin, err := RequireSources(required)
	if err != nil {
		t.Fatalf("RequireSources(twin): %v", err)
	}
	if !OrdinalLayoutRequirementsEqual(sourceRequirement, sourceRequirementTwin) {
		t.Fatal("two wrappers over one immutable binding manifest must compare equal")
	}
	if OrdinalLayoutRequirementsEqual(sourceRequirement, exactRequirement) {
		t.Fatal("source and exact requirements must not compare equal")
	}
	twin, err := values.NewOrdinalLayout(
		layout.Carrier(),
		[]values.OrdinalTileSpec{{Start: 0, Width: 1, Kind: values.OrdinalTileFlat}},
		[]values.OrdinalWindowSpec{{Source: source, FieldPaths: [][]int{{0}}}},
	)
	if err != nil {
		t.Fatalf("twin: %v", err)
	}
	if ok, satisfyErr := exactRequirement.SatisfiedBy(twin); satisfyErr != nil || !ok {
		t.Fatalf("raw-equal exact SatisfiedBy = (%v,%v), want true,nil", ok, satisfyErr)
	}
	twinRequirement, err := RequireExactLayout(twin)
	if err != nil {
		t.Fatalf("RequireExactLayout(twin): %v", err)
	}
	if !OrdinalLayoutRequirementsEqual(exactRequirement, twinRequirement) {
		t.Fatal("independently wrapped raw-equal exact layouts must compare equal")
	}
}

func TestOrdinalEvaluationProgramRejectsHybridAddressing(t *testing.T) {
	t.Parallel()
	carrierType := values.NewRecordType("", false, []values.Field{{Name: "A", FieldType: values.NotNullLong}})
	layout, err := values.NewOrdinalLayoutForCarrierType(
		carrierType, []values.OrdinalTileSpec{{Start: 0, Width: 1, Kind: values.OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	field, err := values.ResolveFieldOrdinals(layout.Carrier(), []int{0})
	if err != nil {
		t.Fatalf("field: %v", err)
	}
	required, err := values.CollectRequiredBindings(layout.Carrier(), []values.Value{field}, nil, nil)
	if err != nil {
		t.Fatalf("required: %v", err)
	}
	if program, programErr := NewOrdinalEvaluationProgram(
		layout, required, OrdinalAddressingSourceBound,
	); program != nil || programErr == nil {
		t.Fatalf("window-free source program = (%T,%v), want nil,error", program, programErr)
	}
	if program, programErr := NewOrdinalEvaluationProgram(
		layout, required, OrdinalAddressingCarrierBound,
	); programErr != nil || program == nil {
		t.Fatalf("carrier program = (%T,%v), want admitted program", program, programErr)
	}
}

type hostileOrdinalPhysicalProperties struct{}

func (*hostileOrdinalPhysicalProperties) RequiredInputLayouts() []OrdinalLayoutRequirement {
	panic("hostile")
}

func (*hostileOrdinalPhysicalProperties) EvaluationPrograms() []OrdinalEvaluationProgram {
	panic("hostile")
}

func (*hostileOrdinalPhysicalProperties) ProvidedOutputLayout() values.OrdinalLayout {
	panic("hostile")
}
func (*hostileOrdinalPhysicalProperties) isOrdinalPhysicalPropertiesView() {}

func TestAsOrdinalPhysicalPropertiesRejectsHostileAndMalformedInputs(t *testing.T) {
	t.Parallel()
	if got, ok := AsOrdinalPhysicalProperties(&hostileOrdinalPhysicalProperties{}); ok || got != nil {
		t.Fatalf("hostile view = (%T,%v), want nil,false", got, ok)
	}
	if got, ok := AsOrdinalPhysicalProperties((*ordinalPhysicalProperties)(nil)); ok || got != nil {
		t.Fatalf("typed-nil view = (%T,%v), want nil,false", got, ok)
	}
	if OrdinalLayoutRequirementsEqual(
		(*exactLayoutRequirement)(nil), (*exactLayoutRequirement)(nil)) {
		t.Fatal("typed-nil requirements compared equal")
	}
	if _, err := RequireSources(nil); err == nil {
		t.Fatal("RequireSources accepted nil")
	} else {
		var coded interface {
			Code() values.ResolutionErrorCode
		}
		if !errors.As(err, &coded) || coded.Code() != values.LayoutForeignValue {
			t.Fatalf("RequireSources(nil) = %v, want LayoutForeignValue", err)
		}
	}
	carrierType := values.NewRecordType("", false, []values.Field{
		{Name: "A", FieldType: values.NotNullLong},
	})
	layout, err := values.NewOrdinalLayoutForCarrierType(
		carrierType,
		[]values.OrdinalTileSpec{{Start: 0, Width: 1, Kind: values.OrdinalTileFlat}},
		nil,
	)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	if got, propertyErr := NewOrdinalPhysicalProperties(
		[]OrdinalLayoutRequirement{(*exactLayoutRequirement)(nil)}, nil, layout,
	); got != nil || propertyErr == nil {
		t.Fatalf("typed-nil requirement = (%T,%v), want nil,error", got, propertyErr)
	}
}

func TestPlanExprBasePublishesPropertiesAtomically(t *testing.T) {
	t.Parallel()
	recordType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
	})
	leaf, err := NewRecordQueryScanPlan([]string{"T"}, recordType, false)
	if err != nil {
		t.Fatalf("leaf: %v", err)
	}

	properties, err := leaf.OrdinalPhysicalProperties()
	if err != nil {
		t.Fatalf("OrdinalPhysicalProperties: %v", err)
	}
	if _, ok := AsOrdinalPhysicalProperties(properties); !ok {
		t.Fatalf("plan returned foreign property view %T", properties)
	}
	provided, err := leaf.ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("ProvidedOutputLayout: %v", err)
	}
	if properties.ProvidedOutputLayout() != provided {
		t.Fatal("plan property and ProvidedOutputLayout published different layouts")
	}
	if got := properties.RequiredInputLayouts(); len(got) != 0 {
		t.Fatalf("leaf requirements = %d, want provided-only", len(got))
	}
	if got := properties.EvaluationPrograms(); len(got) != 0 {
		t.Fatalf("leaf programs = %d, want none without retained program metadata", len(got))
	}
}

func TestPlanPhysicalPropertiesDynamicAndMalformedAreFallible(t *testing.T) {
	t.Parallel()
	dynamic, err := NewRecordQueryScanPlan(
		[]string{"T", "U"}, values.NewAnyRecordType(false), false)
	if err != nil {
		t.Fatalf("dynamic scan: %v", err)
	}
	for name, base := range map[string]PlanExprBase{
		"dynamic":   dynamic.PlanExprBase,
		"malformed": {},
	} {
		name, base := name, base
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			properties, propertyErr := base.OrdinalPhysicalProperties()
			if properties != nil {
				t.Fatalf("properties = %T, want nil", properties)
			}
			var unavailable *OrdinalLayoutUnavailableError
			if !errors.As(propertyErr, &unavailable) {
				t.Fatalf("error = %T %v, want OrdinalLayoutUnavailableError", propertyErr, propertyErr)
			}
			want := OrdinalLayoutMalformedPlan
			if name == "dynamic" {
				want = OrdinalLayoutDynamicCarrier
			}
			if unavailable.Code != want {
				t.Fatalf("code = %v, want %v", unavailable.Code, want)
			}
		})
	}
}

func TestPassThroughPropertiesTrackSelectedChildAcrossReconstruction(t *testing.T) {
	t.Parallel()
	recordType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "K", FieldType: values.NotNullLong},
	})
	first, err := NewRecordQueryScanPlan([]string{"T"}, recordType, false)
	if err != nil {
		t.Fatalf("first leaf: %v", err)
	}
	replacementLayout, err := values.NewOrdinalLayoutForCarrierType(
		recordType,
		[]values.OrdinalTileSpec{
			{Start: 0, Width: 1, Kind: values.OrdinalTileFlat},
			{Start: 1, Width: 1, Kind: values.OrdinalTileFlat},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("replacement layout: %v", err)
	}
	replacementBase, err := newPlanExprBaseWithProvidedLayout(
		"physicalPropertiesTestPlan", replacementLayout.Carrier(), replacementLayout)
	if err != nil {
		t.Fatalf("replacement base: %v", err)
	}
	replacement := &physicalPropertiesTestPlan{PlanExprBase: replacementBase}
	firstLayout, err := first.ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("first layout: %v", err)
	}
	if firstLayout.RawEqual(replacementLayout) {
		t.Fatal("fixture layouts unexpectedly share one physical tiling")
	}

	limit, err := NewRecordQueryLimitPlan(first, 1, 0)
	if err != nil {
		t.Fatalf("limit: %v", err)
	}
	assertExactPassThroughProperties(t, limit, firstLayout, replacementLayout)

	rebuiltExpression, err := limit.WithQuantifiers(
		[]expressions.Quantifier{QuantifierOverPlan(replacement)})
	if err != nil {
		t.Fatalf("WithQuantifiers: %v", err)
	}
	rebuilt, ok := rebuiltExpression.(*RecordQueryLimitPlan)
	if !ok {
		t.Fatalf("WithQuantifiers returned %T", rebuiltExpression)
	}
	assertExactPassThroughProperties(t, rebuilt, replacementLayout, firstLayout)
}

type physicalPropertiesTestPlan struct{ PlanExprBase }

func (p *physicalPropertiesTestPlan) GetResultType() values.Type {
	return p.GetResultValue().Type()
}
func (*physicalPropertiesTestPlan) GetChildren() []RecordQueryPlan { return nil }
func (*physicalPropertiesTestPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	_, ok := other.(*physicalPropertiesTestPlan)
	return ok
}
func (*physicalPropertiesTestPlan) HashCodeWithoutChildren() uint64 { return 0x232 }
func (*physicalPropertiesTestPlan) Explain() string                 { return "physical-properties-fixture" }
func (p *physicalPropertiesTestPlan) EqualsWithoutChildren(
	other expressions.RelationalExpression,
	_ *expressions.AliasMap,
) bool {
	_, ok := other.(*physicalPropertiesTestPlan)
	return ok
}

func (p *physicalPropertiesTestPlan) WithQuantifiers(
	quantifiers []expressions.Quantifier,
) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("physicalPropertiesTestPlan", len(quantifiers), 0); err != nil {
		return nil, err
	}
	return p, nil
}

func TestNaryBaseDoesNotPublishPartialInputRequirementVector(t *testing.T) {
	t.Parallel()
	recordType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
	})
	first, err := NewRecordQueryScanPlan([]string{"T"}, recordType, false)
	if err != nil {
		t.Fatalf("first leaf: %v", err)
	}
	second, err := NewRecordQueryScanPlan([]string{"T"}, recordType, false)
	if err != nil {
		t.Fatalf("second leaf: %v", err)
	}
	union, err := NewRecordQueryUnorderedUnionPlan([]RecordQueryPlan{first, second})
	if err != nil {
		t.Fatalf("union: %v", err)
	}
	properties, err := union.OrdinalPhysicalProperties()
	if err != nil {
		t.Fatalf("OrdinalPhysicalProperties: %v", err)
	}
	if got := properties.RequiredInputLayouts(); len(got) != 0 {
		t.Fatalf("N-ary requirements = %d, want no partial vector", len(got))
	}
	provided, err := union.ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("ProvidedOutputLayout: %v", err)
	}
	if properties.ProvidedOutputLayout() != provided {
		t.Fatal("N-ary property and legacy layout accessor disagree")
	}
}

func TestExploratoryChildDoesNotBecomeAnExactInputRequirement(t *testing.T) {
	t.Parallel()
	recordType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
	})
	child, err := NewRecordQueryScanPlan([]string{"T"}, recordType, false)
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	ref := expressions.InitialOf(child)
	quantifier := expressions.NewPhysicalQuantifier(ref)

	unselected, err := NewRecordQueryLimitPlanFromQuantifier(quantifier, 1, 0, nil)
	if err != nil {
		t.Fatalf("unselected limit: %v", err)
	}
	properties, err := unselected.OrdinalPhysicalProperties()
	if err != nil {
		t.Fatalf("unselected properties: %v", err)
	}
	if got := properties.RequiredInputLayouts(); len(got) != 0 {
		t.Fatalf("exploratory requirements = %d, want no guessed requirement", len(got))
	}

	ref.SetWinner(child)
	selected, err := NewRecordQueryLimitPlanFromQuantifier(quantifier, 1, 0, nil)
	if err != nil {
		t.Fatalf("selected limit: %v", err)
	}
	selectedProperties, err := selected.OrdinalPhysicalProperties()
	if err != nil {
		t.Fatalf("selected properties: %v", err)
	}
	if got := selectedProperties.RequiredInputLayouts(); len(got) != 1 {
		t.Fatalf("winner-selected requirements = %d, want 1", len(got))
	}
}

func TestFirstFinalInLiveGroupDoesNotBecomeAnExactInputRequirement(t *testing.T) {
	t.Parallel()
	recordType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
	})
	child, err := NewRecordQueryScanPlan([]string{"T"}, recordType, false)
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	// During PLANNING a memo group still contains its canonical member when
	// the first implementation arrives in the final lane. The singleton final
	// is not a winner: index and join alternatives may be yielded later.
	ref := expressions.InitialOf(child)
	if !ref.InsertFinal(child) {
		t.Fatal("failed to seed the first live final")
	}

	unselected, err := NewRecordQueryLimitPlanFromQuantifier(
		expressions.NewPhysicalQuantifier(ref), 1, 0, nil)
	if err != nil {
		t.Fatalf("unselected limit: %v", err)
	}
	properties, err := unselected.OrdinalPhysicalProperties()
	if err != nil {
		t.Fatalf("unselected properties: %v", err)
	}
	if got := properties.RequiredInputLayouts(); len(got) != 0 {
		t.Fatalf("live first-final requirements = %d, want no guessed requirement", len(got))
	}
}

func assertExactPassThroughProperties(
	t testing.TB,
	plan RecordQueryPlan,
	want, reject values.OrdinalLayout,
) {
	t.Helper()
	properties, err := plan.OrdinalPhysicalProperties()
	if err != nil {
		t.Fatalf("%T OrdinalPhysicalProperties: %v", plan, err)
	}
	if properties.ProvidedOutputLayout() != want {
		t.Fatalf("%T provided layout did not preserve selected child", plan)
	}
	requirements := properties.RequiredInputLayouts()
	if len(requirements) != 1 {
		t.Fatalf("%T requirements = %d, want 1", plan, len(requirements))
	}
	if ok, satisfyErr := requirements[0].SatisfiedBy(want); satisfyErr != nil || !ok {
		t.Fatalf("selected layout satisfaction = (%v,%v), want true,nil", ok, satisfyErr)
	}
	if ok, satisfyErr := requirements[0].SatisfiedBy(reject); satisfyErr != nil || ok {
		t.Fatalf("stale layout satisfaction = (%v,%v), want false,nil", ok, satisfyErr)
	}
	if got := properties.EvaluationPrograms(); len(got) != 0 {
		t.Fatalf("%T programs = %d, want none without retained Value closure", plan, len(got))
	}

	// The plan view inherits the physical property's defensive-slice contract.
	requirements[0] = nil
	if again := properties.RequiredInputLayouts(); len(again) != 1 || again[0] == nil {
		t.Fatal("mutating the returned requirement slice changed plan properties")
	}
}
