//go:build linux

package transport

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// expireCoalesceWait ends the connection writer's current coalescing wait (naLoop or
// writeLoop), keeping Reset active until done reports true.
func expireCoalesceWait(t *testing.T, c *Conn, done func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen", what)
		}
		_ = c.coalesce.Reset(time.Microsecond)
		time.Sleep(time.Millisecond)
	}
}

// Frames sent while the connection waits share one socket write, made only when
// the wait ends (FlowTransport.actor.cpp:704-732). The delay is an hour, and the
// test ends it once every frame is queued, so no scheduling delay can split the batch.
func TestCoalesceDelaySharesOneWrite(t *testing.T) {
	t.Parallel()
	c, counter := dialWriteCounted(t, WithCoalesceDelay(time.Hour, time.Hour))
	handshake := len(counter.snapshot())
	const frames = 4
	for i := range frames {
		if err := c.SendFrameDeferred(UID{First: uint64(i + 1)}, []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	// writeLoop holds the first frame; the rest wait in writeCh.
	waitFor(t, func() bool { return c.queued.Load() == frames && len(c.writeCh) == frames-1 }, "writeLoop taking the first frame")
	time.Sleep(10 * time.Millisecond)
	if n := len(counter.snapshot()) - handshake; n != 0 {
		t.Fatalf("COALESCE_EARLY: %d socket writes before the wait ended", n)
	}
	expireCoalesceWait(t, c, func() bool { return c.queued.Load() == 0 }, "the batch being written")
	writes := counter.snapshot()[handshake:]
	if len(writes) != 1 {
		t.Fatalf("COALESCE_SPLIT: %d socket writes for %d queued frames, want 1", len(writes), frames)
	}
	var fr FrameReader
	r := bytes.NewReader(writes[0])
	for i := range frames {
		token, body, err := fr.Read(r, false)
		if err != nil || token.First != uint64(i+1) || !bytes.Equal(body, []byte{byte(i)}) {
			t.Fatalf("frame %d = %v %x %v", i, token, body, err)
		}
	}
	if r.Len() != 0 {
		t.Fatalf("%d trailing bytes after %d frames", r.Len(), frames)
	}
}

// A waiting connection never writes on its sender's goroutine, even over an
// idle TCP socket where a direct write would otherwise be taken.
func TestCoalesceDelaySkipsDirectWrite(t *testing.T) {
	t.Parallel()
	c, release, frames := dialStalledPeer(t, WithCoalesceDelay(time.Hour, time.Hour))
	if c.raw == nil {
		t.Fatal("precondition: a plaintext TCP connection supports direct writes")
	}
	close(release)
	if enqueued, err := c.SendFrameEagerContext(context.Background(), UID{First: 7}, []byte("eager")); !enqueued || err != nil {
		t.Fatalf("eager send = (%v, %v)", enqueued, err)
	}
	select {
	case got := <-frames:
		t.Fatalf("COALESCE_BYPASSED: the peer read %q before the wait ended", got)
	case <-time.After(50 * time.Millisecond):
	}
	if c.queued.Load() != 1 {
		t.Fatalf("COALESCE_BYPASSED: %d frames queued, want the eager frame", c.queued.Load())
	}
	expireCoalesceWait(t, c, func() bool { return c.queued.Load() == 0 }, "the eager frame being written")
	if got := within(t, frames, "frame after the wait"); string(got) != "7:eager" {
		t.Fatalf("peer read %q", got)
	}
}

// Teardown ends a pending wait without writing, as cancelling connectionWriter
// cancels its delay (FlowTransport.actor.cpp:882-883). Without that, Close would
// wait out the configured delay.
func TestCoalesceDelayEndsOnClose(t *testing.T) {
	t.Parallel()
	c, counter := dialWriteCounted(t, WithCoalesceDelay(time.Hour, time.Hour))
	handshake := len(counter.snapshot())
	sent := make(chan error, 1)
	go func() {
		_, err := c.SendFrameContext(context.Background(), UID{First: 9}, []byte("never"))
		sent <- err
	}()
	// writeLoop has taken the frame: it is waiting, or about to.
	waitFor(t, func() bool { return c.queued.Load() == 1 && len(c.writeCh) == 0 }, "writeLoop taking the frame")
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("COALESCE_CLOSE_BLOCKED: Close waited for the write delay")
	}
	if err := within(t, sent, "sender after Close"); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("sender = %v, want the connection teardown", err)
	}
	if n := len(counter.snapshot()) - handshake; n != 0 {
		t.Fatalf("COALESCE_WROTE_AFTER_CLOSE: %d writes after teardown", n)
	}
}

// D-NG writer with WithCoalesceDelay shares one socket write across frames queued
// during the wait, and teardown cancels a pending wait without writing.
func TestDNGCoalesceDelaySharesOneWriteAndEndsOnClose(t *testing.T) {
	t.Parallel()
	c, counter := dialWriteCountedRaw(t, WithCoalesceDelay(time.Hour, time.Hour))
	if c.na == nil || !c.na.delay {
		t.Fatalf("precondition: expected D-NG writer with delay=true, got na=%v", c.na)
	}
	enteredBatchWait := make(chan struct{})
	var batchWaitOnce sync.Once
	c.na.onCoalesceWait = func() {
		batchWaitOnce.Do(func() { close(enteredBatchWait) })
	}
	handshake := len(counter.snapshot())
	const frames = 4
	for i := range frames {
		if err := c.SendFrameDeferred(UID{First: uint64(i + 1)}, []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	within(t, enteredBatchWait, "naLoop entering coalesceWait for the batch")
	if n := len(counter.snapshot()) - handshake; n != 0 {
		t.Fatalf("DNG_COALESCE_EARLY: %d socket writes before the wait ended", n)
	}
	expireCoalesceWait(t, c, func() bool {
		return len(counter.snapshot()) > handshake
	}, "the D-NG batch being written")
	writes := counter.snapshot()[handshake:]
	if len(writes) != 1 {
		t.Fatalf("DNG_COALESCE_SPLIT: %d socket writes for %d queued frames, want 1", len(writes), frames)
	}
	var fr FrameReader
	r := bytes.NewReader(writes[0])
	for i := range frames {
		token, body, err := fr.Read(r, false)
		if err != nil || token.First != uint64(i+1) || !bytes.Equal(body, []byte{byte(i)}) {
			t.Fatalf("frame %d = %v %x %v", i, token, body, err)
		}
	}
	if r.Len() != 0 {
		t.Fatalf("%d trailing bytes after %d frames", r.Len(), frames)
	}

	// Queue one more frame under the 1-hour delay, wait until naLoop enters
	// coalesceWait, then Close: Close must return promptly without writing.
	enteredCoalesceWait := make(chan struct{})
	var coalesceWaitOnce sync.Once
	c.na.onCoalesceWait = func() {
		coalesceWaitOnce.Do(func() { close(enteredCoalesceWait) })
	}
	beforeCloseWrites := len(counter.snapshot())
	if err := c.SendFrameDeferred(UID{First: 99}, []byte("never")); err != nil {
		t.Fatal(err)
	}
	within(t, enteredCoalesceWait, "naLoop entering coalesceWait before Close")
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("DNG_COALESCE_CLOSE_BLOCKED: Close waited for the write delay")
	}
	if n := len(counter.snapshot()) - beforeCloseWrites; n != 0 {
		t.Fatalf("DNG_COALESCE_WROTE_AFTER_CLOSE: %d writes after teardown", n)
	}
}
