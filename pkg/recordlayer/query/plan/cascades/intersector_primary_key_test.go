package cascades

import (
	"slices"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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

	planA := &testPlan{name: "scanA"}
	planB := &testPlan{name: "scanB"}
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

	pm := makeDataAccessTestPartialMatch("only", 3, &testPlan{name: "scan"})
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

	plan := &testPlan{name: "scan"}
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

	pmA := makeDataAccessTestPartialMatch("idxA", 1, &testPlan{name: "scanA"})
	pmB := makeDataAccessTestPartialMatch("idxB", 1, &testPlan{name: "scanB"})
	pmC := makeDataAccessTestPartialMatch("idxC", 1, &testPlan{name: "scanC"})

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
		fixedPlan:   &testPlan{name: "a"},
	}
	candidateB := &dataAccessTestCandidate{
		name:        "idxB",
		recordTypes: []string{"TypeB"},
		fixedPlan:   &testPlan{name: "b"},
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
		fixedPlan:   &testPlan{name: "multi"},
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
	pm := makeDataAccessTestPartialMatch("idx", 1, &testPlan{name: "scan"})
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

	pm := makeDataAccessTestPartialMatch("idx", 1, &testPlan{name: "scan"})
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
		fixedPlan:   &testPlan{name: "x"},
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

	expectedPlan := &testPlan{name: "idx_scan"}
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
