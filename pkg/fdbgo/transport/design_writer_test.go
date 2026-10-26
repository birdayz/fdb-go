//go:build linux

package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/internal/sendqueue"
	"fdb.dev/pkg/fdbgo/wire"
)

// Enqueue-only writer (RFC-282) tests against T1 and T2: a send is
// enqueue-plus-wait-for-reply, never a write acknowledgment.

const designDelay = 40 * time.Millisecond // coalescing delay long enough to observe

type writerArm struct {
	name string
	opts []DialOption
}

func writerArms() []writerArm {
	return []writerArm{
		{name: "d-ng", opts: []DialOption{withNetDesign(designNG), WithCoalesceDelay(0, 0)}},
		{name: "d-coalesce", opts: []DialOption{withNetDesign(designD), WithCoalesceDelay(designDelay, designDelay)}},
		{name: "d-adaptive", opts: []DialOption{withNetDesign(designNG), WithAdaptiveLinger(20 * time.Microsecond)}},
	}
}

func dialDesignArm(t *testing.T, opts ...DialOption) (*Conn, <-chan []byte) {
	t.Helper()
	c, release, frames := dialStalledPeer(t, opts...)
	close(release)
	if c.na == nil {
		t.Fatal("positive control: connection has no D-NG writer")
	}
	return c, frames
}

// gatedWriteConn blocks post-handshake Write calls until gate is closed.
type gatedWriteConn struct {
	net.Conn
	armed  atomic.Bool
	gate   chan struct{}
	writes atomic.Int64
}

func (g *gatedWriteConn) Write(p []byte) (int, error) {
	if g.armed.Load() {
		<-g.gate
	}
	n, err := g.Conn.Write(p)
	if g.armed.Load() && err == nil {
		g.writes.Add(1)
	}
	return n, err
}

// T1: a send returns once queued while the socket writer is gated (0 writes),
// and once the gate opens the peer receives every frame whole and in order.
func TestDesignSendIsEnqueueOnly(t *testing.T) {
	t.Parallel()
	for _, arm := range writerArms() {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			s := newSimServer(t)
			frames := make(chan []byte, 64)
			go func() {
				if s.handshake() != nil {
					return
				}
				var fr FrameReader
				for {
					tok, body, err := fr.Read(s.srv, false)
					if err != nil {
						return
					}
					frames <- []byte(fmt.Sprintf("%d:%s", tok.First, body))
				}
			}()
			gw := &gatedWriteConn{Conn: s.cli, gate: make(chan struct{})}
			var gateOnce sync.Once
			openGate := func() { gateOnce.Do(func() { close(gw.gate) }) }
			defer openGate()
			dial := func(context.Context, string, string) (net.Conn, error) { return gw, nil }
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			opts := append([]DialOption{withMonitorCadence(time.Hour, time.Hour)}, arm.opts...)
			if arm.name == "d-coalesce" {
				opts = append(opts, WithCoalesceDelay(10*time.Microsecond, 20*time.Microsecond))
			}
			c, err := dialWith(ctx, "sim", dial, nil, opts...)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() {
				openGate()
				_ = c.Close()
			}()
			if c.na == nil {
				t.Fatal("positive control: connection has no D-NG writer")
			}
			gw.armed.Store(true)

			const n = 20
			for i := uint64(1); i <= n; i++ {
				sendCtx, sendCancel := context.WithTimeout(ctx, 2*time.Second)
				ok, err := c.SendFrameContext(sendCtx, UID{First: i}, frameBody(i))
				sendCancel()
				if !ok || err != nil {
					openGate()
					t.Fatalf("send %d = (%t, %v), want queued without waiting for gated socket write", i, ok, err)
				}
			}
			// Every SendFrameContext, Flush, and FlushContext returned while gated.
			if err := c.Flush(); err != nil {
				openGate()
				t.Fatalf("Flush while socket gated = %v, want nil", err)
			}
			flushCtx, flushCancel := context.WithTimeout(ctx, 2*time.Second)
			err = c.FlushContext(flushCtx)
			flushCancel()
			if err != nil {
				openGate()
				t.Fatalf("FlushContext while socket gated = %v, want nil", err)
			}
			if got := gw.writes.Load(); got != 0 {
				t.Fatalf("writes before gate opened = %d, want 0 (send/flush waited on socket write)", got)
			}
			openGate()
			for i := uint64(1); i <= n; i++ {
				if got, want := string(within(t, frames, "a frame")), fmt.Sprint(i, ":f", i); got != want {
					t.Fatalf("peer read %q, want %q", got, want)
				}
			}
			expectNoFrame(t, frames, "after the last send")
		})
	}
}

// Controllable-clock test for WithAdaptiveLinger: idle flushes immediately,
// active bursts yield until lastWrite+window, and Close exits while yielding.
func TestDesignAdaptiveLingerBurstAndIdle(t *testing.T) {
	t.Parallel()
	const window = time.Hour
	baseTime := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	var nowOffsetNs atomic.Int64
	nowOffsetNs.Store(int64(2 * window)) // 2h after baseTime -> idle (>= 1h window)
	fakeNow := func() time.Time {
		return baseTime.Add(time.Duration(nowOffsetNs.Load()))
	}

	var yieldCalls atomic.Int64
	reachedMultipleYields := make(chan struct{})
	var multOnce sync.Once

	c, counter := dialWriteCountedRaw(t, WithAdaptiveLinger(window))
	if c.na == nil || c.AdaptiveLinger() != window {
		t.Fatalf("precondition: expected D-NG adaptive=%v, got na=%v adaptive=%v", window, c.na, c.AdaptiveLinger())
	}
	c.lastWrite = baseTime
	c.na.now = fakeNow
	c.na.onAdaptiveYield = func() {
		if yieldCalls.Add(1) >= 3 {
			multOnce.Do(func() { close(reachedMultipleYields) })
		}
	}
	handshake := len(counter.snapshot())

	// 1. Idle connection (now = baseTime+2h >= baseTime+1h): flushes with 0 yields.
	if err := c.SendFrameDeferred(UID{First: 1}, []byte{0}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		return len(counter.snapshot()) == handshake+1
	}, "immediate flush on idle connection")
	if got := yieldCalls.Load(); got != 0 {
		t.Fatalf("ADAPTIVE_IDLE_YIELDED: idle connection yielded %d times, want 0", got)
	}

	// 2. Active burst before deadline (baseTime+3h) yields without writing until
	// the fake clock reaches baseTime+3h.
	nowOffsetNs.Store(int64(2*window + 10*time.Millisecond))
	const burstFrames = 4
	if err := c.SendFrameDeferred(UID{First: 2}, []byte{1}); err != nil {
		t.Fatal(err)
	}
	within(t, reachedMultipleYields, "naLoop yielding repeatedly before deadline")
	for i := 1; i < burstFrames; i++ {
		if err := c.SendFrameDeferred(UID{First: uint64(i + 2)}, []byte{byte(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	yieldsBeforeAdvance := yieldCalls.Load()
	waitFor(t, func() bool {
		return yieldCalls.Load() > yieldsBeforeAdvance+2
	}, "continued yielding with all burst frames queued before deadline")
	if got := len(counter.snapshot()) - (handshake + 1); got != 0 {
		t.Fatalf("ADAPTIVE_EARLY: %d socket writes while clock is before deadline, want 0", got)
	}

	// Advance clock to deadline (baseTime+3h): naLoop exits its Before(deadline)
	// loop naturally and flushes all 4 burst frames in one socket write.
	nowOffsetNs.Store(int64(3 * window))
	waitFor(t, func() bool {
		return len(counter.snapshot()) == handshake+2
	}, "coalesced burst write after advancing clock past adaptive deadline")
	writes := counter.snapshot()[handshake+1:]
	if len(writes) != 1 {
		t.Fatalf("ADAPTIVE_SPLIT: %d socket writes for %d burst frames, want 1", len(writes), burstFrames)
	}
	var fr FrameReader
	r := bytes.NewReader(writes[0])
	for i := range burstFrames {
		token, body, err := fr.Read(r, false)
		if err != nil || token.First != uint64(i+2) || !bytes.Equal(body, []byte{byte(i + 1)}) {
			t.Fatalf("burst frame %d = %v %x %v", i, token, body, err)
		}
	}
	if r.Len() != 0 {
		t.Fatalf("%d trailing bytes after %d burst frames", r.Len(), burstFrames)
	}

	// 3. Close during an active adaptive linger must return promptly without writing.
	c2, counter2 := dialWriteCountedRaw(t, WithAdaptiveLinger(time.Hour))
	enteredCloseLinger := make(chan struct{})
	var closeOnce sync.Once
	c2.na.onAdaptiveYield = func() {
		closeOnce.Do(func() { close(enteredCloseLinger) })
	}
	beforeCloseWrites := len(counter2.snapshot())
	if err := c2.SendFrameDeferred(UID{First: 99}, []byte("never")); err != nil {
		t.Fatal(err)
	}
	within(t, enteredCloseLinger, "naLoop entering adaptive linger before Close")
	closed := make(chan error, 1)
	go func() { closed <- c2.Close() }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("ADAPTIVE_CLOSE_BLOCKED: Close waited for the 1h adaptive linger")
	}
	if n := len(counter2.snapshot()) - beforeCloseWrites; n != 0 {
		t.Fatalf("ADAPTIVE_WROTE_AFTER_CLOSE: %d writes after teardown", n)
	}
}

// T2: cancelling the caller's ctx after the send never removes or alters the queued
// frame; a ctx already done before the send queues nothing.
func TestDesignCancelNeverUnqueues(t *testing.T) {
	t.Parallel()
	c, frames := dialDesignArm(t, withNetDesign(designD), WithCoalesceDelay(designDelay, designDelay))
	ctx, cancel := context.WithCancel(context.Background())
	if ok, err := c.SendFrameContext(ctx, UID{First: 1}, frameBody(1)); !ok || err != nil {
		t.Fatalf("send = (%t, %v), want queued", ok, err)
	}
	if ok, err := c.SendFrameDeferredContext(ctx, UID{First: 2}, frameBody(2)); !ok || err != nil {
		t.Fatalf("deferred send = (%t, %v), want queued", ok, err)
	}
	cancel() // the writer is still waiting out its delay: both frames are only queued
	if err := c.FlushContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("flush on a cancelled ctx = %v, want context.Canceled", err)
	}
	if ok, err := c.SendFrameContext(ctx, UID{First: 3}, frameBody(3)); ok || !errors.Is(err, context.Canceled) {
		t.Fatalf("send on a cancelled ctx = (%t, %v), want not queued", ok, err)
	}

	// Cancelling a sender after it reaches beforeLock (while w.mu is held) must
	// reject admission on both stack (<=4KiB) and pooled (>4KiB) encoding paths.
	for _, body := range [][]byte{frameBody(4), bytes.Repeat([]byte{'P'}, 5000)} {
		reachedLock := make(chan struct{})
		var reachedOnce sync.Once
		c.na.beforeLock = func() {
			reachedOnce.Do(func() { close(reachedLock) })
		}
		c.na.mu.Lock()
		ctxContended, cancelContended := context.WithCancel(context.Background())
		contendedDone := make(chan error, 1)
		go func(b []byte) {
			ok, err := c.SendFrameContext(ctxContended, UID{First: 4}, b)
			if ok || !errors.Is(err, context.Canceled) {
				contendedDone <- fmt.Errorf("contended cancelled send = (%t, %v), want (false, context.Canceled)", ok, err)
				return
			}
			contendedDone <- nil
		}(body)
		within(t, reachedLock, "sender reached beforeLock")
		cancelContended()
		c.na.mu.Unlock()
		if err := within(t, contendedDone, "contended cancelled sender"); err != nil {
			t.Fatal(err)
		}
	}
	c.na.beforeLock = nil

	expectFrames(t, frames, 1, 2)
	expectNoFrame(t, frames, "the cancelled-before-send frame")
}

// failingConn fails every write once armed; reads block until Close.
type failingConn struct {
	net.Conn
	counting atomic.Bool
	armed    atomic.Bool
	writes   chan struct{}
}

var errInjectedWrite = errors.New("injected write failure")

func (f *failingConn) Write(p []byte) (int, error) {
	if f.armed.Load() {
		return 0, errInjectedWrite
	}
	n, err := f.Conn.Write(p)
	if f.counting.Load() && err == nil {
		select {
		case f.writes <- struct{}{}:
		default:
		}
	}
	return n, err
}

// T1: a write error fails the connection, and every pending reply on it fails
// with the coded teardown; the senders themselves already returned.
func TestDesignWriteErrorFailsAllPending(t *testing.T) {
	t.Parallel()
	for _, arm := range writerArms() {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			s := newSimServer(t)
			go func() {
				if s.handshake() == nil {
					s.drainUntilClosed()
				}
			}()
			fc := &failingConn{Conn: s.cli, writes: make(chan struct{}, 16)}
			dial := func(context.Context, string, string) (net.Conn, error) { return fc, nil }
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			opts := append([]DialOption{withMonitorCadence(time.Hour, time.Hour)}, arm.opts...)
			if arm.name == "d-coalesce" {
				opts = append(opts, WithCoalesceDelay(10*time.Microsecond, 20*time.Microsecond))
			}
			c, err := dialWith(ctx, "sim", dial, nil, opts...)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.Close()
			if c.na == nil {
				t.Fatal("positive control: no D-NG writer")
			}
			fc.counting.Store(true)
			var replies []<-chan Response
			for i := 0; i < 3; i++ {
				token, ch, h := c.PrepareReply()
				defer h.Release()
				replies = append(replies, ch)
				fc.armed.Store(i == 2) // the third send's write fails
				ok, err := c.SendFrameContext(context.Background(), UID{First: 9}, []byte(fmt.Sprint(token.First)))
				if !ok || err != nil {
					t.Fatalf("send %d = (%t, %v), want queued", i, ok, err)
				}
				if i < 2 {
					within(t, fc.writes, "healthy socket write")
				}
			}
			for i, ch := range replies {
				r := within(t, ch, fmt.Sprint("pending reply ", i))
				var fe *wire.FDBError
				if !errors.Is(r.Err, ErrConnClosed) || !errors.As(r.Err, &fe) || fe.Code != codeRequestMaybeDelivered {
					t.Fatalf("reply %d = %v, want ErrConnClosed with FDB code %d", i, r.Err, codeRequestMaybeDelivered)
				}
			}
			if !c.IsClosed() {
				t.Fatal("a failed write left the connection open")
			}
			for name, err := range map[string]error{
				"SendFrame":         c.SendFrame(UID{First: 9}, nil),
				"SendFrameDeferred": c.SendFrameDeferred(UID{First: 9}, nil),
				"Flush":             c.Flush(),
			} {
				var cfe *wire.FDBError
				if !errors.Is(err, ErrConnClosed) || !errors.As(err, &cfe) || cfe.Code != codeRequestMaybeDelivered {
					t.Fatalf("%s after teardown = %v, want ErrConnClosed with FDB code %d", name, err, codeRequestMaybeDelivered)
				}
			}
		})
	}
}

// T1 under contention: concurrent senders' frames reach the peer whole, each
// exactly once, in each sender's order; none is stranded in a queue.
func TestDesignConcurrentSendersFramesWholeOnce(t *testing.T) {
	t.Parallel()
	for _, arm := range []writerArm{
		{name: "d-ng", opts: []DialOption{withNetDesign(designNG), WithCoalesceDelay(0, 0)}},
		{name: "d-coalesce", opts: []DialOption{withNetDesign(designD), WithCoalesceDelay(20*time.Microsecond, 20*time.Microsecond)}},
		{name: "d-adaptive", opts: []DialOption{withNetDesign(designNG), WithAdaptiveLinger(20 * time.Microsecond)}},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			c, frames := dialDesignArm(t, arm.opts...)
			const senders, each = 16, 50
			padLen := func(i int) int {
				if i%10 == 9 {
					return 5000 // > 4096 bytes: exercises pooled encodeFrame path
				}
				return 64 * (i % 5) // <= 4096 bytes: exercises stackBuf path
			}
			done := make(chan error, senders)
			for s := 0; s < senders; s++ {
				go func(s int) {
					for i := 0; i < each; i++ {
						body := []byte(fmt.Sprintf("s%02d-%03d-%s", s, i, bytes.Repeat([]byte{'p'}, padLen(i))))
						if ok, err := c.SendFrameContext(context.Background(), UID{First: uint64(s)}, body); !ok || err != nil {
							done <- fmt.Errorf("sender %d frame %d = (%t, %v)", s, i, ok, err)
							return
						}
					}
					done <- nil
				}(s)
			}
			for range senders {
				if err := within(t, done, "a sender"); err != nil {
					t.Fatal(err)
				}
			}
			next := make([]int, senders)
			for range senders * each {
				f := string(within(t, frames, "a frame"))
				body := f[strings.Index(f, ":")+1:]
				var s, i int
				if _, err := fmt.Sscanf(body, "s%02d-%03d-", &s, &i); err != nil {
					t.Fatalf("frame %q did not decode: %v", f, err)
				}
				if i != next[s] {
					t.Fatalf("sender %d: frame %d arrived, want %d (lost, duplicated or reordered)", s, i, next[s])
				}
				if gotPad := len(body) - len("s00-000-"); gotPad != padLen(i) {
					t.Fatalf("sender %d frame %d: pad len %d, want %d", s, i, gotPad, padLen(i))
				}
				next[s]++
			}
			expectNoFrame(t, frames, "after every frame arrived")
		})
	}
}

// High-water backpressure: ctx cancellation, naLoop wakeup, and FIFO wire order
// when a sender suspended in WithSuspendHandoff races a second sender on drain.
func TestDesignHighWaterBackpressure(t *testing.T) {
	t.Parallel()
	c, release, frames := dialStalledPeer(t, withNetDesign(designNG))
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	// Pre-fill w.q with >= naHighWater while w.sig is empty so naLoop stays parked.
	_, bigFrame := encodeFrame(UID{First: 100}, bytes.Repeat([]byte{'x'}, naHighWater), false)
	c.na.mu.Lock()
	c.na.q = append(c.na.q[:0], bigFrame...)
	c.na.mu.Unlock()

	if c.tryEnqueue(writeReq{token: UID{First: 99}, body: []byte("drop")}) {
		t.Fatal("tryEnqueue succeeded at naHighWater, want drop")
	}

	ctx, cancel := context.WithCancel(context.Background())
	ctx, signal := WithSuspendSignal(ctx)
	t.Cleanup(signal.Fire)
	done := make(chan error, 1)
	go func() {
		ok, err := c.SendFrameContext(ctx, UID{First: 1}, []byte("blocked"))
		if ok || !errors.Is(err, context.Canceled) {
			done <- fmt.Errorf("cancelled high-water send = (%t, %v), want (false, context.Canceled)", ok, err)
			return
		}
		done <- nil
	}()
	within(t, signal.Done(), "high-water suspend")
	cancel()
	if err := within(t, done, "cancelled high-water sender"); err != nil {
		t.Fatal(err)
	}

	// A panicking NotifySuspend hook at high-water must release its sendWaiters permit.
	panicCtx, panicSig := WithSuspendHandoff(context.Background(), make(chan struct{}), func() {
		panic("injected high-water suspend panic")
	})
	t.Cleanup(panicSig.Fire)
	panickedAtHighWater := false
	func() {
		defer func() {
			if recover() != nil {
				panickedAtHighWater = true
			}
		}()
		_, _ = c.SendFrameContext(panicCtx, UID{First: 98}, []byte("panic-hw"))
	}()
	if !panickedAtHighWater || c.sendWaiters.Pending() {
		t.Fatalf("high-water NotifySuspend panic: panicked=%t sendWaiters.Pending=%t, want (true, false)", panickedAtHighWater, c.sendWaiters.Pending())
	}

	// Sender 2 ("begin") reserves its c.sendWaiters FIFO slot before NotifySuspend,
	// so its frame (2:f2) precedes sender 3's (3:f3) when naLoop drains w.q.
	resumeBeginHandoff := make(chan struct{})
	defer func() {
		select {
		case <-resumeBeginHandoff:
		default:
			close(resumeBeginHandoff)
		}
	}()
	ctx2, signal2 := WithSuspendHandoff(context.Background(), resumeBeginHandoff, nil)
	t.Cleanup(signal2.Fire)
	done2 := make(chan error, 1)
	go func() {
		ok, err := c.SendFrameContext(ctx2, UID{First: 2}, frameBody(2))
		if !ok || err != nil {
			done2 <- fmt.Errorf("resumed begin send = (%t, %v), want (true, nil)", ok, err)
			return
		}
		done2 <- nil
	}()
	within(t, signal2.Done(), "begin high-water suspend handoff")

	ctx3, signal3 := WithSuspendSignal(context.Background())
	t.Cleanup(signal3.Fire)
	done3 := make(chan error, 1)
	go func() {
		ok, err := c.SendFrameContext(ctx3, UID{First: 3}, frameBody(3))
		if !ok || err != nil {
			done3 <- fmt.Errorf("resumed end send = (%t, %v), want (true, nil)", ok, err)
			return
		}
		done3 <- nil
	}()
	within(t, signal3.Done(), "end waiting behind begin's FIFO permit")

	// Let naLoop drain the 16 MiB frame while begin is STILL held in NotifySuspend:
	// neither end nor a concurrent tryEnqueue may overtake begin!
	close(release)
	c.na.sig <- struct{}{}
	if first := string(within(t, frames, "drained high-water frame")); !strings.HasPrefix(first, "100:") {
		t.Fatalf("first frame prefix = %q, want 100:...", first[:min(len(first), 16)])
	}
	if c.tryEnqueue(writeReq{token: UID{First: 99}, body: []byte("drop")}) {
		t.Fatal("tryEnqueue overtook a pending sendWaiters FIFO head")
	}
	close(resumeBeginHandoff)
	if err := within(t, done2, "resumed begin sender"); err != nil {
		t.Fatal(err)
	}
	if err := within(t, done3, "resumed end sender"); err != nil {
		t.Fatal(err)
	}
	if got, want := string(within(t, frames, "begin frame")), "2:f2"; got != want {
		t.Fatalf("peer read %q, want %q", got, want)
	}
	if got, want := string(within(t, frames, "end frame")), "3:f3"; got != want {
		t.Fatalf("peer read %q, want %q", got, want)
	}
}

// Cold/held outbox ordering and un-ordered startup PING on the default D-NG writer.
func TestDesignColdHeldOrderAndStartupPing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Un-ordered connection (no WithSendOrder): forcing handlePing at beforeReadLoop
	// must enqueue on c.na and deliver the PING reply to the peer, not c.writeCh.
	pingSrv := newSimServer(t)
	pingFrames := make(chan UID, 4)
	pingTok := UID{First: 555, Second: 777}
	go func() {
		if pingSrv.handshake() != nil {
			return
		}
		var fr FrameReader
		for {
			tok, _, err := fr.Read(pingSrv.srv, false)
			if err != nil {
				return
			}
			pingFrames <- tok
		}
	}()
	var pingConn *Conn
	pc, err := dialWith(ctx, "sim", pingSrv.dialFunc(), nil, withMonitorCadence(time.Hour, time.Hour), func(c *Conn) {
		pingConn = c
		c.beforeReadLoop = func() {
			c.handlePing(BuildPingRequest(pingTok))
		}
	})
	if err != nil {
		t.Fatalf("dial pingConn: %v", err)
	}
	defer pc.Close()
	if len(pingConn.writeCh) != 0 {
		t.Fatalf("startup PING landed in legacy writeCh (len=%d)", len(pingConn.writeCh))
	}
	if got := within(t, pingFrames, "un-ordered startup PING reply"); got != pingTok {
		t.Fatalf("startup PING token = %v, want %v", got, pingTok)
	}

	// 2. Cold-ordered connection: serialize held-control reservation and publication
	// under controlMu across two concurrent producers gated at beforeControlLock.
	s := newSimServer(t)
	frames := make(chan string, 32)
	go func() {
		if s.handshake() != nil {
			return
		}
		var fr FrameReader
		for {
			tok, body, err := fr.Read(s.srv, false)
			if err != nil {
				return
			}
			frames <- fmt.Sprintf("%d:%s", tok.First, body)
		}
	}()

	var order sendqueue.Queue
	prefixPermit, _ := order.Reserve()
	enteredCtrl1, releaseCtrl1 := make(chan struct{}), make(chan struct{})
	var releaseCtrl1Once sync.Once
	openCtrl1 := func() { releaseCtrl1Once.Do(func() { close(releaseCtrl1) }) }
	reachedCtrl2 := make(chan struct{})
	var armCtrl, gateCtrl1 atomic.Bool
	var ctrlCalls atomic.Int32
	c, err := dialWith(ctx, "sim", s.dialFunc(), nil, withMonitorCadence(time.Hour, time.Hour), WithSendOrder(&order), func(c *Conn) {
		origCtx := c.ctx
		c.ctx = &outboxProbeContext{Context: origCtx, onErr: func() {
			if armCtrl.Load() && gateCtrl1.CompareAndSwap(false, true) {
				close(enteredCtrl1)
				<-releaseCtrl1
			}
		}}
		c.beforeControlLock = func() {
			if armCtrl.Load() && ctrlCalls.Add(1) == 2 {
				close(reachedCtrl2)
			}
		}
	})
	if err != nil {
		prefixPermit.Release()
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	defer openCtrl1()

	// Seal the cold cohort while prefixPermit is held, then gate ctrl1 inside
	// tryEnqueueColdControl until ctrl2 reaches beforeControlLock.
	order.Seal()
	armCtrl.Store(true)

	ctrl1Done, ctrl2Done := make(chan bool, 1), make(chan bool, 1)
	go func() { ctrl1Done <- c.tryEnqueue(writeReq{token: UID{First: 2}, body: []byte("held-ctrl-1")}) }()
	within(t, enteredCtrl1, "ctrl1 holding controlMu after reserving H1")
	go func() { ctrl2Done <- c.tryEnqueue(writeReq{token: UID{First: 3}, body: []byte("held-ctrl-2")}) }()
	within(t, reachedCtrl2, "ctrl2 reached tryEnqueueNAControl before controlMu")
	openCtrl1()
	if !within(t, ctrl1Done, "ctrl1 enqueue") || !within(t, ctrl2Done, "ctrl2 enqueue") {
		prefixPermit.Release()
		t.Fatal("tryEnqueue dropped a held control during a sealed cold cohort")
	}

	held1Ctx, held1Sig := WithSuspendSignal(ctx)
	defer held1Sig.Fire()
	held1Done := make(chan error, 1)
	go func() {
		_, err := c.SendFrameContextOrdered(held1Ctx, UID{First: 4}, []byte("held1"), nil)
		held1Done <- err
	}()
	within(t, held1Sig.Done(), "held1 suspend")

	resumeHeld2 := make(chan struct{})
	var resumeHeld2Once sync.Once
	openHeld2 := func() { resumeHeld2Once.Do(func() { close(resumeHeld2) }) }
	defer openHeld2()
	held2Ctx, held2Sig := WithSuspendHandoff(ctx, resumeHeld2, nil)
	defer held2Sig.Fire()
	held2Done := make(chan error, 1)
	go func() {
		_, err := c.SendFrameContextOrdered(held2Ctx, UID{First: 5}, []byte("held2"), nil)
		held2Done <- err
	}()
	within(t, held2Sig.Done(), "held2 suspend")

	if ok, err := c.SendFrameContextOrdered(ctx, UID{First: 1}, []byte("prefix"), prefixPermit); !ok || err != nil {
		t.Fatalf("prefix send = (%t, %v)", ok, err)
	}
	if err := within(t, held1Done, "held1 completion"); err != nil {
		t.Fatal(err)
	}

	// While held2 is still parked on resumeHeld2 after the prefix has drained
	// (and ClearSendOrder has detached order), a warm sender and a warm control
	// frame must still queue behind held2 on c.heldWaiters rather than overtaking it.
	c.ClearSendOrder(&order)
	warm1Ctx, warm1Sig := WithSuspendSignal(ctx)
	defer warm1Sig.Fire()
	warm1Done := make(chan error, 1)
	go func() {
		_, err := c.SendFrameContext(warm1Ctx, UID{First: 6}, []byte("warm1"))
		warm1Done <- err
	}()
	within(t, warm1Sig.Done(), "warm1 waiting behind held2")
	if !c.tryEnqueue(writeReq{token: UID{First: 7}, body: []byte("warm-ctrl")}) {
		t.Fatal("tryEnqueue dropped a warm control behind held2")
	}

	openHeld2()
	if err := within(t, held2Done, "held2 completion"); err != nil {
		t.Fatal(err)
	}
	if err := within(t, warm1Done, "warm1 completion"); err != nil {
		t.Fatal(err)
	}

	var nonPing []string
	for len(nonPing) < 7 {
		nonPing = append(nonPing, within(t, frames, "ordered frame"))
	}
	want := []string{"1:prefix", "2:held-ctrl-1", "3:held-ctrl-2", "4:held1", "5:held2", "6:warm1", "7:warm-ctrl"}
	for i := range want {
		if nonPing[i] != want[i] {
			t.Fatalf("wire order = %v, want %v", nonPing, want)
		}
	}
}

type panicOnceWriter struct {
	shouldPanic atomic.Bool
	gateNext    atomic.Bool
	enteredGate chan struct{}
	releaseGate chan struct{}
	mu          sync.Mutex
	logged      []uint64
}

func (w *panicOnceWriter) Write(p []byte) (int, error) {
	if w.shouldPanic.CompareAndSwap(true, false) {
		panic("injected wirelog panic")
	}
	if len(p) == 29 && p[0] == 'S' {
		tok := uint64(p[9]) | uint64(p[10])<<8 | uint64(p[11])<<16 | uint64(p[12])<<24 |
			uint64(p[13])<<32 | uint64(p[14])<<40 | uint64(p[15])<<48 | uint64(p[16])<<56
		w.mu.Lock()
		w.logged = append(w.logged, tok)
		entered, release := w.enteredGate, w.releaseGate
		w.mu.Unlock()
		if w.gateNext.CompareAndSwap(true, false) {
			close(entered)
			<-release
		}
	}
	return len(p), nil
}

// A panicking CaptureWireLog writer must not leave w.mu locked or c.sendWaiters
// permits leaked on any D-NG enqueue path (small frame, >4KiB frame, tryEnqueue).
func TestDesignWireLogPanicDoesNotPoisonWriter(t *testing.T) {
	s := newSimServer(t)
	frames := make(chan uint64, 16)
	go func() {
		if s.handshake() != nil {
			return
		}
		var fr FrameReader
		for {
			tok, _, err := fr.Read(s.srv, false)
			if err != nil {
				return
			}
			frames <- tok.First
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialWith(ctx, "sim", s.dialFunc(), nil, withMonitorCadence(time.Hour, time.Hour))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	pw := &panicOnceWriter{}
	stop := CaptureWireLog(pw)
	defer stop()

	expectPanic := func(name string, fn func()) {
		t.Helper()
		pw.shouldPanic.Store(true)
		panicked := false
		func() {
			defer func() {
				if recover() != nil {
					panicked = true
				}
			}()
			fn()
		}()
		if !panicked {
			t.Fatalf("%s did not panic as expected", name)
		}
	}

	expectPanic("small SendFrameContext", func() {
		_, _ = c.SendFrameContext(ctx, UID{First: 1}, []byte("small"))
	})
	expectPanic("large SendFrameContext", func() {
		_, _ = c.SendFrameContext(ctx, UID{First: 2}, bytes.Repeat([]byte{'L'}, 5000))
	})
	expectPanic("tryEnqueue", func() {
		_ = c.tryEnqueue(writeReq{token: UID{First: 3}, body: []byte("try")})
	})

	if ok, err := c.SendFrameContext(ctx, UID{First: 10}, []byte("after-panic-small")); !ok || err != nil {
		t.Fatalf("post-panic small SendFrameContext = (%t, %v)", ok, err)
	}
	if ok, err := c.SendFrameContext(ctx, UID{First: 11}, bytes.Repeat([]byte{'M'}, 5000)); !ok || err != nil {
		t.Fatalf("post-panic large SendFrameContext = (%t, %v)", ok, err)
	}
	if !c.tryEnqueue(writeReq{token: UID{First: 12}, body: []byte("after-panic-try")}) {
		t.Fatal("post-panic tryEnqueue returned false")
	}

	// Verify concurrent FIFO preservation across the unlocked LogSend callback for
	// both small (<=4KiB) and large (>4KiB) frames, and tryEnqueue rejection while pending.
	for _, tc := range []struct {
		tok1, tok2 uint64
		body       []byte
	}{
		{20, 21, []byte("small-ordered")},
		{30, 31, bytes.Repeat([]byte{'B'}, 5000)},
	} {
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		openGate := func() { releaseOnce.Do(func() { close(release) }) }
		defer openGate()
		pw.mu.Lock()
		pw.enteredGate, pw.releaseGate = entered, release
		pw.mu.Unlock()
		pw.gateNext.Store(true)

		done1, done2 := make(chan error, 1), make(chan error, 1)
		go func() {
			_, err := c.SendFrameContext(ctx, UID{First: tc.tok1}, tc.body)
			done1 <- err
		}()
		within(t, entered, "first sender entered unlocked LogSend callback")
		if c.tryEnqueue(writeReq{token: UID{First: 99}, body: []byte("drop-behind-wirelog")}) {
			openGate()
			t.Fatal("tryEnqueue overtook a sender blocked in LogSend")
		}
		ctx2, sig2 := WithSuspendSignal(ctx)
		go func() {
			_, err := c.SendFrameContext(ctx2, UID{First: tc.tok2}, tc.body)
			done2 <- err
		}()
		within(t, sig2.Done(), "second sender suspended on sendWaiters behind first sender's LogSend")
		openGate()
		sig2.Fire()
		if err := within(t, done1, "first gated sender"); err != nil {
			t.Fatal(err)
		}
		if err := within(t, done2, "second gated sender"); err != nil {
			t.Fatal(err)
		}
	}

	// Cancelling a sender while it is blocked in the unlocked LogSend callback must
	// reject admission on re-lock without appending the frame, for both sizes.
	for _, tc := range []struct {
		tok  uint64
		body []byte
	}{
		{40, []byte("cancel-small")},
		{41, bytes.Repeat([]byte{'C'}, 5000)},
	} {
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		openGate := func() { releaseOnce.Do(func() { close(release) }) }
		defer openGate()
		pw.mu.Lock()
		pw.enteredGate, pw.releaseGate = entered, release
		pw.mu.Unlock()
		pw.gateNext.Store(true)

		ctxCancel, cancelSend := context.WithCancel(ctx)
		doneCancel := make(chan error, 1)
		go func() {
			ok, err := c.SendFrameContext(ctxCancel, UID{First: tc.tok}, tc.body)
			if ok || !errors.Is(err, context.Canceled) {
				doneCancel <- fmt.Errorf("cancelled-in-LogSend (%d) = (%t, %v), want (false, context.Canceled)", tc.tok, ok, err)
				return
			}
			doneCancel <- nil
		}()
		within(t, entered, "sender entered LogSend before cancellation")
		cancelSend()
		openGate()
		if err := within(t, doneCancel, "cancelled-in-LogSend sender"); err != nil {
			t.Fatal(err)
		}
	}

	if ok, err := c.SendFrameContext(ctx, UID{First: 50}, []byte("after-cancel")); !ok || err != nil {
		t.Fatalf("post-cancel SendFrameContext = (%t, %v)", ok, err)
	}

	wantSocketOrder := []uint64{10, 11, 12, 20, 21, 30, 31, 50}
	for _, want := range wantSocketOrder {
		if got := within(t, frames, "ordered frame"); got != want {
			t.Fatalf("peer socket frame token = %d, want %d", got, want)
		}
	}
	wantLoggedOrder := []uint64{10, 11, 12, 20, 21, 30, 31, 40, 41, 50}
	pw.mu.Lock()
	gotLogged := append([]uint64(nil), pw.logged...)
	pw.mu.Unlock()
	if fmt.Sprint(gotLogged) != fmt.Sprint(wantLoggedOrder) {
		t.Fatalf("wire-log token order = %v, want %v", gotLogged, wantLoggedOrder)
	}
}
