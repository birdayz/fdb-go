package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func extractionRowType(_ string) values.Type {
	return values.NewRecordType("ExtractionRow", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
}

func mustExtractionConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct plan-extraction fixture: " + err.Error())
	}
	return value
}

// extractionScan returns a Reference holding one exact FullUnorderedScan over
// the given record type. These extraction tests only need a single-record-type
// leaf; spelling its flowed type prevents malformed nil layouts from hiding at
// the plan-extraction boundary.
func extractionScan(t testing.TB, recordType string) *expressions.Reference {
	t.Helper()
	return expressions.InitialOf(mustFullUnorderedScan(
		t, []string{recordType}, extractionRowType(recordType)))
}

// extractionScanQ wraps extractionScan in a ForEach Quantifier.
func extractionScanQ(t testing.TB, recordType string) expressions.Quantifier {
	t.Helper()
	return expressions.ForEachQuantifier(extractionScan(t, recordType))
}

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
	r := extractionScan(t, "T")
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
	innerRef := extractionScan(t, "T")
	f := mustExtractionConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(innerRef),
	))
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
	cheap := mustExtractionConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		extractionScanQ(t, "T"),
	))
	pricy := mustExtractionConstruct(expressions.NewLogicalSortExpression(nil, extractionScanQ(t, "T")))

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
	filterMember := mustExtractionConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		extractionScanQ(t, "T"),
	))
	scanMember := mustFullUnorderedScan(t, []string{"U"}, extractionRowType("U"))
	innerRef := expressions.InitialOf(filterMember)
	innerRef.Insert(scanMember)

	sort := mustExtractionConstruct(expressions.NewLogicalSortExpression(
		nil,
		expressions.ForEachQuantifier(innerRef),
	))
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
	u := mustExtractionConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		extractionScanQ(t, "A"),
		extractionScanQ(t, "B"),
		extractionScanQ(t, "C"),
	}))
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
	innerQ := extractionScanQ(t, "Order")
	del := mustExtractionConstruct(expressions.NewDeleteExpression(innerQ, "Order"))
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
	r := extractionScan(t, "T")
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
	got, err := ExtractBestPlanWith(nil, properties.DefaultStatistics{})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestExtractBestPlanWith_EmptyRef(t *testing.T) {
	t.Parallel()
	got, err := ExtractBestPlanWith(&expressions.Reference{}, properties.DefaultStatistics{})
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
	r := extractionScan(t, "T")
	got, err := ExtractBestPlanFromSelector(r, nil, nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != nil {
		t.Fatalf("got %T, want nil: selector extraction has no physical member", got)
	}
}

// The selector deliberately names a logical member and the ordinary cost
// model ranks that logical Filter below the physical Scan. RFC-224's fallback
// must filter to physical candidates BEFORE comparing, or extraction and its
// verifier disagree and the returned root is not executable.
func TestExtractBestPlanFromSelector_NonPhysicalSelectorFallsToPhysicalCost(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := mustExtractionConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		extractionScanQ(t, "T"),
	))
	scanMember := mustExtractionConstruct(plans.NewRecordQueryScanPlan(
		[]string{"T"}, extractionRowType("T"), false))
	r := expressions.InitialOf(filter)
	r.Insert(scanMember)

	sel := &mockSelector{
		winners: map[*expressions.Reference]expressions.RelationalExpression{
			r: filter,
		},
	}
	got, err := ExtractBestPlanFromSelector(r, sel, nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, ok := got.(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("got %T, want physical Scan fallback", got)
	}
}

func TestExtractBestPlanFromSelector_FallsBackToCostWhenSelectorHasNoBest(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := mustExtractionConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		extractionScanQ(t, "T"),
	))
	r := expressions.InitialOf(filter)

	sel := &mockSelector{winners: map[*expressions.Reference]expressions.RelationalExpression{}}
	got, err := ExtractBestPlanFromSelector(r, sel, nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != nil {
		t.Fatalf("got %T, want nil when no physical fallback exists", got)
	}
}

func TestExtractBestPlanFromSelector_SingleMember(t *testing.T) {
	t.Parallel()
	scan := mustExtractionConstruct(plans.NewRecordQueryScanPlan(
		[]string{"T"}, extractionRowType("T"), false))
	r := expressions.FinalOf(scan)
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
	scanExpr := mustExtractionConstruct(plans.NewRecordQueryScanPlan(
		[]string{"T"}, extractionRowType("T"), false))
	r := expressions.FinalOf(scanExpr)
	r.SetWinner(scanExpr)

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
	innerScan := mustExtractionConstruct(plans.NewRecordQueryScanPlan(
		[]string{"T"}, extractionRowType("T"), false))
	innerRef := expressions.FinalOf(innerScan)
	limit := mustExtractionConstruct(plans.NewRecordQueryLimitPlanFromQuantifier(
		expressions.NewPhysicalQuantifier(innerRef), 1, 0, nil))
	topRef := expressions.FinalOf(limit)

	sel := &mockSelector{
		winners: map[*expressions.Reference]expressions.RelationalExpression{
			innerRef: innerScan,
			topRef:   limit,
		},
	}

	got, err := ExtractBestPlanFromSelector(topRef, sel, nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	exLimit, ok := got.(*plans.RecordQueryLimitPlan)
	if !ok {
		t.Fatalf("got %T, want *RecordQueryLimitPlan", got)
	}
	quantifiers := exLimit.GetQuantifiers()
	if len(quantifiers) != 1 {
		t.Fatalf("extracted Limit quantifiers = %d, want 1", len(quantifiers))
	}
	exInnerRef := quantifiers[0].GetRangesOver()
	if exInnerRef == innerRef {
		t.Fatal("extracted inner Reference is same pointer — should be fresh")
	}
}

// TestExtractBestPlan_CostTieDeterministicAcrossInsertionOrder pins the
// selector-less extraction tie-break (tieBrokenLess): two structurally
// different but cost-tied members must extract the SAME winner no matter
// the member insertion order. Before the tie-break, GetBest under the
// merely cost-partial comparator kept whichever tied member it met first
// — the P0.6 insertion-order nondeterminism, closed on the planner's
// selector path but left open on this exported path.
func TestExtractBestPlan_CostTieDeterministicAcrossInsertionOrder(t *testing.T) {
	t.Parallel()
	build := func(first, second string) *expressions.Reference {
		r := expressions.InitialOf(mustFullUnorderedScan(t, []string{first}, extractionRowType(first)))
		r.Insert(mustFullUnorderedScan(t, []string{second}, extractionRowType(second)))
		return r
	}
	winner := func(t *testing.T, r *expressions.Reference) string {
		t.Helper()
		got, err := ExtractBestPlan(r)
		if err != nil {
			t.Fatalf("ExtractBestPlan err=%v", err)
		}
		s, ok := got.(*expressions.FullUnorderedScanExpression)
		if !ok {
			t.Fatalf("got %T, want *FullUnorderedScanExpression", got)
		}
		return s.GetRecordTypes()[0]
	}
	ab := winner(t, build("A", "B"))
	ba := winner(t, build("B", "A"))
	if ab != ba {
		t.Fatalf("cost-tied winner depends on insertion order: [A,B]→%s, [B,A]→%s", ab, ba)
	}
}

// TestExtractBestPlan_HashTieFallsBackToTypeKey pins the SECOND
// tie-break key: the scan hash is names-only BY DESIGN (wildcard-match
// bucketing), so two scans over the same record name with DIFFERENT
// concrete flowed record types hash equal while Reference.Insert keeps
// them distinct — the hash alone left the winner insertion-order
// dependent. The flowed type's stable rendering discriminates them.
func TestExtractBestPlan_HashTieFallsBackToTypeKey(t *testing.T) {
	t.Parallel()
	typeA := values.NewRecordType("T", false, []values.Field{
		{Name: "A", FieldType: values.NotNullLong, Ordinal: 0},
	})
	typeB := values.NewRecordType("T", false, []values.Field{
		{Name: "B", FieldType: values.TypeString, Ordinal: 0},
	})
	build := func(first, second values.Type) *expressions.Reference {
		r := expressions.InitialOf(mustFullUnorderedScan(t, []string{"T"}, first))
		r.Insert(mustFullUnorderedScan(t, []string{"T"}, second))
		return r
	}
	winner := func(t *testing.T, r *expressions.Reference) string {
		t.Helper()
		got, err := ExtractBestPlan(r)
		if err != nil {
			t.Fatalf("ExtractBestPlan err=%v", err)
		}
		s, ok := got.(*expressions.FullUnorderedScanExpression)
		if !ok {
			t.Fatalf("got %T, want *FullUnorderedScanExpression", got)
		}
		return s.GetResultValue().Type().String()
	}
	ab := winner(t, build(typeA, typeB))
	ba := winner(t, build(typeB, typeA))
	if ab != ba {
		t.Fatalf("hash-tied winner depends on insertion order: [A,B]→%s, [B,A]→%s", ab, ba)
	}
}

// TestExtractBestPlan_ExplodeFieldTieDeterministic pins the tie-break
// against the Explode shape: two cost-tied explodes over DIFFERENT array
// fields share an identical result element type, so the type-key cannot
// discriminate — the discrimination must come from the expression hash,
// which now folds the collection Value's SEMANTIC content (the field
// path) rather than its bare Name() ("field" for either). Same winner
// regardless of insertion order.
func TestExtractBestPlan_ExplodeFieldTieDeterministic(t *testing.T) {
	t.Parallel()
	arr := values.NewArrayType(true, values.NotNullLong)
	rowType := values.NewRecordType("T", false, []values.Field{
		{Name: "ARR1", FieldType: arr, Ordinal: 0},
		{Name: "ARR2", FieldType: arr, Ordinal: 1},
	})
	fieldOf := func(name string) values.Value {
		qov := mustExtractionConstruct(values.NewQuantifiedObjectValue(
			values.NamedCorrelationIdentifier("T"), rowType))
		ordinal := 0
		if name == "ARR2" {
			ordinal = 1
		}
		return mustExtractionConstruct(values.ResolveFieldOrdinals(qov, []int{ordinal}))
	}
	build := func(first, second string) *expressions.Reference {
		r := expressions.InitialOf(mustExtractionConstruct(expressions.NewExplodeExpression(fieldOf(first))))
		r.Insert(mustExtractionConstruct(expressions.NewExplodeExpression(fieldOf(second))))
		return r
	}
	winner := func(t *testing.T, r *expressions.Reference) string {
		t.Helper()
		got, err := ExtractBestPlan(r)
		if err != nil {
			t.Fatalf("ExtractBestPlan err=%v", err)
		}
		ex, ok := got.(*expressions.ExplodeExpression)
		if !ok {
			t.Fatalf("got %T, want *ExplodeExpression", got)
		}
		fv, ok := values.AsFieldValue(ex.GetCollectionValue())
		if !ok {
			t.Fatalf("collection %T, want exact FieldValue", ex.GetCollectionValue())
		}
		return fv.DisplayName()
	}
	ab := winner(t, build("ARR1", "ARR2"))
	ba := winner(t, build("ARR2", "ARR1"))
	if ab != ba {
		t.Fatalf("explode-field-tied winner depends on insertion order: →%s vs →%s", ab, ba)
	}
}

// BenchmarkExtractBestPlan_DeepTree pins ExtractBestPlan perf on a
// 5-deep Filter chain. Each Reference has a single member; the
// extractor walks every Quantifier and rebuilds the tree.
func BenchmarkExtractBestPlan_DeepTree(b *testing.B) {
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	innerQ := extractionScanQ(b, "Order")
	for i := 0; i < 5; i++ {
		f := mustExtractionConstruct(expressions.NewLogicalFilterExpression(
			[]predicates.QueryPredicate{pred}, innerQ,
		))
		innerQ = expressions.ForEachQuantifier(expressions.InitialOf(f))
	}
	r := innerQ.GetRangesOver()
	// Extraction that FAILS returns almost immediately, so an unchecked
	// ExtractBestPlan turns a regression into a headline speedup. The
	// Reference is loop-invariant, so one pre-loop check covers every
	// iteration and the timed region stays untouched.
	if best, err := ExtractBestPlan(r); err != nil || best == nil {
		b.Fatalf("ExtractBestPlan = %v, err %v; want a plan", best, err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ExtractBestPlan(r)
	}
}

// BenchmarkExtractBestPlan_WideAlternatives pins ExtractBestPlan
// perf when the top-level Reference has 5 distinct Filter members
// over a shared inner. Memoisation kicks in for the cost computation.
func BenchmarkExtractBestPlan_WideAlternatives(b *testing.B) {
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	innerScan := extractionScan(b, "Order")
	r := expressions.InitialOf(mustExtractionConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(innerScan),
	)))
	for i := 2; i <= 5; i++ {
		preds := make([]predicates.QueryPredicate, i)
		for j := range preds {
			preds[j] = pred
		}
		r.Insert(mustExtractionConstruct(expressions.NewLogicalFilterExpression(
			preds, expressions.ForEachQuantifier(innerScan),
		)))
	}
	if best, err := ExtractBestPlan(r); err != nil || best == nil {
		b.Fatalf("ExtractBestPlan = %v, err %v; want a plan", best, err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ExtractBestPlan(r)
	}
}
