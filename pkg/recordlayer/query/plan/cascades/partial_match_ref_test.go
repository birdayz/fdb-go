package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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

func refTestEqualityRange(t *testing.T, literal int64) *predicates.ComparisonRange {
	t.Helper()
	comparison := predicates.NewLiteralComparison(predicates.ComparisonEquals, literal)
	merged := predicates.EmptyComparisonRange().Merge(&comparison)
	if !merged.Ok {
		t.Fatalf("failed to build equality range for %d", literal)
	}
	return merged.Range
}

type refTestSemanticFixture struct {
	candidate    MatchCandidate
	queryExpr    expressions.RelationalExpression
	queryRef     *expressions.Reference
	candidateRef *expressions.Reference
}

func newRefTestSemanticFixture(candidateName string) refTestSemanticFixture {
	queryExpr := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	return refTestSemanticFixture{
		candidate: stubMatchCandidate{name: candidateName},
		queryExpr: queryExpr,
		queryRef:  expressions.InitialOf(queryExpr),
		candidateRef: expressions.InitialOf(
			expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
		),
	}
}

func (f refTestSemanticFixture) partialMatch(
	boundAliasMap *AliasMap,
	parameterBindingMap map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) *PartialMatchImpl {
	matchInfo := NewRegularMatchInfo(
		parameterBindingMap,
		boundAliasMap,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	return NewPartialMatch(
		boundAliasMap,
		f.candidate,
		f.queryRef,
		f.queryExpr,
		f.candidateRef,
		matchInfo,
	)
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

// TestAddPartialMatchForCandidate_DistinctSemanticMatchesSurvive pins the
// RFC-190.4b requirement that a Reference retain alternative partial matches
// for the same query expression and candidate reference. Java's Example-2
// matching can produce exactly this shape: the graph endpoints are identical,
// while the selected alias mapping or merged child metadata differs.
//
// Pair-only (queryExpression, candidateRef) dedup loses these alternatives.
func TestAddPartialMatchForCandidate_DistinctSemanticMatchesSurvive(t *testing.T) {
	t.Parallel()

	t.Run("bound alias map", func(t *testing.T) {
		fixture := newRefTestSemanticFixture("idx_alias_variants")
		queryAlias := values.NamedCorrelationIdentifier("query_leg")
		firstAliasMap := AliasMapOfAliases(
			queryAlias,
			values.NamedCorrelationIdentifier("candidate_leg_a"),
		)
		secondAliasMap := AliasMapOfAliases(
			queryAlias,
			values.NamedCorrelationIdentifier("candidate_leg_b"),
		)
		first := fixture.partialMatch(firstAliasMap, nil)
		second := fixture.partialMatch(secondAliasMap, nil)

		if !AddPartialMatchForCandidate(fixture.queryRef, fixture.candidate, first) {
			t.Fatal("first semantic match should be added")
		}
		if !AddPartialMatchForCandidate(fixture.queryRef, fixture.candidate, second) {
			t.Fatal("distinct bound-alias match should survive semantic dedup")
		}

		got := GetPartialMatchesForCandidate(fixture.queryRef, fixture.candidate)
		if len(got) != 2 {
			t.Fatalf("distinct bound-alias matches: got %d, want 2", len(got))
		}
		if got[0] != first || got[1] != second {
			t.Fatal("distinct bound-alias matches not retained in insertion order")
		}
	})

	t.Run("parameter binding metadata", func(t *testing.T) {
		fixture := newRefTestSemanticFixture("idx_binding_variants")
		queryAlias := values.NamedCorrelationIdentifier("query_leg")
		candidateAlias := values.NamedCorrelationIdentifier("candidate_leg")
		boundAliasMap := AliasMapOfAliases(queryAlias, candidateAlias)
		parameterAlias := values.NamedCorrelationIdentifier("index_parameter")
		newMatch := func(literal int64) *PartialMatchImpl {
			return fixture.partialMatch(
				boundAliasMap,
				map[values.CorrelationIdentifier]*predicates.ComparisonRange{
					parameterAlias: refTestEqualityRange(t, literal),
				},
			)
		}

		seven := newMatch(7)
		nine := newMatch(9)
		if !AddPartialMatchForCandidate(fixture.queryRef, fixture.candidate, seven) {
			t.Fatal("first semantic match should be added")
		}
		if !AddPartialMatchForCandidate(fixture.queryRef, fixture.candidate, nine) {
			t.Fatal("distinct parameter-binding match should survive semantic dedup")
		}

		got := GetPartialMatchesForCandidate(fixture.queryRef, fixture.candidate)
		if len(got) != 2 {
			t.Fatalf("distinct parameter-binding matches: got %d, want 2", len(got))
		}
		if got[0] != seven || got[1] != nine {
			t.Fatal("distinct parameter-binding matches not retained in insertion order")
		}
	})
}

// TestAddPartialMatchForCandidate_ExactSemanticReAddCollapses proves that
// semantic dedup is stronger than pointer identity. The duplicate reconstructs
// every map, range, MatchInfo, and PartialMatch, but carries exactly the same
// downstream matching semantics as the first value.
func TestAddPartialMatchForCandidate_ExactSemanticReAddCollapses(t *testing.T) {
	t.Parallel()

	fixture := newRefTestSemanticFixture("idx_semantic_duplicate")
	queryAlias := values.NamedCorrelationIdentifier("query_leg")
	candidateAlias := values.NamedCorrelationIdentifier("candidate_leg")
	parameterAlias := values.NamedCorrelationIdentifier("index_parameter")

	newMatch := func() *PartialMatchImpl {
		boundAliasMap := AliasMapOfAliases(queryAlias, candidateAlias)
		return fixture.partialMatch(
			boundAliasMap,
			map[values.CorrelationIdentifier]*predicates.ComparisonRange{
				parameterAlias: refTestEqualityRange(t, 7),
			},
		)
	}

	first := newMatch()
	exactReAdd := newMatch()
	if !AddPartialMatchForCandidate(fixture.queryRef, fixture.candidate, first) {
		t.Fatal("first semantic match should be added")
	}
	if AddPartialMatchForCandidate(fixture.queryRef, fixture.candidate, exactReAdd) {
		t.Fatal("exact semantic re-add should collapse despite fresh allocations")
	}

	got := GetPartialMatchesForCandidate(fixture.queryRef, fixture.candidate)
	if len(got) != 1 {
		t.Fatalf("exact semantic re-add: got %d matches, want 1", len(got))
	}
	if got[0] != first {
		t.Fatal("semantic re-add replaced the first stored match")
	}
}

func TestAddPartialMatchForCandidate_PrimaryKeyDistinctObligationIsSemanticIdentity(
	t *testing.T,
) {
	t.Parallel()

	fixture := newRefTestSemanticFixture("idx_pk_distinct_identity")
	newMatch := func(requiresPrimaryKeyDistinct bool) *PartialMatchImpl {
		boundAliasMap := EmptyAliasMap()
		var matchInfo *RegularMatchInfo
		if requiresPrimaryKeyDistinct {
			matchInfo = newRegularMatchInfo(
				nil,
				boundAliasMap,
				nil,
				nil,
				nil,
				nil,
				nil,
				nil,
				true,
			)
		} else {
			matchInfo = NewRegularMatchInfo(
				nil,
				boundAliasMap,
				nil,
				nil,
				nil,
				nil,
				nil,
				nil,
			)
		}
		return NewPartialMatch(
			boundAliasMap,
			fixture.candidate,
			fixture.queryRef,
			fixture.queryExpr,
			fixture.candidateRef,
			matchInfo,
		)
	}

	requiresDistinct := newMatch(true)
	exactReconstruction := newMatch(true)
	noDistinct := newMatch(false)
	if !AddPartialMatchForCandidate(
		fixture.queryRef,
		fixture.candidate,
		requiresDistinct,
	) {
		t.Fatal("first required-distinct match should be retained")
	}
	if AddPartialMatchForCandidate(
		fixture.queryRef,
		fixture.candidate,
		exactReconstruction,
	) {
		t.Fatal("identical required-distinct matches should deduplicate")
	}
	if !AddPartialMatchForCandidate(
		fixture.queryRef,
		fixture.candidate,
		noDistinct,
	) {
		t.Fatal("false-vs-true distinct obligations must coexist")
	}

	got := GetPartialMatchesForCandidate(fixture.queryRef, fixture.candidate)
	if len(got) != 2 || got[0] != requiresDistinct || got[1] != noDistinct {
		t.Fatalf(
			"stored obligation alternatives = %v, want [required, not-required]",
			got,
		)
	}
}

func TestMultipleMatchesSameCandidate(t *testing.T) {
	t.Parallel()

	candidate := stubMatchCandidate{name: "idx_b"}
	scanExpr := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryRef := expressions.InitialOf(scanExpr)
	// Two DISTINCT candidate refs → two genuinely distinct matches for the
	// same candidate, both retained. Semantic dedup collapses repeated
	// construction of the same logical match without conflating endpoints.
	candRef1 := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
	)
	candRef2 := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
	)

	mi1 := NewRegularMatchInfo(nil, nil, nil, nil, nil, nil, nil, nil)
	mi2 := NewRegularMatchInfo(nil, nil, nil, nil, nil, nil, nil, nil)
	pm1 := NewPartialMatch(EmptyAliasMap(), candidate, queryRef, scanExpr, candRef1, mi1)
	pm2 := NewPartialMatch(EmptyAliasMap(), candidate, queryRef, scanExpr, candRef2, mi2)

	AddPartialMatchForCandidate(queryRef, candidate, pm1)
	AddPartialMatchForCandidate(queryRef, candidate, pm2)

	got := GetPartialMatchesForCandidate(queryRef, candidate)
	if len(got) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(got))
	}
	if got[0] != pm1 || got[1] != pm2 {
		t.Fatal("matches not in expected order")
	}

	// An exact semantic re-add of pm1 is dropped — this is the accumulation
	// guard.
	dupMi := NewRegularMatchInfo(nil, nil, nil, nil, nil, nil, nil, nil)
	dup := NewPartialMatch(EmptyAliasMap(), candidate, queryRef, scanExpr, candRef1, dupMi)
	if AddPartialMatchForCandidate(queryRef, candidate, dup) {
		t.Fatal("content-equivalent match (same queryExpr+candidateRef) should be deduped")
	}
	if n := len(GetPartialMatchesForCandidate(queryRef, candidate)); n != 2 {
		t.Fatalf("after dup re-add: expected 2 matches, got %d", n)
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
