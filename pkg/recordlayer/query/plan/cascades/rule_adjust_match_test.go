package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustAdjustMatchConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct adjust-match fixture: " + err.Error())
	}
	return value
}

func adjustMatchRowType() *values.RecordType {
	return values.NewRecordType("AdjustMatchRow", false, []values.Field{{
		Name:      "ID",
		FieldType: values.NotNullLong,
	}})
}

func adjustMatchScan() *expressions.FullUnorderedScanExpression {
	return mustAdjustMatchConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, adjustMatchRowType()))
}

func adjustMatchFilter(inner expressions.Quantifier) *expressions.LogicalFilterExpression {
	return mustAdjustMatchConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		inner,
	))
}

func adjustMatchLong(value int64) values.Value {
	return &values.ConstantValue{Value: value, Typ: values.NotNullLong}
}

type adjustMatchCandidate struct {
	name      string
	traversal *Traversal
}

func (c adjustMatchCandidate) CandidateName() string  { return c.name }
func (adjustMatchCandidate) GetColumnNames() []string { return nil }
func (adjustMatchCandidate) GetSargableAliases() []values.CorrelationIdentifier {
	return nil
}
func (adjustMatchCandidate) GetRecordTypes() []string { return nil }
func (adjustMatchCandidate) IsUnique() bool           { return false }
func (c adjustMatchCandidate) GetTraversal() *Traversal {
	return c.traversal
}

func (adjustMatchCandidate) ComputeBoundParameterPrefixMap(
	bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	prefix := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
	for alias, binding := range bindings {
		if binding != nil && !binding.IsEmpty() {
			prefix[alias] = binding
		}
	}
	return prefix
}

func (adjustMatchCandidate) ToScanPlan(
	map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	bool,
) plans.RecordQueryPlan {
	return nil
}

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
	queryScan := adjustMatchScan()
	queryScanRef := expressions.InitialOf(queryScan)

	// --- Candidate side: adjustableFilter(scan) ---
	candidateScan := adjustMatchScan()
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachQuantifier(candidateScanRef)
	innerFilter := adjustMatchFilter(candidateScanQ)
	candidateFilter := &adjustableFilterExpression{LogicalFilterExpression: innerFilter}
	candidateFilterRef := expressions.InitialOf(candidateFilter)
	traversal := NewTraversal(candidateFilterRef)

	mc := &adjustMatchCandidate{name: "idx_adj", traversal: traversal}

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
	queryScan := adjustMatchScan()
	queryScanRef := expressions.InitialOf(queryScan)
	queryScanQ := expressions.ForEachQuantifier(queryScanRef)
	queryFilter := adjustMatchFilter(queryScanQ)
	queryFilterRef := expressions.InitialOf(queryFilter)

	// --- Candidate side: adjustableSort(adjustableFilter(scan)) ---
	candidateScan := adjustMatchScan()
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachQuantifier(candidateScanRef)

	innerFilter := adjustMatchFilter(candidateScanQ)
	candidateFilter := &adjustableFilterExpression{LogicalFilterExpression: innerFilter}
	candidateFilterRef := expressions.InitialOf(candidateFilter)
	candidateFilterQ := expressions.ForEachQuantifier(candidateFilterRef)

	// Use LogicalSortExpression wrapped in adjustable for the sort level.
	innerSort := mustAdjustMatchConstruct(expressions.NewLogicalSortExpression(nil, candidateFilterQ))
	candidateSort := &adjustableSortExpression{LogicalSortExpression: innerSort}
	candidateSortRef := expressions.InitialOf(candidateSort)
	traversal := NewTraversal(candidateSortRef)

	mc := &adjustMatchCandidate{name: "idx_multi", traversal: traversal}

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
	queryScan := adjustMatchScan()
	queryScanRef := expressions.InitialOf(queryScan)

	// --- Candidate side: Select(scanA, scanB) — two quantifiers ---
	candidateScanA := adjustMatchScan()
	candidateScanRefA := expressions.InitialOf(candidateScanA)
	candidateQA := expressions.ForEachQuantifier(candidateScanRefA)

	candidateScanB := adjustMatchScan()
	candidateScanRefB := expressions.InitialOf(candidateScanB)
	candidateQB := expressions.ForEachQuantifier(candidateScanRefB)

	candidateSelect := mustAdjustMatchConstruct(expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{candidateQA, candidateQB},
		nil,
	))
	candidateSelectRef := expressions.InitialOf(candidateSelect)
	traversal := NewTraversal(candidateSelectRef)

	mc := &adjustMatchCandidate{name: "idx_multi_q", traversal: traversal}

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

	scan := adjustMatchScan()
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
	queryScan := adjustMatchScan()
	queryScanRef := expressions.InitialOf(queryScan)

	// --- Candidate side: plain filter(scan) — NOT adjustable ---
	candidateScan := adjustMatchScan()
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachQuantifier(candidateScanRef)
	candidateFilter := adjustMatchFilter(candidateScanQ)
	candidateFilterRef := expressions.InitialOf(candidateFilter)
	traversal := NewTraversal(candidateFilterRef)

	mc := &adjustMatchCandidate{name: "idx_noadj", traversal: traversal}

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
	queryScan := adjustMatchScan()
	queryScanRef := expressions.InitialOf(queryScan)

	// --- Candidate side: filter(scan) ---
	candidateScan := adjustMatchScan()
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachQuantifier(candidateScanRef)
	innerFilter := adjustMatchFilter(candidateScanQ)
	candidateFilter := &adjustableFilterExpression{LogicalFilterExpression: innerFilter}
	candidateFilterRef := expressions.InitialOf(candidateFilter)
	traversal := NewTraversal(candidateFilterRef)

	mc := &adjustMatchCandidate{name: "idx_mismatch", traversal: traversal}

	// Seed a leaf match that points to a DIFFERENT candidate ref
	// (not candidateScanRef).
	otherScan := adjustMatchScan()
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

// seedAdjustableCandidate builds a candidate whose traversal is
// adjustableFilter(scan), seeds a leaf PartialMatch on queryScanRef pointing at
// the candidate's scan ref, and returns the candidate + the parent (filter) ref
// that a successful adjustment should produce a match on.
func seedAdjustableCandidate(name string, queryScanRef *expressions.Reference, queryScan expressions.RelationalExpression) (*adjustMatchCandidate, *expressions.Reference) {
	candidateScan := adjustMatchScan()
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachQuantifier(candidateScanRef)
	innerFilter := adjustMatchFilter(candidateScanQ)
	candidateFilter := &adjustableFilterExpression{LogicalFilterExpression: innerFilter}
	candidateFilterRef := expressions.InitialOf(candidateFilter)
	mc := &adjustMatchCandidate{name: name, traversal: NewTraversal(candidateFilterRef)}

	seedMI := NewRegularMatchInfo(nil, EmptyAliasMap(), nil, nil, nil, EmptyGroupByMappings(), nil, nil)
	seedPM := NewPartialMatch(EmptyAliasMap(), mc, queryScanRef, queryScan, candidateScanRef, seedMI)
	AddPartialMatchForCandidate(queryScanRef, mc, seedPM)
	return mc, candidateFilterRef
}

func candHasAdjustedMatch(ref *expressions.Reference, mc MatchCandidate, parentRef *expressions.Reference) bool {
	for _, pm := range GetPartialMatchesForCandidate(ref, mc) {
		pmi, ok := pm.(*PartialMatchImpl)
		if ok && pmi.GetCandidateRef() == parentRef && pmi.GetMatchInfo().IsAdjusted() {
			return true
		}
	}
	return false
}

// TestAdjustPartialMatches_LateSeededCandidateWaveStillAdjusted pins
// RFC-076-step-1: AdjustPartialMatchesForRef must adjust matches that
// are seeded AFTER an earlier candidate's matches were already adjusted (matches
// arrive in waves across repeated pushDataAccessTasks calls). The retired coarse
// `refHasAdjustedMatch` short-circuit skipped the whole ref once ANY match was
// adjusted, leaving a later candidate's seeds with empty matchedOrderingParts
// (sort elimination silently degrades). Idempotence is now per-match via the
// content dedup, so the second wave is still absorbed.
func TestAdjustPartialMatches_LateSeededCandidateWaveStillAdjusted(t *testing.T) {
	t.Parallel()

	queryScan := adjustMatchScan()
	queryScanRef := expressions.InitialOf(queryScan)

	// Wave 1: candidate A.
	mcA, parentA := seedAdjustableCandidate("idx_A", queryScanRef, queryScan)
	AdjustPartialMatchesForRef(queryScanRef)
	if !candHasAdjustedMatch(queryScanRef, mcA, parentA) {
		t.Fatal("wave-1 candidate A was not adjusted")
	}

	// Wave 2: candidate B seeds AFTER A is already adjusted.
	mcB, parentB := seedAdjustableCandidate("idx_B", queryScanRef, queryScan)
	AdjustPartialMatchesForRef(queryScanRef)
	if !candHasAdjustedMatch(queryScanRef, mcB, parentB) {
		t.Fatal("REGRESSION (finding 1): late-seeded candidate B was NOT adjusted — coarse ref-level idempotence guard skipped it")
	}
	// A must remain adjusted (unchanged).
	if !candHasAdjustedMatch(queryScanRef, mcA, parentA) {
		t.Fatal("candidate A lost its adjustment after wave 2")
	}
}

// TestAdjustPartialMatches_NoDuplicateExplosionOnRepeatedCalls pins that
// repeated AdjustPartialMatchesForRef calls do NOT accumulate duplicate adjusted
// matches (the reason the coarse guard existed) — the content dedup in
// AddPartialMatchForCandidate rejects content-equivalent re-adjustments.
func TestAdjustPartialMatches_NoDuplicateExplosionOnRepeatedCalls(t *testing.T) {
	t.Parallel()

	queryScan := adjustMatchScan()
	queryScanRef := expressions.InitialOf(queryScan)
	mc, _ := seedAdjustableCandidate("idx_X", queryScanRef, queryScan)

	AdjustPartialMatchesForRef(queryScanRef)
	after1 := len(GetPartialMatchesForCandidate(queryScanRef, mc))

	for i := 0; i < 10; i++ {
		AdjustPartialMatchesForRef(queryScanRef)
	}
	afterN := len(GetPartialMatchesForCandidate(queryScanRef, mc))

	if afterN != after1 {
		t.Fatalf("duplicate explosion: %d matches after 1 call, %d after 11 calls (content dedup must keep it stable)", after1, afterN)
	}
}

func TestAdjustMatchForSelect_RejectsMultiMemberLowerReference(t *testing.T) {
	t.Parallel()

	firstLower := mustAdjustMatchConstruct(expressions.NewSelectExpression(
		adjustMatchLong(1),
		nil,
		nil,
	))
	secondLower := mustAdjustMatchConstruct(expressions.NewSelectExpression(
		adjustMatchLong(2),
		nil,
		nil,
	))
	lowerRef := expressions.InitialOf(firstLower)
	inner := expressions.ForEachQuantifier(lowerRef)
	upperResult := mustAdjustMatchConstruct(inner.RequireFlowedObjectValue())
	upperSelect := mustAdjustMatchConstruct(expressions.NewSelectExpression(
		upperResult,
		[]expressions.Quantifier{inner},
		nil,
	))

	matchedValue := adjustMatchLong(7)
	maxMatchMap := NewMaxMatchMap(
		map[values.Value]values.Value{matchedValue: matchedValue},
		matchedValue,
		matchedValue,
	)
	matchedGroupings := NewValueBiMap()
	matchedGroupings.Put(
		adjustMatchLong(8),
		matchedValue,
	)
	matchInfo := NewRegularMatchInfo(
		nil,
		EmptyAliasMap(),
		nil,
		nil,
		maxMatchMap,
		NewGroupByMappings(
			matchedGroupings,
			NewValueBiMap(),
			NewCorrValueBiMap(),
		),
		nil,
		nil,
	)
	queryExpression := mustAdjustMatchConstruct(expressions.NewSelectExpression(matchedValue, nil, nil))
	partialMatch := NewPartialMatch(
		EmptyAliasMap(),
		adjustMatchCandidate{name: "adjust_select_singleton_guard"},
		expressions.InitialOf(queryExpression),
		queryExpression,
		expressions.InitialOf(firstLower),
		matchInfo,
	)

	if adjusted := adjustMatchForSelect(upperSelect, partialMatch); adjusted == nil {
		t.Fatal("singleton lower reference did not reach the adjustment guard")
	}
	if !lowerRef.Insert(secondLower) {
		t.Fatal("test lower alternatives unexpectedly deduplicated")
	}
	if got := len(lowerRef.AllMembers()); got != 2 {
		t.Fatalf("lower member count = %d, want 2", got)
	}
	if adjusted := adjustMatchForSelect(upperSelect, partialMatch); adjusted != nil {
		t.Fatal("multi-member lower reference selected an arbitrary result value")
	}
}

// TestAdjustMatchForSelect_BailsOnNonTautologyPredicate pins Java's
// SelectExpression.adjustMatch predicate walk (SelectExpression.java:612-621):
// an unconstrained Placeholder is skippable and so is a TAUTOLOGY, but any
// other predicate — a constant FALSE / NULL (a sparse index's `WHERE FALSE`
// select) or a comparison — must refuse absorption. The pre-fix Go port
// skipped EVERY ConstantPredicate, which let a scan-leaf match absorb an
// empty sparse index's select and serve it as a full row source
// (boolean-ddl.yamsql: 0 of 1 rows).
func TestAdjustMatchForSelect_BailsOnNonTautologyPredicate(t *testing.T) {
	t.Parallel()

	build := func(preds []predicates.QueryPredicate) (*expressions.SelectExpression, *PartialMatchImpl) {
		lower := mustAdjustMatchConstruct(expressions.NewSelectExpression(adjustMatchLong(1), nil, nil))
		inner := expressions.ForEachQuantifier(expressions.InitialOf(lower))
		upper := mustAdjustMatchConstruct(expressions.NewSelectExpression(
			mustAdjustMatchConstruct(inner.RequireFlowedObjectValue()),
			[]expressions.Quantifier{inner},
			preds,
		))
		matchedValue := adjustMatchLong(7)
		maxMatchMap := NewMaxMatchMap(
			map[values.Value]values.Value{matchedValue: matchedValue},
			matchedValue,
			matchedValue,
		)
		matchInfo := NewRegularMatchInfo(
			nil, EmptyAliasMap(), nil, nil, maxMatchMap,
			EmptyGroupByMappings(), nil, nil,
		)
		queryExpression := mustAdjustMatchConstruct(expressions.NewSelectExpression(matchedValue, nil, nil))
		pm := NewPartialMatch(
			EmptyAliasMap(),
			adjustMatchCandidate{name: "adjust_select_tautology_guard"},
			expressions.InitialOf(queryExpression),
			queryExpression,
			expressions.InitialOf(lower),
			matchInfo,
		)
		return upper, pm
	}

	upperTrue, pmTrue := build([]predicates.QueryPredicate{
		predicates.NewConstantPredicate(predicates.TriTrue),
	})
	if adjustMatchForSelect(upperTrue, pmTrue) == nil {
		t.Error("a tautology (TRUE) candidate predicate must remain absorbable (SelectExpression.java:617)")
	}

	for name, pred := range map[string]predicates.QueryPredicate{
		"constant_false": predicates.NewConstantPredicate(predicates.TriFalse),
		"constant_null":  predicates.NewConstantPredicate(predicates.TriUnknown),
		"comparison": predicates.NewComparisonPredicate(
			adjustMatchLong(3),
			predicates.NewLiteralComparison(predicates.ComparisonLessThan, int64(200)),
		),
	} {
		upper, pm := build([]predicates.QueryPredicate{pred})
		if adjustMatchForSelect(upper, pm) != nil {
			t.Errorf("%s: non-tautology candidate predicate absorbed — the filtered select's row "+
				"elimination is silently discarded (SelectExpression.java:617-620)", name)
		}
	}
}
