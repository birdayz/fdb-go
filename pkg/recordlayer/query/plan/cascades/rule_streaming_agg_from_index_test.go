package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestStreamingAggFromIndex_Fires(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	aliases := []values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()}
	cand := newKnownDistinctValueIndexCandidate(
		"T$region", []string{"T"}, []string{"region"}, aliases, values.UnknownType, false,
		nil,
	)

	results := FireExpressionRuleWithMemo(
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

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	// Index is on "status", not "region".
	aliases := []values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()}
	cand := newKnownDistinctValueIndexCandidate(
		"T$status", []string{"T"}, []string{"status"}, aliases, values.UnknownType, false,
		nil,
	)

	results := FireExpressionRuleWithMemo(
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

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Index covers the grouping key (region) but NOT the aggregate operand (amount).
	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "amount", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	aliases := []values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()}
	cand := newKnownDistinctValueIndexCandidate(
		"T$region", []string{"T"}, []string{"region"}, aliases, values.UnknownType, false,
		nil,
	)

	results := FireExpressionRuleWithMemo(
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

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{
			&values.FieldValue{Field: "region", Typ: values.UnknownType},
			&values.FieldValue{Field: "city", Typ: values.UnknownType},
		},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "amount", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	aliases := []values.CorrelationIdentifier{
		values.UniqueCorrelationIdentifier(),
		values.UniqueCorrelationIdentifier(),
		values.UniqueCorrelationIdentifier(),
	}
	cand := newKnownDistinctValueIndexCandidate(
		"T$region_city_amount", []string{"T"}, []string{"region", "city", "amount"}, aliases, values.UnknownType, false,
		nil,
	)

	results := FireExpressionRuleWithMemo(
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

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Global aggregate — no grouping keys.
	gb := expressions.NewGroupByExpression(
		[]values.Value{},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	aliases := []values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()}
	cand := newKnownDistinctValueIndexCandidate(
		"T$region", []string{"T"}, []string{"region"}, aliases, values.UnknownType, false,
		nil,
	)

	results := FireExpressionRuleWithMemo(
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
			values.UnknownType,
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
		values.UnknownType,
		false,
		nil,
		&scalar,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{
		newCandidate("T$tags_fanout", &fanOut),
		functionCandidate,
		newCandidate("T$tags_scalar", &scalar),
	}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	groupBy := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "TAGS", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{{Function: expressions.AggCount}},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	)
	results := FireExpressionRuleWithMemo(
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
	indexPlan, ok := agg.GetInner().(*plans.RecordQueryIndexPlan)
	if !ok {
		t.Fatalf("expected aggregate over a bare index scan, got %T", agg.GetInner())
	}
	if got := indexPlan.GetIndexName(); got != "T$tags_scalar" {
		t.Fatalf("aggregate shortcut selected %q, want the non-fan-out candidate", got)
	}
}
