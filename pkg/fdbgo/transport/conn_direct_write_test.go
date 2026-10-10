package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

// tcpPair returns a connected loopback pair; direct writes need a real socket.
func tcpPair(t *testing.T) (*net.TCPConn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	srv := <-accepted
	if srv == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { cli.Close(); srv.Close() })
	return cli.(*net.TCPConn), srv
}

// directConn has no writeLoop: a frame queued instead of written by its
// caller stays in writeCh.
func directConn(t *testing.T, nc *net.TCPConn) *Conn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Conn{
		conn: nc, ctx: ctx, cancel: cancel, writeCh: make(chan writeReq, 8),
		pending: make(map[UID]chan Response), raw: newRawWriter(nc), tailKick: make(chan struct{}, 1),
	}
}

func within[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not happen", what)
		var zero T
		return zero
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func readFrame(t *testing.T, r net.Conn) (UID, []byte) {
	t.Helper()
	_ = r.SetReadDeadline(time.Now().Add(10 * time.Second))
	var fr FrameReader
	token, body, err := fr.Read(r, false)
	if err != nil {
		t.Fatalf("peer read: %v", err)
	}
	return token, body
}

// An idle writer lets the caller write its own frame; the round trip through
// writeLoop and back cost two goroutine handoffs per request.
func TestSendWritesDirectlyWhenWriterIdle(t *testing.T) {
	t.Parallel()
	cli, srv := tcpPair(t)
	c := directConn(t, cli)
	sends := []func() error{
		func() error { return c.SendFrame(UID{First: 1}, []byte("commit")) },
		func() error {
			_, err := c.SendFrameContext(context.Background(), UID{First: 2}, []byte("read"))
			return err
		},
	}
	for i, send := range sends {
		done := make(chan error, 1)
		go func() { done <- send() }()
		if err := within(t, done, fmt.Sprintf("send %d without a writeLoop", i)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		if token, _ := readFrame(t, srv); token.First != uint64(i+1) {
			t.Fatalf("send %d: peer got token %d", i, token.First)
		}
	}
	if n := len(c.writeCh); n != 0 || c.queued.Load() != 0 {
		t.Fatalf("WRITE_DIRECT: %d frames queued (counter %d), want both written by their callers", n, c.queued.Load())
	}
}

// A caller must not write ahead of a frame already handed to writeLoop.
func TestDirectWriteNeverOvertakesQueuedFrame(t *testing.T) {
	t.Parallel()
	cli, _ := tcpPair(t)
	c := directConn(t, cli)
	if err := c.SendFrameDeferred(UID{First: 1}, []byte("first")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.SendFrame(UID{First: 2}, []byte("second")) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(c.writeCh) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("WRITE_REORDER: second frame bypassed the queue")
		}
		time.Sleep(time.Millisecond)
	}
	if first, second := <-c.writeCh, <-c.writeCh; first.token.First != 1 || second.token.First != 2 {
		t.Fatalf("queue order = %d, %d", first.token.First, second.token.First)
	}
	c.cancel()
	if err := within(t, done, "stranded queued sender"); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("stranded queued sender returned %v", err)
	}
}

// With nothing queued every earlier frame is on the socket, so a flush on a
// live connection has nothing to wait for; on a dead one it still fails.
func TestFlushWithNothingQueued(t *testing.T) {
	t.Parallel()
	cli, _ := tcpPair(t)
	c := directConn(t, cli)
	done := make(chan error, 2)
	go func() { done <- c.Flush() }()
	go func() { done <- c.FlushContext(context.Background()) }()
	for range 2 {
		if err := within(t, done, "flush with nothing queued"); err != nil {
			t.Fatal(err)
		}
	}
	c.cancel()
	if err := c.Flush(); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("Flush on a dead connection = %v, want the teardown", err)
	}
	if err := c.FlushContext(context.Background()); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("FlushContext on a dead connection = %v, want the teardown", err)
	}
}

// A failed direct write fails the connection exactly as a failed writeLoop write.
func TestDirectWriteFailureFailsConnection(t *testing.T) {
	t.Parallel()
	cli, _ := tcpPair(t)
	c := directConn(t, cli)
	_, replies, handle := c.PrepareReply()
	defer handle.Release()
	cli.Close() // the next socket write fails
	err := c.SendFrame(UID{First: 1}, []byte("x"))
	var fe *wire.FDBError
	if !errors.Is(err, ErrConnClosed) || !errors.As(err, &fe) || fe.Code != codeRequestMaybeDelivered {
		t.Fatalf("SendFrame = %v, want the coded request_maybe_delivered teardown", err)
	}
	if !c.IsClosed() {
		t.Fatal("failed direct write left the connection open")
	}
	if r := within(t, replies, "pending reply"); !errors.Is(r.Err, ErrConnClosed) {
		t.Fatalf("pending reply = %v, want the coded teardown", r.Err)
	}
}

// dialStalledPeer dials a peer that completes the handshake and then reads
// nothing until release is closed; frames it later reads arrive on frames.
func dialStalledPeer(t *testing.T) (c *Conn, release chan struct{}, frames <-chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	release = make(chan struct{})
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	out := make(chan []byte, 64)
	go func() {
		srv, err := ln.Accept()
		if err != nil {
			return
		}
		defer srv.Close()
		var buf [ConnectPacketSize]byte
		if _, err := io.ReadFull(srv, buf[:]); err != nil {
			return
		}
		sp := ConnectPacket{ProtocolVersion: ProtocolVersion73, ConnectionID: 0x5151}
		if _, err := srv.Write(sp.Marshal()); err != nil {
			return
		}
		select {
		case <-release:
		case <-stop:
			return
		}
		var fr FrameReader
		for {
			token, body, err := fr.Read(srv, false)
			if err != nil {
				close(out)
				return
			}
			out <- append([]byte(fmt.Sprint(token.First, ":")), body...)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err = dialWith(ctx, ln.Addr().String(), nil, nil, withMonitorCadence(time.Hour, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, release, out
}

// A full socket never holds a direct writer: the unwritten tail goes to
// writeLoop, later senders queue behind it and can still give up on their ctx,
// and the peer receives every frame whole and in order.
func TestDirectWriteTailOnFullSocket(t *testing.T) {
	t.Parallel()
	c, release, frames := dialStalledPeer(t)
	big := bytes.Repeat([]byte{'b'}, 32<<20) // beyond loopback's autotuned socket buffers
	sent := make(chan error, 1)
	go func() {
		_, err := c.SendFrameContext(context.Background(), UID{First: 1}, big)
		sent <- err
	}()
	if err := within(t, sent, "direct write into a full socket returning"); err != nil {
		t.Fatal(err)
	}
	if c.queued.Load() == 0 {
		t.Fatal("precondition: the socket took a 32 MiB frame whole")
	}
	// Once writeLoop has taken the kick it is stuck writing the tail, so a
	// frame queued now stays in writeCh until the peer reads.
	waitFor(t, func() bool { return len(c.tailKick) == 0 }, "writeLoop taking the tail")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		enqueued bool
		err      error
	}
	abandoned := make(chan outcome, 1)
	go func() {
		enqueued, err := c.SendFrameContext(ctx, UID{First: 2}, []byte("after"))
		abandoned <- outcome{enqueued, err}
	}()
	waitFor(t, func() bool { return len(c.writeCh) == 1 }, "second frame queued behind the tail")
	cancel()
	if r := within(t, abandoned, "queued sender honouring its ctx"); !r.enqueued || !errors.Is(r.err, context.Canceled) {
		t.Fatalf("queued sender behind a full socket = (%v, %v), want (true, context.Canceled)", r.enqueued, r.err)
	}
	close(release)
	if got := within(t, frames, "first frame"); !bytes.Equal(got, append([]byte("1:"), big...)) {
		t.Fatalf("WRITE_TAIL: first frame arrived as %d bytes", len(got))
	}
	if got := within(t, frames, "second frame"); string(got) != "2:after" {
		t.Fatalf("WRITE_TAIL: second frame %q", got)
	}
}

// Direct writers and writeLoop share one socket: every frame arrives whole and
// each sender's frames in its own order. Run under -race.
func TestDirectAndQueuedWritersInterleaveSafely(t *testing.T) {
	t.Parallel()
	c, release, frames := dialStalledPeer(t)
	close(release)
	const senders, perSender = 8, 50
	got := make(chan error, 1)
	go func() {
		next := make([]int, senders)
		for range senders * perSender {
			f, ok := <-frames
			if !ok {
				got <- errors.New("peer stream ended")
				return
			}
			var w, seq int
			if _, err := fmt.Sscanf(string(f), "%d:%d/%d", &w, &w, &seq); err != nil || seq != next[w] {
				got <- fmt.Errorf("frame %q: sender %d expected seq %d (%v)", f, w, next[w], err)
				return
			}
			next[w]++
		}
		got <- nil
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for w := range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seq := range perSender {
				token, body := UID{First: uint64(w)}, []byte(fmt.Sprint(w, "/", seq))
				var err error
				switch seq % 3 {
				case 0:
					err = c.SendFrame(token, body)
				case 1:
					_, err = c.SendFrameContext(ctx, token, body)
				default:
					err = c.SendFrameDeferred(token, body)
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := within(t, got, "peer receiving every frame"); err != nil {
		_ = c.Close()
		t.Fatal(err)
	}
}

// A fixed 32 KiB send buffer makes every large direct write partial, so tails,
// leftover kicks and tail-blocked direct writers all occur under -race.
func TestDirectWriteTailsUnderConcurrency(t *testing.T) {
	t.Parallel()
	c, release, frames := dialStalledPeer(t)
	if err := c.conn.(*net.TCPConn).SetWriteBuffer(32 << 10); err != nil {
		t.Fatal(err)
	}
	close(release)
	const senders, perSender = 4, 12
	sizes := []int{1 << 20, 1 << 10, 64 << 10, 300 << 10}
	got := make(chan error, 1)
	go func() {
		next := make([]int, senders)
		for range senders * perSender {
			f, ok := <-frames
			if !ok {
				got <- errors.New("peer stream ended")
				return
			}
			var w, seq int
			if _, err := fmt.Sscanf(string(f), "%d:%d/%d/", &w, &w, &seq); err != nil || seq != next[w] {
				got <- fmt.Errorf("frame from sender %d: seq %d, want %d (%v)", w, seq, next[w], err)
				return
			}
			want := len(fmt.Sprint(w, "/", seq, "/")) + sizes[seq%len(sizes)]
			if body := f[bytes.IndexByte(f, ':')+1:]; len(body) != want {
				got <- fmt.Errorf("sender %d frame %d: %d bytes, want %d", w, seq, len(body), want)
				return
			}
			next[w]++
		}
		got <- nil
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	send := func(w, seq int) error {
		body := append([]byte(fmt.Sprint(w, "/", seq, "/")), make([]byte, sizes[seq%len(sizes)])...)
		token := UID{First: uint64(w)}
		var err error
		switch seq % 3 {
		case 0:
			err = c.SendFrame(token, body)
		case 1:
			_, err = c.SendFrameContext(ctx, token, body)
		default:
			err = c.SendFrameDeferred(token, body)
		}
		return err
	}
	// On the idle connection this 1 MiB frame goes direct and cannot fit the
	// 64 KiB send buffer, so at least one tail is certain; the rest race it.
	if err := send(0, 0); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for w := range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seq := range perSender {
				if w == 0 && seq == 0 {
					continue
				}
				if err := send(w, seq); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := within(t, got, "peer receiving every frame"); err != nil {
		t.Fatal(err)
	}
	if c.tails.Load() == 0 {
		t.Fatal("WRITE_TAIL: the idle 1 MiB direct write was not partial")
	}
}
