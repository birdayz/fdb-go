package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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

// TestMatchIntermediateRule_CyclicQuantifierPermutation pins RFC-189's
// MatchIntermediate permutation port (finding 13): the rule enumerates ALL
// quantifier bijections (Java's subsumedBy via EnumeratingIterable), not just the
// positional pairing. Three quantifiers are used deliberately — FireExpressionRule
// already swaps the FIRST TWO quantifiers of a ChildrenAsSet Select, so a 2-way
// swap is covered without the port; a CYCLIC permutation is not. Query
// Select(A, B, C) vs candidate Select(B, C, A): the only successful bijection is
// perm=[2,0,1] (query A ↔ candidate A at position 2, B↔position 0, C↔position 1),
// which neither positional pairing nor the single first-two swap reaches.
func TestMatchIntermediateRule_CyclicQuantifierPermutation(t *testing.T) {
	t.Parallel()

	newScanRef := func(rt string) *expressions.Reference {
		return expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{rt}, values.UnknownType))
	}

	// Query: Select(A, B, C).
	qA, qB, qC := newScanRef("A"), newScanRef("B"), newScanRef("C")
	querySelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(qA),
			expressions.ForEachQuantifier(qB),
			expressions.ForEachQuantifier(qC),
		}, nil,
	)
	querySelectRef := expressions.InitialOf(querySelect)

	// Candidate: Select(B, C, A) — a cyclic permutation of the query order.
	cB, cC, cA := newScanRef("B"), newScanRef("C"), newScanRef("A")
	candidateSelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(cB),
			expressions.ForEachQuantifier(cC),
			expressions.ForEachQuantifier(cA),
		}, nil,
	)
	candidateSelectRef := expressions.InitialOf(candidateSelect)

	mc := &testMatchCandidate{name: "idx_join3", traversal: NewTraversal(candidateSelectRef)}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	leafRule := NewMatchLeafRule()
	FireExpressionRuleWithMemo(leafRule, qA, ctx, nil)
	FireExpressionRuleWithMemo(leafRule, qB, ctx, nil)
	FireExpressionRuleWithMemo(leafRule, qC, ctx, nil)

	FireExpressionRuleWithMemo(NewMatchIntermediateRule(), querySelectRef, ctx, nil)

	if len(GetPartialMatchesForCandidate(querySelectRef, mc)) == 0 {
		t.Fatal("expected a PartialMatch via the cyclic bijection [2,0,1]; positional + first-two-swap both miss it")
	}
}

// TestMatchIntermediateRule_NodeReCheckUnderBijectionMap pins RFC-189's
// MatchIntermediate node re-check: the node-level
// EqualsWithoutChildren must be evaluated UNDER the quantifier bijection's alias
// map, not once with an empty map. Query and candidate use DIFFERENT quantifier
// aliases, so a result value that references those aliases is unequal under an
// empty map and equal only under the bijection {query-q → candidate-q}. The
// candidate quantifier order is swapped (cB, cA) so the sole valid child
// bijection is the non-identity perm [1,0]; the result values reference the QOVs
// (record{f: qA, g: qB} vs record{f: cA, g: cB}) so the node check genuinely
// depends on the map. The old empty-map pre-check rejected this valid match
// before any permutation was tried — this test fails without the per-bijection
// re-check.
func TestMatchIntermediateRule_NodeReCheckUnderBijectionMap(t *testing.T) {
	t.Parallel()

	// Query: Select(scanA, scanB) with result record{f: qA.qov, g: qB.qov}.
	qARef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType))
	qBRef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType))
	qA := expressions.ForEachQuantifier(qARef)
	qB := expressions.ForEachQuantifier(qBRef)
	querySelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(
			values.RecordConstructorField{Name: "f", Value: qA.GetFlowedObjectValue()},
			values.RecordConstructorField{Name: "g", Value: qB.GetFlowedObjectValue()},
		),
		[]expressions.Quantifier{qA, qB}, nil,
	)
	querySelectRef := expressions.InitialOf(querySelect)

	// Candidate: Select(scanB, scanA) — SWAPPED positional order — with result
	// record{f: cA.qov, g: cB.qov}. The only same-type child bijection is
	// qA↔cA, qB↔cB, i.e. query positions [0,1] → candidate positions [1,0].
	cBRef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType))
	cARef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType))
	cB := expressions.ForEachQuantifier(cBRef)
	cA := expressions.ForEachQuantifier(cARef)
	candidateSelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(
			values.RecordConstructorField{Name: "f", Value: cA.GetFlowedObjectValue()},
			values.RecordConstructorField{Name: "g", Value: cB.GetFlowedObjectValue()},
		),
		[]expressions.Quantifier{cB, cA}, nil,
	)
	candidateSelectRef := expressions.InitialOf(candidateSelect)

	mc := &testMatchCandidate{name: "idx_join_qov", traversal: NewTraversal(candidateSelectRef)}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	leafRule := NewMatchLeafRule()
	FireExpressionRuleWithMemo(leafRule, qARef, ctx, nil)
	FireExpressionRuleWithMemo(leafRule, qBRef, ctx, nil)

	FireExpressionRuleWithMemo(NewMatchIntermediateRule(), querySelectRef, ctx, nil)

	if len(GetPartialMatchesForCandidate(querySelectRef, mc)) == 0 {
		t.Fatal("expected a PartialMatch: the QOV-referencing result values are equal only under the bijection {qA→cA, qB→cB}; an empty-map node check rejects this valid match")
	}
}

// TestMatchIntermediateRule_QuantifierAttributesMustMatch pins the quantifier-edge
// check: Java pairs quantifiers via quantifier.semanticEquals, which compares the
// edge (kind, null-on-empty, strict-single), but EqualsWithoutChildren excludes
// it. A query Select over a plain ForEach leg must NOT match a candidate Select
// over a ForEach-NULL-ON-EMPTY leg — substituting the index would drop the
// null-extended row. Same scan, same (empty) result value, so the ONLY thing that
// differs is the quantifier edge; without the guard the node re-check passes and
// a wrong match is created.
func TestMatchIntermediateRule_QuantifierAttributesMustMatch(t *testing.T) {
	t.Parallel()

	queryScanRef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	querySelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{expressions.ForEachQuantifier(queryScanRef)}, nil,
	)
	querySelectRef := expressions.InitialOf(querySelect)

	// Candidate Select ranges over the SAME scan via a NULL-ON-EMPTY quantifier.
	candScanRef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	candidateSelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{expressions.ForEachNullOnEmptyQuantifier(candScanRef)}, nil,
	)
	candidateSelectRef := expressions.InitialOf(candidateSelect)

	mc := &testMatchCandidate{name: "idx_noe", traversal: NewTraversal(candidateSelectRef)}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	FireExpressionRuleWithMemo(NewMatchLeafRule(), queryScanRef, ctx, nil)
	FireExpressionRuleWithMemo(NewMatchIntermediateRule(), querySelectRef, ctx, nil)

	if n := len(GetPartialMatchesForCandidate(querySelectRef, mc)); n != 0 {
		t.Fatalf("a plain-ForEach Select must NOT match a NULL-ON-EMPTY Select (edge mismatch), got %d matches", n)
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

// ---------------------------------------------------------------------------
// Filter-vs-Select subsumption tests: predicate-to-Placeholder binding
// ---------------------------------------------------------------------------

// TestMatchIntermediate_FilterSubsumedBySelect_SinglePredicate verifies
// the core subsumption path: a query LogicalFilterExpression with one
// ComparisonPredicate (col0 = 5) matches a candidate SelectExpression
// with one Placeholder (col0). The PartialMatch should carry a
// parameter binding with an equality ComparisonRange.
func TestMatchIntermediate_FilterSubsumedBySelect_SinglePredicate(t *testing.T) {
	t.Parallel()

	// --- Query side: Filter([col0 = 5], Scan("T")) ---
	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryScanRef := expressions.InitialOf(queryScan)
	queryScanQ := expressions.ForEachQuantifier(queryScanRef)

	queryFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "col0"},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(5)),
			),
		},
		queryScanQ,
	)
	queryFilterRef := expressions.InitialOf(queryFilter)

	// --- Candidate side: Select(qov, [ForEach(Scan("T"))], [Placeholder(a0, col0)]) ---
	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachQuantifier(candidateScanRef)

	alias0 := values.UniqueCorrelationIdentifier()
	ph0 := predicates.NewPlaceholder(alias0, &values.FieldValue{Field: "col0"})

	candidateSelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{candidateScanQ},
		[]predicates.QueryPredicate{ph0},
	)
	candidateSelectRef := expressions.InitialOf(candidateSelect)
	traversal := NewTraversal(candidateSelectRef)

	mc := &testMatchCandidate{name: "idx_col0", traversal: traversal}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	// Seed leaf match on scan.
	leafRule := NewMatchLeafRule()
	FireExpressionRuleWithMemo(leafRule, queryScanRef, ctx, nil)
	if len(GetPartialMatchesForCandidate(queryScanRef, mc)) == 0 {
		t.Fatal("leaf PartialMatch not seeded on queryScanRef")
	}

	// Run intermediate rule on the filter.
	intermediateRule := NewMatchIntermediateRule()
	FireExpressionRuleWithMemo(intermediateRule, queryFilterRef, ctx, nil)

	// Verify a PartialMatch was created on the filter ref.
	pms := GetPartialMatchesForCandidate(queryFilterRef, mc)
	if len(pms) == 0 {
		t.Fatal("expected at least one PartialMatch from filter-vs-select subsumption, got 0")
	}

	pmi := pms[0].(*PartialMatchImpl)

	// Verify candidateRef points to the candidate select ref.
	if pmi.GetCandidateRef() != candidateSelectRef {
		t.Fatal("PartialMatch candidateRef does not match candidateSelectRef")
	}

	// Verify the alias map contains the quantifier mapping.
	am := pmi.GetBoundAliasMap()
	if am == nil || am.IsEmpty() {
		t.Fatal("expected non-empty alias map")
	}

	// Verify parameter bindings.
	rmi := pmi.GetRegularMatchInfo()
	if rmi == nil {
		t.Fatal("expected non-nil RegularMatchInfo")
	}
	pbm := rmi.GetParameterBindingMap()
	cr, ok := pbm[alias0]
	if !ok {
		t.Fatalf("expected parameter binding for alias %v, got none", alias0)
	}
	if !cr.IsEquality() {
		t.Fatalf("expected equality ComparisonRange for col0 = 5, got range type %v", cr.GetRangeType())
	}
}

// TestMatchIntermediate_FilterSubsumedBySelect_EdgeMismatchRejected pins the
// quantifier-edge check on the SUBSUMPTION path (matchSingleSourceAgainstSelect),
// which the structural bijection path bypasses. It is the SinglePredicate setup
// with the candidate leg made NULL-ON-EMPTY: identical otherwise, so
// SinglePredicate proves this shape DOES match without the edge guard. With the
// guard the plain-ForEach query filter must NOT subsume the NULL-ON-EMPTY index
// candidate — else a substitution would drop the null-extended row.
func TestMatchIntermediate_FilterSubsumedBySelect_EdgeMismatchRejected(t *testing.T) {
	t.Parallel()

	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryScanRef := expressions.InitialOf(queryScan)
	queryScanQ := expressions.ForEachQuantifier(queryScanRef)
	queryFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "col0"},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(5)),
			),
		},
		queryScanQ,
	)
	queryFilterRef := expressions.InitialOf(queryFilter)

	// Candidate Select ranges over the scan via a NULL-ON-EMPTY quantifier — the
	// ONLY difference from the passing SinglePredicate case.
	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachNullOnEmptyQuantifier(candidateScanRef)
	alias0 := values.UniqueCorrelationIdentifier()
	ph0 := predicates.NewPlaceholder(alias0, &values.FieldValue{Field: "col0"})
	candidateSelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{candidateScanQ},
		[]predicates.QueryPredicate{ph0},
	)
	candidateSelectRef := expressions.InitialOf(candidateSelect)

	mc := &testMatchCandidate{name: "idx_col0_noe", traversal: NewTraversal(candidateSelectRef)}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	FireExpressionRuleWithMemo(NewMatchLeafRule(), queryScanRef, ctx, nil)
	FireExpressionRuleWithMemo(NewMatchIntermediateRule(), queryFilterRef, ctx, nil)

	if n := len(GetPartialMatchesForCandidate(queryFilterRef, mc)); n != 0 {
		t.Fatalf("a plain-ForEach filter must NOT subsume a NULL-ON-EMPTY index candidate via the subsumption path, got %d matches", n)
	}
}

// TestMatchIntermediate_SubsumptionChildMatchOrderIndependent pins the
// subsumption path's order-independence: when the query child reference holds
// several partial matches for the same candidate child (a conflicting one first,
// a compatible one second), matchSingleSourceAgainstSelect must try each and use
// the one that COMPOSES with the quantifier mapping — not commit to the first and
// suppress a valid index match. Two matches with the same (queryExpr, candidateRef)
// are seeded via the direct Reference.AddPartialMatch (bypassing the dedup wrapper)
// to force the multi-match shape.
func TestMatchIntermediate_SubsumptionChildMatchOrderIndependent(t *testing.T) {
	t.Parallel()

	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryScanRef := expressions.InitialOf(queryScan)
	queryScanQ := expressions.ForEachQuantifier(queryScanRef)
	queryFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "col0"},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(5)),
			),
		},
		queryScanQ,
	)
	queryFilterRef := expressions.InitialOf(queryFilter)

	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachQuantifier(candidateScanRef)
	alias0 := values.UniqueCorrelationIdentifier()
	ph0 := predicates.NewPlaceholder(alias0, &values.FieldValue{Field: "col0"})
	candidateSelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{candidateScanQ},
		[]predicates.QueryPredicate{ph0},
	)
	candidateSelectRef := expressions.InitialOf(candidateSelect)

	mc := &testMatchCandidate{name: "idx_col0_order", traversal: NewTraversal(candidateSelectRef)}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	craft := func(boundMap *AliasMap) *PartialMatchImpl {
		candResult := candidateScan.GetResultValue()
		mmm := buildMatchMaxMatchMap(candResult, candResult, boundMap)
		mi := NewRegularMatchInfo(nil, boundMap, nil, nil, mmm, EmptyGroupByMappings(), nil, nil)
		return NewPartialMatch(boundMap, mc, queryScanRef, queryScan, candidateScanRef, mi)
	}
	// First match: bound map already fixes the query quantifier alias to a WRONG
	// leg, so composing with {queryScanQ → candidateScanQ} conflicts.
	wrong := values.UniqueCorrelationIdentifier()
	first := craft(AliasMapOfAliases(queryScanQ.GetAlias(), wrong))
	// Second match: empty bound map, composes cleanly.
	second := craft(EmptyAliasMap())
	if !queryScanRef.AddPartialMatch(mc, first) {
		t.Fatal("failed to seed the first (conflicting) child match")
	}
	if !queryScanRef.AddPartialMatch(mc, second) {
		t.Fatal("failed to seed the second (compatible) child match")
	}

	FireExpressionRuleWithMemo(NewMatchIntermediateRule(), queryFilterRef, ctx, nil)

	if n := len(GetPartialMatchesForCandidate(queryFilterRef, mc)); n == 0 {
		t.Fatal("expected a match via the compatible (second) child match; committing to the conflicting first suppresses it")
	}
}

// TestMatchIntermediate_FilterSubsumedBySelect_MultiplePredicates
// verifies that a query with two ComparisonPredicates (col0 = 5 AND
// col1 > 10) correctly binds both candidate Placeholders.
func TestMatchIntermediate_FilterSubsumedBySelect_MultiplePredicates(t *testing.T) {
	t.Parallel()

	// --- Query: Filter([col0 = 5, col1 > 10], Scan("T")) ---
	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryScanRef := expressions.InitialOf(queryScan)
	queryScanQ := expressions.ForEachQuantifier(queryScanRef)

	queryFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "col0"},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(5)),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "col1"},
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(10)),
			),
		},
		queryScanQ,
	)
	queryFilterRef := expressions.InitialOf(queryFilter)

	// --- Candidate: Select(qov, [ForEach(Scan("T"))], [Placeholder(a0, col0), Placeholder(a1, col1)]) ---
	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachQuantifier(candidateScanRef)

	alias0 := values.UniqueCorrelationIdentifier()
	alias1 := values.UniqueCorrelationIdentifier()
	ph0 := predicates.NewPlaceholder(alias0, &values.FieldValue{Field: "col0"})
	ph1 := predicates.NewPlaceholder(alias1, &values.FieldValue{Field: "col1"})

	candidateSelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{candidateScanQ},
		[]predicates.QueryPredicate{ph0, ph1},
	)
	candidateSelectRef := expressions.InitialOf(candidateSelect)
	traversal := NewTraversal(candidateSelectRef)

	mc := &testMatchCandidate{name: "idx_col0_col1", traversal: traversal}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	// Seed leaf match.
	leafRule := NewMatchLeafRule()
	FireExpressionRuleWithMemo(leafRule, queryScanRef, ctx, nil)

	// Run intermediate rule.
	intermediateRule := NewMatchIntermediateRule()
	FireExpressionRuleWithMemo(intermediateRule, queryFilterRef, ctx, nil)

	pms := GetPartialMatchesForCandidate(queryFilterRef, mc)
	if len(pms) == 0 {
		t.Fatal("expected PartialMatch from multi-predicate subsumption, got 0")
	}

	rmi := pms[0].(*PartialMatchImpl).GetRegularMatchInfo()
	pbm := rmi.GetParameterBindingMap()

	// Verify alias0 binding: equality.
	cr0, ok := pbm[alias0]
	if !ok {
		t.Fatalf("expected parameter binding for alias %v (col0)", alias0)
	}
	if !cr0.IsEquality() {
		t.Fatalf("col0: expected equality range, got %v", cr0.GetRangeType())
	}

	// Verify alias1 binding: inequality.
	cr1, ok := pbm[alias1]
	if !ok {
		t.Fatalf("expected parameter binding for alias %v (col1)", alias1)
	}
	if !cr1.IsInequality() {
		t.Fatalf("col1: expected inequality range, got %v", cr1.GetRangeType())
	}
}

// TestMatchIntermediate_FilterSubsumedBySelect_UnmatchedPlaceholder
// verifies that a candidate Placeholder for a column not filtered by
// the query gets an empty (unconstrained) ComparisonRange binding.
func TestMatchIntermediate_FilterSubsumedBySelect_UnmatchedPlaceholder(t *testing.T) {
	t.Parallel()

	// Query: Filter([col0 = 5], Scan("T"))
	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryScanRef := expressions.InitialOf(queryScan)
	queryScanQ := expressions.ForEachQuantifier(queryScanRef)

	queryFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "col0"},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(5)),
			),
		},
		queryScanQ,
	)
	queryFilterRef := expressions.InitialOf(queryFilter)

	// Candidate: Select(..., [Placeholder(a0, col0), Placeholder(a2, col2)])
	// col2 is NOT in the query predicates.
	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachQuantifier(candidateScanRef)

	alias0 := values.UniqueCorrelationIdentifier()
	alias2 := values.UniqueCorrelationIdentifier()
	ph0 := predicates.NewPlaceholder(alias0, &values.FieldValue{Field: "col0"})
	ph2 := predicates.NewPlaceholder(alias2, &values.FieldValue{Field: "col2"})

	candidateSelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{candidateScanQ},
		[]predicates.QueryPredicate{ph0, ph2},
	)
	candidateSelectRef := expressions.InitialOf(candidateSelect)
	traversal := NewTraversal(candidateSelectRef)

	mc := &testMatchCandidate{name: "idx_col0_col2", traversal: traversal}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	leafRule := NewMatchLeafRule()
	FireExpressionRuleWithMemo(leafRule, queryScanRef, ctx, nil)

	intermediateRule := NewMatchIntermediateRule()
	FireExpressionRuleWithMemo(intermediateRule, queryFilterRef, ctx, nil)

	pms := GetPartialMatchesForCandidate(queryFilterRef, mc)
	if len(pms) == 0 {
		t.Fatal("expected PartialMatch even with unmatched Placeholder, got 0")
	}

	rmi := pms[0].(*PartialMatchImpl).GetRegularMatchInfo()
	pbm := rmi.GetParameterBindingMap()

	// alias0 should be bound (equality).
	cr0, ok := pbm[alias0]
	if !ok {
		t.Fatal("expected parameter binding for alias0 (col0)")
	}
	if !cr0.IsEquality() {
		t.Fatalf("col0: expected equality range, got %v", cr0.GetRangeType())
	}

	// alias2 should be unbound (empty range).
	cr2, ok := pbm[alias2]
	if !ok {
		t.Fatal("expected parameter binding for alias2 (col2) — even if empty")
	}
	if !cr2.IsEmpty() {
		t.Fatalf("col2: expected empty (unconstrained) range, got %v", cr2.GetRangeType())
	}
}

// TestMatchIntermediate_FilterSubsumedBySelect_NoColumnMatch verifies
// that when a query filters on col_x but the candidate's only
// Placeholder is for col0, the subsumption still succeeds (the
// Placeholder is unbound) — the scan-level match still exists, and
// the index can be used with a full scan + residual filter.
func TestMatchIntermediate_FilterSubsumedBySelect_NoColumnMatch(t *testing.T) {
	t.Parallel()

	// Query: Filter([col_x = 42], Scan("T"))
	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryScanRef := expressions.InitialOf(queryScan)
	queryScanQ := expressions.ForEachQuantifier(queryScanRef)

	queryFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "col_x"},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
			),
		},
		queryScanQ,
	)
	queryFilterRef := expressions.InitialOf(queryFilter)

	// Candidate: Select(..., [Placeholder(a0, col0)])
	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachQuantifier(candidateScanRef)

	alias0 := values.UniqueCorrelationIdentifier()
	ph0 := predicates.NewPlaceholder(alias0, &values.FieldValue{Field: "col0"})

	candidateSelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{candidateScanQ},
		[]predicates.QueryPredicate{ph0},
	)
	candidateSelectRef := expressions.InitialOf(candidateSelect)
	traversal := NewTraversal(candidateSelectRef)

	mc := &testMatchCandidate{name: "idx_col0", traversal: traversal}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	leafRule := NewMatchLeafRule()
	FireExpressionRuleWithMemo(leafRule, queryScanRef, ctx, nil)

	intermediateRule := NewMatchIntermediateRule()
	FireExpressionRuleWithMemo(intermediateRule, queryFilterRef, ctx, nil)

	pms := GetPartialMatchesForCandidate(queryFilterRef, mc)
	if len(pms) == 0 {
		t.Fatal("expected PartialMatch even when no column matches — Placeholder stays unbound")
	}

	rmi := pms[0].(*PartialMatchImpl).GetRegularMatchInfo()
	pbm := rmi.GetParameterBindingMap()

	// The Placeholder for col0 is unbound — empty range.
	cr0, ok := pbm[alias0]
	if !ok {
		t.Fatal("expected parameter binding for alias0 (col0) — even if empty")
	}
	if !cr0.IsEmpty() {
		t.Fatalf("col0: expected empty range (no column match), got %v", cr0.GetRangeType())
	}
}

// TestMatchIntermediate_FilterSubsumedBySelect_NoChildMatch verifies
// that the filter-vs-select subsumption requires a child PartialMatch
// on the scan. Without seeding a leaf match, no intermediate match
// should be created.
func TestMatchIntermediate_FilterSubsumedBySelect_NoChildMatch(t *testing.T) {
	t.Parallel()

	// Query: Filter([col0 = 5], Scan("T"))
	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryScanRef := expressions.InitialOf(queryScan)
	queryScanQ := expressions.ForEachQuantifier(queryScanRef)

	queryFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "col0"},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(5)),
			),
		},
		queryScanQ,
	)
	queryFilterRef := expressions.InitialOf(queryFilter)

	// Candidate: Select(..., [Placeholder(a0, col0)])
	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateScanQ := expressions.ForEachQuantifier(candidateScanRef)

	alias0 := values.UniqueCorrelationIdentifier()
	ph0 := predicates.NewPlaceholder(alias0, &values.FieldValue{Field: "col0"})

	candidateSelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{candidateScanQ},
		[]predicates.QueryPredicate{ph0},
	)
	candidateSelectRef := expressions.InitialOf(candidateSelect)
	traversal := NewTraversal(candidateSelectRef)

	mc := &testMatchCandidate{name: "idx_col0", traversal: traversal}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	// Do NOT seed the leaf match.

	intermediateRule := NewMatchIntermediateRule()
	FireExpressionRuleWithMemo(intermediateRule, queryFilterRef, ctx, nil)

	pms := GetPartialMatchesForCandidate(queryFilterRef, mc)
	if len(pms) != 0 {
		t.Fatalf("expected 0 PartialMatches without child scan match, got %d", len(pms))
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
