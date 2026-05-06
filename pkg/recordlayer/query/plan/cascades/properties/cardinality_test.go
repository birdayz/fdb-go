package properties_test

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

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

	// Filter has lower cardinality than Scan → CardinalityLess(filter, scan)
	// should be true.
	if !properties.CardinalityLess(filter, scan) {
		t.Fatal("CardinalityLess(filter, scan) = false, want true (filter narrows)")
	}
	// Reverse: CardinalityLess(scan, filter) should be false.
	if properties.CardinalityLess(scan, filter) {
		t.Fatal("CardinalityLess(scan, filter) = true, want false")
	}
}
