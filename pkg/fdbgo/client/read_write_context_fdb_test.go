package client

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// writeWaitWitness observes the post-enqueue select: enqueue evaluates Done
// once, then the acknowledgment wait evaluates it again. It does not delay or
// manufacture any socket result; the connection still talks to real FDB.
type writeWaitWitness struct {
	context.Context
	calls  atomic.Int32
	queued chan struct{}
}

func (c *writeWaitWitness) Done() <-chan struct{} {
	if c.calls.Add(1) == 2 {
		close(c.queued)
	}
	return c.Context.Done()
}

type readWriteGate struct {
	mu      sync.Mutex
	addr    string
	armed   bool
	parked  chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *readWriteGate) releaseIt() { g.once.Do(func() { close(g.release) }) }

func (g *readWriteGate) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return &readWriteGateConn{Conn: c, gate: g, addr: addr}, nil
}

type readWriteGateConn struct {
	net.Conn
	gate    *readWriteGate
	addr    string
	blocked atomic.Bool
}

func (c *readWriteGateConn) Write(body []byte) (int, error) {
	c.gate.mu.Lock()
	hold := c.gate.armed && c.gate.addr == c.addr
	if hold {
		c.gate.armed = false
		c.blocked.Store(true)
	}
	c.gate.mu.Unlock()
	if hold {
		close(c.gate.parked)
		<-c.gate.release
	}
	return c.Conn.Write(body)
}

func (c *readWriteGateConn) Close() error {
	err := c.Conn.Close()
	if c.blocked.Load() {
		c.gate.releaseIt()
	}
	return err
}

func TestReadWriteContext_RealFDBBackpressure(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"send", "deferred-flush", "teardown"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			gate := &readWriteGate{parked: make(chan struct{}), release: make(chan struct{})}
			defer gate.releaseIt()
			db := newTestDatabase(t, ctx, startProxyFDB(t, ctx), gate.dial)
			defer db.Close()
			key := []byte(t.Name())
			want := []byte("unchanged-body")
			rv := seedReadIncarnationValues(t, ctx, db, key, append(bytes.Clone(key), 'b'), want, []byte("other"))
			loc, err := db.db.locCache.locate(db.db, ctx, key, NoTenantID, types.SpanContext{}, false)
			if err != nil || len(loc.Servers) == 0 {
				t.Fatalf("locate: %v", err)
			}
			server := loc.Servers[0]
			conn, err := db.db.getOrDial(ctx, server.Address)
			if err != nil {
				t.Fatal(err)
			}
			request := func() ([]byte, <-chan transport.Response, *transport.ReplyHandle) {
				token, reply, handle := conn.PrepareReply()
				body, pooled := buildGetValueRequest(key, rv, false, NoTenantID, types.SpanContext{}, token, server.Token)
				owned := bytes.Clone(body)
				getValueBufPool.Put(pooled)
				return owned, reply, handle
			}
			body, reply1, handle1 := request()
			received1 := false
			defer func() {
				if !received1 {
					handle1.TakeReadyOrCancel()
				}
				handle1.Release()
			}()
			gate.mu.Lock()
			gate.addr, gate.armed = server.Address, true
			gate.mu.Unlock()
			if enqueued, err := conn.SendFrameDeferredContext(ctx, server.Token, body); err != nil || !enqueued {
				t.Fatalf("first deferred enqueue=%v err=%v", enqueued, err)
			}
			waitReadParked(t, ctx, gate.parked, "real storage socket write")

			body2, reply2, handle2 := request()
			received2 := false
			defer func() {
				if !received2 {
					handle2.TakeReadyOrCancel()
				}
				handle2.Release()
			}()
			parent, cancelWrite := context.WithCancel(ctx)
			defer cancelWrite()
			witness := &writeWaitWitness{Context: parent, queued: make(chan struct{})}
			type outcome struct {
				enqueued bool
				err      error
			}
			done := make(chan outcome, 1)
			if mode == "deferred-flush" {
				if enqueued, err := conn.SendFrameDeferredContext(parent, server.Token, body2); !enqueued || err != nil {
					t.Fatalf("second deferred enqueue=%v err=%v", enqueued, err)
				}
				go func() { done <- outcome{true, conn.FlushContext(witness)} }()
			} else {
				go func() {
					enqueued, err := conn.SendFrameContext(witness, server.Token, body2)
					done <- outcome{enqueued, err}
				}()
			}
			waitReadParked(t, ctx, witness.queued, "post-enqueue acknowledgment wait")
			if mode == "teardown" {
				if err := conn.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				cancelWrite()
			}
			select {
			case result := <-done:
				if !result.enqueued || result.err == nil || (mode != "teardown" && !errors.Is(result.err, context.Canceled)) {
					t.Fatalf("blocked %s result=%+v", mode, result)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("read write wait did not unblock while first socket write was held")
			}
			// The queued request must no longer borrow the caller's encoding.
			// Change only its key bytes to a different, same-length valid key.
			if bytes.Count(body2, key) != 1 {
				t.Fatal("encoded request must contain its key exactly once")
			}
			otherKey := bytes.Clone(key)
			otherKey[len(otherKey)-1] ^= 1
			copy(body2[bytes.Index(body2, key):], otherKey)
			gate.releaseIt()
			if mode != "teardown" {
				for i, reply := range []<-chan transport.Response{reply1, reply2} {
					select {
					case response := <-reply:
						if i == 0 {
							received1 = true
						} else {
							received2 = true
						}
						value, _, err := parseGetValueReply(response.Body)
						if response.Err != nil || err != nil || !bytes.Equal(value, want) {
							t.Fatalf("real FDB reply=%q, parse=%v transport=%v", value, err, response.Err)
						}
					case <-ctx.Done():
						t.Fatal("queued immutable request never received its real FDB response")
					}
				}
				if conn.IsClosed() {
					t.Fatal("read cancellation closed the shared connection")
				}
				if err := conn.FlushContext(ctx); err != nil {
					t.Fatalf("later flush consumed a stale acknowledgment: %v", err)
				}
			}
			assertIndependentRead(t, ctx, db, key, want)
		})
	}
}
