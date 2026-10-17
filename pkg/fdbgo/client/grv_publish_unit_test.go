package client

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

func publicationBatch(n, capacity int) []grvRequest {
	batch := make([]grvRequest, n)
	for i := range batch {
		batch[i].reply = make(chan grvResult, capacity)
	}
	return batch
}

func TestGRVSampleFollowsBroadcast(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 19, 20, 21, 39, 40, 41, 1000} {
		for _, failed := range []bool{false, true} {
			t.Run(fmt.Sprintf("n=%d/error=%v", n, failed), func(t *testing.T) {
				t.Parallel()
				batch := publicationBatch(n, 1)
				started := time.Unix(1000, 0)
				observations := 0
				clock := started.Add(2 * time.Millisecond)
				var positions, wantPositions []int
				// C++ BROADCAST_BATCH_SIZE is 20, independent of Go's constant.
				for i := 20; i <= n; i += 20 {
					wantPositions = append(wantPositions, i)
				}
				if !failed {
					wantPositions = append(wantPositions, n) // timeReply's final yield
				}
				b := &grvBatcher{now: func() time.Time {
					observations++
					for i, req := range batch {
						if len(req.reply) != 1 {
							t.Errorf("latency sampled before waiter %d received its reply", i)
						}
					}
					return clock
				}, yield: func() {
					delivered := 0
					for _, req := range batch {
						delivered += len(req.reply)
					}
					positions = append(positions, delivered)
					clock = clock.Add(40 * time.Microsecond)
				}}
				result := grvResult{version: 777, locked: true, instant: started}
				if failed {
					result.err = errors.New("completed GRV error")
				}
				published := 0
				b.publishReply(batch, result, started, &published)
				if published != n {
					t.Fatalf("published = %d, want %d", published, n)
				}
				if !slices.Equal(positions, wantPositions) {
					t.Errorf("yield positions = %v, want %v", positions, wantPositions)
				}
				// EMA begins at zero: 2ms RPC -> 100us; each 40us yield adds 2us.
				wantObservations, wantWindow := 1, time.Duration(100+2*len(wantPositions))*time.Microsecond
				if failed {
					wantObservations, wantWindow = 0, 0
				}
				if observations != wantObservations || b.batchTime != wantWindow {
					t.Errorf("observations/window = %d/%v, want %d/%v", observations, b.batchTime, wantObservations, wantWindow)
				}
				for i, req := range batch {
					if got := <-req.reply; got != result || len(req.reply) != 0 {
						t.Errorf("waiter %d = %+v, want one unchanged %+v", i, got, result)
					}
				}
			})
		}
	}
}

func TestGRVRecoveryPreservesPublishedPrefix(t *testing.T) {
	t.Parallel()
	var db database
	b := &grvBatcher{}
	// Capacity two turns an erroneous second publication into an assertion
	// failure instead of letting the regression hang on a full channel.
	batch := publicationBatch(3, 2)
	result := grvResult{version: 777}
	batch[0].reply <- result
	published := 1
	func() {
		defer b.recoverFlush(&db, batch, &published)
		panic("after publishing the first waiter")
	}()
	for i, req := range batch {
		if len(req.reply) != 1 {
			t.Fatalf("waiter %d received %d results, want one", i, len(req.reply))
		}
		got := <-req.reply
		if i == 0 && got != result || i > 0 && got.err == nil {
			t.Errorf("waiter %d result = %+v", i, got)
		}
	}
	if published != len(batch) || db.metrics.Snapshot().RecoveredPanics != 1 {
		t.Fatal("recovery did not complete the remaining waiters exactly once")
	}
}

func TestGRVObservationPanicDoesNotRepublish(t *testing.T) {
	t.Parallel()
	var db database
	b := &grvBatcher{now: func() time.Time { panic("sampling after publication") }}
	batch := publicationBatch(3, 2)
	result := grvResult{version: 777}
	published := 0
	func() {
		defer b.recoverFlush(&db, batch, &published)
		b.publishReply(batch, result, time.Unix(1000, 0), &published)
	}()
	for i, req := range batch {
		if len(req.reply) != 1 {
			t.Fatalf("waiter %d received %d results, want one", i, len(req.reply))
		}
		if got := <-req.reply; got != result {
			t.Errorf("waiter %d result changed after sampling panic: %+v", i, got)
		}
	}
	if published != len(batch) || db.metrics.Snapshot().RecoveredPanics != 1 {
		t.Fatal("sampling panic was not recovered after completed publication")
	}
}
