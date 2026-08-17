package properties

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ---------------------------------------------------------------------------
// Cardinality factory tests
// ---------------------------------------------------------------------------

func TestOfCardinality(t *testing.T) {
	t.Parallel()
	c := OfCardinality(42)
	if c.IsUnknown() {
		t.Fatal("expected known cardinality")
	}
	if c.Value() != 42 {
		t.Fatalf("expected 42, got %d", c.Value())
	}
}

func TestOfCardinalityZero(t *testing.T) {
	t.Parallel()
	c := OfCardinality(0)
	if c.IsUnknown() {
		t.Fatal("expected known cardinality")
	}
	if c.Value() != 0 {
		t.Fatalf("expected 0, got %d", c.Value())
	}
}

func TestOfCardinalityNegativePanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for negative cardinality")
		}
	}()
	OfCardinality(-1)
}

func TestUnknownCardinality(t *testing.T) {
	t.Parallel()
	c := UnknownCardinality()
	if !c.IsUnknown() {
		t.Fatal("expected unknown cardinality")
	}
}

func TestUnknownCardinalityValuePanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when calling Value() on unknown")
		}
	}()
	UnknownCardinality().Value()
}

// ---------------------------------------------------------------------------
// Cardinality.Times tests
// ---------------------------------------------------------------------------

func TestCardinalityTimes_KnownKnown(t *testing.T) {
	t.Parallel()
	a := OfCardinality(3)
	b := OfCardinality(5)
	result := a.Times(b)
	if result.IsUnknown() {
		t.Fatal("expected known result")
	}
	if result.Value() != 15 {
		t.Fatalf("expected 15, got %d", result.Value())
	}
}

func TestCardinalityTimes_KnownUnknown(t *testing.T) {
	t.Parallel()
	a := OfCardinality(3)
	b := UnknownCardinality()
	result := a.Times(b)
	if !result.IsUnknown() {
		t.Fatal("expected unknown result")
	}
}

func TestCardinalityTimes_UnknownKnown(t *testing.T) {
	t.Parallel()
	a := UnknownCardinality()
	b := OfCardinality(5)
	result := a.Times(b)
	if !result.IsUnknown() {
		t.Fatal("expected unknown result")
	}
}

func TestCardinalityTimes_UnknownUnknown(t *testing.T) {
	t.Parallel()
	a := UnknownCardinality()
	b := UnknownCardinality()
	result := a.Times(b)
	if !result.IsUnknown() {
		t.Fatal("expected unknown result")
	}
}

func TestCardinalityTimes_Zero(t *testing.T) {
	t.Parallel()
	a := OfCardinality(0)
	b := OfCardinality(100)
	result := a.Times(b)
	if result.IsUnknown() {
		t.Fatal("expected known result")
	}
	if result.Value() != 0 {
		t.Fatalf("expected 0, got %d", result.Value())
	}
}

// ---------------------------------------------------------------------------
// Cardinality.Floor tests
// ---------------------------------------------------------------------------

func TestCardinalityFloor_KnownBelowMin(t *testing.T) {
	t.Parallel()
	c := OfCardinality(2)
	result := c.Floor(5)
	if result.IsUnknown() {
		t.Fatal("expected known result")
	}
	if result.Value() != 5 {
		t.Fatalf("expected 5, got %d", result.Value())
	}
}

func TestCardinalityFloor_KnownAboveMin(t *testing.T) {
	t.Parallel()
	c := OfCardinality(10)
	result := c.Floor(5)
	if result.IsUnknown() {
		t.Fatal("expected known result")
	}
	if result.Value() != 10 {
		t.Fatalf("expected 10, got %d", result.Value())
	}
}

func TestCardinalityFloor_KnownEqualMin(t *testing.T) {
	t.Parallel()
	c := OfCardinality(5)
	result := c.Floor(5)
	if result.IsUnknown() {
		t.Fatal("expected known result")
	}
	if result.Value() != 5 {
		t.Fatalf("expected 5, got %d", result.Value())
	}
}

func TestCardinalityFloor_UnknownStaysUnknown(t *testing.T) {
	t.Parallel()
	c := UnknownCardinality()
	result := c.Floor(5)
	if !result.IsUnknown() {
		t.Fatal("expected unknown result")
	}
}

// ---------------------------------------------------------------------------
// Cardinality.Equal tests
// ---------------------------------------------------------------------------

func TestCardinalityEqual(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b Cardinality
		want bool
	}{
		{"known==known same", OfCardinality(5), OfCardinality(5), true},
		{"known!=known diff", OfCardinality(5), OfCardinality(3), false},
		{"unknown==unknown", UnknownCardinality(), UnknownCardinality(), true},
		{"known!=unknown", OfCardinality(5), UnknownCardinality(), false},
		{"unknown!=known", UnknownCardinality(), OfCardinality(5), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.a.Equal(tt.b); got != tt.want {
				t.Fatalf("Equal() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Cardinalities factory tests
// ---------------------------------------------------------------------------

func TestExactlyOne(t *testing.T) {
	t.Parallel()
	c := ExactlyOne()
	assertKnown(t, c.GetMinCardinality(), 1)
	assertKnown(t, c.GetMaxCardinality(), 1)
}

func TestAtMostOne(t *testing.T) {
	t.Parallel()
	c := AtMostOne()
	assertKnown(t, c.GetMinCardinality(), 0)
	assertKnown(t, c.GetMaxCardinality(), 1)
}

func TestUnknownMaxCardinalityFactory(t *testing.T) {
	t.Parallel()
	c := UnknownMaxCardinality()
	assertKnown(t, c.GetMinCardinality(), 0)
	if !c.GetMaxCardinality().IsUnknown() {
		t.Fatal("expected unknown max")
	}
}

func TestUnknownCardinalities(t *testing.T) {
	t.Parallel()
	c := UnknownCardinalities()
	if !c.GetMinCardinality().IsUnknown() {
		t.Fatal("expected unknown min")
	}
	if !c.GetMaxCardinality().IsUnknown() {
		t.Fatal("expected unknown max")
	}
}

// ---------------------------------------------------------------------------
// Cardinalities.Times tests
// ---------------------------------------------------------------------------

func TestCardinalitiesTimes_KnownKnown(t *testing.T) {
	t.Parallel()
	a := Cardinalities{
		Min: OfCardinality(2),
		Max: OfCardinality(10),
	}
	b := Cardinalities{
		Min: OfCardinality(3),
		Max: OfCardinality(5),
	}
	result := a.Times(b)
	assertKnown(t, result.GetMinCardinality(), 6)
	assertKnown(t, result.GetMaxCardinality(), 50)
}

func TestCardinalitiesTimes_UnknownMax(t *testing.T) {
	t.Parallel()
	a := UnknownMaxCardinality()
	b := ExactlyOne()
	result := a.Times(b)
	assertKnown(t, result.GetMinCardinality(), 0)
	if !result.GetMaxCardinality().IsUnknown() {
		t.Fatal("expected unknown max")
	}
}

// ---------------------------------------------------------------------------
// Cardinalities.Floor tests
// ---------------------------------------------------------------------------

func TestCardinalitiesFloor_BothBelowMinimum(t *testing.T) {
	t.Parallel()
	c := AtMostOne() // min=0, max=1
	result := c.Floor(2)
	assertKnown(t, result.GetMinCardinality(), 2)
	assertKnown(t, result.GetMaxCardinality(), 2)
}

func TestCardinalitiesFloor_BothAboveMinimum(t *testing.T) {
	t.Parallel()
	c := Cardinalities{
		Min: OfCardinality(5),
		Max: OfCardinality(10),
	}
	result := c.Floor(2)
	assertKnown(t, result.GetMinCardinality(), 5)
	assertKnown(t, result.GetMaxCardinality(), 10)
}

func TestCardinalitiesFloor_UnknownUnchanged(t *testing.T) {
	t.Parallel()
	c := UnknownCardinalities()
	result := c.Floor(5)
	if !result.GetMinCardinality().IsUnknown() {
		t.Fatal("expected unknown min")
	}
	if !result.GetMaxCardinality().IsUnknown() {
		t.Fatal("expected unknown max")
	}
}

// ---------------------------------------------------------------------------
// IntersectCardinalities tests
// ---------------------------------------------------------------------------

func TestIntersectCardinalities_TwoKnown(t *testing.T) {
	t.Parallel()
	a := Cardinalities{
		Min: OfCardinality(5),
		Max: OfCardinality(100),
	}
	b := Cardinalities{
		Min: OfCardinality(10),
		Max: OfCardinality(50),
	}
	result := IntersectCardinalities([]Cardinalities{a, b})
	// min: both known -> 0 (intersection can be empty)
	assertKnown(t, result.GetMinCardinality(), 0)
	// max: min(100, 50) = 50
	assertKnown(t, result.GetMaxCardinality(), 50)
}

func TestIntersectCardinalities_OneUnknownMin(t *testing.T) {
	t.Parallel()
	a := Cardinalities{
		Min: OfCardinality(5),
		Max: OfCardinality(100),
	}
	b := Cardinalities{
		Min: UnknownCardinality(),
		Max: OfCardinality(50),
	}
	result := IntersectCardinalities([]Cardinalities{a, b})
	// min: first known, second unknown -> unknown
	if !result.GetMinCardinality().IsUnknown() {
		t.Fatal("expected unknown min")
	}
	assertKnown(t, result.GetMaxCardinality(), 50)
}

func TestIntersectCardinalities_BothUnknownMax(t *testing.T) {
	t.Parallel()
	a := UnknownMaxCardinality()
	b := UnknownMaxCardinality()
	result := IntersectCardinalities([]Cardinalities{a, b})
	assertKnown(t, result.GetMinCardinality(), 0)
	if !result.GetMaxCardinality().IsUnknown() {
		t.Fatal("expected unknown max")
	}
}

func TestIntersectCardinalities_Empty(t *testing.T) {
	t.Parallel()
	result := IntersectCardinalities(nil)
	if !result.GetMinCardinality().IsUnknown() {
		t.Fatal("expected unknown min for empty input")
	}
	if !result.GetMaxCardinality().IsUnknown() {
		t.Fatal("expected unknown max for empty input")
	}
}

func TestIntersectCardinalities_Single(t *testing.T) {
	t.Parallel()
	a := Cardinalities{
		Min: OfCardinality(5),
		Max: OfCardinality(100),
	}
	result := IntersectCardinalities([]Cardinalities{a})
	assertKnown(t, result.GetMinCardinality(), 5)
	assertKnown(t, result.GetMaxCardinality(), 100)
}

// ---------------------------------------------------------------------------
// UnionCardinalities tests
// ---------------------------------------------------------------------------

func TestUnionCardinalities_TwoKnown(t *testing.T) {
	t.Parallel()
	a := Cardinalities{
		Min: OfCardinality(5),
		Max: OfCardinality(100),
	}
	b := Cardinalities{
		Min: OfCardinality(10),
		Max: OfCardinality(50),
	}
	result := UnionCardinalities([]Cardinalities{a, b})
	assertKnown(t, result.GetMinCardinality(), 15)
	assertKnown(t, result.GetMaxCardinality(), 150)
}

func TestUnionCardinalities_OneUnknownMin(t *testing.T) {
	t.Parallel()
	a := Cardinalities{
		Min: OfCardinality(5),
		Max: OfCardinality(100),
	}
	b := Cardinalities{
		Min: UnknownCardinality(),
		Max: OfCardinality(50),
	}
	result := UnionCardinalities([]Cardinalities{a, b})
	if !result.GetMinCardinality().IsUnknown() {
		t.Fatal("expected unknown min")
	}
	assertKnown(t, result.GetMaxCardinality(), 150)
}

func TestUnionCardinalities_OneUnknownMax(t *testing.T) {
	t.Parallel()
	a := Cardinalities{
		Min: OfCardinality(5),
		Max: UnknownCardinality(),
	}
	b := Cardinalities{
		Min: OfCardinality(10),
		Max: OfCardinality(50),
	}
	result := UnionCardinalities([]Cardinalities{a, b})
	assertKnown(t, result.GetMinCardinality(), 15)
	if !result.GetMaxCardinality().IsUnknown() {
		t.Fatal("expected unknown max")
	}
}

func TestUnionCardinalities_Empty(t *testing.T) {
	t.Parallel()
	result := UnionCardinalities(nil)
	assertKnown(t, result.GetMinCardinality(), 0)
	if !result.GetMaxCardinality().IsUnknown() {
		t.Fatal("expected unknown max for empty union")
	}
}

func TestUnionCardinalities_Single(t *testing.T) {
	t.Parallel()
	a := Cardinalities{
		Min: OfCardinality(3),
		Max: OfCardinality(7),
	}
	result := UnionCardinalities([]Cardinalities{a})
	assertKnown(t, result.GetMinCardinality(), 3)
	assertKnown(t, result.GetMaxCardinality(), 7)
}

func TestUnionCardinalities_Three(t *testing.T) {
	t.Parallel()
	a := Cardinalities{Min: OfCardinality(1), Max: OfCardinality(10)}
	b := Cardinalities{Min: OfCardinality(2), Max: OfCardinality(20)}
	c := Cardinalities{Min: OfCardinality(3), Max: OfCardinality(30)}
	result := UnionCardinalities([]Cardinalities{a, b, c})
	assertKnown(t, result.GetMinCardinality(), 6)
	assertKnown(t, result.GetMaxCardinality(), 60)
}

// ---------------------------------------------------------------------------
// WeakenCardinalities tests
// ---------------------------------------------------------------------------

func TestWeakenCardinalities_TwoKnown(t *testing.T) {
	t.Parallel()
	a := Cardinalities{
		Min: OfCardinality(5),
		Max: OfCardinality(100),
	}
	b := Cardinalities{
		Min: OfCardinality(10),
		Max: OfCardinality(50),
	}
	result := WeakenCardinalities([]Cardinalities{a, b})
	// min: min(5, 10) = 5
	assertKnown(t, result.GetMinCardinality(), 5)
	// max: max(100, 50) = 100
	assertKnown(t, result.GetMaxCardinality(), 100)
}

func TestWeakenCardinalities_OneUnknownMin(t *testing.T) {
	t.Parallel()
	a := Cardinalities{
		Min: OfCardinality(5),
		Max: OfCardinality(100),
	}
	b := Cardinalities{
		Min: UnknownCardinality(),
		Max: OfCardinality(50),
	}
	result := WeakenCardinalities([]Cardinalities{a, b})
	// min: known(5) weakened by unknown -> unknown (less constraining)
	if !result.GetMinCardinality().IsUnknown() {
		t.Fatal("expected unknown min")
	}
	assertKnown(t, result.GetMaxCardinality(), 100)
}

func TestWeakenCardinalities_OneUnknownMax(t *testing.T) {
	t.Parallel()
	a := Cardinalities{
		Min: OfCardinality(5),
		Max: OfCardinality(100),
	}
	b := Cardinalities{
		Min: OfCardinality(10),
		Max: UnknownCardinality(),
	}
	result := WeakenCardinalities([]Cardinalities{a, b})
	assertKnown(t, result.GetMinCardinality(), 5)
	if !result.GetMaxCardinality().IsUnknown() {
		t.Fatal("expected unknown max")
	}
}

func TestWeakenCardinalities_Empty(t *testing.T) {
	t.Parallel()
	result := WeakenCardinalities(nil)
	assertKnown(t, result.GetMinCardinality(), 0)
	if !result.GetMaxCardinality().IsUnknown() {
		t.Fatal("expected unknown max for empty weaken")
	}
}

func TestWeakenCardinalities_Single(t *testing.T) {
	t.Parallel()
	a := Cardinalities{
		Min: OfCardinality(3),
		Max: OfCardinality(7),
	}
	result := WeakenCardinalities([]Cardinalities{a})
	assertKnown(t, result.GetMinCardinality(), 3)
	assertKnown(t, result.GetMaxCardinality(), 7)
}

func TestWeakenCardinalities_MinPicksSmaller(t *testing.T) {
	t.Parallel()
	a := Cardinalities{Min: OfCardinality(10), Max: OfCardinality(20)}
	b := Cardinalities{Min: OfCardinality(3), Max: OfCardinality(30)}
	result := WeakenCardinalities([]Cardinalities{a, b})
	assertKnown(t, result.GetMinCardinality(), 3)
	assertKnown(t, result.GetMaxCardinality(), 30)
}

// ---------------------------------------------------------------------------
// Cardinalities.Equal tests
// ---------------------------------------------------------------------------

func TestCardinalitiesEqual(t *testing.T) {
	t.Parallel()
	a := ExactlyOne()
	b := ExactlyOne()
	if !a.Equal(b) {
		t.Fatal("expected equal")
	}
	c := AtMostOne()
	if a.Equal(c) {
		t.Fatal("expected not equal")
	}
}

// ---------------------------------------------------------------------------
// PropertyMap.GetCardinalities tests
// ---------------------------------------------------------------------------

func TestPropertyMapGetCardinalities_Present(t *testing.T) {
	t.Parallel()
	m := PropertyMap{
		PropCardinalities: ExactlyOne(),
	}
	got := m.GetCardinalities()
	if !got.Equal(ExactlyOne()) {
		t.Fatalf("expected ExactlyOne, got %+v", got)
	}
}

func TestPropertyMapGetCardinalities_Absent(t *testing.T) {
	t.Parallel()
	m := PropertyMap{}
	got := m.GetCardinalities()
	if !got.GetMinCardinality().IsUnknown() || !got.GetMaxCardinality().IsUnknown() {
		t.Fatalf("expected UnknownCardinalities, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Legacy EstimateCardinality tests (pre-existing, kept for regression)
// ---------------------------------------------------------------------------

func TestEstimateCardinality_LeafScan(t *testing.T) {
	t.Parallel()
	scan := mustFullUnorderedScanExpression(t, []string{"T"}, propertyTestFlowedType())
	got := EstimateCardinality(scan)
	want := EstimateCost(scan).Cardinality
	if got != want {
		t.Fatalf("EstimateCardinality = %v, EstimateCost.Cardinality = %v (must match)", got, want)
	}
}

func TestEstimateCardinality_FilterReducesCardinality(t *testing.T) {
	t.Parallel()
	scan := mustFullUnorderedScanExpression(t, []string{"T"}, propertyTestFlowedType())
	pred := predicates.NewValuePredicate(propertyField(t, "active", values.TypeBool))
	filter := mustLogicalFilterExpression(t,
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)))

	scanCard := EstimateCardinality(scan)
	filterCard := EstimateCardinality(filter)
	if filterCard >= scanCard {
		t.Fatalf("filter cardinality %v should be less than scan cardinality %v", filterCard, scanCard)
	}
}

func TestEstimateCardinalityWith_UsesStatistics(t *testing.T) {
	t.Parallel()
	scan := mustFullUnorderedScanExpression(t, []string{"BigTable"}, propertyTestFlowedType())
	stats := MapStatistics{
		PerType: map[string]float64{"BigTable": 5_000_000},
	}
	got := EstimateCardinalityWith(scan, stats)
	if got != 5_000_000 {
		t.Fatalf("EstimateCardinalityWith = %v, want 5_000_000 (from stats)", got)
	}
}

func TestBestRefCardinality_PicksMinAcrossMembers(t *testing.T) {
	t.Parallel()
	scanA := mustFullUnorderedScanExpression(t, []string{"A"}, propertyTestFlowedType())
	scanB := mustFullUnorderedScanExpression(t, []string{"B"}, propertyTestFlowedType())
	ref := expressions.InitialOf(scanA)
	ref.Insert(scanB)

	// Both scans have the same default cardinality, so BestRefCardinality
	// returns that value.
	want := EstimateCardinality(scanA)
	got := BestRefCardinality(ref)
	if got != want {
		t.Fatalf("BestRefCardinality = %v, want %v", got, want)
	}
}

func TestCardinalityLess_OrdersBySize(t *testing.T) {
	t.Parallel()
	scan := mustFullUnorderedScanExpression(t, []string{"T"}, propertyTestFlowedType())
	pred := predicates.NewValuePredicate(propertyField(t, "active", values.TypeBool))
	filter := mustLogicalFilterExpression(t,
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)))

	// Filter has lower cardinality than Scan -> CardinalityLess(filter, scan)
	// should be true.
	if !CardinalityLess(filter, scan) {
		t.Fatal("CardinalityLess(filter, scan) = false, want true (filter narrows)")
	}
	// Reverse: CardinalityLess(scan, filter) should be false.
	if CardinalityLess(scan, filter) {
		t.Fatal("CardinalityLess(scan, filter) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func assertKnown(t *testing.T, c Cardinality, want int64) {
	t.Helper()
	if c.IsUnknown() {
		t.Fatalf("expected known cardinality %d, got unknown", want)
	}
	if c.Value() != want {
		t.Fatalf("expected cardinality %d, got %d", want, c.Value())
	}
}
