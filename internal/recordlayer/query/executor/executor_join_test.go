package executor

import (
	"context"
	"strings"
	"testing"

	"fdb.dev/internal/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

func TestExecuteNestedLoopJoin_CrossJoin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	left := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}))
	right := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: "hello", Typ: values.NewPrimitiveType(values.TypeCodeString, false)},
	}))

	join := mustExecutorConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
		left, right, nil, plans.JoinCross,
		values.NamedCorrelationIdentifier(""), values.NamedCorrelationIdentifier(""),
		mustConcatPlanResultQOV(t, left, right),
	))
	cursor, err := ExecutePlan(ctx, join, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (1×1 cross)", len(results))
	}
	row, _ := rowMapOK(results[0])
	rightType := right.GetResultType().(*values.RecordType)
	rightName := rightType.Fields[0].Name
	if row[rightName] != "hello" {
		t.Errorf("right value under declared name %q = %v, want 'hello'", rightName, row[rightName])
	}
}

func TestExecuteNestedLoopJoin_InnerJoin_WithPredicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	left := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(5), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}))
	right := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(5), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}))

	join := mustExecutorConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
		left, right,
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		plans.JoinInner,
		values.NamedCorrelationIdentifier(""), values.NamedCorrelationIdentifier(""),
		mustConcatPlanResultQOV(t, left, right),
	))
	cursor, err := ExecutePlan(ctx, join, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
}

func TestExecuteNestedLoopJoin_InnerJoin_PredicateRejects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	left := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}))
	right := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(2), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}))

	join := mustExecutorConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
		left, right,
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriFalse)},
		plans.JoinInner,
		values.NamedCorrelationIdentifier(""), values.NamedCorrelationIdentifier(""),
		mustConcatPlanResultQOV(t, left, right),
	))
	cursor, err := ExecutePlan(ctx, join, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0 (predicate rejects all)", len(results))
	}
}

func TestExecuteNestedLoopJoin_LeftOuter_NoInnerMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	left := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}))
	right := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(2), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}))
	outerAlias := values.NamedCorrelationIdentifier("LEFT_VALUE")
	innerAlias := values.NamedCorrelationIdentifier("RIGHT_VALUE")

	join := mustExecutorConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
		left, right,
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriFalse)},
		plans.JoinLeftOuter,
		outerAlias, innerAlias,
		mustRetainedJoinResult(t, left, right, outerAlias, innerAlias, plans.JoinLeftOuter),
	))
	cursor, err := ExecutePlan(ctx, join, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (left outer preserves unmatched)", len(results))
	}
	if results[0].Positional == nil || len(results[0].Positional.Slots) != 2 ||
		results[0].Positional.Slots[0] != int64(1) || results[0].Positional.Slots[1] != nil {
		t.Fatalf("left outer row = %#v, want [1 NULL]", results[0].Positional)
	}
}

// ===========================================================================
// nljCursor — Close safety
// ===========================================================================

func TestNLJCursor_CloseIdempotent(t *testing.T) {
	t.Parallel()

	outer := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
	})
	inner := []QueryResult{qr("val", int64(10))}

	c := mustNLJCursor(t, outer, inner, plans.JoinInner, values.NamedCorrelationIdentifier(""), values.NamedCorrelationIdentifier(""), nil, nil, EmptyEvaluationContext(), nil)

	if c.IsClosed() {
		t.Fatal("cursor should not be closed before Close()")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if !c.IsClosed() {
		t.Fatal("cursor should be closed after Close()")
	}

	// Second close must not panic or error.
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if !c.IsClosed() {
		t.Fatal("cursor should remain closed")
	}
}

func TestNLJCursor_OnNextAfterClose(t *testing.T) {
	t.Parallel()

	outer := recordlayer.FromList([]QueryResult{qr("id", int64(1))})
	inner := []QueryResult{qr("val", int64(10))}

	c := mustNLJCursor(t, outer, inner, plans.JoinInner, values.NamedCorrelationIdentifier(""), values.NamedCorrelationIdentifier(""), nil, nil, EmptyEvaluationContext(), nil)
	c.Close()

	_, err := c.OnNext(context.Background())
	if err == nil {
		t.Fatal("expected error from OnNext after Close")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("error = %q, want it to mention 'closed'", err)
	}
}

// ===========================================================================
// nljCursor — empty inner
// ===========================================================================

func TestNLJCursor_EmptyInner_InnerJoin(t *testing.T) {
	t.Parallel()

	outer := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", int64(2)),
		qr("id", int64(3)),
	})

	c := mustNLJCursor(t, outer, nil, plans.JoinInner, values.NamedCorrelationIdentifier(""), values.NamedCorrelationIdentifier(""), nil, nil, EmptyEvaluationContext(), nil)
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("INNER join with empty inner: got %d results, want 0", len(results))
	}
}

func TestNLJCursor_EmptyInner_LeftJoin(t *testing.T) {
	t.Parallel()

	outerRows := []QueryResult{
		qr("id", int64(1)),
		qr("id", int64(2)),
	}
	outer := recordlayer.FromList(outerRows)

	c := mustNLJCursor(t, outer, nil, plans.JoinLeftOuter, values.NamedCorrelationIdentifier("L"), values.NamedCorrelationIdentifier(""), nil, nil, EmptyEvaluationContext(), nil)
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 2 {
		t.Fatalf("LEFT join with empty inner: got %d results, want 2", len(results))
	}
}

// NOTE: cursor-level EXISTS/NOT-EXISTS semi-join modes were removed (RFC-141).
// The FlatMap/NLJ cursors are now pure maps; the existential semantics are
// emergent from FirstOrDefault-on-inner + a residual existential filter built
// by ImplementNestedLoopJoinRule. The WHERE-EXISTS / NOT-EXISTS behaviour is
// pinned end-to-end by the FDB suite (noncorrelated_exists_fdb_test.go,
// correlated_exists_crossjoin_test.go, cascades_fdb_test.go) at the plan level,
// not by direct cursor construction.

// ===========================================================================
// nljCursor — empty outer
// ===========================================================================

func TestNLJCursor_EmptyOuter_InnerJoin(t *testing.T) {
	t.Parallel()

	outer := recordlayer.FromList([]QueryResult{})
	inner := []QueryResult{qr("val", int64(1)), qr("val", int64(2))}

	c := mustNLJCursor(t, outer, inner, plans.JoinInner, values.NamedCorrelationIdentifier(""), values.NamedCorrelationIdentifier(""), nil, nil, EmptyEvaluationContext(), nil)
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("INNER join with empty outer: got %d results, want 0", len(results))
	}
}

func TestNLJCursor_EmptyOuter_LeftJoin(t *testing.T) {
	t.Parallel()

	outer := recordlayer.FromList([]QueryResult{})
	inner := []QueryResult{qr("val", int64(1))}

	c := mustNLJCursor(t, outer, inner, plans.JoinLeftOuter, values.NamedCorrelationIdentifier("L"), values.NamedCorrelationIdentifier(""), nil, nil, EmptyEvaluationContext(), nil)
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("LEFT join with empty outer: got %d results, want 0", len(results))
	}
}

func TestNLJCursor_EmptyOuter_CrossJoin(t *testing.T) {
	t.Parallel()

	outer := recordlayer.FromList([]QueryResult{})
	inner := []QueryResult{qr("val", int64(1)), qr("val", int64(2))}

	c := mustNLJCursor(t, outer, inner, plans.JoinCross, values.NamedCorrelationIdentifier(""), values.NamedCorrelationIdentifier(""), nil, nil, EmptyEvaluationContext(), nil)
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("CROSS join with empty outer: got %d results, want 0", len(results))
	}
}

// ===========================================================================
// nljCursor — basic INNER join producing correct cross product
// ===========================================================================

func TestNLJCursor_InnerJoin_CrossProduct(t *testing.T) {
	t.Parallel()

	outer := recordlayer.FromList([]QueryResult{
		qr("a", int64(1)),
		qr("a", int64(2)),
	})
	inner := []QueryResult{
		qr("b", int64(10)),
		qr("b", int64(20)),
	}

	c := mustNLJCursor(t, outer, inner, plans.JoinInner, values.NamedCorrelationIdentifier(""), values.NamedCorrelationIdentifier(""), nil, nil, EmptyEvaluationContext(), nil)
	defer c.Close()

	results := collectCursor(t, c)
	// 2 outer x 2 inner = 4 results (no predicate filtering).
	if len(results) != 4 {
		t.Fatalf("got %d results, want 4", len(results))
	}
}

func TestNLJCursor_InnerJoin_PredicateFilters(t *testing.T) {
	t.Parallel()

	outer := recordlayer.FromList([]QueryResult{
		qr("x", int64(1)),
		qr("x", int64(2)),
	})
	inner := []QueryResult{
		qr("y", int64(10)),
		qr("y", int64(20)),
	}

	// Predicate that always rejects: INNER join should produce 0 rows.
	preds := []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriFalse)}
	c := mustNLJCursor(t, outer, inner, plans.JoinInner, values.NamedCorrelationIdentifier(""), values.NamedCorrelationIdentifier(""), preds, nil, EmptyEvaluationContext(), nil)
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0 (predicate rejects all)", len(results))
	}
}

// --- mergeRows unit tests ---

func TestMergeRows_BothMaps(t *testing.T) {
	t.Parallel()
	outer := dmapPK(tuple.Tuple{int64(1)}, map[string]any{"A": 1, "B": 2})
	inner := dmapPK(tuple.Tuple{int64(2)}, map[string]any{"C": 3, "D": 4})
	merged := mergeRows(outer, inner, values.CorrelationIdentifier{}, values.CorrelationIdentifier{})
	m, _ := rowMapOK(merged)
	if m["A"] != 1 || m["B"] != 2 || m["C"] != 3 || m["D"] != 4 {
		t.Fatalf("unexpected merged datum: %v", m)
	}
	if merged.PrimaryKey[0] != int64(1) {
		t.Fatalf("PrimaryKey should come from outer, got %v", merged.PrimaryKey)
	}
}

func TestMergeRows_InnerOverridesOuter(t *testing.T) {
	t.Parallel()
	outer := dmap(map[string]any{"K": "outer"})
	inner := dmap(map[string]any{"K": "inner"})
	merged := mergeRows(outer, inner, values.CorrelationIdentifier{}, values.CorrelationIdentifier{})
	m, _ := rowMapOK(merged)
	if m["K"] != "inner" {
		t.Fatalf("inner should override outer on key conflict, got %v", m["K"])
	}
}

// TestMergeRows_ScalarOuter pins that a bare-scalar outer leg (the
// scalarPositionalRow `_0` wrapper) concatenates with the inner leg like any
// other row.
func TestMergeRows_ScalarOuter(t *testing.T) {
	t.Parallel()
	outer := QueryResult{Positional: scalarPositionalRow("string-datum"), PrimaryKey: tuple.Tuple{int64(1)}}
	inner := dmap(map[string]any{"C": 3})
	merged := mergeRows(outer, inner, values.CorrelationIdentifier{}, values.CorrelationIdentifier{})
	if got := rowVal(merged, "_0"); got != "string-datum" {
		t.Fatalf("expected scalar outer leg preserved, got %v", got)
	}
	if got := rowVal(merged, "C"); got != 3 {
		t.Fatalf("expected inner leg C=3, got %v", got)
	}
}

// TestMergeRows_DerivedTableAlias verifies that mergeRows qualifies each leg's
// columns under its alias via leg windows: a qualified read "SQ1.X" resolves
// through the leg's window, not a flat qualified key. This is also the
// regression mergeRows once had — a chained 3-way join whose outer row was
// itself a merged NLJ result returned dept.name instead of the right column
// (the join_chained conformance failure) — but a bare-key re-qualification
// clobber can no longer occur at all: legs are ordinal windows, never
// re-written bare keys. That shape is now covered end-to-end by the
// three-way-join yamsql scenarios.
func TestMergeRows_DerivedTableAlias(t *testing.T) {
	t.Parallel()

	// Derived table output: (SELECT ida AS x FROM a) AS sq1 -> row {IDA, X}.
	outer := QueryResult{Positional: &PositionalRow{
		Type: exactTestRowType(
			values.Field{Name: "IDA", FieldType: values.NullableLong},
			values.Field{Name: "X", FieldType: values.NullableLong},
		),
		Slots: []any{int64(1), int64(1)},
	}}
	inner := QueryResult{Positional: &PositionalRow{
		Type: exactTestRowType(
			values.Field{Name: "IDB", FieldType: values.NullableLong},
		),
		Slots: []any{int64(4)},
	}}

	merged := mergeRows(outer, inner, values.NamedCorrelationIdentifier("SQ1"), values.NamedCorrelationIdentifier("B"))
	// Qualified reads resolve leg-locally through the alias windows (a baked
	// QOV(alias).col reference through the row's own leg metadata — legRead).
	if v, ok := legRead(merged.Positional, "SQ1", "X"); !ok || v != int64(1) {
		t.Errorf("SQ1.X = %v, want 1", v)
	}
	if v, ok := legRead(merged.Positional, "B", "IDB"); !ok || v != int64(4) {
		t.Errorf("B.IDB = %v, want 4", v)
	}
	// The legs are recorded on the merged row's type.
	if merged.Positional == nil || merged.Positional.Type == nil || len(merged.Positional.Type.Legs) != 2 {
		t.Fatalf("merged row should carry 2 leg windows, got %+v", merged.Positional)
	}
}

// --- passesJoinPredicatesLegs unit tests ---

func TestPassesJoinPredicates_Empty(t *testing.T) {
	t.Parallel()
	qr := dmap(map[string]any{"A": 1})
	ok, err := passesJoinPredicatesLegs(qr, nil, EmptyEvaluationContext(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("empty predicates should pass")
	}
}

func TestPassesJoinPredicates_MatchingPredicate(t *testing.T) {
	t.Parallel()
	qr := dmap(map[string]any{"PRICE": int64(100)})
	pred := predicates.NewComparisonPredicate(
		mustNamedTestField(t, "PRICE", values.NullableLong),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(100)),
	)
	ok, err := passesJoinPredicatesLegs(qr, []predicates.QueryPredicate{pred}, EmptyEvaluationContext(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("matching predicate should pass")
	}
}

func TestPassesJoinPredicates_NonMatchingPredicate(t *testing.T) {
	t.Parallel()
	qr := dmap(map[string]any{"PRICE": int64(100)})
	pred := predicates.NewComparisonPredicate(
		mustNamedTestField(t, "PRICE", values.NullableLong),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(999)),
	)
	ok, err := passesJoinPredicatesLegs(qr, []predicates.QueryPredicate{pred}, EmptyEvaluationContext(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("non-matching predicate should fail")
	}
}
