package transport

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// TestDial_HandshakeStallTimesOut pins the fix for the load-dependent connection
// deadlock: a peer that accepts the TCP socket and reads our ConnectPacket but never
// sends its own would otherwise block ReadConnectPacket's io.ReadFull forever (ctx
// cancellation does not interrupt a blocking socket read). Because dialWith runs
// under the database's global connection lock, that single wedged handshake froze
// every connection acquisition — the chaos-test deadlock. dialWith now bounds the
// handshake with a deadline, so a stalled peer fails the dial promptly instead.
//
// Pre-fix this test HANGS (no deadline) and only dies via the Go test timeout;
// post-fix it returns a timeout error well within the bound.
func TestDial_HandshakeStallTimesOut(t *testing.T) {
	t.Parallel()

	cli, srv := net.Pipe()
	// Server: drain whatever the client writes (so WriteConnectPacket completes on
	// the synchronous pipe) but NEVER reply — simulating an accept-but-stall peer.
	go func() { _, _ = io.Copy(io.Discard, srv) }()
	t.Cleanup(func() { srv.Close() })

	dialFn := func(_ context.Context, _, _ string) (net.Conn, error) { return cli, nil }

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	start := time.Now()
	c, err := dialWith(ctx, "stall", dialFn, nil, withMonitorCadence(time.Hour, time.Hour))
	elapsed := time.Since(start)

	if err == nil {
		c.Close()
		t.Fatal("handshake against a stalled peer must fail, not succeed")
	}
	// Must be bounded by the deadline (with generous slack), not a forever-hang.
	if elapsed > 5*time.Second {
		t.Fatalf("handshake not bounded by a deadline: dial took %v", elapsed)
	}
}

// TestDial_HandshakeHonorsCancellation pins that ctx *cancellation* (not just a
// deadline) aborts the handshake promptly. A cancel-only ctx (no deadline) against an
// accept-but-stall peer must fail as soon as cancel() fires. Pre-this-fix the handshake
// was bounded only by a deadline, so a cancel-only ctx derived no deadline from ctx and
// waited the full defaultHandshakeTimeout (10s) before the fallback deadline fired —
// cancel() did nothing. The cancellation watcher now pushes a past deadline onto the
// socket on ctx.Done(), unblocking the in-flight ConnectPacket read immediately.
func TestDial_HandshakeHonorsCancellation(t *testing.T) {
	t.Parallel()

	cli, srv := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, srv) }()
	t.Cleanup(func() { srv.Close() })
	dialFn := func(_ context.Context, _, _ string) (net.Conn, error) { return cli, nil }

	// Cancel-only ctx: NO deadline. Cancel shortly after the dial enters the handshake.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	c, err := dialWith(ctx, "stall", dialFn, nil, withMonitorCadence(time.Hour, time.Hour))
	elapsed := time.Since(start)
	if err == nil {
		c.Close()
		t.Fatal("handshake must fail when ctx is cancelled mid-handshake")
	}
	// Must abort on cancellation (~200ms), far below defaultHandshakeTimeout (10s).
	// Pre-fix this waits the full 10s default (a cancel-only ctx sets no shorter bound).
	if elapsed > 3*time.Second {
		t.Fatalf("handshake did not honor ctx cancellation: took %v (expected ~200ms)", elapsed)
	}
}

// TestDial_HandshakeDeadlineFromCtx confirms the handshake bound tracks the caller's
// ctx deadline (so an explicit, tight dial timeout shortens the handshake too) rather
// than always waiting the full defaultHandshakeTimeout.
func TestDial_HandshakeDeadlineFromCtx(t *testing.T) {
	t.Parallel()

	cli, srv := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, srv) }()
	t.Cleanup(func() { srv.Close() })
	dialFn := func(_ context.Context, _, _ string) (net.Conn, error) { return cli, nil }

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	start := time.Now()
	c, err := dialWith(ctx, "stall", dialFn, nil, withMonitorCadence(time.Hour, time.Hour))
	elapsed := time.Since(start)
	if err == nil {
		c.Close()
		t.Fatal("expected ctx-bounded handshake to fail")
	}
	// The tight 250ms ctx deadline must dominate the 10s default — allow slack but
	// it must be far under the default ceiling.
	if elapsed > 3*time.Second {
		t.Fatalf("handshake did not honor the ctx deadline: took %v (expected ~250ms)", elapsed)
	}
}
