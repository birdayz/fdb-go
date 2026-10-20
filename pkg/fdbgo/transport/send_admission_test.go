package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/internal/hrtimer"
	"fdb.dev/pkg/fdbgo/internal/sendqueue"
)

// This peer has no writeLoop: the test admits and writes each queued frame to
// the real socket itself, independently checking the order decoded by the peer.
func admissionConn(t *testing.T) (*Conn, net.Conn) {
	t.Helper()
	cli, peer := tcpPair(t)
	c := directConn(t, cli)
	c.raw = nil
	c.writeCh = make(chan writeReq, 1)
	var order sendqueue.Queue
	order.Seal()
	WithSendOrder(&order)(c)
	return c, peer
}

func admissionAwait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatalf("%s did not finish", what)
		var zero T
		return zero
	}
}

func admissionSignal(t *testing.T, ctx context.Context) (context.Context, *SuspendSignal) {
	t.Helper()
	ctx, signal := WithSuspendSignal(ctx)
	t.Cleanup(signal.Fire)
	return ctx, signal
}

// Value fires the actual signal, then stops the sender before NotifySuspend
// returns. The FIFO place must already exist; no production hook is needed.
type admissionPauseContext struct {
	context.Context
	release <-chan struct{}
	once    sync.Once
}

func (c *admissionPauseContext) Value(key any) any {
	value := c.Context.Value(key)
	if _, ok := key.(suspendKey); ok {
		if signal, ok := value.(*SuspendSignal); ok {
			c.once.Do(func() { signal.Fire(); <-c.release })
		}
	}
	return value
}

func admissionPaused(t *testing.T, ctx context.Context) (context.Context, *SuspendSignal, func()) {
	t.Helper()
	ctx, signal := admissionSignal(t, ctx)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	return &admissionPauseContext{Context: ctx, release: release}, signal, unblock
}

func admissionBody(token uint64) []byte { return []byte(fmt.Sprintf("frame-%d", token)) }

func admissionFill(t *testing.T, c *Conn) {
	t.Helper()
	if err := c.SendFrameDeferred(UID{First: 1}, admissionBody(1)); err != nil {
		t.Fatal(err)
	}
}

func admissionControl(t *testing.T, c *Conn, queued int64) {
	t.Helper()
	if !c.sendWaiters.Pending() || c.queued.Load() != queued {
		t.Errorf("positive control: FIFO pending=%t queued=%d, want true/%d", c.sendWaiters.Pending(), c.queued.Load(), queued)
	}
}

func admissionFinish(c *Conn, request writeReq) {
	c.queued.Add(-1)
	completeWrite(request, nil)
}

func admissionDrain(t *testing.T, c *Conn, peer net.Conn, want uint64, acknowledge bool) writeReq {
	t.Helper()
	next := make(chan writeReq, 1)
	go func() {
		for {
			request, frame, ready := c.nextWrite(true)
			if frame || !ready {
				next <- request
				return
			}
			// Shared outbox/tail wakes carry no frame to decode or finish.
		}
	}()
	request := admissionAwait(t, next, "queued request")
	marker := request.token == (UID{}) && request.body == nil
	if marker {
		if want != 0 {
			t.Errorf("frame %d was overtaken by a flush marker", want)
		}
	} else {
		if err := WriteFrame(c.conn, request.token, request.body, false); err != nil {
			t.Fatal(err)
		}
		token, body := readFrame(t, peer)
		if want == 0 {
			t.Errorf("peer decoded token=%d body=%q at the flush-marker position", token.First, body)
		} else if token.First != want || string(body) != string(admissionBody(want)) {
			t.Errorf("peer decoded token=%d body=%q, want %d/%q", token.First, body, want, admissionBody(want))
		}
	}
	if acknowledge {
		admissionFinish(c, request)
	}
	return request
}

type admissionResult struct {
	enqueued bool
	err      error
}

func admissionDeferred(c *Conn, ctx context.Context, token uint64) <-chan admissionResult {
	done := make(chan admissionResult, 1)
	go func() {
		ok, err := c.SendFrameDeferredContext(ctx, UID{First: token}, admissionBody(token))
		done <- admissionResult{ok, err}
	}()
	return done
}

func admissionRequireSend(t *testing.T, done <-chan admissionResult) {
	t.Helper()
	if got := admissionAwait(t, done, "deferred sender"); !got.enqueued || got.err != nil {
		t.Fatalf("send = %+v, want enqueued without error", got)
	}
}

func admissionQuiesced(t *testing.T, c *Conn) {
	t.Helper()
	if c.sendWaiters.Pending() || c.heldWaiters.Pending() || c.heldCount.Load() != 0 || c.queued.Load() != 0 || len(c.writeCh)+len(c.prefixCh)+len(c.heldCh) != 0 {
		t.Errorf("admission did not drain: pending=%t heldPending=%t held=%d queued=%d buffered=%d/%d/%d", c.sendWaiters.Pending(), c.heldWaiters.Pending(), c.heldCount.Load(), c.queued.Load(), len(c.writeCh), len(c.prefixCh), len(c.heldCh))
	}
}

func TestAdmissionFullQueueWireOrder(t *testing.T) {
	t.Parallel()
	for _, follower := range []string{"context", "legacy-deferred", "legacy-flushed", "eager"} {
		t.Run(follower, func(t *testing.T) {
			t.Parallel()
			c, peer := admissionConn(t)
			legacyCtx, legacySignal := admissionSignal(t, c.ctx)
			c.ctx = legacyCtx
			admissionFill(t, c)
			headCtx, headSignal, resumeHead := admissionPaused(t, c.ctx)
			head := admissionDeferred(c, headCtx, 2)
			admissionAwait(t, headSignal.Done(), "head suspension")
			admissionControl(t, c, 2)
			followerCtx, followerSignal := admissionSignal(t, c.ctx)
			tail := make(chan error, 1)
			go func() {
				var err error
				switch follower {
				case "context":
					_, err = c.SendFrameDeferredContext(followerCtx, UID{First: 3}, admissionBody(3))
				case "legacy-deferred":
					err = c.SendFrameDeferred(UID{First: 3}, admissionBody(3))
				case "legacy-flushed":
					err = c.SendFrame(UID{First: 3}, admissionBody(3))
				case "eager":
					_, err = c.SendFrameEagerContext(followerCtx, UID{First: 3}, admissionBody(3))
				}
				tail <- err
			}()
			if follower == "legacy-deferred" || follower == "legacy-flushed" {
				admissionAwait(t, legacySignal.Done(), "legacy follower suspension")
			} else {
				admissionAwait(t, followerSignal.Done(), "follower suspension")
			}
			admissionControl(t, c, 3)
			admissionDrain(t, c, peer, 1, true)
			resumeHead()
			admissionDrain(t, c, peer, 2, true)
			admissionDrain(t, c, peer, 3, true)
			admissionRequireSend(t, head)
			if err := admissionAwait(t, tail, "follower send"); err != nil {
				t.Fatal(err)
			}
			admissionQuiesced(t, c)
		})
	}
}

func TestAdmissionCancellationWireOrder(t *testing.T) {
	t.Parallel()
	for cancelled, name := range []string{"head", "middle", "tail"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, peer := admissionConn(t)
			admissionFill(t, c)
			var cancels []context.CancelFunc
			var results []<-chan admissionResult
			var resumeHead func()
			for i := 0; i < 3; i++ {
				ctx, cancel := context.WithCancel(c.ctx)
				t.Cleanup(cancel)
				cancels = append(cancels, cancel)
				var signal *SuspendSignal
				if i == 0 {
					ctx, signal, resumeHead = admissionPaused(t, ctx)
				} else {
					ctx, signal = admissionSignal(t, ctx)
				}
				results = append(results, admissionDeferred(c, ctx, uint64(i+2)))
				admissionAwait(t, signal.Done(), name+" setup suspension")
			}
			admissionControl(t, c, 4)
			cancels[cancelled]()
			if cancelled == 0 {
				resumeHead() // keep the filler queued so cancellation, not enqueue, wins
			}
			got := admissionAwait(t, results[cancelled], "cancelled reservation")
			if got.enqueued || !errors.Is(got.err, context.Canceled) {
				t.Fatalf("cancelled reservation = %+v", got)
			}
			admissionDrain(t, c, peer, 1, true)
			resumeHead()
			for i := 0; i < 3; i++ {
				if i != cancelled {
					admissionDrain(t, c, peer, uint64(i+2), true)
					admissionRequireSend(t, results[i])
				}
			}
			admissionQuiesced(t, c)
		})
	}
}

func TestAdmissionCancellationRacesGrant(t *testing.T) {
	t.Parallel()
	c, peer := admissionConn(t)
	for i := 0; i < 32; i++ {
		admissionFill(t, c)
		headCtx, headSignal, resumeHead := admissionPaused(t, c.ctx)
		head := admissionDeferred(c, headCtx, 2)
		admissionAwait(t, headSignal.Done(), "race head suspension")
		middleCtx, cancel := context.WithCancel(c.ctx)
		middleCtx, middleSignal := admissionSignal(t, middleCtx)
		middle := admissionDeferred(c, middleCtx, 3)
		admissionAwait(t, middleSignal.Done(), "race middle suspension")
		tailCtx, tailSignal := admissionSignal(t, c.ctx)
		tail := admissionDeferred(c, tailCtx, 4)
		admissionAwait(t, tailSignal.Done(), "race tail suspension")
		admissionControl(t, c, 4)
		startCancel := make(chan struct{})
		cancelled := make(chan struct{})
		go func() {
			select {
			case <-startCancel:
			case <-c.ctx.Done():
			}
			cancel()
			close(cancelled)
		}()
		resumeHead()
		filler := admissionAwait(t, c.writeCh, "race filler")
		close(startCancel) // freeing the slot races head acceptance/grant with cancellation
		if err := WriteFrame(c.conn, filler.token, filler.body, false); err != nil {
			t.Fatal(err)
		}
		if token, body := readFrame(t, peer); token.First != 1 || string(body) != string(admissionBody(1)) {
			t.Fatalf("race filler on wire = %d/%q", token.First, body)
		}
		admissionFinish(c, filler)
		admissionDrain(t, c, peer, 2, true)
		got := admissionAwait(t, middle, "racing middle sender")
		if got.enqueued {
			if got.err != nil {
				t.Fatalf("iteration %d: accepted middle = %+v", i, got)
			}
			admissionDrain(t, c, peer, 3, true)
		} else if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("iteration %d: unaccepted middle = %+v", i, got)
		}
		admissionDrain(t, c, peer, 4, true)
		admissionRequireSend(t, head)
		admissionRequireSend(t, tail)
		admissionAwait(t, cancelled, "cancellation goroutine")
		admissionQuiesced(t, c)
	}
}

func TestAdmissionPendingCountBlocksDirectAndTurn(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"direct", "turn"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			cli, peer := tcpPair(t)
			c := directConn(t, cli)
			c.writeCh = make(chan writeReq, 1)
			if mode == "turn" {
				timer, err := hrtimer.New()
				if errors.Is(err, hrtimer.ErrUnsupported) {
					timer = &hrtimer.Timer{} // the guard must return before touching a timer
				} else if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { timer.Close() })
				c.coalesce = timer
			}
			admissionFill(t, c)
			headCtx, signal, resumeHead := admissionPaused(t, c.ctx)
			head := admissionDeferred(c, headCtx, 2)
			admissionAwait(t, signal.Done(), "counted head suspension")
			admissionDrain(t, c, peer, 1, true)
			admissionControl(t, c, 1) // no buffered frame remains; only the waiter is counted
			var took bool
			var err error
			if mode == "direct" {
				took, err = c.tryWriteDirect(UID{First: 3}, admissionBody(3))
			} else {
				took, err = c.takeTurn(c.ctx, UID{First: 3}, admissionBody(3))
			}
			if took || err != nil {
				t.Errorf("%s bypassed the counted waiter: took=%t err=%v", mode, took, err)
			}
			resumeHead()
			admissionDrain(t, c, peer, 2, true)
			admissionRequireSend(t, head)
			admissionQuiesced(t, c)
		})
	}
}

func TestAdmissionFlushCannotOvertakeHead(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			t.Parallel()
			c, peer := admissionConn(t)
			legacyCtx, legacySignal := admissionSignal(t, c.ctx)
			c.ctx = legacyCtx
			admissionFill(t, c)
			headCtx, headSignal, resumeHead := admissionPaused(t, c.ctx)
			head := admissionDeferred(c, headCtx, 2)
			admissionAwait(t, headSignal.Done(), "flush head suspension")
			admissionDrain(t, c, peer, 1, true)
			admissionControl(t, c, 1)
			flushCtx, flushSignal := admissionSignal(t, c.ctx)
			flushed := make(chan error, 1)
			go func() {
				if legacy {
					flushed <- c.Flush()
				} else {
					flushed <- c.FlushContext(flushCtx)
				}
			}()
			if legacy {
				admissionAwait(t, legacySignal.Done(), "legacy flush suspension")
			} else {
				admissionAwait(t, flushSignal.Done(), "flush suspension")
			}
			admissionControl(t, c, 2)
			resumeHead()
			admissionDrain(t, c, peer, 2, true)
			admissionDrain(t, c, peer, 0, true)
			if err := admissionAwait(t, flushed, "flush acknowledgment"); err != nil {
				t.Fatal(err)
			}
			admissionRequireSend(t, head)
			admissionQuiesced(t, c)
		})
	}
}

func TestAdmissionPingDropsBehindPendingHead(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"monitor", "reply"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			c, peer := admissionConn(t)
			admissionFill(t, c)
			headCtx, signal, resumeHead := admissionPaused(t, c.ctx)
			head := admissionDeferred(c, headCtx, 2)
			admissionAwait(t, signal.Done(), "ping head suspension")
			admissionDrain(t, c, peer, 1, true)
			admissionControl(t, c, 1)
			if mode == "monitor" {
				if done := c.sendPingWithReply(); done != nil {
					t.Error("monitor ping bypassed the pending FIFO")
				}
				c.pendingMu.RLock()
				pending := len(c.pending)
				c.pendingMu.RUnlock()
				if pending != 0 {
					t.Errorf("dropped ping retained %d reply registrations", pending)
				}
			} else {
				c.handlePing(BuildPingRequest(UID{First: 91}))
			}
			resumeHead()
			admissionDrain(t, c, peer, 2, true)
			admissionRequireSend(t, head)
			admissionQuiesced(t, c)
		})
	}
}

func TestAdmissionColdPermitReleasedBeforeFlush(t *testing.T) {
	t.Parallel()
	c, peer := admissionConn(t)
	var order sendqueue.Queue
	first, _ := order.Reserve()
	second, _ := order.Reserve()
	WithSendOrder(&order)(c)
	order.Seal()
	ctx, signal := admissionSignal(t, c.ctx)
	head := make(chan admissionResult, 1)
	go func() {
		ok, err := c.SendFrameContextOrdered(ctx, UID{First: 2}, admissionBody(2), first)
		head <- admissionResult{ok, err}
	}()
	admissionAwait(t, signal.Done(), "first sender's flush wait")
	select {
	case <-second.Ready():
	default:
		t.Error("cold permit remained held after enqueue")
	}
	tailCtx, tailSignal := admissionSignal(t, c.ctx)
	tail := make(chan admissionResult, 1)
	go func() {
		ok, err := c.SendFrameDeferredContextOrdered(tailCtx, UID{First: 3}, admissionBody(3), second)
		tail <- admissionResult{ok, err}
	}()
	admissionAwait(t, tailSignal.Done(), "second sender's queue wait")
	request := admissionDrain(t, c, peer, 2, false) // withhold the first flush acknowledgment
	admissionRequireSend(t, tail)
	select {
	case result := <-head:
		t.Fatalf("first sender completed before its flush acknowledgment: %+v", result)
	default:
	}
	admissionDrain(t, c, peer, 3, true)
	admissionFinish(c, request)
	admissionRequireSend(t, head)
	if order.Pending() {
		t.Error("cold peer permits did not drain")
	}
	admissionQuiesced(t, c)
}
