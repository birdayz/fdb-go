//go:build linux

package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
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
	c, release, frames := dialStalledPeer(t, WithCoalesceDelay(time.Hour, time.Hour), withTurnDelayLimit(2*time.Hour))
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

// A flush marker handed to a turn that leaves a tail waits for the tail: Flush
// promises that every earlier byte is written.
func TestWriterTurnFlushWaitsForTheTail(t *testing.T) {
	t.Parallel()
	c, release, frames := dialStalledPeer(t, WithCoalesceDelay(time.Hour, time.Hour), withTurnDelayLimit(2*time.Hour))
	big := bytes.Repeat([]byte{'b'}, 32<<20) // beyond loopback's autotuned socket buffers
	sent := make(chan error, 1)
	go func() {
		_, err := c.SendFrameContext(context.Background(), UID{First: 1}, big)
		sent <- err
	}()
	waitFor(t, func() bool { return turnWaiting(c) }, "the sender taking the writer's turn")
	flushed := make(chan error, 1)
	go func() { flushed <- c.FlushContext(context.Background()) }()
	waitFor(t, func() bool { return turnHanded(c) == 1 }, "writeLoop handing the flush marker to the turn")
	expireCoalesceWait(t, c, func() bool { return c.tails.Load() == 1 }, "the turn leaving a tail")
	if err := within(t, sent, "the turn's sender"); err != nil {
		t.Fatal(err)
	}
	// The marker stays with the tail rather than re-entering writeCh, which can
	// be full while writeLoop is stuck writing the tail: fill it and check that
	// Flush still waits for the tail.
	c.turnMu.Lock()
	left := len(c.tailMarkers)
	c.turnMu.Unlock()
	if left != 1 {
		t.Fatalf("TURN_MARKER_REQUEUED: %d markers left with the tail, want 1", left)
	}
	for i := range cap(c.writeCh) {
		if err := c.SendFrameDeferred(UID{First: uint64(100 + i)}, []byte("q")); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-flushed:
		t.Fatalf("TURN_FLUSH_BEFORE_TAIL: Flush returned %v with the tail unwritten", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	read := make(chan error, 1)
	go func() {
		if got := <-frames; !bytes.Equal(got, append([]byte("1:"), big...)) {
			read <- fmt.Errorf("first frame arrived as %d bytes", len(got))
			return
		}
		for i := range cap(c.writeCh) {
			if got, want := string(<-frames), fmt.Sprint(100+i, ":q"); got != want {
				read <- fmt.Errorf("peer read %q, want %q", got, want)
				return
			}
		}
		read <- nil
	}()
	// Batches after the tail wait the hour-long delay; end each one.
	expireCoalesceWait(t, c, func() bool { return len(flushed) == 1 && len(read) == 1 }, "Flush and every frame after the tail")
	if err := <-flushed; err != nil {
		t.Fatal(err)
	}
	if err := <-read; err != nil {
		t.Fatal(err)
	}
	if q := c.queued.Load(); q != 0 {
		t.Fatalf("%d frames still counted", q)
	}
}

// A flush marker returned behind a turn's tail fails when the connection fails
// before the tail is written, instead of having reported success.
func TestWriterTurnFlushFailsWithTheTail(t *testing.T) {
	t.Parallel()
	c, _, _ := dialStalledPeer(t, WithCoalesceDelay(time.Hour, time.Hour), withTurnDelayLimit(2*time.Hour))
	big := bytes.Repeat([]byte{'b'}, 32<<20)
	sent := make(chan error, 1)
	go func() {
		_, err := c.SendFrameContext(context.Background(), UID{First: 1}, big)
		sent <- err
	}()
	waitFor(t, func() bool { return turnWaiting(c) }, "the sender taking the writer's turn")
	flushed := make(chan error, 1)
	go func() { flushed <- c.FlushContext(context.Background()) }()
	waitFor(t, func() bool { return turnHanded(c) == 1 }, "writeLoop handing the flush marker to the turn")
	expireCoalesceWait(t, c, func() bool { return c.tails.Load() == 1 }, "the turn leaving a tail")
	if err := within(t, sent, "the turn's sender"); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if err := within(t, flushed, "Flush after Close"); err == nil {
		t.Fatal("TURN_FLUSH_BEFORE_TAIL: Flush reported success for bytes that were never written")
	}
}

// A turn ends its collection when its wait ends while other senders keep
// queueing. writeLoop hands over only what was queued when it woke, which
// bounds each handoff by construction; this test checks liveness and does not
// reliably fail without that bound.
func TestWriterTurnEndsUnderContinuousProducers(t *testing.T) {
	t.Parallel()
	c, _ := dialTurn(t)
	sent := sendAsync(c, 1)
	waitFor(t, func() bool { return turnWaiting(c) }, "the sender taking the writer's turn")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if c.SendFrameDeferred(UID{First: 2}, frameBody(2)) != nil {
					return
				}
			}
		}()
	}
	defer func() {
		close(stop)
		_ = c.Close()
		wg.Wait()
	}()
	waitFor(t, func() bool { return turnHanded(c) > 1000 }, "producers filling the turn")
	expireCoalesceWait(t, c, func() bool { return len(sent) == 1 }, "the turn writing under continuous producers")
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
}

// A sender's wait is bounded by both delay bounds, jitter included.
func TestWriterTurnDelayBound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		minDelay, maxDelay time.Duration
		takes              bool
	}{
		{DefaultMinCoalesceDelay, DefaultMaxCoalesceDelay, true},
		{900 * time.Microsecond, 900 * time.Microsecond, true}, // 990 µs with jitter
		{time.Millisecond, time.Millisecond, false},            // 1.1 ms with jitter
		{time.Hour, 0, false},                                  // min above max
		{0, time.Hour, false},
		{time.Duration(math.MaxInt64), 0, false}, // 1.1× does not wrap negative
		{0, time.Duration(math.MaxInt64), false},
	} {
		got := turnWaitWithin(tc.minDelay, tc.maxDelay, maxTurnDelay)
		if got != tc.takes {
			t.Errorf("TURN_BOUND: delay (%v, %v) takes a turn = %v, want %v", tc.minDelay, tc.maxDelay, got, tc.takes)
		}
	}
	c, release, _ := dialStalledPeer(t, WithCoalesceDelay(time.Hour, 0))
	close(release)
	sent := make(chan error, 1)
	go func() {
		_, err := c.SendFrameContext(context.Background(), UID{First: 1}, frameBody(1))
		sent <- err
	}()
	waitFor(t, func() bool { return c.queued.Load() == 1 && len(c.writeCh) == 0 }, "writeLoop taking the frame")
	if c.turns.Load() != 0 {
		t.Fatal("TURN_UNBOUNDED: a sender took the turn for an hour-long minimum delay")
	}
	_ = c.Close()
	if err := within(t, sent, "the sender after Close"); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("sender = %v, want the connection teardown", err)
	}
}

// Only senders that wait for the write take the turn, and only for a delay
// within maxTurnDelay. A frame whose sender does not wait (eager or deferred)
// stays queued, as libfdb_c's sender never waits for connectionWriter, so the
// frames its caller sends next can join its batch.
func TestWriterTurnTakers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, release, frames := dialStalledPeer(t, WithCoalesceDelay(DefaultMinCoalesceDelay, DefaultMaxCoalesceDelay))
	close(release)
	if _, err := c.SendFrameContext(ctx, UID{First: 1}, frameBody(1)); err != nil {
		t.Fatal(err)
	}
	if err := c.SendFrame(UID{First: 2}, frameBody(2)); err != nil {
		t.Fatal(err)
	}
	if n := c.turns.Load(); n != 2 {
		t.Fatalf("TURN_NOT_TAKEN: %d turns for two waiting senders on an idle connection, want 2", n)
	}
	if enqueued, err := c.SendFrameEagerContext(ctx, UID{First: 3}, frameBody(3)); !enqueued || err != nil {
		t.Fatalf("eager send = (%v, %v)", enqueued, err)
	}
	if enqueued, err := c.SendFrameDeferredContext(ctx, UID{First: 4}, frameBody(4)); !enqueued || err != nil {
		t.Fatalf("deferred send = (%v, %v)", enqueued, err)
	}
	expectFrames(t, frames, 1, 2, 3, 4)
	if n := c.turns.Load(); n != 2 {
		t.Fatalf("TURN_NOT_WAITING: a send that does not wait took the turn (%d turns)", n)
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
