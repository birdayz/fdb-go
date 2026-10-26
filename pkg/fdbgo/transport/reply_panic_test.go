package transport

import (
	"context"
	"fmt"
	"testing"
	"time"
)

const replyPanicMarker = "panic after reply registration"

type replyPanicFrame struct {
	token UID
	body  []byte
}

// The probe observes an actual registration before the injected panic. A
// published reply is left buffered to verify cancellation does not pool it.
type replyPanicProbe struct {
	conn          *Conn
	token         UID
	channel       chan Response
	registrations int
	publish       bool
}

func (p *replyPanicProbe) observe() {
	p.registrations = 0
	for i := range p.conn.pendingShards {
		s := &p.conn.pendingShards[i]
		s.mu.Lock()
		p.registrations += len(s.m)
		for token, entry := range s.m {
			p.token, p.channel = token, entry.ch
			if p.publish {
				entry.ch <- Response{Body: []byte("published")}
				delete(s.m, token)
			}
		}
		s.mu.Unlock()
	}
}

type replyPanicWriter struct {
	probe *replyPanicProbe
	fired bool
}

func (w *replyPanicWriter) Write(body []byte) (int, error) {
	if !w.fired {
		w.fired = true
		w.probe.observe()
		panic(replyPanicMarker)
	}
	return len(body), nil
}

func requireReplyPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if got := recover(); got != replyPanicMarker {
			t.Errorf("panic = %v, want %q", got, replyPanicMarker)
		}
	}()
	fn()
}

func TestSendRegistrationPanicReleasesReply(t *testing.T) {
	t.Parallel()
	for _, route := range []string{"Send", "SendAndWait"} {
		for _, published := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/published=%t", route, published), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				peer := newSimServer(t)
				wireResult := make(chan replyPanicFrame, 1)
				peerDone := make(chan struct{})
				go func() {
					defer close(peerDone)
					if peer.handshake() != nil {
						return
					}
					var reader FrameReader
					token, body, err := reader.Read(peer.srv, false)
					if err == nil {
						wireResult <- replyPanicFrame{token: token, body: body}
						peer.drainUntilClosed()
					}
				}()
				probe := &replyPanicProbe{publish: published}
				writer := &replyPanicWriter{probe: probe}
				conn, err := dialWith(ctx, "reply-panic", peer.dialFunc(), nil, withMonitorCadence(time.Hour, time.Hour), WithLegacyWriter(), func(c *Conn) {
					probe.conn = c
					c.debugFrames, c.debugWriter = true, writer
				})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { conn.Close(); <-peerDone }()
				requireReplyPanic(t, func() {
					if route == "Send" {
						_, _, _ = conn.Send(UID{First: 71}, []byte("panicking send"))
					} else {
						_, _ = conn.SendAndWait(ctx, UID{First: 71}, []byte("panicking send"))
					}
				})
				if probe.registrations != 1 || probe.channel == nil {
					t.Fatalf("positive control: observed %d registrations, channel %v", probe.registrations, probe.channel)
				}
				if conn.HasPendingReply(probe.token) {
					t.Fatal("recovered send panic left its reply registered")
				}
				if published {
					select {
					case response := <-probe.channel:
						if string(response.Body) != "published" {
							t.Fatalf("published reply = %q", response.Body)
						}
					default:
						t.Fatal("panic cleanup pooled and drained an already-published reply")
					}
				}
				if conn.IsClosed() {
					t.Fatal("local panic cleanup closed the connection")
				}
				if err := conn.SendFrame(UID{First: 72}, []byte("reused")); err != nil {
					t.Fatalf("send after panic: %v", err)
				}
				select {
				case frame := <-wireResult:
					if frame.token.First != 72 || string(frame.body) != "reused" {
						t.Fatalf("wire frame after panic = %+v", frame)
					}
				case <-ctx.Done():
					t.Fatal("the same connection did not send after panic cleanup")
				}
			})
		}
	}
}

// Err is an existing call boundary in tryEnqueue, after the ping's registration
// and before handoff. No connection loop or global panic hook is involved.
type replyPanicContext struct {
	context.Context
	probe *replyPanicProbe
	fired bool
}

func (c *replyPanicContext) Err() error {
	if !c.fired {
		c.fired = true
		c.probe.observe()
		panic(replyPanicMarker)
	}
	return c.Context.Err()
}

func TestPingRegistrationPanicReleasesReply(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probe := &replyPanicProbe{}
	conn := &Conn{ctx: &replyPanicContext{Context: ctx, probe: probe}, cancel: cancel, writeCh: make(chan writeReq, 1)}
	probe.conn = conn
	requireReplyPanic(t, func() { conn.sendPingWithReply() })
	if probe.registrations != 1 || probe.channel == nil {
		t.Fatalf("positive control: observed %d registrations, channel %v", probe.registrations, probe.channel)
	}
	if conn.HasPendingReply(probe.token) || conn.IsClosed() {
		t.Fatal("ping panic retained its registration or closed the connection")
	}
	reply, handle := conn.sendPingWithReply()
	if reply == nil || handle == nil {
		t.Fatal("the same connection could not enqueue a ping after panic cleanup")
	}
	request := <-conn.writeCh
	conn.queued.Add(-1)
	token, ok := extractPingReplyToken(request.body)
	if !ok || !conn.HasPendingReply(token) {
		t.Fatal("reused ping did not register its encoded reply token")
	}
	conn.deliverPending(token, Response{})
	select {
	case <-reply:
	case <-ctx.Done():
		t.Fatal("ping reply was not delivered to its owner")
	}
	handle.Release() // received: the monitor's release
	if conn.HasPendingReply(token) {
		t.Fatal("ping owner left its reply registered")
	}
}
