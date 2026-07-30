package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ---------------------------------------------------------------------------
// The fixture's record row
// ---------------------------------------------------------------------------

// dataAccessTestIndexNames declares every synthetic index the fixtures in this
// package build a match candidate for. A name handed to
// makeDataAccessTestPartialMatch and NOT listed here fails loudly at
// dataAccessTestKey, because its columns are then absent from the row layout
// below and the key it mints would state no column identity — which both
// ordering comparators decline. A fixture that declines is not a fixture that
// tests less; it is a fixture that tests nothing, so the failure is deliberate
// and its message says what to add.
var dataAccessTestIndexNames = []string{
	"a", "b", "c",
	"child_idx",
	"idx", "idx1", "idx2", "idx3",
	"idxA", "idxB", "idxC", "idxD", "idxE",
	"idx_a", "idx_b",
	"idx_fanout", "idx_plain",
	"idxL", "idxM",
	"idx_test",
	"nilIdx", "only", "sameIdx",
	"scan_a", "scan_b", "scan_c",
}

// dataAccessTestColumnsPerIndex is how many leading columns each declared index
// gets. Every candidate's columns are DISTINCT from every other candidate's,
// because the partition-redundancy sieve compares the legs' equality-bound
// column sets: two legs binding the same column make each other redundant, and
// a fixture that accidentally shared columns would sieve away the very
// intersections it exists to produce.
const dataAccessTestColumnsPerIndex = 5

// dataAccessTestRow is the ONE record row layout the fixture's candidates bake
// their ordering keys against, and the layout the fixture's scans flow.
//
// Production candidates do exactly this: ValueIndexScanMatchCandidate's
// orderingKeyLayout is the RECORD's row layout, never the per-index entry
// layout, and the primary-key intersection depends on it — two legs baking the
// same primary-key column against two DIFFERENT layouts get two different
// ordinal domain tokens, the comparators (which decide a FieldValue pair by
// column identity and nothing else) call those keys different columns, and the
// merged ordering comes out empty. So the fixture states one layout for one
// record type with many indexes over it, which is the shape the planner
// actually sees.
//
// ID and VERSION lead deliberately: the composite-pk tests assert the baked
// comparison keys land on ordinals 0 and 1 of this layout.
var dataAccessTestRow = newDataAccessTestRow()

func newDataAccessTestRow() *values.RecordType {
	fields := []values.Field{
		{Name: "ID", FieldType: values.NullableLong},
		{Name: "VERSION", FieldType: values.NullableLong},
		{Name: "A", FieldType: values.NullableLong},
		{Name: "B", FieldType: values.NullableLong},
		{Name: "SORT_KEY", FieldType: values.NullableLong},
		{Name: "NAME", FieldType: values.NullableLong},
		{Name: "DUP", FieldType: values.NullableLong},
	}
	for _, index := range dataAccessTestIndexNames {
		for position := 0; position < dataAccessTestColumnsPerIndex; position++ {
			fields = append(fields, values.Field{
				Name:      dataAccessTestColumn(index, position),
				FieldType: values.NullableLong,
			})
		}
	}
	return values.NewRecordType("TestRecord", false, fields)
}

// dataAccessTestColumn names one column of a declared index.
func dataAccessTestColumn(index string, position int) string {
	return index + "_col" + string(rune('a'+position))
}

// dataAccessTestDomain is dataAccessTestRow's ordinal domain token — the layout
// a fixture key's ordinal indexes. Fixtures that need to state an ordinal the
// row's own column order does not give them (a deliberate same-name/different-
// slot pair, a request rooted at a named quantifier) bake into this domain
// directly rather than inventing a second layout.
var dataAccessTestDomain = values.OrdinalDomainOfType(dataAccessTestRow)

// dataAccessTestOrdinal is the slot dataAccessTestRow states for a column.
func dataAccessTestOrdinal(column string) int {
	ordinal, unique := uniqueUpperFieldIndex(dataAccessTestRow, column)
	if !unique {
		panic("dataAccessTestOrdinal: dataAccessTestRow does not state exactly " +
			"one column named " + column)
	}
	return ordinal
}

// dataAccessTestKey bakes one column of dataAccessTestRow the way a production
// candidate's ordering-key mint does — bakeOrderingColumnIn against the record
// row the scan flows — and asserts the result STATES a column identity.
//
// The assertion is the point. bakeOrderingColumnIn falls back to a lazy
// name-only Value for a column the layout does not state, and a lazy key is
// UNADDRESSABLE to both ordering comparators: they would decline it, the
// fixture's intersection or ordering match would silently stop forming, and the
// test would either pass vacuously or fail for a reason unrelated to what it
// names. Fail here instead, where the message can say which column is missing.
func dataAccessTestKey(column string) values.Value {
	key := bakeOrderingColumnIn(dataAccessTestRow, column)
	if !values.StatesOrderingColumn(key) {
		panic("dataAccessTestKey: column " + column + " is not a field of " +
			"dataAccessTestRow, so it bakes to a name-only key that both " +
			"ordering comparators DECLINE. Add the column to " +
			"newDataAccessTestRow (or the index name to " +
			"dataAccessTestIndexNames) rather than letting the fixture mint a " +
			"key with no layout behind its ordinal.")
	}
	return key
}

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// dataAccessTestCandidate is a minimal MatchCandidate for unit tests. It
// returns a fixed plan from ToScanPlan and uses simple sargable aliases.
type dataAccessTestCandidate struct {
	name              string
	sargableAliases   []values.CorrelationIdentifier
	columnNames       []string
	recordTypes       []string
	fixedPlan         plans.RecordQueryPlan
	unique            bool
	createsDuplicates bool
}

func (c *dataAccessTestCandidate) CandidateName() string    { return c.name }
func (c *dataAccessTestCandidate) GetTraversal() *Traversal { return nil }
func (c *dataAccessTestCandidate) GetColumnNames() []string { return c.columnNames }
func (c *dataAccessTestCandidate) GetSargableAliases() []values.CorrelationIdentifier {
	return c.sargableAliases
}
func (c *dataAccessTestCandidate) GetRecordTypes() []string { return c.recordTypes }
func (c *dataAccessTestCandidate) IsUnique() bool           { return c.unique }
func (c *dataAccessTestCandidate) CreatesDuplicates() bool {
	return c.createsDuplicates
}

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

// orderingKeyLayout names the ONE row layout this candidate's ordering keys are
// domained in — the orderingKeyLayoutProvider contract both production
// candidates implement. The double has to implement it too: the primary-key
// intersector bakes the partition's comparison keys into the layout the legs
// AGREE on, and a candidate that names none leaves those keys name-only, so they
// state no column identity and the comparator declines them against the legs'
// own baked keys.
//
// It reads the layout off the scan the candidate flows, so a fixture that
// deliberately flows none (the layout-less decline tests) stays layout-less here
// without a second knob to keep in sync.
func (c *dataAccessTestCandidate) orderingKeyLayout() *values.RecordType {
	if c.fixedPlan == nil {
		return nil
	}
	rt, isRecord := c.fixedPlan.GetResultType().(*values.RecordType)
	if !isRecord {
		return nil
	}
	return rt
}

// testPlan is a minimal RecordQueryPlan for tests.
type testPlan struct {
	plans.PlanExprBase
	name string
	// resultType lets fixtures flow a real row layout (production match
	// candidates always do) — the intersector's comparison-key baking
	// declines candidates whose layout is unknown, so intersector tests
	// must carry one. Zero value keeps UnknownType for everything else.
	resultType values.Type
}

func (p *testPlan) GetResultType() values.Type {
	if p.resultType != nil {
		return p.resultType
	}
	return values.UnknownType
}
func (p *testPlan) GetChildren() []plans.RecordQueryPlan { return nil }
func (p *testPlan) EqualsPlanWithoutChildren(other plans.RecordQueryPlan) bool {
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
	maxMatchMap   *MaxMatchMap
}

func (m *testMatchInfo) GetMatchedOrderingParts() []*MatchedOrderingPart {
	return m.orderingParts
}

func (m *testMatchInfo) GetMaxMatchMap() *MaxMatchMap {
	if m.maxMatchMap != nil {
		return m.maxMatchMap
	}
	// AbstractDataAccessRule requires every real match to carry a pullable
	// MaxMatchMap. Give the shared lightweight fixture the corresponding
	// identity map so tests exercise that production gate instead of relying on
	// the old hard-coded empty translation.
	current := values.LiteralValue("identity")
	return NewMaxMatchMap(
		map[values.Value]values.Value{current: current},
		current,
		current,
	)
}
func (m *testMatchInfo) IsAdjusted() bool { return false }
func (m *testMatchInfo) IsRegular() bool  { return true }
func (m *testMatchInfo) GetGroupByMappings() *GroupByMappings {
	return EmptyGroupByMappings()
}

func (m *testMatchInfo) GetRegularMatchInfo() *RegularMatchInfo {
	return NewRegularMatchInfo(
		m.paramBindings,
		nil, // bindingAliasMap
		nil, // predicateMap
		m.orderingParts,
		m.GetMaxMatchMap(),
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
	return makeDataAccessTestPartialMatchWithPK(name, numParts, plan, "ID")
}

// makeDataAccessTestPartialMatchWithPK is the composite-pk variant: the
// matched ordering continues into the given pk suffix fields in order
// (real candidates append the whole trimmed pk, not just its head).
func makeDataAccessTestPartialMatchWithPK(name string, numParts int, plan plans.RecordQueryPlan, pkFields ...string) *testPartialMatch {
	// Build a RESTRICTED match: each part's sargable alias is bound to a
	// non-empty equality range so hasRestrictedScan(pm) is true. The
	// data-access path now skips zero-prefix matches (a full index scan
	// with no selectivity is strictly dominated by the table scan; this
	// mirrors ImplementIndexScanRule's len(prefix)==0 guard), so a test
	// match must carry a real bound prefix to exercise the scan path.
	eqCmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))
	eqRange := predicates.EmptyComparisonRange().Merge(&eqCmp).Range

	sargAliases := make([]values.CorrelationIdentifier, numParts)
	columnNames := make([]string, numParts)
	parts := make([]*MatchedOrderingPart, numParts)
	paramBindings := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange, numParts)
	for i := 0; i < numParts; i++ {
		pid := values.UniqueCorrelationIdentifier()
		col := dataAccessTestColumn(name, i)
		sargAliases[i] = pid
		columnNames[i] = col
		parts[i] = NewMatchedOrderingPart(
			pid,
			dataAccessTestKey(col),
			eqRange,
			MatchedSortOrderAscending,
		)
		paramBindings[pid] = eqRange
	}

	// Real candidates continue the matched ordering into the trimmed
	// primary-key suffix (ComputeMatchedOrderingParts) — the fixture must
	// model that, or common-ordering derivation would see an index with no
	// primary-key continuation and decline every intersection the tests build.
	for _, pkField := range pkFields {
		parts = append(parts, NewMatchedOrderingPart(
			values.UniqueCorrelationIdentifier(),
			dataAccessTestKey(pkField),
			nil,
			MatchedSortOrderAscending,
		))
	}

	candidate := &dataAccessTestCandidate{
		name:            name,
		sargableAliases: sargAliases,
		columnNames:     columnNames,
		recordTypes:     []string{"TestRecord"},
		fixedPlan:       plan,
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

	orderings := []*properties.RequestedOrdering{properties.PreserveOrdering()}
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
		[]*properties.RequestedOrdering{properties.PreserveOrdering()},
		EmptyPlanContext(),
	)
	if len(accesses) != 1 {
		t.Fatalf("expected 1 access, got %d", len(accesses))
	}
	if accesses[0].GetPartialMatch() != pm {
		t.Fatal("access should reference the original PartialMatch")
	}
}

func TestPrepareMatchesAndCompensations_TranslatesRequestedOrderingAtTop(
	t *testing.T,
) {
	t.Parallel()

	queryResult := values.LiteralValue("query_scalar")
	candidateField := dataAccessTestKey("SORT_KEY")
	candidateResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{
			Name:  "SORT_KEY",
			Value: candidateField,
		},
	)
	maxMatchMap := NewMaxMatchMap(
		map[values.Value]values.Value{queryResult: candidateField},
		queryResult,
		candidateResult,
	)
	topMap, ok := maxMatchMap.PullUpMaybe(
		values.CurrentAlias,
		values.CurrentAlias,
	)
	if !ok {
		t.Fatal("fixture MaxMatchMap did not produce a top-to-top translation")
	}
	requestedValue := values.NewQuantifiedObjectValue(values.CurrentAlias)
	translatedRequest := translateIntersectionRequestedOrdering(
		properties.NewRequestedOrdering(
			[]properties.RequestedOrderingPart{{
				Value:     requestedValue,
				SortOrder: properties.RequestedSortOrderAscending,
			}},
			properties.DistinctnessNotDistinct,
			false,
		),
		topMap,
	)
	translatedValue := translatedRequest.GetParts()[0].Value
	if values.ValuesStructurallyEqual(translatedValue, requestedValue) {
		t.Fatal("fixture top-to-top translation is unexpectedly an identity")
	}

	pm := &testPartialMatch{
		candidate: &dataAccessTestCandidate{
			name:        "idx_sort",
			recordTypes: []string{"TestRecord"},
			fixedPlan:   &testPlan{name: "idx_sort"},
		},
		matchInfo: &testMatchInfo{
			orderingParts: []*MatchedOrderingPart{
				NewMatchedOrderingPart(
					values.UniqueCorrelationIdentifier(),
					translatedValue,
					nil,
					MatchedSortOrderAscending,
				),
			},
			maxMatchMap: maxMatchMap,
		},
	}
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     requestedValue,
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessNotDistinct,
		false,
	)

	accesses := PrepareMatchesAndCompensations(
		[]PartialMatch{pm},
		[]*properties.RequestedOrdering{requested},
		EmptyPlanContext(),
	)
	if len(accesses) != 1 {
		t.Fatalf("translated ordered access count = %d, want 1", len(accesses))
	}
	access := accesses[0]
	if access.GetTopToTopTranslationMap() == nil ||
		access.GetTopToTopTranslationMap().DefinesOnlyIdentities() {
		t.Fatal("prepared access lost its non-identity top-to-top map")
	}
	satisfying := access.GetSatisfyingRequestedOrderings()
	if len(satisfying) != 1 ||
		!values.ValuesStructurallyEqual(
			satisfying[0].GetParts()[0].Value,
			translatedValue,
		) {
		t.Fatalf(
			"prepared satisfying ordering = %#v, want translated candidate-top value",
			satisfying,
		)
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
		[]*properties.RequestedOrdering{properties.PreserveOrdering()},
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
		[]*properties.RequestedOrdering{properties.PreserveOrdering()},
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
		[]*properties.RequestedOrdering{properties.PreserveOrdering()},
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
		[]*properties.RequestedOrdering{properties.PreserveOrdering()},
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
		orderings []*properties.RequestedOrdering,
	) *IntersectionResult {
		intersectorCalled = true
		if len(accesses) != 2 {
			t.Fatalf("intersector received %d accesses, expected 2", len(accesses))
		}
		return NewIntersectionResult(
			properties.EmptyOrdering(),
			NoCompensation,
			[]expressions.RelationalExpression{intersectExpr},
		)
	}

	exprs := DataAccessForMatchPartition(
		[]*properties.RequestedOrdering{properties.PreserveOrdering()},
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
		[]*properties.RequestedOrdering{properties.PreserveOrdering()},
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
		_ []*properties.RequestedOrdering,
	) *IntersectionResult {
		return NoViableIntersection()
	}

	exprs := DataAccessForMatchPartition(
		[]*properties.RequestedOrdering{properties.PreserveOrdering()},
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
	dir := SatisfiesRequestedOrdering(pm, properties.PreserveOrdering())
	if dir == nil {
		t.Fatal("PreserveOrdering should always be satisfied")
	}
	if *dir != ScanDirectionBoth {
		t.Fatalf("expected ScanDirectionBoth, got %d", *dir)
	}
}

func TestSatisfiesRequestedOrdering_SingleAscending(t *testing.T) {
	t.Parallel()

	fieldA := dataAccessTestKey("A")

	parts := []*MatchedOrderingPart{
		NewMatchedOrderingPart(
			values.UniqueCorrelationIdentifier(),
			fieldA,
			predicates.EmptyComparisonRange(),
			MatchedSortOrderAscending,
		),
	}
	pm := makeOrderingTestPartialMatch(parts)

	ro := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: fieldA, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct,
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

	fieldA := dataAccessTestKey("A")

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
	ro := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: fieldA, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct,
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

	fieldA := dataAccessTestKey("A")
	fieldB := dataAccessTestKey("B")

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
	ro := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: fieldB, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct,
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

	fieldA := dataAccessTestKey("A")
	fieldB := dataAccessTestKey("B")

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
	ro := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: fieldB, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct,
		false,
	)

	dir := SatisfiesRequestedOrdering(pm, ro)
	if dir != nil {
		t.Fatal("ordering on unmatched field should return nil")
	}
}

func TestSatisfiesRequestedOrdering_DoesNotCollapseBakedOrdinals(t *testing.T) {
	t.Parallel()

	// The same display name in the same STATED layout at two different slots.
	// Only the ordinal separates them, which is the whole claim: the comparator
	// decides a FieldValue pair by column identity, and a display name is never
	// consulted. Both sides state an identity, so the decline below comes from
	// the identity arm and not from one operand being unaddressable.
	matched := values.NewFieldValueWithResolvedOrdinalInDomain(
		"DUP", 0, values.UnknownType, dataAccessTestDomain)
	requestedOtherSlot := values.NewFieldValueWithResolvedOrdinalInDomain(
		"DUP", 1, values.UnknownType, dataAccessTestDomain)
	pm := makeOrderingTestPartialMatch([]*MatchedOrderingPart{
		NewMatchedOrderingPart(
			values.UniqueCorrelationIdentifier(),
			matched,
			predicates.EmptyComparisonRange(),
			MatchedSortOrderAscending,
		),
	})
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     requestedOtherSlot,
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessNotDistinct,
		false,
	)

	if dir := SatisfiesRequestedOrdering(pm, requested); dir != nil {
		t.Fatalf("different baked ordinal was accepted with direction %v", *dir)
	}
}

// TestSatisfiesRequestedOrdering_DeclinesANameOnlyMatchedKey pins the contract
// type dispatch replaced the lazy/baked NAME bridge with.
//
// A pair of plain FieldValues is decided by column identity and nothing else, so
// a candidate that minted a name-only ordering key — bakeOrderingColumnIn's
// fallback when the scan flows no layout, or when the column name is ambiguous
// in it — is UNADDRESSABLE: the request does not match it and the ordered access
// is not offered. The bridge that used to accept this pair on the strength of
// equal names was INTRANSITIVE inside the FieldValue class (a name-only key
// bridges to ordinal 0 of two different layouts, which identity keeps apart), and
// an ordering set built through a non-equivalence relation depends on the order
// it was built in.
//
// The cost of declining is measured, not assumed: over the whole corpus, neither
// comparator ever sees a FieldValue pair with a non-stating operand
// (pkg/relational/conformance/explaindiff's ordering-census test asserts that
// residual is ZERO). If it ever stops being zero, the fix is at the producer that
// minted the key without its layout — not here.
func TestSatisfiesRequestedOrdering_DeclinesANameOnlyMatchedKey(t *testing.T) {
	t.Parallel()

	matched := &values.FieldValue{Field: "sort_key", Typ: values.UnknownType}
	if values.StatesOrderingColumn(matched) {
		t.Fatalf("test setup: %q states a column identity, so it is not the "+
			"name-only key this test is about",
			values.ExplainValue(matched))
	}
	requestedBaked := dataAccessTestKey("SORT_KEY")
	pm := makeOrderingTestPartialMatch([]*MatchedOrderingPart{
		NewMatchedOrderingPart(
			values.UniqueCorrelationIdentifier(),
			matched,
			predicates.EmptyComparisonRange(),
			MatchedSortOrderAscending,
		),
	})
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     requestedBaked,
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessNotDistinct,
		false,
	)

	// The premise, asserted rather than assumed: the retired arms DID accept
	// this pair on the strength of the equal name, so the decline below is the
	// dispatch and not some unrelated mismatch.
	if !values.CanBridgeOrderingValueRoots(matched, requestedBaked) {
		t.Fatalf("test setup: the name bridge no longer accepts %q against %q, "+
			"so this test no longer demonstrates what type dispatch gave up",
			values.ExplainValue(matched), values.ExplainValue(requestedBaked))
	}

	if dir := SatisfiesRequestedOrdering(pm, requested); dir != nil {
		t.Fatalf("a name-only matched ordering key satisfied a request for the "+
			"same column name (direction %v).\n\n"+
			"That is the name bridge back inside the FieldValue class, and it is "+
			"intransitive there: the same name-only key also bridges to ordinal 0 "+
			"of a DIFFERENT layout, which identity keeps apart, so the ordering "+
			"sets built through the comparator become insertion-order dependent.",
			*dir)
	}
}

// TestSatisfiesRequestedOrdering_AdmitsQualifiedRequestAgainstLocalCandidate
// pins the ONE root asymmetry the identity arm permits: a childless candidate
// key (Go's canonical source-relative root) against a request scoped to the
// quantifier that owns it. Both state the same column of the same layout, so
// identity accepts them — no name comparison is involved.
func TestSatisfiesRequestedOrdering_AdmitsQualifiedRequestAgainstLocalCandidate(t *testing.T) {
	t.Parallel()

	matched := dataAccessTestKey("NAME")
	alias := values.NamedCorrelationIdentifier("C")
	requestedQualified := values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		values.NewQuantifiedObjectValue(alias),
		"NAME",
		dataAccessTestOrdinal("NAME"),
		values.UnknownType,
		dataAccessTestDomain,
	)
	pm := makeOrderingTestPartialMatch([]*MatchedOrderingPart{
		NewMatchedOrderingPart(
			values.UniqueCorrelationIdentifier(),
			matched,
			predicates.EmptyComparisonRange(),
			MatchedSortOrderAscending,
		),
	})
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     requestedQualified,
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessNotDistinct,
		false,
	)

	dir := SatisfiesRequestedOrdering(pm, requested)
	if dir == nil || *dir != ScanDirectionForward {
		t.Fatalf("a request rooted at the quantifier that owns the candidate's "+
			"column did not match the candidate's source-relative key for the "+
			"same column: direction = %v, want forward", dir)
	}
}

func TestSatisfiesAnyRequestedOrderings_MixedResults(t *testing.T) {
	t.Parallel()

	fieldA := dataAccessTestKey("A")
	fieldB := dataAccessTestKey("B")

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
	roGood := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: fieldA, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct,
		false,
	)
	// Second ordering: ascending on "b" — should NOT be satisfied.
	roBad := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: fieldB, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct,
		false,
	)

	satisfied, dir := SatisfiesAnyRequestedOrderings(pm, []*properties.RequestedOrdering{roGood, roBad})
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

func (t *testPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*testPlan)
	return ok && t.EqualsPlanWithoutChildren(o)
}

func (t *testPlan) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return t
}
