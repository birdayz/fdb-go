package transport

import (
	"bytes"
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type readCountingConn struct {
	net.Conn
	reads atomic.Int32
}

func (c *readCountingConn) Read(p []byte) (int, error) {
	c.reads.Add(1)
	return c.Conn.Read(p)
}

func dialCounted(t *testing.T, s *simServer) (*Conn, *readCountingConn) {
	t.Helper()
	counted := &readCountingConn{Conn: s.cli}
	handshook := make(chan error, 1)
	go func() { handshook <- s.handshake() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dial := func(context.Context, string, string) (net.Conn, error) { return counted, nil }
	c, err := dialWith(ctx, "sim", dial, nil, withMonitorCadence(time.Hour, time.Hour))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := <-handshook; err != nil {
		t.Fatalf("handshake: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, counted
}

// sendReplies writes one frame per body to the client in a single socket write.
func sendReplies(t *testing.T, s *simServer, tokens []UID, bodies [][]byte) {
	t.Helper()
	var wire bytes.Buffer
	for i, body := range bodies {
		if err := WriteFrame(&wire, tokens[i], body, false); err != nil {
			t.Fatalf("frame: %v", err)
		}
	}
	go func() { _, _ = s.srv.Write(wire.Bytes()) }()
}

func awaitReplies(t *testing.T, chans []<-chan Response, bodies [][]byte) {
	t.Helper()
	for i, ch := range chans {
		select {
		case r := <-ch:
			if r.Err != nil || !bytes.Equal(r.Body, bodies[i]) {
				t.Fatalf("reply %d: err=%v, %d bytes, want %d", i, r.Err, len(r.Body), len(bodies[i]))
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("reply %d not delivered", i)
		}
	}
}

func prepareReplies(c *Conn, n int) ([]UID, []<-chan Response) {
	tokens, chans := make([]UID, n), make([]<-chan Response, n)
	for i := range n {
		tokens[i], chans[i], _ = c.PrepareReply()
	}
	return tokens, chans
}

// One socket read must deliver every frame it carries, as C++ connectionReader
// does; reading length, checksum and payload separately cost three reads each.
func TestReadLoopDrainsCoalescedRepliesWithOneRead(t *testing.T) {
	t.Parallel()
	s := newSimServer(t)
	c, counted := dialCounted(t, s)
	tokens, chans := prepareReplies(c, 3)
	bodies := [][]byte{[]byte("first"), bytes.Repeat([]byte{'b'}, 900), []byte("third")}
	before := counted.reads.Load()
	sendReplies(t, s, tokens, bodies)
	awaitReplies(t, chans, bodies)
	// The read that carried the frames, plus the next one the loop blocks in.
	if reads := counted.reads.Load() - before; reads > 2 {
		t.Fatalf("READLOOP_UNBUFFERED: %d socket reads for 3 coalesced frames, want <= 2", reads)
	}
}

// Frames larger than the read buffer, and frames straddling it, arrive intact.
func TestReadLoopBufferBoundaries(t *testing.T) {
	t.Parallel()
	s := newSimServer(t)
	c, _ := dialCounted(t, s)
	sizes := []int{1, readBufferSize - 30, 3 * readBufferSize, 0, readBufferSize + 7, 64}
	tokens, chans := prepareReplies(c, len(sizes))
	bodies := make([][]byte, len(sizes))
	for i, n := range sizes {
		bodies[i] = bytes.Repeat([]byte{byte('a' + i)}, n)
	}
	sendReplies(t, s, tokens, bodies)
	awaitReplies(t, chans, bodies)
}
