package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func makeSingleMatchedAccess(t *testing.T) (*SingleMatchedAccess, *PartialMatchImpl) {
	t.Helper()

	pm, _, _, _, _, _, _ := makeTestPartialMatch(t)

	candidateTopAlias := values.NamedCorrelationIdentifier("top1")
	translationMap := EmptyTranslationMap()

	orderings := []*properties.RequestedOrdering{
		properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
			{Value: values.NewQueriedValue([]string{"T"}, values.UnknownType), SortOrder: properties.RequestedSortOrderAscending},
		}, properties.DistinctnessNotDistinct, false),
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
	orderings := []*properties.RequestedOrdering{
		properties.NewRequestedOrdering(nil, properties.DistinctnessPreserveDistinctness, false),
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

func TestSingleMatchedAccess_MultiMemberCandidateRefFailsGroupMetadataClosed(
	t *testing.T,
) {
	t.Parallel()

	lowerAlias := values.NamedCorrelationIdentifier("multi_member_lower")
	firstResult := values.NewQuantifiedObjectValue(lowerAlias)
	firstCandidate := expressions.NewSelectExpression(firstResult, nil, nil)
	secondCandidate := expressions.NewSelectExpression(
		values.LiteralValue(int64(2)),
		nil,
		nil,
	)
	candidateRef := expressions.InitialOf(firstCandidate)
	if !candidateRef.Insert(secondCandidate) {
		t.Fatal("test candidate alternatives unexpectedly deduplicated")
	}
	if got := len(candidateRef.AllMembers()); got != 2 {
		t.Fatalf("candidate member count = %d, want 2", got)
	}

	queryKey := values.LiteralValue(int64(1))
	matched := NewValueBiMap()
	matched.Put(queryKey, firstResult)
	matchInfo := NewRegularMatchInfo(
		nil,
		EmptyAliasMap(),
		nil,
		nil,
		NewMaxMatchMap(nil, nil, nil),
		NewGroupByMappings(
			matched,
			NewValueBiMap(),
			NewCorrValueBiMap(),
		),
		nil,
		nil,
	)
	queryExpression := expressions.NewSelectExpression(queryKey, nil, nil)
	partialMatch := NewPartialMatch(
		EmptyAliasMap(),
		stubMatchCandidate{name: "multi_member_group_metadata"},
		expressions.InitialOf(queryExpression),
		queryExpression,
		candidateRef,
		matchInfo,
	)
	access := NewSingleMatchedAccess(
		partialMatch,
		NoCompensation,
		values.NamedCorrelationIdentifier("candidate_top"),
		false,
		EmptyTranslationMap(),
		nil,
	)

	pulled := access.GetPulledUpGroupByMappingsForOrdering()
	if pulled == nil {
		t.Fatal("fail-closed group metadata is nil")
	}
	if pulled.MatchedGroupingsMap().Len() != 0 ||
		pulled.MatchedAggregatesMap().Len() != 0 ||
		pulled.UnmatchedAggregatesMap().Len() != 0 {
		t.Fatal("multi-member candidate reference retained arbitrary group metadata")
	}
}
