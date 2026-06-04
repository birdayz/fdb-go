package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// testMatchCandidate is a MatchCandidate with a real Traversal built
// from an expression tree. Used to exercise MatchLeafRule against
// candidate structures.
type testMatchCandidate struct {
	name      string
	traversal *Traversal
}

func (c *testMatchCandidate) CandidateName() string                              { return c.name }
func (c *testMatchCandidate) GetColumnNames() []string                           { return nil }
func (c *testMatchCandidate) GetSargableAliases() []values.CorrelationIdentifier { return nil }
func (c *testMatchCandidate) GetRecordTypes() []string                           { return nil }
func (c *testMatchCandidate) IsUnique() bool                                     { return false }
func (c *testMatchCandidate) GetTraversal() *Traversal                           { return c.traversal }
func (c *testMatchCandidate) ComputeBoundParameterPrefixMap(
	bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	// Value-index-like: every non-empty bound alias is consumable as a scan
	// prefix constraint. matchSingleSourceAgainstSelect reconciles its sargable
	// bindings against this map (a binding absent here becomes a residual), so a
	// faithful stub must surface the bound aliases — returning nil would
	// reclassify every match as a residual.
	prefix := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
	for k, v := range bindings {
		if v != nil && !v.IsEmpty() {
			prefix[k] = v
		}
	}
	return prefix
}

func (c *testMatchCandidate) ToScanPlan(
	_ map[values.CorrelationIdentifier]*predicates.ComparisonRange, _ bool,
) plans.RecordQueryPlan {
	return nil
}

// testPlanContextForMatching is a PlanContext that holds match candidates.
type testPlanContextForMatching struct {
	candidates []MatchCandidate
}

func (t testPlanContextForMatching) GetPlannerConfiguration() PlannerConfiguration {
	return DefaultPlannerConfiguration()
}
func (t testPlanContextForMatching) GetMatchCandidates() []MatchCandidate { return t.candidates }
func (t testPlanContextForMatching) GetPrimaryKeyColumns(string) []string { return nil }

func TestMatchLeafRule_MatchingScan(t *testing.T) {
	t.Parallel()

	// Query side: a FullUnorderedScanExpression over record type "T".
	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryRef := expressions.InitialOf(queryScan)

	// Candidate side: an equivalent FullUnorderedScanExpression,
	// wrapped in a Traversal via a MatchCandidate.
	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateRef := expressions.InitialOf(candidateScan)
	traversal := NewTraversal(candidateRef)

	mc := &testMatchCandidate{name: "idx_t", traversal: traversal}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	rule := NewMatchLeafRule()
	FireExpressionRuleWithMemo(rule, queryRef, ctx, nil)

	// Verify: a PartialMatch should be stored on queryRef for the
	// candidate.
	pms := GetPartialMatchesForCandidate(queryRef, mc)
	if len(pms) == 0 {
		t.Fatal("expected at least one PartialMatch, got 0")
	}
	pm := pms[0]
	if pm.GetMatchCandidate() != mc {
		t.Fatalf("PartialMatch candidate = %v, want %v", pm.GetMatchCandidate(), mc)
	}
	mi := pm.GetMatchInfo()
	if mi == nil {
		t.Fatal("PartialMatch has nil MatchInfo")
	}
	if !mi.IsRegular() {
		t.Fatal("expected RegularMatchInfo, got adjusted")
	}
}

func TestMatchLeafRule_DifferentRecordType_NoMatch(t *testing.T) {
	t.Parallel()

	// Query scans type "A", candidate scans type "B".
	queryScan := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	queryRef := expressions.InitialOf(queryScan)

	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	candidateRef := expressions.InitialOf(candidateScan)
	traversal := NewTraversal(candidateRef)

	mc := &testMatchCandidate{name: "idx_b", traversal: traversal}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	rule := NewMatchLeafRule()
	FireExpressionRuleWithMemo(rule, queryRef, ctx, nil)

	pms := GetPartialMatchesForCandidate(queryRef, mc)
	if len(pms) != 0 {
		t.Fatalf("expected 0 PartialMatches for mismatched record types, got %d", len(pms))
	}
}

func TestMatchLeafRule_NonLeafSkipped(t *testing.T) {
	t.Parallel()

	// Build a non-leaf expression: Filter(scan).
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		scanQ,
	)
	queryRef := expressions.InitialOf(filter)

	// Candidate: a leaf scan that could match the inner scan but
	// should NOT match the filter (non-leaf).
	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateRef := expressions.InitialOf(candidateScan)
	traversal := NewTraversal(candidateRef)

	mc := &testMatchCandidate{name: "idx_t", traversal: traversal}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	rule := NewMatchLeafRule()
	FireExpressionRuleWithMemo(rule, queryRef, ctx, nil)

	pms := GetPartialMatchesForCandidate(queryRef, mc)
	if len(pms) != 0 {
		t.Fatalf("MatchLeafRule should skip non-leaf expressions; got %d partial matches", len(pms))
	}
}

func TestMatchLeafRule_NoCandidates_NoPanic(t *testing.T) {
	t.Parallel()

	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryRef := expressions.InitialOf(queryScan)

	// Empty context: no candidates.
	ctx := testPlanContextForMatching{candidates: nil}

	rule := NewMatchLeafRule()
	// Should not panic.
	FireExpressionRuleWithMemo(rule, queryRef, ctx, nil)

	raw := queryRef.GetAllPartialMatches()
	if len(raw) != 0 {
		t.Fatalf("expected 0 partial matches with no candidates, got %d", len(raw))
	}
}

func TestMatchLeafRule_NilTraversal_Skipped(t *testing.T) {
	t.Parallel()

	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryRef := expressions.InitialOf(queryScan)

	// Candidate with nil traversal.
	mc := &testMatchCandidate{name: "no_trav", traversal: nil}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	rule := NewMatchLeafRule()
	FireExpressionRuleWithMemo(rule, queryRef, ctx, nil)

	pms := GetPartialMatchesForCandidate(queryRef, mc)
	if len(pms) != 0 {
		t.Fatalf("expected 0 partial matches when traversal is nil, got %d", len(pms))
	}
}

func TestMatchLeafRule_MultipleCandidates(t *testing.T) {
	t.Parallel()

	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryRef := expressions.InitialOf(queryScan)

	// Two candidates, both matching.
	makeCand := func(name string) *testMatchCandidate {
		s := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		ref := expressions.InitialOf(s)
		return &testMatchCandidate{name: name, traversal: NewTraversal(ref)}
	}

	mc1 := makeCand("idx1")
	mc2 := makeCand("idx2")
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc1, mc2}}

	rule := NewMatchLeafRule()
	FireExpressionRuleWithMemo(rule, queryRef, ctx, nil)

	pms1 := GetPartialMatchesForCandidate(queryRef, mc1)
	pms2 := GetPartialMatchesForCandidate(queryRef, mc2)
	if len(pms1) != 1 {
		t.Fatalf("expected 1 partial match for mc1, got %d", len(pms1))
	}
	if len(pms2) != 1 {
		t.Fatalf("expected 1 partial match for mc2, got %d", len(pms2))
	}
}

func TestMatchLeafRule_PartialMatchFields(t *testing.T) {
	t.Parallel()

	queryScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	queryRef := expressions.InitialOf(queryScan)

	candidateScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	candidateRef := expressions.InitialOf(candidateScan)
	traversal := NewTraversal(candidateRef)

	mc := &testMatchCandidate{name: "primary", traversal: traversal}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{mc}}

	rule := NewMatchLeafRule()
	FireExpressionRuleWithMemo(rule, queryRef, ctx, nil)

	pms := GetPartialMatchesForCandidate(queryRef, mc)
	if len(pms) != 1 {
		t.Fatalf("expected 1 partial match, got %d", len(pms))
	}

	pmi, ok := pms[0].(*PartialMatchImpl)
	if !ok {
		t.Fatalf("expected *PartialMatchImpl, got %T", pms[0])
	}

	// Verify query ref is the query's reference.
	if pmi.GetQueryRef() != queryRef {
		t.Fatal("PartialMatch queryRef does not match the query Reference")
	}

	// Verify query expression is the query scan.
	if pmi.GetQueryExpression() != queryScan {
		t.Fatal("PartialMatch queryExpression does not match the query scan")
	}

	// Verify candidate ref is the candidate's leaf reference.
	if pmi.GetCandidateRef() != candidateRef {
		t.Fatal("PartialMatch candidateRef does not match the candidate Reference")
	}

	// Verify the alias map is empty.
	am := pmi.GetBoundAliasMap()
	if am == nil || !am.IsEmpty() {
		t.Fatal("expected empty bound alias map for leaf match")
	}

	// Verify RegularMatchInfo.
	rmi := pmi.GetRegularMatchInfo()
	if rmi == nil {
		t.Fatal("expected non-nil RegularMatchInfo")
	}
	if len(rmi.GetParameterBindingMap()) != 0 {
		t.Fatal("expected empty parameter binding map")
	}
}
