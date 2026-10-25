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

// Design D's writer (D1, docs/labs/single-network-goroutine/DESIGN.md) against
// T1 and T2: a send is enqueue-plus-wait-for-reply, never a write acknowledgment.

const designDelay = 40 * time.Millisecond // coalescing delay long enough to observe

func dialDesign(t *testing.T, mode string, delay time.Duration) (*Conn, <-chan []byte) {
	t.Helper()
	c, release, frames := dialStalledPeer(t, withNetDesign(mode), WithCoalesceDelay(delay, delay))
	close(release)
	if c.na == nil && c.m == nil && c.cw == nil && c.rw == nil {
		t.Fatalf("positive control: %s connection has no design writer", mode)
	}
	return c, frames
}

// T1: a send returns once its frame is queued, before the writer's delay ends,
// and the peer receives every frame exactly once, in send order.
func TestDesignSendIsEnqueueOnly(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{designD, designNG, designM, designNGYield, designNGChan, designNGRing, designNGAdaptive} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			c, frames := dialDesign(t, mode, designDelay)
			const n = 20
			for i := uint64(1); i <= n; i++ {
				start := time.Now()
				if ok, err := c.SendFrameContext(context.Background(), UID{First: i}, frameBody(i)); !ok || err != nil {
					t.Fatalf("send %d = (%t, %v), want queued", i, ok, err)
				}
				// The D delay is at least 0.9 × 40 ms; a send that waited for its write would exceed it.
				if mode == designD && time.Since(start) >= designDelay*9/10 {
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
	c, frames := dialDesign(t, designD, designDelay)
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
	for _, mode := range []string{designD, designNG, designM, designNGYield, designNGChan, designNGRing, designNGAdaptive} {
		t.Run(mode, func(t *testing.T) {
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
			c, err := dialWith(ctx, "sim", dial, nil, withMonitorCadence(time.Hour, time.Hour), withNetDesign(mode))
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.Close()
			if c.na == nil && c.m == nil && c.cw == nil && c.rw == nil {
				t.Fatal("positive control: no design writer")
			}
			var replies []<-chan Response
			for i := 0; i < 3; i++ {
				token, ch, h := c.PrepareReply()
				defer h.Release()
				replies = append(replies, ch)
				fc.armed.Store(i == 2) // the third send's write fails
				ok, err := c.SendFrameContext(context.Background(), UID{First: 9}, []byte(fmt.Sprint(token.First)))
				if mode == designM && i == 2 {
					// D-M writes on the sender's goroutine: the sender sees its write fail,
					// as a maybe-delivered frame.
					if !ok || !errors.Is(err, ErrConnClosed) {
						t.Fatalf("failing D-M send = (%t, %v), want maybe-delivered teardown", ok, err)
					}
				} else if !ok || err != nil {
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
// exactly once, in each sender's order; none is stranded in a queue.
func TestDesignConcurrentSendersFramesWholeOnce(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{designD, designNG, designM, designNGYield, designNGChan, designNGRing, designNGAdaptive} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			c, frames := dialDesign(t, mode, 20*time.Microsecond)
			const senders, each = 16, 50
			done := make(chan error, senders)
			for s := 0; s < senders; s++ {
				go func(s int) {
					for i := 0; i < each; i++ {
						body := []byte(fmt.Sprintf("s%02d-%03d-%s", s, i, bytes.Repeat([]byte{'p'}, 64*(i%5))))
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
				var s, i int
				if _, err := fmt.Sscanf(f[strings.Index(f, ":")+1:], "s%02d-%03d-", &s, &i); err != nil {
					t.Fatalf("frame %q did not decode: %v", f, err)
				}
				if i != next[s] {
					t.Fatalf("sender %d: frame %d arrived, want %d (lost, duplicated or reordered)", s, i, next[s])
				}
				next[s]++
			}
			expectNoFrame(t, frames, "after every frame arrived")
		})
	}
}

// D-NG-ring: frames that wrap the ring and oversize frames carried in ext
// slots arrive whole, once, in each sender's order.
func TestDesignRingWrapAndOversize(t *testing.T) {
	t.Parallel()
	c, frames := dialDesign(t, designNGRing, 0)
	if c.rw == nil {
		t.Fatal("positive control: no ring writer")
	}
	const senders, each = 6, 60
	size := func(s, i int) int {
		if s == 0 && i%10 == 3 {
			return ringChunk + 4096 + i // oversize: an ext slot outside the ring
		}
		return 1000 + (s*7919+i*104729)%12000 // ~1.4 MiB per round: wraps
	}
	done := make(chan error, senders)
	for s := 0; s < senders; s++ {
		go func(s int) {
			for i := 0; i < each; i++ {
				body := append([]byte(fmt.Sprintf("s%02d-%03d-", s, i)), bytes.Repeat([]byte{byte('a' + s)}, size(s, i))...)
				if ok, err := c.SendFrameContext(context.Background(), UID{First: uint64(s)}, body); !ok || err != nil {
					done <- fmt.Errorf("sender %d frame %d = (%t, %v)", s, i, ok, err)
					return
				}
			}
			done <- nil
		}(s)
	}
	next := make([]int, senders)
	for range senders * each {
		f := string(within(t, frames, "a frame"))
		body := f[strings.Index(f, ":")+1:]
		var s, i int
		if _, err := fmt.Sscanf(body, "s%02d-%03d-", &s, &i); err != nil {
			t.Fatalf("frame did not decode: %v", err)
		}
		if i != next[s] {
			t.Fatalf("sender %d: frame %d arrived, want %d", s, i, next[s])
		}
		pad := body[len("s00-000-"):]
		if len(pad) != size(s, i) || strings.Trim(pad, string(rune('a'+s))) != "" {
			t.Fatalf("sender %d frame %d: torn or wrong padding (%d bytes, want %d)", s, i, len(pad), size(s, i))
		}
		next[s]++
	}
	for range senders {
		if err := within(t, done, "a sender"); err != nil {
			t.Fatal(err)
		}
	}
	expectNoFrame(t, frames, "after every frame arrived")
}
