package cascades

import (
	"slices"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ---------------------------------------------------------------------------
// Test double: PlanContext that returns fixed PK columns
// ---------------------------------------------------------------------------

type testPlanContextForIntersection struct {
	emptyPlanContext
	pkColumns map[string][]string
}

func (c *testPlanContextForIntersection) GetPrimaryKeyColumns(recordType string) []string {
	return c.pkColumns[recordType]
}

// testIntersectorRowType is the row layout the fixture scans flow —
// production match-candidate scans always carry one, and the
// comparison-key baking declines layout-less candidates.
var testIntersectorRowType = values.NewRecordType("TestRecord", false, []values.Field{
	{Name: "ID", FieldType: values.NullableLong},
	{Name: "VERSION", FieldType: values.NullableLong},
})

func newTestPKContext(recordType string, cols []string) *testPlanContextForIntersection {
	return &testPlanContextForIntersection{
		pkColumns: map[string][]string{recordType: cols},
	}
}

// ---------------------------------------------------------------------------
// Helper: build a Vectored[*SingleMatchedAccess] from a testPartialMatch
// ---------------------------------------------------------------------------

func makeVectoredAccess(pm *testPartialMatch, position int) Vectored[*SingleMatchedAccess] {
	alias := values.UniqueCorrelationIdentifier()
	access := NewSingleMatchedAccess(
		pm,
		NoCompensation,
		alias,
		false,
		EmptyTranslationMap(),
		nil,
	)
	return NewVectored(access, position)
}

// ---------------------------------------------------------------------------
// Tests: WithPrimaryKeyIntersector
// ---------------------------------------------------------------------------

func TestIntersector_TwoAccesses_DifferentCandidates(t *testing.T) {
	t.Parallel()

	planA := &testPlan{name: "scanA", resultType: testIntersectorRowType}
	planB := &testPlan{name: "scanB", resultType: testIntersectorRowType}
	pmA := makeDataAccessTestPartialMatch("idxA", 2, planA)
	pmB := makeDataAccessTestPartialMatch("idxB", 1, planB)

	ctx := newTestPKContext("TestRecord", []string{"id"})
	intersector := WithPrimaryKeyIntersector(ctx)

	accesses := []Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(pmA, 0),
		makeVectoredAccess(pmB, 1),
	}

	result := intersector(accesses, nil)
	if !result.IsViable() {
		t.Fatal("expected viable intersection from 2 different candidates")
	}

	exprs := result.GetExpressions()
	if len(exprs) != 1 {
		t.Fatalf("expected 1 intersection expression, got %d", len(exprs))
	}

	// The expression should be a physicalIntersectionWrapper with a
	// 2-leg RecordQueryIntersectionPlan.
	wrapper, ok := exprs[0].(*physicalIntersectionWrapper)
	if !ok {
		t.Fatalf("expected *physicalIntersectionWrapper, got %T", exprs[0])
	}
	plan := wrapper.GetPlan()
	if len(plan.GetChildren()) != 2 {
		t.Fatalf("expected 2 children in intersection plan, got %d", len(plan.GetChildren()))
	}
}

func TestIntersector_SingleAccess_NoIntersection(t *testing.T) {
	t.Parallel()

	pm := makeDataAccessTestPartialMatch("only", 3, &testPlan{name: "scan", resultType: testIntersectorRowType})
	ctx := newTestPKContext("TestRecord", []string{"id"})
	intersector := WithPrimaryKeyIntersector(ctx)

	accesses := []Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(pm, 0),
	}

	result := intersector(accesses, nil)
	if result.IsViable() {
		t.Fatal("expected NoViableIntersection for a single access")
	}
}

func TestIntersector_SameCandidateSkipped(t *testing.T) {
	t.Parallel()

	plan := &testPlan{name: "scan", resultType: testIntersectorRowType}
	pm := makeDataAccessTestPartialMatch("sameIdx", 2, plan)

	// Both accesses share the same candidate (same *testPartialMatch).
	alias1 := values.UniqueCorrelationIdentifier()
	alias2 := values.UniqueCorrelationIdentifier()
	access1 := NewSingleMatchedAccess(pm, NoCompensation, alias1, false, EmptyTranslationMap(), nil)
	access2 := NewSingleMatchedAccess(pm, NoCompensation, alias2, false, EmptyTranslationMap(), nil)

	ctx := newTestPKContext("TestRecord", []string{"id"})
	intersector := WithPrimaryKeyIntersector(ctx)

	accesses := []Vectored[*SingleMatchedAccess]{
		NewVectored(access1, 0),
		NewVectored(access2, 1),
	}

	result := intersector(accesses, nil)
	if result.IsViable() {
		t.Fatal("expected NoViableIntersection when both accesses share the same candidate")
	}
}

func TestIntersector_ThreeWay(t *testing.T) {
	t.Parallel()

	pmA := makeDataAccessTestPartialMatch("idxA", 1, &testPlan{name: "scanA", resultType: testIntersectorRowType})
	pmB := makeDataAccessTestPartialMatch("idxB", 1, &testPlan{name: "scanB", resultType: testIntersectorRowType})
	pmC := makeDataAccessTestPartialMatch("idxC", 1, &testPlan{name: "scanC", resultType: testIntersectorRowType})

	ctx := newTestPKContext("TestRecord", []string{"id"})
	intersector := WithPrimaryKeyIntersector(ctx)

	accesses := []Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(pmA, 0),
		makeVectoredAccess(pmB, 1),
		makeVectoredAccess(pmC, 2),
	}

	result := intersector(accesses, nil)
	if !result.IsViable() {
		t.Fatal("expected viable intersection from 3 different candidates")
	}

	// 3 candidates produce C(3,2) = 3 two-way + C(3,3) = 1 three-way = 4.
	exprs := result.GetExpressions()
	if len(exprs) != 4 {
		t.Fatalf("expected 4 intersection expressions (3 two-way + 1 three-way), got %d", len(exprs))
	}

	// Count 2-leg and 3-leg plans.
	twoWay, threeWay := 0, 0
	for _, e := range exprs {
		wrapper, ok := e.(*physicalIntersectionWrapper)
		if !ok {
			t.Fatalf("expected *physicalIntersectionWrapper, got %T", e)
		}
		switch n := len(wrapper.GetPlan().GetChildren()); n {
		case 2:
			twoWay++
		case 3:
			threeWay++
		default:
			t.Fatalf("unexpected plan child count: %d", n)
		}
	}
	if twoWay != 3 {
		t.Fatalf("expected 3 two-way intersections, got %d", twoWay)
	}
	if threeWay != 1 {
		t.Fatalf("expected 1 three-way intersection, got %d", threeWay)
	}
}

// ---------------------------------------------------------------------------
// Tests: commonPrimaryKeyValues
// ---------------------------------------------------------------------------

func TestCommonPrimaryKeyValues_EmptyAccesses(t *testing.T) {
	t.Parallel()

	result := commonPrimaryKeyValues(nil, EmptyPlanContext())
	if result != nil {
		t.Fatalf("expected nil for empty accesses, got %v", result)
	}
}

func TestCommonPrimaryKeyValues_MixedRecordTypes(t *testing.T) {
	t.Parallel()

	// Two accesses with different record types.
	candidateA := &dataAccessTestCandidate{
		name:        "idxA",
		recordTypes: []string{"TypeA"},
		fixedPlan:   &testPlan{name: "a", resultType: testIntersectorRowType},
	}
	candidateB := &dataAccessTestCandidate{
		name:        "idxB",
		recordTypes: []string{"TypeB"},
		fixedPlan:   &testPlan{name: "b", resultType: testIntersectorRowType},
	}

	pmA := &testPartialMatch{candidate: candidateA, matchInfo: &testMatchInfo{}}
	pmB := &testPartialMatch{candidate: candidateB, matchInfo: &testMatchInfo{}}

	accesses := []Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(pmA, 0),
		makeVectoredAccess(pmB, 1),
	}

	result := commonPrimaryKeyValues(accesses, EmptyPlanContext())
	if result != nil {
		t.Fatalf("expected nil for mixed record types, got %v", result)
	}
}

func TestCommonPrimaryKeyValues_MultipleRecordTypes(t *testing.T) {
	t.Parallel()

	// Candidate covers two record types — len(commonTypes) != 1.
	candidate := &dataAccessTestCandidate{
		name:        "multi",
		recordTypes: []string{"TypeA", "TypeB"},
		fixedPlan:   &testPlan{name: "multi", resultType: testIntersectorRowType},
	}
	pm := &testPartialMatch{candidate: candidate, matchInfo: &testMatchInfo{}}
	accesses := []Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(pm, 0),
	}

	result := commonPrimaryKeyValues(accesses, EmptyPlanContext())
	if result != nil {
		t.Fatalf("expected nil when candidate covers multiple record types, got %v", result)
	}
}

func TestCommonPrimaryKeyValues_NoPKColumns(t *testing.T) {
	t.Parallel()

	// Single record type but PlanContext returns empty PK columns.
	pm := makeDataAccessTestPartialMatch("idx", 1, &testPlan{name: "scan", resultType: testIntersectorRowType})
	accesses := []Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(pm, 0),
	}

	// EmptyPlanContext returns nil for GetPrimaryKeyColumns.
	result := commonPrimaryKeyValues(accesses, EmptyPlanContext())
	if result != nil {
		t.Fatalf("expected nil when PK columns are empty, got %v", result)
	}
}

func TestCommonPrimaryKeyValues_Success(t *testing.T) {
	t.Parallel()

	pm := makeDataAccessTestPartialMatch("idx", 1, &testPlan{name: "scan", resultType: testIntersectorRowType})
	accesses := []Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(pm, 0),
	}

	ctx := newTestPKContext("TestRecord", []string{"id", "version"})
	result := commonPrimaryKeyValues(accesses, ctx)

	if len(result) != 2 {
		t.Fatalf("expected 2 PK values, got %d", len(result))
	}

	// Columns are upper-cased.
	fv0, ok := result[0].(*values.FieldValue)
	if !ok {
		t.Fatalf("expected *values.FieldValue, got %T", result[0])
	}
	if fv0.Field != "ID" {
		t.Fatalf("expected field 'ID', got %q", fv0.Field)
	}

	fv1, ok := result[1].(*values.FieldValue)
	if !ok {
		t.Fatalf("expected *values.FieldValue, got %T", result[1])
	}
	if fv1.Field != "VERSION" {
		t.Fatalf("expected field 'VERSION', got %q", fv1.Field)
	}
}

func TestCommonPrimaryKeyValues_EmptyRecordTypes(t *testing.T) {
	t.Parallel()

	// Candidate with empty record types — returns nil.
	candidate := &dataAccessTestCandidate{
		name:        "noTypes",
		recordTypes: nil,
		fixedPlan:   &testPlan{name: "x", resultType: testIntersectorRowType},
	}
	pm := &testPartialMatch{candidate: candidate, matchInfo: &testMatchInfo{}}
	accesses := []Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(pm, 0),
	}

	result := commonPrimaryKeyValues(accesses, EmptyPlanContext())
	if result != nil {
		t.Fatalf("expected nil for empty record types, got %v", result)
	}
}

// ---------------------------------------------------------------------------
// Tests: slices.Equal (replaces removed stringSliceEqual)
// ---------------------------------------------------------------------------

func TestSlicesEqual(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"both nil", nil, nil, true},
		{"both empty", []string{}, []string{}, true},
		{"nil vs empty", nil, []string{}, true},
		{"equal", []string{"a", "b"}, []string{"a", "b"}, true},
		{"different length", []string{"a"}, []string{"a", "b"}, false},
		{"different content", []string{"a", "b"}, []string{"a", "c"}, false},
		{"order matters", []string{"a", "b"}, []string{"b", "a"}, false},
		{"single equal", []string{"x"}, []string{"x"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := slices.Equal(tc.a, tc.b)
			if got != tc.want {
				t.Fatalf("slices.Equal(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tests: createScanForAccess
// ---------------------------------------------------------------------------

func TestCreateScanForAccess(t *testing.T) {
	t.Parallel()

	expectedPlan := &testPlan{name: "idx_scan", resultType: testIntersectorRowType}
	pm := makeDataAccessTestPartialMatch("idx", 2, expectedPlan)
	access := NewSingleMatchedAccess(
		pm,
		NoCompensation,
		values.UniqueCorrelationIdentifier(),
		false,
		EmptyTranslationMap(),
		nil,
	)

	plan := createScanForAccess(access)
	if plan == nil {
		t.Fatal("expected non-nil plan from createScanForAccess")
	}
	if plan != expectedPlan {
		t.Fatal("createScanForAccess should return the candidate's fixedPlan")
	}
}

func TestCreateScanForAccess_NilPlan(t *testing.T) {
	t.Parallel()

	// Candidate returns nil from ToScanPlan.
	pm := makeDataAccessTestPartialMatch("nilIdx", 1, nil)
	access := NewSingleMatchedAccess(
		pm,
		NoCompensation,
		values.UniqueCorrelationIdentifier(),
		false,
		EmptyTranslationMap(),
		nil,
	)

	plan := createScanForAccess(access)
	if plan != nil {
		t.Fatal("expected nil plan when candidate returns nil")
	}
}

// TestIntersector_DeclinesNonPKMonotoneLeg pins RFC-181 P0.1: the strict
// pk-sorted intersection merge silently DROPS rows when a leg's emission is
// not PK-monotonic — an INEQUALITY-bound index column is a free non-pk
// ordering part at the front (`a > 5` emits (a, pk) order, pk interleaved).
// Such an access must disqualify itself; with fewer than two compatible
// legs the intersection is not viable. A reverse-scan access declines too
// (the executor merge compares forward only).
func TestIntersector_DeclinesNonPKMonotoneLeg(t *testing.T) {
	t.Parallel()

	// idxA: a INEQUALITY-bound (free part on non-pk column A first).
	ineqCmp := predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(5))
	ineqRange := predicates.EmptyComparisonRange().Merge(&ineqCmp).Range
	pidA := values.UniqueCorrelationIdentifier()
	pidAPK := values.UniqueCorrelationIdentifier()
	pmA := &testPartialMatch{
		candidate: &dataAccessTestCandidate{
			name:            "idxA",
			sargableAliases: []values.CorrelationIdentifier{pidA},
			columnNames:     []string{"A"},
			recordTypes:     []string{"TestRecord"},
			fixedPlan:       &testPlan{name: "scanA", resultType: testIntersectorRowType},
		},
		matchInfo: &testMatchInfo{
			orderingParts: []*MatchedOrderingPart{
				NewMatchedOrderingPart(pidA, &values.FieldValue{Field: "A", Typ: values.UnknownType}, ineqRange, MatchedSortOrderAscending),
				NewMatchedOrderingPart(pidAPK, &values.FieldValue{Field: "ID", Typ: values.UnknownType}, nil, MatchedSortOrderAscending),
			},
			paramBindings: map[values.CorrelationIdentifier]*predicates.ComparisonRange{pidA: ineqRange},
		},
	}
	// idxB: equality-bound → pk-monotone (valid leg).
	pmB := makeDataAccessTestPartialMatch("idxB", 1, &testPlan{name: "scanB", resultType: testIntersectorRowType})

	ctx := newTestPKContext("TestRecord", []string{"id"})
	intersector := WithPrimaryKeyIntersector(ctx)

	result := intersector([]Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(pmA, 0),
		makeVectoredAccess(pmB, 1),
	}, nil)
	if result.IsViable() {
		t.Fatal("an inequality-bound (non-PK-monotone) leg must disqualify itself — the pk-sorted merge over it silently drops intersection rows")
	}

	// Two equality-bound legs stay viable (the gate must not over-decline).
	pmC := makeDataAccessTestPartialMatch("idxC", 1, &testPlan{name: "scanC", resultType: testIntersectorRowType})
	result = intersector([]Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(pmB, 0),
		makeVectoredAccess(pmC, 1),
	}, nil)
	if !result.IsViable() {
		t.Fatal("two equality-bound pk-monotone legs must remain viable")
	}
}

// TestIntersector_LowercasePKStillViable pins the case-fold in the gate:
// commonPrimaryKeyValues upper-cases pk columns while candidate pk-suffix
// parts carry FieldNames() verbatim — un-normalized comparison made a
// lowercase pk name miss and silently over-decline every intersection.
func TestIntersector_LowercasePKStillViable(t *testing.T) {
	t.Parallel()

	mk := func(name string) *testPartialMatch {
		eqCmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))
		eqRange := predicates.EmptyComparisonRange().Merge(&eqCmp).Range
		pid := values.UniqueCorrelationIdentifier()
		pkPid := values.UniqueCorrelationIdentifier()
		return &testPartialMatch{
			candidate: &dataAccessTestCandidate{
				name:            name,
				sargableAliases: []values.CorrelationIdentifier{pid},
				columnNames:     []string{name + "_col"},
				recordTypes:     []string{"TestRecord"},
				fixedPlan:       &testPlan{name: name + "_scan", resultType: testIntersectorRowType},
			},
			matchInfo: &testMatchInfo{
				orderingParts: []*MatchedOrderingPart{
					NewMatchedOrderingPart(pid, &values.FieldValue{Field: name + "_col", Typ: values.UnknownType}, eqRange, MatchedSortOrderAscending),
					// The pk-suffix part carries the LOWERCASE field name
					// verbatim, as real candidates do (FieldNames()).
					NewMatchedOrderingPart(pkPid, &values.FieldValue{Field: "id", Typ: values.UnknownType}, nil, MatchedSortOrderAscending),
				},
				paramBindings: map[values.CorrelationIdentifier]*predicates.ComparisonRange{pid: eqRange},
			},
		}
	}

	ctx := newTestPKContext("TestRecord", []string{"id"})
	intersector := WithPrimaryKeyIntersector(ctx)
	result := intersector([]Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(mk("idxL"), 0),
		makeVectoredAccess(mk("idxM"), 1),
	}, nil)
	if !result.IsViable() {
		t.Fatal("a lowercase pk-suffix field name must not over-decline the intersection (case-fold the comparison)")
	}
}

// TestPushCrossCandidateIntersection_StillFires is the over-decline
// sentinel: driven at the PRODUCTION entry (pushCrossCandidateIntersection)
// with two equality-bound partial matches seeded, an intersection final
// must appear — the P0.1 gate declines non-PK-monotone legs, never
// everything. (No e2e drives this path from SQL today — the cost model
// dodges it and no yamsql/planner test produces a cross-candidate
// intersection; that reachability gap is filed in RFC-181 as the P0.1
// follow-up. This sentinel is the closest production seam.)
func TestPushCrossCandidateIntersection_StillFires(t *testing.T) {
	t.Parallel()

	pmA := makeDataAccessTestPartialMatch("idxA", 1, &testPlan{name: "scanA", resultType: testIntersectorRowType})
	pmB := makeDataAccessTestPartialMatch("idxB", 1, &testPlan{name: "scanB", resultType: testIntersectorRowType})

	scan := expressions.NewFullUnorderedScanExpression([]string{"TestRecord"}, values.UnknownType)
	ref := expressions.InitialOf(scan)
	AddPartialMatchForCandidate(ref, pmA.GetMatchCandidate(), pmA)
	AddPartialMatchForCandidate(ref, pmB.GetMatchCandidate(), pmB)

	ctx := newTestPKContext("TestRecord", []string{"id"})
	p := NewPlanner(nil, ctx)
	p.pushCrossCandidateIntersection(ref,
		[]MatchCandidate{pmA.GetMatchCandidate(), pmB.GetMatchCandidate()},
		[]*RequestedOrdering{PreserveOrdering()})

	found := false
	for _, m := range ref.FinalMembers() {
		if _, ok := m.(*physicalIntersectionWrapper); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("pushCrossCandidateIntersection produced no intersection final for two equality-bound matches — the ordering gate over-declined")
	}
}

// TestIntersector_BakesComparisonKeys pins that the pk comparison keys
// on the produced intersection plan carry plan-time-baked ordinals: the
// merge evaluates them against POSITIONAL rows, which have no name
// fallback — an unbaked key was an OrdinalResolutionError on EVERY
// AND-over-two-indexes query, so `WHERE a = x AND b = y` errored
// instead of returning rows.
func TestIntersector_BakesComparisonKeys(t *testing.T) {
	t.Parallel()

	pmA := makeDataAccessTestPartialMatch("idxA", 1, &testPlan{name: "scanA", resultType: testIntersectorRowType})
	pmB := makeDataAccessTestPartialMatch("idxB", 1, &testPlan{name: "scanB", resultType: testIntersectorRowType})
	ctx := newTestPKContext("TestRecord", []string{"id"})

	result := WithPrimaryKeyIntersector(ctx)([]Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(pmA, 0),
		makeVectoredAccess(pmB, 1),
	}, nil)
	if !result.IsViable() {
		t.Fatal("expected viable intersection")
	}
	for _, e := range result.GetExpressions() {
		iw, ok := e.(*physicalIntersectionWrapper)
		if !ok {
			t.Fatalf("expected intersection wrapper, got %T", e)
		}
		for _, k := range iw.plan.GetComparisonKeyValues() {
			fv, isFV := k.(*values.FieldValue)
			if !isFV || fv.Resolved == nil {
				t.Fatalf("comparison key %v is UNBAKED — the positional merge row cannot resolve it", k)
			}
			if fv.Resolved.Root().Ordinal != 0 {
				t.Fatalf("ID must bake to ordinal 0 of the descriptor layout, got %d", fv.Resolved.Root().Ordinal)
			}
		}
	}
}

// TestIntersector_DeclinesLayoutlessLegs pins the plan-time decline:
// a candidate whose scans flow no row layout cannot bind the comparison
// keys, so the intersection is not planned at all — never yielded with
// keys that would fail loud at the merge.
func TestIntersector_DeclinesLayoutlessLegs(t *testing.T) {
	t.Parallel()

	pmA := makeDataAccessTestPartialMatch("idxA", 1, &testPlan{name: "scanA"})
	pmB := makeDataAccessTestPartialMatch("idxB", 1, &testPlan{name: "scanB"})
	ctx := newTestPKContext("TestRecord", []string{"id"})

	result := WithPrimaryKeyIntersector(ctx)([]Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(pmA, 0),
		makeVectoredAccess(pmB, 1),
	}, nil)
	if result.IsViable() {
		t.Fatal("layout-less legs must DECLINE at plan time, not yield unbaked comparison keys")
	}
}

// TestIntersector_BakesCompositePKKeys pins the 2-key composite-pk
// merge: BOTH comparison keys bake, to the descriptor ordinals of their
// columns (ID→0, VERSION→1 in the fixture layout).
func TestIntersector_BakesCompositePKKeys(t *testing.T) {
	t.Parallel()

	pmA := makeDataAccessTestPartialMatchWithPK("idxA", 1, &testPlan{name: "scanA", resultType: testIntersectorRowType}, "ID", "VERSION")
	pmB := makeDataAccessTestPartialMatchWithPK("idxB", 1, &testPlan{name: "scanB", resultType: testIntersectorRowType}, "ID", "VERSION")
	ctx := newTestPKContext("TestRecord", []string{"id", "version"})

	result := WithPrimaryKeyIntersector(ctx)([]Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(pmA, 0),
		makeVectoredAccess(pmB, 1),
	}, nil)
	if !result.IsViable() {
		t.Fatal("expected viable composite-pk intersection")
	}
	for _, e := range result.GetExpressions() {
		iw, ok := e.(*physicalIntersectionWrapper)
		if !ok {
			t.Fatalf("expected intersection wrapper, got %T", e)
		}
		keys := iw.plan.GetComparisonKeyValues()
		if len(keys) != 2 {
			t.Fatalf("composite pk must carry 2 comparison keys, got %d", len(keys))
		}
		for i, want := range []int{0, 1} {
			fv, isFV := keys[i].(*values.FieldValue)
			if !isFV || fv.Resolved == nil {
				t.Fatalf("key %d unbaked: %v", i, keys[i])
			}
			if got := fv.Resolved.Root().Ordinal; got != want {
				t.Fatalf("key %d: baked ordinal %d, want %d", i, got, want)
			}
		}
	}
}

// TestIntersector_DeclinesMixedLayoutLegs pins the leg-order-agnostic
// layout check: ONE layout-less leg declines the candidate regardless of
// which slot it occupies (the old planI-only check made planning
// depend on leg order).
func TestIntersector_DeclinesMixedLayoutLegs(t *testing.T) {
	t.Parallel()

	for name, accesses := range map[string][2]*testPartialMatch{
		"layoutless_first":  {makeDataAccessTestPartialMatch("idxA", 1, &testPlan{name: "scanA"}), makeDataAccessTestPartialMatch("idxB", 1, &testPlan{name: "scanB", resultType: testIntersectorRowType})},
		"layoutless_second": {makeDataAccessTestPartialMatch("idxA", 1, &testPlan{name: "scanA", resultType: testIntersectorRowType}), makeDataAccessTestPartialMatch("idxB", 1, &testPlan{name: "scanB"})},
	} {
		accesses := accesses
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := newTestPKContext("TestRecord", []string{"id"})
			result := WithPrimaryKeyIntersector(ctx)([]Vectored[*SingleMatchedAccess]{
				makeVectoredAccess(accesses[0], 0),
				makeVectoredAccess(accesses[1], 1),
			}, nil)
			if result.IsViable() {
				t.Fatal("a layout-less leg must decline the candidate in EITHER slot")
			}
		})
	}
}

// TestIntersector_ThreeWay_DuplicateCandidateNameSkipped pins the NAME-based
// same-index guard on the TRIPLE loop: epoch
// re-matching can seed TWO candidate OBJECTS for one index, and an object-
// identity check under-detects — an A/A/B triple kept the redundant
// same-index arm the guard exists to eliminate. With candidates
// {A, A', B} (A and A' distinct objects, same CandidateName) only the ONE
// A/B pair may form: no triple, no A/A pair, and the A'/B pair dedupes
// against A/B only at the memo (both are yielded here — the pair loop
// guards identity of the INDEX, not of the access), so exactly the pairs
// with distinct names survive.
func TestIntersector_ThreeWay_DuplicateCandidateNameSkipped(t *testing.T) {
	t.Parallel()

	pmA := makeDataAccessTestPartialMatch("idxA", 1, &testPlan{name: "scanA", resultType: testIntersectorRowType})
	pmA2 := makeDataAccessTestPartialMatch("idxA", 1, &testPlan{name: "scanA2", resultType: testIntersectorRowType})
	pmB := makeDataAccessTestPartialMatch("idxB", 1, &testPlan{name: "scanB", resultType: testIntersectorRowType})

	ctx := newTestPKContext("TestRecord", []string{"id"})
	intersector := WithPrimaryKeyIntersector(ctx)

	accesses := []Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(pmA, 0),
		makeVectoredAccess(pmA2, 1),
		makeVectoredAccess(pmB, 2),
	}

	result := intersector(accesses, nil)
	if !result.IsViable() {
		t.Fatal("expected the distinct-name pairs to remain viable")
	}
	for _, e := range result.GetExpressions() {
		wrapper, ok := e.(*physicalIntersectionWrapper)
		if !ok {
			t.Fatalf("expected *physicalIntersectionWrapper, got %T", e)
		}
		if n := len(wrapper.GetPlan().GetChildren()); n != 2 {
			t.Fatalf("a triple containing a duplicate index name must not form; got a %d-way intersection", n)
		}
	}
	// A/B and A'/B — the A/A' pair is suppressed by name.
	if n := len(result.GetExpressions()); n != 2 {
		t.Fatalf("expected exactly the 2 distinct-name pairs, got %d", n)
	}
}
