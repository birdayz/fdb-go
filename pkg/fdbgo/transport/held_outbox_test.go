package transport

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/internal/sendqueue"
)

func heldOutboxConn(t *testing.T, order *sendqueue.Queue) (*Conn, net.Conn) {
	t.Helper()
	client, peer := tcpPair(t)
	c := directConn(t, client)
	c.writeCh = make(chan writeReq, 1)
	WithSendOrder(order)(c)
	return c, peer
}

func startHeldWriter(t *testing.T, c *Conn, controls bool) {
	t.Helper()
	c.wbuf = bufio.NewWriterSize(stampedWriter{c}, 64<<10)
	c.loopWG.Add(1)
	go c.writeLoop()
	if controls {
		c.loopWG.Add(1)
		go c.coldControlLoop()
	}
	t.Cleanup(func() { _ = c.Close() })
}

func heldWire(t *testing.T, peer net.Conn, want uint64) {
	t.Helper()
	token, body := readFrame(t, peer)
	if token.First != want || string(body) != string(admissionBody(want)) {
		t.Errorf("outbox peer decoded %d/%q, want %d/%q", token.First, body, want, admissionBody(want))
	}
}

type fullAdmissionHandoffFixture struct {
	conn      *Conn
	peer      net.Conn
	queue     chan writeReq
	mutex     *sync.RWMutex
	fifo      *sendqueue.Queue
	order     *sendqueue.Queue
	admission peerAdmission
	follower  *sendqueue.Permit
	cohort    *sendqueue.Permit
}

func newFullAdmissionHandoffFixture(t *testing.T, route string) *fullAdmissionHandoffFixture {
	t.Helper()
	f := &fullAdmissionHandoffFixture{}
	if route == "ordinary" {
		f.conn, f.peer = admissionConn(t)
		f.queue, f.mutex, f.fifo = f.conn.writeCh, &f.conn.writeAdmission, &f.conn.sendWaiters
		admissionFill(t, f.conn)
		return f
	}
	f.order = &sendqueue.Queue{}
	first, _ := f.order.Reserve()
	t.Cleanup(first.Release)
	if route == "prefix" {
		f.admission.permit, _ = f.order.Reserve()
		f.follower, _ = f.order.Reserve()
	} else {
		f.cohort, f.admission.held = first, true
	}
	f.conn, f.peer = heldOutboxConn(t, f.order)
	t.Cleanup(f.releasePermits)
	f.order.Seal()
	if route == "prefix" {
		f.queue, f.mutex, f.fifo = f.conn.prefixCh, &f.conn.writeAdmission, f.order
		if ok, err := f.conn.SendFrameDeferredContextOrdered(context.Background(), UID{First: 1}, admissionBody(1), first); !ok || err != nil {
			t.Fatal(ok, err)
		}
	} else {
		f.queue, f.mutex, f.fifo = f.conn.heldCh, &f.conn.heldAdmission, &f.conn.heldWaiters
		if ok, err := f.conn.SendFrameDeferredContext(context.Background(), UID{First: 1}, admissionBody(1)); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	return f
}

func (f *fullAdmissionHandoffFixture) releaseHeldCohort() {
	if f.cohort != nil {
		f.cohort.Release()
		f.conn.ClearSendOrder(f.order)
	}
}

func (f *fullAdmissionHandoffFixture) releasePermits() {
	f.admission.permit.Release()
	f.follower.Release()
	f.releaseHeldCohort()
	if f.order != nil && !f.order.Pending() {
		f.conn.ClearSendOrder(f.order)
	}
}

// Exclusive: each rejected write returns its completion to the shared pool,
// whose reference census must finish before another test can reacquire it.
func TestFullAdmissionHandoffReleasesReader(t *testing.T) {
	for _, route := range []string{"prefix", "held", "ordinary"} {
		for _, action := range []string{"fifo", "cancel", "close", "panic"} {
			t.Run(route+"/"+action, func(t *testing.T) {
				f := newFullAdmissionHandoffFixture(t, route)
				c := f.conn
				ctx, cancel := context.WithCancel(context.Background())
				resumeCh := make(chan struct{})
				resume := sync.OnceFunc(func() { close(resumeCh) })
				completion := newWriteCompletion()
				type observation struct {
					unlocked bool
					queued   int64
					held     int64
					buffered int
					pending  bool
					refs     int32
				}
				observed := make(chan observation, 1)
				ctx, signal := WithSuspendHandoff(ctx, resumeCh, func() {
					unlocked := f.mutex.TryLock()
					if unlocked {
						f.mutex.Unlock()
					}
					observed <- observation{unlocked, c.queued.Load(), c.heldCount.Load(), len(f.queue), f.fifo.Pending(), completion.refs.Load()}
					if action == "panic" && unlocked {
						panic(replyPanicMarker)
					}
				})
				defer signal.Fire()
				type outcome struct {
					admissionResult
					panicValue any
				}
				headDone := make(chan outcome, 1)
				go func() {
					var result outcome
					defer func() {
						result.panicValue = recover()
						f.admission.permit.Release()
						headDone <- result
					}()
					result.enqueued, result.err = c.writeAdmittedContext(ctx, UID{First: 2}, admissionBody(2), f.admission, completion)
				}()
				var head outcome
				var headJoined, tailJoined bool
				var tail <-chan admissionResult
				waitHead := func() outcome {
					if !headJoined {
						head, headJoined = <-headDone, true
					}
					return head
				}
				defer func() {
					cancel()
					resume()
					c.cancel()
					waitHead()
					if tail != nil && !tailJoined {
						<-tail
					}
					f.releasePermits()
					c.finishOwnedWrites(nil)
				}()
				probe := admissionAwait(t, observed, "full-queue handoff observer")
				if !probe.unlocked {
					// This is the red proof, not a timeout: cancel and release the
					// mutant's handoff, then join it before failing the lock assertion.
					cancel()
					resume()
					waitHead()
					t.Fatal("full-queue NotifySuspend retained its admission reader lock")
				}
				wantHeld := int64(0)
				if route == "held" {
					wantHeld = 2
				}
				if probe.queued != 2 || probe.held != wantHeld || probe.buffered != 1 || !probe.pending || probe.refs != 2 {
					t.Fatalf("handoff lost its unaccepted FIFO owner: %+v, want queued=2 held=%d buffered=1 pending=true refs=2", probe, wantHeld)
				}
				switch action {
				case "fifo":
					tailCtx, tailSignal := WithSuspendSignal(ctx)
					defer tailSignal.Fire()
					tailDone := make(chan admissionResult, 1)
					tail = tailDone
					go func() {
						ok, err := c.SendFrameDeferredContextOrdered(tailCtx, UID{First: 3}, admissionBody(3), f.follower)
						tailDone <- admissionResult{ok, err}
					}()
					admissionAwait(t, tailSignal.Done(), "follower reserved behind the paused handoff")
					select {
					case result := <-tail:
						tailJoined = true
						t.Fatalf("follower escaped the paused FIFO head: %+v", result)
					default:
					}
					wantQueued := int64(3)
					if route == "prefix" {
						wantQueued = 2 // the follower still waits for its cold permit
						select {
						case <-f.follower.Ready():
							t.Fatal("prefix handoff released its permit before acceptance")
						default:
						}
					}
					if len(f.queue) != 1 || c.queued.Load() != wantQueued || !f.fifo.Pending() {
						t.Fatal("follower changed the full channel or lost the paused head's credit")
					}
					f.releaseHeldCohort()
					admissionDrain(t, c, f.peer, 1, true)
					resume()
					admissionDrain(t, c, f.peer, 2, true)
					admissionDrain(t, c, f.peer, 3, true)
					tailResult := admissionAwait(t, tail, "FIFO follower completing")
					tailJoined = true
					if !tailResult.enqueued || tailResult.err != nil {
						t.Fatalf("resumed FIFO follower = %+v", tailResult)
					}
					result := waitHead()
					if !result.enqueued || result.err != nil || result.panicValue != nil {
						t.Fatalf("resumed FIFO head = %+v", result)
					}
				case "cancel":
					// Make the channel send-ready while the head still owns its
					// handoff; cancellation must be rechecked before admission.
					f.releaseHeldCohort()
					admissionDrain(t, c, f.peer, 1, true)
					cancel()
					resume()
					result := waitHead()
					if result.enqueued || !errors.Is(result.err, context.Canceled) || result.panicValue != nil {
						t.Fatalf("cancelled handoff = %+v, want unaccepted/context.Canceled", result)
					}
				case "close":
					// There is no writer in this fixture: run its real finalizer
					// while the handoff remains paused, freeing the full slot.
					c.failConnection(errConnClosed)
					c.finishOwnedWrites(nil)
					cancel() // an observed closed connection wins over caller cancellation
					resume()
					result := waitHead()
					if result.enqueued || !errors.Is(result.err, ErrConnClosed) || result.panicValue != nil {
						t.Fatalf("closed handoff = %+v, want unaccepted/connection closed", result)
					}
				case "panic":
					result := waitHead()
					if result.enqueued || result.panicValue != replyPanicMarker {
						t.Fatalf("notification panic = %+v", result)
					}
					f.releasePermits()
					admissionDrain(t, c, f.peer, 1, true)
				}
				if refs := completion.refs.Load(); refs != 0 {
					t.Errorf("completed handoff retained %d completion owners", refs)
				}
				f.releasePermits()
				admissionQuiesced(t, c)
				if f.order != nil && f.order.Pending() {
					t.Error("completed handoff retained a cold-prefix owner")
				}
			})
		}
	}
}

func TestHeldOutboxFlushesAvailablePrefixBeforeMissingMember(t *testing.T) {
	t.Parallel()
	var order sendqueue.Queue
	first, _ := order.Reserve()
	second, _ := order.Reserve()
	defer first.Release()
	defer second.Release()
	c, peer := heldOutboxConn(t, &order)
	order.Seal()
	if ok, err := c.SendFrameDeferredContextOrdered(c.ctx, UID{First: 1}, admissionBody(1), first); !ok || err != nil {
		t.Fatalf("first prefix enqueue = %t, %v", ok, err)
	}
	if ok, err := c.SendFrameEagerContext(c.ctx, UID{First: 3}, admissionBody(3)); !ok || err != nil {
		t.Fatalf("late eager enqueue = %t, %v", ok, err)
	}
	if len(c.prefixCh) != 1 || len(c.heldCh) != 1 || c.heldCount.Load() != 1 || c.queued.Load() != 2 {
		t.Fatalf("cold frames escaped their bounded channels: prefix=%d held=%d heldCount=%d queued=%d", len(c.prefixCh), len(c.heldCh), c.heldCount.Load(), c.queued.Load())
	}
	startHeldWriter(t, c, true)
	heldWire(t, peer, 1) // must flush while second still owns the cohort
	select {
	case <-order.Drained():
		t.Fatal("prefix control was vacuous: the missing second member had drained")
	default:
	}
	if ok, err := c.SendFrameDeferredContextOrdered(c.ctx, UID{First: 2}, admissionBody(2), second); !ok || err != nil {
		t.Fatalf("second prefix enqueue = %t, %v", ok, err)
	}
	heldWire(t, peer, 2)
	heldWire(t, peer, 3)
	if err := c.FlushContext(c.ctx); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	admissionQuiesced(t, c)
}

func TestHeldOutboxWarmTrafficCannotExtendFiniteWindow(t *testing.T) {
	t.Parallel()
	var order sendqueue.Queue
	prefix, _ := order.Reserve()
	defer prefix.Release()
	c, peer := heldOutboxConn(t, &order)
	order.Seal()
	if ok, err := c.SendFrameDeferredContext(c.ctx, UID{First: 2}, admissionBody(2)); !ok || err != nil {
		t.Fatalf("held enqueue = %t, %v", ok, err)
	}
	prefix.Release()
	c.ClearSendOrder(&order)
	if ok, err := c.SendFrameDeferredContext(c.ctx, UID{First: 3}, admissionBody(3)); !ok || err != nil {
		t.Fatalf("warm enqueue = %t, %v", ok, err)
	}
	if len(c.heldCh) != 1 || len(c.writeCh) != 1 || c.heldCount.Load() != 1 {
		t.Fatalf("warm traffic extended the held cohort: held=%d ordinary=%d heldCount=%d", len(c.heldCh), len(c.writeCh), c.heldCount.Load())
	}
	startHeldWriter(t, c, true)
	heldWire(t, peer, 2)
	heldWire(t, peer, 3)
	if err := c.FlushContext(c.ctx); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	admissionQuiesced(t, c)
}

func TestHeldOutboxReservedHeadPrecedesWarmWireFrame(t *testing.T) {
	t.Parallel()
	var order sendqueue.Queue
	prefix, _ := order.Reserve()
	defer prefix.Release()
	c, peer := heldOutboxConn(t, &order)
	order.Seal()
	if ok, err := c.SendFrameDeferredContext(c.ctx, UID{First: 1}, admissionBody(1)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	headCtx, signal, resume := admissionPaused(t, c.ctx)
	head := admissionDeferred(c, headCtx, 2)
	admissionAwait(t, signal.Done(), "held head's full-channel suspension")
	prefix.Release()
	c.ClearSendOrder(&order)
	if ok, err := c.SendFrameDeferredContext(c.ctx, UID{First: 3}, admissionBody(3)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	admissionDrain(t, c, peer, 1, true)
	if c.heldCount.Load() != 1 || len(c.heldCh) != 0 || len(c.writeCh) != 1 || !c.heldWaiters.Pending() {
		t.Fatalf("missing held-head control: held=%d buffered=%d ordinary=%d pending=%t", c.heldCount.Load(), len(c.heldCh), len(c.writeCh), c.heldWaiters.Pending())
	}
	request, _, available := c.nextWrite(false)
	if available {
		// Continue past the guard to decode the actual overtaking wire frame.
		t.Error("writer accepted an ordinary frame ahead of a reserved held head")
		if err := WriteFrame(c.conn, request.token, request.body, false); err != nil {
			t.Fatal(err)
		}
		heldWire(t, peer, 2)
		admissionFinish(c, request)
		resume()
		admissionRequireSend(t, head)
		admissionDrain(t, c, peer, 3, true)
	} else {
		resume()
		admissionRequireSend(t, head)
		admissionDrain(t, c, peer, 2, true)
		admissionDrain(t, c, peer, 3, true)
	}
	admissionQuiesced(t, c)
}

func TestHeldOutboxFullAdmissionDoesNotBlockPrefix(t *testing.T) {
	t.Parallel()
	for _, cancelAt := range []int{0, 1} {
		t.Run(fmt.Sprintf("cancel=%d", cancelAt), func(t *testing.T) {
			t.Parallel()
			var order sendqueue.Queue
			first, _ := order.Reserve()
			last, _ := order.Reserve()
			defer first.Release()
			defer last.Release()
			c, peer := heldOutboxConn(t, &order)
			order.Seal()
			if ok, err := c.SendFrameDeferredContext(c.ctx, UID{First: 11}, admissionBody(11)); !ok || err != nil {
				t.Fatal(ok, err)
			}
			headCtx, signal, resume := admissionPaused(t, c.ctx)
			head := admissionDeferred(c, headCtx, 12)
			admissionAwait(t, signal.Done(), "full held head")
			var cancels []context.CancelFunc
			var followers []<-chan admissionResult
			for _, token := range []uint64{13, 14} {
				ctx, cancel := context.WithCancel(c.ctx)
				defer cancel()
				ctx, parked := admissionSignal(t, ctx)
				followers = append(followers, admissionDeferred(c, ctx, token))
				cancels = append(cancels, cancel)
				admissionAwait(t, parked.Done(), "held follower")
			}
			if c.heldCount.Load() != 4 || !c.heldWaiters.Pending() {
				t.Fatal("full held head and both followers were not reserved")
			}
			prefix := make(chan admissionResult, 1)
			go func() {
				ok, err := c.SendFrameDeferredContextOrdered(c.ctx, UID{First: 1}, admissionBody(1), first)
				prefix <- admissionResult{ok, err}
			}()
			admissionRequireSend(t, prefix) // heldAdmission is still locked by head
			cancels[cancelAt]()
			if result := admissionAwait(t, followers[cancelAt], "cancelled held follower"); result.enqueued || !errors.Is(result.err, context.Canceled) {
				t.Fatalf("cancelled held follower = %+v", result)
			}
			admissionDrain(t, c, peer, 1, true)
			if ok, err := c.SendFrameDeferredContextOrdered(c.ctx, UID{First: 2}, admissionBody(2), last); !ok || err != nil {
				t.Fatal(ok, err)
			}
			admissionDrain(t, c, peer, 2, true)
			admissionDrain(t, c, peer, 11, true)
			resume()
			admissionRequireSend(t, head)
			admissionDrain(t, c, peer, 12, true)
			survivor := 1 - cancelAt
			admissionRequireSend(t, followers[survivor])
			admissionDrain(t, c, peer, uint64(13+survivor), true)
			admissionQuiesced(t, c)
		})
	}
}

func TestHeldOutboxRechecksParkedOrdinaryRequest(t *testing.T) {
	t.Parallel()
	var order sendqueue.Queue
	prefix, _ := order.Reserve()
	defer prefix.Release()
	c, peer := heldOutboxConn(t, &order)
	order.Seal()
	if ok, err := c.SendFrameDeferredContext(c.ctx, UID{First: 2}, admissionBody(2)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	prefix.Release()
	c.ClearSendOrder(&order)
	if ok, err := c.SendFrameDeferredContext(c.ctx, UID{First: 3}, admissionBody(3)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	// Model an ordinary receive racing held publication. The writer must
	// recheck priority after receive, not treat this saved frame as selected.
	c.pendingWrite, c.hasPendingWrite = <-c.writeCh, true
	if c.heldCount.Load() != 1 || c.queued.Load() != 2 {
		t.Fatal("parked ordinary request has no older held-frame control")
	}
	admissionDrain(t, c, peer, 2, true)
	admissionDrain(t, c, peer, 3, true)
	admissionQuiesced(t, c)
}

func TestHeldOutboxRechecksHeldAfterOrdinaryReceive(t *testing.T) {
	t.Parallel()
	var order sendqueue.Queue
	prefix, _ := order.Reserve()
	defer prefix.Release()
	c, peer := heldOutboxConn(t, &order)
	order.Seal()
	admission, err := c.beginSend(context.Background(), nil)
	if err != nil || !admission.held || admission.permit != nil || !order.Pending() {
		t.Fatalf("pending-prefix admission = %+v, %v", admission, err)
	}
	prefix.Release()
	c.ClearSendOrder(&order)
	entered, release := make(chan struct{}), make(chan struct{})
	resume := sync.OnceFunc(func() { close(release) })
	defer resume()
	c.ctx = &outboxProbeContext{Context: c.ctx, onDone: sync.OnceFunc(func() {
		close(entered)
		<-release // the writer already observed heldCount == 0 before its select
	})}
	t.Cleanup(func() { _ = c.Close() })
	type selection struct {
		request      writeReq
		frame, ready bool
	}
	selected := make(chan selection, 1)
	go func() {
		request, frame, ready := c.nextWrite(true)
		selected <- selection{request, frame, ready}
	}()
	admissionAwait(t, entered, "ordinary writer's pre-select boundary")
	if c.prefixOpen || c.heldCount.Load() != 0 || c.queued.Load() != 0 {
		t.Fatal("writer did not reach an empty ordinary select after the prefix drained")
	}
	request := writeReq{token: UID{First: 2}, body: admissionBody(2)}
	if ok, err := c.enqueueAdmitted(context.Background(), request, nil, admission); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, err := c.SendFrameDeferredContext(context.Background(), UID{First: 3}, admissionBody(3)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if len(c.heldCh) != 1 || len(c.writeCh) != 1 || c.heldCount.Load() != 1 || c.queued.Load() != 2 {
		t.Fatal("raced receive control did not retain both held and ordinary frames")
	}
	select {
	case <-c.tailKick: // leave only the ordinary receive ready in the paused select
	default:
		t.Fatal("held publication did not supply its buffered wake")
	}
	resume()
	got := admissionAwait(t, selected, "ordinary receive's held-priority recheck")
	if !got.frame || !got.ready {
		t.Fatalf("raced ordinary selection frame=%t ready=%t", got.frame, got.ready)
	}
	if err := WriteFrame(c.conn, got.request.token, got.request.body, false); err != nil {
		t.Fatal(err)
	}
	heldWire(t, peer, 2) // removing the post-receive check actually emits 3, then 2
	admissionFinish(c, got.request)
	admissionDrain(t, c, peer, 3, true)
	admissionQuiesced(t, c)
}

// The writer is already inside the held-gap select once Done is evaluated.
// Closing entered is a positive event; an abort kick sent before the actual
// select still remains buffered, so no scheduling delay can hide a lost wake.
type heldGapContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *heldGapContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestHeldOutboxLastAbandonedHeadWakesOrdinaryWriter(t *testing.T) {
	t.Parallel()
	var order sendqueue.Queue
	prefix, _ := order.Reserve()
	defer prefix.Release()
	c, peer := heldOutboxConn(t, &order)
	order.Seal()
	if ok, err := c.SendFrameDeferredContext(c.ctx, UID{First: 1}, admissionBody(1)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	cancelCtx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	headCtx, signal, resume := admissionPaused(t, cancelCtx)
	head := admissionDeferred(c, headCtx, 2)
	admissionAwait(t, signal.Done(), "held cancellation head suspension")
	prefix.Release()
	c.ClearSendOrder(&order)
	admissionDrain(t, c, peer, 1, true)
	if ok, err := c.SendFrameDeferredContext(c.ctx, UID{First: 3}, admissionBody(3)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if c.heldCount.Load() != 1 || len(c.heldCh) != 0 || len(c.writeCh) != 1 {
		t.Fatal("held-head gap was not populated before starting the writer")
	}
	entered := make(chan struct{})
	c.ctx = &heldGapContext{Context: c.ctx, entered: entered}
	startHeldWriter(t, c, false)
	admissionAwait(t, entered, "writer entering its held-gap wait")
	cancel()
	resume()
	if result := admissionAwait(t, head, "abandoned held head"); result.enqueued || !errors.Is(result.err, context.Canceled) {
		t.Fatalf("cancelled held head = %+v", result)
	}
	heldWire(t, peer, 3)
	if err := c.FlushContext(c.ctx); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	admissionQuiesced(t, c)
}

func TestHeldOutboxLateClassifiedSenderWakesWarmWriter(t *testing.T) {
	t.Parallel()
	var order sendqueue.Queue
	prefix, _ := order.Reserve()
	defer prefix.Release()
	c, peer := heldOutboxConn(t, &order)
	order.Seal()
	entered, release := make(chan struct{}), make(chan struct{})
	resume := sync.OnceFunc(func() { close(release) })
	defer resume()
	ctx := &outboxProbeContext{Context: context.Background(), onDone: sync.OnceFunc(func() {
		close(entered)
		<-release // beginSend classified held; enqueueHeld has not counted it yet
	})}
	head := admissionDeferred(c, ctx, 2)
	admissionAwait(t, entered, "held classification before its enqueue")
	if !order.Pending() || c.heldCount.Load() != 0 || c.queued.Load() != 0 {
		t.Fatal("late held classification did not precede its reservation")
	}
	prefix.Release()
	c.ClearSendOrder(&order)
	waiting := make(chan struct{})
	c.ctx = &heldGapContext{Context: c.ctx, entered: waiting}
	startHeldWriter(t, c, false)
	admissionAwait(t, waiting, "writer entering the ordinary three-case wait")
	resume()
	admissionRequireSend(t, head)
	if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	heldWire(t, peer, 2) // no ordinary send or flush marker may supply the wake
	if err := c.FlushContext(c.ctx); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	admissionQuiesced(t, c)
}

func TestHeldOutboxSharedWakeFlushesTailBeforeMissingHead(t *testing.T) {
	t.Parallel()
	var order sendqueue.Queue
	prefix, _ := order.Reserve()
	defer prefix.Release()
	c, peer := heldOutboxConn(t, &order)
	order.Seal()
	if ok, err := c.SendFrameDeferredContext(c.ctx, UID{First: 1}, admissionBody(1)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	headCtx, signal, resume := admissionPaused(t, context.Background())
	defer resume()
	head := admissionDeferred(c, headCtx, 2)
	admissionAwait(t, signal.Done(), "full held head before tail publication")
	prefix.Release()
	c.ClearSendOrder(&order)
	admissionDrain(t, c, peer, 1, true)
	if c.heldCount.Load() != 1 || len(c.heldCh) != 0 {
		t.Fatal("tail control has no missing held head")
	}
	// The buffered held wake and the tail's wake deliberately coalesce. The
	// writer must still enter the socket-writing path, not just recheck a gap.
	c.writeMu.Lock()
	bufp, frame := encodeFrame(UID{First: 9}, admissionBody(9), false)
	n, err := c.conn.Write(frame[:len(frame)/2])
	if err != nil {
		putFrame(bufp, frame)
		c.writeMu.Unlock()
		t.Fatal(err)
	}
	c.leaveTail(bufp, frame, n)
	c.writeMu.Unlock()
	if len(c.tailKick) != 1 {
		t.Fatal("held/tail wake was not buffered before starting the writer")
	}
	startHeldWriter(t, c, false)
	if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	heldWire(t, peer, 9) // the head stays paused until the actual tail is decoded
	resume()
	admissionRequireSend(t, head)
	heldWire(t, peer, 2)
	if err := c.FlushContext(c.ctx); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	admissionQuiesced(t, c)
}

type heldFailConn struct {
	net.Conn
	panics bool
}

func (c *heldFailConn) Write([]byte) (int, error) {
	if c.panics {
		panic("held outbox writer panic")
	}
	return 0, errors.New("held outbox writer failure")
}

// Keep this pool-reference census exclusive: after Close no other benchmark or
// parallel test may reacquire one of the three completions before inspection.
func TestHeldOutboxTerminalWriterReleasesEveryOwner(t *testing.T) {
	for _, tc := range []struct {
		panics, parked bool
	}{{false, false}, {true, false}, {true, true}} {
		t.Run(fmt.Sprintf("panic=%t/parked=%t", tc.panics, tc.parked), func(t *testing.T) {
			var order sendqueue.Queue
			first, _ := order.Reserve()
			last, _ := order.Reserve()
			defer first.Release()
			defer last.Release()
			c, _ := heldOutboxConn(t, &order)
			c.raw = nil
			order.Seal()
			results := make([]<-chan admissionResult, 0, 3)
			start := func(token uint64, permit *sendqueue.Permit) {
				// Keep caller cancellation independent of the connection teardown.
				ctx, signal := admissionSignal(t, context.Background())
				done := make(chan admissionResult, 1)
				go func() {
					ok, err := c.SendFrameContextOrdered(ctx, UID{First: token}, make([]byte, 4096), permit)
					done <- admissionResult{ok, err}
				}()
				admissionAwait(t, signal.Done(), "owned outbox frame's flush wait")
				results = append(results, done)
			}
			start(1, first)
			start(2, nil)
			last.Release()
			c.ClearSendOrder(&order)
			start(3, nil)
			if len(c.prefixCh) != 1 || len(c.heldCh) != 1 || len(c.writeCh) != 1 || c.queued.Load() != 3 {
				t.Fatal("terminal writer control did not populate all three ownership channels")
			}
			var owners []*writeCompletion
			for _, queue := range []chan writeReq{c.prefixCh, c.heldCh, c.writeCh} {
				request := <-queue
				if request.owned == nil || request.owned.refs.Load() != 2 {
					t.Fatal("accepted frame did not retain both completion owners")
				}
				owners = append(owners, request.owned)
				queue <- request
			}
			if tc.parked {
				c.pendingWrite, c.hasPendingWrite = <-c.writeCh, true
			}
			c.conn = &heldFailConn{Conn: c.conn, panics: tc.panics}
			c.wbuf = bufio.NewWriterSize(stampedWriter{c}, 16) // first frame spills before batch collection
			c.loopWG.Add(1)
			go c.writeLoop()
			t.Cleanup(func() { _ = c.Close() })
			for _, done := range results {
				if result := admissionAwait(t, done, "terminal owned write"); !result.enqueued || !errors.Is(result.err, ErrConnClosed) {
					t.Errorf("terminal write accepted=%t err=%T: %v, want accepted and closed", result.enqueued, result.err, result.err)
				}
			}
			_ = c.Close()
			admissionQuiesced(t, c)
			for i, owner := range owners {
				if n := owner.refs.Load(); n != 0 {
					t.Errorf("terminal completion %d retained %d owners", i, n)
				}
			}
		})
	}
}

func TestHeldOutboxControlPrefixAndPostSealReply(t *testing.T) {
	t.Parallel()
	var order sendqueue.Queue
	first, _ := order.Reserve()
	defer first.Release()
	c, peer := heldOutboxConn(t, &order)
	type pingSent struct {
		reply  <-chan Response
		handle *ReplyHandle
	}
	pingResult := make(chan pingSent, 1)
	go func() {
		reply, handle := c.sendPingWithReply()
		pingResult <- pingSent{reply, handle}
	}()
	ping := admissionAwait(t, pingResult, "pre-seal monitor's immediate future")
	if ping.handle == nil || len(c.coldControls) != 1 || c.queued.Load() != 1 {
		t.Fatal("monitor did not reserve its bounded pre-seal control position")
	}
	// A full bounded control inbox drops rather than blocking readLoop.
	c.handlePing(BuildPingRequest(UID{First: 90}))
	if len(c.coldControls) != 1 || c.queued.Load() != 1 {
		t.Fatal("full pre-seal control inbox did not drop without retaining a credit")
	}
	order.Seal()
	c.handlePing(BuildPingRequest(UID{First: 91}))
	c.handlePing(BuildPingRequest(UID{First: 92})) // held capacity is one: drop
	if len(c.heldCh) != 1 || c.heldCount.Load() != 1 || len(c.coldControls) != 1 {
		t.Fatal("post-seal controls spawned work instead of using bounded held admission")
	}
	startHeldWriter(t, c, true)
	c.loopWG.Add(1)
	go c.readLoop()
	if ok, err := c.SendFrameDeferredContextOrdered(c.ctx, UID{First: 1}, admissionBody(1), first); !ok || err != nil {
		t.Fatal(ok, err)
	}
	heldWire(t, peer, 1)
	token, body := readFrame(t, peer)
	if token != WellKnownToken(WLTokenPingPacket) {
		t.Fatalf("prefix monitor wire token = %+v", token)
	}
	replyToken, ok := extractPingReplyToken(body)
	if !ok || !c.HasPendingReply(replyToken) {
		t.Fatal("prefix monitor lost its actual pending reply")
	}
	if err := WriteFrame(peer, replyToken, buildVoidReply(), false); err != nil {
		t.Fatal(err)
	}
	admissionAwait(t, ping.reply, "actual monitor reply completion")
	ping.handle.Release() // received: the monitor's release
	token, body = readFrame(t, peer)
	if token.First != 91 || len(body) == 0 {
		t.Fatalf("held ping response wire token/body = %+v/%x", token, body)
	}
	if err := c.FlushContext(c.ctx); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	admissionQuiesced(t, c)
}

func TestHeldOutboxMonitorTimeoutIsNotPostponedByPrefix(t *testing.T) {
	t.Parallel()
	var order sendqueue.Queue
	first, _ := order.Reserve()
	defer first.Release()
	c, _ := heldOutboxConn(t, &order)
	order.Seal()
	_, _, owner := c.PrepareReply()
	defer func() { owner.Cancel(); owner.Release() }()
	c.monitorLoopInterval, c.monitorTimeout = 10*time.Millisecond, 20*time.Millisecond
	startHeldWriter(t, c, true)
	c.loopWG.Add(1)
	go c.connectionMonitor()
	admissionAwait(t, c.Done(), "monitor timeout while cold prefix remains unbuilt")
	if !order.Pending() || c.monitorCycles.Load() == 0 {
		t.Fatal("monitor timeout control did not retain a pending prefix and execute a cycle")
	}
	_ = c.Close()
	first.Release()
	admissionQuiesced(t, c)
}

// These are existing production context-call boundaries, not send-path hooks.
// Callers choose a Background-based operation context when only Conn's own
// Done/Err calls should be observed.
type outboxProbeContext struct {
	context.Context
	onErr  func()
	onDone func()
}

func (c *outboxProbeContext) Err() error {
	if c.onErr != nil {
		c.onErr()
	}
	return c.Context.Err()
}

func (c *outboxProbeContext) Done() <-chan struct{} {
	if c.onDone != nil {
		c.onDone()
	}
	return c.Context.Done()
}

func TestAdmissionActivationBarrierDrainsEarlierReader(t *testing.T) {
	t.Parallel()
	c, peer := admissionConn(t)
	admissionFill(t, c)
	// There is no callback between the locked Pending check and channel send.
	// Retain that real admission lock and decision, then perform the exact
	// production enqueue primitive after the head publishes its reservation.
	c.queued.Add(1)
	warmAccepted := false
	defer func() {
		if !warmAccepted {
			c.queued.Add(-1)
		}
	}()
	c.writeAdmission.RLock()
	unlockWarm := sync.OnceFunc(c.writeAdmission.RUnlock)
	defer unlockWarm()
	if c.sendWaiters.Pending() {
		t.Fatal("earlier warm reader did not observe an empty FIFO")
	}
	pastBarrier := make(chan struct{})
	c.ctx = &outboxProbeContext{Context: c.ctx, onDone: sync.OnceFunc(func() { close(pastBarrier) })}
	headCtx, headSignal, resumeHead := admissionPaused(t, context.Background())
	head := admissionDeferred(c, headCtx, 3)
	admissionAwait(t, headSignal.Done(), "activating head's first wait")
	// A correct first wait is inside the barrier, before Conn.Done is used
	// by the Ready select. Without the barrier that select necessarily ran
	// before the head's full-channel notification: the signal establishes HB.
	escaped := false
	select {
	case <-pastBarrier:
		escaped = true
	default:
	}
	admissionDrain(t, c, peer, 1, true)
	if escaped {
		resumeHead()
		admissionRequireSend(t, head) // mutant accepts while the older reader remains admitted
	} else {
		warmAccepted = c.tryEnqueueCounted(writeReq{token: UID{First: 2}, body: admissionBody(2)})
		if !warmAccepted {
			t.Fatal("earlier admitted reader could not use the freed slot")
		}
		unlockWarm()
		resumeHead()
	}
	admissionDrain(t, c, peer, 2, true) // mutant actually emits head 3 first
	if escaped {
		warmAccepted = c.tryEnqueueCounted(writeReq{token: UID{First: 2}, body: admissionBody(2)})
		if !warmAccepted {
			t.Fatal("earlier admitted reader could not finish after the overtaking head")
		}
		unlockWarm()
	}
	admissionDrain(t, c, peer, 3, true)
	if !escaped {
		admissionRequireSend(t, head)
	}
	admissionQuiesced(t, c)
}

func TestAdmissionLockedPendingRecheckPreservesWireOrder(t *testing.T) {
	t.Parallel()
	c, peer := admissionConn(t)
	admissionFill(t, c)
	entered, release := make(chan struct{}), make(chan struct{})
	resumeWarm := sync.OnceFunc(func() { close(release) })
	defer resumeWarm()
	var gated atomic.Bool
	c.ctx = &outboxProbeContext{Context: c.ctx, onErr: func() {
		if gated.CompareAndSwap(false, true) {
			close(entered)
			<-release // the warm sender holds RLock, after outer Pending=false
		}
	}}
	warmCtx, warmSignal := admissionSignal(t, context.Background())
	warm := admissionDeferred(c, warmCtx, 3)
	admissionAwait(t, entered, "warm reader admitted before activation")
	headCtx, headSignal, resumeHead := admissionPaused(t, context.Background())
	head := admissionDeferred(c, headCtx, 2)
	admissionAwait(t, headSignal.Done(), "head publishing while the warm reader is admitted")
	admissionControl(t, c, 3)
	admissionDrain(t, c, peer, 1, true)
	resumeWarm()
	warmFinished := false
	select {
	case <-warmSignal.Done(): // recheck joined the FIFO behind the head
	case result := <-warm:
		warmFinished = true // mutant consumed the free channel slot instead
		if !result.enqueued || result.err != nil {
			t.Fatalf("warm sender failed before establishing order: %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("warm reader neither joined the FIFO nor enqueued")
	}
	resumeHead()
	admissionDrain(t, c, peer, 2, true) // mutant emits 3, then 2
	admissionDrain(t, c, peer, 3, true)
	admissionRequireSend(t, head)
	if !warmFinished {
		admissionRequireSend(t, warm)
	}
	admissionQuiesced(t, c)
}

func TestHeldOutboxFastFrameAppendsBeforeFirstWait(t *testing.T) {
	t.Parallel()
	var order sendqueue.Queue
	prefix, _ := order.Reserve()
	defer prefix.Release()
	c, peer := heldOutboxConn(t, &order)
	order.Seal()
	ctx, signal, resume := admissionPaused(t, context.Background())
	head := make(chan admissionResult, 1)
	go func() {
		ok, err := c.SendFrameContextOrdered(ctx, UID{First: 2}, admissionBody(2), nil)
		head <- admissionResult{ok, err}
	}()
	admissionAwait(t, signal.Done(), "held frame's first flush wait")
	if !order.Pending() {
		t.Fatal("held fast-path control had no pending sealed prefix")
	}
	if len(c.heldCh) != 1 || c.heldCount.Load() != 1 {
		t.Errorf("first wait preceded held append: buffered=%d count=%d", len(c.heldCh), c.heldCount.Load())
	}
	prefix.Release()
	c.ClearSendOrder(&order)
	if ok, err := c.SendFrameDeferredContext(c.ctx, UID{First: 3}, admissionBody(3)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	request, _, available := c.nextWrite(false)
	if !available {
		t.Fatal("same-drain probe found a reservation but no appended held frame")
	}
	if err := WriteFrame(c.conn, request.token, request.body, false); err != nil {
		t.Fatal(err)
	}
	heldWire(t, peer, 2) // early Notify instead exposes ordinary frame 3
	admissionFinish(c, request)
	resume()
	admissionDrain(t, c, peer, 3, true)
	admissionRequireSend(t, head)
	admissionQuiesced(t, c)
}

func TestHeldOutboxControlReservationAndAppendAreAtomic(t *testing.T) {
	t.Parallel()
	var order sendqueue.Queue
	c, peer := heldOutboxConn(t, &order)
	c.writeCh = make(chan writeReq, 2)
	WithSendOrder(&order)(c)
	entered, release := make(chan struct{}), make(chan struct{})
	resumeFirst := sync.OnceFunc(func() { close(release) })
	defer resumeFirst()
	workerOwnsLater := make(chan struct{})
	markWorker := sync.OnceFunc(func() { close(workerOwnsLater) })
	var gated, observeWorker atomic.Bool
	c.ctx = &outboxProbeContext{Context: c.ctx, onErr: func() {
		if gated.CompareAndSwap(false, true) {
			close(entered)
			<-release // P1 is reserved, but has not entered coldControls
		}
	}, onDone: func() {
		if observeWorker.Load() && len(c.coldControls) == 0 {
			markWorker() // only the worker can have consumed the queued P2
		}
	}}
	first, second := make(chan bool, 1), make(chan bool, 1)
	go func() { first <- c.tryEnqueue(writeReq{token: UID{First: 1}, body: admissionBody(1)}) }()
	admissionAwait(t, entered, "P1's reserved-before-append boundary")
	if !order.Pending() || len(c.coldControls) != 0 {
		t.Fatal("P1 was not reserved before its bounded inbox append")
	}
	serialized := !c.controlMu.TryLock()
	if !serialized {
		c.controlMu.Unlock()
	}
	go func() { second <- c.tryEnqueue(writeReq{token: UID{First: 2}, body: admissionBody(2)}) }()
	if serialized {
		resumeFirst()
		if !admissionAwait(t, first, "P1 append") || !admissionAwait(t, second, "P2 append") {
			t.Fatal("serialized controls did not fit their two-place inbox")
		}
		order.Seal()
		startHeldWriter(t, c, true)
	} else {
		if !admissionAwait(t, second, "P2 appending before the gated P1") {
			t.Fatal("unserialized P2 did not reach the inbox")
		}
		later := <-c.coldControls
		if later.request.token.First != 2 {
			t.Fatal("inversion control did not queue P2 first")
		}
		select {
		case <-later.permit.Ready():
			t.Fatal("P2 had no missing P1 predecessor")
		default:
		}
		c.coldControls <- later
		observeWorker.Store(true)
		startHeldWriter(t, c, true)
		admissionAwait(t, workerOwnsLater, "sole worker holding unready P2")
		resumeFirst()
		if !admissionAwait(t, first, "P1 appending behind the worker's P2") {
			t.Fatal("P1 did not append behind the inverted worker")
		}
		order.Seal()
		if len(c.coldControls) != 1 {
			t.Fatal("missing P1 did not remain behind the worker's P2")
		}
	}
	if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var reader FrameReader
	for _, want := range []uint64{1, 2} {
		token, body, err := reader.Read(peer, false)
		if err != nil {
			t.Errorf("control wire frame %d: %v; P1 buffered behind worker=%d", want, err, len(c.coldControls))
			break
		}
		if token.First != want || string(body) != string(admissionBody(want)) {
			t.Errorf("control wire frame = %d/%q, want %d/%q", token.First, body, want, admissionBody(want))
		}
	}
	_ = c.Close()
	admissionQuiesced(t, c)
}
