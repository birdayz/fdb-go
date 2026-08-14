package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustStreamingAggIndexConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct streaming-aggregate-from-index fixture: " + err.Error())
	}
	return value
}

func streamingAggIndexRowType() *values.RecordType {
	return values.NewRecordType("STREAMING_AGG_INDEX_ROW", false, []values.Field{
		{Name: "region", FieldType: values.NotNullString},
		{Name: "city", FieldType: values.NotNullString},
		{Name: "amount", FieldType: values.NotNullLong},
		{Name: "id", FieldType: values.NotNullLong},
		{Name: "status", FieldType: values.NotNullString},
		{Name: "TAGS", FieldType: values.NewArrayType(false, values.NotNullString)},
	})
}

func streamingAggIndexScanQ() (
	*expressions.FullUnorderedScanExpression,
	expressions.Quantifier,
) {
	scan := mustStreamingAggIndexConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, streamingAggIndexRowType()))
	return scan, expressions.ForEachQuantifier(expressions.InitialOf(scan))
}

func streamingAggIndexField(q expressions.Quantifier, name string) values.Value {
	root := mustStreamingAggIndexConstruct(q.RequireFlowedObjectValue())
	request := mustStreamingAggIndexConstruct(values.FieldByName(name))
	return mustStreamingAggIndexConstruct(values.ResolveFieldAccess(
		root, []values.FieldRequest{request}))
}

func TestStreamingAggFromIndex_Fires(t *testing.T) {
	t.Parallel()

	_, scanQ := streamingAggIndexScanQ()

	gb := mustStreamingAggIndexConstruct(expressions.NewGroupByExpression(
		[]values.Value{streamingAggIndexField(scanQ, "region")},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount},
		},
		scanQ,
	))
	gbRef := expressions.InitialOf(gb)

	aliases := []values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()}
	cand := newKnownDistinctValueIndexCandidate(
		"T$region", []string{"T"}, []string{"region"}, aliases, streamingAggIndexRowType(), false,
		nil,
	)

	results := mustFireExpressionRuleWithMemo(t,
		NewStreamingAggFromIndexRule(),
		gbRef,
		&indexTestPlanContext{candidates: []MatchCandidate{cand}},
		nil,
	)
	if len(results) == 0 {
		t.Fatal("StreamingAggFromIndexRule didn't fire")
	}

	if !IsPhysicalStreamingAgg(results[0]) {
		t.Fatalf("expected *plans.RecordQueryStreamingAggregationPlan, got %T", results[0])
	}
}

func TestStreamingAggFromIndex_DoesNotFireWhenNoMatchingIndex(t *testing.T) {
	t.Parallel()

	_, scanQ := streamingAggIndexScanQ()

	gb := mustStreamingAggIndexConstruct(expressions.NewGroupByExpression(
		[]values.Value{streamingAggIndexField(scanQ, "region")},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: streamingAggIndexField(scanQ, "id")},
		},
		scanQ,
	))
	gbRef := expressions.InitialOf(gb)

	// Index is on "status", not "region".
	aliases := []values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()}
	cand := newKnownDistinctValueIndexCandidate(
		"T$status", []string{"T"}, []string{"status"}, aliases, streamingAggIndexRowType(), false,
		nil,
	)

	results := mustFireExpressionRuleWithMemo(t,
		NewStreamingAggFromIndexRule(),
		gbRef,
		&indexTestPlanContext{candidates: []MatchCandidate{cand}},
		nil,
	)
	if len(results) != 0 {
		t.Fatal("StreamingAggFromIndexRule should NOT fire when index doesn't cover grouping keys")
	}
}

func TestStreamingAggFromIndex_DoesNotFireWhenAggregateNotCovered(t *testing.T) {
	t.Parallel()

	_, scanQ := streamingAggIndexScanQ()

	// Index covers the grouping key (region) but NOT the aggregate operand (amount).
	gb := mustStreamingAggIndexConstruct(expressions.NewGroupByExpression(
		[]values.Value{streamingAggIndexField(scanQ, "region")},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: streamingAggIndexField(scanQ, "amount")},
		},
		scanQ,
	))
	gbRef := expressions.InitialOf(gb)

	aliases := []values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()}
	cand := newKnownDistinctValueIndexCandidate(
		"T$region", []string{"T"}, []string{"region"}, aliases, streamingAggIndexRowType(), false,
		nil,
	)

	results := mustFireExpressionRuleWithMemo(t,
		NewStreamingAggFromIndexRule(),
		gbRef,
		&indexTestPlanContext{candidates: []MatchCandidate{cand}},
		nil,
	)
	if len(results) != 0 {
		t.Fatal("StreamingAggFromIndexRule should NOT fire when aggregate operand is not in index")
	}
}

func TestStreamingAggFromIndex_MultiColumn(t *testing.T) {
	t.Parallel()

	_, scanQ := streamingAggIndexScanQ()

	gb := mustStreamingAggIndexConstruct(expressions.NewGroupByExpression(
		[]values.Value{
			streamingAggIndexField(scanQ, "region"),
			streamingAggIndexField(scanQ, "city"),
		},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: streamingAggIndexField(scanQ, "amount")},
		},
		scanQ,
	))
	gbRef := expressions.InitialOf(gb)

	aliases := []values.CorrelationIdentifier{
		values.UniqueCorrelationIdentifier(),
		values.UniqueCorrelationIdentifier(),
		values.UniqueCorrelationIdentifier(),
	}
	cand := newKnownDistinctValueIndexCandidate(
		"T$region_city_amount", []string{"T"}, []string{"region", "city", "amount"}, aliases, streamingAggIndexRowType(), false,
		nil,
	)

	results := mustFireExpressionRuleWithMemo(t,
		NewStreamingAggFromIndexRule(),
		gbRef,
		&indexTestPlanContext{candidates: []MatchCandidate{cand}},
		nil,
	)
	if len(results) == 0 {
		t.Fatal("StreamingAggFromIndexRule didn't fire for multi-column index")
	}
	if !IsPhysicalStreamingAgg(results[0]) {
		t.Fatalf("expected *plans.RecordQueryStreamingAggregationPlan, got %T", results[0])
	}
}

func TestStreamingAggFromIndex_DoesNotFireForGlobalAgg(t *testing.T) {
	t.Parallel()

	_, scanQ := streamingAggIndexScanQ()

	// Global aggregate — no grouping keys.
	gb := mustStreamingAggIndexConstruct(expressions.NewGroupByExpression(
		[]values.Value{},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: streamingAggIndexField(scanQ, "id")},
		},
		scanQ,
	))
	gbRef := expressions.InitialOf(gb)

	aliases := []values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()}
	cand := newKnownDistinctValueIndexCandidate(
		"T$region", []string{"T"}, []string{"region"}, aliases, streamingAggIndexRowType(), false,
		nil,
	)

	results := mustFireExpressionRuleWithMemo(t,
		NewStreamingAggFromIndexRule(),
		gbRef,
		&indexTestPlanContext{candidates: []MatchCandidate{cand}},
		nil,
	)
	if len(results) != 0 {
		t.Fatal("StreamingAggFromIndexRule should NOT fire for global aggregates (no grouping keys)")
	}
}

// TestStreamingAggFromIndex_RejectsFanOutCandidate proves that the direct
// GroupBy(Scan) shortcut cannot aggregate raw fan-out entries. Doing so would
// count index entries rather than base records and would omit records whose
// repeated field is empty. A scalar index on the same logical key remains
// eligible.
func TestStreamingAggFromIndex_RejectsFanOutCandidate(t *testing.T) {
	t.Parallel()

	fanOut := true
	scalar := false
	newCandidate := func(name string, createsDuplicates *bool) MatchCandidate {
		return NewValueIndexScanMatchCandidateWithFunctions(
			name,
			[]string{"T"},
			[]string{"TAGS"},
			nil,
			[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
			streamingAggIndexRowType(),
			false,
			nil,
			createsDuplicates,
		)
	}
	functionCandidate := NewValueIndexScanMatchCandidateWithFunctions(
		"T$cardinality_tags",
		[]string{"T"},
		[]string{"TAGS"},
		[]string{FunctionKindCardinality},
		[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
		streamingAggIndexRowType(),
		false,
		nil,
		&scalar,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{
		newCandidate("T$tags_fanout", &fanOut),
		functionCandidate,
		newCandidate("T$tags_scalar", &scalar),
	}}

	_, scanQ := streamingAggIndexScanQ()
	groupBy := mustStreamingAggIndexConstruct(expressions.NewGroupByExpression(
		[]values.Value{streamingAggIndexField(scanQ, "TAGS")},
		[]expressions.AggregateSpec{{Function: expressions.AggCount}},
		scanQ,
	))
	results := mustFireExpressionRuleWithMemo(t,
		NewStreamingAggFromIndexRule(),
		expressions.InitialOf(groupBy),
		ctx,
		nil,
	)

	if len(results) != 1 {
		t.Fatalf("expected only the scalar aggregate-index shortcut, got %d yields", len(results))
	}
	agg, ok := results[0].(*plans.RecordQueryStreamingAggregationPlan)
	if !ok {
		t.Fatalf("expected *plans.RecordQueryStreamingAggregationPlan, got %T", results[0])
	}
	// The rule builds a COVERING scan (RFC-220: coveringness is the plan type,
	// constructed where the scan is built, never stamped later onto a bare scan).
	coveringPlan, ok := agg.GetInner().(*plans.RecordQueryCoveringIndexPlan)
	if !ok {
		t.Fatalf("expected aggregate over a covering index scan, got %T", agg.GetInner())
	}
	if got := coveringPlan.GetIndexName(); got != "T$tags_scalar" {
		t.Fatalf("aggregate shortcut selected %q, want the non-fan-out candidate", got)
	}
}
