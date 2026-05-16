package executor

import (
	"context"
	"fmt"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// sliceQueryCursor is a test helper that yields QueryResults from a slice.
type sliceQueryCursor struct {
	rows   []QueryResult
	idx    int
	closed bool
}

func newSliceQueryCursor(rows []QueryResult) *sliceQueryCursor {
	return &sliceQueryCursor{rows: rows}
}

func (c *sliceQueryCursor) OnNext(context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.idx >= len(c.rows) {
		return recordlayer.NewResultNoNext[QueryResult](
			recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
		), nil
	}
	row := c.rows[c.idx]
	c.idx++
	return recordlayer.NewResultWithValue(row, nonEndContinuation), nil
}

func (c *sliceQueryCursor) Close() error   { c.closed = true; return nil }
func (c *sliceQueryCursor) IsClosed() bool { return c.closed }

func makeRow(fields map[string]any) QueryResult {
	return QueryResult{Datum: fields}
}

func TestHashJoinCursor_InnerJoin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// orders table: {ID, CUSTOMER_ID}
	outerRows := []QueryResult{
		makeRow(map[string]any{"ID": int64(1), "CUSTOMER_ID": int64(10)}),
		makeRow(map[string]any{"ID": int64(2), "CUSTOMER_ID": int64(20)}),
		makeRow(map[string]any{"ID": int64(3), "CUSTOMER_ID": int64(10)}),
		makeRow(map[string]any{"ID": int64(4), "CUSTOMER_ID": int64(99)}), // no match
	}

	// customers table: {ID, NAME}
	innerRows := []QueryResult{
		makeRow(map[string]any{"ID": int64(10), "NAME": "Alice"}),
		makeRow(map[string]any{"ID": int64(20), "NAME": "Bob"}),
		makeRow(map[string]any{"ID": int64(30), "NAME": "Charlie"}), // no outer match
	}

	// Equi-join: orders.CUSTOMER_ID = customers.ID
	equiKeys := []equiJoinKey{{
		outerVal: &values.FieldValue{Field: "ORDERS.CUSTOMER_ID"},
		innerVal: &values.FieldValue{Field: "CUSTOMERS.ID"},
	}}

	hashIdx, _ := buildInnerHashIndex(innerRows, equiKeys, "CUSTOMERS")
	outerCursor := newSliceQueryCursor(outerRows)

	cursor := newHashJoinCursor(
		outerCursor, hashIdx, innerRows, equiKeys, nil,
		plans.JoinInner, "ORDERS", "CUSTOMERS", EmptyEvaluationContext(),
	)
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}

	// Expected: orders 1→Alice, 2→Bob, 3→Alice (order 4 has no match → excluded)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}

	assertJoinResult(t, results[0], int64(1), "Alice")
	assertJoinResult(t, results[1], int64(2), "Bob")
	assertJoinResult(t, results[2], int64(3), "Alice")
}

func TestHashJoinCursor_LeftOuterJoin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	outerRows := []QueryResult{
		makeRow(map[string]any{"ID": int64(1), "CUSTOMER_ID": int64(10)}),
		makeRow(map[string]any{"ID": int64(2), "CUSTOMER_ID": int64(99)}), // no match
	}

	innerRows := []QueryResult{
		makeRow(map[string]any{"ID": int64(10), "NAME": "Alice"}),
	}

	equiKeys := []equiJoinKey{{
		outerVal: &values.FieldValue{Field: "ORDERS.CUSTOMER_ID"},
		innerVal: &values.FieldValue{Field: "CUSTOMERS.ID"},
	}}

	hashIdx, _ := buildInnerHashIndex(innerRows, equiKeys, "CUSTOMERS")
	outerCursor := newSliceQueryCursor(outerRows)

	cursor := newHashJoinCursor(
		outerCursor, hashIdx, innerRows, equiKeys, nil,
		plans.JoinLeftOuter, "ORDERS", "CUSTOMERS", EmptyEvaluationContext(),
	)
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}

	// Expected: order 1→Alice (matched), order 2→(outer only, no inner match)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}

	// First result: matched join
	m0, _ := results[0].Datum.(map[string]any)
	if m0["ORDERS.ID"] != int64(1) {
		t.Errorf("result[0] ORDERS.ID = %v, want 1", m0["ORDERS.ID"])
	}

	// Second result: LEFT OUTER with no match (outer row emitted)
	m1, _ := results[1].Datum.(map[string]any)
	if m1["ORDERS.ID"] != int64(2) {
		t.Errorf("result[1] ORDERS.ID = %v, want 2", m1["ORDERS.ID"])
	}
}

func TestHashJoinCursor_NullKeysExcluded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	outerRows := []QueryResult{
		makeRow(map[string]any{"ID": int64(1), "FK": nil}),       // NULL key → no match
		makeRow(map[string]any{"ID": int64(2), "FK": int64(10)}), // match
	}

	innerRows := []QueryResult{
		makeRow(map[string]any{"ID": nil, "NAME": "Ghost"}), // NULL key → excluded from hash
		makeRow(map[string]any{"ID": int64(10), "NAME": "Real"}),
	}

	equiKeys := []equiJoinKey{{
		outerVal: &values.FieldValue{Field: "A.FK"},
		innerVal: &values.FieldValue{Field: "B.ID"},
	}}

	hashIdx, _ := buildInnerHashIndex(innerRows, equiKeys, "B")
	outerCursor := newSliceQueryCursor(outerRows)

	cursor := newHashJoinCursor(
		outerCursor, hashIdx, innerRows, equiKeys, nil,
		plans.JoinInner, "A", "B", EmptyEvaluationContext(),
	)
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}

	// Only outer row 2 matches inner row "Real" (NULL = NULL is UNKNOWN → no match)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
}

func TestHashJoinCursor_WithResidualPredicates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	outerRows := []QueryResult{
		makeRow(map[string]any{"ID": int64(1), "FK": int64(10), "STATUS": "active"}),
		makeRow(map[string]any{"ID": int64(2), "FK": int64(10), "STATUS": "inactive"}),
	}

	innerRows := []QueryResult{
		makeRow(map[string]any{"ID": int64(10), "NAME": "Alice"}),
	}

	equiKeys := []equiJoinKey{{
		outerVal: &values.FieldValue{Field: "A.FK"},
		innerVal: &values.FieldValue{Field: "B.ID"},
	}}

	// Residual predicate: A.STATUS = 'active'
	residual := []predicates.QueryPredicate{
		&predicates.ComparisonPredicate{
			Operand:    &values.FieldValue{Field: "A.STATUS"},
			Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: "active"}},
		},
	}

	hashIdx, _ := buildInnerHashIndex(innerRows, equiKeys, "B")
	outerCursor := newSliceQueryCursor(outerRows)

	cursor := newHashJoinCursor(
		outerCursor, hashIdx, innerRows, equiKeys, residual,
		plans.JoinInner, "A", "B", EmptyEvaluationContext(),
	)
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}

	// Only row 1 passes the residual predicate
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	m, _ := results[0].Datum.(map[string]any)
	if m["A.ID"] != int64(1) {
		t.Errorf("result[0] A.ID = %v, want 1", m["A.ID"])
	}
}

func BenchmarkNLJ_BruteForce(b *testing.B) {
	benchNLJ(b, false, 1000, 1000)
}

func BenchmarkNLJ_HashJoin(b *testing.B) {
	benchNLJ(b, true, 1000, 1000)
}

func benchNLJ(b *testing.B, useHash bool, outerN, innerN int) {
	b.Helper()
	ctx := context.Background()

	innerRows := make([]QueryResult, innerN)
	for i := range innerRows {
		innerRows[i] = makeRow(map[string]any{"ID": int64(i), "NAME": fmt.Sprintf("name_%d", i)})
	}

	equiKeys := []equiJoinKey{{
		outerVal: &values.FieldValue{Field: "A.FK"},
		innerVal: &values.FieldValue{Field: "B.ID"},
	}}

	var hashIdx map[string][]QueryResult
	if useHash {
		hashIdx, _ = buildInnerHashIndex(innerRows, equiKeys, "B")
	}

	b.ResetTimer()
	for range b.N {
		outerRows := make([]QueryResult, outerN)
		for i := range outerRows {
			outerRows[i] = makeRow(map[string]any{"ID": int64(i), "FK": int64(i % innerN)})
		}
		outerCursor := newSliceQueryCursor(outerRows)

		var cursor recordlayer.RecordCursor[QueryResult]
		if useHash {
			cursor = newHashJoinCursor(
				outerCursor, hashIdx, innerRows, equiKeys, nil,
				plans.JoinInner, "A", "B", EmptyEvaluationContext(),
			)
		} else {
			preds := []predicates.QueryPredicate{
				&predicates.ComparisonPredicate{
					Operand:    &values.FieldValue{Field: "A.FK"},
					Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.FieldValue{Field: "B.ID"}},
				},
			}
			cursor = newNLJCursor(
				outerCursor, innerRows,
				plans.JoinInner, "A", "B",
				preds, EmptyEvaluationContext(),
			)
		}

		_, _ = CollectAll(ctx, cursor)
		cursor.Close()
	}
}

func assertJoinResult(t *testing.T, result QueryResult, expectedOrderID int64, expectedCustomerName string) {
	t.Helper()
	m, ok := result.Datum.(map[string]any)
	if !ok {
		t.Fatalf("result.Datum type = %T, want map[string]any", result.Datum)
	}
	if m["ORDERS.ID"] != expectedOrderID {
		t.Errorf("ORDERS.ID = %v, want %d", m["ORDERS.ID"], expectedOrderID)
	}
	if m["CUSTOMERS.NAME"] != expectedCustomerName {
		t.Errorf("CUSTOMERS.NAME = %v, want %q", m["CUSTOMERS.NAME"], expectedCustomerName)
	}
}
