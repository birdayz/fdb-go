package transport

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

func TestPingRequest_RealServerBidirectional(t *testing.T) {
	t.Parallel()
	setup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	server, err := foundationdbtc.Run(setup, "",
		foundationdbtc.WithoutInit(),
		foundationdbtc.WithKnob("connection_monitor_loop_time", "0.05"),
		foundationdbtc.WithKnob("connection_monitor_idle_timeout", "0.1"),
		foundationdbtc.WithKnob("connection_monitor_incoming_idle_multiplier", "1.0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := foundationdbtc.Terminate(ctx, server); err != nil {
			t.Error(err)
		}
	})
	host, err := server.Host(setup)
	if err != nil {
		t.Fatal(err)
	}
	var socket *pingCaptureConn
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		socket = &pingCaptureConn{Conn: conn}
		return socket, nil
	}
	conn, err := Dial(setup, net.JoinHostPort(host, fmt.Sprint(server.MappedPort())), nil, dial)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// The server sends its next Ping only after consuming the previous reply.
	// Match two independently decoded promises to replies from the read loop.
	ctx, cancelPing := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelPing()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		incoming, outgoing := socket.snapshot()
		if serverPingReplied(t, incoming, outgoing) {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("no two matching replies to real server Pings: %v; recv=%x sent=%x", ctx.Err(), incoming, outgoing)
		}
	}

	token, reply, handle := conn.PrepareReply()
	defer handle.Release()
	defer handle.Cancel()
	if err := conn.SendFrame(WellKnownToken(WLTokenPingPacket), BuildPingRequest(token)); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-reply:
		if response.Err != nil {
			t.Fatal(response.Err)
		}
		if _, err := wire.ReadErrorOr(response.Body); err != nil {
			t.Fatalf("real server rejected Ping: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("real server did not reply to Go Ping: %v", ctx.Err())
	}
}

type pingCaptureConn struct {
	net.Conn
	mu             sync.Mutex
	incoming, sent bytes.Buffer
}

func (c *pingCaptureConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	c.incoming.Write(p[:n])
	c.mu.Unlock()
	return n, err
}

func (c *pingCaptureConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.mu.Lock()
	c.sent.Write(p[:n])
	c.mu.Unlock()
	return n, err
}

func (c *pingCaptureConn) snapshot() ([]byte, []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.incoming.Bytes()), bytes.Clone(c.sent.Bytes())
}

func serverPingReplied(t *testing.T, incoming, outgoing []byte) bool {
	t.Helper()
	if len(incoming) < ConnectPacketSize || len(outgoing) < ConnectPacketSize {
		return false
	}
	matched := make(map[UID]struct{})
	reader := &FrameReader{}
	in := bytes.NewReader(incoming[ConnectPacketSize:])
	for in.Len() > 0 {
		token, body, err := reader.Read(in, false)
		if err != nil {
			return false // The socket may be between reads of a frame.
		}
		if token != WellKnownToken(WLTokenPingPacket) {
			continue
		}
		root, err := wire.NewReader(body)
		if err != nil {
			t.Fatal(err)
		}
		promise, err := root.ReadNestedReader(0)
		if err != nil {
			t.Fatal(err)
		}
		uid := promise.ReadUID(0)
		want := UID{First: binary.LittleEndian.Uint64(uid[:8]), Second: binary.LittleEndian.Uint64(uid[8:])}
		out := bytes.NewReader(outgoing[ConnectPacketSize:])
		for out.Len() > 0 {
			replyToken, replyBody, err := reader.Read(out, false)
			if err != nil {
				break
			}
			if replyToken == want {
				if _, err := wire.ReadErrorOr(replyBody); err != nil {
					t.Fatalf("Go replied to server Ping with an error: %v", err)
				}
				matched[want] = struct{}{}
				if len(matched) == 2 {
					return true
				}
				break
			}
		}
	}
	return false
}
