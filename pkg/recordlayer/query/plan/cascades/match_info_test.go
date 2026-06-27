package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// testVal creates a minimal ConstantValue for test use.
func testVal(v any) values.Value {
	return &values.ConstantValue{Value: v, Typ: values.TypeInt}
}

func TestRegularMatchInfo_Construction(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("p1")
	cr := predicates.EmptyComparisonRange()
	pbm := map[values.CorrelationIdentifier]*predicates.ComparisonRange{alias: cr}
	aliasMap := EmptyAliasMap()
	predMap := &PredicateMultiMap{}
	mop := []*MatchedOrderingPart{
		NewMatchedOrderingPart(alias, testVal(42), nil, MatchedSortOrderAscending),
	}
	mmm := NewMaxMatchMap(nil, nil, nil)
	gbm := EmptyGroupByMappings()
	constraint := &QueryPlanConstraint{}

	rmi := NewRegularMatchInfo(pbm, aliasMap, predMap, mop, mmm, gbm, nil, constraint)

	if rmi == nil {
		t.Fatal("NewRegularMatchInfo returned nil")
	}
	if got := rmi.GetParameterBindingMap(); len(got) != 1 {
		t.Fatalf("parameterBindingMap len=%d, want 1", len(got))
	}
	if rmi.GetBindingAliasMap() != aliasMap {
		t.Fatal("bindingAliasMap mismatch")
	}
	if rmi.GetPredicateMap() != predMap {
		t.Fatal("predicateMap mismatch")
	}
	if len(rmi.GetMatchedOrderingParts()) != 1 {
		t.Fatalf("matchedOrderingParts len=%d, want 1", len(rmi.GetMatchedOrderingParts()))
	}
	if rmi.GetMaxMatchMap() != mmm {
		t.Fatal("maxMatchMap mismatch")
	}
	if rmi.GetGroupByMappings() != gbm {
		t.Fatal("groupByMappings mismatch")
	}
	if rmi.GetRollUpToGroupingValues() != nil {
		t.Fatal("rollUpToGroupingValues should be nil")
	}
	if rmi.GetAdditionalPlanConstraint() != constraint {
		t.Fatal("additionalPlanConstraint mismatch")
	}
}

func TestRegularMatchInfo_IsAdjusted_IsRegular(t *testing.T) {
	t.Parallel()

	rmi := NewRegularMatchInfo(nil, EmptyAliasMap(), &PredicateMultiMap{},
		nil, NewMaxMatchMap(nil, nil, nil), EmptyGroupByMappings(), nil, &QueryPlanConstraint{})

	if rmi.IsAdjusted() {
		t.Fatal("RegularMatchInfo.IsAdjusted() should be false")
	}
	if !rmi.IsRegular() {
		t.Fatal("RegularMatchInfo.IsRegular() should be true")
	}
}

func TestRegularMatchInfo_GetRegularMatchInfo_ReturnsSelf(t *testing.T) {
	t.Parallel()

	rmi := NewRegularMatchInfo(nil, EmptyAliasMap(), &PredicateMultiMap{},
		nil, NewMaxMatchMap(nil, nil, nil), EmptyGroupByMappings(), nil, &QueryPlanConstraint{})

	if got := rmi.GetRegularMatchInfo(); got != rmi {
		t.Fatal("GetRegularMatchInfo should return self for RegularMatchInfo")
	}
}

func TestRegularMatchInfo_ImplementsMatchInfo(t *testing.T) {
	t.Parallel()

	var _ MatchInfo = (*RegularMatchInfo)(nil)
}

func TestRegularMatchInfo_RollUpValues(t *testing.T) {
	t.Parallel()

	rollUp := []values.Value{testVal(1), testVal(2)}
	rmi := NewRegularMatchInfo(nil, EmptyAliasMap(), &PredicateMultiMap{},
		nil, NewMaxMatchMap(nil, nil, nil), EmptyGroupByMappings(), rollUp, &QueryPlanConstraint{})

	got := rmi.GetRollUpToGroupingValues()
	if got == nil {
		t.Fatal("rollUpToGroupingValues should not be nil")
	}
	if len(got) != 2 {
		t.Fatalf("rollUpToGroupingValues len=%d, want 2", len(got))
	}
}

func TestRegularMatchInfo_DefensiveCopy_ParameterBindingMap(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("p1")
	cr := predicates.EmptyComparisonRange()
	pbm := map[values.CorrelationIdentifier]*predicates.ComparisonRange{alias: cr}

	rmi := NewRegularMatchInfo(pbm, EmptyAliasMap(), &PredicateMultiMap{},
		nil, NewMaxMatchMap(nil, nil, nil), EmptyGroupByMappings(), nil, &QueryPlanConstraint{})

	// Mutate the original map -- should not affect the internal copy.
	alias2 := values.NamedCorrelationIdentifier("p2")
	pbm[alias2] = cr

	if len(rmi.GetParameterBindingMap()) != 1 {
		t.Fatal("mutation of original parameterBindingMap leaked into RegularMatchInfo")
	}
}

func TestRegularMatchInfo_DefensiveCopy_MatchedOrderingParts(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("p1")
	mop := []*MatchedOrderingPart{
		NewMatchedOrderingPart(alias, testVal(1), nil, MatchedSortOrderAscending),
	}

	rmi := NewRegularMatchInfo(nil, EmptyAliasMap(), &PredicateMultiMap{},
		mop, NewMaxMatchMap(nil, nil, nil), EmptyGroupByMappings(), nil, &QueryPlanConstraint{})

	// Mutate the original slice -- should not affect the internal copy.
	mop[0] = nil

	if rmi.GetMatchedOrderingParts()[0] == nil {
		t.Fatal("mutation of original matchedOrderingParts leaked into RegularMatchInfo")
	}
}

func TestAdjustedMatchInfo_Construction(t *testing.T) {
	t.Parallel()

	rmi := NewRegularMatchInfo(nil, EmptyAliasMap(), &PredicateMultiMap{},
		nil, NewMaxMatchMap(nil, nil, nil), EmptyGroupByMappings(), nil, &QueryPlanConstraint{})

	newMop := []*MatchedOrderingPart{
		NewMatchedOrderingPart(values.NamedCorrelationIdentifier("x"),
			testVal(99), nil, MatchedSortOrderDescending),
	}
	newMmm := NewMaxMatchMap(nil, testVal(1), testVal(2))
	newGbm := EmptyGroupByMappings()

	ami := NewAdjustedMatchInfo(rmi, newMop, newMmm, newGbm)

	if ami == nil {
		t.Fatal("NewAdjustedMatchInfo returned nil")
	}
	if ami.GetUnderlying() != rmi {
		t.Fatal("underlying mismatch")
	}
	if len(ami.GetMatchedOrderingParts()) != 1 {
		t.Fatalf("matchedOrderingParts len=%d, want 1", len(ami.GetMatchedOrderingParts()))
	}
	if ami.GetMaxMatchMap() != newMmm {
		t.Fatal("maxMatchMap mismatch")
	}
	if ami.GetGroupByMappings() != newGbm {
		t.Fatal("groupByMappings mismatch")
	}
}

func TestAdjustedMatchInfo_IsAdjusted_IsRegular(t *testing.T) {
	t.Parallel()

	rmi := NewRegularMatchInfo(nil, EmptyAliasMap(), &PredicateMultiMap{},
		nil, NewMaxMatchMap(nil, nil, nil), EmptyGroupByMappings(), nil, &QueryPlanConstraint{})
	ami := NewAdjustedMatchInfo(rmi, nil, NewMaxMatchMap(nil, nil, nil), EmptyGroupByMappings())

	if !ami.IsAdjusted() {
		t.Fatal("AdjustedMatchInfo.IsAdjusted() should be true")
	}
	if ami.IsRegular() {
		t.Fatal("AdjustedMatchInfo.IsRegular() should be false")
	}
}

func TestAdjustedMatchInfo_DelegatesToUnderlying(t *testing.T) {
	t.Parallel()

	rmi := NewRegularMatchInfo(nil, EmptyAliasMap(), &PredicateMultiMap{},
		nil, NewMaxMatchMap(nil, nil, nil), EmptyGroupByMappings(), nil, &QueryPlanConstraint{})
	ami := NewAdjustedMatchInfo(rmi, nil, NewMaxMatchMap(nil, nil, nil), EmptyGroupByMappings())

	if got := ami.GetRegularMatchInfo(); got != rmi {
		t.Fatal("AdjustedMatchInfo.GetRegularMatchInfo should delegate to underlying")
	}
}

func TestAdjustedMatchInfo_NestedDelegation(t *testing.T) {
	t.Parallel()

	// Adjusted wrapping adjusted wrapping regular: GetRegularMatchInfo
	// must still return the original RegularMatchInfo.
	rmi := NewRegularMatchInfo(nil, EmptyAliasMap(), &PredicateMultiMap{},
		nil, NewMaxMatchMap(nil, nil, nil), EmptyGroupByMappings(), nil, &QueryPlanConstraint{})
	inner := NewAdjustedMatchInfo(rmi, nil, NewMaxMatchMap(nil, nil, nil), EmptyGroupByMappings())
	outer := NewAdjustedMatchInfo(inner, nil, NewMaxMatchMap(nil, nil, nil), EmptyGroupByMappings())

	if got := outer.GetRegularMatchInfo(); got != rmi {
		t.Fatal("nested AdjustedMatchInfo.GetRegularMatchInfo must reach the original RegularMatchInfo")
	}
}

func TestAdjustedMatchInfo_ImplementsMatchInfo(t *testing.T) {
	t.Parallel()

	var _ MatchInfo = (*AdjustedMatchInfo)(nil)
}

func TestAdjustedBuilder_SetAndBuild(t *testing.T) {
	t.Parallel()

	rmi := NewRegularMatchInfo(nil, EmptyAliasMap(), &PredicateMultiMap{},
		nil, NewMaxMatchMap(nil, nil, nil), EmptyGroupByMappings(), nil, &QueryPlanConstraint{})

	newMop := []*MatchedOrderingPart{
		NewMatchedOrderingPart(values.NamedCorrelationIdentifier("y"),
			testVal(7), nil, MatchedSortOrderAscendingNullsLast),
	}
	newMmm := NewMaxMatchMap(nil, testVal(10), testVal(20))
	newGbm := EmptyGroupByMappings()

	builder := NewAdjustedBuilder(rmi)

	// Before setting: builder inherits from rmi (which had nil ordering parts).
	if parts := builder.GetMatchedOrderingParts(); parts != nil && len(parts) != 0 {
		t.Fatalf("builder should inherit nil/empty ordering parts, got len=%d", len(parts))
	}

	builder.SetMatchedOrderingParts(newMop).
		SetMaxMatchMap(newMmm).
		SetGroupByMappings(newGbm)

	// Verify builder getters return the overridden values.
	if len(builder.GetMatchedOrderingParts()) != 1 {
		t.Fatalf("builder matchedOrderingParts len=%d, want 1", len(builder.GetMatchedOrderingParts()))
	}
	if builder.GetMaxMatchMap() != newMmm {
		t.Fatal("builder maxMatchMap mismatch")
	}
	if builder.GetGroupByMappings() != newGbm {
		t.Fatal("builder groupByMappings mismatch")
	}

	ami := builder.Build()

	if ami.IsRegular() {
		t.Fatal("built AdjustedMatchInfo should not be regular")
	}
	if !ami.IsAdjusted() {
		t.Fatal("built AdjustedMatchInfo should be adjusted")
	}
	if ami.GetRegularMatchInfo() != rmi {
		t.Fatal("built AdjustedMatchInfo should delegate to original RegularMatchInfo")
	}
	if len(ami.GetMatchedOrderingParts()) != 1 {
		t.Fatal("built AdjustedMatchInfo matchedOrderingParts mismatch")
	}
	if ami.GetMaxMatchMap() != newMmm {
		t.Fatal("built AdjustedMatchInfo maxMatchMap mismatch")
	}
	if ami.GetGroupByMappings() != newGbm {
		t.Fatal("built AdjustedMatchInfo groupByMappings mismatch")
	}
}

func TestAdjustedBuilder_ChainedSetters(t *testing.T) {
	t.Parallel()

	rmi := NewRegularMatchInfo(nil, EmptyAliasMap(), &PredicateMultiMap{},
		nil, NewMaxMatchMap(nil, nil, nil), EmptyGroupByMappings(), nil, &QueryPlanConstraint{})

	// Verify chaining returns the same builder.
	b := NewAdjustedBuilder(rmi)
	got := b.SetMatchedOrderingParts(nil).SetMaxMatchMap(NewMaxMatchMap(nil, nil, nil)).SetGroupByMappings(EmptyGroupByMappings())
	if got != b {
		t.Fatal("chained setters should return the same builder instance")
	}
}

func TestMaxMatchMap_Construction(t *testing.T) {
	t.Parallel()

	q := testVal(1)
	c := testVal(2)
	mapping := map[values.Value]values.Value{q: c}

	mmm := NewMaxMatchMap(mapping, q, c)

	if mmm.GetQueryValue() != q {
		t.Fatal("queryValue mismatch")
	}
	if mmm.GetCandidateValue() != c {
		t.Fatal("candidateValue mismatch")
	}
	if len(mmm.GetMap()) != 1 {
		t.Fatalf("mapping len=%d, want 1", len(mmm.GetMap()))
	}
}

func TestMaxMatchMap_DefensiveCopy(t *testing.T) {
	t.Parallel()

	q := testVal(1)
	c := testVal(2)
	mapping := map[values.Value]values.Value{q: c}

	mmm := NewMaxMatchMap(mapping, q, c)

	// Mutate the original map.
	extra := testVal(3)
	mapping[extra] = extra

	if len(mmm.GetMap()) != 1 {
		t.Fatal("mutation of original mapping leaked into MaxMatchMap")
	}
}

func TestMaxMatchMap_NilInputs(t *testing.T) {
	t.Parallel()

	mmm := NewMaxMatchMap(nil, nil, nil)

	if mmm.GetQueryValue() != nil {
		t.Fatal("nil queryValue should stay nil")
	}
	if mmm.GetCandidateValue() != nil {
		t.Fatal("nil candidateValue should stay nil")
	}
	if mmm.GetMap() == nil {
		t.Fatal("map should be non-nil even when input is nil")
	}
	if len(mmm.GetMap()) != 0 {
		t.Fatal("map should be empty when input is nil")
	}
}

func TestNewAdjustedBuilder_InheritsFromUnderlying(t *testing.T) {
	t.Parallel()

	mop := []*MatchedOrderingPart{
		NewMatchedOrderingPart(values.NamedCorrelationIdentifier("z"),
			testVal(55), nil, MatchedSortOrderDescendingNullsFirst),
	}
	mmm := NewMaxMatchMap(nil, testVal(1), testVal(2))
	gbm := EmptyGroupByMappings()

	rmi := NewRegularMatchInfo(nil, EmptyAliasMap(), &PredicateMultiMap{},
		mop, mmm, gbm, nil, &QueryPlanConstraint{})

	builder := NewAdjustedBuilder(rmi)

	// Builder should inherit values from the underlying MatchInfo.
	if len(builder.GetMatchedOrderingParts()) != 1 {
		t.Fatal("builder should inherit matchedOrderingParts from underlying")
	}
	if builder.GetMaxMatchMap() != mmm {
		t.Fatal("builder should inherit maxMatchMap from underlying")
	}
	if builder.GetGroupByMappings() != gbm {
		t.Fatal("builder should inherit groupByMappings from underlying")
	}
}
