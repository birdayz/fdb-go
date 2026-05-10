package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// adjustableFilterExpression wraps a LogicalFilterExpression and
// implements ExpressionMatchAdjuster so that AdjustMatches can absorb
// it. In Java, MatchableSortExpression and SelectExpression override
// adjustMatch; for testing we create a simple adjustable expression.
type adjustableFilterExpression struct {
	*expressions.LogicalFilterExpression
}

func (a *adjustableFilterExpression) AdjustMatch(pm PartialMatch) MatchInfo {
	return NewAdjustedBuilder(pm.GetMatchInfo()).Build()
}

// TestAdjustMatches_ScanToFilter builds a candidate tree: filter(scan)
// and a query tree: just scan. Seeds a leaf match on the scan. Runs
// AdjustMatches. Verifies an adjusted PartialMatch exists pointing to
// the filter candidate ref.
func TestAdjustMatches_ScanToFilter(t *testing.T) {
	t.Parallel()

	// --- Query side: bare scan ---
	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryScanRef := expressions.InitialOf(queryScan)

	// --- Candidate side: adjustableFilter(scan) ---
	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachQuantifier(candidateScanRef)
	innerFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		candidateScanQ,
	)
	candidateFilter := &adjustableFilterExpression{LogicalFilterExpression: innerFilter}
	candidateFilterRef := expressions.InitialOf(candidateFilter)
	traversal := NewTraversal(candidateFilterRef)

	mc := &testMatchCandidate{name: "idx_adj", traversal: traversal}

	// Seed the leaf match: queryScanRef matched against candidateScanRef.
	seedMI := NewRegularMatchInfo(nil, EmptyAliasMap(), nil, nil, nil, EmptyGroupByMappings(), nil, nil)
	seedPM := NewPartialMatch(EmptyAliasMap(), mc, queryScanRef, queryScan, candidateScanRef, seedMI)
	AddPartialMatchForCandidate(queryScanRef, mc, seedPM)

	// Run AdjustMatches.
	AdjustMatches(queryScanRef)

	// Verify: a new adjusted PartialMatch should exist on queryScanRef
	// with candidateRef == candidateFilterRef.
	pms := GetPartialMatchesForCandidate(queryScanRef, mc)
	if len(pms) < 2 {
		t.Fatalf("expected at least 2 PartialMatches (original + adjusted), got %d", len(pms))
	}

	var found bool
	for _, pm := range pms {
		pmi, ok := pm.(*PartialMatchImpl)
		if !ok {
			continue
		}
		if pmi.GetCandidateRef() == candidateFilterRef {
			// Verify it's adjusted.
			if !pmi.GetMatchInfo().IsAdjusted() {
				t.Fatal("expected AdjustedMatchInfo on the adjusted partial match")
			}
			// Verify the query ref/expression are unchanged.
			if pmi.GetQueryRef() != queryScanRef {
				t.Fatal("adjusted PartialMatch queryRef should be queryScanRef")
			}
			if pmi.GetQueryExpression() != queryScan {
				t.Fatal("adjusted PartialMatch queryExpression should be queryScan")
			}
			// Verify the underlying match info is the original.
			ami, ok := pmi.GetMatchInfo().(*AdjustedMatchInfo)
			if !ok {
				t.Fatal("expected *AdjustedMatchInfo")
			}
			if ami.GetUnderlying() != seedMI {
				t.Fatal("AdjustedMatchInfo underlying should be the seed RegularMatchInfo")
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no adjusted PartialMatch found pointing to candidateFilterRef")
	}
}

// TestAdjustMatches_MultiLevel builds a candidate tree:
// sort(filter(scan)). Seeds a leaf match on scan, then an intermediate
// match on filter. Runs AdjustMatches. Verifies adjustments at both
// filter and sort levels.
func TestAdjustMatches_MultiLevel(t *testing.T) {
	t.Parallel()

	// --- Query side: filter(scan) ---
	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryScanRef := expressions.InitialOf(queryScan)
	queryScanQ := expressions.ForEachQuantifier(queryScanRef)
	queryFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		queryScanQ,
	)
	queryFilterRef := expressions.InitialOf(queryFilter)

	// --- Candidate side: adjustableSort(adjustableFilter(scan)) ---
	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachQuantifier(candidateScanRef)

	innerFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		candidateScanQ,
	)
	candidateFilter := &adjustableFilterExpression{LogicalFilterExpression: innerFilter}
	candidateFilterRef := expressions.InitialOf(candidateFilter)
	candidateFilterQ := expressions.ForEachQuantifier(candidateFilterRef)

	// Use LogicalSortExpression wrapped in adjustable for the sort level.
	innerSort := expressions.NewLogicalSortExpression(nil, candidateFilterQ)
	candidateSort := &adjustableSortExpression{LogicalSortExpression: innerSort}
	candidateSortRef := expressions.InitialOf(candidateSort)
	traversal := NewTraversal(candidateSortRef)

	mc := &testMatchCandidate{name: "idx_multi", traversal: traversal}

	// Seed leaf match: queryScanRef -> candidateScanRef.
	leafMI := NewRegularMatchInfo(nil, EmptyAliasMap(), nil, nil, nil, EmptyGroupByMappings(), nil, nil)
	leafPM := NewPartialMatch(EmptyAliasMap(), mc, queryScanRef, queryScan, candidateScanRef, leafMI)
	AddPartialMatchForCandidate(queryScanRef, mc, leafPM)

	// Seed intermediate match: queryFilterRef -> candidateFilterRef.
	interAM := NewAliasMapBuilder()
	interAM.Put(queryScanQ.GetAlias(), candidateScanQ.GetAlias())
	interMI := NewRegularMatchInfo(nil, interAM.Build(), nil, nil, nil, EmptyGroupByMappings(), nil, nil)
	interPM := NewPartialMatch(interAM.Build(), mc, queryFilterRef, queryFilter, candidateFilterRef, interMI)
	AddPartialMatchForCandidate(queryFilterRef, mc, interPM)

	// Run AdjustMatches from the query root (queryFilterRef contains
	// queryScanRef as a child via the filter's quantifier).
	AdjustMatches(queryFilterRef)

	// 1) queryScanRef should have an adjusted match at candidateFilterRef.
	scanPMs := GetPartialMatchesForCandidate(queryScanRef, mc)
	foundScanAdj := false
	for _, pm := range scanPMs {
		pmi := pm.(*PartialMatchImpl)
		if pmi.GetCandidateRef() == candidateFilterRef && pmi.GetMatchInfo().IsAdjusted() {
			foundScanAdj = true
			break
		}
	}
	if !foundScanAdj {
		t.Fatal("expected adjusted PartialMatch on queryScanRef -> candidateFilterRef")
	}

	// 2) queryFilterRef should have an adjusted match at candidateSortRef.
	filterPMs := GetPartialMatchesForCandidate(queryFilterRef, mc)
	foundFilterAdj := false
	for _, pm := range filterPMs {
		pmi := pm.(*PartialMatchImpl)
		if pmi.GetCandidateRef() == candidateSortRef && pmi.GetMatchInfo().IsAdjusted() {
			foundFilterAdj = true
			break
		}
	}
	if !foundFilterAdj {
		t.Fatal("expected adjusted PartialMatch on queryFilterRef -> candidateSortRef")
	}
}

// adjustableSortExpression wraps a LogicalSortExpression and
// implements ExpressionMatchAdjuster.
type adjustableSortExpression struct {
	*expressions.LogicalSortExpression
}

func (a *adjustableSortExpression) AdjustMatch(pm PartialMatch) MatchInfo {
	return NewAdjustedBuilder(pm.GetMatchInfo()).Build()
}

// TestAdjustMatches_MultipleQuantifiers verifies that candidate
// expressions with multiple quantifiers are NOT absorbed (only
// single-quantifier expressions are eligible for adjustment).
func TestAdjustMatches_MultipleQuantifiers(t *testing.T) {
	t.Parallel()

	// --- Query side: bare scan ---
	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryScanRef := expressions.InitialOf(queryScan)

	// --- Candidate side: Select(scanA, scanB) — two quantifiers ---
	candidateScanA := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateScanRefA := expressions.InitialOf(candidateScanA)
	candidateQA := expressions.ForEachQuantifier(candidateScanRefA)

	candidateScanB := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateScanRefB := expressions.InitialOf(candidateScanB)
	candidateQB := expressions.ForEachQuantifier(candidateScanRefB)

	candidateSelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{candidateQA, candidateQB},
		nil,
	)
	candidateSelectRef := expressions.InitialOf(candidateSelect)
	traversal := NewTraversal(candidateSelectRef)

	mc := &testMatchCandidate{name: "idx_multi_q", traversal: traversal}

	// Seed leaf match: queryScanRef -> candidateScanRefA.
	seedMI := NewRegularMatchInfo(nil, EmptyAliasMap(), nil, nil, nil, EmptyGroupByMappings(), nil, nil)
	seedPM := NewPartialMatch(EmptyAliasMap(), mc, queryScanRef, queryScan, candidateScanRefA, seedMI)
	AddPartialMatchForCandidate(queryScanRef, mc, seedPM)

	// Run AdjustMatches.
	AdjustMatches(queryScanRef)

	// Should NOT have an adjusted match at candidateSelectRef because
	// the select has 2 quantifiers.
	pms := GetPartialMatchesForCandidate(queryScanRef, mc)
	for _, pm := range pms {
		pmi := pm.(*PartialMatchImpl)
		if pmi.GetCandidateRef() == candidateSelectRef {
			t.Fatal("should not have adjusted PartialMatch for multi-quantifier candidate expression")
		}
	}
}

// TestAdjustMatches_NoExistingMatches verifies that AdjustMatches is a
// no-op when there are no existing PartialMatches.
func TestAdjustMatches_NoExistingMatches(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	// No partial matches seeded. Should not panic.
	AdjustMatches(scanRef)

	raw := scanRef.GetAllPartialMatches()
	if len(raw) != 0 {
		t.Fatalf("expected 0 partial matches, got %d", len(raw))
	}
}

// TestAdjustMatches_NonAdjustableExpression verifies that a candidate
// expression that does NOT implement ExpressionMatchAdjuster is not
// absorbed (the default returns nil, mirroring Java's
// Optional.empty()).
func TestAdjustMatches_NonAdjustableExpression(t *testing.T) {
	t.Parallel()

	// --- Query side: bare scan ---
	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryScanRef := expressions.InitialOf(queryScan)

	// --- Candidate side: plain filter(scan) — NOT adjustable ---
	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachQuantifier(candidateScanRef)
	candidateFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		candidateScanQ,
	)
	candidateFilterRef := expressions.InitialOf(candidateFilter)
	traversal := NewTraversal(candidateFilterRef)

	mc := &testMatchCandidate{name: "idx_noadj", traversal: traversal}

	// Seed leaf match.
	seedMI := NewRegularMatchInfo(nil, EmptyAliasMap(), nil, nil, nil, EmptyGroupByMappings(), nil, nil)
	seedPM := NewPartialMatch(EmptyAliasMap(), mc, queryScanRef, queryScan, candidateScanRef, seedMI)
	AddPartialMatchForCandidate(queryScanRef, mc, seedPM)

	// Run AdjustMatches.
	AdjustMatches(queryScanRef)

	// Should still have only 1 PartialMatch — the plain filter
	// doesn't implement ExpressionMatchAdjuster.
	pms := GetPartialMatchesForCandidate(queryScanRef, mc)
	if len(pms) != 1 {
		t.Fatalf("expected 1 PartialMatch (non-adjustable parent), got %d", len(pms))
	}
}

// TestAdjustMatches_CandidateRefMismatch verifies that when the
// candidate quantifier's child ref does not match the partial match's
// candidate ref, no adjustment occurs.
func TestAdjustMatches_CandidateRefMismatch(t *testing.T) {
	t.Parallel()

	// --- Query side: bare scan ---
	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryScanRef := expressions.InitialOf(queryScan)

	// --- Candidate side: filter(scan) ---
	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachQuantifier(candidateScanRef)
	innerFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		candidateScanQ,
	)
	candidateFilter := &adjustableFilterExpression{LogicalFilterExpression: innerFilter}
	candidateFilterRef := expressions.InitialOf(candidateFilter)
	traversal := NewTraversal(candidateFilterRef)

	mc := &testMatchCandidate{name: "idx_mismatch", traversal: traversal}

	// Seed a leaf match that points to a DIFFERENT candidate ref
	// (not candidateScanRef).
	otherScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	otherRef := expressions.InitialOf(otherScan)
	seedMI := NewRegularMatchInfo(nil, EmptyAliasMap(), nil, nil, nil, EmptyGroupByMappings(), nil, nil)
	seedPM := NewPartialMatch(EmptyAliasMap(), mc, queryScanRef, queryScan, otherRef, seedMI)
	AddPartialMatchForCandidate(queryScanRef, mc, seedPM)

	// Run AdjustMatches.
	AdjustMatches(queryScanRef)

	// The filter's quantifier ranges over candidateScanRef, but the
	// partial match's candidateRef is otherRef, so no adjustment.
	pms := GetPartialMatchesForCandidate(queryScanRef, mc)
	if len(pms) != 1 {
		t.Fatalf("expected 1 PartialMatch (ref mismatch prevents adjustment), got %d", len(pms))
	}
}
