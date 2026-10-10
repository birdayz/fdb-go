package client

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestGRVIdleFlushRetiresBeforeReplyFanout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := openTestDB(t, ctx)
	b := &grvBatcher{priority: grvPriorityDefault}
	first, second := make(chan grvResult), make(chan grvResult)
	b.mu.Lock()
	b.pending = []grvRequest{{reply: first}, {reply: second}}
	batch := b.detachLocked()
	inFlight := b.inFlight
	b.mu.Unlock()
	if inFlight != 1 {
		t.Fatalf("detachment registered %d batches, want 1", inFlight)
	}
	done := make(chan struct{})
	go func() {
		b.flush(db.db, batch)
		close(done)
	}()
	defer func() {
		// Unblock both test-only unbuffered channels even after a failed assertion.
		db.Close()
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-done:
				return
			case <-first:
			case <-second:
			case <-timer.C:
				t.Error("GRV flush did not terminate during cleanup")
				return
			}
		}
	}()

	var firstResult grvResult
	select {
	case firstResult = <-first:
		if firstResult.err != nil || firstResult.version <= 0 {
			t.Fatalf("first GRV waiter: %+v", firstResult)
		}
	case <-ctx.Done():
		t.Fatal("first GRV waiter never received a result:", ctx.Err())
	}
	// The second send cannot complete yet: retirement after fanout cannot race
	// this assertion into a false pass.
	b.mu.Lock()
	inFlight = b.inFlight
	b.mu.Unlock()
	if inFlight != 0 {
		t.Errorf("GRV_FANOUT_BUSY: %d batches still counted after the first reply, want 0", inFlight)
	}
	select {
	case <-done:
		t.Fatal("flush finished without delivering to its second waiter")
	default:
	}
	select {
	case result := <-second:
		if result.err != nil || result.version != firstResult.version {
			t.Fatalf("second GRV waiter: %+v, first version=%d", result, firstResult.version)
		}
	case <-ctx.Done():
		t.Fatal("second GRV waiter never received a result:", ctx.Err())
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("flush did not finish after fanout:", ctx.Err())
	}
	b.mu.Lock()
	inFlight = b.inFlight
	b.mu.Unlock()
	if inFlight != 0 {
		t.Fatalf("GRV_DOUBLE_RETIRE: inFlight=%d after normal return, want 0", inFlight)
	}
}

func TestGRVIdleLaterReadDoesNotJoinEarlierRPC(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	reader, gate, _ := newGRVReplyGateDB(t, ctx)
	writer := openTestDB(t, ctx)
	key, value := []byte(t.Name()), []byte("committed after the earlier GRV")
	gate.arm(1, false)
	earlierDone := startGatedGRV(reader, ctx)
	awaitGateSignal(t, gate.heldCh, "withholding the earlier GRV reply")
	gate.mu.Lock()
	body := gate.held[0].body
	gate.mu.Unlock()
	heldVersion, _, _, _, _, _, err := parseGetReadVersionReply(body)
	if err != nil || heldVersion <= 0 {
		t.Fatalf("held GRV reply: version=%d err=%v", heldVersion, err)
	}

	write := writer.CreateTransaction()
	write.Set(key, value)
	if err := write.Commit(ctx); err != nil {
		t.Fatal("intervening commit:", err)
	}
	committed, err := write.GetCommittedVersion()
	if err != nil || committed <= heldVersion {
		t.Fatalf("intervening commit version=%d err=%v, want greater than held GRV=%d", committed, err, heldVersion)
	}
	later := reader.CreateTransaction()
	defer later.Cancel()
	type laterResult struct {
		version int64
		value   []byte
		err     error
	}
	laterDone := make(chan laterResult, 1)
	go func() {
		version, err := later.GetReadVersion(ctx)
		var got []byte
		if err == nil {
			got, err = later.Get(ctx, key)
		}
		laterDone <- laterResult{version: version, value: got, err: err}
	}()
	select {
	case result := <-laterDone:
		if result.err != nil || result.version < committed || !bytes.Equal(result.value, value) {
			t.Fatalf("later read: version=%d value=%q error=%v, want version>=%d value=%q", result.version, result.value, result.err, committed, value)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("later transaction did not finish independently of the held reply")
	}
	if requests, held, err := gate.counts(); requests != 2 || held != 1 || err != nil {
		t.Fatalf("later reader needs its own fresh GRV: requests=%d held=%d err=%v", requests, held, err)
	}
	select {
	case result := <-earlierDone:
		t.Fatalf("earlier GRV completed before its reply was released: %+v", result)
	default:
	}
	gate.release()
	select {
	case result := <-earlierDone:
		if result.err != nil || result.version != heldVersion {
			t.Fatalf("earlier GRV after release: %+v, want version=%d", result, heldVersion)
		}
	case <-ctx.Done():
		t.Fatal("earlier GRV did not finish after release:", ctx.Err())
	}
	if reader.db.metrics.Snapshot().GRVCacheHits != 0 {
		t.Fatal("freshness test unexpectedly used the GRV cache")
	}
}
