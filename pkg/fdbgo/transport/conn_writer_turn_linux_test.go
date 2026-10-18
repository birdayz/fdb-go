//go:build linux

package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func turnWaiting(c *Conn) bool {
	c.turnMu.Lock()
	defer c.turnMu.Unlock()
	return c.turnCollecting
}

func turnHanded(c *Conn) int {
	c.turnMu.Lock()
	defer c.turnMu.Unlock()
	return len(c.turnFrames)
}

// dialTurn dials a TCP peer that reads every frame, over a connection whose
// hour-long delay a sender still waits out itself; tests end the wait with
// expireCoalesceWait.
func dialTurn(t *testing.T) (*Conn, <-chan []byte) {
	t.Helper()
	c, release, frames := dialStalledPeer(t, WithCoalesceDelay(time.Hour, time.Hour), withTurnDelayLimit(time.Hour))
	close(release)
	return c, frames
}

func frameBody(i uint64) []byte { return []byte(fmt.Sprint("f", i)) }

func sendAsync(c *Conn, i uint64) <-chan error {
	sent := make(chan error, 1)
	go func() {
		_, err := c.SendFrameContext(context.Background(), UID{First: i}, frameBody(i))
		sent <- err
	}()
	return sent
}

// expectFrames checks that the peer receives exactly the frames with the
// given token.First values, in order, and nothing before them.
func expectFrames(t *testing.T, frames <-chan []byte, want ...uint64) {
	t.Helper()
	for _, i := range want {
		if got, w := string(within(t, frames, "a frame")), fmt.Sprint(i, ":f", i); got != w {
			t.Fatalf("peer read %q, want %q", got, w)
		}
	}
}

func expectNoFrame(t *testing.T, frames <-chan []byte, code string) {
	t.Helper()
	select {
	case got := <-frames:
		t.Fatalf("%s: the peer read %q before the wait ended", code, got)
	case <-time.After(20 * time.Millisecond):
	}
}

// A sender on an idle connection waits out the delay itself, and frames sent
// meanwhile join its write in queue order, as everything unsent joins
// connectionWriter's write (FlowTransport.actor.cpp:704-732).
func TestWriterTurnSharesOneWrite(t *testing.T) {
	t.Parallel()
	c, frames := dialTurn(t)
	sent := sendAsync(c, 1)
	waitFor(t, func() bool { return turnWaiting(c) }, "the sender taking the writer's turn")
	const n = 4
	for i := uint64(2); i <= n; i++ {
		if err := c.SendFrameDeferred(UID{First: i}, frameBody(i)); err != nil {
			t.Fatal(err)
		}
	}
	// Every later frame is in the turn's batch, which it writes at once.
	waitFor(t, func() bool { return turnHanded(c) == n-1 }, "writeLoop handing the later frames to the turn")
	expectNoFrame(t, frames, "TURN_EARLY")
	expireCoalesceWait(t, c, func() bool { return c.queued.Load() == 0 }, "the turn's batch being written")
	if err := within(t, sent, "the turn's sender"); err != nil {
		t.Fatal(err)
	}
	expectFrames(t, frames, 1, 2, 3, 4)
	if c.turns.Load() != 1 || c.tails.Load() != 0 {
		t.Fatalf("%d turns and %d tails, want one whole write", c.turns.Load(), c.tails.Load())
	}
}

// A sender never takes the turn from a writeLoop batch that is waiting, so its
// frame joins that batch behind the frame queued before it. The idle check
// against frames still in writeCh is exercised by TestWriterTurnInterleavesSafely,
// whose per-sender order check fails without it.
func TestWriterTurnNeverOvertakesQueuedFrames(t *testing.T) {
	t.Parallel()
	c, frames := dialTurn(t)
	if err := c.SendFrameDeferred(UID{First: 1}, frameBody(1)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return c.queued.Load() == 1 && len(c.writeCh) == 0 }, "writeLoop taking the first frame")
	sent := sendAsync(c, 2)
	waitFor(t, func() bool { return c.queued.Load() == 2 }, "the second frame being queued")
	if c.turns.Load() != 0 {
		t.Fatal("TURN_OVERTOOK: a sender took the turn behind a queued frame")
	}
	expireCoalesceWait(t, c, func() bool { return c.queued.Load() == 0 }, "the batch being written")
	if err := within(t, sent, "the second sender"); err != nil {
		t.Fatal(err)
	}
	expectFrames(t, frames, 1, 2)
}

// Flush waits for the frame of a turn in progress like any queued frame.
func TestWriterTurnFlushWaitsForTheBatch(t *testing.T) {
	t.Parallel()
	c, frames := dialTurn(t)
	sent := sendAsync(c, 1)
	waitFor(t, func() bool { return turnWaiting(c) }, "the sender taking the writer's turn")
	flushed := make(chan error, 1)
	go func() { flushed <- c.FlushContext(context.Background()) }()
	waitFor(t, func() bool { return turnHanded(c) == 1 }, "writeLoop handing the flush marker to the turn")
	select {
	case err := <-flushed:
		t.Fatalf("TURN_FLUSH_EARLY: Flush returned %v before the turn wrote", err)
	case <-time.After(20 * time.Millisecond):
	}
	expireCoalesceWait(t, c, func() bool { return c.queued.Load() == 0 }, "the turn's batch being written")
	if err := within(t, flushed, "Flush"); err != nil {
		t.Fatal(err)
	}
	if err := within(t, sent, "the turn's sender"); err != nil {
		t.Fatal(err)
	}
	expectFrames(t, frames, 1)
}

// Teardown ends a turn's wait without writing and fails the frames handed to
// it, as cancelling connectionWriter cancels its delay
// (FlowTransport.actor.cpp:882-883).
func TestWriterTurnEndsOnClose(t *testing.T) {
	t.Parallel()
	c, frames := dialTurn(t)
	first := sendAsync(c, 1)
	waitFor(t, func() bool { return turnWaiting(c) }, "the sender taking the writer's turn")
	second := sendAsync(c, 2)
	waitFor(t, func() bool { return turnHanded(c) == 1 }, "writeLoop handing the second frame to the turn")
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("TURN_CLOSE_BLOCKED: Close waited for the write delay")
	}
	for _, sent := range []<-chan error{first, second} {
		if err := within(t, sent, "a sender after Close"); !errors.Is(err, ErrConnClosed) {
			t.Fatalf("sender = %v, want the connection teardown", err)
		}
	}
	if got, ok := <-frames; ok {
		t.Fatalf("TURN_WROTE_AFTER_CLOSE: the peer read %q", got)
	}
	if q := c.queued.Load(); q != 0 {
		t.Fatalf("TURN_QUEUE_LEAK: %d frames still counted after teardown", q)
	}
}

// A full socket never holds a turn: as for a direct write, the unwritten tail
// goes to writeLoop, which writes it before any later frame without a new delay.
func TestWriterTurnTailOnFullSocket(t *testing.T) {
	t.Parallel()
	c, release, frames := dialStalledPeer(t, WithCoalesceDelay(DefaultMinCoalesceDelay, DefaultMaxCoalesceDelay))
	big := bytes.Repeat([]byte{'b'}, 32<<20) // beyond loopback's autotuned socket buffers
	sent := make(chan error, 1)
	go func() {
		_, err := c.SendFrameContext(context.Background(), UID{First: 1}, big)
		sent <- err
	}()
	if err := within(t, sent, "a turn into a full socket returning"); err != nil {
		t.Fatal(err)
	}
	if c.turns.Load() != 1 || c.tails.Load() != 1 {
		t.Fatalf("precondition: %d turns and %d tails, want a turn that left a tail", c.turns.Load(), c.tails.Load())
	}
	if err := c.SendFrameDeferred(UID{First: 2}, []byte("after")); err != nil {
		t.Fatal(err)
	}
	close(release)
	if got := within(t, frames, "first frame"); !bytes.Equal(got, append([]byte("1:"), big...)) {
		t.Fatalf("TURN_TAIL: first frame arrived as %d bytes", len(got))
	}
	if got := within(t, frames, "second frame"); string(got) != "2:after" {
		t.Fatalf("TURN_TAIL: second frame %q", got)
	}
}

// Only senders that wait for the write take the turn, and only for a delay
// within maxTurnDelay. An eager frame stays queued so the frames its caller
// sends next can join its batch.
func TestWriterTurnTakers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, release, frames := dialStalledPeer(t, WithCoalesceDelay(DefaultMinCoalesceDelay, DefaultMaxCoalesceDelay))
	close(release)
	if _, err := c.SendFrameContext(ctx, UID{First: 1}, frameBody(1)); err != nil {
		t.Fatal(err)
	}
	if enqueued, err := c.SendFrameAloneContext(ctx, UID{First: 2}, frameBody(2)); !enqueued || err != nil {
		t.Fatalf("alone send = (%v, %v)", enqueued, err)
	}
	if err := c.SendFrame(UID{First: 3}, frameBody(3)); err != nil {
		t.Fatal(err)
	}
	if n := c.turns.Load(); n != 3 {
		t.Fatalf("TURN_NOT_TAKEN: %d turns for three waiting senders on an idle connection, want 3", n)
	}
	if enqueued, err := c.SendFrameEagerContext(ctx, UID{First: 4}, frameBody(4)); !enqueued || err != nil {
		t.Fatalf("eager send = (%v, %v)", enqueued, err)
	}
	expectFrames(t, frames, 1, 2, 3, 4)
	if n := c.turns.Load(); n != 3 {
		t.Fatalf("TURN_EAGER: an eager send took the turn (%d turns)", n)
	}

	long, longRelease, _ := dialStalledPeer(t, WithCoalesceDelay(time.Hour, time.Hour))
	close(longRelease)
	sent := make(chan error, 1)
	go func() {
		_, err := long.SendFrameContext(ctx, UID{First: 5}, frameBody(5))
		sent <- err
	}()
	waitFor(t, func() bool { return long.queued.Load() == 1 && len(long.writeCh) == 0 }, "writeLoop taking the frame")
	if long.turns.Load() != 0 {
		t.Fatal("TURN_UNBOUNDED: a sender took the turn for an hour-long delay")
	}
	_ = long.Close()
	if err := within(t, sent, "the sender after Close"); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("sender = %v, want the connection teardown", err)
	}
}

// Turns, direct writes and writeLoop batches share one socket safely.
func TestWriterTurnInterleavesSafely(t *testing.T) {
	t.Parallel()
	checkWritersInterleave(t, WithCoalesceDelay(DefaultMinCoalesceDelay, DefaultMaxCoalesceDelay))
}
