package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func makeRefTestPartialMatch(t *testing.T, candidateName string) (*PartialMatchImpl, MatchCandidate, *expressions.Reference) {
	t.Helper()
	candidate := stubMatchCandidate{name: candidateName}
	scanExpr := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryRef := expressions.InitialOf(scanExpr)
	candidateRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
	)
	matchInfo := NewRegularMatchInfo(nil, nil, nil, nil, nil, nil, nil, nil)
	pm := NewPartialMatch(EmptyAliasMap(), candidate, queryRef, scanExpr, candidateRef, matchInfo)
	return pm, candidate, queryRef
}

func TestAddAndGetPartialMatchesForCandidate(t *testing.T) {
	t.Parallel()

	pm, candidate, queryRef := makeRefTestPartialMatch(t, "idx_a")

	added := AddPartialMatchForCandidate(queryRef, candidate, pm)
	if !added {
		t.Fatal("first AddPartialMatchForCandidate should return true")
	}

	got := GetPartialMatchesForCandidate(queryRef, candidate)
	if len(got) != 1 || got[0] != pm {
		t.Fatalf("GetPartialMatchesForCandidate: got %v, want [%p]", got, pm)
	}
}

func TestMultipleMatchesSameCandidate(t *testing.T) {
	t.Parallel()

	candidate := stubMatchCandidate{name: "idx_b"}
	scanExpr := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryRef := expressions.InitialOf(scanExpr)
	candRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
	)

	mi1 := NewRegularMatchInfo(nil, nil, nil, nil, nil, nil, nil, nil)
	mi2 := NewRegularMatchInfo(nil, nil, nil, nil, nil, nil, nil, nil)
	pm1 := NewPartialMatch(EmptyAliasMap(), candidate, queryRef, scanExpr, candRef, mi1)
	pm2 := NewPartialMatch(EmptyAliasMap(), candidate, queryRef, scanExpr, candRef, mi2)

	AddPartialMatchForCandidate(queryRef, candidate, pm1)
	AddPartialMatchForCandidate(queryRef, candidate, pm2)

	got := GetPartialMatchesForCandidate(queryRef, candidate)
	if len(got) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(got))
	}
	if got[0] != pm1 || got[1] != pm2 {
		t.Fatal("matches not in expected order")
	}
}

func TestMultipleCandidatesSameReference(t *testing.T) {
	t.Parallel()

	candA := stubMatchCandidate{name: "idx_a"}
	candB := stubMatchCandidate{name: "idx_b"}
	scanExpr := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryRef := expressions.InitialOf(scanExpr)
	candRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
	)

	mi := NewRegularMatchInfo(nil, nil, nil, nil, nil, nil, nil, nil)
	pmA := NewPartialMatch(EmptyAliasMap(), candA, queryRef, scanExpr, candRef, mi)
	pmB := NewPartialMatch(EmptyAliasMap(), candB, queryRef, scanExpr, candRef, mi)

	AddPartialMatchForCandidate(queryRef, candA, pmA)
	AddPartialMatchForCandidate(queryRef, candB, pmB)

	gotA := GetPartialMatchesForCandidate(queryRef, candA)
	gotB := GetPartialMatchesForCandidate(queryRef, candB)
	if len(gotA) != 1 || gotA[0] != pmA {
		t.Fatal("candidate A mismatch")
	}
	if len(gotB) != 1 || gotB[0] != pmB {
		t.Fatal("candidate B mismatch")
	}
}

func TestDuplicateAddReturnsFalse(t *testing.T) {
	t.Parallel()

	pm, candidate, queryRef := makeRefTestPartialMatch(t, "idx_dup")

	first := AddPartialMatchForCandidate(queryRef, candidate, pm)
	second := AddPartialMatchForCandidate(queryRef, candidate, pm)
	if !first {
		t.Fatal("first add should return true")
	}
	if second {
		t.Fatal("duplicate add should return false")
	}

	got := GetPartialMatchesForCandidate(queryRef, candidate)
	if len(got) != 1 {
		t.Fatalf("after duplicate add, expected 1 match, got %d", len(got))
	}
}

func TestGetPartialMatchesForCandidate_EmptyRef(t *testing.T) {
	t.Parallel()

	ref := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
	)
	candidate := stubMatchCandidate{name: "idx_empty"}

	got := GetPartialMatchesForCandidate(ref, candidate)
	if got != nil {
		t.Fatalf("expected nil from empty ref, got %v", got)
	}
}

func TestGetPartialMatchesForExpression(t *testing.T) {
	t.Parallel()

	candA := stubMatchCandidate{name: "idx_a"}
	candB := stubMatchCandidate{name: "idx_b"}
	exprA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	exprB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	queryRef := expressions.InitialOf(exprA)
	queryRef.Insert(exprB)
	candRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
	)
	mi := NewRegularMatchInfo(nil, nil, nil, nil, nil, nil, nil, nil)

	pmA := NewPartialMatch(EmptyAliasMap(), candA, queryRef, exprA, candRef, mi)
	pmB := NewPartialMatch(EmptyAliasMap(), candB, queryRef, exprB, candRef, mi)

	AddPartialMatchForCandidate(queryRef, candA, pmA)
	AddPartialMatchForCandidate(queryRef, candB, pmB)

	gotA := GetPartialMatchesForExpression(queryRef, exprA)
	if len(gotA) != 1 || gotA[0] != pmA {
		t.Fatalf("GetPartialMatchesForExpression(exprA): got %v, want [%p]", gotA, pmA)
	}

	gotB := GetPartialMatchesForExpression(queryRef, exprB)
	if len(gotB) != 1 || gotB[0] != pmB {
		t.Fatalf("GetPartialMatchesForExpression(exprB): got %v, want [%p]", gotB, pmB)
	}

	// Expression not present in any match should return nil.
	exprC := expressions.NewFullUnorderedScanExpression([]string{"C"}, values.UnknownType)
	gotC := GetPartialMatchesForExpression(queryRef, exprC)
	if gotC != nil {
		t.Fatalf("GetPartialMatchesForExpression(exprC): expected nil, got %v", gotC)
	}
}

func TestGetPartialMatchCandidatesTyped(t *testing.T) {
	t.Parallel()

	candA := stubMatchCandidate{name: "idx_a"}
	candB := stubMatchCandidate{name: "idx_b"}
	scanExpr := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryRef := expressions.InitialOf(scanExpr)
	candRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
	)
	mi := NewRegularMatchInfo(nil, nil, nil, nil, nil, nil, nil, nil)

	pmA := NewPartialMatch(EmptyAliasMap(), candA, queryRef, scanExpr, candRef, mi)
	pmB := NewPartialMatch(EmptyAliasMap(), candB, queryRef, scanExpr, candRef, mi)

	AddPartialMatchForCandidate(queryRef, candA, pmA)
	AddPartialMatchForCandidate(queryRef, candB, pmB)

	candidates := GetPartialMatchCandidatesTyped(queryRef)
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	// Check both candidates are present (map iteration order is non-deterministic).
	names := make(map[string]bool)
	for _, c := range candidates {
		names[c.CandidateName()] = true
	}
	if !names["idx_a"] || !names["idx_b"] {
		t.Fatalf("missing expected candidate names, got %v", names)
	}
}

func TestGetPartialMatchCandidatesTyped_EmptyRef(t *testing.T) {
	t.Parallel()

	ref := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
	)
	got := GetPartialMatchCandidatesTyped(ref)
	if got != nil {
		t.Fatalf("expected nil from empty ref, got %v", got)
	}
}
