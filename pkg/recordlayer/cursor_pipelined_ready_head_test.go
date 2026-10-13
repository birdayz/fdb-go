package recordlayer

import (
	"context"
	"errors"
	"testing"
)

// These schedules are structural: issue exposes a completed head before the
// mapper can advance the source again, or keeps it pending until that advance.
func TestMapPipelinedReadyHeadBeforeSourceError(t *testing.T) {
	t.Parallel()
	for _, ready := range []bool{true, false} {
		name := "pending"
		if ready {
			name = "completed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			boom := &ScanLimitReachedError{}
			continuation := NewBytesContinuation(ListCursorContinuation(1))
			source := &pipelineScanSource{
				rows:   []RecordCursorResult[int]{NewResultWithValue(7, continuation)},
				failAt: 1, err: boom,
			}
			pending := &pipelineRingPending{value: 70, ready: ready}
			issued := 0
			cursor := MapPipelined(source, func(value int) PendingRead[int] {
				issued++
				if value != 7 || pending.IsReady() != ready {
					t.Fatal("mapping gate did not establish the requested readiness")
				}
				return pending
			}, 3)
			defer cursor.Close()
			first, err := cursor.OnNext(context.Background())
			if ready {
				if err != nil || !first.HasNext() || first.GetValue() != 70 || first.GetContinuation() != continuation {
					t.Fatalf("ready row discarded: result=%v error=%v", first, err)
				}
				if source.pulls != 1 || pending.gets != 1 {
					t.Fatalf("ready head advanced source: pulls=%d gets=%d", source.pulls, pending.gets)
				}
			} else if !errors.Is(err, boom) || first.HasNext() || pending.gets != 0 {
				t.Fatalf("pending head hid source failure: result=%v error=%v gets=%d", first, err, pending.gets)
			}
			for range 2 {
				if _, err := cursor.OnNext(context.Background()); !errors.Is(err, boom) {
					t.Fatalf("source failure not sticky: %v", err)
				}
			}
			if source.pulls != 2 || issued != 1 {
				t.Fatalf("gate population: pulls=%d issued=%d, want 2 and 1", source.pulls, issued)
			}
		})
	}
}

func TestMapPipelinedStopsWhenEarlierHeadCompletesDuringIssue(t *testing.T) {
	t.Parallel()
	boom := &ScanLimitReachedError{}
	firstPending := &pipelineRingPending{value: 10}
	secondPending := &pipelineRingPending{value: 20}
	source := &pipelineScanSource{
		rows: []RecordCursorResult[int]{
			NewResultWithValue(1, NewBytesContinuation(ListCursorContinuation(1))),
			NewResultWithValue(2, NewBytesContinuation(ListCursorContinuation(2))),
		}, failAt: 2, err: boom,
	}
	cursor := MapPipelined(source, func(value int) PendingRead[int] {
		if value == 1 {
			return firstPending
		}
		// The second mapping remains pending; the completion check must inspect
		// the head rather than the mapping it has just inserted.
		firstPending.ready = true
		return secondPending
	}, 3)
	defer cursor.Close()
	first, err := cursor.OnNext(context.Background())
	if err != nil || !first.HasNext() || first.GetValue() != 10 || source.pulls != 2 || firstPending.gets != 1 || secondPending.gets != 0 {
		t.Fatalf("completed prefix lost: result=%v error=%v pulls=%d gets=%d/%d", first, err, source.pulls, firstPending.gets, secondPending.gets)
	}
	if _, err := cursor.OnNext(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("pending second mapping hid source failure: %v", err)
	}
	if source.pulls != 3 || secondPending.gets != 0 {
		t.Fatalf("pending branch population: pulls=%d second gets=%d", source.pulls, secondPending.gets)
	}
}

func TestMapPipelinedCompletedMappingFailurePrecedesUnissuedSourceFailure(t *testing.T) {
	t.Parallel()
	mappingError := errors.New("completed mapping failed")
	sourceError := &ScanLimitReachedError{}
	source := &pipelineScanSource{
		rows:   []RecordCursorResult[int]{NewResultWithValue(1, NewBytesContinuation(ListCursorContinuation(1)))},
		failAt: 1, err: sourceError,
	}
	pending := &pipelineRingPending{ready: true, err: mappingError}
	cursor := MapPipelined(source, func(int) PendingRead[int] { return pending }, 3)
	defer cursor.Close()
	for range 2 {
		if _, err := cursor.OnNext(context.Background()); !errors.Is(err, mappingError) {
			t.Fatalf("completed mapping error replaced by later source error: %v", err)
		}
	}
	if source.pulls != 1 || pending.gets != 1 {
		t.Fatalf("mapping error population: source pulls=%d gets=%d, want 1/1", source.pulls, pending.gets)
	}
}

func TestMapPipelinedQueuedReadyHeadStillPullsSourceOnEntry(t *testing.T) {
	t.Parallel()
	boom := &ScanLimitReachedError{}
	source := &pipelineScanSource{
		rows: []RecordCursorResult[int]{
			NewResultWithValue(1, NewBytesContinuation(ListCursorContinuation(1))),
			NewResultWithValue(2, NewBytesContinuation(ListCursorContinuation(2))),
		}, failAt: 2, err: boom,
	}
	pending := [2]pipelineRingPending{{value: 10}, {value: 20}}
	cursor := MapPipelined(source, func(value int) PendingRead[int] { return &pending[value-1] }, 2)
	defer cursor.Close()
	first, err := cursor.OnNext(context.Background())
	if err != nil || !first.HasNext() || first.GetValue() != 10 || source.pulls != 2 {
		t.Fatalf("initial window: result=%v error=%v pulls=%d", first, err, source.pulls)
	}
	// Java starts inner.onNext on re-entry with spare capacity, before its
	// post-insertion completion check. Do not invent a pre-pull shortcut.
	pending[1].ready = true
	if _, err := cursor.OnNext(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("queued readiness incorrectly bypassed source failure: %v", err)
	}
	if source.pulls != 3 || pending[0].gets != 1 || pending[1].gets != 0 {
		t.Fatalf("re-entry population: pulls=%d gets=%d/%d, want 3/1/0", source.pulls, pending[0].gets, pending[1].gets)
	}
}
