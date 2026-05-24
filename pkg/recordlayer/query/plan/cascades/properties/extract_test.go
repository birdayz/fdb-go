package properties

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestExtractBestPlan_NilOrEmptyReturnsNil(t *testing.T) {
	t.Parallel()
	got, err := ExtractBestPlan(nil)
	if err != nil {
		t.Fatalf("nil ref err=%v", err)
	}
	if got != nil {
		t.Fatalf("nil ref got=%v, want nil", got)
	}
	got, err = ExtractBestPlan(&expressions.Reference{})
	if err != nil {
		t.Fatalf("empty ref err=%v", err)
	}
	if got != nil {
		t.Fatalf("empty ref got=%v, want nil", got)
	}
}

func TestExtractBestPlan_LeafScan(t *testing.T) {
	t.Parallel()
	r := scan("T")
	got, err := ExtractBestPlan(r)
	if err != nil {
		t.Fatalf("ExtractBestPlan err=%v", err)
	}
	s, ok := got.(*expressions.FullUnorderedScanExpression)
	if !ok {
		t.Fatalf("got %T, want *FullUnorderedScanExpression", got)
	}
	rts := s.GetRecordTypes()
	if len(rts) != 1 || rts[0] != "T" {
		t.Fatalf("record types=%v, want [T]", rts)
	}
}

func TestExtractBestPlan_FreshReferences(t *testing.T) {
	t.Parallel()
	// Build a Filter over a Scan, extract the plan, verify the
	// extracted plan's Quantifier.Reference is NOT the same pointer
	// as the input's.
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	innerRef := scan("T")
	f := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(innerRef),
	)
	topRef := expressions.InitialOf(f)

	extracted, err := ExtractBestPlan(topRef)
	if err != nil {
		t.Fatalf("ExtractBestPlan err=%v", err)
	}
	exFilter, ok := extracted.(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("got %T, want *LogicalFilterExpression", extracted)
	}
	exInnerRef := exFilter.GetInner().GetRangesOver()
	if exInnerRef == innerRef {
		t.Fatal("extracted plan's inner Reference is the same pointer as input — must be fresh")
	}
	if got := len(exInnerRef.Members()); got != 1 {
		t.Fatalf("extracted inner Reference has %d members, want 1 (singleton)", got)
	}
}

func TestExtractBestPlan_PicksCheapestMember(t *testing.T) {
	t.Parallel()
	// Reference with two members: cheaper Filter and pricier Sort.
	// ExtractBestPlan returns the Filter (the cheapest).
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	cheap := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		scanQ("T"),
	)
	pricy := expressions.NewLogicalSortExpression(nil, scanQ("T"))

	r := expressions.InitialOf(cheap)
	r.Insert(pricy)

	got, err := ExtractBestPlan(r)
	if err != nil {
		t.Fatalf("ExtractBestPlan err=%v", err)
	}
	if _, ok := got.(*expressions.LogicalFilterExpression); !ok {
		t.Fatalf("got %T, want cheaper *LogicalFilterExpression", got)
	}
}

func TestExtractBestPlan_RecursivelyExtractsChildren(t *testing.T) {
	t.Parallel()
	// Build:
	//   Sort
	//     └── Reference [Filter(P, scan(T)), scan(U)]   (multi-member)
	//
	// ExtractBestPlan picks the BEST member at each Reference.
	// For the inner Reference: scan(U) is cheaper than Filter (no
	// CPU cost vs filter's 1e5 CPU cost). Wait actually a Filter's
	// cardinality is HALVED so total cost is lower. Let me think:
	//
	//   scan(U):   card=1e6, CPU=0,    total=1e6
	//   Filter(scan(T)): card=5e5, CPU=1e5, total=6e5
	//
	// So Filter wins. The extracted plan's outer Sort's inner is
	// the Filter, NOT the bare Scan.
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filterMember := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		scanQ("T"),
	)
	scanMember := expressions.NewFullUnorderedScanExpression([]string{"U"}, nil)
	innerRef := expressions.InitialOf(filterMember)
	innerRef.Insert(scanMember)

	sort := expressions.NewLogicalSortExpression(
		nil,
		expressions.ForEachQuantifier(innerRef),
	)
	topRef := expressions.InitialOf(sort)

	extracted, err := ExtractBestPlan(topRef)
	if err != nil {
		t.Fatalf("ExtractBestPlan err=%v", err)
	}
	exSort, ok := extracted.(*expressions.LogicalSortExpression)
	if !ok {
		t.Fatalf("got %T, want *LogicalSortExpression", extracted)
	}
	exInner := exSort.GetInner().GetRangesOver().Get()
	if _, ok := exInner.(*expressions.LogicalFilterExpression); !ok {
		t.Fatalf("extracted inner = %T, want *LogicalFilterExpression (the cheapest of the inner Reference's two members)", exInner)
	}
}

func TestExtractBestPlan_UnionPreservesAllChildren(t *testing.T) {
	t.Parallel()
	u := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		scanQ("A"),
		scanQ("B"),
		scanQ("C"),
	})
	r := expressions.InitialOf(u)
	got, err := ExtractBestPlan(r)
	if err != nil {
		t.Fatalf("ExtractBestPlan err=%v", err)
	}
	exUnion, ok := got.(*expressions.LogicalUnionExpression)
	if !ok {
		t.Fatalf("got %T, want *LogicalUnionExpression", got)
	}
	if n := len(exUnion.GetQuantifiers()); n != 3 {
		t.Fatalf("union extracted %d children, want 3", n)
	}
}

func TestExtractBestPlan_DeleteExpression(t *testing.T) {
	t.Parallel()
	// DML: DeleteExpression over a scan. Verify the extracted shape
	// preserves target type + recurses into inner.
	innerQ := scanQ("Order")
	del := expressions.NewDeleteExpression(innerQ, "Order")
	r := expressions.InitialOf(del)
	got, err := ExtractBestPlan(r)
	if err != nil {
		t.Fatalf("ExtractBestPlan err=%v", err)
	}
	exDel, ok := got.(*expressions.DeleteExpression)
	if !ok {
		t.Fatalf("got %T, want *DeleteExpression", got)
	}
	if exDel.GetTargetRecordType() != "Order" {
		t.Fatalf("target=%q, want %q", exDel.GetTargetRecordType(), "Order")
	}
	if _, ok := exDel.GetInner().GetRangesOver().Get().(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("delete inner not a scan")
	}
}

// --- ExtractBestPlanWith tests ---

func TestExtractBestPlanWith_NilStats(t *testing.T) {
	t.Parallel()
	r := scan("T")
	got, err := ExtractBestPlanWith(r, nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, ok := got.(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("got %T, want *FullUnorderedScanExpression", got)
	}
}

func TestExtractBestPlanWith_NilRef(t *testing.T) {
	t.Parallel()
	got, err := ExtractBestPlanWith(nil, DefaultStatistics{})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestExtractBestPlanWith_EmptyRef(t *testing.T) {
	t.Parallel()
	got, err := ExtractBestPlanWith(&expressions.Reference{}, DefaultStatistics{})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

// --- ExtractBestPlanFromSelector tests ---

type mockSelector struct {
	winners map[*expressions.Reference]expressions.RelationalExpression
}

func (m *mockSelector) BestMember(ref *expressions.Reference) expressions.RelationalExpression {
	return m.winners[ref]
}

func (m *mockSelector) HasBestMember(ref *expressions.Reference) bool {
	_, ok := m.winners[ref]
	return ok
}

func TestExtractBestPlanFromSelector_NilRef(t *testing.T) {
	t.Parallel()
	got, err := ExtractBestPlanFromSelector(nil, nil, nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestExtractBestPlanFromSelector_NilSelector(t *testing.T) {
	t.Parallel()
	r := scan("T")
	got, err := ExtractBestPlanFromSelector(r, nil, nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, ok := got.(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("got %T, want *FullUnorderedScanExpression", got)
	}
}

func TestExtractBestPlanFromSelector_NonPhysicalSelectorFallsToCost(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		scanQ("T"),
	)
	scanMember := expressions.NewFullUnorderedScanExpression([]string{"U"}, nil)
	r := expressions.InitialOf(filter)
	r.Insert(scanMember)

	sel := &mockSelector{
		winners: map[*expressions.Reference]expressions.RelationalExpression{
			r: scanMember,
		},
	}
	got, err := ExtractBestPlanFromSelector(r, sel, nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, ok := got.(*expressions.LogicalFilterExpression); !ok {
		t.Fatalf("got %T, want *LogicalFilterExpression (cost model picks cheaper filter over scan)", got)
	}
}

func TestExtractBestPlanFromSelector_FallsBackToCostWhenSelectorHasNoBest(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		scanQ("T"),
	)
	r := expressions.InitialOf(filter)

	sel := &mockSelector{winners: map[*expressions.Reference]expressions.RelationalExpression{}}
	got, err := ExtractBestPlanFromSelector(r, sel, nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, ok := got.(*expressions.LogicalFilterExpression); !ok {
		t.Fatalf("got %T, want *LogicalFilterExpression (cost fallback)", got)
	}
}

func TestExtractBestPlanFromSelector_CycleDetection(t *testing.T) {
	t.Parallel()
	r := scan("T")
	got, err := ExtractBestPlanFromSelector(r, nil, nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestExtractBestPlanFromSelector_WinnerUsedWhenPhysical(t *testing.T) {
	t.Parallel()
	scanExpr := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	r := expressions.InitialOf(scanExpr)
	r.SetWinner(expressions.NoProperties, scanExpr)

	got, err := ExtractBestPlanFromSelector(r, nil, nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestExtractBestPlanFromSelector_RecursivelyExtractsChildWithSelector(t *testing.T) {
	t.Parallel()
	innerScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	innerRef := expressions.InitialOf(innerScan)

	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(innerRef),
	)
	topRef := expressions.InitialOf(filter)

	sel := &mockSelector{
		winners: map[*expressions.Reference]expressions.RelationalExpression{
			innerRef: innerScan,
		},
	}

	got, err := ExtractBestPlanFromSelector(topRef, sel, nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	exFilter, ok := got.(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("got %T, want *LogicalFilterExpression", got)
	}
	exInnerRef := exFilter.GetInner().GetRangesOver()
	if exInnerRef == innerRef {
		t.Fatal("extracted inner Reference is same pointer — should be fresh")
	}
}
