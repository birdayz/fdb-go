package client

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

// The model's update rules (LoadBalance.actor.h:694-716) with the 7.3.77 knobs.
func TestQueueModelSecondBudget(t *testing.T) {
	t.Parallel()

	t.Run("fresh", func(t *testing.T) {
		q := newQueueModel()
		if b, m := secondState(q); b != 0 || m != 1 {
			t.Fatalf("fresh model budget=%v multiplier=%v; want 0 and 1 (QueueModel.h:121)", b, m)
		}
		if q.spendSecond() {
			t.Fatal("a fresh model paid for a second request")
		}
	})

	// Twenty lone replies earn the first hedge, in the same double arithmetic.
	t.Run("twenty-replies", func(t *testing.T) {
		q := newQueueModel()
		for range 19 {
			q.firstReplied()
		}
		if q.spendSecond() {
			t.Fatal("19 lone replies paid for a second request")
		}
		q.firstReplied()
		if !q.spendSecond() {
			b, _ := secondState(q)
			t.Fatalf("20 lone replies (budget %v) did not pay for a second request", b)
		}
		if q.spendSecond() {
			t.Fatal("one budget unit paid twice")
		}
	})

	t.Run("spend", func(t *testing.T) {
		q := budgetModel(1)
		q.secondMultiplier = 1.2
		if !q.spendSecond() {
			t.Fatal("budget 1.0 did not pay")
		}
		if b, m := secondState(q); b != 0 || math.Abs(m-1.21) > 1e-12 {
			t.Fatalf("after a spend budget=%v multiplier=%v; want 0 and 1.21", b, m)
		}
		q = budgetModel(0.95)
		if q.spendSecond() {
			t.Fatal("budget 0.95 paid")
		}
		if b, m := secondState(q); b != 0.95 || m != 1 {
			t.Fatalf("an unpaid delay changed the model: budget=%v multiplier=%v", b, m)
		}
	})

	t.Run("decay-and-floor", func(t *testing.T) {
		q := newQueueModel()
		q.secondMultiplier = 1.0003
		q.firstReplied()
		if _, m := secondState(q); math.Abs(m-1.00005) > 1e-12 {
			t.Fatalf("multiplier after one lone reply = %v, want 1.00005", m)
		}
		q.firstReplied()
		if _, m := secondState(q); m != 1 {
			t.Fatalf("multiplier fell to %v, below its 1.0 floor", m)
		}
	})

	t.Run("cap", func(t *testing.T) {
		q := budgetModel(99.98)
		q.firstReplied()
		q.firstReplied()
		if b, _ := secondState(q); b != 100 {
			t.Fatalf("budget %v, want capped at 100", b)
		}
	})

	t.Run("concurrent-spend", func(t *testing.T) {
		q := budgetModel(1)
		var spent atomic.Int32
		var wg sync.WaitGroup
		for range 100 {
			wg.Go(func() {
				if q.spendSecond() {
					spent.Add(1)
				}
			})
		}
		wg.Wait()
		if got := spent.Load(); got != 1 {
			t.Fatalf("one budget unit paid for %d concurrent hedges, want 1", got)
		}
		checkSecondModel(t, q, 1, 0, true)
	})

	t.Run("nil-model", func(t *testing.T) {
		var q *QueueModel
		q.firstReplied()
		if q.spendSecond() {
			t.Fatal("loadBalance without a model sent a second request")
		}
	})
}

// A queued primary reply wins over a ready delay without spending budget
// (LoadBalance.actor.h:694,710). A zero delay exposes random select ordering.
func TestHedgeReplyBeforeReadyDelay(t *testing.T) {
	t.Parallel()
	for i := range 200 {
		q := budgetModel(1)
		ch := make(chan transport.Response, 1)
		ch <- transport.Response{Body: []byte("primary")}
		primary := func() inFlightRPC {
			return inFlightRPC{replyCh: ch, replyHandle: &transport.ReplyHandle{}, addr: "p", delta: 1, start: time.Now()}
		}
		secondary := func() inFlightRPC {
			t.Fatalf("iteration %d: hedged a primary whose reply was ready", i)
			return inFlightRPC{}
		}
		res := sendFrameWithHedge(context.Background(), q, 0, primary, secondary, time.Minute)
		if res.err != nil || res.addr != "p" || res.triedSecondary {
			t.Fatalf("iteration %d: %+v, want the primary's reply", i, res)
		}
		checkSecondModel(t, q, 1, 1, false)
	}
}
