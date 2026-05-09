package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestRemoveRangeOneRule_LimitOverValues(t *testing.T) {
	t.Parallel()

	// LogicalValuesExpression produces exactly 1 row — LIMIT 1 is redundant.
	vals := expressions.NewLogicalValuesExpression([]values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.TypeInt},
	})
	valsRef := expressions.InitialOf(vals)
	valsQ := expressions.ForEachQuantifier(valsRef)

	lim := expressions.NewLogicalLimitExpression(1, 0, valsQ)
	ref := expressions.InitialOf(lim)

	rule := NewRemoveRangeOneRule()
	results := FireExpressionRule(rule, ref)
	if len(results) == 0 {
		t.Fatal("rule did not fire — expected LIMIT 1 over VALUES to be eliminated")
	}
	if _, ok := results[0].(*expressions.LogicalValuesExpression); !ok {
		t.Fatalf("expected LogicalValuesExpression, got %T", results[0])
	}
}

func TestRemoveRangeOneRule_LimitOverNonUniqueScan(t *testing.T) {
	t.Parallel()

	// FullUnorderedScan with record types has cardinality 1e6 — not at-most-one-row.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	lim := expressions.NewLogicalLimitExpression(1, 0, scanQ)
	ref := expressions.InitialOf(lim)

	rule := NewRemoveRangeOneRule()
	results := FireExpressionRule(rule, ref)
	if len(results) != 0 {
		t.Fatalf("rule should not fire over non-unique scan, got %d results", len(results))
	}
}

func TestRemoveRangeOneRule_Limit5NotMatched(t *testing.T) {
	t.Parallel()

	// LIMIT 5 should never match, regardless of inner cardinality.
	vals := expressions.NewLogicalValuesExpression([]values.Value{
		&values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
	})
	valsRef := expressions.InitialOf(vals)
	valsQ := expressions.ForEachQuantifier(valsRef)

	lim := expressions.NewLogicalLimitExpression(5, 0, valsQ)
	ref := expressions.InitialOf(lim)

	rule := NewRemoveRangeOneRule()
	results := FireExpressionRule(rule, ref)
	if len(results) != 0 {
		t.Fatalf("rule should not fire for limit=5, got %d results", len(results))
	}
}

func TestRemoveRangeOneRule_Limit1WithOffsetNotMatched(t *testing.T) {
	t.Parallel()

	// LIMIT 1 OFFSET 2 should not match — the offset changes semantics.
	vals := expressions.NewLogicalValuesExpression([]values.Value{
		&values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
	})
	valsRef := expressions.InitialOf(vals)
	valsQ := expressions.ForEachQuantifier(valsRef)

	lim := expressions.NewLogicalLimitExpression(1, 2, valsQ)
	ref := expressions.InitialOf(lim)

	rule := NewRemoveRangeOneRule()
	results := FireExpressionRule(rule, ref)
	if len(results) != 0 {
		t.Fatalf("rule should not fire for offset=2, got %d results", len(results))
	}
}
