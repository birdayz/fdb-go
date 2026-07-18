package executor

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestSingleResultCursor_EmitsResumableContinuation pins the C6
// hardening: the emitted row carries the consumed-token continuation —
// never nil (banned by NewResultWithValue: a nil made every parent
// snapshot the child as START and replay the row on resume).
func TestSingleResultCursor_EmitsResumableContinuation(t *testing.T) {
	t.Parallel()
	c := newSingleResultCursor(QueryResult{})
	r, err := c.OnNext(context.Background())
	if err != nil || !r.HasNext() {
		t.Fatalf("OnNext: %v", err)
	}
	cont := r.GetContinuation()
	if cont == nil || cont.IsEnd() {
		t.Fatalf("single-result row must carry a resumable continuation, got %v", cont)
	}
	b, err := cont.ToBytes()
	if err != nil || len(b) == 0 {
		t.Fatalf("continuation bytes: %v (%d bytes)", err, len(b))
	}
	// Exhausted after the row.
	r2, err := c.OnNext(context.Background())
	if err != nil || r2.HasNext() || !r2.GetContinuation().IsEnd() {
		t.Fatalf("second OnNext must be SourceExhausted END, got %+v (%v)", r2, err)
	}
}

// TestFirstOrDefault_ConsumedTokenResumesEmpty pins the resume arm: a
// continuation equal to the consumed token means the single row was
// already emitted — the resumed cursor is EMPTY, and the inner plan is
// never re-executed (the nil inner would panic if it were).
func TestFirstOrDefault_ConsumedTokenResumesEmpty(t *testing.T) {
	t.Parallel()
	p := plans.NewRecordQueryFirstOrDefaultPlan(nil, nil)
	cur, err := executeFirstOrDefault(
		context.Background(), p, nil, EmptyEvaluationContext(),
		singleResultConsumedToken, recordlayer.DefaultExecuteProperties(),
	)
	if err != nil {
		t.Fatalf("executeFirstOrDefault: %v", err)
	}
	defer cur.Close()
	r, err := cur.OnNext(context.Background())
	if err != nil {
		t.Fatalf("OnNext: %v", err)
	}
	if r.HasNext() {
		t.Fatal("a consumed-token resume must be EMPTY — re-emitting duplicates the row")
	}
}
