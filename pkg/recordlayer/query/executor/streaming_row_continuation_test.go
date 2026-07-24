package executor

// RFC-180 A6: the aggregate and sort cursors' PER-ROW continuations are real,
// lazily-encoded resume points — the retired fake nonEndContinuation ({0x00})
// must never ride a group-break / final-group / sort-emit row again.
//
// Java reference: AggregateCursor.java:84-180 — previousContinuationInGroup is
// the inner continuation of the last row ACCEPTED into the current group; a
// group-break emit carries (previousContinuationInGroup, NO partial state) and
// then advances previousContinuationInGroup to the BREAK row's continuation
// (the single-element-group movement table in Java's comment block). Resuming
// from an emitted row's continuation yields exactly the remaining groups (the
// next group's first row is deliberately re-read).
//
// The sort cursor is a Go extension (Java's Cascades has no physical sort);
// its emit-phase rows carry (inner-EXHAUSTED marker, remaining sorted buffer)
// via the F53 sort codec, so a resume goes straight to emit — never re-running
// the exhausted inner.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestAggregateInputIsFlatFrontier_UnorderedPrimaryKeyDistinct(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan(
		[]string{"T"},
		values.UnknownType,
		false,
	)
	if !aggregateInputIsFlatFrontier(
		plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(scan),
	) {
		t.Fatal("primary-key distinct must transparently preserve a flat scan frontier")
	}
	join := plans.NewRecordQueryNestedLoopJoinPlan(
		nil,
		nil,
		nil,
		plans.JoinInner,
		"outer",
		"inner",
		nil,
	)
	if aggregateInputIsFlatFrontier(
		plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(join),
	) {
		t.Fatal("primary-key distinct must not turn a multi-source join into a flat frontier")
	}
}

// listPosContinuation is recordlayer.FromList's per-row continuation for
// "resume after the row at index pos-1": a 4-byte big-endian position
// (listCursorContinuation, matching Java's ByteBuffer.putInt).
func listPosContinuation(pos int) []byte {
	return []byte{byte(pos >> 24), byte(pos >> 16), byte(pos >> 8), byte(pos)}
}

// decodeAggRowContinuation decodes an emitted aggregate row's continuation and
// asserts it carries NO partial accumulator state (a completed group's emit is
// stateless — Java's 2-arg AggregateCursorContinuation constructor).
func decodeAggRowContinuation(t *testing.T, cont recordlayer.RecordCursorContinuation, numAggs int) []byte {
	t.Helper()
	if cont == nil {
		t.Fatal("emitted aggregate row has nil continuation")
	}
	if cont.IsEnd() {
		t.Fatal("emitted aggregate row has END continuation")
	}
	b, err := cont.ToBytes()
	if err != nil {
		t.Fatalf("row continuation ToBytes: %v", err)
	}
	inner, groupKey, gs, err := decodeAggregateContinuation(b, numAggs)
	if err != nil {
		t.Fatalf("emitted aggregate row's continuation does not decode (the retired fake?): %v", err)
	}
	if gs != nil {
		t.Fatalf("group-break emit carries partial state (groupKey=%q) — Java emits (previousContinuationInGroup, NO state)", groupKey)
	}
	return inner
}

// aggGroupedFixture returns (rows, groupingKeys, aggregates) for
// SUM(amount) GROUP BY dept over dept-sorted rows. qr/dmap lays columns out
// alphabetically: amount@0, dept@1.
func aggGroupedFixture() ([]QueryResult, []values.Value, []expressions.AggregateSpec) {
	rows := []QueryResult{
		qr("dept", "A", "amount", int64(10)),
		qr("dept", "A", "amount", int64(20)),
		qr("dept", "B", "amount", int64(30)),
		qr("dept", "B", "amount", int64(40)),
		qr("dept", "C", "amount", int64(50)),
	}
	groupKeys := []values.Value{values.NewFieldValueWithResolvedOrdinal("dept", 1, values.TypeString)}
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggSum, Operand: values.NewFieldValueWithResolvedOrdinal("amount", 0, values.TypeInt)},
	}
	return rows, groupKeys, aggs
}

// TestAggregateCursor_GroupBreakContinuation_ResumesRemainingGroups pins the
// full Java movement: the group-break row's continuation decodes to (inner
// continuation of the group's LAST ACCEPTED row, no state); resuming a fresh
// cursor from that inner position yields exactly the remaining groups — no
// duplicate of the emitted group, no dropped group. Java AggregateCursor's
// scenario-2 comment: the break row is re-read on resume, by design.
func TestAggregateCursor_GroupBreakContinuation_ResumesRemainingGroups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rows, groupKeys, aggs := aggGroupedFixture()

	c := newAggregateCursor(recordlayer.FromList(rows), groupKeys, aggs, nil, nil)
	defer c.Close()

	// First emit: group A (break on row index 2, the first B row).
	r1, err := c.OnNext(ctx)
	if err != nil {
		t.Fatalf("OnNext: %v", err)
	}
	if !r1.HasNext() {
		t.Fatalf("expected group A row, got no-next (%v)", r1.GetNoNextReason())
	}
	m1, _ := rowMapOK(r1.GetValue())
	if m1["DEPT"] != "A" || m1["SUM(AMOUNT)"] != int64(30) {
		t.Fatalf("group A row = %v, want DEPT=A SUM=30", m1)
	}
	// Its continuation = the LAST row accepted into A (index 1) → list pos 2.
	inner1 := decodeAggRowContinuation(t, r1.GetContinuation(), len(aggs))
	if want := listPosContinuation(2); string(inner1) != string(want) {
		t.Fatalf("group A emit's inner continuation = %v, want %v (last row ACCEPTED into the group)", inner1, want)
	}

	// Resume a FRESH cursor from the emitted row's continuation, wired the way
	// executeAggregation wires it (inner from the decoded position, NO state,
	// previousContinuationInGroup initialized to the decoded inner position).
	c2 := newAggregateCursor(recordlayer.FromListWithContinuation(rows, inner1), groupKeys, aggs, nil, nil)
	c2.previousContinuationInGroup = recordlayer.NewBytesContinuation(inner1)
	defer c2.Close()

	var got []string
	for {
		r, err := c2.OnNext(ctx)
		if err != nil {
			t.Fatalf("resumed OnNext: %v", err)
		}
		if !r.HasNext() {
			if !r.GetNoNextReason().IsSourceExhausted() {
				t.Fatalf("resumed cursor stopped with %v, want SourceExhausted", r.GetNoNextReason())
			}
			break
		}
		m, _ := rowMapOK(r.GetValue())
		got = append(got, fmt.Sprintf("%v=%v", m["DEPT"], m["SUM(AMOUNT)"]))
	}
	want := "[B=70 C=50]"
	if fmt.Sprint(got) != want {
		t.Fatalf("resume from group A's continuation = %v, want %v (no duplicate A, no dropped group)", got, want)
	}
}

// TestAggregateCursor_SingleElementGroups_ContinuationAdvance pins Java's
// post-emit movement (AggregateCursor.java:131-146): after a group-break emit,
// previousContinuationInGroup advances to the BREAK row's continuation, so
// consecutive single-element groups each carry their own (only) row's position.
func TestAggregateCursor_SingleElementGroups_ContinuationAdvance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rows := []QueryResult{
		qr("dept", "A", "amount", int64(1)),
		qr("dept", "B", "amount", int64(2)),
		qr("dept", "C", "amount", int64(3)),
	}
	groupKeys := []values.Value{values.NewFieldValueWithResolvedOrdinal("dept", 1, values.TypeString)}
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggSum, Operand: values.NewFieldValueWithResolvedOrdinal("amount", 0, values.TypeInt)},
	}

	c := newAggregateCursor(recordlayer.FromList(rows), groupKeys, aggs, nil, nil)
	defer c.Close()

	// Emits: A (break at row1) → inner pos 1; B (break at row2) → inner pos 2;
	// C (final, exhausted) → inner pos 3.
	wantInner := [][]byte{listPosContinuation(1), listPosContinuation(2), listPosContinuation(3)}
	for i, want := range wantInner {
		r, err := c.OnNext(ctx)
		if err != nil {
			t.Fatalf("OnNext %d: %v", i, err)
		}
		if !r.HasNext() {
			t.Fatalf("emit %d: expected a group row, got no-next (%v)", i, r.GetNoNextReason())
		}
		inner := decodeAggRowContinuation(t, r.GetContinuation(), len(aggs))
		if string(inner) != string(want) {
			t.Fatalf("emit %d inner continuation = %v, want %v", i, inner, want)
		}
	}

	// After the final emit: SourceExhausted with an END continuation.
	r, err := c.OnNext(ctx)
	if err != nil {
		t.Fatalf("post-final OnNext: %v", err)
	}
	if r.HasNext() || !r.GetNoNextReason().IsSourceExhausted() || !r.GetContinuation().IsEnd() {
		t.Fatalf("post-final result = hasNext=%v reason=%v end=%v, want (false, SourceExhausted, END)",
			r.HasNext(), r.GetNoNextReason(), r.GetContinuation() != nil && r.GetContinuation().IsEnd())
	}
}

// TestAggregateCursor_ScalarFinalContinuation_NoDuplicateOnResume pins the
// scalar (no grouping keys) final emit: its continuation decodes to the LAST
// input row's position with no state, and a cursor resumed from there emits
// NOTHING — Java AggregateCursor.isNoRecords() returns exhausted; the bare
// cursor never fabricates a default row (that is executeAggregation's OrElse
// alternative, mirroring Java's DefaultOnEmpty(StreamingAggregation) plan).
func TestAggregateCursor_ScalarFinalContinuation_NoDuplicateOnResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rows := []QueryResult{
		qr("v", int64(10)),
		qr("v", int64(20)),
		qr("v", int64(30)),
	}
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: nil}}, // COUNT(*)
	}

	c := newAggregateCursor(recordlayer.FromList(rows), nil, aggs, nil, nil)
	defer c.Close()

	r, err := c.OnNext(ctx)
	if err != nil {
		t.Fatalf("OnNext: %v", err)
	}
	if !r.HasNext() {
		t.Fatalf("expected the scalar COUNT row, got no-next (%v)", r.GetNoNextReason())
	}
	m, _ := rowMapOK(r.GetValue())
	if m["COUNT(*)"] != int64(3) {
		t.Fatalf("COUNT(*) = %v, want 3", m["COUNT(*)"])
	}
	inner := decodeAggRowContinuation(t, r.GetContinuation(), len(aggs))
	if want := listPosContinuation(3); string(inner) != string(want) {
		t.Fatalf("scalar final emit's inner continuation = %v, want %v", inner, want)
	}

	// Resume past the last row: the bare cursor must emit NOTHING (no duplicate
	// COUNT row, no fabricated COUNT=0).
	c2 := newAggregateCursor(recordlayer.FromListWithContinuation(rows, inner), nil, aggs, nil, nil)
	c2.previousContinuationInGroup = recordlayer.NewBytesContinuation(inner)
	defer c2.Close()
	r2, err := c2.OnNext(ctx)
	if err != nil {
		t.Fatalf("resumed OnNext: %v", err)
	}
	if r2.HasNext() {
		m2, _ := rowMapOK(r2.GetValue())
		t.Fatalf("resume past the scalar final emit produced a row %v — duplicate/fabricated aggregate output", m2)
	}
	if !r2.GetNoNextReason().IsSourceExhausted() {
		t.Fatalf("resumed scalar cursor reason = %v, want SourceExhausted", r2.GetNoNextReason())
	}
}

// errOnNextCursor fails the test if pulled — proves an emit-phase sort resume
// goes straight to emit and never re-runs the (exhausted) inner.
type errOnNextCursor struct{ closed bool }

func (c *errOnNextCursor) OnNext(context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	return recordlayer.RecordCursorResult[QueryResult]{}, errors.New("inner cursor pulled during an emit-phase sort resume (the inner was EXHAUSTED when the continuation was taken)")
}
func (c *errOnNextCursor) Close() error   { c.closed = true; return nil }
func (c *errOnNextCursor) IsClosed() bool { return c.closed }

// TestCustomSortCursor_EmitContinuation_ResumesRemainingRows pins the sort
// emit-phase per-row continuation: it decodes to (inner-EXHAUSTED marker,
// REMAINING sorted buffer), and a cursor restored the way executeInMemorySort
// restores it (buf = remaining, loaded = true) emits exactly the remaining
// rows without touching the inner.
func TestCustomSortCursor_EmitContinuation_ResumesRemainingRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := recordlayer.FromList([]QueryResult{
		qr("X", int64(3)),
		qr("X", int64(1)),
		qr("X", int64(4)),
		qr("X", int64(2)),
	})
	c := newCustomSortCursor(inner, expressionSortFn([]expressions.SortKey{
		{Value: values.NewFieldValueWithResolvedOrdinal("X", 0, values.UnknownType)},
	}), nil)
	defer c.Close()

	// Pull two rows (1, 2) of the sorted output (1,2,3,4).
	var second recordlayer.RecordCursorResult[QueryResult]
	for i := 0; i < 2; i++ {
		r, err := c.OnNext(ctx)
		if err != nil {
			t.Fatalf("OnNext %d: %v", i, err)
		}
		if !r.HasNext() {
			t.Fatalf("OnNext %d: no row", i)
		}
		second = r
	}
	if got := fieldVal(t, second.GetValue(), "X"); got != int64(2) {
		t.Fatalf("second sorted row = %d, want 2", got)
	}

	cont := second.GetContinuation()
	if cont == nil || cont.IsEnd() {
		t.Fatal("emitted sort row carries nil/END continuation")
	}
	b, err := cont.ToBytes()
	if err != nil {
		t.Fatalf("sort row continuation ToBytes: %v", err)
	}
	_, remaining, exhausted, err := decodeSortContinuation(b, nil)
	if err != nil {
		t.Fatalf("emitted sort row's continuation does not decode (the retired fake?): %v", err)
	}
	if !exhausted {
		t.Fatal("emit-phase sort continuation must carry the inner-EXHAUSTED marker (a resume must not re-run the inner)")
	}
	if len(remaining) != 2 {
		t.Fatalf("remaining buffer = %d rows, want 2 (rows 3 and 4)", len(remaining))
	}

	// Restore exactly as executeInMemorySort does for an emit-phase resume:
	// buf = remaining, loaded = true — straight to emit, inner untouched.
	c2 := newCustomSortCursor(&errOnNextCursor{}, expressionSortFn(nil), nil)
	c2.buf = remaining
	c2.loaded = true
	defer c2.Close()

	var got []int64
	for {
		r, err := c2.OnNext(ctx)
		if err != nil {
			t.Fatalf("resumed OnNext: %v", err)
		}
		if !r.HasNext() {
			if !r.GetNoNextReason().IsSourceExhausted() {
				t.Fatalf("resumed sort stopped with %v, want SourceExhausted", r.GetNoNextReason())
			}
			break
		}
		got = append(got, fieldVal(t, r.GetValue(), "X"))
	}
	if fmt.Sprint(got) != "[3 4]" {
		t.Fatalf("resume from the mid-emit continuation = %v, want [3 4]", got)
	}
}

// TestSortContinuation_FillPhaseCarriesNoExhaustedMarker pins the discriminator:
// a FILL-phase sort continuation (inner NOT exhausted) must NOT carry the
// emit-phase marker — its resume re-opens the inner and keeps loading.
func TestSortContinuation_FillPhaseCarriesNoExhaustedMarker(t *testing.T) {
	t.Parallel()
	buf := []QueryResult{qr("X", int64(1))}
	b, err := encodeSortContinuation(recordlayer.NewBytesContinuation([]byte("INNER")), buf, false)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	inner, got, exhausted, err := decodeSortContinuation(b, nil)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if exhausted {
		t.Fatal("fill-phase continuation decoded as emit-phase (exhausted) — resume would skip the rest of the scan")
	}
	if string(inner) != "INNER" || len(got) != 1 {
		t.Fatalf("fill-phase round-trip: inner=%q rows=%d, want INNER/1", inner, len(got))
	}
}
