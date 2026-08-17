package cascades

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func maxMatchTestQOV(
	t testing.TB,
	alias values.CorrelationIdentifier,
	typ values.Type,
) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(alias, typ)
	return mustConstruct(t, qov, err)
}

func maxMatchTestField(t testing.TB, name string) values.Value {
	t.Helper()
	rowType := values.NewRecordType("MaxMatch"+name, false, []values.Field{
		{Name: name, FieldType: values.NullableLong, Ordinal: 0},
	})
	root := maxMatchTestQOV(
		t,
		values.NamedCorrelationIdentifier("max_match_"+name),
		rowType,
	)
	field, err := values.ResolveFieldOrdinals(root, []int{0})
	return mustConstruct(t, field, err)
}

// ---------------------------------------------------------------------------
// 1. Leaf value matching
// ---------------------------------------------------------------------------

func TestComputeMaxMatchMap_LeafFieldMatch(t *testing.T) {
	t.Parallel()

	qv := maxMatchTestField(t, "col1")
	cv := maxMatchTestField(t, "col1")

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	if mmm.Size() != 1 {
		t.Fatalf("expected 1 mapping entry for identical leaf values, got %d", mmm.Size())
	}
	if mmm.GetQueryValue() != qv {
		t.Fatal("queryValue mismatch")
	}
	if mmm.GetCandidateValue() != cv {
		t.Fatal("candidateValue mismatch")
	}

	// Verify the mapping points to the right values.
	m := mmm.GetMap()
	for q, c := range m {
		queryField, queryOK := values.AsFieldValue(q)
		if !queryOK || queryField.DisplayName() != "col1" {
			t.Fatalf("expected exact query field 'col1', got %q", values.ExplainValue(q))
		}
		candidateField, candidateOK := values.AsFieldValue(c)
		if !candidateOK || candidateField.DisplayName() != "col1" {
			t.Fatalf("expected exact candidate field 'col1', got %q", values.ExplainValue(c))
		}
	}
}

func TestComputeMaxMatchMap_LeafConstantMatch(t *testing.T) {
	t.Parallel()

	qv := &values.ConstantValue{Value: int64(42), Typ: values.NullableLong}
	cv := &values.ConstantValue{Value: int64(42), Typ: values.NullableLong}

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	if mmm.Size() != 1 {
		t.Fatalf("expected 1 mapping for identical constants, got %d", mmm.Size())
	}
}

func TestComputeMaxMatchMap_LeafQOVMatch(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("q1")
	qv := maxMatchTestQOV(t, alias, values.NullableLong)
	cv := maxMatchTestQOV(t, alias, values.NullableLong)

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	if mmm.Size() != 1 {
		t.Fatalf("expected 1 mapping for identical QOVs, got %d", mmm.Size())
	}
}

// ---------------------------------------------------------------------------
// 2. Same-structure tree matching
// ---------------------------------------------------------------------------

func TestComputeMaxMatchMap_SameStructureArithmetic(t *testing.T) {
	t.Parallel()

	left := maxMatchTestField(t, "x")
	right := &values.ConstantValue{Value: int64(5), Typ: values.NullableLong}

	qv := &values.ArithmeticValue{Op: values.OpAdd, Left: left, Right: right}
	cv := &values.ArithmeticValue{Op: values.OpAdd, Left: left, Right: right}

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	// Root-level structural equality holds.
	if mmm.Size() != 1 {
		t.Fatalf("expected 1 mapping for structurally equal arithmetic, got %d", mmm.Size())
	}
}

func TestComputeMaxMatchMap_SameStructureRCV(t *testing.T) {
	t.Parallel()

	fa := maxMatchTestField(t, "A")
	fb := maxMatchTestField(t, "B")

	qv := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: fa},
		values.RecordConstructorField{Name: "b", Value: fb},
	)
	cv := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: fa},
		values.RecordConstructorField{Name: "b", Value: fb},
	)
	mmm := ComputeMaxMatchMap(qv, cv, nil)

	// Entire RCV matches as a single entry.
	if mmm.Size() != 1 {
		t.Fatalf("expected 1 mapping for same-structure RCV, got %d", mmm.Size())
	}
}

// ---------------------------------------------------------------------------
// 3. Different-structure no-match
// ---------------------------------------------------------------------------

func TestComputeMaxMatchMap_DifferentFields(t *testing.T) {
	t.Parallel()

	qv := maxMatchTestField(t, "col1")
	cv := maxMatchTestField(t, "col2")

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	if mmm.Size() != 0 {
		t.Fatalf("expected 0 mapping entries for different fields, got %d", mmm.Size())
	}
}

func TestComputeMaxMatchMap_DifferentTypes(t *testing.T) {
	t.Parallel()

	qv := maxMatchTestField(t, "col1")
	cv := &values.ConstantValue{Value: int64(1), Typ: values.NullableLong}

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	if mmm.Size() != 0 {
		t.Fatalf("expected 0 mapping entries for different value types, got %d", mmm.Size())
	}
}

func TestComputeMaxMatchMap_DifferentArithOps(t *testing.T) {
	t.Parallel()

	left := maxMatchTestField(t, "x")
	right := &values.ConstantValue{Value: int64(5), Typ: values.NullableLong}

	// Same children, different operator → root doesn't match, but
	// children are not reachable through ArithmeticValue (not an RCV),
	// so no matches at all.
	qv := &values.ArithmeticValue{Op: values.OpAdd, Left: left, Right: right}
	cv := &values.ArithmeticValue{Op: values.OpSub, Left: left, Right: right}

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	// ArithmeticValue is not a RecordConstructorValue, so children are
	// not "reachable" — we can't match them.
	if mmm.Size() != 0 {
		t.Fatalf("expected 0 mapping (arithmetic children not reachable), got %d", mmm.Size())
	}
}

// ---------------------------------------------------------------------------
// 4. Nested RecordConstructorValue matching
// ---------------------------------------------------------------------------

func TestComputeMaxMatchMap_NestedRCV(t *testing.T) {
	t.Parallel()

	fa := maxMatchTestField(t, "A")
	fb := maxMatchTestField(t, "B")

	innerQ := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "x", Value: fa},
	)
	innerC := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "x", Value: fa},
	)

	qv := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "inner", Value: innerQ},
		values.RecordConstructorField{Name: "b", Value: fb},
	)
	cv := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "inner", Value: innerC},
		values.RecordConstructorField{Name: "b", Value: fb},
	)

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	// Entire tree matches at the root level (same structure).
	if mmm.Size() != 1 {
		t.Fatalf("expected 1 mapping for nested same-structure RCV, got %d", mmm.Size())
	}
}

func TestComputeMaxMatchMap_NestedRCVPartialMatch(t *testing.T) {
	t.Parallel()

	fa := maxMatchTestField(t, "A")
	fb := maxMatchTestField(t, "B")
	fc := maxMatchTestField(t, "C")

	innerQ := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "x", Value: fa},
	)
	innerC := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "x", Value: fa},
	)

	// Query: rcv(inner: rcv(x: A), b: B)
	// Candidate: rcv(inner: rcv(x: A), b: C) — different second child
	qv := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "inner", Value: innerQ},
		values.RecordConstructorField{Name: "b", Value: fb},
	)
	cv := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "inner", Value: innerC},
		values.RecordConstructorField{Name: "b", Value: fc},
	)

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	// Root doesn't match (different second child). The nested RCV
	// (inner) matches. The second child (B vs C) doesn't match but
	// is constant w.r.t. rangedOverAliases (empty), so it's OK.
	// We expect the inner RCV to be matched.
	if mmm.Size() != 1 {
		t.Fatalf("expected 1 mapping for inner RCV match, got %d", mmm.Size())
	}

	m := mmm.GetMap()
	for q := range m {
		rcv, ok := q.(*values.RecordConstructorValue)
		if !ok || len(rcv.Fields) != 1 || rcv.Fields[0].Name != "x" {
			t.Fatalf("expected one-field inner query constructor, got %q", values.ExplainValue(q))
		}
		field, fieldOK := values.AsFieldValue(rcv.Fields[0].Value)
		if !fieldOK || field.DisplayName() != "A" {
			t.Fatalf("expected inner query field A, got %q", values.ExplainValue(q))
		}
	}
}

// ---------------------------------------------------------------------------
// 5. Reordered children — cross-product finds the match
// ---------------------------------------------------------------------------

func TestComputeMaxMatchMap_ReorderedRCVChildren(t *testing.T) {
	t.Parallel()

	fa := maxMatchTestField(t, "A")
	fb := maxMatchTestField(t, "B")
	fc := maxMatchTestField(t, "C")

	// Query: rcv(a: A, b: B, c: C)
	qv := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: fa},
		values.RecordConstructorField{Name: "b", Value: fb},
		values.RecordConstructorField{Name: "c", Value: fc},
	)
	// Candidate: rcv(c: C, b: B, a: A)  — same fields, different order
	cv := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "c", Value: fc},
		values.RecordConstructorField{Name: "b", Value: fb},
		values.RecordConstructorField{Name: "a", Value: fa},
	)

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	// Root doesn't match (field names at different positions), but
	// individual children are reachable in the candidate RCV.
	// We expect 3 child-level matches.
	if mmm.Size() != 3 {
		t.Fatalf("expected 3 mappings for reordered RCV children, got %d", mmm.Size())
	}
}

// ---------------------------------------------------------------------------
// 6. Partial matching (some children match, some don't)
// ---------------------------------------------------------------------------

func TestComputeMaxMatchMap_PartialChildMatch(t *testing.T) {
	t.Parallel()

	fa := maxMatchTestField(t, "A")
	fb := maxMatchTestField(t, "B")
	fc := maxMatchTestField(t, "C")

	// Query: rcv(a: A, b: B)
	// Candidate: rcv(a: A, b: C)
	// Only first child matches.
	qv := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: fa},
		values.RecordConstructorField{Name: "b", Value: fb},
	)
	cv := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: fa},
		values.RecordConstructorField{Name: "b", Value: fc},
	)

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	// Root doesn't match. First child (A) matches. Second child (B
	// vs C) doesn't match, but B is constant w.r.t. rangedOverAliases
	// (empty set), so partial match is allowed.
	if mmm.Size() < 1 {
		t.Fatalf("expected at least 1 mapping for partial child match, got %d", mmm.Size())
	}
}

func TestRecurseQueryResultValue_InProgressCycleIsPathLocal(t *testing.T) {
	t.Parallel()

	queryValue := maxMatchTestField(t, "cycle")
	candidateValue := maxMatchTestField(t, "cycle")
	memoMap := make(map[values.Value]map[values.Value]matchResult)
	inProgress := map[values.Value]struct{}{queryValue: {}}

	// Model the exact re-entry created when a record QOV expands to resolved
	// FieldValues rooted at that same QOV. The active path must decline without
	// publishing a completed negative result.
	blocked := recurseQueryResultValue(
		queryValue,
		candidateValue,
		nil,
		-1,
		nil,
		math.MaxInt32,
		memoMap,
		inProgress,
	)
	blockedResult, ok := blocked[queryValue]
	if !ok || len(blockedResult.valueMap) != 0 {
		t.Fatalf("expected the in-progress branch to decline, got %#v", blocked)
	}
	if _, cached := memoMap[queryValue]; cached {
		t.Fatal("an in-progress cycle decline must not become a completed memo result")
	}

	// Once the ancestor unwinds, the same value is legal on another path and
	// must receive a full matching attempt. A global negative memo/visited set
	// would make this retry fail.
	delete(inProgress, queryValue)
	retried := recurseQueryResultValue(
		queryValue,
		candidateValue,
		nil,
		-1,
		nil,
		math.MaxInt32,
		memoMap,
		inProgress,
	)
	retriedResult, ok := retried[queryValue]
	if !ok || len(retriedResult.valueMap) != 1 {
		t.Fatalf("expected the released value to match on retry, got %#v", retried)
	}
	if len(inProgress) != 0 {
		t.Fatalf("expected path markers to be released after recursion, got %d", len(inProgress))
	}
	if _, cached := memoMap[queryValue]; !cached {
		t.Fatal("expected the completed retry to remain memoized")
	}
}

func TestComputeMaxMatchMap_PartialChildMatch_RangedOver(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("q")
	qov := maxMatchTestQOV(t, alias, values.NullableLong)
	fa := maxMatchTestField(t, "A")
	fc := maxMatchTestField(t, "C")

	// Query: rcv(a: qov(q), b: A)
	// Candidate: rcv(a: qov(q), b: C)
	// Second child doesn't match. With rangedOverAliases empty, it's OK.
	// But first child (qov) references alias which IS in rangedOverAliases.
	qv := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: qov},
		values.RecordConstructorField{Name: "b", Value: fa},
	)
	cv := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: qov},
		values.RecordConstructorField{Name: "b", Value: fc},
	)

	rangedOver := map[values.CorrelationIdentifier]struct{}{alias: {}}
	mmm := ComputeMaxMatchMap(qv, cv, rangedOver)

	// qov(q) matches, A doesn't match C but A is constant w.r.t. {q}.
	// So we get 1 mapping for the qov.
	if mmm.Size() < 1 {
		t.Fatalf("expected at least 1 mapping, got %d", mmm.Size())
	}
}

// ---------------------------------------------------------------------------
// 7. QOV short-circuit
// ---------------------------------------------------------------------------

func TestComputeMaxMatchMap_QOVShortCircuit(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("q")
	fa := maxMatchTestField(t, "A")
	flowed := maxMatchTestQOV(t, alias, values.NullableLong)

	// Query: rcv(a: qov(q), b: A) — references alias q
	qv := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: flowed},
		values.RecordConstructorField{Name: "b", Value: fa},
	)
	// Candidate IS qov(q) — the identity flow
	cv := maxMatchTestQOV(t, alias, values.NullableLong)

	rangedOver := map[values.CorrelationIdentifier]struct{}{alias: {}}
	mmm := ComputeMaxMatchMap(qv, cv, rangedOver)

	// Short-circuit should fire: the candidate is qov(q), the query
	// references q, and there's exactly one ranged alias.
	if mmm.Size() != 1 {
		t.Fatalf("expected 1 mapping from short-circuit, got %d", mmm.Size())
	}

	// The mapping should be candidateValue → candidateValue.
	m := mmm.GetMap()
	for q, c := range m {
		if q != cv || c != cv {
			t.Fatalf("short-circuit mapping should be cv→cv, got %v→%v",
				values.ExplainValue(q), values.ExplainValue(c))
		}
	}
}

func TestComputeMaxMatchMap_QOVShortCircuit_MultipleAliases(t *testing.T) {
	t.Parallel()

	alias1 := values.NamedCorrelationIdentifier("q1")
	alias2 := values.NamedCorrelationIdentifier("q2")

	qv := maxMatchTestQOV(t, alias1, values.NullableLong)
	cv := maxMatchTestQOV(t, alias1, values.NullableLong)

	// Multiple rangedOverAliases → short-circuit does NOT fire.
	rangedOver := map[values.CorrelationIdentifier]struct{}{alias1: {}, alias2: {}}
	mmm := ComputeMaxMatchMap(qv, cv, rangedOver)

	// Should still match via normal path since qov(q1) == qov(q1).
	if mmm.Size() != 1 {
		t.Fatalf("expected 1 mapping via normal path, got %d", mmm.Size())
	}
}

// ---------------------------------------------------------------------------
// 8. Empty/nil inputs
// ---------------------------------------------------------------------------

func TestComputeMaxMatchMap_NilValues(t *testing.T) {
	t.Parallel()

	mmm := ComputeMaxMatchMap(nil, nil, nil)

	if mmm.Size() != 0 {
		t.Fatalf("expected 0 mapping entries for nil values, got %d", mmm.Size())
	}
	if mmm.GetQueryValue() != nil {
		t.Fatal("queryValue should be nil")
	}
	if mmm.GetCandidateValue() != nil {
		t.Fatal("candidateValue should be nil")
	}
}

func TestComputeMaxMatchMap_NilQueryValue(t *testing.T) {
	t.Parallel()

	cv := maxMatchTestField(t, "col1")
	mmm := ComputeMaxMatchMap(nil, cv, nil)

	if mmm.Size() != 0 {
		t.Fatalf("expected 0 mapping entries for nil queryValue, got %d", mmm.Size())
	}
}

func TestComputeMaxMatchMap_NilCandidateValue(t *testing.T) {
	t.Parallel()

	qv := maxMatchTestField(t, "col1")
	mmm := ComputeMaxMatchMap(qv, nil, nil)

	if mmm.Size() != 0 {
		t.Fatalf("expected 0 mapping entries for nil candidateValue, got %d", mmm.Size())
	}
}

func TestComputeMaxMatchMap_EmptyRangedOverAliases(t *testing.T) {
	t.Parallel()

	qv := maxMatchTestField(t, "col1")
	cv := maxMatchTestField(t, "col1")
	mmm := ComputeMaxMatchMap(qv, cv, map[values.CorrelationIdentifier]struct{}{})

	if mmm.Size() != 1 {
		t.Fatalf("expected 1 mapping for empty rangedOverAliases, got %d", mmm.Size())
	}
}

// ---------------------------------------------------------------------------
// 9. Deep nesting (3+ levels)
// ---------------------------------------------------------------------------

func TestComputeMaxMatchMap_ThreeLevelNesting(t *testing.T) {
	t.Parallel()

	fa := maxMatchTestField(t, "A")

	// 3-level nesting: rcv(x: rcv(y: rcv(z: A)))
	innermost := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "z", Value: fa},
	)
	middle := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "y", Value: innermost},
	)
	outerQ := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "x", Value: middle},
	)

	innermostC := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "z", Value: fa},
	)
	middleC := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "y", Value: innermostC},
	)
	outerC := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "x", Value: middleC},
	)

	mmm := ComputeMaxMatchMap(outerQ, outerC, nil)

	// Entire 3-level tree matches at root.
	if mmm.Size() != 1 {
		t.Fatalf("expected 1 mapping for 3-level identical nesting, got %d", mmm.Size())
	}
}

func TestComputeMaxMatchMap_ThreeLevelPartialMatch(t *testing.T) {
	t.Parallel()

	fa := maxMatchTestField(t, "A")
	fb := maxMatchTestField(t, "B")

	// Query: rcv(x: rcv(y: rcv(z: A)))
	innermostQ := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "z", Value: fa},
	)
	middleQ := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "y", Value: innermostQ},
	)
	outerQ := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "x", Value: middleQ},
	)

	// Candidate: rcv(x: rcv(y: rcv(z: B))) — innermost differs
	innermostC := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "z", Value: fb},
	)
	middleC := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "y", Value: innermostC},
	)
	outerC := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "x", Value: middleC},
	)

	mmm := ComputeMaxMatchMap(outerQ, outerC, nil)

	// Neither outer nor middle nor innermost match fully. A doesn't
	// match B. But since A is constant w.r.t. no rangedOverAliases,
	// the match is partial with 0 real mappings.
	// The algorithm should still return something (possibly empty).
	if mmm.Size() != 0 {
		t.Fatalf("expected 0 mappings when innermost children differ, got %d", mmm.Size())
	}
}

// ---------------------------------------------------------------------------
// 10. MaxDepth correctness
// ---------------------------------------------------------------------------

func TestMatchResult_NotMatched(t *testing.T) {
	t.Parallel()

	r := notMatched()
	if r.isMatched() {
		t.Fatal("notMatched() should not be matched")
	}
	if r.maxDepth != math.MaxInt32 {
		t.Fatalf("notMatched() maxDepth should be MaxInt32, got %d", r.maxDepth)
	}
}

func TestMatchResult_PerfectMatch(t *testing.T) {
	t.Parallel()

	r := matchResultOf(map[string]maxMatchEntry{
		"test": {queryValue: maxMatchTestField(t, "A")},
	}, 0)

	if !r.isMatched() {
		t.Fatal("depth-0 result should be matched")
	}
	if r.maxDepth != 0 {
		t.Fatalf("perfect match should have maxDepth 0, got %d", r.maxDepth)
	}
}

func TestMatchResult_ChildDepth(t *testing.T) {
	t.Parallel()

	// A result at depth 2 means the match was found 2 levels down.
	r := matchResultOf(map[string]maxMatchEntry{}, 2)
	if !r.isMatched() {
		t.Fatal("depth-2 result should be matched")
	}
	if r.maxDepth != 2 {
		t.Fatalf("expected maxDepth 2, got %d", r.maxDepth)
	}
}

// ---------------------------------------------------------------------------
// Additional tests: existing API surface
// ---------------------------------------------------------------------------

func TestTranslateQueryValueMaybe_IdentityMapping(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("candidate")
	qv := maxMatchTestField(t, "col1")
	cv := maxMatchTestField(t, "col1")

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	result := mmm.TranslateQueryValueMaybe(alias)
	if result == nil {
		t.Fatal("TranslateQueryValueMaybe returned nil for identity mapping")
	}

	qov, ok := values.AsQuantifiedObjectValue(result)
	if !ok {
		t.Fatalf("expected QuantifiedObjectValue, got %T", result)
	}
	if qov.Correlation() != alias {
		t.Fatalf("expected correlation %q, got %q", alias.Name(), qov.Correlation().Name())
	}
}

func TestTranslateQueryValueMaybe_EmptyMapping(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("candidate")
	qv := maxMatchTestField(t, "col1")
	cv := maxMatchTestField(t, "col2")

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	result := mmm.TranslateQueryValueMaybe(alias)
	if result != nil {
		t.Fatalf("TranslateQueryValueMaybe should return nil for empty mapping, got %T", result)
	}
}

func TestTranslateQueryValueMaybe_NilQueryValue(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("candidate")
	mmm := ComputeMaxMatchMap(nil, nil, nil)

	result := mmm.TranslateQueryValueMaybe(alias)
	if result != nil {
		t.Fatal("TranslateQueryValueMaybe should return nil for nil queryValue")
	}
}

func TestPullUpMaybe_IdentityMapping(t *testing.T) {
	t.Parallel()

	queryAlias := values.NamedCorrelationIdentifier("query")
	candidateAlias := values.NamedCorrelationIdentifier("candidate")

	qv := maxMatchTestField(t, "col1")
	cv := maxMatchTestField(t, "col1")
	mmm := ComputeMaxMatchMap(qv, cv, nil)

	tm, ok := mmm.PullUpMaybe(queryAlias, candidateAlias)
	if !ok {
		t.Fatal("PullUpMaybe returned false for identity mapping")
	}
	if tm == nil {
		t.Fatal("PullUpMaybe returned nil translation map")
	}
	if !tm.ContainsSourceAlias(queryAlias) {
		t.Fatal("translation map should contain queryAlias")
	}
}

func TestPullUpMaybe_EmptyMapping(t *testing.T) {
	t.Parallel()

	queryAlias := values.NamedCorrelationIdentifier("query")
	candidateAlias := values.NamedCorrelationIdentifier("candidate")

	qv := maxMatchTestField(t, "col1")
	cv := maxMatchTestField(t, "col2")
	mmm := ComputeMaxMatchMap(qv, cv, nil)

	tm, ok := mmm.PullUpMaybe(queryAlias, candidateAlias)
	if ok {
		t.Fatal("PullUpMaybe should return false for empty mapping")
	}
	if tm != nil {
		t.Fatal("PullUpMaybe should return nil for empty mapping")
	}
}

func TestAdjustMaybe_IdentityMapping(t *testing.T) {
	t.Parallel()

	upperAlias := values.NamedCorrelationIdentifier("upper")
	qv := maxMatchTestField(t, "col1")
	cv := maxMatchTestField(t, "col1")
	upperResult := maxMatchTestField(t, "col1")

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	adjusted, ok := mmm.AdjustMaybe(upperAlias, upperResult, nil)
	if !ok {
		t.Fatal("AdjustMaybe returned false for identity mapping")
	}
	if adjusted == nil {
		t.Fatal("AdjustMaybe returned nil for identity mapping")
	}
	if adjusted.GetCandidateValue() != upperResult {
		t.Fatal("adjusted candidateValue should be upperResult")
	}
}

func TestAdjustMaybe_EmptyMapping(t *testing.T) {
	t.Parallel()

	upperAlias := values.NamedCorrelationIdentifier("upper")
	qv := maxMatchTestField(t, "col1")
	cv := maxMatchTestField(t, "col2")
	upperResult := maxMatchTestField(t, "col1")

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	adjusted, ok := mmm.AdjustMaybe(upperAlias, upperResult, nil)
	if ok {
		t.Fatal("AdjustMaybe should return false for empty mapping")
	}
	if adjusted != nil {
		t.Fatal("AdjustMaybe should return nil for empty mapping")
	}
}

func TestMaxMatchMap_GetMap_Accessors(t *testing.T) {
	t.Parallel()

	q := &values.ConstantValue{Value: int64(1), Typ: values.NullableLong}
	c := &values.ConstantValue{Value: int64(2), Typ: values.NullableLong}
	mapping := map[values.Value]values.Value{q: c}

	mmm := NewMaxMatchMap(mapping, q, c)

	if mmm.GetQueryValue() != q {
		t.Fatal("queryValue mismatch")
	}
	if mmm.GetCandidateValue() != c {
		t.Fatal("candidateValue mismatch")
	}
	gotMap := mmm.GetMap()
	if len(gotMap) != 1 {
		t.Fatalf("mapping len=%d, want 1", len(gotMap))
	}
}

func TestMaxMatchMap_NewMaxMatchMap_DefensiveCopy(t *testing.T) {
	t.Parallel()

	q := &values.ConstantValue{Value: int64(1), Typ: values.NullableLong}
	c := &values.ConstantValue{Value: int64(2), Typ: values.NullableLong}
	mapping := map[values.Value]values.Value{q: c}

	mmm := NewMaxMatchMap(mapping, q, c)

	extra := &values.ConstantValue{Value: int64(3), Typ: values.NullableLong}
	mapping[extra] = extra

	if mmm.Size() != 1 {
		t.Fatal("mutation of original mapping leaked into MaxMatchMap")
	}
}

func TestMaxMatchMap_Size(t *testing.T) {
	t.Parallel()

	mmm := ComputeMaxMatchMap(nil, nil, nil)
	if mmm.Size() != 0 {
		t.Fatalf("expected size 0, got %d", mmm.Size())
	}

	qv := maxMatchTestField(t, "a")
	cv := maxMatchTestField(t, "a")
	mmm2 := ComputeMaxMatchMap(qv, cv, nil)
	if mmm2.Size() != 1 {
		t.Fatalf("expected size 1, got %d", mmm2.Size())
	}
}

// ---------------------------------------------------------------------------
// walkReachable / findMatchingReachableCandidate
// ---------------------------------------------------------------------------

func TestFindMatchingReachableCandidate_RCVChild(t *testing.T) {
	t.Parallel()

	fa := maxMatchTestField(t, "A")
	fb := maxMatchTestField(t, "B")

	// Candidate has A as a child of RCV — A is reachable.
	candidate := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "a", Value: fa},
		values.RecordConstructorField{Name: "b", Value: fb},
	)

	found, match := findMatchingReachableCandidateWithEquivalence(fa, candidate, nil)
	if !found {
		t.Fatal("A should be reachable inside RCV")
	}
	if !values.ValuesStructurallyEqual(match, fa) {
		t.Fatal("matched value should be structurally equal to A")
	}
}

func TestFindMatchingReachableCandidate_ArithChild(t *testing.T) {
	t.Parallel()

	fa := maxMatchTestField(t, "A")
	fb := maxMatchTestField(t, "B")

	// Candidate is arithmetic — children are NOT reachable.
	candidate := &values.ArithmeticValue{Op: values.OpAdd, Left: fa, Right: fb}

	found, _ := findMatchingReachableCandidateWithEquivalence(fa, candidate, nil)
	if found {
		t.Fatal("A should NOT be reachable inside ArithmeticValue")
	}
}

func TestTranslateQueryValueMaybe_RangedOverAliasValidation(t *testing.T) {
	t.Parallel()

	candidateAlias := values.NamedCorrelationIdentifier("candidate")
	forbiddenAlias := values.NamedCorrelationIdentifier("forbidden")

	forbidden := maxMatchTestQOV(t, forbiddenAlias, values.NullableLong)
	qv := forbidden
	cv := forbidden

	rangedOver := map[values.CorrelationIdentifier]struct{}{forbiddenAlias: {}}
	mmm := ComputeMaxMatchMap(qv, cv, rangedOver)

	result := mmm.TranslateQueryValueMaybe(candidateAlias)
	if result == nil {
		t.Fatal("TranslateQueryValueMaybe should succeed when rangedOverAlias is fully substituted")
	}
}

func TestComputeMaxMatchMap_NewMaxMatchMap_NilInputs(t *testing.T) {
	t.Parallel()

	mmm := NewMaxMatchMap(nil, nil, nil)

	if mmm.GetQueryValue() != nil {
		t.Fatal("nil queryValue should stay nil")
	}
	if mmm.GetCandidateValue() != nil {
		t.Fatal("nil candidateValue should stay nil")
	}
	gotMap := mmm.GetMap()
	if gotMap == nil {
		t.Fatal("map should be non-nil even when input is nil")
	}
	if len(gotMap) != 0 {
		t.Fatal("map should be empty when input is nil")
	}
}
