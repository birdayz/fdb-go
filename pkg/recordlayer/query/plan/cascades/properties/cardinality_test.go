package properties_test

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// ---------------------------------------------------------------------------
// Cardinality factory tests
// ---------------------------------------------------------------------------

func TestOfCardinality(t *testing.T) {
	t.Parallel()
	c := properties.OfCardinality(42)
	if c.IsUnknown() {
		t.Fatal("expected known cardinality")
	}
	if c.Value() != 42 {
		t.Fatalf("expected 42, got %d", c.Value())
	}
}

func TestOfCardinalityZero(t *testing.T) {
	t.Parallel()
	c := properties.OfCardinality(0)
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
	properties.OfCardinality(-1)
}

func TestUnknownCardinality(t *testing.T) {
	t.Parallel()
	c := properties.UnknownCardinality()
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
	properties.UnknownCardinality().Value()
}

// ---------------------------------------------------------------------------
// Cardinality.Times tests
// ---------------------------------------------------------------------------

func TestCardinalityTimes_KnownKnown(t *testing.T) {
	t.Parallel()
	a := properties.OfCardinality(3)
	b := properties.OfCardinality(5)
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
	a := properties.OfCardinality(3)
	b := properties.UnknownCardinality()
	result := a.Times(b)
	if !result.IsUnknown() {
		t.Fatal("expected unknown result")
	}
}

func TestCardinalityTimes_UnknownKnown(t *testing.T) {
	t.Parallel()
	a := properties.UnknownCardinality()
	b := properties.OfCardinality(5)
	result := a.Times(b)
	if !result.IsUnknown() {
		t.Fatal("expected unknown result")
	}
}

func TestCardinalityTimes_UnknownUnknown(t *testing.T) {
	t.Parallel()
	a := properties.UnknownCardinality()
	b := properties.UnknownCardinality()
	result := a.Times(b)
	if !result.IsUnknown() {
		t.Fatal("expected unknown result")
	}
}

func TestCardinalityTimes_Zero(t *testing.T) {
	t.Parallel()
	a := properties.OfCardinality(0)
	b := properties.OfCardinality(100)
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
	c := properties.OfCardinality(2)
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
	c := properties.OfCardinality(10)
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
	c := properties.OfCardinality(5)
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
	c := properties.UnknownCardinality()
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
		a, b properties.Cardinality
		want bool
	}{
		{"known==known same", properties.OfCardinality(5), properties.OfCardinality(5), true},
		{"known!=known diff", properties.OfCardinality(5), properties.OfCardinality(3), false},
		{"unknown==unknown", properties.UnknownCardinality(), properties.UnknownCardinality(), true},
		{"known!=unknown", properties.OfCardinality(5), properties.UnknownCardinality(), false},
		{"unknown!=known", properties.UnknownCardinality(), properties.OfCardinality(5), false},
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
	c := properties.ExactlyOne()
	assertKnown(t, c.GetMinCardinality(), 1)
	assertKnown(t, c.GetMaxCardinality(), 1)
}

func TestAtMostOne(t *testing.T) {
	t.Parallel()
	c := properties.AtMostOne()
	assertKnown(t, c.GetMinCardinality(), 0)
	assertKnown(t, c.GetMaxCardinality(), 1)
}

func TestUnknownMaxCardinalityFactory(t *testing.T) {
	t.Parallel()
	c := properties.UnknownMaxCardinality()
	assertKnown(t, c.GetMinCardinality(), 0)
	if !c.GetMaxCardinality().IsUnknown() {
		t.Fatal("expected unknown max")
	}
}

func TestUnknownCardinalities(t *testing.T) {
	t.Parallel()
	c := properties.UnknownCardinalities()
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
	a := properties.Cardinalities{
		Min: properties.OfCardinality(2),
		Max: properties.OfCardinality(10),
	}
	b := properties.Cardinalities{
		Min: properties.OfCardinality(3),
		Max: properties.OfCardinality(5),
	}
	result := a.Times(b)
	assertKnown(t, result.GetMinCardinality(), 6)
	assertKnown(t, result.GetMaxCardinality(), 50)
}

func TestCardinalitiesTimes_UnknownMax(t *testing.T) {
	t.Parallel()
	a := properties.UnknownMaxCardinality()
	b := properties.ExactlyOne()
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
	c := properties.AtMostOne() // min=0, max=1
	result := c.Floor(2)
	assertKnown(t, result.GetMinCardinality(), 2)
	assertKnown(t, result.GetMaxCardinality(), 2)
}

func TestCardinalitiesFloor_BothAboveMinimum(t *testing.T) {
	t.Parallel()
	c := properties.Cardinalities{
		Min: properties.OfCardinality(5),
		Max: properties.OfCardinality(10),
	}
	result := c.Floor(2)
	assertKnown(t, result.GetMinCardinality(), 5)
	assertKnown(t, result.GetMaxCardinality(), 10)
}

func TestCardinalitiesFloor_UnknownUnchanged(t *testing.T) {
	t.Parallel()
	c := properties.UnknownCardinalities()
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
	a := properties.Cardinalities{
		Min: properties.OfCardinality(5),
		Max: properties.OfCardinality(100),
	}
	b := properties.Cardinalities{
		Min: properties.OfCardinality(10),
		Max: properties.OfCardinality(50),
	}
	result := properties.IntersectCardinalities([]properties.Cardinalities{a, b})
	// min: both known -> 0 (intersection can be empty)
	assertKnown(t, result.GetMinCardinality(), 0)
	// max: min(100, 50) = 50
	assertKnown(t, result.GetMaxCardinality(), 50)
}

func TestIntersectCardinalities_OneUnknownMin(t *testing.T) {
	t.Parallel()
	a := properties.Cardinalities{
		Min: properties.OfCardinality(5),
		Max: properties.OfCardinality(100),
	}
	b := properties.Cardinalities{
		Min: properties.UnknownCardinality(),
		Max: properties.OfCardinality(50),
	}
	result := properties.IntersectCardinalities([]properties.Cardinalities{a, b})
	// min: first known, second unknown -> unknown
	if !result.GetMinCardinality().IsUnknown() {
		t.Fatal("expected unknown min")
	}
	assertKnown(t, result.GetMaxCardinality(), 50)
}

func TestIntersectCardinalities_BothUnknownMax(t *testing.T) {
	t.Parallel()
	a := properties.UnknownMaxCardinality()
	b := properties.UnknownMaxCardinality()
	result := properties.IntersectCardinalities([]properties.Cardinalities{a, b})
	assertKnown(t, result.GetMinCardinality(), 0)
	if !result.GetMaxCardinality().IsUnknown() {
		t.Fatal("expected unknown max")
	}
}

func TestIntersectCardinalities_Empty(t *testing.T) {
	t.Parallel()
	result := properties.IntersectCardinalities(nil)
	if !result.GetMinCardinality().IsUnknown() {
		t.Fatal("expected unknown min for empty input")
	}
	if !result.GetMaxCardinality().IsUnknown() {
		t.Fatal("expected unknown max for empty input")
	}
}

func TestIntersectCardinalities_Single(t *testing.T) {
	t.Parallel()
	a := properties.Cardinalities{
		Min: properties.OfCardinality(5),
		Max: properties.OfCardinality(100),
	}
	result := properties.IntersectCardinalities([]properties.Cardinalities{a})
	assertKnown(t, result.GetMinCardinality(), 5)
	assertKnown(t, result.GetMaxCardinality(), 100)
}

// ---------------------------------------------------------------------------
// UnionCardinalities tests
// ---------------------------------------------------------------------------

func TestUnionCardinalities_TwoKnown(t *testing.T) {
	t.Parallel()
	a := properties.Cardinalities{
		Min: properties.OfCardinality(5),
		Max: properties.OfCardinality(100),
	}
	b := properties.Cardinalities{
		Min: properties.OfCardinality(10),
		Max: properties.OfCardinality(50),
	}
	result := properties.UnionCardinalities([]properties.Cardinalities{a, b})
	assertKnown(t, result.GetMinCardinality(), 15)
	assertKnown(t, result.GetMaxCardinality(), 150)
}

func TestUnionCardinalities_OneUnknownMin(t *testing.T) {
	t.Parallel()
	a := properties.Cardinalities{
		Min: properties.OfCardinality(5),
		Max: properties.OfCardinality(100),
	}
	b := properties.Cardinalities{
		Min: properties.UnknownCardinality(),
		Max: properties.OfCardinality(50),
	}
	result := properties.UnionCardinalities([]properties.Cardinalities{a, b})
	if !result.GetMinCardinality().IsUnknown() {
		t.Fatal("expected unknown min")
	}
	assertKnown(t, result.GetMaxCardinality(), 150)
}

func TestUnionCardinalities_OneUnknownMax(t *testing.T) {
	t.Parallel()
	a := properties.Cardinalities{
		Min: properties.OfCardinality(5),
		Max: properties.UnknownCardinality(),
	}
	b := properties.Cardinalities{
		Min: properties.OfCardinality(10),
		Max: properties.OfCardinality(50),
	}
	result := properties.UnionCardinalities([]properties.Cardinalities{a, b})
	assertKnown(t, result.GetMinCardinality(), 15)
	if !result.GetMaxCardinality().IsUnknown() {
		t.Fatal("expected unknown max")
	}
}

func TestUnionCardinalities_Empty(t *testing.T) {
	t.Parallel()
	result := properties.UnionCardinalities(nil)
	assertKnown(t, result.GetMinCardinality(), 0)
	if !result.GetMaxCardinality().IsUnknown() {
		t.Fatal("expected unknown max for empty union")
	}
}

func TestUnionCardinalities_Single(t *testing.T) {
	t.Parallel()
	a := properties.Cardinalities{
		Min: properties.OfCardinality(3),
		Max: properties.OfCardinality(7),
	}
	result := properties.UnionCardinalities([]properties.Cardinalities{a})
	assertKnown(t, result.GetMinCardinality(), 3)
	assertKnown(t, result.GetMaxCardinality(), 7)
}

func TestUnionCardinalities_Three(t *testing.T) {
	t.Parallel()
	a := properties.Cardinalities{Min: properties.OfCardinality(1), Max: properties.OfCardinality(10)}
	b := properties.Cardinalities{Min: properties.OfCardinality(2), Max: properties.OfCardinality(20)}
	c := properties.Cardinalities{Min: properties.OfCardinality(3), Max: properties.OfCardinality(30)}
	result := properties.UnionCardinalities([]properties.Cardinalities{a, b, c})
	assertKnown(t, result.GetMinCardinality(), 6)
	assertKnown(t, result.GetMaxCardinality(), 60)
}

// ---------------------------------------------------------------------------
// WeakenCardinalities tests
// ---------------------------------------------------------------------------

func TestWeakenCardinalities_TwoKnown(t *testing.T) {
	t.Parallel()
	a := properties.Cardinalities{
		Min: properties.OfCardinality(5),
		Max: properties.OfCardinality(100),
	}
	b := properties.Cardinalities{
		Min: properties.OfCardinality(10),
		Max: properties.OfCardinality(50),
	}
	result := properties.WeakenCardinalities([]properties.Cardinalities{a, b})
	// min: min(5, 10) = 5
	assertKnown(t, result.GetMinCardinality(), 5)
	// max: max(100, 50) = 100
	assertKnown(t, result.GetMaxCardinality(), 100)
}

func TestWeakenCardinalities_OneUnknownMin(t *testing.T) {
	t.Parallel()
	a := properties.Cardinalities{
		Min: properties.OfCardinality(5),
		Max: properties.OfCardinality(100),
	}
	b := properties.Cardinalities{
		Min: properties.UnknownCardinality(),
		Max: properties.OfCardinality(50),
	}
	result := properties.WeakenCardinalities([]properties.Cardinalities{a, b})
	// min: known(5) weakened by unknown -> unknown (less constraining)
	if !result.GetMinCardinality().IsUnknown() {
		t.Fatal("expected unknown min")
	}
	assertKnown(t, result.GetMaxCardinality(), 100)
}

func TestWeakenCardinalities_OneUnknownMax(t *testing.T) {
	t.Parallel()
	a := properties.Cardinalities{
		Min: properties.OfCardinality(5),
		Max: properties.OfCardinality(100),
	}
	b := properties.Cardinalities{
		Min: properties.OfCardinality(10),
		Max: properties.UnknownCardinality(),
	}
	result := properties.WeakenCardinalities([]properties.Cardinalities{a, b})
	assertKnown(t, result.GetMinCardinality(), 5)
	if !result.GetMaxCardinality().IsUnknown() {
		t.Fatal("expected unknown max")
	}
}

func TestWeakenCardinalities_Empty(t *testing.T) {
	t.Parallel()
	result := properties.WeakenCardinalities(nil)
	assertKnown(t, result.GetMinCardinality(), 0)
	if !result.GetMaxCardinality().IsUnknown() {
		t.Fatal("expected unknown max for empty weaken")
	}
}

func TestWeakenCardinalities_Single(t *testing.T) {
	t.Parallel()
	a := properties.Cardinalities{
		Min: properties.OfCardinality(3),
		Max: properties.OfCardinality(7),
	}
	result := properties.WeakenCardinalities([]properties.Cardinalities{a})
	assertKnown(t, result.GetMinCardinality(), 3)
	assertKnown(t, result.GetMaxCardinality(), 7)
}

func TestWeakenCardinalities_MinPicksSmaller(t *testing.T) {
	t.Parallel()
	a := properties.Cardinalities{Min: properties.OfCardinality(10), Max: properties.OfCardinality(20)}
	b := properties.Cardinalities{Min: properties.OfCardinality(3), Max: properties.OfCardinality(30)}
	result := properties.WeakenCardinalities([]properties.Cardinalities{a, b})
	assertKnown(t, result.GetMinCardinality(), 3)
	assertKnown(t, result.GetMaxCardinality(), 30)
}

// ---------------------------------------------------------------------------
// Cardinalities.Equal tests
// ---------------------------------------------------------------------------

func TestCardinalitiesEqual(t *testing.T) {
	t.Parallel()
	a := properties.ExactlyOne()
	b := properties.ExactlyOne()
	if !a.Equal(b) {
		t.Fatal("expected equal")
	}
	c := properties.AtMostOne()
	if a.Equal(c) {
		t.Fatal("expected not equal")
	}
}

// ---------------------------------------------------------------------------
// PropertyMap.GetCardinalities tests
// ---------------------------------------------------------------------------

func TestPropertyMapGetCardinalities_Present(t *testing.T) {
	t.Parallel()
	m := properties.PropertyMap{
		properties.PropCardinalities: properties.ExactlyOne(),
	}
	got := m.GetCardinalities()
	if !got.Equal(properties.ExactlyOne()) {
		t.Fatalf("expected ExactlyOne, got %+v", got)
	}
}

func TestPropertyMapGetCardinalities_Absent(t *testing.T) {
	t.Parallel()
	m := properties.PropertyMap{}
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
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	got := properties.EstimateCardinality(scan)
	want := properties.EstimateCost(scan).Cardinality
	if got != want {
		t.Fatalf("EstimateCardinality = %v, EstimateCost.Cardinality = %v (must match)", got, want)
	}
}

func TestEstimateCardinality_FilterReducesCardinality(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	)
	scanCard := properties.EstimateCardinality(scan)
	filterCard := properties.EstimateCardinality(filter)
	if filterCard >= scanCard {
		t.Fatalf("filter cardinality %v should be less than scan cardinality %v", filterCard, scanCard)
	}
}

func TestEstimateCardinalityWith_UsesStatistics(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"BigTable"}, values.UnknownType)
	stats := properties.MapStatistics{
		PerType: map[string]float64{"BigTable": 5_000_000},
	}
	got := properties.EstimateCardinalityWith(scan, stats)
	if got != 5_000_000 {
		t.Fatalf("EstimateCardinalityWith = %v, want 5_000_000 (from stats)", got)
	}
}

func TestBestRefCardinality_PicksMinAcrossMembers(t *testing.T) {
	t.Parallel()
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	ref := expressions.InitialOf(scanA)
	ref.Insert(scanB)

	// Both scans have the same default cardinality, so BestRefCardinality
	// returns that value.
	want := properties.EstimateCardinality(scanA)
	got := properties.BestRefCardinality(ref)
	if got != want {
		t.Fatalf("BestRefCardinality = %v, want %v", got, want)
	}
}

func TestCardinalityLess_OrdersBySize(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	)

	// Filter has lower cardinality than Scan -> CardinalityLess(filter, scan)
	// should be true.
	if !properties.CardinalityLess(filter, scan) {
		t.Fatal("CardinalityLess(filter, scan) = false, want true (filter narrows)")
	}
	// Reverse: CardinalityLess(scan, filter) should be false.
	if properties.CardinalityLess(scan, filter) {
		t.Fatal("CardinalityLess(scan, filter) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func assertKnown(t *testing.T, c properties.Cardinality, want int64) {
	t.Helper()
	if c.IsUnknown() {
		t.Fatalf("expected known cardinality %d, got unknown", want)
	}
	if c.Value() != want {
		t.Fatalf("expected cardinality %d, got %d", want, c.Value())
	}
}
