package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestStreamingAggFromIndex_Fires(t *testing.T) {
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

	aliases := []values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()}
	cand := NewValueIndexScanMatchCandidate(
		"T$region", []string{"T"}, []string{"region"}, aliases, values.UnknownType, false,
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
		t.Fatalf("expected physicalStreamingAggWrapper, got %T", results[0])
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
	cand := NewValueIndexScanMatchCandidate(
		"T$status", []string{"T"}, []string{"status"}, aliases, values.UnknownType, false,
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
	cand := NewValueIndexScanMatchCandidate(
		"T$region_city_amount", []string{"T"}, []string{"region", "city", "amount"}, aliases, values.UnknownType, false,
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
		t.Fatalf("expected physicalStreamingAggWrapper, got %T", results[0])
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
	cand := NewValueIndexScanMatchCandidate(
		"T$region", []string{"T"}, []string{"region"}, aliases, values.UnknownType, false,
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
