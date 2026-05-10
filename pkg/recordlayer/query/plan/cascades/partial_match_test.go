package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func makeTestPartialMatch(t *testing.T) (*PartialMatchImpl, *AliasMap, MatchCandidate, *expressions.Reference, expressions.RelationalExpression, *expressions.Reference, *RegularMatchInfo) {
	t.Helper()

	aliasMap := AliasMapOfAliases(
		values.NamedCorrelationIdentifier("q1"),
		values.NamedCorrelationIdentifier("c1"),
	)
	candidate := stubMatchCandidate{name: "idx_price"}

	scanExpr := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	queryRef := expressions.InitialOf(scanExpr)

	candScanExpr := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	candidateRef := expressions.InitialOf(candScanExpr)

	matchInfo := NewRegularMatchInfo(
		nil, // parameterBindingMap
		nil, // bindingAliasMap
		nil, // predicateMap
		nil, // matchedOrderingParts
		nil, // maxMatchMap
		nil, // groupByMappings
		nil, // rollUpToGroupingValues
		nil, // additionalPlanConstraint
	)

	pm := NewPartialMatch(aliasMap, candidate, queryRef, scanExpr, candidateRef, matchInfo)
	return pm, aliasMap, candidate, queryRef, scanExpr, candidateRef, matchInfo
}

func TestPartialMatch_ConstructionAndGetters(t *testing.T) {
	t.Parallel()

	pm, aliasMap, candidate, queryRef, scanExpr, candidateRef, matchInfo := makeTestPartialMatch(t)

	if got := pm.GetBoundAliasMap(); got != aliasMap {
		t.Fatalf("GetBoundAliasMap: got %p, want %p", got, aliasMap)
	}
	if got := pm.GetMatchCandidate(); got != candidate {
		t.Fatalf("GetMatchCandidate: got %v, want %v", got, candidate)
	}
	if got := pm.GetQueryRef(); got != queryRef {
		t.Fatalf("GetQueryRef: got %p, want %p", got, queryRef)
	}
	if got := pm.GetQueryExpression(); got != scanExpr {
		t.Fatalf("GetQueryExpression: got %p, want %p", got, scanExpr)
	}
	if got := pm.GetCandidateRef(); got != candidateRef {
		t.Fatalf("GetCandidateRef: got %p, want %p", got, candidateRef)
	}
	if got := pm.GetMatchInfo(); got != matchInfo {
		t.Fatalf("GetMatchInfo: got %p, want %p", got, matchInfo)
	}
}

func TestPartialMatch_SatisfiesInterface(t *testing.T) {
	t.Parallel()

	pm, _, candidate, _, _, _, matchInfo := makeTestPartialMatch(t)

	// Assign to the interface to verify satisfaction at the call site.
	var iface PartialMatch = pm

	if got := iface.GetMatchCandidate(); got != candidate {
		t.Fatalf("PartialMatch.GetMatchCandidate: got %v, want %v", got, candidate)
	}
	if got := iface.GetMatchInfo(); got != matchInfo {
		t.Fatalf("PartialMatch.GetMatchInfo: got %p, want %p", got, matchInfo)
	}
}

func TestPartialMatch_GetRegularMatchInfo(t *testing.T) {
	t.Parallel()

	pm, _, _, _, _, _, matchInfo := makeTestPartialMatch(t)

	got := pm.GetRegularMatchInfo()
	if got != matchInfo {
		t.Fatalf("GetRegularMatchInfo: got %p, want %p (same RegularMatchInfo)", got, matchInfo)
	}
}

func TestPartialMatch_GetRegularMatchInfo_ViaAdjusted(t *testing.T) {
	t.Parallel()

	regularInfo := NewRegularMatchInfo(nil, nil, nil, nil, nil, nil, nil, nil)
	adjusted := NewAdjustedMatchInfo(regularInfo, nil, nil, nil)

	scanExpr := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	pm := NewPartialMatch(
		EmptyAliasMap(),
		stubMatchCandidate{name: "idx"},
		expressions.InitialOf(scanExpr),
		scanExpr,
		expressions.InitialOf(scanExpr),
		adjusted,
	)

	got := pm.GetRegularMatchInfo()
	if got != regularInfo {
		t.Fatalf("GetRegularMatchInfo via AdjustedMatchInfo: got %p, want %p", got, regularInfo)
	}
}

func TestPartialMatch_String(t *testing.T) {
	t.Parallel()

	pm, _, _, _, _, _, _ := makeTestPartialMatch(t)

	want := "FullUnorderedScanExpression[idx_price]"
	got := pm.String()
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestMatchPartition_ConstructionAndGetter(t *testing.T) {
	t.Parallel()

	pm1, _, _, _, _, _, _ := makeTestPartialMatch(t)
	pm2 := NewPartialMatch(
		EmptyAliasMap(),
		stubMatchCandidate{name: "idx_name"},
		pm1.GetQueryRef(),
		pm1.GetQueryExpression(),
		pm1.GetCandidateRef(),
		pm1.GetMatchInfo(),
	)

	matches := []PartialMatch{pm1, pm2}
	mp := NewMatchPartition(matches)

	got := mp.GetPartialMatches()
	if len(got) != 2 {
		t.Fatalf("GetPartialMatches: len=%d, want 2", len(got))
	}
	if got[0] != pm1 || got[1] != pm2 {
		t.Fatal("GetPartialMatches: elements don't match input")
	}
}

func TestMatchPartition_DefensiveCopy(t *testing.T) {
	t.Parallel()

	pm, _, _, _, _, _, _ := makeTestPartialMatch(t)

	original := []PartialMatch{pm}
	mp := NewMatchPartition(original)

	// Mutate the original slice.
	original[0] = nil

	// The partition's internal copy must be unaffected.
	got := mp.GetPartialMatches()
	if got[0] != pm {
		t.Fatal("MatchPartition did not defensively copy: mutation of original slice was visible")
	}
}

func TestMatchPartition_Empty(t *testing.T) {
	t.Parallel()

	mp := NewMatchPartition(nil)
	if got := mp.GetPartialMatches(); len(got) != 0 {
		t.Fatalf("empty partition: len=%d, want 0", len(got))
	}
}
