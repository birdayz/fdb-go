package cascades

import (
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func makeSingleMatchedAccess(t *testing.T) (*SingleMatchedAccess, *PartialMatchImpl) {
	t.Helper()

	pm, _, _, _, _, _, _ := makeTestPartialMatch(t)

	candidateTopAlias := values.NamedCorrelationIdentifier("top1")
	translationMap := EmptyTranslationMap()

	orderings := []*RequestedOrdering{
		NewRequestedOrdering([]RequestedOrderingPart{
			{Value: values.NewQueriedValue([]string{"T"}, values.UnknownType), SortOrder: RequestedSortOrderAscending},
		}, DistinctnessNotDistinct, false),
	}

	sma := NewSingleMatchedAccess(pm, NoCompensation, candidateTopAlias, false, translationMap, orderings)
	return sma, pm
}

func TestSingleMatchedAccess_ConstructionAndGetters(t *testing.T) {
	t.Parallel()

	sma, pm := makeSingleMatchedAccess(t)

	if got := sma.GetPartialMatch(); got != pm {
		t.Fatalf("GetPartialMatch: got %p, want %p", got, pm)
	}
	if got := sma.GetCompensation(); got != NoCompensation {
		t.Fatalf("GetCompensation: got %v, want NoCompensation", got)
	}
	if got := sma.GetCandidateTopAlias(); got.Name() != "top1" {
		t.Fatalf("GetCandidateTopAlias: got %q, want %q", got.Name(), "top1")
	}
	if got := sma.IsReverseScanOrder(); got {
		t.Fatalf("IsReverseScanOrder: got %v, want false", got)
	}
	if got := sma.GetTopToTopTranslationMap(); got == nil {
		t.Fatal("GetTopToTopTranslationMap: got nil")
	}
	if got := sma.GetSatisfyingRequestedOrderings(); len(got) != 1 {
		t.Fatalf("GetSatisfyingRequestedOrderings: len=%d, want 1", len(got))
	}
}

func TestSingleMatchedAccess_ReverseScanOrder(t *testing.T) {
	t.Parallel()

	pm, _, _, _, _, _, _ := makeTestPartialMatch(t)
	sma := NewSingleMatchedAccess(
		pm, NoCompensation,
		values.NamedCorrelationIdentifier("top2"),
		true, // reverse
		EmptyTranslationMap(),
		nil,
	)

	if !sma.IsReverseScanOrder() {
		t.Fatal("IsReverseScanOrder: got false, want true")
	}
}

func TestSingleMatchedAccess_LazyGroupByMappings(t *testing.T) {
	t.Parallel()

	gbm := EmptyGroupByMappings()
	matchInfo := NewRegularMatchInfo(nil, nil, nil, nil, nil, gbm, nil, nil)

	scanExpr := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	pm := NewPartialMatch(
		EmptyAliasMap(),
		stubMatchCandidate{name: "idx"},
		expressions.InitialOf(scanExpr),
		scanExpr,
		expressions.InitialOf(scanExpr),
		matchInfo,
	)

	sma := NewSingleMatchedAccess(
		pm, NoCompensation,
		values.NamedCorrelationIdentifier("top"),
		false,
		EmptyTranslationMap(),
		nil,
	)

	// First call computes it.
	got1 := sma.GetPulledUpGroupByMappingsForOrdering()
	if got1 == nil {
		t.Fatal("GetPulledUpGroupByMappingsForOrdering returned nil")
	}

	// Second call returns the same cached value (pointer identity).
	got2 := sma.GetPulledUpGroupByMappingsForOrdering()
	if got2 != got1 {
		t.Fatalf("GetPulledUpGroupByMappingsForOrdering not cached: %p != %p", got2, got1)
	}
}

func TestSingleMatchedAccess_EmptySatisfyingOrderings(t *testing.T) {
	t.Parallel()

	pm, _, _, _, _, _, _ := makeTestPartialMatch(t)
	sma := NewSingleMatchedAccess(
		pm, NoCompensation,
		values.NamedCorrelationIdentifier("top"),
		false,
		EmptyTranslationMap(),
		nil,
	)

	got := sma.GetSatisfyingRequestedOrderings()
	if len(got) != 0 {
		t.Fatalf("GetSatisfyingRequestedOrderings: len=%d, want 0", len(got))
	}
}

func TestSingleMatchedAccess_DefensiveCopy(t *testing.T) {
	t.Parallel()

	pm, _, _, _, _, _, _ := makeTestPartialMatch(t)
	orderings := []*RequestedOrdering{
		NewRequestedOrdering(nil, DistinctnessPreserveDistinctness, false),
	}
	sma := NewSingleMatchedAccess(
		pm, NoCompensation,
		values.NamedCorrelationIdentifier("top"),
		false,
		EmptyTranslationMap(),
		orderings,
	)

	// Mutate the original slice.
	orderings[0] = nil

	// Internal copy must be unaffected.
	got := sma.GetSatisfyingRequestedOrderings()
	if got[0] == nil {
		t.Fatal("defensive copy failed: mutation of original slice was visible")
	}
}

func TestSingleMatchedAccess_String(t *testing.T) {
	t.Parallel()

	sma, _ := makeSingleMatchedAccess(t)
	s := sma.String()

	// Verify it contains expected substrings — the exact format mirrors
	// Java's toString which includes partialMatch, compensation, alias,
	// scan direction label.
	if s == "" {
		t.Fatal("String() returned empty")
	}
	// reverseScanOrder=false -> Java uses "reverse" label
	if got := sma.IsReverseScanOrder(); got {
		t.Fatal("precondition: expected reverseScanOrder=false")
	}
	// Verify "reverse" appears (Java's inverted label)
	if !strings.Contains(s, "reverse") {
		t.Fatalf("String() = %q, want to contain 'reverse'", s)
	}
	if !strings.Contains(s, "top1") {
		t.Fatalf("String() = %q, want to contain 'top1'", s)
	}
}
