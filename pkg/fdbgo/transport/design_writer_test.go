//go:build linux

package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

// T1: a send returns once its frame is queued, before the writer's delay ends,
// and the peer receives every frame exactly once, in send order.
func TestDesignSendIsEnqueueOnly(t *testing.T) {
	t.Parallel()
	for _, arm := range writerArms() {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			c, frames := dialDesignArm(t, arm.opts...)
			const n = 20
			for i := uint64(1); i <= n; i++ {
				start := time.Now()
				if ok, err := c.SendFrameContext(context.Background(), UID{First: i}, frameBody(i)); !ok || err != nil {
					t.Fatalf("send %d = (%t, %v), want queued", i, ok, err)
				}
				// The D delay is at least 0.9 × 40 ms; a send that waited for its write would exceed it.
				if arm.name == "d-coalesce" && time.Since(start) >= designDelay*9/10 {
					t.Fatalf("send %d took %v: it waited for the write", i, time.Since(start))
				}
			}
			for i := uint64(1); i <= n; i++ {
				if got, want := string(within(t, frames, "a frame")), fmt.Sprint(i, ":f", i); got != want {
					t.Fatalf("peer read %q, want %q", got, want)
				}
			}
			expectNoFrame(t, frames, "after the last send")
		})
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
	expectFrames(t, frames, 1, 2)
	expectNoFrame(t, frames, "the cancelled-before-send frame")
}

// failingConn fails every write once armed; reads block until Close.
type failingConn struct {
	net.Conn
	armed atomic.Bool
}

var errInjectedWrite = errors.New("injected write failure")

func (f *failingConn) Write(p []byte) (int, error) {
	if f.armed.Load() {
		return 0, errInjectedWrite
	}
	return f.Conn.Write(p)
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
			fc := &failingConn{Conn: s.cli}
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
					time.Sleep(5 * time.Millisecond) // let the healthy sends go out first
				}
			}
			for i, ch := range replies {
				r := within(t, ch, fmt.Sprint("pending reply ", i))
				if !errors.Is(r.Err, ErrConnClosed) {
					t.Fatalf("reply %d = %v, want the coded teardown", i, r.Err)
				}
			}
			if !c.IsClosed() {
				t.Fatal("a failed write left the connection open")
			}
			if ok, err := c.SendFrameContext(context.Background(), UID{First: 9}, nil); ok || !errors.Is(err, ErrConnClosed) {
				t.Fatalf("send after teardown = (%t, %v), want the teardown", ok, err)
			}
		})
	}
}

// T1 under contention: concurrent senders' frames reach the peer whole, each
// exactly once, in each sender's order; none is stranded in a queue. Includes
// both small frames (<= 4 KiB stack-encoded) and large frames (> 4 KiB pooled).
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

// High-water backpressure: when the queue reaches naHighWater while the peer
// is stalled, a sender suspends and respects ctx cancellation; once the writer
// drains the queue, waiting senders resume in order. Also verifies tryEnqueue
// drops without blocking when at naHighWater.
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
	// Pre-fill w.q to naHighWater under w.mu while keeping w.sig empty so naLoop
	// stays parked until we release it.
	c.na.mu.Lock()
	c.na.q = make([]byte, naHighWater)
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

	// Clear the synthetic high-water fill and wake naLoop; a waiting sender resumes.
	ctx2, signal2 := WithSuspendSignal(context.Background())
	t.Cleanup(signal2.Fire)
	done2 := make(chan error, 1)
	go func() {
		ok, err := c.SendFrameContext(ctx2, UID{First: 2}, frameBody(2))
		if !ok || err != nil {
			done2 <- fmt.Errorf("resumed send = (%t, %v), want (true, nil)", ok, err)
			return
		}
		done2 <- nil
	}()
	within(t, signal2.Done(), "second high-water suspend")
	c.na.mu.Lock()
	c.na.q = c.na.q[:0]
	if c.na.drained != nil {
		close(c.na.drained)
		c.na.drained = nil
	}
	c.na.mu.Unlock()
	close(release)
	if err := within(t, done2, "resumed high-water sender"); err != nil {
		t.Fatal(err)
	}
	if got, want := string(within(t, frames, "resumed frame")), "2:f2"; got != want {
		t.Fatalf("peer read %q, want %q", got, want)
	}
}
