package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestMatchIntermediateRule_FilterOverScan builds a two-level tree:
// Filter(scan) on both the query and candidate side. Seeds a
// PartialMatch on the scan Reference via MatchLeafRule, then runs
// MatchIntermediateRule on the filter — verifies it creates a
// PartialMatch on the filter Reference.
func TestMatchIntermediateRule_FilterOverScan(t *testing.T) {
	t.Parallel()

	// --- Query side: Filter(scan) ---
	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryScanRef := expressions.InitialOf(queryScan)
	queryScanQ := expressions.ForEachQuantifier(queryScanRef)
	queryFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		queryScanQ,
	)
	queryFilterRef := expressions.InitialOf(queryFilter)

	// --- Candidate side: Filter(scan) with same structure ---
	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachQuantifier(candidateScanRef)
	candidateFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		candidateScanQ,
	)
	candidateFilterRef := expressions.InitialOf(candidateFilter)
	traversal := NewTraversal(candidateFilterRef)

	mc := &testMatchCandidate{name: "idx_t", traversal: traversal}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	// Step 1: Seed the leaf match on queryScanRef.
	leafRule := NewMatchLeafRule()
	FireExpressionRuleWithMemo(leafRule, queryScanRef, ctx, nil)

	// Verify the leaf match was seeded.
	leafPMs := GetPartialMatchesForCandidate(queryScanRef, mc)
	if len(leafPMs) == 0 {
		t.Fatal("leaf PartialMatch not seeded on queryScanRef")
	}

	// Step 2: Run MatchIntermediateRule on the filter.
	intermediateRule := NewMatchIntermediateRule()
	FireExpressionRuleWithMemo(intermediateRule, queryFilterRef, ctx, nil)

	// Verify: a PartialMatch should be stored on queryFilterRef.
	filterPMs := GetPartialMatchesForCandidate(queryFilterRef, mc)
	if len(filterPMs) == 0 {
		t.Fatal("expected at least one PartialMatch on queryFilterRef, got 0")
	}

	pm := filterPMs[0]
	if pm.GetMatchCandidate() != mc {
		t.Fatalf("PartialMatch candidate = %v, want %v", pm.GetMatchCandidate(), mc)
	}

	pmi, ok := pm.(*PartialMatchImpl)
	if !ok {
		t.Fatalf("expected *PartialMatchImpl, got %T", pm)
	}

	// Verify candidate ref points to the candidate filter ref.
	if pmi.GetCandidateRef() != candidateFilterRef {
		t.Fatal("PartialMatch candidateRef does not match candidateFilterRef")
	}

	// Verify query ref points to the query filter ref.
	if pmi.GetQueryRef() != queryFilterRef {
		t.Fatal("PartialMatch queryRef does not match queryFilterRef")
	}

	// Verify the match info is regular.
	mi := pm.GetMatchInfo()
	if mi == nil || !mi.IsRegular() {
		t.Fatal("expected regular MatchInfo")
	}
}

// TestMatchIntermediateRule_MismatchedType verifies that a filter on
// the query side does not match a union on the candidate side (different
// expression types at the intermediate level).
func TestMatchIntermediateRule_MismatchedType(t *testing.T) {
	t.Parallel()

	// Query side: Filter(scan)
	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryScanRef := expressions.InitialOf(queryScan)
	queryScanQ := expressions.ForEachQuantifier(queryScanRef)
	queryFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		queryScanQ,
	)
	queryFilterRef := expressions.InitialOf(queryFilter)

	// Candidate side: Union(scan) — different intermediate type.
	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachQuantifier(candidateScanRef)
	candidateUnion := expressions.NewLogicalUnionExpression(
		[]expressions.Quantifier{candidateScanQ},
	)
	candidateUnionRef := expressions.InitialOf(candidateUnion)
	traversal := NewTraversal(candidateUnionRef)

	mc := &testMatchCandidate{name: "idx_t", traversal: traversal}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	// Seed leaf match.
	leafRule := NewMatchLeafRule()
	FireExpressionRuleWithMemo(leafRule, queryScanRef, ctx, nil)

	// Run intermediate rule.
	intermediateRule := NewMatchIntermediateRule()
	FireExpressionRuleWithMemo(intermediateRule, queryFilterRef, ctx, nil)

	// Should NOT match: Filter != Union at the intermediate level.
	filterPMs := GetPartialMatchesForCandidate(queryFilterRef, mc)
	if len(filterPMs) != 0 {
		t.Fatalf("expected 0 PartialMatches for mismatched intermediate types, got %d", len(filterPMs))
	}
}

// TestMatchIntermediateRule_NoChildMatches verifies that without child
// PartialMatches, the intermediate rule produces nothing.
func TestMatchIntermediateRule_NoChildMatches(t *testing.T) {
	t.Parallel()

	// Query side: Filter(scan)
	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryScanRef := expressions.InitialOf(queryScan)
	queryScanQ := expressions.ForEachQuantifier(queryScanRef)
	queryFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		queryScanQ,
	)
	queryFilterRef := expressions.InitialOf(queryFilter)

	// Candidate side: matching structure but NO leaf match seeded.
	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachQuantifier(candidateScanRef)
	candidateFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		candidateScanQ,
	)
	candidateFilterRef := expressions.InitialOf(candidateFilter)
	traversal := NewTraversal(candidateFilterRef)

	mc := &testMatchCandidate{name: "idx_t", traversal: traversal}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	// Do NOT seed the leaf match — skip MatchLeafRule entirely.

	// Run intermediate rule directly.
	intermediateRule := NewMatchIntermediateRule()
	FireExpressionRuleWithMemo(intermediateRule, queryFilterRef, ctx, nil)

	// Without child matches, the intermediate rule should produce nothing.
	filterPMs := GetPartialMatchesForCandidate(queryFilterRef, mc)
	if len(filterPMs) != 0 {
		t.Fatalf("expected 0 PartialMatches without child matches, got %d", len(filterPMs))
	}
}

// TestMatchIntermediateRule_MultipleQuantifiers builds a join-like
// expression (SelectExpression with two quantifiers), seeds matches on
// both children, and verifies the intermediate rule creates a match at
// the join level.
func TestMatchIntermediateRule_MultipleQuantifiers(t *testing.T) {
	t.Parallel()

	// --- Query side: Select(scanA, scanB) ---
	queryScanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	queryScanRefA := expressions.InitialOf(queryScanA)
	queryQA := expressions.ForEachQuantifier(queryScanRefA)

	queryScanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	queryScanRefB := expressions.InitialOf(queryScanB)
	queryQB := expressions.ForEachQuantifier(queryScanRefB)

	querySelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{queryQA, queryQB},
		nil, // no predicates
	)
	querySelectRef := expressions.InitialOf(querySelect)

	// --- Candidate side: Select(scanA, scanB) with same structure ---
	candidateScanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	candidateScanRefA := expressions.InitialOf(candidateScanA)
	candidateQA := expressions.ForEachQuantifier(candidateScanRefA)

	candidateScanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	candidateScanRefB := expressions.InitialOf(candidateScanB)
	candidateQB := expressions.ForEachQuantifier(candidateScanRefB)

	candidateSelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{candidateQA, candidateQB},
		nil,
	)
	candidateSelectRef := expressions.InitialOf(candidateSelect)
	traversal := NewTraversal(candidateSelectRef)

	mc := &testMatchCandidate{name: "idx_join", traversal: traversal}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	// Seed leaf matches on both child references.
	leafRule := NewMatchLeafRule()
	FireExpressionRuleWithMemo(leafRule, queryScanRefA, ctx, nil)
	FireExpressionRuleWithMemo(leafRule, queryScanRefB, ctx, nil)

	// Verify both leaf matches are seeded.
	if len(GetPartialMatchesForCandidate(queryScanRefA, mc)) == 0 {
		t.Fatal("leaf PartialMatch not seeded on queryScanRefA")
	}
	if len(GetPartialMatchesForCandidate(queryScanRefB, mc)) == 0 {
		t.Fatal("leaf PartialMatch not seeded on queryScanRefB")
	}

	// Run intermediate rule on the select.
	intermediateRule := NewMatchIntermediateRule()
	FireExpressionRuleWithMemo(intermediateRule, querySelectRef, ctx, nil)

	// Verify: a PartialMatch should be stored on querySelectRef.
	selectPMs := GetPartialMatchesForCandidate(querySelectRef, mc)
	if len(selectPMs) == 0 {
		t.Fatal("expected at least one PartialMatch on querySelectRef, got 0")
	}

	pmi, ok := selectPMs[0].(*PartialMatchImpl)
	if !ok {
		t.Fatalf("expected *PartialMatchImpl, got %T", selectPMs[0])
	}

	// Verify candidate ref is the candidate's select ref.
	if pmi.GetCandidateRef() != candidateSelectRef {
		t.Fatal("PartialMatch candidateRef does not match candidateSelectRef")
	}

	// Verify the alias map has entries for the quantifier aliases.
	am := pmi.GetBoundAliasMap()
	if am == nil || am.IsEmpty() {
		t.Fatal("expected non-empty alias map for intermediate match with quantifiers")
	}
}

// TestMatchIntermediateRule_LeafSkipped verifies that the intermediate
// rule does not fire on leaf expressions (no quantifiers).
func TestMatchIntermediateRule_LeafSkipped(t *testing.T) {
	t.Parallel()

	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryRef := expressions.InitialOf(queryScan)

	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateRef := expressions.InitialOf(candidateScan)
	traversal := NewTraversal(candidateRef)

	mc := &testMatchCandidate{name: "idx_t", traversal: traversal}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	// Only run intermediate rule (not leaf rule).
	intermediateRule := NewMatchIntermediateRule()
	FireExpressionRuleWithMemo(intermediateRule, queryRef, ctx, nil)

	// Should not match — leaf expression, no quantifiers.
	pms := GetPartialMatchesForCandidate(queryRef, mc)
	if len(pms) != 0 {
		t.Fatalf("MatchIntermediateRule should skip leaf expressions; got %d partial matches", len(pms))
	}
}

// TestMatchIntermediateRule_PartialMatchFields verifies the detailed
// fields of the PartialMatch created by the intermediate rule.
func TestMatchIntermediateRule_PartialMatchFields(t *testing.T) {
	t.Parallel()

	// Same setup as FilterOverScan.
	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryScanRef := expressions.InitialOf(queryScan)
	queryScanQ := expressions.ForEachQuantifier(queryScanRef)
	queryFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		queryScanQ,
	)
	queryFilterRef := expressions.InitialOf(queryFilter)

	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachQuantifier(candidateScanRef)
	candidateFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		candidateScanQ,
	)
	candidateFilterRef := expressions.InitialOf(candidateFilter)
	traversal := NewTraversal(candidateFilterRef)

	mc := &testMatchCandidate{name: "primary", traversal: traversal}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	// Seed leaf match.
	leafRule := NewMatchLeafRule()
	FireExpressionRuleWithMemo(leafRule, queryScanRef, ctx, nil)

	// Run intermediate rule.
	intermediateRule := NewMatchIntermediateRule()
	FireExpressionRuleWithMemo(intermediateRule, queryFilterRef, ctx, nil)

	pms := GetPartialMatchesForCandidate(queryFilterRef, mc)
	if len(pms) != 1 {
		t.Fatalf("expected 1 PartialMatch, got %d", len(pms))
	}

	pmi := pms[0].(*PartialMatchImpl)

	// Query fields.
	if pmi.GetQueryRef() != queryFilterRef {
		t.Fatal("queryRef mismatch")
	}
	if pmi.GetQueryExpression() != queryFilter {
		t.Fatal("queryExpression mismatch")
	}

	// Candidate fields.
	if pmi.GetCandidateRef() != candidateFilterRef {
		t.Fatal("candidateRef mismatch")
	}
	if pmi.GetMatchCandidate() != mc {
		t.Fatal("matchCandidate mismatch")
	}

	// Alias map should contain the quantifier alias mapping.
	am := pmi.GetBoundAliasMap()
	if am == nil {
		t.Fatal("expected non-nil alias map")
	}

	// The alias map should contain a mapping from queryScanQ's alias
	// to candidateScanQ's alias (from the leaf match) and from
	// queryFilter's inner quantifier alias to candidateFilter's inner
	// quantifier alias (from the intermediate match).
	queryAlias := queryScanQ.GetAlias()
	candidateAlias := candidateScanQ.GetAlias()
	target := am.GetTarget(queryAlias)
	if target != candidateAlias {
		t.Fatalf("alias map: query alias %v -> %v, want %v", queryAlias, target, candidateAlias)
	}

	// RegularMatchInfo.
	rmi := pmi.GetRegularMatchInfo()
	if rmi == nil {
		t.Fatal("expected non-nil RegularMatchInfo")
	}
}
