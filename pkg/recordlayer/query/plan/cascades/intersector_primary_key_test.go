package cascades

import (
	"slices"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ---------------------------------------------------------------------------
// Test double: PlanContext that returns fixed PK columns
// ---------------------------------------------------------------------------

type testPlanContextForIntersection struct {
	emptyPlanContext
	pkColumns map[string][]string
}

type structuralPKTestCandidate struct {
	*dataAccessTestCandidate
	commonPrimaryKey []values.Value
}

func (c *structuralPKTestCandidate) GetCommonPrimaryKeyValues() []values.Value {
	return append([]values.Value(nil), c.commonPrimaryKey...)
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

// distinctUnmatchedCompensation builds a POSSIBLE compensation whose
// unmatched-aggregate set is unique to the given tag.
//
// It exists so a test can switch the partition-redundancy proof OFF for the
// partitions it builds: that proof compares a partition's unmatched-id set
// against its subpartitions', and gives up (fails open) when they differ. Legs
// that all carry the same set — the default, since NoCompensation's set is empty
// for every leg — make the sieve reject pairs on its own, which silently masks
// whichever guard the test is actually about.
//
// All legs share ONE base quantifier so the compensations still INTERSECT to a
// possible one; a leg over a different base makes the fold impossible and the
// partition yields no expression at all.
func distinctUnmatchedCompensation(
	base expressions.Quantifier,
	tag int64,
) Compensation {
	unmatched := NewCorrValueBiMap()
	unmatched.Put(values.UniqueCorrelationIdentifier(), values.LiteralValue(tag))
	return NewForMatchCompensation(
		false,
		NoCompensation,
		StubPredicateCompensationMap(1),
		[]expressions.Quantifier{base},
		nil,
		map[values.CorrelationIdentifier]struct{}{base.GetAlias(): {}},
		NoResultCompensation(),
		NewGroupByMappings(NewValueBiMap(), NewValueBiMap(), unmatched),
	)
}

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

	planA := mustDataAccessTestPlan(t, "scanA")
	planB := mustDataAccessTestPlan(t, "scanB")
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

	// The expression should be a bare 2-leg RecordQueryIntersectionPlan
	// (RFC-184 W2 — no physical wrapper).
	plan, ok := exprs[0].(*plans.RecordQueryIntersectionPlan)
	if !ok {
		t.Fatalf("expected *plans.RecordQueryIntersectionPlan, got %T", exprs[0])
	}
	if len(plan.GetChildren()) != 2 {
		t.Fatalf("expected 2 children in intersection plan, got %d", len(plan.GetChildren()))
	}
}

func TestIntersector_SingleAccess_NoIntersection(t *testing.T) {
	t.Parallel()

	pm := makeDataAccessTestPartialMatch("only", 3, mustDataAccessTestPlan(t, "scan"))
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

	plan := mustDataAccessTestPlan(t, "scan")
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

	pmA := makeDataAccessTestPartialMatch("idxA", 1, mustDataAccessTestPlan(t, "scanA"))
	pmB := makeDataAccessTestPartialMatch("idxB", 1, mustDataAccessTestPlan(t, "scanB"))
	pmC := makeDataAccessTestPartialMatch("idxC", 1, mustDataAccessTestPlan(t, "scanC"))

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

	// Each leg contributes a different fixed predicate, so the larger
	// partition is useful and evicts its immediate pair subpartitions.
	exprs := result.GetExpressions()
	if len(exprs) != 1 {
		t.Fatalf("expected only the useful three-way intersection, got %d expressions", len(exprs))
	}

	byArity := map[int]int{}
	for _, expr := range exprs {
		plan, ok := expr.(*plans.RecordQueryIntersectionPlan)
		if !ok {
			t.Fatalf("expected *plans.RecordQueryIntersectionPlan, got %T", expr)
		}
		byArity[len(plan.GetChildren())]++
	}
	if byArity[2] != 0 || byArity[3] != 1 {
		t.Fatalf("intersection arities = %v, want map[3:1]", byArity)
	}
}

func TestIntersector_ImpossibleLargerCompensationKeepsUsefulPair(
	t *testing.T,
) {
	t.Parallel()

	baseRef := expressions.InitialOf(mustFullUnorderedScan(
		t,
		[]string{"TestRecord"},
		dataAccessTestRow,
	))
	leftBaseQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("left_scope"),
		baseRef,
	)
	rightBaseQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("right_scope"),
		baseRef,
	)
	sharedPredicate := predicates.NewConstantPredicate(predicates.TriTrue)
	makeCompensation := func(
		baseQ expressions.Quantifier,
	) Compensation {
		return NewForMatchCompensation(
			false,
			NoCompensation,
			NewPredicateCompensationMap(
				[]predicates.QueryPredicate{sharedPredicate},
				[]PredicateCompensationFunc{
					NoPredicateCompensationNeeded(),
				},
			),
			[]expressions.Quantifier{baseQ},
			nil,
			map[values.CorrelationIdentifier]struct{}{
				baseQ.GetAlias(): {},
			},
			NoResultCompensation(),
			EmptyGroupByMappings(),
		)
	}

	makeAccess := func(
		name string,
		compensation Compensation,
		position int,
	) Vectored[*SingleMatchedAccess] {
		pm := makeDataAccessTestPartialMatch(
			name,
			1,
			mustDataAccessTestPlan(t, name),
		)
		return NewVectored(
			NewSingleMatchedAccess(
				pm,
				compensation,
				values.UniqueCorrelationIdentifier(),
				false,
				EmptyTranslationMap(),
				nil,
			),
			position,
		)
	}

	result := WithPrimaryKeyIntersector(
		newTestPKContext("TestRecord", []string{"id"}),
	)([]Vectored[*SingleMatchedAccess]{
		makeAccess("scan_a", makeCompensation(leftBaseQ), 0),
		makeAccess("scan_b", makeCompensation(leftBaseQ), 1),
		makeAccess("scan_c", makeCompensation(rightBaseQ), 2),
	}, nil)

	if !result.IsViable() || len(result.GetExpressions()) != 1 {
		t.Fatalf(
			"zero-expression larger replacement left %d expressions, want useful A/B pair",
			len(result.GetExpressions()),
		)
	}
	intersection, ok := result.GetExpressions()[0].(*plans.RecordQueryIntersectionPlan)
	if !ok {
		t.Fatalf(
			"survivor = %T, want *RecordQueryIntersectionPlan",
			result.GetExpressions()[0],
		)
	}
	if len(intersection.GetChildren()) != 2 {
		t.Fatalf(
			"survivor arity = %d, want 2",
			len(intersection.GetChildren()),
		)
	}
	explain := intersection.Explain()
	if !strings.Contains(explain, "scan_a") ||
		!strings.Contains(explain, "scan_b") ||
		strings.Contains(explain, "scan_c") {
		t.Fatalf("survivor = %s, want the viable A/B pair", explain)
	}
}

func TestIntersector_FourWay(t *testing.T) {
	t.Parallel()

	accesses := make([]Vectored[*SingleMatchedAccess], 0, 4)
	for i, name := range []string{"idxA", "idxB", "idxC", "idxD"} {
		pm := makeDataAccessTestPartialMatch(
			name,
			1,
			mustDataAccessTestPlan(t, name+"_scan"),
		)
		accesses = append(accesses, makeVectoredAccess(pm, i))
	}

	ctx := newTestPKContext("TestRecord", []string{"id"})
	result := WithPrimaryKeyIntersector(ctx)(accesses, nil)
	if !result.IsViable() {
		t.Fatal("expected viable intersections from four different candidates")
	}

	byArity := map[int]int{}
	for _, expr := range result.GetExpressions() {
		plan, ok := expr.(*plans.RecordQueryIntersectionPlan)
		if !ok {
			t.Fatalf("expected *plans.RecordQueryIntersectionPlan, got %T", expr)
		}
		byArity[len(plan.GetChildren())]++
	}
	if got := byArity[4]; got != 1 {
		t.Fatalf("four-way intersections = %d, want C(4,4)=1", got)
	}
	if byArity[2] != 0 || byArity[3] != 0 {
		t.Fatalf("intersection arities = %v, want only map[4:1]", byArity)
	}
	if got := len(result.GetExpressions()); got != 1 {
		t.Fatalf("bounded intersection alternatives = %d, want only the maximal useful plan", got)
	}
}

func TestIntersector_JavaRedundancyExampleKeepsUsefulPair(t *testing.T) {
	t.Parallel()

	equality := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))
	equalityRange := predicates.EmptyComparisonRange().Merge(&equality).Range
	makeMatch := func(name string, fixedColumns ...string) *testPartialMatch {
		aliases := make([]values.CorrelationIdentifier, len(fixedColumns))
		parts := make([]*MatchedOrderingPart, 0, len(fixedColumns)+1)
		bindings := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
		for i, column := range fixedColumns {
			alias := values.UniqueCorrelationIdentifier()
			aliases[i] = alias
			bindings[alias] = equalityRange
			parts = append(parts, NewMatchedOrderingPart(
				alias,
				dataAccessTestKey(column),
				equalityRange,
				MatchedSortOrderAscending,
			))
		}
		parts = append(parts, NewMatchedOrderingPart(
			values.UniqueCorrelationIdentifier(),
			dataAccessTestKey("ID"),
			nil,
			MatchedSortOrderAscending,
		))
		return &testPartialMatch{
			candidate: &dataAccessTestCandidate{
				name:              name,
				sargableAliases:   aliases,
				columnNames:       fixedColumns,
				keyComponentTypes: syntheticIndexKeyTypes(len(fixedColumns)),
				recordTypes:       []string{"TestRecord"},
				fixedPlan:         mustDataAccessTestPlan(t, name),
			},
			matchInfo: &testMatchInfo{
				orderingParts: parts,
				paramBindings: bindings,
			},
		}
	}

	result := WithPrimaryKeyIntersector(
		newTestPKContext("TestRecord", []string{"id"}),
	)([]Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(makeMatch("i1", "A"), 0),
		makeVectoredAccess(makeMatch("i2", "B"), 1),
		makeVectoredAccess(makeMatch("i3", "A", "B"), 2),
	}, nil)
	if !result.IsViable() || len(result.GetExpressions()) != 1 {
		t.Fatalf("Java redundancy example produced %d expressions, want only I1∩I2",
			len(result.GetExpressions()))
	}
	intersection, ok := result.GetExpressions()[0].(*plans.RecordQueryIntersectionPlan)
	if !ok {
		t.Fatalf("survivor = %T, want *RecordQueryIntersectionPlan",
			result.GetExpressions()[0])
	}
	if len(intersection.GetChildren()) != 2 {
		t.Fatalf("survivor has %d children, want one two-leg intersection",
			len(intersection.GetChildren()))
	}
	explain := intersection.Explain()
	if !strings.Contains(explain, "TestPlan(i1)") ||
		!strings.Contains(explain, "TestPlan(i2)") ||
		strings.Contains(explain, "TestPlan(i3)") {
		t.Fatalf("redundancy survivor = %s, want I1∩I2", explain)
	}
}

func TestIntersector_MaxOneSingletonSuppressesIntersection(t *testing.T) {
	t.Parallel()

	equality := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))
	equalityRange := predicates.EmptyComparisonRange().Merge(&equality).Range
	makeMatch := func(name, column string, unique bool) *testPartialMatch {
		alias := values.UniqueCorrelationIdentifier()
		var scan plans.RecordQueryPlan = mustDataAccessTestPlan(t, name)
		if unique {
			indexPlan, err := plans.NewRecordQueryIndexPlan(
				name,
				[]*predicates.ComparisonRange{equalityRange},
				[]string{"TestRecord"},
				dataAccessTestRow,
				false,
			)
			scan = mustConstruct(t, indexPlan, err)
		}
		return &testPartialMatch{
			candidate: &dataAccessTestCandidate{
				name:            name,
				sargableAliases: []values.CorrelationIdentifier{alias},
				columnNames:     []string{column},
				recordTypes:     []string{"TestRecord"},
				fixedPlan:       scan,
				unique:          unique,
			},
			matchInfo: &testMatchInfo{
				orderingParts: []*MatchedOrderingPart{
					NewMatchedOrderingPart(
						alias,
						dataAccessTestKey(column),
						equalityRange,
						MatchedSortOrderAscending,
					),
					NewMatchedOrderingPart(
						values.UniqueCorrelationIdentifier(),
						dataAccessTestKey("ID"),
						nil,
						MatchedSortOrderAscending,
					),
				},
				paramBindings: map[values.CorrelationIdentifier]*predicates.ComparisonRange{
					alias: equalityRange,
				},
			},
		}
	}

	result := WithPrimaryKeyIntersector(
		newTestPKContext("TestRecord", []string{"id"}),
	)([]Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(makeMatch("unique_a", "A", true), 0),
		makeVectoredAccess(makeMatch("idx_b", "B", false), 1),
	}, nil)
	if result.IsViable() || len(result.GetExpressions()) != 0 {
		t.Fatal("a singleton access with proven max cardinality one makes every containing intersection redundant")
	}
}

func TestPartitionRedundancy_UnmatchedMetadata(t *testing.T) {
	t.Parallel()

	makeCompensation := func(ids ...values.CorrelationIdentifier) Compensation {
		unmatched := NewCorrValueBiMap()
		for i, id := range ids {
			unmatched.Put(id, values.LiteralValue(int64(i+1)))
		}
		groupByMappings := NewGroupByMappings(
			NewValueBiMap(),
			NewValueBiMap(),
			unmatched,
		)
		baseAlias := values.UniqueCorrelationIdentifier()
		baseQ := expressions.NamedForEachQuantifier(
			baseAlias,
			expressions.InitialOf(mustFullUnorderedScan(
				t,
				[]string{"TestRecord"},
				dataAccessTestRow,
			)),
		)
		return NewForMatchCompensation(
			false,
			NoCompensation,
			StubPredicateCompensationMap(1),
			[]expressions.Quantifier{baseQ},
			nil,
			map[values.CorrelationIdentifier]struct{}{baseAlias: {}},
			NoResultCompensation(),
			groupByMappings,
		)
	}
	fixedOrdering := func(fixed ...values.Value) *properties.RichOrdering {
		bindings := make(map[values.Value][]properties.OrderingBinding, len(fixed))
		for _, value := range fixed {
			bindings[value] = []properties.OrderingBinding{
				properties.FixedBinding(nil),
			}
		}
		return properties.NewRichOrdering(bindings, fixed, properties.NotDistinct())
	}

	x := values.UniqueCorrelationIdentifier()
	y := values.UniqueCorrelationIdentifier()
	redundancyKey := func(column string) values.Value {
		root, err := values.NewQuantifiedObjectValue(
			values.NamedCorrelationIdentifier("redundancy_row"),
			dataAccessTestRow,
		)
		exactRoot := mustConstruct(t, root, err)
		field, err := values.ResolveFieldOrdinals(
			exactRoot,
			[]int{dataAccessTestOrdinal(column)},
		)
		return mustConstruct(t, field, err)
	}
	aProvided := redundancyKey("A")
	bProvided := redundancyKey("B")
	// Separately allocated required Values pin IDENTITY, not pointer, equality
	// in the proof: each side is baked independently against the fixture's row
	// layout, so the proof succeeds only if the comparator reads the stated
	// column identity off both operands.
	required := []values.Value{
		redundancyKey("A"),
		redundancyKey("B"),
	}
	partition := []Vectored[*SingleMatchedAccess]{
		{Position: 0},
		{Position: 1},
	}
	dummy := mustFullUnorderedScan(
		t,
		[]string{"TestRecord"},
		dataAccessTestRow,
	)
	makeInfos := func(
		leftCompensation, rightCompensation Compensation,
	) map[intersectionInfoKey]*IntersectionInfo {
		left := []Vectored[*SingleMatchedAccess]{{Position: 0}}
		right := []Vectored[*SingleMatchedAccess]{{Position: 1}}
		return map[intersectionInfoKey]*IntersectionInfo{
			intersectionInfoKeyForPartition(left): IntersectionInfoOfSingleAccess(
				fixedOrdering(aProvided, bProvided),
				leftCompensation,
				dummy,
				CardinalityUnknown,
			),
			intersectionInfoKeyForPartition(right): IntersectionInfoOfSingleAccess(
				properties.EmptyOrdering(),
				rightCompensation,
				dummy,
				CardinalityUnknown,
			),
		}
	}

	if !isPrimaryKeyPartitionRedundant(
		partition,
		required,
		makeInfos(makeCompensation(x), makeCompensation(x)),
	) {
		t.Fatal("equal unmatched-ID sets and a fixed-value superset should prove redundancy")
	}
	if isPrimaryKeyPartitionRedundant(
		partition,
		required,
		makeInfos(makeCompensation(x, y), makeCompensation(x)),
	) {
		t.Fatal("different unmatched-ID sets must fail the redundancy proof open")
	}
	if isPrimaryKeyPartitionRedundant(
		partition,
		required,
		makeInfos(ImpossibleCompensation, makeCompensation(x)),
	) {
		t.Fatal("unknown unmatched metadata must fail the redundancy proof open")
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
		fixedPlan:   mustDataAccessTestPlan(t, "a"),
	}
	candidateB := &dataAccessTestCandidate{
		name:        "idxB",
		recordTypes: []string{"TypeB"},
		fixedPlan:   mustDataAccessTestPlan(t, "b"),
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
		fixedPlan:   mustDataAccessTestPlan(t, "multi"),
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
	pm := makeDataAccessTestPartialMatch("idx", 1, mustDataAccessTestPlan(t, "scan"))
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

	pm := makeDataAccessTestPartialMatch("idx", 1, mustDataAccessTestPlan(t, "scan"))
	accesses := []Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(pm, 0),
	}

	ctx := newTestPKContext("TestRecord", []string{"id", "version"})
	result := commonPrimaryKeyValues(accesses, ctx)

	if len(result) != 2 {
		t.Fatalf("expected 2 PK values, got %d", len(result))
	}

	// Columns are upper-cased.
	fv0, ok := values.AsFieldValue(result[0])
	if !ok {
		t.Fatalf("expected exact values.FieldValue, got %T", result[0])
	}
	if fv0.DisplayName() != "ID" {
		t.Fatalf("expected field 'ID', got %q", fv0.DisplayName())
	}

	fv1, ok := values.AsFieldValue(result[1])
	if !ok {
		t.Fatalf("expected exact values.FieldValue, got %T", result[1])
	}
	if fv1.DisplayName() != "VERSION" {
		t.Fatalf("expected field 'VERSION', got %q", fv1.DisplayName())
	}
}

func TestCommonPrimaryKeyValues_EmptyRecordTypes(t *testing.T) {
	t.Parallel()

	// Candidate with empty record types — returns nil.
	candidate := &dataAccessTestCandidate{
		name:        "noTypes",
		recordTypes: nil,
		fixedPlan:   mustDataAccessTestPlan(t, "x"),
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

func TestCommonPrimaryKeyValuesForPartition_StructuralCandidateContract(
	t *testing.T,
) {
	t.Parallel()

	makeAccess := func(
		name string,
		position int,
		commonPrimaryKey []values.Value,
		exposeStructural bool,
	) Vectored[*SingleMatchedAccess] {
		pm := makeDataAccessTestPartialMatch(
			name,
			1,
			mustDataAccessTestPlan(t, name),
		)
		if exposeStructural {
			pm.candidate = &structuralPKTestCandidate{
				dataAccessTestCandidate: pm.candidate.(*dataAccessTestCandidate),
				commonPrimaryKey:        commonPrimaryKey,
			}
		}
		return makeVectoredAccess(pm, position)
	}
	structuralPK := func(lastField string) []values.Value {
		root, err := values.NewQuantifiedObjectValue(
			values.NamedCorrelationIdentifier("structural_pk"),
			dataAccessTestRow,
		)
		exactRoot := mustConstruct(t, root, err)
		field, err := values.ResolveFieldOrdinals(
			exactRoot,
			[]int{dataAccessTestOrdinal(lastField)},
		)
		return []values.Value{
			values.NewRecordTypeValue(nil),
			mustConstruct(t, field, err),
		}
	}

	t.Run("separately_allocated_equal_values", func(t *testing.T) {
		t.Parallel()
		got := commonPrimaryKeyValuesForPartition(
			[]Vectored[*SingleMatchedAccess]{
				makeAccess("idx_a", 0, structuralPK("ID"), true),
				makeAccess("idx_b", 1, structuralPK("ID"), true),
			},
			EmptyPlanContext(),
		)
		if len(got) != 2 {
			t.Fatalf("structural common PK length = %d, want 2", len(got))
		}
		if _, ok := got[0].(*values.RecordTypeValue); !ok {
			t.Fatalf(
				"structural common PK prefix = %T, want *RecordTypeValue",
				got[0],
			)
		}
		field, ok := values.AsFieldValue(got[1])
		if !ok || field.DisplayName() != "ID" || field.Path().Len() != 1 || field.Path().Ordinals()[0] != 0 {
			t.Fatalf("structural common PK suffix = %v, want exact ID#0", got[1])
		}
		root, rooted := values.AsQuantifiedObjectValue(field.ChildValue())
		if !rooted || root.Correlation() != values.CurrentCorrelation() {
			t.Fatalf("structural common PK suffix root = %v, want the partition's owner-current carrier", field.ChildValue())
		}
		if provided := dataAccessTestKey("ID"); !values.SameOrderingColumn(got[1], provided) {
			t.Fatalf("reanchored structural PK %q does not identify the scan ordering column %q; preserving the synthetic metadata root deletes every intersection",
				values.ExplainValue(got[1]), values.ExplainValue(provided))
		}
	})

	t.Run("different_structure_declines", func(t *testing.T) {
		t.Parallel()
		got := commonPrimaryKeyValuesForPartition(
			[]Vectored[*SingleMatchedAccess]{
				makeAccess("idx_a", 0, structuralPK("ID"), true),
				makeAccess("idx_b", 1, structuralPK("VERSION"), true),
			},
			EmptyPlanContext(),
		)
		if got != nil {
			t.Fatalf("different structural PKs = %#v, want nil", got)
		}
	})

	t.Run("mixed_provider_declines", func(t *testing.T) {
		t.Parallel()
		got := commonPrimaryKeyValuesForPartition(
			[]Vectored[*SingleMatchedAccess]{
				makeAccess("idx_a", 0, structuralPK("ID"), true),
				makeAccess("idx_b", 1, nil, false),
			},
			newTestPKContext("TestRecord", []string{"id"}),
		)
		if got != nil {
			t.Fatalf("mixed structural/name-only PKs = %#v, want nil", got)
		}
	})

	t.Run("empty_structural_provider_declines", func(t *testing.T) {
		t.Parallel()
		got := commonPrimaryKeyValuesForPartition(
			[]Vectored[*SingleMatchedAccess]{
				makeAccess("idx_a", 0, nil, true),
				makeAccess("idx_b", 1, nil, true),
			},
			newTestPKContext("TestRecord", []string{"id"}),
		)
		if got != nil {
			t.Fatalf("empty structural PKs = %#v, want nil", got)
		}
	})
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

	expectedPlan := mustDataAccessTestPlan(t, "idx_scan")
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

func TestAdjustedIntersectionOrdering_SignedZeroIsDirectional(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name             string
		literal          int64
		wantFixed        int
		wantOrderingKeys int
	}{
		{name: "signed zero", literal: 0, wantFixed: 0, wantOrderingKeys: 2},
		{name: "nonzero", literal: 7, wantFixed: 1, wantOrderingKeys: 1},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			keyAlias := values.UniqueCorrelationIdentifier()
			pkAlias := values.UniqueCorrelationIdentifier()
			comparison := predicates.NewLiteralComparison(
				predicates.ComparisonEquals,
				tc.literal,
			)
			rangeResult := predicates.EmptyComparisonRange().Merge(&comparison)
			if !rangeResult.Ok {
				t.Fatal("failed to build equality range")
			}

			pm := &testPartialMatch{
				candidate: &dataAccessTestCandidate{
					name:              "idx_a",
					sargableAliases:   []values.CorrelationIdentifier{keyAlias},
					columnNames:       []string{"A"},
					keyComponentTypes: []values.Type{values.NullableDouble},
					recordTypes:       []string{"TestRecord"},
					fixedPlan:         mustDataAccessTestPlan(t, "signed_zero_scan"),
				},
				matchInfo: &testMatchInfo{
					orderingParts: []*MatchedOrderingPart{
						NewMatchedOrderingPart(
							keyAlias,
							dataAccessTestKey("A"),
							rangeResult.Range,
							MatchedSortOrderAscending,
						),
						NewMatchedOrderingPart(
							pkAlias,
							dataAccessTestKey("ID"),
							nil,
							MatchedSortOrderAscending,
						),
					},
					paramBindings: map[values.CorrelationIdentifier]*predicates.ComparisonRange{
						keyAlias: rangeResult.Range,
					},
				},
			}
			access := NewSingleMatchedAccess(
				pm,
				NoCompensation,
				values.UniqueCorrelationIdentifier(),
				false,
				EmptyTranslationMap(),
				nil,
			)
			ordering := adjustedIntersectionOrdering(access, nil)
			if ordering == nil {
				t.Fatal("adjusted ordering unexpectedly absent")
			}
			if got := len(ordering.GetEqualityBoundValues()); got != tc.wantFixed {
				t.Fatalf("fixed bindings = %d, want %d", got, tc.wantFixed)
			}
			if got := len(ordering.GetOrderingKeys()); got != tc.wantOrderingKeys {
				t.Fatalf("directional ordering keys = %d, want %d", got, tc.wantOrderingKeys)
			}
		})
	}
}

// TestIntersector_DeclinesNonPKMonotoneLeg pins RFC-181 P0.1: the strict
// pk-sorted intersection merge silently DROPS rows when a leg's emission is
// not PK-monotonic — an INEQUALITY-bound index column is a free non-pk
// ordering part at the front (`a > 5` emits (a, pk) order, pk interleaved).
// Such an access must disqualify itself; with fewer than two compatible
// legs the intersection is not viable.
func TestIntersector_DeclinesNonPKMonotoneLeg(t *testing.T) {
	t.Parallel()

	// idxA: a INEQUALITY-bound (free part on non-pk column A first).
	ineqCmp := predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(5))
	ineqRange := predicates.EmptyComparisonRange().Merge(&ineqCmp).Range
	pidA := values.UniqueCorrelationIdentifier()
	pidAPK := values.UniqueCorrelationIdentifier()
	pmA := &testPartialMatch{
		candidate: &dataAccessTestCandidate{
			name:              "idxA",
			sargableAliases:   []values.CorrelationIdentifier{pidA},
			columnNames:       []string{"A"},
			keyComponentTypes: []values.Type{values.NullableLong},
			recordTypes:       []string{"TestRecord"},
			fixedPlan:         mustDataAccessTestPlan(t, "scanA"),
		},
		matchInfo: &testMatchInfo{
			orderingParts: []*MatchedOrderingPart{
				NewMatchedOrderingPart(pidA, dataAccessTestKey("A"), ineqRange, MatchedSortOrderAscending),
				NewMatchedOrderingPart(pidAPK, dataAccessTestKey("ID"), nil, MatchedSortOrderAscending),
			},
			paramBindings: map[values.CorrelationIdentifier]*predicates.ComparisonRange{pidA: ineqRange},
		},
	}
	// idxB: equality-bound → pk-monotone (valid leg).
	pmB := makeDataAccessTestPartialMatch("idxB", 1, mustDataAccessTestPlan(t, "scanB"))

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
	pmC := makeDataAccessTestPartialMatch("idxC", 1, mustDataAccessTestPlan(t, "scanC"))
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
				name:              name,
				sargableAliases:   []values.CorrelationIdentifier{pid},
				columnNames:       []string{name + "_col"},
				keyComponentTypes: []values.Type{values.NullableLong},
				recordTypes:       []string{"TestRecord"},
				fixedPlan:         mustDataAccessTestPlan(t, name+"_scan"),
			},
			matchInfo: &testMatchInfo{
				orderingParts: []*MatchedOrderingPart{
					NewMatchedOrderingPart(pid, dataAccessTestKey(dataAccessTestColumn(name, 0)), eqRange, MatchedSortOrderAscending),
					// The pk-suffix part carries the LOWERCASE field name
					// verbatim, as real candidates do (FieldNames()).
					NewMatchedOrderingPart(pkPid, dataAccessTestKey("id"), nil, MatchedSortOrderAscending),
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

func TestIntersector_DescendingCommonSecondaryOrdering(t *testing.T) {
	t.Parallel()

	equality := func(literal int64) *predicates.ComparisonRange {
		comparison := predicates.NewLiteralComparison(predicates.ComparisonEquals, literal)
		return predicates.EmptyComparisonRange().Merge(&comparison).Range
	}
	makeAccess := func(
		name, fixedColumn string,
		literal int64,
		position int,
	) Vectored[*SingleMatchedAccess] {
		fixedAlias := values.UniqueCorrelationIdentifier()
		fixedRange := equality(literal)
		pm := &testPartialMatch{
			candidate: &dataAccessTestCandidate{
				name:              name,
				sargableAliases:   []values.CorrelationIdentifier{fixedAlias},
				columnNames:       []string{fixedColumn, "SORT_KEY"},
				keyComponentTypes: []values.Type{values.NullableLong, values.NullableLong},
				recordTypes:       []string{"TestRecord"},
				fixedPlan:         mustDataAccessTestPlan(t, name),
			},
			matchInfo: &testMatchInfo{
				orderingParts: []*MatchedOrderingPart{
					NewMatchedOrderingPart(
						fixedAlias,
						dataAccessTestKey(fixedColumn),
						fixedRange,
						MatchedSortOrderAscending,
					),
					NewMatchedOrderingPart(
						values.UniqueCorrelationIdentifier(),
						dataAccessTestKey("SORT_KEY"),
						nil,
						MatchedSortOrderAscending,
					),
					NewMatchedOrderingPart(
						values.UniqueCorrelationIdentifier(),
						dataAccessTestKey("ID"),
						nil,
						MatchedSortOrderAscending,
					),
				},
				paramBindings: map[values.CorrelationIdentifier]*predicates.ComparisonRange{
					fixedAlias: fixedRange,
				},
			},
		}
		access := NewSingleMatchedAccess(
			pm,
			NoCompensation,
			values.UniqueCorrelationIdentifier(),
			true,
			EmptyTranslationMap(),
			nil,
		)
		return NewVectored(access, position)
	}

	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{
				Value:     dataAccessTestKey("SORT_KEY"),
				SortOrder: properties.RequestedSortOrderDescending,
			},
			{
				Value:     dataAccessTestKey("ID"),
				SortOrder: properties.RequestedSortOrderDescending,
			},
		},
		properties.DistinctnessNotDistinct,
		false,
	)
	result := WithPrimaryKeyIntersector(
		newTestPKContext("TestRecord", []string{"id"}),
	)([]Vectored[*SingleMatchedAccess]{
		makeAccess("idx_a_sort", "A", 7, 0),
		makeAccess("idx_b_sort", "B", 9, 1),
	}, []*properties.RequestedOrdering{requested})
	if !result.IsViable() || len(result.GetExpressions()) != 1 {
		t.Fatalf("descending common-secondary intersection = %#v, want one viable expression", result)
	}
	intersection, ok := result.GetExpressions()[0].(*plans.RecordQueryIntersectionPlan)
	if !ok {
		t.Fatalf("intersection expression = %T, want *RecordQueryIntersectionPlan", result.GetExpressions()[0])
	}
	if !intersection.IsReverse() {
		t.Fatal("descending common-secondary intersection is not reverse")
	}
	parts := intersection.GetComparisonKeyOrderingParts()
	if len(parts) != 2 ||
		values.ColumnNameValue(parts[0].Value) != "SORT_KEY" ||
		values.ColumnNameValue(parts[1].Value) != "ID" ||
		parts[0].SortOrder != properties.ProvidedSortOrderDescending ||
		parts[1].SortOrder != properties.ProvidedSortOrderDescending {
		t.Fatalf("comparison ordering parts = %#v, want [SORT_KEY DESC, ID DESC]", parts)
	}
}

func TestTranslateIntersectionRequestedOrderingThroughTopMap(t *testing.T) {
	t.Parallel()

	queryTop := values.NamedCorrelationIdentifier("query_top")
	candidateTop := values.NamedCorrelationIdentifier("candidate_top")
	queryRoot, err := values.NewQuantifiedObjectValue(queryTop, dataAccessTestRow)
	exactQueryRoot := mustConstruct(t, queryRoot, err)
	requestedValue, err := values.ResolveFieldOrdinals(
		exactQueryRoot,
		[]int{dataAccessTestOrdinal("SORT_KEY")},
	)
	requestedValue = mustConstruct(t, requestedValue, err)
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     requestedValue,
			SortOrder: properties.RequestedSortOrderDescending,
		}},
		properties.DistinctnessNotDistinct,
		true,
	)

	translated := translateIntersectionRequestedOrdering(
		requested,
		TranslationMapOfAliases(queryTop, candidateTop),
	)
	if translated == requested {
		t.Fatal("non-identity top map should produce a translated ordering")
	}
	if !translated.IsExhaustive() ||
		translated.GetDistinctness() != requested.GetDistinctness() {
		t.Fatal("translation lost requested-ordering metadata")
	}
	translatedField, ok := values.AsFieldValue(translated.GetParts()[0].Value)
	if !ok {
		t.Fatalf("translated value = %T, want exact FieldValue", translated.GetParts()[0].Value)
	}
	translatedRoot, ok := values.AsQuantifiedObjectValue(translatedField.ChildValue())
	if !ok || translatedRoot.Correlation() != candidateTop {
		t.Fatalf("translated root = %#v, want candidate_top", translatedField.ChildValue())
	}
	if translated.GetParts()[0].SortOrder != properties.RequestedSortOrderDescending {
		t.Fatal("translation lost requested direction")
	}
}

func TestIntersector_UsesTranslatedRequestedOrdering(t *testing.T) {
	t.Parallel()

	queryTop := values.NamedCorrelationIdentifier("query_top")
	queryRoot, err := values.NewQuantifiedObjectValue(queryTop, dataAccessTestRow)
	exactQueryRoot := mustConstruct(t, queryRoot, err)
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     exactQueryRoot,
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessNotDistinct,
		false,
	)
	topMapBuilder := NewTranslationMapBuilder()
	topMapBuilder.When(queryTop).Then(
		func(
			_ values.CorrelationIdentifier,
			_ values.LeafValue,
		) values.Value {
			// The translated request must state the SAME column identity the
			// candidates' pk-suffix keys state, because that is the whole
			// point of translating it: a request phrased over the query's top
			// quantifier is rewritten into the row the candidates flow.
			return dataAccessTestKey("ID")
		},
	)
	topMap := topMapBuilder.Build()

	makeAccess := func(
		name string,
		position int,
		translation TranslationMap,
	) Vectored[*SingleMatchedAccess] {
		pm := makeDataAccessTestPartialMatch(
			name,
			1,
			mustDataAccessTestPlan(t, name),
		)
		return NewVectored(
			NewSingleMatchedAccess(
				pm,
				NoCompensation,
				values.UniqueCorrelationIdentifier(),
				false,
				translation,
				nil,
			),
			position,
		)
	}

	result := WithPrimaryKeyIntersector(
		newTestPKContext("TestRecord", []string{"id"}),
	)([]Vectored[*SingleMatchedAccess]{
		makeAccess("idx_a", 0, topMap),
		makeAccess("idx_b", 1, EmptyTranslationMap()),
	}, []*properties.RequestedOrdering{requested})

	if !result.IsViable() || len(result.GetExpressions()) != 1 {
		t.Fatalf(
			"translated requested ordering produced %d expressions, want 1",
			len(result.GetExpressions()),
		)
	}
	intersection, ok := result.GetExpressions()[0].(*plans.RecordQueryIntersectionPlan)
	if !ok {
		t.Fatalf(
			"translated result = %T, want *RecordQueryIntersectionPlan",
			result.GetExpressions()[0],
		)
	}
	keys := intersection.GetComparisonKeyValues()
	if len(keys) != 1 || values.ColumnNameValue(keys[0]) != "ID" {
		t.Fatalf("translated comparison keys = %#v, want [ID]", keys)
	}
}

func TestIntersector_FanoutLegIsPrimaryKeyDistinct(t *testing.T) {
	t.Parallel()

	fanout := makeDataAccessTestPartialMatch(
		"idx_fanout",
		1,
		mustDataAccessTestPlan(t, "fanout"),
	)
	fanout.candidate.(*dataAccessTestCandidate).createsDuplicates = true
	plain := makeDataAccessTestPartialMatch(
		"idx_plain",
		1,
		mustDataAccessTestPlan(t, "plain"),
	)

	result := WithPrimaryKeyIntersector(
		newTestPKContext("TestRecord", []string{"id"}),
	)([]Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(fanout, 0),
		makeVectoredAccess(plain, 1),
	}, nil)
	if !result.IsViable() || len(result.GetExpressions()) != 1 {
		t.Fatalf(
			"fanout intersection produced %d expressions, want 1",
			len(result.GetExpressions()),
		)
	}
	intersection, ok := result.GetExpressions()[0].(*plans.RecordQueryIntersectionPlan)
	if !ok {
		t.Fatalf(
			"fanout result = %T, want *RecordQueryIntersectionPlan",
			result.GetExpressions()[0],
		)
	}
	distinctLegs := 0
	for _, child := range intersection.GetChildren() {
		if _, ok := child.(*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan); ok {
			distinctLegs++
		}
	}
	if distinctLegs != 1 {
		t.Fatalf(
			"fanout intersection has %d PK-distinct legs, want exactly 1",
			distinctLegs,
		)
	}
}

func TestIntersector_DeclinesNonNaturalComparisonDirections(t *testing.T) {
	t.Parallel()

	makeAccess := func(
		name string,
		orders []MatchedSortOrder,
		position int,
	) Vectored[*SingleMatchedAccess] {
		fields := []string{"ID", "VERSION"}
		parts := make([]*MatchedOrderingPart, len(orders))
		for i, order := range orders {
			parts[i] = NewMatchedOrderingPart(
				values.UniqueCorrelationIdentifier(),
				dataAccessTestKey(fields[i]),
				nil,
				order,
			)
		}
		pm := &testPartialMatch{
			candidate: &dataAccessTestCandidate{
				name:        name,
				recordTypes: []string{"TestRecord"},
				fixedPlan:   mustDataAccessTestPlan(t, name),
			},
			matchInfo: &testMatchInfo{orderingParts: parts},
		}
		return makeVectoredAccess(pm, position)
	}

	for _, tc := range []struct {
		name     string
		orders   []MatchedSortOrder
		pkFields []string
	}{
		{
			name: "mixed_asc_desc",
			orders: []MatchedSortOrder{
				MatchedSortOrderAscending,
				MatchedSortOrderDescending,
			},
			pkFields: []string{"id", "version"},
		},
		{
			name: "counterflow_nulls",
			orders: []MatchedSortOrder{
				MatchedSortOrderAscendingNullsLast,
			},
			pkFields: []string{"id"},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := WithPrimaryKeyIntersector(
				newTestPKContext("TestRecord", tc.pkFields),
			)([]Vectored[*SingleMatchedAccess]{
				makeAccess("idx_a", tc.orders, 0),
				makeAccess("idx_b", tc.orders, 1),
			}, nil)
			if result.IsViable() {
				t.Fatalf(
					"%s comparison direction must fail closed",
					tc.name,
				)
			}
		})
	}
}

// TestPushCrossCandidateIntersection_StillFires is the focused over-decline
// sentinel at the production planner entry. The SQL/FDB tests below this
// package prove the same path end to end.
func TestPushCrossCandidateIntersection_StillFires(t *testing.T) {
	t.Parallel()

	pmA := makeDataAccessTestPartialMatch("idxA", 1, mustDataAccessTestPlan(t, "scanA"))
	pmB := makeDataAccessTestPartialMatch("idxB", 1, mustDataAccessTestPlan(t, "scanB"))

	scan := mustFullUnorderedScan(t, []string{"TestRecord"}, dataAccessTestRow)
	ref := expressions.InitialOf(scan)
	AddPartialMatchForCandidate(ref, pmA.GetMatchCandidate(), pmA)
	AddPartialMatchForCandidate(ref, pmB.GetMatchCandidate(), pmB)

	ctx := newTestPKContext("TestRecord", []string{"id"})
	p := NewPlanner(nil, ctx)
	p.pushCrossCandidateIntersection(ref,
		[]MatchCandidate{pmA.GetMatchCandidate(), pmB.GetMatchCandidate()},
		[]*properties.RequestedOrdering{properties.PreserveOrdering()})

	found := false
	for _, m := range ref.FinalMembers() {
		if _, ok := m.(*plans.RecordQueryIntersectionPlan); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("pushCrossCandidateIntersection produced no intersection final for two equality-bound matches — the ordering gate over-declined")
	}
}

func TestPushCrossCandidateIntersection_MatchGrowthReachesFourWay(t *testing.T) {
	t.Parallel()

	matches := make([]*testPartialMatch, 0, 4)
	candidates := make([]MatchCandidate, 0, 4)
	for _, name := range []string{"idxA", "idxB", "idxC", "idxD"} {
		pm := makeDataAccessTestPartialMatch(
			name,
			1,
			mustDataAccessTestPlan(t, name+"_scan"),
		)
		matches = append(matches, pm)
		candidates = append(candidates, pm.GetMatchCandidate())
	}

	scan := mustFullUnorderedScan(t, []string{"TestRecord"}, dataAccessTestRow)
	ref := expressions.InitialOf(scan)
	for i := 0; i < 2; i++ {
		AddPartialMatchForCandidate(ref, candidates[i], matches[i])
	}

	ctx := newTestPKContext("TestRecord", []string{"id"})
	p := NewPlanner(nil, ctx)
	requested := []*properties.RequestedOrdering{properties.PreserveOrdering()}
	p.pushCrossCandidateIntersection(ref, candidates[:2], requested)

	for i := 2; i < 4; i++ {
		AddPartialMatchForCandidate(ref, candidates[i], matches[i])
	}
	p.pushCrossCandidateIntersection(ref, candidates, requested)

	for _, member := range ref.FinalMembers() {
		if plan, ok := member.(*plans.RecordQueryIntersectionPlan); ok &&
			len(plan.GetChildren()) == 4 {
			return
		}
	}
	t.Fatal("a two-to-four match-set growth must produce a four-way intersection final")
}

func TestShouldConsumeIntersection_TracksExactInputSet(t *testing.T) {
	t.Parallel()

	pmA := makeDataAccessTestPartialMatch("idxA", 1, mustDataAccessTestPlan(t, "scanA"))
	pmB := makeDataAccessTestPartialMatch("idxB", 1, mustDataAccessTestPlan(t, "scanB"))
	pmC := makeDataAccessTestPartialMatch("idxC", 1, mustDataAccessTestPlan(t, "scanC"))

	scan := mustFullUnorderedScan(t, []string{"TestRecord"}, dataAccessTestRow)
	ref := expressions.InitialOf(scan)
	p := NewPlanner(nil, newTestPKContext("TestRecord", []string{"id"}))
	requested := []*properties.RequestedOrdering{nil, properties.PreserveOrdering()}

	ab := []Vectored[*SingleMatchedAccess]{makeVectoredAccess(pmA, 0), makeVectoredAccess(pmB, 1)}
	if !p.shouldConsumeIntersection(ref, ab, requested) {
		t.Fatal("first exact input must be consumed")
	}
	ba := []Vectored[*SingleMatchedAccess]{makeVectoredAccess(pmB, 0), makeVectoredAccess(pmA, 1)}
	if p.shouldConsumeIntersection(ref, ba, requested) {
		t.Fatal("the same match set in a different order must not be consumed twice")
	}
	ac := []Vectored[*SingleMatchedAccess]{makeVectoredAccess(pmA, 0), makeVectoredAccess(pmC, 1)}
	if !p.shouldConsumeIntersection(ref, ac, requested) {
		t.Fatal("a different match set at the same cardinality must be consumed")
	}
}

func TestPushCrossCandidateIntersection_RestrictedCandidateCap(t *testing.T) {
	t.Parallel()

	scan := mustFullUnorderedScan(t, []string{"TestRecord"}, dataAccessTestRow)
	ref := expressions.InitialOf(scan)
	var candidates []MatchCandidate
	for _, name := range []string{"idxA", "idxB", "idxC", "idxD", "idxE"} {
		pm := makeDataAccessTestPartialMatch(
			name,
			1,
			mustDataAccessTestPlan(t, name+"_scan"),
		)
		candidate := pm.GetMatchCandidate()
		candidates = append(candidates, candidate)
		AddPartialMatchForCandidate(ref, candidate, pm)
	}

	p := NewPlanner(nil, newTestPKContext("TestRecord", []string{"id"}))
	p.pushCrossCandidateIntersection(
		ref,
		candidates,
		[]*properties.RequestedOrdering{properties.PreserveOrdering()},
	)
	for _, member := range ref.FinalMembers() {
		if _, ok := member.(*plans.RecordQueryIntersectionPlan); ok {
			t.Fatal("five restricted candidates must stay outside the bounded intersection search")
		}
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

	pmA := makeDataAccessTestPartialMatch("idxA", 1, mustDataAccessTestPlan(t, "scanA"))
	pmB := makeDataAccessTestPartialMatch("idxB", 1, mustDataAccessTestPlan(t, "scanB"))
	ctx := newTestPKContext("TestRecord", []string{"id"})

	result := WithPrimaryKeyIntersector(ctx)([]Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(pmA, 0),
		makeVectoredAccess(pmB, 1),
	}, nil)
	if !result.IsViable() {
		t.Fatal("expected viable intersection")
	}
	for _, e := range result.GetExpressions() {
		iw, ok := e.(*plans.RecordQueryIntersectionPlan)
		if !ok {
			t.Fatalf("expected intersection plan, got %T", e)
		}
		for _, k := range iw.GetComparisonKeyValues() {
			fv, isFV := values.AsFieldValue(k)
			if !isFV || fv.Path() == nil || fv.Path().Len() != 1 {
				t.Fatalf("comparison key %v is UNBAKED — the positional merge row cannot resolve it", k)
			}
			if got := fv.Path().Ordinals()[0]; got != 0 {
				t.Fatalf("ID must bake to ordinal 0 of the descriptor layout, got %d", got)
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

	pmA := makeDataAccessTestPartialMatchWithPK("idxA", 1, mustDataAccessTestPlan(t, "scanA"), "ID", "VERSION")
	pmB := makeDataAccessTestPartialMatchWithPK("idxB", 1, mustDataAccessTestPlan(t, "scanB"), "ID", "VERSION")
	ctx := newTestPKContext("TestRecord", []string{"id", "version"})

	result := WithPrimaryKeyIntersector(ctx)([]Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(pmA, 0),
		makeVectoredAccess(pmB, 1),
	}, nil)
	if !result.IsViable() {
		t.Fatal("expected viable composite-pk intersection")
	}
	for _, e := range result.GetExpressions() {
		iw, ok := e.(*plans.RecordQueryIntersectionPlan)
		if !ok {
			t.Fatalf("expected intersection plan, got %T", e)
		}
		keys := iw.GetComparisonKeyValues()
		if len(keys) != 2 {
			t.Fatalf("composite pk must carry 2 comparison keys, got %d", len(keys))
		}
		for i, want := range []int{0, 1} {
			fv, isFV := values.AsFieldValue(keys[i])
			if !isFV || fv.Path() == nil || fv.Path().Len() != 1 {
				t.Fatalf("key %d unbaked: %v", i, keys[i])
			}
			if got := fv.Path().Ordinals()[0]; got != want {
				t.Fatalf("key %d: baked ordinal %d, want %d", i, got, want)
			}
		}
	}
}

func TestCanonicalizeIntersectionOrderingParts_RequiresCandidateOwnedExactRoots(t *testing.T) {
	t.Parallel()

	nested := values.NewRecordType("NESTED", false, []values.Field{
		{Name: "LEAF", FieldType: values.NullableLong},
	})
	layout := values.NewRecordType("COMPOSITE", false, []values.Field{
		{Name: "TENANT", FieldType: values.NullableLong},
		{Name: "SEQ", FieldType: values.NullableLong},
		{Name: "NESTED", FieldType: nested},
	})
	rootA, ok := orderingKeyCarrier(layout)
	if !ok {
		t.Fatal("construct candidate A ordering carrier")
	}
	rootB, ok := orderingKeyCarrier(layout)
	if !ok {
		t.Fatal("construct candidate B ordering carrier")
	}
	field := func(t *testing.T, root values.Value, ordinals ...int) values.Value {
		t.Helper()
		resolved, err := values.ResolveFieldOrdinals(root, ordinals)
		if err != nil {
			t.Fatalf("resolve %v: %v", ordinals, err)
		}
		return resolved
	}
	part := func(value values.Value) properties.ProvidedOrderingPart {
		return properties.ProvidedOrderingPart{
			Value:     value,
			SortOrder: properties.ProvidedSortOrderAscending,
		}
	}

	tenantOnA := field(t, rootA, 0)
	seqOnB := field(t, rootB, 1)
	input := []properties.ProvidedOrderingPart{part(tenantOnA), part(seqOnB)}
	admitted := map[values.QuantifiedObjectValue]struct{}{rootA: {}, rootB: {}}
	normalized, source, admittedOK := canonicalizeIntersectionOrderingParts(
		input, layout, rootA, admitted)
	if !admittedOK || source != rootA {
		t.Fatalf("alternating candidate roots declined: source=%v ok=%v", source, admittedOK)
	}
	for i, normalizedPart := range normalized {
		fv, isField := values.AsFieldValue(normalizedPart.Value)
		if !isField {
			t.Fatalf("normalized part %d = %T, want field", i, normalizedPart.Value)
		}
		root, isRoot := values.AsQuantifiedObjectValue(fv.ChildValue())
		if !isRoot || root != rootA {
			t.Fatalf("normalized part %d root = %v, want pointer-exact candidate A", i, root)
		}
	}
	// Copy-on-write is part of the bridge contract: the source parts still
	// state the two distinct candidate owners after normalization.
	originalTenant, _ := values.AsFieldValue(input[0].Value)
	originalSeq, _ := values.AsFieldValue(input[1].Value)
	tenantRoot, _ := values.AsQuantifiedObjectValue(originalTenant.ChildValue())
	seqRoot, _ := values.AsQuantifiedObjectValue(originalSeq.ChildValue())
	if tenantRoot != rootA || seqRoot != rootB {
		t.Fatal("canonicalization mutated its source comparison parts")
	}

	independent, ok := orderingKeyCarrier(layout)
	if !ok {
		t.Fatal("construct independent same-type current carrier")
	}
	foreign, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("foreign"), layout)
	if err != nil {
		t.Fatalf("construct foreign named carrier: %v", err)
	}
	wrongTypeLayout := values.NewRecordType("COMPOSITE", false, []values.Field{
		{Name: "TENANT", FieldType: values.NotNullLong},
		{Name: "SEQ", FieldType: values.NullableLong},
		{Name: "NESTED", FieldType: nested},
	})
	wrongType, ok := orderingKeyCarrier(wrongTypeLayout)
	if !ok {
		t.Fatal("construct same-domain wrong-type carrier")
	}
	wrongDomainLayout := values.NewRecordType("COMPOSITE", false, []values.Field{
		{Name: "OTHER", FieldType: values.NullableLong},
		{Name: "SEQ", FieldType: values.NullableLong},
		{Name: "NESTED", FieldType: nested},
	})
	wrongDomain, ok := orderingKeyCarrier(wrongDomainLayout)
	if !ok {
		t.Fatal("construct wrong-domain carrier")
	}

	tests := []struct {
		name       string
		value      values.Value
		additional values.QuantifiedObjectValue
	}{
		{name: "independently_minted_same_type_current", value: field(t, independent, 0)},
		{name: "foreign_named", value: field(t, foreign, 0), additional: foreign},
		{name: "wrong_exact_type", value: field(t, wrongType, 0), additional: wrongType},
		{name: "wrong_domain", value: field(t, wrongDomain, 0), additional: wrongDomain},
		{name: "nested_path", value: field(t, rootA, 2, 0), additional: rootA},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			caseAdmitted := map[values.QuantifiedObjectValue]struct{}{rootA: {}, rootB: {}}
			if testCase.additional != nil {
				caseAdmitted[testCase.additional] = struct{}{}
			}
			if got, _, ok := canonicalizeIntersectionOrderingParts(
				[]properties.ProvidedOrderingPart{part(testCase.value)},
				layout,
				rootA,
				caseAdmitted,
			); ok || got != nil {
				t.Fatalf("foreign comparison key admitted: %#v", got)
			}
		})
	}
}

// TestIntersector_DeclinesMixedLayoutLegs pins the leg-order-agnostic
// layout check: ONE layout-less leg declines the candidate regardless of
// which slot it occupies (the old planI-only check made planning
// depend on leg order).
func TestIntersector_DeclinesMixedLayoutLegs(t *testing.T) {
	t.Parallel()

	for name, accesses := range map[string][2]*testPartialMatch{
		"layoutless_first":  {makeDataAccessTestPartialMatch("idxA", 1, &testPlan{name: "scanA"}), makeDataAccessTestPartialMatch("idxB", 1, mustDataAccessTestPlan(t, "scanB"))},
		"layoutless_second": {makeDataAccessTestPartialMatch("idxA", 1, mustDataAccessTestPlan(t, "scanA")), makeDataAccessTestPartialMatch("idxB", 1, &testPlan{name: "scanB"})},
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
// same-index guard in generic subset enumeration: epoch
// re-matching can seed TWO candidate OBJECTS for one index, and an object-
// identity check under-detects — an A/A/B triple kept the redundant
// same-index arm the guard exists to eliminate. With candidates
// {A, A', B} (A and A' distinct objects, same CandidateName) only the ONE
// A/B pair may form: no triple, no A/A pair, and the A'/B pair dedupes
// against A/B only at the memo (both are yielded here — the pair loop
// guards identity of the INDEX, not of the access), so exactly the pairs
// with distinct names survive.
// Each access carries a compensation with a DISTINCT unmatched-aggregate id, and
// that is load-bearing rather than decorative. isPrimaryKeyPartitionRedundant
// proves a partition redundant when a subpartition fixes the same equalities AND
// carries the same unmatched-id set; two matches on ONE index always fix
// prefix-nested equalities, so with equal unmatched sets the redundancy sieve
// rejects the A/A' pair on its own and the NAME guard this test is about becomes
// undetectable — mutating the guard to key on the ACCESS instead of the index
// leaves the suite GREEN. Distinct unmatched sets make that proof fail open, so
// the guard is the only thing left that can reject the pair.
func TestIntersector_ThreeWay_DuplicateCandidateNameSkipped(t *testing.T) {
	t.Parallel()

	pmA := makeDataAccessTestPartialMatch("idxA", 1, mustDataAccessTestPlan(t, "scanA"))
	pmA2 := makeDataAccessTestPartialMatch("idxA", 1, mustDataAccessTestPlan(t, "scanA2"))
	pmB := makeDataAccessTestPartialMatch("idxB", 1, mustDataAccessTestPlan(t, "scanB"))

	ctx := newTestPKContext("TestRecord", []string{"id"})
	intersector := WithPrimaryKeyIntersector(ctx)

	sharedBase := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("dup_name_base"),
		expressions.InitialOf(mustFullUnorderedScan(t,
			[]string{"TestRecord"},
			dataAccessTestRow,
		)),
	)
	accesses := []Vectored[*SingleMatchedAccess]{
		NewVectored(NewSingleMatchedAccess(
			pmA,
			distinctUnmatchedCompensation(sharedBase, 1),
			values.UniqueCorrelationIdentifier(),
			false, EmptyTranslationMap(), nil,
		), 0),
		NewVectored(NewSingleMatchedAccess(
			pmA2,
			distinctUnmatchedCompensation(sharedBase, 2),
			values.UniqueCorrelationIdentifier(),
			false, EmptyTranslationMap(), nil,
		), 1),
		NewVectored(NewSingleMatchedAccess(
			pmB,
			distinctUnmatchedCompensation(sharedBase, 3),
			values.UniqueCorrelationIdentifier(),
			false, EmptyTranslationMap(), nil,
		), 2),
	}

	result := intersector(accesses, nil)
	if !result.IsViable() {
		t.Fatal("expected the distinct-name pairs to remain viable")
	}
	for _, e := range result.GetExpressions() {
		plan, ok := e.(*plans.RecordQueryIntersectionPlan)
		if !ok {
			t.Fatalf("expected *plans.RecordQueryIntersectionPlan, got %T", e)
		}
		if n := len(plan.GetChildren()); n != 2 {
			t.Fatalf("a triple containing a duplicate index name must not form; got a %d-way intersection", n)
		}
	}
	// A/B and A'/B — the A/A' pair is suppressed by name.
	if n := len(result.GetExpressions()); n != 2 {
		t.Fatalf("expected exactly the 2 distinct-name pairs, got %d", n)
	}
}

// TestIntersector_FourWay_BadPairSieve asserts the ENUMERATION OUTCOME around a
// duplicate-index pair, and its name overstates what it can see: the badPairs
// sieve itself is unobservable from a result set. Replacing the sieve lookup with
// an empty map leaves this whole package GREEN, because every partition the sieve
// would have skipped is rejected again by the per-partition checks it exists to
// SKIP (the same-index name guard, the redundancy proof). That is by design — the
// sieve's own comment says it avoids rebuilding scans — so it is a performance
// mechanism, not a semantic one, and no assertion here should be read as its net.
// The name guard's net is TestIntersector_ThreeWay_DuplicateCandidateNameSkipped.
func TestIntersector_FourWay_BadPairSieve(t *testing.T) {
	t.Parallel()

	accesses := []Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(makeDataAccessTestPartialMatch("idxA", 1, mustDataAccessTestPlan(t, "scanA")), 0),
		makeVectoredAccess(makeDataAccessTestPartialMatch("idxA", 1, mustDataAccessTestPlan(t, "scanA2")), 1),
		makeVectoredAccess(makeDataAccessTestPartialMatch("idxB", 1, mustDataAccessTestPlan(t, "scanB")), 2),
		makeVectoredAccess(makeDataAccessTestPartialMatch("idxC", 1, mustDataAccessTestPlan(t, "scanC")), 3),
	}

	result := WithPrimaryKeyIntersector(newTestPKContext("TestRecord", []string{"id"}))(accesses, nil)
	if !result.IsViable() {
		t.Fatal("subsets that do not contain the duplicate-candidate pair must remain viable")
	}
	byArity := map[int]int{}
	for _, expr := range result.GetExpressions() {
		plan, ok := expr.(*plans.RecordQueryIntersectionPlan)
		if !ok {
			t.Fatalf("expected *plans.RecordQueryIntersectionPlan, got %T", expr)
		}
		byArity[len(plan.GetChildren())]++
	}
	if byArity[2] != 0 || byArity[3] != 2 || byArity[4] != 0 {
		t.Fatalf("sieved intersection arities = %v, want map[3:2] and no four-way plan", byArity)
	}
}
