package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

type matchIntermediateDeterminismParent struct {
	label string
	ref   *expressions.Reference
	expr  expressions.RelationalExpression
}

type matchIntermediateDeterminismCandidate struct {
	label     string
	candidate *testMatchCandidate
	leafRef   *expressions.Reference
	parents   []matchIntermediateDeterminismParent
}

func newMatchIntermediateDeterminismCandidate(
	candidateLabel string,
) matchIntermediateDeterminismCandidate {
	leaf := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	leafRef := expressions.InitialOf(leaf)

	newParent := func(label string) matchIntermediateDeterminismParent {
		parent := expressions.NewLogicalFilterExpression(
			[]predicates.QueryPredicate{
				predicates.NewConstantPredicate(predicates.TriTrue),
			},
			expressions.ForEachQuantifier(leafRef),
		)
		return matchIntermediateDeterminismParent{
			label: label,
			ref:   expressions.InitialOf(parent),
			expr:  parent,
		}
	}

	// Create A before Z, then deliberately put Z before A in the root's
	// quantifier list. Traversal parent discovery must follow this DFS insertion
	// order, not reference allocation or a map's iteration order.
	parentA := newParent("parent_a")
	parentZ := newParent("parent_z")
	parentsInTraversalOrder := []matchIntermediateDeterminismParent{
		parentZ,
		parentA,
	}
	root := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(parentZ.ref),
			expressions.ForEachQuantifier(parentA.ref),
		},
		nil,
	)
	rootRef := expressions.InitialOf(root)
	candidate := &testMatchCandidate{
		name:      candidateLabel,
		traversal: NewTraversal(rootRef),
	}

	return matchIntermediateDeterminismCandidate{
		label:     candidateLabel,
		candidate: candidate,
		leafRef:   leafRef,
		parents:   parentsInTraversalOrder,
	}
}

func seedMatchIntermediateDeterminismChild(
	t *testing.T,
	queryLeafRef *expressions.Reference,
	queryLeaf expressions.RelationalExpression,
	candidate matchIntermediateDeterminismCandidate,
) {
	t.Helper()
	matchInfo := NewRegularMatchInfo(
		nil,
		EmptyAliasMap(),
		nil,
		nil,
		nil,
		EmptyGroupByMappings(),
		nil,
		nil,
	)
	child := NewPartialMatch(
		EmptyAliasMap(),
		candidate.candidate,
		queryLeafRef,
		queryLeaf,
		candidate.leafRef,
		matchInfo,
	)
	if !AddPartialMatchForCandidate(queryLeafRef, candidate.candidate, child) {
		t.Fatalf("failed to seed child match for %s", candidate.label)
	}
}

// TestMatchIntermediate_DeterministicCandidateAndParentDiscoveryOrder pins the
// two insertion-ordered surfaces consumed by MatchIntermediateRule.OnMatch:
//
//   - candidates are discovered from a child Reference in first-PartialMatch
//     insertion order; and
//   - findReferencingExpressionsForCandidate returns candidate parents in
//     Traversal discovery order.
//
// Each iteration rebuilds every Reference, expression, alias, candidate, and
// Traversal. Candidate Z is seeded before candidate A even though the context
// lists A first, while each candidate creates parent A before parent Z but
// traverses Z first. The resulting parent PartialMatches must therefore always
// be Z/Z, Z/A, A/Z, A/A rather than an order induced by either Go map.
func TestMatchIntermediate_DeterministicCandidateAndParentDiscoveryOrder(t *testing.T) {
	t.Parallel()

	const repetitions = 100

	for repetition := 0; repetition < repetitions; repetition++ {
		queryLeaf := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		queryLeafRef := expressions.InitialOf(queryLeaf)
		queryQ := expressions.ForEachQuantifier(queryLeafRef)
		queryParent := expressions.NewLogicalFilterExpression(
			[]predicates.QueryPredicate{
				predicates.NewConstantPredicate(predicates.TriTrue),
			},
			queryQ,
		)
		queryParentRef := expressions.InitialOf(queryParent)

		candidateA := newMatchIntermediateDeterminismCandidate("candidate_a")
		candidateZ := newMatchIntermediateDeterminismCandidate("candidate_z")

		// Seed in non-lexical order and opposite to PlanContext order. OnMatch's
		// candidate order comes from the child Reference, not either of those
		// unrelated orderings.
		seedMatchIntermediateDeterminismChild(t, queryLeafRef, queryLeaf, candidateZ)
		seedMatchIntermediateDeterminismChild(t, queryLeafRef, queryLeaf, candidateA)
		context := testPlanContextForMatching{
			candidates: []MatchCandidate{
				candidateA.candidate,
				candidateZ.candidate,
			},
		}

		for _, candidate := range []matchIntermediateDeterminismCandidate{
			candidateZ,
			candidateA,
		} {
			found := findReferencingExpressionsForCandidate(
				[]*expressions.Reference{queryLeafRef},
				candidate.candidate,
				candidate.candidate.GetTraversal(),
			)
			if len(found) != len(candidate.parents) {
				t.Fatalf(
					"repetition %d: %s parent discoveries = %d, want %d",
					repetition,
					candidate.label,
					len(found),
					len(candidate.parents),
				)
			}
			for i, want := range candidate.parents {
				if found[i].ref != want.ref || found[i].expr != want.expr {
					t.Fatalf(
						"repetition %d: %s parent discovery %d = (%p, %p), want %s (%p, %p)",
						repetition,
						candidate.label,
						i,
						found[i].ref,
						found[i].expr,
						want.label,
						want.ref,
						want.expr,
					)
				}
			}
		}

		FireExpressionRuleWithMemo(
			NewMatchIntermediateRule(),
			queryParentRef,
			context,
			nil,
		)

		got := GetPartialMatchesForExpression(queryParentRef, queryParent)
		wantCandidateOrder := []matchIntermediateDeterminismCandidate{
			candidateZ,
			candidateA,
		}
		wantCount := len(wantCandidateOrder) * len(candidateZ.parents)
		if len(got) != wantCount {
			t.Fatalf(
				"repetition %d: discovered parent PartialMatches = %d, want %d",
				repetition,
				len(got),
				wantCount,
			)
		}

		matchIndex := 0
		for _, wantCandidate := range wantCandidateOrder {
			for _, wantParent := range wantCandidate.parents {
				partialMatch := got[matchIndex].(*PartialMatchImpl)
				if partialMatch.GetMatchCandidate() != wantCandidate.candidate ||
					partialMatch.GetCandidateRef() != wantParent.ref {
					t.Fatalf(
						"repetition %d: parent PartialMatch %d = (%s, %p), want (%s, %s, %p)",
						repetition,
						matchIndex,
						partialMatch.GetMatchCandidate().CandidateName(),
						partialMatch.GetCandidateRef(),
						wantCandidate.label,
						wantParent.label,
						wantParent.ref,
					)
				}
				matchIndex++
			}
		}
	}
}
