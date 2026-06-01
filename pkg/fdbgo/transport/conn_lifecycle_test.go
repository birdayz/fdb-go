package transport

// Deterministic, in-process connection-lifecycle tests (RFC-050 / RFC-010 #6).
//
// A net.Pipe()-based fake server does the ConnectPacket handshake, then runs in
// a test-controlled mode (respond to PINGs, go silent, stall reads, or close
// abruptly). No Docker, no real sockets, no wall-clock waits beyond short
// bounded deadlines. The two core tests FAIL (hang / leak) on the pre-fix code
// and pass after; the rest guard the single-failConnection refactor under -race.

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// simServer is an in-process fake FDB server over net.Pipe. The client dials the
// `cli` end via dialFunc(); the test drives the `srv` end. stop is closed on test
// cleanup so a "stalled" server goroutine (which deliberately never reads) exits.
type simServer struct {
	cli, srv net.Conn
	stop     chan struct{}
}

func newSimServer(t *testing.T) *simServer {
	cli, srv := net.Pipe()
	s := &simServer{cli: cli, srv: srv, stop: make(chan struct{})}
	t.Cleanup(func() {
		close(s.stop)
		s.srv.Close() // idempotent on net.Pipe; unblocks any server read
		s.cli.Close()
	})
	return s
}

// dialFunc returns a transport.DialFunc that hands the client the pipe's cli end.
func (s *simServer) dialFunc() DialFunc {
	return func(context.Context, string, string) (net.Conn, error) { return s.cli, nil }
}

// handshake performs the server side of the ConnectPacket exchange.
func (s *simServer) handshake() error {
	var buf [ConnectPacketSize]byte
	if _, err := io.ReadFull(s.srv, buf[:]); err != nil {
		return err
	}
	sp := ConnectPacket{ProtocolVersion: ProtocolVersion73, ConnectionID: 0x5151}
	_, err := s.srv.Write(sp.Marshal())
	return err
}

// drainUntilClosed reads and discards everything the client sends (e.g. PINGs)
// and returns when the client closes its end (Read errors). Used by the
// "go silent" mode: the server stays alive but never sends bytes back, so the
// client's connection monitor sees frozen bytesReceived and must kill the conn.
func (s *simServer) drainUntilClosed() {
	buf := make([]byte, 4096)
	for {
		if _, err := s.srv.Read(buf); err != nil {
			return
		}
	}
}

// awaitWithin fails the test if done has not fired within d.
func awaitWithin(t *testing.T, d time.Duration, done <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal(msg)
	}
}

// TestConn_CloseUnblocksStrandedSendFrame is the Bug 1 proof. A frame is left in
// writeCh while writeLoop is blocked flushing an earlier frame to a stalled
// server. On Close, the pre-fix SendFrame (no ctx.Done arm) blocks on <-errCh
// forever because writeLoop exits on ctx.Done without notifying the queued
// sender. With the fix, ctx cancellation unblocks it. FAILS (times out) on the
// pre-fix code.
func TestConn_CloseUnblocksStrandedSendFrame(t *testing.T) {
	t.Parallel()
	s := newSimServer(t)
	go func() {
		_ = s.handshake()
		// Stall: never read again, so the client's first post-handshake flush
		// blocks on the synchronous pipe and writeLoop gets stuck there.
		<-s.stop
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Long monitor cadence so the monitor never pings/interferes during the test.
	c, err := dialWith(ctx, "sim", false, s.dialFunc(), nil, withMonitorCadence(time.Hour, time.Hour))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	tok := UID{First: 1, Second: 2}

	// Sender A: writeLoop dequeues it and blocks flushing to the stalled server.
	aDone := make(chan struct{})
	go func() { _ = c.SendFrame(tok, []byte("A")); close(aDone) }()
	time.Sleep(50 * time.Millisecond) // let writeLoop reach the blocked Flush

	// Sender B: enqueues into writeCh behind the blocked writeLoop — this is the
	// frame that strands on the pre-fix code.
	bDone := make(chan struct{})
	go func() { _ = c.SendFrame(tok, []byte("B")); close(bDone) }()
	time.Sleep(50 * time.Millisecond) // ensure B is enqueued

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	awaitWithin(t, 3*time.Second, aDone, "SendFrame A stranded after Close (writeLoop never notified it)")
	awaitWithin(t, 3*time.Second, bDone, "SendFrame B stranded after Close — Bug 1: queued sender hangs on <-errCh forever")
}

// TestConn_CloseUnblocksStrandedFlush is the Flush analog of the above.
func TestConn_CloseUnblocksStrandedFlush(t *testing.T) {
	t.Parallel()
	s := newSimServer(t)
	go func() {
		_ = s.handshake()
		<-s.stop // stall: never read, exit on cleanup
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialWith(ctx, "sim", false, s.dialFunc(), nil, withMonitorCadence(time.Hour, time.Hour))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Deferred frame marks the conn dirty so Flush actually synchronizes; the
	// flush then blocks on the stalled server. A second Flush queues behind it.
	_ = c.SendFrameDeferred(UID{First: 9}, []byte("x"))
	fDone := make(chan struct{})
	go func() { _ = c.Flush(); close(fDone) }()
	time.Sleep(50 * time.Millisecond)

	_ = c.Close()
	awaitWithin(t, 3*time.Second, fDone, "Flush stranded after Close — Bug 1")
}

// TestConn_MonitorDeathClosesSocket is the Bug 2 proof. The server completes the
// handshake then goes silent (drains the client's PINGs, sends nothing). The
// connection monitor sees frozen bytesReceived and must declare the conn dead.
// The fix routes that through failConnection, which CLOSES THE SOCKET — so the
// server observes EOF. The pre-fix monitor only called cancel(), never closed
// the socket, so readLoop stayed blocked in Read and the fd + goroutine leaked:
// the server never sees EOF and this times out.
func TestConn_MonitorDeathClosesSocket(t *testing.T) {
	t.Parallel()
	s := newSimServer(t)
	serverSawClose := make(chan struct{})
	go func() {
		if err := s.handshake(); err != nil {
			return
		}
		s.drainUntilClosed() // returns only when the client closes its socket
		close(serverSawClose)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Tiny cadence: monitor fires in ~ (loop + loop + timeout) = 60ms.
	c, err := dialWith(ctx, "sim", false, s.dialFunc(), nil, withMonitorCadence(20*time.Millisecond, 20*time.Millisecond))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close() // cleanup AFTER the assertion (Close would mask the bug)

	// Issue a request so the monitor has pending work and actually pings. It
	// returns (errored) once the monitor kills the conn.
	reqDone := make(chan struct{})
	go func() { _, _ = c.SendAndWait(ctx, UID{First: 7}, []byte("req")); close(reqDone) }()

	awaitWithin(t, 2*time.Second, serverSawClose,
		"monitor declared the conn dead but never closed the socket — Bug 2: fd + readLoop goroutine leak")
	awaitWithin(t, 2*time.Second, reqDone, "pending request not failed after monitor death")
	if !c.IsClosed() {
		t.Error("IsClosed() should be true after monitor death")
	}
}

// TestConn_AbruptServerCloseFailsPending guards the readLoop error path now
// routed through failConnection: a pending request is failed and Close is clean.
func TestConn_AbruptServerCloseFailsPending(t *testing.T) {
	t.Parallel()
	s := newSimServer(t)
	go func() {
		if err := s.handshake(); err != nil {
			return
		}
		// Read the request frame, then close abruptly without replying.
		var fr FrameReader
		_, _, _ = fr.Read(s.srv, false)
		s.srv.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialWith(ctx, "sim", false, s.dialFunc(), nil, withMonitorCadence(time.Hour, time.Hour))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	_, rerr := c.SendAndWait(ctx, UID{First: 3}, []byte("req"))
	if rerr == nil {
		t.Fatal("SendAndWait should fail when the server closes mid-RPC")
	}
	// Close must return promptly (readLoop already exited via failConnection).
	closed := make(chan struct{})
	go func() { _ = c.Close(); close(closed) }()
	awaitWithin(t, 3*time.Second, closed, "Close hung after abrupt server close")
}

// TestConn_FailConnectionIdempotent races the three real teardown triggers —
// readLoop error (abrupt server close), monitor death, and an explicit Close() —
// and asserts no panic, exactly-once teardown, and a clean loopWG drain. -race.
func TestConn_FailConnectionIdempotent(t *testing.T) {
	t.Parallel()
	for i := 0; i < 50; i++ {
		s := newSimServer(t)
		go func() {
			if err := s.handshake(); err != nil {
				return
			}
			// Drain briefly, then close abruptly — racing the monitor + Close.
			buf := make([]byte, 4096)
			_, _ = s.srv.Read(buf)
			s.srv.Close()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		c, err := dialWith(ctx, "sim", false, s.dialFunc(), nil, withMonitorCadence(time.Millisecond, time.Millisecond))
		if err != nil {
			cancel()
			t.Fatalf("dial: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); _, _ = c.SendAndWait(ctx, UID{First: 1}, []byte("a")) }()
		go func() { defer wg.Done(); _ = c.Close() }()
		go func() { defer wg.Done(); _ = c.Close() }() // concurrent double Close
		wg.Wait()

		// A final Close must return promptly — proves loopWG fully drained.
		closed := make(chan struct{})
		go func() { _ = c.Close(); close(closed) }()
		awaitWithin(t, 3*time.Second, closed, "Close hung — teardown left a goroutine alive")
		cancel()
	}
}

// TestConn_StrandedSenderStress hammers the Close-vs-SendFrame race so the errCh
// pool path (ctx.Done arm, no pool-return on that arm — audit #13) is exercised
// repeatedly under -race. Every SendFrame must return; none may hang or panic.
func TestConn_StrandedSenderStress(t *testing.T) {
	t.Parallel()
	for i := 0; i < 30; i++ {
		s := newSimServer(t)
		go func() {
			_ = s.handshake()
			<-s.stop // stall
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		c, err := dialWith(ctx, "sim", false, s.dialFunc(), nil, withMonitorCadence(time.Hour, time.Hour))
		if err != nil {
			cancel()
			t.Fatalf("dial: %v", err)
		}

		var wg sync.WaitGroup
		const senders = 8
		for j := 0; j < senders; j++ {
			wg.Add(1)
			go func() { defer wg.Done(); _ = c.SendFrame(UID{First: uint64(j)}, []byte("x")) }()
		}
		time.Sleep(time.Millisecond) // let some pile into writeCh
		_ = c.Close()

		allReturned := make(chan struct{})
		go func() { wg.Wait(); close(allReturned) }()
		awaitWithin(t, 3*time.Second, allReturned, "a SendFrame stranded under Close race")
		cancel()
	}
}
