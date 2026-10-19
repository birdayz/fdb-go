package client

import (
	"context"
	"testing"
	"time"
)

func TestGRVLatencyCoversRPCAndBroadcast(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, gate, _ := newGRVReplyGateDB(t, ctx)
	gate.arm(1, false)
	batch := []grvRequest{batchRequestForTiming(), batchRequestForTiming()}
	// flush clears its batch for reuse once it returns; waiters keep their own channels.
	replies := []chan grvResult{batch[0].reply, batch[1].reply}
	started := time.Unix(1000, 0)
	observations := 0
	startSampled := make(chan struct{})
	b := &grvBatcher{priority: grvPriorityDefault, inFlight: 1, now: func() time.Time {
		observations++
		if observations == 1 {
			close(startSampled)
			return started
		}
		for i, req := range batch {
			if len(req.reply) != 1 {
				t.Errorf("sample ended before waiter %d received its reply", i)
			}
		}
		return started.Add(2 * time.Millisecond)
	}}
	done := make(chan struct{})
	go func() { defer close(done); b.flush(db.db, batch) }()
	waitCommitLifetimeGate(t, ctx, gate.heldCh, "GRV reply held")
	select {
	case <-startSampled:
	default:
		t.Error("latency sample did not begin before the RPC reply was released")
	}
	gate.release()
	waitCommitLifetimeGate(t, ctx, done, "GRV broadcast and sample completed")
	if observations != 2 || b.batchTime != 100*time.Microsecond || b.inFlight != 0 {
		t.Fatalf("observations/window/inFlight = %d/%v/%d, want 2/100us/0", observations, b.batchTime, b.inFlight)
	}
	for i, reply := range replies {
		result := <-reply
		if result.err != nil || result.version <= 0 || len(reply) != 0 {
			t.Errorf("waiter %d = %+v, want one successful real GRV", i, result)
		}
	}
}

func batchRequestForTiming() grvRequest {
	return grvRequest{reply: make(chan grvResult, 1)}
}
