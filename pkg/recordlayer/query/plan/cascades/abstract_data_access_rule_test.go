package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// dataAccessTestCandidate is a minimal MatchCandidate for unit tests. It
// returns a fixed plan from ToScanPlan and uses simple sargable aliases.
type dataAccessTestCandidate struct {
	name            string
	sargableAliases []values.CorrelationIdentifier
	columnNames     []string
	recordTypes     []string
	fixedPlan       plans.RecordQueryPlan
}

func (c *dataAccessTestCandidate) CandidateName() string    { return c.name }
func (c *dataAccessTestCandidate) GetTraversal() *Traversal { return nil }
func (c *dataAccessTestCandidate) GetColumnNames() []string { return c.columnNames }
func (c *dataAccessTestCandidate) GetSargableAliases() []values.CorrelationIdentifier {
	return c.sargableAliases
}
func (c *dataAccessTestCandidate) GetRecordTypes() []string { return c.recordTypes }
func (c *dataAccessTestCandidate) IsUnique() bool           { return false }

func (c *dataAccessTestCandidate) ComputeBoundParameterPrefixMap(
	bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	// Return a copy of the bindings that match our sargable aliases.
	prefix := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
	for _, alias := range c.sargableAliases {
		if cr, ok := bindings[alias]; ok {
			prefix[alias] = cr
		}
	}
	return prefix
}

func (c *dataAccessTestCandidate) ToScanPlan(
	_ map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	_ bool,
) plans.RecordQueryPlan {
	return c.fixedPlan
}

// testPlan is a minimal RecordQueryPlan for tests.
type testPlan struct {
	name string
}

func (p *testPlan) GetResultType() values.Type           { return values.UnknownType }
func (p *testPlan) GetChildren() []plans.RecordQueryPlan { return nil }
func (p *testPlan) EqualsWithoutChildren(other plans.RecordQueryPlan) bool {
	o, ok := other.(*testPlan)
	return ok && o.name == p.name
}
func (p *testPlan) HashCodeWithoutChildren() uint64 { return 0 }
func (p *testPlan) Explain() string                 { return "TestPlan(" + p.name + ")" }

var _ plans.RecordQueryPlan = (*testPlan)(nil)

// testMatchInfo is a minimal MatchInfo for tests.
type testMatchInfo struct {
	orderingParts []*MatchedOrderingPart
	paramBindings map[values.CorrelationIdentifier]*predicates.ComparisonRange
}

func (m *testMatchInfo) GetMatchedOrderingParts() []*MatchedOrderingPart {
	return m.orderingParts
}
func (m *testMatchInfo) GetMaxMatchMap() *MaxMatchMap { return nil }
func (m *testMatchInfo) IsAdjusted() bool             { return false }
func (m *testMatchInfo) IsRegular() bool              { return true }
func (m *testMatchInfo) GetGroupByMappings() *GroupByMappings {
	return EmptyGroupByMappings()
}

func (m *testMatchInfo) GetRegularMatchInfo() *RegularMatchInfo {
	return NewRegularMatchInfo(
		m.paramBindings,
		nil, // bindingAliasMap
		nil, // predicateMap
		m.orderingParts,
		nil, // maxMatchMap
		EmptyGroupByMappings(),
		nil, // rollUpToGroupingValues
		nil, // additionalPlanConstraint
	)
}

// testPartialMatch is a minimal PartialMatch for tests.
type testPartialMatch struct {
	candidate MatchCandidate
	matchInfo MatchInfo
}

func (pm *testPartialMatch) GetMatchCandidate() MatchCandidate                    { return pm.candidate }
func (pm *testPartialMatch) GetMatchInfo() MatchInfo                              { return pm.matchInfo }
func (pm *testPartialMatch) GetBoundAliasMap() *AliasMap                          { return EmptyAliasMap() }
func (pm *testPartialMatch) GetQueryRef() *expressions.Reference                  { return nil }
func (pm *testPartialMatch) GetQueryExpression() expressions.RelationalExpression { return nil }
func (pm *testPartialMatch) GetCandidateRef() *expressions.Reference              { return nil }
func (pm *testPartialMatch) GetRegularMatchInfo() *RegularMatchInfo {
	return pm.matchInfo.GetRegularMatchInfo()
}

var _ PartialMatch = (*testPartialMatch)(nil)

// makeDataAccessTestPartialMatch creates a test PartialMatch with the given
// number of matched ordering parts (used as a proxy for coverage).
func makeDataAccessTestPartialMatch(name string, numParts int, plan plans.RecordQueryPlan) *testPartialMatch {
	alias := values.NamedCorrelationIdentifier(name + "_alias")
	candidate := &dataAccessTestCandidate{
		name:            name,
		sargableAliases: []values.CorrelationIdentifier{alias},
		columnNames:     []string{name + "_col"},
		recordTypes:     []string{"TestRecord"},
		fixedPlan:       plan,
	}

	parts := make([]*MatchedOrderingPart, numParts)
	paramBindings := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange, numParts)
	for i := 0; i < numParts; i++ {
		pid := values.UniqueCorrelationIdentifier()
		parts[i] = NewMatchedOrderingPart(
			pid,
			&values.FieldValue{Field: name, Typ: values.UnknownType},
			predicates.EmptyComparisonRange(),
			MatchedSortOrderAscending,
		)
		paramBindings[pid] = predicates.EmptyComparisonRange()
	}

	return &testPartialMatch{
		candidate: candidate,
		matchInfo: &testMatchInfo{
			orderingParts: parts,
			paramBindings: paramBindings,
		},
	}
}

// ---------------------------------------------------------------------------
// Tests: PrepareMatchesAndCompensations
// ---------------------------------------------------------------------------

func TestPrepareMatchesAndCompensations_ThreeMatches(t *testing.T) {
	t.Parallel()

	pm1 := makeDataAccessTestPartialMatch("idx1", 3, &testPlan{name: "scan1"})
	pm2 := makeDataAccessTestPartialMatch("idx2", 1, &testPlan{name: "scan2"})
	pm3 := makeDataAccessTestPartialMatch("idx3", 2, &testPlan{name: "scan3"})

	orderings := []*RequestedOrdering{PreserveOrdering()}
	ctx := EmptyPlanContext()

	accesses := PrepareMatchesAndCompensations(
		[]PartialMatch{pm1, pm2, pm3},
		orderings,
		ctx,
	)

	if len(accesses) != 3 {
		t.Fatalf("expected 3 accesses, got %d", len(accesses))
	}

	// Verify sorted by coverage descending.
	for i := 1; i < len(accesses); i++ {
		prev := len(accesses[i-1].GetPartialMatch().GetMatchInfo().GetMatchedOrderingParts())
		curr := len(accesses[i].GetPartialMatch().GetMatchInfo().GetMatchedOrderingParts())
		if prev < curr {
			t.Fatalf("accesses not sorted by coverage: index %d has %d parts but index %d has %d parts",
				i-1, prev, i, curr)
		}
	}

	// Verify each access has a unique candidateTopAlias.
	seen := make(map[values.CorrelationIdentifier]bool)
	for _, a := range accesses {
		alias := a.GetCandidateTopAlias()
		if seen[alias] {
			t.Fatalf("duplicate candidateTopAlias: %s", alias.Name())
		}
		seen[alias] = true
	}

	// testPartialMatch stubs don't implement PartialMatchImpl, so
	// CompensateCompleteMatch falls back to NoCompensation.
	for _, a := range accesses {
		if a.GetCompensation() != NoCompensation {
			t.Fatal("test stubs should yield NoCompensation")
		}
	}

	// Verify forward scan direction.
	for _, a := range accesses {
		if a.IsReverseScanOrder() {
			t.Fatal("test stubs should use forward scan")
		}
	}
}

func TestPrepareMatchesAndCompensations_EmptyInput(t *testing.T) {
	t.Parallel()

	accesses := PrepareMatchesAndCompensations(nil, nil, EmptyPlanContext())
	if len(accesses) != 0 {
		t.Fatalf("expected 0 accesses for nil input, got %d", len(accesses))
	}
}

func TestPrepareMatchesAndCompensations_SingleMatch(t *testing.T) {
	t.Parallel()

	pm := makeDataAccessTestPartialMatch("only", 5, &testPlan{name: "only_scan"})
	accesses := PrepareMatchesAndCompensations(
		[]PartialMatch{pm},
		[]*RequestedOrdering{PreserveOrdering()},
		EmptyPlanContext(),
	)
	if len(accesses) != 1 {
		t.Fatalf("expected 1 access, got %d", len(accesses))
	}
	if accesses[0].GetPartialMatch() != pm {
		t.Fatal("access should reference the original PartialMatch")
	}
}

// ---------------------------------------------------------------------------
// Tests: MaximumCoverageMatches
// ---------------------------------------------------------------------------

func TestMaximumCoverageMatches_WrapsWithPositions(t *testing.T) {
	t.Parallel()

	pm1 := makeDataAccessTestPartialMatch("a", 2, &testPlan{name: "a"})
	pm2 := makeDataAccessTestPartialMatch("b", 4, &testPlan{name: "b"})
	pm3 := makeDataAccessTestPartialMatch("c", 1, &testPlan{name: "c"})

	matches := MaximumCoverageMatches(
		[]PartialMatch{pm1, pm2, pm3},
		[]*RequestedOrdering{PreserveOrdering()},
		EmptyPlanContext(),
	)

	if len(matches) != 3 {
		t.Fatalf("expected 3 vectored matches (no Pareto filtering), got %d", len(matches))
	}

	// Verify positions are 0, 1, 2 (assigned after sorting by coverage).
	for i, m := range matches {
		if m.Position != i {
			t.Fatalf("expected position %d, got %d", i, m.Position)
		}
	}

	// Verify the highest-coverage match is first (pm2 has 4 parts).
	firstPM := matches[0].Value.GetPartialMatch()
	if firstPM != pm2 {
		t.Fatal("first match should be the highest-coverage one")
	}
}

func TestMaximumCoverageMatches_EmptyInput(t *testing.T) {
	t.Parallel()

	matches := MaximumCoverageMatches(nil, nil, EmptyPlanContext())
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches for nil input, got %d", len(matches))
	}
}

// ---------------------------------------------------------------------------
// Tests: CreateScansForMatches
// ---------------------------------------------------------------------------

func TestCreateScansForMatches_UsesCandidateToScanPlan(t *testing.T) {
	t.Parallel()

	plan1 := &testPlan{name: "idx1_scan"}
	plan2 := &testPlan{name: "idx2_scan"}
	pm1 := makeDataAccessTestPartialMatch("idx1", 2, plan1)
	pm2 := makeDataAccessTestPartialMatch("idx2", 3, plan2)

	// Build Vectored accesses.
	accesses := MaximumCoverageMatches(
		[]PartialMatch{pm1, pm2},
		[]*RequestedOrdering{PreserveOrdering()},
		EmptyPlanContext(),
	)

	scanMap := CreateScansForMatches(accesses, EmptyPlanContext())

	if len(scanMap) != 2 {
		t.Fatalf("expected 2 scan plans, got %d", len(scanMap))
	}

	// Verify each PartialMatch maps to the plan its candidate returns.
	for pm, plan := range scanMap {
		cand := pm.GetMatchCandidate().(*dataAccessTestCandidate)
		if plan != cand.fixedPlan {
			t.Fatalf("scan plan for %s should be the candidate's fixed plan", cand.name)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests: DataAccessForMatchPartition
// ---------------------------------------------------------------------------

func TestDataAccessForMatchPartition_SingleMatch(t *testing.T) {
	t.Parallel()

	plan := &testPlan{name: "single_idx"}
	pm := makeDataAccessTestPartialMatch("idx", 2, plan)

	exprs := DataAccessForMatchPartition(
		[]*RequestedOrdering{PreserveOrdering()},
		[]PartialMatch{pm},
		EmptyPlanContext(),
		nil, // no intersector for single match
	)

	if len(exprs) != 1 {
		t.Fatalf("expected 1 expression for single match, got %d", len(exprs))
	}

	// Verify the expression wraps the expected plan.
	spe, ok := exprs[0].(*scanPlanExpression)
	if !ok {
		t.Fatalf("expected *scanPlanExpression, got %T", exprs[0])
	}
	if spe.plan != plan {
		t.Fatal("scan plan expression should wrap the candidate's plan")
	}
}

func TestDataAccessForMatchPartition_NoMatches(t *testing.T) {
	t.Parallel()

	exprs := DataAccessForMatchPartition(
		[]*RequestedOrdering{PreserveOrdering()},
		nil, // no matches
		EmptyPlanContext(),
		nil,
	)

	if len(exprs) != 0 {
		t.Fatalf("expected 0 expressions for no matches, got %d", len(exprs))
	}
}

func TestDataAccessForMatchPartition_MultipleMatchesWithIntersector(t *testing.T) {
	t.Parallel()

	plan1 := &testPlan{name: "idx1"}
	plan2 := &testPlan{name: "idx2"}
	pm1 := makeDataAccessTestPartialMatch("idx1", 2, plan1)
	pm2 := makeDataAccessTestPartialMatch("idx2", 3, plan2)

	intersectExpr := &stubRelExpr{name: "intersection"}
	intersectorCalled := false

	intersector := func(
		accesses []Vectored[*SingleMatchedAccess],
		orderings []*RequestedOrdering,
	) *IntersectionResult {
		intersectorCalled = true
		if len(accesses) != 2 {
			t.Fatalf("intersector received %d accesses, expected 2", len(accesses))
		}
		return NewIntersectionResult(
			EmptyOrdering(),
			NoCompensation,
			[]expressions.RelationalExpression{intersectExpr},
		)
	}

	exprs := DataAccessForMatchPartition(
		[]*RequestedOrdering{PreserveOrdering()},
		[]PartialMatch{pm1, pm2},
		EmptyPlanContext(),
		intersector,
	)

	if !intersectorCalled {
		t.Fatal("intersector should have been called for multiple matches")
	}

	// Should have 2 individual scans + 1 intersection expression = 3.
	if len(exprs) != 3 {
		t.Fatalf("expected 3 expressions (2 scans + 1 intersection), got %d", len(exprs))
	}

	// Verify the intersection expression is among the results.
	found := false
	for _, e := range exprs {
		if e == intersectExpr {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("intersection expression should be in the result")
	}
}

func TestDataAccessForMatchPartition_MultipleMatchesNoIntersector(t *testing.T) {
	t.Parallel()

	plan1 := &testPlan{name: "idx1"}
	plan2 := &testPlan{name: "idx2"}
	pm1 := makeDataAccessTestPartialMatch("idx1", 1, plan1)
	pm2 := makeDataAccessTestPartialMatch("idx2", 1, plan2)

	// nil intersector -- should just return individual scans.
	exprs := DataAccessForMatchPartition(
		[]*RequestedOrdering{PreserveOrdering()},
		[]PartialMatch{pm1, pm2},
		EmptyPlanContext(),
		nil,
	)

	if len(exprs) != 2 {
		t.Fatalf("expected 2 expressions (individual scans only), got %d", len(exprs))
	}
}

func TestDataAccessForMatchPartition_IntersectorNoViable(t *testing.T) {
	t.Parallel()

	pm1 := makeDataAccessTestPartialMatch("idx1", 1, &testPlan{name: "idx1"})
	pm2 := makeDataAccessTestPartialMatch("idx2", 1, &testPlan{name: "idx2"})

	intersector := func(
		_ []Vectored[*SingleMatchedAccess],
		_ []*RequestedOrdering,
	) *IntersectionResult {
		return NoViableIntersection()
	}

	exprs := DataAccessForMatchPartition(
		[]*RequestedOrdering{PreserveOrdering()},
		[]PartialMatch{pm1, pm2},
		EmptyPlanContext(),
		intersector,
	)

	// No viable intersection -- only individual scans.
	if len(exprs) != 2 {
		t.Fatalf("expected 2 expressions (no viable intersection), got %d", len(exprs))
	}
}

// ---------------------------------------------------------------------------
// Tests: scanPlanExpression
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Tests: SatisfiesRequestedOrdering / SatisfiesAnyRequestedOrderings
// ---------------------------------------------------------------------------

// makeOrderingTestPartialMatch builds a testPartialMatch with explicit
// MatchedOrderingPart entries so callers control field names, sort
// orders, and comparison ranges.
func makeOrderingTestPartialMatch(parts []*MatchedOrderingPart) *testPartialMatch {
	paramBindings := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange, len(parts))
	for _, p := range parts {
		paramBindings[p.GetParameterId()] = p.GetComparisonRange()
	}
	return &testPartialMatch{
		candidate: &dataAccessTestCandidate{
			name:        "ordering_test",
			recordTypes: []string{"TestRecord"},
			fixedPlan:   &testPlan{name: "ordering_scan"},
		},
		matchInfo: &testMatchInfo{
			orderingParts: parts,
			paramBindings: paramBindings,
		},
	}
}

func TestSatisfiesRequestedOrdering_Preserve(t *testing.T) {
	t.Parallel()

	pm := makeOrderingTestPartialMatch(nil)
	dir := SatisfiesRequestedOrdering(pm, PreserveOrdering())
	if dir == nil {
		t.Fatal("PreserveOrdering should always be satisfied")
	}
	if *dir != ScanDirectionBoth {
		t.Fatalf("expected ScanDirectionBoth, got %d", *dir)
	}
}

func TestSatisfiesRequestedOrdering_SingleAscending(t *testing.T) {
	t.Parallel()

	fieldA := &values.FieldValue{Field: "a", Typ: values.UnknownType}

	parts := []*MatchedOrderingPart{
		NewMatchedOrderingPart(
			values.UniqueCorrelationIdentifier(),
			fieldA,
			predicates.EmptyComparisonRange(),
			MatchedSortOrderAscending,
		),
	}
	pm := makeOrderingTestPartialMatch(parts)

	ro := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: fieldA, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct,
		false,
	)

	dir := SatisfiesRequestedOrdering(pm, ro)
	if dir == nil {
		t.Fatal("ascending request with ascending match should be satisfied")
	}
	if *dir != ScanDirectionForward {
		t.Fatalf("expected ScanDirectionForward, got %d", *dir)
	}
}

func TestSatisfiesRequestedOrdering_ReverseNeeded(t *testing.T) {
	t.Parallel()

	fieldA := &values.FieldValue{Field: "a", Typ: values.UnknownType}

	parts := []*MatchedOrderingPart{
		NewMatchedOrderingPart(
			values.UniqueCorrelationIdentifier(),
			fieldA,
			predicates.EmptyComparisonRange(),
			MatchedSortOrderDescending,
		),
	}
	pm := makeOrderingTestPartialMatch(parts)

	// Request ascending, but matched is descending → reverse scan.
	ro := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: fieldA, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct,
		false,
	)

	dir := SatisfiesRequestedOrdering(pm, ro)
	if dir == nil {
		t.Fatal("ascending request with descending match should be satisfied via reverse")
	}
	if *dir != ScanDirectionReverse {
		t.Fatalf("expected ScanDirectionReverse, got %d", *dir)
	}
}

func TestSatisfiesRequestedOrdering_EqualitySkip(t *testing.T) {
	t.Parallel()

	fieldA := &values.FieldValue{Field: "a", Typ: values.UnknownType}
	fieldB := &values.FieldValue{Field: "b", Typ: values.UnknownType}

	// First matched part is equality-bound (should be skipped).
	eqComp := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42))
	eqRange := predicates.EmptyComparisonRange()
	merged := eqRange.Merge(&eqComp)
	if !merged.Ok {
		t.Fatal("failed to create equality comparison range")
	}

	parts := []*MatchedOrderingPart{
		NewMatchedOrderingPart(
			values.UniqueCorrelationIdentifier(),
			fieldA,
			merged.Range,
			MatchedSortOrderAscending,
		),
		NewMatchedOrderingPart(
			values.UniqueCorrelationIdentifier(),
			fieldB,
			predicates.EmptyComparisonRange(),
			MatchedSortOrderAscending,
		),
	}
	pm := makeOrderingTestPartialMatch(parts)

	// Request ordering on b only — a is equality-bound so it is
	// skipped during satisfaction.
	ro := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: fieldB, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct,
		false,
	)

	dir := SatisfiesRequestedOrdering(pm, ro)
	if dir == nil {
		t.Fatal("equality-bound prefix should be skipped, b should satisfy")
	}
	if *dir != ScanDirectionForward {
		t.Fatalf("expected ScanDirectionForward, got %d", *dir)
	}
}

func TestSatisfiesRequestedOrdering_NoMatch(t *testing.T) {
	t.Parallel()

	fieldA := &values.FieldValue{Field: "a", Typ: values.UnknownType}
	fieldB := &values.FieldValue{Field: "b", Typ: values.UnknownType}

	parts := []*MatchedOrderingPart{
		NewMatchedOrderingPart(
			values.UniqueCorrelationIdentifier(),
			fieldA,
			predicates.EmptyComparisonRange(),
			MatchedSortOrderAscending,
		),
	}
	pm := makeOrderingTestPartialMatch(parts)

	// Request ordering on field "b", which is not in the matched parts.
	ro := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: fieldB, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct,
		false,
	)

	dir := SatisfiesRequestedOrdering(pm, ro)
	if dir != nil {
		t.Fatal("ordering on unmatched field should return nil")
	}
}

func TestSatisfiesAnyRequestedOrderings_MixedResults(t *testing.T) {
	t.Parallel()

	fieldA := &values.FieldValue{Field: "a", Typ: values.UnknownType}
	fieldB := &values.FieldValue{Field: "b", Typ: values.UnknownType}

	parts := []*MatchedOrderingPart{
		NewMatchedOrderingPart(
			values.UniqueCorrelationIdentifier(),
			fieldA,
			predicates.EmptyComparisonRange(),
			MatchedSortOrderAscending,
		),
	}
	pm := makeOrderingTestPartialMatch(parts)

	// First ordering: ascending on "a" — should be satisfied (forward).
	roGood := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: fieldA, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct,
		false,
	)
	// Second ordering: ascending on "b" — should NOT be satisfied.
	roBad := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: fieldB, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct,
		false,
	)

	satisfied, dir := SatisfiesAnyRequestedOrderings(pm, []*RequestedOrdering{roGood, roBad})
	if dir == nil {
		t.Fatal("at least one ordering should be satisfied")
	}
	if *dir != ScanDirectionForward {
		t.Fatalf("expected ScanDirectionForward, got %d", *dir)
	}
	if len(satisfied) != 1 {
		t.Fatalf("expected 1 satisfied ordering, got %d", len(satisfied))
	}
	if satisfied[0] != roGood {
		t.Fatal("the satisfied ordering should be roGood")
	}
}

func TestScanPlanExpression_GetRecordQueryPlan(t *testing.T) {
	t.Parallel()

	plan := &testPlan{name: "test"}
	expr := &scanPlanExpression{plan: plan}

	if expr.GetRecordQueryPlan() != plan {
		t.Fatal("GetRecordQueryPlan should return the wrapped plan")
	}
}

func TestScanPlanExpression_EqualsWithoutChildren(t *testing.T) {
	t.Parallel()

	plan := &testPlan{name: "same"}
	e1 := &scanPlanExpression{plan: plan}
	e2 := &scanPlanExpression{plan: &testPlan{name: "same"}}
	e3 := &scanPlanExpression{plan: &testPlan{name: "different"}}

	if !e1.EqualsWithoutChildren(e2, nil) {
		t.Fatal("equal plans should produce equal expressions")
	}
	if e1.EqualsWithoutChildren(e3, nil) {
		t.Fatal("different plans should produce non-equal expressions")
	}
	if e1.EqualsWithoutChildren(&stubRelExpr{name: "x"}, nil) {
		t.Fatal("different expression types should not be equal")
	}
}
